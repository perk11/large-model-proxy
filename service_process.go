package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/shlex"
)

type serviceLoggingWriter struct {
	prefix     string
	logger     *log.Logger
	buf        []byte // holds an incomplete line between Write calls
	writeMutex sync.Mutex
}

func (w *serviceLoggingWriter) FinalFlush() {
	if w == nil {
		return
	}
	w.writeMutex.Lock()
	defer w.writeMutex.Unlock()
	if len(w.buf) == 0 {
		return
	}
	w.logger.Print(w.prefix + string(w.buf))
	w.buf = nil
}
func findLowerIndexThatIsNotMinusOne(indexOne int, indexTwo int) int {
	if indexOne == -1 {
		return indexTwo
	}
	if indexTwo == -1 {
		return indexOne
	}
	if indexOne > indexTwo {
		return indexTwo
	}
	return indexOne
}
func (w *serviceLoggingWriter) Write(b []byte) (int, error) {
	w.writeMutex.Lock()
	defer w.writeMutex.Unlock()
	// append new bytes to anything left over from the previous call
	data := append(w.buf, b...)
	for {
		returnIndex := bytes.IndexByte(data, '\r')
		newLineIndex := bytes.IndexByte(data, '\n')
		var cutOffIndex int
		if returnIndex != -1 && newLineIndex != -1 && newLineIndex-returnIndex == 1 {
			//CRLF
			cutOffIndex = newLineIndex
		} else {
			cutOffIndex = findLowerIndexThatIsNotMinusOne(newLineIndex, returnIndex)
		}

		if cutOffIndex == -1 {
			// no complete line yet – remember what we have and return
			w.buf = data
			return len(b), nil
		}
		// strip the trailing '\r' and log the line
		line := strings.TrimRight(string(data[:cutOffIndex]), "\r\n")
		w.logger.Print(w.prefix + line)

		// advance past the newline and continue scanning
		data = data[cutOffIndex+1:]
	}
}

func runServiceCommand(serviceConfig ServiceConfig) (
	*exec.Cmd,
	*serviceLoggingWriter,
	*serviceLoggingWriter,
) {
	if serviceConfig.LogFilePath == "" {
		serviceConfig.LogFilePath = "logs/" + serviceConfig.Name + ".log"
	}
	logDir := filepath.Dir(serviceConfig.LogFilePath)
	err := os.MkdirAll(logDir, os.ModePerm)
	if err != nil {
		log.Printf("[%s] Failed to create log directory %s: %v", serviceConfig.Name, logDir, err)
		return nil, nil, nil
	}

	args, err := shlex.Split(serviceConfig.Args)
	if err != nil {
		log.Printf("[%s] Failed to parse service arguments %s: %v", serviceConfig.Name, serviceConfig.Args, err)
		return nil, nil, nil
	}
	logFormatString, logArguments := produceStartCommandLogString(serviceConfig)
	log.Printf(logFormatString, logArguments...)

	cmd := exec.Command(serviceConfig.Command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}
	if serviceConfig.Workdir != "" {
		cmd.Dir = serviceConfig.Workdir
	}

	logFile, err := os.OpenFile(serviceConfig.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[%s] Error opening log file: %v", serviceConfig.Name, err)
		return nil, nil, nil
	}
	var stdoutSLW, stderrSLW *serviceLoggingWriter

	if *config.OutputServiceLogs {
		stdoutSLW = &serviceLoggingWriter{
			prefix: fmt.Sprintf("[%s/stdout] ", serviceConfig.Name),
			logger: log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lmicroseconds),
		}
		stderrSLW = &serviceLoggingWriter{
			prefix: fmt.Sprintf("[%s/stderr] ", serviceConfig.Name),
			logger: log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lmicroseconds),
		}
		cmd.Stdout = io.MultiWriter(logFile, stdoutSLW)
		cmd.Stderr = io.MultiWriter(logFile, stderrSLW)
	} else {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[%s] Error starting command: %v", serviceConfig.Name, err)
		return nil, nil, nil
	}
	return cmd, stdoutSLW, stderrSLW
}

func produceStartCommandLogString(serviceConfig ServiceConfig) (string, []any) {
	logFormatString := "[%s] Starting \"%s"
	logArguments := []any{
		serviceConfig.Name,
		serviceConfig.Command,
	}
	if serviceConfig.Args != "" {
		logFormatString += " %s"
		logArguments = append(logArguments, serviceConfig.Args)
	}
	logFormatString += "\""
	if serviceConfig.LogFilePath != "" {
		logFormatString += ", log file: %s"
		logArguments = append(logArguments, serviceConfig.LogFilePath)
	}

	if serviceConfig.Workdir != "" {
		logFormatString += ", workdir: %s"
		logArguments = append(logArguments, serviceConfig.Workdir)
	}
	return logFormatString, logArguments
}
func stopService(service ServiceConfig) {
	runningService, ok := resourceManager.maybeGetRunningService(service.Name)
	if !ok {
		log.Printf("[%s] Warning: Failed to find a service in a list of running services while stopping it, multiple stops requested or service already died. Stop aborted.", service.Name)
		return
	}
	stopRunningService(service, runningService)
}

// stopRunningService is the body of stopService with the map lookup factored out,
// so shutdown can stop each service directly from the held runningServices map
// without re-acquiring serviceMutex via maybeGetRunningService.
func stopRunningService(service ServiceConfig, runningService *RunningService) {
	if interrupted.Load() {
		//If the process is being interrupted, we want to stop the service no matter what, even if it's currently locked
		runningService.manageMutex.TryLock()
	} else {
		runningService.manageMutex.Lock()
		defer runningService.manageMutex.Unlock()
	}
	// idleTimer is also accessed (and nilled) under serviceMutex in
	// cleanUpStoppedServiceWhenServiceMutexIsLocked; take serviceMutex here so
	// the read/Stop agrees with that write on the same lock. stopService already
	// establishes a manageMutex -> serviceMutex ordering elsewhere (the
	// cleanUpStoppedServiceWhenServiceMutexIsLocked call below acquires
	// serviceMutex while manageMutex is held), so this is consistent.
	//
	// Guard with !interrupted: during shutdown the signal handler itself holds
	// serviceMutex and calls stopService, so re-Locking here would self-deadlock
	// (Go mutexes are not re-entrant). Skipping is safe on that path because the
	// idle-timer callback returns immediately when interrupted is set, and
	// cleanUp is skipped on the shutdown path too (see the !interrupted guard
	// below) — so nothing nils the timer concurrently while we skip.
	if !interrupted.Load() {
		resourceManager.serviceMutex.Lock()
		if runningService.idleTimer != nil {
			runningService.idleTimer.Stop()
		}
		resourceManager.serviceMutex.Unlock()
	}
	if runningService.cmd != nil && runningService.cmd.Process != nil {
		if service.KillCommand != nil {
			log.Printf("[%s] Sending custom kill command: %s", service.Name, *service.KillCommand)
			cmd := exec.Command("sh", "-c", *service.KillCommand)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setpgid: true,
				Pgid:    0,
			}
			err := cmd.Start()
			if err != nil {
				log.Printf("[%s] Failed to start custom kill command: %v", service.Name, err)
			}
			err = cmd.Wait()
			if err != nil {
				log.Printf("[%s] Failed to wait for custom kill command: %v", service.Name, err)
			}
		}
		log.Printf("[%s] Sending SIGTERM to service process group: -%d", service.Name, runningService.cmd.Process.Pid)
		err := syscall.Kill(-runningService.cmd.Process.Pid, syscall.SIGTERM)
		if err != nil {
			log.Printf("[%s] Failed to send SIGTERM to -%d: %v", service.Name, runningService.cmd.Process.Pid, err)
		}

		processExitedCleanly := waitForProcessToTerminate(runningService.exitWaitGroup)

		if !processExitedCleanly {
			log.Printf("[%s] Timed out waiting, sending SIGKILL to service process group -%d", service.Name, runningService.cmd.Process.Pid)
			err := syscall.Kill(-runningService.cmd.Process.Pid, syscall.SIGKILL)
			if err != nil {
				log.Printf("[%s] Failed to kill service: %v", service.Name, err)
				if runningService.cmd.ProcessState == nil && !errors.Is(err, syscall.ESRCH) { //ESRCH means process not found
					log.Printf("[%s] Manual action required due to error when killing process", service.Name)
					return
				}
			}
		}
	}
	if !interrupted.Load() && !*runningService.resourcesReleased {
		resourceManager.serviceMutex.Lock()
		cleanUpStoppedServiceWhenServiceMutexIsLocked(&service, runningService, true)
		resourceManager.serviceMutex.Unlock()
	}
}
func monitorProcess(serviceName string, process *os.Process, runningService *RunningService) {
	exitProcessState, err := process.Wait()
	exitMessage := fmt.Sprintf("[%s] Process with pid %d terminated", serviceName, process.Pid)
	if exitProcessState == nil {
		exitMessage += " with unknown exit code"
	} else {
		exitMessage += fmt.Sprintf(" with exit code %d", exitProcessState.ExitCode())
	}
	if err != nil {
		exitMessage += fmt.Sprintf(" and an error: %v", err)
	}
	// Signal process exit immediately, before any mutex acquisition.
	// This ensures stopService's waitForProcessToTerminate is not blocked
	// by monitorProcess waiting for serviceMutex.
	log.Print(exitMessage)
	runningService.exitWaitGroup.Done()

	if interrupted.Load() {
		if resourceManager.serviceMutex.TryLock() {
			defer resourceManager.serviceMutex.Unlock()
		} else {
			if config.LogLevel == LogLevelDebug {
				log.Printf("[%s] Not cleaning up resources due to large-model-proxy being interrupted", serviceName)
			}
			return
		}
	} else {
		// Test-only synchronization point (see waitForProcessExitHook): in
		// production builds this is a no-op, so no test scaffolding runs in the
		// hot path and the hook cannot be triggered accidentally.
		waitForProcessExitHook(serviceName)
		if config.LogLevel == LogLevelDebug {
			log.Printf("[%s] Acquiring a serviceMutex lock to clean up resources", serviceName)
		}
		resourceManager.serviceMutex.Lock()
		if config.LogLevel == LogLevelDebug {
			log.Printf("[%s] Acquired serviceMutex lock to clean up resources", serviceName)
		}
		defer resourceManager.serviceMutex.Unlock()
	}

	service := findServiceConfigByName(serviceName)
	cleanUpStoppedServiceWhenServiceMutexIsLocked(service, runningService, *service.ConsiderStoppedOnProcessExit)
}

func cleanUpStoppedServiceWhenServiceMutexIsLocked(service *ServiceConfig, runningService *RunningService, shouldReleaseResources bool) {
	if !shouldReleaseResources || *runningService.resourcesReleased {
		return
	}
	if config.LogLevel == LogLevelDebug {
		log.Printf("[%s] Cleaning up resources for stopped service", service.Name)
	}
	*runningService.resourcesReleased = true
	if runningService.idleTimer != nil {
		if config.LogLevel == LogLevelDebug {
			log.Printf("[%s] Stopping the timer for stopped service", service.Name)
		}
		runningService.idleTimer.Stop()
		runningService.idleTimer = nil
	}
	runningService.stdoutWriter.FinalFlush()
	runningService.stderrWriter.FinalFlush()

	if runningService.resourcesReserved {
		releaseResourcesWhenServiceMutexIsLocked(service.ResourceRequirements)
	}
	runningServiceInRM := resourceManager.runningServices[service.Name]
	if runningServiceInRM != runningService {
		log.Printf("[%s] ERROR: Running service pointer present in resourceManager.runningServices is not the same instance as the one for which clean up was called", service.Name)
	} else {
		delete(resourceManager.runningServices, service.Name)
	}
	resourceManager.broadcastResourceChanges(maps.Keys(service.ResourceRequirements), true)
}

func waitForProcessToTerminate(exitWaitGroup *sync.WaitGroup) bool {
	const ProcessCheckTimeout = 10 * time.Second
	exitChannel := make(chan struct{})
	go func() {
		exitWaitGroup.Wait()
		close(exitChannel)
	}()

	select {
	case <-exitChannel:
		return true
	case <-time.After(ProcessCheckTimeout):
		return false
	}
}

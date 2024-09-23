package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	ShutDownAfterInactivitySeconds                                time.Duration
	MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds *time.Duration
	Services                                                      []ServiceConfig `json:"Services"`
	ResourcesAvailable                                            map[string]int  `json:"ResourcesAvailable"`
}

type ServiceConfig struct {
	Name                            string
	ListenPort                      string
	ProxyTargetHost                 string
	ProxyTargetPort                 string
	Command                         string
	Args                            string
	LogFilePath                     string
	Workdir                         string
	HealthcheckCommand              string
	HealthcheckIntervalMilliseconds time.Duration
	ShutDownAfterInactivitySeconds  time.Duration
	RestartOnConnectionFailure      bool
	ResourceRequirements            map[string]int `json:"ResourceRequirements"`
}
type RunningService struct {
	manageMutex          *sync.Mutex
	cmd                  *exec.Cmd
	activeConnections    int
	lastUsed             time.Time
	idleTimer            *time.Timer
	resourceRequirements map[string]int
}

type ResourceManager struct {
	serviceMutex    *sync.Mutex
	resourcesInUse  map[string]int
	runningServices map[string]RunningService
}

func (rm ResourceManager) getRunningService(name string) RunningService {
	rm.serviceMutex.Lock()
	defer rm.serviceMutex.Unlock()
	return rm.runningServices[name]
}

func (rm ResourceManager) maybeGetRunningService(name string) (RunningService, bool) {
	rm.serviceMutex.Lock()
	defer rm.serviceMutex.Unlock()
	rs, ok := rm.runningServices[name]
	return rs, ok
}

func (rm ResourceManager) storeRunningService(name string, rs RunningService) {
	rm.serviceMutex.Lock()
	defer rm.serviceMutex.Unlock()
	rm.runningServices[name] = rs
}

func (rm ResourceManager) incrementConnection(name string, count int) {
	rm.serviceMutex.Lock()
	defer rm.serviceMutex.Unlock()

	runningService := resourceManager.runningServices[name]
	runningService.activeConnections += count
	resourceManager.runningServices[name] = runningService
}

func (rm ResourceManager) createRunningService(serviceConfig ServiceConfig) RunningService {
	rs := RunningService{
		resourceRequirements: serviceConfig.ResourceRequirements,
		activeConnections:    0,
		lastUsed:             time.Now(),
		manageMutex:          &sync.Mutex{},
	}
	rm.storeRunningService(serviceConfig.Name, rs)
	return rs
}

var (
	config          Config
	resourceManager ResourceManager
	interrupted     bool = false
)

func main() {
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	configFilePath := flag.String("c", "config.json", "path to config.json")
	flag.Parse()

	localConfig, err := loadConfig(*configFilePath)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}
	config = localConfig

	resourceManager = ResourceManager{
		resourcesInUse:  make(map[string]int),
		runningServices: make(map[string]RunningService),
		serviceMutex:    &sync.Mutex{},
	}

	for _, service := range config.Services {
		go startProxy(service)
	}
	for {
		receivedSignal := <-exit
		log.Printf("Received %s signal, terminating all processes", signalToString(receivedSignal))
		interrupted = true
		// no need to unlock as os.Exit will be called
		resourceManager.serviceMutex.Lock()
		for name := range resourceManager.runningServices {
			stopService(name)
		}
		log.Printf("Done, exiting")
		os.Exit(0)
	}
}
func signalToString(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return sig.String()
	}
}

func loadConfig(filePath string) (Config, error) {
	var config Config

	file, err := os.ReadFile(filePath)
	if err != nil {
		return config, err
	}

	decoder := json.NewDecoder(bytes.NewReader(file))
	decoder.DisallowUnknownFields()

	err = decoder.Decode(&config)
	if err != nil {
		return config, err
	}

	return config, nil
}

func startProxy(serviceConfig ServiceConfig) {
	listener, err := net.Listen("tcp", ":"+serviceConfig.ListenPort)
	log.Printf("[%s] Listening on port %s", serviceConfig.Name, serviceConfig.ListenPort)
	if err != nil {
		log.Fatalf("[%s] Fatal error: cannot listen on port %s: %v", serviceConfig.Name, serviceConfig.ListenPort, err)
	}
	defer func(listener net.Listener) {
		_ = listener.Close()
	}(listener)

	for {
		clientConnection, err := listener.Accept()
		if err != nil {
			log.Printf("[%s] Error accepting connection: %v", serviceConfig.Name, err)
			continue
		}
		go handleConnection(clientConnection, serviceConfig)
	}
}
func humanReadableConnection(conn net.Conn) string {
	if conn == nil {
		return "nil"
	}
	return fmt.Sprintf("%s->%s", conn.LocalAddr().String(), conn.RemoteAddr().String())
}
func handleConnection(clientConnection net.Conn, serviceConfig ServiceConfig) {
	defer func(clientConnection net.Conn, serviceConfig ServiceConfig) {
		log.Printf("[%s] Closing client connection %s on port %s", serviceConfig.Name, humanReadableConnection(clientConnection), serviceConfig.ListenPort)
		err := clientConnection.Close()
		if err != nil {
			log.Printf("[%s] Failed to close client connection %s: %v", serviceConfig.Name, humanReadableConnection(clientConnection), err)
		}
	}(clientConnection, serviceConfig)
	log.Printf("[%s] New client connection received %s", serviceConfig.Name, humanReadableConnection(clientConnection))
	serviceConnection := startServiceIfNotAlreadyRunningAndConnect(serviceConfig)

	if serviceConnection == nil {
		return
	}

	log.Printf("[%s] Opened service connection %s", serviceConfig.Name, humanReadableConnection(serviceConnection))
	defer func(serviceConnection net.Conn) {
		log.Printf("[%s] Closing service connection %s", serviceConfig.Name, humanReadableConnection(serviceConnection))
		err := serviceConnection.Close()
		if err != nil {
			log.Printf("[%s] Failed to close service connection %s: %v", serviceConfig.Name, humanReadableConnection(serviceConnection), err)
		}
		trackServiceLastUsed(serviceConfig)
	}(serviceConnection)
	forwardConnection(clientConnection, serviceConnection, serviceConfig.Name)
}

func startServiceIfNotAlreadyRunningAndConnect(serviceConfig ServiceConfig) net.Conn {
	var serviceConnection net.Conn
	runningService, found := resourceManager.maybeGetRunningService(serviceConfig.Name)
	if !found {
		serviceConn, err := startService(serviceConfig)
		if err != nil {
			log.Printf("[%s] Failed to start: %v", serviceConfig.Name, err)
			return nil
		}
		serviceConnection = serviceConn
	} else {
		if !runningService.manageMutex.TryLock() {
			//The service could be currently starting or stopping, so let's wait for that to finish and try again
			runningService.manageMutex.Lock()
			runningService.manageMutex.Unlock()
			//As the service might stop after the mutex is unlocked, we need to run the search for it again
			return startServiceIfNotAlreadyRunningAndConnect(serviceConfig)
		}
		trackServiceLastUsed(serviceConfig)
		runningService.manageMutex.Unlock()
		serviceConnection = connectToService(serviceConfig)
	}
	return serviceConnection
}

func getIdleTimeout(serviceConfig ServiceConfig) time.Duration {
	idleTimeout := serviceConfig.ShutDownAfterInactivitySeconds
	if idleTimeout == 0 {
		idleTimeout = config.ShutDownAfterInactivitySeconds
	}
	// for old configs
	if idleTimeout == 0 {
		idleTimeout = 2 * 60
	}
	idleTimeout = idleTimeout * time.Second
	return idleTimeout
}

func startService(serviceConfig ServiceConfig) (net.Conn, error) {
	runningService := resourceManager.createRunningService(serviceConfig)

	runningService.manageMutex.Lock()
	defer runningService.manageMutex.Unlock()

	if !reserveResources(serviceConfig.ResourceRequirements, serviceConfig.Name) {
		delete(resourceManager.runningServices, serviceConfig.Name)
		return nil, fmt.Errorf("insufficient resources %s", serviceConfig.Name)
	}

	var cmd = runServiceCommand(serviceConfig)
	if cmd == nil {
		releaseResources(serviceConfig.ResourceRequirements)
		delete(resourceManager.runningServices, serviceConfig.Name)
		return nil, fmt.Errorf("failed to run command \"%s %s\"", serviceConfig.Command, serviceConfig.Args)
	}
	performHealthCheck(serviceConfig)
	var serviceConnection = connectWithWaiting(serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort, serviceConfig.Name, 120*time.Second)

	runningService.cmd = cmd

	idleTimeout := getIdleTimeout(serviceConfig)
	runningService.idleTimer = time.AfterFunc(idleTimeout, func() {
		resourceManager.serviceMutex.Lock()
		defer resourceManager.serviceMutex.Unlock()

		if !canBeStopped(serviceConfig.Name) {
			log.Printf("[%s] Idle timeout %s reached, but service is busy, resetting idle time", serviceConfig.Name, idleTimeout)
			runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
			return
		}

		log.Printf("[%s] Idle timeout %s reached, stopping service", serviceConfig.Name, idleTimeout)
		stopService(serviceConfig.Name)
	})
	resourceManager.storeRunningService(serviceConfig.Name, runningService)
	return serviceConnection, nil
}

func performHealthCheck(serviceConfig ServiceConfig) {
	if serviceConfig.HealthcheckCommand == "" {
		return
	}

	log.Printf("[%s] Running healthcheck command \"%s\"", serviceConfig.Name, serviceConfig.HealthcheckCommand)
	for {
		if interrupted {
			return
		}
		cmd := exec.Command("sh", "-c", serviceConfig.HealthcheckCommand)
		err := cmd.Run()

		if err == nil {
			log.Printf("[%s] Healthceck \"%s\" returned exit code 0, healthcheck completed", serviceConfig.Name, serviceConfig.HealthcheckCommand)
			break
		} else {
			log.Printf(
				"[%s] Healtcheck \"%s\" returned exit code %d, trying again in %dms",
				serviceConfig.Name,
				serviceConfig.HealthcheckCommand,
				cmd.ProcessState.ExitCode(),
				serviceConfig.HealthcheckIntervalMilliseconds,
			)
			time.Sleep(serviceConfig.HealthcheckIntervalMilliseconds * time.Millisecond)
		}
	}
}

func connectToService(serviceConfig ServiceConfig) net.Conn {
	log.Printf("[%s] Opening new service connection to %s:%s", serviceConfig.Name, serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort)
	serviceConn, err := net.Dial("tcp", net.JoinHostPort(serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort))
	if err != nil {
		log.Printf("[%s] Error: failed to connect to %s:%s: %v", serviceConfig.Name, serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort, err)
		if serviceConfig.RestartOnConnectionFailure {
			log.Printf("[%s] Restarting service due to connection error", serviceConfig.Name)
			_, isRunning := resourceManager.maybeGetRunningService(serviceConfig.Name)
			if isRunning {
				stopService(serviceConfig.Name)
			}
			serviceConn, err = startService(serviceConfig)
			if err != nil {
				log.Printf("[%s] Failed to restart: %v", serviceConfig.Name, err)
				return nil
			}
			return serviceConn
		}
		return nil
	}
	return serviceConn
}

func connectWithWaiting(serviceHost string, servicePort string, serviceName string, timeout time.Duration) net.Conn {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(serviceHost, servicePort), time.Second)
		if err == nil {
			return conn
		}
		//log.Printf("[%s] Error when connecting to %s:%s, trying again in 100ms %v", serviceName, serviceHost, servicePort, err)
		time.Sleep(time.Millisecond * 100)
	}
	log.Printf("[%s] Error: failed to connect to %s:%s: All connection attempts failed after trying for %s", serviceName, serviceHost, servicePort, timeout)
	return nil
}

func reserveResources(resourceRequirements map[string]int, requestingService string) bool {
	var resourceList []string
	for resource, amount := range resourceRequirements {
		resourceList = append(resourceList, fmt.Sprintf("%s: %d", resource, amount))
	}
	log.Printf("[%s] Reserving %s", requestingService, strings.Join(resourceList, ", "))

	var missingResource *string = nil
	var maxWaitTime time.Duration
	if config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds == nil {
		maxWaitTime = 120 * time.Second
	} else {
		maxWaitTime = *config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds * time.Second
	}
	startTime := time.Now()
	var iteration = 0
	for time.Since(startTime) < maxWaitTime {
		missingResource = findFirstMissingResource(resourceRequirements, requestingService, iteration%60 == 0)
		iteration++
		if missingResource == nil {
			for resource, amount := range resourceRequirements {
				resourceManager.resourcesInUse[resource] += amount
			}
			return true
		}
		earliestLastUsedService := findEarliestLastUsedServiceUsingResource(requestingService, *missingResource)
		if earliestLastUsedService != "" {
			log.Printf("[%s] Stopping service to free resources for %s", earliestLastUsedService, requestingService)
			stopService(earliestLastUsedService)
			continue
		}
		log.Printf("[%s] Failed to find a service to stop, checking again in 1 second", requestingService)
		time.Sleep(1 * time.Second)
	}

	log.Printf("[%s] Failed to find a service to stop, closing client connection", requestingService)
	return false
}

func findEarliestLastUsedServiceUsingResource(requestingService string, missingResource string) string {
	earliestTime := time.Now()
	var earliestLastUsedService string

	resourceManager.serviceMutex.Lock()
	defer resourceManager.serviceMutex.Unlock()

	for serviceName, service := range resourceManager.runningServices {
		if serviceName == requestingService {
			continue
		}
		if service.resourceRequirements[missingResource] == 0 {
			continue
		}
		if !canBeStopped(serviceName) {
			continue
		}
		timeDifference := resourceManager.runningServices[requestingService].lastUsed.Sub(earliestTime)
		if timeDifference < 0 {
			earliestLastUsedService = serviceName
			earliestTime = resourceManager.runningServices[requestingService].lastUsed
		}
	}

	return earliestLastUsedService
}

func findFirstMissingResource(resourceRequirements map[string]int, requestingService string, outputError bool) *string {
	for resource, amount := range resourceRequirements {
		if resourceManager.resourcesInUse[resource]+amount > config.ResourcesAvailable[resource] {
			if outputError {
				log.Printf(
					"[%s] Not enough %s to start. Total: %d, In use: %d, Required: %d",
					requestingService,
					resource,
					config.ResourcesAvailable[resource],
					resourceManager.resourcesInUse[resource],
					amount,
				)
			}
			return &resource
		}
	}
	return nil
}

func trackServiceLastUsed(serviceConfig ServiceConfig) {
	runningService := resourceManager.getRunningService(serviceConfig.Name)
	runningService.lastUsed = time.Now()
	if runningService.idleTimer != nil {
		runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
	}
	resourceManager.storeRunningService(serviceConfig.Name, runningService)
}

func canBeStopped(serviceName string) bool {
	runningService := resourceManager.runningServices[serviceName]
	if !runningService.manageMutex.TryLock() {
		return false
	}
	runningService.manageMutex.Unlock()
	return runningService.activeConnections == 0
}

func releaseResources(used map[string]int) {
	for resource, amount := range used {
		resourceManager.resourcesInUse[resource] -= amount
	}
}

func runServiceCommand(serviceConfig ServiceConfig) *exec.Cmd {
	if serviceConfig.LogFilePath == "" {
		serviceConfig.LogFilePath = "logs/" + serviceConfig.Name + ".log"
	}
	logDir := filepath.Dir(serviceConfig.LogFilePath)
	err := os.MkdirAll(logDir, os.ModePerm)
	if err != nil {
		log.Printf("[%s] Failed to create log directory %s: %v", serviceConfig.Name, logDir, err)
		return nil
	}
	log.Printf("[%s] Starting \"%s %s\", log file: %s, workdir: %s",
		serviceConfig.Name,
		serviceConfig.Command,
		serviceConfig.Args,
		serviceConfig.LogFilePath,
		serviceConfig.Workdir,
	)
	cmd := exec.Command(serviceConfig.Command, strings.Split(serviceConfig.Args, " ")...)
	if serviceConfig.Workdir != "" {
		cmd.Dir = serviceConfig.Workdir
	}

	logFile, err := os.OpenFile(serviceConfig.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[%s] Error opening log file: %v", serviceConfig.Name, err)
		return nil
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		log.Printf("[%s] Error starting command: %v", serviceConfig.Name, err)
		return nil
	}
	return cmd
}

func forwardConnection(clientConnection net.Conn, serviceConnection net.Conn, serviceName string) {
	defer resourceManager.incrementConnection(serviceName, -1)
	resourceManager.incrementConnection(serviceName, 1)

	go copyAndHandleErrors(
		serviceConnection,
		clientConnection,
		fmt.Sprintf("[%s] (service (%s) to client (%s))", serviceName, humanReadableConnection(serviceConnection), humanReadableConnection(clientConnection)),
	)
	copyAndHandleErrors(
		clientConnection,
		serviceConnection,
		fmt.Sprintf("[%s] (client (%s) to service (%s))", serviceName, humanReadableConnection(clientConnection), humanReadableConnection(serviceConnection)),
	)
}

func stopService(serviceName string) {
	resourceManager.runningServices[serviceName].manageMutex.Lock()
	runningService := resourceManager.runningServices[serviceName]
	if runningService.idleTimer != nil {
		runningService.idleTimer.Stop()
	}
	if runningService.cmd != nil && runningService.cmd.Process != nil {
		log.Printf("[%s] Sending SIGTERM to service process: %d", serviceName, runningService.cmd.Process.Pid)
		err := runningService.cmd.Process.Signal(syscall.SIGTERM)
		if err != nil {
			log.Printf("[%s] Failed to send SIGTERM to %d: %v", serviceName, runningService.cmd.Process.Pid, err)
		}

		processExitedCleanly := waitForProcessToTerminate(runningService.cmd.Process)

		if !processExitedCleanly {
			log.Printf("[%s] Timed out waiting, sending SIGKILL to service process %d", serviceName, runningService.cmd.Process.Pid)
			err := runningService.cmd.Process.Kill()
			if err != nil {
				log.Printf("[%s] Failed to kill service: %v", serviceName, err)
				if runningService.cmd.ProcessState == nil {
					log.Printf("[%s] Manual action required due to error when killing process", serviceName)
					return
				}
			}
			log.Printf("[%s] Done killing pid %d", serviceName, runningService.cmd.Process.Pid)
		} else {
			log.Printf("[%s] Done stopping pid %d", serviceName, runningService.cmd.Process.Pid)
		}
	}

	releaseResources(runningService.resourceRequirements)
	resourceManager.runningServices[serviceName].manageMutex.Unlock()
	delete(resourceManager.runningServices, serviceName)
}

func waitForProcessToTerminate(process *os.Process) bool {
	const ProcessCheckTimeout = 10 * time.Second
	exitChannel := make(chan struct{})
	go func() {
		_, err := process.Wait()
		if err != nil {
			return
		}
		close(exitChannel)
	}()

	select {
	case <-exitChannel:
		return true
	case <-time.After(ProcessCheckTimeout):
		return false
	}
}

func copyAndHandleErrors(dst io.Writer, src io.Reader, logPrefix string) {
	_, err := io.Copy(dst, src)
	if err != nil {
		log.Printf("%s error during data transfer: %v", logPrefix, err)
	}
}

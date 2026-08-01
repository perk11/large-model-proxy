package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"
)

func startServiceIfNotAlreadyRunningAndConnect(serviceConfig ServiceConfig, clientDisconnected <-chan struct{}) net.Conn {
	if interrupted.Load() {
		return nil
	}
	var serviceConnection net.Conn
	runningService, found := resourceManager.maybeGetRunningService(serviceConfig.Name)
	if !found {
		serviceConn, err := startService(serviceConfig, clientDisconnected)
		if err != nil {
			log.Printf("[%s] Failed to start: %v", serviceConfig.Name, err)
			return nil
		}
		serviceConnection = serviceConn
	} else {
		if !runningService.manageMutex.TryLock() {
			if interrupted.Load() {
				return nil
			}
			log.Printf("[%s] Service is already starting or stopping, waiting for that operation to finish before proceeding with the current connection", serviceConfig.Name)
			// Wait for the holder to finish, but abort promptly if THIS queued client
			// disconnects. Otherwise its waiting-connection count would
			// stay inflated until the holder finishes/aborts, and when the holder aborts
			// we would recurse into a fresh startService for an already-disconnected
			// client. The mutex is a channelMutex, so LockOrCancel blocks on its token
			// channel and is woken the instant the holder releases it (no polling, no
			// orphaned goroutine) while still selecting on clientDisconnected.
			if !runningService.manageMutex.LockOrCancel(clientDisconnected) {
				return nil // client disconnected while queued
			}
			// We hold the lock. The service may have stopped while we waited, so search
			// for it again (as the original did) — but do not start/connect a service for
			// a client that is already gone.
			runningService.manageMutex.Unlock()
			select {
			case <-clientDisconnected:
				return nil
			default:
			}
			return startServiceIfNotAlreadyRunningAndConnect(serviceConfig, clientDisconnected)
		}
		trackServiceLastUsed(serviceConfig, true)
		runningService.manageMutex.Unlock()
		serviceConnection = connectToService(serviceConfig, clientDisconnected)
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
	return time.Duration(idleTimeout) * time.Second
}

func startService(serviceConfig ServiceConfig, clientDisconnected <-chan struct{}) (net.Conn, error) {
	now := time.Now()
	runningService := RunningService{
		lastUsed:              &now,
		isWaitingForResources: true,
		manageMutex:           newChannelMutex(),
		resourcesReleased:     new(bool),
	}
	runningService.manageMutex.Lock()
	resourceManager.serviceMutex.Lock()
	_, ok := resourceManager.runningServices[serviceConfig.Name]
	if ok {
		resourceManager.serviceMutex.Unlock()
		runningService.manageMutex.Unlock()
		log.Printf("[%s] ERROR: Trying to start a service while it is already present in the list of running services", serviceConfig.Name)
		return nil, fmt.Errorf("service already started")
	}
	resourceManager.runningServices[serviceConfig.Name] = &runningService
	resourceManager.serviceMutex.Unlock()

	if !reserveResources(serviceConfig.ResourceRequirements, serviceConfig.Name, clientDisconnected) {
		if interrupted.Load() {
			return nil, fmt.Errorf("interrupt signal was received")
		}
		resourceManager.serviceMutex.Lock()
		cleanUpStoppedServiceWhenServiceMutexIsLocked(&serviceConfig, &runningService, true)
		resourceManager.serviceMutex.Unlock()
		runningService.manageMutex.Unlock()
		return nil, fmt.Errorf("insufficient resources %s", serviceConfig.Name)
	}
	resourceManager.serviceMutex.Lock()
	runningService.isWaitingForResources = false
	runningService.resourcesReserved = true
	resourceManager.serviceMutex.Unlock()

	cmd, outW, errW := runServiceCommand(serviceConfig)
	if cmd == nil {
		resourceManager.serviceMutex.Lock()
		releaseReservedResourcesWhenServiceMutexIsLocked(serviceConfig.ResourceRequirements)
		cleanUpStoppedServiceWhenServiceMutexIsLocked(&serviceConfig, &runningService, true)
		resourceManager.serviceMutex.Unlock()
		runningService.manageMutex.Unlock()
		return nil, fmt.Errorf("failed to run command \"%s %s\"", serviceConfig.Command, serviceConfig.Args)
	}
	resourceManager.serviceMutex.Lock()
	runningService.cmd = cmd
	runningService.stdoutWriter = outW
	runningService.stderrWriter = errW

	runningService.exitWaitGroup = new(sync.WaitGroup)
	runningService.exitWaitGroup.Add(1)
	go monitorProcess(serviceConfig.Name, cmd.Process, &runningService)

	resourceManager.serviceMutex.Unlock()

	var startupConnectionTimeout time.Duration
	if serviceConfig.StartupTimeoutMilliseconds == nil {
		startupConnectionTimeout = 10 * time.Minute
	} else {
		startupConnectionTimeout = time.Duration(*serviceConfig.StartupTimeoutMilliseconds) * time.Millisecond
	}
	giveUpTime := time.Now().Add(startupConnectionTimeout)
	err := performHealthCheck(serviceConfig, startupConnectionTimeout, runningService.exitWaitGroup, clientDisconnected)
	if err != nil {
		log.Printf("[%s] Stopping service due to healthcheck error: %v", serviceConfig.Name, err)
		runningService.manageMutex.Unlock()
		stopService(serviceConfig)
		releaseReservedResources(serviceConfig.ResourceRequirements)
		return nil, fmt.Errorf("healthcheck failed: %w", err)
	}
	log.Printf("[%s] Service started with pid %d", serviceConfig.Name, cmd.Process.Pid)
	if interrupted.Load() {
		return nil, fmt.Errorf("interrupt signal was received")
	}

	var serviceConnection, processExited = tryConnectingUntilTimeoutOrProcessExit(
		serviceConfig.ProxyTargetHost,
		serviceConfig.ProxyTargetPort,
		serviceConfig.Name,
		time.Until(giveUpTime),
		runningService.exitWaitGroup,
		clientDisconnected,
	)
	if serviceConnection == nil {
		if processExited {
			log.Printf("[%s] Process terminated before a connection to the service could be established, stopping the service", serviceConfig.Name)
			runningService.manageMutex.Unlock()
			stopService(serviceConfig)
			releaseReservedResources(serviceConfig.ResourceRequirements)
			return nil, fmt.Errorf("process terminated before a connection to the service could be established")
		}
		//This log has to happen before the mutex unlock to maintain a logical order of logs
		log.Printf("[%s] Failed to connect to %s:%s, stopping the service", serviceConfig.Name, serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort)
		runningService.manageMutex.Unlock()
		stopService(serviceConfig)
		releaseReservedResources(serviceConfig.ResourceRequirements)
		return nil, fmt.Errorf("failed to connect to service")
	}
	defer runningService.manageMutex.Unlock()
	if interrupted.Load() {
		return nil, fmt.Errorf("interrupt signal was received")
	}

	resourceManager.serviceMutex.Lock()
	releaseReservedResourcesWhenServiceMutexIsLocked(serviceConfig.ResourceRequirements)

	runningService.isReady = true
	idleTimeout := getIdleTimeout(serviceConfig)
	runningService.idleTimer = time.AfterFunc(idleTimeout, func() {
		if interrupted.Load() {
			return
		}
		resourceManager.serviceMutex.Lock()
		// cleanUpStoppedServiceWhenServiceMutexIsLocked sets idleTimer to nil, so a nil
		// timer means the service has already been destroyed and this late-firing
		// callback must do nothing.
		if runningService.idleTimer == nil {
			resourceManager.serviceMutex.Unlock()
			if config.LogLevel == LogLevelDebug {
				log.Printf("[%s] Idle timer fired after the service was already destroyed, ignoring", serviceConfig.Name)
			}
			return
		}
		// Hold serviceMutex across canBeStopped AND the Reset so that
		// cleanUpStoppedServiceWhenServiceMutexIsLocked cannot nil idleTimer
		// between the nil-check above and this Reset. stopService acquires
		// serviceMutex internally, so it must run OUTSIDE this critical section.
		shouldStop := canBeStopped(serviceConfig.Name, &runningService)
		if shouldStop {
			resourceManager.serviceMutex.Unlock()
			log.Printf("[%s] Idle timeout %s reached, stopping service", serviceConfig.Name, idleTimeout)
			stopService(serviceConfig)
		} else {
			runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
			resourceManager.serviceMutex.Unlock()
			log.Printf("[%s] Idle timeout %s reached, but service is busy, resetting idle time", serviceConfig.Name, idleTimeout)
		}
	})
	resourceManager.serviceMutex.Unlock()

	return serviceConnection, nil
}
func performHealthCheck(serviceConfig ServiceConfig, timeout time.Duration, processExitWaitGroup *sync.WaitGroup, clientDisconnected <-chan struct{}) error {
	if serviceConfig.HealthcheckCommand == "" {
		return nil
	}

	log.Printf("[%s] Running healthcheck command \"%s\"", serviceConfig.Name, serviceConfig.HealthcheckCommand)

	totalTimeoutDeadlineTime := time.Now().Add(timeout)
	var sleepDuration time.Duration
	if serviceConfig.HealthcheckIntervalMilliseconds == 0 {
		sleepDuration = 100 * time.Millisecond
	} else {
		sleepDuration = time.Duration(serviceConfig.HealthcheckIntervalMilliseconds) * time.Millisecond
	}

	// processExitedChannel fires as soon as the service process is observed dead
	// (monitorProcess signals exitWaitGroup). When ConsiderStoppedOnProcessExit is
	// set, the proxy treats the child process exiting as the service being down,
	// so selecting on this channel aborts the healthcheck loop the moment the
	// process exits — mirroring tryConnectingUntilTimeoutOrProcessExit. When
	// ConsiderStoppedOnProcessExit is false (e.g. detached services such as docker
	// containers), the child process exiting is expected and the real service may
	// still be starting up, so the healthcheck must keep retrying until the
	// startup timeout instead of aborting. Leaving the channel nil makes the
	// process-exit cases below never selectable, so the loop is unaffected then.
	var processExitedChannel chan struct{}
	if *serviceConfig.ConsiderStoppedOnProcessExit {
		processExitedChannel = make(chan struct{})
		go func() {
			processExitWaitGroup.Wait()
			close(processExitedChannel)
		}()
	}

	for {
		if interrupted.Load() {
			return errors.New("interrupt signal was received")
		}

		remainingUntilDeadlineDuration := time.Until(totalTimeoutDeadlineTime)
		if remainingUntilDeadlineDuration <= 0 {
			return fmt.Errorf("healthcheck timed out after %s", timeout)
		}

		cmd := exec.Command("sh", "-c", serviceConfig.HealthcheckCommand)
		if err := cmd.Start(); err != nil {
			log.Printf("[%s] Failed to start healthcheck command \"%s\": %v", serviceConfig.Name, serviceConfig.HealthcheckCommand, err)
			return fmt.Errorf("failed to start healthcheck command \"%s\": %w", serviceConfig.HealthcheckCommand, err)
		}

		waitResultChan := make(chan error, 1)
		go func() { waitResultChan <- cmd.Wait() }()

		var waitErr error
		select {
		case waitErr = <-waitResultChan:
			// finished within the remaining time
		case <-time.After(remainingUntilDeadlineDuration):
			_ = cmd.Process.Kill()
			<-waitResultChan
			return fmt.Errorf("starting healthcheck command timed out after %s", remainingUntilDeadlineDuration)
		case <-clientDisconnected:
			_ = cmd.Process.Kill()
			<-waitResultChan
			return fmt.Errorf("client disconnected while waiting for healthcheck")
		case <-processExitedChannel:
			_ = cmd.Process.Kill()
			<-waitResultChan
			return fmt.Errorf("service process terminated while waiting for healthcheck, considering the service stopped, set ConsiderStoppedOnProcessExit to true if this is not desired")
		}

		if waitErr == nil {
			log.Printf("[%s] Healthcheck \"%s\" returned exit code 0, healthcheck completed", serviceConfig.Name, serviceConfig.HealthcheckCommand)
			return nil
		}

		exitCode := -1
		if exitError, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		}

		log.Printf(
			"[%s] Healthcheck \"%s\" returned exit code %d, trying again in %s",
			serviceConfig.Name,
			serviceConfig.HealthcheckCommand,
			exitCode,
			sleepDuration,
		)

		remainingUntilDeadlineDuration = time.Until(totalTimeoutDeadlineTime)
		if sleepDuration > remainingUntilDeadlineDuration {
			return fmt.Errorf(
				"healthcheck timed out, not starting another healthcheck command: not enough time left for another HealthcheckInterval(%v) in StartupTimeout(%v)",
				sleepDuration,
				timeout,
			)
		}
		if sleepDuration > 0 {
			select {
			case <-time.After(sleepDuration):
			case <-clientDisconnected:
				return fmt.Errorf("client disconnected while waiting for healthcheck")
			case <-processExitedChannel:
				return fmt.Errorf("service process terminated while waiting for healthcheck")
			}
		}
	}
}

func connectToService(serviceConfig ServiceConfig, clientDisconnected <-chan struct{}) net.Conn {
	log.Printf("[%s] Opening new service connection to %s:%s", serviceConfig.Name, serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort)
	serviceConn, err := net.Dial("tcp", net.JoinHostPort(serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort))
	if err != nil {
		log.Printf("[%s] Error: failed to connect to %s:%s: %v", serviceConfig.Name, serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort, err)
		if serviceConfig.RestartOnConnectionFailure {
			log.Printf("[%s] Restarting service due to connection error", serviceConfig.Name)
			_, isRunning := resourceManager.maybeGetRunningService(serviceConfig.Name)
			if isRunning {
				stopService(serviceConfig)
			}
			serviceConn, err = startService(serviceConfig, clientDisconnected)
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
func tryConnectingUntilTimeoutOrProcessExit(
	serviceHost string,
	servicePort string,
	serviceName string,
	timeout time.Duration,
	processExitWaitGroup *sync.WaitGroup,
	clientDisconnected <-chan struct{},
) (net.Conn, bool) {
	deadline := time.Now().Add(timeout)

	sleepDuration := 1 * time.Microsecond
	maxSleep := 100 * time.Millisecond

	processExitedChannel := make(chan struct{})
	go func() {
		processExitWaitGroup.Wait()
		close(processExitedChannel)
	}()

	for time.Now().Before(deadline) {
		select {
		case <-processExitedChannel:
			log.Printf("[%s] Process terminated while trying to connect to %s:%s", serviceName, serviceHost, servicePort)
			return nil, true
		case <-clientDisconnected:
			log.Printf("[%s] Client disconnected while trying to connect to %s:%s", serviceName, serviceHost, servicePort)
			return nil, false
		default:
		}
		if interrupted.Load() {
			return nil, false
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(serviceHost, servicePort), 1*time.Second)
		if err == nil {
			return conn, false
		}

		select {
		case <-processExitedChannel:
			log.Printf("[%s] Process terminated while trying to connect to %s:%s", serviceName, serviceHost, servicePort)
			return nil, true
		case <-time.After(sleepDuration):
		case <-clientDisconnected:
			log.Printf("[%s] Client disconnected while trying to connect to %s:%s", serviceName, serviceHost, servicePort)
			return nil, false
		}

		// Exponentially increase up to the maximum.
		sleepDuration *= 2
		if sleepDuration > maxSleep {
			sleepDuration = maxSleep
		}
	}

	log.Printf("[%s] Error: failed to connect to %s:%s: All connection attempts failed after trying for %s",
		serviceName, serviceHost, servicePort, timeout)
	return nil, false
}

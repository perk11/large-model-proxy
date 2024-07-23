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
	MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds *time.Duration
	Services                                                      []ServiceConfig `json:"Services"`
	ResourcesAvailable                                            map[string]int  `json:"ResourcesAvailable"`
}

type ServiceConfig struct {
	Name                       string
	ListenPort                 string
	ProxyTargetHost            string
	ProxyTargetPort            string
	Command                    string
	Args                       string
	LogFilePath                string
	Workdir                    string
	RestartOnConnectionFailure bool
	ResourceRequirements       map[string]int `json:"ResourceRequirements"`
}
type RunningService struct {
	manageMutex          *sync.Mutex
	cmd                  *exec.Cmd
	activeConnections    int
	lastUsed             time.Time
	resourceRequirements map[string]int
}

type ResourceManager struct {
	resourcesInUse  map[string]int
	runningServices map[string]RunningService
}

var (
	config          Config
	resourceManager ResourceManager
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
	}

	for _, service := range config.Services {
		go startProxy(service)
	}
	for {
		receivedSignal := <-exit
		log.Printf("Received %s signal, terminating all processes", signalToString(receivedSignal))
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

func startProxy(config ServiceConfig) {
	listener, err := net.Listen("tcp", ":"+config.ListenPort)
	log.Printf("[%s] Listening on port %s", config.Name, config.ListenPort)
	if err != nil {
		log.Fatalf("[%s] Fatal error: cannot listen on port %s: %v", config.Name, config.ListenPort, err)
	}
	defer func(listener net.Listener) {
		_ = listener.Close()
	}(listener)

	for {
		clientConnection, err := listener.Accept()
		if err != nil {
			log.Printf("[%s] Error accepting connection: %v", config.Name, err)
			continue
		}
		go handleConnection(clientConnection, config)
	}
}
func humanReadableConnection(conn net.Conn) string {
	if conn == nil {
		return "nil"
	}
	return fmt.Sprintf("%s->%s", conn.LocalAddr().String(), conn.RemoteAddr().String())
}
func handleConnection(clientConnection net.Conn, config ServiceConfig) {
	defer func(clientConnection net.Conn, config ServiceConfig) {
		log.Printf("[%s] Closing client connection %s on port %s", config.Name, humanReadableConnection(clientConnection), config.ListenPort)
		err := clientConnection.Close()
		if err != nil {
			log.Printf("[%s] Failed to close client connection %s: %v", config.Name, humanReadableConnection(clientConnection), err)
		}
	}(clientConnection, config)
	log.Printf("[%s] New client connection received %s", config.Name, humanReadableConnection(clientConnection))
	var serviceConnection net.Conn
	_, found := resourceManager.runningServices[config.Name]
	if !found {
		serviceConn, err := startService(config)
		if err != nil {
			log.Printf("[%s] Failed to start: %v", config.Name, err)
			return
		}
		serviceConnection = serviceConn
	} else {
		serviceConnection = connectToService(config)
		trackServiceLastUsed(config.Name)
	}

	if serviceConnection == nil {
		return
	}

	log.Printf("[%s] Opened service connection %s on port %s", config.Name, humanReadableConnection(serviceConnection), config.ProxyTargetPort)
	defer func(serviceConnection net.Conn) {
		log.Printf("[%s] Closing service connection %s on port %s", config.Name, humanReadableConnection(serviceConnection), config.ProxyTargetPort)
		err := serviceConnection.Close()
		if err != nil {
			log.Printf("[%s] Failed to close service connection %s: %v", config.Name, humanReadableConnection(serviceConnection), err)
		}
	}(serviceConnection)
	forwardConnection(clientConnection, serviceConnection, config.Name)
}

func startService(config ServiceConfig) (net.Conn, error) {
	resourceManager.runningServices[config.Name] = RunningService{
		resourceRequirements: config.ResourceRequirements,
		activeConnections:    0,
		lastUsed:             time.Now(),
		manageMutex:          &sync.Mutex{},
	}
	resourceManager.runningServices[config.Name].manageMutex.Lock()
	defer resourceManager.runningServices[config.Name].manageMutex.Unlock()
	if !reserveResources(config.ResourceRequirements, config.Name) {
		delete(resourceManager.runningServices, config.Name)
		return nil, fmt.Errorf("insufficient resources %s", config.Name)
	}

	var cmd = runServiceCommand(config)
	if cmd == nil {
		releaseResources(config.ResourceRequirements)
		delete(resourceManager.runningServices, config.Name)
		return nil, fmt.Errorf("failed to run command \"%s %s\"", config.Command, config.Args)
	}
	var serviceConnection = connectWithWaiting(config.ProxyTargetHost, config.ProxyTargetPort, config.Name, 60)
	time.Sleep(2 * time.Second) //TODO: replace with a custom callback

	runningService := resourceManager.runningServices[config.Name]
	runningService.cmd = cmd
	resourceManager.runningServices[config.Name] = runningService
	return serviceConnection, nil
}

func connectToService(config ServiceConfig) net.Conn {
	serviceConn, err := net.Dial("tcp", net.JoinHostPort(config.ProxyTargetHost, config.ProxyTargetPort))
	if err != nil {
		log.Printf("[%s] Error: failed to connect to %s:%s: %v", config.Name, config.ProxyTargetHost, config.ProxyTargetPort, err)
		if config.RestartOnConnectionFailure {
			log.Printf("[%s] Restarting service due to connection error", config.Name)
			stopService(config.Name)
			serviceConn, err = startService(config)
			if err != nil {
				log.Printf("[%s] Failed to restart: %v", config.Name, err)
				return nil
			}
			return serviceConn
		}
		return nil
	}
	return serviceConn
}

func connectWithWaiting(serviceHost string, servicePort string, serviceName string, timeout time.Duration) net.Conn {
	deadline := time.Now().Add(timeout * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(serviceHost, servicePort), time.Second)
		if err == nil {
			return conn
		}
		time.Sleep(time.Millisecond * 100)
	}
	log.Printf("[%s] Error: failed to connect to %s:%s: All connection attempts failed after trying for %ss", serviceName, serviceHost, servicePort, timeout)
	return nil
}

func reserveResources(resourceRequirements map[string]int, requestingService string) bool {
	var resourceList []string
	for resource, amount := range resourceRequirements {
		resourceList = append(resourceList, fmt.Sprintf("%s: %d", resource, amount))
	}
	log.Printf("[%s] Reserving %s", requestingService, strings.Join(resourceList, ", "))

	// First, try to reserve resources directly
	enoughResourcesAreAvailable := true
	var missingResource *string = nil
	for resource, amount := range resourceRequirements {
		if resourceManager.resourcesInUse[resource]+amount > config.ResourcesAvailable[resource] {
			log.Printf(
				"[%s] Not enough %s to start. Total: %d, In use: %d, Required: %d",
				requestingService,
				resource,
				config.ResourcesAvailable[resource],
				resourceManager.resourcesInUse[resource],
				amount,
			)
			missingResource = &resource
			enoughResourcesAreAvailable = false
			break
		}
	}

	if enoughResourcesAreAvailable {
		for resource, amount := range resourceRequirements {
			resourceManager.resourcesInUse[resource] += amount
		}
		return true
	}
	var earliestLastUsedService string
	var maxWaitTime time.Duration
	if config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds == nil {
		maxWaitTime = 120 * time.Second
	} else {
		maxWaitTime = *config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds * time.Second
	}
	startTime := time.Now()
	for time.Since(startTime) < maxWaitTime {
		earliestTime := time.Now()
		for serviceName, service := range resourceManager.runningServices {
			if serviceName == requestingService {
				continue
			}
			if service.resourceRequirements[*missingResource] == 0 {
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
		if earliestLastUsedService != "" {
			break
		}
		log.Printf("[%s] Failed to find a service to stop, checking again in 1 second", requestingService)
		time.Sleep(1 * time.Second)
	}

	if earliestLastUsedService == "" {
		log.Printf("[%s] Failed to find a service to stop, closing client connection", requestingService)
		return false
	}
	log.Printf("[%s] Stopping service to free resources for %s", earliestLastUsedService, requestingService)
	stopService(earliestLastUsedService)
	return reserveResources(resourceRequirements, requestingService)
}

func trackServiceLastUsed(requestingService string) {
	runningService := resourceManager.runningServices[requestingService]
	runningService.lastUsed = time.Now()
	resourceManager.runningServices[requestingService] = runningService
}
func canBeStopped(serviceName string) bool {
	runningService := resourceManager.runningServices[serviceName]
	if !runningService.manageMutex.TryLock() {
		return false
	}
	runningService.manageMutex.Unlock()
	return resourceManager.runningServices[serviceName].activeConnections == 0
}

func releaseResources(used map[string]int) {
	for resource, amount := range used {
		resourceManager.resourcesInUse[resource] -= amount
	}
}

func runServiceCommand(config ServiceConfig) *exec.Cmd {
	if config.LogFilePath == "" {
		config.LogFilePath = "logs/" + config.Name + ".log"
	}
	logDir := filepath.Dir(config.LogFilePath)
	err := os.MkdirAll(logDir, os.ModePerm)
	if err != nil {
		log.Printf("[%s] Failed to create log directory %s: %v", config.Name, logDir, err)
		return nil
	}
	log.Printf("[%s] Starting \"%s %s\", log file: %s, workdir: %s",
		config.Name,
		config.Command,
		config.Args,
		config.LogFilePath,
		config.Workdir,
	)
	cmd := exec.Command(config.Command, strings.Split(config.Args, " ")...)
	if config.Workdir != "" {
		cmd.Dir = config.Workdir
	}

	logFile, err := os.OpenFile(config.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[%s] Error opening log file: %v", config.Name, err)
		return nil
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		log.Printf("[%s] Error starting command: %v", config.Name, err)
		return nil
	}
	return cmd
}

func forwardConnection(clientConnection net.Conn, serviceConnection net.Conn, serviceName string) {
	defer func() {
		runningService := resourceManager.runningServices[serviceName]
		runningService.activeConnections--
		resourceManager.runningServices[serviceName] = runningService
	}()
	runningService := resourceManager.runningServices[serviceName]
	runningService.activeConnections++
	resourceManager.runningServices[serviceName] = runningService
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

	runningService := resourceManager.runningServices[serviceName]
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

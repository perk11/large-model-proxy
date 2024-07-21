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
	"strings"
	"sync"
	"time"
)

type Config struct {
	IdleShutdownTimeoutSeconds *time.Duration  `json:"IdleShutdownTimeoutSeconds"`
	Services                   []ServiceConfig `json:"Services"`
	ResourcesAvailable         map[string]int  `json:"ResourcesAvailable"`
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
	IdleShutdownTimeoutSeconds *time.Duration
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
	mutex              sync.Mutex
	resourcesAvailable map[string]int
	resourcesInUse     map[string]int
	runningServices    map[string]RunningService
}

var (
	resourceManager ResourceManager
)

func main() {
	configFilePath := flag.String("c", "config.json", "path to config.json")
	flag.Parse()

	config, err := loadConfig(*configFilePath)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	resourceManager = ResourceManager{
		resourcesAvailable: config.ResourcesAvailable,
		resourcesInUse:     make(map[string]int),
		runningServices:    make(map[string]RunningService),
	}

	var wg sync.WaitGroup
	for _, service := range config.Services {
		wg.Add(1)
		go manageServiceLifecycle(service, &wg)
	}
	wg.Wait()
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

func manageServiceLifecycle(service ServiceConfig, wg *sync.WaitGroup) {
	defer wg.Done()

	startProxy(service)
}

func startProxy(config ServiceConfig) {
	listener, err := net.Listen("tcp", ":"+config.ListenPort)
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
		handleConnection(clientConnection, config)
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
		if !reserveResources(config.ResourceRequirements, config.Name) {
			{
				log.Printf("[%s] Insufficient resources to start service", config.Name)
				return
			}
		}
		resourceManager.runningServices[config.Name] = RunningService{
			resourceRequirements: config.ResourceRequirements,
			activeConnections:    0,
			lastUsed:             time.Time{},
			manageMutex:          &sync.Mutex{},
		}
		resourceManager.runningServices[config.Name].manageMutex.Lock()
		defer resourceManager.runningServices[config.Name].manageMutex.Unlock()

		var cmd = startService(config)
		if cmd == nil {
			releaseResources(config.ResourceRequirements)
			return
		}
		serviceConnection = connectWithWaiting(config.ProxyTargetHost, config.ProxyTargetPort, config.Name, 60)
		time.Sleep(2 * time.Second) //TODO: replace with a custom callback

		runningService := resourceManager.runningServices[config.Name]
		runningService.cmd = cmd
		resourceManager.runningServices[config.Name] = runningService
	} else {
		serviceConnection = connectToService(config.ProxyTargetHost, config.ProxyTargetPort, config.Name)
	}
	log.Printf("[%s] Opened service connection %s on port %s", config.Name, humanReadableConnection(serviceConnection), config.ProxyTargetPort)

	if serviceConnection == nil {
		return
	}
	defer func(serviceConnection net.Conn) {
		log.Printf("[%s] Closing service connection %s on port %s", config.Name, humanReadableConnection(serviceConnection), config.ProxyTargetPort)
		err := serviceConnection.Close()
		if err != nil {
			log.Printf("[%s] Failed to close service connection %s: %v", config.Name, humanReadableConnection(serviceConnection), err)
		}
	}(serviceConnection)
	forwardConnection(clientConnection, serviceConnection, config.Name)
}

func connectToService(serviceHost string, servicePort string, serviceName string) net.Conn {
	serviceConn, err := net.Dial("tcp", net.JoinHostPort(serviceHost, servicePort))
	if err != nil {
		log.Printf("[%s] Error: failed to connect to %s:%s: %v", serviceName, serviceHost, servicePort, err)
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
	log.Printf("[%s] Reserving %s", requestingService, resourceRequirements)

	// First, try to reserve resources directly
	enoughResourcesAreAvailable := true
	for resource, amount := range resourceRequirements {
		if resourceManager.resourcesInUse[resource]+amount > resourceManager.resourcesAvailable[resource] {
			log.Printf(
				"[%s] Not enough %s to start. Total: %d, In use: %d, Required: %d",
				requestingService,
				resource,
				resourceManager.resourcesAvailable[resource],
				resourceManager.resourcesInUse[resource],
				amount,
			)
			enoughResourcesAreAvailable = false
			break
		}
	}

	if enoughResourcesAreAvailable {
		for resource, amount := range resourceRequirements {
			resourceManager.resourcesInUse[resource] += amount
		}
		runningService := resourceManager.runningServices[requestingService]
		runningService.lastUsed = time.Now()
		resourceManager.runningServices[requestingService] = runningService
		return true
	}
	var earliestLastUsedService string
	maxWaitTime := 120 * time.Second
	startTime := time.Now()
	for time.Since(startTime) < maxWaitTime {
		earliestTime := time.Now()
		for service := range resourceManager.runningServices {
			if service != requestingService && canBeStopped(service) {
				if resourceManager.runningServices[requestingService].lastUsed.Compare(earliestTime) == -1 {
					earliestLastUsedService = service
					earliestTime = resourceManager.runningServices[requestingService].lastUsed
				}
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

func startService(config ServiceConfig) *exec.Cmd {
	log.Printf("[%s] Starting service: %s", config.Name, config.Command)
	cmd := exec.Command(config.Command, strings.Split(config.Args, " ")...)
	if config.Workdir != "" {
		cmd.Dir = config.Workdir
	}
	if config.LogFilePath == "" {
		config.LogFilePath = "logs/" + config.Name + ".log"
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
		log.Printf("[%s] Stopping service process: %d", serviceName, runningService.cmd.Process.Pid)
		err := runningService.cmd.Process.Kill()
		if err != nil {
			log.Printf("[%s] Failed to stop service: %v", serviceName, err)
			if runningService.cmd.ProcessState == nil {
				//the process is still running
				return
			}
		}
	}

	releaseResources(runningService.resourceRequirements)
	delete(resourceManager.runningServices, serviceName)
}

func copyAndHandleErrors(dst io.Writer, src io.Reader, logPrefix string) {
	_, err := io.Copy(dst, src)
	if err != nil {
		log.Printf("%s error during data transfer: %v", logPrefix, err)
	}
}

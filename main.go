package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const IDLE_SHUTDOWN_TIMEOUT_SECONDS_DEFAULT time.Duration = 120

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
	defer listener.Close()

	for {
		clientConnection, err := listener.Accept()
		if err != nil {
			log.Printf("[%s] Error accepting connection: %v", config.Name, err)
			continue
		}
		handleConnection(clientConnection, config)
	}
}

func handleConnection(clientConn net.Conn, config ServiceConfig) {
	defer clientConn.Close()

	var serviceConn net.Conn
	_, found := resourceManager.runningServices[config.Name]
	if !found {
		if !reserveResources(config.ResourceRequirements, config.Name) {
			{
				log.Printf("[%s] Insufficient resources to start service", config.Name)
				clientConn.Close()
				return
			}
		}
		var cmd = startService(config)
		if cmd == nil {
			releaseResources(config.ResourceRequirements)
			return
		}
		resourceManager.runningServices[config.Name] = RunningService{
			cmd:                  cmd,
			resourceRequirements: config.ResourceRequirements,
			activeConnections:    0,
			lastUsed:             time.Time{},
		}
		serviceConn = connectWithWaiting(config.ProxyTargetHost, config.ProxyTargetPort, config.Name, 60)
		time.Sleep(2 * time.Second)
	} else {
		serviceConn = connectToService(config.ProxyTargetHost, config.ProxyTargetPort, config.Name)
	}
	if serviceConn == nil {
		clientConn.Close()
		return
	}
	forwardConnection(clientConn, serviceConn, config.Name)
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
	allAvailable := true
	for resource, amount := range resourceRequirements {
		if resourceManager.resourcesInUse[resource]+amount > resourceManager.resourcesAvailable[resource] {
			allAvailable = false
			break
		}
	}

	if allAvailable {
		for resource, amount := range resourceRequirements {
			resourceManager.resourcesInUse[resource] += amount
		}
		runningService := resourceManager.runningServices[requestingService]
		runningService.lastUsed = time.Now()
		resourceManager.runningServices[requestingService] = runningService
		return true
	}
	var earliestLastUsedService string
	earliestTime := time.Now()
	for service := range resourceManager.runningServices {
		if service != requestingService && canBeStopped(service) {
			if resourceManager.runningServices[requestingService].lastUsed.Compare(earliestTime) == -1 {
				earliestLastUsedService = service
				earliestTime = resourceManager.runningServices[requestingService].lastUsed
			}
		}
	}

	if earliestLastUsedService == "" {
		log.Printf("Failed to find a service to stop")
		return false
	}
	log.Printf("[%s] Stopping service to free resources for %s", earliestLastUsedService, requestingService)
	stopService(earliestLastUsedService)
	return reserveResources(resourceRequirements, requestingService)
}
func canBeStopped(serviceName string) bool {
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

func forwardConnection(clientConn, serviceConn net.Conn, serviceName string) {
	defer clientConn.Close()
	defer serviceConn.Close()
	defer func() {
		runningService := resourceManager.runningServices[serviceName]
		runningService.activeConnections--
		resourceManager.runningServices[serviceName] = runningService
	}()
	runningService := resourceManager.runningServices[serviceName]
	runningService.activeConnections++
	resourceManager.runningServices[serviceName] = runningService
	go copyAndHandleErrors(serviceConn, clientConn, serviceName+" (service to client)")
	copyAndHandleErrors(clientConn, serviceConn, serviceName+" (client to service)")
}

func stopService(serviceName string) {

	runningService := resourceManager.runningServices[serviceName]
	if runningService.cmd != nil && runningService.cmd.Process != nil {
		log.Printf("[%s] Stopping service process: %d", serviceName, runningService.cmd.Process.Pid)
		runningService.cmd.Process.Kill()
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

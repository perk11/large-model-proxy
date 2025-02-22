package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/shlex"
)

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
type OpenAiApiModels struct {
	Object string           `json:"object"`
	Data   []OpenAiApiModel `json:"data"`
}
type OpenAiApiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Created int64  `json:"created"`
}
type ModelContainingRequest struct {
	Model string `json:"model"`
}

// maybeGetRunningServiceNoLock Only use if serviceMutex is already locked.
func (rm ResourceManager) maybeGetRunningServiceNoLock(name string) (RunningService, bool) {
	rs, ok := rm.runningServices[name]
	return rs, ok
}

func (rm ResourceManager) maybeGetRunningService(name string) (RunningService, bool) {
	if interrupted {
		if rm.serviceMutex.TryLock() {
			defer rm.serviceMutex.Unlock()
		}
	} else {
		rm.serviceMutex.Lock()
		defer rm.serviceMutex.Unlock()
	}
	return rm.maybeGetRunningServiceNoLock(name)
}

func (rm ResourceManager) storeRunningService(name string, rs RunningService) {
	rm.serviceMutex.Lock()
	defer rm.serviceMutex.Unlock()
	rm.storeRunningServiceNoLock(name, rs)
}

// storeRunningServiceNoLock Only use if serviceMutex is already locked.
func (rm ResourceManager) storeRunningServiceNoLock(name string, rs RunningService) {
	rm.runningServices[name] = rs
}

func (rm ResourceManager) incrementConnection(name string, count int) {
	rm.serviceMutex.Lock()
	defer rm.serviceMutex.Unlock()
	runningService, ok := rm.maybeGetRunningServiceNoLock(name)
	if !ok {
		log.Printf("[%s] Warning: Tried to increment connection numbers but couldn't get the running service", name)
		return
	}
	runningService.activeConnections += count
	rm.storeRunningServiceNoLock(name, runningService)
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
	interrupted     = false
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	configFilePath := flag.String("c", "config.json", "path to config.json")
	flag.Parse()

	var err error
	config, err = loadConfig(*configFilePath)
	if err != nil {
		log.Printf("Error loading %s:\n", *configFilePath)
		FprintfError("%v\n", err)
		os.Exit(1)
	}

	resourceManager = ResourceManager{
		resourcesInUse:  make(map[string]int),
		runningServices: make(map[string]RunningService),
		serviceMutex:    &sync.Mutex{},
	}

	for _, service := range config.Services {
		if service.ListenPort != "" {
			go startProxy(service)
		}
	}
	if config.OpenAiApi.ListenPort != "" {
		go startOpenAiApi(config.OpenAiApi, config.Services)
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
func createOpenAiApiModel(name string) OpenAiApiModel {
	return OpenAiApiModel{
		ID:      name,
		Object:  "model",
		OwnedBy: "large-model-proxy",
		Created: time.Now().Unix(),
	}
}

type rawCaptureConnection struct {
	net.Conn
	mutex  sync.Mutex
	buffer *bytes.Buffer
}

func (rcc *rawCaptureConnection) Read(p []byte) (int, error) {
	n, err := rcc.Conn.Read(p)
	if n > 0 {
		rcc.mutex.Lock()
		rcc.buffer.Write(p[:n])
		rcc.mutex.Unlock()
	}
	return n, err
}

type rawCaptureListener struct {
	net.Listener
}

func (rawCaptureListener *rawCaptureListener) Accept() (net.Conn, error) {
	connection, err := rawCaptureListener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &rawCaptureConnection{
		Conn:   connection,
		buffer: new(bytes.Buffer),
	}, nil
}

type contextKey string

var rawConnectionContextKey = contextKey("rawConn")

func startOpenAiApi(OpenAiApi OpenAiApi, services []ServiceConfig) {
	mux := http.NewServeMux()
	modelToServiceMap := make(map[string]ServiceConfig)
	models := make([]OpenAiApiModel, 0)
	for _, service := range services {
		if !service.OpenAiApi {
			continue
		}
		// If the service doesn't define specific model names, assume the service name is the model
		if service.OpenAiApiModels == nil || len(service.OpenAiApiModels) == 0 {
			modelToServiceMap[service.Name] = service
			models = append(models, createOpenAiApiModel(service.Name))
		} else {
			for _, model := range service.OpenAiApiModels {
				modelToServiceMap[model] = service
				models = append(models, createOpenAiApiModel(model))
			}
		}
	}
	modelsResponse := OpenAiApiModels{
		Object: "models",
		Data:   models,
	}
	mux.HandleFunc("/v1/models", func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
		err := json.NewEncoder(responseWriter).Encode(modelsResponse)
		if err != nil {
			http.Error(responseWriter, "{error: \"Failed to produce JSON response\"}", http.StatusInternalServerError)
			log.Printf("Failed to produce /v1/models JSON response: %s\n", err.Error())
		}
		resetConnectionBuffer(request)
	})
	mux.HandleFunc("/v1/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		if !handleCompletions(responseWriter, request, &modelToServiceMap) {
			resetConnectionBuffer(request)
		}
	})
	mux.HandleFunc("/v1/chat/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		if !handleCompletions(responseWriter, request, &modelToServiceMap) {
			resetConnectionBuffer(request)
		}
	})
	mux.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		//404
		log.Printf("[LLM Request API] %s request to unsupported URL: %s", request.Method, request.RequestURI)
		http.Error(
			responseWriter,
			fmt.Sprintf("%s %s is not supoprted by large-model-proxy", request.Method, request.RequestURI),
			http.StatusNotFound,
		)
		resetConnectionBuffer(request)
	})

	// Create a custom http.Server that uses ConnContext
	// to attach the *rawCaptureConnection to each request's Context.
	server := &http.Server{
		Addr:    ":" + OpenAiApi.ListenPort,
		Handler: mux,
		// Whenever the server accepts a new net.Conn, this callback runs.
		// If it's our rawCaptureConnection, store it in the request context.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if rcc, ok := c.(*rawCaptureConnection); ok {
				return context.WithValue(ctx, rawConnectionContextKey, rcc)
			}
			return ctx
		},
	}

	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatalf("[LLM API Server] Could not listen on %s: %v", server.Addr, err)
	}
	wrappedLn := &rawCaptureListener{Listener: ln}

	log.Printf("[LLM API Server] Listening on port %s", OpenAiApi.ListenPort)
	if err := server.Serve(wrappedLn); err != nil {
		log.Fatalf("Could not start LLM API server: %s\n", err.Error())
	}
}

// resetConnectionBuffer clears the buffer so that if another request is received through the same connection, it starts from scratch
func resetConnectionBuffer(request *http.Request) {
	rawConnection, ok := request.Context().Value(rawConnectionContextKey).(*rawCaptureConnection)
	if !ok {
		panic("Failed to get raw connection")
	}
	rawConnection.buffer = new(bytes.Buffer)
}

// handleCompletions returns true if connection was proxied, false on HTTP error
func handleCompletions(responseWriter http.ResponseWriter, request *http.Request, modelToServiceMap *map[string]ServiceConfig) bool {
	if request.Method != http.MethodPost {
		http.Error(responseWriter, "Only POST requests allowed", http.StatusBadRequest)
		return false
	}
	originalBody := request.Body
	defer func(originalBody io.ReadCloser) {
		err := originalBody.Close()
		if err != nil {
			log.Printf("[LLM API Server] Error closing request body: %s\n", err.Error())
		}
	}(originalBody)
	//TODO: parse request directly
	bodyBytes, err := io.ReadAll(originalBody)
	if err != nil {
		log.Printf("[LLM API Server] Error reading request body: %v\n", err)
		http.Error(responseWriter, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
		return false
	}

	model, ok := extractModelFromRequest(request.URL.String(), bodyBytes)
	if !ok {
		http.Error(responseWriter, fmt.Sprintf("Failed to parse request: %v", err), http.StatusBadRequest)
		return false
	}

	service, ok := (*modelToServiceMap)[model]
	if !ok {
		log.Printf("[LLM API Server] Unknown model requested: %v\n", model)
		http.Error(responseWriter, fmt.Sprintf("Unknown model: %v", model), http.StatusBadRequest)
		return false
	}
	log.Printf("[LLM API Server] Sending %s request through to %s\n", request.URL, service.Name)
	originalWriter := responseWriter
	hijacker, ok := originalWriter.(http.Hijacker)
	if !ok {
		log.Printf("[LLM API Server] Error: Failed to forward connection: web server does not support hijacking. This could only happen if LLM API Server is running in HTTP/2 mode. Please use HTTP/1.1\n")
		http.Error(responseWriter, "Request forwarding is not possible, please use HTTP 1.1", http.StatusInternalServerError)
		return false
	}
	clientConnection, bufrw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("[LLM API Server] Failed to forward connection: %v", err)
		http.Error(responseWriter, err.Error(), http.StatusInternalServerError)
		return false
	}
	//TODO: check if we can stop buffering and clean up the buffer now
	rawConnection, ok := request.Context().Value(rawConnectionContextKey).(*rawCaptureConnection)
	if !ok {
		panic("Failed to get raw connection")
	}
	rawRequestBytes := rawConnection.buffer.Bytes()

	if bufrw.Reader.Buffered() > 0 {
		bufBytes := make([]byte, bufrw.Reader.Buffered())
		if _, err := bufrw.Read(bufBytes); err != nil {
			log.Printf("[LLM API Server] Error reading buffered data: : %v", err)
		}
		bodyBytes = append(bodyBytes, bufBytes...)
	}
	handleConnection(clientConnection, service, rawRequestBytes)
	return true
}

// extractModelFromRequest returns model name and whether reading model name was successful
func extractModelFromRequest(url string, bodyBytes []byte) (string, bool) {
	var completionRequest ModelContainingRequest
	if err := json.Unmarshal(bodyBytes, &completionRequest); err != nil {
		log.Printf("[LLM API Server] Error decoding %s request: %v\n%s", url, err, bodyBytes)
		return "", false
	}
	return completionRequest.Model, true
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
		if interrupted {
			return
		}
		clientConnection, err := listener.Accept()
		if err != nil {
			log.Printf("[%s] Error accepting connection: %v", serviceConfig.Name, err)
			continue
		}
		log.Printf("[%s] New client connection received %s", serviceConfig.Name, humanReadableConnection(clientConnection))
		go handleConnection(clientConnection, serviceConfig, []byte{})
	}
}
func humanReadableConnection(conn net.Conn) string {
	if conn == nil {
		return "nil"
	}
	return fmt.Sprintf("%s->%s", conn.LocalAddr().String(), conn.RemoteAddr().String())
}

func handleConnection(clientConnection net.Conn, serviceConfig ServiceConfig, dataToSendToServiceBeforeForwardingFromClient []byte) {
	if interrupted {
		_ = clientConnection.Close()
		return
	}
	serviceConnection := startServiceIfNotAlreadyRunningAndConnect(serviceConfig)

	if serviceConnection == nil {
		closeConnectionAndHandleError(
			clientConnection,
			serviceConfig,
			"client",
			"failed to start service",
		)
		return
	}

	log.Printf("[%s] Opened service connection %s", serviceConfig.Name, humanReadableConnection(serviceConnection))
	if len(dataToSendToServiceBeforeForwardingFromClient) > 0 {
		if _, err := serviceConnection.Write(dataToSendToServiceBeforeForwardingFromClient); err != nil {
			log.Printf("[%s] Error writing bytes read from client to service: %v", serviceConfig.Name, err)
			closeConnectionAndHandleError(
				clientConnection,
				serviceConfig,
				"client",
				"internal error",
			)
			closeConnectionAndHandleError(
				serviceConnection,
				serviceConfig,
				"service",
				"internal error",
			)
			return
		}
	}
	//forwardConnection will handle closing the connections at this point
	forwardConnection(clientConnection, serviceConnection, serviceConfig.Name)

	trackServiceLastUsed(serviceConfig)
}

func closeConnectionAndHandleError(connection net.Conn, serviceConfig ServiceConfig, connectionType string, reason string) {
	log.Printf(
		"[%s] Closing %s connection %s: %s",
		serviceConfig.Name,
		connectionType,
		humanReadableConnection(connection),
		reason,
	)
	err := connection.Close()
	if err != nil {
		log.Printf(
			"[%s] Failed to close %s connection %s: %v",
			serviceConfig.Name,
			connectionType,
			humanReadableConnection(connection),
			err,
		)
	}
}

func startServiceIfNotAlreadyRunningAndConnect(serviceConfig ServiceConfig) net.Conn {
	if interrupted {
		return nil
	}
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
			if interrupted {
				return nil
			}
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
	runningService.cmd = cmd
	resourceManager.storeRunningService(serviceConfig.Name, runningService)

	performHealthCheck(serviceConfig)
	log.Printf("[%s] Service started", serviceConfig.Name)
	if interrupted {
		return nil, fmt.Errorf("interrupt signal was received")
	}
	var serviceConnection = connectWithWaiting(serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort, serviceConfig.Name, 120*time.Second)
	if interrupted {
		return nil, fmt.Errorf("interrupt signal was received")
	}

	idleTimeout := getIdleTimeout(serviceConfig)
	runningService.idleTimer = time.AfterFunc(idleTimeout, func() {
		if interrupted {
			return
		}
		resourceManager.serviceMutex.Lock()
		shouldStop := canBeStopped(serviceConfig.Name)
		resourceManager.serviceMutex.Unlock()
		if shouldStop {
			log.Printf("[%s] Idle timeout %s reached, stopping service", serviceConfig.Name, idleTimeout)
			stopService(serviceConfig.Name)
		} else {
			log.Printf("[%s] Idle timeout %s reached, but service is busy, resetting idle time", serviceConfig.Name, idleTimeout)
			runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
		}
	})
	if interrupted {
		return nil, fmt.Errorf("interrupt signal was received")
	}
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

	sleepDuration := 1 * time.Microsecond
	maxSleep := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		if interrupted {
			return nil
		}

		conn, err := net.DialTimeout("tcp", net.JoinHostPort(serviceHost, servicePort), 1*time.Second)
		if err == nil {
			return conn
		}

		time.Sleep(sleepDuration)

		// Exponentially increase up to the maximum.
		sleepDuration *= 2
		if sleepDuration > maxSleep {
			sleepDuration = maxSleep
		}
	}

	log.Printf("[%s] Error: failed to connect to %s:%s: All connection attempts failed after trying for %s",
		serviceName, serviceHost, servicePort, timeout)
	return nil
}

func reserveResources(resourceRequirements map[string]int, requestingService string) bool {
	var resourceList []string
	if len(resourceRequirements) == 0 {
		return true
	}
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
	runningService, ok := resourceManager.maybeGetRunningService(serviceConfig.Name)
	if !ok {
		log.Printf("[%s] Warning, tried to track service usage, but couldn't find it in the list of running services, it was probably stopped", serviceConfig.Name)
		return
	}
	runningService.lastUsed = time.Now()
	if runningService.idleTimer != nil {
		runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
	}
	resourceManager.storeRunningService(serviceConfig.Name, runningService)
}

func canBeStopped(serviceName string) bool {
	//Using nolock version since both callers already lock the service mutex
	runningService, ok := resourceManager.maybeGetRunningServiceNoLock(serviceName)
	if !ok {
		log.Printf("[%s] Warning: A check whether service can be stopped failed to find the service in the running services list, it is probably already being stopped. Assuming it can't be stopped", serviceName)
		return false
	}
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

	args, err := shlex.Split(serviceConfig.Args)
	if err != nil {
		log.Printf("[%s] Failed to parse service arguments %s: %v", serviceConfig.Name, serviceConfig.Args, err)
		return nil
	}

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

func forwardConnection(clientConnection, serviceConnection net.Conn, serviceName string) {
	defer resourceManager.incrementConnection(serviceName, -1)
	resourceManager.incrementConnection(serviceName, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	var EOFOnWriteFromServerToClient *bool

	go func() {
		defer wg.Done()
		copyAndHandleErrors(
			serviceConnection,
			clientConnection,
			fmt.Sprintf("[%s] (service (%s) to client (%s))", serviceName, humanReadableConnection(serviceConnection), humanReadableConnection(clientConnection)),
		)

		if EOFOnWriteFromServerToClient == nil {
			EOFOnWriteFromServerToClient = new(bool)
			*EOFOnWriteFromServerToClient = true
		}
		// Once done copying client->service, close service side.
		err := serviceConnection.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("[%s] Error closing service to client connection: %v", serviceName, err)
		}
	}()
	go func() {
		defer wg.Done()
		copyAndHandleErrors(
			clientConnection,
			serviceConnection,
			fmt.Sprintf("[%s] (client (%s) to service (%s))", serviceName, humanReadableConnection(clientConnection), humanReadableConnection(serviceConnection)),
		)
		if EOFOnWriteFromServerToClient == nil {
			EOFOnWriteFromServerToClient = new(bool)
			*EOFOnWriteFromServerToClient = false
		}
		err := clientConnection.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("[%s] Error closing client to service connection: %v", serviceName, err)
		}
	}()
	wg.Wait()
	var reason string
	if *EOFOnWriteFromServerToClient {
		reason = "EOF on write from server to client"
	} else {
		reason = "EOF on write from client to server"
	}
	log.Printf(
		"[%s] Closed service and client connection %s, %s: %s",
		serviceName,
		humanReadableConnection(serviceConnection),
		humanReadableConnection(clientConnection),
		reason,
	)
}

func stopService(serviceName string) {
	runningService, ok := resourceManager.maybeGetRunningService(serviceName)
	if !ok {
		log.Printf("[%s] Warning: Failed to find a service in a list of running services while stopping it, probably multiple stops requested. Stop aborted.", serviceName)
		return
	}
	if interrupted {
		//If the process is being interrupted, we want to stop the service no matter what, even if it's currently locked
		runningService.manageMutex.TryLock()
	} else {
		runningService.manageMutex.Lock()
	}
	if runningService.idleTimer != nil {
		runningService.idleTimer.Stop()
	}
	if runningService.cmd != nil && runningService.cmd.Process != nil {
		log.Printf("[%s] Sending SIGTERM to service process group: -%d", serviceName, runningService.cmd.Process.Pid)
		err := syscall.Kill(-runningService.cmd.Process.Pid, syscall.SIGTERM)
		if err != nil {
			log.Printf("[%s] Failed to send SIGTERM to -%d: %v", serviceName, runningService.cmd.Process.Pid, err)
		}

		processExitedCleanly := waitForProcessToTerminate(runningService.cmd.Process)

		if !processExitedCleanly {
			log.Printf("[%s] Timed out waiting, sending SIGKILL to service process group -%d", serviceName, runningService.cmd.Process.Pid)
			err := syscall.Kill(-runningService.cmd.Process.Pid, syscall.SIGKILL)
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
	if !interrupted {
		runningService.manageMutex.Unlock()
	}
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
	//ErrClosed is not logged since it happens routinely when connection is closed without sending/receiving EOF
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("%s error during data transfer: %v", logPrefix, err)
	}
}

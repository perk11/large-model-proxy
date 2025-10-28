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
	manageMutex       *sync.Mutex
	cmd               *exec.Cmd
	activeConnections int
	lastUsed          *time.Time
	idleTimer         *time.Timer
	exitWaitGroup     *sync.WaitGroup
	resourcesReleased *bool
	stdoutWriter      *serviceLoggingWriter
	stderrWriter      *serviceLoggingWriter
}

type ResourceManager struct {
	serviceMutex       *sync.Mutex
	resourcesInUse     map[string]int
	resourcesAvailable map[string]int
	runningServices    map[string]RunningService
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
		if count > 0 {
			// Do not print this when decrementing, since it can happen if a service exited before connection was closed
			// which does not necessarily constitute a warning
			log.Printf("[%s] Warning: Tried to increment the number of active connection but couldn't get the running service, did it stop", name)
		}
		return
	}
	runningService.activeConnections += count
	rm.storeRunningServiceNoLock(name, runningService)
}

func (rm ResourceManager) createRunningService(serviceConfig ServiceConfig) RunningService {
	now := time.Now()
	rs := RunningService{
		activeConnections: 0,
		lastUsed:          &now,
		manageMutex:       &sync.Mutex{},
		resourcesReleased: new(bool),
	}
	rm.storeRunningService(serviceConfig.Name, rs)
	return rs
}

var (
	config              Config
	serviceConfigByName map[string]*ServiceConfig
	resourceManager     ResourceManager
	interrupted         = false
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	configFilePath := flag.String("c", "", "path to the config file. If not specified, will look for config.json or config.jsonc in the current directory")
	flag.Parse()

	if (*configFilePath) == "" {
		if _, err := os.Stat("config.json"); err == nil {
			*configFilePath = "config.json"
		} else if _, err := os.Stat("config.jsonc"); err == nil {
			*configFilePath = "config.jsonc"
		} else {
			FprintfError("Could not find config file. Please specify the path to the config file using the -c flag or create a config.jsonc file in the current directory\n")
			os.Exit(1)
		}
	}
	var err error
	config, err = loadConfig(*configFilePath)
	if err != nil {
		log.Printf("Error loading %s:\n", *configFilePath)
		FprintfError("%v\n", err)
		os.Exit(1)
	}

	serviceConfigByName = make(map[string]*ServiceConfig, len(config.Services))
	for serviceIndex := range config.Services {
		serviceConfigByName[config.Services[serviceIndex].Name] = &config.Services[serviceIndex]
	}

	resourceManager = ResourceManager{
		resourcesInUse:     make(map[string]int),
		resourcesAvailable: make(map[string]int),
		runningServices:    make(map[string]RunningService),
		serviceMutex:       &sync.Mutex{},
	}

	for name, resource := range config.ResourcesAvailable {
		resourceManager.resourcesAvailable[name] = resource.Amount
		go monitorResourceAvailability(
			name,
			resource.CheckCommand,
			time.Duration(resource.CheckIntervalMilliseconds)*time.Millisecond,
			&resourceManager,
		)
	}
	for _, service := range config.Services {
		if service.ListenPort != "" {
			go startProxy(service)
		}
	}
	if config.OpenAiApi.ListenPort != "" {
		go startOpenAiApi(config.OpenAiApi, config.Services)
	}
	if config.ManagementApi.ListenPort != "" {
		go startManagementApi(config.ManagementApi, config.Services)
	}

	for {
		receivedSignal := <-exit
		log.Printf("Received %s signal, terminating all processes", signalToString(receivedSignal))
		interrupted = true
		// no need to unlock as os.Exit will be called
		resourceManager.serviceMutex.Lock()
		for name := range resourceManager.runningServices {
			stopService(*findServiceConfigByName(name))
		}
		log.Printf("Done, exiting")
		os.Exit(0)
	}
}

func findServiceConfigByName(serviceName string) *ServiceConfig {
	if service, ok := serviceConfigByName[serviceName]; ok {
		return service
	}
	panic(fmt.Sprintf("Failed to find service config for service %s", serviceName))
}

func createOpenAiApiModel(name string, createdTime int64) OpenAiApiModel {
	return OpenAiApiModel{
		ID:      name,
		Object:  "model",
		OwnedBy: "large-model-proxy",
		Created: createdTime,
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
	startTime := time.Now().Unix()
	for _, service := range services {
		if !service.OpenAiApi {
			continue
		}
		// If the service doesn't define specific model names, assume the service name is the model
		if service.OpenAiApiModels == nil || len(service.OpenAiApiModels) == 0 {
			modelToServiceMap[service.Name] = service
			models = append(models, createOpenAiApiModel(service.Name, startTime))
		} else {
			for _, model := range service.OpenAiApiModels {
				modelToServiceMap[model] = service
				models = append(models, createOpenAiApiModel(model, startTime))
			}
		}
	}
	modelsResponse := OpenAiApiModels{
		Object: "models",
		Data:   models,
	}
	mux.HandleFunc("GET /v1/models/{model}", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
		requestedModelName := request.PathValue("model")
		modelFound := false
		for _, model := range modelsResponse.Data {
			if model.ID == requestedModelName {
				modelFound = true
				err := json.NewEncoder(responseWriter).Encode(model)
				if err != nil {
					http.Error(responseWriter, "{error: \"Failed to produce JSON response\"}", http.StatusInternalServerError)
					log.Printf("Failed to produce /v1/model/{model} JSON response: %s\n", err.Error())
				}
				break
			}
		}
		if !modelFound {
			responseWriter.WriteHeader(http.StatusNotFound)
			if err := json.NewEncoder(responseWriter).Encode(
				map[string]string{
					"error": fmt.Sprintf("Requested model \"%s\" not found", requestedModelName),
				}); err != nil {
				http.Error(responseWriter, "{error: \"Failed to produce not-found JSON\"}", http.StatusInternalServerError)
				log.Printf("Failed to produce not-found JSON for /v1/models/%s: %v\n", requestedModelName, err)
			}
			log.Printf("[OpenAI API Server] Model \"%s\" not found\n", requestedModelName)
		}
		resetConnectionBuffer(request)
	})
	mux.HandleFunc("/v1/models", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
		err := json.NewEncoder(responseWriter).Encode(modelsResponse)
		if err != nil {
			http.Error(responseWriter, "{error: \"Failed to produce JSON response\"}", http.StatusInternalServerError)
			log.Printf("[OpenAI API Server] Failed to produce /v1/models JSON response: %s\n", err.Error())
		}
		resetConnectionBuffer(request)
	})
	mux.HandleFunc("/v1/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		if !handleCompletions(responseWriter, request, &modelToServiceMap) {
			resetConnectionBuffer(request)
		}
	})
	mux.HandleFunc("/v1/chat/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		if !handleCompletions(responseWriter, request, &modelToServiceMap) {
			resetConnectionBuffer(request)
		}
	})
	mux.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		//404
		log.Printf("[OpenAI API Server] Request to unsupported URL: %s %s", request.Method, request.RequestURI)
		http.Error(
			responseWriter,
			fmt.Sprintf("%s %s is not supported by large-model-proxy", request.Method, request.RequestURI),
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
		log.Fatalf("[OpenAI API Server] Could not listen on %s: %v", server.Addr, err)
	}
	wrappedLn := &rawCaptureListener{Listener: ln}

	log.Printf("[OpenAI API Server] Listening on port %s", OpenAiApi.ListenPort)
	if err := server.Serve(wrappedLn); err != nil {
		log.Fatalf("Could not start OpenAI API Server: %s\n", err.Error())
	}
}
func printRequestUrl(request *http.Request) {
	log.Printf("[OpenAI API Server] %s %s", request.Method, request.URL)
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
			log.Printf("[OpenAI API Server] Error closing request body: %s\n", err.Error())
		}
	}(originalBody)
	//TODO: parse request directly
	bodyBytes, err := io.ReadAll(originalBody)
	if err != nil {
		log.Printf("[OpenAI API Server] Error reading request body: %v\n", err)
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
		log.Printf("[OpenAI API Server] Unknown model requested: %v\n", model)
		http.Error(responseWriter, fmt.Sprintf("Unknown model: %v", model), http.StatusBadRequest)
		return false
	}
	log.Printf("[OpenAI API Server] Sending %s request through to %s\n", request.URL, service.Name)
	originalWriter := responseWriter
	hijacker, ok := originalWriter.(http.Hijacker)
	if !ok {
		log.Printf("[OpenAI API Server] Error: Failed to forward connection: web server does not support hijacking. This could only happen if OpenAI API Server is running in HTTP/2 mode. Please use HTTP/1.1\n")
		http.Error(responseWriter, "Request forwarding is not possible, please use HTTP 1.1", http.StatusInternalServerError)
		return false
	}
	clientConnection, bufrw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("[OpenAI API Server] Failed to forward connection: %v", err)
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
			log.Printf("[OpenAI API Server] Error reading buffered data: : %v", err)
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
		log.Printf("[OpenAI API Server] Error decoding %s request: %v\n%s", url, err, bodyBytes)
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
			"failed to establish a connection to the service",
		)
		return
	}

	log.Printf("[%s] Opened service connection %s", serviceConfig.Name, humanReadableConnection(serviceConnection))
	trackServiceLastUsed(serviceConfig, true)

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

	trackServiceLastUsed(serviceConfig, false)
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
		trackServiceLastUsed(serviceConfig, true)
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
	return time.Duration(idleTimeout) * time.Second
}

func startService(serviceConfig ServiceConfig) (net.Conn, error) {
	runningService := resourceManager.createRunningService(serviceConfig)

	runningService.manageMutex.Lock()

	if !reserveResources(serviceConfig.ResourceRequirements, serviceConfig.Name) {
		delete(resourceManager.runningServices, serviceConfig.Name)
		runningService.manageMutex.Unlock()
		return nil, fmt.Errorf("insufficient resources %s", serviceConfig.Name)
	}

	cmd, outW, errW := runServiceCommand(serviceConfig)
	if cmd == nil {
		releaseResourcesWhenServiceMutexIsLocked(serviceConfig.ResourceRequirements)
		delete(resourceManager.runningServices, serviceConfig.Name)
		runningService.manageMutex.Unlock()
		return nil, fmt.Errorf("failed to run command \"%s %s\"", serviceConfig.Command, serviceConfig.Args)
	}
	runningService.cmd = cmd
	runningService.stdoutWriter = outW
	runningService.stderrWriter = errW

	runningService.exitWaitGroup = new(sync.WaitGroup)
	runningService.exitWaitGroup.Add(1)
	go monitorProcess(serviceConfig.Name, cmd.Process, runningService.exitWaitGroup)

	resourceManager.storeRunningService(serviceConfig.Name, runningService)
	var startupConnectionTimeout time.Duration
	if serviceConfig.StartupTimeoutMilliseconds == nil {
		startupConnectionTimeout = 10 * time.Minute
	} else {
		startupConnectionTimeout = time.Duration(*serviceConfig.StartupTimeoutMilliseconds) * time.Millisecond
	}
	giveUpTime := time.Now().Add(startupConnectionTimeout)
	err := performHealthCheck(serviceConfig, startupConnectionTimeout)
	if err != nil {
		log.Printf("[%s] Stopping service due to healthcheck error: %v", serviceConfig.Name, err)
		runningService.manageMutex.Unlock()
		stopService(serviceConfig)
		return nil, fmt.Errorf("healthcheck failed: %w", err)
	}
	log.Printf("[%s] Service started with pid %d", serviceConfig.Name, cmd.Process.Pid)
	if interrupted {
		return nil, fmt.Errorf("interrupt signal was received")
	}

	var serviceConnection, processExited = tryConnectingUntilTimeoutOrProcessExit(
		serviceConfig.ProxyTargetHost,
		serviceConfig.ProxyTargetPort,
		serviceConfig.Name,
		time.Until(giveUpTime),
		runningService.exitWaitGroup,
	)
	if serviceConnection == nil {
		if processExited {
			runningService.manageMutex.Unlock()
			return nil, fmt.Errorf("process terminated before a connection to the service could be established")
		}
		//This log has to happen before the mutex unlock to maintain a logical order of logs
		log.Printf("[%s] Failed to connect to %s:%s, stopping the service", serviceConfig.Name, serviceConfig.ProxyTargetHost, serviceConfig.ProxyTargetPort)
		runningService.manageMutex.Unlock()
		stopService(serviceConfig)
		return nil, fmt.Errorf("failed to connect to service")
	}

	defer runningService.manageMutex.Unlock()
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
			stopService(serviceConfig)
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
func performHealthCheck(serviceConfig ServiceConfig, timeout time.Duration) error {
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

	for {
		if interrupted {
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
				"healthcheck timed out, not starting another healthcheck command due to less time than %dms left out of %s",
				sleepDuration,
				timeout,
			)
		}
		if sleepDuration > 0 {
			time.Sleep(sleepDuration)
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
				stopService(serviceConfig)
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
func tryConnectingUntilTimeoutOrProcessExit(
	serviceHost string,
	servicePort string,
	serviceName string,
	timeout time.Duration,
	processExitWaitGroup *sync.WaitGroup,
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
		default:
		}
		if interrupted {
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

func reserveResources(resourceRequirements map[string]int, requestingService string) bool {
	var resourceList []string
	if len(resourceRequirements) == 0 {
		return true
	}
	for resource, amount := range resourceRequirements {
		resourceList = append(resourceList, fmt.Sprintf("%s: %d", resource, amount))
	}
	log.Printf("[%s] Attempting to reserve %s", requestingService, strings.Join(resourceList, ", "))

	var missingResource *string = nil
	var maxWaitTime time.Duration
	if config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds == nil {
		maxWaitTime = 120 * time.Second
	} else {
		maxWaitTime = time.Duration(*config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds) * time.Second
	}
	startTime := time.Now()
	var iteration = 0
	const logOutputIterationFrequency = 60
	for time.Since(startTime) < maxWaitTime {
		resourceManager.serviceMutex.Lock()
		missingResource = findFirstMissingResourceWhenServiceMutexIsLocked(resourceRequirements, requestingService, iteration%logOutputIterationFrequency == 0)
		iteration++
		if missingResource == nil {
			for resource, amount := range resourceRequirements {
				resourceManager.resourcesInUse[resource] += amount
			}
			resourceManager.serviceMutex.Unlock()
			return true
		}
		resourceManager.serviceMutex.Unlock()
		earliestLastUsedService := findEarliestLastUsedServiceUsingResource(requestingService, *missingResource)
		if earliestLastUsedService != "" {
			log.Printf("[%s] Stopping service to free resources for %s", earliestLastUsedService, requestingService)
			stopService(*findServiceConfigByName(earliestLastUsedService))
			continue
		}

		if iteration == 1 {
			log.Printf("[%s] Failed to find a service to stop; will check every 1s.", requestingService)
		} else if iteration%logOutputIterationFrequency == 0 {
			log.Printf("[%s] Failed to find a service to stop; continuing to check every 1s.", requestingService)
		}

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

	for serviceName := range resourceManager.runningServices {
		if serviceName == requestingService {
			continue
		}
		serviceConfig := findServiceConfigByName(serviceName)
		if serviceConfig.ResourceRequirements[missingResource] == 0 {
			continue
		}
		if !canBeStopped(serviceName) {
			continue
		}
		lastUsed := resourceManager.runningServices[serviceName].lastUsed
		if lastUsed != nil {
			timeDifference := lastUsed.Sub(earliestTime)
			if timeDifference < 0 {
				earliestLastUsedService = serviceName
				earliestTime = *lastUsed
			}
		}
	}

	return earliestLastUsedService
}

func findFirstMissingResourceWhenServiceMutexIsLocked(resourceRequirements map[string]int, requestingService string, outputError bool) *string {
	for resource, amount := range resourceRequirements {
		if resourceManager.resourcesInUse[resource]+amount > resourceManager.resourcesAvailable[resource] {
			if outputError {
				log.Printf(
					"[%s] Not enough %s to start. Total: %d, In use: %d, Required: %d",
					requestingService,
					resource,
					resourceManager.resourcesAvailable[resource],
					resourceManager.resourcesInUse[resource],
					amount,
				)
			}
			return &resource
		}
	}
	return nil
}

func trackServiceLastUsed(serviceConfig ServiceConfig, runningServiceMustExist bool) {
	runningService, ok := resourceManager.maybeGetRunningService(serviceConfig.Name)
	if !ok {
		if runningServiceMustExist {
			log.Printf("[%s] Warning: Tried to track service usage, but couldn't find it in the list of running services, it was probably stopped", serviceConfig.Name)
		}
		return
	}
	now := time.Now()
	runningService.lastUsed = &now
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

func releaseResourcesWhenServiceMutexIsLocked(used map[string]int) {
	for resource, amount := range used {
		resourceManager.resourcesInUse[resource] -= amount
	}
}

type serviceLoggingWriter struct {
	prefix string
	logger *log.Logger
	buf    []byte // holds an incomplete line between Write calls
}

func (w *serviceLoggingWriter) FinalFlush() {
	if w == nil || len(w.buf) == 0 {
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

func stopService(service ServiceConfig) {
	runningService, ok := resourceManager.maybeGetRunningService(service.Name)
	if !ok {
		log.Printf("[%s] Warning: Failed to find a service in a list of running services while stopping it, multiple stops requested or service already died. Stop aborted.", service.Name)
		return
	}
	if interrupted {
		//If the process is being interrupted, we want to stop the service no matter what, even if it's currently locked
		runningService.manageMutex.TryLock()
	} else {
		runningService.manageMutex.Lock()
		defer runningService.manageMutex.Unlock()
	}
	if runningService.idleTimer != nil {
		runningService.idleTimer.Stop()
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
	if !interrupted && !*runningService.resourcesReleased {
		resourceManager.serviceMutex.Lock()
		cleanUpStoppedServiceWhenServiceMutexIsLocked(&service, runningService, true)
		resourceManager.serviceMutex.Unlock()
	}
}
func monitorProcess(serviceName string, process *os.Process, exitWaitGroup *sync.WaitGroup) {
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
	defer func() {
		log.Print(exitMessage)
		exitWaitGroup.Done()
	}()
	if interrupted {
		if resourceManager.serviceMutex.TryLock() {
			defer resourceManager.serviceMutex.Unlock()
		} else {
			log.Printf("[%s] Not cleaning up resources due to large-model-proxy being interrupted", serviceName)
			return
		}
	} else {
		resourceManager.serviceMutex.Lock()
		defer resourceManager.serviceMutex.Unlock()
	}

	runningService, ok := resourceManager.maybeGetRunningServiceNoLock(serviceName)
	if !ok {
		log.Printf("[%s] Process exited, but service was not found in the list of running services, this is probably a bug", serviceName)
		return
	}

	service := findServiceConfigByName(serviceName)
	cleanUpStoppedServiceWhenServiceMutexIsLocked(service, runningService, *service.ConsiderStoppedOnProcessExit)
}

func cleanUpStoppedServiceWhenServiceMutexIsLocked(service *ServiceConfig, runningService RunningService, shouldReleaseResources bool) {
	if !shouldReleaseResources || *runningService.resourcesReleased {
		return
	}
	*runningService.resourcesReleased = true
	if runningService.idleTimer != nil {
		runningService.idleTimer.Stop()
	}
	runningService.stdoutWriter.FinalFlush()
	runningService.stderrWriter.FinalFlush()
	releaseResourcesWhenServiceMutexIsLocked(service.ResourceRequirements)
	delete(resourceManager.runningServices, service.Name)
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

func copyAndHandleErrors(dst io.Writer, src io.Reader, logPrefix string) {
	_, err := io.Copy(dst, src)
	//ErrClosed is not logged since it happens routinely when connection is closed without sending/receiving EOF
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("%s error during data transfer: %v", logPrefix, err)
	}
}

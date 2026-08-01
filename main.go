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
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/shlex"
)

type RunningService struct {
	manageMutex           *channelMutex
	cmd                   *exec.Cmd
	isWaitingForResources bool
	isReady               bool
	lastUsed              *time.Time
	idleTimer             *time.Timer
	exitWaitGroup         *sync.WaitGroup
	resourcesReleased     *bool
	resourcesReserved     bool
	stdoutWriter          *serviceLoggingWriter
	stderrWriter          *serviceLoggingWriter
}
type ServiceConnectionStats struct {
	proxied int
	waiting int
}
type ResourceManager struct {
	serviceMutex      *sync.Mutex    //reads and writes to resourcesInUse, resourcesReserved, runningServices. Never lock while connectionStatsMutex is locked
	resourcesInUse    map[string]int // used by services that are currently starting or running
	runningServices   map[string]*RunningService
	resourcesReserved map[string]int // used by services that are currently starting but have not yet passed the health check

	resourcesAvailableMutex *sync.Mutex
	resourcesAvailable      map[string]int // if CheckCommand is used, the result returned by CheckCommand. Otherwise, unused

	connectionStatsMutex *sync.Mutex //never lock serviceMutex when this is locked
	connectionStats      map[string]ServiceConnectionStats

	monitorUnpauseChansMutex *sync.Mutex
	monitorUnpauseChans      map[string]chan struct{} // writing on this channel makes monitor run the command immediately

	resourceChangeByResourceMutex          *sync.Mutex //Also covers checkCommandFirstChangeByResourceChans
	checkCommandFirstChangeByResourceChans map[string]map[string]chan struct{}
	resourceChangeByResourceChans          map[string]map[string]chan bool // value sent down the channel is the next "firstCheckNeeded" for the waiter: true = cached amount may be stale, re-run CheckCommand; false = amount was just updated by the monitor, trust the cache
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

// maybeGetRunningService looks up a running service under serviceMutex. During
// shutdown it TryLocks instead of blocking (the stop loop holds serviceMutex),
// reporting the service as gone if the lock cannot be acquired rather than
// reading the map unsynchronized. The shutdown path bypasses this via
// stopRunningService.
func (rm ResourceManager) maybeGetRunningService(name string) (*RunningService, bool) {
	if interrupted.Load() {
		if rm.serviceMutex.TryLock() {
			defer rm.serviceMutex.Unlock()
		} else {
			// Shutting down and the lock is held by the stop loop: do not read
			// the map without the lock.
			return nil, false
		}
	} else {
		rm.serviceMutex.Lock()
		defer rm.serviceMutex.Unlock()
	}
	rs, ok := rm.runningServices[name]
	return rs, ok
}

func (rm ResourceManager) incrementConnection(name string, proxiedConnectionsCountChange int, waitingConnectionsCountChange int) {
	rm.connectionStatsMutex.Lock()
	connectionStats := rm.connectionStats[name]
	connectionStats.proxied += proxiedConnectionsCountChange
	connectionStats.waiting += waitingConnectionsCountChange
	if connectionStats.proxied < 0 || connectionStats.waiting < 0 {
		log.Printf("[%s] ERROR: Negative connection count. Proxied: %d. Waiting: %d. Clamping to 0.", name, connectionStats.proxied, connectionStats.waiting)
		if connectionStats.proxied < 0 {
			connectionStats.proxied = 0
		}
		if connectionStats.waiting < 0 {
			connectionStats.waiting = 0
		}
	}
	rm.connectionStats[name] = connectionStats
	rm.connectionStatsMutex.Unlock()

	if connectionStats.proxied == 0 && connectionStats.waiting == 0 {
		if config.LogLevel == LogLevelDebug {
			log.Printf("[%s] All connections closed, sending resourceChange event", name)
		}
		if serviceConfig, ok := serviceConfigByName[name]; ok && serviceConfig != nil {
			rm.broadcastResourceChanges(maps.Keys(serviceConfig.ResourceRequirements), true)
		}
	}
}

var (
	config              Config
	serviceConfigByName map[string]*ServiceConfig
	resourceManager     ResourceManager
	interrupted         atomic.Bool
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
	resourceManager = ResourceManager{
		resourcesInUse:                         make(map[string]int, len(config.ResourcesAvailable)),
		resourcesReserved:                      make(map[string]int, len(config.ResourcesAvailable)),
		resourcesAvailable:                     make(map[string]int, len(config.ResourcesAvailable)),
		runningServices:                        make(map[string]*RunningService),
		serviceMutex:                           &sync.Mutex{},
		resourcesAvailableMutex:                &sync.Mutex{},
		resourceChangeByResourceMutex:          &sync.Mutex{},
		monitorUnpauseChansMutex:               &sync.Mutex{},
		connectionStatsMutex:                   &sync.Mutex{},
		connectionStats:                        make(map[string]ServiceConnectionStats, len(config.Services)),
		monitorUnpauseChans:                    make(map[string]chan struct{}),
		checkCommandFirstChangeByResourceChans: make(map[string]map[string]chan struct{}),
		resourceChangeByResourceChans:          make(map[string]map[string]chan bool, len(config.ResourcesAvailable)),
	}

	serviceConfigByName = make(map[string]*ServiceConfig, len(config.Services))
	for serviceIndex := range config.Services {
		serviceName := config.Services[serviceIndex].Name
		serviceConfigByName[serviceName] = &config.Services[serviceIndex]
		resourceManager.connectionStats[serviceName] = ServiceConnectionStats{proxied: 0, waiting: 0}
	}

	for name, resource := range config.ResourcesAvailable {
		resourceManager.resourcesAvailable[name] = 0
		resourceManager.resourcesInUse[name] = 0
		resourceManager.resourcesReserved[name] = 0
		resourceManager.resourceChangeByResourceChans[name] = make(map[string]chan bool)

		if resource.CheckCommand != "" {
			resourceManager.monitorUnpauseChans[name] = make(chan struct{}, 1) // capacity 1: see sendSignalToChannels — UnpauseResourceAvailabilityMonitoring sends non-blocking, so a signal arriving before the monitor re-enters its receive must be buffered, not dropped
			resourceManager.checkCommandFirstChangeByResourceChans[name] = make(map[string]chan struct{})
			go monitorResourceAvailability(
				name,
				resource.CheckCommand,
				time.Duration(resource.CheckWhenNotEnoughIntervalMilliseconds)*time.Millisecond,
				resourceManager.monitorUnpauseChans[name],
				&resourceManager,
			)
		}
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
		interrupted.Store(true)
		// Stop services directly from the held map: stopService -> maybeGetRunningService
		// would re-TryLock serviceMutex (non-reentrant) and abort every stop. The map is
		// frozen during shutdown (cleanUp is skipped), so ranging it under the lock is safe.
		// no need to unlock as os.Exit will be called
		resourceManager.serviceMutex.Lock()
		for name, runningService := range resourceManager.runningServices {
			stopRunningService(*findServiceConfigByName(name), runningService)
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
	mutex   sync.Mutex
	buffer  *bytes.Buffer
	capture bool
}

func (rcc *rawCaptureConnection) Read(p []byte) (int, error) {
	n, err := rcc.Conn.Read(p)
	if n > 0 {
		rcc.mutex.Lock()
		if rcc.capture {
			rcc.buffer.Write(p[:n])
		}
		rcc.mutex.Unlock()
	}
	return n, err
}

// stopBuffering disables further capturing into the buffer and drops the buffer.
// It is called once the raw request bytes have been extracted for replaying to
// the backend, so the live client->service stream that follows is not pointlessly
// duplicated into a buffer that is never read again.
func (rcc *rawCaptureConnection) stopBuffering() {
	rcc.mutex.Lock()
	rcc.capture = false
	rcc.buffer = nil
	rcc.mutex.Unlock()
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
		Conn:    connection,
		buffer:  new(bytes.Buffer),
		capture: true,
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
		if len(service.OpenAiApiModels) == 0 {
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
	rawConnection.mutex.Lock()
	rawConnection.buffer = new(bytes.Buffer)
	rawConnection.capture = true
	rawConnection.mutex.Unlock()
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
	clientConnection, _, err := hijacker.Hijack()
	if err != nil {
		log.Printf("[OpenAI API Server] Failed to forward connection: %v", err)
		http.Error(responseWriter, err.Error(), http.StatusInternalServerError)
		return false
	}
	rawConnection, ok := request.Context().Value(rawConnectionContextKey).(*rawCaptureConnection)
	if !ok {
		panic("Failed to get raw connection")
	}
	rawRequestBytes := rawConnection.buffer.Bytes()
	rawConnection.stopBuffering()
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
		if interrupted.Load() {
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

// bufferedPipe is an in-memory io.Pipe-like reader/writer pair backed by a
// BOUNDED buffer. Once the accumulated backlog reaches the configured cap, Write
// blocks (true backpressure) until a Read frees space, rather than growing the
// buffer without limit. This re-engages TCP flow control on the client during
// service startup (or any window where the consumer is slow to drain), bounding
// the proxy's RAM in the face of a large upload or a hostile peer.
//
// Unlike io.Pipe, the monitor can still keep issuing Read on the client
// connection (and thereby detect a client disconnect/EOF) while a bounded backlog
// is outstanding — that is the whole reason bufferedPipe exists rather than a
// plain io.Pipe, whose Write would stall the moment the read end is unconsumed
// and so block the monitor until startup completes, defeating prompt client-disconnect detection. The
// tradeoff is now BOUNDED rather than unbounded: request bytes are still buffered
// and preserved for the consumer, but only up to the cap. A limit of 0
// restores the old unbounded/no-backpressure behavior as an operator escape hatch.
type bufferedPipe struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	limit      uint64 // max bytes buffered before Write blocks; 0 => unbounded
	writerDone bool   // set by closeWrite; once set, Reads return io.EOF after the buffer drains
	closed     bool   // read end closed by the consumer via closeRead
	cond       *sync.Cond
}

func newBufferedPipe(limit uint64) *bufferedPipe {
	bp := &bufferedPipe{limit: limit}
	bp.cond = sync.NewCond(&bp.mu)
	return bp
}

// Write appends to the internal buffer, applying backpressure once the accumulated
// backlog reaches the cap. A write into an empty buffer is always allowed (even if
// larger than the cap) so a single large request body still flows; the cap bounds
// the buffered backlog, not any individual message. It blocks until there is room.
func (b *bufferedPipe) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Apply backpressure once the accumulated buffered data reaches the cap. A
	// write into an empty buffer is always allowed (even if larger than the cap)
	// so a single large request body still flows — the cap bounds the buffered
	// backlog, not any individual message. limit == 0 means unbounded.
	for b.limit > 0 && b.buf.Len() > 0 && uint64(b.buf.Len())+uint64(len(p)) > b.limit {
		if b.closed {
			return 0, io.ErrClosedPipe
		}
		b.cond.Wait()
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	n, _ := b.buf.Write(p) // bytes.Buffer.Write never returns an error
	b.cond.Broadcast()
	return n, nil
}

// closeWrite marks the writer as done. Subsequent Reads, after draining any
// buffered data, return io.EOF regardless of err (so partially-buffered request
// bytes are still delivered before EOF). This MUST be tracked with a dedicated
// flag rather than err != nil: io.Copy returns a nil error on a clean EOF, so
// gating on err would leave Read blocked forever after a normal client close
// (which would stall forwardConnection and leak the proxied-connection count).
func (b *bufferedPipe) closeWrite(_ error) {
	b.mu.Lock()
	b.writerDone = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// closeRead closes the read end: in-flight and future Writes return io.ErrClosedPipe
// so the producing goroutine stops.
func (b *bufferedPipe) closeRead() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *bufferedPipe) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 {
		if b.writerDone {
			return 0, io.EOF // writer done and nothing left buffered
		}
		if b.closed {
			return 0, io.ErrClosedPipe
		}
		b.cond.Wait()
	}
	n, _ := b.buf.Read(p)
	b.cond.Broadcast() // wake any Write blocked on backpressure now that room freed up
	return n, nil
}

// defaultClientRequestBufferLimitBytes bounds how many unread client→service
// request bytes the proxy will buffer in RAM during service startup (or whenever
// the service is slow to consume). Beyond this the monitor's Write blocks,
// re-applying TCP backpressure on the client instead of buffering unbounded data.
const defaultClientRequestBufferLimitBytes = 16 << 20 // 16 MiB

// startClientReadMonitor takes ownership of reading from clientConnection for the
// whole lifetime of a proxied connection. All bytes read from the client are
// available through the returned reader, so no request data is lost. The
// returned channel is closed as soon as the client's read side reports an error
// (i.e. the client disconnected), which lets waiting code abort promptly instead
// of holding a stale "waiting" connection count. The returned close func closes
// the reader end of the pipe and must be invoked when the caller is done; it
// unblocks the monitor goroutine if it is stuck writing to a full pipe.
//
// The reader/writer pair is a bufferedPipe rather than an io.Pipe: io.Pipe's Write
// blocks until a Read consumes the data, but the read end is only consumed by
// forwardConnection, which does not start until after service startup. With an
// io.Pipe, a client that sends any request bytes during startup (the common case
// for client-speaks-first protocols) would stall the monitor inside Write, stop
// reading the client connection, and thus fail to detect the client's disconnect
// until maxWait/startup timeout — defeating prompt client-disconnect detection. A bufferedPipe keeps
// reading the client and observes the disconnect promptly, while still preserving
// the buffered request bytes for the consumer. Unlike the old
// unbounded version, its buffer is now capped (default 16 MiB,
// ClientRequestBufferLimitBytes; 0 restores unbounded behavior), so Write only
// blocks once the backlog reaches that cap rather than growing RAM without limit.
func startClientReadMonitor(clientConnection net.Conn) (clientReader io.Reader, closeReader func(), disconnected <-chan struct{}) {
	limit := uint64(defaultClientRequestBufferLimitBytes)
	if config.ClientRequestBufferLimitBytes != nil {
		limit = uint64(*config.ClientRequestBufferLimitBytes) // 0 => unbounded (operator escape hatch)
	}
	pipe := newBufferedPipe(limit)
	disconnectedChan := make(chan struct{})
	go func() {
		_, err := io.Copy(pipe, clientConnection) // pipe.Write blocks only past the cap, so we keep reading the client → detect disconnect
		pipe.closeWrite(err)
		close(disconnectedChan)
	}()
	var once sync.Once
	return pipe, func() {
		once.Do(func() { pipe.closeRead() })
	}, disconnectedChan
}

func handleConnection(clientConnection net.Conn, serviceConfig ServiceConfig, dataToSendToServiceBeforeForwardingFromClient []byte) {
	if interrupted.Load() {
		_ = clientConnection.Close()
		return
	}
	clientReader, closeClientReader, clientDisconnected := startClientReadMonitor(clientConnection)
	defer closeClientReader()

	resourceManager.incrementConnection(serviceConfig.Name, 0, 1)
	serviceConnection := startServiceIfNotAlreadyRunningAndConnect(serviceConfig, clientDisconnected)

	if serviceConnection == nil {
		resourceManager.incrementConnection(serviceConfig.Name, 0, -1)
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
	resourceManager.incrementConnection(serviceConfig.Name, 1, -1)
	defer resourceManager.incrementConnection(serviceConfig.Name, -1, 0)

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
	forwardConnection(clientReader, clientConnection, serviceConnection, serviceConfig.Name)

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

// channelMutex is a mutex backed by a buffered channel of capacity 1. Unlike
// sync.Mutex it supports cancellation via a select (e.g. a client disconnect)
// without spawning a goroutine or polling: LockOrCancel blocks on the token
// channel and is woken the instant the token is returned, or bails out the
// instant the cancel channel closes.
type channelMutex struct {
	token chan struct{}
}

func newChannelMutex() *channelMutex {
	m := &channelMutex{token: make(chan struct{}, 1)}
	m.token <- struct{}{} // start unlocked
	return m
}

func (m *channelMutex) TryLock() bool {
	select {
	case <-m.token:
		return true
	default:
		return false
	}
}

func (m *channelMutex) Lock() {
	<-m.token
}

// LockOrCancel blocks until the lock is acquired or cancel is closed.
// Returns true if acquired, false if cancelled (lock not held).
func (m *channelMutex) LockOrCancel(cancel <-chan struct{}) bool {
	select {
	case <-m.token:
		return true
	case <-cancel:
		return false
	}
}

func (m *channelMutex) Unlock() {
	m.token <- struct{}{}
}

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

func reserveResources(resourceRequirements map[string]int, requestingService string, clientDisconnected <-chan struct{}) bool {
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
	maxWaitTimeTimer := time.NewTimer(maxWaitTime)
	defer maxWaitTimeTimer.Stop()
	recheckNeeded := true
	var pendingCheckChannels map[string]chan struct{}
	for {
		resourceManager.serviceMutex.Lock()
		missingResource, pendingCheckChannels = findFirstMissingResourceWhenServiceMutexIsLocked(resourceRequirements, requestingService, true, recheckNeeded)
		if missingResource == nil && len(pendingCheckChannels) == 0 {
			// Resources are actually available
			resourceManager.resourceChangeByResourceMutex.Lock()
			for resource := range resourceRequirements {
				delete(resourceManager.resourceChangeByResourceChans[resource], requestingService)
			}
			resourceManager.resourceChangeByResourceMutex.Unlock()
			for resource, amount := range resourceRequirements {
				resourceManager.resourcesInUse[resource] += amount
				resourceManager.resourcesReserved[resource] += amount
			}
			log.Printf("[%s] Reserved %s", requestingService, strings.Join(resourceList, ", "))
			resourceManager.serviceMutex.Unlock()
			return true
		}
		resourceManager.serviceMutex.Unlock()

		// Some resources are backed by a CheckCommand and need a fresh measurement
		// before availability can be decided. Wait for the monitor to run those
		// commands WITHOUT holding serviceMutex: a CheckCommand can be slow (or even
		// hang), and holding the global lock for its duration would block every
		// other operation (service start/stop, /status, process cleanup, ...). The
		// maxWaitTime deadline and client disconnection are honored here too.
		if len(pendingCheckChannels) > 0 {
			if !waitForFirstCheckCommands(pendingCheckChannels, requestingService, maxWaitTime, maxWaitTimeTimer, clientDisconnected) {
				return false
			}
			// The cached resource amounts are now fresh, so use them directly on the
			// next pass instead of forcing another CheckCommand run.
			recheckNeeded = false
			continue
		}

		earliestLastUsedService := findEarliestLastUsedServiceUsingResource(requestingService, *missingResource)
		if earliestLastUsedService != "" {
			log.Printf("[%s] Stopping service to free resources for %s", earliestLastUsedService, requestingService)
			stopService(*findServiceConfigByName(earliestLastUsedService))
			// The stopped service may have changed the resource state (e.g. an exit
			// script), so the cached amount is now stale: force a fresh CheckCommand
			// run on the next pass instead of trusting the cache.
			recheckNeeded = true
			continue
		}
		log.Printf("[%s] Not enough %s to start and no services eligible to stop. Waiting until enough resources are free or a service using a resource can be stopped.", requestingService, *missingResource)

		// Buffered (size 1): broadcastResourceChanges -> sendSignalToChannels sends
		// non-blocking, so a signal arriving in the window between registering
		// this channel and entering the select below must be buffered rather than
		// dropped — otherwise the waiter hangs until maxWaitTime and a service can
		// fail to start after a peer frees resources. See sendSignalToChannels and
		// the "should-not-use-an-outdated-resource-check-result" regression test.
		resourceChangeServiceChannel := make(chan bool, 1)
		resourceManager.resourceChangeByResourceMutex.Lock()
		if _, ok := resourceManager.resourceChangeByResourceChans[*missingResource][requestingService]; ok {
			log.Printf("[%s] ERROR: Resource %s is already being reserved by this service", requestingService, *missingResource)
		}
		resourceManager.resourceChangeByResourceChans[*missingResource][requestingService] = resourceChangeServiceChannel
		resourceManager.resourceChangeByResourceMutex.Unlock()

		select {
		case recheckNeeded = <-resourceChangeServiceChannel:
			resourceManager.resourceChangeByResourceMutex.Lock()
			delete(resourceManager.resourceChangeByResourceChans[*missingResource], requestingService)
			resourceManager.resourceChangeByResourceMutex.Unlock()

			log.Printf("[%s] Received a resource change event for %s, rechecking if the service can be started now", requestingService, *missingResource)

			// resource state changed; loop to re-evaluate
		case <-maxWaitTimeTimer.C:
			resourceManager.resourceChangeByResourceMutex.Lock()
			delete(resourceManager.resourceChangeByResourceChans[*missingResource], requestingService)
			resourceManager.resourceChangeByResourceMutex.Unlock()
			log.Printf("[%s] Failed to find a service to stop in %v, closing client connection", requestingService, maxWaitTime)
			return false
		case <-clientDisconnected:
			resourceManager.resourceChangeByResourceMutex.Lock()
			delete(resourceManager.resourceChangeByResourceChans[*missingResource], requestingService)
			resourceManager.resourceChangeByResourceMutex.Unlock()
			log.Printf("[%s] Client disconnected while waiting for resources, aborting", requestingService)
			return false
		}
	}
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
		runningService := resourceManager.runningServices[serviceName]
		if !canBeStopped(serviceName, runningService) {
			continue
		}
		lastUsed := runningService.lastUsed
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

func findFirstMissingResourceWhenServiceMutexIsLocked(
	resourceRequirements map[string]int,
	requestingService string,
	outputError bool,
	firstCheckNeeded bool,
) (missingResource *string, pendingCheckChannels map[string]chan struct{}) {
	// pendingCheckChannels collects the "first change" channels registered for
	// resources backed by a CheckCommand when a fresh measurement is needed. They
	// are returned to the caller, which waits on them WITHOUT holding
	// serviceMutex (see reserveResources / waitForFirstCheckCommands).
	pendingCheckChannels = make(map[string]chan struct{})

	// First evaluate the statically-tracked resources. If any of them is the
	// bottleneck, return it WITHOUT registering any CheckCommand first-change
	// channels — registering those (and Unpause-ing the monitor) would force a
	// wasteful CheckCommand run for a service that cannot start anyway because of
	// a static shortage.
	for resource, requiredAmount := range resourceRequirements {
		resourceConfig := config.ResourcesAvailable[resource]
		if resourceConfig.CheckCommand != "" {
			continue
		}
		inUseAmount, ok := resourceManager.resourcesInUse[resource]
		if !ok {
			log.Printf(
				"[%s] ERROR: Resource \"%s\" is missing from the list of the resources in use. This shouldn't be happening",
				requestingService,
				resource,
			)
			inUseAmount = 0
		}
		totalAvailableAmount := config.ResourcesAvailable[resource].Amount
		if requiredAmount > totalAvailableAmount-inUseAmount {
			handleNotEnoughResource(requestingService, outputError, false, resource, 0, requiredAmount)
			return &resource, nil
		}
	}

	// All static resources are satisfied; now handle CheckCommand-backed resources.
	for resource, requiredAmount := range resourceRequirements {
		resourceConfig := config.ResourcesAvailable[resource]
		if resourceConfig.CheckCommand == "" {
			continue
		}
		if firstCheckNeeded {
			newChannel := make(chan struct{}, 1) // capacity 1: see sendSignalToChannels — the monitor broadcasts the first CheckCommand result non-blocking, so a result arriving between registration and waitForFirstCheckCommands' receive must be buffered, not dropped
			pendingCheckChannels[resource] = newChannel
			resourceManager.resourceChangeByResourceMutex.Lock()
			resourceManager.checkCommandFirstChangeByResourceChans[resource][requestingService] = newChannel
			resourceManager.resourceChangeByResourceMutex.Unlock()
			UnpauseResourceAvailabilityMonitoring(resource)
		} else {
			currentlyAvailableAmount, enoughOfResource :=
				isEnoughResourceForServiceWithCheckCommandThatRan(
					resource,
					requestingService,
					requiredAmount,
				)
			if !enoughOfResource {
				handleNotEnoughResource(requestingService, outputError, true, resource, currentlyAvailableAmount, requiredAmount)
				return &resource, nil
			}
		}
	}
	return nil, pendingCheckChannels
}

// waitForFirstCheckCommands waits for the resource monitor to run the CheckCommand
// for every resource in pendingCheckChannels, WITHOUT holding serviceMutex. This
// keeps the global lock free while a (potentially slow) CheckCommand executes, and
// honors the overall reservation deadline (maxWaitTimeTimer) and client
// disconnection. It always cleans up its channel registrations before returning.
func waitForFirstCheckCommands(
	pendingCheckChannels map[string]chan struct{},
	requestingService string,
	maxWaitTime time.Duration,
	maxWaitTimeTimer *time.Timer,
	clientDisconnected <-chan struct{},
) bool {
	for _, changeChan := range pendingCheckChannels {
		select {
		case <-changeChan:
			// Monitor ran the CheckCommand for this resource.
		case <-maxWaitTimeTimer.C:
			cleanupFirstChangeChannels(pendingCheckChannels, requestingService)
			log.Printf("[%s] Failed to confirm enough resources in %v, closing client connection", requestingService, maxWaitTime)
			return false
		case <-clientDisconnected:
			cleanupFirstChangeChannels(pendingCheckChannels, requestingService)
			log.Printf("[%s] Client disconnected while waiting for resource checks, aborting", requestingService)
			return false
		}
	}
	// All requested checks ran; drop the registrations so the monitor stops
	// signalling them. The monitor's timer remains armed for one more interval
	// from the last check, which covers the brief window before this service
	// either reserves or registers for resource-change notifications.
	cleanupFirstChangeChannels(pendingCheckChannels, requestingService)
	return true
}

// cleanupFirstChangeChannels removes this service's "first change" channel
// registrations so the monitor stops signalling them.
func cleanupFirstChangeChannels(pendingCheckChannels map[string]chan struct{}, requestingService string) {
	resourceManager.resourceChangeByResourceMutex.Lock()
	for resource := range pendingCheckChannels {
		delete(resourceManager.checkCommandFirstChangeByResourceChans[resource], requestingService)
	}
	resourceManager.resourceChangeByResourceMutex.Unlock()
}

func handleNotEnoughResource(requestingService string, outputError bool, currentlyAvailableAmountIsMeasured bool, resource string, currentlyAvailableAmount int, requiredAmount int) {
	resourceManager.resourceChangeByResourceMutex.Lock()
	//This might be not necessary if there is no CheckCommand
	for _, resourceToCheck := range resourceManager.checkCommandFirstChangeByResourceChans {
		delete(resourceToCheck, requestingService)
	}
	resourceManager.resourceChangeByResourceMutex.Unlock()
	if outputError {
		if currentlyAvailableAmountIsMeasured {
			log.Printf(
				"[%s] Not enough %s to start. Total: %d, Available: %d, Reserved by starting services: %d, Required: %d",
				requestingService,
				resource,
				config.ResourcesAvailable[resource].Amount,
				currentlyAvailableAmount,
				resourceManager.resourcesReserved[resource],
				requiredAmount,
			)
		} else {
			log.Printf(
				"[%s] Not enough %s to start. Total: %d, Reserved by running services: %d, Required: %d",
				requestingService,
				resource,
				config.ResourcesAvailable[resource].Amount,
				resourceManager.resourcesInUse[resource],
				requiredAmount,
			)
		}
	}
}

func isEnoughResourceForServiceWithCheckCommandThatRan(resource string, requestingService string, requiredAmount int) (int, bool) {
	// Use resources reserved instead of used for the calculation as we only need
	// to account for the services that are being started, the ones that already started
	// are accounted for by the check command.
	resourceManager.resourcesAvailableMutex.Lock()
	currentlyAvailableAmount, currentlyAvailableAmountIsMeasured := resourceManager.resourcesAvailable[resource]
	resourceManager.resourcesAvailableMutex.Unlock()
	if !currentlyAvailableAmountIsMeasured {
		log.Printf(
			"[%s] ERROR: Resource \"%s\" is missing from the list of the available resources. This shouldn't be happening",
			requestingService,
			resource,
		)
		currentlyAvailableAmount = 0
	}
	var ok bool
	reservedAmount, ok := resourceManager.resourcesReserved[resource]
	if !ok {
		log.Printf(
			"[%s] ERROR: Resource \"%s\" is missing from the list of the reserved resources. This shouldn't be happening",
			requestingService,
			resource,
		)
		reservedAmount = 0
	}
	enoughOfResource := requiredAmount <= currentlyAvailableAmount-reservedAmount
	return currentlyAvailableAmount, enoughOfResource
}

func trackServiceLastUsed(serviceConfig ServiceConfig, runningServiceMustExist bool) {
	runningService, ok := resourceManager.maybeGetRunningService(serviceConfig.Name)
	if !ok {
		if runningServiceMustExist {
			log.Printf("[%s] Warning: Tried to track service usage, but couldn't find it in the list of running services, it was probably stopped", serviceConfig.Name)
		}
		return
	}
	resourceManager.serviceMutex.Lock()
	now := time.Now()
	runningService.lastUsed = &now
	if runningService.idleTimer != nil {
		runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
	}
	resourceManager.serviceMutex.Unlock()
}

func canBeStopped(serviceName string, runningService *RunningService) bool {
	if !runningService.manageMutex.TryLock() {
		return false
	}

	defer runningService.manageMutex.Unlock()
	resourceManager.connectionStatsMutex.Lock()
	defer resourceManager.connectionStatsMutex.Unlock()
	connectionStats := resourceManager.connectionStats[serviceName]
	return connectionStats.proxied == 0
}

func releaseResourcesWhenServiceMutexIsLocked(used map[string]int) {
	for resource, amount := range used {
		resourceManager.resourcesInUse[resource] -= amount
	}
}
func releaseReservedResources(reserved map[string]int) {
	resourceManager.serviceMutex.Lock()
	releaseReservedResourcesWhenServiceMutexIsLocked(reserved)
	resourceManager.serviceMutex.Unlock()
}
func releaseReservedResourcesWhenServiceMutexIsLocked(reserved map[string]int) {
	for resource, amount := range reserved {
		resourceManager.resourcesReserved[resource] -= amount
	}
}

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

func forwardConnection(clientReader io.Reader, clientConnection, serviceConnection net.Conn, serviceName string) {
	var wg sync.WaitGroup
	wg.Add(2)
	var EOFOnWriteFromServerToClient *bool

	go func() {
		defer wg.Done()
		copyAndHandleErrors(
			serviceConnection,
			clientReader,
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

func copyAndHandleErrors(dst io.Writer, src io.Reader, logPrefix string) {
	_, err := io.Copy(dst, src)
	//ErrClosed is not logged since it happens routinely when connection is closed without sending/receiving EOF
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("%s error during data transfer: %v", logPrefix, err)
	}
}

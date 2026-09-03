package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// setupRoutedConnectionTestGlobals prepares the package-level state that
// handleRoutedConnection touches (connection stats, service configs). These
// tests deliberately do not run in parallel because they configure globals.
func setupRoutedConnectionTestGlobals(t *testing.T, services []ServiceConfig) {
	t.Helper()
	config = Config{LogLevel: LogLevelNormal}
	serviceConfigByName = make(map[string]*ServiceConfig, len(services))
	connectionStats := make(map[string]ServiceConnectionStats, len(services))
	runningServices := make(map[string]*RunningService, len(services))
	now := time.Now()
	for i := range services {
		serviceConfigByName[services[i].Name] = &services[i]
		connectionStats[services[i].Name] = ServiceConnectionStats{}
		runningServices[services[i].Name] = &RunningService{manageMutex: newChannelMutex(), lastUsed: &now}
	}
	resourceManager = ResourceManager{
		serviceMutex:                           &sync.Mutex{},
		connectionStatsMutex:                   &sync.Mutex{},
		connectionStats:                        connectionStats,
		runningServices:                        runningServices,
		resourcesInUse:                         map[string]int{},
		resourcesReserved:                      map[string]int{},
		resourcesAvailable:                     map[string]int{},
		resourcesAvailableMutex:                &sync.Mutex{},
		monitorUnpauseChansMutex:               &sync.Mutex{},
		monitorUnpauseChans:                    map[string]chan struct{}{},
		resourceChangeByResourceMutex:          &sync.Mutex{},
		checkCommandFirstChangeByResourceChans: map[string]map[string]chan struct{}{},
		resourceChangeByResourceChans:          map[string]map[string]chan bool{},
	}
}

type recordingBackend struct {
	server *httptest.Server
	hits   atomic.Int64
	marker string
}

// newRecordingBackend starts an HTTP backend that responds to any request with
// a body naming which backend served it, so tests can tell where a request was
// routed. Keep-alive is enabled, like real LLM backends.
func newRecordingBackend(marker string) *recordingBackend {
	backend := &recordingBackend{marker: marker}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.hits.Add(1)
		_, _ = fmt.Fprintf(w, "served-by:%s", marker)
	}))
	return backend
}

func (b *recordingBackend) address() string {
	return b.server.Listener.Addr().String()
}

func (b *recordingBackend) close() {
	b.server.Close()
}

func connectToRecordingBackend(backends map[string]*recordingBackend) func(ServiceConfig, <-chan struct{}) net.Conn {
	return func(serviceConfig ServiceConfig, _ <-chan struct{}) net.Conn {
		backend, found := backends[serviceConfig.Name]
		if !found {
			return nil
		}
		connection, err := net.Dial("tcp", backend.address())
		if err != nil {
			return nil
		}
		return connection
	}
}

// clientServerConnectionPair creates a real TCP connection pair: the server
// side is meant to be handed to the proxy, the client side is returned to the
// test to act as the client.
func clientServerConnectionPair(t *testing.T) (clientSide net.Conn, serverSide net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverConnections := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			serverConnections <- connection
		}
	}()

	clientConnection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConnection.Close() })

	select {
	case serverConnection := <-serverConnections:
		t.Cleanup(func() { _ = serverConnection.Close() })
		return clientConnection, serverConnection
	case <-time.After(5 * time.Second):
		t.Fatalf("server side of the connection pair was not accepted")
		return nil, nil
	}
}

// startRoutedHandler runs the routed connection handler and registers a
// cleanup that makes sure the handler (and its goroutines) has fully exited
// before the next test reinitializes package-level state.
func startRoutedHandler(t *testing.T, serverConnection net.Conn, router *contextRouter, connect func(ServiceConfig, <-chan struct{}) net.Conn) {
	t.Helper()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handleRoutedConnection(serverConnection, router, connect)
	}()
	t.Cleanup(func() {
		_ = serverConnection.Close()
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Errorf("routed connection handler did not exit within 10s after connection close")
		}
	})
}

func writeRequestToConnection(t *testing.T, connection net.Conn, body string) {
	t.Helper()
	request := testChatRequest("", body)
	if _, err := connection.Write(request); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
}

func readResponseFromConnection(t *testing.T, connection net.Conn) (int, string) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return response.StatusCode, string(body)
}

func readResponseFromConnectionForBody(t *testing.T, connection net.Conn) string {
	t.Helper()
	_, body := readResponseFromConnection(t, connection)
	return body
}

func waitForConnectionStats(t *testing.T, expected ServiceConnectionStats, timeout time.Duration, serviceNames ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resourceManager.connectionStatsMutex.Lock()
		allMatch := true
		for _, name := range serviceNames {
			if resourceManager.connectionStats[name] != expected {
				allMatch = false
			}
		}
		resourceManager.connectionStatsMutex.Unlock()
		if allMatch {
			return
		}
		if time.Now().After(deadline) {
			resourceManager.connectionStatsMutex.Lock()
			current := make(map[string]ServiceConnectionStats, len(serviceNames))
			for _, name := range serviceNames {
				current[name] = resourceManager.connectionStats[name]
			}
			resourceManager.connectionStatsMutex.Unlock()
			t.Fatalf("connection stats did not settle on %v within %s, got %v", expected, timeout, current)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testTokenRouterServices() []ServiceConfig {
	return []ServiceConfig{
		{Name: "large", ContextSize: uintPtr(1000), Tokenizer: "qwen3.8"},
		{Name: "small", ContextSize: uintPtr(10), Tokenizer: "qwen3.8"},
	}
}

// "hi" is 1 token; the string below is 30 words => 30 tokens with qwen3.8
const smallRequestContent = "hi"

func largeRequestContent() string {
	return strings.Repeat("word ", 30)
}

func TestRoutedConnectionRoutesFirstRequestToSmallestFittingTier(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+smallRequestContent+`"}]}`)
	_, body := readResponseFromConnection(t, clientConnection)

	assert.Equal(t, "served-by:small", body)
	assert.Equal(t, int64(1), backends["small"].hits.Load())
	assert.Equal(t, int64(0), backends["large"].hits.Load())

	_ = clientConnection.Close()
	waitForConnectionStats(t, ServiceConnectionStats{}, 10*time.Second, "small", "large")
}

func TestRoutedConnectionRoutesOversizedRequestToLargestTier(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+largeRequestContent()+`"}]}`)
	_, body := readResponseFromConnection(t, clientConnection)

	assert.Equal(t, "served-by:large", body)
	assert.Equal(t, int64(0), backends["small"].hits.Load())
	assert.Equal(t, int64(1), backends["large"].hits.Load())
}

func TestRoutedConnectionSwitchesToLargerTierOnSameConnection(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+smallRequestContent+`"}]}`)
	assert.Equal(t, "served-by:small", readResponseFromConnectionForBody(t, clientConnection))

	// The conversation grew past the small tier's limit: the same connection
	// must be re-routed to the large tier for the next request.
	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+largeRequestContent()+`"}]}`)
	assert.Equal(t, "served-by:large", readResponseFromConnectionForBody(t, clientConnection))

	assert.Equal(t, int64(1), backends["small"].hits.Load())
	assert.Equal(t, int64(1), backends["large"].hits.Load())

	_ = clientConnection.Close()
	waitForConnectionStats(t, ServiceConnectionStats{}, 10*time.Second, "small", "large")
}

func TestRoutedConnectionDoesNotSwitchDownMidConnection(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	// First request goes to the large tier...
	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+largeRequestContent()+`"}]}`)
	assert.Equal(t, "served-by:large", readResponseFromConnectionForBody(t, clientConnection))
	// ...a small follow-up request must stay there to avoid thrashing services.
	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+smallRequestContent+`"}]}`)
	assert.Equal(t, "served-by:large", readResponseFromConnectionForBody(t, clientConnection))

	assert.Equal(t, int64(0), backends["small"].hits.Load())
	assert.Equal(t, int64(2), backends["large"].hits.Load())
}

func TestRoutedConnectionPassthroughRoutesToSmallestTier(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	// Not HTTP: cannot be framed, must be blindly forwarded to the smallest tier
	_, err := clientConnection.Write([]byte("PROXY custom handshake\r\n"))
	assert.NoError(t, err)
	statusCode, body := readResponseFromConnection(t, clientConnection)

	// The HTTP backend answers the malformed request with 400 without invoking
	// the handler, proving the bytes were forwarded to the small service (the
	// large one is never hit; a malformed request never reaches a handler, so
	// the hit counters cannot be used here).
	assert.Equal(t, http.StatusBadRequest, statusCode)
	assert.NotEmpty(t, body)
	assert.Equal(t, int64(0), backends["large"].hits.Load())
}

func TestRoutedConnectionBytesModeSwitchesOnByteCount(t *testing.T) {
	services := []ServiceConfig{
		{Name: "large", ContextSizeBytes: uintPtr(1000)},
		{Name: "small", ContextSizeBytes: uintPtr(10)},
	}
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"short"}]}`)
	assert.Equal(t, "served-by:small", readResponseFromConnectionForBody(t, clientConnection))

	// 50 bytes of content > 10 byte limit of the small tier
	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 50)+`"}]}`)
	assert.Equal(t, "served-by:large", readResponseFromConnectionForBody(t, clientConnection))
}

func TestRoutedConnectionClientDisconnectDuringSwitchAborts(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
	}
	t.Cleanup(func() { backends["small"].close() })

	connect := func(serviceConfig ServiceConfig, clientDisconnected <-chan struct{}) net.Conn {
		if serviceConfig.Name == "small" {
			return connectToRecordingBackend(backends)(serviceConfig, clientDisconnected)
		}
		// Simulate a slow-to-start large service: it only "finishes starting"
		// when the client goes away, then fails.
		<-clientDisconnected
		return nil
	}

	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connect)

	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+smallRequestContent+`"}]}`)
	assert.Equal(t, "served-by:small", readResponseFromConnectionForBody(t, clientConnection))

	// Oversized request triggers a switch; client gives up while waiting.
	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"`+largeRequestContent()+`"}]}`)
	_ = clientConnection.Close()

	waitForConnectionStats(t, ServiceConnectionStats{}, 10*time.Second, "small", "large")
}

func TestRoutedConnectionInitialRequestTimeoutFallsBackToSmallestTier(t *testing.T) {
	previousTimeout := contextRoutingInitialRequestTimeout
	contextRoutingInitialRequestTimeout = 150 * time.Millisecond
	t.Cleanup(func() { contextRoutingInitialRequestTimeout = previousTimeout })

	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	// Connect but send nothing until after the initial routing timeout, then
	// send a normal request: it must be forwarded blind to the smallest tier.
	time.Sleep(300 * time.Millisecond)
	writeRequestToConnection(t, clientConnection, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	statusCode, body := readResponseFromConnection(t, clientConnection)

	assert.Equal(t, http.StatusOK, statusCode)
	assert.Equal(t, "served-by:small", body)
	assert.Equal(t, int64(1), backends["small"].hits.Load())
	assert.Equal(t, int64(0), backends["large"].hits.Load())
}

func TestRouteInitialRequestUsesLargestOfInitialBurst(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	router := buildContextRouter(services)

	// A client that pipelines several requests in one burst (legal, though rare,
	// in HTTP/1.1): the routing decision must account for every request already
	// completed in the burst, so the connection starts on the tier that fits
	// the largest of them instead of switching immediately after.
	smallRequest := testChatRequest("", `{"model":"m","messages":[{"role":"user","content":"`+smallRequestContent+`"}]}`)
	largeRequest := testChatRequest("", `{"model":"m","messages":[{"role":"user","content":"`+largeRequestContent()+`"}]}`)
	burst := append(append([]byte{}, smallRequest...), largeRequest...)
	clientChunks := make(chan []byte, 1)
	clientChunks <- burst

	decision, decided := routeInitialRequest(router, clientChunks, make(chan struct{}))

	assert.True(t, decided)
	assert.Equal(t, 1, decision.tierIndex, "the 30-token request must pick the large tier")
	assert.Len(t, decision.requests, 2, "both pipelined requests must be forwarded")
	assert.Equal(t, uint64(31), countRequestUnits(decision.requests[1], router))
}

func TestRoutedConnectionContentLengthIsUsedForUnits(t *testing.T) {
	services := testTokenRouterServices()
	setupRoutedConnectionTestGlobals(t, services)
	backends := map[string]*recordingBackend{
		"small": newRecordingBackend("small"),
		"large": newRecordingBackend("large"),
	}
	t.Cleanup(func() {
		backends["small"].close()
		backends["large"].close()
	})
	router := buildContextRouter(services)
	clientConnection, serverConnection := clientServerConnectionPair(t)
	startRoutedHandler(t, serverConnection, router, connectToRecordingBackend(backends))

	// A chunked request whose content exceeds the small tier
	chunkedRequest := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n" +
		strconv.FormatInt(int64(len(largeRequestContent())), 16) + "\r\n" + largeRequestContent() + "\r\n" +
		"0\r\n\r\n"
	_, err := clientConnection.Write([]byte(chunkedRequest))
	assert.NoError(t, err)

	assert.Equal(t, "served-by:large", readResponseFromConnectionForBody(t, clientConnection))
}

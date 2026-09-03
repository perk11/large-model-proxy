package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// contextRoutingInitialRequestTimeout is how long a connection on a
// context-routed port waits for the first complete HTTP request before
// falling back to blind forwarding to the smallest tier. Clients speak first
// in HTTP, so this only fires for probes or non-HTTP traffic. It is a variable
// so tests can shorten it.
var contextRoutingInitialRequestTimeout = 60 * time.Second

// contextUnitMode distinguishes token-based context sizes from raw-byte-based
// ones within a group of services sharing a listen port.
type contextUnitMode int

const (
	contextUnitsTokens contextUnitMode = iota
	contextUnitsBytes
)

// contextTier is one service of a shared-port group together with the context
// size it can serve.
type contextTier struct {
	serviceConfig ServiceConfig
	limitUnits    uint64 // in tokens or bytes, depending on the router's mode
}

// contextRouter routes client requests to one of several services that share a
// listen port, preferring the service with the smallest context size that still
// fits the request. Config validation guarantees that all services in a group
// use the same unit mode and, for token mode, the same tokenizer.
type contextRouter struct {
	tiers        []contextTier // sorted ascending by limitUnits
	unitMode     contextUnitMode
	tokenCounter TokenCounter // nil in byte mode
}

func buildContextRouter(services []ServiceConfig) *contextRouter {
	router := &contextRouter{}
	if len(services) == 0 {
		return router
	}
	if services[0].ContextSizeBytes != nil {
		router.unitMode = contextUnitsBytes
	} else {
		router.unitMode = contextUnitsTokens
		router.tokenCounter, _ = GetTokenCounter(services[0].Tokenizer)
	}
	for _, serviceConfig := range services {
		limit := uint64(0)
		if serviceConfig.ContextSizeBytes != nil {
			limit = uint64(*serviceConfig.ContextSizeBytes)
		} else if serviceConfig.ContextSize != nil {
			limit = uint64(*serviceConfig.ContextSize)
		}
		router.tiers = append(router.tiers, contextTier{serviceConfig: serviceConfig, limitUnits: limit})
	}
	sort.Slice(router.tiers, func(i, j int) bool {
		if router.tiers[i].limitUnits != router.tiers[j].limitUnits {
			return router.tiers[i].limitUnits < router.tiers[j].limitUnits
		}
		return router.tiers[i].serviceConfig.Name < router.tiers[j].serviceConfig.Name
	})
	return router
}

// selectTierIndex returns the index of the smallest tier whose limit fits the
// given number of units. When nothing fits, the largest tier is returned: the
// request is oversized for every configured service, so the largest context
// gives it the best chance (and the backend reports the error if it cannot).
func (r *contextRouter) selectTierIndex(units uint64) int {
	for index, tier := range r.tiers {
		if units <= tier.limitUnits {
			return index
		}
	}
	return len(r.tiers) - 1
}

// countRequestUnits measures how many units of context (tokens or bytes,
// depending on the router's mode) one complete HTTP request occupies. Only the
// text content of the request body is counted (chat messages, prompts,
// embeddings input); when the body is not recognizable JSON, the raw body is
// measured as a fallback.
func countRequestUnits(requestBytes []byte, router *contextRouter) uint64 {
	body := extractRequestBody(requestBytes)
	texts := extractRequestTexts(body)
	if len(texts) == 0 {
		if len(body) == 0 {
			return 0
		}
		texts = []string{string(body)}
	}

	total := uint64(0)
	for _, text := range texts {
		if router.unitMode == contextUnitsBytes {
			total += uint64(len(text))
		} else {
			total += uint64(router.tokenCounter(text))
		}
	}
	return total
}

func extractRequestBody(requestBytes []byte) []byte {
	terminatorOffset, terminatorLength := findHeaderTerminator(requestBytes)
	if terminatorOffset < 0 {
		return requestBytes
	}
	return requestBytes[terminatorOffset+terminatorLength:]
}

// extractRequestTexts pulls the transcribed text out of an OpenAI-compatible
// request body. It supports messages[].content (string or content-part arrays),
// prompt (string or array) and input (string or array, used by /v1/embeddings).
func extractRequestTexts(body []byte) []string {
	var payload struct {
		Messages []json.RawMessage `json:"messages"`
		Prompt   json.RawMessage   `json:"prompt"`
		Input    json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	var texts []string
	for _, message := range payload.Messages {
		var messageContent struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(message, &messageContent); err != nil {
			return nil
		}
		texts = append(texts, jsonStringOrArrayTexts(messageContent.Content)...)
	}
	texts = append(texts, jsonStringOrArrayTexts(payload.Prompt)...)
	texts = append(texts, jsonStringOrArrayTexts(payload.Input)...)
	return texts
}

// jsonStringOrArrayTexts extracts text from a JSON value that is either a
// string, an array of strings, or an array of content parts
// ({"type":"text","text":...}); non-text parts are skipped.
func jsonStringOrArrayTexts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []string{asString}
	}
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err != nil {
		return nil
	}
	var texts []string
	for _, element := range asArray {
		var asString string
		if err := json.Unmarshal(element, &asString); err == nil {
			texts = append(texts, asString)
			continue
		}
		var contentPart struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(element, &contentPart); err == nil && contentPart.Type == "text" {
			texts = append(texts, contentPart.Text)
		}
	}
	return texts
}

// startContextRoutedProxy listens on one port shared by several services and
// routes every client connection through the context router.
func startContextRoutedProxy(listenPort string, router *contextRouter) {
	serviceNames := make([]string, len(router.tiers))
	descriptions := make([]string, len(router.tiers))
	for i, tier := range router.tiers {
		serviceNames[i] = tier.serviceConfig.Name
		descriptions[i] = fmt.Sprintf("%s (%s)", tier.serviceConfig.Name, tier.limitDescription())
	}
	listener, err := net.Listen("tcp", ":"+listenPort)
	if err != nil {
		log.Fatalf("[%s] Fatal error: cannot listen on port %s: %v", joinStrings(serviceNames, ","), listenPort, err)
	}
	defer func(listener net.Listener) {
		_ = listener.Close()
	}(listener)
	log.Printf("[port %s] Listening with context-based routing: %s", listenPort, joinStrings(descriptions, " < "))

	for {
		if interrupted.Load() {
			return
		}
		clientConnection, err := listener.Accept()
		if err != nil {
			if interrupted.Load() {
				return
			}
			log.Printf("[port %s] Error accepting connection: %v", listenPort, err)
			continue
		}
		log.Printf("[port %s] New client connection received %s", listenPort, humanReadableConnection(clientConnection))
		go handleRoutedConnection(clientConnection, router, startServiceIfNotAlreadyRunningAndConnect)
	}
}

// limitDescription renders a tier's context size for logs.
func (t contextTier) limitDescription() string {
	if t.serviceConfig.ContextSizeBytes != nil {
		return fmt.Sprintf("%d bytes", t.limitUnits)
	}
	return fmt.Sprintf("%d tokens", t.limitUnits)
}

// handleRoutedConnection proxies one client connection through the context
// router. The first complete HTTP request decides which tier serves the
// connection (if several requests complete in the very first burst, the
// largest one decides). Later requests on the same connection that no longer
// fit the current tier cause an in-flight switch to the smallest tier that
// fits: the current service connection is closed, the new service is started
// (reusing all the existing on-demand startup machinery), and forwarding
// continues on a new service connection. Switching only ever goes upwards
// within a connection: a later small request stays on the current tier so
// that auxiliary requests (model listings, health probes) cannot thrash
// services.
//
// The client -> service direction runs in this goroutine; the service ->
// client direction runs in one copier goroutine that survives service
// switches. Requests are only ever committed to a service once fully
// received, so a switch never loses or duplicates request bytes. Switching
// assumes request/response lockstep — a client never sends request N+1
// before fully reading response N — which every real HTTP client honors;
// HTTP/1.1 pipelining clients are not supported across a switch (a switch
// may cut a pipelined response that is still in flight).
func handleRoutedConnection(clientConnection net.Conn, router *contextRouter, connectToService func(ServiceConfig, <-chan struct{}) net.Conn) {
	if interrupted.Load() {
		_ = clientConnection.Close()
		return
	}
	clientReader, closeClientReader, clientDisconnected := startClientReadMonitor(clientConnection)
	defer closeClientReader()

	// Pump client bytes into a channel so the routing loop can select on data
	// arrival, client disconnects, and the initial routing timeout at once.
	clientChunks := make(chan []byte)
	stopPump := make(chan struct{})
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			bytesRead, readErr := clientReader.Read(buffer)
			if bytesRead > 0 {
				chunk := make([]byte, bytesRead)
				copy(chunk, buffer[:bytesRead])
				select {
				case clientChunks <- chunk:
				case <-stopPump:
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
	defer close(stopPump)

	decision, decided := routeInitialRequest(router, clientChunks, clientDisconnected)
	if !decided {
		_ = clientConnection.Close()
		return
	}

	currentTierIndex := decision.tierIndex
	currentServiceConfig := router.tiers[currentTierIndex].serviceConfig

	resourceManager.incrementConnection(currentServiceConfig.Name, 0, 1)
	serviceConnection := connectToService(currentServiceConfig, clientDisconnected)
	if serviceConnection == nil {
		resourceManager.incrementConnection(currentServiceConfig.Name, 0, -1)
		closeConnectionAndHandleError(
			clientConnection,
			currentServiceConfig,
			"client",
			"failed to establish a connection to the service",
		)
		return
	}
	log.Printf("[%s] Routing connection %s to service (context size %s)", currentServiceConfig.Name, humanReadableConnection(clientConnection), router.tiers[currentTierIndex].limitDescription())
	trackServiceLastUsed(currentServiceConfig, true)
	resourceManager.incrementConnection(currentServiceConfig.Name, 1, -1)

	// The copier forwards service -> client and keeps working across service
	// switches. When a service connection ends, it closes the client
	// connection unless a switch is in progress.
	routerDone := make(chan struct{})
	copierDone := make(chan struct{})
	nextServiceConnection := make(chan net.Conn, 1)
	switchCoordinator := struct {
		mutex       sync.Mutex
		swapPending bool
	}{}
	go func(activeConnection net.Conn) {
		defer close(copierDone)
		for {
			_, err := io.Copy(clientConnection, activeConnection)
			if err == nil || isConnectionClosedError(err) {
				// The service connection ended. Find out whether that was our
				// own switch or the end of the road for this client.
				switchCoordinator.mutex.Lock()
				swapPending := switchCoordinator.swapPending
				switchCoordinator.mutex.Unlock()
				if !swapPending {
					_ = clientConnection.Close()
					return
				}
			} else {
				// e.g. the client is gone; nothing more to deliver
				_ = clientConnection.Close()
				return
			}
			select {
			case activeConnection = <-nextServiceConnection:
			case <-routerDone:
				_ = clientConnection.Close()
				return
			case <-clientDisconnected:
				return
			}
		}
	}(serviceConnection)

	connectionCounted := true
	// Deferred teardown, registered in reverse execution order: the pump stops
	// first, then stats are released, the service connection is closed so the
	// copier's io.Copy unblocks, the copier finishes before the client
	// connection is closed so pending response bytes are not lost, and the
	// client read monitor is shut down last (it is idempotent).
	defer closeClientReader()
	defer func() { _ = clientConnection.Close() }()
	defer func() { close(routerDone); <-copierDone }()
	defer func() { _ = serviceConnection.Close() }()
	defer func() {
		if connectionCounted {
			resourceManager.incrementConnection(currentServiceConfig.Name, -1, 0)
			trackServiceLastUsed(currentServiceConfig, false)
		}
	}()

	writeToService := func(data []byte) bool {
		if len(data) == 0 {
			return true
		}
		if _, err := serviceConnection.Write(data); err != nil {
			log.Printf("[%s] Error writing request bytes to service: %v", currentServiceConfig.Name, err)
			return false
		}
		return true
	}

	switchToTier := func(targetTierIndex int, units uint64) (net.Conn, bool) {
		previousServiceConfig := currentServiceConfig
		// Signal the copier BEFORE closing: its in-flight io.Copy must not
		// treat the close as end-of-stream for the client.
		switchCoordinator.mutex.Lock()
		switchCoordinator.swapPending = true
		switchCoordinator.mutex.Unlock()
		_ = serviceConnection.Close()
		resourceManager.incrementConnection(previousServiceConfig.Name, -1, 0)
		trackServiceLastUsed(previousServiceConfig, false)

		newServiceConfig := router.tiers[targetTierIndex].serviceConfig
		log.Printf("[%s] Request of %s exceeds context size of %s, switching to %s (context size %s)",
			previousServiceConfig.Name, router.unitDescription(units), previousServiceConfig.Name, newServiceConfig.Name, router.tiers[targetTierIndex].limitDescription())
		resourceManager.incrementConnection(newServiceConfig.Name, 0, 1)
		newConnection := connectToService(newServiceConfig, clientDisconnected)
		if newConnection == nil {
			resourceManager.incrementConnection(newServiceConfig.Name, 0, -1)
			connectionCounted = false
			return nil, false
		}
		trackServiceLastUsed(newServiceConfig, true)
		resourceManager.incrementConnection(newServiceConfig.Name, 1, -1)
		currentServiceConfig = newServiceConfig
		currentTierIndex = targetTierIndex
		nextServiceConnection <- newConnection
		return newConnection, true
	}

	for _, request := range decision.requests {
		if !writeToService(request) {
			return
		}
	}
	passthroughMode := decision.passthrough
	if passthroughMode && !writeToService(decision.throughBytes) {
		return
	}

	splitter := decision.splitter
	for {
		select {
		case chunk, ok := <-clientChunks:
			if !ok {
				// Client finished sending: deliver any partially received
				// request, then close the service side of the connection.
				if !passthroughMode {
					writeToService(splitter.FlushIncomplete())
				}
				resourceManager.incrementConnection(currentServiceConfig.Name, -1, 0)
				trackServiceLastUsed(currentServiceConfig, false)
				connectionCounted = false
				_ = serviceConnection.Close()
				return
			}
			if passthroughMode {
				if !writeToService(chunk) {
					return
				}
				continue
			}
			result := splitter.Write(chunk)
			for _, request := range result.completedRequests {
				units := countRequestUnits(request, router)
				targetTierIndex := router.selectTierIndex(units)
				if targetTierIndex > currentTierIndex {
					newConnection, switched := switchToTier(targetTierIndex, units)
					if !switched {
						return
					}
					serviceConnection = newConnection
				}
				if !writeToService(request) {
					return
				}
			}
			if result.enteredPassthrough {
				passthroughMode = true
				if !writeToService(result.throughBytes) {
					return
				}
			}
		case <-clientDisconnected:
			// The client is gone; teardown happens in the deferred cleanup and
			// the copier exits on its own.
			return
		}
	}
}

func isConnectionClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || strings.Contains(err.Error(), "connection reset")
}

// initialRoutingDecision captures where a connection should start and what
// already-received bytes must be forwarded to the chosen service.
type initialRoutingDecision struct {
	tierIndex    int
	requests     [][]byte // complete requests received before the decision
	passthrough  bool
	throughBytes []byte // bytes to forward before streaming on (passthrough only)
	splitter     *httpRequestSplitter
}

// routeInitialRequest waits for the first complete HTTP request (or a reason
// to give up on framing) and computes the tier that should serve the
// connection. It returns decided=false when the client went away before any
// routing decision could be made.
func routeInitialRequest(router *contextRouter, clientChunks <-chan []byte, clientDisconnected <-chan struct{}) (initialRoutingDecision, bool) {
	splitter := newHttpRequestSplitter()
	initialTimeout := time.After(contextRoutingInitialRequestTimeout)

	for {
		select {
		case chunk, ok := <-clientChunks:
			if !ok {
				return initialRoutingDecision{}, false
			}
			result := splitter.Write(chunk)
			if len(result.completedRequests) > 0 {
				largestUnits := uint64(0)
				for _, request := range result.completedRequests {
					units := countRequestUnits(request, router)
					if units > largestUnits {
						largestUnits = units
					}
				}
				return initialRoutingDecision{
					tierIndex:    router.selectTierIndex(largestUnits),
					requests:     result.completedRequests,
					passthrough:  result.enteredPassthrough,
					throughBytes: result.throughBytes,
					splitter:     splitter,
				}, true
			}
			if result.enteredPassthrough {
				return initialRoutingDecision{
					tierIndex:    0,
					passthrough:  true,
					throughBytes: result.throughBytes,
					splitter:     splitter,
				}, true
			}
		case <-clientDisconnected:
			return initialRoutingDecision{}, false
		case <-initialTimeout:
			// Nothing framable arrived: behave like a plain proxy to the
			// smallest tier and forward whatever was received so far.
			return initialRoutingDecision{
				tierIndex:    0,
				passthrough:  true,
				throughBytes: splitter.FlushIncomplete(),
				splitter:     splitter,
			}, true
		}
	}
}

// unitDescription renders a request size for logs.
func (r *contextRouter) unitDescription(units uint64) string {
	if r.unitMode == contextUnitsBytes {
		return fmt.Sprintf("%d bytes", units)
	}
	return fmt.Sprintf("%d tokens", units)
}

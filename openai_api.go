package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

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

func createOpenAiApiModel(name string, createdTime int64) OpenAiApiModel {
	return OpenAiApiModel{
		ID:      name,
		Object:  "model",
		OwnedBy: "large-model-proxy",
		Created: createdTime,
	}
}

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
	})
	mux.HandleFunc("/v1/models", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
		err := json.NewEncoder(responseWriter).Encode(modelsResponse)
		if err != nil {
			http.Error(responseWriter, "{error: \"Failed to produce JSON response\"}", http.StatusInternalServerError)
			log.Printf("[OpenAI API Server] Failed to produce /v1/models JSON response: %s\n", err.Error())
		}
	})
	mux.HandleFunc("/v1/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		handleCompletions(responseWriter, request, &modelToServiceMap)
	})
	mux.HandleFunc("/v1/chat/completions", func(responseWriter http.ResponseWriter, request *http.Request) {
		printRequestUrl(request)
		handleCompletions(responseWriter, request, &modelToServiceMap)
	})
	mux.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		//404
		log.Printf("[OpenAI API Server] Request to unsupported URL: %s %s", request.Method, request.RequestURI)
		http.Error(
			responseWriter,
			fmt.Sprintf("%s %s is not supported by large-model-proxy", request.Method, request.RequestURI),
			http.StatusNotFound,
		)
	})

	var clientConnections sync.Map
	server := &http.Server{
		Addr:    ":" + OpenAiApi.ListenPort,
		Handler: mux,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			clientConnection := &openAiApiClientConnection{transportsByServiceName: make(map[string]*http.Transport)}
			clientConnections.Store(connection, clientConnection)
			return context.WithValue(ctx, openAiApiClientConnectionContextKey, clientConnection)
		},
		ConnState: func(connection net.Conn, state http.ConnState) {
			if state != http.StateClosed && state != http.StateHijacked {
				return
			}
			if clientConnection, ok := clientConnections.LoadAndDelete(connection); ok {
				clientConnection.(*openAiApiClientConnection).closeServiceConnections()
			}
		},
	}

	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatalf("[OpenAI API Server] Could not listen on %s: %v", server.Addr, err)
	}

	log.Printf("[OpenAI API Server] Listening on port %s", OpenAiApi.ListenPort)
	if err := server.Serve(ln); err != nil {
		log.Fatalf("Could not start OpenAI API Server: %s\n", err.Error())
	}
}
func printRequestUrl(request *http.Request) {
	log.Printf("[OpenAI API Server] %s %s", request.Method, request.URL)
}

func handleCompletions(responseWriter http.ResponseWriter, request *http.Request, modelToServiceMap *map[string]ServiceConfig) {
	if request.Method != http.MethodPost {
		http.Error(responseWriter, "Only POST requests allowed", http.StatusBadRequest)
		return
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
		return
	}

	model, ok := extractModelFromRequest(request.URL.String(), bodyBytes)
	if !ok {
		http.Error(responseWriter, fmt.Sprintf("Failed to parse request: %v", err), http.StatusBadRequest)
		return
	}

	service, ok := (*modelToServiceMap)[model]
	if !ok {
		log.Printf("[OpenAI API Server] Unknown model requested: %v\n", model)
		http.Error(responseWriter, fmt.Sprintf("Unknown model: %v", model), http.StatusBadRequest)
		return
	}
	log.Printf("[OpenAI API Server] Sending %s request through to %s\n", request.URL, service.Name)
	request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	proxyRequestToService(responseWriter, request, service)
}

type openAiApiContextKey string

const (
	openAiApiClientConnectionContextKey openAiApiContextKey = "clientConnection"
	clientRequestDoneContextKey         openAiApiContextKey = "clientRequestDone"
)

// openAiApiClientConnection mirrors a client connection to the backends: each
// service the client talks to gets one keep-alive connection that lives as long
// as the client connection, so a backend sees the same connection pattern as it
// would from a direct client.
type openAiApiClientConnection struct {
	mutex                   sync.Mutex
	transportsByServiceName map[string]*http.Transport
}

func (clientConnection *openAiApiClientConnection) transportFor(service ServiceConfig) *http.Transport {
	clientConnection.mutex.Lock()
	defer clientConnection.mutex.Unlock()
	if transport, ok := clientConnection.transportsByServiceName[service.Name]; ok {
		return transport
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost: 1,
		DisableCompression:  true,
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			// The Transport detaches dials from request cancellation but keeps context
			// values, so the client's disconnect is passed as a value to stop waiting
			// for the service to start when the client is gone.
			clientRequestDone, _ := ctx.Value(clientRequestDoneContextKey).(<-chan struct{})
			serviceConnection := startServiceIfNotAlreadyRunningAndConnect(service, clientRequestDone)
			if serviceConnection == nil {
				return nil, fmt.Errorf("failed to start or connect to %s", service.Name)
			}
			log.Printf("[%s] Opened service connection %s", service.Name, humanReadableConnection(serviceConnection))
			return serviceConnection, nil
		},
	}
	clientConnection.transportsByServiceName[service.Name] = transport
	return transport
}

func (clientConnection *openAiApiClientConnection) closeServiceConnections() {
	clientConnection.mutex.Lock()
	defer clientConnection.mutex.Unlock()
	for _, transport := range clientConnection.transportsByServiceName {
		transport.CloseIdleConnections()
	}
}

// proxyRequestToService forwards a single HTTP request to the service that
// serves its model. Requests arriving later on the same keep-alive client
// connection are routed independently, by the model each of them names. The
// service counts as in use only while a request is being served.
func proxyRequestToService(responseWriter http.ResponseWriter, request *http.Request, service ServiceConfig) {
	if interrupted.Load() {
		http.Error(responseWriter, "large-model-proxy is shutting down", http.StatusServiceUnavailable)
		return
	}
	clientConnection := request.Context().Value(openAiApiClientConnectionContextKey).(*openAiApiClientConnection)

	resourceManager.incrementConnection(service.Name, 0, 1)
	var gotServiceConnection atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			if gotServiceConnection.Swap(true) {
				return
			}
			trackServiceLastUsed(service, true)
			resourceManager.incrementConnection(service.Name, 1, -1)
		},
	}
	ctx := context.WithValue(request.Context(), clientRequestDoneContextKey, request.Context().Done())
	request = request.WithContext(httptrace.WithClientTrace(ctx, trace))

	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			proxyRequest.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort(service.ProxyTargetHost, service.ProxyTargetPort)})
			proxyRequest.Out.Host = proxyRequest.In.Host
			proxyRequest.Out.Close = proxyRequest.In.Close
			for _, forwardingHeader := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if values, ok := proxyRequest.In.Header[forwardingHeader]; ok {
					proxyRequest.Out.Header[forwardingHeader] = values
				}
			}
		},
		Transport: clientConnection.transportFor(service),
		ModifyResponse: func(response *http.Response) error {
			if response.Close {
				response.Header.Set("Connection", "close")
			}
			return nil
		},
		FlushInterval: -1,
	}
	reverseProxy.ServeHTTP(responseWriter, request)

	if gotServiceConnection.Load() {
		trackServiceLastUsed(service, false)
		resourceManager.incrementConnection(service.Name, -1, 0)
	} else {
		resourceManager.incrementConnection(service.Name, 0, -1)
	}
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

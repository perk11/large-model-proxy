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
	"os"
	"sync"
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

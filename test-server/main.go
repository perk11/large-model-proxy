package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

var appStarted = false

var mainPortConnections = 0

func main() {
	port := flag.String("p", "", "Main port to listen on")
	healthCheckApiPort := flag.String("healthcheck-port", "", "Healthcheck API port to listen on. If not specified, healthcheck API is disabled")
	durationToSleepBeforeListening := flag.Duration("sleep-before-listening", 0, "How much time to sleep before listening starts, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationStartup := flag.Duration("startup-duration", 0, "How much time to sleep after listening starts but before app is responding with PID, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationRequestProcessing := flag.Duration("request-processing-duration", 0, "How much time to sleep after receiving a connection before responding with PID, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationSleepAfterWritingPid := flag.Duration("sleep-after-writing-pid-duration", 0, "How much time to sleep after respond with PID before closing the connection, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationToSleepBeforeListeningForHealthCheck := flag.Duration("sleep-before-listening-for-healthcheck", 0, "How much time to sleep before listening for healthcheck starts, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	OpenAiApiPort := flag.String("openai-api-port", "", "OpenAI API port to listen on. If not specified, OpenAI API is disabled")
	procPort := flag.String("procinfo-port", "", "Port to expose process information")
	flag.Parse()

	if *port != "" {
		go listenOnMainPort(
			port,
			durationToSleepBeforeListening,
			durationStartup,
			durationRequestProcessing,
			durationSleepAfterWritingPid,
		)
	}
	if *healthCheckApiPort != "" {
		go healthCheckListen(healthCheckApiPort, durationToSleepBeforeListeningForHealthCheck)
	}
	if *OpenAiApiPort != "" {
		go OpenAiApiListen(OpenAiApiPort)
	}
	if *procPort != "" {
		go procListen(*procPort)
	}
	for {
		time.Sleep(time.Duration(1<<63 - 1))
	}
}

type HealthcheckResponse struct {
	Message             string `json:"message"`
	MainPortConnections int    `json:"main_port_connections"`
	Status              int    `json:"status"`
}

func listenOnMainPort(
	port *string,
	sleepDuration,
	startupDuration,
	requestProcessingDuration *time.Duration,
	sleepAfterWritingPidDuration *time.Duration,
) {

	// Simulate pre-listening work
	time.Sleep(*sleepDuration)

	// Mark app as started after startupDuration
	time.AfterFunc(*startupDuration, func() {
		appStarted = true
	})

	listener, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		log.Println("Error listening:", err.Error())
		os.Exit(1)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			log.Println("Failed to stop listening:", err.Error())
		}
	}()
	log.Printf("Listening on port %s\n", *port)

	// Accept connections in a loop
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Log and move on rather than exiting
			log.Println("Error accepting connection:", err.Error())
			continue
		}
		go handleMainPortConnection(conn, requestProcessingDuration, sleepAfterWritingPidDuration)
	}
}

func handleMainPortConnection(conn net.Conn, requestProcessingDuration *time.Duration, sleepAfterWritingPidDuration *time.Duration) {
	mainPortConnections++
	log.Println("Connection received on main port.")

	// We'll track if we've already decremented to avoid double decrementConnections.
	var once sync.Once
	decrementConnections := func() {
		once.Do(func() {
			mainPortConnections--
		})
	}

	// Channel to signal that the server is closing the connection normally
	srvClosed := make(chan struct{})

	// Channel to signal that the client is detected to have disconnected early
	clientClosed := make(chan struct{})

	// Goroutine to detect client closure or read errors
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				// Check if the server has already signaled a normal close
				select {
				case <-srvClosed:
					// This means the server intentionally closed the connection first,
					// so it's a normal close — do not log an error or treat it as early disconnect
					return
				default:
					// Otherwise, the client likely closed early or a read error occurred
					log.Println("Client disconnected early or read error:", err.Error())
					decrementConnections()
					close(clientClosed)
					return
				}
			}
		}
	}()

	var content string
	if appStarted {
		if requestProcessingDuration != nil && requestProcessingDuration.Nanoseconds() > 0 {
			log.Printf("Sleeping for %s before returning pid\n", *requestProcessingDuration)
			time.Sleep(*requestProcessingDuration)
		}
		pid := os.Getpid()
		log.Printf("Responding with pid %d", pid)
		content = fmt.Sprintf("%d", pid)
	} else {
		log.Println("Server still starting, responding with error")
		content = "Error, server still starting"
	}

	// Check if the client has already closed (before writing)
	select {
	case <-clientClosed:
		// Client is gone; `mainPortConnections` is already decremented
		_ = conn.Close()
		return
	default:
		// Client still here, proceed
	}
	if _, err := conn.Write([]byte(content)); err != nil {
		log.Println("Error writing to connection:", err.Error())
	}
	time.Sleep(*sleepAfterWritingPidDuration)
	log.Println("Closing connection")
	// Signal that the server is about to close the connection normally
	close(srvClosed)
	if err := conn.Close(); err != nil {
		log.Println("Error closing connection:", err.Error())
	}

	decrementConnections()
}

func healthCheckHandler(responseWriter http.ResponseWriter, _ *http.Request) {
	var response HealthcheckResponse
	if appStarted {
		response = HealthcheckResponse{
			Message:             "ok",
			MainPortConnections: mainPortConnections,
			Status:              200,
		}
	} else {
		response = HealthcheckResponse{
			Message:             "server_starting",
			MainPortConnections: mainPortConnections,
			Status:              503,
		}
	}

	log.Println("Sending healthcheck status code:", response.Status)
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(response.Status)

	if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
		log.Printf("Failed to encode healthcheck response: %v", err)
		http.Error(responseWriter, "Failed to encode JSON response", http.StatusInternalServerError)
	}
}
func healthCheckListen(port *string, sleepDuration *time.Duration) {
	time.Sleep(*sleepDuration)
	http.HandleFunc("/", healthCheckHandler)
	log.Printf("Listening for healthcheck on port %s\n", *port)

	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatalf("Could not start healthcheck server: %s\n", err.Error())
	}
}

type OpenAiApiCompletionRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type OpenAiApiCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Text         string `json:"text"`
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type OpenAiApiChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // e.g. "chat.completion"
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type ChatCompletionChunk struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"` // e.g. "chat.completion.chunk"
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
}
type ChatCompletionChoice struct {
	Index int `json:"index"`
	Delta struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

func OpenAiApiListen(port *string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/completions", handleCompletions)
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)

	server := &http.Server{
		Addr:    ":" + *port,
		Handler: mux,
	}
	server.SetKeepAlivesEnabled(false)
	log.Printf("OpenAI API server listening on :%s", *port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Could not start OpenAI API server: %s\n", err.Error())
	}
}
func handleCompletions(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	completionRequest, done := parseAndValidateRequestAndPrepareResponseHeaders(w, r)
	if done {
		return
	}
	if completionRequest.Stream {
		handleStreamCompletion(w, completionRequest)
		return
	}

	sampleResponse := OpenAiApiCompletionResponse{
		ID:      "test-id",
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Model:   completionRequest.Model,
		Choices: []struct {
			Text         string "json:\"text\""
			Index        int    "json:\"index\""
			FinishReason string "json:\"finish_reason\""
		}{
			{
				Text:         fmt.Sprintf("\nThis is a test completion text.\n Your prompt was:\n<prompt>%s</prompt>", completionRequest.Prompt),
				Index:        0,
				FinishReason: "stop",
			},
		},
		Usage: struct {
			PromptTokens     int "json:\"prompt_tokens\""
			CompletionTokens int "json:\"completion_tokens\""
			TotalTokens      int "json:\"total_tokens\""
		}{
			PromptTokens:     5,
			CompletionTokens: 7,
			TotalTokens:      12,
		},
	}

	if err := json.NewEncoder(w).Encode(sampleResponse); err != nil {
		log.Printf("Failed to encode completion response: %s\n", err.Error())
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

func handleStreamCompletion(w http.ResponseWriter, completionRequest OpenAiApiCompletionRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("Failed to get http.Flusher for stream completion\n")
		http.Error(w, "Streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	partials := []string{
		"Hello, this is chunk #1. ",
		"Now chunk #2 arrives. ",
		"Finally, chunk #3 completes the message.",
		fmt.Sprintf("Your prompt was:\n<prompt>%s</prompt>", completionRequest.Prompt),
	}

	for _, chunk := range partials {
		responseData := fmt.Sprintf(
			`data: {"id":"test-id","object":"text_completion","created":%d,"model":"%s","choices":[{"text":%q}]}`,
			time.Now().Unix(),
			completionRequest.Model,
			chunk,
		)

		// Write chunk + double newline (required in SSE)
		_, err := fmt.Fprint(w, responseData+"\n\n")
		if err != nil {
			print("Failed to write response to client: ", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Flush immediately so the client sees each chunk
		flusher.Flush()
		time.Sleep(time.Millisecond * 300)
	}

	// After all chunks, send a final “done” message)
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	if err != nil {
		log.Printf("Failed to write [DONE] to client: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher.Flush()
}

func parseAndValidateRequestAndPrepareResponseHeaders(w http.ResponseWriter, r *http.Request) (OpenAiApiCompletionRequest, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return OpenAiApiCompletionRequest{}, true
	}

	var completionRequest OpenAiApiCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&completionRequest); err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse request body: %v", err), http.StatusBadRequest)
		return OpenAiApiCompletionRequest{}, true
	}
	w.Header().Set("Content-Type", "application/json")
	return completionRequest, false
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received Chat request: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	chatRequest, done := parseAndValidateChatRequest(w, r)
	if done {
		return
	}
	if chatRequest.Stream {
		handleStreamChat(w, chatRequest)
	} else {
		handleSingleChatCompletion(w, chatRequest)
	}
}

func handleSingleChatCompletion(w http.ResponseWriter, chatRequest OpenAiApiChatRequest) {
	sampleResponse := ChatCompletionResponse{
		ID:      "chatcmpl-test-id",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   chatRequest.Model,
		Choices: []struct {
			Index        int         `json:"index"`
			Message      ChatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{
			{
				Index: 0,
				Message: ChatMessage{
					Role: "assistant",
					Content: fmt.Sprintf("Hello! This is a response from the test Chat endpoint. The last message was: %q",
						chatRequest.Messages[len(chatRequest.Messages)-1].Content),
				},
				FinishReason: "stop",
			},
		},
		Usage: struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		}{
			PromptTokens:     5,
			CompletionTokens: 7,
			TotalTokens:      12,
		},
	}

	if err := json.NewEncoder(w).Encode(sampleResponse); err != nil {
		log.Printf("Failed to encode response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

func handleStreamChat(w http.ResponseWriter, chatRequest OpenAiApiChatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("Failed to get http.Flusher")
		http.Error(w, "Streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	chunks := []string{
		"Hello, this is chunk #1.",
		"Your last message was:\n",
		chatRequest.Messages[len(chatRequest.Messages)-1].Content,
	}

	for i, chunk := range chunks {
		response := ChatCompletionChunk{
			ID:      "chatcmpl-test-id",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   chatRequest.Model,
			Choices: []ChatCompletionChoice{
				{
					Index: 0,
				},
			},
		}

		if i == 0 {
			response.Choices[0].Delta.Role = "assistant"
		}
		response.Choices[0].Delta.Content = chunk

		if !sendResponseChunk(w, response, flusher) {
			return
		}
		time.Sleep(time.Millisecond * 300)
	}
	finishReason := "stop"
	sendResponseChunk(w, ChatCompletionChunk{
		ID:      "chatcmpl-test-id",
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   chatRequest.Model,
		Choices: []ChatCompletionChoice{
			{
				Index:        0,
				FinishReason: &finishReason,
			},
		},
	}, flusher)

	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	if err != nil {
		log.Printf("Failed to write [DONE] to client: %v", err)
	}
	flusher.Flush()
}

func sendResponseChunk(responseWriter http.ResponseWriter, chatCompletionChunk ChatCompletionChunk, flusher http.Flusher) bool {
	data, err := json.Marshal(chatCompletionChunk)
	if err != nil {
		log.Printf("Failed to encode chatCompletionChunk: %v", err)
		http.Error(responseWriter, err.Error(), http.StatusInternalServerError)
		return false
	}

	// SSE requires each message to start with `data: `
	_, err = fmt.Fprintf(responseWriter, "data: %s\n\n", data)
	if err != nil {
		log.Printf("Failed to write SSE to client: %v", err)
		return false
	}
	flusher.Flush()
	return true
}

func parseAndValidateChatRequest(w http.ResponseWriter, r *http.Request) (OpenAiApiChatRequest, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return OpenAiApiChatRequest{}, true
	}

	var req OpenAiApiChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Failed to parse chat request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to parse chat request body: %v", err), http.StatusBadRequest)
		return OpenAiApiChatRequest{}, true
	}
	w.Header().Set("Content-Type", "application/json")
	return req, false
}

func procListen(port string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/procinfo", handleProcinfo)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}
	server.SetKeepAlivesEnabled(false)
	log.Printf("proc info server listening on :%s", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Could not start proc info server: %s\n", err.Error())
	}
}

func handleProcinfo(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(map[string]any{
		"args": os.Args,
		"env":  os.Environ(),
	})
	if err != nil {
		log.Printf("Failed to encode args and env: %v", err)
	}
}

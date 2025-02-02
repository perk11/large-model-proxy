package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

var appStarted = false

func main() {
	port := flag.String("p", "", "Main port to listen on")
	healthCheckApiPort := flag.String("healthcheck-port", "", "Healthcheck API port to listen on. If not specified, healthcheck API is disabled")
	durationToSleepBeforeListening := flag.Duration("sleep-before-listening", 0, "How much time to sleep before listening starts, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationStartup := flag.Duration("startup-duration", 0, "How much time to sleep after listening starts but before app is responding with PID, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationRequestProcessing := flag.Duration("request-processing-duration", 0, "How much time to sleep after receiving a connection before responding with PID, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationToSleepBeforeListeningForHealthCheck := flag.Duration("sleep-before-listening-for-healthcheck", 0, "How much time to sleep before listening for healthcheck starts, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	llmApiPort := flag.String("llm-port", "", "LLM API port to listen on. If not specified, LLM API is disabled")
	flag.Parse()

	if *port != "" {
		go listenOnMainPort(port, durationToSleepBeforeListening, durationStartup, durationRequestProcessing)
	}
	if *healthCheckApiPort != "" {
		go healthCheckListen(healthCheckApiPort, durationToSleepBeforeListeningForHealthCheck)
	}
	if *llmApiPort != "" {
		go llmApiListen(llmApiPort)
	}
	for {
		time.Sleep(time.Duration(1<<63 - 1))
	}
}

type HealthcheckResponse struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func listenOnMainPort(port *string, sleepDuration *time.Duration, startupDuration *time.Duration, requestProcessingDuration *time.Duration) {
	time.Sleep(*sleepDuration)
	time.AfterFunc(*startupDuration, func() {
		appStarted = true
	})
	listener, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		os.Exit(1)
	}
	defer func(listener net.Listener) {
		err := listener.Close()
		if err != nil {
			fmt.Println("Failed to stop listening: ", err.Error())
		}
	}(listener)
	fmt.Printf("Listening on port %s\n", *port)
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		fmt.Println("Connection received.")
		var contentToWriteToSocket string
		if appStarted {
			if requestProcessingDuration.Nanoseconds() > 0 {
				fmt.Printf("Sleeping for %s before returning pid\n", requestProcessingDuration)
				time.Sleep(*requestProcessingDuration)
			}
			fmt.Println("Responding with pid")
			pid := os.Getpid()
			contentToWriteToSocket = fmt.Sprintf("%d", pid)
		} else {
			fmt.Println("Server was still starting, responding with error")
			contentToWriteToSocket = fmt.Sprintf("Error, server still starting")
		}
		_, writeErr := conn.Write([]byte(contentToWriteToSocket))
		if writeErr != nil {
			fmt.Println("Error writing to connection: ", writeErr.Error())
		}

		connectionCloseErr := conn.Close()
		if connectionCloseErr != nil {
			fmt.Println("Error closing connection: ", connectionCloseErr.Error())
		}
	}
}

func healthCheckHandler(responseWriter http.ResponseWriter, _ *http.Request) {
	var response HealthcheckResponse
	if appStarted {
		response = HealthcheckResponse{
			Message: "ok",
			Status:  200,
		}
	} else {
		response = HealthcheckResponse{
			Message: "server_starting",
			Status:  503,
		}
	}

	fmt.Println("Sending healthcheck status code:", response.Status)
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(response.Status)

	if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
		http.Error(responseWriter, "Failed to encode JSON response", http.StatusInternalServerError)
	}
}
func healthCheckListen(port *string, sleepDuration *time.Duration) {
	time.Sleep(*sleepDuration)
	http.HandleFunc("/", healthCheckHandler)
	fmt.Printf("Listening for healthcheck on port %s\n", *port)

	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatalf("Could not start healthcheck server: %s\n", err.Error())
	}
}

type LlmCompletionRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type LlmCompletionResponse struct {
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

func llmApiListen(port *string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/completions", handleCompletions)
	server := &http.Server{
		Addr:    ":" + *port,
		Handler: mux,
	}
	server.SetKeepAlivesEnabled(false)
	log.Printf("LLM server listening on :%s", *port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Could not start LLM server: %s\n", err.Error())
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

	sampleResponse := LlmCompletionResponse{
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
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
}

func handleStreamCompletion(w http.ResponseWriter, completionRequest LlmCompletionRequest) {
	w.Header().Set("Content-Type", "text/event-stream") // SSE or similar
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")

	flusher, ok := w.(http.Flusher)
	if !ok {
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
		print("Failed to write [DONE] to client: ", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher.Flush()
}

func parseAndValidateRequestAndPrepareResponseHeaders(w http.ResponseWriter, r *http.Request) (LlmCompletionRequest, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return LlmCompletionRequest{}, true
	}

	var completionRequest LlmCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&completionRequest); err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse request body: %v", err), http.StatusBadRequest)
		return LlmCompletionRequest{}, true
	}
	w.Header().Set("Content-Type", "application/json")
	return completionRequest, false
}

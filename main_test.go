package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// LlmCompletionResponse is what /v1/completions returns
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

// LlmCompletionRequest is used by /v1/completions
type LlmCompletionRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// LlmChatCompletionRequest is used by /v1/chat/completions
type LlmChatCompletionRequest struct {
	Model    string        `json:"model,omitempty"`
	Messages []ChatMessage `json:"messages,omitempty"`
	Stream   bool          `json:"stream,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LlmChatCompletionResponse is what /v1/chat/completions returns
type LlmChatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      ChatMessage `json:"message"`
		Delta        ChatMessage `json:"delta"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func connectOnly(test *testing.T, proxyAddress string) {
	_, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		test.Error(err)
		return
	}
	//give large-model-proxy time to start the service, so that it doesn't get killed before it started it
	//which can lead to false positive passing tests
	time.Sleep(1 * time.Second)
}

func idleTimeout(test *testing.T, proxyAddress string) {
	pid := runReadPidCloseConnection(test, proxyAddress)
	if pid == 0 {
		//runReadPidCloseConnection already failed the test
		return
	}
	secondPid := runReadPidCloseConnection(test, proxyAddress)
	if secondPid != pid {
		test.Errorf("pid is different during second connection")
		return
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pid) {
		test.Errorf("Process is still running after connection is closed and ShutDownAfterInactivitySeconds have passed")
		return
	}

	thirdPid := runReadPidCloseConnection(test, proxyAddress)
	if thirdPid == 0 {
		return
	}
	if thirdPid == pid {
		test.Errorf("pid during third connection is the same as during first connection ")
		return
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pid) {
		test.Errorf("Process is still running after connection is closed and ShutDownAfterInactivitySeconds have passed")
	}
}

func idleTimeoutMultipleServices(test *testing.T, serviceOneAddress string, serviceTwoAddress string) {
	connOne, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		test.Error(err)
		return
	}
	connTwo, err := net.Dial("tcp", serviceTwoAddress)
	if err != nil {
		test.Error(err)
		return
	}
	pidOne := readPidFromOpenConnection(test, connOne)

	err = connOne.Close()
	if err != nil {
		test.Error(err)
	}
	if pidOne == 0 {
		//readPidFromOpenConnection already failed the test
		return
	}
	pidTwo := readPidFromOpenConnection(test, connTwo)
	if pidTwo == 0 {
		//readPidFromOpenConnection already failed the test
		return
	}
	if isProcessRunning(pidOne) {
		test.Errorf("first service is still running even though it was supposed to be stopped")
	}
	err = connTwo.Close()
	if err != nil {
		test.Error(err)
	}
	if !isProcessRunning(pidTwo) {
		test.Errorf("second service is not running right after closing connection")
	}

	time.Sleep(1 * time.Second)
	newPid := runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached")
	}
	time.Sleep(1 * time.Second)
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached")
	}
	time.Sleep(1 * time.Second)
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached")
	}
	time.Sleep(1 * time.Second)
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached")
	}
	if !isProcessRunning(pidTwo) {
		test.Errorf("second service is not running right after closing connection two")
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pidTwo) {
		test.Errorf("Process is still running after connection is closed and ShutDownAfterInactivitySeconds have passed")
	}

	// Maker sure large-model-proxy hasn't crashed
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid == pidTwo {
		test.Errorf("second Service is reusing old pid, this should not be possible")
	}

	runReadPidCloseConnection(test, serviceOneAddress)
}

func llmApi(test *testing.T) {
	//sanity check  that nothing is running before initial connection
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	client := &http.Client{}
	resp := modelsRequestExpectingSuccess(test, "http://localhost:2016/v1/models", client)
	assertModelsResponse(test, []string{"test-llm-1", "fizz", "buzz"}, resp)

	resp = sendCompletionRequest(test, "http://localhost:2016", LlmCompletionRequest{
		Model:  "non-existent",
		Prompt: "This is a test prompt\nЭто проверочный промт\n这是一个测试提示",
		Stream: false,
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		test.Fatalf("Expected status code 400, got %d", resp.StatusCode)
	}
	if err := resp.Body.Close(); err != nil {
		test.Error(err)
	}

	//Still no services should be running
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	testCompletionRequest(test, "http://localhost:2016", "test-llm-1", nil)
	assertPortsAreClosed(test, []string{"localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	testCompletionStreamingExpectingSuccess(test, "test-llm-1")
	testChatCompletionRequestExpectingSuccess(test, "http://localhost:2016", "test-llm-1")
	testChatCompletionStreamingExpectingSuccess(test, "http://localhost:2016", "test-llm-1")

	llm1Pid := runReadPidCloseConnection(test, "localhost:12018")
	assertPortsAreClosed(test, []string{"localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	time.Sleep(4 * time.Second)

	if isProcessRunning(llm1Pid) {
		test.Fatalf("test-llm-1 service is still running, but inactivity timeout should have shut it down by now")
	}
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	testChatCompletionRequestExpectingSuccess(test, "http://localhost:2016", "fizz")
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12021", "localhost:12022", "localhost:12023"})

	testCompletionRequest(test, "http://localhost:2016", "fizz", nil)
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12021", "localhost:12022", "localhost:12023"})

	testChatCompletionStreamingExpectingSuccess(test, "http://localhost:2016", "fizz")
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12021", "localhost:12022", "localhost:12023"})

	testCompletionStreamingExpectingSuccess(test, "fizz")
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12021", "localhost:12022", "localhost:12023"})
	llm2Pid := runReadPidCloseConnection(test, "localhost:12020")
	time.Sleep(4 * time.Second)
	if isProcessRunning(llm2Pid) {
		test.Fatalf("test-llm-2 service is still running, but inactivity timeout should have shut it down by now")
	}

	testCompletionRequest(test, "http://localhost:2016", "buzz", nil)
	llm2Pid = runReadPidCloseConnection(test, "localhost:12020")
	time.Sleep(4 * time.Second)
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12021", "localhost:12022", "localhost:12023"})
	if isProcessRunning(llm2Pid) {
		test.Fatalf("test-llm-2 service is still running, but inactivity timeout should have shut it down by now")
	}

	testCompletionRequest(test, "http://localhost:2019", "foo", nil)
	llm2Pid = runReadPidCloseConnection(test, "localhost:12020")
	time.Sleep(4 * time.Second)
	if isProcessRunning(llm2Pid) {
		test.Fatalf("test-llm-2 service is still running, but inactivity timeout should have shut it down by now")
	}
	assertPortsAreClosed(test, []string{"localhost:12011", "localhost:12012", "localhost:12013", "localhost:12014", "localhost:12016", "localhost:12017", "localhost:12018"})
}

func assertModelsResponse(test *testing.T, expectedIDs []string, resp *http.Response) {
	var modelsResp LlmApiModels
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		test.Fatalf("Failed to decode /v1/models response: %v", err)
	}

	foundIDs := make([]bool, len(expectedIDs))

	if len(modelsResp.Data) != len(expectedIDs) {
		test.Fatalf("Expected %d models, but got %d", len(expectedIDs), len(modelsResp.Data))
	}
	for _, model := range modelsResp.Data {
		idx := indexOf(expectedIDs, model.ID)
		if idx == -1 {
			test.Errorf("Unexpected model ID returned: %s", model.ID)
			continue
		}
		foundIDs[idx] = true

		if model.Object == "" {
			test.Errorf("Model %s has an empty 'object' field", model.ID)
		}
		if model.Created == 0 {
			test.Errorf("Model %s has 'created' == 0 (expected a non-zero timestamp)", model.ID)
		}
		if model.OwnedBy == "" {
			test.Errorf("Model %s has an empty 'owned_by' field", model.ID)
		}
	}
}
func llmApiReusingConnection(test *testing.T) {
	//sanity check  that nothing is running before initial connection
	assertPortsAreClosed(test, []string{"localhost:12025", "localhost:12026"})
	client := &http.Client{}
	resp := modelsRequestExpectingSuccess(test, "http://localhost:2024/v1/models", client)
	assertModelsResponse(test, []string{"test-llm-keep-alive"}, resp)
	resp = modelsRequestExpectingSuccess(test, "http://localhost:2024/v1/models", client)
	assertModelsResponse(test, []string{"test-llm-keep-alive"}, resp)

	testCompletionRequest(test, "http://localhost:2024", "test-llm-keep-alive", client)
	testCompletionRequest(test, "http://localhost:2024", "test-llm-keep-alive", client)
	//TODO: Enable Keep-Alive in test server
	//TODO: add streaming request
	//TODO: add assertions about number of connections open

	req, err := http.NewRequest("GET", "http://localhost:2024/non-existent", nil)
	if err != nil {
		test.Fatalf("Failed to create request: %v", err)
	}
	resp, err = client.Do(req)
	if err != nil {
		test.Fatalf("/non-existent Request failed: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		test.Fatalf("Expected status code 404, got %d", resp.StatusCode)
	}
	//TODO: this is not maintaining a connection currently, fix this
	testCompletionRequest(test, "http://localhost:2024", "test-llm-keep-alive", client)

	err = resp.Body.Close()
	if err != nil {
		test.Error(err)
	}
}

func modelsRequestExpectingSuccess(test *testing.T, url string, client *http.Client) *http.Response {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		test.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		test.Fatalf("/v1/models Request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		test.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		test.Fatalf("Expected Content Type \"application/json; charset=utf-8\", got %s", resp.Header.Get("Content-Type"))
	}
	return resp
}

func assertPortsAreClosed(test *testing.T, servicesToCheckForClosedPorts []string) {
	for _, address := range servicesToCheckForClosedPorts {
		err := checkPortClosed(address)
		if err != nil {
			test.Errorf("Port %s is open when service is not supposed to be running", address)
		}
	}
}

// testCompletionStreamingExpectingSuccess checks streaming completions from /v1/completions
func testCompletionStreamingExpectingSuccess(t *testing.T, model string) {
	address := "http://localhost:2016"
	testPrompt := "This is a test prompt\nЭто проверочный промт\n这是一个测试提示"
	reqBodyStruct := LlmCompletionRequest{
		Model:  model,
		Prompt: testPrompt,
		Stream: true,
	}

	url := fmt.Sprintf("%s/v1/completions", address)
	testStreamingRequest(t, url, reqBodyStruct, []string{
		"Hello, this is chunk #1. ",
		"Now chunk #2 arrives. ",
		"Finally, chunk #3 completes the message.",
		fmt.Sprintf("Your prompt was:\n<prompt>%s</prompt>", testPrompt),
	},
		func(t *testing.T, payload string) string {
			var chunkResp LlmCompletionResponse
			if err := json.Unmarshal([]byte(payload), &chunkResp); err != nil {
				t.Fatalf("Error unmarshalling SSE chunk JSON: %v", err)
			}
			if len(chunkResp.Choices) == 0 {
				t.Fatalf("Received chunk without choices: %+v", chunkResp)
			}
			return chunkResp.Choices[0].Text
		},
	)
}
func testCompletionRequest(test *testing.T, address string, model string, client *http.Client) {
	testPrompt := "This is a test prompt\nЭто проверочный промт\n这是一个测试提示"

	// Prepare request body
	completionReq := LlmCompletionRequest{
		Model:  model,
		Prompt: testPrompt,
		Stream: false,
	}
	completionResp := sendCompletionRequestExpectingSuccess(test, address, completionReq, client)
	if len(completionResp.Choices) == 0 {
		test.Fatalf("No choices returned in completion response: %+v", completionResp)
	}
	expected := fmt.Sprintf(
		"\nThis is a test completion text.\n Your prompt was:\n<prompt>%s</prompt>",
		testPrompt,
	)

	got := completionResp.Choices[0].Text
	if got != expected {
		test.Fatalf("Completion text mismatch.\nExpected:\n%q\nGot:\n%q", expected, got)
	}

	if completionResp.Model != model {
		test.Fatalf("Model mismatch.\nExpected:\n%q\nGot:\n%q", model, completionResp.Model)
	}
}

// testChatCompletionRequestExpectingSuccess checks a non-streaming chat completion
func testChatCompletionRequestExpectingSuccess(t *testing.T, address, model string) {
	messages := []ChatMessage{
		{Role: "system", Content: "You are a helpful AI assistant."},
		{Role: "user", Content: "Hello, how are you?"},
	}

	chatReq := LlmChatCompletionRequest{
		Model:    model,
		Messages: messages,
		Stream:   false,
	}

	chatResp := sendChatCompletionRequestExpectingSuccess(t, address, chatReq)
	if len(chatResp.Choices) == 0 {
		t.Fatalf("No choices returned in chat completion response: %+v", chatResp)
	}

	expected := fmt.Sprintf("Hello! This is a response from the test Chat endpoint. The last message was: %q", messages[len(messages)-1].Content)
	got := chatResp.Choices[0].Message.Content
	if got != expected {
		t.Fatalf("Chat completion text mismatch.\nExpected:\n%q\nGot:\n%q", expected, got)
	}

	if chatResp.Model != model {
		t.Fatalf("Model mismatch.\nExpected:\n%q\nGot:\n%q", model, chatResp.Model)
	}
}

// testChatCompletionStreamingExpectingSuccess checks streaming chat completions from /v1/chat/completions
func testChatCompletionStreamingExpectingSuccess(t *testing.T, address, model string) {
	messages := []ChatMessage{
		{Role: "system", Content: "You are a helpful AI assistant."},
		{Role: "user", Content: "Tell me something interesting."},
		{Role: "assistant", Content: "I absolutely will not"},
		{Role: "user", Content: "Thanks\nfor\nnothing!"},
	}

	url := fmt.Sprintf("%s/v1/chat/completions", address)
	testStreamingRequest(t, url, LlmChatCompletionRequest{
		Model:    model,
		Messages: messages,
		Stream:   true,
	}, []string{
		"Hello, this is chunk #1.",
		"Your last message was:\n",
		"Thanks\nfor\nnothing!",
		"", //done chunk which doesn't have a delta
	}, func(t *testing.T, payload string) string {
		var chunkResp LlmChatCompletionResponse
		if err := json.Unmarshal([]byte(payload), &chunkResp); err != nil {
			t.Fatalf("Error unmarshalling SSE chunk JSON: %v", err)
		}
		if len(chunkResp.Choices) == 0 {
			t.Fatalf("Received chunk without choices: %+v", chunkResp)
		}
		chunk := chunkResp.Choices[0].Delta.Content
		return chunk
	},
	)
}

func testStreamingRequest(t *testing.T, url string, requestBodyObject any, expectedChunks []string, readChunkFunc func(t *testing.T, payload string) string) {

	reqBody, err := json.Marshal(requestBodyObject)
	if err != nil {
		t.Fatalf("%s: Failed to marshal JSON: %v", url, err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("%s, Failed to create request: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: Streaming request failed: %v", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: Expected status code 200, got %d", url, resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	var allChunks []string
	doneReceived := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				doneReceived = true
				break
			}

			chunk := readChunkFunc(t, payload)
			allChunks = append(allChunks, chunk)
		}
	}

	if !doneReceived {
		t.Fatalf("%s: Did not receive [DONE] marker in SSE stream", url)
	}

	if len(allChunks) != len(expectedChunks) {
		t.Fatalf("%s: Expected %d chunks, got %d\nChunks: %+v", url, len(expectedChunks), len(allChunks), allChunks)
	}

	for i, expected := range expectedChunks {
		if allChunks[i] != expected {
			t.Fatalf("%s: Mismatch in chunk #%d.\nExpected: %q\nGot: %q", url, i+1, expected, allChunks[i])
		}
	}
}

func sendChatCompletionRequestExpectingSuccess(t *testing.T, address string, chatReq LlmChatCompletionRequest) LlmChatCompletionResponse {
	resp := sendChatCompletionRequest(t, address, chatReq)
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	var chatResp LlmChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		t.Fatalf("Failed to decode /v1/chat/completions response: %v", err)
	}
	return chatResp
}

// sendChatCompletionRequest sends a POST to /v1/chat/completions with the given JSON body
func sendChatCompletionRequest(t *testing.T, address string, chatReq LlmChatCompletionRequest) *http.Response {
	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		t.Fatalf("Failed to marshal JSON body: %v", err)
	}

	url := fmt.Sprintf("%s/v1/chat/completions", address)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(reqBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("/v1/chat/completions request failed: %v", err)
	}
	return resp
}

func sendCompletionRequestExpectingSuccess(test *testing.T, address string, completionReq LlmCompletionRequest, client *http.Client) LlmCompletionResponse {
	resp := sendCompletionRequest(test, address, completionReq, client)
	defer func(Body io.ReadCloser) {
		if cerr := Body.Close(); cerr != nil {
			test.Error(cerr)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		test.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	var completionResp LlmCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completionResp); err != nil {
		test.Fatalf("Failed to decode /v1/completions response: %v", err)
	}
	return completionResp
}

func sendCompletionRequest(test *testing.T, address string, completionReq LlmCompletionRequest, client *http.Client) *http.Response {
	reqBody, err := json.Marshal(completionReq)
	if err != nil {
		test.Fatalf("Failed to marshal JSON body: %v", err)
	}

	url := fmt.Sprintf("%s/v1/completions", address)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(reqBody))
	if err != nil {
		test.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if client == nil {
		client = &http.Client{}
		client.Timeout = 5 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		test.Fatalf("/v1/completions Request failed: %v", err)
	}
	return resp
}

// indexOf returns the index of target in arr, or -1 if not found.
func indexOf(arr []string, target string) int {
	for i, val := range arr {
		if val == target {
			return i
		}
	}
	return -1
}

func readPidFromOpenConnection(test *testing.T, conn net.Conn) int {
	buffer := make([]byte, 1024)
	bytesRead, err := conn.Read(buffer)
	if err != nil {
		if err != io.EOF {
			test.Error(err)
			return 0
		}
	}
	pidString := string(buffer[:bytesRead])
	if !isNumeric(pidString) {
		test.Errorf("value \"%s\" is not numeric, expected a pid", pidString)
		return 0
	}
	pidInt, err := strconv.Atoi(pidString)
	if err != nil {
		test.Error(err, pidString)
		return 0
	}
	if pidInt <= 0 {
		test.Errorf("value \"%s\" is not a valid pid", pidString)
		return 0
	}
	return pidInt
}
func runReadPidCloseConnection(test *testing.T, proxyAddress string) int {
	conn, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		test.Error(err)
		return 0
	}

	pid := readPidFromOpenConnection(test, conn)

	if !isProcessRunning(pid) {
		test.Errorf("process \"%d\" is not running while connection is still open", pid)
		return 0
	}

	err = conn.Close()
	if err != nil {
		test.Error(err)
		return 0
	}

	return pid
}
func minimal(test *testing.T, proxyAddress string) {
	runReadPidCloseConnection(test, proxyAddress)
}

func isNumeric(s string) bool {
	for _, char := range s {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
func isProcessRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
func startLargeModelProxy(testCaseName string, configPath string, waitChannel chan error) (*exec.Cmd, error) {
	cmd := exec.Command("./large-model-proxy", "-c", configPath)
	logFilePath := fmt.Sprintf("logs/test_%s.log", testCaseName)
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		log.Printf("Failed to open log file for test %s", logFilePath)
	}
	if err := cmd.Start(); err != nil {
		waitChannel <- err
		return nil, err
	}
	go func() {
		waitChannel <- cmd.Wait()
	}()

	time.Sleep(1 * time.Second)

	select {
	case err := <-waitChannel:
		if err != nil {
			return nil, fmt.Errorf("large-model-proxy exited prematurely with error %v", err)
		} else {
			return nil, fmt.Errorf("large-model-proxy exited prematurely with success")
		}
	default:
	}

	err = cmd.Process.Signal(syscall.Signal(0))
	if err != nil {
		if err.Error() == "os: process already finished" {
			return nil, fmt.Errorf("large-model-proxy exited prematurely")
		}
		return nil, fmt.Errorf("error checking process state: %w", err)
	}

	return cmd, nil
}

func stopApplication(cmd *exec.Cmd, waitChannel chan error) error {
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		return err
	}

	select {
	case err := <-waitChannel:
		if err != nil && err.Error() != "waitid: no child processes" && err.Error() != "wait: no child processes" {
			return err
		}
		return nil
	case <-time.After(15 * time.Second):
		// Optionally kill the process if it hasn't exited
		_ = cmd.Process.Kill()
		return errors.New("large-model-proxy process did not stop within 15 seconds after receiving SIGINT")
	}
}

func checkPortClosed(address string) error {
	_, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		return fmt.Errorf("port %s is still open", address)
	}
	return nil
}

func TestAppScenarios(test *testing.T) {
	tests := []struct {
		Name                          string
		ConfigPath                    string
		AddressesToCheckAfterStopping []string
		TestFunc                      func(t *testing.T)
	}{
		{
			Name:                          "minimal",
			ConfigPath:                    "test-server/minimal.json",
			AddressesToCheckAfterStopping: []string{"localhost:2000", "localhost:12000"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2000")
			},
		},
		{
			Name:                          "healthcheck",
			ConfigPath:                    "test-server/healthcheck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2001", "localhost:12001", "localhost:2011"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2001")
			},
		},
		{
			Name:                          "healthcheck-immediate-listen-start",
			ConfigPath:                    "test-server/healthcheck-immediate-listen-start.json",
			AddressesToCheckAfterStopping: []string{"localhost:2002", "localhost:12002", "localhost:2012"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2002")
			},
		},
		{
			Name:                          "healthcheck-immediate-startup-delayed-healthcheck",
			ConfigPath:                    "test-server/healthcheck-immediate-startup-delayed-healthcheck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2003", "localhost:12003", "localhost:2013"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2003")
			},
		},
		{
			Name:                          "healthcheck-immediate-startup",
			ConfigPath:                    "test-server/healthcheck-immediate-startup.json",
			AddressesToCheckAfterStopping: []string{"localhost:2004", "localhost:2014"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2004")
			},
		},
		{
			Name:                          "healthcheck-stuck",
			ConfigPath:                    "test-server/healthcheck-stuck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2005", "localhost:12005", "localhost:2015"},
			TestFunc: func(t *testing.T) {
				connectOnly(t, "localhost:2005")
			},
		},
		{
			Name:                          "service-stuck-no-healthcheck",
			ConfigPath:                    "test-server/service-stuck-no-healthcheck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2006"},
			TestFunc: func(t *testing.T) {
				connectOnly(t, "localhost:2006")
			},
		},
		{
			Name:                          "idle-timeout",
			ConfigPath:                    "test-server/idle-timeout.json",
			AddressesToCheckAfterStopping: []string{"localhost:2007"},
			TestFunc: func(t *testing.T) {
				idleTimeout(t, "localhost:2007")
			},
		},
		{
			Name:                          "idle-timeout-after-stop",
			ConfigPath:                    "test-server/idle-timeout-after-stop.json",
			AddressesToCheckAfterStopping: []string{"localhost:2008", "localhost:2009"},
			TestFunc: func(t *testing.T) {
				idleTimeoutMultipleServices(t, "localhost:2008", "localhost:2009")
			},
		},
		{
			Name:       "llm-api",
			ConfigPath: "test-server/llm-api.json",
			AddressesToCheckAfterStopping: []string{
				"localhost:2016",
				"localhost:2018",
				"localhost:2019",
				"localhost:2020",
				"localhost:2021",
				"localhost:2022",
				"localhost:12017",
				"localhost:12018",
				"localhost:12019",
				"localhost:12020",
				"localhost:12021",
				"localhost:12022",
				"localhost:12023",
			},
			TestFunc: func(t *testing.T) {
				llmApi(t)
			},
		},
		{
			Name:       "llm-api-keep-alive",
			ConfigPath: "test-server/llm-api-reusing-connection.json",
			AddressesToCheckAfterStopping: []string{
				"localhost:2024",
				"localhost:12025",
				"localhost:12026",
			},
			TestFunc: func(t *testing.T) {
				llmApiReusingConnection(t)
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase // Capture range variable
		test.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			waitChannel := make(chan error, 1)
			cmd, err := startLargeModelProxy(testCase.Name, testCase.ConfigPath, waitChannel)
			if err != nil {
				t.Fatalf("could not start application: %v", err)
			}

			defer func() {
				if cmd == nil {
					t.Errorf("not stopping application since there was a start error: %v", err)
					return
				}
				if err := stopApplication(cmd, waitChannel); err != nil {
					t.Errorf("failed to stop application: %v", err)
				}
				for _, address := range testCase.AddressesToCheckAfterStopping {
					if err := checkPortClosed(address); err != nil {
						t.Errorf("port %s is still open after application exit: %v", address, err)
					}
				}
			}()

			testCase.TestFunc(t)
		})
	}
}

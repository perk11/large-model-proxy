package main

import (
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
	"syscall"
	"testing"
	"time"
)

// OpenAiApiCompletionResponse is what /v1/completions returns
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

// OpenAiApiCompletionRequest is used by /v1/completions
type OpenAiApiCompletionRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// OpenAiApiChatCompletionRequest is used by /v1/chat/completions
type OpenAiApiChatCompletionRequest struct {
	Model    string        `json:"model,omitempty"`
	Messages []ChatMessage `json:"messages,omitempty"`
	Stream   bool          `json:"stream,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenAiApiChatCompletionResponse is what /v1/chat/completions returns
type OpenAiApiChatCompletionResponse struct {
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

func assertModelsResponse(test *testing.T, expectedIDs []string, resp *http.Response) {
	var modelsResp OpenAiApiModels
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

func sendChatCompletionRequestExpectingSuccess(t *testing.T, address string, chatReq OpenAiApiChatCompletionRequest) OpenAiApiChatCompletionResponse {
	resp := sendChatCompletionRequest(t, address, chatReq)
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	var chatResp OpenAiApiChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		t.Fatalf("Failed to decode /v1/chat/completions response: %v", err)
	}
	return chatResp
}

// sendChatCompletionRequest sends a POST to /v1/chat/completions with the given JSON body
func sendChatCompletionRequest(t *testing.T, address string, chatReq OpenAiApiChatCompletionRequest) *http.Response {
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

func sendCompletionRequestExpectingSuccess(test *testing.T, address string, completionReq OpenAiApiCompletionRequest, client *http.Client) OpenAiApiCompletionResponse {
	resp := sendCompletionRequest(test, address, completionReq, client)
	defer func(Body io.ReadCloser) {
		if cerr := Body.Close(); cerr != nil {
			test.Error(cerr)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		test.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	var completionResp OpenAiApiCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completionResp); err != nil {
		test.Fatalf("Failed to decode /v1/completions response: %v", err)
	}
	return completionResp
}

func sendCompletionRequest(test *testing.T, address string, completionReq OpenAiApiCompletionRequest, client *http.Client) *http.Response {
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
		test.Fatalf("value \"%s\" is not numeric, expected a pid", pidString)
		return 0
	}
	pidInt, err := strconv.Atoi(pidString)
	if err != nil {
		test.Fatal(err, pidString)
		return 0
	}
	if pidInt <= 0 {
		test.Fatalf("value \"%s\" is not a valid pid", pidString)
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
func testImplMinimal(test *testing.T, proxyAddress string) {
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
	testLogsFolder := "test-logs"
	if _, err := os.Stat(testLogsFolder); os.IsNotExist(err) {
		os.Mkdir(testLogsFolder, 0755)
	}
	logFilePath := fmt.Sprintf("%s/test_%s.log", testLogsFolder, testCaseName)
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

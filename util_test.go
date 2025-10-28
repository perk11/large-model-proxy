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
	test.Helper()
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
	test.Helper()
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
	test.Helper()
	for _, address := range servicesToCheckForClosedPorts {
		err := checkPortClosed(address)
		if err != nil {
			test.Errorf("Port %s is open when service is not supposed to be running", address)
		}
	}
}

func sendChatCompletionRequestExpectingSuccess(t *testing.T, address string, chatReq OpenAiApiChatCompletionRequest) OpenAiApiChatCompletionResponse {
	t.Helper()
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
	t.Helper()
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
	test.Helper()
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
	test.Helper()
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
	test.Helper()
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
	test.Helper()
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
func startLargeModelProxy(testCaseName string, configPath string, workDir string, waitChannel chan error) (*exec.Cmd, error) {
	args := make([]string, 0)
	if configPath != "" {
		args = append(args, "-c", configPath)
	}
	currentDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	binaryPath := fmt.Sprintf("%s/large-model-proxy", currentDir)
	cmd := exec.Command(binaryPath, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	testLogsFolder := fmt.Sprintf("%s/test-logs", currentDir)
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

func createTempConfig(t *testing.T, cfg Config) string {
	t.Helper()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal config to JSON: %v", err)
	}

	tmpFile, err := os.CreateTemp("", "test-config-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp config file: %v", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		t.Fatalf("Failed to write to temp config file: %v", err)
	}

	filePath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close temp config file: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Remove(filePath); err != nil {
			log.Printf("Warning: failed to remove temp config file %s: %v", filePath, err)
		}
	})

	return filePath
}

func ptrToString(s string) *string {
	return &s
}

// Given a config with services, each service will be given a unique name and log file path.
// If no service name is provided, one will be allocated based on the index in the config.
// The resulting service name will be standardized to include both the test name and the service name.
// The log file will also use the standardized name.
func StandardizeConfigNamesAndPaths(config *Config, testName string, t *testing.T) {
	for i := range config.Services {
		service := &config.Services[i] // Get a pointer to modify the struct in the slice
		originalServiceName := service.Name
		if originalServiceName == "" {
			// If no name was provided in the config, generate a default one based on index.
			// This ensures that even services without explicit names get a unique original name part.
			originalServiceName = fmt.Sprintf("service%d", i)
		}

		standardizedServiceName := fmt.Sprintf("%s_%s", testName, originalServiceName)
		service.Name = standardizedServiceName
		service.LogFilePath = fmt.Sprintf("test-logs/%s.log", standardizedServiceName)
	}
}

// verifyTotalResourceUsage checks if the total resource usage matches the expected values
func verifyTotalResourceUsage(t *testing.T, resp StatusResponse, expectedUsage map[string]int) {
	t.Helper()
	for resource, expectedAmount := range expectedUsage {
		resourceInfo, ok := resp.Resources[resource]
		if !ok {
			t.Errorf("Resource %s not found in status response", resource)
			continue
		}

		if resourceInfo.TotalInUse != expectedAmount {
			t.Errorf("Expected total %s usage: %d, actual: %d",
				resource, expectedAmount, resourceInfo.TotalInUse)
		}
	}
}

// verifyTotalResourcesAvailable checks if the total resource availability matches the expected values
func verifyTotalResourcesAvailable(t *testing.T, resp StatusResponse, expectedAvailable map[string]int) {
	t.Helper()
	for resource, expectedAmount := range expectedAvailable {
		resourceInfo, ok := resp.Resources[resource]
		if !ok {
			t.Errorf("Resource %s not found in status response", resource)
			continue
		}

		if resourceInfo.TotalAvailable != expectedAmount {
			t.Errorf("Expected total %s available: %d, actual: %d",
				resource, expectedAmount, resourceInfo.TotalAvailable)
		}
	}
}

func getStatusFromManagementAPI(t *testing.T, managementApiAddress string) StatusResponse {
	resp, err := http.Get(fmt.Sprintf("http://%s/status", managementApiAddress))
	if err != nil {
		t.Fatalf("Failed to get status from management API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	var statusResp StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode status response: %v", err)
	}

	return statusResp
}

// verifyServiceStatus checks if a specific service has the expected running status and resource usage
func verifyServiceStatus(t *testing.T, resp StatusResponse, serviceName string, expectedRunning bool, expectedResources map[string]int) {
	t.Helper()
	// Find service in Services
	var found bool
	var service ServiceStatus

	for _, s := range resp.Services {
		if s.Name == serviceName {
			service = s
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("Service %s not found in Services", serviceName)
	}

	// Check running status
	if service.IsRunning != expectedRunning {
		t.Errorf("Service %s - expected running: %v, actual: %v", serviceName, expectedRunning, service.IsRunning)
	}

	// Check resource usage
	for resource, expectedAmount := range expectedResources {
		if !expectedRunning {
			if expectedAmount != 0 {
				t.Errorf("Service %s - Error in test logic, expected no usage for resource %s when service is not running", serviceName, resource)
			}
			continue
		}
		resourceInfo, ok := resp.Resources[resource]
		if !ok {
			t.Errorf("Resource %s not found in status response", resource)
			continue
		}

		actualAmount, ok := resourceInfo.UsageByService[serviceName]
		if !ok {
			t.Errorf("Service %s not found in UsageByService for resource %s", serviceName, resource)
			continue
		}

		if actualAmount != expectedAmount {
			t.Errorf("Service %s - expected %s usage: %d, actual: %d",
				serviceName, resource, expectedAmount, actualAmount)
		}
	}
}

func assertRemoteClosedWithin(t *testing.T, connection net.Conn, within time.Duration) {
	t.Helper()

	absoluteDeadline := time.Now().Add(within)
	singleByteBuffer := make([]byte, 1)

	for time.Now().Before(absoluteDeadline) {
		_ = connection.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		bytesRead, readErr := connection.Read(singleByteBuffer)

		if readErr == nil {
			if bytesRead > 0 {
				continue
			}
			continue
		}

		if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			continue
		}

		if errors.Is(readErr, io.EOF) || isConnectionReset(readErr) {
			return
		}

		t.Fatalf("unexpected read error while waiting for remote close: %v", readErr)
	}

	t.Fatalf("connection to %s is still open after %s", connection.RemoteAddr(), within)
}
func isConnectionReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED)
}

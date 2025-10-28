package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testImplConnectOnly(test *testing.T, proxyAddress string) {
	_, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		test.Error(err)
		return
	}
	//give large-model-proxy time to start the service, so that it doesn't get killed before it started it
	//which can lead to false positive passing tests
	time.Sleep(1 * time.Second)
}

func testImplConnectWithTimeoutAssertFailure(test *testing.T, proxyAddress string, managementApiAddress string, timeout time.Duration, serviceName string, resourceName string) {
	statusResponse := getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, serviceName, false, map[string]int{resourceName: 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{resourceName: 0})

	expectedFinishTime := time.Now().Add(timeout).Add(3 * time.Second)
	con, _ := net.DialTimeout("tcp", proxyAddress, timeout)
	defer func() {
		_ = con.Close()
	}()
	sleepTime := expectedFinishTime.Sub(time.Now())
	if sleepTime > 0 {
		time.Sleep(sleepTime)
	}

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, serviceName, false, map[string]int{resourceName: 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{resourceName: 0})
}

func testImplConnectTwo2ServersSimultaneouslyAssertBothAreRunning(test *testing.T, proxyOneAddress string, proxyTwoAddress string) {
	pidOne := runReadPidCloseConnection(test, proxyOneAddress)
	clientTwoConnectTime := time.Now()
	pidTwo := runReadPidCloseConnection(test, proxyTwoAddress)
	readDuration := time.Now().Sub(clientTwoConnectTime)
	if readDuration > time.Second*2 {
		test.Fatalf("PID read from second service took %s, expected under 2s", readDuration)
	}
	if !isProcessRunning(pidOne) {
		test.Fatalf("PID %d is not running, but it's supposed to", pidOne)
	}
	if !isProcessRunning(pidTwo) {
		test.Fatalf("PID %d is not running, but it's supposed to", pidTwo)
	}
}

func testIdleTimeout(test *testing.T, proxyAddress string) {
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

func testIdleTimeoutMultipleServices(test *testing.T, serviceOneAddress string, serviceTwoAddress string) {
	connOne, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		test.Error(err)
		return
	}
	time.Sleep(250 * time.Millisecond) //make sure connTwo is not opened before connOne
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
		test.Errorf("first service is still running with PID %d even though it was supposed to be stopped", pidOne)
	}
	err = connTwo.Close()
	if err != nil {
		test.Error(err)
	}
	if !isProcessRunning(pidTwo) {
		test.Errorf("second service is not running with pid %d right after closing connection", pidTwo)
	}

	time.Sleep(1 * time.Second)
	newPid := runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached. Expected %d, got %d", pidTwo, newPid)
	}
	time.Sleep(1 * time.Second)
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached. Expected %d, got %d", pidTwo, newPid)
	}
	time.Sleep(1 * time.Second)
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached. Expected %d, got %d", pidTwo, newPid)
	}
	time.Sleep(1 * time.Second)
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid != pidTwo {
		test.Errorf("second service has changed pid when idle timeout wasn't reached. Expected %d, got %d", pidTwo, newPid)
	}
	if !isProcessRunning(pidTwo) {
		test.Errorf("second service is not running with pid %d right after closing connection two", pidTwo)
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pidTwo) {
		test.Errorf("Process is still running with pid %d after connection is closed and ShutDownAfterInactivitySeconds have passed", pidTwo)
	}

	// Maker sure large-model-proxy hasn't crashed
	newPid = runReadPidCloseConnection(test, serviceTwoAddress)
	if newPid == pidTwo {
		test.Errorf("second Service is reusing old pid, this should not be possible")
	}

	runReadPidCloseConnection(test, serviceOneAddress)
}

func testHalfCloseClientCloseWriteIdleTimeout(t *testing.T) {
	conn, err := net.Dial("tcp", "localhost:2029")
	if err != nil {
		t.Fatalf("Could not open %s: %v", "localhost:2029", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	pid := readPidFromOpenConnection(t, conn)
	if pid == 0 {
		return
	}
	if !isProcessRunning(pid) {
		t.Fatalf("Service process %d is not running after reading PID", pid)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	time.Sleep(5 * time.Second)

	if isProcessRunning(pid) {
		t.Errorf("Service process %d is still running after idle timeout, expected it to exit", pid)
	}
	assertPortsAreClosed(t, []string{"localhost:12029"})
}

func testClientClose(t *testing.T, address1 string, address1Internal string, address2 string, clientCallBackAfterReading func(conn *net.Conn)) {
	connOne, err := net.Dial("tcp", address1)
	if err != nil {
		t.Fatalf("Could not open %s: %v", address1, err)
	}
	defer func() {
		_ = connOne.Close()
	}()

	pidOne := readPidFromOpenConnection(t, connOne)
	if !isProcessRunning(pidOne) {
		t.Fatalf("Service process %d is not running after reading", pidOne)
	}

	clientCallBackAfterReading(&connOne)
	clientCloseTime := time.Now()

	connTwo, err := net.Dial("tcp", address2)
	if err != nil {
		t.Fatalf("Could not open %s: %v", address2, err)
	}
	defer func() {
		_ = connTwo.Close()
	}()

	readPidFromOpenConnection(t, connTwo)
	readDuration := time.Now().Sub(clientCloseTime)
	if readDuration > time.Second*2 {
		t.Fatalf("PID read from second service took %s, expected under 2s", readDuration)
	}
	t.Logf("PID read from second service took %s", readDuration)
	if isProcessRunning(pidOne) {
		t.Fatalf("%d is still running even though it was supposed to be closed once second connection was handled", pidOne)
	}
	assertPortsAreClosed(t, []string{address1Internal})
}

func testOpenAiApi(test *testing.T) {
	//sanity check  that nothing is running before initial connection
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	client := &http.Client{}
	resp := modelsRequestExpectingSuccess(test, "http://localhost:2016/v1/models", client)
	assertModelsResponse(test, []string{"openai-api_openai-api-1", "fizz", "buzz"}, resp)

	resp = sendCompletionRequest(test, "http://localhost:2016", OpenAiApiCompletionRequest{
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

	testCompletionRequest(test, "http://localhost:2016", "openai-api_openai-api-1", nil)
	assertPortsAreClosed(test, []string{"localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	testCompletionStreamingExpectingSuccess(test, "openai-api_openai-api-1")
	testChatCompletionRequestExpectingSuccess(test, "http://localhost:2016", "openai-api_openai-api-1")
	testChatCompletionStreamingExpectingSuccess(test, "http://localhost:2016", "openai-api_openai-api-1")

	llm1Pid := runReadPidCloseConnection(test, "localhost:12018")
	assertPortsAreClosed(test, []string{"localhost:12019", "localhost:12020", "localhost:12021", "localhost:12022", "localhost:12023"})

	time.Sleep(4 * time.Second)

	if isProcessRunning(llm1Pid) {
		test.Fatalf("openai-api_openai-api-1 service is still running, but inactivity timeout should have shut it down by now")
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
		test.Fatalf("openai-api_openai-api-2 service is still running, but inactivity timeout should have shut it down by now")
	}

	testCompletionRequest(test, "http://localhost:2016", "buzz", nil)
	llm2Pid = runReadPidCloseConnection(test, "localhost:12020")
	time.Sleep(4 * time.Second)
	assertPortsAreClosed(test, []string{"localhost:12017", "localhost:12018", "localhost:12021", "localhost:12022", "localhost:12023"})
	if isProcessRunning(llm2Pid) {
		test.Fatalf("openai-api_openai-api-2 service is still running, but inactivity timeout should have shut it down by now")
	}

	testCompletionRequest(test, "http://localhost:2019", "foo", nil)
	llm2Pid = runReadPidCloseConnection(test, "localhost:12020")
	time.Sleep(4 * time.Second)
	if isProcessRunning(llm2Pid) {
		test.Fatalf("openai-api_openai-api-2 service is still running, but inactivity timeout should have shut it down by now")
	}
	assertPortsAreClosed(test, []string{"localhost:12011", "localhost:12012", "localhost:12013", "localhost:12014", "localhost:12016", "localhost:12017", "localhost:12018"})
}

func testOpenAiApiReusingConnection(test *testing.T) {
	//sanity check  that nothing is running before initial connection
	assertPortsAreClosed(test, []string{"localhost:12025", "localhost:12026"})
	client := &http.Client{}
	resp := modelsRequestExpectingSuccess(test, "http://localhost:2024/v1/models", client)
	assertModelsResponse(test, []string{"openai-api-keep-alive_service0"}, resp)
	resp = modelsRequestExpectingSuccess(test, "http://localhost:2024/v1/models", client)
	assertModelsResponse(test, []string{"openai-api-keep-alive_service0"}, resp)

	testCompletionRequest(test, "http://localhost:2024", "openai-api-keep-alive_service0", client)
	testCompletionRequest(test, "http://localhost:2024", "openai-api-keep-alive_service0", client)
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
	testCompletionRequest(test, "http://localhost:2024", "openai-api-keep-alive_service0", client)

	err = resp.Body.Close()
	if err != nil {
		test.Error(err)
	}
}

// testCompletionStreamingExpectingSuccess checks streaming completions from /v1/completions
func testCompletionStreamingExpectingSuccess(t *testing.T, model string) {
	address := "http://localhost:2016"
	testPrompt := "This is a test prompt\nЭто проверочный промт\n这是一个测试提示"
	reqBodyStruct := OpenAiApiCompletionRequest{
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
			var chunkResp OpenAiApiCompletionResponse
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

func getModelByIDRequestExpectingSuccess(test *testing.T, baseAddress string, modelID string, httpClient *http.Client) {
	requestedEndpointUrl := fmt.Sprintf("%s/v1/models/%s", strings.TrimRight(baseAddress, "/"), url.QueryEscape(modelID))

	req, err := http.NewRequest(http.MethodGet, requestedEndpointUrl, nil)
	if err != nil {
		test.Fatalf("Failed to create GET %s: %v", requestedEndpointUrl, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		test.Errorf("GET %s failed: %v", requestedEndpointUrl, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		test.Errorf("GET %s: expected 200, got %d", requestedEndpointUrl, resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		test.Errorf("GET %s: expected application/json, got %q", requestedEndpointUrl, contentType)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		test.Errorf("GET %s: failed to read body: %v", requestedEndpointUrl, err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
		test.Errorf("GET %s: failed to unmarshal JSON: %v\nBody: %s", requestedEndpointUrl, err, string(bodyBytes))
	}

	rawID, ok := decoded["id"]
	if !ok {
		test.Errorf("GET %s: JSON missing 'id' field", requestedEndpointUrl)
	}
	idString, ok := rawID.(string)
	if !ok {
		test.Errorf("GET %s: 'id' is not a string: %#v", requestedEndpointUrl, rawID)
	}
	if idString != modelID {
		test.Errorf("GET %s: id mismatch, expected %q, got %q", requestedEndpointUrl, modelID, idString)
	}

	if rawObject, ok := decoded["object"]; ok {
		if objectString, ok := rawObject.(string); !ok || objectString != "model" {
			test.Errorf("GET %s: unexpected 'object' value: %#v", requestedEndpointUrl, rawObject)
		}
	}
	if rawOwnedBy, ok := decoded["owned_by"]; ok {
		if objectString, ok := rawOwnedBy.(string); !ok || objectString != "large-model-proxy" {
			test.Errorf("GET %s: unexpected 'owned_by' value: %#v", requestedEndpointUrl, rawOwnedBy)
		}
	}

	_, hasCreated := decoded["created"]
	if !hasCreated {
		test.Errorf("GET %s: JSON missing 'created' field", requestedEndpointUrl)
	} else {
		var strict struct {
			Created int64 `json:"created"`
		}
		if err := json.Unmarshal(bodyBytes, &strict); err != nil {
			test.Errorf("GET %s: 'created' must be integer Unix seconds: %v\nBody: %s", requestedEndpointUrl, err, string(bodyBytes))
		} else {
			createdTime := time.Unix(strict.Created, 0)
			now := time.Now()
			if createdTime.After(now.Add(25 * time.Hour)) {
				test.Errorf("GET %s: 'created' %s is more than 25h in the future (now=%s)", requestedEndpointUrl, createdTime.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
			}
		}
	}
}

// getModelByIDRequestExpectingNotFound performs GET /v1/models/{modelID} and asserts a 404.
func getModelByIDRequestExpectingNotFound(test *testing.T, baseAddress string, modelID string, httpClient *http.Client) {
	url := fmt.Sprintf("%s/v1/models/%s", strings.TrimRight(baseAddress, "/"), modelID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		test.Fatalf("Failed to create GET %s: %v", url, err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		test.Fatalf("GET %s failed: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		bodyPreview, _ := io.ReadAll(resp.Body)
		test.Fatalf("GET %s: expected 404 for missing model, got %d. Body: %s", url, resp.StatusCode, string(bodyPreview))
	}
}

func testOpenAiApiModelsByID(
	test *testing.T,
	openAiApiAddress string,
	expectedModelIDs []string,
	missingModelIDs []string,
) {
	httpClient := &http.Client{Timeout: 15 * time.Second}

	for _, modelID := range expectedModelIDs {
		getModelByIDRequestExpectingSuccess(test, openAiApiAddress, modelID, httpClient)
	}

	for _, missingModelID := range missingModelIDs {
		getModelByIDRequestExpectingNotFound(test, openAiApiAddress, missingModelID, httpClient)
	}
}

func testCompletionRequest(test *testing.T, address string, model string, client *http.Client) {
	testPrompt := "This is a test prompt\nЭто проверочный промт\n这是一个测试提示"

	// Prepare request body
	completionReq := OpenAiApiCompletionRequest{
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

	chatReq := OpenAiApiChatCompletionRequest{
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
	testStreamingRequest(t, url, OpenAiApiChatCompletionRequest{
		Model:    model,
		Messages: messages,
		Stream:   true,
	}, []string{
		"Hello, this is chunk #1.",
		"Your last message was:\n",
		"Thanks\nfor\nnothing!",
		"", //done chunk which doesn't have a delta
	}, func(t *testing.T, payload string) string {
		var chunkResp OpenAiApiChatCompletionResponse
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

func testVerifyArgsAndEnv(test *testing.T, procPort string, mustHaveEnv bool) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/procinfo", procPort), nil)
	if err != nil {
		test.Fatalf("Failed to create request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		test.Fatalf("/procinfo Request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		test.Fatalf("Expected status code OK, got %d", resp.StatusCode)
	}
	value, err := io.ReadAll(resp.Body)
	if err != nil {
		test.Error(err)
	}

	var result map[string]any
	err = json.Unmarshal(value, &result)
	if err != nil {
		test.Error(err)
	}

	if len(result) == 0 {
		test.Fatal("Expected response to have non-empty value, got empty")
	}

	serverArgs := result["args"].([]any)
	for index, arg := range serverArgs {
		if arg.(string) == "" {
			test.Fatalf("Found empty arg at index %d in args = %v", index, serverArgs)
		}
	}

	hasEnv := false
	serverEnv := result["env"].([]any)
	for _, envString := range serverEnv {
		envParts := strings.Split(envString.(string), "=")
		key, value := envParts[0], envParts[1]
		if key == "COOL_VARIABLE" && value == "1" {
			hasEnv = true
		}
		if key == "COOL_VARIABLE" && value != "1" {
			test.Fatalf("COOL_VARIABLE is not set to 1, it is %s", value)
		}
	}

	if mustHaveEnv && !hasEnv {
		test.Fatalf("COOL_VARIABLE not set")
	}

	err = resp.Body.Close()
	if err != nil {
		test.Error(err)
	}
}

func testKillCommand(test *testing.T, proxyAddress string) {
	const killCommandOutputFile = "/tmp/test-server-kill-command-output"

	// Delete the kill command output file if it exists
	err := os.Remove(killCommandOutputFile)
	if err != nil && !os.IsNotExist(err) {
		test.Errorf("Failed to delete kill command output file: %v", err)
	}

	pid := runReadPidCloseConnection(test, proxyAddress)
	if pid == 0 {
		//runReadPidCloseConnection already failed the test
		return
	}
	_, err = os.ReadFile(killCommandOutputFile)
	if err == nil {
		test.Errorf("File \"%s\" exists before kill command was supposed to run", killCommandOutputFile)
	} else if !os.IsNotExist(err) {
		test.Errorf("Unexpected error trying to read \"%s\", expecting file to not exist instead", killCommandOutputFile)
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pid) {
		test.Errorf("Process is still running after connection is closed and ShutDownAfterInactivitySeconds have passed")
	}

	// Check if the kill command output file was created and is 'success'
	content, err := os.ReadFile(killCommandOutputFile)
	if err != nil {
		test.Errorf("Failed to read kill command output file: %v", err)
	}
	if string(content) != "success" {
		test.Errorf("Kill command output file content is not 'success', it is '%s'", string(content))
	}
}
func testDyingProcesses(test *testing.T,
	proxiedSelfDyingServiceAddress string,
	directSelfDyingServiceAddress string,
	proxiedNotDyingServiceAddress string,
	directNotDyingServiceAddress string,
	managementApiAddress string,
) {
	assertPortsAreClosed(test, []string{directSelfDyingServiceAddress})
	pid := runReadPidCloseConnection(test, proxiedSelfDyingServiceAddress)
	conn, err := net.Dial("tcp", proxiedSelfDyingServiceAddress)
	defer conn.Close()
	if err != nil {
		test.Errorf("Failed to open second connection to the self-dying service: %v", err)
	}
	buffer := make([]byte, 1024)
	bytesRead, err := conn.Read(buffer)
	if err != nil {
		test.Fatalf("Error when trying to read PID: %v", err)
	}
	conn2, err := net.Dial("tcp", proxiedNotDyingServiceAddress)
	defer conn2.Close()

	//Not-dying service should not start yet, the self-dying service is still running
	assertPortsAreClosed(test, []string{directNotDyingServiceAddress})
	time.Sleep(1250 * time.Millisecond)
	if isProcessRunning(pid) {
		test.Errorf("test-server is still running when it was supposed to exit")
	}

	assertPortsAreClosed(test, []string{directSelfDyingServiceAddress})

	bytesRead, err = conn.Read(buffer)
	if err == nil || err != io.EOF {
		test.Fatalf("Expected connection to the server to be closed, got %v. read %d bytes: %s", err, bytesRead, buffer)
	}

	statusResponse := getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", false, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", true, map[string]int{"CPU": 1})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 1})

	pid2 := readPidFromOpenConnection(test, conn2)
	if !isProcessRunning(pid2) {
		test.Fatalf("second service is not running")
	}
	err = conn2.Close()
	if err != nil {
		test.Error(err)
	}

	conn3, err := net.Dial("tcp", proxiedNotDyingServiceAddress)
	if err != nil {
		test.Fatalf("Failed to open connection to proxied %s: %v", proxiedNotDyingServiceAddress, err)
	}
	defer conn3.Close()
	pid3 := readPidFromOpenConnection(test, conn3)

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", false, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", true, map[string]int{"CPU": 1})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 1})
	err = syscall.Kill(pid2, syscall.SIGINT)
	if err != nil {
		test.Fatalf("Failed to kill second service: %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", false, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 0})

	if isProcessRunning(pid) {
		test.Errorf("test-server is still running when it was supposed to exit")
	}
	if isProcessRunning(pid3) {
		test.Errorf("test-server is still running when it was supposed to exit")
	}

	_, err = conn3.Read(buffer)
	if err == nil || err != io.EOF {
		test.Fatalf("Expected connection to the server to be closed, got %v", err)
	}
	if isProcessRunning(pid3) {
		test.Errorf("test-server is still running when it was supposed to exit")
	}

	pid = runReadPidCloseConnection(test, proxiedSelfDyingServiceAddress)

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", true, map[string]int{"CPU": 1})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 1})

	time.Sleep(1250 * time.Millisecond)
	if isProcessRunning(pid) {
		test.Errorf("test-server is still running when it was supposed to exit")
	}

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", false, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 0})

	//verify that a service can restart after it died
	runReadPidCloseConnection(test, proxiedSelfDyingServiceAddress)
}
func testFailingToStartServiceIsCleaningUpResources(
	test *testing.T,
	proxyAddress string,
	managementApiAddress string,
	processName string,
	resourceName string,
) {
	statusResponse := getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, processName, false, map[string]int{resourceName: 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{resourceName: 0})

	con, _ := net.DialTimeout("tcp", proxyAddress, time.Duration(3)*time.Second)
	defer func() {
		_ = con.Close()
	}()
	assertRemoteClosedWithin(test, con, 2*time.Second)
	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, processName, false, map[string]int{resourceName: 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{resourceName: 0})
}
func TestAppScenarios(test *testing.T) {
	test.Parallel()
	tests := []struct {
		Name                          string
		GetConfig                     func(t *testing.T, testName string) Config
		AddressesToCheckAfterStopping []string
		TestFunc                      func(t *testing.T)
		SetupFunc                     func(t *testing.T)
	}{
		{
			Name: "minimal",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:      "2000",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12000",
							Command:         "./test-server/test-server",
							Args:            "-p 12000",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2000", "localhost:12000"},
			TestFunc: func(t *testing.T) {
				testImplMinimal(t, "localhost:2000")
			},
		},
		{
			Name: "no-resource-requirements",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{"VRAM": {Amount: 20}},
					Services: []ServiceConfig{
						{
							ListenPort:           "2032",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12032",
							Command:              "./test-server/test-server",
							Args:                 "-p 12032",
							ResourceRequirements: map[string]int{"VRAM": 20},
						},
						{
							ListenPort:      "2033",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12033",
							Command:         "./test-server/test-server",
							Args:            "-p 12033",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2032", "localhost:12032", "localhost:2033", "localhost:12033"},
			TestFunc: func(t *testing.T) {
				testImplConnectTwo2ServersSimultaneouslyAssertBothAreRunning(t, "localhost:2032", "localhost:2033")
			},
		},
		{
			Name: "healthcheck",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:                      "2001",
							ProxyTargetHost:                 "localhost",
							ProxyTargetPort:                 "12001",
							Command:                         "./test-server/test-server",
							Args:                            "-p 12001 -healthcheck-port 2011 -sleep-before-listening 10s -sleep-before-listening-for-healthcheck 3s -startup-duration 5s",
							HealthcheckCommand:              "curl --fail http://localhost:2011",
							HealthcheckIntervalMilliseconds: 200,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2001", "localhost:12001", "localhost:2011"},
			TestFunc: func(t *testing.T) {
				testImplMinimal(t, "localhost:2001")
			},
		},
		{
			Name: "healthcheck-immediate-listen-start",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:                      "2002",
							ProxyTargetHost:                 "localhost",
							ProxyTargetPort:                 "12002",
							Command:                         "./test-server/test-server",
							Args:                            "-p 12002 -healthcheck-port 2012 -sleep-before-listening-for-healthcheck 3s -startup-duration 5s",
							HealthcheckCommand:              "curl --fail http://localhost:2012",
							HealthcheckIntervalMilliseconds: 200,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2002", "localhost:12002", "localhost:2012"},
			TestFunc: func(t *testing.T) {
				testImplMinimal(t, "localhost:2002")
			},
		},
		{
			Name: "healthcheck-immediate-startup-delayed-healthcheck",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:                      "2003",
							ProxyTargetHost:                 "localhost",
							ProxyTargetPort:                 "12003",
							Command:                         "./test-server/test-server",
							Args:                            "-p 12003 -healthcheck-port 2013 -sleep-before-listening-for-healthcheck 3s -startup-duration 5s",
							HealthcheckCommand:              "curl --fail http://localhost:2013",
							HealthcheckIntervalMilliseconds: 200,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2003", "localhost:12003", "localhost:2013"},
			TestFunc: func(t *testing.T) {
				testImplMinimal(t, "localhost:2003")
			},
		},
		{
			Name: "healthcheck-immediate-startup",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:                      "2004",
							ProxyTargetHost:                 "localhost",
							ProxyTargetPort:                 "12004",
							Command:                         "./test-server/test-server",
							Args:                            "-p 12004 -healthcheck-port 2014",
							HealthcheckCommand:              "curl --fail http://localhost:2014",
							HealthcheckIntervalMilliseconds: 200,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2004", "localhost:2014"},
			TestFunc: func(t *testing.T) {
				testImplMinimal(t, "localhost:2004")
			},
		},
		{
			Name: "healthcheck-stuck",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:                      "2005",
							ProxyTargetHost:                 "localhost",
							ProxyTargetPort:                 "12005",
							Command:                         "./test-server/test-server",
							Args:                            "-p 12005 -healthcheck-port 2015",
							HealthcheckCommand:              "false",
							HealthcheckIntervalMilliseconds: 200,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2005", "localhost:12005", "localhost:2015"},
			TestFunc: func(t *testing.T) {
				testImplConnectOnly(t, "localhost:2005")
			},
		},
		{
			Name: "healthcheck-stuck-timeout",
			GetConfig: func(t *testing.T, testName string) Config {
				timeoutMs := uint(2000)
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 1}},
					ManagementApi: ManagementApi{
						ListenPort: "2065",
					},
					Services: []ServiceConfig{
						{
							ListenPort:                      "2064",
							ProxyTargetHost:                 "localhost",
							ProxyTargetPort:                 "12064",
							Command:                         "./test-server/test-server",
							Args:                            "-p 12064 -startup-duration 24h",
							HealthcheckCommand:              "false",
							HealthcheckIntervalMilliseconds: 200,
							StartupTimeoutMilliseconds:      &timeoutMs,
							ResourceRequirements:            map[string]int{"CPU": 1},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:12064", "localhost:2065", "localhost:2064"},
			TestFunc: func(t *testing.T) {
				testImplConnectWithTimeoutAssertFailure(
					t,
					"localhost:2064",
					"localhost:2065",
					time.Duration(2000)*time.Millisecond,
					"healthcheck-stuck-timeout_service0",
					"CPU",
				)
			},
		},
		{
			Name: "service-stuck-no-healthcheck",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:      "2006",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12006",
							Command:         "./test-server/test-server",
							Args:            "-p 12006 -startup-duration 24h",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2006"},
			TestFunc: func(t *testing.T) {
				testImplConnectOnly(t, "localhost:2006")
			},
		},
		{
			Name: "idle-timeout",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ShutDownAfterInactivitySeconds: 3,
					Services: []ServiceConfig{
						{
							ListenPort:      "2007",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12007",
							Command:         "./test-server/test-server",
							Args:            "-p 12007",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2007"},
			TestFunc: func(t *testing.T) {
				testIdleTimeout(t, "localhost:2007")
			},
		},
		{
			Name: "idle-timeout-after-stop",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ShutDownAfterInactivitySeconds: 3,
					ResourcesAvailable:             map[string]ResourceAvailable{"RAM": {Amount: 1}},
					Services: []ServiceConfig{
						{
							ListenPort:           "2008",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12008",
							Command:              "./test-server/test-server",
							Args:                 "-p 12008 -request-processing-duration 2s",
							ResourceRequirements: map[string]int{"RAM": 1},
						},
						{
							ListenPort:           "2009",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12009",
							Command:              "./test-server/test-server",
							Args:                 "-p 12009",
							ResourceRequirements: map[string]int{"RAM": 1},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2008", "localhost:2009"},
			TestFunc: func(t *testing.T) {
				testIdleTimeoutMultipleServices(t, "localhost:2008", "localhost:2009")
			},
		},
		{
			Name: "client-close-full",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{"VRAM": {Amount: 1}},
					Services: []ServiceConfig{
						{
							ListenPort:           "2030",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12030",
							Command:              "./test-server/test-server",
							Args:                 "-p 12030 -sleep-after-writing-pid-duration 10s",
							ResourceRequirements: map[string]int{"VRAM": 1},
						},
						{
							ListenPort:           "2031",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12031",
							Command:              "./test-server/test-server",
							Args:                 "-p 12031",
							ResourceRequirements: map[string]int{"VRAM": 1},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2030", "localhost:12030", "localhost:2031", "localhost:12031"},
			TestFunc: func(t *testing.T) {
				testClientClose(t, "localhost:2030",
					"localhost:12030",
					"localhost:2031",
					func(conn *net.Conn) {
						if err := (*conn).Close(); err != nil {
							t.Fatalf("Close failed: %v", err)
						}
					})
			},
		},
		{
			Name: "client-close-full-idle-timeout",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ShutDownAfterInactivitySeconds: 3,
					Services: []ServiceConfig{
						{
							ListenPort:      "2029",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12029",
							Command:         "./test-server/test-server",
							Args:            "-p 12029 -sleep-after-writing-pid-duration 10s",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2029", "localhost:12029"},
			TestFunc: func(t *testing.T) {
				testHalfCloseClientCloseWriteIdleTimeout(t)
			},
		},
		{
			Name: "openai-api",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					OpenAiApi:                      OpenAiApi{ListenPort: "2016"},
					ShutDownAfterInactivitySeconds: 3,
					Services: []ServiceConfig{
						{
							Name:            "openai-api-1",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12017",
							Command:         "./test-server/test-server",
							Args:            "-openai-api-port 12017 -p 12018",
							OpenAiApi:       true,
						},
						{
							Name:            "openai-api-2",
							ListenPort:      "2019",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12019",
							Command:         "./test-server/test-server",
							Args:            "-openai-api-port 12019 -p 12020",
							OpenAiApi:       true,
							OpenAiApiModels: []string{"fizz", "buzz"},
						},
						{
							Name:            "non-llm-1",
							ListenPort:      "2021",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12021",
							Command:         "./test-server/test-server",
							Args:            "-p 12021",
							OpenAiApi:       false,
						},
						{
							Name:            "non-llm-2",
							ListenPort:      "2022",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12022",
							Command:         "./test-server/test-server",
							Args:            "-openai-api-port 12022 -p 12023",
							OpenAiApi:       false,
						},
					},
				}
			},
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
				testOpenAiApi(t)
			},
		},
		{
			Name: "openai-api-keep-alive",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					OpenAiApi: OpenAiApi{ListenPort: "2024"},
					Services: []ServiceConfig{
						{
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12025",
							Command:         "./test-server/test-server",
							Args:            "-openai-api-port 12025 -p 12026",
							OpenAiApi:       true,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2024",
				"localhost:12025",
				"localhost:12026",
			},
			TestFunc: func(t *testing.T) {
				testOpenAiApiReusingConnection(t)
			},
		},
		{
			Name: "openai-api-models-by-id",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					OpenAiApi: OpenAiApi{ListenPort: "2071"},
					Services: []ServiceConfig{
						{
							Name:            "openai-api-1",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12072",
							Command:         "./test-server/test-server",
							Args:            "-p 12072",
							OpenAiApi:       true,
						},
						{
							Name:            "openai-api-2",
							ListenPort:      "2073",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "120723",
							Command:         "./test-server/test-server",
							Args:            "-p 12073",
							OpenAiApi:       true,
							OpenAiApiModels: []string{"fizz", "buzz", "$-_.+!*'(),проверка"},
						},
						{
							Name:            "non-llm-1",
							ListenPort:      "2074",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12074",
							Command:         "./test-server/test-server",
							Args:            "-p 12074",
							OpenAiApi:       false,
						},
						{
							Name:            "$-_.+!*'(),проверка-2/",
							ListenPort:      "2075",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12075",
							Command:         "./test-server/test-server",
							Args:            "-p 12075",
							OpenAiApi:       true,
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2071",
				"localhost:2072",
				"localhost:2073",
				"localhost:2074",
				"localhost:12072",
				"localhost:12073",
				"localhost:12074",
			},
			TestFunc: func(t *testing.T) {
				expectedModelIDs := []string{
					"openai-api-models-by-id_openai-api-1",
					"fizz",
					"buzz",
					"$-_.+!*'(),проверка",
					"openai-api-models-by-id_$-_.+!*'(),проверка-2/",
				}
				missingModelIDs := []string{
					"totally-non-existent-model",
					"non-llm-1",
					"$-_.+!*'(),проверка-2",
				}
				testOpenAiApiModelsByID(t, "http://localhost:2071", expectedModelIDs, missingModelIDs)
			},
		},
		{
			Name: "args-with-whitespace",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:      "2025",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12027",
							Command:         "./test-server/test-server",
							Args:            "   -procinfo-port 12027",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2025",
				"localhost:12027",
			},
			TestFunc: func(t *testing.T) {
				testVerifyArgsAndEnv(t, "2025", false)
			},
		},
		{
			Name: "args-with-env",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							ListenPort:      "2026",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12028",
							Command:         "env",
							Args:            "COOL_VARIABLE=1 ./test-server/test-server -procinfo-port 12028",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2026",
				"localhost:12028",
			},
			TestFunc: func(t *testing.T) {
				testVerifyArgsAndEnv(t, "2026", true)
			},
		},
		{
			Name: "kill-command",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ShutDownAfterInactivitySeconds: 3,
					Services: []ServiceConfig{
						{
							ListenPort:      "2034",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12034",
							Command:         "./test-server/test-server",
							Args:            "-p 12034",
							KillCommand:     ptrToString("printf 'success' > /tmp/test-server-kill-command-output"),
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2034", "localhost:12034"},
			TestFunc: func(t *testing.T) {
				testKillCommand(t, "localhost:2034")
			},
		},
		{
			Name: "dying-processes",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"CPU": {Amount: 1},
					},
					ManagementApi: ManagementApi{
						ListenPort: "2035",
					},
					Services: []ServiceConfig{
						{
							Name:            "self-dying-process",
							ListenPort:      "2036",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12036",
							Command:         "./test-server/test-server",
							Args:            "-p 12036 -exit-after-duration 1s --sleep-after-writing-pid-duration 3s",
							ResourceRequirements: map[string]int{
								"CPU": 1,
							},
						},
						{
							Name:            "not-dying-process",
							ListenPort:      "2037",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12037",
							Command:         "./test-server/test-server",
							Args:            "-p 12037 --sleep-after-writing-pid-duration 3s",
							ResourceRequirements: map[string]int{
								"CPU": 1,
							},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2035",
				"localhost:2036",
				"localhost:12036",
				"localhost:2037",
				"localhost:12037",
			},
			TestFunc: func(t *testing.T) {
				testDyingProcesses(t,
					"localhost:2036",
					"localhost:12036",
					"localhost:2037",
					"localhost:12037",
					"localhost:2035",
				)
			},
		},
		{
			Name: "failed-to-start-process-exit-immediately",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"CPU": {Amount: 1},
					},
					ManagementApi: ManagementApi{
						ListenPort: "2067",
					},
					Services: []ServiceConfig{
						{
							ListenPort:      "2068",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12068",
							Command:         "exit",
							Args:            "1",
							ResourceRequirements: map[string]int{
								"CPU": 1,
							},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2067",
			},
			TestFunc: func(t *testing.T) {
				testFailingToStartServiceIsCleaningUpResources(t,
					"localhost:2068",
					"localhost:2067",
					"failed-to-start-process-exit-immediately_service0",
					"CPU",
				)
			},
		},
		{
			Name: "failed-to-start-process-exit-after-sleep",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"CPU": {Amount: 1},
					},
					ManagementApi: ManagementApi{
						ListenPort: "2069",
					},
					Services: []ServiceConfig{
						{
							ListenPort:      "2070",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12070",
							Command:         "sleep",
							Args:            "1",
							ResourceRequirements: map[string]int{
								"CPU": 1,
							},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2069",
			},
			TestFunc: func(t *testing.T) {
				testFailingToStartServiceIsCleaningUpResources(t,
					"localhost:2070",
					"localhost:2069",
					"failed-to-start-process-exit-after-sleep_service0",
					"CPU",
				)
			},
		},
		{
			Name: "unmonitored-process",
			GetConfig: func(t *testing.T, testName string) Config {
				monitorProcessStatus := false
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"CPU": {Amount: 1},
					},
					ManagementApi: ManagementApi{
						ListenPort: "2046",
					},
					Services: []ServiceConfig{
						{
							Name:                           "self-dying-unmonitored-process",
							ListenPort:                     "2038",
							ProxyTargetHost:                "localhost",
							ProxyTargetPort:                "12038",
							Command:                        "./test-server/test-server",
							Args:                           "-p 12038 -exit-after-duration 1s",
							ShutDownAfterInactivitySeconds: 3,
							ConsiderStoppedOnProcessExit:   &monitorProcessStatus,
							RestartOnConnectionFailure:     false,
							ResourceRequirements: map[string]int{
								"CPU": 1,
							},
						},
						{
							Name:                         "non-dying-process",
							ListenPort:                   "2039",
							ProxyTargetHost:              "localhost",
							ProxyTargetPort:              "12039",
							Command:                      "./test-server/test-server",
							Args:                         "-p 12039",
							ConsiderStoppedOnProcessExit: &monitorProcessStatus,
							RestartOnConnectionFailure:   false,
							ResourceRequirements: map[string]int{
								"CPU": 1,
							},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2038",
				"localhost:12038",
				"localhost:2039",
				"localhost:12039",
				"localhost:2046",
			},
			TestFunc: func(t *testing.T) {
				testUnmonitoredProcess(t,
					"localhost:2038",
					"localhost:12038",
					"localhost:2039",
					"localhost:2046",
				)
			},
		},
		{
			Name: "logs-output",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					Services: []ServiceConfig{
						{
							Name:            "service1",
							ListenPort:      "2049",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12049",
							Command:         "./test-server/test-server",
							Args:            "-p 12049 --plain-output",
						},
						{
							Name:            "Service TWO2️⃣ Два",
							ListenPort:      "2054",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12054",
							Command:         "./test-server/test-server",
							Args:            "-p 12054 --plain-output --log-to-stdout",
						},
						{
							Name:            "{Service 3}",
							ListenPort:      "2057",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12057", //nothing is actually listening there
							Command:         "./test-server/output-test.sh",
						},
						{
							Name:            "[Service 4]",
							ListenPort:      "2058",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12058", //nothing is actually listening there
							Command:         "./test-server/output-test.sh",
							Args:            "-stderr",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2049",
				"localhost:12049",
				"localhost:2054",
				"localhost:12054",
				"localhost:2057",
				"localhost:2058",
			},
			SetupFunc: func(t *testing.T) {
				err := os.Remove("test-logs/test_logs-output.log")
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("Failed to remove test-logs/test_logs-output.log: %v", err)
				}
			},
			TestFunc: func(t *testing.T) {
				testLogOutput(t,
					"localhost:2049",
					"localhost:2054",
					"localhost:2057",
					"localhost:2058",
					12049,
					12054,
					"logs-output_service1",
					"logs-output_Service TWO2️⃣ Два",
					"logs-output_{Service 3}",
					"logs-output_[Service 4]",
					true,
				)
			},
		},
		{
			Name: "logs-no-output",
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					OutputServiceLogs: new(bool),
					Services: []ServiceConfig{
						{
							Name:            "service1",
							ListenPort:      "2055",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12055",
							Command:         "./test-server/test-server",
							Args:            "-p 12055 --plain-output",
						},
						{
							Name:            "Service TWO2️⃣ Два",
							ListenPort:      "2056",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12056",
							Command:         "./test-server/test-server",
							Args:            "-p 12056 --plain-output --log-to-stdout",
						},
						{
							Name:            "{Service 3}",
							ListenPort:      "2059",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12059", //nothing is actually listening there
							Command:         "./test-server/output-test.sh",
						},
						{
							Name:            "[Service 4]",
							ListenPort:      "2060",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12060", //nothing is actually listening there
							Command:         "./test-server/output-test.sh",
							Args:            "-stderr",
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2055",
				"localhost:12055",
				"localhost:2056",
				"localhost:12056",
				"localhost:2059",
				"localhost:2060",
			},
			SetupFunc: func(t *testing.T) {
				err := os.Remove("test-logs/test_logs-no-output.log")
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("Failed to remove test-logs/test_logs-no-output.log: %v", err)
				}
			},
			TestFunc: func(t *testing.T) {
				testLogOutput(t,
					"localhost:2055",
					"localhost:2056",
					"localhost:2059",
					"localhost:2060",
					12055,
					12056,
					"logs-no-output_service1",
					"logs-no-output_Service TWO2️⃣ Два",
					"logs-no-output_{Service 3}",
					"logs-no-output_[Service 4]",
					false,
				)
			},
		}, {
			Name: "startup-timeout-cleanup",
			GetConfig: func(t *testing.T, testName string) Config {
				timeoutMs := uint(3000)
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 2}},
					ManagementApi:      ManagementApi{ListenPort: "2063"},
					Services: []ServiceConfig{
						{
							Name:                           "fast-start",
							ListenPort:                     "2061",
							ProxyTargetHost:                "localhost",
							ProxyTargetPort:                "12061",
							Command:                        "./test-server/test-server",
							Args:                           "-p 12061 --sleep-after-writing-pid-duration 10s",
							ShutDownAfterInactivitySeconds: 1,
							StartupTimeoutMilliseconds:     &timeoutMs,
							ResourceRequirements:           map[string]int{"CPU": 1},
						},
						{
							Name:                       "slow-start-fail",
							ListenPort:                 "2062",
							ProxyTargetHost:            "localhost",
							ProxyTargetPort:            "12062",
							Command:                    "./test-server/test-server",
							Args:                       "-p 12062 -sleep-before-listening 10s -healthcheck-port 2066",
							StartupTimeoutMilliseconds: &timeoutMs,
							ResourceRequirements:       map[string]int{"CPU": 1},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2061",
				"localhost:12061",
				"localhost:2062",
				"localhost:12062",
				"localhost:2063",
				"localhost:2066",
			},
			TestFunc: func(t *testing.T) {
				testStartupTimeoutCleansResourcesAndClosesClientConnections(
					t,
					"startup-timeout-cleanup",
					"localhost:2061",
					"localhost:2062",
					"localhost:12062",
					"localhost:2066",
					"localhost:2063",
				)
			},
		},
		{
			Name: "resource-check-command",
			TestFunc: func(t *testing.T) {
				testResourceCheckCommand(t, "localhost:2077", "localhost:2076", "TestResource", "resource-check-command_service0")
			},
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"TestResource": {
							//this command increments a number in the file by one every time it runs
							CheckCommand:              "read -r original_integer < resource-check-command.counter.txt.txt; incremented_integer=$((original_integer + 1)); printf '%d\n' \"$incremented_integer\" | tee resource-check-command.counter.txt",
							CheckIntervalMilliseconds: 1000,
						},
					},
					ManagementApi: ManagementApi{
						ListenPort: "2076",
					},
					Services: []ServiceConfig{
						{
							ListenPort:           "2077",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12077",
							Command:              "./test-server/test-server",
							Args:                 "-p 12077",
							ResourceRequirements: map[string]int{"TestResource": 3},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2076",
				"localhost:2077",
				"localhost:12077",
			},
			SetupFunc: func(t *testing.T) {
				err := os.Remove("test-logs/resource-check-command.counter.txt")
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("Failed to remove test-logs/resource-check-command.counter.txt: %v", err)
				}
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase // Capture range variable
		test.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			if testCase.SetupFunc != nil {
				testCase.SetupFunc(t)
			}
			waitChannel := make(chan error, 1)

			currentConfig := testCase.GetConfig(t, testCase.Name)
			StandardizeConfigNamesAndPaths(&currentConfig, testCase.Name, t)
			configFilePath := createTempConfig(t, currentConfig)

			cmd, err := startLargeModelProxy(testCase.Name, configFilePath, "", waitChannel)
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

func testUnmonitoredProcess(
	t *testing.T,
	proxiedDyingUnmonitoredServiceAddress string,
	directDyingUnmonitoredServiceAddress string,
	proxiedNonDyingService string,
	monitoringApiAddress string,
) {
	pid := runReadPidCloseConnection(t, proxiedDyingUnmonitoredServiceAddress)
	time.Sleep(1250 * time.Millisecond)
	if isProcessRunning(pid) {
		t.Errorf("process %d is still running after 1.25s", pid)
	}
	assertPortsAreClosed(t, []string{directDyingUnmonitoredServiceAddress})

	//large-model-proxy should still see the service as running since it's not monitoring it
	statusResponse := getStatusFromManagementAPI(t, monitoringApiAddress)
	verifyServiceStatus(t, statusResponse, "unmonitored-process_self-dying-unmonitored-process", true, map[string]int{"CPU": 1})
	verifyServiceStatus(t, statusResponse, "unmonitored-process_non-dying-process", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{"CPU": 1})

	//Let's make sure we can't read anything from the process - ensures large-model-proxy did not attempt to restart it
	buffer := make([]byte, 32)
	conn, err := net.Dial("tcp", proxiedDyingUnmonitoredServiceAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", proxiedDyingUnmonitoredServiceAddress, err)
	}
	defer func(conn net.Conn) {
		err := conn.Close()
		if err != nil {
			t.Fatalf("failed to close connection: %v", err)
		}
	}(conn)

	bytesRead, err := conn.Read(buffer)
	if err == nil {
		t.Errorf("expected connection to close, but it didn't")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected error while reading from connection: %v", err)
	}
	if bytesRead != 0 {
		t.Fatalf("expected to read 0 bytes, read: %d", bytesRead)
	}

	time.Sleep(3250 * time.Millisecond)
	//Idle timeout should kick in now
	statusResponse = getStatusFromManagementAPI(t, monitoringApiAddress)
	verifyServiceStatus(t, statusResponse, "unmonitored-process_self-dying-unmonitored-process", false, map[string]int{"CPU": 0})
	verifyServiceStatus(t, statusResponse, "unmonitored-process_non-dying-process", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{"CPU": 0})

	assertPortsAreClosed(t, []string{directDyingUnmonitoredServiceAddress})

	//Now start again and try to connect to another service, make sure that shuts down the unmonitored one properly
	pid = runReadPidCloseConnection(t, proxiedDyingUnmonitoredServiceAddress)
	pid2 := runReadPidCloseConnection(t, proxiedNonDyingService)

	statusResponse = getStatusFromManagementAPI(t, monitoringApiAddress)
	verifyServiceStatus(t, statusResponse, "unmonitored-process_self-dying-unmonitored-process", false, map[string]int{"CPU": 0})
	verifyServiceStatus(t, statusResponse, "unmonitored-process_non-dying-process", true, map[string]int{"CPU": 1})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{"CPU": 1})
	if isProcessRunning(pid) {
		t.Fatalf("unmonitored process %d was supposed to shut down", pid)
	}

	if !isProcessRunning(pid2) {
		t.Fatalf("non-dying service is supposed to be running with pid %d", pid)
	}
}

func testLogOutput(
	t *testing.T,
	serviceOneAddress string,
	serviceTwoAddress string,
	serviceThreeAddress string,
	serviceFourAddress string,
	directPortOne int,
	directPortTwo int,
	serviceOneName string,
	serviceTwoName string,
	serviceThreeName string,
	serviceFourName string,
	shouldLog bool,
) {
	const logFileName = "test-logs/test_logs-output.log"
	pidOne := runReadPidCloseConnection(t, serviceOneAddress)
	pidTwo := runReadPidCloseConnection(t, serviceTwoAddress)
	connThree, err := net.Dial("tcp", serviceThreeAddress)
	if err != nil {
		t.Error(err)
	}
	defer func(connThree net.Conn) { _ = connThree.Close() }(connThree)
	connFour, err := net.Dial("tcp", serviceFourAddress)
	if err != nil {
		t.Error(err)
	}
	defer func(connFour net.Conn) { _ = connFour.Close() }(connFour)

	time.Sleep(2 * time.Second)
	logFileContents, err := os.ReadFile(logFileName)
	logFileContentsString := string(logFileContents)
	if err != nil {
		t.Fatalf("failed to read log file %s: %v", logFileName, err)
	}
	var assertFunc func(t assert.TestingT, s, contains interface{}, msgAndArgs ...interface{}) bool
	if shouldLog {
		assertFunc = assert.Contains
	} else {
		assertFunc = assert.NotContains
	}
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stderr] Listening on port %d", serviceOneName, directPortOne))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stdout] Listening on port %d", serviceTwoName, directPortTwo))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stderr] Connection received on main port.", serviceOneName))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stdout] Connection received on main port.", serviceTwoName))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stderr] Responding with pid %d", serviceOneName, pidOne))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stdout] Responding with pid %d", serviceTwoName, pidTwo))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stderr] Closing connection", serviceOneName))
	assertFunc(t, logFileContentsString, fmt.Sprintf("[%s/stdout] Closing connection", serviceTwoName))

	const expectedLogMessage = "I am a test\nThis ends with a return\nWindows style\nNext after CRLF\nsplit write one plus two\nalpha\nbeta\ngamma\nNull byte \x00 inside\nEmoji 😀 test\ndangling line without newline"
	expectedLines := strings.Split(expectedLogMessage, "\n")
	for channel, serviceName := range map[string]string{"stdout": serviceThreeName, "stderr": serviceFourName} {
		linesFound := 0
		prefix := fmt.Sprintf("[%s/%s] ", serviceName, channel)
		if !shouldLog {
			assert.NotContains(t, logFileContentsString, prefix)
		} else {
			for _, line := range strings.Split(logFileContentsString, "\n") {
				prefixIndex := strings.Index(line, prefix)
				if prefixIndex == -1 {
					continue
				}
				linesFound++
				expectedLine := expectedLines[linesFound-1]
				line = line[prefixIndex+len(prefix):]
				assert.Equal(t, expectedLine, line, "line %d of log file %s should match", linesFound, logFileName)
			}
			assert.Equal(t, len(expectedLines), linesFound, "number lines in log file %s", logFileName)
		}
	}
}

func testStartupTimeoutCleansResourcesAndClosesClientConnections(
	t *testing.T,
	testName string,
	fastServiceAddress string,
	slowFailServiceAddress string,
	slowFailDirectAddress string,
	slowFailHealthcheckAddress string,
	managementApiAddress string,
) {
	assertPortsAreClosed(t, []string{slowFailDirectAddress})

	fastConn, err := net.Dial("tcp", fastServiceAddress)
	if err != nil {
		t.Fatalf("failed to connect to fast service at %s: %v", fastServiceAddress, err)
	}
	defer func() { _ = fastConn.Close() }()

	fastPid := readPidFromOpenConnection(t, fastConn)
	if fastPid == 0 {
		return
	}
	if !isProcessRunning(fastPid) {
		t.Fatalf("fast-start service process %d is not running after reading PID", fastPid)
	}
	status := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, status, testName+"_fast-start", true, map[string]int{"CPU": 1})
	verifyServiceStatus(t, status, testName+"_slow-start-fail", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(t, status, map[string]int{"CPU": 1})
	err = fastConn.Close()
	if err != nil {
		t.Fatalf("failed to close connection to fast-start service at %s: %v", slowFailServiceAddress, err)
	}
	slowConn, err := net.Dial("tcp", slowFailServiceAddress)
	if err != nil {
		t.Fatalf("failed to connect to slow-fail service at %s: %v", slowFailServiceAddress, err)
	}
	defer func() { _ = slowConn.Close() }()
	assertPortsAreClosed(t, []string{slowFailDirectAddress})

	buf := make([]byte, 64)
	n, readErr := slowConn.Read(buf)
	if readErr == nil || !errors.Is(readErr, io.EOF) {
		t.Errorf("expected slow-fail client connection to be closed with EOF after startup timeout; got err=%v, bytesRead=%d, data=%q",
			readErr, n, string(buf[:n]))
	}

	status = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, status, testName+"_fast-start", false, map[string]int{"CPU": 0})
	verifyServiceStatus(t, status, testName+"_slow-start-fail", false, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(t, status, map[string]int{"CPU": 0})
	assertPortsAreClosed(t, []string{slowFailDirectAddress, slowFailHealthcheckAddress})
	go func() {
		slowConn, _ := net.Dial("tcp", slowFailServiceAddress)
		_ = slowConn.Close()
	}()
	time.Sleep(500 * time.Millisecond)
	err = checkPortClosed(slowFailHealthcheckAddress)
	if err == nil {
		t.Errorf("expected slow-fail service to be starting with healtcheck working")
	}
	time.Sleep(3000 * time.Millisecond) // let the timeout kill the process before assert that ports are closed that runs after the test
}

func testResourceCheckCommand(t *testing.T, serviceAddress string, managementApiAddress string, resourceName string, serviceName string) {

	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceName, false, map[string]int{resourceName: 0})
	conn, err := net.Dial("tcp", serviceAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceAddress, err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(1000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 1})
	verifyServiceStatus(t, statusResponse, serviceName, true, map[string]int{resourceName: 0})

	time.Sleep(1000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 2})
	verifyServiceStatus(t, statusResponse, serviceName, true, map[string]int{resourceName: 0})

	time.Sleep(1000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 3})

	//since the check whether the resource is available is currently once per second,
	//wait for 1 more second to avoid race conditions
	time.Sleep(1000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceName, true, map[string]int{resourceName: 3})

	pid := readPidFromOpenConnection(t, conn)
	assert.True(t, isProcessRunning(pid))
}

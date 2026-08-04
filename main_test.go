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
	"strconv"
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
	verifyServiceStatus(test, statusResponse, serviceName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
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
	verifyServiceStatus(test, statusResponse, serviceName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
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
	streamingPrompt := "streaming over the keep-alive OpenAI API"
	testStreamingRequest(test, "http://localhost:2024/v1/completions", OpenAiApiCompletionRequest{
		Model:  "openai-api-keep-alive_service0",
		Prompt: streamingPrompt,
		Stream: true,
	}, []string{
		"Hello, this is chunk #1. ",
		"Now chunk #2 arrives. ",
		"Finally, chunk #3 completes the message.",
		fmt.Sprintf("Your prompt was:\n<prompt>%s</prompt>", streamingPrompt),
	}, func(t *testing.T, payload string) string {
		var chunkResp OpenAiApiCompletionResponse
		if err := json.Unmarshal([]byte(payload), &chunkResp); err != nil {
			t.Fatalf("Error unmarshalling SSE chunk JSON: %v", err)
		}
		if len(chunkResp.Choices) == 0 {
			t.Fatalf("Received chunk without choices: %+v", chunkResp)
		}
		return chunkResp.Choices[0].Text
	})
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
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%s/procinfo", procPort), nil)
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
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", ServiceStateRunning, 0, 1, map[string]int{"CPU": 1})
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
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", ServiceStateRunning, 0, 1, map[string]int{"CPU": 1})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 1})
	err = syscall.Kill(pid2, syscall.SIGINT)
	if err != nil {
		test.Fatalf("Failed to kill second service: %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
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
	// Allow the proxy's handleConnection goroutine to finish cleanup.
	// forwardConnection uses wg.Wait() to wait for both copy goroutines.
	// The defer that decrements ProxiedConnections runs after handleConnection returns.
	time.Sleep(50 * time.Millisecond)

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", ServiceStateRunning, 0, 0, map[string]int{"CPU": 1})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{"CPU": 1})

	time.Sleep(1250 * time.Millisecond)
	if isProcessRunning(pid) {
		test.Errorf("test-server is still running when it was supposed to exit")
	}

	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, "dying-processes_self-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(test, statusResponse, "dying-processes_not-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
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
	verifyServiceStatus(test, statusResponse, processName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{resourceName: 0})

	con, _ := net.DialTimeout("tcp", proxyAddress, time.Duration(3)*time.Second)
	defer func() {
		_ = con.Close()
	}()
	assertRemoteClosedWithin(test, con, 2*time.Second)
	statusResponse = getStatusFromManagementAPI(test, managementApiAddress)
	verifyServiceStatus(test, statusResponse, processName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyTotalResourceUsage(test, statusResponse, map[string]int{resourceName: 0})
}
func testMultipleConnectionsWhileWaitingForResources(t *testing.T,
	serviceOneAddress string,
	serviceTwoAddress string,
	serviceOneHealthCheckAddress string,
	serviceTwoHealthCheckAddress string,
	serviceOneName string,
	serviceTwoName string,
	managementApiAddress string,
	resourceName string,
) {
	//sanity checks
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 1}, map[string]int{resourceName: 0}, map[string]int{resourceName: 1})

	// establish 2 connections to serviceTwo and one to serviceOne. serviceOne starts and uses the resource
	// the connections to serviceTwo are waiting for 3s until serviceOne connection is done
	connOne, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceOneAddress, err)
	}
	defer func() { _ = connOne.Close() }()
	time.Sleep(100 * time.Millisecond) //Make sure connections are established in the expected order
	connTwo_1, err := net.Dial("tcp", serviceTwoAddress)
	if err != nil {
		t.Fatalf("connection#1 to %s: %v", serviceTwoAddress, err)
	}
	defer func() { _ = connTwo_1.Close() }()
	connTwo_2, err := net.Dial("tcp", serviceTwoAddress)
	if err != nil {
		t.Fatalf("connection#2 to %s: %v", serviceTwoAddress, err)
	}
	defer func() { _ = connTwo_2.Close() }()

	// Both serviceTwo connections must be registered as waiting. The proxy
	// processes the two dials asynchronously, so checking the count immediately
	// races on slow/loaded machines (the second connection may not be counted
	// yet).
	waitForWaitingConnections(t, managementApiAddress, serviceTwoName, 2, 5*time.Second)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 1})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateWaitingForResources, 2, 0, map[string]int{resourceName: 1})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 1}, map[string]int{resourceName: 0}, map[string]int{resourceName: 1}, map[string]int{resourceName: 1})

	readPidFromOpenConnection(t, connOne) //wait for service one to be ready

	// serviceOne is evicted to free resources for serviceTwo, which then starts.
	// Poll for serviceTwo to reach "starting" instead of a fixed sleep: the
	// eviction/handover timing varies and races on slow/loaded machines.
	statusResponse = waitForServiceState(t, managementApiAddress, serviceTwoName, ServiceStateStarting, 5*time.Second)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStarting, 2, 0, map[string]int{resourceName: 1})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 1}, map[string]int{resourceName: 0}, map[string]int{resourceName: 1}, map[string]int{resourceName: 1})

	// service1 becomes Running once one connection finishes starting it, but the
	// second connection (blocked on the service's manageMutex during startup)
	// transitions from waiting to proxied slightly later. Poll for both
	// connections to be proxied rather than checking immediately after Running.
	waitForServiceState(t, managementApiAddress, serviceTwoName, ServiceStateRunning, 5*time.Second)
	waitForProxiedConnections(t, managementApiAddress, serviceTwoName, 2, 5*time.Second)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 2, map[string]int{resourceName: 1})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 0}, map[string]int{resourceName: 1}, map[string]int{resourceName: 1})

	readPidFromOpenConnection(t, connTwo_1)
	readPidFromOpenConnection(t, connTwo_2)

	// Close connections so the proxy's forwardConnection goroutines finish and
	// decrement ProxiedConnections before we check the status.
	_ = connTwo_1.Close()
	_ = connTwo_2.Close()

	// Wait for the proxy to register that the closed connections are gone.
	waitForProxiedConnections(t, managementApiAddress, serviceTwoName, 0, 5*time.Second)

	//make sure connections went to 0
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 0, map[string]int{resourceName: 1})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 0}, map[string]int{resourceName: 1}, map[string]int{resourceName: 1})
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
					"logs-output",
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
					"logs-no-output",
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
				testResourceCheckCommand(
					t,
					"localhost:2077",
					"localhost:2079",
					"localhost:2080",
					"localhost:2081",
					"resource-check-command_service0",
					"resource-check-command_service1",
					"localhost:2076",
					"TestResource",
				)
			},
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"TestResource": {
							//this command increments a number in the file by one every time it runs
							CheckCommand:                           "read -r original_integer < test-logs/resource-check-command.counter.txt; incremented_integer=$((original_integer + 1)); printf '%d\n' \"$incremented_integer\" | tee test-logs/resource-check-command.counter.txt",
							CheckWhenNotEnoughIntervalMilliseconds: 1000,
						},
					},
					LogLevel: LogLevelDebug,
					ManagementApi: ManagementApi{
						ListenPort: "2076",
					},
					Services: []ServiceConfig{
						{
							ListenPort:           "2077",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12077",
							Command:              "./test-server/test-server",
							Args:                 "-p 12077 -healthcheck-port 2080 -sleep-before-listening 10s",
							ResourceRequirements: map[string]int{"TestResource": 4},
						},
						{
							ListenPort:           "2079",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12079",
							Command:              "./test-server/test-server",
							Args:                 "-p 12079 -healthcheck-port 2081",
							ResourceRequirements: map[string]int{"TestResource": 5},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2076",
				"localhost:2077",
				"localhost:2079",
				"localhost:2080",
				"localhost:2081",
				"localhost:12077",
				"localhost:12079",
			},
			SetupFunc: func(t *testing.T) {
				err := os.Remove("test-logs/resource-check-command.counter.txt")
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("Failed to remove test-logs/resource-check-command.counter.txt: %v", err)
				}
			},
		},
		{
			Name: "should-not-use-an-outdated-resource-check-result",
			TestFunc: func(t *testing.T) {
				testResourceCheckCommandShouldNotUseAnOutdatedResourceCheckResult(
					t,
					"localhost:2082",
					"localhost:2083",
					"localhost:2084",
					"localhost:2085",
					"should-not-use-an-outdated-resource-check-result_service0",
					"should-not-use-an-outdated-resource-check-result_service1",
					"localhost:2086",
					"TestResource",
				)
			},
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"TestResource": {
							CheckCommand:                           "cat test-logs/should-not-use-an-outdated-resource-check-result.resource-amount.txt",
							CheckWhenNotEnoughIntervalMilliseconds: 60000,
							Amount:                                 2, //Initial amount is different to make sure the check command runs
						},
					},
					LogLevel: LogLevelDebug,
					ManagementApi: ManagementApi{
						ListenPort: "2086",
					},
					Services: []ServiceConfig{
						{
							ListenPort:      "2082",
							ProxyTargetHost: "localhost",
							ProxyTargetPort: "12082",
							Command:         "sh",
							Args: "-c \"" +
								"echo '11' > test-logs/should-not-use-an-outdated-resource-check-result.resource-amount.txt &&" +
								"sleep 3 && " +
								"echo '0' > test-logs/should-not-use-an-outdated-resource-check-result.resource-amount.txt &&" +
								"./test-server/test-server -p 12082 -healthcheck-port 2084 -exit-after-duration 2s -exit-script 'echo 12 > test-logs/should-not-use-an-outdated-resource-check-result.resource-amount.txt'" +
								"\"",
							ResourceRequirements: map[string]int{"TestResource": 10},
						},
						{
							ListenPort:           "2083",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12083",
							Command:              "./test-server/test-server",
							Args:                 "-p 12083 -healthcheck-port 2085",
							ResourceRequirements: map[string]int{"TestResource": 10},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2082",
				"localhost:12082",
				"localhost:2083",
				"localhost:12083",
				"localhost:2084",
				"localhost:2085",
				"localhost:2086",
			},
			SetupFunc: func(t *testing.T) {
				err := os.WriteFile("test-logs/should-not-use-an-outdated-resource-check-result.resource-amount.txt", []byte("10"), 0666)
				if err != nil {
					t.Fatalf("failed to write resource-amount file: %v", err)
				}
			},
		},
		{
			Name: "multiple-connections-while-waiting-for-resources",
			TestFunc: func(t *testing.T) {
				testMultipleConnectionsWhileWaitingForResources(
					t,
					"localhost:2087",
					"localhost:2088",
					"localhost:2089",
					"localhost:2090",
					"multiple-connections-while-waiting-for-resources_service0",
					"multiple-connections-while-waiting-for-resources_service1",
					"localhost:2091",
					"TestResource",
				)
			},
			GetConfig: func(t *testing.T, testName string) Config {
				return Config{
					ResourcesAvailable: map[string]ResourceAvailable{
						"TestResource": {
							Amount: 1,
						},
					},
					LogLevel: LogLevelDebug,
					ManagementApi: ManagementApi{
						ListenPort: "2091",
					},
					Services: []ServiceConfig{
						{
							ListenPort:           "2087",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12087",
							Command:              "./test-server/test-server",
							Args:                 "-p 12087 -healthcheck-port 2089 -sleep-before-listening 3s",
							ResourceRequirements: map[string]int{"TestResource": 1},
						},
						{
							ListenPort:           "2088",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12088",
							Command:              "./test-server/test-server",
							Args:                 "-p 12088 -healthcheck-port 2090 -sleep-before-listening 2s -request-processing-duration 3s",
							ResourceRequirements: map[string]int{"TestResource": 1},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2087",
				"localhost:12087",
				"localhost:2088",
				"localhost:12088",
				"localhost:2089",
				"localhost:2090",
				"localhost:2091",
			},
		},
		{
			Name: "resource-check-command-max-wait-timeout",
			TestFunc: func(t *testing.T) {
				testResourceCheckCommandMaxWaitTimeTimeout(
					t,
					"localhost:2092",
					"localhost:2094",
					"resource-check-command-max-wait-timeout_service0",
					"localhost:2093",
					"TestResource",
					2,
				)
			},
			GetConfig: func(t *testing.T, testName string) Config {
				maxWait := uint(2)
				return Config{
					MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds: &maxWait,
					ResourcesAvailable: map[string]ResourceAvailable{
						"TestResource": {
							// Always returns 0 — resources are never sufficient
							CheckCommand: "echo 0",
						},
					},
					LogLevel: LogLevelDebug,
					ManagementApi: ManagementApi{
						ListenPort: "2093",
					},
					Services: []ServiceConfig{
						{
							ListenPort:           "2092",
							ProxyTargetHost:      "localhost",
							ProxyTargetPort:      "12092",
							Command:              "./test-server/test-server",
							Args:                 "-p 12092 -healthcheck-port 2094",
							ResourceRequirements: map[string]int{"TestResource": 5},
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{
				"localhost:2092",
				"localhost:2093",
				"localhost:2094",
				"localhost:12092",
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
			StandardizeConfigNamesAndPaths(&currentConfig, testCase.Name)
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
	verifyServiceStatus(t, statusResponse, "unmonitored-process_self-dying-unmonitored-process", ServiceStateRunning, 0, 0, map[string]int{"CPU": 1})
	verifyServiceStatus(t, statusResponse, "unmonitored-process_non-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
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
	verifyServiceStatus(t, statusResponse, "unmonitored-process_self-dying-unmonitored-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(t, statusResponse, "unmonitored-process_non-dying-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{"CPU": 0})

	assertPortsAreClosed(t, []string{directDyingUnmonitoredServiceAddress})

	//Now start again and try to connect to another service, make sure that shuts down the unmonitored one properly
	pid = runReadPidCloseConnection(t, proxiedDyingUnmonitoredServiceAddress)
	pid2 := runReadPidCloseConnection(t, proxiedNonDyingService)
	// Allow the proxy's handleConnection goroutine to finish cleanup.
	// forwardConnection uses wg.Wait() to wait for both copy goroutines.
	// The defer that decrements ProxiedConnections runs after handleConnection returns.
	time.Sleep(50 * time.Millisecond)

	statusResponse = getStatusFromManagementAPI(t, monitoringApiAddress)
	verifyServiceStatus(t, statusResponse, "unmonitored-process_self-dying-unmonitored-process", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(t, statusResponse, "unmonitored-process_non-dying-process", ServiceStateRunning, 0, 0, map[string]int{"CPU": 1})
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
	testName string,
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
	logFileName := fmt.Sprintf("test-logs/test_%s.log", testName)
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
	verifyServiceStatus(t, status, testName+"_fast-start", ServiceStateRunning, 0, 1, map[string]int{"CPU": 1})
	verifyServiceStatus(t, status, testName+"_slow-start-fail", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
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
	verifyServiceStatus(t, status, testName+"_fast-start", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(t, status, testName+"_slow-start-fail", ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyTotalResourceUsage(t, status, map[string]int{"CPU": 0})
	assertPortsAreClosed(t, []string{slowFailDirectAddress, slowFailHealthcheckAddress})
	// Reconnect to verify the service can restart after the previous startup
	// timeout. The connection is kept open: with client-disconnect handling,
	// a connection that closes immediately would abort the startup, so a real
	// (kept-open) client is needed to observe the service starting again.
	restartConn, restartErr := net.Dial("tcp", slowFailServiceAddress)
	if restartErr != nil {
		t.Fatalf("failed to reconnect to slow-fail service to verify restart: %v", restartErr)
	}
	defer func() { _ = restartConn.Close() }()
	time.Sleep(500 * time.Millisecond)
	err = checkPortClosed(slowFailHealthcheckAddress)
	if err == nil {
		t.Errorf("expected slow-fail service to be starting with healtcheck working")
	}
	time.Sleep(3000 * time.Millisecond) // let the timeout kill the process before assert that ports are closed that runs after the test
}

// TestProcessExitDuringShutdown verifies that when a service process exits during
// shutdown, the proxy completes promptly. This is a regression test for a deadlock
// where monitorProcess's exitWaitGroup.Done() was in a defer that executed after
// serviceMutex.Lock().
//
// The deadlock scenario (without the fix):
// 1. Service process exits BEFORE shutdown signal arrives
// 2. monitorProcess: process.Wait() returns, reads interrupted=false
// 3. monitorProcess: enters else branch, hits test hook, blocks
// 4. Shutdown signal arrives: interrupted=true, signal handler acquires serviceMutex
// 5. signal handler: stopService → waitForProcessToTerminate blocks on exitWaitGroup
// 6. Test releases hook: monitorProcess proceeds to serviceMutex.Lock() → BLOCKS
// 7. exitWaitGroup.Done() is in monitorProcess's defer — never called while blocked
// 8. DEADLOCK: circular wait (Lock() blocked, exitWaitGroup.Wait() blocked)
//
// The fix: call exitWaitGroup.Done() BEFORE any mutex acquisition.
//
// The test uses PROXY_EXIT_HOOK_FILE env var. monitorProcess blocks at this hook
// after reading interrupted=false but before acquiring serviceMutex. The test
// sends SIGINT while monitorProcess is at the hook, then releases the hook.
func TestProcessExitDuringShutdown(t *testing.T) {
	t.Parallel()

	// Hook file: monitorProcess blocks here after reading interrupted=false,
	// waiting for this file to be deleted before acquiring serviceMutex.
	hookDir := t.TempDir()
	hookFile := hookDir + "/exit-hook"
	if err := os.WriteFile(hookFile, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create hook file: %v", err)
	}

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			"CPU": {Amount: 1},
		},
		ShutDownAfterInactivitySeconds: 120,
		ManagementApi: ManagementApi{
			ListenPort: "2099",
		},
		Services: []ServiceConfig{
			{
				ListenPort:      "2098",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12098",
				Command:         "./test-server/test-server",
				Args:            "-p 12098 -exit-after-duration 200ms --ignore-sigterm --sleep-after-writing-pid-duration 100ms",
				ResourceRequirements: map[string]int{
					"CPU": 1,
				},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, "process-exit-during-shutdown")
	configFilePath := createTempConfig(t, cfg)

	// Start proxy with the hook file env var
	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxyWithEnv("process-exit-during-shutdown", configFilePath, "", []string{fmt.Sprintf("PROXY_EXIT_HOOK_FILE=%s", hookFile)}, waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}

	// Connect to start the service
	conn, err := net.DialTimeout("tcp", "localhost:2098", 5*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Failed to connect to service: %v", err)
	}
	buf := make([]byte, 64)
	conn.Read(buf)
	t.Logf("Service started")
	conn.Close()

	// Process exits after 200ms (exit-after-duration, --ignore-sigterm).
	// After exit: monitorProcess: process.Wait() returns → reads interrupted=false
	// → enters else branch → hits hook → blocks (waiting for hook file deletion)
	time.Sleep(300 * time.Millisecond)

	// Now send SIGINT. Signal handler:
	// 1. Sets interrupted = true (too late — monitorProcess already read it as false)
	// 2. Acquires serviceMutex
	// 3. stopService: sends SIGTERM (ignored by --ignore-sigterm)
	// 4. stopService: waitForProcessToTerminate blocks on exitWaitGroup
	shutdownStart := time.Now()
	err = cmd.Process.Signal(syscall.SIGINT)
	if err != nil {
		t.Fatalf("Failed to send SIGINT to proxy: %v", err)
	}

	// Wait for signal handler to acquire serviceMutex and enter waitForProcessToTerminate
	time.Sleep(200 * time.Millisecond)

	// Release hook. monitorProcess proceeds to serviceMutex.Lock() → BLOCKS
	// (signal handler holds serviceMutex)
	//
	// WITHOUT the fix: monitorProcess blocks on Lock(). defer never runs.
	// waitForProcessToTerminate hangs for 10s (ProcessCheckTimeout).
	// SIGKILL sent, proxy exits after ~10 seconds.
	//
	// WITH the fix: exitWaitGroup.Done() was called BEFORE the hook.
	// waitForProcessToTerminate returns immediately. No deadlock.
	if err := os.Remove(hookFile); err != nil {
		t.Fatalf("Failed to remove hook file: %v", err)
	}

	select {
	case err = <-waitChannel:
		shutdownDuration := time.Since(shutdownStart)
		t.Logf("Shutdown completed in %v", shutdownDuration)
		if shutdownDuration > 3*time.Second {
			t.Errorf("Shutdown took %v, expected < 3s. This indicates exitWaitGroup.Done() was not called promptly (deadlock occurred)", shutdownDuration)
		}
		if err != nil && err.Error() != "waitid: no child processes" && err.Error() != "wait: no child processes" {
			t.Logf("Proxy exited with: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Errorf("Shutdown took more than 15 seconds — deadlock: exitWaitGroup.Done() was not called before serviceMutex.Lock()")
		_ = cmd.Process.Kill()
	}
}

// TestEvictionOfAlreadyDeadProcessDoesNotLoop is a regression test for issue #119
// ("Failure to send SIGTERM leads to an endless loop").
//
// The bug: when reserveResources evicts a service whose child process has ALREADY
// exited and been reaped, syscall.Kill(-pgid, SIGTERM) fails with ESRCH ("no such
// process"). If stopService does not clean up in that case, the evicted service
// stays in runningServices still holding its resources, so reserveResources loops
// forever calling stopService on it — visible in the logs as a tight, microsecond-
// spaced repetition of:
//
//   Failed to send SIGTERM to -<pgid>: no such process
//   Stopping service to free resources for <other>
//   Sending SIGTERM to service process group: -<pgid>
//   ...
//
// and the requesting service never starts (its client connection hangs).
//
// This test reproduces the exact precondition: service-one holds the only unit of
// CPU, then exits on its own (-exit-after-duration). The test-only PROXY_EXIT_HOOK_FILE
// blocks monitorProcess AFTER it has reaped the process and called exitWaitGroup.Done()
// but BEFORE it removes service-one from runningServices — so service-one sits in the
// map with an already-dead process. A connection to service-two then forces
// reserveResources to evict service-one, which means sending SIGTERM to a process
// group that no longer exists (ESRCH).
//
// Expected (fixed) behavior: stopService tolerates the ESRCH, cleans service-one up,
// frees CPU, and service-two starts promptly. The test asserts both that
// service-two becomes reachable AND that the ESRCH path was actually exercised, while
// rejecting the runaway repetition of "Stopping service to free resources" that
// characterizes the loop.
func TestEvictionOfAlreadyDeadProcessDoesNotLoop(t *testing.T) {
	t.Parallel()

	// Hook file: monitorProcess blocks here after reaping the exited service-one
	// process, keeping service-one in runningServices with a dead process.
	hookDir := t.TempDir()
	hookFile := hookDir + "/exit-hook"
	if err := os.WriteFile(hookFile, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create hook file: %v", err)
	}

	const (
		managementApiAddress = "localhost:2129"
		holderProxyAddress   = "localhost:2130"
		holderTargetPort     = "12310"
		requesterProxyAddress = "localhost:2131"
		requesterTargetPort   = "12311"
		testCaseName          = "eviction-already-dead-process"
	)

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			"CPU": {Amount: 1},
		},
		// Keep idle services alive so service-one stays in runningServices after its
		// client disconnects (we want monitorProcess, not the idle timer, to be the
		// thing that would remove it — which we then block with the hook).
		ShutDownAfterInactivitySeconds: 120,
		ManagementApi:                  ManagementApi{ListenPort: "2129"},
		Services: []ServiceConfig{
			{
				Name:               "holder",
				ListenPort:         "2130",
				ProxyTargetHost:    "localhost",
				ProxyTargetPort:    holderTargetPort,
				Command:            "./test-server/test-server",
				Args:               "-p " + holderTargetPort + " -exit-after-duration 800ms",
				ResourceRequirements: map[string]int{"CPU": 1},
			},
			{
				Name:               "requester",
				ListenPort:         "2131",
				ProxyTargetHost:    "localhost",
				ProxyTargetPort:    requesterTargetPort,
				Command:            "./test-server/test-server",
				Args:               "-p " + requesterTargetPort,
				ResourceRequirements: map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testCaseName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxyWithEnv(
		testCaseName, configFilePath, "",
		[]string{fmt.Sprintf("PROXY_EXIT_HOOK_FILE=%s", hookFile)},
		waitChannel,
	)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		// Release the hook first so a blocked monitorProcess can finish and the
		// proxy can shut down cleanly.
		_ = os.Remove(hookFile)
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
	}()

	// Start service-one (holder). It reserves the only unit of CPU.
	holderPid := runReadPidCloseConnection(t, holderProxyAddress)
	if holderPid == 0 {
		return // runReadPidCloseConnection already failed the test
	}
	// Ensure the holder has no proxied connection so it is eligible for eviction
	// (canBeStopped requires proxied == 0).
	waitForProxiedConnections(t, managementApiAddress, cfg.Services[0].Name, 0, 3*time.Second)

	// Wait for service-one to die on its own and be reaped by monitorProcess, which
	// then calls exitWaitGroup.Done() and blocks at the hook — leaving service-one in
	// runningServices with an already-reaped process.
	reapDeadline := time.Now().Add(5 * time.Second)
	for {
		if !isProcessRunning(holderPid) {
			break
		}
		if time.Now().After(reapDeadline) {
			t.Fatalf("holder process %d did not exit within %s", holderPid, 5*time.Second)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Give monitorProcess time to run process.Wait() (reap the zombie) and reach the
	// hook. Until the zombie is reaped the process group still resolves and SIGTERM
	// would not return ESRCH; we need the reaped state to reproduce the bug.
	time.Sleep(200 * time.Millisecond)

	// Sanity: the holder is still registered (monitorProcess is blocked at the hook
	// and has not cleaned it up) and still holds CPU.
	statusBefore := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusBefore, cfg.Services[0].Name, ServiceStateRunning, 0, 0, map[string]int{"CPU": 1})

	// Trigger eviction: connecting to service-two forces reserveResources to stop
	// service-one to free CPU. service-one's process is already gone, so the stop
	// must send SIGTERM to a non-existent process group (ESRCH). If the bug were
	// present, stopService would not clean up and this connection would hang forever
	// while the proxy logged the endless SIGTERM/Stopping loop.
	requesterConn, err := net.DialTimeout("tcp", requesterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("Failed to connect to requester proxy: %v", err)
	}
	defer func() { _ = requesterConn.Close() }()

	// service-two must become reachable promptly — proving stopService cleaned up
	// the already-dead holder, freed CPU, and broke out of reserveResources instead
	// of looping.
	if err := requesterConn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("Failed to set read deadline: %v", err)
	}
	requesterPid := readPidFromOpenConnection(t, requesterConn)
	if requesterPid == 0 {
		t.Fatalf("service-two never started within 15s — reserveResources likely looped on an already-dead evicted process (issue #119)")
	}
	t.Logf("service-two started with pid %d after evicting the already-dead holder", requesterPid)

	// Confirm the ESRCH path was genuinely exercised and that the eviction did not
	// repeat uncontrollably (the signature of the loop). Both counts should be tiny
	// (a handful at most); the bug produced thousands within milliseconds.
	holderLog := fmt.Sprintf("test-logs/test_%s.log", testCaseName)
	logBytes, readErr := os.ReadFile(holderLog)
	if readErr != nil {
		t.Logf("Could not read proxy log %s for loop check: %v", holderLog, readErr)
	} else {
		logText := string(logBytes)
		if !strings.Contains(logText, "Failed to send SIGTERM") {
			t.Errorf("Test did not exercise the ESRCH path: expected at least one " +
				"\"Failed to send SIGTERM\" line in the proxy log")
		}
		stopCount := strings.Count(logText, "Stopping service to free resources")
		if stopCount > 5 {
			t.Errorf("Expected the eviction to run a handful of times at most, but " +
				"\"Stopping service to free resources\" appeared %d times in the log — "+
				"this is the endless loop from issue #119", stopCount)
		}
		t.Logf("\"Stopping service to free resources\" appeared %d time(s) in the proxy log", stopCount)
	}
}

// TestWaitingConnectionsDecrementedOnServiceStartFailure is a regression test for
// a leak where a connection that triggered a service start was counted as
// "waiting" but the counter was never decremented when the service failed to
// start (process exit, healthcheck failure, startup timeout, etc.). The leaked
// counter left WaitingConnections > 0 forever and broke subsequent status
// checks / idle-shutdown decisions.
//
// It exercises two failure shapes:
//   - a slow failure (healthcheck always fails, then startup timeout) so the
//     waiting state is observable, and
//   - a fast failure (process exits immediately).
//
// Both are repeated several times: if the decrement regressed, the counter
// would accumulate across iterations instead of returning to 0 each time.
func TestWaitingConnectionsDecrementedOnServiceStartFailure(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2105"
	const slowProxyAddress = "localhost:2106"
	const fastProxyAddress = "localhost:2107"
	const testName = "waiting-conn-decremented"
	const slowServiceName = testName + "_slow"
	const fastServiceName = testName + "_fast"

	slowStartupTimeoutMs := uint(700)
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2105"},
		Services: []ServiceConfig{
			{
				Name:                            "slow",
				ListenPort:                      "2106",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 "12106",
				Command:                         "./test-server/test-server",
				Args:                            "-p 12106 -startup-duration 24h",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 100,
				StartupTimeoutMilliseconds:      &slowStartupTimeoutMs,
				ResourceRequirements:            map[string]int{"CPU": 1},
			},
			{
				Name:                 "fast",
				ListenPort:           "2107",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12107",
				Command:              "exit",
				Args:                 "1",
				ResourceRequirements: map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy("waiting-conn-decremented", configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{slowProxyAddress, fastProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, slowServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(t, statusResponse, fastServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})

	// Slow failure path: the connection waits while the healthcheck runs, so we
	// can observe WaitingConnections rise to 1, then fall back to 0 once the
	// service fails to start and the proxy closes the client connection.
	for iteration := 0; iteration < 3; iteration++ {
		con, err := net.DialTimeout("tcp", slowProxyAddress, 3*time.Second)
		if err != nil {
			t.Fatalf("slow iteration %d: failed to connect to proxy: %v", iteration, err)
		}
		waitForWaitingConnections(t, managementApiAddress, slowServiceName, 1, 1*time.Second)
		assertRemoteClosedWithin(t, con, 2*time.Second)
		_ = con.Close()

		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		verifyServiceStatus(t, statusResponse, slowServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	}

	// Fast failure path: the process exits immediately. There is no observable
	// waiting window, but the counter must still return to 0 after each failure.
	for iteration := 0; iteration < 3; iteration++ {
		con, err := net.DialTimeout("tcp", fastProxyAddress, 3*time.Second)
		if err != nil {
			t.Fatalf("fast iteration %d: failed to connect to proxy: %v", iteration, err)
		}
		assertRemoteClosedWithin(t, con, 2*time.Second)
		_ = con.Close()

		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		verifyServiceStatus(t, statusResponse, fastServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	}
}

// TestWaitingConnectionCountReleasedWhenClientDisconnects is a regression test for
// a counting bug where a connection that disconnects while it is still in the
// "waiting" state (waiting for resources so its service can start) keeps being
// counted as a waiting connection until the resource wait times out. The stale
// counter inflated WaitingConnections and, because the waiting count never
// reached zero, could delay or block idle shutdown / eviction decisions for up
// to MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds.
//
// Scenario:
//  1. A "holder" service starts and keeps a proxied connection (and therefore
//     its resource) open for a long time.
//  2. A second connection targets another service that needs the same resource.
//     It cannot start and is counted as a waiting connection (WaitingConnections == 1).
//  3. The waiting client disconnects.
//  4. WaitingConnections must drop back to 0 promptly, without waiting for the
//     holder to release the resource.
func TestWaitingConnectionCountReleasedWhenClientDisconnects(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2110"
	const holderProxyAddress = "localhost:2111"
	const waiterProxyAddress = "localhost:2112"
	const testName = "waiting-conn-released-on-disconnect"
	const holderServiceName = testName + "_holder"
	const waiterServiceName = testName + "_waiter"

	// The holder keeps its proxied connection open well beyond the duration of
	// this test so the resource stays unavailable to the waiter the whole time.
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2110"},
		Services: []ServiceConfig{
			{
				Name:                 "holder",
				ListenPort:           "2111",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12111",
				Command:              "./test-server/test-server",
				Args:                 "-p 12111 -sleep-after-writing-pid-duration 30s",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
			{
				Name:                 "waiter",
				ListenPort:           "2112",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12112",
				Command:              "./test-server/test-server",
				Args:                 "-p 12112",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy("waiting-conn-released-on-disconnect", configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{holderProxyAddress, waiterProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// 1. Start the holder and keep its connection open so it reserves TestResource.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder: %v", err)
	}
	defer func() { _ = holderConn.Close() }()
	readPidFromOpenConnection(t, holderConn)
	// holder is now running with one proxied connection, holding TestResource.
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, holderServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})

	// 2. A new connection to the waiter must wait for TestResource.
	waiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 1, 3*time.Second)

	// 3. The waiting client disconnects.
	_ = waiterConn.Close()

	// 4. WaitingConnections must return to 0 promptly, well before the holder
	//    releases the resource (which it won't do for 30s).
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 0, 3*time.Second)
}

// TestResourcesReleasedWhenProcessExitsDuringConnectAndConsiderStoppedFalse is a
// regression test for a resource / runningServices leak. When a service process
// exits during the connect-retry phase, startService's process-exit branch used
// to only release the reserved counter and rely on monitorProcess to finish the
// cleanup. But monitorProcess passes *ConsiderStoppedOnProcessExit as the cleanup
// flag, so when that option is false monitorProcess skips cleanup entirely and
// resourcesInUse is never decremented nor is the runningServices entry removed
// (the service is then reported as "starting" forever). This test opts into
// ConsiderStoppedOnProcessExit=false, drives the process to exit before it ever
// listens, and asserts that the resource is fully freed (in_use == 0, free ==
// total) and the service returns to "stopped".
func TestResourcesReleasedWhenProcessExitsDuringConnectAndConsiderStoppedFalse(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2120"
	const serviceProxyAddress = "localhost:2121"
	const testName = "process-exit-during-connect-no-monitor"
	const serviceName = testName + "_dying-process"

	// The service process sleeps 30s before it would start listening on the
	// target port, but exits after 500ms — so the proxy never manages to
	// connect and the process exits during the connect-retry phase, well before
	// the 60s startup timeout.
	startupTimeoutMs := uint(60000)
	considerStoppedOnProcessExit := false
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2120"},
		Services: []ServiceConfig{
			{
				Name:                         "dying-process",
				ListenPort:                   "2121",
				ProxyTargetHost:              "localhost",
				ProxyTargetPort:              "12121",
				Command:                      "./test-server/test-server",
				Args:                         "-p 12121 -sleep-before-listening 30s -exit-after-duration 500ms",
				StartupTimeoutMilliseconds:   &startupTimeoutMs,
				ConsiderStoppedOnProcessExit: &considerStoppedOnProcessExit,
				RestartOnConnectionFailure:   false,
				ResourceRequirements:         map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Connect a client to trigger startService. The client stays connected while
	// the service sits in the connect-retry phase.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// The service reserves TestResource and enters the starting/connect-retry
	// phase, so the resource is held: in_use == 1 and free == 0. Poll, since the
	// reservation and the state transition are observed asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		if info, ok := resp.Resources["TestResource"]; ok && info.InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{"TestResource": 1}, // reserved by starting services
				map[string]int{"TestResource": 0}, // free (held by the starting service)
				map[string]int{"TestResource": 1}, // in_use
				map[string]int{"TestResource": 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource TestResource was never held (in_use never reached 1) within %s", 3*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp := waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 0}, // reserved by starting services
		map[string]int{"TestResource": 1}, // free (full total — no leak)
		map[string]int{"TestResource": 0}, // in_use (no leak)
		map[string]int{"TestResource": 1}, // total
	)
}

// TestNoWastedCheckCommandWhenStaticResourceIsBottleneck pins the two-pass fix
// in findFirstMissingResourceWhenServiceMutexIsLocked (Issue I2).
//
// When a service requires BOTH a CheckCommand-backed resource and a
// statically-tracked resource, and the STATIC resource is the bottleneck, the
// proxy must NOT register a CheckCommand first-change channel / Unpause the
// monitor for the CheckCommand resource. Doing so forces one unnecessary
// external CheckCommand run for a service that cannot start anyway because of
// the static shortage.
//
// Setup: a holder service holds the single unit of a static resource and has an
// active proxied connection (so canBeStopped returns false and it cannot be
// evicted). A victim service requires one unit of the static resource AND one
// unit of a CheckCommand resource whose CheckCommand increments a counter file.
// The resource monitor runs that CheckCommand once at startup (counter 0 -> 1);
// afterwards, with no listener registered for the CheckCommand resource, the
// monitor never re-runs it (its timer is only re-armed when a listener exists or
// it is Unpause-poked). We drive several victim connection attempts — each one
// forces one pass through findFirstMissingResourceWhenServiceMutexIsLocked with
// firstCheckNeeded=true. Under the OLD single-pass loop, a pass whose random map
// iteration visited the CheckCommand resource BEFORE the static one registered a
// first-change channel and Unpause-poked the monitor (one wasted CheckCommand
// run -> counter increments) before returning the static bottleneck. Under the
// fixed two-pass code the static bottleneck is returned WITHOUT ever touching
// the CheckCommand resource, so the counter never advances past its startup
// value of 1.
func TestNoWastedCheckCommandWhenStaticResourceIsBottleneck(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2160"
	const holderProxyAddress = "localhost:2161"
	const victimProxyAddress = "localhost:2162"
	const testName = "no-wasted-check-static-bottleneck"
	const holderServiceName = testName + "_holder"
	const victimServiceName = testName + "_victim"
	const staticResource = "StaticSlot"
	const checkedResource = "Checked"
	const counterFile = "test-logs/no-wasted-check.counter.txt"

	// Ensure test-logs/ exists (standalone parallel tests may run before any
	// proxy start creates it), then initialize the counter file at 0; the
	// monitor's startup CheckCommand bumps it to 1 once the proxy starts.
	if err := os.MkdirAll("test-logs", 0755); err != nil {
		t.Fatalf("could not create test-logs directory: %v", err)
	}
	if err := os.WriteFile(counterFile, []byte("0"), 0644); err != nil {
		t.Fatalf("could not write counter file: %v", err)
	}

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			staticResource: {Amount: 1}, // no CheckCommand -> statically tracked
			checkedResource: {
				CheckCommand:                           "read -r n < " + counterFile + "; n=$((n+1)); printf '%d\\n' \"$n\" | tee " + counterFile,
				CheckWhenNotEnoughIntervalMilliseconds: 1000,
			},
		},
		LogLevel:      LogLevelDebug,
		ManagementApi: ManagementApi{ListenPort: "2160"},
		Services: []ServiceConfig{
			{
				Name:            "holder",
				ListenPort:      "2161",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12161",
				Command:         "./test-server/test-server",
				// sleep-after-writing-pid-duration keeps the proxied connection
				// open for the whole test, so canBeStopped returns false (proxied
				// >= 1) and the holder cannot be evicted to free StaticSlot for the
				// victim — the static resource stays the bottleneck.
				Args:                 "-p 12161 -healthcheck-port 2163 -sleep-after-writing-pid-duration 60s",
				ResourceRequirements: map[string]int{staticResource: 1},
			},
			{
				Name:                 "victim",
				ListenPort:           "2162",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12162",
				Command:              "./test-server/test-server",
				Args:                 "-p 12162 -healthcheck-port 2164",
				ResourceRequirements: map[string]int{staticResource: 1, checkedResource: 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{holderProxyAddress, victimProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Wait for the resource monitor's startup CheckCommand to run (counter 0 -> 1).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if readCounterValue(t, counterFile) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup CheckCommand never ran; counter stuck at 0")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Connect a client to the holder so it reserves the single static slot,
	// reaches "running", and — crucially — keeps a proxied connection open so
	// canBeStopped returns false and the holder cannot be evicted for the victim.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder proxy: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	// Poll until the holder is running, holds the static slot, and reports a
	// proxied connection (which makes it non-evictable).
	deadline = time.Now().Add(5 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		holderRunning := false
		for _, svc := range resp.Services {
			if svc.Name == holderServiceName && svc.Status == ServiceStateRunning && svc.ProxiedConnections >= 1 {
				holderRunning = true
			}
		}
		if holderRunning && resp.Resources[staticResource].InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{staticResource: 0}, // reserved by starting services
				map[string]int{staticResource: 0}, // free: the holder holds the only unit
				map[string]int{staticResource: 1}, // in_use
				map[string]int{staticResource: 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder did not reach running+proxied holding StaticSlot within 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Baseline: with no listener registered for the CheckCommand resource, the
	// monitor must NOT run it again on its own. Give it a brief window to surface
	// any spurious run, then snapshot the baseline (expected to be 1).
	time.Sleep(300 * time.Millisecond)
	baseline := readCounterValue(t, counterFile)
	if baseline != 1 {
		t.Fatalf("counter expected to be 1 after startup check, got %d", baseline)
	}

	// Drive several victim connection attempts. Each attempt forces one pass
	// through findFirstMissingResourceWhenServiceMutexIsLocked with
	// firstCheckNeeded=true. Under the old single-pass loop, ~half of these (map
	// iteration order) registered a first-change channel and Unpause-poked the
	// CheckCommand monitor before hitting the static bottleneck; under the fixed
	// two-pass code the static bottleneck is returned without touching the
	// CheckCommand resource at all.
	for i := 0; i < 6; i++ {
		victimConn, err := net.DialTimeout("tcp", victimProxyAddress, 3*time.Second)
		if err != nil {
			t.Fatalf("victim attempt %d: failed to connect: %v", i, err)
		}
		// Confirm the victim is blocked waiting for the static resource (so a pass
		// through findFirstMissingResourceWhenServiceMutexIsLocked has occurred),
		// then drop the connection so the next iteration gets a fresh pass.
		waitForServiceState(t, managementApiAddress, victimServiceName, ServiceStateWaitingForResources, 3*time.Second)
		_ = victimConn.Close()
		// Let the proxy tear down the victim's reservation before reconnecting.
		time.Sleep(100 * time.Millisecond)
	}

	// Allow any Unpause-poked CheckCommand run (old code) to land, plus margin
	// over the poke -> exec latency.
	time.Sleep(800 * time.Millisecond)

	final := readCounterValue(t, counterFile)
	if final != baseline {
		t.Errorf("CheckCommand ran while the victim was blocked on the static resource: counter went %d -> %d (expected no change). The two-pass fix must evaluate the static resource first and never Unpause the CheckCommand monitor when a static resource is the bottleneck.", baseline, final)
	}
}

// readCounterValue reads the integer stored in the given counter file, returning
// -1 if it is momentarily unparseable (e.g. partially written mid-redirect).
func readCounterValue(t *testing.T, path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read counter file %s: %v", path, err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return v
}

// TestWaitingConnectionCountReleasedWhenClientDisconnectsDuringStartup verifies
// that a client which disconnects while its service is still in the startup
// phase (after resources were reserved but before the service is ready) also
// releases its waiting-connection count promptly. This covers the healthcheck
// and connect-retry phases, not just the resource-wait phase covered by
// TestWaitingConnectionCountReleasedWhenClientDisconnects. Without aborting
// startup on client disconnect, the counter would stay inflated until the
// service eventually started or hit its startup timeout (potentially minutes).
func TestWaitingConnectionCountReleasedWhenClientDisconnectsDuringStartup(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2113"
	const slowConnectProxyAddress = "localhost:2114"
	const slowHealthcheckProxyAddress = "localhost:2115"
	const testName = "waiting-conn-released-startup"
	const slowConnectServiceName = testName + "_slow-connect"
	const slowHealthcheckServiceName = testName + "_slow-healthcheck"

	startupTimeoutMs := uint(60000)
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 2}},
		ManagementApi:      ManagementApi{ListenPort: "2113"},
		Services: []ServiceConfig{
			{
				// No healthcheck command, so the service sits in the connect-retry
				// phase (tryConnectingUntilTimeoutOrProcessExit) for a long time.
				Name:                       "slow-connect",
				ListenPort:                 "2114",
				ProxyTargetHost:            "localhost",
				ProxyTargetPort:            "12114",
				Command:                    "./test-server/test-server",
				Args:                       "-p 12114 -sleep-before-listening 30s",
				StartupTimeoutMilliseconds: &startupTimeoutMs,
				ResourceRequirements:       map[string]int{"CPU": 1},
			},
			{
				// A healthcheck command that always fails, so the service sits in
				// the healthcheck phase (performHealthCheck) for a long time.
				Name:                            "slow-healthcheck",
				ListenPort:                      "2115",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 "12115",
				Command:                         "./test-server/test-server",
				Args:                            "-p 12115 -sleep-before-listening 30s",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 200,
				StartupTimeoutMilliseconds:      &startupTimeoutMs,
				ResourceRequirements:            map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{slowConnectProxyAddress, slowHealthcheckProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// slow-connect: resources are reserved immediately, then the service spends
	// 30s before listening, so the connection waits in the connect-retry phase.
	connA, err := net.DialTimeout("tcp", slowConnectProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to slow-connect: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, slowConnectServiceName, 1, 3*time.Second)

	// slow-healthcheck: the healthcheck command always fails, so the connection
	// waits in the healthcheck phase until the startup timeout.
	connB, err := net.DialTimeout("tcp", slowHealthcheckProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to slow-healthcheck: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, slowHealthcheckServiceName, 1, 3*time.Second)

	// Both waiting clients disconnect while their services are still starting.
	_ = connA.Close()
	_ = connB.Close()

	// The waiting counts must return to 0 promptly, well before the 30s/60s
	// startup windows would otherwise elapse.
	waitForWaitingConnections(t, managementApiAddress, slowConnectServiceName, 0, 5*time.Second)
	waitForWaitingConnections(t, managementApiAddress, slowHealthcheckServiceName, 0, 5*time.Second)
}

// TestQueuedClientDisconnectDropsWaitingCount is a regression test for prompt queued-client disconnect
// covering clients that arrive while their target service is ALREADY starting.
// Such a client cannot TryLock the service's manageMutex and so it QUEUES behind
// the holder. Previously the queue was a plain manageMutex.Lock() that never
// selected on this queued client's clientDisconnected signal, so:
//   - its waiting-connection count stayed inflated until the holder finished or
//     aborted (violating the prompt-drop requirement), and
//   - when the holder aborted, the queued client recursed into a fresh
//     startService even though it had already disconnected (wasted work).
//
// Scenario (a single slow-starting service; both clients target its proxy port):
//  1. Client A dials → service starts and gets stuck in the connect-retry phase,
//     holding manageMutex (state == starting, waiting == 1).
//  2. Client B dials the SAME port → B queues on manageMutex (waiting == 2).
//  3. Client B disconnects while queued → waiting must drop back to 1 PROMPTLY
//     (the H1 assertion: under the old code it stayed at 2 until A's ~30s startup
//     finished).
//  4. Client A disconnects → waiting drops to 0 promptly.
func TestQueuedClientDisconnectDropsWaitingCount(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2122"
	const serviceProxyAddress = "localhost:2123"
	const testName = "queued-client-disconnect"
	const serviceName = testName + "_svc"

	// One slow-starting service: a static resource (immediately available, so the
	// service starts right away) and a 30s pre-listen sleep with no healthcheck,
	// which keeps it parked in the connect-retry phase — and thus holding
	// manageMutex — for the whole test.
	startupTimeoutMs := uint(60000)
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 2}},
		ManagementApi:      ManagementApi{ListenPort: "2122"},
		Services: []ServiceConfig{
			{
				Name:                       "svc",
				ListenPort:                 "2123",
				ProxyTargetHost:            "localhost",
				ProxyTargetPort:            "12123",
				Command:                    "./test-server/test-server",
				Args:                       "-p 12123 -sleep-before-listening 30s",
				StartupTimeoutMilliseconds: &startupTimeoutMs,
				ResourceRequirements:       map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		if err := checkPortClosed(serviceProxyAddress); err != nil {
			t.Errorf("port %s is still open after application exit: %v", serviceProxyAddress, err)
		}
		if err := checkPortClosed(managementApiAddress); err != nil {
			t.Errorf("port %s is still open after application exit: %v", managementApiAddress, err)
		}
	}()

	// 1. Client A dials the service proxy port → service starts and gets stuck in
	//    the connect-retry phase, holding manageMutex. Wait until the service is
	//    registered and reports "starting" so that B is guaranteed to take the
	//    queued path rather than racing the registration.
	clientA, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect client A: %v", err)
	}
	defer func() { _ = clientA.Close() }()
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStarting, 5*time.Second)
	waitForWaitingConnections(t, managementApiAddress, serviceName, 1, 3*time.Second)

	// 2. Client B dials the SAME service proxy port → B cannot TryLock
	//    manageMutex (held by A) and queues. waiting_connections must be 2.
	clientB, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect client B: %v", err)
	}
	defer func() { _ = clientB.Close() }()
	waitForWaitingConnections(t, managementApiAddress, serviceName, 2, 3*time.Second)

	// 3. Client B disconnects WHILE QUEUED. Its waiting count must drop back to 1
	//    promptly — well before A's ~30s startup finishes. Under the old code
	//    (blocking manageMutex.Lock() ignoring clientDisconnected) this stayed at
	//    2 until A's startup completed/aborted, so the short timeout fails fast on
	//    a regression.
	_ = clientB.Close()
	waitForWaitingConnections(t, managementApiAddress, serviceName, 1, 3*time.Second)

	// 4. Client A (the holder) disconnects. Startup is aborted via the holder
	//    disconnect path and waiting drops to 0 promptly.
	_ = clientA.Close()
	waitForWaitingConnections(t, managementApiAddress, serviceName, 0, 5*time.Second)
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 10*time.Second)
}

// TestRawCaptureConnectionStopsBuffering verifies that a rawCaptureConnection
// captures bytes into its buffer while active, that the bytes extracted before
// stopping remain valid, and that after stopBuffering further reads are
// forwarded without being captured (and without panicking on a nil buffer).
func TestRawCaptureConnectionStopsBuffering(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	rcc := &rawCaptureConnection{
		Conn:    serverConn,
		buffer:  new(bytes.Buffer),
		capture: true,
	}

	readAll := func(want string) {
		t.Helper()
		buf := make([]byte, 64)
		n, err := rcc.Read(buf)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if string(buf[:n]) != want {
			t.Fatalf("Read got %q, want %q", string(buf[:n]), want)
		}
	}

	// While capturing, bytes are stored in the buffer.
	go func() { _, _ = clientConn.Write([]byte("request")) }()
	readAll("request")
	if rcc.buffer.String() != "request" {
		t.Fatalf("buffer = %q, want %q", rcc.buffer.String(), "request")
	}

	// Bytes extracted before stopping stay valid after the buffer is dropped.
	raw := rcc.buffer.Bytes()
	rcc.stopBuffering()
	if string(raw) != "request" {
		t.Fatalf("raw = %q, want %q", string(raw), "request")
	}
	if rcc.buffer != nil {
		t.Fatalf("buffer should be nil after stopBuffering")
	}

	// After stopping, reads still work but are no longer captured.
	go func() { _, _ = clientConn.Write([]byte("stream")) }()
	readAll("stream")
	if rcc.buffer != nil {
		t.Fatalf("buffer must remain nil after stopBuffering")
	}
}

// TestStartClientReadMonitorPreservesRequestBytes pins that request bytes the
// client sends during the startup window (before forwardConnection begins draining
// the reader) must not be lost. startClientReadMonitor hands back a reader that
// yields the exact bytes the client wrote, in order, even though those bytes were
// written before any read was issued and sat buffered through the startup wait.
// A real loopback TCP connection is used (not net.Pipe) so the kernel buffers the
// peer's Write and it can complete before any Read is issued.
func TestStartClientReadMonitorPreservesRequestBytes(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	peerConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer peerConn.Close()
	clientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer clientConn.Close()

	request := []byte("GET / HTTP/1.1\r\nHost: example\r\n\r\nsome-request-body")

	// Write the full request BEFORE starting the monitor / before any consumer
	// reads, to simulate a client-speaks-first protocol whose bytes arrive while
	// the service is still starting.
	if _, err := peerConn.Write(request); err != nil {
		t.Fatalf("peer Write failed: %v", err)
	}

	reader, closeReader, _ := startClientReadMonitor(clientConn)
	defer closeReader()

	// Simulate the startup window: forwardConnection has not started draining yet.
	time.Sleep(100 * time.Millisecond)

	// Now drain exactly len(request) bytes (as forwardConnection would) and assert
	// they are the exact, in-order request bytes — none lost across the wait.
	received := make([]byte, len(request))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(reader, received); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadFull failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for buffered request bytes to be delivered")
	}
	if !bytes.Equal(received, request) {
		t.Fatalf("received %q, want exact request %q", string(received), string(request))
	}
}

// TestBufferedPipeBackpressureBlocksWrite verifies that once the buffered backlog
// reaches the cap, Write blocks (re-applying backpressure) until a Read frees
// space — closing the unbounded-buffer memory hole during startup.
func TestBufferedPipeBackpressureBlocksWrite(t *testing.T) {
	t.Parallel()
	const cap = 8
	pipe := newBufferedPipe(cap)

	// Fill the buffer to the cap (allowed into an empty buffer) plus a bit more
	// via subsequent writes that each individually fit but collectively exceed.
	if _, err := pipe.Write([]byte("01234567")); err != nil { // exactly cap, empty buffer => allowed
		t.Fatalf("initial write failed: %v", err)
	}

	writeDone := make(chan int, 1)
	go func() {
		n, err := pipe.Write([]byte("overflow"))
		if err != nil {
			writeDone <- -1
			return
		}
		writeDone <- n
	}()

	// While the buffer is full, the blocked Write must not complete.
	select {
	case n := <-writeDone:
		t.Fatalf("Write completed while buffer was full (n=%d); backpressure is missing", n)
	case <-time.After(50 * time.Millisecond):
	}

	// Drain some bytes; the blocked Write should now complete.
	buf := make([]byte, cap)
	if _, err := io.ReadFull(pipe, buf); err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}
	select {
	case n := <-writeDone:
		if n != len("overflow") {
			t.Fatalf("blocked Write returned n=%d, want %d", n, len("overflow"))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("blocked Write did not complete after the buffer was drained")
	}
}

// TestBufferedPipeZeroLimitIsUnbounded verifies that limit == 0 (the operator
// escape hatch) disables backpressure: a write far larger than 0 completes without
// a reader ever draining the pipe.
func TestBufferedPipeZeroLimitIsUnbounded(t *testing.T) {
	t.Parallel()
	pipe := newBufferedPipe(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := pipe.Write(make([]byte, 1<<20)); err != nil { // 1 MiB, no reader
			t.Errorf("unbounded Write failed: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("limit=0 Write blocked; expected unbounded/no-backpressure behavior")
	}
}

// TestChannelMutexLockOrCancel verifies the channel-backed mutex semantics that
// startServiceIfNotAlreadyRunningAndConnect relies on: TryLock succeeds on a
// fresh/unlocked mutex and fails while held; LockOrCancel blocks while held,
// acquires the instant the token is returned, and returns false (without
// acquiring) the instant the cancel channel closes.
func TestChannelMutexLockOrCancel(t *testing.T) {
	t.Parallel()
	m := newChannelMutex()

	// Fresh mutex is unlocked; re-lock fails until Unlock.
	if !m.TryLock() {
		t.Fatalf("TryLock on fresh mutex should succeed")
	}
	if m.TryLock() {
		t.Fatalf("TryLock on held mutex should fail")
	}
	m.Unlock()
	if !m.TryLock() {
		t.Fatalf("TryLock after Unlock should succeed")
	}
	m.Unlock()

	// LockOrCancel blocks while held, then acquires on release.
	m.Lock()
	acquired := make(chan bool, 1)
	go func() { acquired <- m.LockOrCancel(make(chan struct{})) }() // never-canceling cancel chan
	select {
	case <-acquired:
		t.Fatalf("LockOrCancel acquired while mutex was held")
	case <-time.After(50 * time.Millisecond):
	}
	m.Unlock()
	select {
	case got := <-acquired:
		if !got {
			t.Fatalf("LockOrCancel returned false after release; want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("LockOrCancel did not acquire after the token was returned")
	}
	m.Unlock() // release what the goroutine acquired

	// LockOrCancel returns false when cancel closes before the token is available.
	m.Lock()
	cancel := make(chan struct{})
	gotCancel := make(chan bool, 1)
	go func() { gotCancel <- m.LockOrCancel(cancel) }()
	select {
	case <-gotCancel:
		t.Fatalf("LockOrCancel returned before cancel while mutex was held")
	case <-time.After(50 * time.Millisecond):
	}
	close(cancel)
	select {
	case got := <-gotCancel:
		if got {
			t.Fatalf("LockOrCancel returned true on cancel; want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("LockOrCancel did not return after cancel closed")
	}
	m.Unlock()
}

// TestStartClientReadMonitorReturnsEOFAfterCleanClose pins the behavior that
// broke startup-timeout-cleanup / client-close-full: after the client closes
// its side cleanly (a normal EOF), io.Copy in the monitor returns a nil error,
// so the reader MUST still surface io.EOF once its buffered bytes are drained.
func TestStartClientReadMonitorReturnsEOFAfterCleanClose(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	peerConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer peerConn.Close()
	clientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer clientConn.Close()

	request := []byte("ping")
	if _, err := peerConn.Write(request); err != nil {
		t.Fatalf("peer Write failed: %v", err)
	}
	// Clean close from the client side (sends a FIN, no error). io.Copy in the
	// monitor returns nil here, which is exactly the case the EOF handling must
	// not drop.
	if err := peerConn.Close(); err != nil {
		t.Fatalf("peer Close failed: %v", err)
	}

	reader, closeReader, _ := startClientReadMonitor(clientConn)
	defer closeReader()

	// First the buffered bytes must be delivered ...
	received := make([]byte, len(request))
	if _, err := io.ReadFull(reader, received); err != nil {
		t.Fatalf("ReadFull of buffered bytes failed: %v", err)
	}
	if !bytes.Equal(received, request) {
		t.Fatalf("received %q, want %q", string(received), string(request))
	}
	// ... then the reader MUST return io.EOF (not block) so that forwardConnection
	// finishes and the connection's bookkeeping is released.
	readErrChan := make(chan error, 1)
	one := make([]byte, 1)
	go func() { _, err := reader.Read(one); readErrChan <- err }()
	select {
	case err := <-readErrChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF after clean client close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out: reader did not return io.EOF after a clean client close (forwardConnection would hang and leak the connection)")
	}
}

// A client that sends SOME request bytes and THEN
// disconnects must be observed promptly by the monitor, even though bytes were
// sent and nobody is consuming the read end yet (the situation during service
// startup, before forwardConnection begins). Under the OLD synchronous io.Pipe
// the monitor would stall inside pipeWriter.Write (blocked waiting for a reader
// that does not exist until startup finishes), stop issuing Read on the client
// connection, and thus never observe the close — the disconnected channel would
// not close until the startup timeout / maxWait. With the unbounded buffered
// pipe, Write returns immediately so the monitor keeps reading the client and
// observes the close promptly. (We do not consume the reader, to mirror the
// pre-forwardConnection startup window.)
func TestStartClientReadMonitorDetectsDisconnectAfterBytes(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	peerConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	clientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}

	reader, closeReader, disconnected := startClientReadMonitor(clientConn)
	defer closeReader()
	defer clientConn.Close()
	_ = reader // intentionally not consumed: mirrors the pre-forwardConnection startup window

	// Send some request bytes first — this is exactly the condition that triggers
	// the issue (the old io.Pipe would then stall inside pipeWriter.Write).
	if _, err := peerConn.Write([]byte("GET / HTTP/1.1\r\nHost: example\r\n\r\n")); err != nil {
		t.Fatalf("peer Write failed: %v", err)
	}
	// Give the monitor a moment to read the bytes and settle into its next Read
	// on the client connection.
	time.Sleep(100 * time.Millisecond)

	// The client disconnects after sending bytes. The monitor must observe this
	// promptly because it keeps issuing Read on clientConnection.
	if err := peerConn.Close(); err != nil {
		t.Fatalf("peer Close failed: %v", err)
	}

	select {
	case <-disconnected:
		// disconnect detected promptly — pass
	case <-time.After(2 * time.Second):
		t.Fatalf("client disconnect after sending bytes was not detected within 2s (Issue B regression)")
	}
}

// TestResourceWaitMaxTimeoutCleanup is a regression test for the max-wait-timeout cleanup path: the
// resource-wait event channels must be cleaned up (deleted from the registry)
// on EVERY exit path in reserveResources — reservation success, max-wait
// timeout, and client disconnect. The client-disconnect path is already covered
// by TestWaitingConnectionCountReleasedWhenClientDisconnects; this test covers
// the max-wait-TIMEOUT exit path.
//
// The integration tests run the proxy as a subprocess, so the in-memory
// registry maps (resourceChangeByResourceChans etc.) cannot be inspected
// directly. Instead this test asserts the observable consequences of correct
// cleanup.
//
// Scenario:
//  1. A HOLDER service starts and keeps a proxied connection (and therefore
//     TestResource) open well beyond the test. Because it has an active proxied
//     connection, canBeStopped reports false, so it cannot be evicted.
//  2. A WAITER connection targets a second service that needs the same
//     resource. It cannot start and is counted as a waiting connection
//     (WaitingConnections == 1).
//  3. The holder cannot be evicted and the resource never frees, so the waiter
//     waits until MaxTimeToWait… expires (set short, 2s, for the test) and then
//     gives up. The max-wait-timeout exit path must delete the waiter's channel
//     registration.
//  4. Assert the waiter aborted on timeout: WaitingConnections drops back to 0
//     and the waiter's client connection is closed by the proxy (Read errors).
//  5. "No stranded waiter" (the real point of this test): close the HOLDER's
//     connection so TestResource frees, then dial the WAITER again with a fresh
//     client. If the timed-out waiter's channel registration had leaked, the
//     fresh start could be affected; a prompt start confirms clean state.
func TestResourceWaitMaxTimeoutCleanup(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2170"
	const holderProxyAddress = "localhost:2171"
	const waiterProxyAddress = "localhost:2172"
	const testName = "resource-wait-max-timeout-cleanup"
	const holderServiceName = testName + "_holder"
	const waiterServiceName = testName + "_waiter"

	// Short max-wait so the timeout fires quickly (well under the default 120s).
	maxWait := uint(2)
	maxWaitDuration := time.Duration(maxWait) * time.Second

	// The holder keeps its proxied connection open well beyond the duration of
	// this test (30s) so TestResource stays unavailable to the waiter the whole
	// time, and so the holder is never evictable (canBeStopped == false while it
	// has a proxied connection).
	cfg := Config{
		MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds: &maxWait,
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2170"},
		Services: []ServiceConfig{
			{
				Name:                 "holder",
				ListenPort:           "2171",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12171",
				Command:              "./test-server/test-server",
				Args:                 "-p 12171 -sleep-after-writing-pid-duration 30s",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
			{
				Name:                 "waiter",
				ListenPort:           "2172",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12172",
				Command:              "./test-server/test-server",
				Args:                 "-p 12172",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{holderProxyAddress, waiterProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// 1. Start the holder and keep its connection open so it holds TestResource.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder: %v", err)
	}
	defer func() { _ = holderConn.Close() }()
	readPidFromOpenConnection(t, holderConn)
	// holder is now running with one proxied connection, holding TestResource.
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, holderServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})

	// 2. A new connection to the waiter must wait for TestResource.
	waiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 1, 3*time.Second)

	// 3. The waiter cannot get TestResource (held by the non-evictable holder) and
	//    no service can be evicted, so it waits until MaxTimeToWait… expires.
	// 4. Assert the waiter aborted on timeout: WaitingConnections drops back to 0
	//    (its channel registration was cleaned up on the max-wait exit path).
	//
	// The margin comfortably exceeds the short max-wait plus scheduling/process
	// overhead on a loaded machine.
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 0, maxWaitDuration+6*time.Second)

	// The waiter's client connection must have been closed by the proxy when it
	// gave up waiting. A Read must return an error (io.EOF or reset). Use a
	// goroutine + select so the test fails fast instead of hanging if the close
	// never arrives.
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, readErr := waiterConn.Read(buf)
		readDone <- readErr
	}()
	select {
	case readErr := <-readDone:
		if readErr == nil {
			t.Fatal("expected the waiter's client connection to be closed by the proxy after the max-wait timeout, but Read returned no error")
		}
		// readErr is io.EOF / connection reset — both indicate the proxy closed it.
	case <-time.After(maxWaitDuration + 6*time.Second):
		t.Fatalf("waiter's client connection was not closed within %v of the max-wait timeout", maxWaitDuration+6*time.Second)
	}
	_ = waiterConn.Close()

	// 5. "No stranded waiter": close the HOLDER's connection so TestResource
	//    frees (the holder becomes evictable once its proxied count hits 0), then
	//    dial the WAITER again with a fresh client. It must start promptly — if
	//    the timed-out waiter's channel registration had leaked/stranded this
	//    fresh start could be affected.
	_ = holderConn.Close()
	// Wait until the holder has released its proxied connection (and is thus
	// evictable), so the prompt-start assertion below is not racing the
	// connection-close accounting.
	waitForProxiedConnections(t, managementApiAddress, holderServiceName, 0, 5*time.Second)

	freshWaiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter (fresh): %v", err)
	}
	defer func() { _ = freshWaiterConn.Close() }()
	// The fresh waiter must become Running within a few seconds — confirming the
	// proxy is in clean state after the prior max-wait-timeout exit.
	waitForServiceState(t, managementApiAddress, waiterServiceName, ServiceStateRunning, 8*time.Second)
}

// TestResourceChangeBroadcastOnLastProxiedConnectionClose is a regression test
// for the last-connection-close broadcast. When a service's LAST proxied (or waiting) connection closes,
// incrementConnection broadcasts a resource-change event for that service's
// resources so any waiter is re-evaluated promptly (rather than waiting for the
// holder's idle timeout or the waiter's max-wait timer to fire).
//
// This broadcast is the only thing that lets a waiter unblock within SECONDS of
// a holder's last-close: after the holder's proxied count hits 0 it becomes
// evictable (canBeStopped == true), but without the broadcast nobody would
// re-check resource availability until either the holder idles out (its idle
// timer) or the waiter hits its max-wait. Both of those are configured here to
// be far longer than the asserted window, so a prompt start can only be
// explained by the last-close broadcast.
//
// Setup: a HOLDER holds the single unit of TestResource with a LONG idle timeout
// (300s) and a process that stays up for 60s after writing its PID, so it does
// NOT idle out or exit during the test — it keeps holding TestResource until it
// is evicted. A WAITER requires the same resource and blocks in reserveResources.
// We close the holder's LAST proxied connection and assert the waiter becomes
// Running within a few seconds — only the last-close broadcast (→ waiter
// re-evaluation → holder eviction → TestResource freed and re-reserved) can do
// it that fast. A generous MaxTimeToWait (120s) ensures the waiter is not timing
// out. The holder ends up Stopped (it was evicted).
func TestResourceChangeBroadcastOnLastProxiedConnectionClose(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2180"
	const holderProxyAddress = "localhost:2181"
	const waiterProxyAddress = "localhost:2182"
	const testName = "resource-change-broadcast-on-last-proxied-close"
	const holderServiceName = testName + "_holder"
	const waiterServiceName = testName + "_waiter"

	// The holder's idle timeout (300s) is far longer than anything this test
	// waits for, so the holder does NOT idle out after its last proxied conn
	// closes. Its process also stays up for 60s after writing its PID, so it
	// keeps holding TestResource until it is evicted. The waiter's max-wait
	// (120s) is also generous, so the waiter is not timing out during the test.
	holderIdleTimeoutSeconds := uint(300)
	maxWaitSeconds := uint(120)
	cfg := Config{
		MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds: &maxWaitSeconds,
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2180"},
		Services: []ServiceConfig{
			{
				Name:                           "holder",
				ListenPort:                     "2181",
				ProxyTargetHost:                "localhost",
				ProxyTargetPort:                "12181",
				Command:                        "./test-server/test-server",
				Args:                           "-p 12181 -sleep-after-writing-pid-duration 60s",
				ShutDownAfterInactivitySeconds: holderIdleTimeoutSeconds,
				ResourceRequirements:           map[string]int{"TestResource": 1},
			},
			{
				Name:                 "waiter",
				ListenPort:           "2182",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12182",
				Command:              "./test-server/test-server",
				Args:                 "-p 12182 -sleep-after-writing-pid-duration 30s",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{holderProxyAddress, waiterProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// 1. Start the holder and keep its proxied connection open so it holds TestResource.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder: %v", err)
	}
	defer func() { _ = holderConn.Close() }()
	readPidFromOpenConnection(t, holderConn)
	// holder is now running with one proxied connection, holding TestResource.
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, holderServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})

	// 2. A new connection to the waiter must wait for TestResource (held by the
	//    non-evictable holder, which still has a proxied connection open).
	waiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter: %v", err)
	}
	defer func() { _ = waiterConn.Close() }()
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 1, 3*time.Second)

	// 3. Close the holder's LAST proxied connection. This must trigger the last-connection-close
	//    broadcast (the holder's proxied AND waiting counts are now both 0). The
	//    broadcast wakes the waiter, which re-evaluates: the holder is now idle
	//    (proxied==0 → canBeStopped true), so the waiter evicts it, frees
	//    TestResource, and reserves it.
	_ = holderConn.Close()

	// 4. The waiter must become Running PROMPTLY. The asserted window (8s) is far
	//    smaller than the holder's idle timeout (300s) AND the waiter's max-wait
	//    (120s), so the ONLY mechanism that can unblock the waiter this fast is
	//    the last-close broadcast → eviction. Without it the waiter would sit
	//    blocked until the holder idles out (300s) or the max-wait fires.
	waitForServiceState(t, managementApiAddress, waiterServiceName, ServiceStateRunning, 8*time.Second)

	// 5. The holder must have been evicted to free TestResource for the waiter.
	// The waiter's waiting connection converts to proxied slightly after the
	// Running state, so poll for proxied==1 (same race as the multi-connection
	// handover test), then confirm the proxied data path by reading the PID the
	// waiter's service wrote back.
	waitForProxiedConnections(t, managementApiAddress, waiterServiceName, 1, 5*time.Second)
	readPidFromOpenConnection(t, waiterConn)
	finalStatus := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, finalStatus, holderServiceName, ServiceStateStopped, 0, 0, map[string]int{"TestResource": 0})
	verifyServiceStatus(t, finalStatus, waiterServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})
}

// TestServiceMutexNotHeldDuringSlowCheckCommand pins that, while a
// service waits for a CheckCommand-backed resource whose measurement is in
// flight, the waiter blocks on a channel and the global serviceMutex is NOT held
// for the (potentially slow) duration of the CheckCommand.
//
// handleStatus (the /status management endpoint) acquires serviceMutex, so if
// serviceMutex were held while a slow CheckCommand executed, /status would block
// for the whole duration of that check. This test proves the invariant
// behaviorally: it configures a resource whose CheckCommand is slow AND reports
// an insufficient amount ("sleep 2; echo 0" -> ~2s, 0 available), connects a
// client whose service requires 1 unit (forcing reserveResources to request a
// first CheckCommand run via UnpauseResourceAvailabilityMonitoring and then wait
// on a channel in waitForFirstCheckCommands with serviceMutex released), and
// samples /status latency throughout the ~2s check window. Every sample must
// return in well under the check duration (sub-second) — if any sample
// approaches the ~2s check duration, serviceMutex was held during the check and
// the invariant is violated.
func TestServiceMutexNotHeldDuringSlowCheckCommand(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2190"
	const serviceProxyAddress = "localhost:2191"
	const testName = "service-mutex-not-held-during-slow-check"
	const serviceName = testName + "_svc"
	const slowGpu = "SlowGpu"

	// "sleep 2; echo 0" -> the monitor's CheckCommand takes ~2s and reports 0
	// available, so a service requiring 1 unit can never reserve and stays parked
	// in waiting_for_resources. The long CheckWhenNotEnoughIntervalMilliseconds
	// keeps the monitor from re-running the command on a short interval during the
	// test, so the only check in flight while the waiter is blocked is the one
	// triggered by the client's reserveResources.
	const checkCommand = "sleep 2; echo 0"
	checkDuration := 2 * time.Second

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			slowGpu: {
				CheckCommand:                           checkCommand,
				CheckWhenNotEnoughIntervalMilliseconds: 60000,
			},
		},
		LogLevel:      LogLevelDebug,
		ManagementApi: ManagementApi{ListenPort: "2190"},
		Services: []ServiceConfig{
			{
				Name:                 "svc",
				ListenPort:           "2191",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12191", // need not be reachable: the service never gets past resource reservation
				Command:              "./test-server/test-server",
				Args:                 "-p 12191",
				ResourceRequirements: map[string]int{slowGpu: 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// The resource monitor fires its initial CheckCommand immediately at startup
	// (time.NewTimer(0)); that first run also takes ~checkDuration. Wait for it to
	// finish so that the check sampled below is the one triggered by the client's
	// reserveResources (via UnpauseResourceAvailabilityMonitoring), giving a
	// deterministic ~checkDuration window in which the waiter is blocked on the
	// first-check channel with serviceMutex released. startLargeModelProxy already
	// slept ~1s after start, so sleeping another checkDuration lands well past the
	// initial check's completion (~checkDuration after process start).
	time.Sleep(checkDuration)

	// Dial the service proxy port. This triggers handleConnection -> startService
	// -> reserveResources, which registers a first-check channel and pokes the
	// monitor (UnpauseResourceAvailabilityMonitoring) to run the slow CheckCommand,
	// then calls waitForFirstCheckCommands with serviceMutex RELEASED. Keep the
	// conn open so the waiter stays parked in the wait through the sampling window.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy port: %v", err)
	}

	// Sample /status latency throughout the slow CheckCommand window. handleStatus
	// takes serviceMutex, so if serviceMutex were held during the check, each
	// overlapping /status request would block for ~checkDuration. The generous
	// client Timeout is just a safety net; loopback /status normally returns in a
	// few milliseconds.
	statusClient := &http.Client{Timeout: 5 * time.Second}
	const statusLatencyLimit = 500 * time.Millisecond // far under checkDuration (2s); far above normal loopback latency
	const sampleInterval = 200 * time.Millisecond
	sampleWindow := checkDuration + 800*time.Millisecond
	deadline := time.Now().Add(sampleWindow)
	var maxStatusLatency time.Duration
	sampleCount := 0
	for time.Now().Before(deadline) {
		reqStart := time.Now()
		resp, err := statusClient.Get(fmt.Sprintf("http://%s/status", managementApiAddress))
		latency := time.Since(reqStart)
		if err != nil {
			t.Fatalf("/status request failed while the slow CheckCommand was running: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/status returned status %d during the slow CheckCommand", resp.StatusCode)
		}
		if latency > maxStatusLatency {
			maxStatusLatency = latency
		}
		sampleCount++
		if remain := sampleInterval - time.Since(reqStart); remain > 0 {
			time.Sleep(remain)
		}
	}
	t.Logf("sampled /status %d times over %v while the slow CheckCommand ran; max latency %v (limit %v)",
		sampleCount, sampleWindow, maxStatusLatency, statusLatencyLimit)

	if maxStatusLatency >= statusLatencyLimit {
		t.Errorf(
			"/status was not responsive while a slow CheckCommand ran for a waiting service: max latency %v over %d samples >= limit %v. handleStatus takes serviceMutex, so this means serviceMutex was held during the CheckCommand, violating the invariant that the waiter must block on a channel with serviceMutex released, not under the lock).",
			maxStatusLatency, sampleCount, statusLatencyLimit,
		)
	}

	// Sanity: confirm the wait-on-channel path was actually exercised. The first
	// CheckCommand ran and reported 0 (< 1 required), so the service must be
	// parked in waiting_for_resources — proving the waiter is blocked on a channel
	// (waiting on a channel, not busy-polling) and never reached starting. The timeout covers
	// the check duration plus scheduling margin.
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateWaitingForResources, checkDuration+2*time.Second)

	// Tear down the waiter cleanly: closing the client unblocks reserveResources
	// on the client-disconnect path so the proxy is in a clean state before the
	// deferred SIGINT, rather than racing a still-blocked waiter.
	_ = clientConn.Close()
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
}

// Scenario: the healthcheck command always fails ("false"), so the service
// sits in the healthcheck phase. The process itself exits after 500ms (well
// before the 60s startup timeout), so the process dies DURING the healthcheck
// phase. With ConsiderStoppedOnProcessExit=true, performHealthCheck must abort
// the moment the process exits instead of keep re-spawning the failing
// healthcheck subprocess until StartupTimeout. The sibling test
// TestProcessExitDuringHealthCheckDoesNotAbortWhenConsiderStoppedOnProcessExitFalse
// verifies the opposite behavior holds when ConsiderStoppedOnProcessExit is
// false.
func TestProcessExitDuringHealthCheckAbortsHealthcheckLoop(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2124"
	const serviceProxyAddress = "localhost:2125"
	const testName = "process-exit-during-healthcheck"
	const serviceName = testName + "_dying-process"

	// The healthcheck command always fails, so the service sits in the
	// healthcheck phase. The process sleeps 30s before it would start listening
	// on the target port, but exits after 500ms — so the process dies during the
	// healthcheck phase, well before the 60s startup timeout.
	startupTimeoutMs := uint(60000)
	considerStoppedOnProcessExit := true
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2124"},
		Services: []ServiceConfig{
			{
				Name:                            "dying-process",
				ListenPort:                      "2125",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 "12125",
				Command:                         "./test-server/test-server",
				Args:                            "-p 12125 -sleep-before-listening 30s -exit-after-duration 500ms",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 200,
				StartupTimeoutMilliseconds:      &startupTimeoutMs,
				ConsiderStoppedOnProcessExit:    &considerStoppedOnProcessExit,
				RestartOnConnectionFailure:      false,
				ResourceRequirements:            map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Connect a client to trigger startService. The client stays connected while
	// the service sits in the healthcheck phase.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// The service reserves TestResource and enters the starting/healthcheck
	// phase, so the resource is held: in_use == 1 and free == 0. Poll, since the
	// reservation and the state transition are observed asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		if info, ok := resp.Resources["TestResource"]; ok && info.InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{"TestResource": 1}, // reserved by starting services
				map[string]int{"TestResource": 0}, // free (held by the starting service)
				map[string]int{"TestResource": 1}, // in_use
				map[string]int{"TestResource": 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource TestResource was never held (in_use never reached 1) within %s", 3*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// With the fix, performHealthCheck aborts as soon as the process exits
	// (~500ms) instead of spinning for the full 60s StartupTimeout. The service
	// returns to "stopped" and the resource is fully freed promptly — well
	// before the 60s window. waitForServiceState's 5s deadline proves the loop
	// did not run to StartupTimeout.
	resp := waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 0}, // reserved by starting services
		map[string]int{"TestResource": 1}, // free (full total — no leak)
		map[string]int{"TestResource": 0}, // in_use (no leak)
		map[string]int{"TestResource": 1}, // total
	)

	// The waiting client connection must be closed (EOF) promptly too, instead
	// of hanging until the startup timeout.
	assertRemoteClosedWithin(t, clientConn, 2*time.Second)
}

// TestProcessExitDuringHealthCheckDoesNotAbortWhenConsiderStoppedOnProcessExitFalse
// is the counterpart to TestProcessExitDuringHealthCheckAbortsHealthcheckLoop.
// For services whose process detaches from the proxy (ConsiderStoppedOnProcessExit=false,
// e.g. docker containers), the child process exiting is expected and does not
// mean the service is down, so performHealthCheck must NOT abort on process
// exit — it keeps re-running the healthcheck command until StartupTimeout.
//
// Scenario: same as the abort test (healthcheck command always fails, process
// exits after 500ms), but ConsiderStoppedOnProcessExit=false and StartupTimeout
// is only 3s. After the process exits we assert the resource is STILL held (the
// loop did not abort), and that it is only released once the StartupTimeout
// elapses and the healthcheck times out.
func TestProcessExitDuringHealthCheckDoesNotAbortWhenConsiderStoppedOnProcessExitFalse(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2126"
	const serviceProxyAddress = "localhost:2127"
	const testName = "process-exit-during-healthcheck-no-abort"
	const serviceName = testName + "_dying-process"
	const proxyTargetPort = "12126"

	// The process exits after 500ms, but the healthcheck loop must keep going
	// until the 3s StartupTimeout, since ConsiderStoppedOnProcessExit is false.
	startupTimeoutMs := uint(3000)
	considerStoppedOnProcessExit := false
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2126"},
		Services: []ServiceConfig{
			{
				Name:                            "dying-process",
				ListenPort:                      "2127",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 proxyTargetPort,
				Command:                         "./test-server/test-server",
				Args:                            "-p " + proxyTargetPort + " -sleep-before-listening 30s -exit-after-duration 500ms",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 200,
				StartupTimeoutMilliseconds:      &startupTimeoutMs,
				ConsiderStoppedOnProcessExit:    &considerStoppedOnProcessExit,
				RestartOnConnectionFailure:      false,
				ResourceRequirements:            map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Connect a client to trigger startService. The client stays connected while
	// the service sits in the healthcheck phase.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// The service reserves TestResource and enters the starting/healthcheck
	// phase, so the resource is held: in_use == 1 and free == 0. Poll, since the
	// reservation and the state transition are observed asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		if info, ok := resp.Resources["TestResource"]; ok && info.InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{"TestResource": 1}, // reserved by starting services
				map[string]int{"TestResource": 0}, // free (held by the starting service)
				map[string]int{"TestResource": 1}, // in_use
				map[string]int{"TestResource": 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource TestResource was never held (in_use never reached 1) within %s", 3*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The process exits after 500ms. Because ConsiderStoppedOnProcessExit is
	// false, monitorProcess does not clean up and performHealthCheck must NOT
	// abort. So even after the process has exited the service stays "starting"
	// and the resource stays held until the StartupTimeout (3s) elapses. Waiting
	// 1s here lands safely past the 500ms process exit but ~2s before the 3s
	// StartupTimeout, proving the loop kept going.
	time.Sleep(1 * time.Second)
	resp := getStatusFromManagementAPI(t, managementApiAddress)
	// The client is still waiting for the service to become ready (the healthcheck
	// is still failing), so it counts as one waiting connection.
	verifyServiceStatus(t, resp, serviceName, ServiceStateStarting, 1, 0, map[string]int{"TestResource": 1})
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 1}, // reserved by starting services
		map[string]int{"TestResource": 0}, // free (still held by the starting service)
		map[string]int{"TestResource": 1}, // in_use
		map[string]int{"TestResource": 1}, // total
	)

	// Once the 3s StartupTimeout elapses, the healthcheck times out, startService
	// returns, and the service stops and frees the resource. The 5s deadline
	// proves cleanup did happen (as opposed to hanging forever).
	resp = waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 0}, // reserved by starting services
		map[string]int{"TestResource": 1}, // free (full total — no leak)
		map[string]int{"TestResource": 0}, // in_use (no leak)
		map[string]int{"TestResource": 1}, // total
	)

	// The waiting client connection is closed (EOF) once the healthcheck times
	// out and the service stops.
	assertRemoteClosedWithin(t, clientConn, 2*time.Second)
}

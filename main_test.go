package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
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

func TestAppScenarios(test *testing.T) {
	tests := []struct {
		Name                          string
		GetConfig                     func(t *testing.T, testName string) Config
		AddressesToCheckAfterStopping []string
		TestFunc                      func(t *testing.T)
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
					ResourcesAvailable: map[string]int{"VRAM": 20},
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
					ResourcesAvailable:             map[string]int{"RAM": 1},
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
					ResourcesAvailable: map[string]int{"VRAM": 1},
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
							KillCommand:     ptrToString("echo -n 'success' > /tmp/test-server-kill-command-output"),
						},
					},
				}
			},
			AddressesToCheckAfterStopping: []string{"localhost:2034", "localhost:12034"},
			TestFunc: func(t *testing.T) {
				testKillCommand(t, "localhost:2034")
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase // Capture range variable
		test.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			waitChannel := make(chan error, 1)

			currentConfig := testCase.GetConfig(t, testCase.Name)
			StandardizeConfigNamesAndPaths(&currentConfig, testCase.Name, t)
			configFilePath := createTempConfig(t, currentConfig)

			cmd, err := startLargeModelProxy(testCase.Name, configFilePath, waitChannel)
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

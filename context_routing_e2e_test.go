package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestContextBasedRouting verifies the context-based service selection feature
// end to end: several services share one listen port and requests are routed
// to the service with the smallest context size that fits the request, with a
// mid-connection switch to a larger service once the context grows past the
// current service's limit. Both token-based and raw-byte-based context sizes
// are covered.
func TestContextBasedRouting(t *testing.T) {
	t.Parallel()
	testName := t.Name()

	config := Config{
		Services: []ServiceConfig{
			{
				Name:            "tokens-small",
				ListenPort:      "2150",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12150",
				Command:         "./test-server/test-server",
				Args:            "-openai-api-port 12150 -openai-api-keep-alive",
				ContextSize:     uintPtr(5),
				Tokenizer:       "qwen3.8",
			},
			{
				Name:            "tokens-large",
				ListenPort:      "2150",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12151",
				Command:         "./test-server/test-server",
				Args:            "-openai-api-port 12151 -openai-api-keep-alive",
				ContextSize:     uintPtr(5000),
				Tokenizer:       "qwen3.8",
			},
			{
				Name:             "bytes-small",
				ListenPort:       "2151",
				ProxyTargetHost:  "localhost",
				ProxyTargetPort:  "12152",
				Command:          "./test-server/test-server",
				Args:             "-openai-api-port 12152 -openai-api-keep-alive",
				ContextSizeBytes: uintPtr(10),
			},
			{
				Name:             "bytes-large",
				ListenPort:       "2151",
				ProxyTargetHost:  "localhost",
				ProxyTargetPort:  "12153",
				Command:          "./test-server/test-server",
				Args:             "-openai-api-port 12153 -openai-api-keep-alive",
				ContextSizeBytes: uintPtr(5000),
			},
		},
		ManagementApi: ManagementApi{ListenPort: "4150"},
	}
	StandardizeConfigNamesAndPaths(&config, testName)
	configFilePath := createTempConfig(t, config)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy("context-based-routing", configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if stopErr := stopApplication(cmd, waitChannel); stopErr != nil {
			t.Errorf("failed to stop application: %v", stopErr)
		}
		for _, address := range []string{
			"localhost:2150", "localhost:2151", "localhost:4150",
			"localhost:12150", "localhost:12151", "localhost:12152", "localhost:12153",
		} {
			if portErr := checkPortClosed(address); portErr != nil {
				t.Errorf("port %s is still open after application exit: %v", address, portErr)
			}
		}
	}()

	managementApiAddress := "localhost:4150"
	tokenRoutingAddress := "localhost:2150"

	// The first, small request must start and be served by the small service.
	smallContent := "hi"
	connection, err := net.Dial("tcp", tokenRoutingAddress)
	if err != nil {
		t.Fatalf("failed to connect to routed port: %v", err)
	}
	defer func() { _ = connection.Close() }()

	response := sendChatRequestOnConnection(t, connection, smallContent, 30*time.Second)
	assert.Contains(t, response, `"role":"assistant"`, "expected a chat completion response")
	waitForServiceState(t, managementApiAddress, testName+"_tokens-small", ServiceStateRunning, 30*time.Second)
	status := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, status, testName+"_tokens-large", ServiceStateStopped, 0, 0, map[string]int{})

	// A request that no longer fits the small service's context must be served
	// by the large service on the same client connection.
	largeContent := strings.Repeat("word ", 30)
	response = sendChatRequestOnConnection(t, connection, largeContent, 60*time.Second)
	assert.Contains(t, response, `"role":"assistant"`, "expected a chat completion response after switching tiers")
	waitForServiceState(t, managementApiAddress, testName+"_tokens-large", ServiceStateRunning, 30*time.Second)

	// Byte-based routing: 5 bytes fit the 10-byte tier, 50 bytes do not.
	bytesRoutingAddress := "localhost:2151"
	bytesConnection, err := net.Dial("tcp", bytesRoutingAddress)
	if err != nil {
		t.Fatalf("failed to connect to byte-routed port: %v", err)
	}
	defer func() { _ = bytesConnection.Close() }()

	response = sendChatRequestOnConnection(t, bytesConnection, "short", 30*time.Second)
	assert.Contains(t, response, `"role":"assistant"`)
	waitForServiceState(t, managementApiAddress, testName+"_bytes-small", ServiceStateRunning, 30*time.Second)

	response = sendChatRequestOnConnection(t, bytesConnection, strings.Repeat("x", 50), 60*time.Second)
	assert.Contains(t, response, `"role":"assistant"`)
	waitForServiceState(t, managementApiAddress, testName+"_bytes-large", ServiceStateRunning, 30*time.Second)
}

// sendChatRequestOnConnection writes one chat completion request on a raw TCP
// connection (which the proxy keeps open thanks to keep-alive) and returns the
// response body. The timeout must cover on-demand service startup, which can
// take a while for a real model.
func sendChatRequestOnConnection(t *testing.T, connection net.Conn, content string, timeout time.Duration) string {
	t.Helper()
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%q}]}`, content)
	request := fmt.Sprintf(
		"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body)
	if _, err := connection.Write([]byte(request)); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(timeout))
	response, err := readResponse(connection)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	return response
}

func readResponse(connection net.Conn) (string, error) {
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read status line: %w", err)
	}
	if !strings.Contains(statusLine, "200") {
		return "", fmt.Errorf("unexpected status line: %s", strings.TrimSpace(statusLine))
	}
	contentLength := -1
	chunked := false
	for {
		headerLine, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("failed to read header line: %w", err)
		}
		if headerLine == "\r\n" || headerLine == "\n" {
			break
		}
		lower := strings.ToLower(headerLine)
		if strings.HasPrefix(lower, "content-length:") {
			_, scanErr := fmt.Sscanf(strings.TrimSpace(headerLine[len("content-length:"):]), "%d", &contentLength)
			if scanErr != nil {
				return "", fmt.Errorf("failed to parse content-length %q: %w", headerLine, scanErr)
			}
		}
		if strings.HasPrefix(lower, "transfer-encoding:") && strings.Contains(lower, "chunked") {
			chunked = true
		}
	}
	if chunked {
		var body strings.Builder
		for {
			sizeLine, err := reader.ReadString('\n')
			if err != nil {
				return "", fmt.Errorf("failed to read chunk size: %w", err)
			}
			var chunkSize int
			_, scanErr := fmt.Sscanf(strings.TrimSpace(sizeLine), "%x", &chunkSize)
			if scanErr != nil {
				return "", fmt.Errorf("failed to parse chunk size %q: %w", sizeLine, scanErr)
			}
			if chunkSize == 0 {
				_, _ = reader.ReadString('\n') // trailing CRLF after last chunk
				return body.String(), nil
			}
			chunk := make([]byte, chunkSize)
			if _, err := readFull(reader, chunk); err != nil {
				return "", fmt.Errorf("failed to read chunk: %w", err)
			}
			body.Write(chunk)
			_, _ = reader.ReadString('\n') // CRLF after chunk data
		}
	}
	if contentLength >= 0 {
		body := make([]byte, contentLength)
		if _, err := readFull(reader, body); err != nil {
			return "", fmt.Errorf("failed to read body: %w", err)
		}
		return string(body), nil
	}
	return "", fmt.Errorf("response has neither content-length nor chunked encoding")
}

func readFull(reader *bufio.Reader, buffer []byte) (int, error) {
	total := 0
	for total < len(buffer) {
		n, err := reader.Read(buffer[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

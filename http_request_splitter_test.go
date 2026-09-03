package main

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testChatRequestBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

func testChatRequest(contentLengthExtra string, body string) []byte {
	return []byte("POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		contentLengthExtra +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" +
		body)
}

func writeInChunks(t *testing.T, splitter *httpRequestSplitter, data []byte, chunkSize int) []httpRequestSplitResult {
	t.Helper()
	results := make([]httpRequestSplitResult, 0)
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		results = append(results, splitter.Write(data[offset:end]))
	}
	return results
}

func completedRequestBytes(results []httpRequestSplitResult) [][]byte {
	var completed [][]byte
	for _, result := range results {
		completed = append(completed, result.completedRequests...)
	}
	return completed
}

func TestSplitterCompleteRequestInOneWrite(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := testChatRequest("", testChatRequestBody)
	result := splitter.Write(request)

	assert.False(t, result.enteredPassthrough)
	assert.Len(t, result.completedRequests, 1)
	assert.True(t, bytes.Equal(request, result.completedRequests[0]))
}

func TestSplitterRequestSplitIntoTinyChunks(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := testChatRequest("", testChatRequestBody)

	results := writeInChunks(t, splitter, request, 1)

	var completedBeforeLast int
	for _, result := range results[:len(results)-1] {
		assert.False(t, result.enteredPassthrough)
		completedBeforeLast += len(result.completedRequests)
	}
	assert.Equal(t, 0, completedBeforeLast, "request must not complete before the last byte of the body arrives")
	assert.Len(t, results[len(results)-1].completedRequests, 1)
	assert.True(t, bytes.Equal(request, completedRequestBytes(results)[0]))
}

func TestSplitterRequestSplitAtBodyBoundary(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := testChatRequest("", testChatRequestBody)
	headerEnd := bytes.Index(request, []byte("\r\n\r\n")) + 4

	result1 := splitter.Write(request[:headerEnd])
	assert.Len(t, result1.completedRequests, 0)

	result2 := splitter.Write(request[headerEnd:])
	assert.Len(t, result2.completedRequests, 1)
	assert.True(t, bytes.Equal(request, result2.completedRequests[0]))
}

func TestSplitterPipelinedRequests(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request1 := testChatRequest("", "first")
	request2 := testChatRequest("", "second")

	result := splitter.Write(append(append([]byte{}, request1...), request2...))

	assert.False(t, result.enteredPassthrough)
	completed := result.completedRequests
	assert.Len(t, completed, 2)
	assert.True(t, bytes.Equal(request1, completed[0]))
	assert.True(t, bytes.Equal(request2, completed[1]))
}

func TestSplitterGetRequestCompletesAtHeaderEnd(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := []byte("GET /v1/models HTTP/1.1\r\nHost: localhost\r\n\r\n")

	result := splitter.Write(request)

	assert.False(t, result.enteredPassthrough)
	assert.Len(t, result.completedRequests, 1)
	assert.True(t, bytes.Equal(request, result.completedRequests[0]))
}

func TestSplitterPostWithoutContentLengthEntersPassthrough(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := []byte("POST /v1/completions HTTP/1.1\r\nHost: localhost\r\n\r\nbody-until-close")

	result := splitter.Write(request)

	assert.True(t, result.enteredPassthrough)
	assert.Empty(t, result.completedRequests)
	assert.Equal(t, string(request), string(result.throughBytes),
		"everything must be handed back for blind forwarding when body length is undeterminable")
}

func TestSplitterChunkedRequest(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	chunkedRequest := []byte("POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n" +
		"5\r\nhello\r\n" +
		"6\r\n world\r\n" +
		"0\r\n\r\n")

	result := splitter.Write(chunkedRequest)

	assert.False(t, result.enteredPassthrough)
	assert.Len(t, result.completedRequests, 1)
	assert.True(t, bytes.Equal(chunkedRequest, result.completedRequests[0]))
}

func TestSplitterChunkedRequestSplitAcrossWrites(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	chunkedRequest := []byte("POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Transfer-Encoding: chunked\r\n\r\n" +
		"5\r\nhello\r\n" +
		"6\r\n world\r\n" +
		"0\r\n\r\n")

	results := writeInChunks(t, splitter, chunkedRequest, 7)

	var completedCount int
	for _, result := range results {
		completedCount += len(result.completedRequests)
		assert.False(t, result.enteredPassthrough)
	}
	assert.Equal(t, 1, completedCount)
}

func TestSplitterChunkedRequestWithTrailers(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	chunkedRequest := []byte("POST /x HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"3\r\nabc\r\n" +
		"0\r\n" +
		"X-Trailer: v\r\n\r\n")

	result := splitter.Write(chunkedRequest)

	assert.False(t, result.enteredPassthrough)
	assert.Len(t, result.completedRequests, 1)
}

func TestSplitterGarbageEntersPassthrough(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()

	result := splitter.Write([]byte("not an http request at all"))

	assert.True(t, result.enteredPassthrough)
	assert.Empty(t, result.completedRequests)
	assert.Equal(t, "not an http request at all", string(result.throughBytes))
}

func TestSplitterGarbageAfterValidRequest(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := testChatRequest("", "ok")
	garbage := []byte("garbage that is not a request line\r\n\r\n")

	result := splitter.Write(append(append([]byte{}, request...), garbage...))

	assert.Len(t, result.completedRequests, 1, "the framed request must still be emitted")
	assert.True(t, result.enteredPassthrough)
	assert.Equal(t, string(garbage), string(result.throughBytes))
}

func TestSplitterUpgradeRequestCompletesHeadersAndPassesThroughRest(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	headers := []byte("GET /ws HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")

	result := splitter.Write(append(append([]byte{}, headers...), []byte("websocket payload bytes")...))

	assert.Len(t, result.completedRequests, 1, "header part must be emitted as a completed request")
	assert.True(t, bytes.Equal(headers, result.completedRequests[0]))
	assert.True(t, result.enteredPassthrough)
	assert.Equal(t, "websocket payload bytes", string(result.throughBytes))
}

func TestSplitterExpect100ContinueEntersPassthrough(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := []byte("POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\nhello")

	result := splitter.Write(request)

	// The client waits for a 100 Continue that only the backend can send, so the
	// body cannot be counted here: fall back to blind forwarding.
	assert.True(t, result.enteredPassthrough)
	assert.Empty(t, result.completedRequests)
}

func TestSplitterConnectMethodEntersPassthrough(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()

	result := splitter.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"))

	assert.True(t, result.enteredPassthrough)
}

func TestSplitterPartialHeadersProduceNothing(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()

	result := splitter.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: localh"))

	assert.False(t, result.enteredPassthrough)
	assert.Empty(t, result.completedRequests)
	assert.Equal(t, "POST /v1/chat/completions HTTP/1.1\r\nHost: localh", string(splitter.FlushIncomplete()))
}

func TestSplitterPassthroughForwardsSubsequentWrites(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()

	first := splitter.Write([]byte("garbage"))
	assert.True(t, first.enteredPassthrough)

	second := splitter.Write([]byte(" more bytes"))
	assert.True(t, second.enteredPassthrough)
	assert.Empty(t, second.completedRequests)
	assert.Equal(t, " more bytes", string(second.throughBytes))
}

func TestSplitterContentLengthZero(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := []byte("POST /v1/models HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\n\r\n")

	result := splitter.Write(request)

	assert.False(t, result.enteredPassthrough)
	assert.Len(t, result.completedRequests, 1)
}

func TestSplitterHeaderCaseInsensitivity(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := []byte("POST /x HTTP/1.1\r\nHost: localhost\r\ncOnTeNt-LeNgTh: 5\r\n\r\nhello")

	result := splitter.Write(request)

	assert.Len(t, result.completedRequests, 1)
}

func TestSplitterFlushIncompleteAfterCompleteRequest(t *testing.T) {
	t.Parallel()
	splitter := newHttpRequestSplitter()
	request := testChatRequest("", "body")
	partial := []byte("GET /v1/mod")

	result := splitter.Write(append(append([]byte{}, request...), partial...))
	assert.Len(t, result.completedRequests, 1)
	assert.Equal(t, "GET /v1/mod", string(splitter.FlushIncomplete()))
}

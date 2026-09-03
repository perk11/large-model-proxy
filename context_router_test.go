package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func testContextRouterServices() []ServiceConfig {
	return []ServiceConfig{
		{Name: "large", ListenPort: "9000", ContextSize: uintPtr(1000), Tokenizer: "qwen3.8"},
		{Name: "small", ListenPort: "9000", ContextSize: uintPtr(10), Tokenizer: "qwen3.8"},
		{Name: "medium", ListenPort: "9000", ContextSize: uintPtr(100), Tokenizer: "qwen3.8"},
	}
}

func uintPtr(n uint) *uint {
	return &n
}

func TestBuildContextRouterSortsTiersAscending(t *testing.T) {
	t.Parallel()
	router := buildContextRouter(testContextRouterServices())

	assert.Equal(t, contextUnitsTokens, router.unitMode)
	assert.Len(t, router.tiers, 3)
	assert.Equal(t, uint64(10), router.tiers[0].limitUnits)
	assert.Equal(t, "small", router.tiers[0].serviceConfig.Name)
	assert.Equal(t, uint64(100), router.tiers[1].limitUnits)
	assert.Equal(t, "medium", router.tiers[1].serviceConfig.Name)
	assert.Equal(t, uint64(1000), router.tiers[2].limitUnits)
	assert.Equal(t, "large", router.tiers[2].serviceConfig.Name)
	assert.NotNil(t, router.tokenCounter, "token counter must be resolved from the registry")
}

func TestSelectTierIndexPicksSmallestFittingTier(t *testing.T) {
	t.Parallel()
	router := buildContextRouter(testContextRouterServices())

	assert.Equal(t, 0, router.selectTierIndex(0), "empty request goes to the smallest tier")
	assert.Equal(t, 0, router.selectTierIndex(9), "just below the first limit")
	assert.Equal(t, 0, router.selectTierIndex(10), "exactly the first limit fits")
	assert.Equal(t, 1, router.selectTierIndex(11), "one over the first limit")
	assert.Equal(t, 1, router.selectTierIndex(100))
	assert.Equal(t, 2, router.selectTierIndex(101))
	assert.Equal(t, 2, router.selectTierIndex(100000), "over all limits falls back to the largest tier")
}

func TestBuildContextRouterBytesMode(t *testing.T) {
	t.Parallel()
	services := []ServiceConfig{
		{Name: "large-bytes", ListenPort: "9001", ContextSizeBytes: uintPtr(1000)},
		{Name: "small-bytes", ListenPort: "9001", ContextSizeBytes: uintPtr(10)},
	}
	router := buildContextRouter(services)

	assert.Equal(t, contextUnitsBytes, router.unitMode)
	assert.Nil(t, router.tokenCounter)
	assert.Equal(t, uint64(10), router.tiers[0].limitUnits)
	assert.Equal(t, 0, router.selectTierIndex(5))
	assert.Equal(t, 1, router.selectTierIndex(9999))
}

// --- request units counting ---

func testUnitsRouter(t *testing.T) *contextRouter {
	t.Helper()
	router := buildContextRouter([]ServiceConfig{
		{Name: "small", ListenPort: "9000", ContextSize: uintPtr(10), Tokenizer: "qwen3.8"},
		{Name: "large", ListenPort: "9000", ContextSize: uintPtr(1000), Tokenizer: "qwen3.8"},
	})
	assert.NotNil(t, router.tokenCounter)
	return router
}

// qwen3.8 counting: "hello world" = 3 tokens, "hi" = 1 token (see tokenizer_test.go)
func TestCountRequestUnitsChatMessages(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := testChatRequest("", `{"model":"m","messages":[
		{"role":"system","content":"hello world"},
		{"role":"user","content":"hi"}]}`)

	units := countRequestUnits(request, router)

	assert.Equal(t, uint64(4), units)
}

func TestCountRequestUnitsMultipartContent(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := testChatRequest("", `{"model":"m","messages":[
		{"role":"user","content":[
			{"type":"text","text":"hello world"},
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"http://example.com/image.png"}}]}]}`)

	units := countRequestUnits(request, router)

	// only the text parts are counted
	assert.Equal(t, uint64(4), units)
}

func TestCountRequestUnitsCompletionPrompt(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := testChatRequest("", `{"model":"m","prompt":"hello world"}`)

	assert.Equal(t, uint64(3), countRequestUnits(request, router))
}

func TestCountRequestUnitsPromptArray(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := testChatRequest("", `{"model":"m","prompt":["hello world","hi"]}`)

	assert.Equal(t, uint64(4), countRequestUnits(request, router))
}

func TestCountRequestUnitsEmbeddingsInput(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := testChatRequest("", `{"model":"m","input":["hello world","hi"]}`)

	assert.Equal(t, uint64(4), countRequestUnits(request, router))
}

func TestCountRequestUnitsInvalidJsonFallsBackToRawBody(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := testChatRequest("", `this is not json`)

	// "this"=1 + " is"=1 + " not"=1 + " json"=1 = 4 with the qwen3.8 heuristic
	assert.Equal(t, uint64(4), countRequestUnits(request, router))
}

func TestCountRequestUnitsNonJsonContentTypeFallsBackToRawBody(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := []byte("POST /v1/audio/transcriptions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Content-Type: multipart/form-data\r\n" +
		"Content-Length: 10\r\n\r\n" +
		"0123456789")

	// digits: ceil(10/3) = 4
	assert.Equal(t, uint64(4), countRequestUnits(request, router))
}

func TestCountRequestUnitsGetRequest(t *testing.T) {
	t.Parallel()
	router := testUnitsRouter(t)
	request := []byte("GET /v1/models HTTP/1.1\r\nHost: localhost\r\n\r\n")

	assert.Equal(t, uint64(0), countRequestUnits(request, router))
}

func TestCountRequestUnitsBytesModeMeasuresTextBytes(t *testing.T) {
	t.Parallel()
	router := buildContextRouter([]ServiceConfig{
		{Name: "small", ListenPort: "9000", ContextSizeBytes: uintPtr(10)},
		{Name: "large", ListenPort: "9000", ContextSizeBytes: uintPtr(1000)},
	})
	request := testChatRequest("", `{"model":"m","messages":[{"role":"user","content":"hello world"}]}`)
	requestWithExtraJsonKeys := testChatRequest("", `{"model":"m","stream":true,"temperature":0.7,"messages":[{"role":"user","content":"hello world"}]}`)

	// only the text content is measured, not the JSON envelope
	assert.Equal(t, uint64(11), countRequestUnits(request, router))
	assert.Equal(t, uint64(11), countRequestUnits(requestWithExtraJsonKeys, router))
}

func TestCountRequestUnitsBytesModeMultibyteContent(t *testing.T) {
	t.Parallel()
	router := buildContextRouter([]ServiceConfig{
		{Name: "small", ListenPort: "9000", ContextSizeBytes: uintPtr(10)},
		{Name: "large", ListenPort: "9000", ContextSizeBytes: uintPtr(1000)},
	})
	request := testChatRequest("", `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)

	assert.Equal(t, uint64(6), countRequestUnits(request, router), "raw byte length of the UTF-8 content")
}

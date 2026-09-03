package main

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Exact token counts below are hand-computed from the documented heuristic
// tokenizer parameters (see tokenizer.go). They are intentionally exact so any
// accidental change to the counting algorithm that would silently alter routing
// decisions breaks a test.
func TestQwen38TokenCounting(t *testing.T) {
	t.Parallel()
	counter, found := GetTokenCounter("qwen3.8")
	assert.True(t, found, "qwen3.8 tokenizer should be registered")

	assert.Equal(t, 0, counter(""), "empty text")
	// "hello" -> ceil(5/5.0)=1, " world" (space merged) -> ceil(6/5.0)=2
	assert.Equal(t, 3, counter("hello world"))
	// 4 CJK runes = 12 bytes -> ceil(12/3.0)=4
	assert.Equal(t, 4, counter("你好世界"))
	// "abc"=1, double space (not merged)=1, "123"=ceil(3/3)=1
	assert.Equal(t, 3, counter("abc  123"))
	// "Count"=1, ":"=1, " 42" (merged space + digits)=ceil(3/3)=1, "!"=1
	assert.Equal(t, 4, counter("Count: 42!"))
	// single long word: ceil(20/5.0)
	assert.Equal(t, 4, counter("internationalization"))
	// leading space merges into the word: ceil(6/5.0)
	assert.Equal(t, 2, counter(" hello"))
	// newline is whitespace that cannot merge: "a"=1 + "\n"=1 + "b"=1
	assert.Equal(t, 3, counter("a\nb"))
}

func TestGemma4TokenCounting(t *testing.T) {
	t.Parallel()
	counter, found := GetTokenCounter("gemma4")
	assert.True(t, found, "gemma4 tokenizer should be registered")

	assert.Equal(t, 0, counter(""))
	// "hello"=ceil(5/5.5)=1, " world"=ceil(6/5.5)=2
	assert.Equal(t, 3, counter("hello world"))
	// Gemma falls back to byte-level tokens for CJK: ceil(12/1.5)=8
	assert.Equal(t, 8, counter("你好世界"))
	// letters: ceil(20/5.5)=4
	assert.Equal(t, 4, counter("internationalization"))
}

func TestTokenCounterAliases(t *testing.T) {
	t.Parallel()
	qwen38, found38 := GetTokenCounter("qwen3.8")
	qwen3, found3 := GetTokenCounter("qwen3")
	assert.True(t, found38)
	assert.True(t, found3)
	// Aliases must resolve to the same counting behavior
	assert.Equal(t, qwen38("hello world"), qwen3("hello world"))

	gemma4, foundGemma4 := GetTokenCounter("gemma4")
	gemma3, foundGemma3 := GetTokenCounter("gemma3")
	assert.True(t, foundGemma4)
	assert.True(t, foundGemma3)
	assert.Equal(t, gemma4("hello world"), gemma3("hello world"))
}

func TestGetTokenCounterUnknownName(t *testing.T) {
	t.Parallel()
	_, found := GetTokenCounter("does-not-exist")
	assert.False(t, found)
}

func TestRegisteredTokenCounterNames(t *testing.T) {
	t.Parallel()
	names := RegisteredTokenCounterNames()
	assert.Contains(t, names, "qwen3.8")
	assert.Contains(t, names, "gemma4")
}

// Adding support for a new model must be a single registration call.
func TestRegisterCustomTokenCounter(t *testing.T) {
	t.Parallel()
	RegisterTokenCounter("test-model", func(text string) int {
		return len(text)
	})
	counter, found := GetTokenCounter("test-model")
	assert.True(t, found, "newly registered counter should be retrievable")
	assert.Equal(t, 5, counter("hello"))
}

func TestTokenCounterMonotonicallyIncreases(t *testing.T) {
	t.Parallel()
	counter, _ := GetTokenCounter("qwen3.8")
	previousCount := 0
	text := ""
	for i := 0; i < 30; i++ {
		text += "word "
		count := counter(text)
		assert.GreaterOrEqual(t, count, previousCount, "adding text must never decrease token count (text=%q)", text)
		previousCount = count
	}
}

func TestHeuristicTokenCounterNeverReturnsNegativeOrNaN(t *testing.T) {
	t.Parallel()
	counter := newHeuristicTokenCounter(heuristicTokenizerConfig{
		lettersPerToken:       5.0,
		digitsPerToken:        3.0,
		whitespacePerToken:    4.0,
		punctuationPerToken:   3.0,
		nonAsciiBytesPerToken: 3.0,
	})
	for _, text := range []string{"", " ", "\x00\x01", "😀😀", "a b\tc\rd"} {
		count := counter(text)
		assert.False(t, math.IsNaN(float64(count)))
		assert.GreaterOrEqual(t, count, 0)
	}
}

// The same CJK text must cost more tokens on the byte-fallback tokenizer.
func TestGemmaCountsMoreTokensForCJKThanQwen(t *testing.T) {
	t.Parallel()
	qwen, _ := GetTokenCounter("qwen3.8")
	gemma, _ := GetTokenCounter("gemma4")
	cjk := "这是一段中文文本"
	assert.Greater(t, gemma(cjk), qwen(cjk))
}

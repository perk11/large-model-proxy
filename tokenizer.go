package main

import (
	"math"
	"sort"
	"sync"
	"unicode"
	"unicode/utf8"
)

// TokenCounter estimates how many tokens a given text occupies for a specific
// model family's tokenizer.
//
// Counters in this package are heuristics: they do not load real BPE/SentencePiece
// vocabularies, but approximate a model family's tokenization using per-model
// character-class ratios. For context-tier routing (choosing between e.g. a 4k
// and a 32k context instance of the same model) this approximation is sufficient
// as long as operators leave headroom when configuring ContextSize values. The
// run-based algorithm below tends to slightly overestimate, which is the safe
// direction: it switches to a larger context service a bit early rather than
// sending an oversized context to a service that cannot fit it.
type TokenCounter func(text string) int

var (
	tokenCounterMutex sync.RWMutex
	tokenCounters     = map[string]TokenCounter{}
)

// RegisterTokenCounter makes a token counter available by name for use in the
// Tokenizer configuration field. Adding support for a new model family is a
// single call to this function (usually from an init()).
func RegisterTokenCounter(name string, counter TokenCounter) {
	tokenCounterMutex.Lock()
	defer tokenCounterMutex.Unlock()
	tokenCounters[name] = counter
}

// GetTokenCounter looks up a registered token counter by name.
func GetTokenCounter(name string) (TokenCounter, bool) {
	tokenCounterMutex.RLock()
	defer tokenCounterMutex.RUnlock()
	counter, found := tokenCounters[name]
	return counter, found
}

// RegisteredTokenCounterNames returns the sorted list of all registered
// token counter names.
func RegisteredTokenCounterNames() []string {
	tokenCounterMutex.RLock()
	defer tokenCounterMutex.RUnlock()
	names := make([]string, 0, len(tokenCounters))
	for name := range tokenCounters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// heuristicTokenizerConfig holds the per-model-family approximation parameters.
// They describe how many units of each character class (runes, or bytes for
// non-ASCII) a tokenizer of that family produces per token on average.
type heuristicTokenizerConfig struct {
	lettersPerToken       float64
	digitsPerToken        float64
	whitespacePerToken    float64
	punctuationPerToken   float64
	nonAsciiBytesPerToken float64
}

func init() {
	// Qwen3-family BPE tokenizer (e.g. Qwen3-8B): efficient on both English and CJK
	qwen := newHeuristicTokenCounter(heuristicTokenizerConfig{
		lettersPerToken:       5.0,
		digitsPerToken:        3.0,
		whitespacePerToken:    4.0,
		punctuationPerToken:   3.0,
		nonAsciiBytesPerToken: 3.0,
	})
	RegisterTokenCounter("qwen3.8", qwen)
	RegisterTokenCounter("qwen3", qwen)

	// Gemma-family SentencePiece tokenizer: byte-fallback for CJK makes
	// non-ASCII text far more expensive than on Qwen
	gemma := newHeuristicTokenCounter(heuristicTokenizerConfig{
		lettersPerToken:       5.5,
		digitsPerToken:        3.0,
		whitespacePerToken:    4.0,
		punctuationPerToken:   3.0,
		nonAsciiBytesPerToken: 1.5,
	})
	RegisterTokenCounter("gemma4", gemma)
	RegisterTokenCounter("gemma3", gemma)
}

// newHeuristicTokenCounter builds a TokenCounter from per-family parameters.
//
// The text is split into maximal runs of one character class (letters, digits,
// whitespace, ASCII punctuation, everything else measured in UTF-8 bytes).
// Tokenizers never merge across script boundaries, so each run costs at least
// one token, and long runs are split proportionally to the family's ratio. A
// single inter-word space is merged into the following word run, mirroring how
// BPE/SentencePiece encode " word" as a single token.
func newHeuristicTokenCounter(config heuristicTokenizerConfig) TokenCounter {
	return func(text string) int {
		var tokens float64
		var pendingMergedSpace int

		for _, characterClass := range classifyRunes(text) {
			mergedSpace := 0
			if characterClass.class == runeClassLetter || characterClass.class == runeClassDigit {
				// A single space immediately before a word is counted together
				// with the word (the " word" merge), as tokenizers do.
				mergedSpace = pendingMergedSpace
			}
			pendingMergedSpace = 0

			switch characterClass.class {
			case runeClassLetter:
				tokens += math.Ceil(float64(characterClass.runes+mergedSpace) / config.lettersPerToken)
			case runeClassDigit:
				tokens += math.Ceil(float64(characterClass.runes+mergedSpace) / config.digitsPerToken)
			case runeClassWhitespace:
				if characterClass.singleSpace {
					// not consumed by a merge (e.g. trailing space): count it alone
					pendingMergedSpace = characterClass.runes
					continue
				}
				tokens += math.Ceil(float64(characterClass.runes) / config.whitespacePerToken)
			case runeClassPunctuation:
				tokens += math.Ceil(float64(characterClass.runes) / config.punctuationPerToken)
			case runeClassOther:
				tokens += math.Ceil(float64(characterClass.bytes) / config.nonAsciiBytesPerToken)
			}
		}
		// A trailing single space that never merged still occupies roughly a
		// fraction of a token; charge it so that trailing whitespace is not free.
		if pendingMergedSpace > 0 {
			tokens += math.Ceil(float64(pendingMergedSpace) / config.whitespacePerToken)
		}
		return int(tokens)
	}
}

type runeClass int

const (
	runeClassLetter runeClass = iota
	runeClassDigit
	runeClassWhitespace
	runeClassPunctuation
	runeClassOther
)

type runeRun struct {
	class runeClass
	runes int
	bytes int // only used for runeClassOther
	// singleSpace is true when the run is exactly one plain space character
	singleSpace bool
}

func classifyRunes(text string) []runeRun {
	var runs []runeRun
	appendRun := func(class runeClass, runes int, bytes int, singleSpace bool) {
		runs = append(runs, runeRun{class: class, runes: runes, bytes: bytes, singleSpace: singleSpace})
	}

	var currentClass runeClass
	var currentRunes int
	var currentBytes int
	var lastRune rune
	flush := func() {
		if currentRunes == 0 {
			return
		}
		// Only a plain U+0020 space participates in the " word" merge; other
		// whitespace (tabs, newlines) always forms its own run.
		singleSpace := currentClass == runeClassWhitespace && currentRunes == 1 && currentBytes == 1 && lastRune == ' '
		appendRun(currentClass, currentRunes, currentBytes, singleSpace)
		currentRunes = 0
		currentBytes = 0
	}

	for _, character := range text {
		var class runeClass
		switch {
		case unicode.IsDigit(character):
			class = runeClassDigit
		case unicode.IsSpace(character):
			class = runeClassWhitespace
		case unicode.IsLetter(character) && character < utf8.RuneSelf:
			class = runeClassLetter
		case (unicode.IsPunct(character) || unicode.IsSymbol(character)) && character < utf8.RuneSelf:
			class = runeClassPunctuation
		default:
			class = runeClassOther
		}

		if class != currentClass {
			flush()
			currentClass = class
		}
		currentRunes++
		currentBytes += utf8.RuneLen(character)
		lastRune = character
	}
	flush()
	return runs
}

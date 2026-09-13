// Package providerkit contains helpers for provider plugin authors: SSE decoding,
// Responses<->Chat Completions translation and token estimation.
package providerkit

import (
	"strings"
	"sync"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// DefaultCharsPerToken is the heuristic used when the upstream reports no usage.
// It is intentionally conservative (4 characters per token) so that in-flight
// estimates do not understate consumption.
const DefaultCharsPerToken = 4

// CharEstimator accumulates text and estimates tokens as chars/charsPerToken.
// It is safe for concurrent use.
type CharEstimator struct {
	charsPerToken int
	mu            sync.Mutex
	chars         int64
}

// NewCharEstimator creates an estimator (charsPerToken <= 0 uses the default).
func NewCharEstimator(charsPerToken int) *CharEstimator {
	if charsPerToken <= 0 {
		charsPerToken = DefaultCharsPerToken
	}
	return &CharEstimator{charsPerToken: charsPerToken}
}

// Add records more text and returns the estimated total token count so far.
func (e *CharEstimator) Add(text string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chars += int64(len(text))
	return e.tokensLocked()
}

// Tokens returns the current estimate.
func (e *CharEstimator) Tokens() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tokensLocked()
}

// Chars returns the accumulated character count.
func (e *CharEstimator) Chars() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.chars
}

// Reset clears the accumulator (used between attempts).
func (e *CharEstimator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chars = 0
}

func (e *CharEstimator) tokensLocked() int64 {
	if e.chars == 0 {
		return 0
	}
	// Round up: a partial token still costs a token.
	return (e.chars + int64(e.charsPerToken) - 1) / int64(e.charsPerToken)
}

// EstimateTokens estimates the token count of a string.
func EstimateTokens(text string, charsPerToken int) int64 {
	if charsPerToken <= 0 {
		charsPerToken = DefaultCharsPerToken
	}
	if text == "" {
		return 0
	}
	return (int64(len(text)) + int64(charsPerToken) - 1) / int64(charsPerToken)
}

// EstimateInputTokens estimates the input tokens of a canonical request.
func EstimateInputTokens(req *pluginapi.Request, charsPerToken int) int64 {
	if req == nil {
		return 0
	}
	if charsPerToken <= 0 {
		charsPerToken = DefaultCharsPerToken
	}
	var chars int64
	chars += int64(len(req.Instructions))
	for _, item := range req.Input {
		chars += int64(len(item.Content))
		chars += int64(len(item.Arguments))
		if len(item.OutputContent) > 0 {
			chars += int64(len(item.OutputContent))
		} else {
			chars += int64(len(item.Output))
		}
		for _, part := range item.Summary {
			chars += int64(len(part.Text))
		}
	}
	for _, tool := range req.Tools {
		chars += int64(len(tool.Name)) + int64(len(tool.Description)) + int64(len(tool.Parameters))
	}
	if chars == 0 {
		return 0
	}
	return (chars + int64(charsPerToken) - 1) / int64(charsPerToken)
}

// MergeDimensions adds src into dst (dst may be nil).
func MergeDimensions(dst, src map[string]int64) map[string]int64 {
	if dst == nil {
		dst = map[string]int64{}
	}
	for k, v := range src {
		dst[k] += v
	}
	return dst
}

// TotalTokens sums every token-like dimension.
func TotalTokens(dims map[string]int64) int64 {
	var total int64
	for _, v := range dims {
		total += v
	}
	return total
}

// NormalizeText collapses whitespace; useful for tests and previews.
func NormalizeText(s string) string { return strings.Join(strings.Fields(s), " ") }

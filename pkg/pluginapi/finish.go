package pluginapi

import "strings"

// Finish-reason vocabulary shared by every dialect this gateway speaks. A chat
// upstream calls a token-limit stop "length", a Responses upstream reports status
// "incomplete" with its own details, and DeepSeek adds "insufficient_system_resource"
// when it cannot answer at all. They all have to land on one client-visible
// meaning, so the mapping lives next to the protocol instead of in each provider.
const (
	// ReasonStop is a normal, complete answer.
	ReasonStop = "stop"
)

// IncompleteReason classifies an upstream finish reason. It returns the Responses
// API's `incomplete_details.reason` and true when the answer is a fragment the
// client must be told about (the OpenAI enum is max_output_tokens | content_filter;
// an upstream that admits truncation without saying why reports max_output_tokens).
// A complete answer returns ("", false) — including reasons this gateway does not
// know, because inventing truncation would make healthy answers look broken.
func IncompleteReason(finishReason string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "length", "max_tokens", "max_output_tokens":
		return "max_output_tokens", true
	case "content_filter":
		return "content_filter", true
	case "incomplete":
		// The upstream said "this is not the whole answer" but not why. The
		// Responses API has no "unknown" member, and max_output_tokens is the only
		// reason a conforming client can act on (continue the answer).
		return "max_output_tokens", true
	default:
		return "", false
	}
}

package responses

import (
	"strings"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// UserInput is the recorded payload of the default input policy
// (recording.record_input=user): what the user wrote, plus a summary of everything the
// client sent that the gateway deliberately does not store.
//
// The default used to be the whole request body. In an agent loop that body is not the
// user's input: one real request carried 67 input items, of which 2 were user messages —
// the rest were tool definitions and function_call_output items holding whole file
// contents. Storing that by default turned the request log into a second copy of every
// file an agent read, and made "input" mean something the operator never chose.
type UserInput struct {
	Model   string           `json:"model,omitempty"`
	Input   []pluginapi.Item `json:"input"`
	Omitted map[string]int   `json:"omitted,omitempty"`
	Bytes   int              `json:"request_bytes,omitempty"`
}

// UserInputDocument builds that document. bodyBytes is the serialized size of the whole
// request, which the caller already has; it is recorded so a reader can still tell a
// 200-byte prompt from a 200 KB one without the body being kept.
func (r *Request) UserInputDocument(bodyBytes int) (*UserInput, error) {
	items, apiErr := r.Items()
	if apiErr != nil {
		return nil, apiErr
	}
	doc := &UserInput{
		Model:   r.Model,
		Input:   []pluginapi.Item{},
		Omitted: map[string]int{},
		Bytes:   bodyBytes,
	}
	for _, item := range items {
		if item.Type == "message" && item.Role == "user" {
			doc.Input = append(doc.Input, item)
			continue
		}
		doc.Omitted[omittedKey(item)]++
	}
	if len(r.Tools) > 0 {
		doc.Omitted["tools"] = len(r.Tools)
	}
	if strings.TrimSpace(r.Instructions) != "" {
		doc.Omitted["instructions"] = 1
	}
	if len(doc.Omitted) == 0 {
		doc.Omitted = nil
	}
	return doc, nil
}

// omittedKey names one dropped item in the document's tally: by type, and for messages
// by role too, because "one assistant turn" and "one developer instruction" are
// different diagnoses.
func omittedKey(item pluginapi.Item) string {
	itemType := strings.TrimSpace(item.Type)
	if itemType == "" {
		itemType = "unknown"
	}
	if itemType == "message" && item.Role != "" {
		return itemType + ":" + item.Role
	}
	return itemType
}

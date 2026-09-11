package responses

import (
	"encoding/json"
	"fmt"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// FeedItems replays a non-streaming provider response through the assembler so that
// both code paths produce identical output items and streaming events.
func FeedItems(a *Assembler, items []pluginapi.Item) error {
	for _, item := range items {
		switch item.Type {
		case "message", "":
			text, refusal, err := contentText(item.Content)
			if err != nil {
				return err
			}
			if refusal != "" {
				if err := a.Add(pluginapi.Event{Type: pluginapi.EventRefusalDelta, Text: refusal}); err != nil {
					return err
				}
			}
			if text != "" {
				if err := a.Add(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: text}); err != nil {
					return err
				}
			}
		case "reasoning":
			for _, part := range reasoningParts(item) {
				if err := a.Add(pluginapi.Event{Type: pluginapi.EventReasoningDelta, Text: part}); err != nil {
					return err
				}
			}
		case "function_call":
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			if err := a.Add(pluginapi.Event{
				Type: pluginapi.EventToolCallStart, CallID: id, ItemID: item.ID, Name: item.Name,
			}); err != nil {
				return err
			}
			if item.Arguments != "" {
				if err := a.Add(pluginapi.Event{
					Type: pluginapi.EventToolArgsDelta, CallID: id, ItemID: item.ID, Text: item.Arguments,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// reasoningParts extracts the chain-of-thought text of a reasoning item.
//
// Summary wins when it carries text because that is where a response built by this
// gateway keeps its reasoning; upstreams that send reasoning in the Responses shape
// (reasoning_text content parts, as DeepSeek does) only fill content, so that is the
// fallback.
func reasoningParts(item pluginapi.Item) []string {
	out := make([]string, 0, len(item.Summary))
	for _, part := range item.Summary {
		if part.Text != "" {
			out = append(out, part.Text)
		}
	}
	if len(out) > 0 {
		return out
	}
	return contentTypeText(item.Content, "reasoning_text")
}

// contentTypeText reads the text of specific content parts from a raw content value.
func contentTypeText(raw json.RawMessage, partType string) []string {
	if len(raw) == 0 {
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Text == "" {
			continue
		}
		if partType != "" && part.Type != partType {
			continue
		}
		out = append(out, part.Text)
	}
	return out
}

// contentText flattens Responses content parts into plain text (and any refusal).
func contentText(raw json.RawMessage) (text string, refusal string, err error) {
	if len(raw) == 0 {
		return "", "", nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, "", nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", "", fmt.Errorf("responses: unsupported content shape: %w", err)
	}
	for _, part := range parts {
		switch part.Type {
		case "refusal":
			refusal += part.Text
		default:
			text += part.Text
		}
	}
	return text, refusal, nil
}

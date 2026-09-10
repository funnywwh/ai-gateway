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
			for _, part := range item.Summary {
				if part.Text == "" {
					continue
				}
				if err := a.Add(pluginapi.Event{Type: pluginapi.EventReasoningDelta, Text: part.Text}); err != nil {
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

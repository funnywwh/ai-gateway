package chat

import (
	"encoding/json"
	"strings"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/pkg/pluginapi"
)

// The conversation that gets replayed to the model is built from stored provider items,
// never by re-parsing the text the console displayed. Three rules follow from that and all
// matter for correctness:
//
//  1. The whole conversation is replayed by default. A deployment may configure a window
//     (chat.max_history_messages / chat.max_history_bytes) instead, and then a turn is the
//     unit of truncation: cutting in the middle of a turn can leave a function_call without
//     its function_call_output, which providers reject outright (and which would silently
//     teach the model that tools may be ignored).
//  2. The newest turn is always kept. A window that cannot fit even one question must
//     produce a refusal the user can act on, not a call with half a question in it.
//  3. What is replayed has to be self-consistent: a tool exchange that lost its other half
//     is dropped (see pairToolItems). Without a window there is nothing to slide it out of.

// historyPlan is the slice of stored messages that will be replayed.
type historyPlan struct {
	items    []pluginapi.Item
	turns    int
	messages int
	bytes    int
	// dropped counts the turns left out because of a configured window, for the UI's
	// "earlier messages are not in this request" note.
	dropped int
	// tooLarge is set when even the newest turn does not fit a configured window; the
	// caller must stop.
	tooLarge bool
}

// buildHistory turns stored messages into provider items. A bound of 0 (or less) means that
// side is not bounded, and with both at 0 the whole conversation is replayed.
func buildHistory(messages []*domain.ChatMessage, maxMessages, maxBytes int) historyPlan {
	groups := groupByTurn(messages)
	plan := historyPlan{}
	// Walk groups newest-first, then reverse, so a window always drops the oldest turn.
	kept := make([][]pluginapi.Item, 0, len(groups))
	for i := len(groups) - 1; i >= 0; i-- {
		groupItems, size := itemsForMessages(groups[i])
		if len(groupItems) == 0 {
			continue
		}
		// The two bounds are independent: an operator who configures only one of them still
		// gets the whole conversation on the other axis.
		overMessages := maxMessages > 0 && plan.messages+len(groups[i]) > maxMessages
		overBytes := maxBytes > 0 && plan.bytes+size > maxBytes
		if (plan.messages > 0 || plan.turns > 0) && (overMessages || overBytes) {
			plan.dropped += i + 1
			break
		}
		kept = append(kept, groupItems)
		plan.messages += len(groups[i])
		plan.bytes += size
		plan.turns++
	}
	if len(kept) > 0 {
		// The newest turn alone exceeds a configured window. Reporting it beats sending a
		// request whose answer would be built on a truncated question.
		plan.tooLarge = (maxMessages > 0 && plan.messages > maxMessages) ||
			(maxBytes > 0 && plan.bytes > maxBytes)
	}
	plan.items = make([]pluginapi.Item, 0, plan.messages*2)
	for i := len(kept) - 1; i >= 0; i-- {
		plan.items = append(plan.items, kept[i]...)
	}
	plan.items = pairToolItems(plan.items)
	return plan
}

// pairToolItems makes the replayed history self-consistent by dropping tool exchanges that
// lost their other half. A turn interrupted before its tool ran — the user pressed stop, a
// step hit the tool budget, the arguments were cut off — leaves a function_call whose output
// never arrived, and providers reject such a history outright. While a window was sliding,
// that turn could eventually fall out of it; with the whole conversation replayed it would
// break the session for good. An output without its call is dropped for the same reason.
//
// Only stored history goes through this: the running turn's own items are paired by
// construction (toolResultItems always emits both halves).
func pairToolItems(items []pluginapi.Item) []pluginapi.Item {
	if len(items) == 0 {
		return items
	}
	calls := map[string]bool{}
	outputs := map[string]bool{}
	for _, item := range items {
		switch item.Type {
		case "function_call":
			calls[item.CallID] = true
		case "function_call_output":
			outputs[item.CallID] = true
		}
	}
	out := make([]pluginapi.Item, 0, len(items))
	dropped := false
	for _, item := range items {
		// An item with no call id cannot be paired with anything, so it cannot be sent
		// either: the upstream reads call_id as a required key of both halves.
		switch item.Type {
		case "function_call":
			if item.CallID == "" || !outputs[item.CallID] {
				dropped = true
				continue
			}
		case "function_call_output":
			if item.CallID == "" || !calls[item.CallID] {
				dropped = true
				continue
			}
		}
		out = append(out, item)
	}
	if !dropped {
		return items
	}
	return out
}

// groupByTurn splits messages at turn boundaries, preserving order. Messages with no turn
// id (older rows, or a user message written just before a failure) each stand alone.
func groupByTurn(messages []*domain.ChatMessage) [][]*domain.ChatMessage {
	groups := [][]*domain.ChatMessage{}
	for _, m := range messages {
		if m == nil {
			continue
		}
		if m.TurnID == "" || len(groups) == 0 || groups[len(groups)-1][0].TurnID != m.TurnID {
			groups = append(groups, []*domain.ChatMessage{m})
			continue
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], m)
	}
	return groups
}

// itemsForMessages converts one turn's stored messages into provider items and reports
// their serialized size.
func itemsForMessages(messages []*domain.ChatMessage) ([]pluginapi.Item, int) {
	items := []pluginapi.Item{}
	size := 0
	for _, m := range messages {
		switch m.Role {
		case domain.ChatRoleUser:
			items = append(items, userItem(m.Content))
			size += len(m.Content)
		case domain.ChatRoleAssistant:
			stored := decodeItems(m.ProviderItems)
			if len(stored) == 0 {
				// A message stored without provider items (an aborted turn, or a failure
				// before the first step) still has text worth replaying as context.
				if strings.TrimSpace(m.Content) == "" {
					continue
				}
				items = append(items, assistantTextItem(m.Content))
				size += len(m.Content)
				continue
			}
			items = append(items, stored...)
			size += len(m.ProviderItems)
		}
	}
	return items, size
}

// userItem builds one user message item.
func userItem(text string) pluginapi.Item {
	content, _ := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
	return pluginapi.Item{Type: "message", Role: "user", Content: content}
}

// assistantTextItem builds one assistant message item from plain text.
func assistantTextItem(text string) pluginapi.Item {
	content, _ := json.Marshal([]map[string]string{{"type": "output_text", "text": text}})
	return pluginapi.Item{Type: "message", Role: "assistant", Content: content}
}

// functionCallItem builds the canonical function_call item for one executed tool call.
func functionCallItem(callID, name, arguments string) pluginapi.Item {
	return pluginapi.Item{Type: "function_call", CallID: callID, Name: name, Arguments: arguments}
}

// functionOutputItem builds the canonical function_call_output item that answers it.
func functionOutputItem(callID, output string) pluginapi.Item {
	return pluginapi.Item{Type: "function_call_output", CallID: callID, Output: output}
}

func decodeItems(raw string) []pluginapi.Item {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []pluginapi.Item
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func encodeItems(items []pluginapi.Item) string {
	if len(items) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

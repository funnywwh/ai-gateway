package providerkit

import (
	"encoding/json"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestToolOutputContentChatAndEstimate(t *testing.T) {
	item := pluginapi.Item{Type: "function_call_output", CallID: "call_1", Output: "stale", OutputContent: json.RawMessage(`[{"type":"input_text","text":"first"},{"type":"input_text","text":"second"}]`)}
	messages, ok, err := itemToChatMessages(item)
	if err != nil || !ok || len(messages) != 1 {
		t.Fatalf("conversion: %v %v %v", messages, ok, err)
	}
	if messages[0].Content != "first second" || messages[0].ToolCallID != "call_1" {
		t.Fatalf("lost tool result: %+v", messages[0])
	}
	if got := EstimateInputTokens(&pluginapi.Request{Input: []pluginapi.Item{item}}, 1); got != int64(len(item.OutputContent)) {
		t.Fatalf("estimate %d", got)
	}
}

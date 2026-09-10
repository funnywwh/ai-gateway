package providerkit

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

const lf = 0x0A

func TestSSEReaderParsesEventsCommentsAndDone(t *testing.T) {
	stream := `event: message
data: {"chunk":1}

: keep-alive comment
data: {"chunk":2}
data: continued

data: [DONE]

`
	r := NewSSEReader(strings.NewReader(stream), 0)

	ev, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "message" || string(ev.Data) != `{"chunk":1}` {
		t.Fatalf("event 1 mismatch: %+v %q", ev, string(ev.Data))
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantMulti := `{"chunk":2}` + string([]byte{lf}) + "continued"
	if string(ev.Data) != wantMulti {
		t.Fatalf("multi-line data mismatch: %q", string(ev.Data))
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "done" {
		t.Fatalf("expected the DONE sentinel, got %+v", ev)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after DONE, got %v", err)
	}
}

func TestSSEReaderNextJSON(t *testing.T) {
	stream := `data: {"a":1}

data: {"b":2}
`
	r := NewSSEReader(strings.NewReader(stream), 0)
	var first map[string]int
	if _, err := r.NextJSON(&first); err != nil {
		t.Fatal(err)
	}
	if first["a"] != 1 {
		t.Fatalf("first payload mismatch: %+v", first)
	}
	var second map[string]int
	if _, err := r.NextJSON(&second); err != nil {
		t.Fatal(err)
	}
	if second["b"] != 2 {
		t.Fatalf("second payload mismatch: %+v", second)
	}
}

func TestResponsesToChatTranslation(t *testing.T) {
	maxTokens := 256
	temp := 0.2
	content, err := json.Marshal([]map[string]string{{"type": "input_text", "text": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal([]map[string]string{{"type": "output_text", "text": "prior answer"}})
	if err != nil {
		t.Fatal(err)
	}
	args := `{"q":"x"}`

	req := &pluginapi.Request{
		Model:           "gpt-x",
		Instructions:    "be brief",
		MaxOutputTokens: &maxTokens,
		Temperature:     &temp,
		Reasoning:       &pluginapi.Reasoning{Effort: "low"},
		Input: []pluginapi.Item{
			{Type: "message", Role: "user", Content: content},
			{Type: "reasoning", ID: "rs_1"},
			{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: args},
			{Type: "function_call_output", CallID: "call_1", Output: "42"},
			{Type: "message", Role: "assistant", Content: output},
		},
		Tools: []pluginapi.Tool{{Type: "function", Name: "lookup", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}

	chat, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Model != "gpt-x" || chat.MaxTokens == nil || *chat.MaxTokens != 256 {
		t.Fatalf("model/tokens mismatch: %+v", chat)
	}
	if chat.ReasoningEffort != "low" {
		t.Fatalf("reasoning effort not mapped: %q", chat.ReasoningEffort)
	}
	if len(chat.Messages) != 5 {
		t.Fatalf("expected 5 messages (system + 4 usable items), got %d: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "be brief" {
		t.Fatalf("instructions must become a system message: %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || chat.Messages[1].Content != "hello" {
		t.Fatalf("user content mismatch: %+v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "assistant" || len(chat.Messages[2].ToolCalls) != 1 {
		t.Fatalf("function_call must become assistant.tool_calls: %+v", chat.Messages[2])
	}
	if chat.Messages[2].ToolCalls[0].Function.Arguments != args {
		t.Fatalf("tool arguments must pass through: %+v", chat.Messages[2].ToolCalls[0])
	}
	if chat.Messages[3].Role != "tool" || chat.Messages[3].ToolCallID != "call_1" || chat.Messages[3].Content != "42" {
		t.Fatalf("function_call_output must become role:tool: %+v", chat.Messages[3])
	}
	if chat.Messages[4].Role != "assistant" || chat.Messages[4].Content != "prior answer" {
		t.Fatalf("assistant history mismatch: %+v", chat.Messages[4])
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "lookup" {
		t.Fatalf("tools mismatch: %+v", chat.Tools)
	}
}

func TestChatResponseToResponsesMapsUsageDimensions(t *testing.T) {
	usageJSON := `{
      "prompt_tokens": 100,
      "completion_tokens": 20,
      "total_tokens": 120,
      "prompt_cache_hit_tokens": 80,
      "prompt_cache_miss_tokens": 20,
      "completion_tokens_details": {"reasoning_tokens": 5}
    }`
	var usage ChatUsage
	if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
		t.Fatal(err)
	}
	resp := &ChatResponse{
		ID: "chatcmpl-1",
		Choices: []ChatChoice{{
			Index:        0,
			Message:      ChatMessage{Role: "assistant", Content: "hi there"},
			FinishReason: "stop",
		}},
		Usage: &usage,
	}

	out, err := ChatResponseToResponses(resp)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || len(out.Items) != 1 {
		t.Fatalf("response mismatch: %+v", out)
	}
	var parts []map[string]string
	if err := json.Unmarshal(out.Items[0].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if parts[0]["type"] != "output_text" || parts[0]["text"] != "hi there" {
		t.Fatalf("content parts mismatch: %+v", parts)
	}
	dims := out.Usage.Dimensions
	if dims["input_cache_hit"] != 80 || dims["input_cache_miss"] != 20 {
		t.Fatalf("cache dimensions not mapped: %+v", dims)
	}
	if dims["output"] != 20 || dims["reasoning"] != 5 {
		t.Fatalf("output/reasoning dimensions wrong: %+v", dims)
	}
	if _, ok := dims["input"]; ok {
		t.Fatalf("input must not be double counted when cache dims exist: %+v", dims)
	}
}

func TestChatResponseToResponsesWithoutCacheFields(t *testing.T) {
	resp := &ChatResponse{
		Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "x"}, FinishReason: "length"}},
		Usage:   &ChatUsage{PromptTokens: 10, CompletionTokens: 3},
	}
	out, err := ChatResponseToResponses(resp)
	if err != nil {
		t.Fatal(err)
	}
	if out.Usage.Dimensions["input"] != 10 {
		t.Fatalf("plain input tokens must map to the input dimension: %+v", out.Usage.Dimensions)
	}
	if out.Status != "incomplete" {
		t.Fatalf("finish_reason=length must map to incomplete, got %q", out.Status)
	}
}

func TestChatResponseToResponsesMapsToolCalls(t *testing.T) {
	var call ChatToolCall
	call.ID = "call_9"
	call.Type = "function"
	call.Function.Name = "lookup"
	call.Function.Arguments = `{"q":1}`

	resp := &ChatResponse{Choices: []ChatChoice{{
		Message:      ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{call}},
		FinishReason: "tool_calls",
	}}}
	out, err := ChatResponseToResponses(resp)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || len(out.Items) != 1 {
		t.Fatalf("tool call response mismatch: %+v", out)
	}
	if out.Items[0].Type != "function_call" || out.Items[0].Name != "lookup" || out.Items[0].CallID != "call_9" {
		t.Fatalf("function_call item mismatch: %+v", out.Items[0])
	}
}

func TestChatStreamStateTranslatesDeltas(t *testing.T) {
	state := NewChatStreamState()
	var got []pluginapi.Event
	emit := func(ev pluginapi.Event) error {
		got = append(got, ev)
		return nil
	}

	var start, args ChatToolCall
	start.Index, start.ID, start.Type = 0, "call_1", "function"
	start.Function.Name = "lookup"
	args.Index = 0
	args.Function.Arguments = `{"q":1}`

	chunks := []ChatResponse{
		{Choices: []ChatChoice{{Delta: ChatMessage{Role: "assistant", Content: "Hel"}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{Role: "assistant", Content: "lo"}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{start}}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{args}}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{Role: "assistant"}, FinishReason: "tool_calls"}}},
		{Usage: &ChatUsage{PromptTokens: 7, CompletionTokens: 4}},
	}
	for i := range chunks {
		if _, err := state.Translate(&chunks[i], emit); err != nil {
			t.Fatal(err)
		}
	}

	var text string
	var sawStart, sawArgs bool
	for _, ev := range got {
		switch ev.Type {
		case pluginapi.EventTextDelta:
			text += ev.Text
		case pluginapi.EventToolCallStart:
			sawStart = true
			if ev.Name != "lookup" || ev.CallID != "call_1" {
				t.Fatalf("tool call start mismatch: %+v", ev)
			}
		case pluginapi.EventToolArgsDelta:
			sawArgs = true
			if ev.Text != args.Function.Arguments {
				t.Fatalf("tool args mismatch: %+v", ev)
			}
		}
	}
	if text != "Hello" {
		t.Fatalf("streamed text = %q", text)
	}
	if !sawStart || !sawArgs {
		t.Fatalf("tool call events missing (start=%t args=%t)", sawStart, sawArgs)
	}
	if state.FinishReason != "tool_calls" || state.Usage == nil || state.Usage.CompletionTokens != 4 {
		t.Fatalf("state not accumulated: %+v", state)
	}
}

func TestCharEstimatorRoundsUp(t *testing.T) {
	e := NewCharEstimator(4)
	if got := e.Add("abcde"); got != 2 {
		t.Fatalf("5 chars / 4 = 2 (rounded up), got %d", got)
	}
	if got := e.Add(""); got != 2 {
		t.Fatalf("empty add must not change the estimate: %d", got)
	}
	if got := e.Add("abc"); got != 2 {
		t.Fatalf("8 chars / 4 = 2, got %d", got)
	}
	e.Reset()
	if e.Tokens() != 0 {
		t.Fatalf("reset must clear the estimate")
	}
	if EstimateTokens("", 4) != 0 {
		t.Fatal("empty text estimates zero")
	}
}

func TestEstimateInputTokens(t *testing.T) {
	content, err := json.Marshal("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	req := &pluginapi.Request{
		Instructions: "12345678",
		Input:        []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
	}
	if got := EstimateInputTokens(req, 4); got <= 0 {
		t.Fatalf("estimate must be positive, got %d", got)
	}
	if EstimateInputTokens(nil, 4) != 0 {
		t.Fatal("nil request estimates zero")
	}
}

func TestMergeAndTotalTokens(t *testing.T) {
	dims := MergeDimensions(map[string]int64{"input": 1}, map[string]int64{"input": 2, "output": 3})
	if dims["input"] != 3 || dims["output"] != 3 {
		t.Fatalf("merge mismatch: %+v", dims)
	}
	if got := TotalTokens(dims); got != 6 {
		t.Fatalf("total = %d, want 6", got)
	}
}

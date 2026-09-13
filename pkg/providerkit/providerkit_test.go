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

// ---------------------------------------------------------------------------
// chain-of-thought translation (M17)
// ---------------------------------------------------------------------------

// reasoningItem builds a reasoning item the way the gateway stores one.
func reasoningItem(text string) pluginapi.Item {
	return ReasoningItem(text)
}

func TestReasoningDeltaIsTranslated(t *testing.T) {
	state := NewChatStreamState()
	var got []pluginapi.Event
	emit := func(ev pluginapi.Event) error {
		got = append(got, ev)
		return nil
	}

	chunks := []ChatResponse{
		{Choices: []ChatChoice{{Delta: ChatMessage{ReasoningContent: "think "}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{ReasoningContent: "harder"}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{Content: "answer"}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{}, FinishReason: "stop"}}},
	}
	for i := range chunks {
		if _, err := state.Translate(&chunks[i], emit); err != nil {
			t.Fatal(err)
		}
	}

	if state.ReasoningText != "think harder" {
		t.Fatalf("reasoning text = %q", state.ReasoningText)
	}
	if len(got) != 3 {
		t.Fatalf("expected 2 reasoning + 1 text event, got %+v", got)
	}
	if got[0].Type != pluginapi.EventReasoningDelta || got[0].Text != "think " ||
		got[1].Type != pluginapi.EventReasoningDelta || got[1].Text != "harder" {
		t.Fatalf("reasoning events mismatch: %+v", got[:2])
	}
	if got[2].Type != pluginapi.EventTextDelta || got[2].Text != "answer" {
		t.Fatalf("text event must follow the reasoning: %+v", got[2])
	}
}

func TestToolArgumentDeltasReuseTheCallID(t *testing.T) {
	state := NewChatStreamState()
	var got []pluginapi.Event
	emit := func(ev pluginapi.Event) error {
		got = append(got, ev)
		return nil
	}

	// Upstreams send the id and name once, then only argument fragments.
	var head, frag ChatToolCall
	head.Index, head.ID, head.Type = 0, "call_7", "function"
	head.Function.Name = "lookup"
	frag.Index = 0
	frag.Function.Arguments = `{"q":1}`

	chunks := []ChatResponse{
		{Choices: []ChatChoice{{Delta: ChatMessage{ToolCalls: []ChatToolCall{head}}}}},
		{Choices: []ChatChoice{{Delta: ChatMessage{ToolCalls: []ChatToolCall{frag}}}}},
	}
	for i := range chunks {
		if _, err := state.Translate(&chunks[i], emit); err != nil {
			t.Fatal(err)
		}
	}

	if len(got) != 2 {
		t.Fatalf("expected a start and an arguments event, got %+v", got)
	}
	if got[0].CallID != "call_7" {
		t.Fatalf("start event must carry the call id: %+v", got[0])
	}
	if got[1].CallID != "call_7" || got[1].ItemID != "call_7" {
		t.Fatalf("argument delta must reuse the call id, got %+v", got[1])
	}
	if got[1].Name != "lookup" {
		t.Fatalf("argument delta must carry the tool name: %+v", got[1])
	}
}

// TestAssistantTextIsFoldedIntoTheToolCall pins the shape a thinking-mode upstream
// validates as one turn. A Responses client sends the assistant's text and its tool
// calls as separate items, and two assistant messages are what the upstream rejects
// ("The `reasoning_content` in the thinking mode must be passed back to the API.") even
// when the tool-calling message carries the field — the text that announces the call
// belongs to the same turn. Folding reproduces the message the upstream's own API would
// have produced.
func TestAssistantTextIsFoldedIntoTheToolCall(t *testing.T) {
	content, _ := json.Marshal("what is the weather")
	calls := []pluginapi.Item{
		{Type: "function_call", CallID: "call_1", Name: "weather", Arguments: "{}"},
		{Type: "function_call_output", CallID: "call_1", Output: "cloudy"},
	}
	assistantText := pluginapi.Item{Type: "message", Role: "assistant", Content: json.RawMessage(`[{"type":"output_text","text":"let me check"}]`)}
	secondText := pluginapi.Item{Type: "message", Role: "assistant", Content: json.RawMessage(`[{"type":"output_text","text":"and now:"}]`)}

	req := &pluginapi.Request{Model: "m", Input: append([]pluginapi.Item{
		{Type: "message", Role: "user", Content: content},
		reasoningItem("check the tool"),
		assistantText, secondText,
	}, calls...)}

	chat, err := ResponsesToChatWithOptions(req, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	assistants := 0
	var tool *ChatMessage
	for i := range chat.Messages {
		if chat.Messages[i].Role != "assistant" {
			continue
		}
		assistants++
		if len(chat.Messages[i].ToolCalls) > 0 {
			tool = &chat.Messages[i]
		}
	}
	if assistants != 1 || tool == nil {
		t.Fatalf("the turn must travel as one assistant message, got %d: %+v", assistants, chat.Messages)
	}
	if !strings.Contains(tool.Content, "let me check") || !strings.Contains(tool.Content, "and now:") {
		t.Fatalf("the announcing text must survive the fold: %q", tool.Content)
	}
	if tool.ReasoningContent != "check the tool" || !tool.ReasoningRequired {
		t.Fatalf("the folded message must carry the chain of thought: %+v", tool)
	}

	// Without the opt-in the message boundary is untouched: the fold belongs to the
	// dialect that needs it.
	plain, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	plainAssistants := 0
	for _, msg := range plain.Messages {
		if msg.Role == "assistant" {
			plainAssistants++
		}
	}
	if plainAssistants != 3 {
		t.Fatalf("default translation must keep text and calls apart, got %d: %+v", plainAssistants, plain.Messages)
	}
}

func TestResponsesToChatReplaysReasoningOnlyForToolTurns(t *testing.T) {
	content, _ := json.Marshal("what is the weather")
	base := []pluginapi.Item{
		{Type: "message", Role: "user", Content: content},
		reasoningItem("check the tool"),
		{Type: "function_call", CallID: "call_1", Name: "weather", Arguments: "{}"},
		{Type: "function_call_output", CallID: "call_1", Output: "cloudy"},
	}

	withReplay, err := ResponsesToChatWithOptions(&pluginapi.Request{Model: "m", Input: base},
		ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	assistant := withReplay.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("expected the tool-calling assistant message second: %+v", withReplay.Messages)
	}
	if assistant.ReasoningContent != "check the tool" {
		t.Fatalf("reasoning must be replayed onto the tool call: %+v", assistant)
	}

	// Opting out keeps the old shape: reasoning items are simply dropped.
	withoutReplay, err := ResponsesToChat(&pluginapi.Request{Model: "m", Input: base})
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range withoutReplay.Messages {
		if msg.ReasoningContent != "" {
			t.Fatalf("default translation must not emit reasoning_content: %+v", withoutReplay.Messages)
		}
	}

	// The upstream demands the key itself, not just a non-empty value: a client that
	// sends no reasoning items at all (DSH pointed at a gateway does not) must still
	// see the request go through, so the tool-calling message carries an empty
	// reasoning_content rather than a missing one.
	bare, err := ResponsesToChatWithOptions(&pluginapi.Request{Model: "m",
		Input: []pluginapi.Item{
			{Type: "message", Role: "user", Content: content},
			{Type: "function_call", CallID: "call_1", Name: "weather", Arguments: "{}"},
			{Type: "function_call_output", CallID: "call_1", Output: "cloudy"},
		}}, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bare)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_content":""`) {
		t.Fatalf("a tool turn without reasoning must still send the key: %s", raw)
	}
	// ... and without the opt-in the field stays off the wire entirely.
	def, err := ResponsesToChat(&pluginapi.Request{Model: "m",
		Input: []pluginapi.Item{
			{Type: "message", Role: "user", Content: content},
			{Type: "function_call", CallID: "call_1", Name: "weather", Arguments: "{}"},
			{Type: "function_call_output", CallID: "call_1", Output: "cloudy"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	rawDefault, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawDefault), "reasoning_content") {
		t.Fatalf("default translation must not mention reasoning_content: %s", rawDefault)
	}

	// A reasoning item that carries its text in content parts (the shape an upstream
	// Responses stream produces) is honoured too.
	contentReasoning := pluginapi.Item{Type: "reasoning", ID: "rs_2",
		Content: json.RawMessage(`[{"type":"reasoning_text","text":"from content"}]`)}
	fromContent, err := ResponsesToChatWithOptions(&pluginapi.Request{Model: "m",
		Input: []pluginapi.Item{
			{Type: "message", Role: "user", Content: content},
			contentReasoning,
			{Type: "function_call", CallID: "call_1", Name: "weather", Arguments: "{}"},
			{Type: "function_call_output", CallID: "call_1", Output: "cloudy"},
		}}, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := fromContent.Messages[1].ReasoningContent; got != "from content" {
		t.Fatalf("reasoning carried in content parts = %q, want %q", got, "from content")
	}

	// A conversation without tool calls never carries the field: it is only
	// required when the upstream asked for tools.
	plain, err := ResponsesToChatWithOptions(&pluginapi.Request{
		Model: "m",
		Input: []pluginapi.Item{{Type: "message", Role: "user", Content: content}, reasoningItem("thinking")},
	}, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range plain.Messages {
		if msg.ReasoningContent != "" {
			t.Fatalf("reasoning must not be replayed without tool calls: %+v", plain.Messages)
		}
	}
}

func TestReasoningReplayStopsAtATurnBoundary(t *testing.T) {
	content, _ := json.Marshal("next question")
	req := &pluginapi.Request{
		Model: "m",
		Input: []pluginapi.Item{
			reasoningItem("belongs to the old turn"),
			{Type: "function_call", CallID: "call_1", Name: "a", Arguments: "{}"},
			{Type: "function_call_output", CallID: "call_1", Output: "x"},
			{Type: "message", Role: "user", Content: content},
			{Type: "function_call", CallID: "call_2", Name: "b", Arguments: "{}"},
			{Type: "function_call_output", CallID: "call_2", Output: "y"},
		},
	}
	chat, err := ResponsesToChatWithOptions(req, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	// The second call carries an output on purpose: a chat request may not contain a
	// tool_call that no tool message answers, so an unanswered fixture would be repaired
	// away and the turn boundary would no longer be observable.
	if len(chat.Messages) != 5 {
		t.Fatalf("message count = %d: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0].ReasoningContent != "belongs to the old turn" {
		t.Fatalf("first tool call must keep its reasoning: %+v", chat.Messages[0])
	}
	if chat.Messages[3].ReasoningContent != "" {
		t.Fatalf("a user message closes the turn; the next tool call has no reasoning: %+v", chat.Messages[3])
	}
}

// Parallel tool calls are one assistant turn, and Chat Completions ties an assistant
// message's tool_calls to the tool messages directly after it: one assistant message per
// function_call item leaves the earlier ones unanswered and the upstream rejects the whole
// request ("An assistant message with 'tool_calls' must be followed by tool messages
// responding to each 'tool_call_id'").
func TestParallelToolCallsShareOneAssistantMessage(t *testing.T) {
	req := &pluginapi.Request{Model: "m", Input: []pluginapi.Item{
		{Type: "function_call", CallID: "call_a", Name: "bash", Arguments: "{}"},
		{Type: "function_call", CallID: "call_b", Name: "grep", Arguments: "{}"},
		{Type: "function_call_output", CallID: "call_a", Output: "file1"},
		{Type: "function_call_output", CallID: "call_b", Output: "no match"},
	}}
	chat, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("messages = %+v, want one assistant message plus two tool messages", chat.Messages)
	}
	if chat.Messages[0].Role != "assistant" || len(chat.Messages[0].ToolCalls) != 2 {
		t.Fatalf("parallel calls must share one assistant message: %+v", chat.Messages[0])
	}
	if chat.Messages[0].ToolCalls[0].ID != "call_a" || chat.Messages[0].ToolCalls[1].ID != "call_b" {
		t.Fatalf("tool call order must be preserved: %+v", chat.Messages[0].ToolCalls)
	}
	for i, want := range []string{"call_a", "call_b"} {
		msg := chat.Messages[i+1]
		if msg.Role != "tool" || msg.ToolCallID != want {
			t.Fatalf("messages[%d] = %+v, want a tool answer for %s", i+1, msg, want)
		}
	}
}

// An interrupted turn (call without output) and a trimmed history (output without call) are
// shapes a client cannot avoid producing; both violate the same upstream rule, so the
// translation repairs them instead of letting the whole request fail.
func TestUnansweredAndOrphanToolMessagesAreRepaired(t *testing.T) {
	req := &pluginapi.Request{Model: "m", Input: []pluginapi.Item{
		{Type: "function_call", CallID: "answered", Name: "a", Arguments: "{}"},
		{Type: "function_call", CallID: "never_ran", Name: "b", Arguments: "{}"},
		{Type: "function_call_output", CallID: "answered", Output: "ok"},
		{Type: "function_call_output", CallID: "from_a_trimmed_turn", Output: "orphan"},
		{Type: "message", Role: "user", Content: json.RawMessage(`"next"`)},
	}}
	chat, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("messages = %+v, want assistant + its tool answer + the user message", chat.Messages)
	}
	if len(chat.Messages[0].ToolCalls) != 1 || chat.Messages[0].ToolCalls[0].ID != "answered" {
		t.Fatalf("the unanswered call must be dropped: %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "tool" || chat.Messages[1].ToolCallID != "answered" {
		t.Fatalf("the answered call must keep its tool message: %+v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "user" {
		t.Fatalf("the orphan tool message must be dropped: %+v", chat.Messages)
	}
}

func TestChatResponseKeepsReasoningAsItem(t *testing.T) {
	resp := &ChatResponse{Choices: []ChatChoice{{
		Message:      ChatMessage{Role: "assistant", Content: "answer", ReasoningContent: "because"},
		FinishReason: "stop",
	}}}

	kept, err := ChatResponseToResponsesWithOptions(resp, ChatConvertOptions{KeepReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Items) != 2 || kept.Items[0].Type != "reasoning" {
		t.Fatalf("reasoning item must lead: %+v", kept.Items)
	}
	if kept.Items[0].Summary[0].Type != "summary_text" || kept.Items[0].Summary[0].Text != "because" {
		t.Fatalf("reasoning text must ride in the summary part: %+v", kept.Items[0])
	}
	if kept.Items[1].Type != "message" {
		t.Fatalf("answer item mismatch: %+v", kept.Items[1])
	}

	// Default behaviour is unchanged: no reasoning item at all.
	dropped, err := ChatResponseToResponses(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped.Items) != 1 || dropped.Items[0].Type != "message" {
		t.Fatalf("default conversion must drop reasoning: %+v", dropped.Items)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"stop":           "completed",
		"tool_calls":     "completed",
		"length":         "incomplete",
		"content_filter": "incomplete",
		"":               "completed",
	}
	for reason, want := range cases {
		resp := &ChatResponse{Choices: []ChatChoice{{
			Message: ChatMessage{Role: "assistant", Content: "x"}, FinishReason: reason,
		}}}
		out, err := ChatResponseToResponses(resp)
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != want {
			t.Fatalf("finish_reason %q → %q, want %q", reason, out.Status, want)
		}
	}

	// A response without choices must still look completed, not blank.
	empty, err := ChatResponseToResponses(&ChatResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Status != "completed" {
		t.Fatalf("empty choices must still produce a completed status, got %q", empty.Status)
	}
}

func TestUpstreamFailureReasons(t *testing.T) {
	for _, reason := range []string{"insufficient_system_resource", "aborted"} {
		if !UpstreamFailure(reason) {
			t.Fatalf("%q must be treated as an upstream failure", reason)
		}
	}
	for _, reason := range []string{"stop", "tool_calls", "length", "content_filter", ""} {
		if UpstreamFailure(reason) {
			t.Fatalf("%q must not be treated as an upstream failure", reason)
		}
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

func TestParseProxyURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string // normalized URL, empty means nil
		wantErr string // substring of the expected error, empty means success
	}{
		{name: "empty means unconfigured", raw: "   ", want: ""},
		{name: "http with port", raw: "http://127.0.0.1:2334", want: "http://127.0.0.1:2334"},
		{name: "https without port", raw: "https://proxy.example.com", want: "https://proxy.example.com"},
		{name: "socks5 with port", raw: "socks5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
		{name: "socks5h with port", raw: "socks5h://10.0.0.9:1080", want: "socks5h://10.0.0.9:1080"},
		{name: "scheme is lowercased", raw: "HTTP://127.0.0.1:2334", want: "http://127.0.0.1:2334"},
		{name: "userinfo is preserved", raw: "http://user:pw@127.0.0.1:2334", want: "http://user:pw@127.0.0.1:2334"},
		{name: "surrounding space is trimmed", raw: "  http://127.0.0.1:2334  ", want: "http://127.0.0.1:2334"},

		{name: "missing scheme is named", raw: "127.0.0.1:2334", wantErr: "must start with a scheme"},
		{name: "unknown scheme is rejected", raw: "ftp://127.0.0.1:2121", wantErr: "unsupported proxy scheme"},
		{name: "missing host is rejected", raw: "http://", wantErr: "must include a host"},
		{name: "socks5 without port is rejected", raw: "socks5://127.0.0.1", wantErr: "socks5 proxy URL must include a port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProxyURL(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseProxyURL(%q) = %v, want error containing %q", tc.raw, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProxyURL(%q) unexpected error: %v", tc.raw, err)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("ParseProxyURL(%q) = %v, want nil for an unconfigured proxy", tc.raw, got)
				}
				return
			}
			if got == nil || got.String() != tc.want {
				t.Fatalf("ParseProxyURL(%q) = %v, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMaskProxyURLNeverLeaksCredentials(t *testing.T) {
	u, err := ParseProxyURL("http://alice:s3cret@proxy.example.com:2334")
	if err != nil {
		t.Fatal(err)
	}
	got := MaskProxyURL(u)
	if got != "http://proxy.example.com:2334" {
		t.Fatalf("MaskProxyURL = %q, want %q", got, "http://proxy.example.com:2334")
	}
	if strings.Contains(got, "s3cret") || strings.Contains(got, "alice") {
		t.Fatalf("masked URL leaked userinfo: %q", got)
	}
	if MaskProxyURL(nil) != "" {
		t.Fatal("MaskProxyURL(nil) must be empty")
	}
}

// Chat Completions names the system-level channel "system"; "developer" is the newer
// Responses-side alias and most OpenAI-compatible upstreams reject it outright.
func TestChatTranslationNormalizesSystemRoles(t *testing.T) {
	req := &pluginapi.Request{Model: "m", Input: []pluginapi.Item{
		{Type: "message", Role: "developer", Content: json.RawMessage(`[{"type":"input_text","text":"be terse"}]`)},
		{Type: "message", Role: "user", Content: json.RawMessage(`[{"type":"input_text","text":"hi"}]`)},
		{Type: "message", Role: "assistant", Content: json.RawMessage(`[{"type":"output_text","text":"ok"}]`)},
	}}
	out, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %+v", out.Messages)
	}
	if out.Messages[0].Role != "system" {
		t.Fatalf("developer must be sent as system, got %q", out.Messages[0].Role)
	}
	if out.Messages[1].Role != "user" || out.Messages[2].Role != "assistant" {
		t.Fatalf("other roles must be untouched: %+v", out.Messages)
	}
}

// Chat Completions can only express function tools; the richer Responses types are for
// upstreams that implement them natively.
func TestChatTranslationDropsToolsItCannotExpress(t *testing.T) {
	req := &pluginapi.Request{Model: "m", Tools: []pluginapi.Tool{
		{Type: "web_search", Raw: json.RawMessage(`{"type":"web_search","external_web_access":false}`)},
		{Type: "function", Name: "bash", Description: "run"},
	}}
	out, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("chat tools = %+v, want only the function tool", out.Tools)
	}
	if out.Tools[0].Function.Name != "bash" || out.Tools[0].Type != "function" {
		t.Fatalf("chat tool = %+v", out.Tools[0])
	}
}

// TestEveryAssistantMessageOfAToolConversationCarriesTheReasoningKey pins the full
// extent of the thinking-mode requirement, which is wider than "the message with the
// tool calls".
//
// The shape below is the one DSH produces and the one the earlier tool-call-only rule
// missed: the assistant narrates what the tool returned, and that message is the FIRST
// assistant message of the request while the tool call sits later in the history. An
// upstream that validates the turn as a whole (DeepSeek: "The `reasoning_content` in the
// thinking mode must be passed back to the API.") rejects the request over it.
func TestEveryAssistantMessageOfAToolConversationCarriesTheReasoningKey(t *testing.T) {
	user := func(text string) pluginapi.Item {
		raw, _ := json.Marshal(text)
		return pluginapi.Item{Type: "message", Role: "user", Content: raw}
	}
	assistant := func(text string) pluginapi.Item {
		raw, _ := json.Marshal([]map[string]string{{"type": "output_text", "text": text}})
		return pluginapi.Item{Type: "message", Role: "assistant", Content: raw}
	}
	call := func(id string) pluginapi.Item {
		return pluginapi.Item{Type: "function_call", CallID: id, Name: "read_file", Arguments: "{}"}
	}
	output := func(id string) pluginapi.Item {
		return pluginapi.Item{Type: "function_call_output", CallID: id, Output: "42"}
	}

	// Two full tool turns: the text that follows each answer is not adjacent to the
	// next call, so it cannot be folded into it — and it is still part of the turn.
	req := &pluginapi.Request{Model: "m", Input: []pluginapi.Item{
		user("read both files"),
		reasoningItem("start with the first"),
		assistant("opening the first file"),
		call("c1"), output("c1"),
		assistant("the first file has 42 lines"),
		assistant("now the second one"),
		call("c2"), output("c2"),
		assistant("both files are read"),
	}}
	out, err := ResponsesToChatWithOptions(req, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	messages := body.Messages
	assistants := 0
	for i, message := range messages {
		if message["role"] != "assistant" {
			continue
		}
		assistants++
		if _, present := message["reasoning_content"]; !present {
			t.Fatalf("assistant message %d has no reasoning_content: %v", i, message)
		}
	}
	// The announcing text of each turn is folded into its own tool call, so three
	// assistant messages travel: two tool turns and the closing narrative.
	if assistants != 3 {
		t.Fatalf("expected the two tool turns plus the closing narrative, got %d: %v", assistants, messages)
	}
	// The chain of thought stays on the turn it belongs to, and is not duplicated.
	if got := messages[1]["reasoning_content"]; got != "start with the first" {
		t.Fatalf("reasoning_content of the first tool turn = %v, want the replayed text", got)
	}
	if _, present := messages[1]["reasoning_content"]; !present {
		t.Fatalf("the first tool turn must carry the key: %v", messages[1])
	}
	// The second turn has no reasoning item of its own: the key travels empty rather
	// than missing, and the first turn's text is not borrowed for it.
	if got := messages[3]["reasoning_content"]; got != "" {
		t.Fatalf("the second tool turn must carry an empty key, got %v", got)
	}
	if got := messages[5]["reasoning_content"]; got != "" {
		t.Fatalf("the closing narrative must carry an empty key, got %v", got)
	}
	if !strings.Contains(messages[5]["content"].(string), "both files are read") {
		t.Fatalf("the closing narrative must survive: %v", messages[5])
	}

	// A conversation that never reaches a tool call keeps the field off the wire: the
	// requirement is what the opt-in pays for, not a blanket new field.
	plain, err := ResponsesToChatWithOptions(&pluginapi.Request{Model: "m", Input: []pluginapi.Item{
		user("hello"), assistant("hi"),
	}}, ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	plainRaw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainRaw), "reasoning_content") {
		t.Fatalf("a tool-free conversation must not carry the key: %s", plainRaw)
	}
}

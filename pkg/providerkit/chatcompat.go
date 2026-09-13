package providerkit

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// ---------------------------------------------------------------------------
// Chat Completions wire types (the subset the gateway needs)
// ---------------------------------------------------------------------------

// ChatToolCall is one tool invocation in a chat-completions message or delta.
type ChatToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// ChatMessage is one chat-completions message.
//
// ReasoningContent carries the chain of thought some upstreams (DeepSeek and
// compatible deployments) return at the same level as content. It is only ever
// populated or sent when the caller opts in through ChatConvertOptions.
type ChatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Refusal          string         `json:"refusal,omitempty"`
	// ReasoningRequired puts reasoning_content on the wire even when it is empty.
	// Thinking-mode upstreams require the key on every assistant message that carries
	// tool_calls, and reject the whole request when it is missing ("The
	// `reasoning_content` in the thinking mode must be passed back to the API."). A
	// stateless gateway cannot recover a chain of thought the client did not send
	// back, so the key travels with the value it can honestly offer — empty — instead
	// of taking the request down. The empty string is accepted by the upstream;
	// omitting the field is not.
	ReasoningRequired bool `json:"-"`
}

// MarshalJSON writes the message with reasoning_content present when required. An
// empty chain of thought is omitted otherwise: most OpenAI-compatible upstreams do
// not know the field and should not see it.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	type plain ChatMessage
	if !m.ReasoningRequired {
		return json.Marshal(plain(m))
	}
	// The outer field shadows the embedded one (encoding/json prefers the shallowest
	// match), so this writes reasoning_content without omitempty.
	type withReasoning struct {
		plain
		ReasoningContent string `json:"reasoning_content"`
	}
	return json.Marshal(withReasoning{plain: plain(m), ReasoningContent: m.ReasoningContent})
}

// ChatTool is a function tool in chat-completions form.
type ChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"function"`
}

// ChatStreamOptions asks the upstream to include usage in the final chunk.
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatRequest is an upstream /chat/completions request body.
type ChatRequest struct {
	Model             string             `json:"model"`
	Messages          []ChatMessage      `json:"messages"`
	MaxTokens         *int               `json:"max_tokens,omitempty"`
	Temperature       *float64           `json:"temperature,omitempty"`
	TopP              *float64           `json:"top_p,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *ChatStreamOptions `json:"stream_options,omitempty"`
	Tools             []ChatTool         `json:"tools,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
}

// ChatUsage is the upstream usage block (DeepSeek-style cache fields included).
type ChatUsage struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	TotalTokens             int64 `json:"total_tokens"`
	PromptCacheHitTokens    int64 `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens   int64 `json:"prompt_cache_miss_tokens,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	} `json:"completion_tokens_details,omitempty"`
}

// ChatChoice is one choice of a chat-completions response.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	Delta        ChatMessage `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// ChatResponse is an upstream /chat/completions response body.
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Translation
// ---------------------------------------------------------------------------

// ChatConvertOptions switches the opt-in behaviours of the chat-completions
// translation. The zero value is the conservative default: no reasoning content
// moves in either direction, which is what generic OpenAI-compatible upstreams
// expect.
type ChatConvertOptions struct {
	// ReplayReasoningContent writes the text of reasoning items onto the assistant
	// message that carries the following tool calls. Upstreams that reason before
	// calling tools (DeepSeek) reject the request when it is missing.
	ReplayReasoningContent bool
	// KeepReasoningContent maps upstream reasoning_content onto a reasoning item
	// (non-streaming) so the gateway can surface and persist the chain of thought.
	KeepReasoningContent bool
}

// ResponsesToChat converts a canonical (Responses-shaped) request into a
// chat-completions request. Reasoning items are dropped (they are not replayable),
// function calls/results become tool_calls / role:tool messages.
func ResponsesToChat(req *pluginapi.Request) (*ChatRequest, error) {
	return ResponsesToChatWithOptions(req, ChatConvertOptions{})
}

// ResponsesToChatWithOptions is ResponsesToChat with the opt-in behaviours enabled.
func ResponsesToChatWithOptions(req *pluginapi.Request, opts ChatConvertOptions) (*ChatRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("providerkit: nil request")
	}
	out := &ChatRequest{
		Model:             req.Model,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		ToolChoice:        req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
	}
	if req.MaxOutputTokens != nil {
		// Chat Completions has no 16-token minimum, but the gateway enforces it upstream.
		out.MaxTokens = req.MaxOutputTokens
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.ReasoningEffort = req.Reasoning.Effort
	}

	if req.Instructions != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: "system", Content: req.Instructions})
	}
	// Reasoning replay is only defined for a conversation that has tool-calling turns:
	// that is the case the upstream's requirement covers, and the only case where the
	// gateway has a chain of thought to place.
	replay := map[int]string{}
	reasoningTurn := opts.ReplayReasoningContent && hasToolCallItems(req.Input)
	if reasoningTurn {
		replay = reasoningByToolTurn(req.Input)
	}
	for index, item := range req.Input {
		msg, ok, err := itemToChatMessages(item)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// Upstreams that demand their chain of thought back demand the key itself on
		// every assistant message it can reach, not only the tool-calling one: a
		// client that never sends reasoning items (DSH pointed at this gateway does
		// not) must still get its request through, with an empty value rather than a
		// missing field, and the text is used whenever the client does provide it.
		if opts.ReplayReasoningContent {
			text := replay[index]
			for i := range msg {
				if msg[i].Role == "assistant" && len(msg[i].ToolCalls) > 0 {
					msg[i].ReasoningContent = text
					msg[i].ReasoningRequired = true
					break
				}
			}
		}
		if mergeParallelToolCalls(out.Messages, msg) {
			continue
		}
		if opts.ReplayReasoningContent {
			if folded, ok := foldAssistantTextIntoCall(out.Messages, msg); ok {
				out.Messages = folded
				continue
			}
		}
		out.Messages = append(out.Messages, msg...)
	}
	out.Messages = repairToolSequences(out.Messages)
	if reasoningTurn {
		requireReasoningKeys(out.Messages)
	}
	for _, tool := range req.Tools {
		// Chat Completions can only express function tools. The richer Responses types
		// (web_search, namespace, ...) are client-side conveniences for upstreams that
		// implement them natively; here they are simply not offered to the model, which is
		// the honest translation — inventing a nameless function tool would garble the
		// request, and rejecting it would fail a call the model can otherwise serve.
		if tool.Type != "" && tool.Type != "function" {
			continue
		}
		var ct ChatTool
		ct.Type = "function"
		ct.Function.Name = tool.Name
		ct.Function.Description = tool.Description
		ct.Function.Parameters = tool.Parameters
		ct.Function.Strict = tool.Strict
		out.Tools = append(out.Tools, ct)
	}
	return out, nil
}

// hasToolCallItems reports whether the input replays a tool-calling turn.
func hasToolCallItems(items []pluginapi.Item) bool {
	for _, item := range items {
		if item.Type == "function_call" {
			return true
		}
	}
	return false
}

// reasoningByToolTurn maps the index of each assistant tool-calling message onto
// the reasoning text that precedes it within the same turn. A reasoning item with
// no following tool call (or with empty text) is left alone: guessing where it
// belongs would send the upstream a context it never produced.
func reasoningByToolTurn(items []pluginapi.Item) map[int]string {
	out := map[int]string{}
	pending := ""
	for index, item := range items {
		switch item.Type {
		case "reasoning":
			pending = itemReasoningText(item)
		case "function_call":
			if pending != "" {
				out[index] = pending
				pending = ""
			}
		case "message":
			// Assistant history stays inside the turn; a user message opens a new one.
			if role := item.Role; role != "assistant" && role != "" {
				pending = ""
			}
		default:
			pending = ""
		}
	}
	return out
}

// itemReasoningText extracts the chain of thought of a reasoning item. The text
// rides in the summary parts when the gateway itself produced the item (that is the
// part it streams as reasoning summary events), and in content parts when it came
// from an upstream Responses shape — the same "summary first, content as fallback"
// rule the assembler applies, so a client may send either form.
func itemReasoningText(item pluginapi.Item) string {
	var sb strings.Builder
	for _, part := range item.Summary {
		sb.WriteString(part.Text)
	}
	if sb.Len() == 0 && len(item.Content) > 0 {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item.Content, &parts); err == nil {
			for _, part := range parts {
				sb.WriteString(part.Text)
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

func itemToChatMessages(item pluginapi.Item) ([]ChatMessage, bool, error) {
	switch item.Type {
	case "message", "":
		role := item.Role
		if role == "" {
			role = "user"
		}
		// Chat Completions names the system-level channel "system"; "developer" is the newer
		// Responses-side alias for that same slot, and most OpenAI-compatible servers
		// (DeepSeek, vLLM, Ollama, ...) reject it as an unknown role variant. Translating
		// here lets a client that speaks the newer dialect reach a classic upstream unchanged
		// — the alternative is telling every such client to rewrite its requests.
		if role == "developer" {
			role = "system"
		}
		return []ChatMessage{{Role: role, Content: contentToText(item.Content, role)}}, true, nil
	case "function_call":
		call := ChatToolCall{ID: item.CallID, Type: "function"}
		if call.ID == "" {
			call.ID = item.ID
		}
		call.Function.Name = item.Name
		call.Function.Arguments = item.Arguments
		return []ChatMessage{{Role: "assistant", ToolCalls: []ChatToolCall{call}}}, true, nil
	case "function_call_output":
		return []ChatMessage{{Role: "tool", ToolCallID: item.CallID, Content: item.Output}}, true, nil
	case "reasoning":
		return nil, false, nil // not replayable upstream
	default:
		return nil, false, nil
	}
}

// contentToText flattens Responses content parts into a chat string.
func contentToText(raw json.RawMessage, role string) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw)
	}
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += " "
		}
		out += part.Text
	}
	return out
}

// ReasoningItem builds the canonical reasoning item for a chain of thought.
// The text rides in Summary because that is the part the gateway maps onto
// response.reasoning_summary_text.delta and persists for continuation.
func ReasoningItem(text string) pluginapi.Item {
	return pluginapi.Item{
		Type:    "reasoning",
		Status:  "completed",
		Summary: []pluginapi.SummaryPart{{Type: "summary_text", Text: text}},
	}
}

// ChatResponseToResponses converts a non-streaming chat response.
func ChatResponseToResponses(resp *ChatResponse) (*pluginapi.Response, error) {
	return ChatResponseToResponsesWithOptions(resp, ChatConvertOptions{})
}

// ChatResponseToResponsesWithOptions is ChatResponseToResponses with the opt-in
// behaviours enabled: KeepReasoningContent turns the upstream chain of thought
// into a leading reasoning item (the gateway renders it as reasoning summary
// events and persists it for continuation).
func ChatResponseToResponsesWithOptions(resp *ChatResponse, opts ChatConvertOptions) (*pluginapi.Response, error) {
	if resp == nil {
		return nil, fmt.Errorf("providerkit: nil chat response")
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("providerkit: upstream error: %s", resp.Error.Message)
	}
	out := &pluginapi.Response{Status: "completed"}
	if len(resp.Choices) == 0 {
		return out, nil
	}
	choice := resp.Choices[0]

	if opts.KeepReasoningContent {
		if reasoning := strings.TrimSpace(choice.Message.ReasoningContent); reasoning != "" {
			out.Items = append(out.Items, ReasoningItem(reasoning))
		}
	}
	if choice.Message.Content != "" {
		content, err := json.Marshal([]map[string]string{{"type": "output_text", "text": choice.Message.Content}})
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, pluginapi.Item{
			Type: "message", Role: "assistant", Content: content, Status: "completed",
		})
	}
	for _, call := range choice.Message.ToolCalls {
		id := call.ID
		if id == "" {
			id = "fc_" + call.Function.Name
		}
		out.Items = append(out.Items, pluginapi.Item{
			Type: "function_call", ID: id, CallID: id,
			Name: call.Function.Name, Arguments: call.Function.Arguments, Status: "completed",
		})
	}
	if choice.FinishReason != "" {
		out.Status = mapFinishReason(choice.FinishReason)
		// Status collapses every reason into completed|incomplete; the host needs the
		// raw value to say why an answer was cut short (token limit vs content filter).
		out.FinishReason = choice.FinishReason
	}
	out.Usage = ChatUsageToDimensions(resp.Usage)
	return out, nil
}

// ChatUsageToDimensions maps upstream usage onto the gateway's dimension table.
func ChatUsageToDimensions(usage *ChatUsage) pluginapi.Usage {
	dims := map[string]int64{}
	if usage == nil {
		return pluginapi.Usage{Dimensions: dims}
	}
	hit := usage.PromptCacheHitTokens
	miss := usage.PromptCacheMissTokens
	if hit > 0 || miss > 0 {
		dims["input_cache_hit"] = hit
		dims["input_cache_miss"] = miss
	} else {
		dims["input"] = usage.PromptTokens
	}
	dims["output"] = usage.CompletionTokens
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		dims["reasoning"] = usage.CompletionTokensDetails.ReasoningTokens
	}
	return pluginapi.Usage{Dimensions: dims}
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop", "tool_calls", "function_call":
		return "completed"
	case "length", "content_filter":
		// Both mean the answer is not the whole answer: truncated at the token
		// limit, or cut by the upstream's content filter.
		return "incomplete"
	case "":
		// A response without a finish reason is complete as far as the upstream said.
		return "completed"
	default:
		return reason
	}
}

// UpstreamFailure reports whether a finish reason means the upstream failed to
// produce the answer (as opposed to the model finishing it). DeepSeek reports
// `insufficient_system_resource` when it cannot allocate capacity and `aborted`
// when generation was cut short; both must fail over rather than look complete.
func UpstreamFailure(reason string) bool {
	switch reason {
	case "insufficient_system_resource", "aborted":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Streaming translation
// ---------------------------------------------------------------------------

// ChatStreamState accumulates tool-call argument fragments across chunks.
type ChatStreamState struct {
	// ReasoningText accumulates chain-of-thought deltas when the upstream exposes them.
	ReasoningText string
	// FinishReason is the last finish reason seen.
	FinishReason string
	// Usage is the final usage block, when the upstream reports it.
	Usage *ChatUsage

	toolIndex  map[int]bool
	toolName   map[int]string
	toolCallID map[int]string
}

// NewChatStreamState creates an empty stream state.
func NewChatStreamState() *ChatStreamState {
	return &ChatStreamState{
		toolIndex:  map[int]bool{},
		toolName:   map[int]string{},
		toolCallID: map[int]string{},
	}
}

// Translate converts one streaming chat chunk into pluginapi events.
// It returns true when the chunk carries a finish reason (stream completed).
func (s *ChatStreamState) Translate(chunk *ChatResponse, emit func(pluginapi.Event) error) (bool, error) {
	if chunk == nil {
		return false, nil
	}
	if chunk.Error != nil {
		return false, fmt.Errorf("providerkit: upstream stream error: %s", chunk.Error.Message)
	}
	if chunk.Usage != nil {
		s.Usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return false, nil
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		s.FinishReason = choice.FinishReason
	}

	delta := choice.Delta
	// Upstreams that think out loud send the chain of thought before the answer.
	if delta.ReasoningContent != "" {
		s.ReasoningText += delta.ReasoningContent
		if err := emit(pluginapi.Event{Type: pluginapi.EventReasoningDelta, Text: delta.ReasoningContent, Index: 0}); err != nil {
			return false, err
		}
	}
	if delta.Content != "" {
		if err := emit(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: delta.Content, Index: 0}); err != nil {
			return false, err
		}
	}
	if delta.Refusal != "" {
		if err := emit(pluginapi.Event{Type: pluginapi.EventRefusalDelta, Text: delta.Refusal, Index: 0}); err != nil {
			return false, err
		}
	}
	for _, call := range delta.ToolCalls {
		// Only the first chunk of a call carries the id and name; the argument
		// fragments that follow must reuse them or the two halves cannot be paired.
		if !s.toolIndex[call.Index] {
			s.toolIndex[call.Index] = true
			s.toolName[call.Index] = call.Function.Name
			s.toolCallID[call.Index] = call.ID
			if err := emit(pluginapi.Event{
				Type: pluginapi.EventToolCallStart, Index: call.Index,
				CallID: call.ID, ItemID: call.ID, Name: call.Function.Name,
			}); err != nil {
				return false, err
			}
		}
		if call.ID != "" {
			s.toolCallID[call.Index] = call.ID
		}
		if call.Function.Name != "" {
			s.toolName[call.Index] = call.Function.Name
		}
		if call.Function.Arguments != "" {
			if err := emit(pluginapi.Event{
				Type: pluginapi.EventToolArgsDelta, Index: call.Index,
				CallID: s.toolCallID[call.Index], ItemID: s.toolCallID[call.Index],
				Name: s.toolName[call.Index], Text: call.Function.Arguments,
			}); err != nil {
				return false, err
			}
		}
	}
	return choice.FinishReason != "", nil
}

// requireReasoningKeys puts the reasoning_content key on every assistant message of a
// conversation that contains a tool-calling turn, whether or not it has a chain of
// thought to carry.
//
// The upstream validates the request turn by turn, and one tool-calling turn spans the
// whole assistant side of it: DeepSeek rejects `[user, text(no key), tool turn(key),
// tool result]` as well as an assistant message that carries tool_calls without the
// key — "The `reasoning_content` in the thinking mode must be passed back to the API."
// An assistant message that follows an answered tool call belongs to that same turn
// (the model announces the call, then narrates what it found), so it needs the key too.
// The folds above cover the text that precedes a call; this covers what follows it, and
// anything else the caller's item order produced — the reported failure was exactly the
// "after the tool result" shape, which the earlier tool-call-only rule missed.
//
// Messages that already carry the key are left alone: the guard is the key's presence,
// so replayed text is never overwritten with an empty value.
func requireReasoningKeys(messages []ChatMessage) {
	for i := range messages {
		if messages[i].Role != "assistant" {
			continue
		}
		if !hasToolTurnBefore(messages, i) {
			// Only a conversation that reaches a tool call carries the requirement; a
			// plain chat keeps the field off the wire entirely.
			continue
		}
		messages[i].ReasoningRequired = true
	}
}

// hasToolTurnBefore reports whether an assistant tool call appears before messages[i].
func hasToolTurnBefore(messages []ChatMessage, i int) bool {
	for j := 0; j < i; j++ {
		if messages[j].Role == "assistant" && len(messages[j].ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// foldAssistantTextIntoCall merges the plain assistant messages that directly precede a
// tool-calling message into it, so one assistant turn travels as ONE chat message:
// content, reasoning_content and tool_calls together.
//
// A Responses client sends the assistant's text and its tool calls as separate items
// (DSH does), which the translation turns into two assistant messages. That shape is
// what a thinking-mode upstream validates as one turn: with only the tool-calling
// message carrying reasoning_content, DeepSeek still rejects the request
// ("The `reasoning_content` in the thinking mode must be passed back to the API."),
// because the text that announces the call belongs to the same turn and has none.
// Folding reproduces what the upstream itself would have produced, instead of leaving
// it to accept a conversation its own API never generates.
//
// It returns the possibly shortened message list; ok is false when there is nothing to
// fold (no preceding text-only assistant message).
func foldAssistantTextIntoCall(messages []ChatMessage, msg []ChatMessage) ([]ChatMessage, bool) {
	if len(msg) != 1 || msg[0].Role != "assistant" || len(msg[0].ToolCalls) == 0 {
		return messages, false
	}
	start := len(messages)
	for start > 0 {
		prev := messages[start-1]
		if prev.Role != "assistant" || len(prev.ToolCalls) > 0 {
			break
		}
		start--
	}
	if start == len(messages) {
		return messages, false
	}
	parts := make([]string, 0, len(messages)-start+1)
	for _, prev := range messages[start:] {
		if strings.TrimSpace(prev.Content) != "" {
			parts = append(parts, prev.Content)
		}
	}
	if strings.TrimSpace(msg[0].Content) != "" {
		parts = append(parts, msg[0].Content)
	}
	msg[0].Content = strings.Join(parts, "\n")
	// Cap the slice so the append cannot write into the caller's array.
	return append(messages[:start:start], msg[0]), true
}

// mergeParallelToolCalls folds msg into the previous assistant tool-call message when both
// carry tool calls.
//
// Chat Completions ties an assistant message's tool_calls to the tool messages that directly
// follow it, so parallel calls have to travel in ONE assistant message. Emitting one
// assistant message per function_call item leaves every earlier one with unmatched
// tool_calls, and upstreams reject the whole request
// (DeepSeek: "An assistant message with 'tool_calls' must be followed by tool messages
// responding to each 'tool_call_id'").
func mergeParallelToolCalls(messages []ChatMessage, msg []ChatMessage) bool {
	if len(messages) == 0 || len(msg) != 1 {
		return false
	}
	if msg[0].Role != "assistant" || len(msg[0].ToolCalls) == 0 {
		return false
	}
	last := &messages[len(messages)-1]
	if last.Role != "assistant" || len(last.ToolCalls) == 0 {
		return false
	}
	last.ToolCalls = append(last.ToolCalls, msg[0].ToolCalls...)
	if last.ReasoningContent == "" {
		last.ReasoningContent = msg[0].ReasoningContent
	}
	return true
}

// repairToolSequences enforces the same rule from both sides, because the upstream applies it
// to the finished list: every announced tool_call must be answered by the tool message that
// follows, and a tool message must answer a call announced directly before it.
//
// The repairs cover input shapes a client cannot avoid producing: an interrupted turn (a call
// whose output never arrived) and a trimmed history (an output whose call was cut off). Both
// would otherwise be rejected outright, taking the whole request down with them.
func repairToolSequences(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			if msg.Role == "tool" {
				continue // orphan: no assistant tool_calls directly before it
			}
			out = append(out, msg)
			continue
		}
		var answers []ChatMessage
		byCallID := map[string]ChatMessage{}
		for j := i + 1; j < len(messages) && messages[j].Role == "tool"; j++ {
			answers = append(answers, messages[j])
			byCallID[messages[j].ToolCallID] = messages[j]
			i = j
		}
		keptCalls := make([]ChatToolCall, 0, len(msg.ToolCalls))
		keptAnswers := make([]ChatMessage, 0, len(answers))
		seen := map[string]bool{}
		for _, call := range msg.ToolCalls {
			answer, ok := byCallID[call.ID]
			if !ok || seen[call.ID] {
				continue // never answered (or an upstream-invalid duplicate): the call cannot stay
			}
			seen[call.ID] = true
			keptCalls = append(keptCalls, call)
			keptAnswers = append(keptAnswers, answer)
		}
		if len(keptCalls) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue // nothing left to say and no call left to answer
		}
		msg.ToolCalls = keptCalls
		out = append(out, msg)
		out = append(out, keptAnswers...)
	}
	return out
}

// EventsToChatDelta is the inverse of Translate: it converts canonical events into
// chat-completions shaped deltas (used by tests and by gateways exposing chat upstreams).
func EventsToChatDelta(ev pluginapi.Event) (ChatMessage, bool) {
	switch ev.Type {
	case pluginapi.EventTextDelta:
		return ChatMessage{Role: "assistant", Content: ev.Text}, true
	case pluginapi.EventRefusalDelta:
		return ChatMessage{Role: "assistant", Refusal: ev.Text}, true
	case pluginapi.EventToolCallStart:
		call := ChatToolCall{Index: ev.Index, ID: ev.CallID, Type: "function"}
		call.Function.Name = ev.Name
		return ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{call}}, true
	case pluginapi.EventToolArgsDelta:
		call := ChatToolCall{Index: ev.Index, ID: ev.CallID, Type: "function"}
		call.Function.Arguments = ev.Text
		return ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{call}}, true
	default:
		return ChatMessage{}, false
	}
}

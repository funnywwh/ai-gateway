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
	// Reasoning replay is only defined for tool-calling turns: an upstream that
	// requires its own chain of thought back rejects a request that drops it, and
	// only tool-calling turns carry that requirement.
	replay := map[int]string{}
	if opts.ReplayReasoningContent && hasToolCallItems(req.Input) {
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
		if text := replay[index]; text != "" {
			for i := range msg {
				if msg[i].Role == "assistant" && len(msg[i].ToolCalls) > 0 {
					msg[i].ReasoningContent = text
					break
				}
			}
		}
		out.Messages = append(out.Messages, msg...)
	}
	for _, tool := range req.Tools {
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

// itemReasoningText concatenates the summary parts of a reasoning item.
func itemReasoningText(item pluginapi.Item) string {
	var sb strings.Builder
	for _, part := range item.Summary {
		sb.WriteString(part.Text)
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

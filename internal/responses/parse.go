package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// MinMaxOutputTokens is the documented lower bound of max_output_tokens.
const MinMaxOutputTokens = 16

// knownFields lists the top-level fields the gateway understands; anything else is
// preserved in Request.Extra and forwarded verbatim (forward compatibility).
var knownFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "max_output_tokens": true,
	"temperature": true, "top_p": true, "stream": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "previous_response_id": true, "store": true, "metadata": true,
	"reasoning": true, "text": true, "truncation": true, "user": true, "include": true,
	"service_tier": true, "safety_identifier": true, "prompt_cache_key": true, "background": true,
}

// Parse decodes and validates a request body.
func Parse(raw []byte) (*Request, *domain.APIError) {
	if len(raw) == 0 {
		return nil, domain.ErrInvalidRequest("request body is empty")
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, domain.ErrInvalidRequest("malformed JSON body: " + err.Error())
	}

	// Keep unknown top-level fields for pass-through.
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err == nil {
		extra := map[string]json.RawMessage{}
		for key, value := range all {
			if !knownFields[key] {
				extra[key] = value
			}
		}
		if len(extra) > 0 {
			req.Extra = extra
		}
	}

	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

// Validate enforces the documented request rules.
func (r *Request) Validate() *domain.APIError {
	if strings.TrimSpace(r.Model) == "" {
		return domain.ErrInvalidRequest("model is required").WithParam("model")
	}
	if r.Background != nil && *r.Background {
		return domain.ErrUnsupported("background responses are not supported").WithParam("background")
	}
	if r.MaxOutputTokens != nil && *r.MaxOutputTokens < MinMaxOutputTokens {
		return domain.ErrInvalidRequest(fmt.Sprintf("max_output_tokens must be >= %d", MinMaxOutputTokens)).
			WithParam("max_output_tokens")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return domain.ErrInvalidRequest("temperature must be between 0 and 2").WithParam("temperature")
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		return domain.ErrInvalidRequest("top_p must be between 0 and 1").WithParam("top_p")
	}
	if r.Text != nil {
		if _, err := TextFormatLevel(r.Text.Format); err != nil {
			return err
		}
	}
	for i, tool := range r.Tools {
		// Types this gateway does not model (web_search, namespace, ...) are accepted and
		// forwarded verbatim: which of them an upstream can actually use is a property of
		// that upstream, and the provider layer is where that dialect is known. Rejecting
		// here would fail the whole request over an optional tool — real clients ship such
		// tools by default, and they cannot be asked to change.
		if toolType(tool.Type) != "function" {
			continue
		}
		if tool.Name == "" && (tool.Function == nil || tool.Function.Name == "") {
			return domain.ErrInvalidRequest("function tools require a name").
				WithParam(fmt.Sprintf("tools[%d].name", i))
		}
	}
	if _, err := r.Items(); err != nil {
		return err
	}
	return nil
}

// Items converts Request.Input (a string or an array of items) into canonical items.
func (r *Request) Items() ([]pluginapi.Item, *domain.APIError) {
	raw := strings.TrimSpace(string(r.Input))
	if raw == "" || raw == "null" {
		return nil, domain.ErrInvalidRequest("input must not be empty").WithParam("input")
	}

	if strings.HasPrefix(raw, "[") {
		var items []pluginapi.Item
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			return nil, domain.ErrInvalidRequest("input must be a string or an array of items").WithParam("input")
		}
		if len(items) == 0 {
			return nil, domain.ErrInvalidRequest("input must not be empty").WithParam("input")
		}
		for i := range items {
			if items[i].Type == "" {
				items[i].Type = "message"
			}
			if items[i].Type == "message" && items[i].Role == "" {
				items[i].Role = "user"
			}
		}
		return items, nil
	}

	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		return nil, domain.ErrInvalidRequest("input must be a string or an array of items").WithParam("input")
	}
	if strings.TrimSpace(text) == "" {
		return nil, domain.ErrInvalidRequest("input must not be empty").WithParam("input")
	}
	content, err := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
	if err != nil {
		return nil, domain.ErrInternal(err.Error())
	}
	return []pluginapi.Item{{Type: "message", Role: "user", Content: content}}, nil
}

// ToProviderRequest builds the canonical provider request for the resolved upstream model.
func (r *Request) ToProviderRequest(upstreamModel string) (*pluginapi.Request, *domain.APIError) {
	items, apiErr := r.Items()
	if apiErr != nil {
		return nil, apiErr
	}
	out := &pluginapi.Request{
		Model:             upstreamModel,
		Instructions:      r.Instructions,
		Input:             items,
		MaxOutputTokens:   r.MaxOutputTokens,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		ToolChoice:        r.ToolChoice,
		ParallelToolCalls: r.ParallelToolCalls,
		Metadata:          r.Metadata,
		Stream:            r.Stream,
		Extra:             r.Extra,
	}
	if r.Reasoning != nil {
		out.Reasoning = &pluginapi.Reasoning{Effort: r.Reasoning.Effort, Summary: r.Reasoning.Summary}
	}
	if r.Text != nil {
		// An explicit `text` level is the absence of a structured-output request, so it
		// travels as "no format" rather than as a level every provider would have to
		// re-interpret. The three levels share one vocabulary in this package.
		format := r.Text.Format
		if level, _ := TextFormatLevel(format); level == TextFormatText {
			format = nil
		}
		out.Text = &pluginapi.TextConfig{Format: format}
	}
	for _, tool := range r.Tools {
		if toolType(tool.Type) != "function" {
			// Unmodelled shape: hand the provider the client's own bytes rather than trying
			// to squeeze it into the function fields (which produced a nameless tool). A
			// provider that understands the type can use it as-is; the rest drop it.
			out.Tools = append(out.Tools, pluginapi.Tool{Type: toolType(tool.Type), Raw: tool.Raw})
			continue
		}
		t := pluginapi.Tool{Type: "function", Name: tool.Name, Description: tool.Description,
			Parameters: tool.Parameters, Strict: tool.Strict}
		if tool.Function != nil {
			if t.Name == "" {
				t.Name = tool.Function.Name
			}
			if t.Description == "" {
				t.Description = tool.Function.Description
			}
			if len(t.Parameters) == 0 {
				t.Parameters = tool.Function.Parameters
			}
			if t.Strict == nil {
				t.Strict = tool.Function.Strict
			}
		}
		out.Tools = append(out.Tools, t)
	}
	return out, nil
}

// Stored reports whether the response should be persisted (store defaults to true).
func (r *Request) Stored() bool {
	if r.Store == nil {
		return true
	}
	return *r.Store
}

// toolType reports a tool's effective type; an omitted type means a function tool.
func toolType(raw string) string {
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		return trimmed
	}
	return "function"
}

// HasFunctionTools reports whether the request offers at least one tool the routing layer
// can reason about. Tool types the gateway does not model are not counted: whether an
// upstream can use them is that upstream's business (see Tool.Raw).
func (r *Request) HasFunctionTools() bool {
	for _, tool := range r.Tools {
		if toolType(tool.Type) == "function" {
			return true
		}
	}
	return false
}

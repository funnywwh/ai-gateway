// Package pluginapi is the public SDK for provider plugins.
//
// A plugin is a standalone executable that speaks newline-delimited JSON frames
// over stdin/stdout. Plugin authors implement Provider and call Serve; the host
// side lives in internal/pluginhost.
package pluginapi

import (
	"context"
	"encoding/json"
)

// ProtocolVersion is the wire protocol major version. Breaking changes bump it.
const ProtocolVersion = 1

// Capabilities describes what a plugin can do.
type Capabilities struct {
	Complete        bool     `json:"complete,omitempty"`
	Stream          bool     `json:"stream,omitempty"`
	ListModels      bool     `json:"list_models,omitempty"`
	Health          bool     `json:"health,omitempty"`
	UsageEstimated  bool     `json:"usage_estimated,omitempty"`
	UsageDelta      bool     `json:"usage_delta,omitempty"`
	UsageDimensions bool     `json:"usage_dimensions,omitempty"`
	NeedsLogin      bool     `json:"needs_login,omitempty"`
	Actions         []Action `json:"actions,omitempty"`
}

// Action is an interactive operation exposed by a plugin (for example a device-code login).
type Action struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	ParamsSchema json.RawMessage `json:"params_schema,omitempty"`
}

// Info is the plugin identity reported in the handshake.
type Info struct {
	Name         string
	Version      string
	Capabilities Capabilities
}

// ModelInfo describes one upstream model a plugin can serve.
type ModelInfo struct {
	ID              string          `json:"id"`
	UpstreamModel   string          `json:"upstream_model,omitempty"`
	DisplayName     string          `json:"display_name,omitempty"`
	ContextWindow   int             `json:"context_window,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Capabilities    map[string]bool `json:"capabilities,omitempty"`
	PricingRules    json.RawMessage `json:"pricing_rules,omitempty"`
}

// Usage is the metered quantity of one attempt, expressed as named dimensions:
// input_cache_hit, input_cache_miss, input, output, reasoning, plus provider-specific keys.
type Usage struct {
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	Estimated  bool             `json:"estimated,omitempty"`
}

// Reasoning mirrors the Responses API reasoning parameter.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// TextConfig mirrors the Responses API text.format parameter.
type TextConfig struct {
	Format json.RawMessage `json:"format,omitempty"`
}

// Tool is a function tool exposed to the model.
// Tool is one tool offered to the model.
//
// Only "function" tools are modelled here: the richer Responses types (web_search,
// file_search, computer_use, namespace, ...) have payloads that vary and keep growing.
// Those are carried verbatim in Raw so they survive the plugin protocol and reach a
// provider that may well support them, instead of the core deciding what to discard.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`

	// Raw is the tool exactly as the client sent it, set for types this package does not
	// model. It wins over the structured fields when marshalling.
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON emits Raw verbatim for an unmodelled tool, the structured form otherwise.
func (t Tool) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	type plain Tool // sheds the method set, so this cannot recurse
	return json.Marshal(plain(t))
}

// UnmarshalJSON preserves the original bytes of a tool type this package does not model.
func (t *Tool) UnmarshalJSON(data []byte) error {
	type plain Tool
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = Tool(decoded)
	if t.Type != "" && t.Type != "function" {
		t.Raw = append(json.RawMessage(nil), data...)
	}
	return nil
}

// Item is one element of the canonical input/output list. Unknown fields are preserved in Extra.
type Item struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
	// OutputContent preserves array-valued tool results (text, images, files).
	// When set it takes precedence over the legacy string Output on the wire.
	OutputContent json.RawMessage `json:"-"`
	Status        string          `json:"status,omitempty"`
	// Summary is written by MarshalJSON instead of a struct tag. A reasoning item's
	// summary is a required key upstream and an empty array is a meaningful value ("there
	// was no summary text"), so an empty-but-non-nil slice has to survive the round trip
	// while a nil slice stays absent — a distinction `omitempty` cannot express. Losing it
	// is what turns a valid request into "Missing required parameter:
	// 'input[3].summary'". Assign []SummaryPart{} to force the key onto the wire.
	Summary []SummaryPart              `json:"-"`
	Extra   map[string]json.RawMessage `json:"-"`

	// sent marks the wire keys whose empty value is a legal value (see sentFields).
	sent sentFields
}

var itemJSONFields = map[string]bool{
	"type": true, "id": true, "role": true, "content": true, "call_id": true,
	"name": true, "arguments": true, "output": true, "status": true, "summary": true,
}

// sentFields records which of the fields whose *empty* value is meaningful a decoded
// document carried. "The client sent the key" is information an upstream may be strict
// about: a no-argument tool call sends `"arguments":""` and an empty tool result sends
// `"output":""`, and dropping either produces an item the upstream rejects for a missing
// required parameter. A bitset rather than a map because Item is copied by value in the
// hot path (assembler, request building).
type sentFields uint8

const (
	sentArguments sentFields = 1 << iota
	sentOutput
)

var emptyStringJSON = json.RawMessage(`""`)

func (s sentFields) has(f sentFields) bool { return s&f != 0 }

// emptySentFields reports the sent fields whose current value is empty, i.e. the ones a
// struct tag dropped and MarshalJSON has to put back.
func (i Item) emptySentFields() sentFields {
	var out sentFields
	if i.sent.has(sentArguments) && i.Arguments == "" {
		out |= sentArguments
	}
	// An array-valued result (OutputContent) already carries the key.
	if i.sent.has(sentOutput) && i.Output == "" && len(i.OutputContent) == 0 {
		out |= sentOutput
	}
	return out
}

// MarshalJSON keeps fields from input item types that the canonical protocol does not
// model yet, and keeps the empty values that a struct tag would drop. Some Responses
// clients put provider-specific data inside an input item (for example
// additional_tools.tools), so dropping Extra here changes a valid request into an upstream
// validation error; and some providers require the *key* even when its value is empty
// (a reasoning item's `summary`, a tool item's `arguments`/`output`), so a key the client
// sent is never dropped just because its value is zero.
func (i Item) MarshalJSON() ([]byte, error) {
	type plain Item
	encoded, err := json.Marshal(plain(i))
	if err != nil {
		return nil, err
	}
	restore := i.emptySentFields()
	if i.Summary == nil && len(i.Extra) == 0 && len(i.OutputContent) == 0 && restore == 0 {
		return encoded, err
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if i.Summary != nil {
		summary, err := json.Marshal(i.Summary)
		if err != nil {
			return nil, err
		}
		fields["summary"] = summary
	}
	if len(i.OutputContent) > 0 {
		fields["output"] = i.OutputContent
	}
	for key, value := range i.Extra {
		if itemJSONFields[key] {
			continue
		}
		fields[key] = value
	}
	if restore.has(sentArguments) {
		fields["arguments"] = emptyStringJSON
	}
	if restore.has(sentOutput) {
		fields["output"] = emptyStringJSON
	}
	return json.Marshal(fields)
}

// UnmarshalJSON records fields the canonical item shape does not know about. They are
// emitted again by MarshalJSON so a provider adapter can translate only the fields it
// understands without making newer client item types unusable.
func (i *Item) UnmarshalJSON(data []byte) error {
	type plain Item
	var decoded plain
	// Shadow output and summary during decoding: newer clients return content-part
	// arrays, while existing plugin callers still construct string Output values, and
	// summary is decoded here so that `[]` (an empty but present array) and a missing key
	// stay distinguishable — see the field comment.
	wire := struct {
		*plain
		Output  json.RawMessage `json:"output"`
		Summary []SummaryPart   `json:"summary"`
	}{plain: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	decoded.Summary = wire.Summary
	if len(wire.Output) > 0 {
		if wire.Output[0] == '[' {
			decoded.OutputContent = append(json.RawMessage(nil), wire.Output...)
		} else if err := json.Unmarshal(wire.Output, &decoded.Output); err != nil {
			return err
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	extra := make(map[string]json.RawMessage)
	for key, value := range fields {
		if !itemJSONFields[key] {
			extra[key] = append(json.RawMessage(nil), value...)
		}
	}
	// "summary" needs no marker: decoding `[]` yields an empty non-nil slice, which
	// MarshalJSON emits again, while a missing (or null) key stays nil and absent.
	if _, ok := fields["arguments"]; ok {
		decoded.sent |= sentArguments
	}
	if _, ok := fields["output"]; ok {
		decoded.sent |= sentOutput
	}
	*i = Item(decoded)
	if len(extra) > 0 {
		i.Extra = extra
	}
	return nil
}

// SummaryPart is one reasoning summary fragment.
type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Request is the canonical provider request (Responses-shaped).
// Unknown fields from the client are passed through in Extra.
type Request struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions,omitempty"`
	Input             []Item            `json:"input,omitempty"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Reasoning         *Reasoning        `json:"reasoning,omitempty"`
	Text              *TextConfig       `json:"text,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	// PromptCacheKey is forwarded verbatim, independently of the gateway's session affinity key.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// Include carries the extra output data a client asked for, verbatim (for example
	// "reasoning.encrypted_content"). It has to reach the upstream: a client replaying a
	// reasoning item from an earlier turn needs the encrypted blob that include asked for,
	// and without it a stateless upstream answers "Item with id 'rs_…' not found. Items are
	// not persisted when `store` is set to false."
	Include []string                   `json:"include,omitempty"`
	Stream  bool                       `json:"stream,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

// Event is a streaming increment emitted by a plugin.
//
// Event types: text.delta, reasoning.delta, refusal.delta, tool_call.start,
// tool_call.arguments.delta, output_item.done, usage, usage.delta, finish.
//
// finish is the terminal event: Reason carries the upstream finish reason
// verbatim ("stop", "length", "content_filter", ...). The host consumes it — it
// never reaches the client as an event, it becomes the stream's end frame, and
// when the reason says the answer was cut short the response ends as
// `response.incomplete` instead of `response.completed`. Emitting it is how a
// provider distinguishes "the model finished" from "the answer was truncated";
// an upstream that dies mid-answer must NOT end the stream silently, or the
// half-sentence is served as a complete answer.
type Event struct {
	Type string `json:"type"`
	// Item carries a completed output item not represented by delta events.
	Item      *Item  `json:"item,omitempty"`
	Index     int    `json:"index,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Text      string `json:"text,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	Estimated bool   `json:"estimated,omitempty"`
	// Reason is the terminal finish reason carried by a finish event.
	Reason string `json:"reason,omitempty"`
}

// Event type constants.
const (
	EventOutputItemDone = "output_item.done"
	EventTextDelta      = "text.delta"
	EventReasoningDelta = "reasoning.delta"
	EventRefusalDelta   = "refusal.delta"
	EventToolCallStart  = "tool_call.start"
	EventToolArgsDelta  = "tool_call.arguments.delta"
	EventUsage          = "usage"
	EventUsageDelta     = "usage.delta"
	EventFinish         = "finish"
)

// Response is the canonical non-streaming provider response.
type Response struct {
	Items  []Item `json:"items"`
	Usage  Usage  `json:"usage"`
	Status string `json:"status,omitempty"`
	// FinishReason is the upstream's own reason for stopping ("stop", "length",
	// "content_filter", ...). Status collapses those into completed|incomplete,
	// which loses the distinction between "hit the token limit" and "cut by the
	// content filter" — the host needs the raw value to report why.
	FinishReason string `json:"finish_reason,omitempty"`
}

// Provider is implemented by plugin authors.
type Provider interface {
	// Info reports identity; Serve sends it in the handshake.
	Info() Info
	// ListModels returns the upstream models this plugin can serve (optional).
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// Complete performs a non-streaming call.
	Complete(ctx context.Context, req *Request) (*Response, error)
	// Stream performs a streaming call; emit blocks under backpressure.
	// Cancelling ctx must abort the upstream request.
	Stream(ctx context.Context, req *Request, emit func(Event) error) error
	// Health probes upstream reachability (optional).
	Health(ctx context.Context) error
	// Actions lists interactive operations (optional).
	Actions() []Action
	// RunAction executes one interactive operation (optional).
	RunAction(ctx context.Context, name string, in json.RawMessage) (json.RawMessage, error)
	// StateDir is the writable per-provider directory handed to the plugin.
	StateDir() string
	// SetCredentials receives credentials pushed by the host (initial + rotations).
	SetCredentials(creds map[string]string)
}

// SchemaProvider is optionally implemented by plugins that describe their own
// configuration form (rendered generically by the admin UI).
type SchemaProvider interface {
	ConfigSchema() json.RawMessage
	CredentialsSchema() json.RawMessage
}

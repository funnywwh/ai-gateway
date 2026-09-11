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
	Type      string                     `json:"type"`
	ID        string                     `json:"id,omitempty"`
	Role      string                     `json:"role,omitempty"`
	Content   json.RawMessage            `json:"content,omitempty"`
	CallID    string                     `json:"call_id,omitempty"`
	Name      string                     `json:"name,omitempty"`
	Arguments string                     `json:"arguments,omitempty"`
	Output    string                     `json:"output,omitempty"`
	Status    string                     `json:"status,omitempty"`
	Summary   []SummaryPart              `json:"summary,omitempty"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// SummaryPart is one reasoning summary fragment.
type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Request is the canonical provider request (Responses-shaped).
// Unknown fields from the client are passed through in Extra.
type Request struct {
	Model             string                     `json:"model"`
	Instructions      string                     `json:"instructions,omitempty"`
	Input             []Item                     `json:"input,omitempty"`
	Tools             []Tool                     `json:"tools,omitempty"`
	ToolChoice        json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                      `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   *int                       `json:"max_output_tokens,omitempty"`
	Temperature       *float64                   `json:"temperature,omitempty"`
	TopP              *float64                   `json:"top_p,omitempty"`
	Reasoning         *Reasoning                 `json:"reasoning,omitempty"`
	Text              *TextConfig                `json:"text,omitempty"`
	Metadata          map[string]string          `json:"metadata,omitempty"`
	Stream            bool                       `json:"stream,omitempty"`
	Extra             map[string]json.RawMessage `json:"-"`
}

// Event is a streaming increment emitted by a plugin.
//
// Event types: text.delta, reasoning.delta, refusal.delta, tool_call.start,
// tool_call.arguments.delta, usage, usage.delta.
type Event struct {
	Type      string `json:"type"`
	Index     int    `json:"index,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Text      string `json:"text,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	Estimated bool   `json:"estimated,omitempty"`
}

// Event type constants.
const (
	EventTextDelta      = "text.delta"
	EventReasoningDelta = "reasoning.delta"
	EventRefusalDelta   = "refusal.delta"
	EventToolCallStart  = "tool_call.start"
	EventToolArgsDelta  = "tool_call.arguments.delta"
	EventUsage          = "usage"
	EventUsageDelta     = "usage.delta"
)

// Response is the canonical non-streaming provider response.
type Response struct {
	Items  []Item `json:"items"`
	Usage  Usage  `json:"usage"`
	Status string `json:"status,omitempty"`
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

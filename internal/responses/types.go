// Package responses implements the OpenAI Responses API surface: request parsing and
// validation, the response assembler shared by the streaming and non-streaming paths,
// and the wire types of the protocol.
package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Request is the accepted subset of POST /v1/responses.
type Request struct {
	SessionHeaders     SessionHeaders    `json:"-"`
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	Tools              []Tool            `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Reasoning          *Reasoning        `json:"reasoning,omitempty"`
	Text               *TextConfig       `json:"text,omitempty"`
	Truncation         string            `json:"truncation,omitempty"`
	User               string            `json:"user,omitempty"`
	Include            []string          `json:"include,omitempty"`
	ServiceTier        string            `json:"service_tier,omitempty"`
	SafetyIdentifier   string            `json:"safety_identifier,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
	Background         *bool             `json:"background,omitempty"`
	// Extra keeps unrecognised fields so they can be forwarded verbatim.
	Extra map[string]json.RawMessage `json:"-"`
}

// Tool is a function tool as accepted by the Responses API.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
	// Function supports the nested form some clients still send.
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"function,omitempty"`

	// Raw is the tool exactly as the client sent it, set for the types this struct does
	// not model (web_search, namespace, ...). The provider layer forwards or translates
	// it: which tool types an upstream understands is a property of that upstream, not of
	// the client-facing surface, so the core keeps the shape intact instead of judging it.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON preserves the original bytes of an unmodelled tool type.
func (t *Tool) UnmarshalJSON(data []byte) error {
	type plain Tool
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = Tool(decoded)
	if toolType(t.Type) != "function" {
		t.Raw = append(json.RawMessage(nil), data...)
	}
	return nil
}

// Reasoning mirrors the reasoning parameter.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// TextConfig mirrors the text parameter.
type TextConfig struct {
	Format json.RawMessage `json:"format,omitempty"`
}

// text.format levels. They are the only three the Responses API defines, and the
// provider layer needs to recognise them by name to decide what (if anything) to
// forward upstream — an upstream that is handed a level it cannot serve answers 400
// at best and silently ignores the client's structured-output request at worst.
const (
	TextFormatText       = "text"
	TextFormatJSONObject = "json_object"
	TextFormatJSONSchema = "json_schema"
)

// TextFormatLevel returns the level named by text.format, "" when the client asked for
// nothing, and an error when the client asked for something unrecognisable.
//
// A malformed level is rejected here rather than ignored downstream: dropping it would
// turn a request for structured output into a plain-text answer, and the client would
// only find out by failing to parse what it got back.
func TextFormatLevel(raw json.RawMessage) (string, *domain.APIError) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return "", domain.ErrInvalidRequest("text.format must be an object").
			WithParam("text.format")
	}
	switch format.Type {
	case "":
		// `{"format":{}}` names nothing; read it as the default rather than guessing.
		return "", nil
	case TextFormatText, TextFormatJSONObject, TextFormatJSONSchema:
		return format.Type, nil
	default:
		return "", domain.ErrInvalidRequest(
			fmt.Sprintf("text.format.type must be text|json_object|json_schema, got %q", format.Type)).
			WithParam("text.format.type")
	}
}

// Response is the OpenAI response object.
type Response struct {
	ID                 string             `json:"id"`
	Object             string             `json:"object"`
	CreatedAt          int64              `json:"created_at"`
	Status             string             `json:"status"`
	Model              string             `json:"model"`
	Output             []OutputItem       `json:"output"`
	Usage              *Usage             `json:"usage,omitempty"`
	Instructions       string             `json:"instructions,omitempty"`
	Metadata           map[string]string  `json:"metadata,omitempty"`
	Error              *ErrorPayload      `json:"error,omitempty"`
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details,omitempty"`
	PreviousResponseID string             `json:"previous_response_id,omitempty"`
}

// OutputItem is one entry of Response.Output.
type OutputItem struct {
	// Raw preserves newer Responses output item types through SSE and storage.
	Raw       *pluginapi.Item `json:"-"`
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Status    string          `json:"status,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   []ContentPart   `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Summary   []ContentPart   `json:"summary,omitempty"`
}

func (i OutputItem) MarshalJSON() ([]byte, error) {
	if i.Raw != nil {
		return json.Marshal(i.Raw)
	}
	type plain OutputItem
	return json.Marshal(plain(i))
}

func (i *OutputItem) UnmarshalJSON(data []byte) error {
	type plain OutputItem
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = OutputItem(decoded)
	if i.Type != "message" && i.Type != "reasoning" && i.Type != "function_call" {
		var raw pluginapi.Item
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		i.Raw = &raw
	}
	return nil
}

// ContentPart is one content fragment of a message or reasoning item.
type ContentPart struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Annotations []any  `json:"annotations,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	InputTokens         int64                `json:"input_tokens"`
	OutputTokens        int64                `json:"output_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// InputTokensDetails breaks input tokens down by cache status.
type InputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// OutputTokensDetails breaks output tokens down by kind.
type OutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ErrorPayload is the error object embedded in a failed response.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
}

// IncompleteDetails explains why a response stopped early.
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// Event is one SSE event of the streaming protocol.
type Event struct {
	Type           string        `json:"type"`
	SequenceNumber int           `json:"sequence_number"`
	Response       *Response     `json:"response,omitempty"`
	Item           *OutputItem   `json:"item,omitempty"`
	ItemID         string        `json:"item_id,omitempty"`
	OutputIndex    int           `json:"output_index,omitempty"`
	ContentIndex   int           `json:"content_index,omitempty"`
	SummaryIndex   int           `json:"summary_index,omitempty"`
	Part           *ContentPart  `json:"part,omitempty"`
	Delta          string        `json:"delta,omitempty"`
	Text           string        `json:"text,omitempty"`
	Arguments      string        `json:"arguments,omitempty"`
	Error          *ErrorPayload `json:"error,omitempty"`
}

// itemScoped are the events the protocol scopes to one output item: they carry
// output_index, and the first item's index is 0.
var itemScoped = map[string]bool{
	EventOutputItemAdded:        true,
	EventOutputItemDone:         true,
	EventContentPartAdded:       true,
	EventContentPartDone:        true,
	EventOutputTextDelta:        true,
	EventOutputTextDone:         true,
	EventRefusalDelta:           true,
	EventRefusalDone:            true,
	EventFunctionArgsDelta:      true,
	EventFunctionArgsDone:       true,
	EventReasoningSummaryAdded:  true,
	EventReasoningSummaryDelta:  true,
	EventReasoningSummaryDone:   true,
	EventReasoningSummaryClosed: true,
}

// contentScoped are the item-scoped events that additionally address a content part.
var contentScoped = map[string]bool{
	EventContentPartAdded: true,
	EventContentPartDone:  true,
	EventOutputTextDelta:  true,
	EventOutputTextDone:   true,
	EventRefusalDelta:     true,
	EventRefusalDone:      true,
}

// summaryScoped are the reasoning events that address a summary part.
var summaryScoped = map[string]bool{
	EventReasoningSummaryAdded:  true,
	EventReasoningSummaryDelta:  true,
	EventReasoningSummaryDone:   true,
	EventReasoningSummaryClosed: true,
}

// MarshalJSON writes an event with the index fields its type requires.
//
// The struct tags are omitempty because the fields are meaningless on
// response.created/completed/error, but that also dropped them when they were 0 —
// i.e. for the first item of every response. Clients key their item table by
// output_index (Codex reports "OutputTextDelta without active item"; pi-ai, which
// DSH uses, looks the slot up by output_index and silently discards a delta it
// cannot place), so the protocol's "always present" fields are decided by event
// type here rather than by their value.
func (e Event) MarshalJSON() ([]byte, error) {
	type wireEvent struct {
		Type           string        `json:"type"`
		SequenceNumber int           `json:"sequence_number"`
		Response       *Response     `json:"response,omitempty"`
		Item           *OutputItem   `json:"item,omitempty"`
		ItemID         string        `json:"item_id,omitempty"`
		OutputIndex    *int          `json:"output_index,omitempty"`
		ContentIndex   *int          `json:"content_index,omitempty"`
		SummaryIndex   *int          `json:"summary_index,omitempty"`
		Part           *ContentPart  `json:"part,omitempty"`
		Delta          string        `json:"delta,omitempty"`
		Text           string        `json:"text,omitempty"`
		Arguments      string        `json:"arguments,omitempty"`
		Error          *ErrorPayload `json:"error,omitempty"`
	}
	out := wireEvent{
		Type:           e.Type,
		SequenceNumber: e.SequenceNumber,
		Response:       e.Response,
		Item:           e.Item,
		ItemID:         e.ItemID,
		Part:           e.Part,
		Delta:          e.Delta,
		Text:           e.Text,
		Arguments:      e.Arguments,
		Error:          e.Error,
	}
	if itemScoped[e.Type] {
		index := e.OutputIndex
		out.OutputIndex = &index
	}
	if contentScoped[e.Type] {
		index := e.ContentIndex
		out.ContentIndex = &index
	}
	if summaryScoped[e.Type] {
		index := e.SummaryIndex
		out.SummaryIndex = &index
	}
	return json.Marshal(out)
}

// Event type names (mirrors the Responses API streaming protocol).
const (
	EventCreated                = "response.created"
	EventInProgress             = "response.in_progress"
	EventOutputItemAdded        = "response.output_item.added"
	EventOutputItemDone         = "response.output_item.done"
	EventContentPartAdded       = "response.content_part.added"
	EventContentPartDone        = "response.content_part.done"
	EventOutputTextDelta        = "response.output_text.delta"
	EventOutputTextDone         = "response.output_text.done"
	EventRefusalDelta           = "response.refusal.delta"
	EventRefusalDone            = "response.refusal.done"
	EventReasoningSummaryAdded  = "response.reasoning_summary_part.added"
	EventReasoningSummaryDelta  = "response.reasoning_summary_text.delta"
	EventReasoningSummaryDone   = "response.reasoning_summary_text.done"
	EventReasoningSummaryClosed = "response.reasoning_summary_part.done"
	EventFunctionArgsDelta      = "response.function_call_arguments.delta"
	EventFunctionArgsDone       = "response.function_call_arguments.done"
	EventCompleted              = "response.completed"
	EventFailed                 = "response.failed"
	EventIncomplete             = "response.incomplete"
	EventError                  = "error"
)

// Model is the object returned by GET /v1/models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Pricing is the gateway extension: sale prices only, never cost.
	Pricing *ModelPricing `json:"x-gateway-pricing,omitempty"`
}

// ModelPricing describes the sale price of a model.
type ModelPricing struct {
	Currency            string `json:"currency"`
	InputMicrosPerMTok  int64  `json:"input_micros_per_mtok,omitempty"`
	OutputMicrosPerMTok int64  `json:"output_micros_per_mtok,omitempty"`
	Basis               string `json:"basis,omitempty"`
	MarkupBP            int    `json:"markup_bp,omitempty"`
}

// ModelList is the response of GET /v1/models.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

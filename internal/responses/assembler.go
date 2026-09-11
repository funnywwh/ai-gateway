package responses

import (
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Assembler translates canonical provider events into Responses API events and builds
// the final response object. The non-streaming path uses the same assembler with a
// no-op emitter, which guarantees GET /v1/responses/{id} matches the streamed output.
type Assembler struct {
	id        string
	model     string
	createdAt int64
	emit      func(*Event) error
	seq       int

	output     []OutputItem
	open       *openItem
	status     string
	errPayload *ErrorPayload
	incomplete *IncompleteDetails

	usage    pluginapi.Usage
	hasUsage bool

	reasoningText string
	text          string
	refusalText   string
	deltas        int
}

type openItem struct {
	index     int
	item      *OutputItem
	partIndex int
}

// NewAssembler creates an assembler. emit may be nil (non-streaming use).
func NewAssembler(model string, emit func(*Event) error) *Assembler {
	if emit == nil {
		emit = func(*Event) error { return nil }
	}
	return &Assembler{
		id:        ids.Response(),
		model:     model,
		createdAt: time.Now().UTC().Unix(),
		emit:      emit,
		status:    "in_progress",
		open:      nil,
	}
}

// ID returns the response id (resp_…).
func (a *Assembler) ID() string { return a.id }

// Status returns the current response status.
func (a *Assembler) Status() string { return a.status }

// SetID overrides the response id (used when replaying a stored response).
func (a *Assembler) SetID(id string) { a.id = id }

// Deltas reports how many content deltas were emitted (the "first byte" signal that
// forbids provider failover).
func (a *Assembler) Deltas() int { return a.deltas }

// Usage returns the accumulated canonical usage.
func (a *Assembler) Usage() pluginapi.Usage { return a.usage }

// Text returns the accumulated final output text.
func (a *Assembler) Text() string { return a.text }

// Reasoning returns the accumulated thinking text.
func (a *Assembler) Reasoning() string { return a.reasoningText }

// Response returns the current response snapshot.
func (a *Assembler) Response() *Response { return a.build() }

// Items returns the canonical output items (stored for continuation).
func (a *Assembler) Items() []pluginapi.Item {
	items := make([]pluginapi.Item, 0, len(a.output))
	for i := range a.output {
		item := &a.output[i]
		switch item.Type {
		case "message":
			content := ""
			if len(item.Content) > 0 {
				content = item.Content[0].Text
			}
			raw := []byte("[]")
			if content != "" {
				partType := "output_text"
				if len(item.Content) > 0 && item.Content[0].Type != "" {
					partType = item.Content[0].Type
				}
				encoded, err := jsonMarshal([]map[string]string{{"type": partType, "text": content}})
				if err == nil {
					raw = encoded
				}
			}
			items = append(items, pluginapi.Item{Type: "message", ID: item.ID, Role: item.Role, Content: raw, Status: item.Status})
		case "function_call":
			items = append(items, pluginapi.Item{
				Type: "function_call", ID: item.ID, CallID: item.CallID,
				Name: item.Name, Arguments: item.Arguments, Status: item.Status,
			})
		case "reasoning":
			summary := make([]pluginapi.SummaryPart, 0, len(item.Summary))
			for _, part := range item.Summary {
				summary = append(summary, pluginapi.SummaryPart{Type: part.Type, Text: part.Text})
			}
			items = append(items, pluginapi.Item{Type: "reasoning", ID: item.ID, Summary: summary, Status: item.Status})
		}
	}
	return items
}

// Start emits response.created and response.in_progress.
func (a *Assembler) Start() error {
	if err := a.send(&Event{Type: EventCreated, Response: a.build()}); err != nil {
		return err
	}
	return a.send(&Event{Type: EventInProgress, Response: a.build()})
}

// Add consumes one canonical provider event.
func (a *Assembler) Add(ev pluginapi.Event) error {
	switch ev.Type {
	case pluginapi.EventTextDelta:
		return a.addText(ev.Text)
	case pluginapi.EventReasoningDelta:
		return a.addReasoning(ev.Text)
	case pluginapi.EventRefusalDelta:
		return a.addRefusal(ev.Text)
	case pluginapi.EventToolCallStart:
		return a.startFunctionCall(ev)
	case pluginapi.EventToolArgsDelta:
		return a.addFunctionArguments(ev)
	case pluginapi.EventUsage, pluginapi.EventUsageDelta:
		if ev.Usage != nil {
			if ev.Type == pluginapi.EventUsage {
				a.usage = *ev.Usage
			} else {
				if a.usage.Dimensions == nil {
					a.usage.Dimensions = map[string]int64{}
				}
				for k, v := range ev.Usage.Dimensions {
					a.usage.Dimensions[k] += v
				}
				a.usage.Estimated = true
			}
			a.hasUsage = true
		}
	}
	return nil
}

func (a *Assembler) addText(delta string) error {
	if delta == "" {
		return nil
	}
	item, err := a.ensurePart("message", "output_text")
	if err != nil {
		return err
	}
	a.text += delta
	a.deltas++
	item.Content[0].Text += delta
	return a.send(&Event{
		Type: EventOutputTextDelta, ItemID: item.ID, OutputIndex: a.open.index,
		ContentIndex: 0, Delta: delta,
	})
}

func (a *Assembler) addRefusal(delta string) error {
	if delta == "" {
		return nil
	}
	item, err := a.ensurePart("message", "refusal")
	if err != nil {
		return err
	}
	a.refusalText += delta
	a.deltas++
	item.Content[0].Text += delta
	return a.send(&Event{
		Type: EventRefusalDelta, ItemID: item.ID, OutputIndex: a.open.index,
		ContentIndex: 0, Delta: delta,
	})
}

func (a *Assembler) addReasoning(delta string) error {
	if delta == "" {
		return nil
	}
	if a.open == nil || a.open.item.Type != "reasoning" {
		if err := a.closeOpen(); err != nil {
			return err
		}
		item := OutputItem{
			Type: "reasoning", ID: ids.Reasoning(), Status: "in_progress",
			// The chain of thought is kept twice on purpose: summary drives the
			// reasoning_summary events, content carries the text in the upstream shape
			// (reasoning_text parts) so a stored response can replay it verbatim.
			Content: []ContentPart{{Type: "reasoning_text"}},
			Summary: []ContentPart{{Type: "summary_text"}},
		}
		a.output = append(a.output, item)
		a.open = &openItem{index: len(a.output) - 1, item: &a.output[len(a.output)-1]}
		if err := a.send(&Event{Type: EventOutputItemAdded, OutputIndex: a.open.index, Item: a.open.item}); err != nil {
			return err
		}
		if err := a.send(&Event{Type: EventReasoningSummaryAdded, OutputIndex: a.open.index,
			ItemID: a.open.item.ID, SummaryIndex: 0, Part: &a.open.item.Summary[0]}); err != nil {
			return err
		}
	}
	a.reasoningText += delta
	a.deltas++
	a.open.item.Summary[0].Text += delta
	a.open.item.Content[0].Text += delta
	return a.send(&Event{
		Type: EventReasoningSummaryDelta, ItemID: a.open.item.ID,
		OutputIndex: a.open.index, SummaryIndex: 0, Delta: delta,
	})
}

func (a *Assembler) startFunctionCall(ev pluginapi.Event) error {
	if err := a.closeOpen(); err != nil {
		return err
	}
	id := ev.ItemID
	if id == "" {
		id = ev.CallID
	}
	if id == "" {
		id = ids.FunctionCall()
	}
	callID := ev.CallID
	if callID == "" {
		callID = id
	}
	a.output = append(a.output, OutputItem{
		Type: "function_call", ID: id, CallID: callID, Name: ev.Name, Status: "in_progress",
	})
	a.open = &openItem{index: len(a.output) - 1, item: &a.output[len(a.output)-1]}
	a.deltas++
	return a.send(&Event{Type: EventOutputItemAdded, OutputIndex: a.open.index, Item: a.open.item})
}

func (a *Assembler) addFunctionArguments(ev pluginapi.Event) error {
	if a.open == nil || a.open.item.Type != "function_call" {
		if err := a.startFunctionCall(ev); err != nil {
			return err
		}
	}
	if ev.Text == "" {
		return nil
	}
	a.deltas++
	a.open.item.Arguments += ev.Text
	return a.send(&Event{
		Type: EventFunctionArgsDelta, ItemID: a.open.item.ID,
		OutputIndex: a.open.index, Delta: ev.Text,
	})
}

// ensurePart opens (if needed) a message item with the given content part type.
func (a *Assembler) ensurePart(itemType, partType string) (*OutputItem, error) {
	if a.open != nil && a.open.item.Type == itemType && len(a.open.item.Content) > 0 &&
		a.open.item.Content[0].Type == partType {
		return a.open.item, nil
	}
	if err := a.closeOpen(); err != nil {
		return nil, err
	}
	role := "assistant"
	a.output = append(a.output, OutputItem{
		Type: itemType, ID: ids.Message(), Role: role, Status: "in_progress",
		Content: []ContentPart{{Type: partType}},
	})
	a.open = &openItem{index: len(a.output) - 1, item: &a.output[len(a.output)-1]}
	if err := a.send(&Event{Type: EventOutputItemAdded, OutputIndex: a.open.index, Item: a.open.item}); err != nil {
		return nil, err
	}
	if err := a.send(&Event{
		Type: EventContentPartAdded, ItemID: a.open.item.ID, OutputIndex: a.open.index,
		ContentIndex: 0, Part: &a.open.item.Content[0],
	}); err != nil {
		return nil, err
	}
	return a.open.item, nil
}

// closeOpen finalises the currently open output item.
func (a *Assembler) closeOpen() error {
	if a.open == nil {
		return nil
	}
	item := a.open.item
	item.Status = "completed"

	switch item.Type {
	case "message":
		partType := "output_text"
		text := ""
		if len(item.Content) > 0 {
			partType = item.Content[0].Type
			text = item.Content[0].Text
		}
		doneType := EventOutputTextDone
		if partType == "refusal" {
			doneType = EventRefusalDone
		}
		if err := a.send(&Event{Type: doneType, ItemID: item.ID, OutputIndex: a.open.index,
			ContentIndex: 0, Text: text}); err != nil {
			return err
		}
		if err := a.send(&Event{Type: EventContentPartDone, ItemID: item.ID,
			OutputIndex: a.open.index, ContentIndex: 0, Part: &item.Content[0]}); err != nil {
			return err
		}
	case "reasoning":
		if len(item.Summary) == 0 {
			item.Summary = []ContentPart{{Type: "summary_text"}}
		}
		if err := a.send(&Event{Type: EventReasoningSummaryDone, ItemID: item.ID,
			OutputIndex: a.open.index, SummaryIndex: 0, Text: item.Summary[0].Text}); err != nil {
			return err
		}
		if err := a.send(&Event{Type: EventReasoningSummaryClosed, ItemID: item.ID,
			OutputIndex: a.open.index, SummaryIndex: 0, Part: &item.Summary[0]}); err != nil {
			return err
		}
	case "function_call":
		if err := a.send(&Event{Type: EventFunctionArgsDone, ItemID: item.ID,
			OutputIndex: a.open.index, Arguments: item.Arguments}); err != nil {
			return err
		}
	}

	if err := a.send(&Event{Type: EventOutputItemDone, OutputIndex: a.open.index, Item: item}); err != nil {
		return err
	}
	a.open = nil
	return nil
}

// Complete finalises the response successfully.
func (a *Assembler) Complete(usage pluginapi.Usage) (*Response, error) {
	if usage.Dimensions != nil || len(usage.Dimensions) > 0 {
		a.usage = usage
		a.hasUsage = true
	}
	if err := a.closeOpen(); err != nil {
		return nil, err
	}
	a.status = "completed"
	resp := a.build()
	if err := a.send(&Event{Type: EventCompleted, Response: resp}); err != nil {
		return nil, err
	}
	return resp, nil
}

// Incomplete finalises a truncated response.
func (a *Assembler) Incomplete(reason string, usage pluginapi.Usage) (*Response, error) {
	if len(usage.Dimensions) > 0 {
		a.usage = usage
		a.hasUsage = true
	}
	if err := a.closeOpen(); err != nil {
		return nil, err
	}
	a.status = "incomplete"
	a.incomplete = &IncompleteDetails{Reason: reason}
	resp := a.build()
	if err := a.send(&Event{Type: EventIncomplete, Response: resp}); err != nil {
		return nil, err
	}
	return resp, nil
}

// Fail finalises a failed response: an error event followed by response.failed.
func (a *Assembler) Fail(payload *ErrorPayload) (*Response, error) {
	if payload == nil {
		payload = &ErrorPayload{Code: "upstream_error", Message: "the upstream request failed"}
	}
	_ = a.closeOpen() // best effort: keep whatever was produced
	a.status = "failed"
	a.errPayload = payload
	if err := a.send(&Event{Type: EventError, Error: payload}); err != nil {
		return nil, err
	}
	resp := a.build()
	if err := a.send(&Event{Type: EventFailed, Response: resp}); err != nil {
		return nil, err
	}
	return resp, nil
}

// build renders the current response snapshot.
func (a *Assembler) build() *Response {
	out := &Response{
		ID: a.id, Object: "response", CreatedAt: a.createdAt,
		Status: a.status, Model: a.model, Output: a.output,
		Error: a.errPayload, IncompleteDetails: a.incomplete,
	}
	if a.hasUsage {
		out.Usage = UsageToWire(a.usage)
	}
	if out.Output == nil {
		out.Output = []OutputItem{}
	}
	return out
}

func (a *Assembler) send(ev *Event) error {
	a.seq++
	ev.SequenceNumber = a.seq
	return a.emit(ev)
}

// UsageToWire maps canonical usage dimensions onto the Responses usage object.
func UsageToWire(u pluginapi.Usage) *Usage {
	dims := u.Dimensions
	input := dims["input"] + dims["input_cache_hit"] + dims["input_cache_miss"]
	output := dims["output"]
	reasoning := dims["reasoning"]
	out := &Usage{
		InputTokens:  input,
		OutputTokens: output,
		TotalTokens:  input + output,
	}
	if cached := dims["input_cache_hit"]; cached > 0 {
		out.InputTokensDetails = &InputTokensDetails{CachedTokens: cached}
	}
	if reasoning > 0 {
		out.OutputTokensDetails = &OutputTokensDetails{ReasoningTokens: reasoning}
	}
	return out
}

// TextPreview returns a truncated single-line preview (for logs and diagnostics).
func TextPreview(s string, max int) string {
	s = strings.ReplaceAll(s, string([]byte{0x0A}), " ")
	if max > 0 && len(s) > max {
		return s[:max] + "..."
	}
	return s
}

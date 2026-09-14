package pluginapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestEncoderDecoderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)

	ev := Event{Type: EventTextDelta, Text: "hello", Index: 2}
	if err := enc.Write(Frame{ID: "7", Type: FrameEvent, Event: &ev}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(Frame{ID: "7", Type: FrameEnd}); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(&buf)
	first, err := dec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if first.Type != FrameEvent || first.Event == nil || first.Event.Text != "hello" || first.Event.Index != 2 {
		t.Fatalf("event frame mismatch: %+v", first)
	}
	second, err := dec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if second.Type != FrameEnd || second.ID != "7" {
		t.Fatalf("end frame mismatch: %+v", second)
	}
	if _, err := dec.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestFramesAreNewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Write(Frame{Type: FramePong}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), string(newlineByte)), string(newlineByte))
	if len(lines) != 1 {
		t.Fatalf("expected exactly one line, got %d: %q", len(lines), buf.String())
	}
	if !json.Valid([]byte(lines[0])) {
		t.Fatalf("line is not valid JSON: %q", lines[0])
	}
}

func TestOversizedFrameIsRejected(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	big := strings.Repeat("x", MaxFrameBytes+1)
	if err := enc.Write(Frame{Type: FrameEvent, Event: &Event{Text: big}}); err == nil {
		t.Fatal("expected an error for an oversized frame")
	}
}

func TestErrorKinds(t *testing.T) {
	re := NewRetryableError("upstream_5xx", "boom", 503)
	if !re.Retryable || re.Kind != KindRetryable || re.HTTPStatus != 503 {
		t.Fatalf("retryable error fields wrong: %+v", re)
	}
	qe := NewQuotaError("out of quota", 1_800_000_000)
	if qe.Kind != KindQuotaExhausted || qe.ResetAt != 1_800_000_000 || !qe.Retryable {
		t.Fatalf("quota error fields wrong: %+v", qe)
	}
	if _, ok := IsError(nil); ok {
		t.Fatal("nil is not an API error")
	}
	if got, ok := IsError(qe); !ok || got != qe {
		t.Fatal("IsError must return the same pointer")
	}
}

func TestSchemaValidation(t *testing.T) {
	raw := json.RawMessage(`{
      "type": "object",
      "required": ["base_url", "timeout_s"],
      "properties": {
        "base_url": {"type": "string", "format": "uri"},
        "timeout_s": {"type": "integer", "minimum": 1, "maximum": 600},
        "mode": {"type": "string", "enum": ["fast", "balanced"]},
        "api_key": {"type": "string", "x-secret": true},
        "headers": {"type": "array", "maxItems": 2, "items": {"type": "string"}},
        "retry": {"type": "object", "properties": {"attempts": {"type": "integer"}}},
        "name": {"type": "string", "pattern": "^[a-z]+$"}
      }
    }`)

	schema, err := ParseSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !schema.Properties["api_key"].XSecret {
		t.Fatal("x-secret extension must be preserved")
	}

	ok := map[string]any{
		"base_url":  "https://api.example.com/v1",
		"timeout_s": float64(30),
		"mode":      "fast",
		"headers":   []any{"a", "b"},
		"retry":     map[string]any{"attempts": float64(2)},
		"name":      "abc",
	}
	if errs := schema.Validate(ok); len(errs) != 0 {
		t.Fatalf("valid config rejected: %+v", errs)
	}

	bad := map[string]any{
		"timeout_s": float64(0),           // below minimum
		"mode":      "turbo",              // not in enum
		"headers":   []any{"a", "b", "c"}, // too many items
		"name":      "ABC",                // pattern mismatch
		"retry":     map[string]any{"attempts": "two"},
	}
	errs := schema.Validate(bad)
	paths := map[string]bool{}
	for _, e := range errs {
		paths[e.Path] = true
	}
	for _, want := range []string{"base_url", "timeout_s", "mode", "headers", "name", "retry.attempts"} {
		if !paths[want] {
			t.Errorf("expected a validation error at %q, got %+v", want, errs)
		}
	}
}

func TestSchemaTypeChecks(t *testing.T) {
	schema, err := ParseSchema(json.RawMessage(`{"type":"object","properties":{
        "flag":{"type":"boolean"},"count":{"type":"integer"},"ratio":{"type":"number"},"list":{"type":"array"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	errs := schema.Validate(map[string]any{
		"flag":  "yes",
		"count": float64(1.5),
		"ratio": "half",
		"list":  "not-a-list",
	})
	if len(errs) != 4 {
		t.Fatalf("expected 4 errors, got %+v", errs)
	}
}

func TestParseEmptySchemaIsPermissive(t *testing.T) {
	schema, err := ParseSchema(nil)
	if err != nil {
		t.Fatal(err)
	}
	if errs := schema.Validate(map[string]any{"anything": 1}); len(errs) != 0 {
		t.Fatalf("empty schema must accept anything: %+v", errs)
	}
}

// A tool type this package does not model must survive the plugin protocol byte for byte:
// which tool types an upstream understands is that upstream's business, so the frame must
// not quietly reshape it into the function fields.
func TestToolRawRoundTripsThroughTheProtocol(t *testing.T) {
	raw := json.RawMessage(`{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"close_agent"}]}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatal(err)
	}
	if string(tool.Raw) != string(raw) {
		t.Fatalf("Raw = %s, want %s", tool.Raw, raw)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(raw) {
		t.Fatalf("encoded = %s, want the original bytes %s", encoded, raw)
	}

	// A function tool keeps the structured form, so provider-side edits still apply.
	fn := Tool{Type: "function", Name: "bash", Description: "run"}
	encoded, err = json.Marshal(fn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"name":"bash"`) || strings.Contains(string(encoded), `"Raw"`) {
		t.Fatalf("function tool encoded as %s", encoded)
	}
}

// Newer Responses clients may carry provider-specific data inside an input item.
// additional_tools.tools is the production example: losing it leaves the upstream
// with an item that fails validation as "input[0].tools" is missing.
func TestItemExtraFieldsRoundTripThroughTheProtocol(t *testing.T) {
	raw := []byte(`{"model":"m","input":[{"type":"additional_tools","tools":[{"type":"function","name":"bash"}]}]}`)
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if got := string(req.Input[0].Extra["tools"]); got != `[{"type":"function","name":"bash"}]` {
		t.Fatalf("input item Extra[tools] = %s", got)
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if got := string(wire.Input[0]["tools"]); got != `[{"type":"function","name":"bash"}]` {
		t.Fatalf("encoded input item tools = %s", got)
	}
}

// A reasoning item's summary is a *required* key upstream and an empty array is a real
// value, not a missing one. Production example: the ChatGPT subscription backend answers
// "Missing required parameter: 'input[1].summary'" for a reasoning item whose empty
// summary was dropped, and the item then poisons every later request of that session.
func TestItemKeepsExplicitlyEmptyValues(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // keys the encoded item must still carry
		gone []string // keys the encoded item must not carry
	}{
		{
			name: "empty reasoning summary survives",
			in:   `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"abc"}`,
			want: []string{`"summary":[]`, `"encrypted_content":"abc"`},
		},
		{
			name: "non-empty reasoning summary survives",
			in:   `{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"t"}]}`,
			want: []string{`"summary":[{"type":"summary_text","text":"t"}]`},
		},
		{
			name: "absent summary is not invented",
			in:   `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
			gone: []string{`"summary"`},
		},
		{
			name: "null summary is not a summary",
			in:   `{"type":"reasoning","id":"rs_3","summary":null}`,
			gone: []string{`"summary"`},
		},
		{
			name: "empty tool arguments survive",
			in:   `{"type":"function_call","call_id":"call_1","name":"noop","arguments":""}`,
			want: []string{`"arguments":""`},
		},
		{
			name: "empty tool output survives",
			in:   `{"type":"function_call_output","call_id":"call_1","output":""}`,
			want: []string{`"output":""`},
		},
		{
			name: "array tool output still wins over the string form",
			in:   `{"type":"custom_tool_call_output","call_id":"call_1","output":[]}`,
			want: []string{`"output":[]`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var item Item
			if err := json.Unmarshal([]byte(tc.in), &item); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(encoded), want) {
					t.Errorf("encoded item %s is missing %s", encoded, want)
				}
			}
			for _, gone := range tc.gone {
				if strings.Contains(string(encoded), gone) {
					t.Errorf("encoded item %s must not carry %s", encoded, gone)
				}
			}
		})
	}
}

// The fidelity has to survive the plugin hop, which re-encodes every item: the host
// serialises the provider request into the frame's params and the plugin decodes it again.
func TestItemEmptySummarySurvivesTheProtocolHop(t *testing.T) {
	req := Request{
		Model: "m",
		Input: []Item{{Type: "reasoning", ID: "rs_1", Summary: []SummaryPart{},
			Extra: map[string]json.RawMessage{"encrypted_content": json.RawMessage(`"abc"`)}}},
	}
	params, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Write(Frame{ID: "1", Method: MethodStream, Params: params}); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(&buf)
	frame, err := dec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var decoded Request
	if err := json.Unmarshal(frame.Params, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Input) != 1 || decoded.Input[0].Summary == nil || len(decoded.Input[0].Summary) != 0 {
		t.Fatalf("decoded item = %+v, want an empty but present summary", decoded.Input)
	}
	encoded, err := json.Marshal(decoded.Input[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"summary":[]`) {
		t.Fatalf("plugin-side item = %s, want an explicit empty summary", encoded)
	}
	// A provider adapter can also force the key for a client that never sent one.
	forced := Item{Type: "reasoning", ID: "rs_2", Summary: []SummaryPart{}}
	encoded, err = json.Marshal(forced)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"summary":[]`) {
		t.Fatalf("forced item = %s, want an explicit empty summary", encoded)
	}
}

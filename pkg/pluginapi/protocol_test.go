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
		"timeout_s": float64(0),     // below minimum
		"mode":      "turbo",        // not in enum
		"headers":   []any{"a", "b", "c"}, // too many items
		"name":      "ABC",          // pattern mismatch
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

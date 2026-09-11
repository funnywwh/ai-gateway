package pluginapi

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// finishStub is a provider whose Stream emits the given events and returns nil.
// It stands in for a real plugin process (the SDK is the code under test here).
type finishStub struct {
	events []Event
	err    error
}

func (p *finishStub) Info() Info {
	return Info{Name: "finish-stub", Version: "0.0.1", Capabilities: Capabilities{Stream: true}}
}
func (p *finishStub) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *finishStub) Complete(context.Context, *Request) (*Response, error) {
	return &Response{Status: "completed"}, nil
}
func (p *finishStub) Stream(ctx context.Context, req *Request, emit func(Event) error) error {
	for _, ev := range p.events {
		if err := emit(ev); err != nil {
			return err
		}
	}
	return p.err
}
func (p *finishStub) Health(context.Context) error { return nil }
func (p *finishStub) Actions() []Action            { return nil }
func (p *finishStub) RunAction(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, NewError("unknown_action", "nope")
}
func (p *finishStub) StateDir() string                 { return "" }
func (p *finishStub) SetCredentials(map[string]string) {}

// dispatchStub runs one stream through the SDK side and returns the frames the host saw.
func dispatchStub(t *testing.T, events ...Event) []Frame {
	t.Helper()
	hostR, pluginW := io.Pipe()
	defer hostR.Close()

	srv := &server{provider: &finishStub{events: events}, enc: NewEncoder(pluginW), active: map[string]context.CancelFunc{}}
	srv.dispatchStream("42", &Request{Model: "m", Stream: true})

	frames := []Frame{}
	dec := NewDecoder(hostR)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			frame, err := dec.ReadFrame()
			if err != nil {
				return
			}
			frames = append(frames, *frame)
			if frame.Type == FrameEnd || frame.Type == FrameError {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stream to end")
	}
	srv.wg.Wait()
	return frames
}

// TestFinishEventBecomesTheEndFrameReason pins the SDK contract: a plugin states why
// generation stopped with a finish event, and the host must receive it as the end
// frame's finish_reason — that value is what decides whether the client is told the
// answer was truncated.
func TestFinishEventBecomesTheEndFrameReason(t *testing.T) {
	frames := dispatchStub(t,
		Event{Type: EventTextDelta, Text: "half an answer"},
		Event{Type: EventFinish, Reason: "length"},
		Event{Type: EventUsage, Usage: &Usage{Dimensions: map[string]int64{"output": 3}}},
	)
	last := frames[len(frames)-1]
	if last.Type != FrameEnd {
		t.Fatalf("expected an end frame, got %+v", last)
	}
	var end StreamEnd
	if err := json.Unmarshal(last.Result, &end); err != nil {
		t.Fatalf("decoding end frame: %v", err)
	}
	if end.FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", end.FinishReason)
	}
	// The terminal marker is protocol plumbing: it must not leak as a client event.
	for _, frame := range frames {
		if frame.Type == FrameEvent && frame.Event != nil && frame.Event.Type == EventFinish {
			t.Fatal("the finish event must not be relayed as a stream event")
		}
	}
}

// TestStreamWithoutFinishEventKeepsLegacyReading: a plugin built against the older
// SDK never emits a finish event, and its end frame must keep reporting a plain stop
// rather than inventing truncation.
func TestStreamWithoutFinishEventKeepsLegacyReading(t *testing.T) {
	frames := dispatchStub(t, Event{Type: EventTextDelta, Text: "answer"})
	last := frames[len(frames)-1]
	if last.Type != FrameEnd {
		t.Fatalf("expected an end frame, got %+v", last)
	}
	var end StreamEnd
	if err := json.Unmarshal(last.Result, &end); err != nil {
		t.Fatalf("decoding end frame: %v", err)
	}
	if end.FinishReason != ReasonStop {
		t.Fatalf("finish_reason = %q, want %q", end.FinishReason, ReasonStop)
	}
}

// TestIncompleteReasonMapping pins which upstream reasons mean "this is a fragment".
// A reason nobody knows must stay complete: marking a healthy answer incomplete would
// make clients retry answers that were fine.
func TestIncompleteReasonMapping(t *testing.T) {
	cases := []struct {
		reason string
		want   string
		cut    bool
	}{
		{"stop", "", false},
		{"tool_calls", "", false},
		{"end_turn", "", false},
		{"", "", false},
		{"length", "max_output_tokens", true},
		{"max_tokens", "max_output_tokens", true},
		{"max_output_tokens", "max_output_tokens", true},
		{"content_filter", "content_filter", true},
		{"incomplete", "max_output_tokens", true},
		{"LENGTH", "max_output_tokens", true},
	}
	for _, c := range cases {
		got, cut := IncompleteReason(c.reason)
		if got != c.want || cut != c.cut {
			t.Errorf("IncompleteReason(%q) = %q,%v want %q,%v", c.reason, got, cut, c.want, c.cut)
		}
	}
}

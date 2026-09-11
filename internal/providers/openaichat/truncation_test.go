package openaichat

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// collect runs one stream and returns the text plus the events it saw.
func collect(t *testing.T, p *Provider) (string, []pluginapi.Event) {
	t.Helper()
	var text string
	events := []pluginapi.Event{}
	err := p.Stream(context.Background(), userRequest(), func(ev pluginapi.Event) error {
		if ev.Type == pluginapi.EventTextDelta {
			text += ev.Text
		}
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	return text, events
}

// TestStreamEndedWithoutTerminalEventIsRetryable pins the defect that made a dropped
// upstream connection look like a finished answer: the body simply ends — no
// finish_reason chunk, no [DONE] sentinel — which is exactly what a truncated
// response looks like to a reader. That fragment must fail the attempt (failover when
// nothing was emitted yet, `response.failed` when the client already saw text), never
// be reported as a complete answer.
func TestStreamEndedWithoutTerminalEventIsRetryable(t *testing.T) {
	p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"I will write that "}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"into the workspace:"}}]}`,
		} {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// The connection just ends here: the sentence never finishes.
	}, nil)

	var text string
	err := p.Stream(context.Background(), userRequest(), func(ev pluginapi.Event) error {
		if ev.Type == pluginapi.EventTextDelta {
			text += ev.Text
		}
		return nil
	})
	if text == "" {
		t.Fatal("the partial text must still be relayed as it arrives")
	}
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("a stream cut without a terminal event must fail, got %v", err)
	}
	if apiErr.Code != "upstream_stream_incomplete" {
		t.Fatalf("error code = %q, want upstream_stream_incomplete", apiErr.Code)
	}
	if apiErr.Kind != pluginapi.KindRetryable {
		t.Fatalf("a cut stream must allow failover: %+v", apiErr)
	}
}

// TestStreamReportsFinishReason: a stream can end cleanly and still be a fragment —
// the upstream says so with finish_reason=length. Only the provider sees that, so it
// must reach the host as a finish event carrying the reason verbatim.
func TestStreamReportsFinishReason(t *testing.T) {
	p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"half"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}, nil)

	text, events := collect(t, p)
	if text != "half" {
		t.Fatalf("streamed text = %q", text)
	}
	var finish *pluginapi.Event
	var usage *pluginapi.Event
	for i := range events {
		switch events[i].Type {
		case pluginapi.EventFinish:
			finish = &events[i]
		case pluginapi.EventUsage:
			usage = &events[i]
		}
	}
	if finish == nil {
		t.Fatal("a stream that declared its end must report a finish event")
	}
	if finish.Reason != "length" {
		t.Fatalf("finish reason = %q, want length", finish.Reason)
	}
	// Usage stays the last event: metering reads it after the terminal marker.
	if usage == nil || events[len(events)-1].Type != pluginapi.EventUsage {
		t.Fatalf("usage must be the last event, got %+v", events[len(events)-1])
	}
}

// TestStreamNormalStopReportsStop: the ordinary path must not be turned into a
// truncation by the new terminal bookkeeping.
func TestStreamNormalStopReportsStop(t *testing.T) {
	p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"done"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}, nil)

	_, events := collect(t, p)
	last := events[len(events)-1]
	if last.Type != pluginapi.EventUsage {
		t.Fatalf("expected usage last, got %s", last.Type)
	}
	for _, ev := range events {
		if ev.Type == pluginapi.EventFinish && ev.Reason != "stop" {
			t.Fatalf("finish reason = %q, want stop", ev.Reason)
		}
	}
}

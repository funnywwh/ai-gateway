package openairesponses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// lf is the line feed used to build SSE frames (escape-free construction).
var lf = string([]byte{0x0A})

func newUpstream(t *testing.T, chunks ...string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	raw, err := json.Marshal(map[string]any{"base_url": srv.URL, "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New("upstream", string(raw), t.TempDir(), map[string]string{"api_key": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func streamRequest() *pluginapi.Request {
	content, _ := json.Marshal("hi")
	return &pluginapi.Request{
		Model:  "public-model",
		Stream: true,
		Input:  []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
	}
}

func runStream(p *Provider, sink *[]pluginapi.Event) error {
	return p.Stream(context.Background(), streamRequest(), func(ev pluginapi.Event) error {
		if sink != nil {
			*sink = append(*sink, ev)
		}
		return nil
	})
}

// TestStreamEndedWithoutTerminalEventIsRetryable: a Responses upstream declares the
// end of the answer with response.completed / response.incomplete. A body that just
// stops (dropped connection, truncated proxy buffer) is a fragment and must fail the
// attempt instead of being served as a complete answer.
func TestStreamEndedWithoutTerminalEventIsRetryable(t *testing.T) {
	p := newUpstream(t,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"I will write that "}`,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"into the workspace:"}`,
	)
	var events []pluginapi.Event
	err := runStream(p, &events)
	if len(events) == 0 {
		t.Fatal("partial text must still be relayed")
	}
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("a stream cut without a terminal event must fail, got %v", err)
	}
	if apiErr.Code != "upstream_stream_incomplete" || apiErr.Kind != pluginapi.KindRetryable {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
}

// TestStreamIncompleteCarriesTheUpstreamReason: an upstream that admits truncation
// (hit the token limit, content filter) must have that reason forwarded, otherwise
// the host cannot tell the client why the answer stops mid-sentence.
func TestStreamIncompleteCarriesTheUpstreamReason(t *testing.T) {
	p := newUpstream(t,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"half"}`,
		`event: response.incomplete`+"\n"+`data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":1,"total_tokens":10}}}`,
	)
	var events []pluginapi.Event
	if err := runStream(p, &events); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	finish := events[len(events)-2]
	if finish.Type != pluginapi.EventFinish || finish.Reason != "max_output_tokens" {
		t.Fatalf("expected a finish event carrying max_output_tokens, got %+v", events)
	}
	if events[len(events)-1].Type != pluginapi.EventUsage {
		t.Fatalf("usage must be last, got %s", events[len(events)-1].Type)
	}
}

// TestStreamCompletedReportsStop is the ordinary path: it must stay complete.
func TestStreamCompletedReportsStop(t *testing.T) {
	p := newUpstream(t,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"done"}`,
		`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
	)
	var events []pluginapi.Event
	if err := runStream(p, &events); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	finish := events[len(events)-2]
	if finish.Type != pluginapi.EventFinish || finish.Reason != "stop" {
		t.Fatalf("expected finish reason stop, got %+v", events)
	}
}

// TestCompleteReportsFinishReason covers the non-streaming path: the host needs the
// raw reason there too, because Status alone collapses length and content_filter.
func TestCompleteReportsFinishReason(t *testing.T) {
	// The non-streaming body decodes into responseWire, so the mapping is exercised
	// through the wire type it is built from.
	var wire responseWire
	if err := json.Unmarshal([]byte(`{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`), &wire); err != nil {
		t.Fatal(err)
	}
	out := convertResponse(&wire)
	if out.Status != "incomplete" || out.FinishReason != "content_filter" {
		t.Fatalf("status=%q finish_reason=%q", out.Status, out.FinishReason)
	}

	var complete responseWire
	if err := json.Unmarshal([]byte(`{"status":"completed"}`), &complete); err != nil {
		t.Fatal(err)
	}
	out = convertResponse(&complete)
	if out.Status != "completed" || out.FinishReason != "stop" {
		t.Fatalf("status=%q finish_reason=%q", out.Status, out.FinishReason)
	}
}

// TestStreamForwardsCompactionItems: an upstream with native compaction answers a Codex
// remote-compaction turn with a `compaction` output item, and that item exists only in the
// output_item.done frame — there are no deltas to rebuild it from. Dropping it makes the client
// fail the whole thread with "expected exactly one compaction output item, got 0".
func TestStreamForwardsCompactionItems(t *testing.T) {
	p := newUpstream(t,
		`event: response.output_item.done`+"\n"+`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"blob-1"}}`,
		`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
	)
	var events []pluginapi.Event
	if err := runStream(p, &events); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	var forwarded []pluginapi.Item
	for _, ev := range events {
		if ev.Type == pluginapi.EventOutputItemDone && ev.Item != nil {
			forwarded = append(forwarded, *ev.Item)
		}
	}
	if len(forwarded) != 1 {
		t.Fatalf("forwarded items = %+v, want exactly the compaction item", forwarded)
	}
	if forwarded[0].Type != "compaction" {
		t.Fatalf("item type = %q, want compaction", forwarded[0].Type)
	}
	if got := string(forwarded[0].Extra["encrypted_content"]); got != `"blob-1"` {
		t.Fatalf("encrypted_content = %s, want the upstream's blob", got)
	}
}

// TestStreamDoesNotForwardOrdinaryDoneItems: message/reasoning items are rebuilt from the
// deltas this provider already relays, so forwarding their done frames too would make the host
// publish every item twice.
func TestStreamDoesNotForwardOrdinaryDoneItems(t *testing.T) {
	p := newUpstream(t,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`event: response.output_item.done`+"\n"+`data: {"type":"response.output_item.done","item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`,
		`event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
	)
	var events []pluginapi.Event
	if err := runStream(p, &events); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	for _, ev := range events {
		if ev.Type == pluginapi.EventOutputItemDone {
			t.Fatalf("an ordinary done frame must stay on the delta path: %+v", ev.Item)
		}
	}
}

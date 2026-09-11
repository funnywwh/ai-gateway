package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// setProviderConfig rewrites the fixture's provider config and reloads the registry.
// The config version is bumped because the dispatcher caches a built provider per
// version — without it the old instance would keep serving.
func (f *fixture) setProviderConfig(t testing.TB, configJSON string) {
	t.Helper()
	ctx := context.Background()
	prov, err := f.db.GetProviderByName(ctx, "echo")
	if err != nil {
		t.Fatalf("loading provider: %v", err)
	}
	prov.ConfigJSON = configJSON
	prov.ConfigVersion++
	if _, err := f.db.UpsertProvider(ctx, prov); err != nil {
		t.Fatalf("updating provider: %v", err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatalf("reloading registry: %v", err)
	}
}

type sseFrame struct {
	Name string
	Data map[string]any
	Raw  string
}

// collectSSE drains one streaming response body into its events.
func collectSSE(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	var frames []sseFrame
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var name, data string
	flush := func() {
		if name == "" {
			return
		}
		frame := sseFrame{Name: name, Raw: data}
		if err := json.Unmarshal([]byte(data), &frame.Data); err != nil {
			t.Fatalf("bad event payload %q: %v", data, err)
		}
		frames = append(frames, frame)
		name, data = "", ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			flush()
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	flush()
	return frames
}

func lastFrameOfType(frames []sseFrame, want ...string) *sseFrame {
	for i := len(frames) - 1; i >= 0; i-- {
		for _, name := range want {
			if frames[i].Name == name {
				return &frames[i]
			}
		}
	}
	return nil
}

// TestStreamCutShortIsNotReportedAsComplete is the regression for the silent truncation
// that made DSH tasks stop mid-work with no error anywhere: the provider's stream ends
// without ever saying why it stopped (a dropped upstream connection looks exactly like
// that), the client has already received part of the answer, and the gateway used to
// close the response as `response.completed` — so the half sentence was served as the
// model's final answer and the agent ended its turn.
func TestStreamCutShortIsNotReportedAsComplete(t *testing.T) {
	f := newFixture(t)
	f.setProviderConfig(t, `{"prefix":"echo:","chunks":2,"cut_stream":true}`)

	resp := f.do(t, "POST", "/v1/responses",
		`{"model":"echo-model","input":"stream me","stream":true}`, nil)
	defer resp.Body.Close()
	frames := collectSSE(t, resp.Body)

	if done := lastFrameOfType(frames, "response.completed"); done != nil {
		t.Fatalf("a stream cut without a terminal event must not complete: %s", done.Raw)
	}
	failed := lastFrameOfType(frames, "response.failed")
	if failed == nil {
		t.Fatal("expected response.failed for a stream that ended without a finish reason")
	}
	response, _ := failed.Data["response"].(map[string]any)
	if response == nil {
		t.Fatalf("response.failed without a response object: %s", failed.Raw)
	}
	if response["status"] != "failed" {
		t.Fatalf("status = %v, want failed", response["status"])
	}
	errObj, _ := response["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("response.failed without an error object: %s", failed.Raw)
	}
	// The streaming path reports provider failures as code=upstream_error with the
	// specific reason in the message (the message is what clients surface), so the cut
	// has to be named there.
	message, _ := errObj["message"].(string)
	if !strings.Contains(message, "upstream_stream_incomplete") {
		t.Fatalf("expected the cut stream to be named in the error, got %q", message)
	}
	// The partial text still reaches the client: it saw those deltas before the cut.
	var text strings.Builder
	for _, frame := range frames {
		if frame.Name == "response.output_text.delta" {
			delta, _ := frame.Data["delta"].(string)
			text.WriteString(delta)
		}
	}
	if text.Len() == 0 {
		t.Fatal("partial text must still be streamed")
	}
}

// TestTruncatedAnswerEndsAsIncomplete: a stream can end cleanly and still be a
// fragment (the upstream hit the token limit). The upstream says so with
// finish_reason=length, and the client must receive `response.incomplete` — a client
// that derives its stop reason from the terminal status reads `completed` as "the
// model finished", which is what made a cut-off answer look like a finished turn.
func TestTruncatedAnswerEndsAsIncomplete(t *testing.T) {
	f := newFixture(t)
	f.setProviderConfig(t, `{"prefix":"echo:","chunks":1,"finish_reason":"length"}`)

	resp := f.do(t, "POST", "/v1/responses",
		`{"model":"echo-model","input":"stream me","stream":true}`, nil)
	defer resp.Body.Close()
	frames := collectSSE(t, resp.Body)

	if done := lastFrameOfType(frames, "response.completed"); done != nil {
		t.Fatalf("a truncated answer must not complete: %s", done.Raw)
	}
	last := lastFrameOfType(frames, "response.incomplete")
	if last == nil {
		t.Fatalf("expected response.incomplete, got %s", frames[len(frames)-1].Name)
	}
	response, _ := last.Data["response"].(map[string]any)
	if response == nil || response["status"] != "incomplete" {
		t.Fatalf("unexpected terminal response: %s", last.Raw)
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if details == nil || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason must name the token limit: %s", last.Raw)
	}
}

// TestNonStreamingTruncatedAnswerIsIncomplete is the same guarantee without streaming:
// the JSON body carries status=incomplete so a client cannot mistake it for a full answer.
func TestNonStreamingTruncatedAnswerIsIncomplete(t *testing.T) {
	f := newFixture(t)
	f.setProviderConfig(t, `{"prefix":"echo:","chunks":1,"finish_reason":"content_filter"}`)

	resp := f.do(t, "POST", "/v1/responses", `{"model":"echo-model","input":"ping"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Status != "incomplete" {
		t.Fatalf("status = %q, want incomplete", body.Status)
	}
	if body.IncompleteDetails == nil || body.IncompleteDetails.Reason != "content_filter" {
		t.Fatalf("incomplete_details mismatch: %+v", body.IncompleteDetails)
	}
}

// TestStreamedEventsCarryOutputIndex: item-scoped events must carry output_index even
// when it is 0. Clients key their item table by it (Codex logs "OutputTextDelta without
// active item"; pi-ai drops deltas whose slot it cannot find), so an omitted zero
// detaches every delta of the first item from its item.
func TestStreamedEventsCarryOutputIndex(t *testing.T) {
	f := newFixture(t)
	resp := f.do(t, "POST", "/v1/responses",
		`{"model":"echo-model","input":"stream me","stream":true}`, nil)
	defer resp.Body.Close()
	frames := collectSSE(t, resp.Body)

	scoped := map[string]bool{
		"response.output_item.added":             true,
		"response.output_item.done":              true,
		"response.content_part.added":            true,
		"response.content_part.done":             true,
		"response.output_text.delta":             true,
		"response.output_text.done":              true,
		"response.function_call_arguments.delta": true,
		"response.function_call_arguments.done":  true,
		"response.reasoning_summary_text.delta":  true,
		"response.reasoning_summary_text.done":   true,
		"response.reasoning_summary_part.added":  true,
		"response.reasoning_summary_part.done":   true,
	}
	seen := 0
	for _, frame := range frames {
		if !scoped[frame.Name] {
			if _, present := frame.Data["output_index"]; present {
				t.Fatalf("%s must not carry output_index: %s", frame.Name, frame.Raw)
			}
			continue
		}
		seen++
		index, present := frame.Data["output_index"]
		if !present {
			t.Fatalf("%s is missing output_index: %s", frame.Name, frame.Raw)
		}
		if index != float64(0) {
			t.Fatalf("%s output_index = %v, want 0", frame.Name, index)
		}
	}
	if seen < 4 {
		t.Fatalf("expected several item-scoped events, saw %d", seen)
	}
}

// TestIncompleteUsageIsFlaggedInTheRecord: the usage table keeps counting the attempt
// as served (the tokens were produced and billed upstream) while saying why it ended,
// so a truncated answer is visible in the record instead of looking identical to one
// the model finished on its own.
func TestIncompleteUsageIsFlaggedInTheRecord(t *testing.T) {
	f := newFixture(t)
	f.setProviderConfig(t, `{"prefix":"echo:","chunks":1,"finish_reason":"length"}`)

	resp := f.do(t, "POST", "/v1/responses", `{"model":"echo-model","input":"ping","stream":true}`, nil)
	defer resp.Body.Close()
	_ = collectSSE(t, resp.Body)

	rows, err := f.db.ListUsage(context.Background(), f.key.AccountID, time.Time{}, time.Time{}, 5)
	if err != nil {
		t.Fatalf("reading usage: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected a usage record")
	}
	row := rows[0]
	if row.Status != "completed" {
		t.Fatalf("status = %q, want completed (the attempt was served)", row.Status)
	}
	if row.TerminatedReason != "incomplete" {
		t.Fatalf("terminated_reason = %q, want incomplete", row.TerminatedReason)
	}
}

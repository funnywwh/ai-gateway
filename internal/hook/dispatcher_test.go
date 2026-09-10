package hook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

func testEvent() *domain.Event {
	return &domain.Event{
		Name:      "response.completed",
		Timestamp: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		Payload: map[string]any{
			"request_id": "req_1", "model": "demo", "input": "hello", "output": "world",
		},
	}
}

func parseTS(header string) string {
	ts, _, _ := parseSignature(header)
	return ts
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

func TestWebhookDeliveryWithSignature(t *testing.T) {
	var (
		gotBody []byte
		gotSig  string
		gotEvt  string
		calls   atomic.Int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotSig = r.Header.Get("X-AIGW-Signature")
		gotEvt = r.Header.Get("X-AIGW-Event")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, []*domain.Hook{{
		Name: "ops", Type: "webhook", URL: srv.URL, Secret: "s3cret",
		Enabled: true, SampleRate: 1, IncludeContent: true,
	}}, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent())
	waitFor(t, func() bool { return d.Stats().Delivered == 1 }, "delivery")

	if gotEvt != "response.completed" {
		t.Fatalf("event header = %q", gotEvt)
	}
	if !strings.HasPrefix(gotSig, "t=") || !strings.Contains(gotSig, ",v1=") {
		t.Fatalf("signature header malformed: %q", gotSig)
	}
	if !VerifySignature("s3cret", gotSig, gotBody) {
		t.Fatalf("signature must verify against the body: sig=%q bodyLen=%d expected=%q",
			gotSig, len(gotBody), Signature("s3cret", parseTS(gotSig), gotBody))
	}
	if VerifySignature("wrong", gotSig, gotBody) {
		t.Fatal("signature must not verify with the wrong secret")
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["input"] != "hello" || payload["output"] != "world" {
		t.Fatalf("content must be included when the hook opts in: %+v", payload)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one delivery, got %d", calls.Load())
	}
}

func TestContentIsOmittedUnlessOptedIn(t *testing.T) {
	bodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies <- string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, []*domain.Hook{{
		Name: "meta", Type: "webhook", URL: srv.URL, Enabled: true, SampleRate: 1,
		IncludeContent: false,
	}}, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent())
	select {
	case body := <-bodies:
		if strings.Contains(body, "hello") || strings.Contains(body, "world") {
			t.Fatalf("content must not be sent when include_content is false: %s", body)
		}
		if !strings.Contains(body, "req_1") {
			t.Fatalf("metadata must still be sent: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestServerErrorRetriesThenDeadLetters(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dead := filepath.Join(t.TempDir(), "dead.jsonl")
	cfg := DefaultConfig()
	cfg.Retries = 1
	cfg.DeadLetter = dead
	d := New(cfg, []*domain.Hook{{Name: "flaky", Type: "webhook", URL: srv.URL, Enabled: true, SampleRate: 1}}, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent())
	waitFor(t, func() bool { return d.Stats().Failed == 1 }, "failure")

	if calls.Load() != 2 {
		t.Fatalf("expected 1 attempt + 1 retry, got %d", calls.Load())
	}
	raw, err := os.ReadFile(dead)
	if err != nil {
		t.Fatalf("dead letter missing: %v", err)
	}
	if !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("dead letter content mismatch: %s", raw)
	}
}

func TestClientErrorDoesNotRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Retries = 3
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, []*domain.Hook{{Name: "bad", Type: "webhook", URL: srv.URL, Enabled: true, SampleRate: 1}}, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent())
	waitFor(t, func() bool { return d.Stats().Failed == 1 }, "failure")
	if calls.Load() != 1 {
		t.Fatalf("4xx must not be retried, attempts = %d", calls.Load())
	}
}

func TestJSONLSinkAppendsLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	cfg := DefaultConfig()
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, []*domain.Hook{{Name: "audit", Type: "jsonl", URL: path, Enabled: true, SampleRate: 1}}, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent())
	d.Emit(context.Background(), testEvent())
	waitFor(t, func() bool { return d.Stats().Delivered == 2 }, "jsonl writes")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), string([]byte{0x0A}))
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if parsed["event"] != "response.completed" {
		t.Fatalf("event name mismatch: %+v", parsed)
	}
}

func TestEventFilterAndSampling(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, []*domain.Hook{{
		Name: "filtered", Type: "webhook", URL: srv.URL, Enabled: true, SampleRate: 1,
		EventsJSON: `["request.denied"]`,
	}}, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent()) // response.completed: not subscribed
	waitFor(t, func() bool { return d.Stats().Skipped == 1 }, "skip")
	if calls.Load() != 0 {
		t.Fatalf("unsubscribed events must not be delivered, calls = %d", calls.Load())
	}
}

func TestQueueOverflowDropsWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block the single worker
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	cfg := DefaultConfig()
	cfg.QueueSize = 2
	cfg.Workers = 1
	cfg.Timeout = 2 * time.Second
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, []*domain.Hook{{Name: "slow", Type: "webhook", URL: srv.URL, Enabled: true, SampleRate: 1}}, nil)

	started := time.Now()
	for i := 0; i < 20; i++ {
		d.Emit(context.Background(), testEvent())
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Emit must never block: took %s", elapsed)
	}
	if d.Stats().Dropped == 0 {
		t.Fatal("expected drops once the queue is full")
	}
	d.Close(100 * time.Millisecond)
}

func TestSetHooksHotSwap(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.DeadLetter = filepath.Join(t.TempDir(), "dead.jsonl")
	d := New(cfg, nil, nil)
	defer d.Close(time.Second)

	d.Emit(context.Background(), testEvent())
	if calls.Load() != 0 {
		t.Fatal("no hooks configured: nothing should be delivered")
	}

	d.SetHooks([]*domain.Hook{{Name: "new", Type: "webhook", URL: srv.URL, Enabled: true, SampleRate: 1}})
	d.Emit(context.Background(), testEvent())
	waitFor(t, func() bool { return d.Stats().Delivered == 1 }, "delivery after hot swap")

	// Disabled hooks are filtered out at SetHooks time.
	d.SetHooks([]*domain.Hook{{Name: "off", Type: "webhook", URL: srv.URL, Enabled: false, SampleRate: 1}})
	if len(d.Hooks()) != 0 {
		t.Fatalf("disabled hooks must be excluded: %d", len(d.Hooks()))
	}
}

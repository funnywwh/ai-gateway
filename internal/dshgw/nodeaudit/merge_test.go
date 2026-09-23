package nodeaudit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
)

// fakeCursors stands in for the node store: the merger only needs "where did I get to".
type fakeCursors struct {
	mu     sync.Mutex
	values map[string]string
	fail   error
}

func newCursors() *fakeCursors { return &fakeCursors{values: map[string]string{}} }

func (c *fakeCursors) AuditCursor(node string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[node]
}

func (c *fakeCursors) SetAuditCursor(node, cursor string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.values[node] = cursor
	return nil
}

// nodeStub answers audit-tail with a scripted sequence of batches.
type nodeStub struct {
	mu      sync.Mutex
	batches []nodeproto.AuditTailResult
	cursors []string
	status  int
	errBody any
}

func (n *nodeStub) server(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(nodeproto.ControlPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "bad token")
			return
		}
		var request nodeproto.AuditTailRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		n.mu.Lock()
		n.cursors = append(n.cursors, request.Cursor)
		status := n.status
		var batch nodeproto.AuditTailResult
		if len(n.batches) > 0 {
			batch = n.batches[0]
			n.batches = n.batches[1:]
		}
		n.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			nodeproto.WriteError(w, status, nodeproto.CodeUnreachable, "node is unreachable")
			return
		}
		nodeproto.WriteValue(w, http.StatusOK, batch)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (n *nodeStub) askedCursors() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.cursors...)
}

func mergerFixture(t *testing.T) (*Merger, *nodeStub, *fakeCursors, string) {
	t.Helper()
	stub := &nodeStub{}
	server := stub.server(t, "tok")
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: server.URL, Token: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	cursors := newCursors()
	merger := &Merger{Clients: set, Cursors: cursors, Sink: &audit.JSONL{Path: path}}
	return merger, stub, cursors, path
}

func readAudit(t *testing.T, path string) []audit.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var events []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event audit.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("audit line %q is not an event: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// TestDrainTagsAndAdvances is the whole point of the merger: a node's events land in the gateway's
// own audit file, tagged with the node, and the cursor remembers how far it got.
func TestDrainTagsAndAdvances(t *testing.T) {
	merger, stub, cursors, path := mergerFixture(t)
	stub.batches = []nodeproto.AuditTailResult{{
		Lines: []string{
			`{"time":"2026-09-23T01:00:00Z","kind":"ssh-mount-refused","tenant":"alice","reason":"self-nesting","status":403}`,
			`{"time":"2026-09-23T01:01:00Z","kind":"logout_mount_detach","tenant":"alice","reason":"2 mounts","status":303}`,
		},
		Cursor: "off:4096",
	}}
	client, _ := merger.Clients.Get("node-a")
	written, err := merger.Drain(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if written != 2 {
		t.Fatalf("written = %d", written)
	}
	events := readAudit(t, path)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	for _, event := range events {
		if event.Node != "node-a" {
			t.Fatalf("event was not tagged with its node: %+v", event)
		}
	}
	if events[0].Kind != "ssh-mount-refused" || events[0].Tenant != "alice" || events[0].Status != 403 {
		t.Fatalf("the first event lost its fields: %+v", events[0])
	}
	if got := cursors.AuditCursor("node-a"); got != "off:4096" {
		t.Fatalf("cursor = %q", got)
	}
	// The next drain resumes from the recorded cursor.
	if _, err := merger.Drain(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	asked := stub.askedCursors()
	if len(asked) != 2 || asked[0] != "" || asked[1] != "off:4096" {
		t.Fatalf("cursors asked = %v", asked)
	}
	// Nothing new: nothing appended.
	if len(readAudit(t, path)) != 2 {
		t.Fatalf("a drain with no events appended something: %+v", readAudit(t, path))
	}
}

// TestDrainReportsUnreadableLines keeps a broken line visible instead of dropping it: the
// operator's question is "did anything happen on that node", and silence is the wrong answer.
func TestDrainReportsUnreadableLines(t *testing.T) {
	merger, stub, _, path := mergerFixture(t)
	stub.batches = []nodeproto.AuditTailResult{{
		Lines:  []string{"not json at all"},
		Cursor: "off:10",
	}}
	client, _ := merger.Clients.Get("node-a")
	if _, err := merger.Drain(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	events := readAudit(t, path)
	if len(events) != 1 || events[0].Kind != "node_audit_unreadable" || events[0].Node != "node-a" {
		t.Fatalf("events = %+v", events)
	}
	if !strings.Contains(events[0].Reason, "not json") {
		t.Fatalf("the unreadable line was not preserved: %+v", events[0])
	}
	// A line that parses as JSON but carries no kind is treated the same way: an event without a
	// kind is not an event.
	stub.batches = []nodeproto.AuditTailResult{{Lines: []string{`{"time":"2026-09-23T01:00:00Z","reason":"x"}`}, Cursor: "off:20"}}
	if _, err := merger.Drain(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	events = readAudit(t, path)
	if events[len(events)-1].Kind != "node_audit_unreadable" {
		t.Fatalf("events = %+v", events)
	}
}

func TestDrainPropagatesNodeFailure(t *testing.T) {
	merger, stub, _, path := mergerFixture(t)
	stub.status = http.StatusServiceUnavailable
	client, _ := merger.Clients.Get("node-a")
	_, err := merger.Drain(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("err = %v", err)
	}
	if len(readAudit(t, path)) != 0 {
		t.Fatal("a failed drain wrote something")
	}
}

func TestDrainAllContinuesAfterAFailure(t *testing.T) {
	stub := &nodeStub{status: http.StatusServiceUnavailable}
	broken := stub.server(t, "tok")
	healthy := &nodeStub{}
	healthy.batches = []nodeproto.AuditTailResult{{
		Lines: []string{`{"time":"2026-09-23T01:00:00Z","kind":"mount.expire","reason":"x","status":200}`}, Cursor: "off:7",
	}}
	good := healthy.server(t, "tok2")
	set, err := nodeclient.NewSet([]nodeclient.Spec{
		{Name: "node-a", BaseURL: broken.URL, Token: "tok"},
		{Name: "node-b", BaseURL: good.URL, Token: "tok2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	merger := &Merger{Clients: set, Cursors: newCursors(), Sink: &audit.JSONL{Path: path}}
	err = merger.DrainAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("err = %v", err)
	}
	events := readAudit(t, path)
	if len(events) != 1 || events[0].Node != "node-b" {
		t.Fatalf("one broken node stopped the other: %+v", events)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	merger, stub, _, _ := mergerFixture(t)
	merger.Interval = 10 * time.Millisecond
	stub.batches = []nodeproto.AuditTailResult{{Cursor: "off:1"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		merger.Run(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	if len(stub.askedCursors()) == 0 {
		t.Fatal("Run never drained")
	}
}

func TestWriteFailureIsReported(t *testing.T) {
	merger, stub, _, _ := mergerFixture(t)
	// A sink that refuses is how a full disk shows up; the cursor must not advance past events
	// that never landed.
	merger.Sink = failingSink{}
	stub.batches = []nodeproto.AuditTailResult{{Lines: []string{`{"kind":"x","reason":"y","status":1}`}, Cursor: "off:9"}}
	client, _ := merger.Clients.Get("node-a")
	if _, err := merger.Drain(context.Background(), client); err == nil {
		t.Fatal("a failing sink must be reported")
	}
	if got := merger.Cursors.(*fakeCursors).AuditCursor("node-a"); got != "" {
		t.Fatalf("the cursor advanced past events that were not written: %q", got)
	}
}

type failingSink struct{}

func (failingSink) Write(audit.Event) error { return os.ErrPermission }

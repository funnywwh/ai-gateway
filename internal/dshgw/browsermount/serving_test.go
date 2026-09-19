package browsermount

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// A worker profile must never advertise a mount the browser can no longer
// answer: the profile resolves every mount path and bubblewrap stats every bind
// source, so one clientless mount blocks the whole worker start and surfaces as
// "worker restart before close" on an unrelated mount.
func TestMountsForSkipsUnservedMount(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	if paths := s.MountsFor(tenant.Name); len(paths) != 1 || paths[0] != sh.path {
		t.Fatalf("fresh mount not advertised: %v", paths)
	}
	sh.mu.Lock()
	sh.seen = time.Now().Add(-lease - time.Second)
	sh.mu.Unlock()
	if paths := s.MountsFor(tenant.Name); len(paths) != 0 {
		t.Fatalf("expired mount still advertised: %v", paths)
	}
	// The same rule Call already applies to a queued operation.
	if _, err := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""}); err == nil {
		t.Fatal("expired mount still accepted a request")
	}
}

// The poll request is the browser side of the mount. When its connection dies
// (page reload/close/crash, or the client's abort before close) the mount is
// disconnected at once instead of at the end of the lease.
func TestCanceledPollDisconnectsMount(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	// A tenant operation that the vanished browser will never answer.
	operation := make(chan error, 1)
	go func() {
		_, err := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""})
		operation <- err
	}()
	<-sh.queue

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeTenant(w, r, tenant, "owner")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	body := strings.NewReader(`{"token":"` + sh.token + `"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/browser-workspace/poll", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The client goes away while the gateway is holding the long poll.
	if _, err := server.Client().Do(req); err == nil {
		t.Fatal("poll returned although its client was gone")
	}

	select {
	case err := <-operation:
		if err == nil {
			t.Fatal("pending operation survived the browser disconnect")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending operation was not woken by the browser disconnect")
	}
	if paths := s.MountsFor(tenant.Name); len(paths) != 0 {
		t.Fatalf("disconnected mount still advertised: %v", paths)
	}
	// The close that follows such a poll failure must still clean up.
	result, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: sh.token})
	if err != nil {
		t.Fatalf("close after a dead poll: %v", err)
	}
	if !result.(map[string]bool)["closed"] {
		t.Fatal("close did not report success", result)
	}
	if !m.closed {
		t.Fatal("mount was not unmounted")
	}
}

// The real-host report: closing one mount restarted the worker while a second
// mount of the same tenant had no browser left. The restart resolved that dead
// path and failed with "resolve browser mount: lstat ...: connection timed out",
// which left the first mount uncleaned.
func TestCloseAfterDeadSiblingMount(t *testing.T) {
	var restarts atomic.Int32
	var observed []string
	mounts := map[string]*fakeMount{}
	var s *Service
	s = NewWithMount(func(_ context.Context, t registry.Tenant) error {
		restarts.Add(1)
		observed = s.MountsFor(t.Name)
		return nil
	}, func(path string, _ fs.Backend) (Mounted, error) {
		m := &fakeMount{}
		mounts[path] = m
		return m, nil
	})
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	t.Cleanup(func() { _ = s.DropTenant(context.Background(), tenant.Name) })
	dead, err := s.open(tenant, "owner", "dead", true)
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.open(tenant, "owner", "live", true)
	if err != nil {
		t.Fatal(err)
	}
	// The dead mount's page disappeared without a close. Nothing about that
	// share says "closed" — only that its browser stopped answering (here: the
	// lease ran out; a canceled poll is the same state, sooner).
	dead.mu.Lock()
	dead.seen = time.Now().Add(-lease - time.Second)
	dead.mu.Unlock()
	if paths := s.MountsFor(tenant.Name); len(paths) != 1 || paths[0] != live.path {
		t.Fatalf("dead mount still advertised: %v", paths)
	}
	result, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: live.token})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !result.(map[string]bool)["closed"] {
		t.Fatal("close did not report success", result)
	}
	if restarts.Load() != 1 {
		t.Fatalf("worker restarts = %d, want 1", restarts.Load())
	}
	if len(observed) != 0 {
		t.Fatalf("worker profile still carried mounts: %v", observed)
	}
	if !mounts[live.path].closed {
		t.Fatal("closing mount was not unmounted")
	}
}

// Every endpoint that would serve a mount is refused once it stops being
// serveable, so a late poll cannot revive a capability the gateway has already
// excluded from worker profiles.
func TestExpiredCapabilityIsRefused(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	sh.seen = time.Now().Add(-lease - time.Second)
	sh.mu.Unlock()
	for _, op := range []string{"poll", "activate", "heartbeat"} {
		body, err := json.Marshal(payload{Token: sh.token})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/browser-workspace/"+op, strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		s.ServeTenant(w, req, tenant, "owner")
		var result struct {
			OK    bool `json:"ok"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.OK {
			t.Fatalf("%s accepted an expired capability", op)
		}
	}
}

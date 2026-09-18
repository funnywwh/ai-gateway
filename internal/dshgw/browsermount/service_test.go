package browsermount

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

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

type fakeMount struct {
	mu     sync.Mutex
	closed bool
}

func (m *fakeMount) Unmount() error { m.mu.Lock(); defer m.mu.Unlock(); m.closed = true; return nil }
func fixture(t *testing.T) (*Service, registry.Tenant, *fakeMount) {
	t.Helper()
	m := &fakeMount{}
	s := NewWithMount(func(context.Context, registry.Tenant) error { return nil }, func(string, fs.Backend) (Mounted, error) { return m, nil })
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	t.Cleanup(func() { _ = s.DropTenant(context.Background(), tenant.Name) })
	return s, tenant, m
}
func TestCapabilityAndReverseExchange(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "session-a", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"bob", "session-a"}, {"alice", "session-b"}} {
		if _, err := s.get(sh.token, identity[0], identity[1]); err == nil {
			t.Fatal("cross identity allowed")
		}
	}
	done := make(chan fs.Response, 1)
	failed := make(chan error, 1)
	go func() {
		r, e := sh.Call(context.Background(), fs.Request{Op: "read", Path: "file", Size: 5})
		if e != nil {
			failed <- e
		} else {
			done <- r
		}
	}()
	value, err := s.dispatch(context.Background(), "poll", tenant, "session-a", payload{Token: sh.token})
	if err != nil {
		t.Fatal(err)
	}
	req := value.(map[string]any)["requests"].([]fs.Request)[0]
	reply := payload{Token: sh.token, ID: req.ID, Result: fs.Response{OK: true, Value: fs.Value{Data: []byte("hello"), Bytes: 5}}}
	if _, err := s.dispatch(context.Background(), "respond", tenant, "session-a", reply); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if string(r.Value.Data) != "hello" {
			t.Fatal(r)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("unanswered request")
	}
	duplicate, err := s.dispatch(context.Background(), "respond", tenant, "session-a", reply)
	if err != nil || duplicate.(map[string]bool)["accepted"] {
		t.Fatal("duplicate must be discarded without disconnect", duplicate, err)
	}
	if paths := s.MountsFor("bob"); len(paths) != 0 {
		t.Fatal(paths)
	}
	if paths := s.MountsFor("alice"); len(paths) != 1 || paths[0] != sh.path {
		t.Fatal(paths)
	}
}
func TestDisconnectWakesRequests(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "files", true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""}); done <- e }()
	<-sh.queue
	if err := s.close(sh); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnect succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("request hung")
	}
	if !m.closed {
		t.Fatal("not unmounted")
	}
}
func TestReadOnlyAndBounds(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "files", false)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sh.Call(context.Background(), fs.Request{Op: "write", Path: "x", Data: []byte("x")})
	if err != nil || resp.OK || resp.Error == nil || resp.Error.Code != "EROFS" {
		t.Fatal("read-only error must preserve errno", resp, err)
	}
	for range 3 {
		if _, err := s.open(tenant, "owner", "files", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.open(tenant, "owner", "fifth", false); err == nil {
		t.Fatal("mount quota bypassed")
	}
}
func TestPrepareRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "browser")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(root, "safe-id"); err == nil {
		t.Fatal("symlink container accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "safe-id")); !os.IsNotExist(err) {
		t.Fatal("created outside workspace")
	}
}
func TestHTTPRejectsMalformedBodies(t *testing.T) {
	s, tenant, _ := fixture(t)
	for _, body := range []string{`{`, `{"tenant":"bob"}`, `{} {}`, strings.Repeat("x", maxBody+1)} {
		req := httptest.NewRequest(http.MethodPost, "/browser-workspace/open", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeTenant(w, req, tenant, "owner")
		var result struct{ OK bool }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.OK {
			t.Fatalf("accepted %q", body[:1])
		}
	}
	w := httptest.NewRecorder()
	s.ServeTenant(w, httptest.NewRequest(http.MethodGet, "/browser-workspace/open", nil), tenant, "owner")
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}
func TestCanceledMutationNeverDispatched(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "files", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sh.Call(ctx, fs.Request{Op: "write", Path: "file", Data: []byte("bad")}); err == nil {
		t.Fatal("canceled request succeeded")
	}
	done := make(chan error, 1)
	go func() { _, err := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""}); done <- err }()
	result, err := s.dispatch(context.Background(), "poll", tenant, "owner", payload{Token: sh.token})
	if err != nil {
		t.Fatal(err)
	}
	requests := result.(map[string]any)["requests"].([]fs.Request)
	if len(requests) != 1 || requests[0].Op != "stat" {
		t.Fatal("canceled mutation leaked", requests)
	}
	_ = s.close(sh)
	<-done
}

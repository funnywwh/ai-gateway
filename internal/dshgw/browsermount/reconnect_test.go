package browsermount

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// cancelPoll drives one real poll request and then makes its client go away, which is
// exactly what a page reload does to the mount's browser side. It returns once the gateway
// has observed the cancellation.
func cancelPoll(t *testing.T, s *Service, tenant registry.Tenant, owner, token string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeTenant(w, r, tenant, owner)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/browser-workspace/poll",
		strings.NewReader(`{"token":"`+token+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if _, err := server.Client().Do(request); err == nil {
		t.Fatal("poll returned although its client was gone")
	}
	// The gateway learns about the cancellation on its own goroutine, so wait for the state
	// it is about to produce rather than assuming it has already happened.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sh, err := s.capability(token, tenant.Name, owner); err == nil {
			sh.mu.Lock()
			waiting := sh.awaitingResume()
			sh.mu.Unlock()
			if waiting {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the gateway never noticed that the poll client went away")
}

// The reload path, at the level the gateway owns: losing the page must stop the mount from
// being served and from being advertised to a worker, but it must NOT tear the mount down.
// Tearing it down is what made a 300ms reload cost a worker restart, a new mount point, and
// a DSH workspace entry pointing at a deleted directory.
func TestDisconnectKeepsTheMountWaitingForAReload(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	if paths := s.MountsFor(tenant.Name); len(paths) != 1 {
		t.Fatalf("fresh mount not advertised: %v", paths)
	}
	cancelPoll(t, s, tenant, "owner", sh.token)

	if paths := s.MountsFor(tenant.Name); len(paths) != 0 {
		t.Fatalf("a mount whose page is gone is still advertised to worker starts: %v", paths)
	}
	if _, err := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""}); err == nil {
		t.Fatal("a mount whose page is gone accepted an operation")
	}
	sh.mu.Lock()
	waiting, closed, mounted := sh.awaitingResume(), sh.closed, sh.mounted
	sh.mu.Unlock()
	if !waiting || closed || mounted == nil {
		t.Fatalf("the mount was not kept waiting: waiting=%v closed=%v mounted=%v", waiting, closed, mounted != nil)
	}
	if m.closed {
		t.Fatal("the kernel mount was released before the grace window passed")
	}
	if s.recordPath(sh.tenant.Name, sh.id) == "" {
		t.Fatal("no record path")
	}
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: sh.token}); err != nil {
		t.Fatalf("close after a dead poll: %v", err)
	}
}

// The resume path: the reloaded page (or the tab that replaced it) proves the same
// capability and gets the SAME mount back — same mount point, no second mount, no worker
// restart, because none of that was given up in the first place.
func TestResumeRestoresTheSameMountWithoutRebuilding(t *testing.T) {
	var mounts, restarts atomic.Int32
	var s *Service
	s = NewWithMount(func(context.Context, registry.Tenant) error { restarts.Add(1); return nil },
		func(string, fs.Backend) (Mounted, error) { mounts.Add(1); return &fakeMount{}, nil })
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	t.Cleanup(func() { _ = s.DropTenant(context.Background(), tenant.Name) })
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	cancelPoll(t, s, tenant, "owner", sh.token)
	before := sh.path

	value, err := s.dispatch(context.Background(), "resume", tenant, "owner", payload{Token: sh.token})
	if err != nil {
		t.Fatalf("resume after a reload: %v", err)
	}
	answer := value.(map[string]any)
	if answer["mountpoint"] != before {
		t.Fatalf("resume moved the mount: %v want %v", answer["mountpoint"], before)
	}
	if answer["id"] != sh.id {
		t.Fatalf("resume changed identity: %v want %v", answer["id"], sh.id)
	}
	if mounts.Load() != 1 || restarts.Load() != 0 {
		t.Fatalf("resume rebuilt the mount: mounts=%d restarts=%d", mounts.Load(), restarts.Load())
	}
	if paths := s.MountsFor(tenant.Name); len(paths) != 1 || paths[0] != before {
		t.Fatalf("resumed mount not advertised again: %v", paths)
	}
	// Serving really resumed: an operation queues and can be answered.
	done := make(chan error, 1)
	go func() { _, err := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""}); done <- err }()
	request := (<-sh.queue)
	reply := payload{Token: sh.token, ID: request.ID, Result: fs.Response{OK: true, Value: fs.Value{Kind: "directory"}}}
	if _, err := s.dispatch(context.Background(), "respond", tenant, "owner", reply); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the resumed mount could not serve an operation: %v", err)
	}
}

// Past the window the mount is finished, exactly as it used to be the moment the poll died:
// unmounted, mount point and record gone, and no resume can bring it back.
func TestGraceExpiryTearsTheMountDownAndRefusesResume(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	mountpoint := sh.path
	cancelPoll(t, s, tenant, "owner", sh.token)

	// Still inside the window: the reaper must leave it alone.
	if err := s.expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.closed {
		t.Fatal("the reaper tore down a mount that was still inside its grace window")
	}
	sh.mu.Lock()
	sh.disconnectedAt = time.Now().Add(-reconnectGrace - time.Second)
	sh.mu.Unlock()
	if err := s.expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.closed {
		t.Fatal("the reaper left an expired mount mounted")
	}
	if _, err := s.dispatch(context.Background(), "resume", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("an expired mount was resumed")
	}
	if _, err := s.dispatch(context.Background(), "poll", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("an expired mount still answered a poll")
	}
	if _, err := os.Stat(mountpoint); !os.IsNotExist(err) {
		t.Fatalf("the mount point survived the grace expiry: %v", err)
	}
}

// A page that flaps must not extend its own grace window: the first departure starts it,
// and later ones leave it where it is.
func TestRepeatedDisconnectsDoNotExtendTheWindow(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	cancelPoll(t, s, tenant, "owner", sh.token)
	sh.mu.Lock()
	first := sh.disconnectedAt
	sh.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	sh.disconnect()
	sh.mu.Lock()
	again, closed := sh.disconnectedAt, sh.closed
	sh.mu.Unlock()
	if !again.Equal(first) {
		t.Fatalf("a second disconnect moved the window: %v -> %v", first, again)
	}
	if closed {
		t.Fatal("disconnect closed the share outright")
	}
	if m.closed {
		t.Fatal("disconnect released the kernel mount")
	}
}

// Resume is not a weaker authorization: only the session that held the capability, for the
// tenant that owns it, can take a waiting mount back.
func TestResumeKeepsCapabilityChecks(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "session-a", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	cancelPoll(t, s, tenant, "session-a", sh.token)
	other := registry.Tenant{Name: "bob", Workspace: t.TempDir()}
	for _, identity := range []struct {
		what  string
		token string
		t     registry.Tenant
		owner string
	}{
		{"another session", sh.token, tenant, "session-b"},
		{"another tenant", sh.token, other, "session-a"},
		{"an invented token", strings.Repeat("a", 48), tenant, "session-a"},
	} {
		if _, err := s.dispatch(context.Background(), "resume", identity.t, identity.owner, payload{Token: identity.token}); err == nil {
			t.Fatalf("resume accepted %s", identity.what)
		}
	}
	// The session that held the capability does get it back — that is the point of resume.
	value, err := s.dispatch(context.Background(), "resume", tenant, "session-a", payload{Token: sh.token})
	if err != nil {
		t.Fatalf("the owning session could not resume its own waiting mount: %v", err)
	}
	if value.(map[string]any)["mountpoint"] != sh.path {
		t.Fatalf("resume answered %v", value)
	}
}

// A mount a page is still POLLING belongs to that page: the connection is the ownership
// signal, and a resume while it is held is refused. What is NOT refused is a mount whose
// request the gateway has merely not seen die yet — see
// TestResumeTakesOverAConnectionTheGatewayHasNotSeenDie, which is the case that matters
// when a page's transport breaks.
func TestResumeRefusesAMountAnotherPageIsPolling(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polled := make(chan error, 1)
	go func() { _, err := s.dispatch(ctx, "poll", tenant, "owner", payload{Token: sh.token}); polled <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		polling := sh.polling
		sh.mu.Unlock()
		if polling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poll never took the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.dispatch(context.Background(), "resume", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("resume took over a mount another page is polling")
	} else if !strings.Contains(err.Error(), "another page") {
		t.Fatalf("resume refused a polled mount for the wrong reason: %v", err)
	}
	if m.closed {
		t.Fatal("the refused resume disturbed the live mount")
	}
	if paths := s.MountsFor(tenant.Name); len(paths) != 1 {
		t.Fatalf("the refused resume unadvertised the live mount: %v", paths)
	}
	sh.mu.Lock()
	revoked := sh.revoked
	sh.mu.Unlock()
	if revoked {
		t.Fatal("a refused resume revoked the page that owns the mount")
	}
	cancel()
	<-polled
}

// The reload path end to end through the HTTP surface, with the gateway's answer visible:
// the poll that dies keeps the capability, and the page's resume gets the same mount point.
func TestResumeOverTheWire(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeTenant(w, r, tenant, "owner")
	}))
	defer server.Close()

	post := func(endpoint, body string, timeout time.Duration) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/browser-workspace/"+endpoint,
			strings.NewReader(body))
		if err != nil {
			return "", err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		var buffer [4096]byte
		n, _ := response.Body.Read(buffer[:])
		return string(buffer[:n]), nil
	}

	// The poll is the request that dies with the page: its context ends it, so the client
	// reports a transport error. That error is the reload.
	if _, err := post("poll", `{"token":"`+sh.token+`"}`, 250*time.Millisecond); err == nil {
		t.Fatal("the poll returned instead of dying with its client")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sh.mu.Lock()
		waiting := sh.awaitingResume()
		sh.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	answer, err := post("resume", `{"token":"`+sh.token+`"}`, 10*time.Second)
	if err != nil {
		t.Fatalf("resume over HTTP: %v", err)
	}
	if !strings.Contains(answer, `"ok":true`) || !strings.Contains(answer, sh.path) {
		t.Fatalf("resume answered %s", answer)
	}
	if paths := s.MountsFor(tenant.Name); !reflect.DeepEqual(paths, []string{sh.path}) {
		t.Fatalf("mounts after resume = %v", paths)
	}
}

// The real race behind "the page cannot reconnect after its own transport broke": the
// browser gives up on the parked poll, but the gateway has not processed that death yet, so
// its last sight of the page is a live connection. A resume that arrives in that window must
// still work — the page is asking for its OWN mount back, and refusing it because a request
// nobody will ever answer is still parked is exactly how a recoverable blip became
// "已断线，请重新选择目录".
func TestResumeTakesOverAConnectionTheGatewayHasNotSeenDie(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	polled := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := s.dispatch(ctx, "poll", tenant, "owner", payload{Token: sh.token}); polled <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		polling := sh.polling
		sh.mu.Unlock()
		if polling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poll never took the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The browser walks away from the request. Until the gateway notices, it still holds the
	// connection — and in that window a resume is refused, because the mount is being served.
	cancel()
	if _, err := s.dispatch(context.Background(), "resume", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("a resume took a mount whose poll connection was still claimed")
	}
	// The death lands: the handler releases the connection and starts the grace window.
	select {
	case err := <-polled:
		if err == nil {
			t.Fatal("the abandoned poll answered instead of ending")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned poll never ended")
	}
	sh.mu.Lock()
	waiting, polling := sh.awaitingResume(), sh.polling
	sh.mu.Unlock()
	if !waiting || polling {
		t.Fatalf("the mount is not waiting for its page: waiting=%v polling=%v", waiting, polling)
	}
	value, err := s.dispatch(context.Background(), "resume", tenant, "owner", payload{Token: sh.token})
	if err != nil {
		t.Fatalf("the page could not take its own mount back: %v", err)
	}
	if value.(map[string]any)["mountpoint"] != sh.path {
		t.Fatalf("the reconnect moved the mount: %v", value)
	}
	if mounts := s.MountsFor(tenant.Name); len(mounts) != 1 || mounts[0] != sh.path {
		t.Fatalf("the reconnected mount is not being served again: %v", mounts)
	}
	if m.closed {
		t.Fatal("the reconnect released the kernel mount")
	}
	// And the page's loop can serve again: a queued operation gets answered.
	answer := make(chan error, 1)
	go func() { _, err := sh.Call(context.Background(), fs.Request{Op: "stat", Path: ""}); answer <- err }()
	request := <-sh.queue
	if _, err := s.dispatch(context.Background(), "respond", tenant, "owner",
		payload{Token: sh.token, ID: request.ID, Result: fs.Response{OK: true, Value: fs.Value{Kind: "directory"}}}); err != nil {
		t.Fatal(err)
	}
	if err := <-answer; err != nil {
		t.Fatalf("the reconnected page could not serve: %v", err)
	}
}

// Only one browser side may hold a mount's poll connection: a second poll from a stale page
// would let two pages answer the same FUSE requests.
func TestSecondPollOnTheSameMountIsRefused(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := s.dispatch(ctx, "poll", tenant, "owner", payload{Token: sh.token}); first <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		polling := sh.polling
		sh.mu.Unlock()
		if polling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first poll never took the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.dispatch(context.Background(), "poll", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("two pages were allowed to serve the same mount")
	}
	cancel()
	<-first
}

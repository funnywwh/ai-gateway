package nodeplane

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// workerStub is a stand-in for a tenant's dsh: it records the request it saw and answers.
type workerStub struct {
	requests []*http.Request
	respond  func(w http.ResponseWriter, r *http.Request)
}

func (s *workerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests = append(s.requests, r.Clone(context.Background()))
	if s.respond != nil {
		s.respond(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_, _ = io.WriteString(w, "dsh shell")
}

// planeFixture wires a Plane against a real registry and a stand-in worker on a loopback port.
func planeFixture(t *testing.T) (*Plane, registry.Tenant, *workerStub) {
	t.Helper()
	worker := &workerStub{}
	server := httptest.NewServer(worker)
	t.Cleanup(server.Close)
	host, port := hostPort(t, server.URL)

	root := t.TempDir()
	cfg := &config.Config{StateDir: root, RegistryPath: root + "/registry.json", KeyMapPath: root + "/keys.map"}
	tenant := registry.Tenant{
		Name: "alice", UID: 1000, PublicPort: 32601, WorkerPort: port,
		DshHome: root + "/tenants/alice/.dsh", Workspace: root + "/srv/alice",
		CreatedAt: time.Now().UTC(), Handshake: registry.HandshakeOK, Isolation: registry.IsolationBwrap,
	}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	plane := &Plane{Config: cfg, Registry: reg, Workers: statusStub{running: true}}
	_ = host
	return plane, tenant, worker
}

// statusStub answers the one question the plane asks its lifecycle layer.
type statusStub struct{ running bool }

func (s statusStub) Status(context.Context, registry.Tenant) (tenancy.WorkerState, error) {
	return tenancy.WorkerState{Running: s.running}, nil
}

func hostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	value := 0
	_, err = fmt.Sscanf(port, "%d", &value)
	if err != nil {
		t.Fatal(err)
	}
	return host, value
}

// TestServeTenantRestoresThePathAndPresentsTheAuthority is the invariant the whole multi-machine
// design rests on: the worker sees the authority its cookie was handshaken with, plus the tenant's
// own path.
func TestServeTenantRestoresThePathAndPresentsTheAuthority(t *testing.T) {
	plane, tenant, worker := planeFixture(t)
	request := httptest.NewRequest(http.MethodGet, nodeproto.TenantPath+"api/session?page=2", nil)
	request.Host = "node-a.example:18400"
	request.Header.Set(nodeproto.HeaderTenant, tenant.Name)
	request.Header.Set(nodeproto.HeaderProtocol, nodeproto.ProtocolHeaderValue)
	request.Header.Set("Authorization", "Bearer node-token")
	request.Header.Set("Cookie", "dsh-auth-test=held")
	recorder := httptest.NewRecorder()
	plane.ServeTenant(recorder, request, tenant.Name, "session-digest")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "dsh shell" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
	if len(worker.requests) != 1 {
		t.Fatalf("worker requests = %d", len(worker.requests))
	}
	seen := worker.requests[0]
	authority := fmt.Sprintf("127.0.0.1:%d", tenant.WorkerPort)
	if seen.Host != authority {
		t.Fatalf("worker Host = %q, want %q (the handshake authority)", seen.Host, authority)
	}
	if seen.URL.Path != "/api/session" || seen.URL.RawQuery != "page=2" {
		t.Fatalf("worker path = %q query = %q", seen.URL.Path, seen.URL.RawQuery)
	}
	if seen.Header.Get("Cookie") != "dsh-auth-test=held" {
		t.Fatalf("worker cookie = %q: the credential must survive this hop", seen.Header.Get("Cookie"))
	}
	for _, header := range []string{nodeproto.HeaderTenant, nodeproto.HeaderProtocol, "Authorization"} {
		if value := seen.Header.Get(header); value != "" {
			t.Fatalf("control header %s reached the worker: %q", header, value)
		}
	}
}

// TestRestorePathKeepsEscaping: a path with an encoded slash must not be re-encoded on the way to
// the worker, or dsh's plugin routes stop resolving.
func TestRestorePathKeepsEscaping(t *testing.T) {
	cases := []struct {
		path    string
		rawPath string
		want    string
		wantRaw string
	}{
		{"/node/v1/tenant/api", "", "/api", ""},
		// The decoded path and the original escaping travel together in a real request: Go's server
		// fills Path (decoded) and RawPath (as sent). Trimming both the same way is what keeps an
		// encoded slash from being re-encoded into a different path.
		{"/node/v1/tenant/plugins/a/b.js", "/node/v1/tenant/plugins/a%2Fb.js", "/plugins/a/b.js", "/plugins/a%2Fb.js"},
		{"/node/v1/tenant/", "", "/", ""},
		{"/unrelated/path", "", "/unrelated/path", ""},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.URL.Path = tc.path
		request.URL.RawPath = tc.rawPath
		restorePath(request)
		if request.URL.Path != tc.want || request.URL.RawPath != tc.wantRaw {
			t.Errorf("restorePath(%q, %q) = (%q, %q), want (%q, %q)",
				tc.path, tc.rawPath, request.URL.Path, request.URL.RawPath, tc.want, tc.wantRaw)
		}
	}
}

func TestUnknownTenantIsRefused(t *testing.T) {
	plane, _, worker := planeFixture(t)
	request := httptest.NewRequest(http.MethodGet, nodeproto.TenantPath+"api", nil)
	recorder := httptest.NewRecorder()
	plane.ServeTenant(recorder, request, "bob", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	if code := recorder.Header().Get(nodeproto.HeaderError); code != nodeproto.CodeTenantUnknown {
		t.Fatalf("%s = %q", nodeproto.HeaderError, code)
	}
	if len(worker.requests) != 0 {
		t.Fatal("an unknown tenant's request reached a worker")
	}
}

func TestWorkerNotRunningIsRefused(t *testing.T) {
	plane, tenant, worker := planeFixture(t)
	// A suspended tenant's worker is not running: the plane asks and gets "no".
	plane.Workers = statusStub{running: false}
	request := httptest.NewRequest(http.MethodGet, nodeproto.TenantPath+"api", nil)
	recorder := httptest.NewRecorder()
	plane.ServeTenant(recorder, request, tenant.Name, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if code := recorder.Header().Get(nodeproto.HeaderError); code != nodeproto.CodeWorkerNotRunning {
		t.Fatalf("%s = %q", nodeproto.HeaderError, code)
	}
	if len(worker.requests) != 0 {
		t.Fatal("a stopped worker was still contacted")
	}
}

// TestWorkerCannotSpeakTheNodeErrorChannel: only the node may set X-Dshgw-Error, or a tenant plugin
// could have its own response replaced by a gateway error.
func TestWorkerCannotSpeakTheNodeErrorChannel(t *testing.T) {
	plane, tenant, worker := planeFixture(t)
	worker.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(nodeproto.HeaderError, nodeproto.CodeAuthFailed)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "tenant content")
	}
	request := httptest.NewRequest(http.MethodGet, nodeproto.TenantPath+"api", nil)
	recorder := httptest.NewRecorder()
	plane.ServeTenant(recorder, request, tenant.Name, "")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "tenant content" {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if code := recorder.Header().Get(nodeproto.HeaderError); code != "" {
		t.Fatalf("the worker's %s survived: %q", nodeproto.HeaderError, code)
	}
}

// TestWebSocketUpgradePassesThrough proves the node hop keeps upgraded connections working: the
// dsh UI and its plugins are useless without it.
func TestWebSocketUpgradePassesThrough(t *testing.T) {
	plane, tenant, worker := planeFixture(t)
	worker.respond = func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		buffer := make([]byte, 4)
		if _, err := io.ReadFull(conn, buffer); err != nil {
			return
		}
		_, _ = conn.Write(buffer)
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plane.ServeTenant(w, r, tenant.Name, "")
	}))
	t.Cleanup(node.Close)

	host := strings.TrimPrefix(node.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %sbrowser-fs/ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCookie: dsh-auth-test=held\r\n\r\n", nodeproto.TenantPath, host)
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		rest, _ := io.ReadAll(reader)
		t.Fatalf("status=%q err=%v rest=%s", status, err, rest)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo = %q err = %v", echo, err)
	}
	if len(worker.requests) == 0 || worker.requests[0].Header.Get("Upgrade") != "websocket" {
		t.Fatalf("the upgrade did not reach the worker: %+v", worker.requests)
	}
}

// TestBrowserMountsAreAnsweredHereWhenPresent: the long poll belongs to the process that owns the
// mount, and this node is that process.
func TestBrowserMountsAreAnsweredHereWhenPresent(t *testing.T) {
	plane, tenant, worker := planeFixture(t)
	mount := &mountRecorder{}
	plane.Browser = mount
	request := httptest.NewRequest(http.MethodGet, nodeproto.TenantPath+"browser-workspace/poll", nil)
	recorder := httptest.NewRecorder()
	plane.ServeTenant(recorder, request, tenant.Name, "digest-123")
	if mount.served != 1 || mount.session != "digest-123" {
		t.Fatalf("mount calls = %d session = %q", mount.served, mount.session)
	}
	if len(worker.requests) != 0 {
		t.Fatal("the long poll was also forwarded to the worker")
	}
	// Without the mount service the path is forwarded: a node that does not run mounts must not
	// pretend the tenant has one.
	plane.Browser = nil
	recorder = httptest.NewRecorder()
	plane.ServeTenant(recorder, request, tenant.Name, "digest-123")
	if len(worker.requests) != 1 {
		t.Fatalf("worker requests = %d, want the path forwarded", len(worker.requests))
	}
}

type mountRecorder struct {
	served  int
	session string
}

func (m *mountRecorder) ServeTenant(w http.ResponseWriter, r *http.Request, t registry.Tenant, session string) {
	m.served++
	m.session = session
	_, _ = io.WriteString(w, "mount")
}

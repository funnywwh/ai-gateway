package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

// The multi-machine tests (M77) reuse the package's single-machine fixture and swap in a fake
// node: the tenant is placed on "node-a", the proxy must address the node, and the loopback worker
// the fixture built must never be contacted. That last part is asserted by making the worker
// handler fail the test if it is reached.

// fakeNodes is the proxy's node seam (M77), driven by hand.
type fakeNodes struct {
	node   NodeRef
	remote bool
	serves bool
	fail   error

	mu        sync.Mutex
	handshake int
}

func (f *fakeNodes) NodeFor(t registry.Tenant) (NodeRef, bool) {
	if !f.remote || t.Node != f.node.Name {
		return NodeRef{}, false
	}
	return f.node, true
}

func (f *fakeNodes) Handshake(ctx context.Context, t registry.Tenant, authority string) (*session.Upstream, error) {
	f.mu.Lock()
	f.handshake++
	f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	return &session.Upstream{Name: "dsh-auth-test", Value: "worker-cookie", Authority: authority}, nil
}

func (f *fakeNodes) ServesBrowserWorkspaces(registry.Tenant) bool { return f.serves }

func (f *fakeNodes) handshakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handshake
}

// nodePlane records what the node receives and answers like a node agent would.
type nodePlane struct {
	mu       sync.Mutex
	requests []nodeRequest
	respond  func(w http.ResponseWriter, r *http.Request)
}

type nodeRequest struct {
	method string
	host   string
	path   string
	query  string
	header http.Header
}

func (n *nodePlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.requests = append(n.requests, nodeRequest{
		method: r.Method, host: r.Host, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(),
	})
	respond := n.respond
	n.mu.Unlock()
	if respond != nil {
		respond(w, r)
		return
	}
	w.Header().Set("X-From-Node", "yes")
	_, _ = io.WriteString(w, "answered by the node")
}

func (n *nodePlane) seen() []nodeRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]nodeRequest(nil), n.requests...)
}

// remoteFixture places the fixture's tenant on a fake node and returns both sides.
func remoteFixture(t *testing.T) (*Proxy, registry.Tenant, *nodePlane, *fakeNodes) {
	t.Helper()
	workerReached := false
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerReached = true
		http.Error(w, "the control plane dialled the worker directly", http.StatusTeapot)
	}))
	t.Cleanup(up.Close)
	tenant.Node = "node-a"
	if err := p.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := p.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if workerReached {
			t.Errorf("a remote tenant's traffic reached this machine's loopback worker")
		}
	})

	plane := &nodePlane{}
	node := httptest.NewServer(plane)
	t.Cleanup(node.Close)
	nodes := &fakeNodes{node: NodeRef{Name: "node-a", BaseURL: node.URL, Token: "node-token"}, remote: true}
	p.Nodes = nodes
	return p, tenant, plane, nodes
}

// tenantRequest builds the request a browser would send to the tenant's own origin.
func remoteTenantRequest(p *Proxy, tenant registry.Tenant, token, method, path string, headers map[string]string) *http.Request {
	origin := "https://dsh.test:" + strconv.Itoa(tenant.PublicPort)
	req := httptest.NewRequest(method, path, nil)
	req.Host = "dsh.test:" + strconv.Itoa(tenant.PublicPort)
	req.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
	req.Header.Set("Origin", origin)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req
}

func callTenant(p *Proxy, tenant registry.Tenant, token, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	p.TenantHandler(tenant).ServeHTTP(recorder, remoteTenantRequest(p, tenant, token, method, path, headers))
	return recorder
}

// TestRemoteTenantIsForwardedToItsNode is the core of the multi-machine data plane.
func TestRemoteTenantIsForwardedToItsNode(t *testing.T) {
	p, tenant, plane, nodes := remoteFixture(t)
	token := issue(t, p, tenant.Name, nil)

	recorder := callTenant(p, tenant, token, http.MethodGet, "/api/session?page=2", map[string]string{
		// Forgeries: every one of these must be replaced by what the control plane itself writes.
		nodeproto.HeaderTenant:   "bob",
		nodeproto.HeaderProtocol: "99",
		"Authorization":          "Bearer forged-browser-token",
		nodeproto.HeaderError:    "worker_not_running",
		"X-Forwarded-For":        "203.0.113.7",
		// A legitimate fetch-metadata value: the fence accepts it, and it still must not travel.
		"Sec-Fetch-Site": "same-origin",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-From-Node") != "yes" {
		t.Fatalf("the answer did not come from the node: %+v", recorder.Header())
	}
	if nodes.handshakes() != 1 {
		t.Fatalf("handshakes = %d: the node must perform it", nodes.handshakes())
	}
	seen := plane.seen()
	if len(seen) != 1 {
		t.Fatalf("the node received %d requests", len(seen))
	}
	got := seen[0]
	if !strings.HasPrefix(got.host, "127.0.0.1:") {
		t.Fatalf("host = %q: the node is addressed as itself", got.host)
	}
	if got.path != nodeproto.TenantPath+"api/session" || got.query != "page=2" {
		t.Fatalf("path = %q query = %q", got.path, got.query)
	}
	if got.header.Get(nodeproto.HeaderTenant) != tenant.Name {
		t.Fatalf("%s = %q: the browser's forgery must be overwritten", nodeproto.HeaderTenant, got.header.Get(nodeproto.HeaderTenant))
	}
	if got.header.Get(nodeproto.HeaderProtocol) != nodeproto.ProtocolHeaderValue {
		t.Fatalf("protocol header = %q", got.header.Get(nodeproto.HeaderProtocol))
	}
	if got.header.Get("Authorization") != "Bearer node-token" {
		t.Fatalf("authorization = %q: the node token must replace the browser's", got.header.Get("Authorization"))
	}
	if cookie := got.header.Get("Cookie"); cookie != "dsh-auth-test=worker-cookie" {
		t.Fatalf("cookie = %q: only the worker credential may travel", cookie)
	}
	// The browser's own credentials and the headers that describe the browser's context must not
	// travel: the node's request is the gateway's, not the browser's. (Headers with no such meaning
	// — Accept, User-Agent, a tenant's own X-* — travel exactly as they do in a single-machine
	// deployment; stripping them would change what dsh serves.)
	if strings.Contains(got.header.Get("Cookie"), "dshgw_s_") {
		t.Fatalf("the browser session cookie reached the node: %q", got.header.Get("Cookie"))
	}
	if got.header.Get("X-Forwarded-For") != "" {
		t.Fatalf("X-Forwarded-For reached the node: %q", got.header.Get("X-Forwarded-For"))
	}
	if got.header.Get("Sec-Fetch-Site") != "" {
		t.Fatalf("Sec-Fetch-Site reached the node: %q", got.header.Get("Sec-Fetch-Site"))
	}
	if got.header.Get(nodeproto.HeaderError) != "" {
		t.Fatalf("a browser-supplied %s reached the node", nodeproto.HeaderError)
	}
}

// TestRemoteHandshakeIsDelegatedAndRetried: the worker refuses the cached cookie, so the control
// plane asks the node for a fresh one and retries exactly once.
func TestRemoteHandshakeIsDelegatedAndRetried(t *testing.T) {
	p, tenant, plane, nodes := remoteFixture(t)
	token := issue(t, p, tenant.Name, nil)
	var attempts int
	plane.respond = func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "second attempt")
	}
	recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("worker attempts = %d, want 2", attempts)
	}
	if nodes.handshakes() != 2 {
		t.Fatalf("handshakes = %d, want 2", nodes.handshakes())
	}
}

// TestNodeRefusalBecomesAGatewayAnswer: the browser gets our status and our wording, never the
// node's internal message.
func TestNodeRefusalBecomesAGatewayAnswer(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		status int
		want   int
		text   string
	}{
		{"worker not running", nodeproto.CodeWorkerNotRunning, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "worker is not running"},
		{"unknown tenant", nodeproto.CodeTenantUnknown, http.StatusNotFound, http.StatusNotFound, "unknown tenant"},
		{"not implemented", nodeproto.CodeNotImplemented, http.StatusNotImplemented, http.StatusServiceUnavailable, "does not support"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, tenant, plane, _ := remoteFixture(t)
			token := issue(t, p, tenant.Name, nil)
			plane.respond = func(w http.ResponseWriter, r *http.Request) {
				nodeproto.WriteTenantError(w, tc.status, tc.code, "node-a internals: /srv/secret")
			}
			recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
			if !strings.Contains(recorder.Body.String(), tc.text) {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), tc.text)
			}
			if strings.Contains(recorder.Body.String(), "node-a internals") {
				t.Fatalf("the node's wording reached the browser: %q", recorder.Body.String())
			}
		})
	}
}

// TestUnreachableNodeIs503 covers both discoveries of a dead node: the handshake (a fresh session
// makes a control call) and a cached cookie (nothing calls the control plane, so only the
// transport notices).
func TestUnreachableNodeIs503(t *testing.T) {
	t.Run("during the handshake", func(t *testing.T) {
		p, tenant, _, nodes := remoteFixture(t)
		nodes.fail = nodeproto.Errorf(nodeproto.CodeUnreachable, "node node-a is unreachable: dial tcp: connection refused")
		token := issue(t, p, tenant.Name, nil)
		recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (the upstream is our own node, not a broken worker)", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "unreachable") {
			t.Fatalf("body = %q", recorder.Body.String())
		}
	})
	t.Run("with a cached cookie", func(t *testing.T) {
		p, tenant, _, nodes := remoteFixture(t)
		token := issue(t, p, tenant.Name, nil)
		if recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil); recorder.Code != http.StatusOK {
			t.Fatalf("warm-up status = %d", recorder.Code)
		}
		// The node goes away; the session still holds a live cookie, so no control call happens.
		nodes.node.BaseURL = "http://127.0.0.1:1"
		recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "node is unreachable") {
			t.Fatalf("body = %q", recorder.Body.String())
		}
		if nodes.handshakes() != 1 {
			t.Fatalf("handshakes = %d, want 1: the cached cookie skipped the control call", nodes.handshakes())
		}
	})
}

// TestRemoteTenantWithoutNodeClientsIsRefused is the fail-closed case: a tenant recorded on a node
// in a process that has no node clients must not fall back to loopback.
func TestRemoteTenantWithoutNodeClientsIsRefused(t *testing.T) {
	p, tenant, plane, _ := remoteFixture(t)
	p.Nodes = nil
	token := issue(t, p, tenant.Name, nil)
	recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if len(plane.seen()) != 0 {
		t.Fatal("the node was contacted")
	}
}

func TestRemoteTenantOnAnUndefinedNodeIsRefused(t *testing.T) {
	p, tenant, _, _ := remoteFixture(t)
	tenant.Node = "node-z"
	if err := p.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	token := issue(t, p, tenant.Name, nil)
	recorder := callTenant(p, tenant, token, http.MethodGet, "/api", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "not available") {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

// TestBrowserWorkspaceFollowsTheMount: the long poll is answered here only when the mount is here.
func TestBrowserWorkspaceFollowsTheMount(t *testing.T) {
	t.Run("remote tenant forwards it to the node", func(t *testing.T) {
		p, tenant, plane, _ := remoteFixture(t)
		mount := &mountStub{}
		p.BrowserWorkspaces = mount
		token := issue(t, p, tenant.Name, nil)
		recorder := callTenant(p, tenant, token, http.MethodGet, "/browser-workspace/poll", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if mount.served != 0 {
			t.Fatal("the control plane answered a remote tenant's long poll")
		}
		seen := plane.seen()
		if len(seen) != 1 || seen[0].path != nodeproto.TenantPath+"browser-workspace/poll" {
			t.Fatalf("node requests = %+v", seen)
		}
		// The digest is the node's only way to know which browser session the poll belongs to.
		if seen[0].header.Get(nodeproto.HeaderBrowserSession) == "" {
			t.Fatal("the browser-session digest did not reach the node")
		}
	})
	t.Run("local tenant is answered here", func(t *testing.T) {
		p, tenant, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the worker must not see the long poll", http.StatusTeapot)
		}))
		t.Cleanup(up.Close)
		mount := &mountStub{}
		p.BrowserWorkspaces = mount
		token := issue(t, p, tenant.Name, nil)
		recorder := callTenant(p, tenant, token, http.MethodGet, "/browser-workspace/poll", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if mount.served != 1 {
			t.Fatalf("the local mount service was not used (%d)", mount.served)
		}
	})
}

type mountStub struct {
	served   int
	sessions []string
}

func (m *mountStub) ServeTenant(w http.ResponseWriter, r *http.Request, t registry.Tenant, sessionID string) {
	m.served++
	m.sessions = append(m.sessions, sessionID)
	_, _ = io.WriteString(w, "mount answered")
}

// TestRemoteStreamingIsNotBuffered checks the property that makes SSE work through the extra hop:
// the node's bytes reach the client while the node is still holding the response open.
func TestRemoteStreamingIsNotBuffered(t *testing.T) {
	p, tenant, plane, _ := remoteFixture(t)
	token := issue(t, p, tenant.Name, nil)
	released := make(chan struct{})
	plane.respond = func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: one\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-released
		_, _ = io.WriteString(w, "event: two\n\n")
	}
	recorder := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.TenantHandler(tenant).ServeHTTP(recorder, remoteTenantRequest(p, tenant, token, http.MethodGet, "/api/stream", nil))
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(recorder.body(), "event: one") {
		time.Sleep(20 * time.Millisecond)
	}
	first := strings.Contains(recorder.body(), "event: one")
	close(released)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the stream never finished")
	}
	if !first {
		t.Fatalf("the first event was withheld until the stream ended: %q", recorder.body())
	}
	if !strings.Contains(recorder.body(), "event: two") {
		t.Fatalf("the second event is missing: %q", recorder.body())
	}
}

// streamRecorder is a ResponseWriter that can be read while a handler is still writing to it.
type streamRecorder struct {
	mu     sync.Mutex
	header http.Header
	status int
	body_  strings.Builder
	done   chan struct{}
	once   sync.Once
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: http.Header{}, done: make(chan struct{})}
}

func (s *streamRecorder) Header() http.Header { return s.header }

func (s *streamRecorder) WriteHeader(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		s.status = status
	}
}

func (s *streamRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.body_.Write(p)
	s.mu.Unlock()
	return len(p), nil
}

func (s *streamRecorder) Flush() {}

func (s *streamRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body_.String()
}

// TestNodeRefusalClassification pins the two error classes the error handler distinguishes.
func TestNodeRefusalClassification(t *testing.T) {
	sentinel := &nodeRefusal{code: nodeproto.CodeWorkerNotRunning, status: http.StatusServiceUnavailable}
	var target *nodeRefusal
	if !errors.As(error(sentinel), &target) {
		t.Fatal("the sentinel does not survive errors.As, which ReverseProxy relies on")
	}
	if isNodeTransportFailure(sentinel) {
		t.Fatal("a node's refusal is not a transport failure")
	}
	transportErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if !isNodeTransportFailure(transportErr) {
		t.Fatal("a dial failure must classify as a transport failure")
	}
	if isNodeTransportFailure(io.EOF) {
		t.Fatal("a plain EOF must not be reported as an unreachable node")
	}
	if status, message := nodeRefusalMessage(nodeproto.CodeWorkerNotRunning); status != http.StatusServiceUnavailable || !strings.Contains(message, "worker") {
		t.Fatalf("message = %q (%d)", message, status)
	}
	if _, message := nodeRefusalMessage("something_else"); !strings.Contains(message, "refused") {
		t.Fatalf("unknown codes must still produce a usable message, got %q", message)
	}
	// A local tenant's worker failure keeps returning 502: the upstream there is a real process.
	if _, err := net.ResolveTCPAddr("tcp", "127.0.0.1:1"); err != nil {
		t.Skip("no loopback networking")
	}
}

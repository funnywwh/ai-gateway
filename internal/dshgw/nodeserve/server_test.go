package nodeserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
)

const testToken = "0123456789abcdef0123456789abcdef"

type testPlane struct {
	calls []planeCall
}

type planeCall struct {
	tenant  string
	session string
	path    string
	method  string
}

func (p *testPlane) ServeTenant(w http.ResponseWriter, r *http.Request, tenant string, session string) {
	p.calls = append(p.calls, planeCall{tenant: tenant, session: session, path: r.URL.Path, method: r.Method})
	w.Header().Set("X-Test-Tenant", tenant)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("worker said hi"))
}

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "node-a"
	}
	if opts.Token == "" {
		opts.Token = testToken
	}
	if opts.Version == "" {
		opts.Version = "9.9.9"
		opts.Revision = "abc1234"
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now().UTC()
	}
	server, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func do(t *testing.T, handler http.Handler, method, path, token string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) nodeproto.Envelope {
	t.Helper()
	env, err := nodeproto.DecodeEnvelope(recorder.Body)
	if err != nil {
		t.Fatalf("decode envelope: %v (body %q)", err, recorder.Body.String())
	}
	return env
}

func TestConstructionRequiresNameAndToken(t *testing.T) {
	if _, err := New(Options{Token: testToken}); err == nil {
		t.Fatal("a node without a name must not build")
	}
	if _, err := New(Options{Name: "node-a"}); err == nil {
		t.Fatal("a node without a token must not build: that listener is open to the LAN")
	}
}

// TestGateRunsBeforeRouting is the security property this listener stands on: an
// unauthenticated caller learns nothing, not even whether a path exists.
func TestGateRunsBeforeRouting(t *testing.T) {
	plane := &testPlane{}
	server := newTestServer(t, Options{Tenants: plane})

	cases := []struct {
		name   string
		token  string
		path   string
		method string
	}{
		{"no token on health", "", nodeproto.HealthPath, http.MethodGet},
		{"wrong token on health", "nope", nodeproto.HealthPath, http.MethodGet},
		{"wrong token on control", "nope", nodeproto.ControlPath + "ping", http.MethodPost},
		{"wrong token on tenant", "nope", nodeproto.TenantPath + "api/session", http.MethodGet},
		{"wrong token on unknown path", "nope", "/whatever", http.MethodGet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := do(t, server.Handler(), tc.method, tc.path, tc.token, "", map[string]string{nodeproto.HeaderTenant: "alice"})
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			env := decode(t, recorder)
			if env.OK || env.Error == nil || env.Error.Code != nodeproto.CodeAuthFailed {
				t.Fatalf("envelope = %+v", env)
			}
			if recorder.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("a 401 should say how to authenticate")
			}
		})
	}
	if len(plane.calls) != 0 {
		t.Fatalf("tenant traffic reached the plane without a token: %+v", plane.calls)
	}
}

func TestProtocolHeaderMismatch(t *testing.T) {
	server := newTestServer(t, Options{})
	recorder := do(t, server.Handler(), http.MethodGet, nodeproto.HealthPath, testToken, "",
		map[string]string{nodeproto.HeaderProtocol: "2"})
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", recorder.Code)
	}
	env := decode(t, recorder)
	if env.Error == nil || env.Error.Code != nodeproto.CodeProtocolMismatch {
		t.Fatalf("envelope = %+v", env)
	}
	if !strings.Contains(env.Error.Message, "2") {
		t.Fatalf("the message should name the announced version: %q", env.Error.Message)
	}
}

func TestHealthShape(t *testing.T) {
	server := newTestServer(t, Options{})
	recorder := do(t, server.Handler(), http.MethodGet, nodeproto.HealthPath, testToken, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	env := decode(t, recorder)
	var health nodeproto.Health
	if err := json.Unmarshal(env.Value, &health); err != nil {
		t.Fatal(err)
	}
	if health.Name != "node-a" || health.Protocol != nodeproto.Version || health.Revision != "abc1234" || health.Version != "9.9.9" {
		t.Fatalf("health = %+v", health)
	}
	if health.StartedAt.IsZero() {
		t.Fatal("health must report when the node started")
	}
	if recorder := do(t, server.Handler(), http.MethodPost, nodeproto.HealthPath, testToken, "", nil); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST health status = %d, want 405", recorder.Code)
	}
}

// TestHealthCarriesStatusCounts proves health is enriched from the node's own status
// operation, which is what lets the control plane notice drift from one endpoint.
func TestHealthCarriesStatusCounts(t *testing.T) {
	server := newTestServer(t, Options{Ops: map[string]Handler{
		"status": func(context.Context, []byte) (any, error) {
			return nodeproto.Status{
				Health: nodeproto.Health{Tenants: 3, Running: 2, Suspended: 1},
				Tenants: []nodeproto.TenantState{
					{Name: "alice", WorkerPort: 32100, Running: true},
					{Name: "bob", WorkerPort: 32101},
				},
			}, nil
		},
	}})
	recorder := do(t, server.Handler(), http.MethodGet, nodeproto.HealthPath, testToken, "", nil)
	var health nodeproto.Health
	if err := json.Unmarshal(decode(t, recorder).Value, &health); err != nil {
		t.Fatal(err)
	}
	if health.Tenants != 3 || health.Running != 2 || health.Suspended != 1 {
		t.Fatalf("health = %+v", health)
	}
	// The build identity stays this process's, even when the status operation reports its own.
	if health.Name != "node-a" || health.Protocol != nodeproto.Version || health.Revision != "abc1234" {
		t.Fatalf("health = %+v", health)
	}

	// The status operation must describe the same node as /health: a control plane reading one
	// endpoint and an operator reading the other must not see two different machines.
	recorder = do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"status", testToken, "", nil)
	var status nodeproto.Status
	if err := json.Unmarshal(decode(t, recorder).Value, &status); err != nil {
		t.Fatal(err)
	}
	if status.Health.Name != "node-a" || status.Health.Protocol != nodeproto.Version ||
		status.Health.Revision != "abc1234" || status.Health.Tenants != 3 || status.Health.Running != 2 {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Tenants) != 2 || status.Tenants[0].Name != "alice" {
		t.Fatalf("status tenants = %+v", status.Tenants)
	}
}

// TestStatusIdentityOverlayLeavesOtherShapesAlone documents the wrapper's boundary: a status
// handler returning something unexpected is passed through rather than reinterpreted, and the
// health endpoint still answers from the build's own half.
func TestStatusIdentityOverlayLeavesOtherShapesAlone(t *testing.T) {
	server := newTestServer(t, Options{Ops: map[string]Handler{
		"status": func(context.Context, []byte) (any, error) {
			return map[string]any{"unexpected": true}, nil
		},
	}})
	recorder := do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"status", testToken, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "unexpected") {
		t.Fatalf("body = %q", body)
	}
	// /health cannot use that value, so it answers the build's own description with zero
	// tenant counts instead of failing.
	recorder = do(t, server.Handler(), http.MethodGet, nodeproto.HealthPath, testToken, "", nil)
	var health nodeproto.Health
	if err := json.Unmarshal(decode(t, recorder).Value, &health); err != nil {
		t.Fatal(err)
	}
	if health.Name != "node-a" || health.Protocol != nodeproto.Version || health.Tenants != 0 {
		t.Fatalf("health = %+v", health)
	}
}

func TestControlOperationDispatch(t *testing.T) {
	server := newTestServer(t, Options{Ops: map[string]Handler{
		"ping": func(context.Context, []byte) (any, error) { return nil, nil },
		"refuse": func(context.Context, []byte) (any, error) {
			return nil, nodeproto.Errorf(nodeproto.CodeTenantUnknown, "no such tenant")
		},
		"crash": func(context.Context, []byte) (any, error) { return nil, errors.New("boom") },
		"echo": func(_ context.Context, body []byte) (any, error) {
			var request struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				return nil, nodeproto.Errorf(nodeproto.CodeBadRequest, "bad request body")
			}
			return map[string]any{"name": request.Name}, nil
		},
	}})

	recorder := do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"ping", testToken, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ping status = %d", recorder.Code)
	}
	if env := decode(t, recorder); !env.OK || len(env.Value) == 0 {
		t.Fatalf("ping envelope = %+v", env)
	}

	// An op this build does not implement is reported as such rather than as a 404: the
	// control plane can then tell "older node build" from "wrong address".
	recorder = do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"missing", testToken, "", nil)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("missing op status = %d, want 501", recorder.Code)
	}
	if env := decode(t, recorder); env.Error == nil || env.Error.Code != nodeproto.CodeNotImplemented {
		t.Fatalf("missing op envelope = %+v", env)
	}

	// A code the caller branches on maps onto a status that carries the same meaning.
	recorder = do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"refuse", testToken, "", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("tenant_unknown status = %d, want 404", recorder.Code)
	}
	if env := decode(t, recorder); env.Error == nil || env.Error.Code != nodeproto.CodeTenantUnknown {
		t.Fatalf("envelope = %+v", env)
	}

	// An unexpected failure is an internal error, and its text is what the operator reads.
	recorder = do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"crash", testToken, "", nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("crash status = %d, want 500", recorder.Code)
	}
	if env := decode(t, recorder); env.Error == nil || env.Error.Code != nodeproto.CodeInternal || !strings.Contains(env.Error.Message, "boom") {
		t.Fatalf("crash envelope = %+v", env)
	}

	// A body that is not a JSON object is refused before the handler, so the handler never has
	// to report a syntax error as its own failure.
	recorder = do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"echo", testToken, "[1,2,3]", map[string]string{"Content-Type": "application/json"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("array body status = %d, want 400", recorder.Code)
	}
	recorder = do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"echo", testToken, `{"name":"alice"}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("echo status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "alice") {
		t.Fatalf("echo body = %q", body)
	}

	// GET on the control prefix is a protocol error, not a route: the surface is POST-only.
	if recorder := do(t, server.Handler(), http.MethodGet, nodeproto.ControlPath+"ping", testToken, "", nil); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET control status = %d, want 405", recorder.Code)
	}
}

func TestControlBodyLimit(t *testing.T) {
	server := newTestServer(t, Options{MaxControlBytes: 16, Ops: map[string]Handler{
		"ping": func(context.Context, []byte) (any, error) { return nil, nil },
	}})
	big := `{"name":"` + strings.Repeat("x", 64) + `"}`
	recorder := do(t, server.Handler(), http.MethodPost, nodeproto.ControlPath+"ping", testToken, big, nil)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
}

func TestUnknownPathAnswersInProtocolShape(t *testing.T) {
	server := newTestServer(t, Options{})
	recorder := do(t, server.Handler(), http.MethodGet, "/not-a-node-endpoint", testToken, "", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	env := decode(t, recorder)
	if env.Error == nil || env.Error.Code != nodeproto.CodeProtocolMismatch {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestTenantForwarding(t *testing.T) {
	plane := &testPlane{}
	server := newTestServer(t, Options{Tenants: plane})

	recorder := do(t, server.Handler(), http.MethodGet, nodeproto.TenantPath+"api/session", testToken, "",
		map[string]string{nodeproto.HeaderTenant: "alice", nodeproto.HeaderBrowserSession: "deadbeef"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "worker said hi" {
		t.Fatalf("body = %q", body)
	}
	if len(plane.calls) != 1 {
		t.Fatalf("calls = %+v", plane.calls)
	}
	call := plane.calls[0]
	if call.tenant != "alice" || call.session != "deadbeef" || call.method != http.MethodGet {
		t.Fatalf("call = %+v", call)
	}
	if !strings.HasSuffix(call.path, "/api/session") {
		t.Fatalf("the tenant's own path must survive: %q", call.path)
	}

	// Without a tenant name there is nothing to forward to: refuse rather than guess.
	recorder = do(t, server.Handler(), http.MethodGet, nodeproto.TenantPath+"api", testToken, "", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status without a tenant = %d, want 400", recorder.Code)
	}
}

// TestTenantForwardingWithoutPlane proves the P1 skeleton reports its own gap instead of
// pretending the tenant is gone.
func TestTenantForwardingWithoutPlane(t *testing.T) {
	server := newTestServer(t, Options{})
	recorder := do(t, server.Handler(), http.MethodGet, nodeproto.TenantPath+"api", testToken, "",
		map[string]string{nodeproto.HeaderTenant: "alice"})
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", recorder.Code)
	}
}

// TestStatusWithoutOperationReturnsPlainHealth proves health still answers when the node has
// no status operation registered (a partially wired node must not look dead).
func TestStatusWithoutOperationReturnsPlainHealth(t *testing.T) {
	server := newTestServer(t, Options{})
	healthExtra := do(t, server.Handler(), http.MethodGet, nodeproto.HealthPath, testToken, "", nil)
	var health nodeproto.Health
	if err := json.Unmarshal(decode(t, healthExtra).Value, &health); err != nil {
		t.Fatal(err)
	}
	if health.Tenants != 0 || health.Running != 0 {
		t.Fatalf("health = %+v", health)
	}
}

func TestServeBindsAndShutsDown(t *testing.T) {
	server := newTestServer(t, Options{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener, time.Second) }()

	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+nodeproto.HealthPath, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

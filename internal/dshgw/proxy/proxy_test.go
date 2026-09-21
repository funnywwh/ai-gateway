package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

type validatorFunc func(context.Context, string) ([]aigw.Model, error)

func (f validatorFunc) ValidateKey(ctx context.Context, key string) ([]aigw.Model, error) {
	return f(ctx, key)
}

type authorizerFunc func(context.Context, string) (string, error)

func (f authorizerFunc) Authorize(ctx context.Context, key string) (string, error) {
	return f(ctx, key)
}

// identityAuthorizer is an authorization client that also names the person (M67): one double
// answers both halves, exactly as the real aigw client does. calls records how many identity
// lookups happened, so a test can pin "once per tenant per TTL, not once per request".
type identityAuthorizer struct {
	account  string
	feishu   string
	failWith error
	calls    int
}

func (a *identityAuthorizer) Authorize(context.Context, string) (string, error) {
	if a.failWith != nil {
		return "", a.failWith
	}
	return "alice", nil
}

func (a *identityAuthorizer) Identity(context.Context, string) (aigw.Identity, error) {
	a.calls++
	if a.failWith != nil {
		return aigw.Identity{}, a.failWith
	}
	return aigw.Identity{Tenant: "alice", Account: a.account, FeishuName: a.feishu}, nil
}

// writeTenantKey puts a mode-0640 gateway.key where FileKeySource looks for it: the
// credential the proxy authenticates to aigw with.
func writeTenantKey(t *testing.T, root, tenant string) {
	t.Helper()
	dir := filepath.Join(root, tenant)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway.key"), []byte("sk-gw-tenant-key-0001\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

type sourceStub string

func (s sourceStub) TokenURL(string) (string, error) { return string(s), nil }

type exchangeStub struct {
	mu    sync.Mutex
	count int
	value string
}

func (e *exchangeStub) Exchange(_ context.Context, _ string, authority string) (*session.Upstream, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.count++
	return &session.Upstream{Name: "dsh-auth-test", Value: e.value, Authority: authority}, nil
}

func fixture(t *testing.T, worker http.Handler) (*Proxy, registry.Tenant, string, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(worker)
	u, _ := url.Parse(up.URL)
	_, rawPort, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(rawPort)
	dir := t.TempDir()
	cfg := &config.Config{PublicHost: "dsh.test", PortalPort: 32600, TenantPortLo: 32601, TenantPortHi: 32799, WorkerPortLo: port, WorkerPortHi: port, Listen: "127.0.0.1:3099", AigwBaseURL: "http://aigw.test", ValidateTimeout: config.Duration(time.Second), SessionTTL: config.Duration(time.Hour), KeyRevalidate: "off", LoginRate: config.RateLimit{Requests: 10, Window: config.Duration(time.Minute)}, DirectoryPicker: "clamp", PluginBrowserFS: "on", WorkspaceSeed: []string{"work"}, StateDir: dir, TenantRoot: filepath.Join(dir, "tenants"), WorkspaceRoot: filepath.Join(dir, "work"), HandshakeDir: filepath.Join(dir, "handshake"), RegistryPath: filepath.Join(dir, "registry.json"), KeyMapPath: filepath.Join(dir, "keys.map"), SessionPath: filepath.Join(dir, "sessions.json"), Deploy: config.DeployConfig{TenantConfigRoot: filepath.Join(dir, "tenant-config")}, AccountCard: config.AccountCard{Enabled: true}}
	writeTenantKey(t, cfg.Deploy.TenantConfigRoot, "alice")
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	tenant := registry.Tenant{Name: "alice", UID: 1001, PublicPort: 32601, WorkerPort: port, KeyPrefix: "sk-aaaaaaaaa", DshHome: "/dsh", Workspace: "/work", CreatedAt: time.Now(), Handshake: registry.HandshakeOK, Account: "李智超(colin)"}
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewFileStore(cfg.SessionPath)
	if err != nil {
		t.Fatal(err)
	}
	ex := &exchangeStub{value: "fresh"}
	p := New(cfg, reg, store, sourceStub("http://127.0.0.1:1/?token=x"), ex, validatorFunc(func(context.Context, string) ([]aigw.Model, error) { return []aigw.Model{{ID: "m"}}, nil }))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "alice", nil })
	p.KeySource = FileKeySource{Root: cfg.Deploy.TenantConfigRoot}
	return p, tenant, up.URL, up
}

type prepareFunc func(context.Context, string, string) error

func (f prepareFunc) PrepareLogin(ctx context.Context, tenant, submittedKey string) error {
	return f(ctx, tenant, submittedKey)
}

type stopFunc func(context.Context, string) error

func (f stopFunc) StopSignedOut(ctx context.Context, tenant string) error {
	return f(ctx, tenant)
}

func issue(t *testing.T, p *Proxy, tenant string, upstream *session.Upstream) string {
	t.Helper()
	token, err := p.Sessions.Issue(tenant, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if upstream != nil {
		if err := p.Sessions.SetUpstream(token, upstream); err != nil {
			t.Fatal(err)
		}
	}
	return token
}

func TestPortalHeadersPermitSameOriginFormPolicy(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dsh.test:32600"
	w := httptest.NewRecorder()
	p.PortalHandler().ServeHTTP(w, req)
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy=%q", got)
	}
	want := "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://dsh.test:32601; base-uri 'none'; frame-ancestors 'none'"
	if got := w.Header().Get("Content-Security-Policy"); got != want {
		t.Fatalf("CSP=%q, want %q", got, want)
	}
}

func TestPortalLoginUsesPrefixAndTenantCookie(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("key=sk-aaaaaaaaa-rest"))
	req.Host = "dsh.test:32600"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://dsh.test:32600")
	req.Header.Set("X-Real-IP", "192.0.2.1")
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	res := w.Result()
	if res.StatusCode != 302 || res.Header.Get("Location") != "https://dsh.test:32601/" {
		t.Fatalf("status=%d location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "dshgw_s_alice" || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("cookies=%#v", cookies)
	}
}

func TestProxyHeaderAndCookieInvariants(t *testing.T) {
	var seen *http.Request
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		seen.Header = r.Header.Clone()
		http.SetCookie(w, &http.Cookie{Name: "dsh-auth-leak", Value: "no"})
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	req := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader("x"))
	req.Host = "dsh.test:32601"
	req.Header.Set("Cookie", "browser=secret; "+p.Config.SessionCookieName("alice")+"="+token)
	req.Header.Set("Origin", "https://dsh.test:32601")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Forwarded-For", "attacker")
	req.Header.Set("X-Real-IP", "198.51.100.9")
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if seen == nil || seen.Host != authority || seen.Header.Get("Cookie") != "dsh-auth-test=held" || seen.Header.Get("Origin") != "" || seen.Header.Get("Sec-Fetch-Site") != "" || seen.Header.Get("X-Forwarded-For") != "" || seen.Header.Get("X-Real-IP") != "" {
		t.Fatalf("upstream=%#v headers=%v", seen, seen.Header)
	}
	for _, c := range w.Result().Cookies() {
		if strings.HasPrefix(c.Name, "dsh-auth-") {
			t.Fatalf("upstream cookie leaked: %#v", c)
		}
	}
}
func Test401RehandshakesExactlyOnce(t *testing.T) {
	calls := 0
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.Contains(r.Header.Get("Cookie"), "old") {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, "fresh")
	}))
	defer up.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "old", Authority: authority})
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "fresh" || calls != 2 {
		t.Fatalf("status=%d body=%q calls=%d", w.Code, w.Body.String(), calls)
	}
	ex := p.Exchanger.(*exchangeStub)
	if ex.count != 1 {
		t.Fatalf("exchanges=%d", ex.count)
	}
}
func TestOriginAndTargetFence(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer up.Close()
	for _, tc := range []struct {
		method, target, origin string
		code                   int
	}{{"POST", "/api", "https://dsh.test:32602", 403}, {"POST", "/api", "null", 403}, {"GET", "/%2e%2e/etc", "", 400}, {"GET", "/%252e%252e/etc", "", 400}, {"GET", "/%25252e%25252e/etc", "", 400}} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Host = "dsh.test:32601"
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("%s %s origin=%q: got %d want %d", tc.method, tc.target, tc.origin, w.Code, tc.code)
		}
	}
}
func TestDispatchUnknownPort(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "dsh.test:32699"
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
}

func TestWebSocketUpgradeStreamingAndOrigin(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocket(r) {
			http.Error(w, "upgrade required", 426)
			return
		}
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSet-Cookie: dsh-auth-leak=no\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 4)
		_, _ = io.ReadFull(conn, buf)
		_, _ = conn.Write(buf)
	})
	p, tenant, _, up := fixture(t, upstream)
	defer up.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	edge := httptest.NewServer(p.Dispatch())
	defer edge.Close()
	edgeURL, _ := url.Parse(edge.URL)
	conn, err := net.Dial("tcp", edgeURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /browser-fs/ws HTTP/1.1\r\nHost: dsh.test:32601\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: https://dsh.test:32601\r\nCookie: %s=%s\r\n\r\n", p.Config.SessionCookieName("alice"), token)
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		rest, _ := io.ReadAll(reader)
		t.Fatalf("status=%q err=%v rest=%s", status, err, rest)
	}
	for {
		line, _ := reader.ReadString('\n')
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "set-cookie:") && strings.Contains(line, "dsh-auth") {
			t.Fatalf("cookie leaked on 101: %q", line)
		}
	}
	_, _ = conn.Write([]byte("ping"))
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo=%q err=%v", echo, err)
	}
	bad := httptest.NewRequest("GET", "/browser-fs/ws", nil)
	bad.Host = "dsh.test:32601"
	bad.Header.Set("Connection", "Upgrade")
	bad.Header.Set("Upgrade", "websocket")
	bad.Header.Set("Origin", "https://dsh.test:32602")
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, bad)
	if w.Code != 403 {
		t.Fatalf("cross-origin ws=%d", w.Code)
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

func TestDuplicateTenantCookieIsRejected(t *testing.T) {
	p, _, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("upstream reached") }))
	defer upstream.Close()
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	req.Host = "dsh.test:32601"
	req.Header.Set("Origin", "https://dsh.test:32601")
	name := p.Config.SessionCookieName("alice")
	req.Header.Set("Cookie", name+"=one; "+name+"=two")
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate cookies returned %d", w.Code)
	}
}

func TestTrustedEdgePortHeaderMustMatchAuthority(t *testing.T) {
	p, _, _, upstream := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	p.Config.EdgePortHeader = "X-DSHGW-Port"
	for _, value := range []string{"", "32601", "32602", "garbage"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "dsh.test:32600"
		if value != "" {
			req.Header.Set("X-DSHGW-Port", value)
		}
		w := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("edge port %q returned %d", value, w.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dsh.test:32600"
	req.Header.Set("X-DSHGW-Port", "32600")
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("matching edge port returned %d", w.Code)
	}
}

func TestRawRequestTargetsFailClosed(t *testing.T) {
	p, _, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("upstream reached") }))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: p.Dispatch(), DisableGeneralOptionsHandler: true}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	for _, raw := range []string{
		"GET http://dsh.test:32601/api HTTP/1.1\r\nHost: dsh.test:32601\r\n\r\n",
		"CONNECT dsh.test:32601 HTTP/1.1\r\nHost: dsh.test:32601\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: dsh.test:32601\r\n\r\n",
		"GET //authority/path HTTP/1.1\r\nHost: dsh.test:32601\r\n\r\n",
	} {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, raw)
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("raw %q: %v", raw, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("raw %q returned %d", raw, response.StatusCode)
		}
	}
}

func TestLegalPathAndRawQueryArePreserved(t *testing.T) {
	seen := make(chan string, 1)
	p, tenant, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	req := httptest.NewRequest(http.MethodGet, "/api//exact/?x=%2Fkeep%2Braw", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := <-seen; got != "/api//exact/?x=%2Fkeep%2Braw" {
		t.Fatalf("request target changed to %q", got)
	}
}

func TestBrowserTokenQueryAndUnsafeMissingOriginRejected(t *testing.T) {
	p, _, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("upstream reached") }))
	defer upstream.Close()
	for _, tc := range []struct {
		method string
		target string
		code   int
	}{{http.MethodGet, "/?token=secret", 400}, {http.MethodPost, "/api", 403}} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Host = "dsh.test:32601"
		w := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("%s %s returned %d", tc.method, tc.target, w.Code)
		}
	}
}

func TestConcurrent401RefreshUsesOneExchange(t *testing.T) {
	p, tenant, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "old") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "old", Authority: authority})
	const count = 32
	start := make(chan struct{})
	errs := make(chan error, count)
	var group sync.WaitGroup
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Host = "dsh.test:32601"
			req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
			w := httptest.NewRecorder()
			p.Dispatch().ServeHTTP(w, req)
			if w.Code != http.StatusNoContent {
				errs <- fmt.Errorf("status %d", w.Code)
			}
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := p.Exchanger.(*exchangeStub).count; got != 1 {
		t.Fatalf("exchange count %d, want 1", got)
	}
}

func TestFinal401IsReturnedAfterOneRefresh(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p, tenant, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "old", Authority: authority})
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || calls != 2 || p.Exchanger.(*exchangeStub).count != 1 {
		t.Fatalf("status=%d calls=%d exchanges=%d", w.Code, calls, p.Exchanger.(*exchangeStub).count)
	}
}

type keySourceFunc func(string) (string, error)

func (f keySourceFunc) Key(tenant string) (string, error) { return f(tenant) }

func TestPerRequestRevalidationRevokesInvalidSession(t *testing.T) {
	p, _, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("upstream reached") }))
	defer upstream.Close()
	p.Config.KeyRevalidate = "per-request"
	p.KeySource = keySourceFunc(func(string) (string, error) { return "sk-aaaaaaaaa-rest", nil })
	p.Validator = validatorFunc(func(context.Context, string) ([]aigw.Model, error) { return nil, aigw.ErrInvalidKey })
	token := issue(t, p, "alice", nil)
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status %d", w.Code)
	}
	if _, err := p.Sessions.Get(token); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("session remains: %v", err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || cookies[0].MaxAge >= 0 {
		t.Fatalf("missing expired cookie: %#v", cookies)
	}
}

func TestIntervalRevalidationCachesResult(t *testing.T) {
	p, tenant, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	p.Config.KeyRevalidate = "interval:60"
	p.KeySource = keySourceFunc(func(string) (string, error) { return "sk-aaaaaaaaa-rest", nil })
	calls := 0
	p.Validator = validatorFunc(func(context.Context, string) ([]aigw.Model, error) { calls++; return []aigw.Model{{ID: "m"}}, nil })
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api", nil)
		req.Host = "dsh.test:32601"
		req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
		w := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status %d", w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("validator calls %d", calls)
	}
}

func TestWorkerRedirectIsRewrittenOrRejected(t *testing.T) {
	for _, tc := range []struct {
		location string
		code     int
		want     string
	}{{"self", http.StatusFound, "https://dsh.test:32601/next"}, {"external", http.StatusBadGateway, ""}, {"authority-relative", http.StatusBadGateway, ""}, {"token", http.StatusBadGateway, ""}} {
		t.Run(tc.location, func(t *testing.T) {
			var authority string
			p, tenant, _, upstream := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				location := "http://" + authority + "/next"
				if tc.location == "external" {
					location = "https://example.invalid/leak"
				} else if tc.location == "authority-relative" {
					location = "//example.invalid/leak"
				} else if tc.location == "token" {
					location = "/?token=secret"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusFound)
			}))
			defer upstream.Close()
			authority = net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
			token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
			req := httptest.NewRequest(http.MethodGet, "/api", nil)
			req.Host = "dsh.test:32601"
			req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
			w := httptest.NewRecorder()
			p.Dispatch().ServeHTTP(w, req)
			if w.Code != tc.code || tc.want != "" && w.Header().Get("Location") != tc.want {
				t.Fatalf("status=%d location=%q", w.Code, w.Header().Get("Location"))
			}
		})
	}
}

func TestAuditOriginSanitization(t *testing.T) {
	got := safeOrigins([]string{"https://dsh.test:32601/path?key=sk-secret", "sk-secret"})
	if !reflect.DeepEqual(got, []string{"https://dsh.test:32601", "[invalid]"}) {
		t.Fatalf("origins=%v", got)
	}
}

// Single-domain path mode: one origin serves every tenant, so the public URLs, the
// session cookie's path and the Origin fence all have to be prefix-aware. Without
// the cookie path in particular, a browser would attach alice's session to a
// request for bob's path — the same origin would no longer imply the same tenant.
func TestPathModeUsesPrefixesForURLsCookiesAndFences(t *testing.T) {
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	p.Config.PublicHost = "chat.example"
	p.Config.PublicBaseURL = "https://chat.example"
	p.Config.TenantPathPrefix = "/t"
	p.Config.PortalPathPrefix = "/dshgw"
	p.Config.SetTenantPorts(map[string]int{tenant.Name: tenant.PublicPort})

	// The login form must post to the portal path, not to the origin root.
	// The front proxy still presents dshgw's internal contract (host:port plus the
	// edge port header) — only the *public* URLs it generates become path-based.
	// Keeping one internal contract is what lets both modes share every code path.
	page := httptest.NewRecorder()
	portalRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	portalRequest.Host = "chat.example:32600"
	p.PortalHandler().ServeHTTP(page, portalRequest)
	if body := page.Body.String(); !strings.Contains(body, `action="/dshgw/login"`) {
		t.Fatalf("login form action is not prefix-aware:\n%s", body)
	}

	// A browser at https://chat.example/dshgw/ sends Origin: https://chat.example —
	// no path — so that is what the fence must accept.
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("key=sk-aaaaaaaaa-rest"))
	login.Host = "chat.example:32600"
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login.Header.Set("Origin", "https://chat.example")
	login.RemoteAddr = "198.51.100.9:1234"
	loginResponse := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(loginResponse, login)
	result := loginResponse.Result()
	if result.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", result.StatusCode)
	}
	if got := result.Header.Get("Location"); got != "https://chat.example/t/alice/" {
		t.Fatalf("login redirect = %q, want the tenant path", got)
	}
	cookies := result.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	if got := cookies[0].Path; got != "/t/alice/" {
		t.Fatalf("session cookie path = %q, want the tenant prefix", got)
	}

	// The tenant request path itself: the edge header routes it, and the fence
	// compares the base origin.
	token := issue(t, p, tenant.Name, nil)
	tenantRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	tenantRequest.Host = "chat.example:32601"
	tenantRequest.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
	tenantRequest.Header.Set("Origin", "https://chat.example")
	tenantRecorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(tenantRecorder, tenantRequest)
	if got := tenantRecorder.Result().StatusCode; got == http.StatusForbidden {
		t.Fatalf("base origin rejected for a tenant request: %d", got)
	}

	// A foreign origin is still refused: one origin for many tenants must not
	// become one origin for the whole internet.
	foreign := httptest.NewRequest(http.MethodGet, "/", nil)
	foreign.Host = "chat.example:32601"
	foreign.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
	foreign.Header.Set("Origin", "https://evil.example")
	foreignRecorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(foreignRecorder, foreign)
	if got := foreignRecorder.Result().StatusCode; got != http.StatusForbidden {
		t.Fatalf("foreign origin accepted: %d", got)
	}
}

// Login is the only moment the deployment holds a key it has already proven valid
// for a tenant, and (M69) the moment the tenant's platform configuration is re-applied
// and its worker is brought back up. The proxy's part of that contract is the call and
// the failure policy: a provisioning failure must not turn a valid login into a failure —
// the user gets their session either way.
func TestLoginPreparesTheTenantAndSurvivesPreparationFailure(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	var prepared string
	p.LoginPrepare = prepareFunc(func(_ context.Context, tenant, key string) error {
		prepared = tenant + "|" + key
		return nil
	})
	login := func() *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("key=sk-aaaaaaaaa-rest"))
		req.Host = "dsh.test:32600"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://dsh.test:32600")
		req.RemoteAddr = "198.51.100.9:1234"
		recorder := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(recorder, req)
		return recorder.Result()
	}
	if response := login(); response.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d", response.StatusCode)
	}
	if prepared != "alice|sk-aaaaaaaaa-rest" {
		t.Fatalf("the tenant was not prepared with the login key: %q", prepared)
	}

	p.LoginPrepare = prepareFunc(func(context.Context, string, string) error {
		return errors.New("aigw unreachable")
	})
	if response := login(); response.StatusCode != http.StatusFound {
		t.Fatalf("a failed preparation blocked a valid login: %d", response.StatusCode)
	}
}

// A Feishu login carries no key: it still prepares the tenant, with an empty submitted key,
// because the platform slice comes from the tenant's stored worker key.
func TestFeishuLoginPreparesTheTenantWithoutAKey(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var prepared, submitted = "unset", "unset"
	setup.proxy.LoginPrepare = prepareFunc(func(_ context.Context, tenant, key string) error {
		prepared, submitted = tenant, key
		return nil
	})
	ticket := setup.signer(setup.tenant, time.Minute, "nonce-prepare")
	response := setup.portalRequest(t, ticket, "")
	if response.StatusCode != http.StatusFound {
		t.Fatalf("feishu login status = %d", response.StatusCode)
	}
	if prepared != setup.tenant || submitted != "" {
		t.Fatalf("feishu login prepared %q with submitted key %q", prepared, submitted)
	}
}

// Signing out stops the tenant's dsh, unconditionally — and that is a decision with a reason
// recorded in the deployment: a closed browser leaves a valid session behind for the rest of the
// TTL, so waiting for "the last session" is waiting for days (the host's own tenant had sixteen
// live sessions, most of them days old).
func TestLogoutStopsTheTenantEvenWithOtherLiveSessions(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	tenant, _ := p.Registry.Get("alice")
	var stopped []string
	p.LogoutStop = stopFunc(func(_ context.Context, name string) error {
		stopped = append(stopped, name)
		return nil
	})
	token := issue(t, p, tenant.Name, nil)
	// A second live session of the same tenant (an older browser, a second window) must not hold
	// the worker open.
	_ = issue(t, p, tenant.Name, nil)
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Host = "dsh.test:32600"
	req.Header.Set("Origin", "https://dsh.test:32600")
	req.RemoteAddr = "198.51.100.9:1234"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName(tenant.Name), Value: token})
	recorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(recorder, req)
	if status := recorder.Result().StatusCode; status != http.StatusSeeOther {
		t.Fatalf("logout status = %d", status)
	}
	if len(stopped) != 1 || stopped[0] != tenant.Name {
		t.Fatalf("logout did not stop the tenant's dsh: %v", stopped)
	}
}

// A logout that carries no session for a tenant must not stop that tenant's dsh: only the
// tenants whose session this request actually revoked are touched.
func TestLogoutLeavesTenantsItDidNotSignOutAlone(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	var stopped []string
	p.LogoutStop = stopFunc(func(_ context.Context, name string) error {
		stopped = append(stopped, name)
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Host = "dsh.test:32600"
	req.Header.Set("Origin", "https://dsh.test:32600")
	req.RemoteAddr = "198.51.100.9:1234"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: "not-a-real-token"})
	recorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(recorder, req)
	if status := recorder.Result().StatusCode; status != http.StatusSeeOther {
		t.Fatalf("logout status = %d", status)
	}
	if len(stopped) != 0 {
		t.Fatalf("a logout stopped a tenant it did not sign out: %v", stopped)
	}
}

// A worker that will not die is an operator's problem, not a failed logout: the browser still
// goes back to the portal.
func TestLogoutSurvivesAWorkerThatWillNotStop(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	tenant, _ := p.Registry.Get("alice")
	p.LogoutStop = stopFunc(func(context.Context, string) error { return errors.New("SIGKILL survived") })
	token := issue(t, p, tenant.Name, nil)
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Host = "dsh.test:32600"
	req.Header.Set("Origin", "https://dsh.test:32600")
	req.RemoteAddr = "198.51.100.9:1234"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName(tenant.Name), Value: token})
	recorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(recorder, req)
	if status := recorder.Result().StatusCode; status != http.StatusSeeOther {
		t.Fatalf("logout status = %d", status)
	}
}

// The tenant-side logout (the sidebar's account row) stops that tenant's dsh on the same rule.
func TestTenantLogoutStopsTheTenantWhenItsLastSessionLeaves(t *testing.T) {
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	var stopped []string
	p.LogoutStop = stopFunc(func(_ context.Context, name string) error {
		stopped = append(stopped, name)
		return nil
	})
	p.Config.AccountCard.Enabled = true
	token := issue(t, p, tenant.Name, &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: net.JoinHostPort("127.0.0.1", itoa(tenant.WorkerPort))})
	req := httptest.NewRequest(http.MethodPost, "/dshgw/logout/", nil)
	req.Host = net.JoinHostPort("dsh.test", itoa(tenant.PublicPort))
	req.Header.Set("Origin", "https://"+net.JoinHostPort("dsh.test", itoa(tenant.PublicPort)))
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName(tenant.Name), Value: token})
	recorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(recorder, req)
	if status := recorder.Result().StatusCode; status != http.StatusSeeOther {
		t.Fatalf("tenant logout status = %d", status)
	}
	if len(stopped) != 1 || stopped[0] != tenant.Name {
		t.Fatalf("tenant logout did not stop its dsh: %v", stopped)
	}
}

// dsh's settings/models panel only loads when the client believes the transport owns
// its host (`transport?.ownsHost`), which a browser never concludes for a LAN host.
// dshgw fronts the tenant UI, so it declares it — the same patch the pre-existing
// deployment applied in nginx. The declaration must reach the shell document and
// nothing else.
func TestSettingsBootstrapIsInjectedIntoTheShellOnly(t *testing.T) {
	page := "<!doctype html><html><head><title>DeepSeek Harness</title></head><body><script type=\"module\" src=\"./assets/index.js\"></script></body></html>"
	p, _, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	}))
	defer up.Close()
	tenant, _ := p.Registry.Get("alice")
	token := issue(t, p, tenant.Name, &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: net.JoinHostPort("127.0.0.1", itoa(tenant.WorkerPort))})

	fetch := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = net.JoinHostPort("dsh.test", itoa(tenant.PublicPort))
		req.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
		req.Header.Set("Origin", "https://dsh.test:"+itoa(tenant.PublicPort))
		recorder := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(recorder, req)
		return recorder.Body.String()
	}
	body := fetch("/")
	if !strings.Contains(body, `__DSH_TRANSPORT__=Object.assign(globalThis.__DSH_TRANSPORT__||{},{ownsHost:true})`) {
		t.Fatalf("the transport declaration is missing from the shell:\n%s", body)
	}
	// It has to run before the shell's own modules, which are deferred but would
	// still read the transport at import time.
	if strings.Index(body, "__DSH_TRANSPORT__") > strings.Index(body, `src="./assets/index.js"`) {
		t.Fatalf("the declaration lands after the shell module:\n%s", body)
	}
	// Declaring it twice would be harmless but signals a double pass; the injector
	// skips a document that already carries the name. (The declaration itself names
	// the global twice, so the marker counted here is the payload.)
	if got := strings.Count(body, "ownsHost:true"); got != 1 {
		t.Fatalf("the declaration appears %d times: %s", got, body)
	}

	// Turning it off must leave dsh's own gating in place.
	p.Config.SettingsUI = "loopback"
	if body := fetch("/"); strings.Contains(body, "__DSH_TRANSPORT__") {
		t.Fatalf("settings_ui=loopback still injected the declaration:\n%s", body)
	}
	p.Config.SettingsUI = "lan"
}

func TestSettingsBootstrapSkipsNonHTML(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	tenant, _ := p.Registry.Get("alice")
	token := issue(t, p, tenant.Name, &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: net.JoinHostPort("127.0.0.1", itoa(tenant.WorkerPort))})
	req := httptest.NewRequest(http.MethodPost, "/api/session/list", strings.NewReader("{}"))
	req.Host = net.JoinHostPort("dsh.test", itoa(tenant.PublicPort))
	req.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
	req.Header.Set("Origin", "https://dsh.test:"+itoa(tenant.PublicPort))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(recorder, req)
	if body := recorder.Body.String(); strings.Contains(body, "__DSH_TRANSPORT__") || body != `{"ok":true}` {
		t.Fatalf("a JSON response was rewritten: %s", body)
	}
}

// The shell is rewritten, so it must reach dshgw uncompressed — splicing into a
// gzipped body makes the browser fail with ERR_CONTENT_DECODING_FAILED — while
// subresources keep their compression.
func TestShellRequestDropsCompressionAndEncodedBodiesAreLeftAlone(t *testing.T) {
	page := "<!doctype html><html><head><title>t</title></head><body></body></html>"
	var sawAcceptEncoding string
	p, _, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/encoded" {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte{0x1f, 0x8b, 0x08, 0x00}) // not valid gzip: must not be touched
			return
		}
		_, _ = io.WriteString(w, page)
	}))
	defer up.Close()
	tenant, _ := p.Registry.Get("alice")
	token := issue(t, p, tenant.Name, &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: net.JoinHostPort("127.0.0.1", itoa(tenant.WorkerPort))})

	send := func(path, accept, dest string) *http.Response {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = net.JoinHostPort("dsh.test", itoa(tenant.PublicPort))
		req.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
		req.Header.Set("Origin", "https://dsh.test:"+itoa(tenant.PublicPort))
		req.Header.Set("Accept", accept)
		req.Header.Set("Accept-Encoding", "gzip, br")
		if dest != "" {
			req.Header.Set("Sec-Fetch-Dest", dest)
		}
		recorder := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(recorder, req)
		return recorder.Result()
	}

	response := send("/", "text/html,application/xhtml+xml", "document")
	// The client advertised "gzip, br"; what must reach dsh is only the transport's
	// own gzip (which Go decodes transparently). Forwarding "br" verbatim would hand
	// this proxy a body it cannot decode, and splicing into it corrupts the document.
	if sawAcceptEncoding != "gzip" {
		t.Fatalf("shell upstream saw Accept-Encoding %q, want the transport's own gzip", sawAcceptEncoding)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "ownsHost:true") {
		t.Fatalf("the shell was not rewritten: %s", body)
	}
	if response.Header.Get("ETag") != "" {
		t.Fatal("a rewritten body kept the upstream ETag")
	}

	// A subresource keeps the client's own negotiation: only the document is rewritten.
	send("/assets/index.js", "*/*", "script")
	if sawAcceptEncoding != "gzip, br" {
		t.Fatalf("subresource upstream saw Accept-Encoding %q, want it forwarded", sawAcceptEncoding)
	}

	// A body this proxy cannot decode must be passed through byte for byte. The
	// transport already hides its own gzip, so the guard is exercised directly with
	// an encoding Go never negotiates for us (brotli).
	raw := []byte{0x1b, 0x02, 0x80, 0x00}
	encoded := &http.Response{
		Header:        http.Header{"Content-Type": []string{"text/html"}, "Content-Encoding": []string{"br"}},
		Body:          io.NopCloser(bytes.NewReader(raw)),
		ContentLength: int64(len(raw)),
	}
	if err := p.injectSettingsBootstrap(encoded); err != nil {
		t.Fatal(err)
	}
	after, _ := io.ReadAll(encoded.Body)
	if !bytes.Equal(after, raw) {
		t.Fatalf("an undecodable body was modified: %v", after)
	}
	if encoded.Header.Get("Content-Encoding") != "br" {
		t.Fatalf("Content-Encoding was dropped from an untouched body: %q", encoded.Header.Get("Content-Encoding"))
	}
}

// dsh sends no cache directives on its API answers, so a shared cache in front (or a
// browser) may reuse an answer that belongs to one authenticated tenant. dshgw marks
// those responses uncacheable and leaves the shell and its assets alone.
func TestTenantAPIDataIsMarkedUncacheable(t *testing.T) {
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, tenant.Name, &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	call := func(path string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Host = net.JoinHostPort("dsh.test", strconv.Itoa(tenant.PublicPort))
		req.Header.Set("Cookie", p.Config.SessionCookieName(tenant.Name)+"="+token)
		req.Header.Set("Origin", "https://dsh.test:"+strconv.Itoa(tenant.PublicPort))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(recorder, req)
		return recorder.Result()
	}

	const want = "no-store, no-cache, must-revalidate, max-age=0"
	for _, path := range []string{"/api/session/list", "/api"} {
		response := call(path)
		if got := response.Header.Get("Cache-Control"); got != want {
			t.Errorf("%s Cache-Control = %q, want %q", path, got, want)
		}
		if response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Expires") != "0" {
			t.Errorf("%s is missing the legacy cache headers", path)
		}
	}

	// A deployment that wants upstream caching back can say so.
	off := false
	p.Config.NoStoreAPIs = &off
	if got := call("/api/session/list").Header.Get("Cache-Control"); got != "" {
		t.Fatalf("no_store_apis=false still marked the response: %q", got)
	}
}

package proxy

import (
	"bufio"
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

type validatorFunc func(context.Context, string) ([]string, error)

func (f validatorFunc) ValidateKey(ctx context.Context, key string) ([]string, error) {
	return f(ctx, key)
}

type authorizerFunc func(context.Context, string) (string, error)

func (f authorizerFunc) Authorize(ctx context.Context, key string) (string, error) {
	return f(ctx, key)
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
	cfg := &config.Config{PublicHost: "dsh.test", PortalPort: 32600, TenantPortLo: 32601, TenantPortHi: 32799, WorkerPortLo: port, WorkerPortHi: port, Listen: "127.0.0.1:3099", AigwBaseURL: "http://aigw.test", ValidateTimeout: config.Duration(time.Second), SessionTTL: config.Duration(time.Hour), KeyRevalidate: "off", LoginRate: config.RateLimit{Requests: 10, Window: config.Duration(time.Minute)}, DirectoryPicker: "clamp", PluginBrowserFS: "on", WorkspaceSeed: []string{"work"}, StateDir: dir, TenantRoot: filepath.Join(dir, "tenants"), WorkspaceRoot: filepath.Join(dir, "work"), HandshakeDir: filepath.Join(dir, "handshake"), RegistryPath: filepath.Join(dir, "registry.json"), KeyMapPath: filepath.Join(dir, "keys.map"), SessionPath: filepath.Join(dir, "sessions.json")}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	tenant := registry.Tenant{Name: "alice", UID: 1001, PublicPort: 32601, WorkerPort: port, KeyPrefix: "sk-aaaaaaaaa", DshHome: "/dsh", Workspace: "/work", CreatedAt: time.Now(), Handshake: registry.HandshakeOK}
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
	p := New(cfg, reg, store, sourceStub("http://127.0.0.1:1/?token=x"), ex, validatorFunc(func(context.Context, string) ([]string, error) { return []string{"m"}, nil }))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "alice", nil })
	return p, tenant, up.URL, up
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
	p.Validator = validatorFunc(func(context.Context, string) ([]string, error) { return nil, aigw.ErrInvalidKey })
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
	p.Validator = validatorFunc(func(context.Context, string) ([]string, error) { calls++; return []string{"m"}, nil })
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

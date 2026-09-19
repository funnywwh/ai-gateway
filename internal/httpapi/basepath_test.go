package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/feishu"
	"github.com/winger/ai-gateway/internal/store"
)

// A prefixed deployment is not a second code path: the prefix is stripped once, at the
// edge of the handler chain, so the mux keeps serving the paths it always served. These
// tests pin the three places where the prefix cannot be invisible — the routes
// themselves, the session cookie's Path, and URLs the server generates for the browser.

func basePathFixture(t *testing.T, base string) (*httptest.Server, *store.DB) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database = config.Database{
		Path: filepath.Join(t.TempDir(), "basepath.db"), BusyTimeoutMS: 2000, WAL: false, MaxOpenConns: 2,
	}
	cfg.Server.BasePath = base
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hash, err := admin.HashPassword("s3cret-prefix")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	adminID, err := db.UpsertAdminUser(ctx, &domain.AdminUser{
		Username: "prefix-admin", PasswordHash: hash, Role: "admin",
	})
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	// One conversation for the preview test: the artifact upload is keyed by session.
	if err := db.CreateChatSession(ctx, &domain.ChatSession{
		ID: "sess_prefix", OwnerUserID: adminID, OwnerName: "prefix-admin",
		Model: "test-model", Title: "prefixed console",
	}); err != nil {
		t.Fatalf("seed chat session: %v", err)
	}

	srv := New(Deps{
		Config:     &cfg,
		Admin:      admin.NewAuth(db, admin.Config{SessionTTL: time.Hour, LoginAttempts: 10, LoginWindow: time.Minute}),
		AdminStore: db,
		ChatStore:  db,
		UI:         http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "console-shell") }),
		Version:    "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, db
}

// noRedirectClient returns the redirect itself instead of following it, which is what a
// browser does before it resolves the relative Location against the current URL.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func callBaseWith(t *testing.T, client *http.Client, ts *httptest.Server, method, path, body, cookie string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func callBase(t *testing.T, ts *httptest.Server, method, path, body, cookie string) *http.Response {
	t.Helper()
	return callBaseWith(t, http.DefaultClient, ts, method, path, body, cookie)
}

// TestBasePathServesEverySurfaceUnderThePrefix is the whole point of the setting: with
// `base_path: /aigw` a reverse proxy can forward the prefix unstripped, and the data
// plane, the console and the management API all answer under it.
func TestBasePathServesEverySurfaceUnderThePrefix(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw")

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/aigw/healthz", http.StatusOK},
		// The version endpoint is public and prefix-mounted like the probes: the console
		// badge fetches it with a path relative to its own mount.
		{http.MethodGet, "/aigw/version", http.StatusOK},
		{http.MethodGet, "/aigw/admin/ui/", http.StatusOK},
		{http.MethodGet, "/aigw/admin/ui/index.html", http.StatusOK},
		{http.MethodGet, "/aigw/admin/ui/js/api.js", http.StatusOK},
		{http.MethodGet, "/aigw/admin/api/v1/auth/me", http.StatusUnauthorized},
		{http.MethodPost, "/aigw/v1/responses", http.StatusUnauthorized},
		{http.MethodGet, "/aigw/v1/models", http.StatusUnauthorized},
		// MCP validates the JSON-RPC envelope before it looks at credentials, so an
		// empty body is a 400; what matters here is that the route is mounted at all.
		{http.MethodPost, "/aigw/mcp", http.StatusBadRequest},
	} {
		resp := callBase(t, ts, tc.method, tc.path, "", "")
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.status)
		}
	}
}

// The Feishu surface is mounted like every other route: its patterns are the served paths
// without the prefix, because withBasePath strips the prefix before the mux sees the
// request. A pattern written with the prefix — which is what the browser-visible callback
// URL carries — would never match, so the login entry point would answer 404 in exactly the
// deployments that run behind a reverse proxy.
func TestBasePathServesTheFeishuSurface(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database = config.Database{
		Path: filepath.Join(t.TempDir(), "basepath-feishu.db"), BusyTimeoutMS: 2000, WAL: false, MaxOpenConns: 2,
	}
	cfg.Server.BasePath = "/aigw"
	cfg.Feishu.Enabled = true
	cfg.Feishu.AdminLogin = true
	cfg.Feishu.CallbackURL = "http://gw.example:8090/aigw/feishu/callback"
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	states, err := feishu.NewStateCodec([]byte("state-key"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	invites, err := feishu.NewStateCodec([]byte("invite-key"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{
		Config:     &cfg,
		AdminStore: db,
		Feishu: &FeishuDeps{
			Client: &feishu.Client{
				AppID: "cli_test", AppSecret: "secret",
				AuthorizeURL: "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
			},
			States:       states,
			Invites:      invites,
			RedirectURI:  cfg.Feishu.CallbackURL,
			LoginPath:    "/feishu/login",
			CallbackPath: "/feishu/callback",
			InvitePath:   "/feishu/invite",
			AdminLogin:   true,
			ConsoleURL:   "/aigw/admin/ui/",
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// The login entry point redirects to the consent page...
	resp := callBaseWith(t, noRedirectClient(), ts, http.MethodGet, "/aigw/feishu/login?mode=admin", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /aigw/feishu/login?mode=admin: status = %d, want 302", resp.StatusCode)
	}
	// ...and the callback needs a state, so an unknown one is refused with an explanation
	// rather than a 404: the route exists under the prefix.
	resp = callBaseWith(t, noRedirectClient(), ts, http.MethodGet, "/aigw/feishu/callback?state=x", "", "")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || !strings.Contains(string(body), "飞书登录未能完成") {
		t.Fatalf("GET /aigw/feishu/callback: status = %d body = %q", resp.StatusCode, body)
	}
	// The invitation entry point is part of the same surface.
	resp = callBaseWith(t, noRedirectClient(), ts, http.MethodGet, "/aigw/feishu/invite?invite=x", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /aigw/feishu/invite: status = %d, want the explanation page", resp.StatusCode)
	}
}

// TestBasePathRejectsUnprefixedRequests keeps the two mounts from agreeing by accident:
// the mux would happily answer /healthz, so the guard has to be the thing that says no.
func TestBasePathRejectsUnprefixedRequests(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw")

	for _, path := range []string{"/healthz", "/admin/ui/", "/admin/api/v1/auth/me", "/v1/models"} {
		resp := callBase(t, ts, http.MethodGet, path, "", "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (the mount is /aigw)", path, resp.StatusCode)
		}
	}
}

// TestBasePathDoesNotSwallowSiblingPrefixes pins the segment boundary: a mount at /aigw
// must not claim /aigw-other, which is a different service on the same proxy.
func TestBasePathDoesNotSwallowSiblingPrefixes(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw")

	for _, path := range []string{"/aigw-other/healthz", "/aigwfoo", "/aigw2"} {
		resp := callBase(t, ts, http.MethodGet, path, "", "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, resp.StatusCode)
		}
	}
	// The bare prefix lands on "/", where no route is mounted: a 404, not the data plane.
	resp := callBase(t, ts, http.MethodGet, "/aigw", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /aigw: status = %d, want 404 (no route is mounted at the bare prefix)", resp.StatusCode)
	}
}

// TestBasePathConsoleRedirectStaysInsideTheMount covers the one redirect the console
// relies on. It is generated from the mount prefix, so a prefixed deployment never sends
// the browser to a path outside its own mount (which would 404 at the proxy).
func TestBasePathConsoleRedirectStaysInsideTheMount(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw")

	redirect := callBaseWith(t, noRedirectClient(), ts, http.MethodGet, "/aigw/admin/ui", "", "")
	_ = redirect.Body.Close()
	if redirect.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("GET /aigw/admin/ui: status = %d, want 301", redirect.StatusCode)
	}
	if got := redirect.Header.Get("Location"); got != "/aigw/admin/ui/" {
		t.Fatalf("GET /aigw/admin/ui: Location = %q, want %q", got, "/aigw/admin/ui/")
	}

	shell := callBase(t, ts, http.MethodGet, "/aigw/admin/ui/", "", "")
	defer shell.Body.Close()
	body, _ := io.ReadAll(shell.Body)
	if shell.StatusCode != http.StatusOK || !strings.Contains(string(body), "console-shell") {
		t.Fatalf("GET /aigw/admin/ui/: status = %d body = %q", shell.StatusCode, body)
	}
}

// TestBasePathSessionCookieCarriesThePrefix: a cookie scoped to /admin would never be
// sent to /aigw/admin/api/v1/..., so login would look like it silently failed.
func TestBasePathSessionCookieCarriesThePrefix(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw")

	resp := callBase(t, ts, http.MethodPost, "/aigw/admin/api/v1/auth/login",
		`{"username":"prefix-admin","password":"s3cret-prefix"}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status = %d body=%s", resp.StatusCode, raw)
	}
	var session *http.Cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == adminCookieName {
			session = cookie
		}
	}
	if session == nil {
		t.Fatalf("login issued no %s cookie", adminCookieName)
	}
	if session.Path != "/aigw/admin" {
		t.Fatalf("session cookie Path = %q, want %q", session.Path, "/aigw/admin")
	}

	// The cookie has to be usable at the prefixed API, and /auth/me proves it end to end.
	me := callBase(t, ts, http.MethodGet, "/aigw/admin/api/v1/auth/me", "", session.Value)
	defer me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("auth/me with the prefixed cookie: status = %d", me.StatusCode)
	}

	// Logout clears the same path it set, or the browser keeps a dead session around.
	out := callBase(t, ts, http.MethodPost, "/aigw/admin/api/v1/auth/logout", "{}", session.Value)
	defer out.Body.Close()
	cleared := false
	for _, cookie := range out.Cookies() {
		if cookie.Name == adminCookieName && cookie.MaxAge < 0 {
			cleared = true
			if cookie.Path != "/aigw/admin" {
				t.Errorf("clearing cookie Path = %q, want %q", cookie.Path, "/aigw/admin")
			}
		}
	}
	if !cleared {
		t.Errorf("logout did not clear %s", adminCookieName)
	}
}

// TestBasePathUnsetKeepsTheRootMount is the regression guard for every existing
// deployment: an empty base_path must behave exactly as it did before the setting existed.
func TestBasePathUnsetKeepsTheRootMount(t *testing.T) {
	ts, _ := basePathFixture(t, "")

	resp := callBase(t, ts, http.MethodGet, "/healthz", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want 200", resp.StatusCode)
	}
	resp = callBase(t, ts, http.MethodGet, "/aigw/healthz", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /aigw/healthz on a root mount: status = %d, want 404", resp.StatusCode)
	}

	login := callBase(t, ts, http.MethodPost, "/admin/api/v1/auth/login",
		`{"username":"prefix-admin","password":"s3cret-prefix"}`, "")
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("root-mount login: status = %d", login.StatusCode)
	}
	seen := false
	for _, cookie := range login.Cookies() {
		if cookie.Name != adminCookieName {
			continue
		}
		seen = true
		if cookie.Path != "/admin" {
			t.Fatalf("root-mount session cookie Path = %q, want %q", cookie.Path, "/admin")
		}
	}
	if !seen {
		t.Fatalf("root-mount login issued no %s cookie", adminCookieName)
	}

	redirect := callBaseWith(t, noRedirectClient(), ts, http.MethodGet, "/admin/ui", "", "")
	_ = redirect.Body.Close()
	if got := redirect.Header.Get("Location"); got != "/admin/ui/" {
		t.Fatalf("GET /admin/ui: Location = %q, want %q", got, "/admin/ui/")
	}
}

// TestBasePathTrailingSlashIsNormalized accepts the spelling an operator is most likely to
// write in a proxy config (a trailing slash) without changing the behaviour.
func TestBasePathTrailingSlashIsNormalized(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw/")

	resp := callBase(t, ts, http.MethodGet, "/aigw/healthz", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /aigw/healthz with base_path=%q: status = %d, want 200", "/aigw/", resp.StatusCode)
	}
}

// TestBasePathReachesChatArtifactURLs: the console renders a preview in a sandboxed frame,
// so the URL in the API response is what the browser fetches. It has to carry the prefix,
// because the frame has no cookie and cannot be redirected through a second round trip.
func TestBasePathReachesChatArtifactURLs(t *testing.T) {
	ts, _ := basePathFixture(t, "/aigw")

	login := callBase(t, ts, http.MethodPost, "/aigw/admin/api/v1/auth/login",
		`{"username":"prefix-admin","password":"s3cret-prefix"}`, "")
	defer login.Body.Close()
	value := ""
	for _, cookie := range login.Cookies() {
		if cookie.Name == adminCookieName {
			value = cookie.Value
		}
	}
	if value == "" {
		t.Fatal("login issued no session cookie")
	}

	resp := callBase(t, ts, http.MethodPost, "/aigw/admin/api/v1/chat/sessions/sess_prefix/artifacts",
		`{"format":"html","key":"block-1","title":"demo","body":"<html><body>hi</body></html>"}`, value)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("artifact upload status = %d body=%s", resp.StatusCode, raw)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode artifact response %q: %v", raw, err)
	}
	url, _ := payload["url"].(string)
	if !strings.HasPrefix(url, "/aigw/admin/chat-artifact/") {
		t.Fatalf("artifact url = %q, want an /aigw-prefixed path", url)
	}
	// The prefixed artifact path is served too (a bad ticket is refused, not 404'd),
	// which is what tells a wrong prefix apart from a missing route.
	fetch := callBase(t, ts, http.MethodGet, url+"?ticket=nope", "", "")
	defer fetch.Body.Close()
	if fetch.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s with a bogus ticket: status = %d, want 404 from the ticket check", url, fetch.StatusCode)
	}
}

// TestTrimBasePath pins the matching rule itself, including the near misses.
func TestTrimBasePath(t *testing.T) {
	for _, tc := range []struct {
		path, prefix, want string
		ok                 bool
	}{
		{path: "/aigw", prefix: "/aigw", want: "/", ok: true},
		{path: "/aigw/", prefix: "/aigw", want: "/", ok: true},
		{path: "/aigw/admin/ui/", prefix: "/aigw", want: "/admin/ui/", ok: true},
		{path: "/aigw/v1/responses", prefix: "/aigw", want: "/v1/responses", ok: true},
		{path: "/gateway/aigw/x", prefix: "/gateway/aigw", want: "/x", ok: true},
		{path: "/aigwfoo", prefix: "/aigw", ok: false},
		{path: "/aigw-other/x", prefix: "/aigw", ok: false},
		{path: "/healthz", prefix: "/aigw", ok: false},
		{path: "/", prefix: "/aigw", ok: false},
	} {
		got, ok := trimBasePath(tc.path, tc.prefix)
		if ok != tc.ok || got != tc.want {
			t.Errorf("trimBasePath(%q, %q) = %q, %v; want %q, %v", tc.path, tc.prefix, got, ok, tc.want, tc.ok)
		}
	}
}

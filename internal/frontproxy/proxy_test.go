package frontproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upstreams records what the proxy sent, so the tests assert the routing contract
// (path, Host, edge header) rather than just the status code.
type upstreams struct {
	requests []*http.Request
	body     string
	status   int
	ctype    string
}

func newUpstream(t *testing.T, u *upstreams) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		body, _ := io.ReadAll(r.Body)
		clone.Body = io.NopCloser(strings.NewReader(string(body)))
		u.requests = append(u.requests, clone)
		if u.status != 0 {
			w.WriteHeader(u.status)
		}
		if u.ctype != "" {
			w.Header().Set("Content-Type", u.ctype)
		}
		_, _ = io.WriteString(w, u.body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func registryFileFor(t *testing.T, tenants map[string]int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	entries := make([]string, 0, len(tenants))
	for name, port := range tenants {
		entries = append(entries, `{"name":"`+name+`","public_port":`+itoa(port)+`}`)
	}
	doc := `{"version":1,"tenants":[` + strings.Join(entries, ",") + `]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func fixture(t *testing.T, aigw, portal, tenant *upstreams, ports map[string]int) (*Proxy, *Config) {
	t.Helper()
	cfg := &Config{
		Listen: "127.0.0.1:0", PublicHost: "chat.example",
		AigwUpstream:   newUpstream(t, aigw),
		PortalUpstream: newUpstream(t, portal),
		TenantUpstream: newUpstream(t, tenant),
		PortalPort:     32600,
		RegistryPath:   registryFileFor(t, ports),
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	proxy, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.RefreshTenants(true); err != nil {
		t.Fatal(err)
	}
	return proxy, cfg
}

func request(t *testing.T, proxy *Proxy, method, path, host string, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = host
	recorder := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(recorder, req)
	return recorder.Result()
}

func TestRoutesSeparateServicesByPrefix(t *testing.T) {
	var aigw, portal, tenant upstreams
	aigw.body, portal.body, tenant.body = "aigw", "portal", "tenant"
	proxy, _ := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})

	// aigw keeps its prefix: its server.base_path serves it, so cookie Path and
	// generated console URLs already carry it.
	response := request(t, proxy, http.MethodGet, "/aigw/version", "chat.example:8443", "")
	if response.StatusCode != 200 || aigw.requests[0].URL.Path != "/aigw/version" {
		t.Fatalf("aigw route: status=%d path=%s", response.StatusCode, aigw.requests[0].URL.Path)
	}
	// aigw builds absolute redirects from the request Host (its own /admin/ui ->
	// /admin/ui/ 301 does that), so the browser's authority — port included — is what
	// it must see. A request for any other host never reaches here.
	if got := aigw.requests[0].Host; got != "chat.example:8443" {
		t.Fatalf("aigw Host = %q, want the browser's authority", got)
	}

	// The portal is stripped and must carry dshgw's two routing inputs.
	response = request(t, proxy, http.MethodGet, "/dshgw/login", "chat.example:8443", "")
	if response.StatusCode != 200 || portal.requests[0].URL.Path != "/login" {
		t.Fatalf("portal route: status=%d path=%s", response.StatusCode, portal.requests[0].URL.Path)
	}
	if portal.requests[0].Host != "chat.example:32600" || portal.requests[0].Header.Get("X-DSHGW-Port") != "32600" {
		t.Fatalf("portal headers: host=%q edge=%q", portal.requests[0].Host, portal.requests[0].Header.Get("X-DSHGW-Port"))
	}

	// A tenant is <prefix>/<tenant>/... and keeps dshgw's session/handshake path
	// intact: the tenant's public port becomes the Host authority and edge header.
	response = request(t, proxy, http.MethodGet, "/t/alice/api/session", "chat.example:8443", "")
	if response.StatusCode != 200 || tenant.requests[0].URL.Path != "/api/session" {
		t.Fatalf("tenant route: status=%d path=%s", response.StatusCode, tenant.requests[0].URL.Path)
	}
	if tenant.requests[0].Host != "chat.example:32601" || tenant.requests[0].Header.Get("X-DSHGW-Port") != "32601" {
		t.Fatalf("tenant headers: host=%q edge=%q", tenant.requests[0].Host, tenant.requests[0].Header.Get("X-DSHGW-Port"))
	}
}

// A deployment that must keep aigw reachable directly on its own port cannot give
// aigw a base path (that setting is exclusive: a request without the prefix is a
// 404). The proxy then strips the prefix itself, and aigw sees the same paths it
// serves on its port.
func TestAigwStripPrefixKeepsDirectAccessWorking(t *testing.T) {
	var aigw, portal, tenant upstreams
	aigw.body = "aigw"
	proxy, cfg := fixture(t, &aigw, &portal, &tenant, map[string]int{})
	cfg.AigwStripPrefix = true

	response := request(t, proxy, http.MethodGet, "/aigw/version", "chat.example:8443", "")
	if response.StatusCode != 200 {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := aigw.requests[0].URL.Path; got != "/version" {
		t.Fatalf("upstream path = %q, want the prefix stripped", got)
	}
	if got := aigw.requests[0].URL.RawQuery; got != "" {
		t.Fatalf("query was rewritten: %q", got)
	}
	// The prefix itself maps to the upstream root rather than to an empty path.
	if response := request(t, proxy, http.MethodGet, "/aigw", "chat.example:8443", ""); response.StatusCode != 200 {
		t.Fatalf("bare prefix status = %d", response.StatusCode)
	}
	if got := aigw.requests[1].URL.Path; got != "/" {
		t.Fatalf("bare prefix upstream path = %q, want /", got)
	}
}

// Root-mounted aigw: "/" is the fallback for everything the portal and tenant
// prefixes do not claim, which is what lets /admin/ui/, /version and /v1/... work
// on the same domain without giving aigw a base path (that setting is exclusive
// and would break direct access on aigw's own port).
func TestRootMountedAigwServesConsoleAndKeepsPortalPrefixes(t *testing.T) {
	var aigw, portal, tenant upstreams
	aigw.body, portal.body, tenant.body = "aigw", "portal", "tenant"
	proxy, cfg := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})
	cfg.AigwPrefix = "/"
	cfg.RootRedirect = "/admin/ui/"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("root-mounted aigw rejected: %v", err)
	}

	// The console and the API keep their paths: aigw serves them at its root.
	for _, path := range []string{"/admin/ui/", "/admin/ui/assets/app.js", "/version", "/v1/models"} {
		response := request(t, proxy, http.MethodGet, path, "chat.example:8443", "")
		if response.StatusCode != 200 {
			t.Fatalf("%s status = %d", path, response.StatusCode)
		}
	}
	seen := map[string]bool{}
	for _, r := range aigw.requests {
		seen[r.URL.Path] = true
	}
	for _, path := range []string{"/admin/ui/", "/admin/ui/assets/app.js", "/version", "/v1/models"} {
		if !seen[path] {
			t.Errorf("aigw did not receive %s", path)
		}
	}

	// The root itself goes to the console: aigw answers 404 there, and a bare 404 is
	// a poor front door for a deployment whose console lives one path deeper.
	root := request(t, proxy, http.MethodGet, "/", "chat.example:8443", "")
	if root.StatusCode != http.StatusFound || root.Header.Get("Location") != "/admin/ui/" {
		t.Fatalf("root = %d %q, want a redirect to the console", root.StatusCode, root.Header.Get("Location"))
	}

	// Specific prefixes still win: a root-mounted aigw must not swallow them.
	if response := request(t, proxy, http.MethodGet, "/dshgw/", "chat.example:8443", ""); response.StatusCode != 200 {
		t.Fatalf("portal status = %d", response.StatusCode)
	}
	if response := request(t, proxy, http.MethodGet, "/t/alice/", "chat.example:8443", ""); response.StatusCode != 200 {
		t.Fatalf("tenant status = %d", response.StatusCode)
	}
	if !strings.HasSuffix(portal.requests[0].URL.Path, "/") || tenant.requests[0].URL.Path != "/" {
		t.Fatalf("prefixes were not routed to dshgw: portal=%q tenant=%q",
			portal.requests[0].URL.Path, tenant.requests[0].URL.Path)
	}
}

// Only aigw may be the root fallback: a portal or tenant prefix of "/" would make
// every other prefix unreachable.
func TestOnlyAigwMayBeRootMounted(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.PortalPrefix = "/" },
		func(c *Config) { c.TenantPrefix = "/" },
	} {
		cfg := &Config{
			Listen: "127.0.0.1:8443", PublicHost: "chat.example",
			AigwUpstream: "http://127.0.0.1:8088", PortalUpstream: "http://127.0.0.1:18099",
			TenantUpstream: "http://127.0.0.1:18099", PortalPort: 32600,
			RegistryPath: "/tmp/registry.json",
		}
		mutate(cfg)
		cfg.applyDefaults()
		if err := cfg.Validate(); err == nil {
			t.Fatalf("a root-mounted dshgw surface was accepted: %+v", cfg)
		}
	}
}

func TestRouterRejectsForeignHostUnknownTenantAndServesHealth(t *testing.T) {
	var aigw, portal, tenant upstreams
	proxy, cfg := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})

	if got := request(t, proxy, http.MethodGet, "/aigw/version", "other.example:8443", "").StatusCode; got != 404 {
		t.Fatalf("foreign host accepted: %d", got)
	}
	if got := request(t, proxy, http.MethodGet, "/t/bob/", "chat.example:8443", "").StatusCode; got != 404 {
		t.Fatalf("unknown tenant accepted: %d", got)
	}
	if got := request(t, proxy, http.MethodGet, "/healthz", "chat.example:8443", "").StatusCode; got != 200 {
		t.Fatalf("health endpoint: %d", got)
	}
	redirect := request(t, proxy, http.MethodGet, "/", "chat.example:8443", "")
	if redirect.StatusCode != http.StatusFound || redirect.Header.Get("Location") != "/dshgw/" {
		t.Fatalf("root did not lead to the portal: %d %q", redirect.StatusCode, redirect.Header.Get("Location"))
	}
	if len(aigw.requests)+len(portal.requests)+len(tenant.requests) != 0 {
		t.Fatal("a rejected request reached an upstream")
	}
	_ = cfg
}

// dsh has no base-path option, so the shell's two root-absolute references are
// rewritten. This is the narrowest possible fix and must not touch anything else.
func TestTenantHTMLRewriteIsNarrow(t *testing.T) {
	var aigw, portal, tenant upstreams
	tenant.ctype = "text/html; charset=utf-8"
	tenant.body = `<html><head><base href="/">` +
		`<script src="/plugins/??@deepseek-ai/dsh-client-modules/client.js"></script>` +
		`<link href="./assets/index.js"></head><body><a href="/">home</a>` +
		`<img src="/plugins/icon.svg"><p>href="/" stays in text` +
		`<a href="/">home</a></p></body></html>`
	proxy, _ := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})

	response := request(t, proxy, http.MethodGet, "/t/alice/", "chat.example:8443", "")
	page, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, want := range []string{
		`src="/t/alice/plugins/??@deepseek-ai/dsh-client-modules/client.js"`,
		`src="/t/alice/plugins/icon.svg"`,
		`href="./assets/index.js"`, // relative references are already correct
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rewritten shell is missing %q:\n%s", want, html)
		}
	}
	// Every root link in markup is rewritten — the <base> plus both anchors — while a
	// mention inside prose is not, which is the whole point of walking tags instead
	// of doing a blanket string replacement.
	if got := strings.Count(html, `href="/t/alice/"`); got != 3 {
		t.Errorf("rewritten root links = %d, want 3 (base + two anchors):\n%s", got, html)
	}
	if !strings.Contains(html, `<p>href="/" stays in text`) {
		t.Errorf("prose was rewritten:\n%s", html)
	}
	if got := response.Header.Get("Content-Length"); got != itoa(len(html)) {
		t.Errorf("Content-Length = %q, want %d", got, len(html))
	}
}

func TestNonHTMLResponsesAreUntouched(t *testing.T) {
	var aigw, portal, tenant upstreams
	tenant.ctype = "application/json"
	tenant.body = `{"href":"/","plugins":"/plugins/x.js"}`
	proxy, _ := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})

	response := request(t, proxy, http.MethodGet, "/t/alice/api", "chat.example:8443", "")
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != tenant.body {
		t.Fatalf("JSON body was rewritten: %s", body)
	}
}

func TestConfigRejectsOverlappingPrefixesAndBadUpstreams(t *testing.T) {
	base := func(mutate func(*Config)) error {
		cfg := &Config{
			Listen: "127.0.0.1:8443", PublicHost: "chat.example",
			AigwUpstream: "http://127.0.0.1:8088", PortalUpstream: "http://127.0.0.1:18099",
			TenantUpstream: "http://127.0.0.1:18099", PortalPort: 32600,
			RegistryPath: "/tmp/registry.json",
		}
		mutate(cfg)
		cfg.applyDefaults()
		return cfg.Validate()
	}
	if err := base(func(*Config) {}); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"prefix overlap":    func(c *Config) { c.PortalPrefix = "/aigw/dshgw" },
		"prefix is root":    func(c *Config) { c.TenantPrefix = "/" },
		"relative registry": func(c *Config) { c.RegistryPath = "registry.json" },
		"upstream with path": func(c *Config) {
			c.AigwUpstream = "http://127.0.0.1:8088/aigw"
		},
		"half a certificate": func(c *Config) { c.TLS.Certificate = "/tmp/cert.pem" },
		"host with port":     func(c *Config) { c.PublicHost = "chat.example:8443" },
	} {
		if err := base(mutate); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A deployment that cannot use subdomains and cannot host the UI under a path uses
// the domain only as a front door: the prefixes redirect to the service's own port,
// which is the origin the dsh client requires (it builds its API URLs from
// location.origin, and an origin never carries a path).
func TestPortalAndTenantRedirectsToTheirOwnOrigins(t *testing.T) {
	var aigw, portal, tenant upstreams
	proxy, cfg := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})
	cfg.PortalRedirect = true
	cfg.TenantRedirect = true
	cfg.PublicScheme = "http"

	response := request(t, proxy, http.MethodGet, "/dshgw/login", "chat.example:8443", "")
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "http://chat.example:32600/login" {
		t.Fatalf("portal redirect = %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	// A deep link keeps its sub-path, so a bookmarked page still resolves.
	response = request(t, proxy, http.MethodGet, "/t/alice/settings/profile", "chat.example:8443", "")
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "http://chat.example:32601/settings/profile" {
		t.Fatalf("tenant redirect = %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	if response := request(t, proxy, http.MethodGet, "/t/nobody/", "chat.example:8443", ""); response.StatusCode != 404 {
		t.Fatalf("unknown tenant = %d, want 404", response.StatusCode)
	}
	// Nothing was proxied: the prefix is a hand-off, not a second code path.
	if len(portal.requests)+len(tenant.requests) != 0 {
		t.Fatalf("a redirect reached an upstream: portal=%d tenant=%d", len(portal.requests), len(tenant.requests))
	}
}

// An API answer reached with a caller's own credentials must never be reusable: the
// proxy marks those responses no-store, drops conditional-request validators so a
// stale copy cannot be revalidated into a 304, and leaves static assets alone so the
// console does not re-download itself on every view.
func TestAPIDataIsNeverCacheable(t *testing.T) {
	var aigw, portal, tenant upstreams
	aigw.ctype, portal.ctype, tenant.ctype = "application/json", "text/html", "application/json"
	aigw.body, portal.body, tenant.body = `{"v":1}`, "<html></html>", `{"api":true}`
	proxy, cfg := fixture(t, &aigw, &portal, &tenant, map[string]int{"alice": 32601})
	_ = cfg

	send := func(path, header string) *http.Response {
		request, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "chat.example:8443"
		if header != "" {
			request.Header.Set("If-None-Match", header)
		}
		recorder := httptest.NewRecorder()
		proxy.Handler().ServeHTTP(recorder, request)
		return recorder.Result()
	}

	// aigw data plane and management API: no-store, and the validator is not
	// forwarded. The fixture mounts aigw under its configured prefix, and the API test
	// looks through that prefix — which is exactly the case a root-mounted deployment
	// exercises with the bare paths.
	for _, path := range []string{"/aigw/v1/models", "/aigw/admin/api/v1/accounts", "/aigw/version", "/healthz"} {
		response := send(path, `"stale"`)
		if got := response.Header.Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, max-age=0" {
			t.Errorf("%s Cache-Control = %q", path, got)
		}
		if response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Expires") != "0" {
			t.Errorf("%s legacy cache headers missing: %v", path, response.Header)
		}
	}
	for _, seen := range aigw.requests {
		if seen.Header.Get("If-None-Match") != "" {
			t.Errorf("aigw saw a cache validator: %q", seen.Header.Get("If-None-Match"))
		}
	}

	// The tenant's API is behind a path prefix; the test must look through it.
	response := send("/t/alice/api/session/list", `"stale"`)
	if got := response.Header.Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, max-age=0" {
		t.Errorf("tenant api Cache-Control = %q", got)
	}
	if len(tenant.requests) != 1 || tenant.requests[0].Header.Get("If-None-Match") != "" {
		t.Errorf("tenant upstream saw a cache validator")
	}

	// Static assets keep the upstream's own caching: forcing no-store on the console
	// bundle would make every page load refetch it.
	asset := send("/aigw/admin/ui/app.css", "")
	if got := asset.Header.Get("Cache-Control"); got != "" {
		t.Errorf("a static asset was marked %q", got)
	}
}

// A deployment that wants upstream caching back can say so explicitly; the default
// is the safe one.
func TestAPIPathsAndNoStoreAreConfigurable(t *testing.T) {
	var aigw, portal, tenant upstreams
	aigw.ctype = "application/json"
	proxy, cfg := fixture(t, &aigw, &portal, &tenant, map[string]int{})

	if !cfg.IsAPIPath("/api/x", "/v1/y", "/admin/api/z", "/version", "/healthz", "/readyz") || !cfg.IsAPIPath("/api/") {
		t.Fatal("the default API surfaces are not recognised")
	}
	if cfg.IsAPIPath("/admin/ui/", "/assets/app.css", "/") {
		t.Fatal("static surfaces were treated as API paths")
	}
	if cfg.StoreAPIData() {
		t.Fatal("the default must be no-store")
	}
	off := false
	cfg.NoStoreAPIs = &off
	cfg.APIPaths = []string{"/rpc/"}
	if !cfg.StoreAPIData() {
		t.Fatal("an explicit opt-in was ignored")
	}
	if cfg.IsAPIPath("/v1/models") || !cfg.IsAPIPath("/rpc/call") {
		t.Fatal("api_paths was not honoured")
	}
	_ = proxy
}

func TestConfigRejectsUnusableAPIPaths(t *testing.T) {
	base := func(mutate func(*Config)) error {
		cfg := &Config{
			Listen: "127.0.0.1:8443", PublicHost: "chat.example",
			AigwUpstream: "http://127.0.0.1:8088", PortalUpstream: "http://127.0.0.1:18099",
			TenantUpstream: "http://127.0.0.1:18099", PortalPort: 32600,
			RegistryPath: "/tmp/registry.json",
		}
		mutate(cfg)
		cfg.applyDefaults()
		return cfg.Validate()
	}
	if err := base(func(*Config) {}); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"relative path": func(c *Config) { c.APIPaths = []string{"api/"} },
		"root path":     func(c *Config) { c.APIPaths = []string{"/"} },
	} {
		if err := base(mutate); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

// tenantRequest builds one request as the tenant's browser sends it: the tenant's own host
// and port, and optionally the session cookie the page carries.
func tenantRequest(t *testing.T, method, path, origin, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = "dsh.test:32601"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Cookie", "dshgw_s_alice="+token)
	}
	return req
}

// decodeIdentity reads the {ok, value} envelope the sidebar's client reads.
func decodeIdentity(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		OK    bool           `json:"ok"`
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", res.Body.String(), err)
	}
	if !body.OK {
		t.Fatalf("envelope not ok: %s", res.Body.String())
	}
	return body.Value
}

func TestTenantSessionEndpointNamesTheSignedInPerson(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the identity route must not reach the worker")
	}))
	defer worker.Close()
	auth := &identityAuthorizer{account: "李智超(colin)", feishu: "李智超"}
	p.Authorizer = auth
	token := issue(t, p, tenant.Name, nil)

	res := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodGet, accountSessionPath, "", token))
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	value := decodeIdentity(t, res)
	if value["authenticated"] != true || value["tenant"] != tenant.Name {
		t.Fatalf("identity=%v", value)
	}
	if value["feishu_name"] != "李智超" || value["account"] != "李智超(colin)" {
		t.Fatalf("names=%v", value)
	}
	if value["name"] != "李智超" {
		t.Fatalf("name=%v, want the Feishu name first", value["name"])
	}

	// The sidebar asks again on every page load: one aigw lookup serves them all until the
	// cache expires, and the second answer matches the first.
	res = httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodGet, accountSessionPath, "", token))
	if res.Code != http.StatusOK || decodeIdentity(t, res)["feishu_name"] != "李智超" {
		t.Fatalf("second read: status=%d body=%s", res.Code, res.Body.String())
	}
	if auth.calls != 1 {
		t.Fatalf("identity lookups=%d, want one per tenant within the TTL", auth.calls)
	}
	if _, err := p.Sessions.Get(token); err != nil {
		t.Fatalf("reading the identity must not disturb the session: %v", err)
	}
}

func TestTenantSessionEndpointFallsBackToTheTenantName(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	p.Authorizer = &identityAuthorizer{failWith: errors.New("aigw is down")}
	token := issue(t, p, tenant.Name, nil)

	res := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodGet, accountSessionPath, "", token))
	if res.Code != http.StatusOK {
		t.Fatalf("a display lookup must not fail the request: status=%d", res.Code)
	}
	value := decodeIdentity(t, res)
	if value["name"] != "李智超(colin)" {
		// The label recorded on the tenant is the fallback when aigw cannot be asked.
		t.Fatalf("name=%v, want the tenant's recorded account label", value["name"])
	}
	if value["feishu_name"] != nil {
		t.Fatalf("feishu_name=%v, want it absent", value["feishu_name"])
	}
}

func TestTenantSessionEndpointRequiresTheTenantsOwnSession(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	token := issue(t, p, tenant.Name, nil)

	for _, input := range []struct {
		name   string
		method string
		origin string
		token  string
		status int
	}{
		{"no cookie GET", http.MethodGet, "", "", http.StatusFound},
		// A POST is an unsafe request, so it needs an Origin: with one, the missing session is
		// what answers (401 JSON), which is the shape an API caller sees.
		{"no cookie POST", http.MethodPost, "https://dsh.test:32601", "", http.StatusUnauthorized},
		{"wrong method", http.MethodPost, "https://dsh.test:32601", token, http.StatusMethodNotAllowed},
		{"stale session", http.MethodGet, "", "not-a-session", http.StatusFound},
	} {
		res := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(res, tenantRequest(t, input.method, accountSessionPath, input.origin, input.token))
		if res.Code != input.status {
			t.Fatalf("%s: status=%d body=%s", input.name, res.Code, res.Body.String())
		}
	}
}

func TestUnknownPathUnderTheReservedNamespaceIsNotFound(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a reserved path must not reach the worker")
	}))
	defer worker.Close()
	token := issue(t, p, tenant.Name, nil)

	res := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodGet, "/dshgw/other/", "", token))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestTenantLogoutRevokesOnlyThisTenantAndSendsThePortal(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	token := issue(t, p, tenant.Name, nil)
	// A second tenant's session in the same browser: pressing 退出 in one tenant must not
	// sign the person out of the other.
	other := registry.Tenant{Name: "bob", UID: 1002, PublicPort: 32602, WorkerPort: 32699, KeyPrefix: "sk-bbbbbbbbb", DshHome: "/dsh-bob", Workspace: "/work-bob", CreatedAt: tenant.CreatedAt, Handshake: registry.HandshakeOK}
	if err := p.Registry.Put(other); err != nil {
		t.Fatal(err)
	}
	otherToken := issue(t, p, other.Name, nil)

	// A cross-origin POST is refused before anything is revoked.
	res := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodPost, accountLogoutPath, "https://evil.test", token))
	if res.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: status=%d", res.Code)
	}
	if _, err := p.Sessions.Get(token); err != nil {
		t.Fatalf("a rejected logout must not revoke: %v", err)
	}

	// GET is not a logout (it would be a cross-site image away from signing someone out).
	res = httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodGet, accountLogoutPath, "", token))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout: status=%d", res.Code)
	}
	if _, err := p.Sessions.Get(token); err != nil {
		t.Fatalf("a GET must not revoke: %v", err)
	}

	res = httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodPost, accountLogoutPath, "https://dsh.test:32601", token))
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if got, want := res.Header().Get("Location"), "https://dsh.test:32600/"; got != want {
		t.Fatalf("Location=%q, want the portal login page %q", got, want)
	}
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", res.Header().Get("Cache-Control"))
	}
	cookies := res.Result().Cookies()
	cleared := false
	for _, cookie := range cookies {
		if cookie.Name == p.Config.SessionCookieName(tenant.Name) && cookie.Value == "" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the tenant's session cookie must be cleared: %v", cookies)
	}
	if _, err := p.Sessions.Get(token); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("logout did not revoke the session: %v", err)
	}
	if _, err := p.Sessions.Get(otherToken); err != nil {
		t.Fatalf("logout of one tenant touched another tenant's session: %v", err)
	}

	// The revoked cookie is no longer a key to anything: the next request is unauthenticated.
	res = httptest.NewRecorder()
	p.Dispatch().ServeHTTP(res, tenantRequest(t, http.MethodGet, accountSessionPath, "", token))
	if res.Code != http.StatusFound {
		t.Fatalf("status=%d after logout", res.Code)
	}
}

func TestIdentityCacheIsDroppedWhenATenantDisappears(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	auth := &identityAuthorizer{account: "李智超(colin)", feishu: "李智超"}
	p.Authorizer = auth
	// Warm the cache, then remove the tenant and let the reload notice: a name must not
	// outlive the tenant it belonged to, or a recreated tenant would inherit it.
	p.identity(context.Background(), tenant)
	if len(p.identities) != 1 {
		t.Fatalf("cache=%v", p.identities)
	}
	p.Registry.Delete(tenant.Name)
	p.forgetIdentities(p.Registry.TenantPorts())
	if len(p.identities) != 0 {
		t.Fatalf("cache survived the tenant: %v", p.identities)
	}
}

package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
)

// M52: the account-level dsh entitlement. Login always consults aigw's authorize check
// (fail closed); request-time enforcement only runs for the non-login dsh_enforce modes.

func dshgwLogin(t *testing.T, p *Proxy) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("key=sk-aaaaaaaaa-rest"))
	req.Host = "dsh.test:32600"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://dsh.test:32600")
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	res := w.Result()
	cookie := ""
	for _, c := range res.Cookies() {
		if strings.HasPrefix(c.Name, "dshgw_s_") {
			cookie = c.Name + "=" + c.Value
		}
	}
	return res, cookie
}

func dshgwTenantGet(t *testing.T, p *Proxy, cookie string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dsh.test:32601"
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	return w.Result()
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPortalLoginRejectsDisabledDSHAccount(t *testing.T) {
	for _, tc := range []struct {
		reason, wantText string
	}{
		{"dsh_disabled", "未启用"},
		{"account_status", "已停用"},
		{"something_new", "未启用"},
	} {
		p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) {
			return "", &aigw.DSHDenial{Reason: tc.reason}
		})
		res, cookie := dshgwLogin(t, p)
		if res.StatusCode != http.StatusForbidden || cookie != "" {
			t.Fatalf("%s: status=%d cookie=%q", tc.reason, res.StatusCode, cookie)
		}
		if !strings.Contains(bodyOf(t, res), tc.wantText) {
			t.Fatalf("%s: body missing %q", tc.reason, tc.wantText)
		}
		up.Close()
	}
}

func TestPortalLoginFailsClosedWithoutAuthorizer(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p.Authorizer = nil
	defer up.Close()
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusServiceUnavailable || cookie != "" {
		t.Fatalf("status=%d cookie=%q", res.StatusCode, cookie)
	}
}

func TestPortalLoginTreatsAuthorizeInvalidKeyAsInvalid(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "", aigw.ErrInvalidKey })
	defer up.Close()
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusUnauthorized || cookie != "" {
		t.Fatalf("status=%d cookie=%q", res.StatusCode, cookie)
	}
}

func TestPortalLoginUnavailableWhenAuthorizeErrors(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "", errors.New("connection refused") })
	defer up.Close()
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusServiceUnavailable || cookie != "" {
		t.Fatalf("status=%d cookie=%q", res.StatusCode, cookie)
	}
}

func TestDispatchRequestTimeDSHEnforcement(t *testing.T) {
	var calls, deny atomic.Int64
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	p.Config.DSHEnforce = "per-request"
	p.KeySource = keySourceFunc(func(string) (string, error) { return "sk-gateway-dummy-key", nil })
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		if deny.Load() == 1 {
			return "", &aigw.DSHDenial{Reason: "dsh_disabled"}
		}
		return "alice", nil
	})

	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusFound || cookie == "" {
		t.Fatalf("login status=%d", res.StatusCode)
	}
	// per-request mode consults on every tenant request: login (1) + first GET (2).
	if res := dshgwTenantGet(t, p, cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("per-request allowed: status=%d", res.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("authorize calls after login+get = %d, want 2", got)
	}

	// A denial revokes the session: the browser is sent back to the portal...
	deny.Store(1)
	if res := dshgwTenantGet(t, p, cookie); res.StatusCode != http.StatusFound {
		t.Fatalf("per-request denial: status=%d", res.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("authorize calls after denial = %d, want 3", got)
	}
	// ...and the revoked cookie stays unauthenticated without further checks (the cookie
	// fails session validation before the entitlement check runs).
	if res := dshgwTenantGet(t, p, cookie); res.StatusCode != http.StatusFound {
		t.Fatalf("revoked cookie must stay unauthenticated: status=%d", res.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("revoked cookie must not consult aigw: calls=%d", got)
	}
}

func TestDispatchLoginModeKeepsExistingSessions(t *testing.T) {
	var deny atomic.Int64
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	// DSHEnforce stays at the "login" default: no request-time checks, no KeySource needed.
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) {
		if deny.Load() == 1 {
			return "", &aigw.DSHDenial{Reason: "dsh_disabled"}
		}
		return "alice", nil
	})
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusFound || cookie == "" {
		t.Fatalf("login status=%d", res.StatusCode)
	}
	deny.Store(1)
	if res := dshgwTenantGet(t, p, cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("login mode must not revoke existing sessions: status=%d", res.StatusCode)
	}
}

func TestDispatchIntervalModeCachesTheVerdict(t *testing.T) {
	var calls, deny atomic.Int64
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	p.Config.DSHEnforce = "interval:3600"
	p.KeySource = keySourceFunc(func(string) (string, error) { return "sk-gateway-dummy-key", nil })
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		if deny.Load() == 1 {
			return "", &aigw.DSHDenial{Reason: "dsh_disabled"}
		}
		return "alice", nil
	})
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusFound || cookie == "" {
		t.Fatalf("login status=%d", res.StatusCode)
	}
	deny.Store(1)
	if res := dshgwTenantGet(t, p, cookie); res.StatusCode != http.StatusFound {
		t.Fatalf("interval denial: status=%d", res.StatusCode)
	}
	// Re-login (the stub allows logins again) and retry: the cached denial must answer
	// without another authorize call inside the interval.
	deny.Store(0)
	res, cookie = dshgwLogin(t, p)
	if res.StatusCode != http.StatusFound || cookie == "" {
		t.Fatalf("re-login status=%d", res.StatusCode)
	}
	if res := dshgwTenantGet(t, p, cookie); res.StatusCode != http.StatusFound {
		t.Fatalf("cached denial must persist within the interval: status=%d", res.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("authorize calls = %d, want 3 (login, first get, re-login)", got)
	}
}

// Legacy deployments (aigw without tenant mapping) resolve by key-prefix binding: an
// empty authorize tenant falls through to the registry prefix lookup.
func TestPortalLoginFallsBackToPrefixBinding(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "", nil })
	defer up.Close()
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusFound || cookie == "" {
		t.Fatalf("status=%d cookie=%q", res.StatusCode, cookie)
	}
	if res.Header.Get("Location") != "https://dsh.test:32601/" {
		t.Fatalf("location=%q", res.Header.Get("Location"))
	}
}

// An account mapped to a tenant the gateway does not know (created later, or a typo in
// the console) must not log in with a generic prefix error: it stays unauthenticated
// until the tenant exists.
func TestPortalLoginRejectsUnknownAuthTenant(t *testing.T) {
	p, _, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	p.Authorizer = authorizerFunc(func(context.Context, string) (string, error) { return "ghost", nil })
	defer up.Close()
	res, cookie := dshgwLogin(t, p)
	if res.StatusCode != http.StatusForbidden || cookie != "" {
		t.Fatalf("status=%d cookie=%q", res.StatusCode, cookie)
	}
}

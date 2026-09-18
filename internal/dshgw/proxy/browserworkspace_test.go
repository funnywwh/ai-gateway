package proxy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

type browserHandlerFunc func(http.ResponseWriter, *http.Request, registry.Tenant, string)

func (f browserHandlerFunc) ServeTenant(w http.ResponseWriter, r *http.Request, t registry.Tenant, binding string) {
	f(w, r, t, binding)
}

func TestBrowserRouteUsesGatewaySessionWithoutWorker(t *testing.T) {
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("worker called") }))
	up.Close()
	var bindings []string
	p.BrowserWorkspaces = browserHandlerFunc(func(w http.ResponseWriter, r *http.Request, got registry.Tenant, binding string) {
		if got.Name != tenant.Name {
			t.Error("wrong tenant")
		}
		bindings = append(bindings, binding)
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < 2; i++ {
		token := issue(t, p, tenant.Name, nil)
		req := httptest.NewRequest(http.MethodPost, "/browser-workspace/poll", nil)
		req.Host = "dsh.test:32601"
		req.Header.Set("Origin", "https://dsh.test:32601")
		req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName(tenant.Name), Value: token})
		w := httptest.NewRecorder()
		p.TenantHandler(tenant).ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if len(w.Result().Cookies()) == 0 {
			t.Fatal("session not refreshed")
		}
		if bindings[i] != fmt.Sprintf("%x", sha256.Sum256([]byte(token))) {
			t.Fatal("incorrect binding")
		}
	}
	if bindings[0] == bindings[1] {
		t.Fatal("distinct browser sessions share a binding")
	}
	if p.Exchanger.(*exchangeStub).count != 0 {
		t.Fatal("browser route required worker handshake")
	}
}

func TestBrowserRouteRejectsInvalidRequests(t *testing.T) {
	p, tenant, _, up := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	p.BrowserWorkspaces = browserHandlerFunc(func(http.ResponseWriter, *http.Request, registry.Tenant, string) {
		t.Error("rejected request reached browser service")
	})
	alice := issue(t, p, "alice", nil)
	bob := issue(t, p, "bob", nil)
	for _, tc := range []struct {
		name, path, origin, token string
		status                    int
	}{
		{"cross origin", "/browser-workspace/poll", "https://evil.test", alice, 403},
		{"wrong tenant", "/browser-workspace/poll", "https://dsh.test:32601", bob, 0},
		{"missing cookie", "/browser-workspace/poll", "https://dsh.test:32601", "", 0},
		{"malformed target", "/browser-workspace/%2e%2e/poll", "https://dsh.test:32601", alice, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Host = "dsh.test:32601"
			req.Header.Set("Origin", tc.origin)
			if tc.token != "" {
				req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName(tenant.Name), Value: tc.token})
			}
			w := httptest.NewRecorder()
			p.TenantHandler(tenant).ServeHTTP(w, req)
			if tc.status != 0 && w.Code != tc.status {
				t.Fatalf("status %d want %d", w.Code, tc.status)
			}
			if w.Code >= 200 && w.Code < 300 {
				t.Fatal("request succeeded")
			}
		})
	}
}

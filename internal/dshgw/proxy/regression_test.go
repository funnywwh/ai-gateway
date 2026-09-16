package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/session"
)

func TestLateResponseCannotOverwriteRefreshedCookie(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, tenant.Name, &session.Upstream{Name: "dsh-auth-test", Value: "old", Authority: authority})
	old, _ := p.Sessions.Get(token)
	if err := p.Sessions.SetUpstream(token, &session.Upstream{Name: "dsh-auth-test", Value: "new", Authority: authority}); err != nil {
		t.Fatal(err)
	}
	response := &http.Response{Header: http.Header{"Set-Cookie": []string{"dsh-auth-test=late"}}}
	transport := &retryTransport{p: p, token: token, tenant: tenant}
	if err := transport.captureResponseCookie(response, old.Upstream); err != nil {
		t.Fatal(err)
	}
	current, _ := p.Sessions.Get(token)
	if current.Upstream.Value != "new" {
		t.Fatalf("late response replaced new cookie: %s", current.Upstream.Value)
	}
}

func TestRegistryReloadFailureIsFailClosed(t *testing.T) {
	p, _, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("worker reached") }))
	defer worker.Close()
	if err := os.WriteFile(p.Config.RegistryPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dsh.test:32600"
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt canonical registry status=%d", w.Code)
	}
}

func TestLoginRequiresOneBodyKeyNotURLKey(t *testing.T) {
	for _, input := range []struct{ target, body string }{
		{"/login?key=sk-aaaaaaaaa-rest", ""},
		{"/login?key=sk-aaaaaaaaa-rest", "key=sk-aaaaaaaaa-rest"},
		{"/login", "key=sk-aaaaaaaaa-rest&key=sk-bbbbbbbbb-rest"},
	} {
		p, _, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		p.Validator = validatorFunc(func(context.Context, string) ([]string, error) {
			t.Error("ambiguous key reached validator")
			return nil, nil
		})
		req := httptest.NewRequest(http.MethodPost, input.target, strings.NewReader(input.body))
		req.Host = "dsh.test:32600"
		req.Header.Set("Origin", "https://dsh.test:32600")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		p.Dispatch().ServeHTTP(w, req)
		worker.Close()
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status=%d", input.target, input.body, w.Code)
		}
	}
}

func TestCookieBelongsToOnlyOneTenant(t *testing.T) {
	p, _, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cross-tenant worker reached") }))
	defer worker.Close()
	token := issue(t, p, "bob", nil)
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("foreign session status=%d", w.Code)
	}
}

func TestLiteralPercentPathsRemainExact(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/files/100%25?x=one+two&x=three%20four" {
			t.Errorf("rewritten URI: %q", r.RequestURI)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer worker.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	req := httptest.NewRequest(http.MethodGet, "/files/100%25?x=one+two&x=three%20four", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("legal percent path status=%d", w.Code)
	}
}

func TestFetchMetadataNavigationException(t *testing.T) {
	p := &Proxy{}
	for _, tc := range []struct {
		site, mode, dest string
		accepted         bool
	}{
		{"same-site", "navigate", "document", true},
		{"same-site", "no-cors", "image", false},
		{"cross-site", "no-cors", "script", false},
		{"same-origin", "cors", "empty", true},
		{"none", "navigate", "document", true},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api", nil)
		req.Header.Set("Sec-Fetch-Site", tc.site)
		req.Header.Set("Sec-Fetch-Mode", tc.mode)
		req.Header.Set("Sec-Fetch-Dest", tc.dest)
		if err := p.checkEdgeOrigin(req, "https://dsh.test:32601", false); (err == nil) != tc.accepted {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestOriginRejectionLogDoesNotContainSecretQuery(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer worker.Close()
	var logs bytes.Buffer
	p.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	req := httptest.NewRequest(http.MethodPost, "/api", nil)
	req.Header.Set("Origin", "https://dsh.test:32602/?key=sk-never-log-this")
	p.TenantHandler(tenant).ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(logs.String(), "sk-never-log-this") || strings.Contains(logs.String(), "?key=") {
		t.Fatalf("origin secret logged: %s", logs.String())
	}
}

func TestCookieTrailersAreFiltered(t *testing.T) {
	p, tenant, _, worker := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Trailer", "Set-Cookie")
		w.Header().Add("Trailer", "Set-Cookie2")
		w.Header().Add("Trailer", "X-Checksum")
		_, _ = io.WriteString(w, "body")
		w.Header().Set("Set-Cookie", "dsh-auth-leak=secret")
		w.Header().Set("Set-Cookie2", "secret=other")
		w.Header().Set("X-Checksum", "ok")
	}))
	defer worker.Close()
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(tenant.WorkerPort))
	token := issue(t, p, "alice", &session.Upstream{Name: "dsh-auth-test", Value: "held", Authority: authority})
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Host = "dsh.test:32601"
	req.AddCookie(&http.Cookie{Name: p.Config.SessionCookieName("alice"), Value: token})
	w := httptest.NewRecorder()
	p.Dispatch().ServeHTTP(w, req)
	response := w.Result()
	if w.Code != http.StatusOK || response.Trailer.Get("Set-Cookie") != "" || response.Trailer.Get("Set-Cookie2") != "" || response.Trailer.Get("X-Checksum") != "ok" {
		t.Fatalf("status=%d trailers=%v body=%s", w.Code, response.Trailer, w.Body.String())
	}
}

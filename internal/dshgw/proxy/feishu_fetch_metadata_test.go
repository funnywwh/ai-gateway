package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Preserve the gateway fence. The fix must reset the navigation chain at the
// OAuth callback, not exempt a portal or tenant request from these checks.
func TestFeishuRedirectMetadataFenceAndFreshNavigation(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tenant-ok")
	}))
	ticket := setup.signer(setup.tenant, time.Minute, "fetch-metadata-handoff")
	request := func(site, mode, dest string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/login/feishu", nil)
		req.Host = "dsh.test:32600"
		req.Header.Set("Sec-Fetch-Site", site)
		req.Header.Set("Sec-Fetch-Mode", mode)
		req.Header.Set("Sec-Fetch-Dest", dest)
		req.AddCookie(&http.Cookie{Name: feishuTicketCookieName, Value: ticket})
		return req
	}
	for _, metadata := range [][3]string{
		{"cross-site", "navigate", "document"},
		{"cross-site", "no-cors", "image"},
		{"cross-site", "no-cors", "script"},
		{"same-site", "navigate", "iframe"},
		{"same-site", "cors", "empty"},
	} {
		recorder := httptest.NewRecorder()
		setup.proxy.PortalHandler().ServeHTTP(recorder, request(metadata[0], metadata[1], metadata[2]))
		if recorder.Code != http.StatusForbidden || strings.TrimSpace(recorder.Body.String()) != "forbidden" || len(recorder.Result().Cookies()) != 0 {
			t.Fatalf("rejected portal metadata %v did not retain its fence", metadata)
		}
	}

	// Origin rejection must not consume the ticket. A committed callback page
	// starts this fresh same-site document navigation with the very same cookie.
	recorder := httptest.NewRecorder()
	setup.proxy.PortalHandler().ServeHTTP(recorder, request("same-site", "navigate", "document"))
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "http://dsh.test:32601/" {
		t.Fatalf("fresh portal navigation failed: status %d", recorder.Code)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == setup.proxy.Config.SessionCookieName(setup.tenant) {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("fresh navigation did not establish the tenant session")
	}
	tenant, ok := setup.proxy.Registry.Get(setup.tenant)
	if !ok {
		t.Fatal("fixture tenant is missing")
	}
	for _, site := range []string{"cross-site", "same-site"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "dsh.test:32601"
		req.AddCookie(sessionCookie)
		req.Header.Set("Sec-Fetch-Site", site)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
		result := httptest.NewRecorder()
		setup.proxy.TenantHandler(tenant).ServeHTTP(result, req)
		if site == "cross-site" {
			if result.Code != http.StatusForbidden {
				t.Fatalf("tenant cross-site fence: status %d, want 403", result.Code)
			}
		} else if result.Code != http.StatusOK || result.Body.String() != "tenant-ok" {
			t.Fatalf("fresh same-site chain did not reach the tenant: status %d", result.Code)
		}
	}

	replay := httptest.NewRecorder()
	setup.proxy.PortalHandler().ServeHTTP(replay, request("same-site", "navigate", "document"))
	if replay.Code != http.StatusForbidden || !strings.Contains(replay.Body.String(), "已经使用过") {
		t.Fatal("sequential ticket replay must still be rejected after the successful navigation")
	}
}

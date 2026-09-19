package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/feishu"
)

func crossSiteFeishuNavigation(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	return req
}

func assertHandoffDocument(t *testing.T, recorder *httptest.ResponseRecorder, target string) {
	t.Helper()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Location") != "" {
		t.Fatalf("handoff must commit a 200 document, got status %d", recorder.Code)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("handoff lacks non-cacheable, non-referring HTML protections")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `id="feishu-continue"`) || !strings.Contains(body, target) {
		t.Fatal("handoff lacks its fixed configured continuation link")
	}
	_, scriptAndRest, ok := strings.Cut(body, "<script>")
	if !ok || strings.Count(body, "<script>") != 1 {
		t.Fatal("handoff must have exactly one fixed script")
	}
	script, _, ok := strings.Cut(scriptAndRest, "</script>")
	if !ok || script != feishuHandoffScript {
		t.Fatal("handoff contains an unexpected executable script")
	}
	digest := sha256.Sum256([]byte(script))
	csp := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'none'",
		"'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'",
	} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("handoff CSP lacks %s", directive)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatal("handoff CSP must allow only its hash-pinned script")
	}
}

func TestFeishuHandoffDocumentSelection(t *testing.T) {
	const callback = "https://chat.test/feishu/callback"
	const target = "https://chat.test:18300/login/feishu"
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"no metadata", func(r *http.Request) { r.Header = make(http.Header) }},
		{"same site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }},
		{"same origin", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }},
		{"cors fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "cors") }},
		{"image", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "image") }},
		{"iframe", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "iframe") }},
		{"missing destination", func(r *http.Request) { r.Header.Del("Sec-Fetch-Dest") }},
		{"duplicate site", func(r *http.Request) { r.Header.Add("Sec-Fetch-Site", "cross-site") }},
		{"duplicate mode", func(r *http.Request) { r.Header.Add("Sec-Fetch-Mode", "navigate") }},
		{"duplicate destination", func(r *http.Request) { r.Header.Add("Sec-Fetch-Dest", "document") }},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://idp.test") }},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{"empty origin present", func(r *http.Request) { r.Header.Set("Origin", "") }},
		{"upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }},
		{"post", func(r *http.Request) { r.Method = http.MethodPost }},
		{"head", func(r *http.Request) { r.Method = http.MethodHead }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := crossSiteFeishuNavigation(callback)
			tc.mutate(req)
			recorder := httptest.NewRecorder()
			redirectFeishuHandoff(recorder, req, callback, target)
			if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != target {
				t.Fatal("non-matching requests must retain the existing 303 contract")
			}
			if strings.Contains(recorder.Body.String(), "feishu-continue") {
				t.Fatal("non-matching request received a handoff document")
			}
		})
	}

	for _, target := range []string{
		"https://other.test:18300/login/feishu",
		"http://chat.test:18300/login/feishu",
		"https://chat.test:18300/login/feishu?ticket=never-render-this",
		"https://chat.test:18300/login/feishu#fragment",
		"https://user@chat.test:18300/login/feishu",
		"/login/feishu",
	} {
		if feishuHandoffNeedsDocument(crossSiteFeishuNavigation(callback), callback, target) {
			t.Fatalf("unsupported deployment must not get a handoff document: %s", target)
		}
	}

	recorder := httptest.NewRecorder()
	recorder.Header().Add("Set-Cookie", "test-cookie=opaque; Path=/; Secure; HttpOnly; SameSite=Lax")
	recorder.Header().Set("Vary", "Accept-Encoding")
	redirectFeishuHandoff(recorder, crossSiteFeishuNavigation(callback+"?code=do-not-echo&state=secret-state"), callback, target)
	assertHandoffDocument(t, recorder, target)
	if len(recorder.Result().Cookies()) != 1 || !strings.Contains(strings.Join(recorder.Header().Values("Vary"), ","), "Accept-Encoding") {
		t.Fatal("handoff discarded caller cookies or cache variation")
	}
	for _, secret := range []string{"do-not-echo", "secret-state", "opaque"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatal("handoff reflected request or cookie credentials")
		}
	}
}

func TestFeishuHandoffEscapesConfiguredLink(t *testing.T) {
	const callback = "https://chat.test/feishu/callback"
	const target = `https://chat.test:18300/path"><script>alert(1)</script>`
	recorder := httptest.NewRecorder()
	redirectFeishuHandoff(recorder, crossSiteFeishuNavigation(callback), callback, target)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected escaped handoff, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, target) || strings.Contains(body, "<script>alert(1)</script>") || strings.Count(body, "<script>") != 1 {
		t.Fatal("configured target escaped its HTML attribute or entered JavaScript")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("continuation target was not HTML-escaped")
	}
}

func TestFeishuDSHBrowserCallbackCommitsDocumentWithCookie(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	f.bindAndEnable(t, key, true, "alice")
	f.api.deps.Feishu.RedirectURI = "https://chat.test/feishu/callback"
	f.api.deps.Feishu.PortalURL = "https://chat.test:18300"
	state, err := f.states.Sign(feishu.FlowDSHLogin, 0, "", "browser-handoff-success")
	if err != nil {
		t.Fatal(err)
	}
	req := crossSiteFeishuNavigation("https://chat.test/feishu/callback?code=private-code&state=" + url.QueryEscape(state))
	recorder := httptest.NewRecorder()
	f.api.handleFeishuCallback(recorder, req)
	assertHandoffDocument(t, recorder, "https://chat.test:18300/login/feishu")
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != feishuTicketCookieName {
		t.Fatal("successful OAuth callback must retain exactly its ticket cookie")
	}
	cookie := cookies[0]
	if cookie.Domain != "" || cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 120 {
		t.Fatal("browser handoff changed the ticket cookie security attributes")
	}
	ticket, err := f.api.deps.Feishu.Tickets.VerifyTicket(cookie.Value)
	if err != nil || ticket.Tenant != "alice" {
		t.Fatal("browser handoff did not retain a valid signed tenant ticket")
	}
	for _, secret := range []string{cookie.Value, state, "private-code", "ticket="} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatal("same-host handoff leaked a bearer or OAuth parameter into HTML")
		}
	}
	replay := httptest.NewRecorder()
	f.api.handleFeishuCallback(replay, req)
	if replay.Code != http.StatusBadRequest || len(replay.Result().Cookies()) != 0 || strings.Contains(replay.Body.String(), "feishu-continue") {
		t.Fatal("replayed OAuth state received a successful handoff")
	}
}

func TestFeishuDSHBrowserRefusalsNeverRenderHandoff(t *testing.T) {
	for _, name := range []string{"invalid state", "expired state", "unbound", "disabled", "exchange failure"} {
		t.Run(name, func(t *testing.T) {
			f := newFeishuFixture(t)
			key := f.seedKey(t)
			f.api.deps.Feishu.RedirectURI = "https://chat.test/feishu/callback"
			f.api.deps.Feishu.PortalURL = "https://chat.test:18300"
			if name != "unbound" {
				f.bindAndEnable(t, key, name != "disabled", "alice")
			}
			state, err := f.states.Sign(feishu.FlowDSHLogin, 0, "", "browser-refusal")
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "invalid state":
				state = "invalid"
			case "expired state":
				f.now = f.now.Add(11 * time.Minute)
			case "exchange failure":
				f.stub.tokenBody = `{"code":20003,"msg":"invalid authorization code"}`
			}
			recorder := httptest.NewRecorder()
			f.api.handleFeishuCallback(recorder, crossSiteFeishuNavigation("https://chat.test/feishu/callback?code=c&state="+url.QueryEscape(state)))
			if recorder.Code == http.StatusOK || len(recorder.Result().Cookies()) != 0 || strings.Contains(recorder.Body.String(), "feishu-continue") {
				t.Fatal("failed OAuth or entitlement check received a handoff or ticket")
			}
		})
	}
}

func TestFeishuDSHCrossHostBrowserRetainsQueryRedirect(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	f.bindAndEnable(t, key, true, "alice")
	f.api.deps.Feishu.RedirectURI = "https://chat.test/feishu/callback"
	f.api.deps.Feishu.PortalURL = "https://other.test:18300"
	state, err := f.states.Sign(feishu.FlowDSHLogin, 0, "", "cross-host-browser")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	f.api.handleFeishuCallback(recorder, crossSiteFeishuNavigation("https://chat.test/feishu/callback?code=c&state="+url.QueryEscape(state)))
	if recorder.Code != http.StatusSeeOther || strings.Contains(recorder.Body.String(), "feishu-continue") {
		t.Fatal("cross-host query-ticket path must not use the document handoff")
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil || location.Host != "other.test:18300" || location.Query().Get("ticket") == "" {
		t.Fatal("cross-host fallback contract changed")
	}
}

package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/feishu"
)

const feishuTicketSecret = "ticket-secret-for-tests"

// feishuSetup turns the standard fixture into one with Feishu login enabled, and returns a
// signer that mints tickets the way aigw does.
type feishuSetup struct {
	proxy  *Proxy
	tenant string
	signer func(tenant string, expires time.Duration, nonce string) string
	now    time.Time
}

func setupFeishu(t *testing.T, worker http.Handler) *feishuSetup {
	t.Helper()
	p, tenant, _, _ := fixture(t, worker)
	verifier, err := feishu.New([]byte(feishuTicketSecret))
	if err != nil {
		t.Fatal(err)
	}
	// The deployment under test is plain HTTP on the LAN, which is the shape where the session
	// cookie must not be Secure (see the M61 design §public_scheme).
	p.Config.PublicScheme = "http"
	setup := &feishuSetup{proxy: p, tenant: tenant.Name, now: time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)}
	verifier.Now = func() time.Time { return setup.now }
	p.Feishu = &FeishuPortal{Enabled: true, AigwLoginURL: "http://aigw.test:8090/feishu/login", Verifier: verifier}
	// The recheck needs the tenant's worker key, so the fixture supplies one: a gateway that
	// cannot read it must refuse the login (which the last case below asserts).
	p.KeySource = keySourceStub("sk-gw-tenant-worker-key")
	setup.signer = func(tenantName string, expires time.Duration, nonce string) string {
		return signTicket(t, tenantName, setup.now.Add(expires), nonce)
	}
	return setup
}

// signTicket reproduces aigw's wire format: base64url(payload).base64url(HMAC).
func signTicket(t *testing.T, tenant string, expires time.Time, nonce string) string {
	t.Helper()
	payload := `{"v":1,"mode":"dsh","tenant":"` + tenant + `","key_id":7,"account_id":3,"open_id":"ou_alice","nonce":"` +
		nonce + `","exp":` + itoaTest(expires.Unix()) + `}`
	encoded := base64URL(payload)
	return encoded + "." + ticketMAC(encoded)
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

// portalRequest drives the portal's Feishu login with the ticket delivered the way the
// deployment does: as a host-only cookie, optionally as a query parameter too.
func (s *feishuSetup) portalRequest(t *testing.T, ticket, query string) *http.Response {
	t.Helper()
	target := "http://dsh.test:32600/login/feishu"
	if query != "" {
		target += "?ticket=" + url.QueryEscape(query)
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ticket != "" {
		req.AddCookie(&http.Cookie{Name: feishuTicketCookieName, Value: ticket})
	}
	recorder := httptest.NewRecorder()
	s.proxy.PortalHandler().ServeHTTP(recorder, req)
	return recorder.Result()
}

// A ticket for a tenant whose account is still enabled logs the person in with exactly the
// same cookie a key login would issue.
func TestFeishuLoginStartsTheTenantSession(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ticket := setup.signer(setup.tenant, time.Minute, "nonce-1")

	res := setup.portalRequest(t, ticket, "")
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want a redirect into the tenant", res.StatusCode)
	}
	if got, want := res.Header.Get("Location"), "http://dsh.test:32601/"; got != want {
		t.Fatalf("location = %q, want %q", got, want)
	}
	var session *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == setup.proxy.Config.SessionCookieName(setup.tenant) {
			session = cookie
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("no session cookie was issued")
	}
	if !session.HttpOnly || session.Path != "/" {
		t.Fatalf("session cookie attributes are wrong: %+v", session)
	}
	if session.Secure {
		t.Error("a plain-HTTP deployment must not issue a Secure session cookie: the browser would drop it")
	}
	// The session is a real one: it maps back to the tenant, which is what every later
	// request checks.
	stored, err := setup.proxy.Sessions.Get(session.Value)
	if err != nil {
		t.Fatalf("the issued session does not resolve: %v", err)
	}
	if stored.Tenant != setup.tenant {
		t.Fatalf("session tenant = %q", stored.Tenant)
	}
}

// A ticket is single use: replaying it (a refresh, a shared link, browser history) must not
// sign anyone in twice.
func TestFeishuTicketCannotBeReplayed(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ticket := setup.signer(setup.tenant, time.Minute, "nonce-replay")

	if res := setup.portalRequest(t, ticket, ""); res.StatusCode != http.StatusFound {
		t.Fatalf("first login: status=%d", res.StatusCode)
	}
	res := setup.portalRequest(t, ticket, "")
	if res.StatusCode == http.StatusFound {
		t.Fatal("a replayed ticket still logged in")
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "已经使用过") {
		t.Fatalf("the page must say the link was already used: %s", body)
	}
}

// Expiry and a bad signature are refused, and the page tells the person to start again.
func TestFeishuTicketRefusalsAreRefused(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := map[string]struct {
		ticket string
		want   string
	}{
		"expired":        {setup.signer(setup.tenant, -time.Minute, "nonce-expired"), "过期"},
		"tampered":       {setup.signer(setup.tenant, time.Minute, "nonce-tamper")[:40] + "AAAA", "无效"},
		"foreign key":    {signWithOtherKey(t, setup.tenant, setup.now.Add(time.Minute)), "无效"},
		"unknown tenant": {setup.signer("bob", time.Minute, "nonce-bob"), "不存在"},
	}
	for name, test := range cases {
		res := setup.portalRequest(t, test.ticket, "")
		if res.StatusCode == http.StatusFound {
			t.Errorf("%s: logged in", name)
			continue
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), test.want) {
			t.Errorf("%s: page does not explain the refusal (want %q): %s", name, test.want, body)
		}
		// No session may be created for a refused ticket.
		for _, cookie := range res.Cookies() {
			if strings.HasPrefix(cookie.Name, "dshgw_s_") {
				t.Errorf("%s: a refused login issued a session cookie", name)
			}
		}
	}
}

func signWithOtherKey(t *testing.T, tenant string, expires time.Time) string {
	t.Helper()
	payload := `{"v":1,"mode":"dsh","tenant":"` + tenant + `","key_id":7,"account_id":3,"open_id":"ou_alice","nonce":"nonce-foreign","exp":` +
		itoaTest(expires.Unix()) + `}`
	encoded := base64URL(payload)
	other, err := feishu.New([]byte("a-different-secret"))
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	// Sign with the wrong key by reusing the helper with a different secret.
	return encoded + "." + ticketMACWithKey(encoded, "a-different-secret")
}

// A missing ticket is refused rather than treated as "no identity needed".
func TestFeishuLoginWithoutATicketIsRefused(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	res := setup.portalRequest(t, "", "")
	if res.StatusCode == http.StatusFound {
		t.Fatal("a request without a ticket logged in")
	}
	// A cookie and a query that disagree is a tampered request.
	ticket := setup.signer(setup.tenant, time.Minute, "nonce-mismatch")
	other := setup.signer(setup.tenant, time.Minute, "nonce-other")
	res = setup.portalRequest(t, ticket, other)
	if res.StatusCode == http.StatusFound {
		t.Fatal("a mismatched ticket pair logged in")
	}
}

// The entitlement check runs again here, so a revocation that aigw only learns about later
// (or that happens between issue and redemption) still stops the login — and a gateway that
// cannot check must refuse rather than admit.
func TestFeishuLoginRechecksTheAccountEntitlement(t *testing.T) {
	cases := map[string]struct {
		authorize func(context.Context, string) (string, error)
		want      string
	}{
		"dsh disabled": {
			authorize: func(context.Context, string) (string, error) { return "", &aigw.DSHDenial{Reason: "dsh_disabled"} },
			want:      "未启用 dsh",
		},
		"account suspended": {
			authorize: func(context.Context, string) (string, error) { return "", &aigw.DSHDenial{Reason: "account_status"} },
			want:      "已停用",
		},
		"worker key revoked": {
			authorize: func(context.Context, string) (string, error) { return "", aigw.ErrInvalidKey },
			want:      "吊销",
		},
		"aigw unreachable": {
			authorize: func(context.Context, string) (string, error) { return "", &aigw.StatusError{Status: 502} },
			want:      "暂不可用",
		},
	}
	for name, test := range cases {
		setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		setup.proxy.Authorizer = authorizerFunc(test.authorize)
		res := setup.portalRequest(t, setup.signer(setup.tenant, time.Minute, "nonce-"+strings.ReplaceAll(name, " ", "-")), "")
		if res.StatusCode == http.StatusFound {
			t.Errorf("%s: logged in despite the refusal", name)
			continue
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), test.want) {
			t.Errorf("%s: page does not explain the refusal (want %q): %s", name, test.want, body)
		}
	}
	// An unavailable authorizer is a 503, never a silent success: the person can fall back to
	// the key form, which asks the same question at login time.
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	setup.proxy.Authorizer = nil
	if res := setup.portalRequest(t, setup.signer(setup.tenant, time.Minute, "nonce-no-authorizer"), ""); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the check cannot run", res.StatusCode)
	}
}

// With the feature off the portal must look exactly as it did before: no button, no route.
func TestFeishuDisabledKeepsThePortalUnchanged(t *testing.T) {
	p, _, _, _ := fixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	page := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dsh.test:32600/", nil)
	p.PortalHandler().ServeHTTP(page, req)
	body := page.Body.String()
	// The stylesheet always carries the .feishu rule (it is one static document), so what must
	// be absent is the button itself and any mention to a person.
	if strings.Contains(body, `<a class="feishu"`) {
		t.Fatalf("the portal offers a Feishu button while the feature is off: %s", body)
	}
	if strings.Contains(body, "飞书登录") {
		t.Fatalf("the portal mentions Feishu login while the feature is off: %s", body)
	}
	if !strings.Contains(body, "aigw API Key") {
		t.Fatal("the key form must stay: it is both the fallback and the way to bind")
	}
	res := httptest.NewRecorder()
	p.PortalHandler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://dsh.test:32600/login/feishu", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("the login route exists while the feature is off: %d", res.Code)
	}
}

// When it is on, the portal offers the button and points it at aigw.
func TestFeishuEnabledRendersThePortalButton(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	recorder := httptest.NewRecorder()
	setup.proxy.PortalHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://dsh.test:32600/", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, "飞书登录") {
		t.Fatalf("the portal does not offer Feishu login: %s", body)
	}
	if !strings.Contains(body, setup.proxy.Feishu.AigwLoginURL) {
		t.Fatalf("the button must point at aigw's login entry: %s", body)
	}
	// The key form stays: it is the fallback when Feishu is unreachable and the way a person
	// gets bound in the first place.
	if !strings.Contains(body, "aigw API Key") {
		t.Fatal("the key form must remain available")
	}
}

// A cross-site request must not be able to start a login, exactly as for the key form.
func TestFeishuLoginRefusesACrossSiteOrigin(t *testing.T) {
	setup := setupFeishu(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ticket := setup.signer(setup.tenant, time.Minute, "nonce-origin")
	req := httptest.NewRequest(http.MethodGet, "http://dsh.test:32600/login/feishu", nil)
	req.AddCookie(&http.Cookie{Name: feishuTicketCookieName, Value: ticket})
	req.Header.Set("Origin", "http://evil.test")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	recorder := httptest.NewRecorder()
	setup.proxy.PortalHandler().ServeHTTP(recorder, req)
	if recorder.Code == http.StatusFound {
		t.Fatal("a cross-site request started a session")
	}
}

// The error page the portal renders must explain each reason aigw can send.
func TestFeishuErrorMessageCoversEveryReason(t *testing.T) {
	for _, reason := range []string{
		"unbound", "dsh_disabled", "account_status", "tenant_missing", "tenant", "ticket",
		"expired", "already used", "revoked", "unavailable", "cancelled", "no_app_permission",
		"rate_limited", "app_error", "invalid", "error",
	} {
		message := feishuErrorMessage(reason)
		if strings.TrimSpace(message) == "" {
			t.Errorf("%s: empty message", reason)
		}
		if strings.Contains(message, reason) {
			t.Errorf("%s: the message must be for a person, not echo the code (%q)", reason, message)
		}
	}
	if !strings.Contains(feishuErrorMessage("something-new"), "something-new") {
		t.Error("an unknown reason must be surfaced rather than silently replaced")
	}
}

// keySourceStub stands in for the per-tenant worker key file.
type keySourceStub string

func (s keySourceStub) Key(string) (string, error) { return string(s), nil }

// base64URL and the ticket MAC are reproduced here on purpose: this test plays the role of
// aigw, so it must build the wire form from the shared contract rather than by calling any
// helper of the implementation under test.
func base64URL(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func ticketMAC(encoded string) string { return ticketMACWithKey(encoded, feishuTicketSecret) }

func ticketMACWithKey(encoded, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("feishu-ticket:" + encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

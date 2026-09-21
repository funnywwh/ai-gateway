package proxy

import (
	"context"
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

// The key picker (M72) is the third way into a tenant, and the only one where the browser posts a
// value the gateway has to distrust. These tests cover the two halves that matter: the list of
// keys comes from aigw rather than from the form, and a submitted id that is not in it — a
// revoked key, another account's key, a hand-made post — is refused without signing anyone in.

// pickSetup is the standard fixture with a picker and a stub aigw that answers one account.
type pickSetup struct {
	proxy    *Proxy
	tenant   string
	verifier *feishu.Verifier
	// keys is what the stub aigw reports for the tenant's worker key.
	keys []aigw.KeyRef
	// accountID is what the stub reports as the tenant's account.
	accountID int64
}

func setupPick(t *testing.T, worker http.Handler) *pickSetup {
	t.Helper()
	p, tenant, _, _ := fixture(t, worker)
	p.Config.PublicScheme = "http"
	verifier, err := feishu.New([]byte(feishuTicketSecret))
	if err != nil {
		t.Fatal(err)
	}
	setup := &pickSetup{proxy: p, tenant: tenant.Name, verifier: verifier, accountID: 3}
	verifier.Now = func() time.Time { return time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC) }
	verifier.SetIssueTTL(time.Minute)
	p.Feishu = &FeishuPortal{Enabled: true, AigwLoginURL: "http://aigw.test:8090/feishu/login", Verifier: verifier}
	// The picker resolves tenants through aigw, so the authorizer answers as both roles: the
	// admission check (DSHAuthorizer) and the naming/id call (AccountNamer).
	p.Authorizer = pickAuthorizer{setup: setup}
	p.KeySource = keySourceStub("sk-gw-tenant-worker-key")
	setup.keys = []aigw.KeyRef{
		{ID: 11, Name: "laptop", KeyPrefix: "sk-gw-laptop", LastUsedAt: "2026-09-21T10:00:00Z"},
		{ID: 12, Name: "phone", KeyPrefix: "sk-gw-phone"},
	}
	return setup
}

// pickAuthorizer plays aigw: it admits the tenant and answers the account id and key list the
// picker needs.
type pickAuthorizer struct{ setup *pickSetup }

func (a pickAuthorizer) Authorize(context.Context, string) (string, error) {
	return a.setup.tenant, nil
}

func (a pickAuthorizer) Identity(context.Context, string) (aigw.Identity, error) {
	return aigw.Identity{Tenant: a.setup.tenant, Account: "李智超(colin)", AccountID: a.setup.accountID, Keys: a.setup.keys}, nil
}

// pickRequest drives the picker: GET with a ticket in the query, POST with the ticket and the
// chosen key id in the form. It goes through Dispatch(), like a real browser: the portal is
// reached on the public host and portal port, and a POST needs the portal's own Origin.
func (s *pickSetup) pickRequest(t *testing.T, method, ticket, keyID string) *http.Response {
	t.Helper()
	target := "/login/pick"
	var req *http.Request
	var err error
	if method == http.MethodPost {
		form := url.Values{}
		if ticket != "" {
			form.Set("ticket", ticket)
		}
		if keyID != "" {
			form.Set("key_id", keyID)
		}
		req, err = http.NewRequest(method, target, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "http://dsh.test:32600")
		}
	} else {
		if ticket != "" {
			target += "?ticket=" + url.QueryEscape(ticket)
		}
		req, err = http.NewRequest(method, target, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	// The public authority decides which surface serves the request, so the Host has to be the
	// deployment's, not the one the URL implies.
	req.Host = "dsh.test:32600"
	recorder := httptest.NewRecorder()
	s.proxy.Dispatch().ServeHTTP(recorder, req)
	return recorder.Result()
}

// keyLogin posts a key to the portal's login form, through Dispatch like a browser.
func (s *pickSetup) keyLogin(t *testing.T, key string) *http.Response {
	t.Helper()
	form := url.Values{"key": {key}}
	req, err := http.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://dsh.test:32600")
	req.Host = "dsh.test:32600"
	recorder := httptest.NewRecorder()
	s.proxy.Dispatch().ServeHTTP(recorder, req)
	return recorder.Result()
}

// The picker renders one radio button per key aigw reported, and nothing about key material.
func TestKeyPickRendersTheAccountsKeys(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ticket, err := setup.verifier.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	res := setup.pickRequest(t, http.MethodGet, ticket, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the picker", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	page := string(body)
	for _, want := range []string{`name="key_id" value="11"`, `name="key_id" value="12"`, "laptop", "phone", "李智超(colin)"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the picker is missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "sk-gw-tenant-worker-key") {
		t.Fatal("the picker leaked key material")
	}
	// Rendering does not consume the ticket: the person still has to submit the form.
	if res := setup.pickRequest(t, http.MethodGet, ticket, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("a second look at the picker must work: status=%d", res.StatusCode)
	}
}

// Choosing a key signs the person in with the same session a key login would issue, and the
// choice is what the audit trail names.
func TestKeyPickSignsInWithTheChosenKey(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var prepared string
	setup.proxy.LoginPrepare = prepareFunc(func(_ context.Context, tenant, submitted string) error {
		prepared = tenant + "|" + submitted
		return nil
	})
	ticket, err := setup.verifier.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	res := setup.pickRequest(t, http.MethodPost, ticket, "12")
	if res.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want a redirect into the tenant: %s", res.StatusCode, body)
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
	// The chosen key never becomes the tenant's credential: the lifecycle hook is called with an
	// empty submitted key, exactly as a Feishu login does (M72 D6).
	if prepared != setup.tenant+"|" {
		t.Fatalf("login preparation saw %q, want no submitted key", prepared)
	}
}

// A submission the gateway did not offer — a key id from another account, a revoked key, a
// hand-made post — cannot sign anyone in, and the page keeps the choice available.
func TestKeyPickRefusesAKeyThatIsNotInTheList(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ticket, err := setup.verifier.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	res := setup.pickRequest(t, http.MethodPost, ticket, "999")
	if res.StatusCode == http.StatusFound {
		t.Fatal("an unknown key id signed the person in")
	}
	for _, cookie := range res.Cookies() {
		if strings.HasPrefix(cookie.Name, "dshgw_s_") {
			t.Fatal("a refused pick issued a session cookie")
		}
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "不可用") {
		t.Fatalf("the page must explain the refusal: %s", body)
	}
	// A missing or unparsable id is a 400 — with a fresh ticket, because the submission above
	// spent the previous one (which is exactly what the replay test asserts).
	fresh, err := setup.verifier.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	if res := setup.pickRequest(t, http.MethodPost, fresh, ""); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("no key id: status=%d, want 400", res.StatusCode)
	}
}

// A pick ticket is single use: submitting the form twice (a back button, a double click) must not
// create a second session.
func TestKeyPickTicketCannotBeReplayed(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ticket, err := setup.verifier.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	if res := setup.pickRequest(t, http.MethodPost, ticket, "11"); res.StatusCode != http.StatusFound {
		t.Fatalf("first pick: status=%d", res.StatusCode)
	}
	res := setup.pickRequest(t, http.MethodPost, ticket, "11")
	if res.StatusCode == http.StatusFound {
		t.Fatal("a replayed pick signed the person in again")
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "已经使用过") {
		t.Fatalf("the page must say the link was already used: %s", body)
	}
}

// A ticket for an account this gateway does not serve (or one minted by another deployment) is
// refused without a session, and the person is sent back to the login page with a reason.
func TestKeyPickRefusesAForeignAccount(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	other, err := feishu.New([]byte(feishuTicketSecret))
	if err != nil {
		t.Fatal(err)
	}
	other.Now = setup.verifier.Now
	ticket, err := other.SignPick(4242, "ou_nobody")
	if err != nil {
		t.Fatal(err)
	}
	res := setup.pickRequest(t, http.MethodGet, ticket, "")
	if res.StatusCode == http.StatusOK {
		t.Fatal("a ticket for an unserved account rendered the picker")
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "租户") {
		t.Fatalf("the page must explain that the tenant is unknown: %s", body)
	}
	// A ticket signed with another key is refused as invalid, not as "unknown account".
	foreign, err := feishu.New([]byte("a-different-secret"))
	if err != nil {
		t.Fatal(err)
	}
	foreign.Now = setup.verifier.Now
	bad, err := foreign.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	res = setup.pickRequest(t, http.MethodGet, bad, "")
	if res.StatusCode == http.StatusOK {
		t.Fatal("a foreign signature rendered the picker")
	}
	body, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(body), "无效") {
		t.Fatalf("the page must say the link is invalid: %s", body)
	}
}

// A deployment that cannot reach aigw must not answer "this account is not served": the reason a
// person reads has to be about availability, not about their account existing.
func TestKeyPickReportsAuthorizationUnavailableAsSuch(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	setup.proxy.Authorizer = failingAuthorizer{}
	ticket, err := setup.verifier.SignPick(setup.accountID, "ou_alice")
	if err != nil {
		t.Fatal(err)
	}
	res := setup.pickRequest(t, http.MethodGet, ticket, "")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "暂不可用") {
		t.Fatalf("the page must say the service is unavailable: %s", body)
	}
}

// failingAuthorizer models an aigw that cannot be reached.
type failingAuthorizer struct{}

func (failingAuthorizer) Authorize(context.Context, string) (string, error) {
	return "", &aigw.StatusError{Status: http.StatusBadGateway}
}

func (failingAuthorizer) Identity(context.Context, string) (aigw.Identity, error) {
	return aigw.Identity{}, &aigw.StatusError{Status: http.StatusBadGateway}
}

// A key login for a multi-key account redirects to the picker instead of signing in, and a
// single-key account keeps the one-step login it always had.
func TestKeyLoginOffersThePickerOnlyWhenThereIsAChoice(t *testing.T) {
	setup := setupPick(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	setup.proxy.Validator = validatorFunc(func(context.Context, string) ([]aigw.Model, error) {
		return []aigw.Model{{ID: "m"}}, nil
	})
	login := func(t *testing.T) *http.Response {
		t.Helper()
		return setup.keyLogin(t, "sk-gw-some-user-key-abcdef")
	}

	res := login(t)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the picker", res.StatusCode)
	}
	location := res.Header.Get("Location")
	if !strings.Contains(location, "/login/pick?ticket=") {
		t.Fatalf("location = %q, want the picker with a ticket", location)
	}
	// The ticket the login minted has to verify here, and it has to name the fixture's account.
	wire := location[strings.Index(location, "ticket=")+len("ticket="):]
	ticket, err := setup.verifier.VerifyPick(wire)
	if err != nil {
		t.Fatalf("the minted pick ticket does not verify: %v", err)
	}
	if ticket.AccountID != setup.accountID {
		t.Fatalf("ticket account = %d, want %d", ticket.AccountID, setup.accountID)
	}
	for _, cookie := range res.Cookies() {
		if strings.HasPrefix(cookie.Name, "dshgw_s_") {
			t.Fatal("the picker path issued a session before the choice was made")
		}
	}

	// One usable key: no choice to make, so the login completes as it did before M72.
	setup.keys = setup.keys[:1]
	res = login(t)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("single-key login: status=%d, want the tenant redirect", res.StatusCode)
	}
	if got, want := res.Header.Get("Location"), "http://dsh.test:32601/"; got != want {
		t.Fatalf("single-key location = %q, want %q", got, want)
	}
}

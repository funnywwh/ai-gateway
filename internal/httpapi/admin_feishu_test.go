package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/feishu"
)

const (
	feishuCallbackPath = "/feishu/callback"
	feishuLoginPath    = "/feishu/login"
	feishuInvitePath   = "/feishu/invite"
	feishuPortalURL    = "http://dsh.example:18300"
)

// feishuStub answers the two endpoints the server calls. The authorization page is not one
// of them: the browser goes there, and the tests assert the redirect the server builds.
type feishuStub struct {
	server *httptest.Server
	// identity is what the user-info call answers.
	identity feishu.Identity
	// tokenBody overrides the token answer (to exercise refusals).
	tokenBody string
	// infoBody overrides the identity answer.
	infoBody string
	// calls counts the exchanges, so a test can prove nothing was called.
	calls int
}

func newFeishuStub(t *testing.T) *feishuStub {
	t.Helper()
	stub := &feishuStub{identity: feishu.Identity{OpenID: "ou_alice", UnionID: "on_alice", Name: "张三"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		stub.calls++
		body := stub.tokenBody
		if body == "" {
			body = `{"code":0,"access_token":"u-token","expires_in":7200}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		body := stub.infoBody
		if body == "" {
			body = `{"code":0,"data":{"open_id":"` + stub.identity.OpenID + `","union_id":"` +
				stub.identity.UnionID + `","name":"` + stub.identity.Name + `"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

// feishuFixture is the admin fixture with the identity integration wired the way the
// composition root wires it.
type feishuFixture struct {
	*adminFixture
	stub    *feishuStub
	states  *feishu.StateCodec
	tickets *feishu.TicketCodec
	now     time.Time
}

func newFeishuFixture(t *testing.T) *feishuFixture {
	t.Helper()
	return newFeishuFixtureWithConsole(t, "")
}

// newFeishuFixtureWithConsole is the same fixture with the console addressed explicitly. An
// empty console URL means "the console is where the callback is", which is the default and
// the shape every deployment had before feishu.console_url existed; a stated one on another
// host is the case the handoff ticket exists for (M66).
func newFeishuFixtureWithConsole(t *testing.T, consoleURL string) *feishuFixture {
	t.Helper()
	stub := newFeishuStub(t)
	fixture := &feishuFixture{stub: stub, now: time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)}

	states, err := feishu.NewStateCodec([]byte("state-key"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	states.Now = func() time.Time { return fixture.now }
	fixture.states = states
	tickets, err := feishu.NewTicketCodec([]byte("ticket-key"), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tickets.Now = func() time.Time { return fixture.now }
	// The invitation codec is a second codec on purpose (a link outlives one consent screen),
	// so the fixture builds one exactly as the composition root does (M66).
	invites, err := feishu.NewStateCodec([]byte("invite-key"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	invites.Now = func() time.Time { return fixture.now }
	consoleTickets, err := feishu.NewTicketCodec([]byte("ticket-key"), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	consoleTickets.Now = func() time.Time { return fixture.now }
	fixture.tickets = tickets

	// The identity port is installed before the server is built: the transport decides
	// which surfaces exist while it is constructed, so a port wired afterwards would not
	// register its routes at all.
	fixture.adminFixture = newAdminFixtureWith(t, "", func(deps *Deps) {
		deps.Config.Feishu.Enabled = true
		deps.Config.Feishu.DSHLogin = true
		deps.Config.Feishu.AppID = "cli_test"
		deps.Config.Feishu.AppSecret = "secret"
		deps.Config.Feishu.CallbackURL = "http://dsh.example:8090" + feishuCallbackPath
		deps.Config.Feishu.AuthorizeURL = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
		deps.Config.Feishu.TokenURL = stub.server.URL + "/token"
		deps.Config.Feishu.UserInfoURL = stub.server.URL + "/userinfo"
		deps.Config.Feishu.PortalURL = feishuPortalURL
		deps.Config.Feishu.AdminLogin = true
		deps.Feishu = &FeishuDeps{
			Client: &feishu.Client{
				AppID: "cli_test", AppSecret: "secret",
				AuthorizeURL: deps.Config.Feishu.AuthorizeURL,
				TokenURL:     deps.Config.Feishu.TokenURL,
				UserInfoURL:  deps.Config.Feishu.UserInfoURL,
				Timeout:      5 * time.Second,
			},
			States:         states,
			Tickets:        tickets,
			Invites:        invites,
			RedirectURI:    deps.Config.Feishu.CallbackURL,
			LoginPath:      feishuLoginPath,
			CallbackPath:   feishuCallbackPath,
			InvitePath:     feishuInvitePath,
			DSHLogin:       true,
			PortalURL:      feishuPortalURL,
			LoginURL:       "http://dsh.example:8090" + feishuLoginPath,
			AdminLogin:     true,
			ConsoleURL:     consoleURL,
			ConsoleTickets: consoleTickets,
		}
	})
	fixture.stub = stub
	return fixture
}

// seedKey creates an active key on the fixture's account and returns it.
func (f *feishuFixture) seedKey(t *testing.T) *domain.APIKey {
	t.Helper()
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
		AccountID: account.ID, Name: "alice-key", KeyPrefix: "sk-gw-feishu1", KeyHash: "hash-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// stateFrom extracts the state parameter from a redirect the server produced.
func stateFrom(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("location %q is not a URL: %v", location, err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatalf("location %q carries no state", location)
	}
	return state
}

// request performs one request without following redirects, which is what a browser does
// when the answer is a 302 to Feishu. session is the value of the admin cookie, empty for
// an anonymous request.
func (f *feishuFixture) request(t *testing.T, method, path, session string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: session})
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// The binding flow end to end: an administrator asks for it, Feishu confirms the identity,
// and the callback writes it — then the console list shows it.
func TestFeishuBindFlowWritesTheBindingAndTheListShowsIt(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	key := f.seedKey(t)

	// The console's own entry point goes to the consent page in one hop. It has to: the
	// administrator's cookie is scoped to /admin, so a second hop through a public route
	// would arrive without a session (which is exactly what a browser reported as
	// "missing admin session" before this was fixed).
	res := f.request(t, http.MethodGet, "/admin/api/v1/keys/1/feishu/bind", cookie)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("bind start: status=%d", res.StatusCode)
	}
	authorize := res.Header.Get("Location")
	if !strings.HasPrefix(authorize, "https://accounts.feishu.cn/open-apis/authen/v1/authorize?") {
		t.Fatalf("bind start must go straight to the consent page, got %q", authorize)
	}
	parsed, err := url.Parse(authorize)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("client_id") != "cli_test" || query.Get("response_type") != "code" {
		t.Fatalf("authorize URL lacks the required parameters: %s", authorize)
	}
	if query.Get("redirect_uri") != "http://dsh.example:8090"+feishuCallbackPath {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("scope") != "" {
		t.Errorf("the authorize URL asks for scopes that were never configured: %s", authorize)
	}

	// The browser comes back with a code and the state it was given.
	res = f.request(t, http.MethodGet, feishuCallbackPath+"?code=the-code&state="+
		url.QueryEscape(query.Get("state")), "")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: status=%d", res.StatusCode)
	}
	if got := res.Header.Get("Location"); !strings.Contains(got, "/admin/ui/#/keys?feishu=bound") {
		t.Fatalf("callback location = %q, want the console keys page with a result", got)
	}

	bound, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.FeishuOpenID != "ou_alice" || bound.FeishuName != "张三" || bound.FeishuBoundBy != adminUser {
		t.Fatalf("binding not written: %+v", bound)
	}

	// The list carries it in a stable shape.
	res = f.request(t, http.MethodGet, "/admin/api/v1/keys", cookie)
	var payload struct {
		Data []struct {
			Feishu map[string]any `json:"feishu"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) == 0 || payload.Data[0].Feishu["bound"] != true {
		t.Fatalf("list does not carry the binding: %+v", payload.Data)
	}
	if payload.Data[0].Feishu["open_id"] != "ou_alice" || payload.Data[0].Feishu["name"] != "张三" {
		t.Fatalf("list binding shape: %+v", payload.Data[0].Feishu)
	}
}

// Unbinding is idempotent and reported honestly.
func TestFeishuUnbindIsIdempotent(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	key := f.seedKey(t)
	if err := f.db.BindAPIKeyFeishu(context.Background(), key.ID, domain.FeishuBinding{OpenID: "ou_alice", Name: "张三"}); err != nil {
		t.Fatal(err)
	}

	res := f.request(t, http.MethodDelete, "/admin/api/v1/keys/1/feishu", cookie)
	var first map[string]any
	if err := json.NewDecoder(res.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || first["unbound"] != true {
		t.Fatalf("first unbind: status=%d payload=%v", res.StatusCode, first)
	}
	res = f.request(t, http.MethodDelete, "/admin/api/v1/keys/1/feishu", cookie)
	var second map[string]any
	if err := json.NewDecoder(res.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || second["unbound"] != false {
		t.Fatalf("second unbind: status=%d payload=%v", res.StatusCode, second)
	}
	after, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "" {
		t.Fatalf("unbind left the identity behind: %+v", after)
	}
}

// Binding and unbinding are administrator actions: a viewer may look, never change.
func TestFeishuBindingRequiresAnAdministrator(t *testing.T) {
	f := newFeishuFixture(t)
	viewer := f.login(t, "reader", adminPassword)
	key := f.seedKey(t)

	if res := f.request(t, http.MethodGet, "/admin/api/v1/keys/1/feishu/bind", viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer bind start: status=%d, want 403", res.StatusCode)
	}
	if res := f.request(t, http.MethodDelete, "/admin/api/v1/keys/1/feishu", viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer unbind: status=%d, want 403", res.StatusCode)
	}
	// And without a session at all.
	if res := f.request(t, http.MethodGet, "/admin/api/v1/keys/1/feishu/bind", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous bind start: status=%d, want 401", res.StatusCode)
	}
	// The public start route serves the portal flow only: a binding has no public entry
	// point, so asking for one there is simply not a route.
	if res := f.request(t, http.MethodGet, feishuLoginPath+"?mode=bind&key=1", ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("mode=bind on the public route: status=%d, want 404", res.StatusCode)
	}
	after, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "" {
		t.Fatalf("a refused request still bound something: %+v", after)
	}
}

// A binding may only target a key that can actually be used.
func TestFeishuBindRefusesInactiveAndUnknownKeys(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	key := f.seedKey(t)

	key.Status = "disabled"
	if _, err := f.db.UpsertAPIKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if res := f.request(t, http.MethodGet, "/admin/api/v1/keys/1/feishu/bind", cookie); res.StatusCode != http.StatusConflict {
		t.Fatalf("disabled key bind: status=%d, want 409", res.StatusCode)
	}
	if res := f.request(t, http.MethodGet, "/admin/api/v1/keys/424242/feishu/bind", cookie); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown key bind: status=%d, want 404", res.StatusCode)
	}
}

// The state is the whole capability of the callback, so every way of getting it wrong has
// to be refused — and none of them may write a binding.
func TestFeishuCallbackRefusesBadStates(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	valid, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = key
	expired, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-2"})
	if err != nil {
		t.Fatal(err)
	}

	// A state that cannot be trusted leaves the flow unknown, so the server explains
	// itself instead of guessing a destination.
	for name, path := range map[string]string{
		"missing state":  feishuCallbackPath + "?code=c",
		"garbage state":  feishuCallbackPath + "?code=c&state=nonsense",
		"tampered state": feishuCallbackPath + "?code=c&state=" + url.QueryEscape(valid[:len(valid)-2]) + "xy",
	} {
		res := f.request(t, http.MethodGet, path, "")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", name, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), "重新") {
			t.Errorf("%s: the page must tell the person to start again: %s", name, body)
		}
	}
	// Expiry is judged against the codec's clock, and is also unattributable: the state is
	// refused before its flow is read.
	f.now = f.now.Add(11 * time.Minute)
	if res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(expired), ""); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an expired state was answered with %d, want 400", res.StatusCode)
	}
	// A refusal and a missing code with a valid state do return to the console, because the
	// flow is known. Each case needs its own state: a state is single use.
	for name, test := range map[string]struct{ query string }{
		"cancelled":    {query: "?error=access_denied&state="},
		"missing code": {query: "?state="},
	} {
		fresh, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-" + strings.ReplaceAll(name, " ", "-")})
		if err != nil {
			t.Fatal(err)
		}
		res := f.request(t, http.MethodGet, feishuCallbackPath+test.query+url.QueryEscape(fresh), "")
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: status=%d, want a redirect", name, res.StatusCode)
		}
		if got := res.Header.Get("Location"); !strings.Contains(got, "/admin/ui/#/keys?feishu=") {
			t.Errorf("%s: location = %q, want the console with a result code", name, got)
		}
	}
	// A state that was already redeemed cannot be replayed.
	f.now = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	replay, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-3"})
	if err != nil {
		t.Fatal(err)
	}
	first := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(replay), "")
	if got := first.Header.Get("Location"); !strings.Contains(got, "feishu=bound") {
		t.Fatalf("first redemption location = %q", got)
	}
	if _, err := f.db.UnbindAPIKeyFeishu(context.Background(), key.ID); err != nil {
		t.Fatal(err)
	}
	// A replayed state is refused before its flow is known, so it gets the explanation page
	// rather than a redirect — and it must not re-bind anything.
	second := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(replay), "")
	if second.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed state status = %d, want 400", second.StatusCode)
	}
	after, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "" {
		t.Fatalf("a refused callback still bound something: %+v", after)
	}
}

// A demoted administrator cannot finish a binding they started: the role is re-checked at
// the callback, not only when the state was minted.
func TestFeishuCallbackRechecksTheAdministrator(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-demote"})
	if err != nil {
		t.Fatal(err)
	}
	// The operator is demoted between starting and finishing the flow.
	user, err := f.db.GetAdminUserByUsername(context.Background(), adminUser)
	if err != nil {
		t.Fatal(err)
	}
	user.Role = "viewer"
	if _, err := f.db.UpsertAdminUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}

	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	if got := res.Header.Get("Location"); !strings.Contains(got, "feishu=rejected") {
		t.Fatalf("demoted actor location = %q", got)
	}
	after, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "" {
		t.Fatalf("a demoted administrator completed a binding: %+v", after)
	}
}

// One Feishu identity belongs to one key: a second binding is refused and the first is left
// exactly as it was.
func TestFeishuBindConflictDoesNotOverwrite(t *testing.T) {
	f := newFeishuFixture(t)
	first := f.seedKey(t)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
		AccountID: account.ID, Name: "bob-key", KeyPrefix: "sk-gw-feishu2", KeyHash: "hash-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.BindAPIKeyFeishu(context.Background(), first.ID, domain.FeishuBinding{OpenID: "ou_alice", Name: "张三"}); err != nil {
		t.Fatal(err)
	}

	// The stub answers the same open id for the second attempt.
	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: secondID, Actor: adminUser, Nonce: "nonce-conflict"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	if got := res.Header.Get("Location"); !strings.Contains(got, "feishu=conflict") {
		t.Fatalf("conflict location = %q", got)
	}
	kept, err := f.db.GetAPIKeyByID(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.FeishuOpenID != "ou_alice" {
		t.Fatalf("the original binding was disturbed: %+v", kept)
	}
	other, err := f.db.GetAPIKeyByID(context.Background(), secondID)
	if err != nil {
		t.Fatal(err)
	}
	if other.FeishuOpenID != "" {
		t.Fatalf("the rejected key kept a partial binding: %+v", other)
	}
}

// Rebinding the same key to a new identity replaces the old pair and says so.
func TestFeishuRebindReplaces(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	if err := f.db.BindAPIKeyFeishu(context.Background(), key.ID, domain.FeishuBinding{OpenID: "ou_old", Name: "旧"}); err != nil {
		t.Fatal(err)
	}
	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-rebind"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	if got := res.Header.Get("Location"); !strings.Contains(got, "feishu=replaced") {
		t.Fatalf("rebind location = %q", got)
	}
	after, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "ou_alice" {
		t.Fatalf("rebind did not replace the identity: %+v", after)
	}
	if stale, err := f.db.FindAPIKeyByFeishuOpenID(context.Background(), "ou_old"); err != nil || stale != nil {
		t.Fatalf("the replaced identity still resolves: %v %+v", err, stale)
	}
}

// A deployment that never configured Feishu must not grow its surface: the routes are not
// even registered.
func TestFeishuRoutesAreAbsentWhenDisabled(t *testing.T) {
	f := newAdminFixture(t)
	for _, path := range []string{feishuLoginPath, feishuCallbackPath, feishuLoginPath + "?mode=bind&key=1"} {
		res := f.call(t, http.MethodGet, path, "", "")
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status=%d, want 404", path, res.StatusCode)
		}
		res.Body.Close()
	}
}

// The rate limiter guards the only unauthenticated route that makes an outbound call.
func TestFeishuLoginIsRateLimited(t *testing.T) {
	f := newFeishuFixture(t)
	started := 0
	for i := 0; i < feishuAttemptLimit+3; i++ {
		res := f.request(t, http.MethodGet, feishuLoginPath, "")
		if res.StatusCode == http.StatusFound {
			started++
			continue
		}
		if res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=%d", i, res.StatusCode)
		}
	}
	if started != feishuAttemptLimit {
		t.Fatalf("attempts allowed = %d, want %d", started, feishuAttemptLimit)
	}
}

// The ticket and its cookie are how the portal learns the tenant. This covers the handoff
// itself; the portal's half is tested with the DSH gateway (M61).
func TestFeishuDSHLoginIssuesATicketForTheTenant(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	account.DSHEnabled = true
	account.DshTenant = "alice"
	if _, err := f.db.UpsertAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if err := f.db.BindAPIKeyFeishu(context.Background(), key.ID, domain.FeishuBinding{OpenID: "ou_alice", Name: "张三"}); err != nil {
		t.Fatal(err)
	}

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowDSHLogin, Nonce: "nonce-login"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login callback: status=%d", res.StatusCode)
	}
	want := feishuPortalURL + "/login/feishu"
	if got := res.Header.Get("Location"); got != want {
		t.Fatalf("login location = %q, want %q", got, want)
	}
	// The portal is on the same host, so the ticket travels as a host-only cookie and does
	// not appear in the URL.
	var ticket *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == feishuTicketCookieName {
			ticket = cookie
		}
	}
	if ticket == nil {
		t.Fatal("no ticket cookie was issued")
	}
	if !ticket.HttpOnly || ticket.Path != "/" || ticket.SameSite != http.SameSiteLaxMode {
		t.Fatalf("ticket cookie attributes are wrong: %+v", ticket)
	}
	if ticket.Secure {
		t.Error("a plain-HTTP deployment must not mark the ticket cookie Secure")
	}
	if strings.Contains(res.Header.Get("Location"), "ticket=") {
		t.Error("the same-host handoff must not put the ticket in the URL")
	}
	decoded, err := f.api.deps.Feishu.Tickets.VerifyTicket(ticket.Value)
	if err != nil {
		t.Fatalf("the issued ticket does not verify: %v", err)
	}
	if decoded.Tenant != "alice" || decoded.OpenID != "ou_alice" || decoded.AccountID != account.ID {
		t.Fatalf("ticket contents: %+v", decoded)
	}
}

// Every reason a login can be refused ends at the portal's own error page, and none of them
// issues a ticket.
func TestFeishuDSHLoginRefusals(t *testing.T) {
	cases := map[string]struct {
		setup  func(t *testing.T, f *feishuFixture, key *domain.APIKey)
		reason string
	}{
		"unbound identity": {
			setup:  func(t *testing.T, f *feishuFixture, key *domain.APIKey) {},
			reason: "unbound",
		},
		"dsh disabled": {
			setup: func(t *testing.T, f *feishuFixture, key *domain.APIKey) {
				f.bindAndEnable(t, key, false, "alice")
			},
			reason: "dsh_disabled",
		},
		"tenant unassigned": {
			setup: func(t *testing.T, f *feishuFixture, key *domain.APIKey) {
				f.bindAndEnable(t, key, true, "")
			},
			reason: "tenant_missing",
		},
		"suspended account": {
			setup: func(t *testing.T, f *feishuFixture, key *domain.APIKey) {
				f.bindAndEnable(t, key, true, "alice")
				account, err := f.db.GetAccountByName(context.Background(), "acme")
				if err != nil {
					t.Fatal(err)
				}
				account.Status = "suspended"
				if _, err := f.db.UpsertAccount(context.Background(), account); err != nil {
					t.Fatal(err)
				}
			},
			reason: "account_status",
		},
	}
	for name, test := range cases {
		f := newFeishuFixture(t)
		key := f.seedKey(t)
		test.setup(t, f, key)
		state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowDSHLogin, Nonce: "nonce-" + strings.ReplaceAll(name, " ", "-")})
		if err != nil {
			t.Fatal(err)
		}
		res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
		location := res.Header.Get("Location")
		if !strings.Contains(location, "feishu/error?reason="+test.reason) {
			t.Errorf("%s: location = %q, want the portal error page with reason %s", name, location, test.reason)
		}
		for _, cookie := range res.Cookies() {
			if cookie.Name == feishuTicketCookieName {
				t.Errorf("%s: a refused login still issued a ticket", name)
			}
		}
	}
}

func (f *feishuFixture) bindAndEnable(t *testing.T, key *domain.APIKey, enabled bool, tenant string) {
	t.Helper()
	ctx := context.Background()
	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	account.DSHEnabled = enabled
	account.DshTenant = tenant
	if _, err := f.db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := f.db.BindAPIKeyFeishu(ctx, key.ID, domain.FeishuBinding{OpenID: "ou_alice", Name: "张三"}); err != nil {
		t.Fatal(err)
	}
}

// A Feishu refusal must not become a binding, and must not be reported as a success.
func TestFeishuExchangeFailureIsReported(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	f.stub.tokenBody = `{"code":20004,"error":"expired","error_description":"code expired"}`

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-fail"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	// A rejected authorization code is not a deployment fault: the person is told to start
	// again, not that the gateway is broken.
	if got := res.Header.Get("Location"); !strings.Contains(got, "feishu=expired") {
		t.Fatalf("failed exchange location = %q, want the retry result", got)
	}
	after, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "" {
		t.Fatalf("a failed exchange still bound something: %+v", after)
	}
}

// The callback must never echo a secret: the page and the redirect carry a code, not values.
func TestFeishuCallbackNeverEchoesSecrets(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-secret"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=the-code&state="+url.QueryEscape(state), "")
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "u-token", "the-code"} {
		if strings.Contains(string(body), secret) || strings.Contains(res.Header.Get("Location"), secret) {
			t.Errorf("the callback leaked %q", secret)
		}
	}
	// The audit trail records the identity and the actor, and no credential.
	entries, err := f.db.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.ChangesJSON, "u-token") || strings.Contains(entry.ChangesJSON, "the-code") ||
			strings.Contains(entry.ChangesJSON, "\"secret\"") {
			t.Errorf("audit entry %s leaks a credential: %s", entry.Action, entry.ChangesJSON)
		}
		if entry.Action == "feishu_bind" {
			found = true
			if !strings.Contains(entry.ChangesJSON, "ou_alice") {
				t.Errorf("the binding audit entry should name the identity: %s", entry.ChangesJSON)
			}
		}
	}
	if !found {
		t.Fatal("no binding audit entry was written")
	}
}

// When the portal is on another hostname the ticket cannot travel as a cookie, so it goes in
// the URL — and the deployment is warned, because that value lands in browser history.
func TestFeishuDSHLoginFallsBackToAQueryTicketAcrossHosts(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	f.bindAndEnable(t, key, true, "alice")
	f.api.deps.Feishu.PortalURL = "http://portal.example:18300"

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowDSHLogin, Nonce: "nonce-crosshost"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	location := res.Header.Get("Location")
	if !strings.HasPrefix(location, "http://portal.example:18300/login/feishu?ticket=") {
		t.Fatalf("cross-host location = %q, want the ticket in the query", location)
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == feishuTicketCookieName {
			t.Error("a cross-host handoff must not rely on a cookie the portal will never receive")
		}
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.api.deps.Feishu.Tickets.VerifyTicket(parsed.Query().Get("ticket"))
	if err != nil {
		t.Fatalf("the query ticket does not verify: %v", err)
	}
	if ticket.Tenant != "alice" {
		t.Fatalf("ticket tenant = %q", ticket.Tenant)
	}
}

// An HTTPS deployment marks the ticket cookie Secure; a plain-HTTP one must not, because a
// browser silently drops a Secure cookie there and the login would look successful.
func TestFeishuTicketCookieFollowsTheScheme(t *testing.T) {
	f := newFeishuFixture(t)
	key := f.seedKey(t)
	f.bindAndEnable(t, key, true, "alice")
	f.api.deps.Feishu.RedirectURI = "https://dsh.example/feishu/callback"

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowDSHLogin, Nonce: "nonce-secure"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	var ticket *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == feishuTicketCookieName {
			ticket = cookie
		}
	}
	if ticket == nil {
		t.Fatal("no ticket cookie was issued")
	}
	if !ticket.Secure {
		t.Error("an https deployment must mark the ticket cookie Secure")
	}
}

// Binding a key now also opts the account in to DSH, so the person can sign in immediately
// instead of waiting for a second, easily forgotten step. The identity is written either way;
// these cases pin what happens to the account around it.
func TestFeishuBindAutoEnablesDSH(t *testing.T) {
	f := newFeishuFixture(t)
	f.api.deps.Feishu.AutoEnableDSH = true
	key := f.seedKey(t)

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-auto-enable"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	location := res.Header.Get("Location")
	if !strings.Contains(location, "feishu=bound") || !strings.Contains(location, "dsh=enabled") {
		t.Fatalf("location = %q, want a bound binding that enabled DSH", location)
	}
	// The tenant was actually provisioned through the same channel the console's button uses.
	if len(f.dshgwAdmin.Created) != 1 {
		t.Fatalf("provisioned tenants = %v, want exactly one", f.dshgwAdmin.Created)
	}
	tenant := f.dshgwAdmin.Created[0]
	if !strings.Contains(location, "tenant="+tenant) {
		t.Fatalf("location = %q must name the tenant %q", location, tenant)
	}
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !account.DSHEnabled || account.DshTenant != tenant {
		t.Fatalf("account not opted in: enabled=%v tenant=%q", account.DSHEnabled, account.DshTenant)
	}
	// The worker credential is minted for that tenant, exactly as the console does it.
	if len(f.dshgwAdmin.Keys) != 1 || !strings.HasPrefix(f.dshgwAdmin.Keys[0], "sk") {
		t.Fatalf("worker key = %v", f.dshgwAdmin.Keys)
	}
	// And it is audited as an enable, so the account history shows where it came from.
	entries, err := f.db.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	for _, entry := range entries {
		if entry.Action == "dsh_enable" && strings.Contains(entry.ChangesJSON, tenant) {
			enabled = true
		}
	}
	if !enabled {
		t.Fatal("the automatic enable was not audited")
	}

	// A second key of the same account finds it already enabled and provisions nothing new.
	// It needs a different Feishu identity: one identity binds exactly one key.
	second := f.seedSecondKey(t)
	f.stub.identity = feishu.Identity{OpenID: "ou_bob", UnionID: "on_bob", Name: "李四"}
	state, err = f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: second, Actor: adminUser, Nonce: "nonce-already"})
	if err != nil {
		t.Fatal(err)
	}
	res = f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	if got := res.Header.Get("Location"); !strings.Contains(got, "dsh=already") {
		t.Fatalf("second bind location = %q, want dsh=already", got)
	}
	if len(f.dshgwAdmin.Created) != 1 {
		t.Fatalf("a second tenant was provisioned: %v", f.dshgwAdmin.Created)
	}
}

// An account an administrator explicitly disabled stays disabled: silently undoing that on an
// unrelated binding (someone binding another key) is the kind of surprise that reads as a bug
// later, so it is reported as declined instead.
func TestFeishuBindDoesNotOverrideAnExplicitDisable(t *testing.T) {
	f := newFeishuFixture(t)
	f.api.deps.Feishu.AutoEnableDSH = true
	key := f.seedKey(t)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	account.DSHEnabled = false
	account.DshTenant = "dsh-previously"
	if _, err := f.db.UpsertAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-declined"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	location := res.Header.Get("Location")
	if !strings.Contains(location, "dsh=declined") {
		t.Fatalf("location = %q, want dsh=declined", location)
	}
	if len(f.dshgwAdmin.Created) != 0 || len(f.dshgwAdmin.Started) != 0 {
		t.Fatalf("a disabled account was provisioned anyway: created=%v started=%v", f.dshgwAdmin.Created, f.dshgwAdmin.Started)
	}
	after, err := f.db.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DSHEnabled || after.DshTenant != "dsh-previously" {
		t.Fatalf("the account was modified: enabled=%v tenant=%q", after.DSHEnabled, after.DshTenant)
	}
}

// Provisioning can fail (the local channel is down, no free port, the template is missing).
// The binding is about identity and stays; the failure is reported so the administrator can
// retry from the account page with the full error in front of them.
func TestFeishuBindSurvivesAFailedAutoEnable(t *testing.T) {
	f := newFeishuFixture(t)
	f.api.deps.Feishu.AutoEnableDSH = true
	f.dshgwAdmin.FailWith = errors.New("admin channel unavailable")
	key := f.seedKey(t)

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-failed-enable"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	location := res.Header.Get("Location")
	if !strings.Contains(location, "feishu=bound") || !strings.Contains(location, "dsh=failed") {
		t.Fatalf("location = %q, want a bound binding with a failed enable", location)
	}
	bound, err := f.db.GetAPIKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.FeishuOpenID != "ou_alice" {
		t.Fatalf("the binding was rolled back on a provisioning failure: %+v", bound)
	}
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if account.DSHEnabled {
		t.Fatal("a failed provisioning still marked the account enabled")
	}
}

// The switch turns the whole automatic step off, for deployments that want binding to be
// purely about identity.
func TestFeishuBindAutoEnableCanBeDisabled(t *testing.T) {
	f := newFeishuFixture(t)
	f.api.deps.Feishu.AutoEnableDSH = false
	key := f.seedKey(t)

	state, err := f.states.Sign(feishu.Attempt{Flow: feishu.FlowBind, KeyID: key.ID, Actor: adminUser, Nonce: "nonce-off"})
	if err != nil {
		t.Fatal(err)
	}
	res := f.request(t, http.MethodGet, feishuCallbackPath+"?code=c&state="+url.QueryEscape(state), "")
	if got := res.Header.Get("Location"); !strings.Contains(got, "dsh=off") {
		t.Fatalf("location = %q, want dsh=off", got)
	}
	if len(f.dshgwAdmin.Created) != 0 {
		t.Fatalf("provisioning ran while the switch was off: %v", f.dshgwAdmin.Created)
	}
}

// seedSecondKey adds another key to the fixture's account.
func (f *feishuFixture) seedSecondKey(t *testing.T) int64 {
	t.Helper()
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
		AccountID: account.ID, Name: "bob-key", KeyPrefix: "sk-gw-feishu2", KeyHash: "hash-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

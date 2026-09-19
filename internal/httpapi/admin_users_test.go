package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/feishu"
)

// The console's administrators themselves (M66): who exists, what they may do, how they get
// in, and the three things the API refuses to do no matter who asks.

// adminListItems calls the list endpoint and returns the rows.
func adminListItems(t *testing.T, f *feishuFixture, cookie string) []map[string]any {
	t.Helper()
	res := f.request(t, http.MethodGet, "/admin/api/v1/admin-users", cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list admin users: status=%d", res.StatusCode)
	}
	// The list envelope is the shared one: data/count/total/limit/offset/has_more.
	var payload struct {
		Items []map[string]any `json:"data"`
	}
	if err := decodeInto(res, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Items
}

// The login page asks which ways in exist before it has a session, so this endpoint is
// public and answers for both.
func TestAdminAuthMethodsIsPublicAndDescribesFeishuLogin(t *testing.T) {
	f := newFeishuFixture(t)
	res := f.request(t, http.MethodGet, "/admin/api/v1/auth/methods", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 without a session", res.StatusCode)
	}
	var payload struct {
		Password bool `json:"password"`
		Feishu   struct {
			Enabled  bool   `json:"enabled"`
			LoginURL string `json:"login_url"`
		} `json:"feishu"`
	}
	if err := decodeInto(res, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Password || !payload.Feishu.Enabled {
		t.Fatalf("methods = %+v, want password and feishu enabled", payload)
	}
	if payload.Feishu.LoginURL != feishuLoginPath+"?mode=admin" {
		t.Fatalf("login_url = %q", payload.Feishu.LoginURL)
	}

	// With the integration off the console must not offer a button that leads to a 404.
	plain := newAdminFixture(t)
	res = plain.call(t, http.MethodGet, "/admin/api/v1/auth/methods", "", "")
	var off struct {
		Password bool `json:"password"`
		Feishu   struct {
			Enabled bool `json:"enabled"`
		} `json:"feishu"`
	}
	if err := decodeInto(res, &off); err != nil {
		t.Fatal(err)
	}
	if !off.Password || off.Feishu.Enabled {
		t.Fatalf("a deployment without Feishu advertised it: %+v", off)
	}
}

// Creating an administrator without a password is the invitation path: the account exists,
// cannot sign in yet, and says so.
func TestAdminUserCreateWithAndWithoutPassword(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users",
		`{"username":"invitee","role":"viewer"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create without a password: status=%d", res.StatusCode)
	}
	created := decodeJSONBody(t, res)
	if created["status"] != domain.AdminPending || created["has_password"] != false {
		t.Fatalf("an invitation-only account is %+v, want pending without a password", created)
	}
	if _, leaked := created["invite_nonce"]; leaked {
		t.Fatal("the list response carries the invitation handle")
	}

	res = f.call(t, http.MethodPost, "/admin/api/v1/admin-users",
		`{"username":"boss2","role":"admin","password":"a-good-password"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create with a password: status=%d", res.StatusCode)
	}
	if got := decodeJSONBody(t, res)["status"]; got != domain.AdminActive {
		t.Fatalf("an account with a password is %q, want active", got)
	}

	// The new account really works: signing in with it is the whole point.
	if other := f.login(t, "boss2", "a-good-password"); other == "" {
		t.Fatal("the created administrator could not sign in")
	}

	// Refusals: a bad role, a short password, a duplicate username.
	for name, body := range map[string]string{
		"unknown role":   `{"username":"x1","role":"root"}`,
		"short password": `{"username":"x2","role":"viewer","password":"abc"}`,
		"duplicate":      `{"username":"boss2","role":"viewer"}`,
		"bad username":   `{"username":"has space","role":"viewer"}`,
	} {
		res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", body, cookie)
		if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusConflict {
			t.Errorf("%s: status=%d, want 400 or 409", name, res.StatusCode)
		}
		res.Body.Close()
	}
}

// The list never leaks a credential, and it does tell an operator which accounts are usable
// and which are not.
func TestAdminUserListShape(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 1, domain.FeishuBinding{OpenID: "ou_admin", Name: "管理员"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RotateAdminUserInvite(ctx, 2, "handle"); err != nil {
		t.Fatal(err)
	}
	items := adminListItems(t, f, cookie)
	if len(items) != 2 {
		t.Fatalf("list returned %d administrators, want 2", len(items))
	}
	first := items[0]
	if first["username"] != adminUser || first["bootstrap"] != false {
		t.Fatalf("first row = %+v", first)
	}
	binding, ok := first["feishu"].(map[string]any)
	if !ok || binding["bound"] != true || binding["open_id"] != "ou_admin" {
		t.Fatalf("bound row's feishu object = %+v", first["feishu"])
	}
	for _, forbidden := range []string{"password_hash", "invite_nonce"} {
		if _, present := first[forbidden]; present {
			t.Fatalf("the row carries %s", forbidden)
		}
	}
	second := items[1]
	if second["invite_pending"] != true {
		t.Fatalf("an outstanding invitation is not reported: %+v", second)
	}
}

// The guards are the point of the whole surface: a mistake here locks everybody out.
func TestAdminUserGuardsRefuseToLockTheConsole(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// The fixture's second administrator is a viewer, so the signed-in one is the only
	// usable administrator.
	for name, call := range map[string]func() *http.Response{
		"disable the last admin": func() *http.Response {
			return f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/1", `{"status":"disabled"}`, cookie)
		},
		"demote the last admin": func() *http.Response {
			return f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/1", `{"role":"viewer"}`, cookie)
		},
		"delete the last admin": func() *http.Response {
			return f.call(t, http.MethodDelete, "/admin/api/v1/admin-users/1", "", cookie)
		},
	} {
		res := call()
		body := decodeJSONBody(t, res)
		if res.StatusCode != http.StatusConflict {
			t.Errorf("%s: status=%d (%v), want 409", name, res.StatusCode, body)
		}
	}
	user, err := f.db.GetAdminUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != domain.AdminActive || user.Role != domain.RoleAdmin {
		t.Fatalf("a refused change was written anyway: %+v", user)
	}
	// ...and the session the admin is using is still good, which is what "did not lock
	// anybody out" means in practice.
	if res := f.call(t, http.MethodGet, "/admin/api/v1/auth/me", "", cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("the administrator lost their own session: status=%d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	// Deleting yourself is refused even when somebody else could undo it.
	promote := f.call(t, http.MethodPost, "/admin/api/v1/admin-users",
		`{"username":"second","role":"admin","password":"another-password"}`, cookie)
	if promote.StatusCode != http.StatusCreated {
		t.Fatalf("create a second admin: status=%d", promote.StatusCode)
	}
	promote.Body.Close()
	res := f.call(t, http.MethodDelete, "/admin/api/v1/admin-users/1", "", cookie)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("self-delete: status=%d, want 409", res.StatusCode)
	}
	res.Body.Close()

	// With somebody else in charge, the first administrator may be demoted (the second
	// administrator is now the last one in charge).
	second := f.login(t, "second", "another-password")
	res = f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/1", `{"role":"viewer"}`, second)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("demote with a second admin available: status=%d", res.StatusCode)
	}
	res.Body.Close()

	// A viewer may read the list but not change anything.
	res = f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"nope","role":"viewer"}`, cookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a demoted administrator could still write: status=%d", res.StatusCode)
	}
	res.Body.Close()
	if items := adminListItems(t, f, cookie); len(items) == 0 {
		t.Fatal("a viewer cannot read the administrator list")
	}
}

// Disabling ends the account's sessions immediately, and the account cannot sign in again
// until it is enabled.
func TestAdminUserDisableEndsSessionsAndLogins(t *testing.T) {
	f := newFeishuFixture(t)
	owner := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	if _, err := f.db.CreateAdminUser(ctx, &domain.AdminUser{
		Username: "temp", Role: domain.RoleAdmin, Status: domain.AdminActive,
	}); err != nil {
		t.Fatal(err)
	}
	hash, err := admin.HashPassword("temp-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetAdminUserPassword(ctx, 3, hash); err != nil {
		t.Fatal(err)
	}
	victim := f.login(t, "temp", "temp-password")

	res := f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/3", `{"status":"disabled"}`, owner)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable: status=%d", res.StatusCode)
	}
	res.Body.Close()

	// The session that already existed is gone...
	if res := f.call(t, http.MethodGet, "/admin/api/v1/auth/me", "", victim); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disabled administrator's session answered %d, want 401", res.StatusCode)
	} else {
		res.Body.Close()
	}
	// ...and the password no longer buys one.
	body := `{"username":"temp","password":"temp-password"}`
	res = f.call(t, http.MethodPost, "/admin/api/v1/auth/login", body, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disabled administrator signed in: status=%d", res.StatusCode)
	}
	res.Body.Close()

	// Re-enabling works, because the account still has a password.
	res = f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/3", `{"status":"active"}`, owner)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-enable: status=%d", res.StatusCode)
	}
	res.Body.Close()
	if f.login(t, "temp", "temp-password") == "" {
		t.Fatal("a re-enabled administrator cannot sign in")
	}

	// An account with no credential at all cannot be switched on: it would look healthy and
	// refuse every sign-in.
	if res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"empty","role":"admin"}`, owner); res.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	res = f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/4", `{"status":"active"}`, owner)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("enabling an account with no credential: status=%d, want 409", res.StatusCode)
	}
	res.Body.Close()
}

// A password reset hands out a one-time password and ends the sessions that were opened
// with the old one.
func TestAdminUserPasswordReset(t *testing.T) {
	f := newFeishuFixture(t)
	owner := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	if _, err := f.db.CreateAdminUser(ctx, &domain.AdminUser{
		Username: "locked", Role: domain.RoleViewer, Status: domain.AdminPending,
	}); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users/3/password", "", owner)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reset: status=%d", res.StatusCode)
	}
	payload := decodeJSONBody(t, res)
	password, _ := payload["password"].(string)
	if password == "" || payload["sessions_revoked"] != true {
		t.Fatalf("reset payload = %+v", payload)
	}
	// The reset also activates an account that had no credential, which is what makes it a
	// way back in for an account whose Feishu binding was removed.
	if f.login(t, "locked", password) == "" {
		t.Fatal("the one-time password does not work")
	}
}

// The bootstrap row is recreated from the configuration at every start, so deleting it is
// refused with an explanation instead of succeeding and reappearing.
func TestAdminUserBootstrapRowCannotBeDeleted(t *testing.T) {
	f := newFeishuFixture(t)
	f.cfg.Bootstrap.Admin.Username = adminUser
	cookie := f.login(t, adminUser, adminPassword)

	items := adminListItems(t, f, cookie)
	if items[0]["bootstrap"] != true {
		t.Fatalf("the seeded row is not marked: %+v", items[0])
	}
	res := f.call(t, http.MethodDelete, "/admin/api/v1/admin-users/1", "", cookie)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("deleting the bootstrap row: status=%d, want 409", res.StatusCode)
	}
	res.Body.Close()
	// Disabling it stays allowed: the seeder writes the password and the role, not the state.
	res = f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/1", `{"status":"disabled"}`, cookie)
	if res.StatusCode != http.StatusConflict {
		// It is the last active administrator here, so this is refused by the other guard.
		t.Fatalf("disabling the only administrator: status=%d, want 409", res.StatusCode)
	}
	res.Body.Close()
}

// An administrator may hold a bound identity and a password at once; unbinding takes away
// only the identity.
func TestAdminUserUnbindIsIdempotent(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_reader", Name: "读者"}); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, http.MethodDelete, "/admin/api/v1/admin-users/2/feishu", "", cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unbind: status=%d", res.StatusCode)
	}
	if got := decodeJSONBody(t, res)["unbound"]; got != true {
		t.Fatalf("unbound = %v, want true", got)
	}
	res = f.call(t, http.MethodDelete, "/admin/api/v1/admin-users/2/feishu", "", cookie)
	if got := decodeJSONBody(t, res)["unbound"]; got != false {
		t.Fatalf("second unbind = %v, want false", got)
	}
	user, err := f.db.GetAdminUser(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if user.FeishuOpenID != "" || user.PasswordHash == "" {
		t.Fatalf("unbind changed more than the identity: %+v", user)
	}
}

// ---------------------------------------------------------------------------
// Feishu: signing in to the console
// ---------------------------------------------------------------------------

// adminLoginStart asks for the console login flow and returns the consent-page URL.
func adminLoginStart(t *testing.T, f *feishuFixture) string {
	t.Helper()
	res := f.request(t, http.MethodGet, feishuLoginPath+"?mode=admin", "")
	if res.StatusCode != http.StatusFound {
		t.Fatalf("console login start: status=%d, want a redirect", res.StatusCode)
	}
	location := res.Header.Get("Location")
	if !strings.HasPrefix(location, "https://accounts.feishu.cn/open-apis/authen/v1/authorize?") {
		t.Fatalf("console login must go to the consent page, got %q", location)
	}
	return location
}

// consentAndCallback plays the browser's part after the consent page: the state comes out
// of the URL Feishu would have shown, and the callback receives an authorization code. The
// consent page itself is Feishu's, which is why it is never fetched here.
func consentAndCallback(t *testing.T, f *feishuFixture, authorize string) *http.Response {
	t.Helper()
	state := stateFrom(t, authorize)
	return f.request(t, http.MethodGet,
		feishuCallbackPath+"?code=the-code&state="+url.QueryEscape(state), "")
}

// The whole console login: start, consent, callback, cookie, whoami.
func TestFeishuAdminLoginSignsInWithTheConsoleCookie(t *testing.T) {
	f := newFeishuFixture(t)
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_alice"}); err != nil {
		t.Fatal(err)
	}
	f.stub.identity = feishu.Identity{OpenID: "ou_alice", UnionID: "on_alice", Name: "张三"}

	authorize := adminLoginStart(t, f)
	if parsed, err := url.Parse(authorize); err != nil {
		t.Fatal(err)
	} else if parsed.Query().Get("redirect_uri") != "http://dsh.example:8090"+feishuCallbackPath {
		t.Fatalf("redirect_uri = %q", parsed.Query().Get("redirect_uri"))
	}
	res := consentAndCallback(t, f, authorize)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: status=%d, want 303 into the console", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/admin/ui/" {
		t.Fatalf("callback location = %q, want the console", got)
	}
	var session string
	for _, cookie := range res.Cookies() {
		if cookie.Name == adminCookieName {
			session = cookie.Value
			if !cookie.HttpOnly || cookie.Path != "/admin" || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("the Feishu login cookie is not the console cookie: %+v", cookie)
			}
		}
	}
	if session == "" {
		t.Fatal("the callback did not issue a console session")
	}
	me := f.call(t, http.MethodGet, "/admin/api/v1/auth/me", "", session)
	payload := decodeJSONBody(t, me)
	if payload["username"] != "reader" || payload["role"] != "viewer" {
		t.Fatalf("whoami = %+v, want the bound administrator", payload)
	}
	// The login is recorded as a Feishu login, so the audit trail says how somebody got in.
	entries, err := f.db.ListAudit(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Action == "login" && entry.Actor == "reader" && strings.Contains(entry.ChangesJSON, "feishu") {
			found = true
		}
	}
	if !found {
		t.Fatal("no audit entry records the Feishu login")
	}
}

// Refusals: an identity nobody bound, an account somebody disabled, and — the important one
// — an identity that belongs to a customer's API key.
func TestFeishuAdminLoginRefusals(t *testing.T) {
	f := newFeishuFixture(t)
	ctx := context.Background()
	key := f.seedKey(t)

	// A customer's Feishu identity is bound to an API key and to nothing else. It must not
	// reach the console, whatever it is bound to on the data plane.
	if err := f.db.BindAPIKeyFeishu(ctx, key.ID, domain.FeishuBinding{OpenID: "ou_customer"}); err != nil {
		t.Fatal(err)
	}
	f.stub.identity = feishu.Identity{OpenID: "ou_customer", Name: "客户"}
	res := consentAndCallback(t, f, adminLoginStart(t, f))
	body := readBody(t, res.Body)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a customer identity got status=%d, want 403", res.StatusCode)
	}
	if !strings.Contains(body, "还没有绑定任何管理员账号") {
		t.Fatalf("the refusal does not explain itself: %q", body)
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == adminCookieName {
			t.Fatal("a refused login issued a console session")
		}
	}

	// A bound but disabled account is refused too, and the page says which of the two it is.
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_reader"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetAdminUserStatus(ctx, 2, domain.AdminDisabled); err != nil {
		t.Fatal(err)
	}
	f.stub.identity = feishu.Identity{OpenID: "ou_reader", Name: "读者"}
	res = consentAndCallback(t, f, adminLoginStart(t, f))
	body = readBody(t, res.Body)
	if !strings.Contains(body, "已被停用") {
		t.Fatalf("a disabled account's refusal = %q", body)
	}
}

// ---------------------------------------------------------------------------
// Feishu: invitations
// ---------------------------------------------------------------------------

// inviteFor mints an invitation link for one account and returns it.
func inviteFor(t *testing.T, f *feishuFixture, cookie string, id string) string {
	t.Helper()
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users/"+id+"/invite", "", cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("invite %s: status=%d", id, res.StatusCode)
	}
	link, _ := decodeJSONBody(t, res)["url"].(string)
	if link == "" {
		t.Fatal("the invitation response carries no url")
	}
	return link
}

// The invitation path end to end: mint, open, consent, bind, activate, sign in — and the
// same link afterwards says so instead of binding somebody else.
func TestFeishuAdminInviteBindsAndSignsIn(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"invitee","role":"admin"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create invitee: status=%d", res.StatusCode)
	}
	id := int64(decodeJSONBody(t, res)["id"].(float64))
	link := inviteFor(t, f, cookie, "3")
	if !strings.HasPrefix(link, "http://dsh.example:8090"+feishuInvitePath+"?invite=") {
		t.Fatalf("invitation link = %q", link)
	}
	// The row now reports an outstanding invitation without exposing its handle.
	user, err := f.db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.InviteNonce == "" {
		t.Fatal("the invitation handle was not stored")
	}
	if items := adminListItems(t, f, cookie); items[2]["invite_pending"] != true {
		t.Fatalf("invite_pending = %v, want true", items[2]["invite_pending"])
	}

	// The invitee opens the link in a browser that has never seen this console.
	f.stub.identity = feishu.Identity{OpenID: "ou_new", UnionID: "on_new", Name: "新管理员"}
	token := strings.TrimPrefix(link, "http://dsh.example:8090"+feishuInvitePath+"?invite=")
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(token), "")
	if res.StatusCode != http.StatusFound {
		t.Fatalf("invitation entry: status=%d, want a redirect to the consent page", res.StatusCode)
	}
	res = consentAndCallback(t, f, res.Header.Get("Location"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/admin/ui/" {
		t.Fatalf("invitation callback: status=%d location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	var session string
	for _, c := range res.Cookies() {
		if c.Name == adminCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("redeeming an invitation did not sign the invitee in")
	}

	// The account is bound, active, and its invitation handle is gone.
	user, err = f.db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.FeishuOpenID != "ou_new" || user.Status != domain.AdminActive || user.InviteNonce != "" {
		t.Fatalf("after redemption the account is %+v", user)
	}
	me := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/auth/me", "", session))
	if me["username"] != "invitee" || me["role"] != "admin" {
		t.Fatalf("the invitee signed in as %+v", me)
	}

	// The same link is inert now: it must not rebind the account to whoever opens it next.
	f.stub.identity = feishu.Identity{OpenID: "ou_attacker", Name: "别人"}
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(token), "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a redeemed invitation was accepted: status=%d", res.StatusCode)
	}
	if body := readBody(t, res.Body); !strings.Contains(body, "已失效") {
		t.Fatalf("the refusal does not explain itself: %q", body)
	}
	user, err = f.db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.FeishuOpenID != "ou_new" {
		t.Fatalf("a redeemed invitation rebound the account: %+v", user)
	}
}

// Regenerating retires the previous link, and cancelling the consent screen does not spend
// the link at all — that is why the entry point peeks instead of consuming.
func TestFeishuAdminInviteRotationAndCancel(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"invitee","role":"viewer"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%d", res.StatusCode)
	}
	first := inviteFor(t, f, cookie, "3")
	second := inviteFor(t, f, cookie, "3")
	if first == second {
		t.Fatal("regenerating produced the same link")
	}
	firstToken := strings.TrimPrefix(first, "http://dsh.example:8090"+feishuInvitePath+"?invite=")
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(firstToken), "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a retired invitation was accepted: status=%d", res.StatusCode)
	}
	res.Body.Close()

	// Cancelling at the consent page: the state is spent, the invitation is not.
	secondToken := strings.TrimPrefix(second, "http://dsh.example:8090"+feishuInvitePath+"?invite=")
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(secondToken), "")
	state := stateFrom(t, res.Header.Get("Location"))
	res = f.request(t, http.MethodGet, feishuCallbackPath+"?error=access_denied&state="+url.QueryEscape(state), "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cancel: status=%d, want the explanation page", res.StatusCode)
	}
	res.Body.Close()
	// The link still works, so the person can simply try again.
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(secondToken), "")
	if res.StatusCode != http.StatusFound {
		t.Fatalf("the invitation stopped working after a cancel: status=%d", res.StatusCode)
	}
	res.Body.Close()
}

// An invitation names an identity that is already an administrator elsewhere: that is a
// conflict, and the account it was offered to stays as it was.
func TestFeishuAdminInviteConflictAndDisabled(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_taken"}); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"invitee","role":"viewer"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%d", res.StatusCode)
	}
	link := inviteFor(t, f, cookie, "3")
	token := strings.TrimPrefix(link, "http://dsh.example:8090"+feishuInvitePath+"?invite=")

	f.stub.identity = feishu.Identity{OpenID: "ou_taken", Name: "已占用"}
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(token), "")
	res = consentAndCallback(t, f, res.Header.Get("Location"))
	body := readBody(t, res.Body)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "已经绑定到另一个管理员账号") {
		t.Fatalf("conflict refusal: status=%d body=%q", res.StatusCode, body)
	}
	user, err := f.db.GetAdminUser(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if user.FeishuOpenID != "" || user.Status != domain.AdminPending {
		t.Fatalf("a refused redemption changed the account: %+v", user)
	}

	// A disabled account cannot be invited into: the link would fail at redemption anyway.
	res = f.call(t, http.MethodPatch, "/admin/api/v1/admin-users/3", `{"status":"disabled"}`, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable: status=%d", res.StatusCode)
	}
	res.Body.Close()
	res = f.call(t, http.MethodPost, "/admin/api/v1/admin-users/3/invite", "", cookie)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("inviting a disabled account: status=%d, want 409", res.StatusCode)
	}
	res.Body.Close()
}

// Without the Feishu integration the invitation endpoint answers 501 rather than minting a
// link nobody could redeem.
func TestAdminInviteRequiresFeishuAdminLogin(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users/1/invite", "", cookie)
	// domain.ErrUnsupported renders as 400 unsupported_error in this API, like every other
	// port that a deployment left unwired.
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invite without Feishu: status=%d, want 400", res.StatusCode)
	}
	res.Body.Close()
	// ...and the invitation route does not exist at all.
	res = f.call(t, http.MethodGet, feishuInvitePath+"?invite=x", "", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("invitation route without Feishu: status=%d, want 404", res.StatusCode)
	}
	res.Body.Close()
}

// A deployment that never configured Feishu must not grow its surface (M60's rule, kept for
// the invitation entry point).
func TestFeishuInviteRouteIsAbsentWhenDisabled(t *testing.T) {
	f := newAdminFixture(t)
	for _, path := range []string{feishuInvitePath, feishuInvitePath + "?invite=x"} {
		res := f.call(t, http.MethodGet, path, "", "")
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status=%d, want 404", path, res.StatusCode)
		}
		res.Body.Close()
	}
	// The console login entry point is part of the same surface.
	res := f.call(t, http.MethodGet, feishuLoginPath+"?mode=admin", "", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("%s?mode=admin: status=%d, want 404", feishuLoginPath, res.StatusCode)
	}
	res.Body.Close()
}

// The invitation link is long-lived by design, so its expiry is a separate failure from the
// OAuth state's: the page has to say which one ran out.
func TestFeishuAdminInviteExpiry(t *testing.T) {
	f := newFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"invitee","role":"viewer"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%d", res.StatusCode)
	}
	link := inviteFor(t, f, cookie, "3")
	token := strings.TrimPrefix(link, "http://dsh.example:8090"+feishuInvitePath+"?invite=")

	// The invitation codec keeps its own clock in the fixture, so the link can be aged
	// without waiting an hour.
	f.now = f.now.Add(2 * time.Hour)
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(token), "")
	body := readBody(t, res.Body)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "已经过期") {
		t.Fatalf("expired invitation: status=%d body=%q", res.StatusCode, body)
	}
}

// decodeInto reads one JSON response body into a struct or map.
func decodeInto(res *http.Response, target any) error {
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(target)
}

// readBody reads a response body as text. It exists because several refusals here are HTML
// pages whose wording is the contract: the reason a person is turned away is the only thing
// they see.
func readBody(t *testing.T, body io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A deployment can have Feishu for key binding and the DSH portal while leaving the console
// login off: then the login page shows no entry, the invitation route does not exist, and
// the administrators page keeps working as plain account management.
func TestAdminUserSurfaceWithFeishuLoginOff(t *testing.T) {
	states, err := feishu.NewStateCodec([]byte("state-key"), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f := newAdminFixtureWith(t, "", func(deps *Deps) {
		deps.Config.Feishu.Enabled = true
		deps.Config.Feishu.AdminLogin = false
		deps.Config.Feishu.CallbackURL = "http://dsh.example:8090" + feishuCallbackPath
		deps.Feishu = &FeishuDeps{
			Client:       &feishu.Client{AuthorizeURL: "https://accounts.feishu.cn/open-apis/authen/v1/authorize"},
			States:       states,
			RedirectURI:  deps.Config.Feishu.CallbackURL,
			LoginPath:    feishuLoginPath,
			CallbackPath: feishuCallbackPath,
			InvitePath:   feishuInvitePath,
			AdminLogin:   false,
		}
	})
	cookie := f.login(t, adminUser, adminPassword)

	var methods struct {
		Feishu struct {
			Enabled bool `json:"enabled"`
		} `json:"feishu"`
	}
	res := f.call(t, http.MethodGet, "/admin/api/v1/auth/methods", "", "")
	if err := decodeInto(res, &methods); err != nil {
		t.Fatal(err)
	}
	if methods.Feishu.Enabled {
		t.Fatal("the console advertised a login entry the deployment turned off")
	}
	res = f.call(t, http.MethodGet, feishuLoginPath+"?mode=admin", "", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("scan login with admin_login off: status=%d, want 404", res.StatusCode)
	}
	res.Body.Close()
	res = f.call(t, http.MethodGet, feishuInvitePath+"?invite=x", "", "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("invitation route with admin_login off: status=%d, want 404", res.StatusCode)
	}
	res.Body.Close()
	// The accounts themselves are still manageable: that part does not depend on Feishu.
	res = f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"extra","role":"viewer"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("creating an administrator without Feishu: status=%d", res.StatusCode)
	}
	res.Body.Close()
	res = f.call(t, http.MethodPost, "/admin/api/v1/admin-users/3/invite", "", cookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("inviting with admin_login off: status=%d, want the unsupported refusal", res.StatusCode)
	}
	res.Body.Close()
}

// ---------------------------------------------------------------------------
// Feishu: the console on another host than the callback
// ---------------------------------------------------------------------------

// A session cookie belongs to a host name, and the callback can only run on the origin
// registered with Feishu. When the console is reached under a different name — a LAN console
// behind a public callback, which is exactly this deployment — the callback hands the browser
// a one-time ticket instead, and the console's own origin redeems it.
func TestFeishuAdminLoginHandsOffAcrossHosts(t *testing.T) {
	f := newFeishuFixtureWithConsole(t, "http://console.test:8088/admin/ui/")
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_alice"}); err != nil {
		t.Fatal(err)
	}
	f.stub.identity = feishu.Identity{OpenID: "ou_alice", UnionID: "on_alice", Name: "张三"}

	res := consentAndCallback(t, f, adminLoginStart(t, f))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: status=%d", res.StatusCode)
	}
	location := res.Header.Get("Location")
	if !strings.HasPrefix(location, "http://console.test:8088/admin/feishu/session?ticket=") {
		t.Fatalf("callback location = %q, want the console's own origin with a ticket", location)
	}
	// The callback must NOT try to set the console's cookie: a cookie for the callback's host
	// would never reach the console, and one for another host cannot be set from here at all.
	for _, cookie := range res.Cookies() {
		if cookie.Name == adminCookieName {
			t.Fatal("the callback set a console cookie it cannot deliver")
		}
	}

	// The console's origin is the same server, reached under whatever name the operator uses,
	// so the redeem route is served here too. The ticket is the capability and it is spent.
	ticket := location[strings.Index(location, "ticket=")+len("ticket="):]
	redeem := f.request(t, http.MethodGet, "/admin/feishu/session?ticket="+url.QueryEscape(ticket), "")
	if redeem.StatusCode != http.StatusSeeOther || redeem.Header.Get("Location") != "/admin/ui/" {
		t.Fatalf("redeem: status=%d location=%q", redeem.StatusCode, redeem.Header.Get("Location"))
	}
	var session string
	for _, cookie := range redeem.Cookies() {
		if cookie.Name == adminCookieName {
			session = cookie.Value
		}
	}
	if session == "" {
		t.Fatal("redeeming the ticket did not set the console cookie")
	}
	me := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/auth/me", "", session))
	if me["username"] != "reader" || me["role"] != "viewer" {
		t.Fatalf("whoami = %+v, want the bound administrator", me)
	}

	// Single use: the same ticket cannot open a second session (browser history, a shared
	// URL, a retry all land here).
	replay := f.request(t, http.MethodGet, "/admin/feishu/session?ticket="+url.QueryEscape(ticket), "")
	if replay.StatusCode != http.StatusForbidden {
		t.Fatalf("replayed ticket: status=%d, want 403", replay.StatusCode)
	}
	for _, cookie := range replay.Cookies() {
		if cookie.Name == adminCookieName {
			t.Fatal("a replayed ticket issued a session")
		}
	}
	replay.Body.Close()
}

// The ticket route refuses everything that is not a live console ticket, including a ticket
// minted for the DSH portal (same key, other mode).
func TestFeishuConsoleTicketRefusals(t *testing.T) {
	f := newFeishuFixtureWithConsole(t, "http://console.test:8088/admin/ui/")
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_alice"}); err != nil {
		t.Fatal(err)
	}
	f.stub.identity = feishu.Identity{OpenID: "ou_alice", Name: "张三"}
	res := consentAndCallback(t, f, adminLoginStart(t, f))
	location := res.Header.Get("Location")
	ticket := location[strings.Index(location, "ticket=")+len("ticket="):]

	// A tampered ticket, a DSH ticket, and nothing at all.
	dsh, _, err := f.tickets.Issue("tenant", 1, 1, "ou_alice", "nonce-dsh")
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"tampered": ticket + "x",
		"dsh mode": dsh,
		"empty":    "",
	} {
		res := f.request(t, http.MethodGet, "/admin/feishu/session?ticket="+url.QueryEscape(raw), "")
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status=%d, want 403", name, res.StatusCode)
		}
		res.Body.Close()
	}

	// An account disabled between the consent screen and the redemption: the ticket proves the
	// identity, never the right to sign in.
	if err := f.db.SetAdminUserStatus(ctx, 2, domain.AdminDisabled); err != nil {
		t.Fatal(err)
	}
	late := f.request(t, http.MethodGet, "/admin/feishu/session?ticket="+url.QueryEscape(ticket), "")
	if late.StatusCode != http.StatusForbidden {
		t.Fatalf("ticket for a disabled account: status=%d, want 403", late.StatusCode)
	}
	if body := readBody(t, late.Body); !strings.Contains(body, "已被停用") {
		t.Fatalf("the refusal does not explain itself: %q", body)
	}
}

// An invitation redeemed from another host takes the same path, and the invitee ends up
// signed in on the console's own origin.
func TestFeishuAdminInviteHandsOffAcrossHosts(t *testing.T) {
	f := newFeishuFixtureWithConsole(t, "http://console.test:8088/admin/ui/")
	cookie := f.login(t, adminUser, adminPassword)
	res := f.call(t, http.MethodPost, "/admin/api/v1/admin-users", `{"username":"invitee","role":"admin"}`, cookie)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%d", res.StatusCode)
	}
	link := inviteFor(t, f, cookie, "3")
	token := strings.TrimPrefix(link, "http://dsh.example:8090"+feishuInvitePath+"?invite=")

	f.stub.identity = feishu.Identity{OpenID: "ou_new", Name: "新管理员"}
	res = f.request(t, http.MethodGet, feishuInvitePath+"?invite="+url.QueryEscape(token), "")
	authorize := res.Header.Get("Location")
	state := stateFrom(t, authorize)
	res = f.request(t, http.MethodGet, feishuCallbackPath+"?code=the-code&state="+url.QueryEscape(state), "")
	location := res.Header.Get("Location")
	if !strings.HasPrefix(location, "http://console.test:8088/admin/feishu/session?ticket=") {
		t.Fatalf("invitation callback location = %q, want the console's origin with a ticket", location)
	}
	ticket := location[strings.Index(location, "ticket=")+len("ticket="):]
	redeem := f.request(t, http.MethodGet, "/admin/feishu/session?ticket="+url.QueryEscape(ticket), "")
	var session string
	for _, c := range redeem.Cookies() {
		if c.Name == adminCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatalf("redeeming an invitation ticket did not sign the invitee in: status=%d", redeem.StatusCode)
	}
	me := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/auth/me", "", session))
	if me["username"] != "invitee" {
		t.Fatalf("the invitee signed in as %+v", me)
	}
}

// Without a configured console URL nothing changes: the callback sets the cookie directly.
func TestFeishuAdminLoginKeepsTheCookieWhenHostsMatch(t *testing.T) {
	f := newFeishuFixtureWithConsole(t, "http://dsh.example:8090/admin/ui/")
	ctx := context.Background()
	if err := f.db.BindAdminUserFeishu(ctx, 2, domain.FeishuBinding{OpenID: "ou_alice"}); err != nil {
		t.Fatal(err)
	}
	f.stub.identity = feishu.Identity{OpenID: "ou_alice", Name: "张三"}
	res := consentAndCallback(t, f, adminLoginStart(t, f))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "http://dsh.example:8090/admin/ui/" {
		t.Fatalf("callback: status=%d location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	found := false
	for _, cookie := range res.Cookies() {
		if cookie.Name == adminCookieName {
			found = true
		}
	}
	if !found {
		t.Fatal("a same-host login must still set the cookie on the callback")
	}
}

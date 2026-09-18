package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
)

// stubFeishu is a local stand-in for the three Feishu endpoints: the authorization page is
// not called by the client (the browser goes there), so only the token and user-info calls
// need to answer.
type stubFeishu struct {
	tokenStatus int
	tokenBody   string
	infoBody    string
	infoStatus  int
	// redirect, when set, is answered with a 302 so the "never follow a redirect" rule can
	// be exercised: following it would send the app secret somewhere else.
	redirect bool

	lastTokenForm url.Values
	lastInfoAuth  string
	server        *httptest.Server
}

func newStub(t *testing.T) *stubFeishu {
	t.Helper()
	stub := &stubFeishu{
		tokenStatus: http.StatusOK,
		tokenBody:   `{"code":0,"access_token":"u-token","expires_in":7200,"token_type":"Bearer"}`,
		infoStatus:  http.StatusOK,
		infoBody:    `{"code":0,"msg":"success","data":{"name":"张三","open_id":"ou_alice","union_id":"on_alice","avatar_url":"https://example.invalid/a.png"}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if stub.redirect {
			http.Redirect(w, r, "http://example.invalid/elsewhere", http.StatusFound)
			return
		}
		_ = r.ParseForm()
		stub.lastTokenForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.tokenStatus)
		_, _ = w.Write([]byte(stub.tokenBody))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		stub.lastInfoAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.infoStatus)
		_, _ = w.Write([]byte(stub.infoBody))
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *stubFeishu) client() *Client {
	return &Client{
		AppID: "cli_test", AppSecret: "secret",
		AuthorizeURL: "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
		TokenURL:     s.server.URL + "/token",
		UserInfoURL:  s.server.URL + "/userinfo",
		Timeout:      5 * time.Second,
	}
}

func TestExchangeReturnsTheIdentity(t *testing.T) {
	stub := newStub(t)
	identity, err := stub.client().Exchange(context.Background(), "the-code", "http://gw:8090/feishu/callback")
	if err != nil {
		t.Fatal(err)
	}
	if identity.OpenID != "ou_alice" || identity.UnionID != "on_alice" || identity.Name != "张三" {
		t.Fatalf("identity = %+v", identity)
	}
	// The token call must carry the exact redirect_uri that obtained the code (20071
	// otherwise) and the code itself, and the identity call must use the token.
	form := stub.lastTokenForm
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" {
		t.Fatalf("token form = %v", form)
	}
	if form.Get("redirect_uri") != "http://gw:8090/feishu/callback" {
		t.Fatalf("redirect_uri = %q", form.Get("redirect_uri"))
	}
	if form.Get("client_id") != "cli_test" || form.Get("client_secret") != "secret" {
		t.Fatalf("credentials missing from the token form: %v", form)
	}
	if stub.lastInfoAuth != "Bearer u-token" {
		t.Fatalf("user-info authorization = %q", stub.lastInfoAuth)
	}
}

func TestExchangeClassifiesFeishuErrors(t *testing.T) {
	cases := map[string]struct {
		code int
		kind ErrorKind
	}{
		"code not found":         {20003, KindCodeRejected},
		"code expired":           {20004, KindCodeRejected},
		"code already used":      {20065, KindCodeRejected},
		"client secret invalid":  {20002, KindCredentials},
		"app not installed":      {20009, KindAppUnavailable},
		"user has no permission": {20010, KindAppUnavailable},
		"app disabled":           {20069, KindAppUnavailable},
		"server error":           {20050, KindUnreachable},
	}
	for name, test := range cases {
		stub := newStub(t)
		stub.tokenBody = `{"code":` + itoa(test.code) + `,"error":"e","error_description":"d"}`
		stub.tokenStatus = http.StatusBadRequest
		_, err := stub.client().Exchange(context.Background(), "the-code", "")
		var feishuErr *Error
		if !asError(err, &feishuErr) {
			t.Fatalf("%s: error = %v, want a typed Feishu error", name, err)
		}
		if feishuErr.Kind != test.kind {
			t.Errorf("%s: kind = %s, want %s", name, feishuErr.Kind, test.kind)
		}
		if strings.Contains(feishuErr.Message, "secret") && strings.Contains(feishuErr.Message, "cli_test") {
			t.Errorf("%s: the message leaks credentials: %s", name, feishuErr.Message)
		}
	}
}

func TestExchangeRejectsMalformedAndOversizedAnswers(t *testing.T) {
	// Not JSON at all.
	stub := newStub(t)
	stub.tokenBody = `<html>proxy error</html>`
	if _, err := stub.client().Exchange(context.Background(), "c", ""); !asError(err, nil) {
		t.Fatalf("a non-JSON token response was accepted: %v", err)
	}
	// A success code with no token.
	stub = newStub(t)
	stub.tokenBody = `{"code":0}`
	if _, err := stub.client().Exchange(context.Background(), "c", ""); !asError(err, nil) {
		t.Fatalf("a token response without a token was accepted: %v", err)
	}
	// Bigger than the read cap.
	stub = newStub(t)
	stub.tokenBody = `{"code":0,"access_token":"` + strings.Repeat("x", maxBody+100) + `"}`
	if _, err := stub.client().Exchange(context.Background(), "c", ""); !asError(err, nil) {
		t.Fatalf("an oversized token response was accepted: %v", err)
	}
	// An unexpected status is not a credential verdict.
	stub = newStub(t)
	stub.tokenStatus = http.StatusBadGateway
	stub.tokenBody = `{"code":0}`
	_, err := stub.client().Exchange(context.Background(), "c", "")
	var feishuErr *Error
	if !asError(err, &feishuErr) || feishuErr.Kind != KindUnreachable {
		t.Fatalf("a 502 was classified as %v, want unreachable", err)
	}
}

// Following a redirect would hand the app secret to whatever the Location names, so the
// client refuses to follow one and reports it as unreachable.
func TestExchangeRefusesRedirects(t *testing.T) {
	stub := newStub(t)
	stub.redirect = true
	_, err := stub.client().Exchange(context.Background(), "c", "")
	var feishuErr *Error
	if !asError(err, &feishuErr) || feishuErr.Kind != KindUnreachable {
		t.Fatalf("a redirecting token endpoint produced %v, want unreachable", err)
	}
	if feishuErr.Status != http.StatusFound {
		t.Fatalf("status = %d, want the redirect status", feishuErr.Status)
	}
}

func TestUserInfoProblems(t *testing.T) {
	// A refusal from the identity call.
	stub := newStub(t)
	stub.infoBody = `{"code":20005,"msg":"invalid token"}`
	if _, err := stub.client().Exchange(context.Background(), "c", ""); !asError(err, nil) {
		t.Fatalf("an identity refusal was accepted: %v", err)
	}
	// A success answer without an open id is useless: the binding keys on it.
	stub = newStub(t)
	stub.infoBody = `{"code":0,"data":{"name":"张三"}}`
	_, err := stub.client().Exchange(context.Background(), "c", "")
	var feishuErr *Error
	if !asError(err, &feishuErr) || feishuErr.Kind != KindUserInfo {
		t.Fatalf("missing open id produced %v", err)
	}
}

func TestExchangeRejectsAnEmptyCode(t *testing.T) {
	stub := newStub(t)
	_, err := stub.client().Exchange(context.Background(), "  ", "")
	var feishuErr *Error
	if !asError(err, &feishuErr) || feishuErr.Kind != KindCodeRejected {
		t.Fatalf("an empty code produced %v", err)
	}
	if stub.lastTokenForm != nil {
		t.Fatal("an empty code still reached the token endpoint")
	}
}

func TestAuthorizeURLForCarriesTheRequiredParameters(t *testing.T) {
	client := &Client{
		AppID:        "cli_test",
		AuthorizeURL: "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
	}
	raw, err := client.AuthorizeURLFor("state.value", "http://gw:8090/feishu/callback")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"client_id=cli_test",
		"response_type=code",
		"state=state.value",
		"redirect_uri=http%3A%2F%2Fgw%3A8090%2Ffeishu%2Fcallback",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("authorize URL is missing %q: %s", want, raw)
		}
	}
	// No scope is requested by default: open_id and the name need no permission, and a
	// consent screen for unread data is a bad first impression.
	if strings.Contains(raw, "scope=") {
		t.Errorf("authorize URL asks for scopes that are not configured: %s", raw)
	}
	// Configured scopes do travel.
	client.Scopes = []string{"offline_access"}
	raw, err = client.AuthorizeURLFor("s", "http://gw/feishu/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "scope=offline_access") {
		t.Fatalf("configured scope missing: %s", raw)
	}
}

// New wires the configuration into a client without inventing values.
func TestNewReadsTheConfiguration(t *testing.T) {
	cfg := config.Default().Feishu
	cfg.AppID = " cli_x "
	cfg.AppSecret = " s "
	cfg.Scopes = "a  b"
	cfg.TimeoutS = 7
	client := New(cfg)
	if client.AppID != "cli_x" || client.AppSecret != "s" {
		t.Fatalf("values not trimmed: %+v", client)
	}
	if len(client.Scopes) != 2 || client.Scopes[0] != "a" || client.Scopes[1] != "b" {
		t.Fatalf("scopes = %v", client.Scopes)
	}
	if client.Timeout != 7*time.Second {
		t.Fatalf("timeout = %s", client.Timeout)
	}
	if client.AuthorizeURL != cfg.AuthorizeURL || client.TokenURL != cfg.TokenURL {
		t.Fatalf("endpoints not taken from the configuration: %+v", client)
	}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			if target != nil {
				*target = e
			}
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

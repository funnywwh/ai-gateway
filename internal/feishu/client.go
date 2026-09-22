package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/config"
)

// maxBody caps every response this client reads. Feishu's answers are small; anything
// larger is either a proxy page or an attack, and neither belongs in a log line.
const maxBody = 64 << 10

// Identity is the person Feishu says the browser belongs to. OpenID is app-scoped and
// stable, and is the only field used as a binding key; the rest is display material.
type Identity struct {
	OpenID    string
	UnionID   string
	Name      string
	AvatarURL string
}

// ErrorKind classifies a failed call so the caller can show a useful message without
// leaking Feishu's own text into the page.
type ErrorKind string

const (
	// KindCodeRejected means the authorization code was missing, expired or already used:
	// the user needs to start over, and nothing is wrong with the deployment.
	KindCodeRejected ErrorKind = "code_rejected"
	// KindCredentials means the app id/secret pair was refused: a deployment mistake.
	KindCredentials ErrorKind = "credentials"
	// KindAppUnavailable means the app is not installed/enabled, or the user has no
	// permission to use it (20009/20010/20069): the deployment's availability settings.
	KindAppUnavailable ErrorKind = "app_unavailable"
	// KindRateLimited means Feishu asked us to slow down.
	KindRateLimited ErrorKind = "rate_limited"
	// KindUnreachable means the call did not complete: network, timeout, TLS, bad JSON.
	KindUnreachable ErrorKind = "unreachable"
	// KindUserInfo means the identity call failed or answered without an open id.
	KindUserInfo ErrorKind = "user_info"
)

// Error is one failed call. Message is safe to log and to branch on; it never contains a
// token, a secret or a raw response body.
type Error struct {
	Kind    ErrorKind
	Message string
	Status  int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("feishu %s: %s (HTTP %d)", e.Kind, e.Message, e.Status)
	}
	return fmt.Sprintf("feishu %s: %s", e.Kind, e.Message)
}

// Client performs the two server-side calls of the authorization code flow plus the
// contact-directory reads of the org sync (M70, see directory.go).
type Client struct {
	AppID        string
	AppSecret    string
	AuthorizeURL string
	TokenURL     string
	UserInfoURL  string
	// TenantTokenURL and ContactURL serve the directory read. Empty means the documented
	// production endpoints (DefaultTenantTokenURL/DefaultContactURL); they are configurable
	// so tests can point them at a stub server.
	TenantTokenURL string
	ContactURL     string
	Scopes         []string
	// token caches the tenant access token between directory calls. It lives in memory
	// only — like every Feishu token in this package, it is never persisted.
	token tenantToken
	// HTTP is injectable for tests. It never follows redirects and never carries a jar:
	// the only cookies in this flow belong to the browser.
	HTTP    *http.Client
	Timeout time.Duration
}

// directTransport mirrors the provider transports: no environment proxy, because the
// gateway's own egress is a deployment decision (and a proxy here would silently receive
// the app secret).
var directTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

// New builds a client from the configuration block.
func New(cfg config.Feishu) *Client {
	return &Client{
		AppID:          strings.TrimSpace(cfg.AppID),
		AppSecret:      strings.TrimSpace(cfg.AppSecret),
		AuthorizeURL:   strings.TrimSpace(cfg.AuthorizeURL),
		TokenURL:       strings.TrimSpace(cfg.TokenURL),
		UserInfoURL:    strings.TrimSpace(cfg.UserInfoURL),
		TenantTokenURL: strings.TrimSpace(cfg.TenantTokenURL),
		ContactURL:     strings.TrimSpace(cfg.ContactURL),
		Scopes:         strings.Fields(cfg.Scopes),
		Timeout:        time.Duration(cfg.TimeoutS) * time.Second,
	}
}

func (c *Client) httpClient() *http.Client {
	client := http.Client{Timeout: c.Timeout}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	if client.Transport == nil {
		client.Transport = directTransport
	}
	if client.Timeout == 0 {
		client.Timeout = 5 * time.Second
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// AuthorizeURLFor builds the URL the browser is sent to. state is the signed state; the
// redirect URI must be byte-identical to the one registered in the Feishu console and to
// the one sent to the token endpoint, so it is passed through unchanged.
func (c *Client) AuthorizeURLFor(state, redirectURI string) (string, error) {
	parsed, err := url.Parse(c.AuthorizeURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("feishu: authorize_url is not an absolute URL")
	}
	query := parsed.Query()
	query.Set("client_id", c.AppID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	if len(c.Scopes) > 0 {
		query.Set("scope", strings.Join(c.Scopes, " "))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// Exchange turns an authorization code into an identity: it fetches the user access token
// and then the user's own information. The token is used once and dropped — this feature
// stores no Feishu credential at all.
func (c *Client) Exchange(ctx context.Context, code, redirectURI string) (Identity, error) {
	if strings.TrimSpace(code) == "" {
		return Identity{}, &Error{Kind: KindCodeRejected, Message: "the authorization code is missing"}
	}
	token, err := c.userAccessToken(ctx, code, redirectURI)
	if err != nil {
		return Identity{}, err
	}
	return c.userInfo(ctx, token)
}

func (c *Client) userAccessToken(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", c.AppID)
	form.Set("client_secret", c.AppSecret)
	form.Set("code", code)
	if redirectURI != "" {
		// Feishu rejects a mismatch between this value and the one used to obtain the
		// code (20071), so it is always sent when known.
		form.Set("redirect_uri", redirectURI)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", &Error{Kind: KindUnreachable, Message: "building the token request failed"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, status, err := c.do(req)
	if err != nil {
		return "", err
	}
	var payload struct {
		Code        int    `json:"code"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", &Error{Kind: KindUnreachable, Message: "the token response was not JSON", Status: status}
	}
	// Feishu reports its own refusals as HTTP 400 with a JSON body carrying the error code,
	// so the body decides the classification and the status only says whether an answer
	// arrived at all.
	if payload.Code != 0 {
		return "", classifyTokenError(payload.Code, status)
	}
	if status != http.StatusOK {
		return "", &Error{Kind: KindUnreachable, Message: "the token endpoint answered with an unexpected status", Status: status}
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", &Error{Kind: KindUnreachable, Message: "the token response carried no access token", Status: status}
	}
	return payload.AccessToken, nil
}

// classifyTokenError maps Feishu's documented error codes onto the four situations a
// deployment can actually act on. The codes are from the token endpoint's documentation.
func classifyTokenError(code, status int) error {
	switch code {
	case 20003, 20004, 20065:
		return &Error{Kind: KindCodeRejected, Message: "the authorization code is invalid, expired or already used", Status: status}
	case 20002, 20001, 20024, 20048, 20070:
		return &Error{Kind: KindCredentials, Message: "Feishu rejected the application credentials", Status: status}
	case 20009, 20010, 20066, 20069, 20008:
		return &Error{Kind: KindAppUnavailable, Message: "the application is not available to this user", Status: status}
	case 20072:
		return &Error{Kind: KindUnreachable, Message: "Feishu reported a temporary failure", Status: status}
	default:
		return &Error{Kind: KindUnreachable, Message: fmt.Sprintf("Feishu returned error code %d", code), Status: status}
	}
}

func (c *Client) userInfo(ctx context.Context, accessToken string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UserInfoURL, nil)
	if err != nil {
		return Identity{}, &Error{Kind: KindUnreachable, Message: "building the user-info request failed"}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	body, status, err := c.do(req)
	if err != nil {
		return Identity{}, err
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OpenID    string `json:"open_id"`
			UnionID   string `json:"union_id"`
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Identity{}, &Error{Kind: KindUserInfo, Message: "the user-info response was not JSON", Status: status}
	}
	if payload.Code != 0 {
		kind := KindUserInfo
		if payload.Code == 20005 {
			// The token was refused: treat it as a deployment-side failure rather than a
			// user error, because the user did nothing wrong at this point.
			kind = KindUnreachable
		}
		return Identity{}, &Error{Kind: kind, Message: fmt.Sprintf("Feishu returned error code %d for the user info", payload.Code), Status: status}
	}
	if strings.TrimSpace(payload.Data.OpenID) == "" {
		return Identity{}, &Error{Kind: KindUserInfo, Message: "the user-info response carried no open id", Status: status}
	}
	return Identity{
		OpenID:    payload.Data.OpenID,
		UnionID:   payload.Data.UnionID,
		Name:      payload.Data.Name,
		AvatarURL: payload.Data.AvatarURL,
	}, nil
}

// do performs one request with the shared defences: bounded body, redirects refused
// (a redirect here would hand the app secret to whatever the Location names) and an
// explicit split between "Feishu answered" and "we never got an answer". A non-2xx status
// is not by itself a failure: Feishu reports its own errors inside a JSON body, so the
// caller classifies those. Server errors and rate limits are decided here, because there
// is nothing in their body worth interpreting.
func (c *Client) do(req *http.Request) ([]byte, int, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, 0, &Error{Kind: KindUnreachable, Message: "the call to Feishu timed out"}
		}
		return nil, 0, &Error{Kind: KindUnreachable, Message: "the call to Feishu failed"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, resp.StatusCode, &Error{Kind: KindRateLimited, Message: "Feishu is rate limiting this application", Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, resp.StatusCode, &Error{Kind: KindUnreachable, Message: "reading Feishu's response failed", Status: resp.StatusCode}
	}
	if len(body) > maxBody {
		return nil, resp.StatusCode, &Error{Kind: KindUnreachable, Message: "Feishu's response exceeded the size limit", Status: resp.StatusCode}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, resp.StatusCode, &Error{Kind: KindUnreachable, Message: "Feishu answered with a server error", Status: resp.StatusCode}
	}
	return body, resp.StatusCode, nil
}

// Command provider-codex is a reference plugin that wraps a subscription-backed
// Responses endpoint (ChatGPT/Codex style) behind the gateway's plugin protocol.
//
// It is deliberately an example rather than a builtin provider: such backends are
// unofficial, may stop working without notice, and carry terms-of-service risk, so
// they must be deployable and removable out of tree and stay disabled by default.
//
// Credentials are resolved in this order: refresh_token (OAuth refresh, fully
// automatic) > session_cookie (exchanged at the session endpoint) > access_token
// (static, expires eventually). Everything is stored in the plugin's state
// directory; tokens never appear in logs or error messages.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

const (
	defaultBaseURL    = "https://chatgpt.com/backend-api/codex"
	defaultSessionURL = "https://chatgpt.com/api/auth/session"
	defaultTokenURL   = "https://auth.openai.com/oauth/token"
	// defaultClientID comes from public precedent for the Codex client; override it
	// in credentials when the upstream expects a different one.
	defaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	sessionFile     = "session.json"
	// defaultHealthPrompt is the probe prompt: short enough to be cheap, real
	// enough to exercise the whole path (auth, model, stream, translation).
	defaultHealthPrompt = "hi"
	// refreshSkew refreshes slightly before expiry so an in-flight request never
	// carries a token that dies mid-stream; startupSkew is the startup margin.
	refreshSkew = 60 * time.Second
	startupSkew = 5 * time.Minute
)

type modelConfig struct {
	ID              string          `json:"id"`
	UpstreamModel   string          `json:"upstream_model"`
	DisplayName     string          `json:"display_name"`
	ContextWindow   int             `json:"context_window"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Capabilities    map[string]bool `json:"capabilities"`
}

type config struct {
	BaseURL    string            `json:"base_url"`
	SessionURL string            `json:"session_url"`
	TokenURL   string            `json:"token_url"`
	ClientID   string            `json:"client_id"`
	Models     []modelConfig     `json:"models"`
	Headers    map[string]string `json:"headers"`
	// Health probes a real streaming completion (see Health), which is the only
	// reliable liveness signal for this backend: the /me style endpoint probe it
	// replaced was Cloudflare-challenged and reported a healthy provider as broken.
	HealthPrompt    string `json:"health_prompt"`
	HealthModel     string `json:"health_model"`
	Store           bool   `json:"store"`
	ReasoningEffort string `json:"reasoning_effort"`
	TimeoutMS       int    `json:"timeout_ms"`
	AccountID       string `json:"account_id"`
	// Proxy is the egress proxy for every upstream call. Empty means "follow the
	// process environment" (HTTPS_PROXY/HTTP_PROXY/NO_PROXY), i.e. the behaviour
	// before M10b. Credentials may override it; see credentialsSchema.
	Proxy string `json:"proxy"`
}

var configSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "base_url": {"type": "string", "description": "Responses endpoint root"},
    "session_url": {"type": "string", "description": "Endpoint that turns a session cookie into an access token"},
    "token_url": {"type": "string", "description": "OAuth token endpoint used with refresh_token"},
    "client_id": {"type": "string"},
    "account_id": {"type": "string", "description": "Value for the chatgpt-account-id header"},
    "reasoning_effort": {"type": "string", "enum": ["minimal", "low", "medium", "high"]},
    "store": {"type": "boolean", "description": "Whether the upstream may store the response (usually false)"},
    "health_prompt": {"type": "string", "x-advanced": true,
      "description": "Prompt sent by the health probe. The probe performs a real streaming completion, so it costs a few tokens; keep it short."},
    "health_model": {"type": "string", "x-advanced": true,
      "description": "Model the health probe asks for. Empty uses the first configured model."},
    "timeout_ms": {"type": "integer", "minimum": 1000},
    "proxy": {"type": "string", "x-advanced": true,
      "description": "Egress proxy for upstream calls: http://host:port, https://host:port, socks5://host:port. Empty follows HTTPS_PROXY/NO_PROXY. Only needed when the direct egress is blocked (for example by a region restriction)."},
    "headers": {"type": "object"},
    "models": {"type": "array", "items": {"type": "object"}}
  }
}`)

var credentialsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "refresh_token": {"type": "string", "description": "Preferred: refreshed automatically"},
    "session_cookie": {"type": "string", "description": "__Secure-next-auth.session-token from the browser"},
    "access_token": {"type": "string", "description": "Static token; expires and then needs replacing"},
    "client_id": {"type": "string"},
    "token_endpoint": {"type": "string"},
    "token_file": {"type": "string", "description": "Import tokens from a local file once (for example a CLI auth.json)"},
    "account_id": {"type": "string"},
    "proxy": {"type": "string", "x-secret": true,
      "description": "Overrides the configured proxy. Put it here when the URL carries credentials (user:pass), because config is stored and displayed in clear text while credentials are sealed."}
  }
}`)

// session is the persisted credential state.
type session struct {
	Mode                  string     `json:"mode"`
	AccessToken           string     `json:"access_token"`
	RefreshToken          string     `json:"refresh_token"`
	AccountID             string     `json:"account_id"`
	ExpiresAt             *time.Time `json:"expires_at"`
	RefreshTokenExpiresAt *time.Time `json:"refresh_token_expires_at"`
	LastRefreshAt         *time.Time `json:"last_refresh_at"`
	LastError             string     `json:"last_error"`
}

// proxySetting is one immutable view of the egress proxy in force. It is swapped
// as a whole through an atomic pointer so the request path never locks and never
// races with a credential push.
type proxySetting struct {
	url    *url.URL // nil with err == nil means "not configured: follow the environment"
	source string   // credentials | config | env
	err    error    // non-nil means the configured value is unusable
}

type provider struct {
	cfg      config
	stateDir string
	http     *http.Client
	now      func() time.Time

	// transport is shared for the whole process; only its Proxy hook changes.
	transport *http.Transport
	proxy     atomic.Pointer[proxySetting]
	// envProxy is a seam for tests: the standard library memoizes the environment
	// on first use (net/http envProxyOnce), so asserting the fallback behaviour via
	// t.Setenv would depend on test ordering.
	envProxy func(*http.Request) (*url.URL, error)

	mu          sync.Mutex
	creds       map[string]string
	state       session
	stateLoaded bool
	refreshing  bool
	refreshCh   chan struct{}
}

func main() {
	p := &provider{
		cfg:      config{HealthPrompt: defaultHealthPrompt, TimeoutMS: 300000},
		now:      func() time.Time { return time.Now().UTC() },
		envProxy: http.ProxyFromEnvironment,
	}
	// Clone the stock transport so the dial/TLS timeouts and HTTP/2 support the
	// standard library tuned for us are preserved; only Proxy is replaced.
	p.installTransport()

	if raw := os.Getenv(pluginapi.EnvConfig); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.cfg); err != nil {
			fmt.Fprintln(os.Stderr, "provider-codex: bad GW_PLUGIN_CONFIG:", err)
			os.Exit(2)
		}
	}
	p.stateDir = os.Getenv(pluginapi.EnvStateDir)
	// applyDefaults validates the configured proxy and records any fault. A bad
	// proxy value is deliberately NOT a startup exit: the host surfaces a dead
	// plugin only as "handshake: EOF" (its stderr is not visible in the console nor
	// in the gateway log), whereas a recorded fault reaches the operator as the
	// provider's last_error, e.g. "proxy_invalid: provider-codex: bad config proxy: ...".
	p.applyDefaults()
	if err := pluginapi.Serve(p); err != nil {
		fmt.Fprintln(os.Stderr, "provider-codex:", err)
		os.Exit(1)
	}
}

func (p *provider) applyDefaults() {
	if p.cfg.BaseURL == "" {
		p.cfg.BaseURL = defaultBaseURL
	}
	p.cfg.BaseURL = strings.TrimRight(p.cfg.BaseURL, "/")
	if p.cfg.SessionURL == "" {
		p.cfg.SessionURL = defaultSessionURL
	}
	if p.cfg.TokenURL == "" {
		p.cfg.TokenURL = defaultTokenURL
	}
	if p.cfg.ClientID == "" {
		p.cfg.ClientID = defaultClientID
	}
	if p.cfg.HealthPrompt == "" {
		p.cfg.HealthPrompt = defaultHealthPrompt
	}
	if p.cfg.TimeoutMS <= 0 {
		p.cfg.TimeoutMS = 300000
	}
	p.http.Timeout = time.Duration(p.cfg.TimeoutMS) * time.Millisecond
	p.applyProxy()
}

// ---------------------------------------------------------------------------
// egress proxy
// ---------------------------------------------------------------------------

// installTransport wires the shared client whose transport carries the dynamic
// Proxy hook. Only Proxy is replaced: cloning DefaultTransport keeps the dial and
// TLS timeouts and the HTTP/2 opt-in that the standard library chose.
func (p *provider) installTransport() {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	p.transport = base.Clone()
	p.transport.Proxy = p.proxyFor
	p.http = &http.Client{Transport: p.transport}
}

// effectiveProxy resolves the proxy in force: credentials beat config, and an
// empty value means "follow the process environment" (the pre-M10b behaviour).
func (p *provider) effectiveProxy() *proxySetting {
	raw, source := strings.TrimSpace(p.credential("proxy")), "credentials"
	if raw == "" {
		raw, source = strings.TrimSpace(p.cfg.Proxy), "config"
	}
	if raw == "" {
		return &proxySetting{source: "env"}
	}
	u, err := providerkit.ParseProxyURL(raw)
	if err != nil {
		return &proxySetting{source: source, err: fmt.Errorf("provider-codex: bad %s proxy: %w", source, err)}
	}
	return &proxySetting{url: u, source: source}
}

// applyProxy installs the current setting. It runs at startup and whenever
// credentials arrive; the request path only ever reads the atomic pointer.
func (p *provider) applyProxy() {
	next := p.effectiveProxy()
	prev := p.proxy.Load()
	p.proxy.Store(next)
	if prev != nil && proxyKey(prev) != proxyKey(next) && p.transport != nil {
		// Pooled connections were dialled through the previous route; drop them so
		// the change applies to the next request instead of the next reconnect.
		p.transport.CloseIdleConnections()
	}
}

// proxyKey is the identity used to notice a change. Source and error are
// deliberately excluded so a credential push that does not move the URL does not
// churn the connection pool.
func proxyKey(s *proxySetting) string {
	if s == nil || s.url == nil {
		return ""
	}
	return s.url.String()
}

// proxyFor is the transport's Proxy hook: it returns the configured proxy, or
// falls back to the environment when none is configured. Returning an error here
// fails the request with a readable message, which is why no call site needs a
// special case for an invalid setting.
func (p *provider) proxyFor(req *http.Request) (*url.URL, error) {
	s := p.proxy.Load()
	if s == nil || (s.err == nil && s.url == nil) {
		return p.envProxyFor(req)
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.url, nil
}

// envProxyFor guards the test seam: a provider built directly in a test may not
// have set envProxy.
func (p *provider) envProxyFor(req *http.Request) (*url.URL, error) {
	if p.envProxy == nil {
		return nil, nil
	}
	return p.envProxy(req)
}

// proxyError surfaces an unusable proxy setting as a fatal protocol error:
// retrying or failing over cannot fix a typo, and the operator sees the reason in
// the provider's health and last_error.
func (p *provider) proxyError() error {
	s := p.proxy.Load()
	if s == nil || s.err == nil {
		return nil
	}
	return pluginapi.NewError("proxy_invalid", s.err.Error())
}

func (p *provider) Info() pluginapi.Info {
	return pluginapi.Info{
		Name:    "provider-codex",
		Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{
			Complete: true, Stream: true, Health: true,
			UsageDimensions: true, UsageDelta: true,
			Actions: []pluginapi.Action{
				{Name: "whoami", Title: "Show the current session state"},
				{Name: "refresh_session", Title: "Refresh the access token now"},
				{Name: "set_token", Title: "Replace the stored credentials"},
			},
		},
	}
}

func (p *provider) ConfigSchema() json.RawMessage      { return configSchema }
func (p *provider) CredentialsSchema() json.RawMessage { return credentialsSchema }
func (p *provider) StateDir() string                   { return p.stateDir }
func (p *provider) ListModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	out := make([]pluginapi.ModelInfo, 0, len(p.cfg.Models))
	for _, model := range p.cfg.Models {
		id := model.UpstreamModel
		if id == "" {
			id = model.ID
		}
		out = append(out, pluginapi.ModelInfo{
			ID: model.ID, UpstreamModel: id, DisplayName: model.DisplayName,
			ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxOutputTokens,
			Capabilities: model.Capabilities,
		})
	}
	return out, nil
}

func (p *provider) SetCredentials(creds map[string]string) {
	p.mu.Lock()
	p.creds = creds
	p.mu.Unlock()
	// Credentials may carry a proxy override, and SetCredentials has no error
	// return, so an invalid value is recorded and reported through Health.
	p.applyProxy()
}

func (p *provider) credential(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.creds == nil {
		return ""
	}
	return strings.TrimSpace(p.creds[name])
}

// Actions lists the interactive operations; they are what an operator uses to
// recover from an expired credential without restarting anything.
func (p *provider) Actions() []pluginapi.Action {
	return p.Info().Capabilities.Actions
}

func (p *provider) RunAction(ctx context.Context, name string, in json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "whoami":
		state, err := p.currentState()
		if err != nil {
			return nil, err
		}
		return json.Marshal(p.describe(state))
	case "refresh_session":
		if _, err := p.ensureToken(ctx, true); err != nil {
			return nil, err
		}
		state, err := p.currentState()
		if err != nil {
			return nil, err
		}
		return json.Marshal(p.describe(state))
	case "set_token":
		var body struct {
			AccessToken   string `json:"access_token"`
			RefreshToken  string `json:"refresh_token"`
			SessionCookie string `json:"session_cookie"`
			AccountID     string `json:"account_id"`
		}
		if len(in) > 0 {
			if err := json.Unmarshal(in, &body); err != nil {
				return nil, pluginapi.NewError("bad_request", "set_token: invalid JSON")
			}
		}
		if body.AccessToken == "" && body.RefreshToken == "" && body.SessionCookie == "" {
			return nil, pluginapi.NewError("bad_request", "set_token: provide access_token, refresh_token or session_cookie")
		}
		p.mu.Lock()
		if p.creds == nil {
			p.creds = map[string]string{}
		}
		if body.AccessToken != "" {
			p.creds["access_token"] = body.AccessToken
		}
		if body.RefreshToken != "" {
			p.creds["refresh_token"] = body.RefreshToken
		}
		if body.SessionCookie != "" {
			p.creds["session_cookie"] = body.SessionCookie
		}
		if body.AccountID != "" {
			p.creds["account_id"] = body.AccountID
		}
		p.mu.Unlock()
		if _, err := p.ensureToken(ctx, true); err != nil {
			return nil, err
		}
		state, err := p.currentState()
		if err != nil {
			return nil, err
		}
		return json.Marshal(p.describe(state))
	default:
		return nil, pluginapi.NewError("unknown_action", "unknown action: "+name)
	}
}

// ---------------------------------------------------------------------------
// credentials and session state
// ---------------------------------------------------------------------------

type credSnapshot struct {
	AccessToken   string
	RefreshToken  string
	SessionCookie string
	AccountID     string
	ClientID      string
	TokenEndpoint string
	TokenFile     string
}

func (p *provider) snapshot() credSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return credSnapshot{
		AccessToken:   strings.TrimSpace(p.creds["access_token"]),
		RefreshToken:  strings.TrimSpace(p.creds["refresh_token"]),
		SessionCookie: strings.TrimSpace(p.creds["session_cookie"]),
		AccountID:     strings.TrimSpace(p.creds["account_id"]),
		ClientID:      strings.TrimSpace(p.creds["client_id"]),
		TokenEndpoint: strings.TrimSpace(p.creds["token_endpoint"]),
		TokenFile:     strings.TrimSpace(p.creds["token_file"]),
	}
}

func (p *provider) statePath() string {
	if p.stateDir == "" {
		return ""
	}
	return filepath.Join(p.stateDir, sessionFile)
}

func (p *provider) loadState() error {
	p.mu.Lock()
	if p.stateLoaded {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	path := p.statePath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			p.mu.Lock()
			p.stateLoaded = true
			p.mu.Unlock()
			return nil
		}
		return pluginapi.NewError("state_read_failed", "cannot read the session state file")
	}
	var state session
	if err := json.Unmarshal(raw, &state); err != nil {
		return pluginapi.NewError("state_corrupt", "the session state file is not valid JSON")
	}
	p.mu.Lock()
	p.state = state
	p.stateLoaded = true
	p.mu.Unlock()
	return nil
}

// saveState replaces the state file atomically so a crash cannot leave a half
// written refresh token behind (that would break the refresh chain for good).
func (p *provider) saveState(state session) error {
	path := p.statePath()
	p.mu.Lock()
	p.state = state
	p.stateLoaded = true
	p.mu.Unlock()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return pluginapi.NewError("state_write_failed", "cannot create the plugin state directory")
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return pluginapi.NewError("state_write_failed", "cannot encode the session state")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return pluginapi.NewError("state_write_failed", "cannot write the session state")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return pluginapi.NewError("state_write_failed", "cannot replace the session state")
	}
	return nil
}

func (p *provider) currentState() (session, error) {
	if err := p.loadState(); err != nil {
		return session{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, nil
}

// expiring reports whether a deadline is missing or within the margin.
func (p *provider) expiring(at *time.Time, margin time.Duration) bool {
	if at == nil {
		return true
	}
	return at.Sub(p.now()) <= margin
}

// ensureToken returns a usable access token, refreshing when necessary. Concurrent
// callers share one refresh: the first performs it, the rest wait for the result
// instead of stampeding the token endpoint.
func (p *provider) ensureToken(ctx context.Context, force bool) (string, error) {
	if err := p.loadState(); err != nil {
		return "", err
	}
	snap := p.snapshot()
	p.mu.Lock()
	state := p.state
	p.mu.Unlock()

	if snap.TokenFile != "" && state.AccessToken == "" && state.RefreshToken == "" {
		imported, err := importTokenFile(snap.TokenFile)
		if err != nil {
			return "", err
		}
		imported.Mode = "imported"
		if err := p.saveState(imported); err != nil {
			return "", err
		}
		state = imported
	}

	mode := "access_token"
	switch {
	case snap.RefreshToken != "" || state.RefreshToken != "":
		mode = "refresh_token"
	case snap.SessionCookie != "":
		mode = "session_cookie"
	}

	refreshable := mode == "refresh_token" || mode == "session_cookie"
	if !force && state.AccessToken != "" && !p.expiring(state.ExpiresAt, refreshSkew) {
		return state.AccessToken, nil
	}
	if !refreshable {
		if snap.AccessToken != "" && !force {
			return snap.AccessToken, nil
		}
		if snap.AccessToken != "" {
			return "", pluginapi.NewError("token_expired",
				"the static access token was rejected; provide a refresh_token or a session_cookie so it can renew itself")
		}
		return "", pluginapi.NewError("no_credentials",
			"no credentials configured: set refresh_token, session_cookie or access_token on the provider")
	}

	// single-flight
	p.mu.Lock()
	if p.refreshing {
		wait := p.refreshCh
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		p.mu.Lock()
		state = p.state
		p.mu.Unlock()
		if state.AccessToken != "" {
			return state.AccessToken, nil
		}
		return "", pluginapi.NewError("token_expired", "the token refresh failed")
	}
	p.refreshing = true
	p.refreshCh = make(chan struct{})
	done := p.refreshCh
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.refreshing = false
		close(done)
		p.mu.Unlock()
	}()

	fresh, err := p.refresh(ctx, snap, state, mode)
	if err != nil {
		p.recordError(err)
		return "", err
	}
	if err := p.saveState(fresh); err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

func (p *provider) recordError(err error) {
	p.mu.Lock()
	p.state.LastError = err.Error()
	state := p.state
	p.mu.Unlock()
	_ = p.saveState(state)
}

// refresh performs one credential exchange and returns the new state. Rotation of
// the refresh token is persisted by the caller.
func (p *provider) refresh(ctx context.Context, snap credSnapshot, state session, mode string) (session, error) {
	now := p.now()
	switch mode {
	case "refresh_token":
		refreshToken := snap.RefreshToken
		if refreshToken == "" {
			refreshToken = state.RefreshToken
		}
		clientID := snap.ClientID
		if clientID == "" {
			clientID = p.cfg.ClientID
		}
		endpoint := snap.TokenEndpoint
		if endpoint == "" {
			endpoint = p.cfg.TokenURL
		}
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		form.Set("client_id", clientID)
		form.Set("scope", "openid profile email")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return session{}, pluginapi.NewError("bad_request", "cannot build the token request")
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := p.http.Do(req)
		if err != nil {
			return session{}, pluginapi.NewRetryableError("token_endpoint_unreachable", err.Error(), 502)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 400 {
			return session{}, tokenError(resp, body)
		}
		var payload struct {
			AccessToken           string `json:"access_token"`
			RefreshToken          string `json:"refresh_token"`
			ExpiresIn             int64  `json:"expires_in"`
			RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.AccessToken == "" {
			return session{}, pluginapi.NewError("token_response_invalid", "the token endpoint did not return an access_token")
		}
		next := session{
			Mode:          mode,
			AccessToken:   payload.AccessToken,
			RefreshToken:  refreshToken,
			AccountID:     state.AccountID,
			LastRefreshAt: &now,
		}
		if payload.RefreshToken != "" {
			// Rotation: keeping the new token is what makes the next refresh work.
			next.RefreshToken = payload.RefreshToken
		}
		if payload.ExpiresIn > 0 {
			expires := now.Add(time.Duration(payload.ExpiresIn) * time.Second)
			next.ExpiresAt = &expires
		} else if exp, ok := jwtExpiry(payload.AccessToken); ok {
			next.ExpiresAt = &exp
		}
		if payload.RefreshTokenExpiresIn > 0 {
			expires := now.Add(time.Duration(payload.RefreshTokenExpiresIn) * time.Second)
			next.RefreshTokenExpiresAt = &expires
		}
		if next.AccountID == "" {
			next.AccountID = jwtAccountID(payload.AccessToken)
		}
		return next, nil
	case "session_cookie":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.SessionURL, nil)
		if err != nil {
			return session{}, pluginapi.NewError("bad_request", "cannot build the session request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Cookie", "__Secure-next-auth.session-token="+snap.SessionCookie)
		resp, err := p.http.Do(req)
		if err != nil {
			return session{}, pluginapi.NewRetryableError("session_endpoint_unreachable", err.Error(), 502)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 400 {
			return session{}, tokenError(resp, body)
		}
		var payload struct {
			AccessToken    string `json:"accessToken"`
			AccessTokenAlt string `json:"access_token"`
			Expires        string `json:"expires"`
			SessionToken   string `json:"sessionToken"`
			AuthProvider   string `json:"authProvider"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return session{}, pluginapi.NewError("session_response_invalid", "the session endpoint returned invalid JSON")
		}
		token := payload.AccessToken
		if token == "" {
			token = payload.AccessTokenAlt
		}
		if token == "" {
			// A logged-out browser session returns an empty object: say so plainly.
			return session{}, pluginapi.NewError("token_expired",
				"the session endpoint returned no access token; the session cookie has probably expired")
		}
		next := session{
			Mode:          mode,
			AccessToken:   token,
			AccountID:     state.AccountID,
			LastRefreshAt: &now,
		}
		if payload.Expires != "" {
			if parsed, err := time.Parse(time.RFC3339, payload.Expires); err == nil {
				expires := parsed.UTC()
				next.ExpiresAt = &expires
			}
		}
		if next.ExpiresAt == nil {
			if exp, ok := jwtExpiry(token); ok {
				next.ExpiresAt = &exp
			}
		}
		if next.AccountID == "" {
			next.AccountID = jwtAccountID(token)
		}
		return next, nil
	default:
		return session{}, pluginapi.NewError("no_credentials", "unsupported credential mode")
	}
}

// importTokenFile reads a CLI-style auth file once. Field names vary between tools,
// so both the flat and the nested shapes are accepted.
func importTokenFile(path string) (session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return session{}, pluginapi.NewError("token_file_unreadable", "cannot read token_file: "+filepath.Base(path))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
		Tokens       struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return session{}, pluginapi.NewError("token_file_invalid", "token_file is not valid JSON")
	}
	state := session{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		AccountID:    payload.AccountID,
	}
	if state.AccessToken == "" {
		state.AccessToken = payload.Tokens.AccessToken
	}
	if state.RefreshToken == "" {
		state.RefreshToken = payload.Tokens.RefreshToken
	}
	if state.AccountID == "" {
		state.AccountID = payload.Tokens.AccountID
	}
	if state.AccessToken == "" && state.RefreshToken == "" {
		return session{}, pluginapi.NewError("token_file_invalid", "token_file contains neither an access_token nor a refresh_token")
	}
	if exp, ok := jwtExpiry(state.AccessToken); ok {
		state.ExpiresAt = &exp
	}
	if state.AccountID == "" {
		state.AccountID = jwtAccountID(state.AccessToken)
	}
	return state, nil
}

// jwtExpiry reads exp from a JWT without verifying it: the token came from the
// upstream over TLS, and this is only used to refresh early.
func jwtExpiry(token string) (time.Time, bool) {
	claims, ok := jwtClaims(token)
	if !ok {
		return time.Time{}, false
	}
	switch value := claims["exp"].(type) {
	case float64:
		return time.Unix(int64(value), 0).UTC(), true
	case json.Number:
		seconds, err := value.Int64()
		if err == nil {
			return time.Unix(seconds, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func jwtAccountID(token string) string {
	claims, ok := jwtClaims(token)
	if !ok {
		return ""
	}
	for _, key := range []string{"chatgpt_account_id", "account_id", "sub"} {
		if value, ok := claims[key].(string); ok && strings.Contains(key, "account") {
			return value
		}
	}
	// The auth claim nests the account id under a URL-shaped key in some tokens.
	for _, key := range []string{"https://api.openai.com/auth", "auth"} {
		if nested, ok := claims[key].(map[string]any); ok {
			if value, ok := nested["chatgpt_account_id"].(string); ok {
				return value
			}
		}
	}
	return ""
}

func jwtClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

// tokenError classifies a credential endpoint failure. A rejected grant is fatal
// (retrying cannot help), while an unreachable endpoint is retryable.
func tokenError(resp *http.Response, body []byte) *pluginapi.Error {
	if resp.StatusCode >= 500 {
		return pluginapi.NewRetryableError("token_endpoint_error", "the token endpoint returned "+resp.Status, 502)
	}
	code := upstreamErrorCode(body)
	if code == "" {
		code = "invalid_grant"
	}
	return pluginapi.NewError("token_expired", "credential refresh rejected ("+code+"); sign in again and update the stored credentials")
}

func upstreamErrorCode(body []byte) string {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Error) == 0 {
		return ""
	}
	var asObject struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload.Error, &asObject); err == nil {
		if asObject.Code != "" {
			return asObject.Code
		}
		if asObject.Type != "" {
			return asObject.Type
		}
	}
	var asString string
	if err := json.Unmarshal(payload.Error, &asString); err == nil {
		return asString
	}
	return ""
}

// ---------------------------------------------------------------------------
// requests and event translation
// ---------------------------------------------------------------------------

// responsesRequest is the upstream request body. Only documented Responses fields
// are sent: store is false by default because this backend refuses stored responses.
// max_output_tokens is absent on purpose — this backend rejects it (see buildRequest).
type responsesRequest struct {
	PromptCacheKey string               `json:"prompt_cache_key,omitempty"`
	Model          string               `json:"model"`
	Instructions   string               `json:"instructions,omitempty"`
	Input          []pluginapi.Item     `json:"input,omitempty"`
	Tools          []pluginapi.Tool     `json:"tools,omitempty"`
	ToolChoice     json.RawMessage      `json:"tool_choice,omitempty"`
	Reasoning      *pluginapi.Reasoning `json:"reasoning,omitempty"`
	Store          bool                 `json:"store"`
	Stream         bool                 `json:"stream"`
}

type wireUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	InputDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

// wireEvent is the subset of upstream events this adapter understands. Unknown
// event types are ignored so an upstream addition cannot break the stream.
type wireEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	Text        string          `json:"text"`
	ItemID      string          `json:"item_id"`
	OutputIndex int             `json:"output_index"`
	Arguments   string          `json:"arguments"`
	Item        *pluginapi.Item `json:"item"`
	Response    *wireResponse   `json:"response"`
	Error       *wireError      `json:"error"`
}

type wireResponse struct {
	Status            string     `json:"status"`
	Usage             *wireUsage `json:"usage"`
	Error             *wireError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// rewriteSystemRoles renames input items whose role is "system" to "developer".
//
// This backend rejects a system message outright ("System messages are not allowed"),
// while clients legitimately send their system prompt as one — the Responses API allows
// that shape, so the translation belongs here rather than in every client. "developer"
// is the same role under its modern name and is accepted in every position (first,
// middle, last, alongside tools).
//
// Renaming rather than folding the text into instructions keeps the message at its
// original position, needs no content parsing, and cannot leave input empty (this
// backend also rejects an empty input).
//
// The caller's slice is never mutated: the gateway may reuse the request.
func rewriteSystemRoles(items []pluginapi.Item) []pluginapi.Item {
	changed := false
	for _, item := range items {
		if item.Role == "system" {
			changed = true
			break
		}
	}
	if !changed {
		return items
	}
	out := append([]pluginapi.Item(nil), items...)
	for i := range out {
		if out[i].Role == "system" {
			out[i].Role = "developer"
		}
	}
	return out
}

// normalizeInputItems rewrites input items into the shapes this backend accepts.
//
// Two of its input rules collide with what clients legitimately send, because the gateway's
// own output items carry exactly the fields this backend refuses on input:
//
//   - a `reasoning` item must carry `summary`, and an empty array is the right answer when
//     the model produced no summary text. A client that never saw the key, or that lost the
//     empty array on the way through, otherwise gets a hard
//     "Missing required parameter: 'input[N].summary'" — and since the item stays in the
//     history, every later request of that session fails the same way;
//   - a `reasoning` item must NOT carry `content` (measured: "array too long. Expected an
//     array with maximum length 0") nor `status` ("Unknown parameter: 'input[N].status'").
//     Both are output-only fields that the gateway returned with the item, so replaying what
//     the client received must not be a way to make a request invalid.
//
// This is the same kind of translation as rewriteSystemRoles: the backend's input dialect is
// this adapter's business, not every client's.
//
// The caller's slice is never mutated: the gateway may reuse the request.
func normalizeInputItems(items []pluginapi.Item) []pluginapi.Item {
	var out []pluginapi.Item
	for i, item := range items {
		if !needsInputNormalization(item) {
			continue
		}
		if out == nil {
			out = append([]pluginapi.Item(nil), items...)
		}
		// status is an output-only field on every item type this backend validates.
		out[i].Status = ""
		if item.Type != "reasoning" {
			continue
		}
		if out[i].Summary == nil {
			out[i].Summary = []pluginapi.SummaryPart{}
		}
		out[i].Content = nil
	}
	if out == nil {
		return items
	}
	return out
}

// needsInputNormalization reports whether one item carries something this backend refuses
// on input: an output-only status on any type, or a reasoning item missing the required
// summary key / carrying the forbidden content array.
func needsInputNormalization(item pluginapi.Item) bool {
	if item.Status != "" {
		return true
	}
	return item.Type == "reasoning" && (item.Summary == nil || len(item.Content) != 0)
}

func (p *provider) buildRequest(req *pluginapi.Request, stream bool) ([]byte, error) {
	if req == nil {
		return nil, pluginapi.NewError("bad_request", "provider-codex: nil request")
	}
	model := req.Model
	for _, configured := range p.cfg.Models {
		if configured.ID == req.Model && configured.UpstreamModel != "" {
			model = configured.UpstreamModel
			break
		}
	}
	wire := responsesRequest{
		PromptCacheKey: req.PromptCacheKey,
		Model:          model,
		Instructions:   req.Instructions,
		Input:          normalizeInputItems(rewriteSystemRoles(req.Input)),
		Tools:          explicitToolStrictness(req.Tools),
		ToolChoice:     req.ToolChoice,
		Store:          p.cfg.Store,
		Stream:         stream,
	}
	// req.MaxOutputTokens is deliberately NOT forwarded: this backend rejects the
	// parameter outright ("Unsupported parameter: max_output_tokens", measured for
	// every value), so passing a client's cap through turned a normal request into a
	// hard 400. The gateway still accounts for an in-flight reservation from its own
	// billing.configuration; the client's cap simply does not reach the upstream.
	if req.Reasoning != nil {
		wire.Reasoning = req.Reasoning
	} else if p.cfg.ReasoningEffort != "" {
		wire.Reasoning = &pluginapi.Reasoning{Effort: p.cfg.ReasoningEffort}
	}
	return json.Marshal(wire)
}

// The subscription backend normalizes schemas to strict mode when strict is
// absent, making even optional arguments required. Clients such as DSH omit
// strict for generic Responses endpoints; preserve their optional arguments by
// explicitly opting out, while respecting an explicit strict request.
func explicitToolStrictness(tools []pluginapi.Tool) []pluginapi.Tool {
	var out []pluginapi.Tool
	for i, tool := range tools {
		if (tool.Type != "function" && tool.Type != "") || len(tool.Raw) != 0 || tool.Strict != nil {
			continue
		}
		if out == nil {
			out = append([]pluginapi.Tool(nil), tools...)
		}
		strict := false
		out[i].Strict = &strict
	}
	if out == nil {
		return tools
	}
	return out
}

func (p *provider) doRequest(ctx context.Context, body []byte, token string, state session, snap credSnapshot, acceptStream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/responses", strings.NewReader(string(body)))
	if err != nil {
		return nil, pluginapi.NewError("bad_request", "provider-codex: cannot build the upstream request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if acceptStream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if account := p.resolveAccountID(state, snap); account != "" {
		req.Header.Set("chatgpt-account-id", account)
	}
	for key, value := range p.cfg.Headers {
		req.Header.Set(key, value)
	}
	return p.http.Do(req)
}

func (p *provider) resolveAccountID(state session, snap credSnapshot) string {
	if p.cfg.AccountID != "" {
		return p.cfg.AccountID
	}
	if snap.AccountID != "" {
		return snap.AccountID
	}
	if state.AccountID != "" {
		return state.AccountID
	}
	return ""
}

// Stream forwards events immediately and retries a broken upstream stream once
// only before the first event is delivered. Never replay visible output or tools.
func (p *provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	for attempt := 0; ; attempt++ {
		delivered := false
		err := p.streamAttempt(ctx, req, func(event pluginapi.Event) error {
			delivered = true // A consumer error must never trigger a replay.
			return emit(event)
		})
		apiErr, ok := pluginapi.IsError(err)
		if err == nil || attempt >= 1 || delivered || ctx.Err() != nil || !ok ||
			(apiErr.Code != "stream_read_failed" && apiErr.Code != "upstream_stream_incomplete") {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// streamAttempt refreshes the token once if rejected, and translates upstream events.
func (p *provider) streamAttempt(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	// Fail an unusable proxy setting as fatal before any token or network work.
	// Without this the transport error would be classified retryable and the
	// router would waste attempts failing over on a configuration typo.
	if err := p.proxyError(); err != nil {
		return err
	}
	body, err := p.buildRequest(req, true)
	if err != nil {
		return err
	}
	token, err := p.ensureToken(ctx, false)
	if err != nil {
		return err
	}
	state, _ := p.currentState()
	snap := p.snapshot()

	resp, err := p.doRequest(ctx, body, token, state, snap, true)
	if err != nil {
		return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 502)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		// One refresh-and-retry: a token that died between requests is recoverable,
		// a second rejection means the credentials themselves are gone.
		if _, refreshErr := p.ensureToken(ctx, true); refreshErr != nil {
			return refreshErr
		}
		state, _ = p.currentState()
		snap = p.snapshot()
		resp, err = p.doRequest(ctx, body, state.AccessToken, state, snap, true)
		if err != nil {
			return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 502)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return classifyResponse(resp, payload)
	}

	reader := providerkit.NewSSEReader(resp.Body, 0)
	// finishReason is what the upstream said about the end of the answer; terminal
	// records whether it said anything at all. A stream that ends without a
	// terminal event (response.completed / response.incomplete) was cut off, and a
	// fragment must not be forwarded as a complete answer.
	finishReason := ""
	terminal := false
	lastEvent := "none"
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return pluginapi.NewRetryableError("stream_read_failed", fmt.Sprintf(
				"provider-codex: reading the upstream stream failed: %v (http=%q upstream_request_id=%q last_event=%q)",
				err, resp.Proto, resp.Header.Get("x-request-id"), lastEvent), 502)
		}
		if event.Name == "done" {
			break
		}
		if len(event.Data) == 0 {
			continue
		}
		var payload wireEvent
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			continue // a data line the adapter does not model
		}
		lastEvent = payload.Type
		if reason, ok := terminalReason(payload); ok {
			terminal = true
			if reason != "" {
				finishReason = reason
			}
		}
		if err := p.translate(payload, emit); err != nil {
			return err
		}
		// A terminal event completes the protocol; the HTTP body may stay open
		// or end with a transport error after the answer has already finished.
		if terminal {
			break
		}
	}
	if !terminal {
		return pluginapi.NewRetryableError("upstream_stream_incomplete",
			"provider-codex: the upstream stream ended before the response was finished", 502)
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	return emit(pluginapi.Event{Type: pluginapi.EventFinish, Reason: finishReason})
}

// terminalReason reports whether one upstream event ends the answer, and why. The
// subscription backend states it with response.completed / response.incomplete;
// an incomplete response carries the reason the model stopped early
// (max_output_tokens, content_filter, ...).
func terminalReason(event wireEvent) (string, bool) {
	switch event.Type {
	case "response.completed":
		return "stop", true
	case "response.incomplete":
		if event.Response != nil && event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason != "" {
			return event.Response.IncompleteDetails.Reason, true
		}
		return "incomplete", true
	case "response.failed":
		// Handled as an error by translate; the stream is over either way.
		return "", false
	default:
		return "", false
	}
}

// translate maps one upstream event onto zero or more plugin events.
func (p *provider) translate(event wireEvent, emit func(pluginapi.Event) error) error {
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta == "" {
			return nil
		}
		return emit(pluginapi.Event{
			Type: pluginapi.EventTextDelta, Index: event.OutputIndex,
			ItemID: event.ItemID, Text: event.Delta,
		})
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta == "" {
			return nil
		}
		return emit(pluginapi.Event{
			Type: pluginapi.EventReasoningDelta, Index: event.OutputIndex,
			ItemID: event.ItemID, Text: event.Delta,
		})
	case "response.output_item.added":
		if event.Item == nil || event.Item.Type != "function_call" {
			return nil
		}
		return emit(pluginapi.Event{
			Type: pluginapi.EventToolCallStart, Index: event.OutputIndex,
			ItemID: event.Item.ID, CallID: event.Item.CallID, Name: event.Item.Name,
		})
	case "response.output_item.done":
		// Newer Codex clients use custom tools as well as function tools. Keep
		// their complete items intact; ignoring them silently ends the agent turn.
		// The three types below are already emitted through the delta path.
		if event.Item != nil && event.Item.Type != "message" && event.Item.Type != "reasoning" && event.Item.Type != "function_call" {
			return emit(pluginapi.Event{Type: pluginapi.EventOutputItemDone, Item: event.Item})
		}
		return nil
	case "response.function_call_arguments.delta":
		if event.Delta == "" {
			return nil
		}
		return emit(pluginapi.Event{
			Type: pluginapi.EventToolArgsDelta, Index: event.OutputIndex,
			ItemID: event.ItemID, Text: event.Delta,
		})
	case "response.completed", "response.incomplete":
		if event.Response == nil || event.Response.Usage == nil {
			return nil
		}
		return emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: usageFromWire(event.Response.Usage)})
	case "response.failed":
		message := "the upstream reported a failed response"
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
			message = event.Response.Error.Message
		}
		return pluginapi.NewError("upstream_failed", message)
	case "error":
		message := "the upstream reported an error"
		code := "upstream_error"
		if event.Error != nil {
			if event.Error.Message != "" {
				message = event.Error.Message
			}
			if event.Error.Code != "" {
				code = event.Error.Code
			}
		}
		return pluginapi.NewRetryableError(code, message, 502)
	default:
		return nil
	}
}

// usageFromWire maps upstream usage onto the gateway's dimensions. Cache hits are
// split out because they are priced differently, and reasoning tokens are reported
// separately so the pricing engine can decide how to treat them.
func usageFromWire(usage *wireUsage) *pluginapi.Usage {
	dims := map[string]int64{}
	if usage == nil {
		return &pluginapi.Usage{Dimensions: dims, Estimated: true}
	}
	cached := int64(0)
	if usage.InputDetails != nil {
		cached = usage.InputDetails.CachedTokens
	}
	switch {
	case cached > 0 && cached < usage.InputTokens:
		dims["input_cache_hit"] = cached
		dims["input_cache_miss"] = usage.InputTokens - cached
	case cached > 0:
		dims["input_cache_hit"] = usage.InputTokens
	default:
		dims["input"] = usage.InputTokens
	}
	reasoning := int64(0)
	if usage.OutputDetails != nil {
		reasoning = usage.OutputDetails.ReasoningTokens
	}
	if reasoning > 0 {
		dims["reasoning"] = reasoning
	}
	output := usage.OutputTokens
	if reasoning > 0 && reasoning <= output {
		output -= reasoning
	}
	if output > 0 {
		dims["output"] = output
	}
	return &pluginapi.Usage{Dimensions: dims}
}

// Complete assembles a non-streaming response from the same stream, so there is
// exactly one place where upstream events are interpreted.
func (p *provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	var (
		builder      strings.Builder
		finalUsage   *pluginapi.Usage
		finishReason string
		items        []pluginapi.Item
	)
	flushText := func(status string) {
		if builder.Len() == 0 {
			return
		}
		items = append(items, pluginapi.Item{Type: "message", ID: fmt.Sprintf("msg_codex_%d", len(items)), Role: "assistant", Content: outputText(builder.String()), Status: status})
		builder.Reset()
	}
	var outputChars strings.Builder
	err := p.Stream(ctx, req, func(event pluginapi.Event) error {
		switch event.Type {
		case pluginapi.EventOutputItemDone:
			if event.Item != nil {
				flushText("completed")
				items = append(items, *event.Item)
			}
		case pluginapi.EventTextDelta:
			builder.WriteString(event.Text)
			outputChars.WriteString(event.Text)
		case pluginapi.EventUsage:
			if event.Usage != nil {
				finalUsage = event.Usage
			}
		case pluginapi.EventFinish:
			finishReason = event.Reason
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if finalUsage == nil {
		finalUsage = &pluginapi.Usage{Dimensions: map[string]int64{"output": providerkit.EstimateTokens(outputChars.String(), 0)}, Estimated: true}
	}
	status := "completed"
	if _, truncated := pluginapi.IncompleteReason(finishReason); truncated {
		status = "incomplete"
	}
	flushText(status)
	return &pluginapi.Response{Items: items, Usage: *finalUsage, Status: status, FinishReason: finishReason}, nil
}

// Health probes by issuing a real streaming completion, not by poking a status
// endpoint. Two reasons, both learned the hard way:
//
//   - This backend has no trustworthy liveness endpoint. The /me style probe this
//     replaced was answered by a Cloudflare challenge (403 + cf-mitigated), which
//     the classifier reported as "credentials rejected" while the credentials were
//     in fact fine — a permanently red light on a working provider.
//   - A real completion exercises the path that actually matters: proxy, token
//     refresh, model acceptance, streaming and event translation.
//
// The probe is healthy only when the request succeeded AND the stream reached its
// terminal usage event. Stream already fails a body that ends without the upstream
// declaring the end of the answer; the probe keeps its own check so a plugin whose
// Stream behaves differently cannot report a truncated body as a healthy provider.
func (p *provider) Health(ctx context.Context) error {
	if err := p.proxyError(); err != nil {
		return err
	}
	model, err := p.healthModel()
	if err != nil {
		return err
	}
	prompt := p.cfg.HealthPrompt
	if prompt == "" {
		prompt = defaultHealthPrompt
	}
	probe := &pluginapi.Request{Model: model, Input: []pluginapi.Item{healthInput(prompt)}}

	sawTerminal := false
	if err := p.Stream(ctx, probe, func(event pluginapi.Event) error {
		if event.Type == pluginapi.EventUsage {
			sawTerminal = true
		}
		return nil
	}); err != nil {
		// A cut stream is exactly what this probe exists to catch, and the diagnosis
		// belongs in the health report: the transport error behind it is retryable,
		// which would leave the console showing a transient blip instead of the
		// reason the provider cannot be trusted.
		if apiErr, ok := pluginapi.IsError(err); ok && apiErr.Code == "upstream_stream_incomplete" {
			return pluginapi.NewError("health_stream_incomplete",
				"provider-codex: the health stream ended before the response was finished")
		}
		return err
	}
	if !sawTerminal {
		return pluginapi.NewError("health_stream_incomplete",
			"provider-codex: the health stream ended without a terminal usage event")
	}
	return nil
}

// healthModel resolves the model the probe asks for: health_model when set,
// otherwise the first configured model. buildRequest maps a configured ID onto its
// upstream model, so passing the configured ID is correct here.
func (p *provider) healthModel() (string, error) {
	if model := strings.TrimSpace(p.cfg.HealthModel); model != "" {
		return model, nil
	}
	if len(p.cfg.Models) == 0 {
		return "", pluginapi.NewError("health_unconfigured",
			"provider-codex: no models are configured, so the health probe has nothing to call; add a model or set health_model")
	}
	return p.cfg.Models[0].ID, nil
}

// healthInput builds the probe prompt as a minimal user message.
func healthInput(prompt string) pluginapi.Item {
	content, _ := json.Marshal([]map[string]string{{"type": "input_text", "text": prompt}})
	return pluginapi.Item{Type: "message", Role: "user", Content: content}
}

// classifyResponse turns an HTTP failure into the plugin error kinds the router
// understands. Messages carry the upstream code but never request headers.
func classifyResponse(resp *http.Response, body []byte) *pluginapi.Error {
	message := upstreamMessage(body)
	if message == "" {
		message = "the upstream returned " + resp.Status
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return pluginapi.NewQuotaError(message, retryAfterUnix(resp))
	case isCloudflareChallenge(resp, body):
		// A challenge means the request never reached the model. Calling it
		// "credentials rejected" sends the operator off to re-mint tokens that are
		// perfectly fine, so it gets its own retryable code: another egress may pass.
		return pluginapi.NewRetryableError("upstream_challenge",
			"the upstream answered with a Cloudflare challenge, so the egress IP is probably blocked: "+resp.Status, resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return pluginapi.NewError("token_expired", message)
	case resp.StatusCode >= 500:
		return pluginapi.NewRetryableError("upstream_5xx", message, resp.StatusCode)
	default:
		code := upstreamErrorCode(body)
		if code == "" {
			code = "upstream_error"
		}
		if apiErr := pluginapi.NewError(code, message); apiErr != nil {
			apiErr.HTTPStatus = resp.StatusCode
			return apiErr
		}
	}
	return pluginapi.NewError("upstream_error", message)
}

// isCloudflareChallenge reports whether a rejection came from Cloudflare's bot
// challenge rather than from the API. The header is the reliable marker; the HTML
// body check is the fallback for intermediaries that strip it.
func isCloudflareChallenge(resp *http.Response, body []byte) bool {
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return false
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("cf-mitigated")), "challenge") {
		return true
	}
	head := strings.ToLower(strings.TrimSpace(string(body)))
	if len(head) > 64 {
		head = head[:64]
	}
	return strings.HasPrefix(head, "<html") || strings.HasPrefix(head, "<!doctype html")
}

func upstreamMessage(body []byte) string {
	// Upstream error envelopes come in more than one shape and all of them have
	// been observed in the wild: {"error":{"message":…}}, {"message":…} and the
	// FastAPI style {"detail":…}. Missing the last one turned "the model is not
	// supported" into an uninformative "upstream returned 400".
	var payload struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if len(payload.Error) > 0 {
		var asObject struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload.Error, &asObject); err == nil && asObject.Message != "" {
			return asObject.Message
		}
	}
	if payload.Detail != "" {
		return payload.Detail
	}
	return payload.Message
}

func retryAfterUnix(resp *http.Response) int64 {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(raw + "s"); err == nil {
		return time.Now().Add(seconds).Unix()
	}
	if at, err := http.ParseTime(raw); err == nil {
		return at.Unix()
	}
	return 0
}

// describe reports session metadata; secrets are never included, only their presence.
func (p *provider) describe(state session) map[string]any {
	snap := p.snapshot()
	payload := map[string]any{
		"mode":               state.Mode,
		"account_id":         p.resolveAccountID(state, snap),
		"has_access_token":   state.AccessToken != "" || snap.AccessToken != "",
		"has_refresh_token":  state.RefreshToken != "" || snap.RefreshToken != "",
		"has_session_cookie": snap.SessionCookie != "",
		"last_error":         state.LastError,
	}
	if state.ExpiresAt != nil {
		payload["expires_at"] = state.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if state.RefreshTokenExpiresAt != nil {
		payload["refresh_token_expires_at"] = state.RefreshTokenExpiresAt.UTC().Format(time.RFC3339)
	}
	if state.LastRefreshAt != nil {
		payload["last_refresh_at"] = state.LastRefreshAt.UTC().Format(time.RFC3339)
	}
	// The effective egress proxy, masked: an operator needs to confirm the setting
	// took effect, but the URL may carry credentials.
	proxyValue, proxySource := "", "env"
	if s := p.proxy.Load(); s != nil {
		proxySource = s.source
		switch {
		case s.err != nil:
			proxyValue = "invalid"
		case s.url != nil:
			proxyValue = providerkit.MaskProxyURL(s.url)
		}
	}
	payload["proxy"] = proxyValue
	payload["proxy_source"] = proxySource
	return payload
}

func outputText(text string) json.RawMessage {
	parts := []map[string]string{{"type": "output_text", "text": text}}
	raw, err := json.Marshal(parts)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// Package proxy implements the portal, edge-origin fence, session mapping, and
// streaming reverse proxy in front of per-tenant dsh workers.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/feishu"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

const maxReplayBody = 64 << 20

type KeyValidator interface {
	ValidateKey(context.Context, string) ([]aigw.Model, error)
}

// DSHAuthorizer is the account-level entitlement check (M52): aigw answers whether the
// key's account is opted in to the dsh gateway and which tenant the account uses. An
// empty tenant name means the aigw side has no mapping yet and the caller falls back to
// legacy prefix binding. aigw.Client implements it.
type DSHAuthorizer interface {
	Authorize(context.Context, string) (string, error)
}

// LoginPrepare is the login moment's lifecycle hook (M69).
//
// Every successful login re-applies the platform's slice of a tenant's dsh configuration —
// the model list its own worker key is granted on aigw, and the credential reference that
// provider reads — and makes sure the tenant's worker is up, because signing out stops it.
// It is implemented by the lifecycle layer: the proxy decides identity, it does not write
// tenant state itself.
//
// submittedKey is the key this login presented, or "" for a login that carries none (a Feishu
// ticket). It is only ever adopted when the tenant has no key at all; the platform slice itself
// always comes from the tenant's stored worker key.
type LoginPrepare interface {
	PrepareLogin(ctx context.Context, tenant, submittedKey string) error
}

// LogoutStop stops a tenant's dsh once its last session has signed out (M69). The proxy decides
// *when* (it owns the session store, so it knows whether another window is still signed in); the
// lifecycle layer decides *how*.
type LogoutStop interface {
	StopSignedOut(ctx context.Context, tenant string) error
}

type Proxy struct {
	Config            *config.Config
	Registry          *registry.Registry
	Sessions          session.Store
	HandshakeSource   handshake.Source
	Exchanger         handshake.Exchanger
	Validator         KeyValidator
	Authorizer        DSHAuthorizer
	KeySource         KeySource
	LoginPrepare      LoginPrepare
	LogoutStop        LogoutStop
	Transport         http.RoundTripper
	BrowserWorkspaces interface {
		ServeTenant(http.ResponseWriter, *http.Request, registry.Tenant, string)
	}
	// Feishu carries the identity handoff from aigw (M61), or nil when the feature is off.
	Feishu   *FeishuPortal
	Logger   *slog.Logger
	Auditor  audit.Sink
	Activity activity.Recorder
	Now      func() time.Time

	exchangeLocks     [256]sync.Mutex
	revalidationLocks [256]sync.Mutex
	rateMu            sync.Mutex
	rates             map[string]*rateBucket
	revalidateMu      sync.Mutex
	revalidations     map[string]revalidation
	dshMu             sync.Mutex
	dshChecks         map[string]revalidation
	// identities caches who each tenant's sidebar shows (M67); see identity.go.
	identityMu sync.Mutex
	identities map[string]tenantIdentity
	reloadMu   sync.Mutex
	lastReload time.Time
}
type rateBucket struct {
	Start time.Time
	Count int
}
type revalidation struct {
	Checked time.Time
	Valid   bool
	Err     error
}

func New(cfg *config.Config, reg *registry.Registry, sessions session.Store, source handshake.Source, exchanger handshake.Exchanger, validator KeyValidator) *Proxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	p := &Proxy{Config: cfg, Registry: reg, Sessions: sessions, HandshakeSource: source, Exchanger: exchanger, Validator: validator, Transport: transport, rates: map[string]*rateBucket{}, revalidations: map[string]revalidation{}, dshChecks: map[string]revalidation{}, identities: map[string]tenantIdentity{}}
	cfg.SetTenantPorts(reg.TenantPorts())
	return p
}
func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
func (p *Proxy) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
func (p *Proxy) audit(r *http.Request, tenant, kind, reason string, status int) {
	if p.Auditor == nil {
		return
	}
	remote := requestIP(r)
	event := audit.Event{Time: p.now(), Kind: kind, Tenant: tenant, RemoteIP: remote, Method: r.Method, Path: r.URL.Path, Origin: safeOrigins(r.Header.Values("Origin")), Reason: reason, Status: status}
	if err := p.Auditor.Write(event); err != nil {
		p.log().Error("write dshgw security audit failed", "err", err)
	}
}

func requestIP(r *http.Request) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	remoteIP := net.ParseIP(remote)
	if forwarded := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); forwarded != nil && remoteIP != nil && remoteIP.IsLoopback() {
		return forwarded.String()
	}
	if remoteIP != nil {
		return remoteIP.String()
	}
	return "unknown"
}

func safeOrigins(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		u, err := url.Parse(value)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
			out = append(out, "[invalid]")
			continue
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return out
}

func (p *Proxy) baseTransport() http.RoundTripper {
	if p.Transport != nil {
		return p.Transport
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

// Dispatch selects the portal or tenant solely from the configured public Host
// authority. Unknown hosts and ports deliberately look like a normal 404.
func (p *Proxy) Dispatch() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := p.reloadRegistry(); err != nil {
			http.Error(w, "tenant registry unavailable", http.StatusServiceUnavailable)
			return
		}
		host, port, ok := splitAuthority(r.Host)
		if !ok || !strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(p.Config.PublicHost, ".")) {
			p.audit(r, "", "edge_reject", "host authority mismatch", http.StatusNotFound)
			http.NotFound(w, r)
			return
		}
		if p.Config.EdgePortHeader != "" {
			values := r.Header.Values(p.Config.EdgePortHeader)
			edgePort, edgeErr := strconv.Atoi(strings.TrimSpace(strings.Join(values, "")))
			if len(values) != 1 || edgeErr != nil || edgePort != port {
				p.audit(r, "", "edge_reject", "trusted edge port mismatch", http.StatusNotFound)
				http.NotFound(w, r)
				return
			}
		}
		if _, err := validateTarget(r); err != nil {
			p.audit(r, "", "target_reject", err.Error(), http.StatusBadRequest)
			http.Error(w, "bad request target", http.StatusBadRequest)
			return
		}
		if port == p.Config.PortalPort {
			p.PortalHandler().ServeHTTP(w, r)
			return
		}
		tenant, ok := p.Registry.ByPublicPort(port)
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.TenantHandler(tenant).ServeHTTP(w, r)
	})
}
func (p *Proxy) reloadRegistry() error {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	now := p.now()
	if now.Sub(p.lastReload) < time.Second {
		return nil
	}
	if err := p.Registry.Reload(); err != nil {
		p.log().Error("reload dshgw registry failed", "error_type", fmt.Sprintf("%T", err))
		return err
	}
	ports := p.Registry.TenantPorts()
	p.Config.SetTenantPorts(ports)
	p.revalidateMu.Lock()
	for tenant := range p.revalidations {
		if _, exists := ports[tenant]; !exists {
			delete(p.revalidations, tenant)
		}
	}
	p.revalidateMu.Unlock()
	p.forgetIdentities(ports)
	p.lastReload = now
	return nil
}
func splitAuthority(authority string) (string, int, bool) {
	host, raw, err := net.SplitHostPort(authority)
	if err != nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

func (p *Proxy) PortalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.setPortalHeaders(w)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			p.renderLogin(w, http.StatusOK, "")
		case r.Method == http.MethodGet && r.URL.Path == "/login/feishu":
			// The identity handoff from aigw (M61). It carries a short-lived ticket rather
			// than credentials, so it needs no form and no session.
			p.feishuLogin(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/feishu/error":
			p.renderLogin(w, http.StatusUnauthorized, feishuErrorMessage(r.URL.Query().Get("reason")))
		case r.Method == http.MethodPost && r.URL.Path == "/login":
			p.login(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/logout":
			p.logout(w, r)
		case r.URL.Path == "/logout":
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	})
}
func (p *Proxy) setPortalHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	formAction := []string{"'self'"}
	for _, t := range p.Registry.List() {
		formAction = append(formAction, p.Config.TenantOrigin(t.Name))
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+strings.Join(formAction, " ")+"; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>dsh 登录</title><style>body{font:16px system-ui;max-width:34rem;margin:10vh auto;padding:1rem;background:#101318;color:#eef}main{background:#1b2028;padding:2rem;border-radius:12px}input,button{box-sizing:border-box;width:100%;padding:.8rem;margin:.4rem 0}button{cursor:pointer}a.feishu{display:block;box-sizing:border-box;width:100%;padding:.8rem;margin:.4rem 0;text-align:center;background:#3370ff;color:#fff;border-radius:6px;text-decoration:none}.error{color:#ff9b9b}.note{color:#bcc6d6;font-size:.9rem}</style></head><body><main><h1>DeepSeek Harness</h1>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}{{if .FeishuLoginURL}}<p><a class="feishu" href="{{.FeishuLoginURL}}">飞书登录</a></p><p class="note">用飞书登录的账号由管理员在 aigw 控制台绑定；未绑定时请先用下面的 API Key 登录或联系管理员。</p>{{end}}<form method="post" action="{{.PortalPath}}login"><label>aigw API Key<input type="password" name="key" autocomplete="off" spellcheck="false" required></label><button type="submit">登录</button></form><form method="post" action="{{.PortalPath}}logout"><button type="submit">退出此浏览器的全部租户会话</button></form><p class="note">Key 只用于向 aigw 验证身份；browser-fs 默认开启后，只有你在浏览器明确授权的本机目录可被 agent 访问，内容可能进入模型请求。</p></main></body></html>`))

func (p *Proxy) renderLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// The form action is built from the portal path so the same page works behind a
	// path prefix (single-domain mode) and on a portal port (default mode).
	_ = loginPage.Execute(w, struct {
		Error          string
		PortalPath     string
		FeishuLoginURL string
	}{message, p.Config.PortalPath(), p.feishuLoginURL()})
}

func (p *Proxy) login(w http.ResponseWriter, r *http.Request) {
	if err := p.checkEdgeOrigin(r, p.Config.ExpectedOrigin(p.Config.PortalPort), false); err != nil {
		p.audit(r, "", "login_reject", err.Error(), http.StatusForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ip := requestIP(r)
	if !p.allowLogin(ip) {
		p.audit(r, "", "login_reject", "rate limit", http.StatusTooManyRequests)
		w.Header().Set("Retry-After", strconv.Itoa(int(p.Config.LoginRate.Window.Duration().Seconds())))
		p.renderLogin(w, http.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		p.renderLogin(w, http.StatusBadRequest, "请求格式无效")
		return
	}
	keys := r.PostForm["key"]
	if len(keys) != 1 || r.URL.Query().Has("key") {
		p.renderLogin(w, http.StatusBadRequest, "请仅在表单中提交一个 Key")
		return
	}
	key, normalizeErr := aigw.NormalizeKey(keys[0])
	if normalizeErr != nil {
		p.renderLogin(w, http.StatusUnauthorized, "Key 无效或已停用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.Config.ValidateTimeout.Duration())
	defer cancel()
	models, err := p.Validator.ValidateKey(ctx, key)
	if errors.Is(err, aigw.ErrInvalidKey) {
		p.audit(r, "", "login_reject", "invalid key", http.StatusUnauthorized)
		p.renderLogin(w, http.StatusUnauthorized, "Key 无效或已停用")
		return
	}
	if err != nil {
		p.log().Warn("aigw key validation failed", "err", err, "source", ip)
		p.renderLogin(w, http.StatusServiceUnavailable, "认证服务暂不可用")
		return
	}
	// The account-level entitlement check (M52) runs after the key itself validated.
	// An enabled account answers its tenant name, so EVERY key of the account — existing
	// and newly created — logs into that tenant without per-key prefix binding. A denial
	// is a 403 with an accurate message, never a silent success and never confused with
	// "aigw is down" (which stays a 503).
	authTenant, err := p.authorizeDSH(ctx, key)
	if err != nil {
		var denial *aigw.DSHDenial
		switch {
		case errors.As(err, &denial):
			reason, message := "dsh disabled", "该账号未启用 dsh"
			if denial.Reason == "account_status" {
				reason, message = "account suspended", "账号已停用，无法登录 dsh"
			}
			p.audit(r, "", "login_reject", reason, http.StatusForbidden)
			p.renderLogin(w, http.StatusForbidden, message)
		case errors.Is(err, aigw.ErrInvalidKey):
			p.renderLogin(w, http.StatusUnauthorized, "Key 无效或已停用")
		default:
			p.log().Warn("dsh authorization check failed", "err", err)
			p.renderLogin(w, http.StatusServiceUnavailable, "认证服务暂不可用")
		}
		return
	}
	tenant, resolved := p.resolveTenant(authTenant, key)
	if !resolved {
		p.audit(r, "", "login_reject", "unbound key prefix", http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, "该账号的 dsh 租户尚未就绪，请联系管理员启用或检查租户状态")
		return
	}
	// The login moment owns the tenant's lifecycle (M69): the platform slice of its dsh
	// configuration is re-applied from aigw on every sign-in, and the worker — stopped when the
	// last session signed out — is brought back up before the browser is sent to it.
	p.prepareLogin(r, tenant, key)
	token, err := p.Sessions.Issue(tenant.Name, p.Config.SessionTTL.Duration())
	if err != nil {
		p.log().Error("issue dshgw session failed", "err", err)
		p.renderLogin(w, http.StatusInternalServerError, "无法创建会话")
		return
	}
	// Resolve who just signed in before sending them on: the tenant's sidebar asks for it as
	// soon as its page loads, which is one redirect away (M67). Best effort by design — the
	// login must not fail because a display name could not be looked up.
	p.identity(r.Context(), tenant)
	p.setSessionCookie(w, tenant.Name, token, false)
	p.audit(r, tenant.Name, "login_success", "authenticated", http.StatusFound)
	if p.Activity != nil {
		if err := p.Activity.MarkLogin(tenant.Name, p.now()); err != nil {
			p.log().Error("persist login activity failed", "tenant", tenant.Name, "err", err)
		}
	}
	if len(models) == 0 {
		w.Header().Set("X-DSHGW-Warning", "valid key has no currently available models")
	}
	http.Redirect(w, r, p.Config.WithTrailingSlash(p.Config.TenantOrigin(tenant.Name)), http.StatusFound)
}

// loginPrepareTimeout bounds the login-time lifecycle work: an aigw model refresh plus, on a
// cold tenant, a worker start and its readiness probe. Generous on purpose — the alternative is
// sending the browser to a worker that is not up yet, which is a 502 the person cannot act on —
// but bounded, so a wedged worker cannot hold the login request open forever.
const loginPrepareTimeout = 45 * time.Second

// logoutStopTimeout bounds the logout-time worker stop. The runner signals TERM and escalates to
// KILL, so this only has to cover a worker that is slow to die.
const logoutStopTimeout = 30 * time.Second

// prepareLogin runs the login-time lifecycle hook (M69).
//
// It is fail-soft on purpose: the person has already proved who they are, so a configuration
// refresh or a cold start that did not work out must not turn into a refused login. The failure
// is logged and audited; the tenant's page will report the worker's absence.
func (p *Proxy) prepareLogin(r *http.Request, tenant registry.Tenant, submittedKey string) {
	if p.LoginPrepare == nil {
		return
	}
	// The request context dies with the response, and this work deliberately outlives the
	// browser's patience: a worker left half-started is worse than a slow login.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), loginPrepareTimeout)
	defer cancel()
	if err := p.LoginPrepare.PrepareLogin(ctx, tenant.Name, submittedKey); err != nil {
		p.log().Warn("preparing the tenant for login failed; the session is still issued",
			"tenant", tenant.Name, "err", err)
		p.audit(r, tenant.Name, "login_prepare_failed", fmt.Sprintf("%T", err), http.StatusFound)
	}
}

// stopSignedOutTenants stops the dsh of every tenant whose last session this logout revoked
// (M69). A tenant that still has a live session anywhere keeps its worker: signing out in one
// window must not kill a turn somebody is running in another.
func (p *Proxy) stopSignedOutTenants(r *http.Request, tenants []string) {
	if p.LogoutStop == nil || len(tenants) == 0 {
		return
	}
	for _, name := range tenants {
		remaining, err := p.Sessions.CountTenant(name)
		if err != nil {
			p.log().Error("counting a tenant's remaining sessions failed", "tenant", name, "error_type", fmt.Sprintf("%T", err))
			continue
		}
		if remaining > 0 {
			p.log().Info("tenant dsh kept running: another session is still signed in", "tenant", name, "sessions", remaining)
			p.audit(r, name, "logout_worker_kept", "another session is still signed in", http.StatusSeeOther)
			continue
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), logoutStopTimeout)
		err = p.LogoutStop.StopSignedOut(ctx, name)
		cancel()
		if err != nil {
			// The browser's session is already gone, so the logout itself succeeded; a worker
			// that would not die is an operator's problem and is reported as one.
			p.log().Error("stopping a signed-out tenant's dsh failed", "tenant", name, "error_type", fmt.Sprintf("%T", err))
			p.audit(r, name, "logout_worker_stop_failed", fmt.Sprintf("%T", err), http.StatusSeeOther)
			continue
		}
		p.log().Info("tenant dsh stopped: its last session signed out", "tenant", name)
		p.audit(r, name, "logout_worker_stop", "last session signed out", http.StatusSeeOther)
	}
}

func (p *Proxy) allowLogin(ip string) bool {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	now := p.now()
	b := p.rates[ip]
	window := p.Config.LoginRate.Window.Duration()
	if b == nil && len(p.rates) >= 65536 {
		for key, bucket := range p.rates {
			if now.Sub(bucket.Start) >= window {
				delete(p.rates, key)
			}
		}
		if len(p.rates) >= 65536 {
			ip = "overflow"
			b = p.rates[ip]
		}
	}
	if b == nil || now.Sub(b.Start) >= window {
		p.rates[ip] = &rateBucket{Start: now, Count: 1}
		return true
	}
	if b.Count >= p.Config.LoginRate.Requests {
		return false
	}
	b.Count++
	return true
}
func (p *Proxy) logout(w http.ResponseWriter, r *http.Request) {
	if err := p.checkEdgeOrigin(r, p.Config.ExpectedOrigin(p.Config.PortalPort), false); err != nil {
		p.audit(r, "", "logout_reject", err.Error(), http.StatusForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tenants := p.Registry.List()
	revoked := make([]string, 0, len(tenants))
	for _, t := range tenants {
		for _, cookie := range r.Cookies() {
			if cookie.Name == p.Config.SessionCookieName(t.Name) {
				if err := p.Sessions.Delete(cookie.Value); err != nil {
					p.log().Error("revoke browser session failed", "error_type", fmt.Sprintf("%T", err))
					http.Error(w, "logout unavailable; please retry", http.StatusServiceUnavailable)
					return
				}
				revoked = append(revoked, t.Name)
			}
		}
	}
	// Signing out stops that tenant's dsh once nobody is left in it (M69).
	p.stopSignedOutTenants(r, revoked)
	for _, t := range tenants {
		p.setSessionCookie(w, t.Name, "", true)
	}
	p.audit(r, "", "logout_success", "browser sessions revoked", http.StatusSeeOther)
	http.Redirect(w, r, p.Config.PortalPath(), http.StatusSeeOther)
}
func (p *Proxy) setSessionCookie(w http.ResponseWriter, tenant, token string, remove bool) {
	maxAge := int(p.Config.SessionTTL.Duration().Seconds())
	expires := p.now().Add(p.Config.SessionTTL.Duration())
	if remove {
		maxAge = -1
		expires = time.Unix(1, 0)
	}
	// Path is the tenant's path in single-domain mode: every tenant shares one
	// origin there, and the cookie's path is what stops alice's session from being
	// attached to a request for /t/bob/. The removal cookie must carry the same
	// path, or the browser would keep the old one.
	http.SetCookie(w, &http.Cookie{
		Name: p.Config.SessionCookieName(tenant), Value: token,
		Path:     p.Config.SessionCookiePath(tenant),
		HttpOnly: true,
		// Secure follows the deployment's real scheme: a browser drops a Secure
		// cookie on a plain-HTTP origin, which turns a successful login into a
		// silent redirect back to the portal.
		Secure:   p.Config.SecureSessionCookie(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge, Expires: expires,
	})
}

// sessionCookieNames lists the dshgw session cookies a request carried (names only: the
// values are credentials and never belong in a log).
func sessionCookieNames(r *http.Request) []string {
	names := make([]string, 0, 4)
	for _, cookie := range r.Cookies() {
		if strings.HasPrefix(cookie.Name, "dshgw_s_") {
			names = append(names, cookie.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (p *Proxy) TenantHandler(t registry.Tenant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := validateTarget(r); err != nil {
			p.audit(r, t.Name, "target_reject", err.Error(), http.StatusBadRequest)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		expected := p.Config.ExpectedOrigin(t.PublicPort)
		if err := p.checkEdgeOrigin(r, expected, isWebSocket(r)); err != nil {
			p.audit(r, t.Name, "edge_reject", err.Error(), http.StatusForbidden)
			p.log().Warn("dshgw edge origin rejected", "tenant", t.Name, "origin", safeOrigins(r.Header.Values("Origin")), "reason", err.Error())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		cookie, ok := p.tenantSession(w, r, t)
		if !ok {
			return
		}
		// dshgw's own paths under the tenant's origin (M67): they are served here, not by the
		// worker, so they are handled before the upstream handshake — a page asking who it is
		// signed in as must not depend on the worker being up.
		if p.handleAccountRoute(w, r, t, cookie) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/browser-workspace/") && p.BrowserWorkspaces != nil {
			if err := p.Sessions.Touch(cookie.Value, p.Config.SessionTTL.Duration()); err != nil {
				p.unauthenticated(w, r)
				return
			}
			p.setSessionCookie(w, t.Name, cookie.Value, false)
			p.BrowserWorkspaces.ServeTenant(w, r, t, fmt.Sprintf("%x", sha256.Sum256([]byte(cookie.Value))))
			return
		}
		if err := prepareReplayable(r); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		if _, err := p.ensureUpstream(r.Context(), cookie.Value, t); err != nil {
			p.log().Error("dsh handshake failed", "tenant", t.Name, "err", err)
			http.Error(w, "worker authentication unavailable", http.StatusBadGateway)
			return
		}
		if err := p.Sessions.Touch(cookie.Value, p.Config.SessionTTL.Duration()); err != nil {
			p.unauthenticated(w, r)
			return
		}
		p.setSessionCookie(w, t.Name, cookie.Value, false)
		p.reverseProxy(t, cookie.Value, r.URL.Path).ServeHTTP(w, r)
	})
}
func uniqueCookie(r *http.Request, name string) (*http.Cookie, error) {
	var found []*http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			found = append(found, cookie)
		}
	}
	if len(found) != 1 || found[0].Value == "" {
		return nil, http.ErrNoCookie
	}
	return found[0], nil
}

// tenantSession authenticates one tenant-origin request and reports whether the caller may
// continue, answering the request itself when it may not.
//
// It is the whole gate a tenant request passes — the session cookie names a session, the
// session names this tenant, the tenant's key must still be valid and the account must still
// be opted in — extracted so the dshgw-owned paths under a tenant's origin (M67: who am I,
// sign me out) are gated by exactly the same chain as a worker request. A second copy of
// these checks is how a route eventually ships with one of them missing.
func (p *Proxy) tenantSession(w http.ResponseWriter, r *http.Request, t registry.Tenant) (*http.Cookie, bool) {
	cookie, err := uniqueCookie(r, p.Config.SessionCookieName(t.Name))
	if err != nil {
		// A login that "did nothing" looks exactly like this: the browser comes back to
		// the tenant without the session cookie (a proxy, an extension, a cookie the
		// browser dropped, or a cookie set for a different host). Naming the case is the
		// difference between a guess and a diagnosis, so the presence of ANY tenant cookie
		// is reported alongside the address it came from.
		p.log().Warn("tenant request without a session cookie",
			"tenant", t.Name, "source", requestIP(r), "path", r.URL.Path,
			"cookies", sessionCookieNames(r), "cookie_error", err.Error())
		p.unauthenticated(w, r)
		return nil, false
	}
	record, err := p.Sessions.Get(cookie.Value)
	if err != nil || record.Tenant != t.Name {
		reason := "unknown or expired session"
		if err == nil && record.Tenant != t.Name {
			// A session for another tenant under this tenant's cookie name: either a
			// hand-copied cookie or two tenants sharing one browser profile.
			reason = "session belongs to another tenant"
		}
		p.log().Warn("tenant request with an unusable session",
			"tenant", t.Name, "source", requestIP(r), "path", r.URL.Path, "reason", reason)
		p.unauthenticated(w, r)
		return nil, false
	}
	if err := p.revalidateKey(r.Context(), t.Name); err != nil {
		if errors.Is(err, aigw.ErrInvalidKey) {
			_ = p.Sessions.Delete(cookie.Value)
			p.setSessionCookie(w, t.Name, "", true)
			p.unauthenticated(w, r)
			return nil, false
		}
		p.log().Warn("tenant key revalidation failed", "tenant", t.Name, "err", err)
		http.Error(w, "key validation unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	if err := p.enforceDSHAccess(r.Context(), t.Name); err != nil {
		var denial *aigw.DSHDenial
		if errors.As(err, &denial) {
			// The account lost its dsh opt-in: revoke this browser session and send
			// the user back to the portal, the same path an invalid key takes.
			_ = p.Sessions.Delete(cookie.Value)
			p.setSessionCookie(w, t.Name, "", true)
			p.unauthenticated(w, r)
			return nil, false
		}
		p.log().Warn("dsh entitlement check failed", "tenant", t.Name, "err", err)
		http.Error(w, "dsh authorization unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return cookie, true
}

// accountPathPrefix is this gateway's reserved path namespace under a tenant's origin (M67).
// The prefix, not the route, is what a request must match to reach the routes below, so an
// unknown path under it is a 404 here instead of a 404 from the worker — which is the honest
// answer, since the namespace is ours and never proxied.
const accountPathPrefix = "/dshgw/"

const (
	accountSessionPath = "/dshgw/session/"
	accountLogoutPath  = "/dshgw/logout/"
)

// accountIdentity is the JSON a tenant page reads to render who is signed in.
type accountIdentity struct {
	Authenticated bool   `json:"authenticated"`
	Tenant        string `json:"tenant"`
	Account       string `json:"account,omitempty"`
	FeishuName    string `json:"feishu_name,omitempty"`
	Name          string `json:"name"`
}

// accountEnvelope is the same {ok, value} shape the browser-workspace channel answers with,
// so one client-side helper reads both.
type accountEnvelope struct {
	OK    bool             `json:"ok"`
	Value *accountIdentity `json:"value,omitempty"`
	Error *accountFailure  `json:"error,omitempty"`
}

type accountFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleAccountRoute serves dshgw's own paths under a tenant's origin and reports whether it
// answered the request. The tenant's session has already been validated by tenantSession.
func (p *Proxy) handleAccountRoute(w http.ResponseWriter, r *http.Request, t registry.Tenant, cookie *http.Cookie) bool {
	if !strings.HasPrefix(r.URL.Path, accountPathPrefix) {
		return false
	}
	if !p.Config.AccountCard.Enabled {
		// The feature is off: the namespace is not served at all, so an old client gets the
		// same 404 a fresh one does and cannot render a row nothing backs.
		http.NotFound(w, r)
		return true
	}
	switch {
	case r.URL.Path == accountSessionPath:
		p.sessionIdentity(w, r, t)
	case r.URL.Path == accountLogoutPath:
		p.tenantLogout(w, r, t, cookie)
	default:
		http.NotFound(w, r)
	}
	return true
}

// sessionIdentity answers who the tenant's page is signed in as. It never touches the worker:
// the answer comes from this gateway's own session and from aigw, and it is display data, so
// a name that cannot be resolved arrives as the tenant name rather than as an error.
func (p *Proxy) sessionIdentity(w http.ResponseWriter, r *http.Request, t registry.Tenant) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	identity := p.identity(r.Context(), t)
	_ = json.NewEncoder(w).Encode(accountEnvelope{OK: true, Value: &accountIdentity{
		Authenticated: true,
		Tenant:        identity.Tenant,
		Account:       identity.Account,
		FeishuName:    identity.FeishuName,
		Name:          identity.Name(),
	}})
}

// tenantLogout signs this browser out of THIS tenant and sends it to the portal's login page.
//
// Why not the portal's own POST /logout: that route requires an Origin equal to the portal's
// origin, and a tenant page can only ever send its own tenant origin (in port mode the two
// are different ports, and the request is rejected). Revoking here, with the same session
// store, keeps one browser's other tenants signed in — which is what a button in one tenant's
// sidebar should mean — and the portal is where the person lands, ready to sign in again.
func (p *Proxy) tenantLogout(w http.ResponseWriter, r *http.Request, t registry.Tenant, cookie *http.Cookie) {
	target := p.Config.WithTrailingSlash(p.Config.OriginForPort(p.Config.PortalPort))
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		p.audit(r, t.Name, "tenant_logout_reject", "method not allowed", http.StatusMethodNotAllowed)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Same fence as every other state-changing tenant request: the origin must be this
	// tenant's own, so another site cannot sign a person out.
	if err := p.checkEdgeOrigin(r, p.Config.ExpectedOrigin(t.PublicPort), false); err != nil {
		p.audit(r, t.Name, "tenant_logout_reject", err.Error(), http.StatusForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := p.Sessions.Delete(cookie.Value); err != nil {
		p.log().Error("revoking a tenant session failed", "tenant", t.Name, "error_type", fmt.Sprintf("%T", err))
		p.audit(r, t.Name, "tenant_logout_reject", "revocation failed", http.StatusServiceUnavailable)
		http.Error(w, "logout unavailable; please retry", http.StatusServiceUnavailable)
		return
	}
	// Signing out of this tenant stops its dsh once nobody is left in it (M69).
	p.stopSignedOutTenants(r, []string{t.Name})
	p.setSessionCookie(w, t.Name, "", true)
	p.audit(r, t.Name, "tenant_logout_success", "session revoked", http.StatusSeeOther)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (p *Proxy) unauthenticated(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && !isWebSocket(r) {
		http.Redirect(w, r, p.Config.WithTrailingSlash(p.Config.OriginForPort(p.Config.PortalPort)), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	http.Error(w, `{"error":"dshgw_session_required"}`, http.StatusUnauthorized)
}

func (p *Proxy) checkEdgeOrigin(r *http.Request, expected string, ws bool) error {
	origins := r.Header.Values("Origin")
	mustCheck := ws || r.Method != http.MethodGet && r.Method != http.MethodHead || len(origins) > 0
	// Permit CLI GETs (no Fetch Metadata) and direct same-origin GETs. A
	// portal -> tenant navigation may be same-site with no Origin, but a
	// cross-port image/script/subresource request is not that exception.
	site := r.Header.Values("Sec-Fetch-Site")
	if !mustCheck {
		if len(site) == 0 || len(site) == 1 && (site[0] == "same-origin" || site[0] == "none") {
			return nil
		}
		if len(site) == 1 && site[0] == "same-site" && r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document" {
			return nil
		}
		return errors.New("cross-site subresource request")
	}
	if len(site) > 1 {
		return errors.New("multiple Sec-Fetch-Site values")
	}
	if len(site) == 1 && site[0] != "same-origin" && site[0] != "none" {
		return errors.New("cross-site request")
	}
	if len(origins) == 0 {
		if ws || r.Method != http.MethodGet && r.Method != http.MethodHead {
			return errors.New("origin is required for websocket and unsafe requests")
		}
		return nil
	}
	if len(origins) != 1 || origins[0] != expected {
		return errors.New("origin mismatch")
	}
	return nil
}
func isWebSocket(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}
func headerHasToken(h http.Header, key, want string) bool {
	for _, line := range h.Values(key) {
		for _, token := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func validateTarget(r *http.Request) (string, error) {
	raw := r.RequestURI
	if r.Method == http.MethodConnect || raw == "*" || strings.HasPrefix(raw, "//") || strings.HasPrefix(strings.ToLower(raw), "http://") || strings.HasPrefix(strings.ToLower(raw), "https://") || r.URL.IsAbs() || r.URL.Scheme != "" || r.URL.Host != "" || r.URL.Opaque != "" || r.URL.User != nil {
		return "", errors.New("absolute or authority request target rejected")
	}
	escaped := r.URL.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", err
	}
	for depth := 0; ; depth++ {
		if strings.ContainsRune(decoded, '\x00') || strings.Contains(decoded, "\\") {
			return "", errors.New("invalid path character")
		}
		for _, part := range strings.Split(decoded, "/") {
			if part == "." || part == ".." {
				return "", errors.New("dot path segment rejected")
			}
		}
		next, err := url.PathUnescape(decoded)
		if err != nil {
			// A literal percent (for example /100%25) is a legal path. Only
			// recursively inspect complete encodings; never rewrite the target.
			break
		}
		if next == decoded {
			break
		}
		if depth == 7 {
			return "", errors.New("excessively nested path escape")
		}
		decoded = next
	}
	if !strings.HasPrefix(r.URL.Path, "/") {
		return "", errors.New("path must be absolute")
	}
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil {
		return "", errors.New("invalid query string")
	}
	if _, hasToken := query["token"]; hasToken {
		return "", errors.New("browser requests may not invoke the dsh token exchange")
	}
	return r.URL.Path, nil
}
func prepareReplayable(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if r.ContentLength > maxReplayBody {
		return fmt.Errorf("request body exceeds %d bytes", maxReplayBody)
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxReplayBody+1))
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	if len(data) > maxReplayBody {
		return fmt.Errorf("request body exceeds %d bytes", maxReplayBody)
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	return nil
}

// authorizeDSH runs the mandatory login-time entitlement check (dsh_enforce=login and
// stricter modes alike). A nil Authorizer is a configuration error and fails closed: the
// portal answers 503 rather than admitting a login it could not check.
func (p *Proxy) authorizeDSH(ctx context.Context, key string) (string, error) {
	if _, err := config.ParseDSHEnforce(p.Config.DSHEnforce); err != nil {
		return "", err
	}
	if p.Authorizer == nil {
		return "", errors.New("dsh authorization is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, p.Config.ValidateTimeout.Duration())
	defer cancel()
	return p.Authorizer.Authorize(ctx, key)
}

// resolveTenant maps the authorized account onto a dshgw tenant. The account mapping is
// authoritative; legacy prefix binding only applies when aigw returned no tenant name
// (older aigw without the mapping, or a deployment that has not enabled the account yet).
func (p *Proxy) resolveTenant(authTenant, key string) (registry.Tenant, bool) {
	if authTenant != "" {
		if t, ok := p.Registry.Get(authTenant); ok {
			return t, true
		}
		return registry.Tenant{}, false
	}
	prefix, err := aigw.KeyPrefix(key)
	if err != nil {
		return registry.Tenant{}, false
	}
	return p.Registry.ByPrefix(prefix)
}

// enforceDSHAccess is the request-time counterpart for dsh_enforce=per-request and
// interval:<seconds>. login-only mode (the default) does nothing here: existing sessions
// stay valid until logout/TTL, which the deployment documents. Denials and errors are
// cached exactly like key revalidations, so interval mode bounds both the revocation lag
// and the added aigw traffic.
func (p *Proxy) enforceDSHAccess(ctx context.Context, tenant string) error {
	mode, err := config.ParseDSHEnforce(p.Config.DSHEnforce)
	if err != nil {
		return err
	}
	if !mode.PerRequest && mode.Interval == 0 {
		return nil
	}
	if p.Authorizer == nil {
		return errors.New("dsh_enforce is enabled but no authorizer is configured")
	}
	if p.KeySource == nil {
		return errors.New("dsh_enforce is enabled but no gateway key source is configured")
	}
	sum := sha256.Sum256([]byte(tenant))
	lock := &p.revalidationLocks[sum[0]]
	lock.Lock()
	defer lock.Unlock()
	p.dshMu.Lock()
	prior, cached := p.dshChecks[tenant]
	p.dshMu.Unlock()
	if !mode.PerRequest && cached && p.now().Sub(prior.Checked) < mode.Interval {
		return prior.Err
	}
	key, err := p.KeySource.Key(tenant)
	if err == nil {
		checkCtx, cancel := context.WithTimeout(ctx, p.Config.ValidateTimeout.Duration())
		_, err = p.Authorizer.Authorize(checkCtx, key)
		cancel()
	}
	p.dshMu.Lock()
	p.dshChecks[tenant] = revalidation{Checked: p.now(), Valid: err == nil, Err: err}
	p.dshMu.Unlock()
	return err
}

func (p *Proxy) revalidateKey(ctx context.Context, tenant string) error {
	mode, _ := config.ParseRevalidate(p.Config.KeyRevalidate)
	if !mode.PerRequest && mode.Interval == 0 {
		return nil
	}
	if p.KeySource == nil {
		return errors.New("key_revalidate is enabled but no gateway key source is configured")
	}
	sum := sha256.Sum256([]byte(tenant))
	lock := &p.revalidationLocks[sum[0]]
	lock.Lock()
	defer lock.Unlock()
	p.revalidateMu.Lock()
	prior, cached := p.revalidations[tenant]
	p.revalidateMu.Unlock()
	if !mode.PerRequest && cached && p.now().Sub(prior.Checked) < mode.Interval {
		return prior.Err
	}
	key, err := p.KeySource.Key(tenant)
	if err == nil {
		checkCtx, cancel := context.WithTimeout(ctx, p.Config.ValidateTimeout.Duration())
		_, err = p.Validator.ValidateKey(checkCtx, key)
		cancel()
	}
	p.revalidateMu.Lock()
	p.revalidations[tenant] = revalidation{Checked: p.now(), Valid: err == nil, Err: err}
	p.revalidateMu.Unlock()
	return err
}

func (p *Proxy) ensureUpstream(ctx context.Context, token string, t registry.Tenant) (*session.Upstream, error) {
	record, err := p.Sessions.Get(token)
	if err != nil {
		return nil, err
	}
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.WorkerPort))
	if record.Upstream != nil && record.Upstream.Authority == authority && (record.Upstream.ExpiresAt.IsZero() || record.Upstream.ExpiresAt.After(p.now())) {
		return record.Upstream, nil
	}
	// Fixed stripes bound memory without retaining plaintext browser tokens.
	sum := sha256.Sum256([]byte(token))
	lock := &p.exchangeLocks[sum[0]]
	lock.Lock()
	defer lock.Unlock()
	record, err = p.Sessions.Get(token)
	if err != nil {
		return nil, err
	}
	if record.Upstream != nil && record.Upstream.Authority == authority && (record.Upstream.ExpiresAt.IsZero() || record.Upstream.ExpiresAt.After(p.now())) {
		return record.Upstream, nil
	}
	raw, err := p.HandshakeSource.TokenURL(t.Name)
	if err != nil {
		return nil, err
	}
	upstream, err := p.Exchanger.Exchange(ctx, raw, authority)
	if err != nil {
		return nil, err
	}
	if upstream.Authority != authority {
		return nil, errors.New("handshake exchanger returned the wrong authority")
	}
	if err := p.Sessions.SetUpstream(token, upstream); err != nil {
		return nil, err
	}
	record, err = p.Sessions.Get(token)
	if err != nil {
		return nil, err
	}
	return record.Upstream, nil
}

func (p *Proxy) reverseProxy(t registry.Tenant, token, requestPath string) http.Handler {
	noStore := !p.Config.StoreAPIData() && isAPIPath(requestPath)
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.WorkerPort))
	target := &url.URL{Scheme: "http", Host: authority}
	rp := &httputil.ReverseProxy{FlushInterval: -1, Transport: &retryTransport{p: p, tenant: t, token: token, base: p.baseTransport()}, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Host = authority
		pr.Out.Header.Del(p.Config.EdgePortHeader)
		// The shell document is rewritten below, so it must arrive uncompressed:
		// rewriting a gzipped body corrupts it (the browser reports
		// ERR_CONTENT_DECODING_FAILED). Assets keep their compression.
		if p.Config.LANSettingsUI() && wantsHTMLDocument(pr.In) {
			pr.Out.Header.Del("Accept-Encoding")
		}
		stripRequestHeaders(pr.Out.Header)
	}, ModifyResponse: func(resp *http.Response) error {
		stripWorkerCookies(resp)
		if noStore {
			noStoreHeaders(resp.Header)
		}
		if err := p.injectSettingsBootstrap(resp); err != nil {
			return err
		}
		locations := resp.Header.Values("Location")
		if len(locations) == 0 {
			return nil
		}
		if len(locations) != 1 || locations[0] == "" || strings.Contains(locations[0], "\\") || strings.ContainsAny(locations[0], "\x00\r\n") {
			return errors.New("worker returned an invalid redirect")
		}
		u, err := url.Parse(locations[0])
		if err != nil || u.Opaque != "" || u.User != nil || !u.IsAbs() && u.Host != "" {
			return errors.New("worker returned an invalid redirect")
		}
		query, queryErr := url.ParseQuery(u.RawQuery)
		if queryErr != nil {
			return errors.New("worker returned an invalid redirect query")
		}
		for key := range query {
			if strings.EqualFold(key, "token") {
				return errors.New("worker redirect attempted to expose a token")
			}
		}
		if u.IsAbs() {
			if u.Scheme != "http" || u.Host != authority {
				return errors.New("worker returned an external redirect")
			}
			// The scheme follows the deployment, not an assumption: a plain-HTTP
			// deployment that rewrote this to https sent browsers to a port nobody
			// listens on.
			u.Scheme = p.Config.Scheme()
			u.Host = net.JoinHostPort(p.Config.PublicHost, strconv.Itoa(t.PublicPort))
			resp.Header.Set("Location", u.String())
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		p.log().Error("dsh reverse proxy failed", "tenant", t.Name, "error_type", fmt.Sprintf("%T", err))
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}}
	return rp
}

// settingsBootstrap declares the transport dshgw fronts as owning its host, which is
// what makes dsh's settings/models panel usable on a non-loopback page. It merges into
// any pre-existing value instead of replacing it.
const settingsBootstrap = `<script>globalThis.__DSH_TRANSPORT__=Object.assign(globalThis.__DSH_TRANSPORT__||{},{ownsHost:true});</script>`

// injectSettingsBootstrap adds that declaration to the tenant shell document.
//
// Only text/html is touched, the body is read with a bound, and the script goes
// immediately after <head> so it runs before the shell's own (deferred) modules.
func (p *Proxy) injectSettingsBootstrap(resp *http.Response) error {
	if !p.Config.LANSettingsUI() {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return nil
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		// A body we cannot decode must not be rewritten: splicing into compressed
		// bytes produces a document the browser cannot decode at all.
		p.log().Warn("shell arrived encoded; leaving the settings declaration out", "encoding", encoding)
		return nil
	}
	const limit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	closeErr := resp.Body.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(body)) > limit {
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return nil
	}
	page := string(body)
	if strings.Contains(page, "__DSH_TRANSPORT__") {
		// Already declared (a patched build, or a second pass through this proxy).
		resp.Body = io.NopCloser(strings.NewReader(page))
		return nil
	}
	if index := headTagEnd(page); index >= 0 {
		page = page[:index] + settingsBootstrap + page[index:]
	} else {
		page = settingsBootstrap + page
	}
	resp.Body = io.NopCloser(strings.NewReader(page))
	resp.ContentLength = int64(len(page))
	resp.Header.Set("Content-Length", strconv.Itoa(len(page)))
	// The body no longer matches the upstream's validator.
	resp.Header.Del("ETag")
	return nil
}

// wantsHTMLDocument reports whether a request is a browser navigation for the shell
// (as opposed to a subresource or an API call).
func wantsHTMLDocument(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" && dest != "iframe" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// headTagEnd returns the offset just past the opening <head …> tag, or -1.
func headTagEnd(page string) int {
	lower := strings.ToLower(page)
	start := strings.Index(lower, "<head")
	if start < 0 {
		return -1
	}
	end := strings.IndexByte(lower[start:], '>')
	if end < 0 {
		return -1
	}
	return start + end + 1
}

// isAPIPath reports whether a tenant request path is one of dsh's API surfaces. The
// shell and its assets are excluded on purpose: they are versioned, cacheable and
// large, while every answer under /api belongs to one authenticated tenant.
func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// noStoreHeaders is the single definition of "uncacheable" this gateway uses.
func noStoreHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
}

func stripRequestHeaders(h http.Header) {
	h.Del("Cookie")
	h.Del("Origin")
	for key := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "Sec-Fetch-") {
			h.Del(key)
		}
	}
	for _, key := range []string{"Cookie2", "Authorization", "X-API-Key", "Forwarded", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port", "X-Forwarded-Proto", "Proxy-Authorization", "Proxy-Connection"} {
		h.Del(key)
	}
}

type retryTransport struct {
	p      *Proxy
	tenant registry.Tenant
	token  string
	base   http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	upstream, err := t.p.ensureUpstream(req.Context(), t.token, t.tenant)
	if err != nil {
		return nil, err
	}
	first := cloneForCookie(req, upstream)
	resp, err := t.base.RoundTrip(first)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		if err := t.captureResponseCookie(resp, upstream); err != nil {
			_ = resp.Body.Close()
			return nil, err
		}
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	_, err = t.p.Sessions.ClearUpstreamIf(t.token, upstream.Generation)
	if err != nil {
		return nil, err
	}
	next, err := t.p.ensureUpstream(req.Context(), t.token, t.tenant)
	if err != nil {
		return nil, err
	}
	retry := cloneForCookie(req, next)
	if req.GetBody != nil {
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
	} else if req.Body != nil && req.Body != http.NoBody {
		return nil, errors.New("request body cannot be replayed after worker reauthentication")
	}
	resp, err = t.base.RoundTrip(retry)
	if err == nil && resp.StatusCode != http.StatusUnauthorized {
		if captureErr := t.captureResponseCookie(resp, next); captureErr != nil {
			_ = resp.Body.Close()
			return nil, captureErr
		}
	}
	return resp, err
}

func (t *retryTransport) captureResponseCookie(resp *http.Response, used *session.Upstream) error {
	var matches []*http.Cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == used.Name {
			matches = append(matches, cookie)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	if len(matches) != 1 {
		return errors.New("worker returned conflicting authentication cookies")
	}
	cookie := matches[0]
	if cookie.Value == "" || cookie.MaxAge < 0 || !cookie.Expires.IsZero() && !cookie.Expires.After(t.p.now()) {
		_, err := t.p.Sessions.ClearUpstreamIf(t.token, used.Generation)
		return err
	}
	record, err := t.p.Sessions.Get(t.token)
	if err != nil {
		return err
	}
	expires := cookie.Expires
	if expires.IsZero() || expires.After(record.ExpiresAt) {
		expires = record.ExpiresAt
	}
	if cookie.MaxAge > 0 {
		maxAgeExpiry := t.p.now().Add(time.Duration(cookie.MaxAge) * time.Second)
		if maxAgeExpiry.Before(expires) {
			expires = maxAgeExpiry
		}
	}
	_, err = t.p.Sessions.SetUpstreamIf(t.token, used.Generation, &session.Upstream{Name: used.Name, Value: cookie.Value, Authority: used.Authority, ExpiresAt: expires})
	return err
}

func cloneForCookie(req *http.Request, u *session.Upstream) *http.Request {
	out := req.Clone(req.Context())
	out.Header = req.Header.Clone()
	stripRequestHeaders(out.Header)
	out.Header.Set("Cookie", (&http.Cookie{Name: u.Name, Value: u.Value}).String())
	out.Host = u.Authority
	out.URL.Scheme = "http"
	out.URL.Host = u.Authority
	return out
}

// FeishuPortal is everything the portal needs to redeem a login ticket from aigw (M61).
// There is deliberately no client, no app id and no secret here: aigw owns the Feishu
// application, and this side only verifies what it signed.
type FeishuPortal struct {
	// Enabled opens the portal's Feishu login. While it is off the button is not rendered and
	// the login route does not exist.
	Enabled bool
	// AigwLoginURL is where the browser starts: aigw's /feishu/login.
	AigwLoginURL string
	// Verifier checks tickets and enforces single use.
	Verifier *feishu.Verifier
}

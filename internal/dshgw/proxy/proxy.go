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
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
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

// LogoutResult is what one tenant's logout teardown did (M76). The proxy audits it, because
// "signed out but a mount stayed mounted, and here is which one" is exactly what an operator has
// to be able to answer afterwards — the pre-M76 audit recorded a failure without saying what
// failed.
type LogoutResult struct {
	// MountsDetached counts the mounts that left the kernel mount table (browser directory mounts
	// and ssh workspaces together).
	MountsDetached int
	// MountsLeftover names the mount points that are still attached.
	MountsLeftover []string
	// WorkerStopped is true only when the teardown verified the tenant's dsh is gone.
	WorkerStopped bool
}

// LogoutStop tears a tenant's dsh (and its mounts) down once its last session has signed out
// (M69, sequenced by M76). The proxy decides *when* (it owns the session store, so it knows
// whether another window is still signed in); the lifecycle layer decides *how*.
type LogoutStop interface {
	StopSignedOut(ctx context.Context, tenant string) (LogoutResult, error)
}

// NodeRef is where one tenant's worker listens, as the node it runs on describes it (M77).
type NodeRef struct {
	// Name is the node's name: it appears in logs, audits and operator-facing messages.
	Name string
	// BaseURL is the node agent's address (http://host:port), and Token the shared secret every
	// request to it carries.
	BaseURL string
	Token   string
}

// NodeUpstream is the proxy's view of the worker nodes (M77).
//
// Nil means this deployment is single-machine. A tenant recorded on a node then has nowhere to
// go, and the proxy refuses it rather than falling back to loopback: serving a machine nobody
// believes in is worse than a clear error.
type NodeUpstream interface {
	// NodeFor returns the node a tenant runs on, or ok=false when it runs in this process.
	NodeFor(t registry.Tenant) (NodeRef, bool)
	// Handshake asks the node to exchange its worker's startup token for the upstream cookie,
	// against the authority the control plane will present on every forwarded request.
	Handshake(ctx context.Context, t registry.Tenant, authority string) (*session.Upstream, error)
	// ServesBrowserWorkspaces reports whether this process answers /browser-workspace/ for that
	// tenant. False for a remote tenant: its FUSE mount lives on the node, so the long poll has
	// to be forwarded there.
	ServesBrowserWorkspaces(t registry.Tenant) bool
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
	// Nodes is the multi-machine seam (M77); nil keeps every tenant local.
	Nodes NodeUpstream
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
		case r.URL.Path == "/login/pick":
			// The key picker (M72): a GET renders the choice, a POST performs it. Both read the
			// one-time pick ticket, which is why they are the same route.
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			p.pickHandler(w, r)
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
	// Which key this session is recorded against (M72). Every key of the account enters the same
	// tenant, so with more than one the person picks — and the pick is what the audit trail
	// names. The list arrives with the authorize answer that just admitted the key, so the
	// picker costs no extra round trip.
	if identity, err := p.aigwIdentity(ctx, key); err == nil && len(identity.Keys) > 1 {
		p.offerKeyPick(w, r, tenant.Name)
		return
	}
	// The login moment owns the tenant's lifecycle (M69): the platform slice of its dsh
	// configuration is re-applied from aigw on every sign-in, and the worker — stopped when the
	// last session signed out — is brought back up before the browser is sent to it.
	p.issueTenantSession(w, r, tenant, key, "login_success", 0, "")
	if len(models) == 0 {
		w.Header().Set("X-DSHGW-Warning", "valid key has no currently available models")
	}
}

// issueTenantSession is everything that happens after "this browser may enter this tenant":
// the login-time lifecycle work, the session, the cookie, the identity warm-up, the audit entry
// and the redirect.
//
// It exists because M72 gave the portal a third way in (the key picker). Three copies of this
// sequence would drift, and the parts that must not drift are exactly the ones a copy loses
// silently: the audit action, the Set-Cookie flags, and the redirect target.
//
// keyID names the key the login is recorded against (0 when there was no choice to make), and
// keyName is what the audit trail shows. It is audit data, never a credential: the tenant's
// model credential stays the worker key.
func (p *Proxy) issueTenantSession(w http.ResponseWriter, r *http.Request, tenant registry.Tenant, submittedKey, action string, keyID int64, keyName string) {
	p.prepareLogin(r, tenant, submittedKey)
	token, err := p.Sessions.Issue(tenant.Name, p.Config.SessionTTL.Duration())
	if err != nil {
		p.log().Error("issue dshgw session failed", "tenant", tenant.Name, "err", err)
		p.renderLogin(w, http.StatusInternalServerError, "无法创建会话")
		return
	}
	// Resolve who just signed in before sending them on: the tenant's sidebar asks for it as
	// soon as its page loads, which is one redirect away (M67). Best effort by design — the
	// login must not fail because a display name could not be looked up.
	p.identity(r.Context(), tenant)
	p.setSessionCookie(w, tenant.Name, token, false)
	p.audit(r, tenant.Name, action, "authenticated", http.StatusFound)
	if keyID != 0 {
		p.audit(r, tenant.Name, "login_key_selected", keyName, http.StatusFound)
	}
	if p.Activity != nil {
		if err := p.Activity.MarkLogin(tenant.Name, p.now()); err != nil {
			p.log().Error("persist login activity failed", "tenant", tenant.Name, "err", err)
		}
	}
	http.Redirect(w, r, p.Config.WithTrailingSlash(p.Config.TenantOrigin(tenant.Name)), http.StatusFound)
}

// aigwIdentity asks aigw who this key belongs to, including the account's usable keys. It is
// best effort: the caller has already been admitted, so a failure only costs the picker.
func (p *Proxy) aigwIdentity(ctx context.Context, key string) (aigw.Identity, error) {
	namer, ok := p.Authorizer.(AccountNamer)
	if !ok {
		return aigw.Identity{}, errors.New("the authorization client cannot name accounts")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, p.Config.ValidateTimeout.Duration())
	defer cancel()
	return namer.Identity(lookupCtx, key)
}

// loginPrepareTimeout bounds the login-time lifecycle work: an aigw model refresh plus, on a
// cold tenant, a worker start and its readiness probe. Generous on purpose — the alternative is
// sending the browser to a worker that is not up yet, which is a 502 the person cannot act on —
// but bounded, so a wedged worker cannot hold the login request open forever.
const loginPrepareTimeout = 45 * time.Second

// logoutStopTimeout bounds one tenant's logout teardown: the mounts are force-detached first and
// the dsh is stopped LAST (M76), so this has to cover both — two mount phases of at most 15s each
// plus the runner's TERM (20s) with its escalation to KILL, with room to spare.
const logoutStopTimeout = 55 * time.Second

// logoutTotalTimeout bounds the whole portal logout when it revokes several tenants' sessions in
// one request. Without it a browser signed into eight tenants could hold the request for eight
// minutes; past this budget the remaining tenants are reported as skipped (`logout_worker_stop_skipped`)
// and the next sign-in (or the operator) deals with them.
const logoutTotalTimeout = 150 * time.Second

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
		p.audit(r, tenant.Name, "login_prepare_failed", truncateReason(err.Error()), http.StatusFound)
	}
}

// stopSignedOutTenants tears down the dsh of every tenant this logout revoked a session for (M69,
// sequenced by M76: mounts first, dsh last).
//
// It stops unconditionally, and the deployment host is why: a browser that closed its tabs
// leaves a still-valid session behind for the rest of the TTL, so "nobody is left in this
// tenant" is not something a session count can answer — the operator's own tenant had sixteen
// live sessions, most of them days old, which would have kept its dsh running long after signing
// out. Signing out means the tenant's dsh goes away; a second window of the same person (a tenant
// is one account) loses it too and reconnects by signing in again.
//
// Every step is audited with its outcome, including the reason a step failed: the audit line used
// to record only the Go error type, which is what made the 2026-09-22 incident (three logouts
// reporting failure while the dsh itself had already exited) impossible to diagnose from the log.
func (p *Proxy) stopSignedOutTenants(r *http.Request, tenants []string) {
	if p.LogoutStop == nil || len(tenants) == 0 {
		return
	}
	deadline := p.now().Add(logoutTotalTimeout)
	for _, name := range tenants {
		budget := logoutStopTimeout
		if left := deadline.Sub(p.now()); left < budget {
			if left <= 0 {
				p.log().Warn("skipping a signed-out tenant's teardown: the logout budget is spent", "tenant", name)
				p.audit(r, name, "logout_worker_stop_skipped", "logout budget spent", http.StatusSeeOther)
				continue
			}
			budget = left
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), budget)
		result, err := p.LogoutStop.StopSignedOut(ctx, name)
		cancel()
		if result.MountsDetached > 0 {
			p.audit(r, name, "logout_mount_detach", fmt.Sprintf("%d mount(s) detached", result.MountsDetached), http.StatusSeeOther)
		}
		for _, path := range result.MountsLeftover {
			p.log().Error("a mount outlived a signed-out tenant", "tenant", name, "mountpoint", path)
			p.audit(r, name, "logout_mount_leftover", truncateReason(path), http.StatusSeeOther)
		}
		if err != nil {
			// The browser's session is already gone, so the logout itself succeeded; a mount or a
			// worker that would not go away is an operator's problem and is reported as one, with
			// the reason rather than its type.
			p.log().Error("tearing down a signed-out tenant failed", "tenant", name, "err", err)
			p.audit(r, name, "logout_worker_stop_failed", truncateReason(err.Error()), http.StatusSeeOther)
			continue
		}
		if !result.WorkerStopped {
			p.log().Warn("the signed-out tenant's dsh could not be verified as stopped", "tenant", name)
			p.audit(r, name, "logout_worker_stop_failed", "worker state not verified", http.StatusSeeOther)
			continue
		}
		p.log().Info("tenant dsh stopped on logout", "tenant", name, "mounts_detached", result.MountsDetached)
		p.audit(r, name, "logout_worker_stop", "logout", http.StatusSeeOther)
	}
}

// truncateReason bounds what goes into an audit line: the file is append-only and read by people,
// and a FUSE or ssh failure message can carry a lot of output.
func truncateReason(reason string) string {
	const limit = 512
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit] + "…"
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
			if cookie.Name != p.Config.SessionCookieName(t.Name) {
				continue
			}
			// A tenant counts as signed out only when the session really belonged to it. The
			// stop below is triggered by this list, and a fabricated cookie name must not be
			// able to take somebody else's dsh down.
			if session, err := p.Sessions.Get(cookie.Value); err == nil && session.Tenant == t.Name {
				revoked = append(revoked, t.Name)
			}
			if err := p.Sessions.Delete(cookie.Value); err != nil {
				p.log().Error("revoke browser session failed", "error_type", fmt.Sprintf("%T", err))
				http.Error(w, "logout unavailable; please retry", http.StatusServiceUnavailable)
				return
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
		ref, remote, err := p.remoteFor(t)
		if err != nil {
			p.log().Error("tenant placement is unusable", "tenant", t.Name, "node", t.Node, "err", err)
			p.audit(r, t.Name, "node_unknown", err.Error(), http.StatusServiceUnavailable)
			http.Error(w, "this tenant's node is not available", http.StatusServiceUnavailable)
			return
		}
		browserSession := fmt.Sprintf("%x", sha256.Sum256([]byte(cookie.Value)))
		// A browser workspace's long poll is answered by the process that owns the FUSE mount. For
		// a remote tenant that is the node, so the path is forwarded instead of intercepted — and
		// the session digest goes with it, because the browser's own cookie never leaves here.
		if strings.HasPrefix(r.URL.Path, "/browser-workspace/") && p.BrowserWorkspaces != nil && !remote {
			if err := p.Sessions.Touch(cookie.Value, p.Config.SessionTTL.Duration()); err != nil {
				p.unauthenticated(w, r)
				return
			}
			p.setSessionCookie(w, t.Name, cookie.Value, false)
			p.BrowserWorkspaces.ServeTenant(w, r, t, browserSession)
			return
		}
		if err := prepareReplayable(r); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		if _, err := p.ensureUpstream(r.Context(), cookie.Value, t); err != nil {
			p.log().Error("dsh handshake failed", "tenant", t.Name, "node", t.Node, "err", err)
			if remote && nodeproto.IsCode(err, nodeproto.CodeUnreachable) {
				// The node is the upstream here, so "unreachable" is a gateway-side outage, not a
				// broken worker: 503 with the node named, and an audit line an operator can find.
				p.audit(r, t.Name, "node_unreachable", err.Error(), http.StatusServiceUnavailable)
				http.Error(w, "this tenant's node is unreachable", http.StatusServiceUnavailable)
				return
			}
			// A node that refuses because its worker is gone is the same situation as a dead local
			// worker, and gets the same answer.
			if remote && nodeproto.IsCode(err, nodeproto.CodeWorkerNotRunning) {
				p.audit(r, t.Name, "worker_not_running", err.Error(), http.StatusServiceUnavailable)
				http.Error(w, "this tenant's worker is not running on its node", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "worker authentication unavailable", http.StatusBadGateway)
			return
		}
		if err := p.Sessions.Touch(cookie.Value, p.Config.SessionTTL.Duration()); err != nil {
			p.unauthenticated(w, r)
			return
		}
		p.setSessionCookie(w, t.Name, cookie.Value, false)
		p.reverseProxy(t, cookie.Value, r.URL.Path, ref, remote, browserSession).ServeHTTP(w, r)
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

// remoteFor reports where a tenant's worker runs. A tenant whose placement this process cannot
// resolve is refused here, once, rather than being sent to a loopback port that is not serving it.
func (p *Proxy) remoteFor(t registry.Tenant) (NodeRef, bool, error) {
	if p.Config.IsLocalNode(t.Node) {
		return NodeRef{}, false, nil
	}
	if p.Nodes == nil {
		return NodeRef{}, false, fmt.Errorf("tenant %s runs on node %s, but this process has no node clients", t.Name, t.Node)
	}
	ref, ok := p.Nodes.NodeFor(t)
	if !ok {
		return NodeRef{}, false, fmt.Errorf("tenant %s runs on node %s, which this deployment does not define", t.Name, t.Node)
	}
	return ref, true, nil
}

// nodeRefusal carries a node's refusal of a tenant request out of ModifyResponse and into
// ErrorHandler, which is the only place a ReverseProxy lets us write our own answer.
type nodeRefusal struct {
	code   string
	status int
}

func (r *nodeRefusal) Error() string { return "node refused the request: " + r.code }

// nodeRefusalMessage is what the browser is told. The node's own wording names machines and
// internals; the operator gets that in the log and the audit line instead.
func nodeRefusalMessage(code string) (int, string) {
	switch code {
	case nodeproto.CodeWorkerNotRunning:
		return http.StatusServiceUnavailable, "the tenant's worker is not running on its node; please retry shortly"
	case nodeproto.CodeTenantUnknown:
		return http.StatusNotFound, "unknown tenant"
	case nodeproto.CodeNotImplemented:
		return http.StatusServiceUnavailable, "this node does not support that path yet"
	default:
		return http.StatusServiceUnavailable, "the tenant's node refused the request"
	}
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
	var upstream *session.Upstream
	if _, remote, remoteErr := p.remoteFor(t); remoteErr != nil {
		return nil, remoteErr
	} else if remote {
		// The token file and the loopback socket are on the node, so the node handshakes; it must
		// use the authority this control plane will present on every forwarded request.
		upstream, err = p.Nodes.Handshake(ctx, t, authority)
	} else {
		var raw string
		raw, err = p.HandshakeSource.TokenURL(t.Name)
		if err != nil {
			return nil, err
		}
		upstream, err = p.Exchanger.Exchange(ctx, raw, authority)
	}
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

// isNodeTransportFailure reports whether an error from the node hop is a connection-level failure
// (dial, reset, timeout) rather than something the node said. A node's own refusal arrives as a
// response, so anything here means the machine did not answer.
func isNodeTransportFailure(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

func (p *Proxy) reverseProxy(t registry.Tenant, token, requestPath string, ref NodeRef, remote bool, browserSession string) http.Handler {
	noStore := !p.Config.StoreAPIData() && isAPIPath(requestPath)
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.WorkerPort))
	target := &url.URL{Scheme: "http", Host: authority}
	if remote {
		// The node is the upstream: the request is addressed at it, and the worker's loopback
		// authority is presented by the node on the final hop (which is what keeps dsh's
		// authority-bound cookie valid without either side sharing a key).
		if parsed, err := url.Parse(ref.BaseURL); err == nil {
			target = parsed
		}
	}
	var refusal *nodeRefusal
	rp := &httputil.ReverseProxy{FlushInterval: -1, Transport: &retryTransport{p: p, tenant: t, token: token, base: p.baseTransport(), node: nodeForTransport(ref, remote), browserSession: browserSession}, Rewrite: func(pr *httputil.ProxyRequest) {
		if remote {
			// Path prefixing preserves the tenant's own path (and its escaping) exactly: the node
			// strips the prefix again before it talks to the worker.
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = nodeproto.TenantPath + strings.TrimPrefix(pr.In.URL.Path, "/")
			if pr.In.URL.RawPath != "" {
				pr.Out.URL.RawPath = nodeproto.TenantPath + strings.TrimPrefix(pr.In.URL.RawPath, "/")
			} else {
				pr.Out.URL.RawPath = ""
			}
			pr.Out.Host = target.Host
		} else {
			pr.SetURL(target)
			pr.Out.Host = authority
		}
		pr.Out.Header.Del(p.Config.EdgePortHeader)
		// The shell document is rewritten below, so it must arrive uncompressed:
		// rewriting a gzipped body corrupts it (the browser reports
		// ERR_CONTENT_DECODING_FAILED). Assets keep their compression.
		if p.Config.LANSettingsUI() && wantsHTMLDocument(pr.In) {
			pr.Out.Header.Del("Accept-Encoding")
		}
		stripRequestHeaders(pr.Out.Header)
		// Node headers go on AFTER the strip: they are ours. Everything the browser labelled with
		// our protocol's namespace goes first — Set would overwrite the values we write anyway, but
		// a header we do not set on this path (the browser-session digest, on a non-poll request)
		// would otherwise be the browser's.
		if remote {
			stripNodeHeaders(pr.Out.Header)
			pr.Out.Header.Set(nodeproto.HeaderTenant, t.Name)
			pr.Out.Header.Set(nodeproto.HeaderProtocol, nodeproto.ProtocolHeaderValue)
			pr.Out.Header.Set("Authorization", "Bearer "+ref.Token)
			if browserSession != "" {
				pr.Out.Header.Set(nodeproto.HeaderBrowserSession, browserSession)
			}
		}
	}, ModifyResponse: func(resp *http.Response) error {
		// Read the node's refusal code first, then take the whole namespace off the response: the
		// browser must not see these headers, and a worker must not be able to inject one that
		// this control plane would read as the node's word.
		refusalCode := ""
		if remote {
			refusalCode = strings.TrimSpace(resp.Header.Get(nodeproto.HeaderError))
			stripNodeHeaders(resp.Header)
		}
		if refusalCode != "" {
			// A node-side refusal: drop its wording (it names machines and internals) and answer
			// from here, where the operator's log and audit live.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			refusal = &nodeRefusal{code: refusalCode, status: resp.StatusCode}
			return refusal
		}
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
		var sentinel *nodeRefusal
		if errors.As(err, &sentinel) {
			status, message := nodeRefusalMessage(sentinel.code)
			p.log().Error("node refused a tenant request", "tenant", t.Name, "node", ref.Name, "code", sentinel.code, "status", status)
			p.audit(r, t.Name, "node_refusal_"+sentinel.code, sentinel.code, status)
			http.Error(w, message, status)
			return
		}
		// A remote tenant's upstream is our own node agent, so failing to reach it is a
		// gateway-side outage (503), not "the upstream broke" (502). This is the path a cached
		// handshake takes: the session still holds a live cookie, so no control call happens
		// before the dial, and the failure only shows up here.
		if remote && isNodeTransportFailure(err) {
			p.log().Error("tenant node is unreachable", "tenant", t.Name, "node", ref.Name, "err", err)
			p.audit(r, t.Name, "node_unreachable", err.Error(), http.StatusServiceUnavailable)
			http.Error(w, "this tenant's node is unreachable", http.StatusServiceUnavailable)
			return
		}
		p.log().Error("dsh reverse proxy failed", "tenant", t.Name, "node", ref.Name, "error_type", fmt.Sprintf("%T", err))
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

// stripNodeHeaders removes every header in the node protocol's namespace. It is applied to the
// outbound request (so a browser's own X-Dshgw-* can never be mistaken for ours) and to the
// inbound response (so a worker cannot speak the node's error channel).
func stripNodeHeaders(h http.Header) {
	for key := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "X-Dshgw-") {
			h.Del(key)
		}
	}
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
	// node is non-nil for a tenant that runs on a worker node: then the request is addressed at
	// the node and only the worker credential has to be (re)injected.
	node           *NodeRef
	browserSession string
}

// nodeForTransport returns the node reference for a remote tenant, or nil for a local one.
func nodeForTransport(ref NodeRef, remote bool) *NodeRef {
	if !remote {
		return nil
	}
	return &ref
}

// decorate injects the worker credential into an outbound request.
//
// For a local tenant that also means pointing the request at the worker's loopback authority
// (cloneForCookie). For a remote tenant the URL stays where it is — on the node — and the node
// presents the loopback authority itself; rewriting it here would send another machine's traffic
// to this machine's loopback port.
func (t *retryTransport) decorate(req *http.Request, upstream *session.Upstream) *http.Request {
	if t.node == nil {
		return cloneForCookie(req, upstream)
	}
	out := req.Clone(req.Context())
	out.Header = req.Header.Clone()
	stripRequestHeaders(out.Header)
	stripNodeHeaders(out.Header)
	out.Header.Set("Cookie", (&http.Cookie{Name: upstream.Name, Value: upstream.Value}).String())
	out.Header.Set("Authorization", "Bearer "+t.node.Token)
	out.Header.Set(nodeproto.HeaderTenant, t.tenant.Name)
	out.Header.Set(nodeproto.HeaderProtocol, nodeproto.ProtocolHeaderValue)
	if t.browserSession != "" {
		out.Header.Set(nodeproto.HeaderBrowserSession, t.browserSession)
	}
	return out
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	upstream, err := t.p.ensureUpstream(req.Context(), t.token, t.tenant)
	if err != nil {
		return nil, err
	}
	first := t.decorate(req, upstream)
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
	retry := t.decorate(req, next)
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
	// Verifier checks tickets and enforces single use. It checks both ticket kinds (a login and
	// a key pick) because the two are told apart by the mode inside the signed payload (M72).
	Verifier *feishu.Verifier
}

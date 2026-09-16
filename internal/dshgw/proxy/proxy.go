// Package proxy implements the portal, edge-origin fence, session mapping, and
// streaming reverse proxy in front of per-tenant dsh workers.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

const maxReplayBody = 64 << 20

type KeyValidator interface {
	ValidateKey(context.Context, string) ([]string, error)
}

type Proxy struct {
	Config          *config.Config
	Registry        *registry.Registry
	Sessions        session.Store
	HandshakeSource handshake.Source
	Exchanger       handshake.Exchanger
	Validator       KeyValidator
	KeySource       KeySource
	Transport       http.RoundTripper
	Logger          *slog.Logger
	Auditor         audit.Sink
	Activity        activity.Recorder
	Now             func() time.Time

	exchangeLocks     [256]sync.Mutex
	revalidationLocks [256]sync.Mutex
	rateMu            sync.Mutex
	rates             map[string]*rateBucket
	revalidateMu      sync.Mutex
	revalidations     map[string]revalidation
	reloadMu          sync.Mutex
	lastReload        time.Time
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
	p := &Proxy{Config: cfg, Registry: reg, Sessions: sessions, HandshakeSource: source, Exchanger: exchanger, Validator: validator, Transport: transport, rates: map[string]*rateBucket{}, revalidations: map[string]revalidation{}}
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

var loginPage = template.Must(template.New("login").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>dsh 登录</title><style>body{font:16px system-ui;max-width:34rem;margin:10vh auto;padding:1rem;background:#101318;color:#eef}main{background:#1b2028;padding:2rem;border-radius:12px}input,button{box-sizing:border-box;width:100%;padding:.8rem;margin:.4rem 0}button{cursor:pointer}.error{color:#ff9b9b}.note{color:#bcc6d6;font-size:.9rem}</style></head><body><main><h1>DeepSeek Harness</h1>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}<form method="post" action="/login"><label>aigw API Key<input type="password" name="key" autocomplete="off" spellcheck="false" required></label><button type="submit">登录</button></form><form method="post" action="/logout"><button type="submit">退出此浏览器的全部租户会话</button></form><p class="note">Key 只用于向 aigw 验证身份；browser-fs 默认开启后，只有你在浏览器明确授权的本机目录可被 agent 访问，内容可能进入模型请求。</p></main></body></html>`))

func (p *Proxy) renderLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = loginPage.Execute(w, struct{ Error string }{message})
}

func (p *Proxy) login(w http.ResponseWriter, r *http.Request) {
	if err := p.checkEdgeOrigin(r, p.Config.OriginForPort(p.Config.PortalPort), false); err != nil {
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
	prefix, err := aigw.KeyPrefix(key)
	if err != nil {
		p.renderLogin(w, http.StatusUnauthorized, "Key 无效或已停用")
		return
	}
	tenant, ok := p.Registry.ByPrefix(prefix)
	if !ok {
		p.audit(r, "", "login_reject", "unbound key prefix", http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, "该 Key 尚未绑定 dsh 租户，请联系管理员执行 tenant create 或 bind")
		return
	}
	token, err := p.Sessions.Issue(tenant.Name, p.Config.SessionTTL.Duration())
	if err != nil {
		p.log().Error("issue dshgw session failed", "err", err)
		p.renderLogin(w, http.StatusInternalServerError, "无法创建会话")
		return
	}
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
	if err := p.checkEdgeOrigin(r, p.Config.OriginForPort(p.Config.PortalPort), false); err != nil {
		p.audit(r, "", "logout_reject", err.Error(), http.StatusForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tenants := p.Registry.List()
	for _, t := range tenants {
		for _, cookie := range r.Cookies() {
			if cookie.Name == p.Config.SessionCookieName(t.Name) {
				if err := p.Sessions.Delete(cookie.Value); err != nil {
					p.log().Error("revoke browser session failed", "error_type", fmt.Sprintf("%T", err))
					http.Error(w, "logout unavailable; please retry", http.StatusServiceUnavailable)
					return
				}
			}
		}
	}
	for _, t := range tenants {
		p.setSessionCookie(w, t.Name, "", true)
	}
	p.audit(r, "", "logout_success", "browser sessions revoked", http.StatusSeeOther)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (p *Proxy) setSessionCookie(w http.ResponseWriter, tenant, token string, remove bool) {
	maxAge := int(p.Config.SessionTTL.Duration().Seconds())
	expires := p.now().Add(p.Config.SessionTTL.Duration())
	if remove {
		maxAge = -1
		expires = time.Unix(1, 0)
	}
	http.SetCookie(w, &http.Cookie{Name: p.Config.SessionCookieName(tenant), Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: expires})
}

func (p *Proxy) TenantHandler(t registry.Tenant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := validateTarget(r); err != nil {
			p.audit(r, t.Name, "target_reject", err.Error(), http.StatusBadRequest)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		expected := p.Config.OriginForPort(t.PublicPort)
		if err := p.checkEdgeOrigin(r, expected, isWebSocket(r)); err != nil {
			p.audit(r, t.Name, "edge_reject", err.Error(), http.StatusForbidden)
			p.log().Warn("dshgw edge origin rejected", "tenant", t.Name, "origin", safeOrigins(r.Header.Values("Origin")), "reason", err.Error())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		cookie, err := uniqueCookie(r, p.Config.SessionCookieName(t.Name))
		if err != nil {
			p.unauthenticated(w, r)
			return
		}
		record, err := p.Sessions.Get(cookie.Value)
		if err != nil || record.Tenant != t.Name {
			p.unauthenticated(w, r)
			return
		}
		if err := p.revalidateKey(r.Context(), t.Name); err != nil {
			if errors.Is(err, aigw.ErrInvalidKey) {
				_ = p.Sessions.Delete(cookie.Value)
				p.setSessionCookie(w, t.Name, "", true)
				p.unauthenticated(w, r)
				return
			}
			p.log().Warn("tenant key revalidation failed", "tenant", t.Name, "err", err)
			http.Error(w, "key validation unavailable", http.StatusServiceUnavailable)
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
		p.reverseProxy(t, cookie.Value).ServeHTTP(w, r)
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

func (p *Proxy) reverseProxy(t registry.Tenant, token string) http.Handler {
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.WorkerPort))
	target := &url.URL{Scheme: "http", Host: authority}
	rp := &httputil.ReverseProxy{FlushInterval: -1, Transport: &retryTransport{p: p, tenant: t, token: token, base: p.baseTransport()}, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Host = authority
		pr.Out.Header.Del(p.Config.EdgePortHeader)
		stripRequestHeaders(pr.Out.Header)
	}, ModifyResponse: func(resp *http.Response) error {
		stripWorkerCookies(resp)
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
			u.Scheme = "https"
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

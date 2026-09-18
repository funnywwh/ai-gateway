package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/feishu"
)

// FeishuDeps is everything the Feishu routes need. It is built once at startup from the
// configuration, so a half-configured deployment fails there instead of during a user's
// first login.
type FeishuDeps struct {
	// Client performs the two server-side calls against Feishu.
	Client *feishu.Client
	// States signs and verifies authorization attempts.
	States *feishu.StateCodec
	// Tickets signs the short-lived handoff the DSH gateway redeems (M61). Nil while the
	// DSH login flow is off.
	Tickets *feishu.TicketCodec
	// RedirectURI is the callback URL exactly as registered in the Feishu console: it is
	// sent in the authorization redirect and again in the token exchange, and Feishu
	// refuses the exchange if the two differ.
	RedirectURI string
	// LoginPath and CallbackPath are the paths this server serves, with the mount prefix
	// already applied. They exist so startup can prove RedirectURI points back here.
	LoginPath    string
	CallbackPath string
	// DSHLogin opens the DSH portal login flow; PortalURL is where that flow returns the
	// browser (the dshgw portal), and LoginURL is where the browser starts.
	DSHLogin  bool
	PortalURL string
	LoginURL  string
}

// feishuNoStore marks a Feishu handoff uncacheable. The responses here either redirect the
// browser with a signed value or set a cookie, and a cached copy of either is a stale login.
func feishuNoStore(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Pragma", "no-cache")
}

// feishuEnabled reports whether the integration is wired at all.
func (s *Server) feishuEnabled() bool {
	return s.deps.Feishu != nil && s.deps.Feishu.Client != nil && s.deps.Feishu.States != nil
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

// handleFeishuLogin starts an authorization attempt. It is unauthenticated on purpose for
// the DSH login flow — any employee may sign in to their own tenant — while the binding
// flow is gated by the admin session that also mints its state.
func (s *Server) handleFeishuLogin(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		http.NotFound(w, r)
		return
	}
	deps := s.deps.Feishu
	if r.URL.Query().Has("code") || r.URL.Query().Has("state") {
		// A URL carrying an authorization code belongs to the callback, not to a start
		// link: accepting it here would put the code in browser history and referrers.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	flow := feishu.FlowDSHLogin
	keyID := int64(0)
	actor := ""
	switch r.URL.Query().Get("mode") {
	case "", string(feishu.FlowDSHLogin):
		if !deps.DSHLogin || deps.PortalURL == "" {
			http.NotFound(w, r)
			return
		}
	case string(feishu.FlowBind):
		// Binding a key is an administrator action; the session that started it is the
		// capability, and the callback re-checks the role.
		user, ok := s.adminActor(w, r, true)
		if !ok {
			return
		}
		parsed, err := strconv.ParseInt(r.URL.Query().Get("key"), 10, 64)
		if err != nil || parsed <= 0 {
			writeAPIError(w, domain.ErrInvalidRequest("key must be the numeric id of an API key"))
			return
		}
		key, err := s.deps.AdminStore.GetAPIKeyByID(r.Context(), parsed)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		flow, keyID, actor = feishu.FlowBind, key.ID, user.Username
	default:
		writeAPIError(w, domain.ErrInvalidRequest(`mode must be "dsh" or "bind"`))
		return
	}
	if !s.allowFeishuAttempt(w, r) {
		return
	}
	nonce, err := feishuNonce()
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot start the Feishu flow"))
		return
	}
	state, err := deps.States.Sign(flow, keyID, actor, nonce)
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot start the Feishu flow"))
		return
	}
	target, err := deps.Client.AuthorizeURLFor(state, deps.RedirectURI)
	if err != nil {
		s.deps.Log.Error("building the Feishu authorization URL failed", "err", err)
		writeAPIError(w, domain.ErrInternal("the Feishu integration is misconfigured"))
		return
	}
	// The browser is leaving for Feishu: nothing about this handoff may be cached.
	feishuNoStore(w.Header())
	http.Redirect(w, r, target, http.StatusFound)
}

// handleFeishuCallback is the single redirect target for both flows. It carries no session
// of its own: the signed state proves who started the attempt, and for a binding the actor
// is re-checked here so a demoted administrator cannot finish what they started.
//
// When the state cannot be trusted the flow is unknown, so the answer is a small page on
// this server rather than a redirect: guessing a destination would send an administrator to
// the portal (or a person to the console) with no explanation of what went wrong.
func (s *Server) handleFeishuCallback(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		http.NotFound(w, r)
		return
	}
	feishuNoStore(w.Header())
	deps := s.deps.Feishu
	query := r.URL.Query()

	// A state is verified even when Feishu reports a refusal, because the flow it names
	// decides where the browser goes back to.
	state, stateErr := deps.States.Verify(query.Get("state"))
	if stateErr != nil {
		reason := feishuReason(stateErr)
		s.audit(r.Context(), "", "feishu_login_reject", "feishu_callback", "", map[string]any{"reason": reason}, "denied")
		s.renderFeishuStop(w, http.StatusBadRequest, feishuStateMessage(reason))
		return
	}
	flow := state.Flow
	failFlow := func(result string) {
		if flow == feishu.FlowBind {
			s.redirectConsole(w, r, result, state.KeyID)
			return
		}
		s.redirectFeishuError(w, r, result)
	}
	if denial := query.Get("error"); denial != "" {
		// The person declined the consent screen: nothing was written, nothing is broken.
		s.audit(r.Context(), state.Actor, "feishu_login_reject", "feishu_callback", "", map[string]any{"reason": denial}, "denied")
		failFlow("cancelled")
		return
	}
	code := query.Get("code")
	if strings.TrimSpace(code) == "" {
		s.audit(r.Context(), state.Actor, "feishu_login_reject", "feishu_callback", "", map[string]any{"reason": "missing code"}, "denied")
		failFlow("error")
		return
	}
	identity, err := deps.Client.Exchange(r.Context(), code, deps.RedirectURI)
	if err != nil {
		reason := feishuExchangeReason(err)
		s.deps.Log.Warn("the Feishu exchange failed", "reason", reason, "flow", string(flow), "err", err)
		s.audit(r.Context(), state.Actor, "feishu_login_reject", "feishu_callback", "", map[string]any{"reason": reason}, "failed")
		failFlow(reason)
		return
	}
	if flow == feishu.FlowBind {
		s.finishFeishuBind(w, r, state, identity)
		return
	}
	s.finishFeishuLogin(w, r, identity)
}

// renderFeishuStop explains a callback that could not be attributed to a flow. It is
// deliberately plain: no script, no styling beyond the browser's, and no values echoed back.
func (s *Server) renderFeishuStop(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	console := s.url("/admin/ui/#/keys")
	body := "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width\"><title>飞书登录</title></head><body>" +
		"<h1>飞书登录未能完成</h1><p>" + message + "</p>" +
		"<p><a href=\"" + console + "\">返回控制台 API Keys</a></p>" +
		"</body></html>"
	_, _ = io.WriteString(w, body)
}

// feishuStateMessage turns a state refusal into something a person can act on.
func feishuStateMessage(reason string) string {
	switch reason {
	case "expired":
		return "这次授权已经超时（授权页停留太久）。请重新点击「绑定飞书」或「飞书登录」。"
	case "replay":
		return "这个链接已经使用过了。请重新发起一次授权。"
	default:
		return "这次授权请求无法校验（链接不完整或被修改）。请从控制台或门户重新发起。"
	}
}

// finishFeishuBind writes the binding the administrator asked for.
func (s *Server) finishFeishuBind(w http.ResponseWriter, r *http.Request, state feishu.State, identity feishu.Identity) {
	ctx := r.Context()
	// The role is re-checked against the live record: a state minted before a demotion or a
	// deletion must not complete a binding.
	if !s.feishuActorStillAdmin(ctx, state.Actor) {
		s.audit(ctx, state.Actor, "feishu_bind_reject", "api_key", strconv.FormatInt(state.KeyID, 10), map[string]any{"reason": "actor is no longer an administrator"}, "denied")
		s.redirectConsole(w, r, "rejected", state.KeyID)
		return
	}
	key, err := s.deps.AdminStore.GetAPIKeyByID(ctx, state.KeyID)
	if err != nil {
		s.audit(ctx, state.Actor, "feishu_bind_reject", "api_key", strconv.FormatInt(state.KeyID, 10), map[string]any{"reason": "key is gone"}, "failed")
		s.redirectConsole(w, r, "error", state.KeyID)
		return
	}
	previous := key.FeishuOpenID
	err = s.deps.AdminStore.BindAPIKeyFeishu(ctx, key.ID, domain.FeishuBinding{
		OpenID: identity.OpenID, UnionID: identity.UnionID, Name: identity.Name, BoundBy: state.Actor,
	})
	if err != nil {
		reason := "binding failed"
		result := "failed"
		code := "error"
		if apiErr := toAPIError(err); apiErr.Status == http.StatusConflict {
			reason = "already bound to another key"
			code = "conflict"
		}
		s.audit(ctx, state.Actor, "feishu_bind_reject", "api_key", strconv.FormatInt(key.ID, 10),
			map[string]any{"reason": reason, "open_id": identity.OpenID}, result)
		s.redirectConsole(w, r, code, key.ID)
		return
	}
	result := "bound"
	if previous != "" {
		result = "replaced"
	}
	s.audit(ctx, state.Actor, "feishu_bind", "api_key", strconv.FormatInt(key.ID, 10), map[string]any{
		"open_id": identity.OpenID, "union_id": identity.UnionID, "name": identity.Name,
		"previous_open_id": previous,
	}, "ok")
	s.redirectConsole(w, r, result, key.ID)
}

// finishFeishuLogin resolves the identity to an account that may use the DSH gateway and
// hands the browser a short-lived ticket for that tenant.
func (s *Server) finishFeishuLogin(w http.ResponseWriter, r *http.Request, identity feishu.Identity) {
	ctx := r.Context()
	deps := s.deps.Feishu
	if !deps.DSHLogin || deps.Tickets == nil || deps.PortalURL == "" {
		http.NotFound(w, r)
		return
	}
	key, err := s.deps.AdminStore.FindAPIKeyByFeishuOpenID(ctx, identity.OpenID)
	if err != nil {
		s.deps.Log.Error("resolving the Feishu identity failed", "err", err)
		s.redirectFeishuError(w, r, "error")
		return
	}
	if key == nil {
		s.audit(ctx, "", "feishu_login_reject", "api_key", "", map[string]any{"reason": "unbound open id", "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "unbound")
		return
	}
	account, err := s.deps.AdminStore.GetAccount(ctx, key.AccountID)
	if err != nil {
		s.deps.Log.Error("loading the account of a bound key failed", "err", err, "account", key.AccountID)
		s.redirectFeishuError(w, r, "error")
		return
	}
	switch {
	case account.Status != "" && account.Status != "active":
		// Suspended or closed: the identity is fine, the account is not.
		s.audit(ctx, "", "feishu_login_reject", "account", account.Name, map[string]any{"reason": "account " + account.Status, "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "account_status")
		return
	case !account.DSHEnabled:
		s.audit(ctx, "", "feishu_login_reject", "account", account.Name, map[string]any{"reason": "dsh disabled", "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "dsh_disabled")
		return
	case strings.TrimSpace(account.DshTenant) == "":
		s.audit(ctx, "", "feishu_login_reject", "account", account.Name, map[string]any{"reason": "tenant unassigned", "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "tenant_missing")
		return
	}
	nonce, err := feishuNonce()
	if err != nil {
		s.redirectFeishuError(w, r, "error")
		return
	}
	wire, ticket, err := deps.Tickets.Issue(account.DshTenant, key.ID, account.ID, identity.OpenID, nonce)
	if err != nil {
		s.deps.Log.Error("issuing a DSH login ticket failed", "err", err)
		s.redirectFeishuError(w, r, "error")
		return
	}
	s.audit(ctx, "", "feishu_dsh_login", "account", account.Name, map[string]any{
		"tenant": account.DshTenant, "open_id": identity.OpenID, "expires_at": time.Unix(ticket.Expires, 0).UTC().Format(time.RFC3339),
	}, "ok")
	// The ticket travels as a host-only cookie, which is what makes it reach the portal on
	// its own port: cookies are scoped to a host, not to a port. A deployment whose portal
	// lives on a different host gets it in the query string as well, because there the
	// cookie would never arrive.
	if s.feishuSameHost(deps.PortalURL) {
		s.setFeishuTicketCookie(w, wire)
		http.Redirect(w, r, s.trailingSlash(deps.PortalURL)+"login/feishu", http.StatusSeeOther)
		return
	}
	target := s.trailingSlash(deps.PortalURL) + "login/feishu?ticket=" + url.QueryEscape(wire)
	s.deps.Log.Warn("the DSH portal is on another host; the login ticket travels in the URL", "portal", deps.PortalURL)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Console binding routes
// ---------------------------------------------------------------------------

// handleAdminBindKeyFeishu starts the binding flow from the console. It answers a redirect
// rather than JSON because the browser has to visit Feishu itself.
func (s *Server) handleAdminBindKeyFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		http.NotFound(w, r)
		return
	}
	user, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid key id"))
		return
	}
	key, err := s.deps.AdminStore.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if key.Status != "" && key.Status != "active" {
		// A disabled key cannot be used, so binding an identity to it would only create a
		// login that always fails.
		writeAPIError(w, domain.ErrConflict("bind a Feishu account to an active API key"))
		return
	}
	target := fmt.Sprintf("%s?mode=%s&key=%d", s.deps.Feishu.LoginPath, feishu.FlowBind, key.ID)
	s.audit(r.Context(), user.Username, "feishu_bind_start", "api_key", strconv.FormatInt(key.ID, 10), nil, "ok")
	feishuNoStore(w.Header())
	http.Redirect(w, r, target, http.StatusFound)
}

// handleAdminUnbindKeyFeishu clears a key's Feishu identity.
func (s *Server) handleAdminUnbindKeyFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		http.NotFound(w, r)
		return
	}
	user, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid key id"))
		return
	}
	key, err := s.deps.AdminStore.GetAPIKeyByID(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	previous := key.FeishuOpenID
	changed, err := s.deps.AdminStore.UnbindAPIKeyFeishu(r.Context(), key.ID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if changed {
		s.audit(r.Context(), user.Username, "feishu_unbind", "api_key", strconv.FormatInt(key.ID, 10),
			map[string]any{"open_id": previous}, "ok")
	}
	writeJSON(w, http.StatusOK, map[string]any{"unbound": changed, "key_id": key.ID})
}

// feishuBindingJSON is the console's view of a key's Feishu identity. It always has the
// same shape, bound or not, so the page never has to guess whether a missing field means
// "unbound" or "old server".
func feishuBindingJSON(key *domain.APIKey) map[string]any {
	if key == nil || key.FeishuOpenID == "" {
		return map[string]any{"bound": false}
	}
	out := map[string]any{
		"bound":    true,
		"open_id":  key.FeishuOpenID,
		"name":     key.FeishuName,
		"union_id": key.FeishuUnionID,
		"bound_by": key.FeishuBoundBy,
	}
	if key.FeishuBoundAt != nil {
		out["bound_at"] = key.FeishuBoundAt.UTC().Format(time.RFC3339)
	} else {
		out["bound_at"] = nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// feishuAttemptLimit and feishuAttemptWindow bound how often one address may start a Feishu
// attempt. The flow costs an outbound call and a consent screen, so the ceiling is the same
// order as the console's own password login rather than something a person can reach by
// clicking around.
const (
	feishuAttemptLimit  = 10
	feishuAttemptWindow = time.Minute
)

// allowFeishuAttempt applies that per-IP ceiling.
func (s *Server) allowFeishuAttempt(w http.ResponseWriter, r *http.Request) bool {
	s.feishuMu.Lock()
	now := time.Now().UTC()
	ip := clientIP(r)
	bucket := s.feishuRates[ip]
	if bucket == nil && len(s.feishuRates) >= 65536 {
		for key, entry := range s.feishuRates {
			if now.Sub(entry.start) >= feishuAttemptWindow {
				delete(s.feishuRates, key)
			}
		}
	}
	if bucket == nil || now.Sub(bucket.start) >= feishuAttemptWindow {
		s.feishuRates[ip] = &feishuRate{start: now, count: 1}
		s.feishuMu.Unlock()
		return true
	}
	if bucket.count >= feishuAttemptLimit {
		s.feishuMu.Unlock()
		s.audit(r.Context(), "", "feishu_login_reject", "feishu_login", "", map[string]any{"reason": "rate limit"}, "denied")
		w.Header().Set("Retry-After", strconv.Itoa(int(feishuAttemptWindow.Seconds())))
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return false
	}
	bucket.count++
	s.feishuMu.Unlock()
	return true
}

// feishuActorStillAdmin re-reads the operator's record. A binding started before a
// demotion must not complete after it.
func (s *Server) feishuActorStillAdmin(ctx context.Context, username string) bool {
	if strings.TrimSpace(username) == "" || s.deps.AdminStore == nil {
		return false
	}
	user, err := s.deps.AdminStore.GetAdminUserByUsername(ctx, username)
	if err != nil {
		s.deps.Log.Warn("re-checking the Feishu binding actor failed", "err", err)
		return false
	}
	return user.Role == "admin"
}

// feishuSameHost reports whether the portal shares a hostname with this server, which is
// what lets the ticket travel as a cookie instead of in the URL.
func (s *Server) feishuSameHost(portalURL string) bool {
	portal, err := url.Parse(portalURL)
	if err != nil {
		return false
	}
	callback, err := url.Parse(s.deps.Feishu.RedirectURI)
	if err != nil {
		return false
	}
	return strings.EqualFold(portal.Hostname(), callback.Hostname())
}

// setFeishuTicketCookie hands the ticket to the browser. The cookie is host-only (no
// Domain attribute) so it reaches the portal whatever port it listens on, HttpOnly so page
// script cannot read it, and short-lived because the ticket is redeemed within one
// redirect. Secure follows the deployment's real scheme: a browser silently drops a Secure
// cookie on a plain-HTTP origin.
func (s *Server) setFeishuTicketCookie(w http.ResponseWriter, ticket string) {
	maxAge := 120
	if s.deps.Config != nil && s.deps.Config.Feishu.TicketTTLS > 0 {
		maxAge = s.deps.Config.Feishu.TicketTTLS
	}
	http.SetCookie(w, &http.Cookie{
		Name:     feishuTicketCookieName,
		Value:    ticket,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.feishuSecureCookie(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Expires:  time.Now().UTC().Add(time.Duration(maxAge) * time.Second),
	})
}

const feishuTicketCookieName = "aigw_dshgw_ticket"

// feishuSecureCookie follows the deployment's scheme, using the same rule the admin cookie
// does: the callback URL is the one address the browser demonstrably reached.
func (s *Server) feishuSecureCookie() bool {
	if s.deps.Feishu == nil {
		return false
	}
	parsed, err := url.Parse(s.deps.Feishu.RedirectURI)
	if err != nil {
		return true
	}
	return parsed.Scheme == "https"
}

// redirectConsole returns the administrator to the API keys page with a result the page
// turns into a message. It is a relative redirect, so it keeps whichever origin the
// administrator is actually using (a port, a front proxy, a path prefix).
func (s *Server) redirectConsole(w http.ResponseWriter, r *http.Request, result string, keyID int64) {
	target := s.url("/admin/ui/#/keys?feishu=" + url.QueryEscape(result))
	if keyID > 0 {
		target += "&key=" + strconv.FormatInt(keyID, 10)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// redirectFeishuError sends the browser back to the DSH portal, which owns the page the
// person is looking at and can explain what happened in the portal's own words.
func (s *Server) redirectFeishuError(w http.ResponseWriter, r *http.Request, reason string) {
	if s.deps.Feishu == nil || s.deps.Feishu.PortalURL == "" {
		http.Error(w, "feishu login is unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, s.trailingSlash(s.deps.Feishu.PortalURL)+"feishu/error?reason="+url.QueryEscape(reason), http.StatusSeeOther)
}

func (s *Server) trailingSlash(raw string) string { return strings.TrimRight(raw, "/") + "/" }

// feishuNonce is the per-attempt random value that makes a state single-use.
func feishuNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// feishuReason maps a state refusal onto a small, stable vocabulary the portal can
// translate. The distinction that matters to a person is "start again" versus "ask an
// administrator".
func feishuReason(err error) string {
	var stateErr *feishu.StateError
	if errors.As(err, &stateErr) {
		switch stateErr.Reason {
		case "expired", "expiry beyond the configured window":
			return "expired"
		case "replayed":
			return "replay"
		default:
			return "invalid"
		}
	}
	return "invalid"
}

// feishuExchangeReason maps a failed exchange onto the same vocabulary. Credential and
// availability problems are deployment faults: they are logged with detail and reported
// generically, because the person cannot act on them.
func feishuExchangeReason(err error) string {
	var feishuErr *feishu.Error
	if !errors.As(err, &feishuErr) {
		return "error"
	}
	switch feishuErr.Kind {
	case feishu.KindCodeRejected:
		return "expired"
	case feishu.KindRateLimited:
		return "rate_limited"
	case feishu.KindAppUnavailable:
		return "no_app_permission"
	case feishu.KindCredentials:
		return "app_error"
	default:
		return "error"
	}
}

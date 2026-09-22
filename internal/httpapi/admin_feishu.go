package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// Invites signs the long-lived administrator invitation links (M66). It is a second
	// codec rather than a longer TTL on States: an invitation outlives one consent screen by
	// design, and a purpose-bound key keeps one kind of link from signing the other. Nil
	// while the console login flow is off, and then the invitation route does not exist.
	Invites *feishu.StateCodec
	// RedirectURI is the callback URL exactly as registered in the Feishu console: it is
	// sent in the authorization redirect and again in the token exchange, and Feishu
	// refuses the exchange if the two differ.
	RedirectURI string
	// LoginPath, CallbackPath and InvitePath are the paths this server serves, WITHOUT the
	// mount prefix: the mux sees a request only after withBasePath has stripped the prefix,
	// so a pattern written with the prefix would never match (the browser-visible URLs carry
	// it, and startup compares those against the prefix plus these paths).
	LoginPath    string
	CallbackPath string
	InvitePath   string
	// DSHLogin opens the DSH portal login flow; PortalURL is where that flow returns the
	// browser (the dshgw portal), and LoginURL is where the browser starts.
	DSHLogin  bool
	PortalURL string
	LoginURL  string
	// AdminLogin opens the console login flow (M66): an administrator whose Feishu identity
	// is bound to an admin_users row signs in by scanning Feishu's consent page. ConsoleURL
	// is where that flow returns the browser, and the page a refused attempt is explained on.
	AdminLogin bool
	ConsoleURL string
	// ConsoleTickets mints the one-time handoff used when the console and the callback are
	// reached under different host names, where a cookie set here could never arrive. Nil
	// while the console login flow is off.
	ConsoleTickets *feishu.TicketCodec
	// AutoEnableDSH makes a successful binding also opt the key's account in to DSH, so the
	// person can log in immediately instead of waiting for an administrator to press 启用
	// DSH as a second step. See autoEnableDSHForBinding for what it deliberately refuses to
	// do.
	AutoEnableDSH bool
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

// handleFeishuLogin starts one of the two public login flows. It is unauthenticated on
// purpose — any employee may sign in to their own tenant, and any administrator may sign in
// to the console — and rate limited, because it is the route that makes an outbound call
// before anyone is authenticated.
//
// Binding is NOT started here: an administrator's session cookie is scoped to "/admin", so a
// route outside that prefix cannot see it (which is exactly the bug this comment replaces).
// Binding has its own admin route, which mints its state and goes to Feishu in one hop.
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
	mode := r.URL.Query().Get("mode")
	attempt := feishu.Attempt{Flow: feishu.FlowDSHLogin}
	switch mode {
	case "", string(feishu.FlowDSHLogin):
		if !deps.DSHLogin || deps.PortalURL == "" {
			http.NotFound(w, r)
			return
		}
	case string(feishu.FlowAdminLogin):
		// The console login. It is the only other flow with a public entry point; a key
		// binding starts at its admin route (`/admin/api/v1/keys/{id}/feishu/bind`) and an
		// invitation at `/feishu/invite`.
		if !deps.AdminLogin {
			http.NotFound(w, r)
			return
		}
		attempt.Flow = feishu.FlowAdminLogin
	default:
		http.NotFound(w, r)
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
	attempt.Nonce = nonce
	state, err := deps.States.Sign(attempt)
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot start the Feishu flow"))
		return
	}
	s.redirectToFeishu(w, r, state)
}

// redirectToFeishu sends the browser to the consent page for a signed state.
func (s *Server) redirectToFeishu(w http.ResponseWriter, r *http.Request, state string) {
	target, err := s.deps.Feishu.Client.AuthorizeURLFor(state, s.deps.Feishu.RedirectURI)
	if err != nil {
		s.deps.Log.Error("building the Feishu authorization URL failed", "err", err)
		writeAPIError(w, domain.ErrInternal("the Feishu integration is misconfigured"))
		return
	}
	// The browser is leaving for Feishu: nothing about this handoff may be cached.
	feishuNoStore(w.Header())
	http.Redirect(w, r, target, http.StatusFound)
}

// handleFeishuCallback is the single redirect target for every flow. It carries no session
// of its own: the signed state proves who started the attempt, and for a binding or an
// invitation the actor is re-checked here so a demoted administrator cannot finish what they
// started.
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
		s.renderFeishuStop(w, http.StatusBadRequest, feishuStateMessage(reason), "")
		return
	}
	flow := state.Flow
	failFlow := func(result string) {
		switch flow {
		case feishu.FlowBind:
			s.redirectConsole(w, r, result, state.KeyID)
		case feishu.FlowAdminLogin, feishu.FlowAdminInvite:
			// Both console flows end on a page of ours: the person is not signed in (or, for
			// an invitation, may never have seen the console), so there is no page to send
			// them back to with a hidden reason code.
			s.renderFeishuAdminStop(w, flow, result)
		default:
			s.redirectFeishuError(w, r, result)
		}
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
	switch flow {
	case feishu.FlowBind:
		s.finishFeishuBind(w, r, state, identity)
	case feishu.FlowAdminInvite:
		s.finishFeishuAdminInvite(w, r, state, identity)
	case feishu.FlowAdminLogin:
		s.finishFeishuAdminLogin(w, r, identity)
	default:
		s.finishFeishuLogin(w, r, identity)
	}
}

// ---------------------------------------------------------------------------
// Console administrator login and invitation (M66)
// ---------------------------------------------------------------------------

// handleFeishuInvite is the entry point of an administrator invitation link. It is public
// because the person following the link has no session yet — the signed invitation is the
// capability — and rate limited like the other public entry points.
//
// The invitation token is only PEEKED here, never consumed: an invitation link stays usable
// until it is redeemed, so a person who cancels the consent screen (or opens the link twice)
// can simply try again. Each visit mints a fresh short-lived state, which is what Feishu's
// one-time state actually protects.
func (s *Server) handleFeishuInvite(w http.ResponseWriter, r *http.Request) {
	deps := s.deps.Feishu
	if !s.feishuEnabled() || deps.Invites == nil || !deps.AdminLogin {
		http.NotFound(w, r)
		return
	}
	feishuNoStore(w.Header())
	if r.URL.Query().Has("code") || r.URL.Query().Has("state") {
		// Same rule as the login entry point: an authorization code belongs to the callback.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.allowFeishuAttempt(w, r) {
		return
	}
	invite, err := deps.Invites.Peek(r.URL.Query().Get("invite"))
	if err != nil {
		reason := feishuInviteReason(err)
		s.audit(r.Context(), "", "feishu_invite_reject", "admin_user", "", map[string]any{"reason": reason}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, reason)
		return
	}
	ctx := r.Context()
	// The account may have been deleted, disabled or had its invitation regenerated since
	// the link was minted. All three mean the same thing to the person holding the link.
	target, reason, ok := s.inviteTarget(ctx, invite.AdminUserID)
	if !ok || target.InviteNonce != invite.Nonce {
		if ok {
			reason = "invite_revoked"
		}
		s.audit(ctx, invite.Actor, "feishu_invite_reject", "admin_user", inviteUserID(invite.AdminUserID),
			map[string]any{"reason": reason}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, reason)
		return
	}
	if !s.feishuAdminActorStillAdmin(ctx, invite.Actor) {
		s.audit(ctx, invite.Actor, "feishu_invite_reject", "admin_user", inviteUserID(target.ID),
			map[string]any{"reason": "actor is no longer an administrator"}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, "invite_revoked")
		return
	}
	nonce, err := feishuNonce()
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot start the Feishu flow"))
		return
	}
	state, err := deps.States.Sign(feishu.Attempt{
		Flow: feishu.FlowAdminInvite, AdminUserID: target.ID, Actor: invite.Actor,
		Nonce: nonce, Invite: invite.Nonce,
	})
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot start the Feishu flow"))
		return
	}
	s.redirectToFeishu(w, r, state)
}

// finishFeishuAdminLogin signs an administrator in to the console.
//
// The identity is resolved against admin_users, never against the API keys a customer may
// have bound: an open_id that belongs to a customer is not an administrator, whatever else
// it is bound to. That separation is the whole point of the flow (M66 §D2).
func (s *Server) finishFeishuAdminLogin(w http.ResponseWriter, r *http.Request, identity feishu.Identity) {
	ctx := r.Context()
	user, err := s.deps.AdminStore.FindAdminUserByFeishuOpenID(ctx, identity.OpenID)
	if err != nil {
		s.deps.Log.Error("resolving the Feishu identity failed", "err", err)
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "error")
		return
	}
	if user == nil {
		s.audit(ctx, "", "feishu_login_reject", "admin_user", "", map[string]any{"reason": "unbound open id", "open_id": identity.OpenID}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "unbound")
		return
	}
	if user.Status != domain.AdminActive {
		s.audit(ctx, "", "feishu_login_reject", "admin_user", user.Username,
			map[string]any{"reason": "account " + user.Status, "open_id": identity.OpenID}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "disabled")
		return
	}
	s.issueAdminFeishuSession(w, r, user, "feishu", identity.OpenID)
}

// finishFeishuAdminInvite binds the identity that just authorized to the account the
// invitation names, activates it and signs that browser in.
//
// The row is re-read and the invitation handle re-compared: between minting the link and
// redeeming it the account may have been disabled, deleted, or had its invitation
// regenerated (which is how an operator retires a link that was sent to the wrong person).
func (s *Server) finishFeishuAdminInvite(w http.ResponseWriter, r *http.Request, state feishu.State, identity feishu.Identity) {
	ctx := r.Context()
	target, reason, ok := s.inviteTarget(ctx, state.AdminUserID)
	if !ok || target.InviteNonce != state.Invite {
		if ok {
			reason = "invite_revoked"
		}
		s.audit(ctx, state.Actor, "feishu_bind_reject", "admin_user", inviteUserID(state.AdminUserID),
			map[string]any{"reason": reason, "open_id": identity.OpenID}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, reason)
		return
	}
	if !s.feishuAdminActorStillAdmin(ctx, state.Actor) {
		s.audit(ctx, state.Actor, "feishu_bind_reject", "admin_user", inviteUserID(target.ID),
			map[string]any{"reason": "actor is no longer an administrator"}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, "invite_revoked")
		return
	}
	previous := target.FeishuOpenID
	// The binding, the redeemed invitation and the activation are one statement, so no
	// window exists in which the account is bound but still unusable (see the store method).
	if err := s.deps.AdminStore.BindAdminUserFeishu(ctx, target.ID, domain.FeishuBinding{
		OpenID: identity.OpenID, UnionID: identity.UnionID, Name: identity.Name, BoundBy: "invite:" + state.Actor,
	}); err != nil {
		reason := "error"
		if apiErr := toAPIError(err); apiErr.Status == http.StatusConflict {
			reason = "conflict"
		}
		s.audit(ctx, state.Actor, "feishu_bind_reject", "admin_user", inviteUserID(target.ID),
			map[string]any{"reason": reason, "open_id": identity.OpenID}, "failed")
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, reason)
		return
	}
	s.audit(ctx, state.Actor, "feishu_bind", "admin_user", inviteUserID(target.ID), map[string]any{
		"open_id": identity.OpenID, "union_id": identity.UnionID, "name": identity.Name,
		"previous_open_id": previous, "invited": true,
	}, "ok")
	// Re-read: the write above activated the account and is also what the session is for.
	bound, err := s.deps.AdminStore.GetAdminUser(ctx, target.ID)
	if err != nil || bound == nil {
		s.deps.Log.Error("re-reading an invited administrator failed", "err", err, "admin_user", target.ID)
		s.renderFeishuAdminStop(w, feishu.FlowAdminInvite, "error")
		return
	}
	s.issueAdminFeishuSession(w, r, bound, "feishu_invite", identity.OpenID)
}

// inviteTarget loads the account an invitation names and says whether the link may still be
// used. A missing row and a disabled account are both "this link is no longer good" as far as
// the person holding it is concerned; the reason differs so the page can tell them whether to
// ask for a new link or to ask why the account was stopped. An unexpected store failure is
// logged rather than dressed up as a revoked invitation — that one is the operator's problem.
func (s *Server) inviteTarget(ctx context.Context, id int64) (*domain.AdminUser, string, bool) {
	user, err := s.deps.AdminStore.GetAdminUser(ctx, id)
	if err != nil {
		if toAPIError(err).Status != http.StatusNotFound {
			s.deps.Log.Error("loading the administrator an invitation names failed", "err", err, "admin_user", id)
		}
		return nil, "invite_revoked", false
	}
	if user.Status == domain.AdminDisabled {
		return user, "disabled", false
	}
	return user, "", true
}

// issueAdminFeishuSession hands the just-proven administrator a console session and sends the
// browser into the console.
//
// How it hands it over depends on where the console lives relative to the callback, because a
// session cookie is scoped to a host name and the callback can only ever run on the origin
// registered with Feishu:
//
//   - same host name (the usual case, and what a deployment without feishu.console_url has):
//     set the cookie here and redirect, exactly as the password login does;
//   - different host name (a LAN console with a public callback, say): mint a one-time ticket
//     and send the browser to the console's own origin to redeem it, which is where the
//     cookie can be set. This is the console's half of what M61 did for the DSH portal.
func (s *Server) issueAdminFeishuSession(w http.ResponseWriter, r *http.Request, user *domain.AdminUser, method, openID string) {
	ctx := r.Context()
	if s.deps.Admin == nil {
		writeAPIError(w, domain.ErrUnsupported("the management API is disabled"))
		return
	}
	if s.feishuConsoleNeedsTicket() {
		s.handOffAdminFeishuTicket(w, r, user, method, openID)
		return
	}
	session, err := s.deps.Admin.IssueSession(ctx, user)
	if err != nil {
		s.deps.Log.Error("issuing an administrator session failed", "err", err, "admin_user", user.Username)
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "error")
		return
	}
	s.setAdminCookie(w, session)
	s.audit(ctx, user.Username, "login", "admin_user", user.Username,
		map[string]any{"method": method, "open_id": openID}, "ok")
	feishuNoStore(w.Header())
	http.Redirect(w, r, s.adminConsoleURL(), http.StatusSeeOther)
}

// handOffAdminFeishuTicket sends the browser to the console with a one-time ticket instead of
// a cookie. The session is issued when the ticket is redeemed (handleAdminFeishuSession), so
// the cookie is written by the console's own origin.
func (s *Server) handOffAdminFeishuTicket(w http.ResponseWriter, r *http.Request, user *domain.AdminUser, method, openID string) {
	deps := s.deps.Feishu
	if deps.ConsoleTickets == nil {
		// A misconfiguration rather than a user error: the deployment says the console lives
		// elsewhere but was built without the codec that gets the browser there.
		s.deps.Log.Error("the console login needs a ticket handoff but no ticket codec is configured",
			"console_url", deps.ConsoleURL)
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "error")
		return
	}
	nonce, err := feishuNonce()
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot start the Feishu flow"))
		return
	}
	wire, ticket, err := deps.ConsoleTickets.IssueConsole(user.ID, openID, nonce)
	if err != nil {
		s.deps.Log.Error("issuing a console ticket failed", "err", err, "admin_user", user.Username)
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "error")
		return
	}
	s.audit(r.Context(), user.Username, "login", "admin_user", user.Username, map[string]any{
		"method": method, "open_id": openID, "handoff": "ticket", "role": user.Role,
		"expires_at": time.Unix(ticket.Expires, 0).UTC().Format(time.RFC3339),
	}, "ok")
	feishuNoStore(w.Header())
	http.Redirect(w, r, s.consoleTicketURL(wire), http.StatusSeeOther)
}

// consoleTicketURL is where the callback sends a browser that must be signed in on another
// host: the console's origin, the redeem path this server serves, and the ticket.
func (s *Server) consoleTicketURL(ticket string) string {
	origin := s.consoleOrigin()
	return origin + s.url("/admin/feishu/session") + "?ticket=" + url.QueryEscape(ticket)
}

// consoleOrigin is the scheme://host[:port] part of the configured console URL, or empty when
// the console is served from the callback's own origin.
func (s *Server) consoleOrigin() string {
	if s.deps.Feishu == nil {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(s.deps.Feishu.ConsoleURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// feishuConsoleNeedsTicket reports whether the console is reached under a different host name
// than the callback. Only the host name matters: cookies ignore the port, so a console on
// :8088 and a callback on :8090 of the same host still share the session.
func (s *Server) feishuConsoleNeedsTicket() bool {
	deps := s.deps.Feishu
	if deps == nil || deps.ConsoleTickets == nil || strings.TrimSpace(deps.ConsoleURL) == "" {
		// Without a configured console URL the console is where the callback is.
		return false
	}
	console, err := url.Parse(strings.TrimSpace(deps.ConsoleURL))
	if err != nil || console.Host == "" {
		return false
	}
	callback, err := url.Parse(strings.TrimSpace(deps.RedirectURI))
	if err != nil || callback.Host == "" {
		return false
	}
	return !strings.EqualFold(console.Hostname(), callback.Hostname())
}

// handleAdminFeishuSession redeems a console ticket on the console's own origin and sets the
// session cookie there. It is public because the browser arriving here has no session yet —
// the ticket is the capability — and it is single-use, short-lived and bound to one account.
func (s *Server) handleAdminFeishuSession(w http.ResponseWriter, r *http.Request) {
	deps := s.deps.Feishu
	if !s.feishuEnabled() || deps.ConsoleTickets == nil || !deps.AdminLogin {
		http.NotFound(w, r)
		return
	}
	feishuNoStore(w.Header())
	raw := r.URL.Query().Get("ticket")
	ticket, err := deps.ConsoleTickets.VerifyConsoleTicket(raw)
	if err != nil {
		s.deps.Log.Warn("a console ticket was refused", "err", err)
		s.audit(r.Context(), "", "feishu_login_reject", "feishu_callback", "", map[string]any{"reason": "console ticket"}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, feishuTicketReason(err))
		return
	}
	// Single use: the nonce is spent here rather than at the verifier, because this is the
	// side that consumes the ticket (the callback only mints it).
	if !s.feishuConsoleTickets.Consume(ticket.Nonce) {
		s.audit(r.Context(), "", "feishu_login_reject", "admin_user", "", map[string]any{"reason": "console ticket replay"}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "expired")
		return
	}
	user, err := s.deps.AdminStore.GetAdminUser(r.Context(), ticket.AdminUserID)
	if err != nil || user.Status != domain.AdminActive {
		// The account may have been disabled between the consent screen and this redemption;
		// the ticket proves the identity, never the right to sign in.
		s.audit(r.Context(), "", "feishu_login_reject", "admin_user", inviteUserID(ticket.AdminUserID),
			map[string]any{"reason": "account is not active"}, "denied")
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "disabled")
		return
	}
	session, err := s.deps.Admin.IssueSession(r.Context(), user)
	if err != nil {
		s.deps.Log.Error("issuing an administrator session failed", "err", err, "admin_user", user.Username)
		s.renderFeishuAdminStop(w, feishu.FlowAdminLogin, "error")
		return
	}
	s.setAdminCookie(w, session)
	s.audit(r.Context(), user.Username, "login", "admin_user", user.Username,
		map[string]any{"method": "feishu_ticket", "open_id": ticket.OpenID}, "ok")
	http.Redirect(w, r, s.adminConsolePath(), http.StatusSeeOther)
}

// feishuTicketReason maps a ticket refusal onto the person-facing vocabulary. An expired
// ticket and a used one read the same way to them: start again.
func feishuTicketReason(err error) string {
	var ticketErr *feishu.TicketError
	if errors.As(err, &ticketErr) && ticketErr.Reason == "expired" {
		return "expired"
	}
	return "invalid"
}

// adminConsolePath is the console's path on this server, used for redirects that must stay on
// the origin the browser is already talking to (redeeming a ticket, for instance). The
// configured console URL is for the other case: sending the browser to another host.
func (s *Server) adminConsolePath() string { return s.url("/admin/ui/") }

// adminConsoleURL is where an administrator lands after signing in (or after being told why
// they could not). The console is served from one mount, and this is the only place that
// builds its address.
func (s *Server) adminConsoleURL() string {
	if s.deps.Feishu != nil && strings.TrimSpace(s.deps.Feishu.ConsoleURL) != "" {
		return strings.TrimSpace(s.deps.Feishu.ConsoleURL)
	}
	return s.adminConsolePath()
}

// feishuAdminActorStillAdmin re-reads the operator who minted an invitation or started a
// binding: a demotion or a deletion retires whatever they had started.
func (s *Server) feishuAdminActorStillAdmin(ctx context.Context, username string) bool {
	if strings.TrimSpace(username) == "" || s.deps.AdminStore == nil {
		return false
	}
	user, err := s.deps.AdminStore.GetAdminUserByUsername(ctx, username)
	if err != nil {
		s.deps.Log.Warn("re-checking the Feishu administrator actor failed", "err", err, "actor", username)
		return false
	}
	return user.Role == domain.RoleAdmin && user.Status == domain.AdminActive
}

// inviteUserID renders an administrator id for an audit row. An invitation names its target
// by id, and a deleted account has no username left to report.
func inviteUserID(id int64) string { return strconv.FormatInt(id, 10) }

// renderFeishuStop explains a callback that could not be attributed to a flow. It is
// deliberately plain: no script, no styling beyond the browser's, and no values echoed back.
// target is where the "go back" link points; empty means the console's API keys page, which
// is where an administrator whose key binding failed wants to be.
func (s *Server) renderFeishuStop(w http.ResponseWriter, status int, message, target string) {
	console := s.url("/admin/ui/#/keys")
	label := "返回控制台 API Keys"
	if strings.TrimSpace(target) != "" {
		console, label = target, "返回控制台"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	body := "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width\"><title>飞书登录</title></head><body>" +
		"<h1>飞书登录未能完成</h1><p>" + message + "</p>" +
		"<p><a href=\"" + console + "\">" + label + "</a></p>" +
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

// ---------------------------------------------------------------------------
// Console administrator messages
// ---------------------------------------------------------------------------

// renderFeishuAdminStop explains a console login or invitation that did not complete. The
// console flows get their own page rather than a redirect with a reason code: on a login
// failure there is no console page to return to (the person front of it is not signed in
// yet), and on an invitation failure the person may never have seen the console at all.
func (s *Server) renderFeishuAdminStop(w http.ResponseWriter, flow feishu.Flow, reason string) {
	title := "飞书登录未能完成"
	message := feishuAdminMessage(reason)
	if flow == feishu.FlowAdminInvite {
		title = "管理员邀请未能完成"
		if reason == "invite_revoked" || reason == "invite_expired" {
			message += "若你仍需要这个账号，请让管理员重新生成一条邀请链接。"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	feishuNoStore(w.Header())
	w.WriteHeader(http.StatusForbidden)
	body := "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width\"><title>" + title + "</title></head><body>" +
		"<h1>" + title + "</h1><p>" + message + "</p>" +
		"<p><a href=\"" + s.adminConsoleURL() + "\">返回控制台登录</a></p>" +
		"</body></html>"
	_, _ = io.WriteString(w, body)
}

// feishuAdminMessage is the console's vocabulary. It says what happened and who can fix it,
// and it never repeats anything Feishu told us: a misconfiguration is logged, not shown.
func feishuAdminMessage(reason string) string {
	switch reason {
	case "unbound":
		return "这个飞书账号还没有绑定任何管理员账号。请联系管理员在控制台「管理员」页生成一条邀请链接。"
	case "disabled":
		return "该管理员账号已被停用，无法登录。请联系其他管理员重新启用，或用其他账号登录。"
	case "cancelled":
		return "已取消授权，没有做任何改动。可以重新点击「飞书扫码登录」再试一次。"
	case "expired":
		return "这次授权已经超时（授权页停留太久）。请回到控制台登录页重新发起。"
	case "replay", "invalid":
		return "这次授权请求无法校验（链接不完整、已被使用或被修改）。请重新发起。"
	case "invite_expired":
		return "这条邀请链接已经过期。"
	case "invite_revoked":
		return "这条邀请链接已失效：它可能已经被使用过，或者管理员重新生成了新的链接。"
	case "conflict":
		return "这个飞书账号已经绑定到另一个管理员账号了。请先在那边解绑，或换一个飞书账号。"
	case "no_app_permission":
		return "你在飞书侧没有该应用的使用权限，请联系飞书管理员把可用范围加上你。"
	case "app_error":
		return "飞书应用凭据或可用范围有问题，请检查网关的 feishu 配置与飞书后台。"
	case "rate_limited":
		return "尝试过于频繁，请稍后再试。"
	default:
		return "飞书登录未能完成，请重试；若持续失败请联系管理员查看网关日志。"
	}
}

// feishuInviteReason maps a refused invitation token onto that vocabulary. An expired link
// and a retired one are different answers for the person holding it: the first needs the
// administrator to generate a new one, the second usually means somebody already used it.
func feishuInviteReason(err error) string {
	var stateErr *feishu.StateError
	if errors.As(err, &stateErr) {
		switch stateErr.Reason {
		case "expired":
			return "invite_expired"
		case "replayed":
			return "invite_revoked"
		default:
			return "invalid"
		}
	}
	return "invalid"
}

// finishFeishuBind writes the binding the administrator asked for.
func (s *Server) finishFeishuBind(w http.ResponseWriter, r *http.Request, state feishu.State, identity feishu.Identity) {
	ctx := r.Context()
	// The role is re-checked against the live record: a state minted before a demotion or a
	// deletion must not complete a binding.
	if !s.feishuAdminActorStillAdmin(ctx, state.Actor) {
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
	// A binding means "this person should be able to get in", so the account is opted in to
	// DSH here rather than leaving a second step to remember. A failure to provision is
	// reported to the console but does not undo the binding: who this person is and whether
	// their tenant is running are two different facts, and only the second one is retryable.
	dsh := s.autoEnableDSHForBinding(ctx, state.Actor, key)
	s.redirectConsole(w, r, result, key.ID, dsh)
}

// dshBindingOutcome is what the console tells the administrator about the account's DSH state
// after a binding: the identity is written either way, so this only says whether the person
// can sign in yet.
type dshBindingOutcome struct {
	// State is one of "enabled", "already", "declined", "failed", "off".
	State  string
	Tenant string
	// Reason is a short, non-secret explanation for "declined" and "failed".
	Reason string
}

// autoEnableDSHForBinding opts the key's account in to DSH when it has never been enabled.
//
// It deliberately does NOT touch an account that was enabled and then explicitly disabled
// (a dsh_tenant with dsh_enabled false): an administrator pressed 停用 for a reason, and
// silently undoing that on an unrelated binding — someone binding a second key, say — is the
// kind of surprise that later reads as a bug. That case is reported as "declined" so the
// console can say exactly what to do.
func (s *Server) autoEnableDSHForBinding(ctx context.Context, actor string, key *domain.APIKey) dshBindingOutcome {
	if s.deps.Feishu == nil || !s.deps.Feishu.AutoEnableDSH {
		return dshBindingOutcome{State: "off"}
	}
	account, err := s.deps.AdminStore.GetAccount(ctx, key.AccountID)
	if err != nil {
		s.deps.Log.Warn("loading the account of a bound key failed", "err", err, "account", key.AccountID)
		return dshBindingOutcome{State: "failed", Reason: "账号信息读取失败"}
	}
	if account.DSHEnabled {
		return dshBindingOutcome{State: "already", Tenant: account.DshTenant}
	}
	if strings.TrimSpace(account.DshTenant) != "" {
		return dshBindingOutcome{State: "declined", Tenant: account.DshTenant, Reason: "该账号此前被显式停用"}
	}
	if s.deps.DshgwAdmin == nil {
		return dshBindingOutcome{State: "failed", Reason: "本机 dshgw provisioning 通道未配置"}
	}
	accounts, ok := s.deps.Accounts.(AccountAdmin)
	if !ok || accounts == nil {
		return dshBindingOutcome{State: "failed", Reason: "账户接口不可用"}
	}
	tenant, err := s.provisionAccountDSH(ctx, actor, accounts, s.deps.AdminStore, account, nil)
	if err != nil {
		s.deps.Log.Warn("enabling dsh for a bound key failed", "err", err, "account", account.ID)
		// The reason is truncated: this string travels in a URL and is shown to a person,
		// while the full error is in the log.
		return dshBindingOutcome{State: "failed", Reason: truncateReason(err.Error())}
	}
	return dshBindingOutcome{State: "enabled", Tenant: tenant}
}

// truncateReason keeps a failure explanation short enough for a redirect while staying
// recognisable; the full text is always in the log.
func truncateReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) <= 120 {
		return reason
	}
	return string([]rune(reason)[:117]) + "…"
}

// finishFeishuLogin resolves the identity to an account that may use the DSH gateway and hands
// the browser a short-lived ticket — for the tenant, or for the key picker when the account has
// more than one usable key.
//
// Since M72 the identity is the ACCOUNT's (accounts.feishu_open_id): the binding says which
// account a Feishu person is, the OAuth exchange just proved which person this browser is, and
// the account's DSH entitlement decides whether they may enter. The key-level lookup is a
// pre-migration fallback for a deployment whose startup backfill did not run: without it, an
// upgrade would lock people out until an operator finished the move by hand.
func (s *Server) finishFeishuLogin(w http.ResponseWriter, r *http.Request, identity feishu.Identity) {
	ctx := r.Context()
	deps := s.deps.Feishu
	if !deps.DSHLogin || deps.Tickets == nil || deps.PortalURL == "" {
		http.NotFound(w, r)
		return
	}
	account, err := s.deps.AdminStore.FindAccountByFeishuOpenID(ctx, identity.OpenID)
	if err != nil {
		s.deps.Log.Error("resolving the Feishu identity failed", "err", err)
		s.redirectFeishuError(w, r, "error")
		return
	}
	if account == nil {
		account, err = s.feishuAccountFromLegacyKeyBinding(ctx, identity.OpenID)
		if err != nil {
			s.deps.Log.Error("resolving the Feishu identity through its key binding failed", "err", err)
			s.redirectFeishuError(w, r, "error")
			return
		}
	}
	if account == nil {
		s.audit(ctx, "", "feishu_login_reject", "account", "", map[string]any{"reason": "unbound open id", "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "unbound")
		return
	}
	autoEnable := s.deps.Config != nil && s.deps.Config.Dshgw.AutoEnable
	switch {
	case account.Status != "" && account.Status != "active":
		// Suspended or closed: the identity is fine, the account is not.
		s.audit(ctx, "", "feishu_login_reject", "account", account.Name, map[string]any{"reason": "account " + account.Status, "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "account_status")
		return
	case !accountDSHEffective(autoEnable, account):
		s.audit(ctx, "", "feishu_login_reject", "account", account.Name, map[string]any{"reason": "dsh disabled", "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "dsh_disabled")
		return
	}
	// From here the tenant has to exist. With auto_enable an entitled account without one is
	// provisioned by the portal's own authorization call a moment later (dshgw asks aigw before
	// it issues a session), so this path only has to refuse the case the deployment did not opt
	// into: an account whose DSH was enabled by hand but whose tenant was never recorded.
	tenant := strings.TrimSpace(account.DshTenant)
	if tenant == "" && !autoEnable {
		s.audit(ctx, "", "feishu_login_reject", "account", account.Name, map[string]any{"reason": "tenant unassigned", "open_id": identity.OpenID}, "denied")
		s.redirectFeishuError(w, r, "tenant_missing")
		return
	}

	// Which key this session is recorded against (M72): with more than one usable key the person
	// chooses, and the choice travels back through the portal. The picker is a separate ticket
	// because it names an account rather than a tenant, and the portal is the side that owns the
	// form. 0/1 keys keep the single-step login of M61.
	keys := s.accountKeyChoices(ctx, account.ID)
	if len(keys) > 1 || tenant == "" {
		// tenant == "" (auto-provisioning at the portal) also goes through the picker: it is a
		// page the portal already owns, so the first login of an account needs no special case
		// there, and the key it picks is the tenant's own worker key when there is nothing else.
		s.handOffFeishuKeyPick(w, r, account, identity)
		return
	}
	keyID := int64(0)
	if len(keys) == 1 {
		if id, ok := keys[0]["id"].(int64); ok {
			keyID = id
		}
	}
	nonce, err := feishuNonce()
	if err != nil {
		s.redirectFeishuError(w, r, "error")
		return
	}
	wire, ticket, err := deps.Tickets.Issue(tenant, keyID, account.ID, identity.OpenID, nonce)
	if err != nil {
		s.deps.Log.Error("issuing a DSH login ticket failed", "err", err)
		s.redirectFeishuError(w, r, "error")
		return
	}
	s.audit(ctx, "", "feishu_dsh_login", "account", account.Name, map[string]any{
		"tenant": tenant, "open_id": identity.OpenID, "key_id": keyID,
		"expires_at": time.Unix(ticket.Expires, 0).UTC().Format(time.RFC3339),
	}, "ok")
	// The ticket travels as a host-only cookie, which is what makes it reach the portal on
	// its own port: cookies are scoped to a host, not to a port. A deployment whose portal
	// lives on a different host gets it in the query string as well, because there the
	// cookie would never arrive.
	if s.feishuSameHost(deps.PortalURL) {
		s.setFeishuTicketCookie(w, wire)
		redirectFeishuHandoff(w, r, deps.RedirectURI, s.trailingSlash(deps.PortalURL)+"login/feishu")
		return
	}
	target := s.trailingSlash(deps.PortalURL) + "login/feishu?ticket=" + url.QueryEscape(wire)
	s.deps.Log.Warn("the DSH portal is on another host; the login ticket travels in the URL", "portal", deps.PortalURL)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// feishuAccountFromLegacyKeyBinding resolves an identity that is still bound to a key (M60) to
// that key's account. It exists so an upgrade in which the startup backfill did not run — or was
// interrupted — does not lock people out of a portal they could use yesterday. The binding stays
// on the key; nothing is written here, and the next start migrates it for real.
func (s *Server) feishuAccountFromLegacyKeyBinding(ctx context.Context, openID string) (*domain.Account, error) {
	key, err := s.deps.AdminStore.FindAPIKeyByFeishuOpenID(ctx, openID)
	if err != nil || key == nil {
		return nil, err
	}
	s.deps.Log.Warn("a Feishu identity was still bound at the key level; the account-level binding is missing",
		"key", key.ID, "account", key.AccountID)
	account, err := s.deps.AdminStore.GetAccount(ctx, key.AccountID)
	if err != nil {
		return nil, err
	}
	return account, nil
}

// handOffFeishuKeyPick continues a login at the portal's key picker (M72 §"多 Key 选择").
//
// The pick ticket names the ACCOUNT rather than a tenant, and the portal renders the choice from
// it. aigw signs it because it is the side that proved the identity: the portal's own wait — a
// key login — cannot reach here at all (there is no Feishu identity involved), so a ticket that
// says "this account may choose" only ever comes from the callback.
//
// The ticket is delivered exactly like a login ticket: as a host-only cookie when the portal
// shares the callback's host, and in the URL when it does not.
func (s *Server) handOffFeishuKeyPick(w http.ResponseWriter, r *http.Request, account *domain.Account, identity feishu.Identity) {
	deps := s.deps.Feishu
	if deps.Tickets == nil {
		// Only reachable in a build whose DSH login flow is off, which the caller already
		// refuses; answering as unavailable beats a nil dereference.
		s.redirectFeishuError(w, r, "error")
		return
	}
	nonce, err := feishuNonce()
	if err != nil {
		s.redirectFeishuError(w, r, "error")
		return
	}
	wire, ticket, err := deps.Tickets.IssueKeyPick(account.ID, identity.OpenID, nonce)
	if err != nil {
		s.deps.Log.Error("issuing a key-pick ticket failed", "err", err)
		s.redirectFeishuError(w, r, "error")
		return
	}
	s.audit(r.Context(), "", "feishu_key_pick", "account", account.Name, map[string]any{
		"open_id": identity.OpenID, "keys": len(s.accountKeyChoices(r.Context(), account.ID)),
		"expires_at": time.Unix(ticket.Expires, 0).UTC().Format(time.RFC3339),
	}, "ok")
	if s.feishuSameHost(deps.PortalURL) {
		s.setFeishuPickCookie(w, wire)
		redirectFeishuHandoff(w, r, deps.RedirectURI, s.trailingSlash(deps.PortalURL)+"login/pick")
		return
	}
	target := s.trailingSlash(deps.PortalURL) + "login/pick?ticket=" + url.QueryEscape(wire)
	s.deps.Log.Warn("the DSH portal is on another host; the key-pick ticket travels in the URL", "portal", deps.PortalURL)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Console binding routes
// ---------------------------------------------------------------------------

// handleAdminBindKeyFeishu is the retired key-level binding entry point (M60 → M72).
//
// It used to sign a state and send the browser to Feishu's consent page — the "扫码绑定" that
// M72 replaces with an administrator picking a person from the directory. The route stays
// registered so that a bookmarked link, an old console tab or a script gets an explanation
// instead of a bare 404. The answer is `unsupported_parameter` (400) rather than 410 for the
// same reason M70 used it for "this deployment has no Feishu": this repository has one shape for
// "the request is understood and will not be served", and a second code for the same situation
// is one more thing for a client to learn.
func (s *Server) handleAdminBindKeyFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.adminActor(w, r, true); !ok {
		return
	}
	writeAPIError(w, domain.ErrUnsupported(
		"binding a Feishu identity to an API key is retired: bind it to the account instead "+
			"(PUT /admin/api/v1/accounts/{id}/feishu, or the organization page's 绑定飞书 button)"))
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

// ---------------------------------------------------------------------------
// Account-level binding routes (M72)
// ---------------------------------------------------------------------------

// handleAdminBindAccountFeishu writes a Feishu identity onto an account: this is what the
// organization page's 绑定飞书 button calls after the administrator picked a person.
//
// It takes the identity as a body rather than looking the person up in the directory, for three
// reasons: the console has just read the directory and already knows the name; a person who left
// the company must still be bindable-by-id (and, more importantly, unbindable); and a lookup here
// would make binding depend on Feishu being reachable, which is not needed to write a row.
//
// It deliberately does NOT touch the account's organization membership. The M70 person-first
// route (PUT /org/feishu/users/{open_id}/account) also links the person's departments; an
// administrator binding one account from the person list is doing one thing, and silently
// changing what its keys are authorized for is the kind of side effect this repository keeps out
// of write paths. Assigning the account to a node is its own action on the same page.
func (s *Server) handleAdminBindAccountFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	account, err := store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body struct {
		OpenID  string `json:"open_id"`
		UnionID string `json:"union_id"`
		Name    string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	openID := strings.TrimSpace(body.OpenID)
	if openID == "" {
		writeAPIError(w, domain.ErrInvalidRequest("open_id is required (the person's ou_… id from the directory)").WithParam("open_id"))
		return
	}
	previous := account.FeishuOpenID
	if previous != "" && previous != openID {
		// Rebinding an account that already carries someone else: allowed (an administrator
		// correcting a person list is exactly what this route is for), and the write below has
		// to succeed — the unique index only stops a second Claimant, not a first one.
		if err := s.releaseFeishuIdentity(r.Context(), store, previous); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	if err := store.BindAccountFeishu(r.Context(), account.ID, domain.FeishuBinding{
		OpenID: openID, UnionID: strings.TrimSpace(body.UnionID), Name: strings.TrimSpace(body.Name),
		BoundBy: actor.Username,
	}); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	result := "bound"
	if previous != "" && previous != openID {
		// Only a DIFFERENT person is a replacement: re-binding the same identity (a double
		// click, or opening the picker again and pressing 绑定) is the same binding, and
		// reporting it as a change would tell an operator something happened that did not.
		result = "replaced"
	}
	s.audit(r.Context(), actor.Username, "feishu_bind", "account", strconv.FormatInt(account.ID, 10), map[string]any{
		"open_id": openID, "name": strings.TrimSpace(body.Name), "previous_open_id": previous,
		"matched_by": "manual",
	}, "ok")
	s.invalidateFeishuDirectory()
	// The identity is what the portal logs in with, so a binding changes who may enter: the
	// verifier caches account rows, and a stale row would keep refusing (or admitting) the
	// previous person for one TTL.
	s.reload(r.Context(), "account feishu binding updated", true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result,
		"account": map[string]any{"id": account.ID, "name": account.Name},
		"feishu":  accountFeishuJSON(&domain.Account{FeishuOpenID: openID, FeishuName: strings.TrimSpace(body.Name)}),
	})
}

// releaseFeishuIdentity frees a Feishu identity that is still attached to whatever account an
// earlier write put it on, so a replacement can claim it.
//
// It exists because the unique index is per identity and enforced at insert time: rebinding an
// account to a different person would otherwise collide, not with its own old value, but with
// the account the new person is currently on (usually none — yet "usually" is not a rule).
// A failure to clear the old holder is reported rather than ignored: the binding below would
// fail anyway, and a vague conflict is harder to act on than "this identity is on account #7".
func (s *Server) releaseFeishuIdentity(ctx context.Context, store AccountAdmin, openID string) error {
	if strings.TrimSpace(openID) == "" {
		return nil
	}
	holder, err := store.FindAccountByFeishuOpenID(ctx, openID)
	if err != nil || holder == nil {
		return err
	}
	changed, err := store.UnbindAccountFeishu(ctx, holder.ID)
	if err != nil {
		return err
	}
	if changed {
		s.audit(ctx, "binding-replacement", "feishu_unbind", "account", strconv.FormatInt(holder.ID, 10),
			map[string]any{"open_id": openID, "reason": "replaced by another binding"}, "ok")
	}
	return nil
}

// handleAdminUnbindAccountFeishu clears an account's Feishu identity and reports whether
// anything changed, so the console can answer idempotently instead of guessing.
func (s *Server) handleAdminUnbindAccountFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	account, err := store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	previous := account.FeishuOpenID
	changed, err := store.UnbindAccountFeishu(r.Context(), account.ID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if changed {
		s.audit(r.Context(), actor.Username, "feishu_unbind", "account", strconv.FormatInt(account.ID, 10),
			map[string]any{"open_id": previous, "account_name": account.Name}, "ok")
		s.reload(r.Context(), "account feishu binding updated", true)
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound": changed, "account_id": account.ID})
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

// setFeishuPickCookie hands the key-pick ticket to the browser (M72). It reuses the login
// ticket's cookie name on purpose: the portal reads one cookie and tells the two kinds apart by
// the mode inside the signed payload, so a second name would only be one more thing to get wrong.
// The lifetime follows pick_ttl_s, which is a separate setting because this ticket covers a form
// submission rather than a redirect.
func (s *Server) setFeishuPickCookie(w http.ResponseWriter, ticket string) {
	maxAge := 120
	if s.deps.Config != nil {
		switch {
		case s.deps.Config.Feishu.PickTTLS > 0:
			maxAge = s.deps.Config.Feishu.PickTTLS
		case s.deps.Config.Feishu.TicketTTLS > 0:
			maxAge = s.deps.Config.Feishu.TicketTTLS
		}
	}
	s.setFeishuCookie(w, feishuTicketCookieName, ticket, maxAge)
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
	s.setFeishuCookie(w, feishuTicketCookieName, ticket, maxAge)
}

func (s *Server) setFeishuCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
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
func (s *Server) redirectConsole(w http.ResponseWriter, r *http.Request, result string, keyID int64, dsh ...dshBindingOutcome) {
	target := s.url("/admin/ui/#/keys?feishu=" + url.QueryEscape(result))
	if keyID > 0 {
		target += "&key=" + strconv.FormatInt(keyID, 10)
	}
	if len(dsh) > 0 && dsh[0].State != "" {
		target += "&dsh=" + url.QueryEscape(dsh[0].State)
		if dsh[0].Tenant != "" {
			target += "&tenant=" + url.QueryEscape(dsh[0].Tenant)
		}
		if dsh[0].Reason != "" {
			target += "&dsh_reason=" + url.QueryEscape(dsh[0].Reason)
		}
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

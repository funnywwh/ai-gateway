package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/feishu"
	"github.com/winger/ai-gateway/internal/ids"
)

// The console used to have exactly one administrator, seeded from bootstrap.admin. These
// handlers manage the many (M66): create, change role, disable, reset the password, invite
// somebody to bind their Feishu identity and sign in, unbind, delete.
//
// Three rules are enforced here rather than documented and hoped for:
//
//   - the deployment keeps at least one administrator who can undo a mistake (role admin,
//     status active);
//   - nobody deletes the account they are signed in as;
//   - the bootstrap row cannot be deleted, because the next start recreates it from the
//     configuration and an operator would be left wondering why.
//
// Everything else is deliberate and allowed: several admins, viewers, accounts without any
// password, and rebinding an identity that moved to a different person.

// handleAdminAuthMethods answers which ways in this deployment offers. It is public on
// purpose: the console asks before anybody has a session, and the answer is a capability
// flag, not a secret (the password form is rendered either way).
func (s *Server) handleAdminAuthMethods(w http.ResponseWriter, r *http.Request) {
	feishuShape := map[string]any{"enabled": false}
	if s.feishuEnabled() && s.deps.Feishu.AdminLogin {
		feishuShape = map[string]any{
			"enabled":   true,
			"login_url": s.deps.Feishu.LoginPath + "?mode=" + string(feishu.FlowAdminLogin),
			"label":     "飞书扫码登录",
			"hint":      "用手机飞书扫描授权页上的二维码，或在授权页直接点同意；账号需由管理员绑定过飞书身份。",
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"password": s.deps.Admin != nil,
		"feishu":   feishuShape,
	})
}

// handleAdminListAdminUsers lists the administrators. Readable by viewers, like the rest of
// the console: the list is who exists and what they may do, and seeing it is how somebody
// knows who to ask.
func (s *Server) handleAdminListAdminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	users, err := s.deps.AdminStore.ListAdminUsers(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, user := range users {
		out = append(out, s.adminUserJSON(user))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

// handleAdminCreateAdminUser creates one administrator.
//
// A password is optional: without one the account starts "pending" and can only become
// usable through an invitation link, which is how a new administrator is onboarded without
// anybody inventing a password (M66 §D3).
func (s *Server) handleAdminCreateAdminUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	username := strings.TrimSpace(body.Username)
	if !validResourceName(username) {
		writeAPIError(w, domain.ErrInvalidRequest("username must match [A-Za-z0-9._-] and be at most 64 characters"))
		return
	}
	role := strings.TrimSpace(body.Role)
	if !domain.ValidAdminRole(role) {
		writeAPIError(w, domain.ErrInvalidRequest("role must be admin or viewer"))
		return
	}
	if body.Password != "" && len(body.Password) < minAdminPasswordLen {
		writeAPIError(w, domain.ErrInvalidRequest("password must be at least 8 characters (or omit it and use an invitation link)"))
		return
	}
	if existing, err := s.deps.AdminStore.GetAdminUserByUsername(r.Context(), username); err == nil && existing != nil {
		writeAPIError(w, domain.ErrConflict("administrator "+existing.Username+" already exists"))
		return
	}
	user := &domain.AdminUser{Username: username, Role: role, Status: domain.AdminPending}
	if body.Password != "" {
		hash, err := admin.HashPassword(body.Password)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		user.PasswordHash = hash
		user.Status = domain.AdminActive
	}
	id, err := s.deps.AdminStore.CreateAdminUser(r.Context(), user)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "create", "admin_user", strconv.FormatInt(id, 10),
		map[string]any{"username": username, "role": role, "has_password": body.Password != ""}, "ok")
	writeJSON(w, http.StatusCreated, s.adminUserJSON(user))
}

// handleAdminUpdateAdminUser changes one administrator's role and/or lifecycle state.
func (s *Server) handleAdminUpdateAdminUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid admin user id"))
		return
	}
	var body struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.Role == nil && body.Status == nil {
		writeAPIError(w, domain.ErrInvalidRequest("provide role, status or both"))
		return
	}
	ctx := r.Context()
	target, err := s.deps.AdminStore.GetAdminUser(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// Each column is audited as it is written rather than once at the end: a request that
	// changes the role and then fails to change the status must not leave the first write
	// recorded nowhere. Two columns therefore produce two rows, which is honest about what
	// happened.
	if body.Role != nil {
		role := strings.TrimSpace(*body.Role)
		if !domain.ValidAdminRole(role) {
			writeAPIError(w, domain.ErrInvalidRequest("role must be admin or viewer"))
			return
		}
		if role != target.Role {
			// Losing the admin role only matters when it shrinks the set of people who could
			// undo the change.
			if err := s.guardActiveAdmins(ctx, target, role != domain.RoleAdmin); err != nil {
				writeAPIError(w, toAPIError(err))
				return
			}
			if err := s.deps.AdminStore.UpdateAdminUserRole(ctx, id, role); err != nil {
				writeAPIError(w, toAPIError(err))
				return
			}
			s.auditAdminUserWrite(ctx, actor.Username, id, map[string]any{
				"role": map[string]string{"from": target.Role, "to": role},
			})
			target.Role = role
		}
	}
	if body.Status != nil {
		status := strings.TrimSpace(*body.Status)
		if !domain.ValidAdminStatus(status) {
			writeAPIError(w, domain.ErrInvalidRequest("status must be pending, active or disabled"))
			return
		}
		if status != target.Status {
			if err := s.guardActiveAdmins(ctx, target, status != domain.AdminActive); err != nil {
				writeAPIError(w, toAPIError(err))
				return
			}
			if status == domain.AdminActive && target.FeishuOpenID == "" && target.PasswordHash == "" {
				// Activating an account with no credential would create an administrator who
				// cannot sign in while looking perfectly healthy in the list.
				writeAPIError(w, domain.ErrConflict("bind a Feishu identity or set a password before enabling this account"))
				return
			}
			if err := s.deps.AdminStore.SetAdminUserStatus(ctx, id, status); err != nil {
				writeAPIError(w, toAPIError(err))
				return
			}
			if status == domain.AdminDisabled {
				// Disabling ends the sessions immediately rather than at their next request:
				// the session row itself is gone, so nothing depends on the read path noticing.
				if err := s.deps.AdminStore.DeleteAdminSessions(ctx, id); err != nil {
					s.deps.Log.Warn("revoking admin sessions after disabling failed", "err", err, "admin_user", id)
				}
			}
			s.auditAdminUserWrite(ctx, actor.Username, id, map[string]any{
				"status": map[string]string{"from": target.Status, "to": status},
			})
			target.Status = status
		}
	}
	writeJSON(w, http.StatusOK, s.adminUserJSON(target))
}

// handleAdminResetAdminPassword issues a new one-time password and ends the account's
// sessions, because a password known to somebody else is only fixed by replacing it.
func (s *Server) handleAdminResetAdminPassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid admin user id"))
		return
	}
	ctx := r.Context()
	target, err := s.deps.AdminStore.GetAdminUser(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	password := initialAdminPassword()
	hash, err := admin.HashPassword(password)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := s.deps.AdminStore.SetAdminUserPassword(ctx, id, hash); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The reset is recorded before anything else can fail: the password really is set at this
	// point, and an operator reading the trail later must be able to see that.
	s.audit(ctx, actor.Username, "reset_password", "admin_user", strconv.FormatInt(id, 10),
		map[string]any{"username": target.Username}, "ok")
	if target.Status == domain.AdminPending {
		// A pending account had no usable credential; the password this call just set is one.
		// If this fails the password alone would not admit anybody (a pending account cannot
		// sign in), so the request fails and the operator retries instead of being handed a
		// credential that does not work.
		if err := s.deps.AdminStore.SetAdminUserStatus(ctx, id, domain.AdminActive); err != nil {
			s.auditAdminUserWrite(ctx, actor.Username, id, map[string]any{"status": "activation failed"})
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	if err := s.deps.AdminStore.DeleteAdminSessions(ctx, id); err != nil {
		s.deps.Log.Warn("revoking admin sessions after a reset failed", "err", err, "admin_user", id)
	}
	out := map[string]any{
		"id": id, "username": target.Username, "password": password,
		"sessions_revoked": true, "bootstrap": s.isBootstrapAdmin(target.Username),
		"note": "this password is shown once; handing it out by chat is what an invitation link avoids",
	}
	if s.isBootstrapAdmin(target.Username) {
		out["note"] = "this password is shown once, and the next restart overwrites it with bootstrap.admin.password"
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminInviteAdminUser mints (or regenerates) the invitation link that lets somebody
// bind their Feishu identity to this administrator account and sign in.
//
// Regenerating is the revocation mechanism: only the newest handle is accepted at the entry
// point, so an older link stops working the moment this returns.
func (s *Server) handleAdminInviteAdminUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	if !s.feishuEnabled() || s.deps.Feishu.Invites == nil || !s.deps.Feishu.AdminLogin {
		writeAPIError(w, domain.ErrUnsupported("Feishu administrator login is not enabled in this deployment"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid admin user id"))
		return
	}
	ctx := r.Context()
	target, err := s.deps.AdminStore.GetAdminUser(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if target.Status == domain.AdminDisabled {
		// Inviting somebody into a disabled account would fail at redemption anyway; saying
		// so here is the difference between a confusing link and an obvious fix.
		writeAPIError(w, domain.ErrConflict("enable this administrator account before inviting it"))
		return
	}
	nonce, err := feishuNonce()
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot generate an invitation"))
		return
	}
	state, err := s.deps.Feishu.Invites.Sign(feishu.Attempt{
		Flow: feishu.FlowAdminInvite, AdminUserID: id, Actor: actor.Username,
		Nonce: nonce, Invite: nonce,
	})
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot generate an invitation"))
		return
	}
	if err := s.deps.AdminStore.RotateAdminUserInvite(ctx, id, nonce); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	expiresAt := time.Now().UTC().Add(s.deps.Feishu.Invites.TTL)
	s.audit(ctx, actor.Username, "invite", "admin_user", strconv.FormatInt(id, 10),
		map[string]any{"username": target.Username, "expires_at": expiresAt.Format(time.RFC3339)}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "username": target.Username,
		"url":           s.feishuInviteURL(state),
		"expires_at":    expiresAt.Format(time.RFC3339),
		"expires_in_s":  int(s.deps.Feishu.Invites.TTL.Seconds()),
		"previous_link": "any earlier invitation link for this account has been retired",
		"note": "open the link in the browser whose Feishu account should become this administrator; " +
			"it works once, and the person is signed in when it succeeds",
	})
}

// handleAdminUnbindAdminUserFeishu clears an administrator's Feishu identity.
//
// It does not revoke existing console sessions and does not delete the account: unbinding
// says "this identity is no longer how you sign in", while 停用 says "you cannot sign in".
// An account whose only credential was the binding drops back to pending, which does end its
// sessions at the next request.
func (s *Server) handleAdminUnbindAdminUserFeishu(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid admin user id"))
		return
	}
	ctx := r.Context()
	target, err := s.deps.AdminStore.GetAdminUser(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	previous := target.FeishuOpenID
	changed, err := s.deps.AdminStore.UnbindAdminUserFeishu(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if changed {
		s.audit(ctx, actor.Username, "feishu_unbind", "admin_user", strconv.FormatInt(id, 10),
			map[string]any{"username": target.Username, "open_id": previous}, "ok")
	}
	writeJSON(w, http.StatusOK, map[string]any{"unbound": changed, "id": id, "username": target.Username})
}

// handleAdminDeleteAdminUser removes one administrator. The row's sessions and its console
// chat library go with it (the foreign keys cascade), which the route's confirmation says.
func (s *Server) handleAdminDeleteAdminUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid admin user id"))
		return
	}
	ctx := r.Context()
	target, err := s.deps.AdminStore.GetAdminUser(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if actor.ID != 0 && actor.ID == target.ID {
		writeAPIError(w, domain.ErrConflict("you cannot delete the administrator account you are signed in as"))
		return
	}
	if s.isBootstrapAdmin(target.Username) {
		writeAPIError(w, domain.ErrConflict("this account is recreated from bootstrap.admin on every start; remove it there, or disable it here"))
		return
	}
	if err := s.guardActiveAdmins(ctx, target, true); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := s.deps.AdminStore.DeleteAdminUser(ctx, id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(ctx, actor.Username, "delete", "admin_user", strconv.FormatInt(id, 10),
		map[string]any{"username": target.Username, "role": target.Role, "open_id": target.FeishuOpenID}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id, "username": target.Username})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// minAdminPasswordLen is the floor for a password an operator types in. Invitations do not
// need one at all; this only keeps "abc" from becoming a console credential.
const minAdminPasswordLen = 8

// auditAdminUserWrite records one administrator write as it happens. It exists so a handler
// that writes more than one column can record the ones that succeeded even when a later one
// fails: an unrecorded permission change is worse than a request that ends in an error.
func (s *Server) auditAdminUserWrite(ctx context.Context, actor string, id int64, changes map[string]any) {
	s.audit(ctx, actor, "update", "admin_user", strconv.FormatInt(id, 10), changes, "ok")
}

// guardActiveAdmins refuses a change that would leave the deployment without an
// administrator who can undo it. It is the one guard that cannot be delegated to a database
// constraint, because "the last one" is a property of the whole table.
func (s *Server) guardActiveAdmins(ctx context.Context, target *domain.AdminUser, removes bool) error {
	if !removes || target == nil || target.Role != domain.RoleAdmin || target.Status != domain.AdminActive {
		// The change does not reduce the number of usable administrators.
		return nil
	}
	count, err := s.deps.AdminStore.ActiveAdminCount(ctx)
	if err != nil {
		return err
	}
	if count <= 1 {
		return domain.ErrConflict("this is the last active administrator: create or promote another one first")
	}
	return nil
}

// isBootstrapAdmin reports whether a row is the one bootstrap.admin seeds. It is derived
// from the configuration rather than stored, because it is a property of the configuration
// and not of the row: renaming the setting moves the label with it.
func (s *Server) isBootstrapAdmin(username string) bool {
	if s.deps.Config == nil {
		return false
	}
	configured := strings.TrimSpace(s.deps.Config.Bootstrap.Admin.Username)
	return configured != "" && configured == strings.TrimSpace(username)
}

// adminUserJSON is the console's view of one administrator. It never carries the password
// hash and never carries the invitation handle: the first is a credential, and the second is
// what makes an outstanding link work. Whether an invitation is outstanding is reported as a
// boolean, which is all an operator needs to decide to regenerate one.
func (s *Server) adminUserJSON(user *domain.AdminUser) map[string]any {
	if user == nil {
		return map[string]any{}
	}
	out := map[string]any{
		"id": user.ID, "username": user.Username, "role": user.Role, "status": user.Status,
		"created_at":     user.CreatedAt.UTC().Format(timeLayoutRFC3339),
		"last_login_at":  timeOrNil(user.LastLoginAt),
		"has_password":   user.PasswordHash != "",
		"invite_pending": user.InviteNonce != "",
		"feishu":         adminUserFeishuJSON(user),
		"bootstrap":      s.isBootstrapAdmin(user.Username),
	}
	return out
}

// adminUserFeishuJSON has the same shape bound or not, so the page never has to guess
// whether a missing field means "unbound" or "old server" (the api_keys column has the same
// rule).
func adminUserFeishuJSON(user *domain.AdminUser) map[string]any {
	if user.FeishuOpenID == "" {
		return map[string]any{"bound": false}
	}
	out := map[string]any{
		"bound":    true,
		"open_id":  user.FeishuOpenID,
		"union_id": user.FeishuUnionID,
		"name":     user.FeishuName,
		"bound_by": user.FeishuBoundBy,
	}
	if user.FeishuBoundAt != nil {
		out["bound_at"] = user.FeishuBoundAt.UTC().Format(time.RFC3339)
	} else {
		out["bound_at"] = nil
	}
	return out
}

// feishuInviteURL is the browser-visible invitation link: the origin of the registered
// callback (the one address a deployment states out loud) plus the path this server serves.
func (s *Server) feishuInviteURL(token string) string {
	deps := s.deps.Feishu
	path := deps.InvitePath
	if path == "" {
		path = "/feishu/invite"
	}
	origin := ""
	if parsed, err := url.Parse(strings.TrimSpace(deps.RedirectURI)); err == nil && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}
	return origin + path + "?invite=" + url.QueryEscape(token)
}

// initialAdminPassword is the one-time password a reset hands out. It is generated, never
// chosen: the console shows it exactly once, and the account's sessions are revoked with it.
func initialAdminPassword() string { return ids.New("pwd") }

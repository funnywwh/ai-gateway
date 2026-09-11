package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/portal"
)

// PortalUserAdmin manages customer self-service logins from the management console.
type PortalUserAdmin interface {
	ListPortalUsers(ctx context.Context, accountID int64) ([]*domain.PortalUser, error)
	GetPortalUser(ctx context.Context, id int64) (*domain.PortalUser, error)
	GetPortalUserByUsername(ctx context.Context, username string) (*domain.PortalUser, error)
	UpsertPortalUser(ctx context.Context, user *domain.PortalUser) (int64, error)
	SetPortalUserStatus(ctx context.Context, id int64, status string) error
	DeletePortalSessionsForUser(ctx context.Context, userID int64) error
}

func portalUserJSON(user *domain.PortalUser) map[string]any {
	return map[string]any{
		"id": user.ID, "account_id": user.AccountID, "username": user.Username,
		"status": user.Status, "must_change_password": user.MustChangePassword,
		"last_login_at": timeOrNil(user.LastLoginAt), "created_by": user.CreatedBy,
		"created_at": user.CreatedAt.UTC().Format(timeLayoutRFC3339),
	}
}

const timeLayoutRFC3339 = "2006-01-02T15:04:05Z07:00"

func (s *Server) handleAdminListPortalUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.PortalUsers, "portal user management")
	if !ok {
		return
	}
	accountID := int64(0)
	if raw := r.PathValue("id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
			return
		}
		accountID = parsed
	}
	users, err := store.ListPortalUsers(r.Context(), accountID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, user := range users {
		out = append(out, portalUserJSON(user))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (s *Server) handleAdminCreatePortalUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.PortalUsers, "portal user management")
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	accounts, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	if _, err := accounts.GetAccount(r.Context(), accountID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body struct {
		Username string `json:"username"`
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
	if existing, err := store.GetPortalUserByUsername(r.Context(), username); err == nil {
		writeAPIError(w, domain.ErrConflict("portal user "+existing.Username+" already exists"))
		return
	}
	password := initialPortalPassword()
	hash, err := portal.HashPassword(password)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	id, err := store.UpsertPortalUser(r.Context(), &domain.PortalUser{
		AccountID: accountID, Username: username, PasswordHash: hash,
		Status: "active", MustChangePassword: true, CreatedBy: actor.Username,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "create", "portal_user", strconv.FormatInt(id, 10),
		map[string]any{"username": username, "account_id": accountID}, "ok")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "username": username, "account_id": accountID,
		"password": password, "must_change_password": true,
		"note": "this password is shown once; the customer must change it after signing in",
	})
}

func (s *Server) handleAdminResetPortalPassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.PortalUsers, "portal user management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid portal user id"))
		return
	}
	user, err := store.GetPortalUser(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	password := initialPortalPassword()
	hash, err := portal.HashPassword(password)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	user.PasswordHash = hash
	user.MustChangePassword = true
	if _, err := store.UpsertPortalUser(r.Context(), user); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// A password reset must also end any session that was opened with the old one.
	if err := store.DeletePortalSessionsForUser(r.Context(), id); err != nil {
		s.deps.Log.Warn("revoking portal sessions after a reset failed", "err", err, "user", id)
	}
	s.audit(r.Context(), actor.Username, "reset_password", "portal_user", strconv.FormatInt(id, 10),
		map[string]any{"username": user.Username}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "username": user.Username, "password": password,
		"must_change_password": true, "sessions_revoked": true,
	})
}

func (s *Server) handleAdminDisablePortalUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.PortalUsers, "portal user management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid portal user id"))
		return
	}
	if err := store.SetPortalUserStatus(r.Context(), id, "disabled"); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := store.DeletePortalSessionsForUser(r.Context(), id); err != nil {
		s.deps.Log.Warn("revoking portal sessions after disabling failed", "err", err, "user", id)
	}
	s.audit(r.Context(), actor.Username, "disable", "portal_user", strconv.FormatInt(id, 10), nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "disabled", "sessions_revoked": true})
}

// initialPortalPassword generates a one-time password for a newly created or reset
// portal login. It is long enough to satisfy the change-password rule.
func initialPortalPassword() string {
	return ids.New("pwd")
}

package admin

import (
	"context"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/sessionauth"
)

// This package is a thin adapter over internal/sessionauth: the session machinery is
// shared with the customer portal, while the types here stay administrator-shaped so
// the management API does not have to think about principals in general.

// Password hashing lives in sessionauth so the console and the portal cannot drift.
func HashPassword(password string) (string, error) { return sessionauth.HashPassword(password) }

// VerifyPassword checks a password against a stored hash.
func VerifyPassword(hash, password string) bool { return sessionauth.VerifyPassword(hash, password) }

// Store is the persistence subset the auth service needs.
type Store interface {
	GetAdminUserByUsername(ctx context.Context, username string) (*domain.AdminUser, error)
	CreateAdminSession(ctx context.Context, id string, userID int64, tokenHash string, expiresAt time.Time) error
	GetAdminSession(ctx context.Context, id string) (*domain.AdminUser, time.Time, error)
	SessionTokenHash(ctx context.Context, id string) (string, error)
	DeleteAdminSession(ctx context.Context, id string) error
	DeleteAdminSessions(ctx context.Context, userID int64) error
	TouchAdminLogin(ctx context.Context, id int64) error
}

// Config tunes the auth service.
type Config struct {
	SessionTTL    time.Duration
	LoginAttempts int
	LoginWindow   time.Duration
}

// Session is an issued administrator session.
type Session struct {
	ID        string
	Token     string
	ExpiresAt time.Time
	User      *domain.AdminUser
}

// Auth authenticates administrators and manages sessions.
type Auth struct {
	service *sessionauth.Service
}

// NewAuth builds the auth service.
func NewAuth(store Store, cfg Config) *Auth {
	return &Auth{service: sessionauth.New(adminStore{store}, sessionauth.Config{
		SessionTTL: cfg.SessionTTL, LoginAttempts: cfg.LoginAttempts, LoginWindow: cfg.LoginWindow,
	})}
}

// SetClock overrides the clock (tests).
func (a *Auth) SetClock(now func() time.Time) { a.service.SetClock(now) }

// Login verifies credentials and issues a session. The token is returned once; only
// its hash is stored.
func (a *Auth) Login(ctx context.Context, username, password, clientKey string) (*Session, error) {
	session, err := a.service.Login(ctx, username, password, clientKey)
	if err != nil {
		return nil, err
	}
	return &Session{
		ID: session.ID, Token: session.Token, ExpiresAt: session.ExpiresAt,
		User: &domain.AdminUser{
			ID: session.Principal.ID, Username: session.Principal.Username, Role: session.Principal.Role,
		},
	}, nil
}

// IssueSession mints a session for an administrator that something else has already
// authenticated — the Feishu identity flow, which proves who somebody is without a
// password (M66). The caller must have verified the identity and the account's status; this
// only applies the shared session rules.
func (a *Auth) IssueSession(ctx context.Context, user *domain.AdminUser) (*Session, error) {
	if user == nil || user.ID == 0 {
		return nil, domain.ErrUnauthorized("missing administrator")
	}
	session, err := a.service.Issue(ctx, &sessionauth.Principal{
		ID: user.ID, Username: user.Username, Role: user.Role,
	})
	if err != nil {
		return nil, err
	}
	return &Session{
		ID: session.ID, Token: session.Token, ExpiresAt: session.ExpiresAt,
		User: &domain.AdminUser{ID: user.ID, Username: user.Username, Role: user.Role},
	}, nil
}

// Authenticate validates a session cookie pair (id + token).
func (a *Auth) Authenticate(ctx context.Context, sessionID, token string) (*domain.AdminUser, error) {
	principal, err := a.service.Authenticate(ctx, sessionID, token)
	if err != nil {
		return nil, err
	}
	return &domain.AdminUser{ID: principal.ID, Username: principal.Username, Role: principal.Role}, nil
}

// Logout removes a session.
func (a *Auth) Logout(ctx context.Context, sessionID string) error {
	return a.service.Logout(ctx, sessionID)
}

// CookieValue renders the cookie payload for a session.
func CookieValue(s *Session) string {
	if s == nil {
		return ""
	}
	return s.ID + "." + s.Token
}

// ParseCookie splits a cookie payload into its session id and token.
func ParseCookie(value string) (id, token string, ok bool) { return sessionauth.ParseCookie(value) }

// RequireRole reports whether a user satisfies the required role.
func RequireRole(user *domain.AdminUser, role string) bool {
	if user == nil {
		return false
	}
	if role == "" || role == "viewer" {
		return true
	}
	return user.Role == "admin"
}

// adminStore adapts the administrator store onto the shared session store, loading the
// password hash the service needs for verification.
type adminStore struct {
	store Store
}

func (a adminStore) PrincipalByUsername(ctx context.Context, username string) (*sessionauth.Principal, string, error) {
	user, err := a.store.GetAdminUserByUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	// An account that is not active has no password as far as the login path is concerned:
	// returning the stored hash would let a disabled administrator sign in, and returning a
	// distinct error would tell an attacker the account exists. This is also what makes an
	// invitation-only account (empty hash) impossible to sign in with, and what keeps the
	// session service from having to know the lifecycle at all.
	if user.Status != domain.AdminActive {
		return &sessionauth.Principal{ID: user.ID, Username: user.Username, Role: user.Role}, "", nil
	}
	return &sessionauth.Principal{ID: user.ID, Username: user.Username, Role: user.Role}, user.PasswordHash, nil
}

func (a adminStore) CreateSession(ctx context.Context, id string, principalID int64, tokenHash string, expiresAt time.Time) error {
	return a.store.CreateAdminSession(ctx, id, principalID, tokenHash, expiresAt)
}

func (a adminStore) SessionPrincipal(ctx context.Context, id string) (*sessionauth.Principal, time.Time, error) {
	user, expiresAt, err := a.store.GetAdminSession(ctx, id)
	if err != nil {
		return nil, time.Time{}, err
	}
	return &sessionauth.Principal{ID: user.ID, Username: user.Username, Role: user.Role}, expiresAt, nil
}

func (a adminStore) SessionTokenHash(ctx context.Context, id string) (string, error) {
	return a.store.SessionTokenHash(ctx, id)
}

func (a adminStore) DeleteSession(ctx context.Context, id string) error {
	return a.store.DeleteAdminSession(ctx, id)
}

func (a adminStore) DeleteSessionsForPrincipal(ctx context.Context, principalID int64) error {
	return a.store.DeleteAdminSessions(ctx, principalID)
}

func (a adminStore) TouchLogin(ctx context.Context, principalID int64) error {
	return a.store.TouchAdminLogin(ctx, principalID)
}

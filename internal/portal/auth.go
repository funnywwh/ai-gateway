// Package portal provides the customer self-service identity layer: portal users live in
// their own table, are bound to exactly one account, and share the session machinery
// with the management console through internal/sessionauth.
package portal

import (
	"context"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/sessionauth"
)

// Store is the persistence the portal identity layer needs.
type Store interface {
	GetPortalUserByUsername(ctx context.Context, username string) (*domain.PortalUser, error)
	GetPortalUser(ctx context.Context, id int64) (*domain.PortalUser, error)
	UpsertPortalUser(ctx context.Context, user *domain.PortalUser) (int64, error)
	CreatePortalSession(ctx context.Context, id string, userID int64, tokenHash string, expiresAt time.Time) error
	GetPortalSession(ctx context.Context, id string) (*domain.PortalUser, time.Time, error)
	PortalSessionTokenHash(ctx context.Context, id string) (string, error)
	DeletePortalSession(ctx context.Context, id string) error
	DeletePortalSessionsForUser(ctx context.Context, userID int64) error
	TouchPortalLogin(ctx context.Context, userID int64) error
}

// Config tunes the portal authentication service.
type Config struct {
	SessionTTL    time.Duration
	LoginAttempts int
	LoginWindow   time.Duration
}

// Session is an issued portal session.
type Session struct {
	ID        string
	Token     string
	ExpiresAt time.Time
	User      *domain.PortalUser
}

// Auth authenticates portal users and manages their sessions.
type Auth struct {
	service *sessionauth.Service
	store   Store
}

// NewAuth builds the portal auth service.
func NewAuth(store Store, cfg Config) *Auth {
	return &Auth{
		store: store,
		service: sessionauth.New(portalStore{store}, sessionauth.Config{
			SessionTTL: cfg.SessionTTL, LoginAttempts: cfg.LoginAttempts, LoginWindow: cfg.LoginWindow,
		}),
	}
}

// SetClock overrides the clock (tests).
func (a *Auth) SetClock(now func() time.Time) { a.service.SetClock(now) }

// Login verifies credentials and issues a session.
func (a *Auth) Login(ctx context.Context, username, password, clientKey string) (*Session, error) {
	session, err := a.service.Login(ctx, strings.TrimSpace(username), password, clientKey)
	if err != nil {
		return nil, err
	}
	user, err := a.currentUser(ctx, session.Principal.ID)
	if err != nil {
		return nil, err
	}
	return &Session{ID: session.ID, Token: session.Token, ExpiresAt: session.ExpiresAt, User: user}, nil
}

// Authenticate validates a session cookie pair and returns the portal user it belongs to.
func (a *Auth) Authenticate(ctx context.Context, sessionID, token string) (*domain.PortalUser, error) {
	principal, err := a.service.Authenticate(ctx, sessionID, token)
	if err != nil {
		return nil, err
	}
	return a.currentUser(ctx, principal.ID)
}

// Logout removes one session.
func (a *Auth) Logout(ctx context.Context, sessionID string) error {
	return a.service.Logout(ctx, sessionID)
}

// LogoutAll removes every session of a portal user (password change, revoke).
func (a *Auth) LogoutAll(ctx context.Context, userID int64) error {
	return a.service.LogoutOthers(ctx, userID)
}

// CookieValue renders the cookie payload for a session.
func CookieValue(session *Session) string {
	if session == nil {
		return ""
	}
	return session.ID + "." + session.Token
}

// ParseCookie splits a cookie payload into its session id and token.
func ParseCookie(value string) (id, token string, ok bool) { return sessionauth.ParseCookie(value) }

// HashPassword and VerifyPassword mirror the console's parameters.
func HashPassword(password string) (string, error) { return sessionauth.HashPassword(password) }

// VerifyPassword checks a password against a stored hash.
func VerifyPassword(hash, password string) bool { return sessionauth.VerifyPassword(hash, password) }

func (a *Auth) currentUser(ctx context.Context, id int64) (*domain.PortalUser, error) {
	return a.store.GetPortalUser(ctx, id)
}

// portalStore adapts the portal tables onto the shared session store.
type portalStore struct {
	store Store
}

func (p portalStore) PrincipalByUsername(ctx context.Context, username string) (*sessionauth.Principal, string, error) {
	user, err := p.store.GetPortalUserByUsername(ctx, username)
	if err != nil {
		return nil, "", err
	}
	if user.Status != "active" {
		// Disabled accounts must not be able to sign in, and the reason stays out of the
		// response so a login form cannot probe which accounts exist.
		return nil, "", domain.ErrUnauthorized("invalid username or password")
	}
	return &sessionauth.Principal{ID: user.ID, Username: user.Username, Role: "portal"}, user.PasswordHash, nil
}

func (p portalStore) CreateSession(ctx context.Context, id string, principalID int64, tokenHash string, expiresAt time.Time) error {
	return p.store.CreatePortalSession(ctx, id, principalID, tokenHash, expiresAt)
}

func (p portalStore) SessionPrincipal(ctx context.Context, id string) (*sessionauth.Principal, time.Time, error) {
	user, expiresAt, err := p.store.GetPortalSession(ctx, id)
	if err != nil {
		return nil, time.Time{}, err
	}
	return &sessionauth.Principal{ID: user.ID, Username: user.Username, Role: "portal"}, expiresAt, nil
}

func (p portalStore) SessionTokenHash(ctx context.Context, id string) (string, error) {
	return p.store.PortalSessionTokenHash(ctx, id)
}

func (p portalStore) DeleteSession(ctx context.Context, id string) error {
	return p.store.DeletePortalSession(ctx, id)
}

func (p portalStore) DeleteSessionsForPrincipal(ctx context.Context, principalID int64) error {
	return p.store.DeletePortalSessionsForUser(ctx, principalID)
}

func (p portalStore) TouchLogin(ctx context.Context, principalID int64) error {
	return p.store.TouchPortalLogin(ctx, principalID)
}

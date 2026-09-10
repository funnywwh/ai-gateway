// Package admin implements the management API surface: session authentication and the
// administrative operations that keep the in-memory snapshot and caches fresh.
package admin

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/secret"
)

// Password hashing parameters (PBKDF2-HMAC-SHA256, standard library, Go 1.24+).
const (
	passwordIterations = 210_000
	passwordSaltBytes  = 16
	passwordKeyBytes   = 32
	hashScheme         = "pbkdf2-sha256"
)

// HashPassword renders a storable password hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("admin: salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyBytes)
	if err != nil {
		return "", fmt.Errorf("admin: derive key: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a stored hash.
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Store is the persistence subset the auth service needs.
type Store interface {
	GetAdminUserByUsername(ctx context.Context, username string) (*domain.AdminUser, error)
	CreateAdminSession(ctx context.Context, id string, userID int64, tokenHash string, expiresAt time.Time) error
	GetAdminSession(ctx context.Context, id string) (*domain.AdminUser, time.Time, error)
	SessionTokenHash(ctx context.Context, id string) (string, error)
	DeleteAdminSession(ctx context.Context, id string) error
	TouchAdminLogin(ctx context.Context, id int64) error
}

// Config tunes the auth service.
type Config struct {
	SessionTTL        time.Duration
	LoginAttempts     int
	LoginWindow       time.Duration
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
	store Store
	cfg   Config
	now   func() time.Time

	mu       sync.Mutex
	attempts map[string]*attemptWindow
}

type attemptWindow struct {
	start time.Time
	count int
}

// NewAuth builds the auth service.
func NewAuth(store Store, cfg Config) *Auth {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.LoginAttempts <= 0 {
		cfg.LoginAttempts = 10
	}
	if cfg.LoginWindow <= 0 {
		cfg.LoginWindow = 5 * time.Minute
	}
	return &Auth{
		store:    store,
		cfg:      cfg,
		now:      func() time.Time { return time.Now().UTC() },
		attempts: map[string]*attemptWindow{},
	}
}

// SetClock overrides the clock (tests).
func (a *Auth) SetClock(now func() time.Time) { a.now = now }

// Login verifies credentials and issues a session. The token is returned once;
// only its hash is stored.
func (a *Auth) Login(ctx context.Context, username, password, clientKey string) (*Session, error) {
	if !a.allowAttempt(clientKey) {
		return nil, domain.ErrRateLimited("too many failed login attempts; try again later")
	}
	user, err := a.store.GetAdminUserByUsername(ctx, username)
	if err != nil {
		a.recordFailure(clientKey)
		return nil, domain.ErrUnauthorized("invalid username or password")
	}
	if !VerifyPassword(user.PasswordHash, password) {
		a.recordFailure(clientKey)
		return nil, domain.ErrUnauthorized("invalid username or password")
	}
	a.clearFailures(clientKey)

	now := a.now()
	session := &Session{
		ID:        ids.Session(),
		Token:     ids.New("tok"),
		ExpiresAt: now.Add(a.cfg.SessionTTL),
		User:      user,
	}
	if err := a.store.CreateAdminSession(ctx, session.ID, user.ID, secret.Hash(session.Token), session.ExpiresAt); err != nil {
		return nil, err
	}
	_ = a.store.TouchAdminLogin(ctx, user.ID)
	return session, nil
}

// Authenticate validates a session cookie pair (id + token).
func (a *Auth) Authenticate(ctx context.Context, sessionID, token string) (*domain.AdminUser, error) {
	if sessionID == "" || token == "" {
		return nil, domain.ErrUnauthorized("missing session")
	}
	stored, err := a.store.SessionTokenHash(ctx, sessionID)
	if err != nil {
		return nil, domain.ErrUnauthorized("invalid session")
	}
	if !secret.Equal(stored, secret.Hash(token)) {
		return nil, domain.ErrUnauthorized("invalid session")
	}
	user, _, err := a.store.GetAdminSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// Logout removes a session.
func (a *Auth) Logout(ctx context.Context, sessionID string) error {
	return a.store.DeleteAdminSession(ctx, sessionID)
}

// CookieValue renders the cookie payload for a session.
func CookieValue(s *Session) string { return s.ID + "." + s.Token }

// ParseCookie splits a cookie payload into its session id and token.
func ParseCookie(value string) (id, token string, ok bool) {
	idx := strings.LastIndex(value, ".")
	if idx <= 0 || idx == len(value)-1 {
		return "", "", false
	}
	return value[:idx], value[idx+1:], true
}

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

// ---------------------------------------------------------------------------
// login throttling
// ---------------------------------------------------------------------------

func (a *Auth) allowAttempt(clientKey string) bool {
	key := throttleKey(clientKey)
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.attempts[key]
	if !ok {
		return true
	}
	if a.now().Sub(w.start) > a.cfg.LoginWindow {
		delete(a.attempts, key)
		return true
	}
	return w.count < a.cfg.LoginAttempts
}

func (a *Auth) recordFailure(clientKey string) {
	key := throttleKey(clientKey)
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.attempts[key]
	if !ok || a.now().Sub(w.start) > a.cfg.LoginWindow {
		a.attempts[key] = &attemptWindow{start: a.now(), count: 1}
		return
	}
	w.count++
}

func (a *Auth) clearFailures(clientKey string) {
	key := throttleKey(clientKey)
	a.mu.Lock()
	delete(a.attempts, key)
	a.mu.Unlock()
}

func throttleKey(clientKey string) string {
	if clientKey == "" {
		clientKey = "unknown"
	}
	return strings.ToLower(clientKey)
}

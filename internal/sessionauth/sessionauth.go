// Package sessionauth issues and verifies browser sessions for a principal, without
// knowing what a principal is: the management console and the customer portal both use
// it, each with its own table, cookie and role rules.
//
// Passwords are PBKDF2-HMAC-SHA256; sessions are `<id>.<token>` cookie values where only
// the token hash is stored, so a database leak does not hand over live sessions.
package sessionauth

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

const (
	// hashIterations and hashSaltBytes follow current OWASP guidance for PBKDF2-SHA256.
	hashIterations = 210000
	hashSaltBytes  = 16
	hashKeyBytes   = 32
)

// Principal is the authenticated subject, independent of which table it lives in.
type Principal struct {
	ID       int64
	Username string
	Role     string
}

// Store is the persistence a principal's sessions need. PrincipalByUsername also
// returns the stored password hash so the service stays in charge of verification.
type Store interface {
	PrincipalByUsername(ctx context.Context, username string) (*Principal, string, error)
	CreateSession(ctx context.Context, id string, principalID int64, tokenHash string, expiresAt time.Time) error
	SessionPrincipal(ctx context.Context, id string) (*Principal, time.Time, error)
	SessionTokenHash(ctx context.Context, id string) (string, error)
	DeleteSession(ctx context.Context, id string) error
	DeleteSessionsForPrincipal(ctx context.Context, principalID int64) error
	TouchLogin(ctx context.Context, principalID int64) error
}

// Config tunes the service.
type Config struct {
	SessionTTL    time.Duration
	LoginAttempts int
	LoginWindow   time.Duration
}

// Session is an issued session; Token is shown once and never stored.
type Session struct {
	ID        string
	Token     string
	ExpiresAt time.Time
	Principal *Principal
}

// Service authenticates principals and manages their sessions.
type Service struct {
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

// New builds a service with defaults applied.
func New(store Store, cfg Config) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.LoginAttempts <= 0 {
		cfg.LoginAttempts = 10
	}
	if cfg.LoginWindow <= 0 {
		cfg.LoginWindow = 5 * time.Minute
	}
	return &Service{
		store:    store,
		cfg:      cfg,
		now:      func() time.Time { return time.Now().UTC() },
		attempts: map[string]*attemptWindow{},
	}
}

// SetClock overrides the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// HashPassword derives a storable hash: pbkdf2-sha256$<iterations>$<salt>$<key>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, hashSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("sessionauth: salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, hashKeyBytes)
	if err != nil {
		return "", fmt.Errorf("sessionauth: derive key: %w", err)
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", hashIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword compares a password against a stored hash in constant time.
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
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

// Login verifies credentials and issues a session. The token is returned once; only
// its hash reaches the database.
func (s *Service) Login(ctx context.Context, username, password, clientKey string) (*Session, error) {
	if !s.allowAttempt(clientKey) {
		return nil, domain.ErrRateLimited("too many failed login attempts; try again later")
	}
	principal, hash, err := s.store.PrincipalByUsername(ctx, username)
	if err != nil {
		s.recordFailure(clientKey)
		return nil, domain.ErrUnauthorized("invalid username or password")
	}
	if !VerifyPassword(hash, password) {
		s.recordFailure(clientKey)
		return nil, domain.ErrUnauthorized("invalid username or password")
	}
	s.clearFailures(clientKey)
	return s.Issue(ctx, principal)
}

// Issue mints a session for a principal that something else has already authenticated —
// today the Feishu identity flow, which proves who somebody is without a password
// (M66). It is the only other way into the session machinery, so the token shape, the TTL
// and the "only the hash is stored" rule stay in one place instead of being re-derived by
// every caller that has an identity in hand.
//
// The caller owns the proof: Issue performs no verification of its own and must never be
// reachable with an unverified principal.
func (s *Service) Issue(ctx context.Context, principal *Principal) (*Session, error) {
	if principal == nil || principal.ID == 0 {
		return nil, domain.ErrUnauthorized("missing principal")
	}
	now := s.now()
	session := &Session{
		ID:        ids.Session(),
		Token:     ids.New("tok"),
		ExpiresAt: now.Add(s.cfg.SessionTTL),
		Principal: principal,
	}
	if err := s.store.CreateSession(ctx, session.ID, principal.ID, secret.Hash(session.Token), session.ExpiresAt); err != nil {
		return nil, err
	}
	_ = s.store.TouchLogin(ctx, principal.ID)
	return session, nil
}

// Authenticate validates a session cookie pair (id + token).
func (s *Service) Authenticate(ctx context.Context, sessionID, token string) (*Principal, error) {
	if sessionID == "" || token == "" {
		return nil, domain.ErrUnauthorized("missing session")
	}
	stored, err := s.store.SessionTokenHash(ctx, sessionID)
	if err != nil {
		return nil, domain.ErrUnauthorized("invalid session")
	}
	if !secret.Equal(stored, secret.Hash(token)) {
		return nil, domain.ErrUnauthorized("invalid session")
	}
	principal, _, err := s.store.SessionPrincipal(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return principal, nil
}

// Logout removes one session.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.store.DeleteSession(ctx, sessionID)
}

// LogoutOthers removes every session of a principal, optionally sparing one.
func (s *Service) LogoutOthers(ctx context.Context, principalID int64) error {
	return s.store.DeleteSessionsForPrincipal(ctx, principalID)
}

// CookieValue renders the cookie payload for a session.
func CookieValue(session *Session) string {
	if session == nil {
		return ""
	}
	return session.ID + "." + session.Token
}

// ParseCookie splits a cookie payload into its session id and token.
func ParseCookie(value string) (id, token string, ok bool) {
	idx := strings.LastIndex(value, ".")
	if idx <= 0 || idx == len(value)-1 {
		return "", "", false
	}
	return value[:idx], value[idx+1:], true
}

// ---------------------------------------------------------------------------
// login throttling
// ---------------------------------------------------------------------------

func (s *Service) allowAttempt(clientKey string) bool {
	key := throttleKey(clientKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.attempts[key]
	if !ok {
		return true
	}
	if s.now().Sub(window.start) > s.cfg.LoginWindow {
		delete(s.attempts, key)
		return true
	}
	return window.count < s.cfg.LoginAttempts
}

func (s *Service) recordFailure(clientKey string) {
	key := throttleKey(clientKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	window, ok := s.attempts[key]
	if !ok || s.now().Sub(window.start) > s.cfg.LoginWindow {
		s.attempts[key] = &attemptWindow{start: s.now(), count: 1}
		return
	}
	window.count++
}

func (s *Service) clearFailures(clientKey string) {
	key := throttleKey(clientKey)
	s.mu.Lock()
	delete(s.attempts, key)
	s.mu.Unlock()
}

func throttleKey(clientKey string) string {
	if clientKey == "" {
		clientKey = "unknown"
	}
	return strings.ToLower(clientKey)
}

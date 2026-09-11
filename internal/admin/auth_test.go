package admin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

type memStore struct {
	user       *domain.AdminUser
	sessions   map[string]sessionRow
	loginTicks int
}

type sessionRow struct {
	userID    int64
	hash      string
	expiresAt time.Time
}

func newMemStore(t *testing.T, password string) *memStore {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return &memStore{
		user:     &domain.AdminUser{ID: 1, Username: "admin", PasswordHash: hash, Role: "admin"},
		sessions: map[string]sessionRow{},
	}
}

func (m *memStore) GetAdminUserByUsername(ctx context.Context, username string) (*domain.AdminUser, error) {
	if m.user == nil || m.user.Username != username {
		return nil, domain.ErrNotFound("admin user")
	}
	return m.user, nil
}

func (m *memStore) CreateAdminSession(ctx context.Context, id string, userID int64, tokenHash string, expiresAt time.Time) error {
	m.sessions[id] = sessionRow{userID: userID, hash: tokenHash, expiresAt: expiresAt}
	return nil
}

func (m *memStore) GetAdminSession(ctx context.Context, id string) (*domain.AdminUser, time.Time, error) {
	row, ok := m.sessions[id]
	if !ok {
		return nil, time.Time{}, domain.ErrUnauthorized("invalid session")
	}
	if time.Now().UTC().After(row.expiresAt) {
		return nil, row.expiresAt, domain.ErrUnauthorized("session expired")
	}
	return m.user, row.expiresAt, nil
}

func (m *memStore) SessionTokenHash(ctx context.Context, id string) (string, error) {
	row, ok := m.sessions[id]
	if !ok {
		return "", domain.ErrUnauthorized("invalid session")
	}
	return row.hash, nil
}

func (m *memStore) DeleteAdminSession(ctx context.Context, id string) error {
	delete(m.sessions, id)
	return nil
}

func (m *memStore) DeleteAdminSessions(ctx context.Context, userID int64) error {
	for id, session := range m.sessions {
		if session.userID == userID {
			delete(m.sessions, id)
		}
	}
	return nil
}

func (m *memStore) TouchAdminLogin(ctx context.Context, id int64) error {
	m.loginTicks++
	return nil
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("valid password must verify")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("invalid password must not verify")
	}
	if VerifyPassword("garbage", "whatever") {
		t.Fatal("malformed hash must not verify")
	}

	// Salts make hashes unique.
	other, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if other == hash {
		t.Fatal("hashes must be salted")
	}
}

func TestLoginAuthenticateLogout(t *testing.T) {
	store := newMemStore(t, "s3cret-password")
	auth := NewAuth(store, Config{SessionTTL: time.Hour})
	ctx := context.Background()

	session, err := auth.Login(ctx, "admin", "s3cret-password", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(session.ID, "sess_") {
		t.Fatalf("session id = %q", session.ID)
	}
	// Only the hash is stored.
	stored := store.sessions[session.ID].hash
	if stored == session.Token || stored != secret.Hash(session.Token) {
		t.Fatal("session token must be stored hashed")
	}

	cookie := CookieValue(session)
	id, token, ok := ParseCookie(cookie)
	if !ok || id != session.ID || token != session.Token {
		t.Fatalf("cookie round trip failed: %q", cookie)
	}
	user, err := auth.Authenticate(ctx, id, token)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "admin" {
		t.Fatalf("unexpected user: %+v", user)
	}

	// Wrong token is rejected.
	if _, err := auth.Authenticate(ctx, id, "not-the-token"); err == nil {
		t.Fatal("a wrong token must be rejected")
	}

	if err := auth.Logout(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, id, token); err == nil {
		t.Fatal("a logged-out session must be rejected")
	}
}

func TestLoginRejectsBadCredentialsAndThrottles(t *testing.T) {
	store := newMemStore(t, "right-password")
	auth := NewAuth(store, Config{LoginAttempts: 3, LoginWindow: time.Minute})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := auth.Login(ctx, "admin", "wrong", "9.9.9.9"); !domain.IsUnauthorized(err) {
			t.Fatalf("attempt %d: expected 401, got %v", i, err)
		}
	}
	// The window is exhausted: even the correct password is throttled.
	_, err := auth.Login(ctx, "admin", "right-password", "9.9.9.9")
	if !domain.IsRateLimited(err) {
		t.Fatalf("expected 429 after too many failures, got %v", err)
	}
	// A different client key is unaffected.
	if _, err := auth.Login(ctx, "admin", "right-password", "8.8.8.8"); err != nil {
		t.Fatalf("a different client must not be throttled: %v", err)
	}
	// Unknown user yields the same error shape (no user enumeration).
	if _, err := auth.Login(ctx, "ghost", "whatever", "7.7.7.7"); !domain.IsUnauthorized(err) {
		t.Fatalf("unknown user must yield 401, got %v", err)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	store := newMemStore(t, "pw")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	auth := NewAuth(store, Config{SessionTTL: time.Minute})
	auth.SetClock(func() time.Time { return now })

	session, err := auth.Login(context.Background(), "admin", "pw", "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	// Expire it in the store and check the auth path.
	row := store.sessions[session.ID]
	row.expiresAt = now.Add(-time.Second)
	store.sessions[session.ID] = row

	if _, err := auth.Authenticate(context.Background(), session.ID, session.Token); !domain.IsUnauthorized(err) {
		t.Fatalf("expired session must be rejected, got %v", err)
	}
}

func TestRequireRole(t *testing.T) {
	admin := &domain.AdminUser{Role: "admin"}
	viewer := &domain.AdminUser{Role: "viewer"}
	if !RequireRole(admin, "admin") || RequireRole(viewer, "admin") {
		t.Fatal("admin role check failed")
	}
	if !RequireRole(viewer, "viewer") || !RequireRole(admin, "viewer") {
		t.Fatal("viewer role check failed")
	}
	if RequireRole(nil, "admin") {
		t.Fatal("nil user must not pass")
	}
}

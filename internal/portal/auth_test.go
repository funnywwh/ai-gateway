package portal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

func newPortalFixture(t *testing.T) (*Auth, *store.DB, int64) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "portal.db")
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword("customer-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertPortalUser(ctx, &domain.PortalUser{
		AccountID: accountID, Username: "customer", PasswordHash: hash, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	auth := NewAuth(db, Config{SessionTTL: time.Hour, LoginAttempts: 3, LoginWindow: time.Minute})
	return auth, db, accountID
}

func TestPortalLoginAuthenticateLogout(t *testing.T) {
	ctx := context.Background()
	auth, db, accountID := newPortalFixture(t)

	session, err := auth.Login(ctx, "customer", "customer-password-1", "127.0.0.1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if session.User.AccountID != accountID {
		t.Fatalf("session bound to account %d, want %d", session.User.AccountID, accountID)
	}
	if session.Token == "" || session.ID == "" {
		t.Fatalf("session = %+v", session)
	}

	user, err := auth.Authenticate(ctx, session.ID, session.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if user.Username != "customer" {
		t.Fatalf("authenticated %q", user.Username)
	}

	// The stored hash must never equal the plaintext password or the token.
	stored, err := db.GetPortalUserByUsername(ctx, "customer")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == "customer-password-1" || stored.PasswordHash == session.Token {
		t.Fatal("the stored secret is a plaintext value")
	}

	if _, err := auth.Authenticate(ctx, session.ID, "wrong-token"); err == nil {
		t.Fatal("a wrong token must not authenticate")
	}
	if err := auth.Logout(ctx, session.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := auth.Authenticate(ctx, session.ID, session.Token); err == nil {
		t.Fatal("a logged-out session must not authenticate")
	}
}

func TestPortalRejectsBadCredentialsAndDisabledUsers(t *testing.T) {
	ctx := context.Background()
	auth, db, _ := newPortalFixture(t)

	if _, err := auth.Login(ctx, "customer", "nope", "client"); err == nil {
		t.Fatal("a wrong password must not sign in")
	}
	if _, err := auth.Login(ctx, "ghost", "nope", "client"); err == nil {
		t.Fatal("an unknown user must not sign in")
	}

	// Disabling the user must block new sign-ins and reject existing sessions.
	session, err := auth.Login(ctx, "customer", "customer-password-1", "client2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := db.SetPortalUserStatus(ctx, session.User.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, session.ID, session.Token); err == nil {
		t.Fatal("a disabled user's session must be rejected")
	}
	if _, err := auth.Login(ctx, "customer", "customer-password-1", "client3"); err == nil {
		t.Fatal("a disabled user must not sign in")
	}
}

func TestPortalLogoutAllRevokesEverySession(t *testing.T) {
	ctx := context.Background()
	auth, _, _ := newPortalFixture(t)

	first, err := auth.Login(ctx, "customer", "customer-password-1", "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.Login(ctx, "customer", "customer-password-1", "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.LogoutAll(ctx, first.User.ID); err != nil {
		t.Fatalf("logout all: %v", err)
	}
	for _, session := range []*Session{first, second} {
		if _, err := auth.Authenticate(ctx, session.ID, session.Token); err == nil {
			t.Fatalf("session %s survived a global logout", session.ID)
		}
	}
}

func TestPortalThrottlesRepeatedFailures(t *testing.T) {
	ctx := context.Background()
	auth, _, _ := newPortalFixture(t)
	for index := 0; index < 3; index++ {
		if _, err := auth.Login(ctx, "customer", "wrong", "client"); err == nil {
			t.Fatal("expected a failure")
		}
	}
	_, err := auth.Login(ctx, "customer", "customer-password-1", "client")
	if err == nil {
		t.Fatal("the throttle must reject further attempts")
	}
	apiErr, ok := domain.AsAPIError(err)
	if !ok || apiErr.Status != 429 {
		t.Fatalf("error = %v, want 429", err)
	}
}

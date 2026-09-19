package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// newAdminUser creates one administrator with the given role/status and no binding.
func newAdminUser(t *testing.T, db *DB, username, role, status string) int64 {
	t.Helper()
	id, err := db.CreateAdminUser(context.Background(), &domain.AdminUser{
		Username: username, Role: role, Status: status,
	})
	if err != nil {
		t.Fatalf("create admin %s: %v", username, err)
	}
	return id
}

func TestAdminUserLifecycle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// An invitation-only account: no password at all, so the empty string satisfies the
	// NOT NULL column and nothing can ever match it.
	id, err := db.CreateAdminUser(ctx, &domain.AdminUser{Username: "invited", Role: domain.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != domain.AdminPending {
		t.Fatalf("a password-less account was created %q, want pending", user.Status)
	}
	if user.PasswordHash != "" || user.FeishuOpenID != "" || user.InviteNonce != "" {
		t.Fatalf("a fresh account carries credentials: %+v", user)
	}

	// Duplicate usernames are a conflict, not a second row.
	if _, err := db.CreateAdminUser(ctx, &domain.AdminUser{Username: "invited", Role: domain.RoleAdmin}); err == nil {
		t.Fatal("a duplicate administrator was created")
	}
	if _, err := db.CreateAdminUser(ctx, &domain.AdminUser{Username: "bad-role", Role: "root"}); err == nil {
		t.Fatal("an unknown role was stored")
	}

	// One column at a time: a role change must not undo a password, and the other way round.
	if err := db.SetAdminUserPassword(ctx, id, "pbkdf2-sha256$1$a$b"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAdminUserRole(ctx, id, domain.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	user, err = db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordHash == "" || user.Role != domain.RoleAdmin {
		t.Fatalf("column-scoped writes clobbered each other: %+v", user)
	}
	if err := db.SetAdminUserStatus(ctx, id, domain.AdminActive); err != nil {
		t.Fatal(err)
	}
	if count, err := db.ActiveAdminCount(ctx); err != nil || count != 1 {
		t.Fatalf("active admin count = %d (%v), want 1", count, err)
	}

	// Deletion is a row delete; a second attempt is a not-found rather than a silent success.
	if err := db.DeleteAdminUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAdminUser(ctx, id); err == nil {
		t.Fatal("deleting a missing administrator reported success")
	}
	if count, err := db.ActiveAdminCount(ctx); err != nil || count != 0 {
		t.Fatalf("active admin count after delete = %d (%v), want 0", count, err)
	}
}

func TestAdminUserFeishuBinding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	alice := newAdminUser(t, db, "alice", domain.RoleAdmin, domain.AdminPending)
	bob := newAdminUser(t, db, "bob", domain.RoleAdmin, domain.AdminPending)

	// An unbound identity resolves to nothing: that is an ordinary answer on the login path,
	// and it is what keeps a customer's key binding from ever reaching the console.
	if found, err := db.FindAdminUserByFeishuOpenID(ctx, "ou_alice"); err != nil || found != nil {
		t.Fatalf("an unbound identity resolved: %v %+v", err, found)
	}
	if found, err := db.FindAdminUserByFeishuOpenID(ctx, "  "); err != nil || found != nil {
		t.Fatalf("an empty identity resolved: %v %+v", err, found)
	}

	// Binding activates the account and clears any pending invitation in the same write.
	if err := db.RotateAdminUserInvite(ctx, alice, "handle-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindAdminUserFeishu(ctx, alice, domain.FeishuBinding{
		OpenID: "ou_alice", UnionID: "on_alice", Name: "张三", BoundBy: "invite:boss",
	}); err != nil {
		t.Fatal(err)
	}
	user, err := db.GetAdminUser(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != domain.AdminActive || user.FeishuOpenID != "ou_alice" || user.FeishuBoundAt == nil {
		t.Fatalf("binding did not activate the account: %+v", user)
	}
	if user.InviteNonce != "" {
		t.Fatalf("a redeemed invitation is still pending: %q", user.InviteNonce)
	}
	if user.FeishuBoundBy != "invite:boss" {
		t.Fatalf("bound_by = %q", user.FeishuBoundBy)
	}
	found, err := db.FindAdminUserByFeishuOpenID(ctx, "ou_alice")
	if err != nil || found == nil || found.ID != alice {
		t.Fatalf("a bound identity did not resolve: %v %+v", err, found)
	}

	// One identity, one administrator: the second binding is a conflict, not a takeover.
	if err := db.BindAdminUserFeishu(ctx, bob, domain.FeishuBinding{OpenID: "ou_alice"}); err == nil {
		t.Fatal("two administrators ended up with the same Feishu identity")
	}
	if user, err := db.GetAdminUser(ctx, bob); err != nil || user.FeishuOpenID != "" {
		t.Fatalf("the refused binding still wrote something: %v %+v", err, user)
	}

	// Unbinding is idempotent and leaves an account with no other credential pending.
	changed, err := db.UnbindAdminUserFeishu(ctx, alice)
	if err != nil || !changed {
		t.Fatalf("unbind = %v (%v), want true", changed, err)
	}
	user, err = db.GetAdminUser(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if user.FeishuOpenID != "" || user.FeishuBoundAt != nil {
		t.Fatalf("unbind left an identity behind: %+v", user)
	}
	if user.Status != domain.AdminPending {
		t.Fatalf("an account with no credential left is %q, want pending", user.Status)
	}
	if again, err := db.UnbindAdminUserFeishu(ctx, alice); err != nil || again {
		t.Fatalf("second unbind = %v (%v), want false", again, err)
	}

	// An account that still has a password keeps it (and stays active) across an unbind.
	if err := db.SetAdminUserPassword(ctx, bob, "pbkdf2-sha256$1$a$b"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAdminUserStatus(ctx, bob, domain.AdminActive); err != nil {
		t.Fatal(err)
	}
	if err := db.BindAdminUserFeishu(ctx, bob, domain.FeishuBinding{OpenID: "ou_bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UnbindAdminUserFeishu(ctx, bob); err != nil {
		t.Fatal(err)
	}
	user, err = db.GetAdminUser(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != domain.AdminActive || user.PasswordHash == "" {
		t.Fatalf("unbinding a password account changed it: %+v", user)
	}
}

func TestAdminUserSessionRequiresAnActiveAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id := newAdminUser(t, db, "alice", domain.RoleAdmin, domain.AdminActive)

	if err := db.CreateAdminSession(ctx, "sess_active", id, "hash", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetAdminSession(ctx, "sess_active"); err != nil {
		t.Fatalf("an active account's session was refused: %v", err)
	}

	// Disabling the account ends the session at the next read, not at the next login.
	if err := db.SetAdminUserStatus(ctx, id, domain.AdminDisabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetAdminSession(ctx, "sess_active"); err == nil {
		t.Fatal("a disabled account's session was accepted")
	}

	// A pending account (an invitation that was never redeemed, then unbound) is refused too.
	if err := db.SetAdminUserStatus(ctx, id, domain.AdminPending); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetAdminSession(ctx, "sess_active"); err == nil {
		t.Fatal("a pending account's session was accepted")
	}
}

func TestAdminUserInviteRotation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id := newAdminUser(t, db, "invitee", domain.RoleViewer, domain.AdminPending)

	if err := db.RotateAdminUserInvite(ctx, id, "handle-1"); err != nil {
		t.Fatal(err)
	}
	user, err := db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.InviteNonce != "handle-1" {
		t.Fatalf("invite handle = %q", user.InviteNonce)
	}
	// Regenerating replaces the handle, which is what retires the previous link: the row
	// only ever holds the newest one.
	if err := db.RotateAdminUserInvite(ctx, id, "handle-2"); err != nil {
		t.Fatal(err)
	}
	user, err = db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.InviteNonce != "handle-2" {
		t.Fatalf("invite handle after rotation = %q", user.InviteNonce)
	}
	if err := db.RotateAdminUserInvite(ctx, id, "  "); err == nil {
		t.Fatal("an empty invitation handle was stored")
	}
	if err := db.RotateAdminUserInvite(ctx, 99999, "handle-3"); err == nil {
		t.Fatal("an invitation was rotated for a missing account")
	}
}

// The bootstrap seeder must not resurrect decisions an operator made in the console: it
// writes the configured password and role on every start, and nothing else.
func TestUpsertAdminUserKeepsConsoleDecisions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id := newAdminUser(t, db, "admin", domain.RoleAdmin, domain.AdminActive)
	if err := db.BindAdminUserFeishu(ctx, id, domain.FeishuBinding{OpenID: "ou_boss"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAdminUserStatus(ctx, id, domain.AdminDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAdminUser(ctx, &domain.AdminUser{
		Username: "admin", PasswordHash: "pbkdf2-sha256$1$a$b", Role: domain.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	user, err := db.GetAdminUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != domain.AdminDisabled {
		t.Fatalf("restart re-enabled a disabled administrator: %+v", user)
	}
	if user.FeishuOpenID != "ou_boss" {
		t.Fatalf("restart cleared a Feishu binding: %+v", user)
	}
	if user.PasswordHash != "pbkdf2-sha256$1$a$b" {
		t.Fatalf("restart did not apply the configured password: %+v", user)
	}
}

func TestListAdminUsersIsStable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	first := newAdminUser(t, db, "first", domain.RoleAdmin, domain.AdminActive)
	second := newAdminUser(t, db, "second", domain.RoleViewer, domain.AdminPending)
	users, err := db.ListAdminUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].ID != first || users[1].ID != second {
		t.Fatalf("list order = %+v", users)
	}
}

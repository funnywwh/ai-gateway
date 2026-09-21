package store

import (
	"context"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// M72 moves M60's key-level Feishu bindings onto their accounts, because from this milestone
// the DSH portal resolves an identity through accounts.feishu_open_id only. The interesting
// part is not the happy path but what must NOT happen: a conflicting binding has to survive on
// the key row instead of being overwritten or dropped, since guessing which person owns an
// account is exactly the silent identity change the milestone exists to prevent.

// boundKeyFixture creates one account with one key, binds that key to a Feishu identity and
// returns the account and key ids.
func boundKeyFixture(t *testing.T, db *DB, accountName, openID, name string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: accountName, BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: accountName + "-key",
		KeyPrefix: "sk-gw-" + accountName, KeyHash: "hash-" + accountName,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{
		OpenID: openID, UnionID: "on_" + openID, Name: name, BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	return accountID, keyID
}

// accountWithKey creates one unbound account with one unbound key.
func accountWithKey(t *testing.T, db *DB, accountName string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: accountName, BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: accountName + "-key",
		KeyPrefix: "sk-gw-" + accountName, KeyHash: "hash-" + accountName,
	})
	if err != nil {
		t.Fatal(err)
	}
	return accountID, keyID
}

func TestMigrateKeyFeishuToAccountsMovesTheBinding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, keyID := boundKeyFixture(t, db, "migrate-owner", "ou_zhou", "周八")

	out, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 1 || out.Moves[0].AccountID != accountID {
		t.Fatalf("moves = %+v, want one move onto account %d", out.Moves, accountID)
	}
	if out.Moves[0].FromKeyID != keyID || out.Moves[0].OpenID != "ou_zhou" || out.Moves[0].AccountName != "migrate-owner" {
		t.Fatalf("the move must name where the identity came from: %+v", out.Moves[0])
	}
	if len(out.Conflicts) != 0 {
		t.Fatalf("conflicts = %v, want none", out.Conflicts)
	}

	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "ou_zhou" || account.FeishuName != "周八" || account.FeishuUnionID != "on_ou_zhou" {
		t.Fatalf("the identity did not land on the account: %+v", account)
	}
	// The audit trail has to say where it came from, so the backfill is attributable.
	if account.FeishuBoundBy != "key-migration" {
		t.Fatalf("bound_by = %q, want key-migration", account.FeishuBoundBy)
	}
	key, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if key.FeishuOpenID != "" || key.FeishuBoundBy != "" {
		t.Fatalf("the key kept its identity: %+v", key)
	}

	// Idempotent: a second run finds nothing to move and writes nothing.
	again, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Moves) != 0 || len(again.Cleared) != 0 || len(again.Conflicts) != 0 {
		t.Fatalf("second run did work: %+v", again)
	}
}

// An account that was already bound to the same person keeps its own metadata (the admin who
// did it, the time), and only the redundant key row is cleared.
func TestMigrateKeyFeishuToAccountsKeepsAnIdenticalAccountBinding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, keyID := boundKeyFixture(t, db, "already-bound", "ou_same", "同一人")
	if err := db.BindAccountFeishu(ctx, accountID, domain.FeishuBinding{
		OpenID: "ou_same", UnionID: "on_ou_same", Name: "同一人", BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 0 || len(out.Conflicts) != 0 {
		t.Fatalf("an identical binding must not be re-migrated or reported as a conflict: %+v", out)
	}
	if len(out.Cleared) != 1 || out.Cleared[0] != keyID {
		t.Fatalf("cleared = %v, want [%d]", out.Cleared, keyID)
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuBoundBy != "admin" {
		t.Fatalf("the account's own binding was overwritten: %+v", account)
	}
}

// The account is bound to a different person than its key: nothing moves, and the key row
// remains readable (and unbindable) in the console.
func TestMigrateKeyFeishuToAccountsLeavesConflictsAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, keyID := boundKeyFixture(t, db, "conflicted", "ou_from_key", "Key 上的人")
	if err := db.BindAccountFeishu(ctx, accountID, domain.FeishuBinding{
		OpenID: "ou_on_account", UnionID: "on_ou_on_account", Name: "账号上的人", BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Conflicts) != 1 || out.Conflicts[0] != keyID {
		t.Fatalf("conflicts = %v, want [%d]", out.Conflicts, keyID)
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "ou_on_account" {
		t.Fatalf("the account's binding was changed: %+v", account)
	}
	key, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if key.FeishuOpenID != "ou_from_key" {
		t.Fatalf("the conflicting key lost its identity: %+v", key)
	}
}

// seedLegacyKeyBinding writes a key-level Feishu identity straight into the row, bypassing the
// uniqueness rules BindAPIKeyFeishu enforces. It models a pre-M72 row — the only shape the
// backfill ever sees — and it is how this test builds the one state the backfill must refuse to
// resolve: two keys of a single account naming two different people. Nothing in the admin API
// produces that state any more, which is exactly why the backfill cannot assume it away.
func seedLegacyKeyBinding(t *testing.T, db *DB, keyID int64, openID, name string) {
	t.Helper()
	if _, err := db.write.ExecContext(context.Background(), `UPDATE api_keys
SET feishu_open_id = ?, feishu_union_id = ?, feishu_name = ?, feishu_bound_by = 'admin'
WHERE id = ?`, openID, "on_"+openID, name, keyID); err != nil {
		t.Fatalf("seed legacy key binding: %v", err)
	}
}

// Two keys of the same account naming different people: refuse to choose. The oldest binding
// is the tempting answer, and it is wrong — one of the two people would silently gain access.
func TestMigrateKeyFeishuToAccountsRefusesAmbiguousGroup(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, first := boundKeyFixture(t, db, "ambiguous", "ou_first", "第一人")
	second, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "ambiguous-second", KeyPrefix: "sk-gw-ambiguous2", KeyHash: "hash-ambiguous2",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two keys of one account bound to different people: BindAPIKeyFeishu refuses this by
	// design (a person belongs to one key), so the row is written directly — which is exactly
	// the state a pre-M72 deployment can present to the backfill.
	seedLegacyKeyBinding(t, db, second, "ou_second", "第二人")
	out, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 0 {
		t.Fatalf("an ambiguous group must not migrate: %+v", out)
	}
	if len(out.Conflicts) != 2 {
		t.Fatalf("conflicts = %v, want both keys", out.Conflicts)
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "" {
		t.Fatalf("an ambiguous group must leave the account unbound: %+v", account)
	}
	for _, keyID := range []int64{first, second} {
		key, err := db.GetAPIKeyByID(ctx, keyID)
		if err != nil {
			t.Fatal(err)
		}
		if key.FeishuOpenID == "" {
			t.Fatalf("key %d lost its identity", keyID)
		}
	}
}

// The same person cannot fill two accounts, but this is the *reachable* shape of that rule
// interacting with the backfill: the account was bound by hand to someone, while its key still
// names a different person. Nothing moves, both rows stay visible, and the account keeps the
// identity an administrator chose.
func TestMigrateKeyFeishuToAccountsKeepsTheAdministratorsChoice(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, keyID := boundKeyFixture(t, db, "chosen-by-admin", "ou_from_key", "Key 上的人")
	if err := db.BindAccountFeishu(ctx, accountID, domain.FeishuBinding{
		OpenID: "ou_chosen", UnionID: "on_ou_chosen", Name: "管理员选的人", BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := db.MigrateKeyFeishuToAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 0 || len(out.Conflicts) != 1 || out.Conflicts[0] != keyID {
		t.Fatalf("outcome = %+v, want the key reported as a conflict", out)
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "ou_chosen" || account.FeishuBoundBy != "admin" {
		t.Fatalf("the administrator's binding was overwritten: %+v", account)
	}
}

// Nothing bound: the migration is a no-op, which is what every deployment that never used the
// key-level binding sees on upgrade.
func TestMigrateKeyFeishuToAccountsIsANoOpWithoutBindings(t *testing.T) {
	db := testDB(t)
	out, err := db.MigrateKeyFeishuToAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 0 || len(out.Cleared) != 0 || len(out.Conflicts) != 0 {
		t.Fatalf("outcome = %+v, want empty", out)
	}
}

// The M72 column round-trips, and a plain account write must not clear it: the administrator's
// 停用 decision survives every ordinary edit (the same rule the Feishu columns follow).
func TestAccountDSHDisabledAtSurvivesOrdinaryWrites(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "dsh-opt-out", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.DshDisabledAt != nil {
		t.Fatalf("a fresh account carries a disable timestamp: %+v", fresh)
	}

	when := timeFromUnix(1758500000)
	if err := db.SetAccountDSHDisabledAt(ctx, accountID, &when); err != nil {
		t.Fatal(err)
	}
	marked, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if marked.DshDisabledAt == nil || !marked.DshDisabledAt.Equal(when) {
		t.Fatalf("timestamp not persisted: %+v", marked)
	}

	marked.Note = "edited later"
	if _, err := db.UpsertAccount(ctx, marked); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DshDisabledAt == nil {
		t.Fatalf("an ordinary account write cleared the disable timestamp: %+v", after)
	}

	if err := db.SetAccountDSHDisabledAt(ctx, accountID, nil); err != nil {
		t.Fatal(err)
	}
	cleared, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.DshDisabledAt != nil {
		t.Fatalf("clearing failed: %+v", cleared)
	}
	if err := db.SetAccountDSHDisabledAt(ctx, 999999, nil); err == nil {
		t.Fatal("setting the timestamp of a missing account must fail")
	}
}

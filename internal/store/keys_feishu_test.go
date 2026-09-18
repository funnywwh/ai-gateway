package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// feishuFixture creates one account with one active key and returns their ids.
func feishuFixture(t *testing.T, db *DB) (accountID, keyID int64) {
	t.Helper()
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "feishu-owner", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "alice-key", KeyPrefix: "sk-gw-feishu1", KeyHash: "hash-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return accountID, id
}

func TestAPIKeyFeishuBindingRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, keyID := feishuFixture(t, db)

	unbound, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if unbound.FeishuOpenID != "" || unbound.FeishuBoundAt != nil {
		t.Fatalf("a fresh key carries a Feishu identity: %+v", unbound)
	}

	boundAt := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	if err := db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{
		OpenID: "ou_alice", UnionID: "on_alice", Name: "张三", BoundAt: boundAt, BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FeishuOpenID != "ou_alice" || got.FeishuUnionID != "on_alice" || got.FeishuName != "张三" {
		t.Fatalf("binding not persisted: %+v", got)
	}
	if got.FeishuBoundAt == nil || !got.FeishuBoundAt.Equal(boundAt) || got.FeishuBoundBy != "admin" {
		t.Fatalf("binding metadata not persisted: %+v", got)
	}

	// The identity resolves back to its key, which is how the DSH portal login finds a
	// tenant (M61).
	byOpenID, err := db.FindAPIKeyByFeishuOpenID(ctx, "ou_alice")
	if err != nil || byOpenID == nil || byOpenID.ID != keyID {
		t.Fatalf("resolve by open id: %v %+v", err, byOpenID)
	}
	if missing, err := db.FindAPIKeyByFeishuOpenID(ctx, "ou_nobody"); err != nil || missing != nil {
		t.Fatalf("an unbound identity must resolve to nothing, got %v %+v", err, missing)
	}
	if blank, err := db.FindAPIKeyByFeishuOpenID(ctx, "  "); err != nil || blank != nil {
		t.Fatalf("a blank identity must resolve to nothing, got %v %+v", err, blank)
	}

	// Unbinding is idempotent and reports whether it changed anything.
	changed, err := db.UnbindAPIKeyFeishu(ctx, keyID)
	if err != nil || !changed {
		t.Fatalf("unbind = %v, %v; want changed", changed, err)
	}
	again, err := db.UnbindAPIKeyFeishu(ctx, keyID)
	if err != nil || again {
		t.Fatalf("second unbind = %v, %v; want unchanged", again, err)
	}
	cleared, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.FeishuOpenID != "" || cleared.FeishuUnionID != "" || cleared.FeishuName != "" ||
		cleared.FeishuBoundAt != nil || cleared.FeishuBoundBy != "" {
		t.Fatalf("unbind left fields behind: %+v", cleared)
	}
}

// One Feishu identity binds one key: the second binding must fail as a conflict and must
// leave both keys exactly as they were.
func TestAPIKeyFeishuBindingIsUniquePerIdentity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, first := feishuFixture(t, db)
	second, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "bob-key", KeyPrefix: "sk-gw-feishu2", KeyHash: "hash-2",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.BindAPIKeyFeishu(ctx, first, domain.FeishuBinding{OpenID: "ou_shared", Name: "张三"}); err != nil {
		t.Fatal(err)
	}
	err = db.BindAPIKeyFeishu(ctx, second, domain.FeishuBinding{OpenID: "ou_shared", Name: "张三"})
	if err == nil {
		t.Fatal("the same identity was bound to a second key")
	}
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("conflict error = %v, want a 409 API error", err)
	}

	kept, err := db.GetAPIKeyByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if kept.FeishuOpenID != "ou_shared" {
		t.Fatalf("the original binding was disturbed: %+v", kept)
	}
	untouched, err := db.GetAPIKeyByID(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.FeishuOpenID != "" {
		t.Fatalf("the rejected key kept a partial binding: %+v", untouched)
	}

	// A second key may of course carry a different identity, and rebinding one key to a
	// new identity replaces the old pair rather than accumulating.
	if err := db.BindAPIKeyFeishu(ctx, second, domain.FeishuBinding{OpenID: "ou_bob", Name: "李四"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BindAPIKeyFeishu(ctx, first, domain.FeishuBinding{OpenID: "ou_alice2", Name: "张三（新）"}); err != nil {
		t.Fatal(err)
	}
	if stale, err := db.FindAPIKeyByFeishuOpenID(ctx, "ou_shared"); err != nil || stale != nil {
		t.Fatalf("the replaced identity still resolves: %v %+v", err, stale)
	}
	rebound, err := db.GetAPIKeyByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.FeishuOpenID != "ou_alice2" || rebound.FeishuName != "张三（新）" {
		t.Fatalf("rebind did not replace the binding: %+v", rebound)
	}
	if err := db.BindAPIKeyFeishu(ctx, 0, domain.FeishuBinding{OpenID: "ou_x"}); err == nil {
		t.Fatal("binding to a non-existent key was accepted")
	}
	if err := db.BindAPIKeyFeishu(ctx, first, domain.FeishuBinding{}); err == nil {
		t.Fatal("binding an empty identity was accepted")
	}
}

// A PATCH of a key rewrites the whole row through UpsertAPIKey. The binding has its own
// write path and must not be expressible through that rewrite: this is the failure that
// once silently reverted the per-key recording switches.
func TestAPIKeyFeishuBindingSurvivesARowRewrite(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, keyID := feishuFixture(t, db)

	if err := db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{
		OpenID: "ou_alice", UnionID: "on_alice", Name: "张三", BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	// Exactly what handleAdminPatchKey does: read the row, edit some fields, write it
	// back. The Feishu columns are not part of that write.
	row, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	row.TagsJSON = `["dsh"]`
	row.Status = "active"
	row.RecordInputMode = "user"
	row.FeishuOpenID = "" // even a caller that forgot to copy the field cannot clear it
	row.FeishuUnionID = ""
	row.FeishuName = ""
	row.FeishuBoundBy = ""
	if _, err := db.UpsertAPIKey(ctx, row); err != nil {
		t.Fatal(err)
	}

	after, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "ou_alice" || after.FeishuUnionID != "on_alice" || after.FeishuName != "张三" {
		t.Fatalf("a row rewrite cleared the Feishu binding: %+v", after)
	}
	if after.FeishuBoundAt == nil || after.FeishuBoundBy != "admin" {
		t.Fatalf("a row rewrite cleared the binding metadata: %+v", after)
	}
	if after.TagsJSON != `["dsh"]` || after.RecordInputMode != "user" {
		t.Fatalf("the rewrite itself did not take effect: %+v", after)
	}
	if after.AccountID != accountID {
		t.Fatalf("account changed: %d", after.AccountID)
	}
}

// The new column must also be readable through the list path the console uses.
func TestAPIKeyListCarriesFeishuBinding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, keyID := feishuFixture(t, db)
	if err := db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{OpenID: "ou_alice", Name: "张三"}); err != nil {
		t.Fatal(err)
	}
	keys, err := db.ListAPIKeys(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].FeishuOpenID != "ou_alice" {
		t.Fatalf("list did not carry the binding: %+v", keys)
	}
}

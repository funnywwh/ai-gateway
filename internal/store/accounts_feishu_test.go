package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// The account-level Feishu identity (M70) mirrors the key-level one (M60) and exists for
// the same reason: the directory sync writes "this Feishu person is this account" as one
// column-scoped statement that no account rewrite can express.

func TestAccountFeishuBindingRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "sync-owner", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.FeishuOpenID != "" || fresh.FeishuBoundAt != nil {
		t.Fatalf("a fresh account carries a Feishu identity: %+v", fresh)
	}

	boundAt := time.Date(2026, 9, 22, 8, 30, 0, 0, time.UTC)
	if err := db.BindAccountFeishu(ctx, accountID, domain.FeishuBinding{
		OpenID: "ou_wang", UnionID: "on_wang", Name: "王五", BoundAt: boundAt, BoundBy: "sync",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FeishuOpenID != "ou_wang" || got.FeishuUnionID != "on_wang" || got.FeishuName != "王五" {
		t.Fatalf("binding not persisted: %+v", got)
	}
	if got.FeishuBoundAt == nil || !got.FeishuBoundAt.Equal(boundAt) || got.FeishuBoundBy != "sync" {
		t.Fatalf("binding metadata not persisted: %+v", got)
	}

	byOpenID, err := db.FindAccountByFeishuOpenID(ctx, "ou_wang")
	if err != nil || byOpenID == nil || byOpenID.ID != accountID {
		t.Fatalf("resolve by open id: %v %+v", err, byOpenID)
	}
	if missing, err := db.FindAccountByFeishuOpenID(ctx, "ou_nobody"); err != nil || missing != nil {
		t.Fatalf("an unbound identity must resolve to nothing, got %v %+v", err, missing)
	}
	if blank, err := db.FindAccountByFeishuOpenID(ctx, "   "); err != nil || blank != nil {
		t.Fatalf("a blank identity must resolve to nothing, got %v %+v", err, blank)
	}

	changed, err := db.UnbindAccountFeishu(ctx, accountID)
	if err != nil || !changed {
		t.Fatalf("unbind = %v, %v; want changed", changed, err)
	}
	again, err := db.UnbindAccountFeishu(ctx, accountID)
	if err != nil || again {
		t.Fatalf("second unbind = %v, %v; want unchanged", again, err)
	}
	cleared, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.FeishuOpenID != "" || cleared.FeishuUnionID != "" || cleared.FeishuName != "" ||
		cleared.FeishuBoundAt != nil || cleared.FeishuBoundBy != "" {
		t.Fatalf("unbind left fields behind: %+v", cleared)
	}
	if err := db.BindAccountFeishu(ctx, accountID, domain.FeishuBinding{}); err == nil {
		t.Fatal("binding an empty identity was accepted")
	}
	if err := db.BindAccountFeishu(ctx, 999999, domain.FeishuBinding{OpenID: "ou_x"}); err == nil {
		t.Fatal("binding to a non-existent account was accepted")
	}
}

// One Feishu person is one account. The unique index makes that a database invariant, and
// the losing write must surface as a 409 without disturbing the winner.
func TestAccountFeishuBindingIsUniquePerIdentity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	first, err := db.UpsertAccount(ctx, &domain.Account{Name: "sync-first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.UpsertAccount(ctx, &domain.Account{Name: "sync-second"})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.BindAccountFeishu(ctx, first, domain.FeishuBinding{OpenID: "ou_shared", Name: "同名"}); err != nil {
		t.Fatal(err)
	}
	err = db.BindAccountFeishu(ctx, second, domain.FeishuBinding{OpenID: "ou_shared", Name: "同名"})
	if err == nil {
		t.Fatal("the same identity was bound to a second account")
	}
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("conflict error = %v, want a 409 API error", err)
	}

	winner, err := db.GetAccount(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if winner.FeishuOpenID != "ou_shared" {
		t.Fatalf("the original binding was disturbed: %+v", winner)
	}
	loser, err := db.GetAccount(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if loser.FeishuOpenID != "" {
		t.Fatalf("the rejected account kept a partial binding: %+v", loser)
	}
}

// UpsertAccount rewrites the whole row — it is what the console's account editor uses. The
// sync mapping has its own write path and must not be expressible through that rewrite.
func TestAccountFeishuBindingSurvivesARowRewrite(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "sync-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindAccountFeishu(ctx, accountID, domain.FeishuBinding{
		OpenID: "ou_zhou", UnionID: "on_zhou", Name: "周八", BoundBy: "sync",
	}); err != nil {
		t.Fatal(err)
	}

	// Exactly what handleAdminPatchAccount does: read the row, edit some fields, write it
	// back — with the Feishu fields zeroed, because they are not part of the editor's shape.
	row, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	row.Note = "re-edited"
	row.FeishuOpenID = ""
	row.FeishuUnionID = ""
	row.FeishuName = ""
	row.FeishuBoundAt = nil
	row.FeishuBoundBy = ""
	if _, err := db.UpsertAccount(ctx, row); err != nil {
		t.Fatal(err)
	}

	after, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FeishuOpenID != "ou_zhou" || after.FeishuUnionID != "on_zhou" || after.FeishuName != "周八" {
		t.Fatalf("a row rewrite cleared the Feishu mapping: %+v", after)
	}
	if after.FeishuBoundAt == nil || after.FeishuBoundBy != "sync" {
		t.Fatalf("a row rewrite cleared the mapping metadata: %+v", after)
	}
	if after.Note != "re-edited" {
		t.Fatalf("the rewrite itself did not take effect: %+v", after)
	}
}

// Key-level bindings (M60) are what the sync reads to recognize people bound before the
// account mapping existed.
func TestListAPIKeyFeishuIdentities(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "sync-owner"})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "ran-key", KeyPrefix: "sk-gw-feishu3", KeyHash: "hash-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second, unbound key must stay out of the answer.
	if _, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "plain-key", KeyPrefix: "sk-gw-feishu4", KeyHash: "hash-4",
	}); err != nil {
		t.Fatal(err)
	}

	identities, err := db.ListAPIKeyFeishuIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 0 {
		t.Fatalf("nothing is bound yet: %+v", identities)
	}

	if err := db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{
		OpenID: "ou_wang", UnionID: "on_wang", Name: "王五", BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	identities, err = db.ListAPIKeyFeishuIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 {
		t.Fatalf("want exactly the one bound key: %+v", identities)
	}
	got := identities[0]
	if got.KeyID != keyID || got.AccountID != accountID {
		t.Fatalf("identity carries the wrong key/account: %+v", got)
	}
	if got.Binding.OpenID != "ou_wang" || got.Binding.Name != "王五" || got.Binding.BoundBy != "admin" {
		t.Fatalf("binding projection incomplete: %+v", got.Binding)
	}
	if got.Binding.BoundAt.IsZero() {
		t.Fatalf("bound_at lost: %+v", got.Binding)
	}
}

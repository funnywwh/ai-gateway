package store

import (
	"context"
	"testing"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/secret"
)

// Two secrets may wear the same display label — sub2api lets callers pick their key values, so a
// migration meets this on day one — and each is still its own row, reachable by its own hash.
func TestTwoKeysMayShareAPrefix(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}

	first := "sk-shared-label-0123456789abcdef"
	second := "sk-shared-label-ffffffffffffffffff"
	if secret.Prefix(first) != secret.Prefix(second) {
		t.Fatalf("the fixture must share a prefix: %q vs %q", secret.Prefix(first), secret.Prefix(second))
	}

	firstID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "first",
		KeyPrefix: secret.Prefix(first), KeyHash: secret.Hash(first), RecordInputMode: "inherit",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "second",
		KeyPrefix: secret.Prefix(second), KeyHash: secret.Hash(second), RecordInputMode: "inherit",
	})
	if err != nil {
		t.Fatalf("a second key wearing the same label must be stored: %v", err)
	}
	if firstID == secondID {
		t.Fatalf("the two keys collapsed into one row (%d)", firstID)
	}

	// Each is reachable by its own hash, and one's hash never resolves to the other.
	gotFirst, err := db.GetAPIKeyByHash(ctx, secret.Hash(first))
	if err != nil || gotFirst.ID != firstID {
		t.Fatalf("first key by hash = %+v, err = %v", gotFirst, err)
	}
	gotSecond, err := db.GetAPIKeyByHash(ctx, secret.Hash(second))
	if err != nil || gotSecond.ID != secondID {
		t.Fatalf("second key by hash = %+v, err = %v", gotSecond, err)
	}

	// The label is now a question with two answers.
	byLabel, err := db.ListAPIKeysByPrefix(ctx, secret.Prefix(first))
	if err != nil {
		t.Fatal(err)
	}
	if len(byLabel) != 2 {
		t.Fatalf("rows sharing a label = %d, want 2", len(byLabel))
	}
	if found, err := db.FindAPIKeyByPrefix(ctx, secret.Prefix(first)); err != nil || found == nil {
		t.Fatalf("the mint-time pre-check must see the label as taken: %+v (err %v)", found, err)
	}
	if free, err := db.FindAPIKeyByPrefix(ctx, "sk-nobody000"); err != nil || free != nil {
		t.Fatalf("an unused label must be free: %+v (err %v)", free, err)
	}

	// An unknown secret is an authentication failure on the data plane's port and an ordinary
	// "not here" on the administrative one.
	if _, err := db.GetAPIKeyByHash(ctx, secret.Hash("sk-gw-nobody")); !domain.IsUnauthorized(err) {
		t.Fatalf("an unknown hash must read as unauthorized, got %v", err)
	}
	if missing, err := db.FindAPIKeyByHash(ctx, secret.Hash("sk-gw-nobody")); err != nil || missing != nil {
		t.Fatalf("an unknown hash must read as (nil, nil), got %+v (err %v)", missing, err)
	}
}

// The hash is the identity, so upserting it again is an update — including the label, which is
// the only thing an importer is allowed to correct.
func TestUpsertAPIKeyIsIdempotentByHash(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme-2"})
	if err != nil {
		t.Fatal(err)
	}

	token := "sk-idempotent-0123456789abcdef"
	id, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "before", KeyPrefix: secret.Prefix(token),
		KeyHash: secret.Hash(token), RecordInputMode: "inherit", Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}

	againID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "after", KeyPrefix: "sk-corrected-label",
		KeyHash: secret.Hash(token), RecordInputMode: "inherit", Status: "disabled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if againID != id {
		t.Fatalf("the same secret must stay one row: %d -> %d", id, againID)
	}
	row, err := db.GetAPIKeyByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != "after" || row.KeyPrefix != "sk-corrected-label" || row.Status != "disabled" {
		t.Fatalf("the update did not land: %+v", row)
	}
	keys, err := db.ListAPIKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("rows = %d, want 1: a re-upsert must not duplicate", len(keys))
	}
}

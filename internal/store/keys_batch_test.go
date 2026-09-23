package store

import (
	"context"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// M80's batch import validates a whole batch before it writes, so the store method it calls
// has to be all-or-nothing as well: the failure mode this test rules out is a batch that is
// half written when the database refuses row N, which would leave the operator guessing which
// of their keys are live.
func TestUpsertAPIKeysIsAtomic(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	row := func(name, prefix string) *domain.APIKey {
		return &domain.APIKey{AccountID: accountID, Name: name, KeyPrefix: prefix,
			KeyHash: "hash-" + prefix, Status: "active"}
	}

	// The happy path: rows come back in the order they were given, with their ids resolved.
	keys := []*domain.APIKey{row("first", "sk-gw-first"), row("second", "sk-gw-second")}
	ids, err := db.UpsertAPIKeys(ctx, keys)
	if err != nil {
		t.Fatalf("batch upsert: %v", err)
	}
	if len(ids) != 2 || ids[0] == 0 || ids[1] == 0 || ids[0] == ids[1] {
		t.Fatalf("ids = %v, want two distinct ids", ids)
	}
	for i, key := range keys {
		if key.ID != ids[i] {
			t.Errorf("key %d carries id %d, want %d", i, key.ID, ids[i])
		}
	}

	// An invalid row in the middle refuses the whole batch: the two good rows must not land.
	_, err = db.UpsertAPIKeys(ctx, []*domain.APIKey{
		row("third", "sk-gw-third"),
		{AccountID: accountID, Name: "broken", KeyPrefix: "sk-gw-broken"}, // no hash
	})
	if err == nil {
		t.Fatal("a batch with an invalid row must fail")
	}
	stored, err := db.ListAPIKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("a refused batch left %d rows, want the original 2", len(stored))
	}
	for _, key := range stored {
		if key.Name == "third" {
			t.Fatalf("a refused batch wrote its earlier rows: %+v", key)
		}
	}

	// The upsert still updates in place by prefix, exactly like the single-key path.
	again := []*domain.APIKey{row("first-renamed", "sk-gw-first")}
	againIDs, err := db.UpsertAPIKeys(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if againIDs[0] != ids[0] {
		t.Fatalf("re-upsert changed the id: %d → %d", ids[0], againIDs[0])
	}
	stored, err = db.ListAPIKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("re-upsert changed the row count to %d", len(stored))
	}
	renamed := false
	for _, key := range stored {
		if key.ID == ids[0] && key.Name == "first-renamed" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("re-upsert did not rename the row: %+v", stored)
	}
}

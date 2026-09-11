package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// seedRequestLog writes one row with an explicit age in days.
func seedRequestLog(t *testing.T, db *DB, id string, ageDays int) {
	t.Helper()
	if err := db.PutRequestLog(context.Background(), &domain.RequestLogRecord{
		RequestID: id, APIKeyID: 1, AccountID: 1, Endpoint: "/v1/responses",
		RequestJSON: `{"input":"ping"}`, Status: "completed",
		CreatedAt: time.Now().UTC().AddDate(0, 0, -ageDays),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRequestLogsHonoursCutoffAndLimit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	seedRequestLog(t, db, "req_old_1", 40)
	seedRequestLog(t, db, "req_old_2", 31)
	seedRequestLog(t, db, "req_edge", 30) // exactly on the cutoff: kept
	seedRequestLog(t, db, "req_new", 1)

	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	deleted, err := db.PruneRequestLogs(ctx, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("limit must bound one call, deleted %d", deleted)
	}

	deleted, err = db.PruneRequestLogs(ctx, cutoff, 500)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("only the second old row was due, deleted %d", deleted)
	}

	logs, err := db.ListRequestLogs(ctx, 0, time.Time{}, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, row := range logs {
		kept[row.RequestID] = true
	}
	if kept["req_old_1"] || kept["req_old_2"] {
		t.Fatalf("rows older than the cutoff must be gone: %v", kept)
	}
	if !kept["req_edge"] || !kept["req_new"] {
		t.Fatalf("the cutoff row and fresh rows must survive: %v", kept)
	}
}

func TestPruneExpiredResponsesKeepsRowsWithoutExpiry(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	rows := []*domain.ResponseRecord{
		{ID: "resp_expired", Model: "m", Status: "completed", ExpiresAt: &past},
		{ID: "resp_live", Model: "m", Status: "completed", ExpiresAt: &future},
		{ID: "resp_forever", Model: "m", Status: "completed"}, // retention disabled: no expiry
	}
	for _, rec := range rows {
		if err := db.PutResponse(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := db.PruneExpiredResponses(ctx, now, 500)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want only the expired row", deleted)
	}
	if _, err := db.GetResponse(ctx, "resp_expired"); err == nil {
		t.Fatal("the expired response must be gone")
	}
	for _, id := range []string{"resp_live", "resp_forever"} {
		if _, err := db.GetResponse(ctx, id); err != nil {
			t.Fatalf("%s must survive: %v", id, err)
		}
	}
}

func TestPruneOnAnEmptyDatabaseIsANoOp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if deleted, err := db.PruneRequestLogs(ctx, time.Now().UTC(), 500); err != nil || deleted != 0 {
		t.Fatalf("request logs: deleted=%d err=%v", deleted, err)
	}
	if deleted, err := db.PruneExpiredResponses(ctx, time.Now().UTC(), 500); err != nil || deleted != 0 {
		t.Fatalf("responses: deleted=%d err=%v", deleted, err)
	}
	// A non-positive limit falls back to the batch default instead of deleting everything.
	seedRequestLog(t, db, "req_old", 90)
	if deleted, err := db.PruneRequestLogs(ctx, time.Now().UTC(), 0); err != nil || deleted != 1 {
		t.Fatalf("limit=0 must fall back to a batch: deleted=%d err=%v", deleted, err)
	}
}

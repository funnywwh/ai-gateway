package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

// The startup backfill (M72) decides whether a person who could sign in yesterday still can
// today, so what it says afterwards matters as much as what it writes: an operator reading the
// log has to be able to tell "nothing to do" from "12 moved, 1 refused".
func TestMigrateKeyFeishuBindingsMovesAndReports(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	log, logged := capturingLogger()

	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "colin", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "colin-key", KeyPrefix: "sk-gw-colin", KeyHash: "hash-colin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{
		OpenID: "ou_colin", UnionID: "on_colin", Name: "李智超", BoundBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	migrateKeyFeishuBindings(ctx, db, log)

	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "ou_colin" || account.FeishuBoundBy != "key-migration" {
		t.Fatalf("the identity did not reach the account: %+v", account)
	}
	key, err := db.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if key.FeishuOpenID != "" {
		t.Fatalf("the key kept its identity: %+v", key)
	}

	out := logged.String()
	if !strings.Contains(out, "legacy key-level Feishu bindings migrated to accounts") ||
		!strings.Contains(out, "migrated=1") || !strings.Contains(out, "keys_cleared=1") {
		t.Fatalf("the migration must report what it did:\n%s", out)
	}

	// The audit entry is what makes a move attributable later, and it has to name both sides:
	// the account that gained the identity and the key it came from.
	audit, err := db.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range audit {
		if entry.Action != "feishu_bind" || entry.TargetType != "account" {
			continue
		}
		var changes map[string]any
		if err := json.Unmarshal([]byte(entry.ChangesJSON), &changes); err != nil {
			t.Fatalf("audit changes are not JSON: %v", err)
		}
		if changes["from_key_id"] != float64(keyID) || changes["open_id"] != "ou_colin" {
			t.Fatalf("audit entry does not name the source: %v", changes)
		}
		found = true
	}
	if !found {
		t.Fatalf("no audit entry recorded the migration: %+v", audit)
	}
}

// A migration with nothing to move stays silent: every deployment that never used the
// key-level binding sees an unchanged startup log.
func TestMigrateKeyFeishuBindingsIsSilentWithoutWork(t *testing.T) {
	db := openTestStore(t)
	log, logged := capturingLogger()
	migrateKeyFeishuBindings(context.Background(), db, log)
	if strings.TrimSpace(logged.String()) != "" {
		t.Fatalf("an empty migration must not log: %s", logged.String())
	}
}

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

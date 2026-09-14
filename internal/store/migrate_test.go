package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "test.db")
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	first, err := db.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("expected at least one applied migration")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate must succeed: %v", err)
	}
	second, err := db.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Fatalf("migration count changed: %v -> %v", first, second)
	}
}

func TestSchemaHasCoreTables(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	want := []string{
		"accounts", "api_keys", "providers", "provider_models", "models", "model_mappings",
		"routes", "tags", "mcp_tokens", "ledger_entries", "invoices", "invoice_lines",
		"usage_records", "request_logs", "responses", "backup_jobs", "audit_logs", "settings",
		// M32 console chat: conversations are owned by administrator accounts, so these
		// tables reference admin_users rather than accounts.
		"chat_sessions", "chat_turns", "chat_messages", "chat_tool_calls",
		"chat_skills", "chat_artifacts",
	}
	for _, table := range want {
		var name string
		err := db.Reader().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	rows, err := db.Reader().QueryContext(ctx, "PRAGMA table_info(accounts)")
	if err != nil {
		t.Fatal(err)
	}
	foundTags := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if name == "tags_json" {
			foundTags = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if !foundTags {
		t.Fatal("accounts.tags_json column missing")
	}
}

func TestWALEnabled(t *testing.T) {
	db := testDB(t)
	var mode string
	if err := db.Reader().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

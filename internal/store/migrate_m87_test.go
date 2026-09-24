package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/config"

	_ "modernc.org/sqlite"
)

// M87 turned the old lookup key (the display prefix) into a label and made the hash the identity.
// The schema has to say so: one unique index per secret, and the prefix indexes demoted to
// non-unique.
func TestHashIndexesAreUniqueAndPrefixIndexesAreNot(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	unique := map[string]string{}
	rows, err := db.Reader().QueryContext(ctx, `
		SELECT name, sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(ddl, " UNIQUE ") {
			unique[name] = ddl
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"idx_api_keys_hash", "idx_mcp_tokens_hash"} {
		if _, ok := unique[want]; !ok {
			t.Errorf("%s must be a unique index: %v", want, unique)
		}
	}
	for _, gone := range []string{"idx_api_keys_prefix", "idx_mcp_tokens_prefix"} {
		if _, ok := unique[gone]; ok {
			t.Errorf("%s must not be unique any more (M87): two keys may share a label", gone)
		}
	}
	for _, want := range []string{"idx_api_keys_prefix_lookup", "idx_mcp_tokens_prefix_lookup"} {
		var name string
		if err := db.Reader().QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", want).Scan(&name); err != nil {
			t.Errorf("%s (the non-unique label index) is missing: %v", want, err)
		}
	}
}

// The one state that can refuse the upgrade: two rows sharing a hash, which can only come from
// the hash-form import registering the same secret twice under different labels. The migration
// must fail loudly (and roll back) instead of picking a winner — a deployment ends up not
// starting, with an operator-readable reason, rather than silently losing a key.
func TestMigration0028RefusesDuplicateHashes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "duplicate-hashes.db")

	// Build a pre-M87 database: every migration before 0028 applied, the two M87 ones still
	// pending. Applying the bodies directly is what Migrate would do; skipping 0028/0029 leaves
	// exactly the schema an operator would upgrade from.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, createMigrationsTable); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	var pending []int
	for _, m := range migrations {
		if m.version >= 28 {
			pending = append(pending, m.version)
			continue
		}
		if _, err := raw.ExecContext(ctx, m.body); err != nil {
			raw.Close()
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := raw.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, 0)", m.version, m.name); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	if len(pending) != 2 {
		raw.Close()
		t.Fatalf("expected the two M87 migrations to be pending, got %v", pending)
	}
	// Two rows, same secret (same hash), different labels: the state an inconsistent import
	// produces, and the only one 0028 cannot express.
	for _, prefix := range []string{"sk-duplicate", "sk-f69aeca55"} {
		if _, err := raw.ExecContext(ctx, `
			INSERT INTO api_keys(account_id, name, key_prefix, key_hash, tags_json, grants_json,
			  policy_json, record_input_mode, record_output_text, record_reasoning, status,
			  created_by, created_at)
			VALUES(1, 'dup', ?, ?, '', '', '', 'inherit', 0, 0, 'active', 'test', 0)`,
			prefix, strings.Repeat("a", 64)); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default().Database
	cfg.Path = path
	if _, err := Open(ctx, cfg); err == nil {
		t.Fatal("Open must refuse a database whose hashes are duplicated")
	} else if !strings.Contains(err.Error(), "key_hash") || !strings.Contains(err.Error(), "0028") {
		t.Fatalf("the failure must name the column and the migration: %v", err)
	}

	// The migration ran inside a transaction, so nothing about the old schema changed: the rows
	// are all still there and no hash index was created.
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var rows int
	if err := check.QueryRowContext(ctx, "SELECT count(*) FROM api_keys").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("rows after the failed migration = %d, want 2 (the migration must roll back)", rows)
	}
	var name string
	err = check.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_api_keys_hash'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the unique hash index must not exist after the rollback: %v", err)
	}
	// The operator's fix, then a retry: keeping one row is a decision a person makes, and the
	// migration is happy once the duplicates are gone.
	if _, err := check.ExecContext(ctx, "DELETE FROM api_keys WHERE rowid = (SELECT min(rowid) FROM api_keys)"); err != nil {
		t.Fatal(err)
	}
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("the migration must succeed once the duplicate is gone: %v", err)
	}
	defer db.Close()
	applied, err := db.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != len(migrations) {
		t.Fatalf("applied migrations = %d, want %d", len(applied), len(migrations))
	}
}

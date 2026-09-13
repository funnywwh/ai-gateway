package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

func TestModelReasoningJSONPersistsIndependently(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	model := &domain.Model{
		PublicName:    "reasoning-model",
		Enabled:       true,
		PolicyJSON:    `{"rpm":60}`,
		ReasoningJSON: `{"mode":"force","effort":"high"}`,
	}
	if _, err := db.UpsertModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetModelByName(ctx, model.PublicName)
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicyJSON != model.PolicyJSON {
		t.Errorf("policy_json = %q, want %q", got.PolicyJSON, model.PolicyJSON)
	}
	if got.ReasoningJSON != model.ReasoningJSON {
		t.Errorf("reasoning_json = %q, want %q", got.ReasoningJSON, model.ReasoningJSON)
	}

	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, config.Database{Path: path, BusyTimeoutMS: 5000, WAL: true, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err = db.GetModelByName(ctx, model.PublicName)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningJSON != `{"mode":"force","effort":"high"}` {
		t.Fatalf("reasoning was not retained across reopen: %+v", got)
	}

	model.ReasoningJSON = `{"mode":"default","effort":"minimal"}`
	if _, err := db.UpsertModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetModelByName(ctx, model.PublicName)
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicyJSON != `{"rpm":60}` || got.ReasoningJSON != model.ReasoningJSON {
		t.Fatalf("updated model = %+v", got)
	}

	model.ReasoningJSON = ""
	if _, err := db.UpsertModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetModelByName(ctx, model.PublicName)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReasoningJSON != "" || got.PolicyJSON != `{"rpm":60}` {
		t.Fatalf("clear changed independent fields: %+v", got)
	}

}

func TestModelReasoningMigrationPreservesOldModelsAndDefaultsEmpty(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "pre-reasoning.db")
	old := openDatabaseAtMigration(t, ctx, cfg, 14)
	if _, err := old.ExecContext(ctx, `
INSERT INTO models(public_name, display_name, aliases_json, enabled, sale_pricing_json, policy_json, created_at, updated_at)
VALUES('old-model', 'Old model', '["old"]', 1, '{"input":1}', '{"rpm":60}', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	model, err := db.GetModelByName(ctx, "old-model")
	if err != nil {
		t.Fatal(err)
	}
	if model.PolicyJSON != `{"rpm":60}` || model.SalePricingJSON != `{"input":1}` || model.ReasoningJSON != "" {
		t.Fatalf("migrated model = %+v", model)
	}
}

// openDatabaseAtMigration creates a historical database from the embedded migration
// chain, then marks exactly that chain as applied. It lets the next Open exercise
// the real upgrade path rather than simulating a new database.
func openDatabaseAtMigration(t *testing.T, ctx context.Context, cfg config.Database, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", buildDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version > version {
			break
		}
		if _, err := db.ExecContext(ctx, migration.body); err != nil {
			t.Fatalf("apply historical migration %s: %v", migration.name, err)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)",
			migration.version, migration.name, time.Now().UTC().Unix()); err != nil {
			t.Fatalf("record historical migration %s: %v", migration.name, err)
		}
	}
	return db
}

func TestBootstrapRejectsInvalidReasoningBeforeSeeding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, err := db.Bootstrap(ctx, config.Bootstrap{
		Mode: "upsert",
		Accounts: []config.BootstrapAccount{{
			Name: "must-not-be-created",
		}},
		Models: []config.BootstrapModel{{
			PublicName: "invalid-reasoning",
			Reasoning:  &config.BootstrapModelReasoning{Mode: "bad", Effort: "high"},
		}},
	}, "")
	if err == nil {
		t.Fatal("Bootstrap accepted invalid reasoning")
	}
	if _, err := db.GetAccountByName(ctx, "must-not-be-created"); !domain.IsNotFound(err) {
		t.Fatalf("bootstrap mutated accounts before validation: %v", err)
	}
}

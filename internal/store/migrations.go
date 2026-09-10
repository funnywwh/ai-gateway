package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	body    string
}

// loadMigrations reads embedded migrations and validates their naming/ordering.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		idx := strings.Index(base, "_")
		if idx <= 0 {
			return nil, fmt.Errorf("store: migration %q must be named NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(base[:idx])
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has a non-numeric version: %w", e.Name(), err)
		}
		body, err := migrationsFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: base, body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("store: duplicate migration version %d", out[i].version)
		}
	}
	return out, nil
}

// Migrate applies every pending migration. It is idempotent: applied versions are skipped.
func (db *DB) Migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := db.write.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
)`

func (db *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := db.write.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: list applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan migration version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate migrations: %w", err)
	}
	return applied, nil
}

func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)",
		m.version, m.name, time.Now().UTC().Unix()); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.name, err)
	}
	return nil
}

// AppliedMigrations returns the applied migration versions (for diagnostics/tests).
func (db *DB) AppliedMigrations(ctx context.Context) ([]int, error) {
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(applied))
	for v := range applied {
		out = append(out, v)
	}
	sort.Ints(out)
	return out, nil
}

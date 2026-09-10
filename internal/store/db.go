// Package store implements persistence on SQLite using a pure-Go driver (no cgo),
// with a single-writer pool plus a reader pool (WAL allows concurrent readers).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/winger/ai-gateway/internal/config"
)

// DB holds the two connection pools over one SQLite file.
type DB struct {
	write *sql.DB
	read  *sql.DB
	path  string
}

// Open opens (creating if needed) the database, applies migrations and returns the handle.
func Open(ctx context.Context, cfg config.Database) (*DB, error) {
	if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create directory %s: %w", dir, err)
		}
	}

	dsn := buildDSN(cfg)

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	// One writer: serialises all writes in-process, which removes SQLITE_BUSY contention.
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	maxConns := cfg.MaxOpenConns
	if maxConns < 2 {
		maxConns = 2
	}
	read.SetMaxOpenConns(maxConns)
	read.SetMaxIdleConns(maxConns)

	db := &DB{write: write, read: read, path: cfg.Path}
	if err := db.ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) ping(ctx context.Context) error {
	if err := db.write.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping writer: %w", err)
	}
	if err := db.read.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping reader: %w", err)
	}
	return nil
}

// buildDSN renders the sqlite DSN with the pragmas the gateway relies on.
func buildDSN(cfg config.Database) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", cfg.BusyTimeoutMS))
	if cfg.WAL {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	return "file:" + cfg.Path + "?" + q.Encode()
}

// Path returns the SQLite file path.
func (db *DB) Path() string { return db.path }

// Writer exposes the single-writer pool (settlement writer, migrations).
func (db *DB) Writer() *sql.DB { return db.write }

// Reader exposes the reader pool (dashboards, MCP queries, admin reads).
func (db *DB) Reader() *sql.DB { return db.read }

// Close closes both pools.
func (db *DB) Close() error {
	var first error
	if db.read != nil {
		if err := db.read.Close(); err != nil {
			first = err
		}
	}
	if db.write != nil {
		if err := db.write.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

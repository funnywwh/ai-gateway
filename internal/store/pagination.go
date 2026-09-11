package store

import (
	"context"
	"fmt"
)

// normalizeLimit clamps a page size to (default, max]. A history table must never be
// read whole because a caller forgot to pass a limit, and a caller asking for more
// than the cap gets the cap rather than an error: the console offers fixed page sizes
// and an MCP agent should not have to know every endpoint's ceiling.
func normalizeLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// countRows runs COUNT(*) with the very WHERE clause a list query used, so a page and
// its total always describe the same row set (docs/design/m24-console-pagination.md).
// table is a package-level literal, never caller input.
func (db *DB) countRows(ctx context.Context, table, where string, args []any, label string) (int, error) {
	var total int
	if err := db.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count %s: %w", label, err)
	}
	return total, nil
}

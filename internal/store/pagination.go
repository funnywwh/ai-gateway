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

// historyPageOrder ends every paged history list. It is deliberately "created_at DESC,
// id DESC" rather than "id DESC": these lists filter on a created_at window, so SQLite
// drives them from the created_at index, and only an ORDER BY that index already provides
// avoids its "USE TEMP B-TREE FOR ORDER BY" fallback. That fallback buffers *every row of
// the window* before LIMIT is applied — and request_logs rows carry the recorded request
// bodies, so the console's 50-row page spent ~1.2s sorting ~700 MB on a 1.8k-row database
// (docs/design/m24-console-pagination.md §8.10). id stays the tiebreaker so rows stamped
// in the same second still page deterministically. Newest-first by time is not a semantic
// change: created_at and id are both stamped when the row is written, so the two orders
// agree, and created_at is what the console actually displays.
const historyPageOrder = " ORDER BY created_at DESC, id DESC"

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

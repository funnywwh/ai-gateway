package store

import (
	"context"
	"fmt"
	"time"
)

// PruneRequestLogs deletes recorded requests older than the cutoff, at most limit rows per
// call, and reports how many it deleted.
//
// Retention is deliberately batched: every write in this process goes through one
// connection (see Open), so a single unbounded DELETE over hundreds of megabytes would
// hold that connection long enough to time out the writes happening behind it — the very
// failure this cleanup exists to keep rare.
func (db *DB) PruneRequestLogs(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	res, err := db.write.ExecContext(ctx, `
DELETE FROM request_logs WHERE id IN (
    SELECT id FROM request_logs WHERE created_at < ? ORDER BY id LIMIT ?
)`, unix(before), limit)
	if err != nil {
		return 0, fmt.Errorf("store: prune request logs: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune request logs: %w", err)
	}
	return int(deleted), nil
}

// PruneExpiredResponses deletes stored Responses API responses whose expiry has passed.
// A NULL expires_at means "keep forever" (retention disabled) and is never touched.
func (db *DB) PruneExpiredResponses(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	res, err := db.write.ExecContext(ctx, `
DELETE FROM responses WHERE id IN (
    SELECT id FROM responses WHERE expires_at IS NOT NULL AND expires_at < ? LIMIT ?
)`, unix(now), limit)
	if err != nil {
		return 0, fmt.Errorf("store: prune responses: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune responses: %w", err)
	}
	return int(deleted), nil
}

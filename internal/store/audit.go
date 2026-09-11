package store

import (
	"context"
	"fmt"
	"time"
)

// AuditEntry is one administrative change record.
type AuditEntry struct {
	ID          int64
	Actor       string
	Action      string
	TargetType  string
	TargetID    string
	ChangesJSON string
	Result      string
	CreatedAt   time.Time
}

// InsertAudit appends an audit record.
func (db *DB) InsertAudit(ctx context.Context, e *AuditEntry) error {
	if e == nil || e.Action == "" {
		return nil
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO audit_logs(actor, action, target_type, target_id, changes_json, result, created_at)
VALUES(?,?,?,?,?,?,?)`,
		e.Actor, e.Action, e.TargetType, e.TargetID, e.ChangesJSON, e.Result, unix(e.CreatedAt)); err != nil {
		return fmt.Errorf("store: insert audit entry: %w", err)
	}
	return nil
}

// ListAudit returns the newest audit records (first page).
func (db *DB) ListAudit(ctx context.Context, limit int) ([]*AuditEntry, error) {
	return db.ListAuditPage(ctx, limit, 0)
}

// ListAuditPage returns one page of the audit trail, newest first.
func (db *DB) ListAuditPage(ctx context.Context, limit, offset int) ([]*AuditEntry, error) {
	limit = normalizeLimit(limit, 100, 1000)
	if offset < 0 {
		offset = 0
	}
	rows, err := db.read.QueryContext(ctx, `
SELECT id, actor, action, target_type, target_id, changes_json, result, created_at
FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()

	out := []*AuditEntry{}
	for rows.Next() {
		var (
			e         AuditEntry
			createdAt int64
		)
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TargetType, &e.TargetID,
			&e.ChangesJSON, &e.Result, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan audit entry: %w", err)
		}
		e.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate audit: %w", err)
	}
	return out, nil
}

// CountAudit counts the whole audit trail.
func (db *DB) CountAudit(ctx context.Context) (int, error) {
	return db.countRows(ctx, "audit_logs", "", nil, "audit logs")
}

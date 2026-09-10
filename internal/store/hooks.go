package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const hookCols = `id, name, type, url, secret, events_json, include_content, max_bytes,
	sample_rate, enabled, created_at`

// ListHooks returns every configured hook.
func (db *DB) ListHooks(ctx context.Context) ([]*domain.Hook, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT "+hookCols+" FROM hooks ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("store: list hooks: %w", err)
	}
	defer rows.Close()

	out := []*domain.Hook{}
	for rows.Next() {
		var (
			h         domain.Hook
			include   int
			enabled   int
			createdAt int64
		)
		if err := rows.Scan(&h.ID, &h.Name, &h.Type, &h.URL, &h.Secret, &h.EventsJSON,
			&include, &h.MaxBytes, &h.SampleRate, &enabled, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan hook: %w", err)
		}
		h.IncludeContent = include != 0
		h.Enabled = enabled != 0
		h.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate hooks: %w", err)
	}
	return out, nil
}

// UpsertHook inserts or updates a hook (matched by unique name).
func (db *DB) UpsertHook(ctx context.Context, h *domain.Hook) (int64, error) {
	if h == nil || h.Name == "" {
		return 0, domain.ErrInvalidRequest("hook name is required")
	}
	if h.Type == "" {
		h.Type = "webhook"
	}
	switch h.Type {
	case "webhook", "jsonl":
	default:
		return 0, domain.ErrInvalidRequest("hook type must be webhook|jsonl")
	}
	if h.SampleRate == 0 {
		h.SampleRate = 1
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now().UTC()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO hooks(name, type, url, secret, events_json, include_content, max_bytes, sample_rate, enabled, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  type = excluded.type,
  url = excluded.url,
  secret = excluded.secret,
  events_json = excluded.events_json,
  include_content = excluded.include_content,
  max_bytes = excluded.max_bytes,
  sample_rate = excluded.sample_rate,
  enabled = excluded.enabled`,
		h.Name, h.Type, h.URL, h.Secret, h.EventsJSON, boolInt(h.IncludeContent),
		h.MaxBytes, h.SampleRate, boolInt(h.Enabled), unix(h.CreatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert hook %q: %w", h.Name, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM hooks WHERE name = ?", h.Name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("store: hook %q vanished after upsert", h.Name)
		}
		return 0, fmt.Errorf("store: resolve hook id: %w", err)
	}
	h.ID = id
	return id, nil
}

// DeleteHook removes a hook.
func (db *DB) DeleteHook(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM hooks WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete hook %d: %w", id, err)
	}
	return nil
}

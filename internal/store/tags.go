package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const tagCols = "id, name, description, grants_json, policy_json, priority, created_at"

func scanTag(row rowScanner) (*domain.Tag, error) {
	var (
		t         domain.Tag
		createdAt int64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Description, &t.GrantsJSON, &t.PolicyJSON,
		&t.Priority, &createdAt); err != nil {
		return nil, err
	}
	t.CreatedAt = timeFromUnix(createdAt)
	return &t, nil
}

// ListTags returns all tags ordered by evaluation priority.
func (db *DB) ListTags(ctx context.Context) ([]*domain.Tag, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT "+tagCols+" FROM tags ORDER BY priority, name")
	if err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	defer rows.Close()

	out := []*domain.Tag{}
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan tag: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tags: %w", err)
	}
	return out, nil
}

// GetTagByName loads one tag.
func (db *DB) GetTagByName(ctx context.Context, name string) (*domain.Tag, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+tagCols+" FROM tags WHERE name = ?", name)
	t, err := scanTag(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("tag " + name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get tag %q: %w", name, err)
	}
	return t, nil
}

// UpsertTag inserts or updates a tag (matched by unique name).
func (db *DB) UpsertTag(ctx context.Context, t *domain.Tag) (int64, error) {
	if t == nil || t.Name == "" {
		return 0, domain.ErrInvalidRequest("tag name is required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO tags(name, description, grants_json, policy_json, priority, created_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  description = excluded.description,
  grants_json = excluded.grants_json,
  policy_json = excluded.policy_json,
  priority = excluded.priority`,
		t.Name, t.Description, t.GrantsJSON, t.PolicyJSON, t.Priority, unix(t.CreatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert tag %q: %w", t.Name, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM tags WHERE name = ?", t.Name).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve tag id %q: %w", t.Name, err)
	}
	t.ID = id
	return id, nil
}

// DeleteTag removes a tag.
func (db *DB) DeleteTag(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM tags WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete tag %d: %w", id, err)
	}
	return nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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

// GetTagByID loads one tag by its numeric id.
//
// The id is the identity and the name is a label, which is what makes an update of a tag
// whose name is not an ASCII identifier possible at all: the UI edit path used to be an
// upsert *by name* (UpsertTag), so a name the writer refused to accept left that row
// permanently uneditable.
func (db *DB) GetTagByID(ctx context.Context, id int64) (*domain.Tag, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+tagCols+" FROM tags WHERE id = ?", id)
	t, err := scanTag(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("tag " + strconv.FormatInt(id, 10))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get tag %d: %w", id, err)
	}
	return t, nil
}

// UpdateTag writes every mutable column of an existing row, addressed by id.
//
// Unlike UpsertTag it can rename: the row keeps its id and therefore the audit trail and
// any reference that goes by id. Callers that must not rename simply never set Name to a
// different value (the management API refuses the change, because bindings in
// accounts.tags_json / api_keys.tags_json are stored by name).
func (db *DB) UpdateTag(ctx context.Context, t *domain.Tag) error {
	if t == nil || t.ID <= 0 {
		return domain.ErrInvalidRequest("tag id is required")
	}
	if t.Name == "" {
		return domain.ErrInvalidRequest("tag name is required")
	}
	res, err := db.write.ExecContext(ctx, `
UPDATE tags SET name = ?, description = ?, grants_json = ?, policy_json = ?, priority = ?
WHERE id = ?`, t.Name, t.Description, t.GrantsJSON, t.PolicyJSON, t.Priority, t.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("another tag already uses the name " + t.Name)
		}
		return fmt.Errorf("store: update tag %d: %w", t.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update tag %d: %w", t.ID, err)
	}
	if affected == 0 {
		return domain.ErrNotFound("tag " + strconv.FormatInt(t.ID, 10))
	}
	return nil
}

// DeleteTag removes a tag.
func (db *DB) DeleteTag(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM tags WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete tag %d: %w", id, err)
	}
	return nil
}

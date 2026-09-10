package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetSetting reads a JSON settings value; missing keys return ("", false, nil).
func (db *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := db.read.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetSetting writes a JSON settings value.
func (db *DB) SetSetting(ctx context.Context, key, valueJSON string) error {
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO settings(key, value_json, updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		key, valueJSON, unix(time.Now())); err != nil {
		return fmt.Errorf("store: set setting %q: %w", key, err)
	}
	return nil
}

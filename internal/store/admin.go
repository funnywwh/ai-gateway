package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// GetAdminUserByUsername loads one administrator.
func (db *DB) GetAdminUserByUsername(ctx context.Context, username string) (*domain.AdminUser, error) {
	row := db.read.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, created_at, last_login_at FROM admin_users WHERE username = ?", username)

	var (
		u           domain.AdminUser
		createdAt   int64
		lastLoginAt sql.NullInt64
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &createdAt, &lastLoginAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound("admin user " + username)
		}
		return nil, fmt.Errorf("store: get admin user: %w", err)
	}
	u.CreatedAt = timeFromUnix(createdAt)
	u.LastLoginAt = timePtrFromNull(lastLoginAt)
	return &u, nil
}

// UpsertAdminUser creates or updates an administrator (matched by username).
func (db *DB) UpsertAdminUser(ctx context.Context, u *domain.AdminUser) (int64, error) {
	if u == nil || u.Username == "" || u.PasswordHash == "" {
		return 0, domain.ErrInvalidRequest("admin user requires username and password hash")
	}
	if u.Role == "" {
		u.Role = "admin"
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO admin_users(username, password_hash, role, created_at, last_login_at)
VALUES(?,?,?,?,?)
ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash, role = excluded.role`,
		u.Username, u.PasswordHash, u.Role, unix(u.CreatedAt), unixPtr(u.LastLoginAt)); err != nil {
		return 0, fmt.Errorf("store: upsert admin user %q: %w", u.Username, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM admin_users WHERE username = ?", u.Username).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve admin user id: %w", err)
	}
	u.ID = id
	return id, nil
}

// TouchAdminLogin records the last successful login.
func (db *DB) TouchAdminLogin(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx,
		"UPDATE admin_users SET last_login_at = ? WHERE id = ?", unix(time.Now()), id); err != nil {
		return fmt.Errorf("store: touch admin login: %w", err)
	}
	return nil
}

// CreateAdminSession stores a session (token hash only).
func (db *DB) CreateAdminSession(ctx context.Context, id string, userID int64, tokenHash string, expiresAt time.Time) error {
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO admin_sessions(id, user_id, token_hash, expires_at, created_at) VALUES(?,?,?,?,?)`,
		id, userID, tokenHash, unix(expiresAt), unix(time.Now())); err != nil {
		return fmt.Errorf("store: create admin session: %w", err)
	}
	return nil
}

// GetAdminSession returns the session and its user when the session is still valid.
func (db *DB) GetAdminSession(ctx context.Context, id string) (*domain.AdminUser, time.Time, error) {
	row := db.read.QueryRowContext(ctx, `
SELECT u.id, u.username, u.password_hash, u.role, u.created_at, u.last_login_at, s.token_hash, s.expires_at
FROM admin_sessions s JOIN admin_users u ON u.id = s.user_id
WHERE s.id = ?`, id)

	var (
		u                       domain.AdminUser
		createdAt               int64
		lastLoginAt             sql.NullInt64
		tokenHash               string
		expiresAt               int64
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &createdAt, &lastLoginAt, &tokenHash, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, domain.ErrUnauthorized("invalid session")
		}
		return nil, time.Time{}, fmt.Errorf("store: get admin session: %w", err)
	}
	u.CreatedAt = timeFromUnix(createdAt)
	u.LastLoginAt = timePtrFromNull(lastLoginAt)
	expiry := timeFromUnix(expiresAt)
	if time.Now().UTC().After(expiry) {
		return nil, expiry, domain.ErrUnauthorized("session expired")
	}
	return &u, expiry, nil
}

// SessionTokenHash returns the stored hash for a session id (constant-time compared by the caller).
func (db *DB) SessionTokenHash(ctx context.Context, id string) (string, error) {
	var hash string
	if err := db.read.QueryRowContext(ctx, "SELECT token_hash FROM admin_sessions WHERE id = ?", id).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", domain.ErrUnauthorized("invalid session")
		}
		return "", fmt.Errorf("store: session hash: %w", err)
	}
	return hash, nil
}

// DeleteAdminSession removes one session (logout).
func (db *DB) DeleteAdminSession(ctx context.Context, id string) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM admin_sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete admin session: %w", err)
	}
	return nil
}

// PurgeExpiredSessions removes sessions past their expiry.
func (db *DB) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := db.write.ExecContext(ctx, "DELETE FROM admin_sessions WHERE expires_at < ?", unix(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: purge sessions: %w", err)
	}
	return res.RowsAffected()
}

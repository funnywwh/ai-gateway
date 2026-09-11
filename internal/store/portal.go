package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// DeleteAdminSessions removes every session of one administrator (password change, or
// an administrator revoking their own other sessions).
func (db *DB) DeleteAdminSessions(ctx context.Context, userID int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM admin_sessions WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("store: delete admin sessions: %w", err)
	}
	return nil
}

const portalUserCols = `id, account_id, username, password_hash, status, must_change_password,
	last_login_at, created_by, created_at`

func scanPortalUser(row rowScanner) (*domain.PortalUser, error) {
	var (
		user        domain.PortalUser
		mustChange  int
		lastLoginAt sql.NullInt64
		createdAt   int64
	)
	if err := row.Scan(&user.ID, &user.AccountID, &user.Username, &user.PasswordHash,
		&user.Status, &mustChange, &lastLoginAt, &user.CreatedBy, &createdAt); err != nil {
		return nil, err
	}
	user.MustChangePassword = mustChange != 0
	user.LastLoginAt = timePtrFromNull(lastLoginAt)
	user.CreatedAt = timeFromUnix(createdAt)
	return &user, nil
}

// UpsertPortalUser creates or updates a portal user (matched by username).
func (db *DB) UpsertPortalUser(ctx context.Context, user *domain.PortalUser) (int64, error) {
	if user == nil || user.Username == "" || user.AccountID == 0 {
		return 0, domain.ErrInvalidRequest("portal user requires username and account_id")
	}
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if user.Status == "" {
		user.Status = "active"
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO portal_users(account_id, username, password_hash, status, must_change_password,
  created_by, created_at)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(username) DO UPDATE SET
  account_id = excluded.account_id,
  password_hash = excluded.password_hash,
  status = excluded.status,
  must_change_password = excluded.must_change_password`,
		user.AccountID, user.Username, user.PasswordHash, user.Status, boolInt(user.MustChangePassword),
		user.CreatedBy, unix(user.CreatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert portal user %q: %w", user.Username, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx,
		"SELECT id FROM portal_users WHERE username = ?", user.Username).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve portal user id: %w", err)
	}
	user.ID = id
	return id, nil
}

// GetPortalUserByUsername loads one portal user (with the password hash).
func (db *DB) GetPortalUserByUsername(ctx context.Context, username string) (*domain.PortalUser, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+portalUserCols+" FROM portal_users WHERE username = ?", username)
	user, err := scanPortalUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("portal user " + username)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get portal user %q: %w", username, err)
	}
	return user, nil
}

// GetPortalUser loads one portal user by id.
func (db *DB) GetPortalUser(ctx context.Context, id int64) (*domain.PortalUser, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+portalUserCols+" FROM portal_users WHERE id = ?", id)
	user, err := scanPortalUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("portal user %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get portal user %d: %w", id, err)
	}
	return user, nil
}

// ListPortalUsers lists the portal users of one account (0 means every account).
func (db *DB) ListPortalUsers(ctx context.Context, accountID int64) ([]*domain.PortalUser, error) {
	query := "SELECT " + portalUserCols + " FROM portal_users"
	args := []any{}
	if accountID > 0 {
		query += " WHERE account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY account_id, username"
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list portal users: %w", err)
	}
	defer rows.Close()
	out := []*domain.PortalUser{}
	for rows.Next() {
		user, err := scanPortalUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan portal user: %w", err)
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate portal users: %w", err)
	}
	return out, nil
}

// SetPortalUserStatus enables or disables a portal user.
func (db *DB) SetPortalUserStatus(ctx context.Context, id int64, status string) error {
	res, err := db.write.ExecContext(ctx,
		"UPDATE portal_users SET status = ? WHERE id = ?", status, id)
	if err != nil {
		return fmt.Errorf("store: set portal user status: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return domain.ErrNotFound(fmt.Sprintf("portal user %d", id))
	}
	return nil
}

// CreatePortalSession stores a session for one portal user.
func (db *DB) CreatePortalSession(ctx context.Context, id string, userID int64, tokenHash string, expiresAt time.Time) error {
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO portal_sessions(id, user_id, token_hash, expires_at, created_at) VALUES(?,?,?,?,?)`,
		id, userID, tokenHash, unix(expiresAt), unix(time.Now())); err != nil {
		return fmt.Errorf("store: create portal session: %w", err)
	}
	return nil
}

// GetPortalSession returns the portal user of a live session.
func (db *DB) GetPortalSession(ctx context.Context, id string) (*domain.PortalUser, time.Time, error) {
	var userID int64
	var expiresAt int64
	if err := db.read.QueryRowContext(ctx,
		"SELECT user_id, expires_at FROM portal_sessions WHERE id = ?", id).Scan(&userID, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, domain.ErrUnauthorized("invalid session")
		}
		return nil, time.Time{}, fmt.Errorf("store: get portal session: %w", err)
	}
	expiry := timeFromUnix(expiresAt)
	if time.Now().UTC().After(expiry) {
		_ = db.DeletePortalSession(ctx, id)
		return nil, time.Time{}, domain.ErrUnauthorized("session expired")
	}
	user, err := db.GetPortalUser(ctx, userID)
	if err != nil {
		return nil, time.Time{}, err
	}
	if user.Status != "active" {
		return nil, time.Time{}, domain.ErrForbidden("this portal account is disabled")
	}
	return user, expiry, nil
}

// PortalSessionTokenHash returns the stored hash of a portal session token.
func (db *DB) PortalSessionTokenHash(ctx context.Context, id string) (string, error) {
	var hash string
	if err := db.read.QueryRowContext(ctx,
		"SELECT token_hash FROM portal_sessions WHERE id = ?", id).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", domain.ErrUnauthorized("invalid session")
		}
		return "", fmt.Errorf("store: portal session token hash: %w", err)
	}
	return hash, nil
}

// DeletePortalSession removes one portal session.
func (db *DB) DeletePortalSession(ctx context.Context, id string) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM portal_sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete portal session: %w", err)
	}
	return nil
}

// DeletePortalSessionsForUser removes every session of one portal user (password
// change or an explicit sign-out everywhere).
func (db *DB) DeletePortalSessionsForUser(ctx context.Context, userID int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM portal_sessions WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("store: delete portal sessions: %w", err)
	}
	return nil
}

// TouchPortalLogin records the last successful sign-in.
func (db *DB) TouchPortalLogin(ctx context.Context, userID int64) error {
	if _, err := db.write.ExecContext(ctx,
		"UPDATE portal_users SET last_login_at = ? WHERE id = ?", unix(time.Now()), userID); err != nil {
		return fmt.Errorf("store: touch portal login: %w", err)
	}
	return nil
}

// SeedPortalUser creates a portal user from bootstrap configuration, leaving the
// password untouched when the user already exists (upsert must never reset a password).
func (db *DB) SeedPortalUser(ctx context.Context, user *domain.PortalUser) (bool, error) {
	if existing, err := db.GetPortalUserByUsername(ctx, user.Username); err == nil {
		if existing.AccountID != user.AccountID || existing.Status != user.Status {
			if _, err := db.UpsertPortalUser(ctx, &domain.PortalUser{
				AccountID: user.AccountID, Username: user.Username,
				PasswordHash: existing.PasswordHash, Status: user.Status,
				CreatedBy: existing.CreatedBy, CreatedAt: existing.CreatedAt,
			}); err != nil {
				return false, err
			}
			return false, nil
		}
		return false, nil
	}
	if _, err := db.UpsertPortalUser(ctx, user); err != nil {
		return false, err
	}
	return true, nil
}

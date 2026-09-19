package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// adminUserCols is the column list every administrator read shares, in the order
// scanAdminUser expects. One list keeps the console page, the single-row reads and the
// session join from drifting apart (a missing column here once silently returned an empty
// binding for every administrator).
const adminUserCols = `u.id, u.username, u.password_hash, u.role, u.status, u.created_at, u.last_login_at,
	u.feishu_open_id, u.feishu_union_id, u.feishu_name, u.feishu_bound_at, u.feishu_bound_by, u.invite_nonce`

// scanAdminUser reads one row of adminUserCols.
func scanAdminUser(row interface{ Scan(...any) error }) (*domain.AdminUser, error) {
	var (
		u           domain.AdminUser
		createdAt   int64
		lastLoginAt sql.NullInt64
		boundAt     sql.NullInt64
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &createdAt, &lastLoginAt,
		&u.FeishuOpenID, &u.FeishuUnionID, &u.FeishuName, &boundAt, &u.FeishuBoundBy, &u.InviteNonce); err != nil {
		return nil, err
	}
	u.CreatedAt = timeFromUnix(createdAt)
	u.LastLoginAt = timePtrFromNull(lastLoginAt)
	u.FeishuBoundAt = timePtrFromNull(boundAt)
	return &u, nil
}

// GetAdminUserByUsername loads one administrator.
func (db *DB) GetAdminUserByUsername(ctx context.Context, username string) (*domain.AdminUser, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+adminUserCols+" FROM admin_users u WHERE u.username = ?", username)
	u, err := scanAdminUser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound("admin user " + username)
		}
		return nil, fmt.Errorf("store: get admin user: %w", err)
	}
	return u, nil
}

// GetAdminUser loads one administrator by id.
func (db *DB) GetAdminUser(ctx context.Context, id int64) (*domain.AdminUser, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+adminUserCols+" FROM admin_users u WHERE u.id = ?", id)
	u, err := scanAdminUser(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound(fmt.Sprintf("admin user %d", id))
		}
		return nil, fmt.Errorf("store: get admin user %d: %w", id, err)
	}
	return u, nil
}

// ListAdminUsers returns every administrator, oldest first, so the console page can show
// them in the order they were created.
func (db *DB) ListAdminUsers(ctx context.Context) ([]*domain.AdminUser, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT "+adminUserCols+" FROM admin_users u ORDER BY u.id")
	if err != nil {
		return nil, fmt.Errorf("store: list admin users: %w", err)
	}
	defer rows.Close()
	out := make([]*domain.AdminUser, 0, 8)
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan admin user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list admin users: %w", err)
	}
	return out, nil
}

// ActiveAdminCount counts the administrators that may both sign in and write. It is what
// keeps the console from removing the last one of them (M66): with that row gone, nobody
// could undo anything through the management API again.
func (db *DB) ActiveAdminCount(ctx context.Context) (int, error) {
	var count int
	if err := db.read.QueryRowContext(ctx, `
SELECT COUNT(*) FROM admin_users WHERE role = ? AND status = ?`,
		domain.RoleAdmin, domain.AdminActive).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count active admins: %w", err)
	}
	return count, nil
}

// CreateAdminUser inserts one administrator.
//
// password_hash may be empty: an invitation-only administrator has no password at all, and
// the column being NOT NULL is satisfied by the empty string (see migration 0023). The
// caller decides the status; an account without a password and without a binding is
// normally created "pending" so it cannot be mistaken for one that can sign in.
func (db *DB) CreateAdminUser(ctx context.Context, u *domain.AdminUser) (int64, error) {
	if u == nil || strings.TrimSpace(u.Username) == "" {
		return 0, domain.ErrInvalidRequest("an admin user requires a username")
	}
	if !domain.ValidAdminRole(u.Role) {
		return 0, domain.ErrInvalidRequest("an admin user role must be admin or viewer")
	}
	if u.Status == "" {
		u.Status = domain.AdminPending
	}
	if !domain.ValidAdminStatus(u.Status) {
		return 0, domain.ErrInvalidRequest("an admin user status must be pending, active or disabled")
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	result, err := db.write.ExecContext(ctx, `
INSERT INTO admin_users(username, password_hash, role, status, created_at, last_login_at)
VALUES(?,?,?,?,?,?)`,
		u.Username, u.PasswordHash, u.Role, u.Status, unix(u.CreatedAt), unixPtr(u.LastLoginAt))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, domain.ErrConflict("admin user " + u.Username + " already exists")
		}
		return 0, fmt.Errorf("store: create admin user %q: %w", u.Username, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: resolve admin user id: %w", err)
	}
	u.ID = id
	return id, nil
}

// UpdateAdminUserRole changes one administrator's role.
func (db *DB) UpdateAdminUserRole(ctx context.Context, id int64, role string) error {
	if !domain.ValidAdminRole(role) {
		return domain.ErrInvalidRequest("an admin user role must be admin or viewer")
	}
	return db.updateAdminUser(ctx, id, "role", role)
}

// SetAdminUserStatus changes one administrator's lifecycle state.
func (db *DB) SetAdminUserStatus(ctx context.Context, id int64, status string) error {
	if !domain.ValidAdminStatus(status) {
		return domain.ErrInvalidRequest("an admin user status must be pending, active or disabled")
	}
	return db.updateAdminUser(ctx, id, "status", status)
}

// SetAdminUserPassword replaces one administrator's password hash. An empty hash removes
// the password entirely, leaving the account to whichever other credential it has.
func (db *DB) SetAdminUserPassword(ctx context.Context, id int64, hash string) error {
	return db.updateAdminUser(ctx, id, "password_hash", hash)
}

// updateAdminUser writes exactly one column, so a role change cannot undo a password reset
// and a password reset cannot undo a status change. A write that matched no row is a
// not-found, never a silent success.
func (db *DB) updateAdminUser(ctx context.Context, id int64, column, value string) error {
	result, err := db.write.ExecContext(ctx, "UPDATE admin_users SET "+column+" = ? WHERE id = ?", value, id)
	if err != nil {
		return fmt.Errorf("store: update admin user %d %s: %w", id, column, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update admin user %d %s: %w", id, column, err)
	}
	if affected == 0 {
		if _, err := db.GetAdminUser(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// RotateAdminUserInvite records the handle of a freshly minted invitation link. Writing a
// new handle is what retires the previous link: the callback only accepts the handle the
// row holds, so an older link stops working the moment this returns.
func (db *DB) RotateAdminUserInvite(ctx context.Context, id int64, nonce string) error {
	if strings.TrimSpace(nonce) == "" {
		return domain.ErrInvalidRequest("an invitation requires a handle")
	}
	return db.updateAdminUser(ctx, id, "invite_nonce", nonce)
}

// DeleteAdminUser removes one administrator. Sessions and any console chat library owned by
// the row go with it (the foreign keys cascade), which is why the console says so before
// asking for confirmation.
func (db *DB) DeleteAdminUser(ctx context.Context, id int64) error {
	result, err := db.write.ExecContext(ctx, "DELETE FROM admin_users WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: delete admin user %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete admin user %d: %w", id, err)
	}
	if affected == 0 {
		return domain.ErrNotFound(fmt.Sprintf("admin user %d", id))
	}
	return nil
}

// BindAdminUserFeishu writes an administrator's Feishu identity, replacing any previous one,
// clearing the pending invitation and activating the account in the same statement.
//
// One statement for the same reason BindAPIKeyFeishu is one statement: the identity, the
// redeemed invitation and the activation are one fact ("this person may now sign in"), and
// splitting them would leave windows in which the account is bound but still pending, or
// bound while an invitation link from somebody else is still alive. The unique index over
// NULLIF(feishu_open_id, ”) makes "one Feishu identity, one administrator" a database
// invariant; the violation is reported as a conflict.
func (db *DB) BindAdminUserFeishu(ctx context.Context, id int64, binding domain.FeishuBinding) error {
	if strings.TrimSpace(binding.OpenID) == "" {
		return domain.ErrInvalidRequest("a Feishu binding requires an open_id")
	}
	boundAt := binding.BoundAt
	if boundAt.IsZero() {
		boundAt = time.Now().UTC()
	}
	result, err := db.write.ExecContext(ctx, `
UPDATE admin_users SET feishu_open_id = ?, feishu_union_id = ?, feishu_name = ?,
  feishu_bound_at = ?, feishu_bound_by = ?, invite_nonce = '', status = ?
WHERE id = ?`,
		binding.OpenID, binding.UnionID, binding.Name, unix(boundAt), binding.BoundBy,
		domain.AdminActive, id)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("this Feishu account is already bound to another administrator")
		}
		return fmt.Errorf("store: bind admin user %d to Feishu: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: bind admin user %d to Feishu: %w", id, err)
	}
	if affected == 0 {
		if _, err := db.GetAdminUser(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// UnbindAdminUserFeishu clears an administrator's Feishu identity and reports whether
// anything changed, so the caller can answer idempotently.
//
// The account is left alone otherwise: unbinding does not disable it (a password may still
// work) and does not delete it. An administrator whose only credential was the binding ends
// up back in "pending", which is exactly what it is.
func (db *DB) UnbindAdminUserFeishu(ctx context.Context, id int64) (bool, error) {
	result, err := db.write.ExecContext(ctx, `
UPDATE admin_users SET feishu_open_id = '', feishu_union_id = '', feishu_name = '',
  feishu_bound_at = NULL, feishu_bound_by = '',
  status = CASE WHEN status = ? AND password_hash = '' THEN ? ELSE status END
WHERE id = ? AND feishu_open_id <> ''`, domain.AdminActive, domain.AdminPending, id)
	if err != nil {
		return false, fmt.Errorf("store: unbind admin user %d from Feishu: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unbind admin user %d from Feishu: %w", id, err)
	}
	return affected > 0, nil
}

// FindAdminUserByFeishuOpenID resolves a bound Feishu identity to its administrator. A
// missing row is (nil, nil): on the login path "this identity is not an administrator" is an
// ordinary answer, and it is also the answer that keeps a customer's key binding from ever
// reaching the console.
func (db *DB) FindAdminUserByFeishuOpenID(ctx context.Context, openID string) (*domain.AdminUser, error) {
	if strings.TrimSpace(openID) == "" {
		return nil, nil
	}
	row := db.read.QueryRowContext(ctx, "SELECT "+adminUserCols+" FROM admin_users u WHERE u.feishu_open_id = ?", openID)
	u, err := scanAdminUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find admin user by Feishu open id: %w", err)
	}
	return u, nil
}

// UpsertAdminUser creates or updates an administrator (matched by username). It is the
// bootstrap seeder, so it deliberately touches only the credential and the role: a
// deployment that disables the seeded administrator in the console must not have that
// decision silently reverted by the next restart, and the same goes for a Feishu binding.
func (db *DB) UpsertAdminUser(ctx context.Context, u *domain.AdminUser) (int64, error) {
	if u == nil || u.Username == "" || u.PasswordHash == "" {
		return 0, domain.ErrInvalidRequest("admin user requires username and password hash")
	}
	if u.Role == "" {
		u.Role = domain.RoleAdmin
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
//
// The row's status is part of the answer: an administrator an operator disabled loses every
// existing session at the next request, not at the next login. The role comes from the join
// on every call for the same reason — a demotion takes effect immediately.
func (db *DB) GetAdminSession(ctx context.Context, id string) (*domain.AdminUser, time.Time, error) {
	row := db.read.QueryRowContext(ctx, `
SELECT `+adminUserCols+`, s.token_hash, s.expires_at
FROM admin_sessions s JOIN admin_users u ON u.id = s.user_id
WHERE s.id = ?`, id)

	var (
		u           domain.AdminUser
		createdAt   int64
		lastLoginAt sql.NullInt64
		boundAt     sql.NullInt64
		tokenHash   string
		expiresAt   int64
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &createdAt, &lastLoginAt,
		&u.FeishuOpenID, &u.FeishuUnionID, &u.FeishuName, &boundAt, &u.FeishuBoundBy, &u.InviteNonce,
		&tokenHash, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, domain.ErrUnauthorized("invalid session")
		}
		return nil, time.Time{}, fmt.Errorf("store: get admin session: %w", err)
	}
	u.CreatedAt = timeFromUnix(createdAt)
	u.LastLoginAt = timePtrFromNull(lastLoginAt)
	u.FeishuBoundAt = timePtrFromNull(boundAt)
	expiry := timeFromUnix(expiresAt)
	if time.Now().UTC().After(expiry) {
		return nil, expiry, domain.ErrUnauthorized("session expired")
	}
	if u.Status != domain.AdminActive {
		return nil, expiry, domain.ErrUnauthorized("administrator account is not active")
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

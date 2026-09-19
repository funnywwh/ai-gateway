-- M66: the Feishu identity, the lifecycle state and the invitation handle of one console
-- administrator (docs/design/m66-console-admin-feishu-login.md).
--
-- The console used to have exactly one administrator, seeded from bootstrap.admin. It now
-- has a row per administrator: role says what the row may do (admin | viewer), status says
-- whether it may sign in at all (pending | active | disabled), and feishu_open_id is the
-- identity that lets its owner sign in by scanning a code with Feishu instead of typing a
-- password. As with api_keys (migration 0022), open_id — the app-scoped user id returned by
-- /authen/v1/user_info — is the binding key, and union_id/name are display and audit facts.
--
-- status defaults to 'active' so every administrator that already exists keeps working.
-- password_hash stays NOT NULL: an invitation-only administrator is written with an empty
-- hash, which makes password verification fail rather than making the column optional.
--
-- invite_nonce is the handle of the newest invitation link that has not been redeemed yet.
-- Nothing is stored for an invitation that was never generated; regenerating one replaces
-- this value, which is exactly what retires the previous link.
ALTER TABLE admin_users ADD COLUMN status          TEXT    NOT NULL DEFAULT 'active';
ALTER TABLE admin_users ADD COLUMN feishu_open_id  TEXT    NOT NULL DEFAULT '';
ALTER TABLE admin_users ADD COLUMN feishu_union_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE admin_users ADD COLUMN feishu_name     TEXT    NOT NULL DEFAULT '';
ALTER TABLE admin_users ADD COLUMN feishu_bound_at INTEGER;
ALTER TABLE admin_users ADD COLUMN feishu_bound_by TEXT    NOT NULL DEFAULT '';
ALTER TABLE admin_users ADD COLUMN invite_nonce    TEXT    NOT NULL DEFAULT '';

-- One Feishu identity signs in as at most one administrator. The index is over
-- NULLIF(open_id, '') for the same reason it is on api_keys: SQLite treats NULLs in a
-- unique index as distinct while empty strings would all collide, and administrators
-- without a binding must not conflict with each other.
CREATE UNIQUE INDEX IF NOT EXISTS idx_admin_users_feishu_open_id ON admin_users(NULLIF(feishu_open_id, ''));

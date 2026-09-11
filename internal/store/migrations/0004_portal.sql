-- Customer self-service portal identities. Usernames are global because the login form
-- asks for one field; each user belongs to exactly one account, and deleting the account
-- cascades to its users and sessions.
CREATE TABLE IF NOT EXISTS portal_users (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id           INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    username             TEXT    NOT NULL UNIQUE,
    password_hash        TEXT    NOT NULL,
    status               TEXT    NOT NULL DEFAULT 'active',
    must_change_password INTEGER NOT NULL DEFAULT 0,
    last_login_at        INTEGER,
    created_by           TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_portal_users_account ON portal_users(account_id);

CREATE TABLE IF NOT EXISTS portal_sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES portal_users(id) ON DELETE CASCADE,
    token_hash TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_portal_sessions_user ON portal_sessions(user_id);

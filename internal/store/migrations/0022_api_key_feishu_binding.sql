-- M60: the Feishu identity bound to one API key (docs/design/m60-aigw-key-feishu-binding.md).
--
-- A bound key is what lets a person sign in to the DSH portal with Feishu instead of
-- pasting a key: the console binds it once through Feishu's OAuth flow, and the identity
-- proves ownership at bind time. open_id is the app-scoped user id (stable, and returned
-- by /authen/v1/user_info with no extra permission), so it — not a name or an email — is
-- the binding key. union_id and name are display/audit facts only.
--
-- feishu_bound_by records the admin who performed the binding: an audit trail, not a
-- permission check.
ALTER TABLE api_keys ADD COLUMN feishu_open_id  TEXT    NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN feishu_union_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN feishu_name     TEXT    NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN feishu_bound_at INTEGER;
ALTER TABLE api_keys ADD COLUMN feishu_bound_by TEXT    NOT NULL DEFAULT '';

-- One Feishu identity binds at most one key. The index is over NULLIF(open_id, ''),
-- because SQLite treats NULLs in a unique index as distinct while empty strings would all
-- collide: unbound keys must not conflict with each other, bound ones must.
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_feishu_open_id ON api_keys(NULLIF(feishu_open_id, ''));

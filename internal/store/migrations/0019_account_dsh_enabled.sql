-- M52: account-level opt-in for the dsh multi-tenant gateway (docs/design/m52-dsh-enable.md).
-- The aigw console owns the truth; the dshgw portal asks POST /v1/dshgw/authorize before it
-- issues a session. 0 (disabled) is the default so existing accounts keep their behavior.
ALTER TABLE accounts ADD COLUMN dsh_enabled INTEGER NOT NULL DEFAULT 0;

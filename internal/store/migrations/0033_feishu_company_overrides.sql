-- M95: the console's per-field override of a company whose values come from the configuration.
--
-- M94 added a name-only override table (feishu_company_names, name NOT NULL: "a row exists = the name
-- was changed"). M95 lets the console edit four local fields plus — for client companies — the
-- application secret, and "only the note was changed" has to be expressible, so the table is rebuilt:
-- every column is NULLable, NULL meaning "not overridden, use the configured value".
--
-- secret_enc holds a client company's secret sealed with the app id as AAD (a different scope from
-- the row-id-sealed secrets of console-registered companies, so the two kinds cannot be swapped).
-- The deployment's own application (feishu.app_id) is never stored here: its secret also drives the
-- login flows, so overriding it would make directory reads and logins disagree.
--
-- The rows M94 wrote are carried over as-is.
CREATE TABLE IF NOT EXISTS feishu_company_overrides (
    app_id     TEXT PRIMARY KEY,
    name       TEXT,
    root_node  TEXT,
    note       TEXT,
    enabled    INTEGER,
    secret_enc BLOB,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO feishu_company_overrides(app_id, name, updated_by, updated_at)
    SELECT app_id, name, updated_by, updated_at FROM feishu_company_names;

DROP TABLE IF EXISTS feishu_company_names;

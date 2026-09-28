-- M93: the companies the console manages.
--
-- Until M93 the only way to import a client company's organization structure was
-- feishu.companies in the configuration file, which meant editing YAML and restarting the
-- gateway. This table holds the console-managed half of that registry: one row per company,
-- with its own self-built Feishu application.
--
-- The deployment's own application (feishu.app_id) is deliberately NOT a row here. It carries
-- the callback URL and the three signing keys of the identity flows, so it belongs to the
-- deployment configuration; moving it into the database would create a second source of truth
-- for "which app do people log in with". The companies list therefore merges configuration and
-- this table, with the configuration winning when an app id appears in both.
--
-- secret_enc is AES-256-GCM (internal/creds) with a scoped AAD: "feishu_app" + the row id. The
-- scope is what keeps a company secret from being replayable as a provider credential (and the
-- other way round) when the numeric ids coincide. A NULL/empty blob means "no secret yet" —
-- a state the console shows and the sync refuses, never a crash.
CREATE TABLE IF NOT EXISTS feishu_apps (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    app_id     TEXT    NOT NULL,
    secret_enc BLOB,
    -- Optional override of the local company node's name (empty = the company name).
    root_node  TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    -- Who wrote the row, for the audit trail the console shows next to it.
    created_by TEXT    NOT NULL DEFAULT '',
    updated_by TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);

-- The company parameter of every org-sync endpoint accepts an app id or a company name, so both
-- have to be unique for the parameter to be unambiguous. The merged list checks the configuration
-- side in code (a config company and a row with the same app id are the same company).
CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_apps_app_id ON feishu_apps(app_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_apps_name ON feishu_apps(name);

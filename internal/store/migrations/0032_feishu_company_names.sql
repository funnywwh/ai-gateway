-- M94: the console's rename of a company whose name comes from the configuration.
--
-- The identity application and the companies written into feishu.companies get their name from the
-- configuration file, and the console could not change it (M93's rule: the file owns those rows).
-- That turned out to be the first thing an operator wants to fix — "the company is called 客户组一,
-- not 本公司" — and editing the file plus restarting the gateway is a poor answer to a rename.
--
-- So this table holds exactly one thing: a name override for companies that have no console row of
-- their own. Console-registered companies (feishu_apps) keep renaming their own row. Precedence when
-- the company list is built: console row > this override > configuration.
--
-- The override carries who wrote it and when, because a name that silently differs from the file an
-- operator is looking at needs an owner.
CREATE TABLE IF NOT EXISTS feishu_company_names (
    app_id     TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

-- M92: company scope for the Feishu directory sync.
--
-- One aigw used to serve exactly one Feishu enterprise (the deployment's identity app).
-- From M92 a deployment can import several companies' organization structures, each through
-- its own self-built app (feishu.companies in the configuration), so every Feishu fact needs
-- to say *which company* it belongs to:
--
--   * org_nodes.feishu_app_id scopes a department link to one company's application. The old
--     index made feishu_department_id globally unique; open_department_id is only unique inside
--     a tenant, and "two companies both have a 研发部" is the normal shape, so uniqueness has
--     to be per application. The virtual root "0" (the company itself) rides the same index,
--     which is how a company node is found again after either side is renamed.
--   * feishu_person_links holds the person ↔ account mapping for companies that are NOT the
--     identity app. The identity app's mapping stays on accounts.feishu_* because that column
--     *is* the DSH portal login identity (M72) — this table must never become a second source
--     for "who may log in".
--
-- Legacy rows (written before M92) have feishu_app_id = '' and are adopted by the deployment's
-- identity app at startup (store.AdoptLegacyFeishuScope), which is the only writer of that
-- backfill: a SQL migration cannot know which app_id the configuration states.

ALTER TABLE org_nodes ADD COLUMN feishu_app_id TEXT NOT NULL DEFAULT '';

DROP INDEX IF EXISTS idx_org_nodes_feishu_dept;
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_nodes_feishu_dept_app
    ON org_nodes(feishu_app_id, NULLIF(feishu_department_id, ''));
CREATE INDEX IF NOT EXISTS idx_org_nodes_feishu_app ON org_nodes(feishu_app_id);

-- A mapping row is "this person in that company's directory = this account". Both keys are
-- unique per company so the tables answer the two questions the sync asks: which account does
-- this person map to, and which person does this account map to. The second index is what
-- keeps one company's directory from putting two of its people on one account; across companies
-- the same account may appear twice (one human in two customers' directories is legitimate).
CREATE TABLE IF NOT EXISTS feishu_person_links (
    feishu_app_id TEXT    NOT NULL,
    open_id       TEXT    NOT NULL,
    union_id      TEXT    NOT NULL DEFAULT '',
    name          TEXT    NOT NULL DEFAULT '',
    account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    bound_by      TEXT    NOT NULL DEFAULT '',
    bound_at      INTEGER,
    created_at    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (feishu_app_id, open_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_person_links_account
    ON feishu_person_links(feishu_app_id, account_id);
CREATE INDEX IF NOT EXISTS idx_feishu_person_links_account_only
    ON feishu_person_links(account_id);

-- M70: the Feishu directory sync (docs/design/m70-feishu-org-sync.md) links two existing
-- entities to Feishu objects: org nodes to departments, accounts to people.
--
-- Both sides mirror the M60 shape on api_keys (feishu_open_id/union_id/name + who bound it
-- and when): open_id is the app-scoped, stable identifier, so it — never a name — is the
-- merge key. union_id and name are display/audit facts. feishu_bound_by records who wrote
-- the binding ("sync" for an automatic merge, an admin username for a manual one); it is an
-- audit trail, not a permission check.
--
-- org_nodes.feishu_department_id is what makes a department survive a rename: the first sync
-- matches by sibling name and stamps the id, and every later sync recognizes the node by id.
-- feishu_synced_at is informational (when the link was last written), like feishu_bound_at.
--
-- Both empty-string columns are indexed through NULLIF: SQLite treats NULLs in a unique index
-- as distinct, so unlinked rows must not collide with each other while linked ids stay unique.
-- The same expression form is already in use by 0022_api_key_feishu_binding.sql.

ALTER TABLE org_nodes ADD COLUMN feishu_department_id TEXT NOT NULL DEFAULT '';
ALTER TABLE org_nodes ADD COLUMN feishu_synced_at INTEGER;
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_nodes_feishu_dept
    ON org_nodes(NULLIF(feishu_department_id, ''));

ALTER TABLE accounts ADD COLUMN feishu_open_id  TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN feishu_union_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN feishu_name     TEXT    NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN feishu_bound_at INTEGER;
ALTER TABLE accounts ADD COLUMN feishu_bound_by TEXT    NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_feishu_open_id
    ON accounts(NULLIF(feishu_open_id, ''));

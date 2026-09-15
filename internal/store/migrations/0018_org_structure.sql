-- Organization structure: an independent tree of nodes that accounts can be attached to.
--
-- This is deliberately NOT a second tag table. A node carries tag *names* (tags_json, the
-- same format as accounts.tags_json) and those names are inherited by every account in the
-- node's subtree, which is how an organization grants or restricts access without touching
-- each account. Tags themselves stay in `tags`; this table only references them by name.
--
-- parent_id is NULL for a root: the structure is a forest, because "several companies, each
-- with departments" is the normal shape. The foreign key is RESTRICT rather than CASCADE
-- because a subtree delete has to be ordered by depth (see store.DeleteOrgNode); an
-- accidental cascade would delete children while the caller believes it deleted a leaf.
CREATE TABLE IF NOT EXISTS org_nodes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id  INTEGER REFERENCES org_nodes(id),
    name       TEXT    NOT NULL,
    note       TEXT    NOT NULL DEFAULT '',
    -- Tag names inherited by the whole subtree. Same encoding as accounts.tags_json so the
    -- resolver can feed both through one decoder.
    tags_json  TEXT    NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 100,
    created_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_org_nodes_parent ON org_nodes(parent_id, sort_order, name);

-- Sibling names are unique. COALESCE maps "no parent" (a root) to 0 so roots are compared
-- against each other: without it SQLite treats NULLs as distinct and two roots could share
-- a name. Cross-parent duplicates stay legal — two divisions may each have a 研发部.
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_nodes_sibling_name
    ON org_nodes(COALESCE(parent_id, 0), name);

-- Membership is many-to-many: one account may sit in several nodes (a shared service, a
-- person in two teams), and one node holds many accounts. Both sides cascade: deleting a
-- node removes its memberships, and the account rows themselves are never touched by a node
-- delete. The reverse index matters because the snapshot build reads memberships by account.
CREATE TABLE IF NOT EXISTS org_node_accounts (
    node_id    INTEGER NOT NULL REFERENCES org_nodes(id) ON DELETE CASCADE,
    account_id INTEGER NOT NULL REFERENCES accounts(id)  ON DELETE CASCADE,
    created_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, account_id)
);

CREATE INDEX IF NOT EXISTS idx_org_node_accounts_account ON org_node_accounts(account_id);

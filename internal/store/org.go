package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const orgNodeCols = "id, parent_id, name, note, tags_json, sort_order, created_at, updated_at"

func scanOrgNode(row rowScanner) (*domain.OrgNode, error) {
	var (
		n                    domain.OrgNode
		parentID             sql.NullInt64
		createdAt, updatedAt int64
	)
	if err := row.Scan(&n.ID, &parentID, &n.Name, &n.Note, &n.TagsJSON, &n.SortOrder, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if parentID.Valid {
		value := parentID.Int64
		n.ParentID = &value
	}
	n.CreatedAt = timeFromUnix(createdAt)
	n.UpdatedAt = timeFromUnix(updatedAt)
	return &n, nil
}

// ListOrgNodes returns every node ordered by how the tree is displayed (parent, then the
// sort order and name the index uses). The caller rebuilds the hierarchy; doing it in SQL
// would only move the same recursion into a query that cannot be indexed.
func (db *DB) ListOrgNodes(ctx context.Context) ([]*domain.OrgNode, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+orgNodeCols+" FROM org_nodes ORDER BY COALESCE(parent_id, 0), sort_order, name, id")
	if err != nil {
		return nil, fmt.Errorf("store: list org nodes: %w", err)
	}
	defer rows.Close()

	out := []*domain.OrgNode{}
	for rows.Next() {
		n, err := scanOrgNode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan org node: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate org nodes: %w", err)
	}
	return out, nil
}

// GetOrgNode loads one node by id.
func (db *DB) GetOrgNode(ctx context.Context, id int64) (*domain.OrgNode, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+orgNodeCols+" FROM org_nodes WHERE id = ?", id)
	n, err := scanOrgNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("org node " + strconv.FormatInt(id, 10))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get org node %d: %w", id, err)
	}
	return n, nil
}

// CreateOrgNode inserts a node and returns its id.
//
// The existence of the parent is checked here rather than left to the foreign key: a raw
// constraint failure surfaces as a 500, which reads like a gateway fault instead of "you
// pointed the new node at a department that does not exist" (the same reasoning as the API
// key import path).
func (db *DB) CreateOrgNode(ctx context.Context, n *domain.OrgNode) (int64, error) {
	if n == nil || n.Name == "" {
		return 0, domain.ErrInvalidRequest("org node name is required")
	}
	if parent := n.ParentIDValue(); parent != 0 {
		if _, err := db.GetOrgNode(ctx, parent); err != nil {
			return 0, err
		}
	}
	now := time.Now().UTC()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = now
	}
	n.UpdatedAt = now

	res, err := db.write.ExecContext(ctx, `
INSERT INTO org_nodes(parent_id, name, note, tags_json, sort_order, created_at, updated_at)
VALUES(?,?,?,?,?,?,?)`,
		int64PtrNull(n.ParentID), n.Name, n.Note, n.TagsJSON, n.SortOrder, unix(n.CreatedAt), unix(n.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, domain.ErrConflict(siblingNameConflict(n))
		}
		return 0, fmt.Errorf("store: create org node %q: %w", n.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: create org node %q: %w", n.Name, err)
	}
	n.ID = id
	return id, nil
}

// UpdateOrgNode writes every mutable column of an existing row, addressed by id.
func (db *DB) UpdateOrgNode(ctx context.Context, n *domain.OrgNode) error {
	if n == nil || n.ID <= 0 {
		return domain.ErrInvalidRequest("org node id is required")
	}
	if n.Name == "" {
		return domain.ErrInvalidRequest("org node name is required")
	}
	if parent := n.ParentIDValue(); parent != 0 {
		if parent == n.ID {
			return domain.ErrInvalidRequest("an org node cannot be its own parent")
		}
		if _, err := db.GetOrgNode(ctx, parent); err != nil {
			return err
		}
	}
	n.UpdatedAt = time.Now().UTC()
	res, err := db.write.ExecContext(ctx, `
UPDATE org_nodes SET parent_id = ?, name = ?, note = ?, tags_json = ?, sort_order = ?, updated_at = ?
WHERE id = ?`,
		int64PtrNull(n.ParentID), n.Name, n.Note, n.TagsJSON, n.SortOrder, unix(n.UpdatedAt), n.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict(siblingNameConflict(n))
		}
		return fmt.Errorf("store: update org node %d: %w", n.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update org node %d: %w", n.ID, err)
	}
	if affected == 0 {
		return domain.ErrNotFound("org node " + strconv.FormatInt(n.ID, 10))
	}
	return nil
}

// DeleteOrgNode removes one node, or its whole subtree when cascade is set, and reports how
// many nodes disappeared.
//
// The subtree is collected with a recursive CTE and then deleted deepest-first inside one
// transaction. The order is not cosmetic: org_nodes.parent_id is a RESTRICT foreign key, so
// deleting a parent before its children would fail — and cascading in the schema instead
// would delete subtrees the caller never asked about.
func (db *DB) DeleteOrgNode(ctx context.Context, id int64, cascade bool) (int, error) {
	if _, err := db.GetOrgNode(ctx, id); err != nil {
		return 0, err
	}
	ids, err := db.orgSubtreeDeepestFirst(ctx, id)
	if err != nil {
		return 0, err
	}
	if len(ids) > 1 && !cascade {
		return 0, domain.ErrConflict(fmt.Sprintf(
			"org node %d has %d descendant(s); deleting it would remove the whole subtree. "+
				"Pass cascade=true to confirm, or move/delete the children first", id, len(ids)-1))
	}

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin org node delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deleted := 0
	for _, nodeID := range ids {
		res, err := tx.ExecContext(ctx, "DELETE FROM org_nodes WHERE id = ?", nodeID)
		if err != nil {
			return 0, fmt.Errorf("store: delete org node %d: %w", nodeID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: delete org node %d: %w", nodeID, err)
		}
		deleted += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit org node delete: %w", err)
	}
	return deleted, nil
}

// orgSubtreeDeepestFirst returns the node and its descendants ordered so that every node
// comes after its own children.
func (db *DB) orgSubtreeDeepestFirst(ctx context.Context, id int64) ([]int64, error) {
	rows, err := db.read.QueryContext(ctx, `
WITH RECURSIVE subtree(id, depth) AS (
  SELECT id, 0 FROM org_nodes WHERE id = ?
  UNION ALL
  SELECT n.id, s.depth + 1 FROM org_nodes n JOIN subtree s ON n.parent_id = s.id
)
SELECT id FROM subtree ORDER BY depth DESC, id DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("store: collect org subtree of %d: %w", id, err)
	}
	defer rows.Close()

	out := []int64{}
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			return nil, fmt.Errorf("store: scan org subtree row: %w", err)
		}
		out = append(out, nodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate org subtree: %w", err)
	}
	if len(out) == 0 {
		return nil, domain.ErrNotFound("org node " + strconv.FormatInt(id, 10))
	}
	return out, nil
}

// ListOrgMemberships returns every (node, account) pair.
//
// It is read whole because the routing snapshot needs all of them at once: resolving "what
// does this account inherit" per request would put a query on the hot path, which is exactly
// what this design avoids.
func (db *DB) ListOrgMemberships(ctx context.Context) ([]domain.OrgMembership, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT node_id, account_id FROM org_node_accounts ORDER BY account_id, node_id")
	if err != nil {
		return nil, fmt.Errorf("store: list org memberships: %w", err)
	}
	defer rows.Close()

	out := []domain.OrgMembership{}
	for rows.Next() {
		var m domain.OrgMembership
		if err := rows.Scan(&m.NodeID, &m.AccountID); err != nil {
			return nil, fmt.Errorf("store: scan org membership: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate org memberships: %w", err)
	}
	return out, nil
}

// ListOrgMembershipsByAccount groups the memberships for the snapshot build.
func (db *DB) ListOrgMembershipsByAccount(ctx context.Context) (map[int64][]int64, error) {
	rows, err := db.ListOrgMemberships(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]int64, len(rows))
	for _, m := range rows {
		out[m.AccountID] = append(out[m.AccountID], m.NodeID)
	}
	return out, nil
}

// ListOrgNodeAccountIDs returns the accounts attached to one node, in id order.
func (db *DB) ListOrgNodeAccountIDs(ctx context.Context, nodeID int64) ([]int64, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT account_id FROM org_node_accounts WHERE node_id = ? ORDER BY account_id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: list org node %d accounts: %w", nodeID, err)
	}
	defer rows.Close()

	out := []int64{}
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, fmt.Errorf("store: scan org node account: %w", err)
		}
		out = append(out, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate org node accounts: %w", err)
	}
	return out, nil
}

// SetOrgNodeMembers replaces one node's members with exactly accountIDs.
//
// Replacement (not add/remove) is what makes the operation idempotent and replayable: the
// caller states the membership it wants, and running the same call twice changes nothing.
// Every id is validated before the transaction so a typo answers 404 instead of a foreign
// key error that reads like a server fault.
func (db *DB) SetOrgNodeMembers(ctx context.Context, nodeID int64, accountIDs []int64) error {
	if _, err := db.GetOrgNode(ctx, nodeID); err != nil {
		return err
	}
	unique, err := db.checkAccountsExist(ctx, accountIDs)
	if err != nil {
		return err
	}

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin org node members write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM org_node_accounts WHERE node_id = ?", nodeID); err != nil {
		return fmt.Errorf("store: clear org node %d members: %w", nodeID, err)
	}
	now := unix(time.Now())
	for _, accountID := range unique {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO org_node_accounts(node_id, account_id, created_at) VALUES(?,?,?)",
			nodeID, accountID, now); err != nil {
			return fmt.Errorf("store: add account %d to org node %d: %w", accountID, nodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit org node members write: %w", err)
	}
	return nil
}

// SetAccountOrgNodes replaces one account's memberships with exactly nodeIDs.
func (db *DB) SetAccountOrgNodes(ctx context.Context, accountID int64, nodeIDs []int64) error {
	if _, err := db.GetAccount(ctx, accountID); err != nil {
		return err
	}
	unique, err := db.checkOrgNodesExist(ctx, nodeIDs)
	if err != nil {
		return err
	}

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin account org write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM org_node_accounts WHERE account_id = ?", accountID); err != nil {
		return fmt.Errorf("store: clear account %d org memberships: %w", accountID, err)
	}
	now := unix(time.Now())
	for _, nodeID := range unique {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO org_node_accounts(node_id, account_id, created_at) VALUES(?,?,?)",
			nodeID, accountID, now); err != nil {
			return fmt.Errorf("store: add org node %d to account %d: %w", nodeID, accountID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit account org write: %w", err)
	}
	return nil
}

// checkAccountsExist validates a set of account ids, de-duplicated and sorted.
func (db *DB) checkAccountsExist(ctx context.Context, ids []int64) ([]int64, error) {
	unique := uniquePositive(ids)
	for _, id := range unique {
		if _, err := db.GetAccount(ctx, id); err != nil {
			return nil, err
		}
	}
	return unique, nil
}

// checkOrgNodesExist validates a set of node ids, de-duplicated and sorted.
func (db *DB) checkOrgNodesExist(ctx context.Context, ids []int64) ([]int64, error) {
	unique := uniquePositive(ids)
	for _, id := range unique {
		if _, err := db.GetOrgNode(ctx, id); err != nil {
			return nil, err
		}
	}
	return unique, nil
}

// uniquePositive drops zero/negative ids and duplicates, keeping the result ordered. Sorting
// matters: it makes the stored membership set independent of the caller's argument order.
func uniquePositive(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, repeat := seen[id]; repeat {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// siblingNameConflict explains the uniqueness rule that was violated: names are unique among
// siblings, not globally, so the message has to say which scope was checked.
func siblingNameConflict(n *domain.OrgNode) string {
	scope := "at the root level"
	if parent := n.ParentIDValue(); parent != 0 {
		scope = fmt.Sprintf("under parent node %d", parent)
	}
	return fmt.Sprintf("another org node is already named %q %s", n.Name, scope)
}

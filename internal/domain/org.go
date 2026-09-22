package domain

import "time"

// OrgNode is one node of the organization structure: an independent tree that answers
// "which accounts belong together", separate from tags which answer "what may they use".
//
// The two meet at exactly one place: TagsJSON carries tag *names* that every account in this
// node's subtree inherits (see registry.Snapshot.InheritedTagNames). A node never holds
// grants or policy itself, so there is no second authorization implementation to keep in
// step with the tag one.
type OrgNode struct {
	ID int64
	// ParentID is nil for a root node. The structure is a forest, not a single tree:
	// several companies or divisions each with their own departments is the normal shape.
	ParentID *int64
	Name     string
	Note     string
	// TagsJSON is a JSON array of tag names, encoded exactly like Account.TagsJSON.
	TagsJSON  string
	SortOrder int
	CreatedAt time.Time
	UpdatedAt time.Time

	// FeishuDepartmentID is the open_department_id this node was created from or merged
	// with by the directory sync (M70). The first sync matches by sibling name and stamps
	// the id; every later sync then recognizes the node by id, so a department survives a
	// rename on either side. Empty means "not linked". Written only by the sync paths —
	// a console PATCH of the node must never clear it.
	FeishuDepartmentID string
	// FeishuSyncedAt records when that link was last written. Display/audit only.
	FeishuSyncedAt *time.Time
}

// ParentIDValue returns the node's parent as a plain value, with 0 meaning "no parent".
// It is the form the sibling-uniqueness index and the tree index work with.
func (n *OrgNode) ParentIDValue() int64 {
	if n == nil || n.ParentID == nil {
		return 0
	}
	return *n.ParentID
}

// OrgMembership is one account's attachment to one node. Both directions matter: the
// snapshot reads them by account (what does this account inherit), the console reads them by
// node (who is in this department).
type OrgMembership struct {
	NodeID    int64
	AccountID int64
}

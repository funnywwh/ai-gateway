package store

import (
	"context"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// orgFixture creates the account plus the tree the organization tests work on:
//
//	总部(1) ── 研发部(2) ── 平台组(4)
//	       └── 市场部(3)
func orgFixture(t *testing.T, db *DB) (map[string]int64, int64) {
	t.Helper()
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "org-acct"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	ids := map[string]int64{}
	parent := int64(0)
	for _, spec := range []struct {
		key, name string
		parentOf  string
	}{
		{"hq", "总部", ""},
		{"dev", "研发部", "hq"},
		{"sales", "市场部", "hq"},
		{"platform", "平台组", "dev"},
	} {
		node := &domain.OrgNode{Name: spec.name, SortOrder: 100}
		if spec.parentOf != "" {
			value := ids[spec.parentOf]
			node.ParentID = &value
		} else {
			parent = 0
			_ = parent
		}
		id, err := db.CreateOrgNode(ctx, node)
		if err != nil {
			t.Fatalf("create node %s: %v", spec.name, err)
		}
		ids[spec.key] = id
	}
	return ids, accountID
}

func TestOrgNodeRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	hq, err := db.GetOrgNode(ctx, ids["hq"])
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if hq.ParentID != nil {
		t.Fatalf("root parent = %v, want nil", hq.ParentID)
	}
	if hq.Name != "总部" {
		t.Fatalf("root name = %q", hq.Name)
	}

	dev, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if dev.ParentIDValue() != ids["hq"] {
		t.Fatalf("研发部 parent = %d, want %d", dev.ParentIDValue(), ids["hq"])
	}

	list, err := db.ListOrgNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("listed %d nodes, want 4", len(list))
	}
}

func TestOrgNodeTagsAndNoteRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id, err := db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "研发部", Note: "负责平台", TagsJSON: `["internal","vip"]`, SortOrder: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetOrgNode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "负责平台" || got.TagsJSON != `["internal","vip"]` || got.SortOrder != 5 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps were not stored: %+v", got)
	}
}

func TestOrgNodeSiblingNamesAreUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	// Same parent as 研发部: refused, and the error has to say which scope was checked,
	// because the same name is legal elsewhere.
	parent := ids["hq"]
	_, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "研发部", ParentID: &parent})
	if !domain.IsConflict(err) {
		t.Fatalf("duplicate sibling error = %v, want conflict", err)
	}
	if !strings.Contains(err.Error(), "under parent node") {
		t.Fatalf("conflict message does not name the scope: %v", err)
	}

	// A different parent may reuse the name: 分公司/研发部 is a normal organization.
	other, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "分公司"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "研发部", ParentID: &other}); err != nil {
		t.Fatalf("the same name under a different parent must be allowed: %v", err)
	}
}

func TestOrgNodeRootNamesAreUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "总部"}); err != nil {
		t.Fatal(err)
	}
	// Roots have parent_id NULL; without the COALESCE in the unique index SQLite would treat
	// two NULLs as distinct and let this through.
	_, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "总部"})
	if !domain.IsConflict(err) {
		t.Fatalf("duplicate root name error = %v, want conflict", err)
	}
}

func TestOrgNodeRenameCollisionIsAConflict(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	dev, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	dev.Name = "市场部" // a sibling's name
	if err := db.UpdateOrgNode(ctx, dev); !domain.IsConflict(err) {
		t.Fatalf("rename onto a sibling error = %v, want conflict", err)
	}

	// A rename that does not collide (and a move to the root) must go through. Renaming is
	// allowed precisely because nodes are addressed by id, not by name.
	dev.Name = "研发中心"
	dev.ParentID = nil
	if err := db.UpdateOrgNode(ctx, dev); err != nil {
		t.Fatalf("rename + move to root: %v", err)
	}
	after, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "研发中心" || after.ParentID != nil {
		t.Fatalf("update did not land: %+v", after)
	}
}

func TestOrgNodeUnknownIDsAreNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.GetOrgNode(ctx, 4242); !domain.IsNotFound(err) {
		t.Fatalf("get unknown = %v, want not found", err)
	}
	if err := db.UpdateOrgNode(ctx, &domain.OrgNode{ID: 4242, Name: "x"}); !domain.IsNotFound(err) {
		t.Fatalf("update unknown = %v, want not found", err)
	}
	if _, err := db.DeleteOrgNode(ctx, 4242, true); !domain.IsNotFound(err) {
		t.Fatalf("delete unknown = %v, want not found", err)
	}
	parent := int64(4242)
	if _, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "orphan", ParentID: &parent}); !domain.IsNotFound(err) {
		t.Fatalf("create under a missing parent = %v, want not found", err)
	}
}

func TestOrgNodeSelfParentIsRefused(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id, err := db.CreateOrgNode(ctx, &domain.OrgNode{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	self := id
	if err := db.UpdateOrgNode(ctx, &domain.OrgNode{ID: id, Name: "n", ParentID: &self}); !domain.IsInvalidRequest(err) {
		t.Fatalf("self parent = %v, want invalid request", err)
	}
}

func TestDeleteOrgNodeRefusesToRemoveASubtreeByAccident(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	_, err := db.DeleteOrgNode(ctx, ids["hq"], false)
	if !domain.IsConflict(err) {
		t.Fatalf("deleting a parent without cascade = %v, want conflict", err)
	}
	if !strings.Contains(err.Error(), "cascade=true") {
		t.Fatalf("the refusal does not say how to proceed: %v", err)
	}
	// Nothing was deleted: a refused delete must not be a partial one.
	list, err := db.ListOrgNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("a refused delete removed nodes: %d left, want 4", len(list))
	}
}

func TestDeleteOrgNodeLeafNeedsNoCascade(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	deleted, err := db.DeleteOrgNode(ctx, ids["platform"], false)
	if err != nil {
		t.Fatalf("deleting a leaf: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d nodes, want 1", deleted)
	}
	if _, err := db.GetOrgNode(ctx, ids["platform"]); !domain.IsNotFound(err) {
		t.Fatalf("the leaf is still there: %v", err)
	}
	// Its parent survives.
	if _, err := db.GetOrgNode(ctx, ids["dev"]); err != nil {
		t.Fatalf("deleting a leaf removed its parent: %v", err)
	}
}

func TestDeleteOrgNodeCascadeRemovesExactlyThatSubtree(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	deleted, err := db.DeleteOrgNode(ctx, ids["dev"], true)
	if err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d nodes, want 2 (研发部 + 平台组)", deleted)
	}
	for _, key := range []string{"hq", "sales"} {
		if _, err := db.GetOrgNode(ctx, ids[key]); err != nil {
			t.Fatalf("%s must survive a sibling subtree delete: %v", key, err)
		}
	}
	if _, err := db.GetOrgNode(ctx, ids["platform"]); !domain.IsNotFound(err) {
		t.Fatalf("the grandchild survived: %v", err)
	}
}

func TestDeleteOrgNodeRemovesMembershipsButKeepsAccounts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)

	if err := db.SetOrgNodeMembers(ctx, ids["dev"], []int64{accountID}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteOrgNode(ctx, ids["dev"], true); err != nil {
		t.Fatal(err)
	}
	members, err := db.ListOrgMemberships(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("memberships survived the node delete: %+v", members)
	}
	// The account itself is never touched by an organization change.
	if _, err := db.GetAccount(ctx, accountID); err != nil {
		t.Fatalf("deleting an org node must not touch accounts: %v", err)
	}
}

func TestSetAccountOrgNodesReplacesMembership(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)

	if err := db.SetAccountOrgNodes(ctx, accountID, []int64{ids["dev"], ids["sales"]}); err != nil {
		t.Fatal(err)
	}
	members, err := db.ListOrgMembershipsByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members[accountID]) != 2 {
		t.Fatalf("account sits in %v, want two nodes", members[accountID])
	}

	// Replacement, not addition: an account can be moved out of a department in one call.
	if err := db.SetAccountOrgNodes(ctx, accountID, []int64{ids["sales"]}); err != nil {
		t.Fatal(err)
	}
	members, err = db.ListOrgMembershipsByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members[accountID]) != 1 || members[accountID][0] != ids["sales"] {
		t.Fatalf("after replacement the account sits in %v, want only 市场部", members[accountID])
	}

	if err := db.SetAccountOrgNodes(ctx, accountID, nil); err != nil {
		t.Fatal(err)
	}
	members, err = db.ListOrgMembershipsByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members[accountID]) != 0 {
		t.Fatalf("an empty list must clear the memberships, got %v", members[accountID])
	}
}

func TestSetOrgNodeMembersIsIdempotentAndValidatesIDs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)
	second, err := db.UpsertAccount(ctx, &domain.Account{Name: "org-acct-2"})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SetOrgNodeMembers(ctx, ids["dev"], []int64{accountID, second, accountID}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListOrgNodeAccountIDs(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != accountID || got[1] != second {
		t.Fatalf("members = %v, want both accounts once, in id order", got)
	}

	// Same call again: no duplicate rows (the primary key would refuse them).
	if err := db.SetOrgNodeMembers(ctx, ids["dev"], []int64{accountID, second}); err != nil {
		t.Fatalf("replaying the same membership: %v", err)
	}
	got, _ = db.ListOrgNodeAccountIDs(ctx, ids["dev"])
	if len(got) != 2 {
		t.Fatalf("replay changed the membership: %v", got)
	}

	// An unknown account is a 404, not a foreign key failure that reads like a server bug.
	if err := db.SetOrgNodeMembers(ctx, ids["dev"], []int64{4242}); !domain.IsNotFound(err) {
		t.Fatalf("unknown account = %v, want not found", err)
	}
	// ...and the rejected call must not have cleared the existing members.
	got, _ = db.ListOrgNodeAccountIDs(ctx, ids["dev"])
	if len(got) != 2 {
		t.Fatalf("a rejected write still modified the membership: %v", got)
	}
	if err := db.SetAccountOrgNodes(ctx, accountID, []int64{4242}); !domain.IsNotFound(err) {
		t.Fatalf("unknown node = %v, want not found", err)
	}
	if err := db.SetOrgNodeMembers(ctx, 4242, nil); !domain.IsNotFound(err) {
		t.Fatalf("unknown node for a member write = %v, want not found", err)
	}
}

func TestOrgMembershipIsIndependentPerSide(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)

	// The two write directions reach the same table: a membership set from the account side
	// is visible from the node side.
	if err := db.SetAccountOrgNodes(ctx, accountID, []int64{ids["platform"]}); err != nil {
		t.Fatal(err)
	}
	byNode, err := db.ListOrgNodeAccountIDs(ctx, ids["platform"])
	if err != nil {
		t.Fatal(err)
	}
	if len(byNode) != 1 || byNode[0] != accountID {
		t.Fatalf("node-side read = %v, want the account", byNode)
	}
}

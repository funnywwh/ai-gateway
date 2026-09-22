package store

import (
	"context"
	"errors"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// The org-node side of the directory sync (M70): the Feishu department link is what lets a
// later sync recognize a node by id after a rename on either side.

func TestSetOrgNodeFeishuDepartmentRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	node, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if node.FeishuDepartmentID != "" || node.FeishuSyncedAt != nil {
		t.Fatalf("a fresh node carries a Feishu link: %+v", node)
	}

	if err := db.SetOrgNodeFeishuDepartment(ctx, ids["dev"], "od_dev"); err != nil {
		t.Fatal(err)
	}
	linked, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if linked.FeishuDepartmentID != "od_dev" || linked.FeishuSyncedAt == nil {
		t.Fatalf("link not persisted: %+v", linked)
	}

	// A rename must not disturb the link — it is written by a column-scoped UPDATE, and
	// this is the write the console rename actually performs.
	linked.Name = "研发中心"
	if err := db.UpdateOrgNode(ctx, linked); err != nil {
		t.Fatal(err)
	}
	renamed, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "研发中心" || renamed.FeishuDepartmentID != "od_dev" || renamed.FeishuSyncedAt == nil {
		t.Fatalf("a rename disturbed the Feishu link: %+v", renamed)
	}

	// Unlinking clears both fields and is idempotent.
	if err := db.SetOrgNodeFeishuDepartment(ctx, ids["dev"], ""); err != nil {
		t.Fatal(err)
	}
	cleared, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if cleared.FeishuDepartmentID != "" || cleared.FeishuSyncedAt != nil {
		t.Fatalf("unlink left fields behind: %+v", cleared)
	}
	if err := db.SetOrgNodeFeishuDepartment(ctx, ids["dev"], ""); err != nil {
		t.Fatalf("second unlink = %v", err)
	}
	if err := db.SetOrgNodeFeishuDepartment(ctx, 999999, "od_x"); err == nil {
		t.Fatal("linking a non-existent node was accepted")
	}
}

// One Feishu department links to at most one node — otherwise two syncs would keep
// fighting over the same department, and the id-keyed lookup could not be a map lookup.
func TestSetOrgNodeFeishuDepartmentIsUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	if err := db.SetOrgNodeFeishuDepartment(ctx, ids["dev"], "od_shared"); err != nil {
		t.Fatal(err)
	}
	err := db.SetOrgNodeFeishuDepartment(ctx, ids["sales"], "od_shared")
	if err == nil {
		t.Fatal("the same department was linked to a second node")
	}
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("conflict error = %v, want a 409 API error", err)
	}
	// Re-linking the same node is a refresh, not a conflict.
	if err := db.SetOrgNodeFeishuDepartment(ctx, ids["dev"], "od_shared"); err != nil {
		t.Fatalf("re-linking the same node = %v", err)
	}
}

// CreateOrgNode persists the link when the sync creates a node that is born linked.
func TestCreateOrgNodeCarriesFeishuLink(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	syncedAt := timeFromUnix(1769000000)
	id, err := db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "财务部", SortOrder: 100, FeishuDepartmentID: "od_fin", FeishuSyncedAt: &syncedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := db.GetOrgNode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if node.FeishuDepartmentID != "od_fin" || node.FeishuSyncedAt == nil || !node.FeishuSyncedAt.Equal(syncedAt) {
		t.Fatalf("created node lost the Feishu link: %+v", node)
	}
}

// AddAccountOrgNodes is additive: memberships the console set before the sync must stay,
// duplicates must be no-ops, and unknown ids must be 404s.
func TestAddAccountOrgNodes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)

	// Pre-existing membership on a node the sync will not mention.
	if err := db.SetAccountOrgNodes(ctx, accountID, []int64{ids["hq"]}); err != nil {
		t.Fatal(err)
	}

	if err := db.AddAccountOrgNodes(ctx, accountID, []int64{ids["dev"], ids["dev"], ids["sales"]}); err != nil {
		t.Fatal(err)
	}
	memberships, err := db.ListOrgMembershipsByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := memberships[accountID]
	if len(got) != 3 {
		t.Fatalf("memberships = %v, want hq+dev+sales", got)
	}
	// Ordered comparison against the sorted expectation: hq < dev < sales by node id here
	// only by luck of creation order, so compare as a set.
	want := map[int64]bool{ids["hq"]: true, ids["dev"]: true, ids["sales"]: true}
	for _, nodeID := range got {
		if !want[nodeID] {
			t.Fatalf("unexpected membership %d in %v", nodeID, got)
		}
	}

	// Re-running the same call must change nothing (the sync is idempotent).
	if err := db.AddAccountOrgNodes(ctx, accountID, []int64{ids["dev"]}); err != nil {
		t.Fatal(err)
	}
	memberships, err = db.ListOrgMembershipsByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships[accountID]) != 3 {
		t.Fatalf("a repeated add changed the memberships: %v", memberships[accountID])
	}

	// A different account on the same nodes must not leak into the first account.
	other, err := db.UpsertAccount(ctx, &domain.Account{Name: "sync-other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddAccountOrgNodes(ctx, other, []int64{ids["platform"]}); err != nil {
		t.Fatal(err)
	}
	memberships, err = db.ListOrgMembershipsByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships[accountID]) != 3 || len(memberships[other]) != 1 {
		t.Fatalf("memberships crossed accounts: %v", memberships)
	}

	if err := db.AddAccountOrgNodes(ctx, 999999, []int64{ids["dev"]}); err == nil {
		t.Fatal("adding memberships for a non-existent account was accepted")
	}
	if err := db.AddAccountOrgNodes(ctx, accountID, []int64{999999}); err == nil {
		t.Fatal("adding a non-existent node was accepted")
	}
}

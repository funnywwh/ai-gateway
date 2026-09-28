package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/funnywwh/ai-gateway/internal/domain"
)

// The company-scoped half of the Feishu sync (M92): the person mappings of companies that are
// not the identity application, and the startup adoption of rows written before the company
// dimension existed.

func TestFeishuPersonLinksUpsertAndDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)
	_ = ids

	link := domain.FeishuPersonLink{
		AppID: "cli_bbb", OpenID: "ou_1", UnionID: "on_1", Name: "张三",
		AccountID: accountID, BoundBy: "sync",
	}
	if err := db.UpsertFeishuPersonLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	links, err := db.ListFeishuPersonLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].AppID != "cli_bbb" || links[0].AccountID != accountID {
		t.Fatalf("links = %+v", links)
	}
	if links[0].BoundAt == nil || links[0].BoundBy != "sync" {
		t.Fatalf("link lost its audit fields: %+v", links[0])
	}

	// A re-sync of the same person updates the display facts instead of failing.
	link.Name = "张三（改名）"
	link.UnionID = "on_1b"
	if err := db.UpsertFeishuPersonLink(ctx, link); err != nil {
		t.Fatalf("re-upsert = %v", err)
	}
	links, _ = db.ListFeishuPersonLinks(ctx)
	if len(links) != 1 || links[0].Name != "张三（改名）" || links[0].UnionID != "on_1b" {
		t.Fatalf("re-upsert did not refresh: %+v", links)
	}

	// The same open id under another company is that company's own person.
	if err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
		AppID: "cli_ccc", OpenID: "ou_1", Name: "张三", AccountID: accountID,
	}); err != nil {
		t.Fatalf("another company's same open id = %v", err)
	}
	if links, _ = db.ListFeishuPersonLinks(ctx); len(links) != 2 {
		t.Fatalf("links = %+v, want one per company", links)
	}

	// Deleting one mapping is idempotent and does not touch the other company's.
	changed, err := db.DeleteFeishuPersonLink(ctx, "cli_bbb", "ou_1")
	if err != nil || !changed {
		t.Fatalf("delete = %v %v", changed, err)
	}
	changed, err = db.DeleteFeishuPersonLink(ctx, "cli_bbb", "ou_1")
	if err != nil || changed {
		t.Fatalf("second delete = %v %v, want a no-op", changed, err)
	}
	remaining, _ := db.ListFeishuPersonLinks(ctx)
	if len(remaining) != 1 || remaining[0].AppID != "cli_ccc" {
		t.Fatalf("remaining = %+v", remaining)
	}
}

// One company's directory never puts two of its people on one account; across companies the same
// account may appear twice (one human in two customers' directories).
func TestFeishuPersonLinksRefuseTwoPeoplePerAccountPerCompany(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, accountID := orgFixture(t, db)

	if err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
		AppID: "cli_bbb", OpenID: "ou_1", Name: "张三", AccountID: accountID,
	}); err != nil {
		t.Fatal(err)
	}
	err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
		AppID: "cli_bbb", OpenID: "ou_2", Name: "李四", AccountID: accountID,
	})
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("second person of one company = %v, want a 409", err)
	}
	// The same account under another company is fine: that is the same human in two directories.
	if err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
		AppID: "cli_ccc", OpenID: "ou_2", Name: "李四", AccountID: accountID,
	}); err != nil {
		t.Fatalf("another company's mapping for the same account = %v", err)
	}
}

func TestFeishuPersonLinksRequireRealValues(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, accountID := orgFixture(t, db)

	cases := []domain.FeishuPersonLink{
		{OpenID: "ou_1", AccountID: accountID},
		{AppID: "cli_bbb", AccountID: accountID},
		{AppID: "cli_bbb", OpenID: "ou_1"},
		{AppID: "cli_bbb", OpenID: "ou_1", AccountID: 999999},
	}
	for i, link := range cases {
		if err := db.UpsertFeishuPersonLink(ctx, link); err == nil {
			t.Fatalf("case %d was accepted: %+v", i, link)
		}
	}
}

// Deleting an account removes its mappings: the mapping is about the account, so it cannot
// outlive it.
func TestFeishuPersonLinksCascadeWithTheAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, accountID := orgFixture(t, db)
	if err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
		AppID: "cli_bbb", OpenID: "ou_1", Name: "张三", AccountID: accountID,
	}); err != nil {
		t.Fatal(err)
	}
	// Accounts are never deleted through the management API, but the database must not keep a
	// mapping that points at nothing: the foreign key cascades.
	if _, err := db.write.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", accountID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	links, err := db.ListFeishuPersonLinks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("links survived their account: %+v", links)
	}
}

// Purging one company's mappings (a client offboarding) touches nothing else.
func TestDeleteFeishuPersonLinksByApp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, accountID := orgFixture(t, db)
	other, err := db.UpsertAccount(ctx, &domain.Account{Name: "purge-other"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		appID, openID string
		accountID     int64
	}{
		{"cli_bbb", "ou_a", accountID},
		{"cli_bbb", "ou_b", other},
		{"cli_ccc", "ou_c", accountID},
	} {
		if err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
			AppID: row.appID, OpenID: row.openID, Name: "人", AccountID: row.accountID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := db.DeleteFeishuPersonLinksByApp(ctx, "cli_bbb")
	if err != nil || deleted != 2 {
		t.Fatalf("purge = %d %v, want 2", deleted, err)
	}
	remaining, _ := db.ListFeishuPersonLinks(ctx)
	if len(remaining) != 1 || remaining[0].AppID != "cli_ccc" {
		t.Fatalf("remaining = %+v", remaining)
	}
	// Nodes and memberships are a separate decision: nothing about them changed.
	if _, err := db.GetOrgNode(ctx, ids["dev"]); err != nil {
		t.Fatalf("the purge touched org nodes: %v", err)
	}
	if deleted, err := db.DeleteFeishuPersonLinksByApp(ctx, ""); err == nil || deleted != 0 {
		t.Fatalf("empty app id = %d %v, want a refusal", deleted, err)
	}
}

// AdoptLegacyFeishuScope stamps the identity application on department links written before M92
// and is idempotent — an upgrade runs it on every start.
func TestAdoptLegacyFeishuScope(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ids, _ := orgFixture(t, db)

	// Legacy shape: department links with no company at all, which the M92 migration allows.
	if _, err := db.write.ExecContext(ctx, `
UPDATE org_nodes SET feishu_department_id = ?, feishu_synced_at = ? WHERE id = ?`,
		"od_legacy_dev", unix(time.Now()), ids["dev"]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.write.ExecContext(ctx, `
UPDATE org_nodes SET feishu_department_id = ?, feishu_synced_at = ? WHERE id = ?`,
		"od_legacy_platform", unix(time.Now()), ids["platform"]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.write.ExecContext(ctx,
		"UPDATE org_nodes SET feishu_department_id = 'od_root' WHERE id = ?", ids["hq"]); err != nil {
		t.Fatal(err)
	}

	updated, err := db.AdoptLegacyFeishuScope(ctx, "cli_aaa")
	if err != nil || updated != 3 {
		t.Fatalf("adoption = %d %v, want 3 nodes stamped", updated, err)
	}
	for _, key := range []string{"dev", "platform", "hq"} {
		node, err := db.GetOrgNode(ctx, ids[key])
		if err != nil {
			t.Fatal(err)
		}
		if node.FeishuAppID != "cli_aaa" {
			t.Fatalf("node %s not adopted: %+v", key, node)
		}
	}
	// A node without a department link is never stamped: it is a hand-made node, not a company's.
	sales, err := db.GetOrgNode(ctx, ids["sales"])
	if err != nil {
		t.Fatal(err)
	}
	if sales.FeishuAppID != "" {
		t.Fatalf("an unlinked node was adopted: %+v", sales)
	}
	// Second run: nothing left to do.
	updated, err = db.AdoptLegacyFeishuScope(ctx, "cli_aaa")
	if err != nil || updated != 0 {
		t.Fatalf("second adoption = %d %v, want a no-op", updated, err)
	}
	// An already-stamped link is never rewritten by a later adoption (a different app id would
	// otherwise silently steal another company's nodes).
	node, err := db.GetOrgNode(ctx, ids["dev"])
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetOrgNodeFeishuDepartment(ctx, node.ID, "cli_bbb", "od_legacy_dev"); err != nil {
		t.Fatal(err)
	}
	if updated, err := db.AdoptLegacyFeishuScope(ctx, "cli_aaa"); err != nil || updated != 0 {
		t.Fatalf("adoption rewrote a stamped link: %d %v", updated, err)
	}
}

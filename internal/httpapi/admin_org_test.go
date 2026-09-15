package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// orgTestSetup logs in as admin and creates the tree these tests work on:
//
//	总部(1) ── 研发部(2) ── 平台组(3)
//	分公司(4)
//
// It returns the node ids by name plus the session cookie.
func orgTestSetup(t *testing.T, f *adminFixture) (map[string]int64, string) {
	t.Helper()
	cookie := f.login(t, adminUser, adminPassword)
	ids := map[string]int64{}
	mk := func(name string, parentID int64) int64 {
		body := fmt.Sprintf(`{"name":%q`, name)
		if parentID != 0 {
			body += fmt.Sprintf(`,"parent_id":%d`, parentID)
		}
		body += "}"
		resp := f.call(t, http.MethodPost, "/admin/api/v1/org/nodes", body, cookie)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create node %s: status %d", name, resp.StatusCode)
		}
		payload := decodeJSONBody(t, resp)
		id := int64(payload["id"].(float64))
		ids[name] = id
		return id
	}
	hq := mk("总部", 0)
	mk("研发部", hq)
	mk("平台组", ids["研发部"])
	mk("分公司", 0)
	return ids, cookie
}

func TestOrgNodeCRUDThroughTheAPI(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)

	// The list is flat with the hierarchy fields the console needs, in display order.
	resp := f.call(t, http.MethodGet, "/admin/api/v1/org/nodes", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	payload := decodeJSONBody(t, resp)
	rows := payload["data"].([]any)
	if len(rows) != 4 {
		t.Fatalf("listed %d nodes, want 4", len(rows))
	}
	byName := map[string]map[string]any{}
	order := []string{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		byName[row["name"].(string)] = row
		order = append(order, row["name"].(string))
	}
	// Roots come first in sibling order (sort_order, then name), each parent before its
	// children: a console can render this by indenting runs of increasing depth.
	if want := []string{"分公司", "总部", "研发部", "平台组"}; !slices.Equal(order, want) {
		t.Fatalf("display order = %v, want %v", order, want)
	}
	if root := byName["总部"]; root["depth"].(float64) != 0 || root["path"] != "总部" || root["parent_id"] != nil {
		t.Fatalf("root row = %v", root)
	}
	if leaf := byName["平台组"]; leaf["path"] != "总部/研发部/平台组" || leaf["depth"].(float64) != 2 {
		t.Fatalf("grandchild row = %v", leaf)
	}

	// Rename + attach a tag. The tag must exist first: an unknown name is refused.
	tagResp := f.call(t, http.MethodPost, "/admin/api/v1/tags",
		`{"name":"dev-tag","grants":{"models":["gpt-*"]}}`, cookie)
	if tagResp.StatusCode != http.StatusOK {
		t.Fatalf("create tag status = %d", tagResp.StatusCode)
	}
	tagResp.Body.Close()

	devID := ids["研发部"]
	patch := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", devID),
		`{"name":"研发中心","tags":["dev-tag"],"note":"平台"}`, cookie)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d", patch.StatusCode)
	}
	patched := decodeJSONBody(t, patch)
	if patched["name"] != "研发中心" {
		t.Fatalf("rename did not land: %v", patched)
	}
	if tags, _ := patched["tags"].([]any); len(tags) != 1 || tags[0] != "dev-tag" {
		t.Fatalf("tags = %v, want [dev-tag]", patched["tags"])
	}
	// The path of a descendant follows the rename.
	rows = decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/org/nodes", "", cookie))["data"].([]any)
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["name"] == "平台组" && row["path"] != "总部/研发中心/平台组" {
			t.Fatalf("descendant path did not follow the rename: %v", row["path"])
		}
	}
}

func TestOrgNodeUnknownTagIsRefused(t *testing.T) {
	f := newAdminFixture(t)
	_, cookie := orgTestSetup(t, f)

	// A name that does not exist would be dropped at resolution time, and an account left
	// with no grants at all falls back to default_grant. Refusing it is the safe answer.
	resp := f.call(t, http.MethodPost, "/admin/api/v1/org/nodes",
		`{"name":"ghost-dept","tags":["no-such-tag"]}`, cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown tag status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestOrgNodeSiblingNameConflict(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)

	dup := f.call(t, http.MethodPost, "/admin/api/v1/org/nodes",
		fmt.Sprintf(`{"name":"研发部","parent_id":%d}`, ids["总部"]), cookie)
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate sibling status = %d, want 409", dup.StatusCode)
	}
	dup.Body.Close()

	// The same name under another parent is a normal organization, not an error.
	ok := f.call(t, http.MethodPost, "/admin/api/v1/org/nodes",
		fmt.Sprintf(`{"name":"研发部","parent_id":%d}`, ids["分公司"]), cookie)
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("same name under another parent status = %d, want 201", ok.StatusCode)
	}
	ok.Body.Close()
}

func TestOrgNodeMoveRefusesCyclesAndDepthOverflow(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)

	// Moving 总部 under its own grandchild would detach the subtree from every root.
	cycle := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["总部"]),
		fmt.Sprintf(`{"parent_id":%d}`, ids["平台组"]), cookie)
	if cycle.StatusCode != http.StatusBadRequest {
		t.Fatalf("cycle status = %d, want 400", cycle.StatusCode)
	}
	cycle.Body.Close()

	// Moving a node onto itself is the degenerate case of the same mistake.
	self := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["研发部"]),
		fmt.Sprintf(`{"parent_id":%d}`, ids["研发部"]), cookie)
	if self.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-parent status = %d, want 400", self.StatusCode)
	}
	self.Body.Close()

	// A chain that would exceed the depth limit is refused too: build one long chain and try
	// to hang the existing three-level subtree under its end.
	parent := ids["分公司"]
	for i := 0; i < 14; i++ {
		resp := f.call(t, http.MethodPost, "/admin/api/v1/org/nodes",
			fmt.Sprintf(`{"name":"level-%02d","parent_id":%d}`, i, parent), cookie)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("deep chain node %d status = %d", i, resp.StatusCode)
		}
		parent = int64(decodeJSONBody(t, resp)["id"].(float64))
	}
	deep := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["总部"]),
		fmt.Sprintf(`{"parent_id":%d}`, parent), cookie)
	if deep.StatusCode != http.StatusBadRequest {
		t.Fatalf("depth overflow status = %d, want 400", deep.StatusCode)
	}
	deep.Body.Close()

	// A legal move still works: 分公司 under 总部.
	legal := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["分公司"]),
		fmt.Sprintf(`{"parent_id":%d}`, ids["总部"]), cookie)
	if legal.StatusCode != http.StatusOK {
		t.Fatalf("legal move status = %d", legal.StatusCode)
	}
	legal.Body.Close()
}

func TestOrgNodeDeleteRequiresCascadeForASubtree(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)

	refused := f.call(t, http.MethodDelete, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["总部"]), "", cookie)
	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("subtree delete without cascade = %d, want 409", refused.StatusCode)
	}
	refused.Body.Close()

	deleted := f.call(t, http.MethodDelete,
		fmt.Sprintf("/admin/api/v1/org/nodes/%d?cascade=true", ids["总部"]), "", cookie)
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("cascade delete status = %d", deleted.StatusCode)
	}
	payload := decodeJSONBody(t, deleted)
	if payload["nodes_deleted"].(float64) != 3 {
		t.Fatalf("nodes_deleted = %v, want 3", payload["nodes_deleted"])
	}
	// The unrelated root survives.
	rows := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/org/nodes", "", cookie))["data"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "分公司" {
		t.Fatalf("after the cascade delete the tree is %v", rows)
	}
}

func TestOrgMembershipFromBothDirections(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)
	ctx := context.Background()

	second, err := f.db.UpsertAccount(ctx, &domain.Account{Name: "acme-2", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	first := int64(1) // the fixture seeds "acme" first

	// Node side: set the whole member list.
	resp := f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["平台组"]),
		fmt.Sprintf(`{"account_ids":[%d]}`, first), cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set members status = %d", resp.StatusCode)
	}
	payload := decodeJSONBody(t, resp)
	if payload["count"].(float64) != 1 {
		t.Fatalf("count = %v, want 1", payload["count"])
	}

	// The account side sees it, and the account list carries the organization refs.
	accounts := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/accounts", "", cookie))["data"].([]any)
	found := false
	for _, raw := range accounts {
		row := raw.(map[string]any)
		if int64(row["id"].(float64)) != first {
			continue
		}
		found = true
		refs := row["org_nodes"].([]any)
		if len(refs) != 1 {
			t.Fatalf("account org_nodes = %v, want one", refs)
		}
		ref := refs[0].(map[string]any)
		if ref["path"] != "总部/研发部/平台组" {
			t.Fatalf("org ref path = %v", ref["path"])
		}
	}
	if !found {
		t.Fatal("the account was not listed")
	}

	// Account side: replace the memberships, which is how an account moves between teams.
	patch := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/accounts/%d", first),
		fmt.Sprintf(`{"org_node_ids":[%d]}`, ids["分公司"]), cookie)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("account patch status = %d", patch.StatusCode)
	}
	patched := decodeJSONBody(t, patch)
	if ids2, _ := patched["org_node_ids"].([]any); len(ids2) != 1 || int64(ids2[0].(float64)) != ids["分公司"] {
		t.Fatalf("org_node_ids after the patch = %v", patched["org_node_ids"])
	}

	// A second account can sit in the same nodes as the first: membership is many-to-many.
	multi := f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["分公司"]),
		fmt.Sprintf(`{"account_ids":[%d,%d]}`, first, second), cookie)
	if multi.StatusCode != http.StatusOK {
		t.Fatalf("multi-member status = %d", multi.StatusCode)
	}
	if decodeJSONBody(t, multi)["count"].(float64) != 2 {
		t.Fatal("both accounts should be members of 分公司")
	}
}

func TestOrgMembershipRejectsUnknownIDs(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)

	resp := f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["平台组"]),
		`{"account_ids":[4242]}`, cookie)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// An unknown node on the account side is a 404 as well.
	patch := f.call(t, http.MethodPatch, "/admin/api/v1/accounts/1", `{"org_node_ids":[4242]}`, cookie)
	if patch.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown node status = %d, want 404", patch.StatusCode)
	}
	patch.Body.Close()

	// The body field is required: an empty body would otherwise mean "clear everything".
	missing := f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["平台组"]),
		`{}`, cookie)
	if missing.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing account_ids status = %d, want 400", missing.StatusCode)
	}
	missing.Body.Close()
}

// TestAccountOrgFilterCoversTheSubtree is the console's "show me this division" query.
func TestAccountOrgFilterCoversTheSubtree(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)
	ctx := context.Background()

	// acme (id 1) sits at the leaf; a second account sits at the root.
	root, err := f.db.UpsertAccount(ctx, &domain.Account{Name: "root-acct", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetAccountOrgNodes(ctx, 1, []int64{ids["平台组"]}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetAccountOrgNodes(ctx, root, []int64{ids["总部"]}); err != nil {
		t.Fatal(err)
	}

	names := func(query string) []string {
		payload := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/accounts"+query, "", cookie))
		out := []string{}
		for _, raw := range payload["data"].([]any) {
			out = append(out, raw.(map[string]any)["name"].(string))
		}
		return out
	}

	// Descendants by default: the root's subtree contains both accounts.
	got := names(fmt.Sprintf("?org_node_id=%d", ids["总部"]))
	if len(got) != 2 {
		t.Fatalf("subtree filter returned %v, want both accounts", got)
	}
	// Exactly this node: only the account attached to it.
	got = names(fmt.Sprintf("?org_node_id=%d&include_descendants=false", ids["总部"]))
	if len(got) != 1 || got[0] != "root-acct" {
		t.Fatalf("direct filter returned %v, want only root-acct", got)
	}
	// A leaf: only the account under it.
	got = names(fmt.Sprintf("?org_node_id=%d", ids["平台组"]))
	if len(got) != 1 || got[0] != "acme" {
		t.Fatalf("leaf filter returned %v, want only acme", got)
	}
	// No filter: everything.
	if got = names(""); len(got) != 2 {
		t.Fatalf("unfiltered list returned %v", got)
	}
}

// TestOrganizationChangeIsVisibleToTheKeyResolution is the end-to-end authorization claim: an
// account placed in a node that carries a tag can use what that tag grants, and loses it when
// it is moved out. It runs against the same registry the data plane reads.
func TestOrganizationChangeIsVisibleToTheKeyResolution(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)

	tag := f.call(t, http.MethodPost, "/admin/api/v1/tags",
		`{"name":"org-grant","grants":{"providers":["*"],"models":["org-only-model"]}}`, cookie)
	if tag.StatusCode != http.StatusOK {
		t.Fatalf("tag status = %d", tag.StatusCode)
	}
	tag.Body.Close()

	attach := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["平台组"]),
		`{"tags":["org-grant"]}`, cookie)
	if attach.StatusCode != http.StatusOK {
		t.Fatalf("attach tag status = %d", attach.StatusCode)
	}
	attach.Body.Close()

	// The key of account 1, with no tags and no grants of its own.
	key := &domain.APIKey{ID: 1, AccountID: 1, Status: "active"}
	effective := func() []string {
		snap := f.reg.Snapshot()
		return effectiveTagNames(snap, key)
	}
	if got := effective(); len(got) != 0 {
		t.Fatalf("before joining, the key resolved %v", got)
	}

	join := f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["平台组"]),
		`{"account_ids":[1]}`, cookie)
	if join.StatusCode != http.StatusOK {
		t.Fatalf("join status = %d", join.StatusCode)
	}
	join.Body.Close()

	// The admin write reloads the registry, so the new snapshot carries the inheritance.
	if got := effective(); len(got) != 1 || got[0] != "org-grant" {
		t.Fatalf("after joining, the key resolved %v, want [org-grant]", got)
	}

	// Leaving the organization removes it again.
	leave := f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["平台组"]),
		`{"account_ids":[]}`, cookie)
	if leave.StatusCode != http.StatusOK {
		t.Fatalf("leave status = %d", leave.StatusCode)
	}
	leave.Body.Close()
	if got := effective(); len(got) != 0 {
		t.Fatalf("after leaving, the key still resolved %v", got)
	}
}

func TestOrgWritesRequireAdminRole(t *testing.T) {
	f := newAdminFixture(t)
	ids, _ := orgTestSetup(t, f)
	reader := f.login(t, "reader", adminPassword)

	// A viewer may read the tree...
	if resp := f.call(t, http.MethodGet, "/admin/api/v1/org/nodes", "", reader); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("viewer list status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// ...but not change it.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/admin/api/v1/org/nodes", `{"name":"nope"}`},
		{http.MethodPatch, fmt.Sprintf("/admin/api/v1/org/nodes/%d", ids["总部"]), `{"note":"x"}`},
		{http.MethodDelete, fmt.Sprintf("/admin/api/v1/org/nodes/%d?cascade=true", ids["总部"]), ""},
		{http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["总部"]), `{"account_ids":[]}`},
	} {
		resp := f.call(t, tc.method, tc.path, tc.body, reader)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestOrgNodeAccountCountIsAlwaysAccurate pins the field the console's tree renders. It used to
// be computed from the inline member list, so it read 0 for every node unless the caller also
// passed include_accounts=true — a count that is present but wrong is worse than an absent one,
// because the tree shows "0 个账号" for a department that has members.
func TestOrgNodeAccountCountIsAlwaysAccurate(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)
	ctx := context.Background()
	if err := f.db.SetAccountOrgNodes(ctx, 1, []int64{ids["平台组"]}); err != nil {
		t.Fatal(err)
	}
	second, err := f.db.UpsertAccount(ctx, &domain.Account{Name: "acme-2", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetAccountOrgNodes(ctx, second, []int64{ids["平台组"]}); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"", "?include_accounts=true"} {
		payload := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/org/nodes"+query, "", cookie))
		counts := map[string]float64{}
		for _, raw := range payload["data"].([]any) {
			row := raw.(map[string]any)
			counts[row["name"].(string)] = row["account_count"].(float64)
		}
		if counts["平台组"] != 2 {
			t.Errorf("with query %q, 平台组 account_count = %v, want 2", query, counts["平台组"])
		}
		if counts["总部"] != 0 {
			t.Errorf("with query %q, 总部 account_count = %v, want 0", query, counts["总部"])
		}
	}
}

// TestOrgNodeMembersArePagedSeparately covers the read path the console uses for the member
// picker, which is independent of the inline list.
func TestOrgNodeMembersArePagedSeparately(t *testing.T) {
	f := newAdminFixture(t)
	ids, cookie := orgTestSetup(t, f)
	if err := f.db.SetAccountOrgNodes(context.Background(), 1, []int64{ids["平台组"]}); err != nil {
		t.Fatal(err)
	}
	payload := decodeJSONBody(t,
		f.call(t, http.MethodGet, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", ids["平台组"]), "", cookie))
	rows := payload["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("member list = %v, want one account", rows)
	}
	// The name comes from the account table, so the picker does not have to look it up itself.
	if row := rows[0].(map[string]any); row["name"] != "acme" {
		t.Fatalf("member row = %v, want the account name", row)
	}
	if resp := f.call(t, http.MethodGet, "/admin/api/v1/org/nodes/4242/accounts", "", cookie); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("unknown node member list = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// TestOrgEndpointsAnswerUnsupportedWithoutThePort keeps the contract that a deployment
// without an organization port behaves exactly as it did before the feature existed.
func TestOrgEndpointsAnswerUnsupportedWithoutThePort(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	f.api.deps.Org = nil

	resp := f.call(t, http.MethodGet, "/admin/api/v1/org/nodes", "", cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("org list without the port = %d, want 400", resp.StatusCode)
	}
	if payload := decodeJSONBody(t, resp); payload["error"].(map[string]any)["code"] != "unsupported_parameter" {
		t.Fatalf("unwired port error = %v, want unsupported_parameter", payload["error"])
	}

	// The account list still works and simply reports no organizations.
	accounts := f.call(t, http.MethodGet, "/admin/api/v1/accounts", "", cookie)
	if accounts.StatusCode != http.StatusOK {
		t.Fatalf("accounts without the org port = %d, want 200", accounts.StatusCode)
	}
	payload := decodeJSONBody(t, accounts)
	row := payload["data"].([]any)[0].(map[string]any)
	if ids, _ := row["org_node_ids"].([]any); len(ids) != 0 {
		t.Fatalf("org_node_ids = %v, want an empty array", row["org_node_ids"])
	}
	if _, present := row["org_nodes"]; !present {
		t.Fatal("org_nodes must still be present (as an empty array) so the contract is stable")
	}
}

// effectiveTagNames is the resolution the data plane performs for one key, read through the
// same registry the request path uses.
func effectiveTagNames(snap *registry.Snapshot, key *domain.APIKey) []string {
	return registry.ResolveTagNames(snap, key)
}

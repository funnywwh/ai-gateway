package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/feishu"
)

// Renaming a company from the console (M94).
//
// The rule under test: every company can be renamed, whichever side owns its other fields. A company
// whose name comes from the configuration is addressed by its app id, and only `name` is accepted
// there — its credentials, root node, note and enabled flag stay in the file.

// renameCompany PATCHes by app id (the M94 handle) and returns the decoded answer.
func renameCompany(t *testing.T, f *orgFeishuFixture, cookie, appID, body string) (int, map[string]any) {
	t.Helper()
	return f.callJSON(t, http.MethodPatch,
		"/admin/api/v1/org/feishu/companies/"+appID, body, cookie)
}

// registerIdentityCompanyRow renames the identity application and returns the list row for it.
func identityCompanyRow(t *testing.T, f *orgFeishuFixture, cookie string) map[string]any {
	t.Helper()
	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/companies", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("list status=%d payload=%v", status, payload)
	}
	for _, raw := range payload["data"].([]any) {
		row := raw.(map[string]any)
		if row["identity"] == true {
			return row
		}
	}
	t.Fatal("the identity company is missing from the list")
	return nil
}

func TestFeishuRenameIdentityCompany(t *testing.T) {
	f := newOrgFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	before := identityCompanyRow(t, f, cookie)
	if before["name"] != "本公司" || before["name_source"] != "config" {
		t.Fatalf("identity row = %v, want 本公司 from the configuration", before)
	}

	status, payload := renameCompany(t, f, cookie, f.identityAppID(), `{"name":"智天成"}`)
	if status != http.StatusOK {
		t.Fatalf("rename status=%d payload=%v", status, payload)
	}
	if payload["app_id"] != f.identityAppID() || payload["company"].(map[string]any)["name"] != "智天成" {
		t.Fatalf("rename payload = %v", payload)
	}
	if payload["company"].(map[string]any)["name_source"] != "override" {
		t.Fatalf("name_source = %v, want override", payload["company"])
	}
	// No company node exists yet, so there was nothing to rename.
	if payload["node_renamed"] != nil {
		t.Fatalf("node_renamed = %v, want nil (the company node does not exist yet)", payload["node_renamed"])
	}

	// The list and the resolution both use the new name.
	after := identityCompanyRow(t, f, cookie)
	if after["name"] != "智天成" || after["name_source"] != "override" {
		t.Fatalf("identity row after rename = %v", after)
	}
	if status, _ := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", `{"company":"智天成"}`, cookie); status != http.StatusOK {
		t.Fatalf("sync by the new name = %d, want 200", status)
	}

	// Clearing the field goes back to the configured name.
	status, payload = renameCompany(t, f, cookie, f.identityAppID(), `{"name":""}`)
	if status != http.StatusOK {
		t.Fatalf("clear status=%d payload=%v", status, payload)
	}
	cleared := identityCompanyRow(t, f, cookie)
	if cleared["name"] != "本公司" || cleared["name_source"] != "config" {
		t.Fatalf("after clearing = %v, want the configured name back", cleared)
	}
}

// The company node follows the rename — but only while it still wears the old name.
func TestFeishuRenameFollowsTheCompanyNode(t *testing.T) {
	f := newOrgFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// A sync creates the company node (named after the company) and moves the company's legacy
	// top-level departments under it.
	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("sync status=%d payload=%v", status, payload)
	}
	node := companyNode(t, f, f.identityAppID())
	if node == nil || node.Name != "本公司" {
		t.Fatalf("company node = %+v, want 本公司", node)
	}
	// A department node named exactly like the new company name, under the company node: that is
	// the shape the real deployment has, and the sibling rule allows it (different parents).
	inner, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "智天成", SortOrder: 100, FeishuAppID: f.identityAppID(), ParentID: &node.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	status, payload = renameCompany(t, f, cookie, f.identityAppID(), `{"name":"智天成"}`)
	if status != http.StatusOK {
		t.Fatalf("rename status=%d payload=%v", status, payload)
	}
	renamed := payload["node_renamed"].(map[string]any)
	if renamed["id"].(float64) != float64(node.ID) || renamed["name"] != "智天成" {
		t.Fatalf("node_renamed = %v", renamed)
	}
	moved, err := f.db.GetOrgNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Name != "智天成" {
		t.Fatalf("company node = %q, want the new name", moved.Name)
	}
	// The same-named child node was not touched: it is a department, not the company.
	untouched, err := f.db.GetOrgNode(ctx, inner)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Name != "智天成" || untouched.ParentIDValue() != node.ID {
		t.Fatalf("the department node was disturbed: %+v", untouched)
	}

	// A node the operator renamed by hand is never renamed again by a company rename.
	hand := *moved
	hand.Name = "总部（手工改的）"
	if err := f.db.UpdateOrgNode(ctx, &hand); err != nil {
		t.Fatal(err)
	}
	status, payload = renameCompany(t, f, cookie, f.identityAppID(), `{"name":"智天成科技"}`)
	if status != http.StatusOK {
		t.Fatalf("second rename status=%d payload=%v", status, payload)
	}
	if !warningList(payload).has("node_name_kept") {
		t.Fatalf("warnings = %v, want node_name_kept", payload["warnings"])
	}
	kept, err := f.db.GetOrgNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Name != "总部（手工改的）" {
		t.Fatalf("a hand-renamed node was overwritten: %q", kept.Name)
	}
}

// A root-level node that already wears the new name does not block the rename; it is reported, with
// the two ways out, because the next sync will refuse to create the company node.
func TestFeishuRenameReportsRootNameTaken(t *testing.T) {
	f := newOrgFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// The real deployment's shape before its first sync: a legacy top-level department node named
	// like the company, pinned to the identity application, no company node yet.
	legacy, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "智天成", SortOrder: 100,
		FeishuAppID: f.identityAppID(), FeishuDepartmentID: "od_legacy",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Before the rename the company is still called 本公司, so nothing wears its name yet.
	before := identityCompanyRow(t, f, cookie)
	if before["root_name_taken"] != nil {
		t.Fatalf("root_name_taken = %v, want nil while the name is still 本公司", before["root_name_taken"])
	}

	status, payload := renameCompany(t, f, cookie, f.identityAppID(), `{"name":"智天成"}`)
	if status != http.StatusOK {
		t.Fatalf("the rename itself must succeed: %d %v", status, payload)
	}
	if !warningList(payload).has("root_name_taken") {
		t.Fatalf("warnings = %v, want root_name_taken", payload["warnings"])
	}
	// And the row now reports both facts: the name is taken, and the next sync would be refused.
	row := identityCompanyRow(t, f, cookie)
	taken, _ := row["root_name_taken"].(map[string]any)
	if taken == nil || taken["node_id"].(float64) != float64(legacy) {
		t.Fatalf("root_name_taken after the rename = %v, want the legacy node", row["root_name_taken"])
	}
	if row["root_blocked"] != true {
		t.Fatalf("root_blocked = %v, want true (the sync would refuse)", row["root_blocked"])
	}
	// And the sync still refuses, with M92's message — the operator has the guidance already.
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusConflict {
		t.Fatalf("sync status=%d payload=%v, want the M92 refusal", status, payload)
	}
	if !strings.Contains(payload["error"].(map[string]any)["message"].(string), "root_node") {
		t.Fatalf("refusal message = %v, want the root_node hint", payload["error"])
	}

	// The documented way out works: sync first (old name), then rename — the same-named department
	// node is now *under* the company node, so nothing collides.
	f2 := newOrgFeishuFixture(t)
	cookie2 := f2.login(t, adminUser, adminPassword)
	if _, err := f2.db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "智天成", SortOrder: 100,
		FeishuAppID: f2.identityAppID(), FeishuDepartmentID: "od_legacy",
	}); err != nil {
		t.Fatal(err)
	}
	if status, out := f2.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie2); status != http.StatusOK {
		t.Fatalf("first sync status=%d payload=%v", status, out)
	}
	root := companyNode(t, f2, f2.identityAppID())
	if root == nil {
		t.Fatal("the company node was not created")
	}
	status, payload = renameCompany(t, f2, cookie2, f2.identityAppID(), `{"name":"智天成"}`)
	if status != http.StatusOK || warningList(payload).has("root_name_taken") {
		t.Fatalf("rename after the first sync = %d %v (no root_name_taken expected)", status, payload)
	}
	renamed, err := f2.db.GetOrgNode(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "智天成" {
		t.Fatalf("company node = %q", renamed.Name)
	}
	// Both nodes exist, one under the other: 智天成 → 智天成(部门) → 平台组…
	nodes, err := f2.db.ListOrgNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sameName := 0
	for _, node := range nodes {
		if node.Name == "智天成" {
			sameName++
		}
	}
	if sameName != 2 {
		t.Fatalf("nodes named 智天成 = %d, want the company node and the department node", sameName)
	}
}

// Renaming a console-registered company through its app id is the same write as through its row id.
func TestFeishuRenameConsoleCompanyByAppID(t *testing.T) {
	f := newOrgFeishuFixture(t)
	stub := clientStub(t, f, "cli_ccc")
	installClientStub(t, f, stub)
	cookie := f.login(t, adminUser, adminPassword)
	id := createCompany(t, f, cookie, `{"name":"客户公司","app_id":"cli_ccc","app_secret":"s"}`)

	status, payload := renameCompany(t, f, cookie, "cli_ccc", `{"name":"客户公司（新）"}`)
	if status != http.StatusOK {
		t.Fatalf("rename status=%d payload=%v", status, payload)
	}
	if payload["company"].(map[string]any)["name"] != "客户公司（新）" {
		t.Fatalf("payload = %v", payload)
	}
	app, err := f.db.GetFeishuApp(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "客户公司（新）" {
		t.Fatalf("row name = %q, want the new one", app.Name)
	}
	// The name override table is for configuration-sourced companies only.
	names, err := f.db.ListFeishuCompanyNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("a console company wrote an override: %v", names)
	}
}

func TestFeishuRenameGuards(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.clientCompany(t) // a config company: cli_bbb / 某某科技
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// A configuration-owned company accepts the name and nothing else.
	for _, body := range []string{
		`{"name":"新名字","app_secret":"s"}`,
		`{"name":"新名字","note":"x"}`,
		`{"name":"新名字","enabled":false}`,
		`{"name":"新名字","root_node":"x"}`,
	} {
		status, out := renameCompany(t, f, cookie, f.identityAppID(), body)
		message, _ := out["error"].(map[string]any)["message"].(string)
		if status != http.StatusBadRequest || !strings.Contains(message, "feishu.") {
			t.Fatalf("body %s = %d %v, want a 400 naming the configuration", body, status, out)
		}
	}
	// Name validation is the configuration's own rule.
	for _, body := range []string{`{}`, `{"name":"cli_nope"}`, fmt.Sprintf(`{"name":%q}`, strings.Repeat("名", 65))} {
		if status, out := renameCompany(t, f, cookie, f.identityAppID(), body); status != http.StatusBadRequest {
			t.Fatalf("body %s = %d %v, want 400", body, status, out)
		}
	}
	// Colliding with another company is a 409 that names it.
	status, out := renameCompany(t, f, cookie, f.identityAppID(), `{"name":"某某科技"}`)
	if status != http.StatusConflict || !strings.Contains(out["error"].(map[string]any)["message"].(string), "feishu.companies") {
		t.Fatalf("collision = %d %v, want a 409 naming the config company", status, out)
	}
	// Unknown app id, and the identity application's own app id cannot be changed through the body.
	if status, _ := renameCompany(t, f, cookie, "cli_nope", `{"name":"x"}`); status != http.StatusNotFound {
		t.Fatalf("unknown app id = %d, want 404", status)
	}
	if status, _ := renameCompany(t, f, cookie, f.identityAppID(), `{"name":"x","app_id":"cli_other"}`); status != http.StatusBadRequest {
		t.Fatalf("changing app_id = %d, want 400", status)
	}
	// A viewer cannot rename.
	viewer := f.login(t, "reader", adminPassword)
	if res := f.call(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+f.identityAppID(), `{"name":"x"}`, viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer rename = %d, want 403", res.StatusCode)
	}

	// The company node rename can collide at the root level: refuse before writing the name.
	if _, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "本公司", SortOrder: 100, FeishuAppID: f.identityAppID(),
		FeishuDepartmentID: feishu.RootDepartmentID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{Name: "撞名", SortOrder: 100}); err != nil {
		t.Fatal(err)
	}
	status, out = renameCompany(t, f, cookie, f.identityAppID(), `{"name":"撞名"}`)
	if status != http.StatusConflict {
		t.Fatalf("node rename collision = %d %v, want 409", status, out)
	}
	// The name was not written: the company is still called 本公司.
	row := identityCompanyRow(t, f, cookie)
	if row["name"] != "本公司" {
		t.Fatalf("a refused rename still wrote the name: %v", row)
	}
}

// warningList is a tiny helper for asserting on the warnings of a payload.
type warningList map[string]any

func (w warningList) has(code string) bool {
	raw, ok := w["warnings"]
	if !ok {
		return false
	}
	list, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if text, ok := item.(string); ok && strings.HasPrefix(text, code) {
			return true
		}
	}
	return false
}

func warningCodes(payload map[string]any) []string {
	out := []string{}
	list, _ := payload["warnings"].([]any)
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

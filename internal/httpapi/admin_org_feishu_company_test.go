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

// The multi-company half of the directory sync (M92): one aigw importing several companies'
// organization structures, each through its own self-built application.
//
// The stub is the same fake Feishu tenant the M70 tests use; a second one stands for a client
// company. What must hold: the two companies' trees never merge, one account is never claimed by
// two companies' automatic merge, and the identity application's mapping stays the only login
// identity.

// clientCompany loads a second directory that deliberately reuses the identity company's names
// (研发部 / 市场部 / 王五) — the case that would silently merge without company scoping.
func (f *orgFeishuFixture) clientCompany(t *testing.T) string {
	t.Helper()
	stub := newDirStub(t)
	stub.departments = map[string][]dirDept{
		"0":   {{OpenDepartmentID: "od_x", Name: "研发部"}},
		"od_x": {},
	}
	stub.members = map[string][]dirUser{
		"od_x": {{OpenID: "ou_wang_b", UnionID: "on_wang_b", Name: "王五"}},
	}
	return f.withCompany(t, "cli_bbb", "某某科技", stub)
}

func TestFeishuMultiCompanySyncIsolatesCompanies(t *testing.T) {
	f := newOrgFeishuFixture(t)
	const appB = "cli_bbb"
	f.clientCompany(t)
	ranID, _, _ := f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)

	// The identity company first: 王五 is merged by name (the account carries no identity yet).
	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("identity sync status=%d payload=%v", status, payload)
	}
	if got := payload["company"].(map[string]any)["app_id"]; got != f.identityAppID() {
		t.Fatalf("sync reported company %v, want the identity application", got)
	}

	// Then the client company: same department names, same person name.
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync",
		`{"company":"`+appB+`","department_ids":["0","od_x"]}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("client sync status=%d payload=%v", status, payload)
	}
	created := payload["created_nodes"].([]any)
	if len(created) != 2 {
		t.Fatalf("client sync created %v, want its company node and 研发部", created)
	}

	nodes, err := f.db.ListOrgNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byNameApp := map[string]*domain.OrgNode{}
	for _, node := range nodes {
		byNameApp[node.FeishuAppID+"|"+node.Name] = node
	}
	rootA, rootB := byNameApp[f.identityAppID()+"|本公司"], byNameApp[appB+"|某某科技"]
	if rootA == nil || rootB == nil {
		t.Fatalf("company nodes missing: %v", byNameApp)
	}
	if rootB.ParentID != nil || rootB.FeishuDepartmentID != feishu.RootDepartmentID {
		t.Fatalf("client company node wrong: %+v", rootB)
	}
	devA, devB := byNameApp[f.identityAppID()+"|研发部"], byNameApp[appB+"|研发部"]
	if devA == nil || devB == nil {
		t.Fatalf("two companies' 研发部 must be two nodes: %v", byNameApp)
	}
	if devA.ID == devB.ID {
		t.Fatal("the two companies' 研发部 collapsed into one node")
	}
	if devA.ParentIDValue() != rootA.ID || devB.ParentIDValue() != rootB.ID {
		t.Fatalf("departments hang under the wrong company node: %+v %+v", devA, devB)
	}

	// The client company's 王五 must not steal the account the identity company already merged
	// by name: a person of one company is never another company's account.
	linked := payload["linked_users"].([]any)
	if len(linked) != 0 {
		t.Fatalf("client sync linked %v, want nothing (the account is already claimed)", linked)
	}
	ran, err := f.db.GetAccount(context.Background(), ranID)
	if err != nil {
		t.Fatal(err)
	}
	if ran.FeishuOpenID != "ou_wang" {
		t.Fatalf("the identity application's binding was overwritten: %+v", ran)
	}
	links, err := f.db.ListFeishuPersonLinks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("a company-scoped mapping was written for an unmatched person: %+v", links)
	}

	// Idempotent: the second run of the client company writes nothing.
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync",
		`{"company":"`+appB+`","department_ids":["0","od_x"]}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("second client sync status=%d", status)
	}
	if got := payload["created_nodes"].([]any); len(got) != 0 {
		t.Fatalf("second client sync created %v", got)
	}
	if got := payload["linked_users"].([]any); len(got) != 0 {
		t.Fatalf("second client sync linked %v", got)
	}
}

// A client company's people are mapped, not bound: the account's own Feishu identity (the DSH
// portal login) is never touched, and the mapping is what makes the next sync idempotent.
func TestFeishuClientCompanyMappingIsNotALoginIdentity(t *testing.T) {
	f := newOrgFeishuFixture(t)
	const appB = "cli_bbb"
	f.clientCompany(t)
	cookie := f.login(t, adminUser, adminPassword)
	// A local account named 王五 with no identity at all.
	accountID, err := f.db.UpsertAccount(context.Background(), &domain.Account{Name: "王五", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}

	status, payload := f.callJSON(t, http.MethodPut,
		"/admin/api/v1/org/feishu/users/ou_wang_b/account?company="+appB,
		`{"account_id":`+fmt.Sprint(accountID)+`}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("bind status=%d payload=%v", status, payload)
	}
	account, err := f.db.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "" {
		t.Fatalf("a client company's mapping became a login identity: %+v", account)
	}
	links, err := f.db.ListFeishuPersonLinks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].AppID != appB || links[0].OpenID != "ou_wang_b" || links[0].AccountID != accountID {
		t.Fatalf("links = %+v", links)
	}
	// The person's department node came with the binding (company node + 研发部).
	memberships, err := f.db.ListOrgMembershipsByAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships[accountID]) != 1 {
		t.Fatalf("memberships = %v, want the company's 研发部", memberships[accountID])
	}

	// A second person of that company cannot take the same account.
	stub := f.api.deps.Feishu.Companies[1].Client
	_ = stub
	status, payload = f.callJSON(t, http.MethodPut,
		"/admin/api/v1/org/feishu/users/ou_wang/account?company="+appB,
		`{"account_id":`+fmt.Sprint(accountID)+`}`, cookie)
	if status != http.StatusNotFound && status != http.StatusConflict {
		t.Fatalf("a person outside that company's directory = %d %v", status, payload)
	}

	// Unbind removes the mapping and leaves the account (and any identity) alone.
	status, payload = f.callJSON(t, http.MethodDelete,
		"/admin/api/v1/org/feishu/users/ou_wang_b/account?company="+appB, "", cookie)
	if status != http.StatusOK || payload["unbound"] != true {
		t.Fatalf("unbind = %d %v", status, payload)
	}
	if links, _ = f.db.ListFeishuPersonLinks(context.Background()); len(links) != 0 {
		t.Fatalf("unbind left the mapping: %+v", links)
	}
	status, payload = f.callJSON(t, http.MethodDelete,
		"/admin/api/v1/org/feishu/users/ou_wang_b/account?company="+appB, "", cookie)
	if status != http.StatusOK || payload["unbound"] != false {
		t.Fatalf("second unbind = %d %v, want a no-op", status, payload)
	}
}

// The company node is adopted when it already exists, moved into when the company's departments
// are still at the root level, and created when there is nothing to adopt.
func TestFeishuCompanyNodeAdoptionAndLegacyReparent(t *testing.T) {
	f := newOrgFeishuFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	// Legacy local state: a hand-made company node (名字与配置一致) and a top-level department
	// node written before M92 (department link, no company).
	rootID, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{Name: "本公司", SortOrder: 100})
	if err != nil {
		t.Fatal(err)
	}
	legacyID, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{Name: "老部门", SortOrder: 100, FeishuDepartmentID: "od_legacy"})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := f.db.CreateOrgNode(ctx, &domain.OrgNode{Name: "老部门-子", SortOrder: 100, ParentID: &legacyID})
	if err != nil {
		t.Fatal(err)
	}

	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("sync status=%d payload=%v", status, payload)
	}
	company := payload["company"].(map[string]any)
	root := company["root"].(map[string]any)
	if root["will_adopt"] != true {
		t.Fatalf("the existing same-name root was not adopted: %v", root)
	}
	adopted, err := f.db.GetOrgNode(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.FeishuAppID != f.identityAppID() || adopted.FeishuDepartmentID != feishu.RootDepartmentID {
		t.Fatalf("adoption did not stamp the company node: %+v", adopted)
	}
	if len(payload["created_nodes"].([]any)) != 3 {
		t.Fatalf("the company node must not be created twice: %v", payload["created_nodes"])
	}

	// The legacy department (and only it — its child follows the tree) moved under the company
	// node, and its link was stamped with the identity application.
	reparented := payload["reparented_nodes"].([]any)
	if len(reparented) != 1 || reparented[0].(map[string]any)["name"] != "老部门" {
		t.Fatalf("reparented = %v", reparented)
	}
	moved, err := f.db.GetOrgNode(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ParentIDValue() != rootID {
		t.Fatalf("legacy department parent = %v, want the company node", moved.ParentID)
	}
	if moved.FeishuAppID != f.identityAppID() {
		t.Fatalf("legacy link not stamped: %+v", moved)
	}
	child, err := f.db.GetOrgNode(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentIDValue() != legacyID {
		t.Fatalf("a child of the moved node was disturbed: %+v", child)
	}

	// A department too deep to move is reported instead of moved (16 levels is the limit).
	f2 := newOrgFeishuFixture(t)
	deepRoot, err := f2.db.CreateOrgNode(ctx, &domain.OrgNode{Name: "深部门", SortOrder: 100, FeishuDepartmentID: "od_deep"})
	if err != nil {
		t.Fatal(err)
	}
	parent := deepRoot
	for i := 0; i < 16; i++ {
		id, err := f2.db.CreateOrgNode(ctx, &domain.OrgNode{
			Name: fmt.Sprintf("层%d", i), SortOrder: 100, ParentID: &parent,
		})
		if err != nil {
			t.Fatal(err)
		}
		parent = id
	}
	cookie2 := f2.login(t, adminUser, adminPassword)
	status, payload = f2.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie2)
	if status != http.StatusOK {
		t.Fatalf("deep sync status=%d payload=%v", status, payload)
	}
	if got := payload["reparented_nodes"].([]any); len(got) != 0 {
		t.Fatalf("a too-deep department was moved anyway: %v", got)
	}
	warnings := strings.Join(warningStrings(payload["warnings"]), " ")
	if !strings.Contains(warnings, "reparent_depth_exceeded") && !strings.Contains(warnings, "层") {
		t.Fatalf("the depth refusal must be reported: %v", payload["warnings"])
	}
	stillRoot, err := f2.db.GetOrgNode(ctx, deepRoot)
	if err != nil {
		t.Fatal(err)
	}
	if stillRoot.ParentID != nil {
		t.Fatalf("the deep department moved: %+v", stillRoot)
	}
}

// A same-name root node that belongs to another company is a refusal, not a guess: the two
// company nodes must not be confused with each other.
func TestFeishuCompanyRootNameConflict(t *testing.T) {
	f := newOrgFeishuFixture(t)
	ctx := context.Background()
	const appB = "cli_bbb"
	f.clientCompany(t)
	cookie := f.login(t, adminUser, adminPassword)

	// The client company gets its node first (它的名字取自配置).
	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync",
		`{"company":"`+appB+`","department_ids":["0","od_x"]}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("client sync status=%d payload=%v", status, payload)
	}
	// Now the identity company is told to use the client company's node name.
	f.api.deps.Feishu.Companies[0].RootName = "某某科技"
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusConflict {
		t.Fatalf("status=%d payload=%v, want a 409 naming the setting", status, payload)
	}
	if msg := payload["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "root_node") {
		t.Fatalf("conflict message = %v, want the root_node hint", msg)
	}
	// Nothing else was written by the refused run.
	nodes, err := f.db.ListOrgNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.Name == "研发部" && node.FeishuAppID == f.identityAppID() {
			t.Fatal("a refused sync still created department nodes")
		}
	}
}

// The company parameter resolves an app id or a configured name, defaults to the identity
// application, and refuses anything else with the list of what it knows.
func TestFeishuCompanyParameterResolution(t *testing.T) {
	f := newOrgFeishuFixture(t)
	const appB = "cli_bbb"
	f.clientCompany(t)
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory?company=某某科技", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("by name status=%d payload=%v", status, payload)
	}
	if got := payload["company"].(map[string]any)["app_id"]; got != appB {
		t.Fatalf("resolved company = %v, want %s", got, appB)
	}
	if got := payload["company"].(map[string]any)["identity"]; got != false {
		t.Fatalf("a client company must not claim to be the identity application: %v", payload["company"])
	}
	status, payload = f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory?company="+appB, "", cookie)
	if status != http.StatusOK {
		t.Fatalf("by app id status=%d payload=%v", status, payload)
	}
	status, payload = f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", cookie)
	if status != http.StatusOK || payload["company"].(map[string]any)["identity"] != true {
		t.Fatalf("omitted company must be the identity application: %d %v", status, payload)
	}
	status, payload = f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory?company=不存在的公司", "", cookie)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown company status=%d payload=%v, want 400", status, payload)
	}
	msg := payload["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "某某科技") || !strings.Contains(msg, "本公司") {
		t.Fatalf("the refusal must list the known companies: %v", msg)
	}
}

// The companies endpoint is the dialog's dropdown: which companies exist, where each one's node
// is, and how much is already imported. A viewer may read it (the write routes stay admin-only).
func TestFeishuCompaniesEndpoint(t *testing.T) {
	f := newOrgFeishuFixture(t)
	const appB = "cli_bbb"
	f.clientCompany(t)

	// The identity company's node is there after a sync; the client company's is not yet.
	cookie := f.login(t, adminUser, adminPassword)
	if status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie); status != http.StatusOK {
		t.Fatalf("sync status=%d payload=%v", status, payload)
	}

	viewer := f.login(t, "reader", adminPassword)
	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/companies", "", viewer)
	if status != http.StatusOK {
		t.Fatalf("viewer list status=%d payload=%v", status, payload)
	}
	if payload["identity_app_id"] != f.identityAppID() {
		t.Fatalf("identity_app_id = %v", payload["identity_app_id"])
	}
	rows := payload["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("companies = %v, want two", rows)
	}
	first := rows[0].(map[string]any)
	if first["identity"] != true || first["root_node_name"] != "本公司" {
		t.Fatalf("identity row = %v", first)
	}
	if first["company_nodes"].(float64) == 0 {
		t.Fatalf("the identity company's node count is 0 after a sync: %v", first)
	}
	second := rows[1].(map[string]any)
	if second["app_id"] != appB || second["root_will_create"] != true {
		t.Fatalf("client row = %v, want a to-be-created company node", second)
	}

	// Purging a client company's mappings is a write: viewers are refused, and the identity
	// application cannot be purged at all.
	if res := f.call(t, http.MethodDelete, "/admin/api/v1/org/feishu/companies/"+appB+"/links", "", viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer purge status=%d, want 403", res.StatusCode)
	}
	res := f.call(t, http.MethodDelete, "/admin/api/v1/org/feishu/companies/"+f.identityAppID()+"/links", "", cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("purging the identity application status=%d, want 400", res.StatusCode)
	}

	// A real purge removes the mappings of one company and nothing else.
	if err := f.db.UpsertFeishuPersonLink(context.Background(), domain.FeishuPersonLink{
		AppID: appB, OpenID: "ou_wang_b", Name: "王五", AccountID: 1, BoundBy: adminUser,
	}); err != nil {
		t.Fatal(err)
	}
	status, payload = f.callJSON(t, http.MethodDelete, "/admin/api/v1/org/feishu/companies/"+appB+"/links", "", cookie)
	if status != http.StatusOK || payload["deleted_links"].(float64) != 1 {
		t.Fatalf("purge = %d %v", status, payload)
	}
	nodesBefore, _ := f.db.ListOrgNodes(context.Background())
	links, _ := f.db.ListFeishuPersonLinks(context.Background())
	if len(links) != 0 {
		t.Fatalf("links survived the purge: %+v", links)
	}
	if len(nodesBefore) == 0 {
		t.Fatal("the purge deleted org nodes: that is a separate decision")
	}
}

// The account payload carries the company-scoped mappings next to the identity (M92), so an
// operator can see a client company's person on the account row.
func TestFeishuCompanyLinksAppearOnTheAccount(t *testing.T) {
	f := newOrgFeishuFixture(t)
	const appB = "cli_bbb"
	f.clientCompany(t)
	cookie := f.login(t, adminUser, adminPassword)
	accountID, err := f.db.UpsertAccount(context.Background(), &domain.Account{Name: "客户-王五", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpsertFeishuPersonLink(context.Background(), domain.FeishuPersonLink{
		AppID: appB, OpenID: "ou_wang_b", Name: "王五", AccountID: accountID, BoundBy: "sync",
	}); err != nil {
		t.Fatal(err)
	}

	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/accounts", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("accounts status=%d", status)
	}
	found := false
	for _, raw := range payload["data"].([]any) {
		row := raw.(map[string]any)
		if row["id"].(float64) != float64(accountID) {
			continue
		}
		found = true
		feishuBlock := row["feishu"].(map[string]any)
		if feishuBlock["bound"] != false {
			t.Fatalf("a company mapping must not read as a login identity: %v", feishuBlock)
		}
		links := feishuBlock["links"].([]any)
		if len(links) != 1 {
			t.Fatalf("links = %v, want the client company's mapping", links)
		}
		link := links[0].(map[string]any)
		if link["app_id"] != appB || link["company"] != "某某科技" || link["name"] != "王五" {
			t.Fatalf("link = %v", link)
		}
	}
	if !found {
		t.Fatal("the account is missing from the list")
	}

	// The org node list labels which company a node came from.
	status, payload = f.callJSON(t, http.MethodGet, "/admin/api/v1/org/nodes", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("org nodes status=%d", status)
	}
	nodesPrinted := 0
	for _, raw := range payload["data"].([]any) {
		node := raw.(map[string]any)
		if node["feishu_app_id"] != "" {
			nodesPrinted++
		}
		if node["feishu_app_id"] != "" && node["company"] == "" {
			t.Fatalf("a company id without a configured name: %v", node)
		}
	}
	_ = nodesPrinted
}

// warningStrings is a small helper for asserting on the warning list of a payload.
func warningStrings(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

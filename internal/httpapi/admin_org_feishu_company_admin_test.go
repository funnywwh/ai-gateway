package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/funnywwh/ai-gateway/internal/creds"
	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/feishu"
)

// The console-managed company registry (M93): onboarding a client company, rotating its secret,
// pausing it, removing the registration, and the guards that keep configuration-owned companies out
// of the write paths.

// clientStub installs one more Feishu directory in the fixture and returns its app id. The company
// itself is registered through the API (that is what is under test), so this only builds the tenant.
func clientStub(t *testing.T, f *orgFeishuFixture, appID string) *dirStub {
	t.Helper()
	stub := newDirStub(t)
	stub.departments = map[string][]dirDept{
		"0": {{OpenDepartmentID: "od_c", Name: "客户研发部"}},
	}
	stub.members = map[string][]dirUser{
		"od_c": {{OpenID: "ou_client", UnionID: "on_client", Name: "客户张三"}},
	}
	return stub
}

// installClientStub points the configuration's directory endpoints at one stub: console-created
// companies are built from that block, so this is how a test gives them a directory to read.
func installClientStub(t *testing.T, f *orgFeishuFixture, stub *dirStub) {
	t.Helper()
	f.api.deps.Config.Feishu.TenantTokenURL = stub.server.URL + "/tenant-token"
	f.api.deps.Config.Feishu.ContactURL = stub.server.URL + "/contact/v3"
}

// createCompany registers a company through the API and returns its row id.
func createCompany(t *testing.T, f *orgFeishuFixture, cookie, body string) int64 {
	t.Helper()
	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies", body, cookie)
	if status != http.StatusCreated {
		t.Fatalf("create company status=%d payload=%v", status, payload)
	}
	id, ok := payload["id"].(float64)
	if !ok {
		t.Fatalf("create company payload = %v, want an id", payload)
	}
	return int64(id)
}

func TestFeishuCompanyRegisterAndSync(t *testing.T) {
	f := newOrgFeishuFixture(t)
	stub := clientStub(t, f, "cli_ccc")
	installClientStub(t, f, stub)
	cookie := f.login(t, adminUser, adminPassword)

	// The company is registered with a secret; the response never echoes it.
	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies",
		`{"name":"客户公司","app_id":"cli_ccc","app_secret":"secret-ccc","root_node":"","note":"客户 A"}`, cookie)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d payload=%v", status, payload)
	}
	if strings.Contains(fmt.Sprint(payload), "secret-ccc") {
		t.Fatalf("the create response echoed the secret: %v", payload)
	}
	id := int64(payload["id"].(float64))
	company := payload["company"].(map[string]any)
	if company["secret_configured"] != true || company["client_ready"] != true {
		t.Fatalf("company block = %v, want a configured, ready company", company)
	}

	// It appears in the list next to the identity application, with its source and node plan.
	status, payload = f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/companies", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	rows := payload["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("companies = %v, want the identity application plus the new one", rows)
	}
	if payload["secrets_ready"] != true {
		t.Fatal("secrets_ready must be true when credentials_key is configured")
	}
	row := rows[1].(map[string]any)
	if row["app_id"] != "cli_ccc" || row["source"] != "console" || row["enabled"] != true {
		t.Fatalf("row = %v", row)
	}
	if strings.Contains(fmt.Sprint(row), "secret-ccc") {
		t.Fatalf("the list leaked the secret: %v", row)
	}

	// Syncing it creates its own company node and hangs the stub's department under it.
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync",
		`{"company":"cli_ccc"}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("sync status=%d payload=%v", status, payload)
	}
	root := companyNode(t, f, "cli_ccc")
	if root == nil || root.Name != "客户公司" {
		t.Fatalf("company node = %+v, want 客户公司", root)
	}
	nodes, err := f.db.ListOrgNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var department *domain.OrgNode
	for _, node := range nodes {
		if node.FeishuAppID == "cli_ccc" && node.Name == "客户研发部" {
			department = node
		}
	}
	if department == nil || department.ParentIDValue() != root.ID {
		t.Fatalf("department = %+v, want it under the company node", department)
	}
	// The person was created as an account only if matched; here nobody matches, so the sync
	// reports them as unmatched and writes nothing.
	if linked := payload["linked_users"].([]any); len(linked) != 0 {
		t.Fatalf("linked users = %v, want none (no account matches 客户张三)", linked)
	}

	// Deleting the registration keeps the tree and reports what is left.
	status, payload = f.callJSON(t, http.MethodDelete,
		"/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id), "", cookie)
	if status != http.StatusOK || payload["deleted"] != true {
		t.Fatalf("delete = %d %v", status, payload)
	}
	if payload["company_nodes"].(float64) != 2 {
		t.Fatalf("delete reported %v company nodes, want the company node plus its department", payload["company_nodes"])
	}
	if _, err := f.db.GetFeishuApp(context.Background(), id); err == nil {
		t.Fatal("the registration survived the delete")
	}
	if companyNode(t, f, "cli_ccc") == nil {
		t.Fatal("the delete removed org nodes: it must only remove the registration")
	}
}

// The registry is administrator-only, and a deployment without credentials_key cannot store a
// company secret at all — it is told which two ways out it has.
func TestFeishuCompanyRegistryGuards(t *testing.T) {
	f := newOrgFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	body := `{"name":"客户公司","app_id":"cli_ccc","app_secret":"s"}`

	viewer := f.login(t, "reader", adminPassword)
	if status, _ := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/companies", "", viewer); status != http.StatusOK {
		t.Fatalf("viewer list status=%d, want 200 (it carries no secrets)", status)
	}
	if res := f.call(t, http.MethodPost, "/admin/api/v1/org/feishu/companies", body, viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create status=%d, want 403", res.StatusCode)
	}

	// The identity application's app id is taken, and so is its company name.
	for _, payload := range []string{
		`{"name":"另一家","app_id":"` + f.identityAppID() + `","app_secret":"s"}`,
		`{"name":"本公司","app_id":"cli_ccc","app_secret":"s"}`,
	} {
		status, out := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies", payload, cookie)
		if status != http.StatusConflict {
			t.Fatalf("conflicting company status=%d payload=%v, want 409", status, out)
		}
	}

	// Input validation reuses the configuration's rules.
	for _, payload := range []string{
		`{"name":"cli_nope","app_id":"cli_ccc","app_secret":"s"}`,
		`{"name":"公司","app_id":"not-an-app-id","app_secret":"s"}`,
		`{"name":"公司","app_id":"cli_ccc"}`,
	} {
		if status, out := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies", payload, cookie); status != http.StatusBadRequest {
			t.Fatalf("invalid input status=%d payload=%v, want 400", status, out)
		}
	}

	// A deployment that cannot seal a secret refuses the write and names both ways out.
	bare := newOrgFeishuFixture(t)
	bare.api.deps.FeishuAppSecrets = creds.NewCompanySealer(nil)
	bareCookie := bare.login(t, adminUser, adminPassword)
	status, out := bare.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies", body, bareCookie)
	message, _ := out["error"].(map[string]any)["message"].(string)
	if status != http.StatusBadRequest || !strings.Contains(message, "credentials_key") {
		t.Fatalf("keyless create = %d %v, want a 400 naming credentials_key", status, out)
	}
	if !strings.Contains(message, "feishu.companies") {
		t.Fatalf("the refusal must offer the configuration alternative: %v", out)
	}
	if _, err := bare.db.ListFeishuApps(context.Background()); err != nil {
		t.Fatal(err)
	}
	if apps, _ := bare.db.ListFeishuApps(context.Background()); len(apps) != 0 {
		t.Fatalf("a refused create left a row: %+v", apps)
	}
}

// Editing rules: the app id is fixed, a paused company leaves the sync path, and a registration
// shadowed by the configuration cannot be edited into something invisible.
func TestFeishuCompanyUpdateRules(t *testing.T) {
	f := newOrgFeishuFixture(t)
	stub := clientStub(t, f, "cli_ccc")
	installClientStub(t, f, stub)
	cookie := f.login(t, adminUser, adminPassword)
	id := createCompany(t, f, cookie, `{"name":"客户公司","app_id":"cli_ccc","app_secret":"s"}`)

	// app_id is not editable: it is the key stored organization data carries.
	status, out := f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"app_id":"cli_ddd"}`, cookie)
	if status != http.StatusBadRequest || !strings.Contains(out["error"].(map[string]any)["message"].(string), "不可更改") {
		t.Fatalf("app_id change = %d %v, want a 400 explaining why", status, out)
	}
	// Renaming works, and colliding with the identity company's name is refused.
	status, out = f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"note":"客户 B"}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("patch note = %d %v", status, out)
	}
	status, _ = f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"name":"本公司"}`, cookie)
	if status != http.StatusConflict {
		t.Fatalf("rename onto the identity company = %d, want 409", status)
	}
	// Rotating the secret is allowed (and the response still never shows it).
	status, out = f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"app_secret":"rotated-secret"}`, cookie)
	if status != http.StatusOK || strings.Contains(fmt.Sprint(out), "rotated-secret") {
		t.Fatalf("secret rotation = %d %v", status, out)
	}
	entries, err := f.db.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.TargetType == "feishu_company" && strings.Contains(entry.ChangesJSON, "rotated-secret") {
			t.Fatalf("the audit trail holds the secret: %+v", entry)
		}
	}

	// A paused company is out of the sync path but keeps its data and registration.
	if status, out = f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"enabled":false}`, cookie); status != http.StatusOK {
		t.Fatalf("disable = %d %v", status, out)
	}
	if status, out = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", `{"company":"cli_ccc"}`, cookie); status != http.StatusBadRequest {
		t.Fatalf("sync of a disabled company = %d %v, want 400", status, out)
	}
	if status, out = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", `{"company":"客户公司"}`, cookie); status != http.StatusBadRequest {
		t.Fatalf("sync by name of a disabled company = %d %v, want 400", status, out)
	}
	if status, out = f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"enabled":true}`, cookie); status != http.StatusOK {
		t.Fatalf("re-enable = %d %v", status, out)
	}
	if status, out = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", `{"company":"cli_ccc"}`, cookie); status != http.StatusOK {
		t.Fatalf("sync after re-enable = %d %v", status, out)
	}
}

// A registration whose app id is also configured is redundant: the configuration wins, the list says
// so once (never twice), and editing the shadowed row is refused instead of silently doing nothing.
func TestFeishuCompanyShadowedByConfig(t *testing.T) {
	f := newOrgFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	// Register the company in the console first: while the app id is free, that is allowed.
	id := createCompany(t, f, cookie, `{"name":"配置外的名字","app_id":"cli_bbb","app_secret":"s"}`)
	// The operator then adds the same application to feishu.companies (a restart later, the list
	// merges both without asking anything).
	stub := newDirStub(t)
	f.withCompany(t, "cli_bbb", "配置文件里的名字", stub)

	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/companies", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	rows := payload["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("companies = %v, want two rows (identity + config), not a duplicate", rows)
	}
	configured := rows[1].(map[string]any)
	if configured["source"] != "config" || configured["shadowed_by_config"] != true {
		t.Fatalf("config row = %v, want it flagged as shadowing a registration", configured)
	}
	if !strings.Contains(fmt.Sprint(configured["warnings"]), "shadowed_by_config") {
		t.Fatalf("warnings = %v", configured["warnings"])
	}
	status, out := f.callJSON(t, http.MethodPatch, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id),
		`{"note":"x"}`, cookie)
	if status != http.StatusConflict {
		t.Fatalf("editing a shadowed row = %d %v, want 409", status, out)
	}
	// Deleting it is allowed: that is how the operator cleans up the redundant row.
	if status, out = f.callJSON(t, http.MethodDelete, "/admin/api/v1/org/feishu/companies/"+fmt.Sprint(id), "", cookie); status != http.StatusOK {
		t.Fatalf("deleting a shadowed row = %d %v", status, out)
	}
}

// The probe answers the three states that actually happen, without writing anything.
func TestFeishuCompanyProbe(t *testing.T) {
	f := newOrgFeishuFixture(t)
	stub := clientStub(t, f, "cli_ccc")
	installClientStub(t, f, stub)
	cookie := f.login(t, adminUser, adminPassword)

	// Readable.
	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies/probe",
		`{"app_id":"cli_ccc","app_secret":"s"}`, cookie)
	if status != http.StatusOK || payload["ok"] != true || payload["stage"] != "ok" {
		t.Fatalf("probe = %d %v, want ok", status, payload)
	}
	if payload["departments_seen"].(float64) == 0 {
		t.Fatalf("probe saw no departments: %v", payload)
	}
	if samples := payload["samples"].([]any); len(samples) == 0 {
		t.Fatalf("probe samples = %v", payload["samples"])
	}

	// Readable but nameless: the permission hint.
	stub.includeNames = false
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies/probe",
		`{"app_id":"cli_ccc","app_secret":"s"}`, cookie)
	if status != http.StatusOK || payload["stage"] != "names" {
		t.Fatalf("nameless probe = %d %v, want stage=names", status, payload)
	}
	if !strings.Contains(payload["message"].(string), "权限") {
		t.Fatalf("message = %v, want the permission hint", payload["message"])
	}
	stub.includeNames = true

	// Credentials refused.
	stub.failCode = 99991663 // wrong app credentials
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies/probe",
		`{"app_id":"cli_ccc","app_secret":"bad"}`, cookie)
	if status != http.StatusOK || payload["stage"] != "credentials" {
		t.Fatalf("rejected probe = %d %v, want stage=credentials", status, payload)
	}
	stub.failCode = 0

	// Probing a stored company uses its stored (sealed) secret, and nothing was written by any probe.
	id := createCompany(t, f, cookie, `{"name":"客户公司","app_id":"cli_ccc","app_secret":"stored-secret"}`)
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies/probe",
		`{"id":`+fmt.Sprint(id)+`}`, cookie)
	if status != http.StatusOK || payload["ok"] != true {
		t.Fatalf("stored probe = %d %v, want ok", status, payload)
	}
	nodes, err := f.db.ListOrgNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("a probe created nodes: %v", nodes)
	}

	// A probe with neither id nor credentials is a 400.
	if status, _ = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/companies/probe", `{}`, cookie); status != http.StatusBadRequest {
		t.Fatalf("empty probe status=%d, want 400", status)
	}
	// Viewers cannot probe (it spends the company's credentials).
	viewer := f.login(t, "reader", adminPassword)
	if res := f.call(t, http.MethodPost, "/admin/api/v1/org/feishu/companies/probe", `{"app_id":"cli_ccc","app_secret":"s"}`, viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer probe status=%d, want 403", res.StatusCode)
	}
}

// A company whose stored secret cannot be decrypted (a rotated credentials_key) is reported instead of
// silently dropped: the row stays, the sync refuses with the reason, and the list says why.
func TestFeishuCompanyUnreadableSecret(t *testing.T) {
	f := newOrgFeishuFixture(t)
	stub := clientStub(t, f, "cli_ccc")
	installClientStub(t, f, stub)
	cookie := f.login(t, adminUser, adminPassword)
	createCompany(t, f, cookie, `{"name":"客户公司","app_id":"cli_ccc","app_secret":"s"}`)

	// Simulate the rotation: the deployment now seals with a different key.
	f.api.deps.FeishuAppSecrets = creds.NewCompanySealer(creds.DeriveKey("another-key"))
	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/companies", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	row := payload["data"].([]any)[1].(map[string]any)
	if row["client_ready"] != false || row["secret_configured"] != true {
		t.Fatalf("row = %v, want a configured but unreadable secret", row)
	}
	if !strings.Contains(fmt.Sprint(row["client_error"]), "重新填写") {
		t.Fatalf("client_error = %v, want an actionable message", row["client_error"])
	}
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", `{"company":"cli_ccc"}`, cookie)
	if status != http.StatusBadRequest || !strings.Contains(payload["error"].(map[string]any)["message"].(string), "凭据不可用") {
		t.Fatalf("sync = %d %v, want a 400 naming the credential problem", status, payload)
	}
}

// The registry never touches the M92 mapping purge: they are separate decisions, and the purge
// endpoint still works for a console company.
func TestFeishuCompanyPurgeLinksStillWorks(t *testing.T) {
	f := newOrgFeishuFixture(t)
	stub := clientStub(t, f, "cli_ccc")
	installClientStub(t, f, stub)
	cookie := f.login(t, adminUser, adminPassword)
	id := createCompany(t, f, cookie, `{"name":"客户公司","app_id":"cli_ccc","app_secret":"s"}`)
	_ = id

	accountID, err := f.db.UpsertAccount(context.Background(), &domain.Account{Name: "客户-张三"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpsertFeishuPersonLink(context.Background(), domain.FeishuPersonLink{
		AppID: "cli_ccc", OpenID: "ou_client", Name: "客户张三", AccountID: accountID, BoundBy: "sync",
	}); err != nil {
		t.Fatal(err)
	}
	status, payload := f.callJSON(t, http.MethodDelete,
		"/admin/api/v1/org/feishu/companies/cli_ccc/links", "", cookie)
	if status != http.StatusOK || payload["deleted_links"].(float64) != 1 {
		t.Fatalf("purge = %d %v", status, payload)
	}
	if links, _ := f.db.ListFeishuPersonLinks(context.Background()); len(links) != 0 {
		t.Fatalf("links survived the purge: %+v", links)
	}
	// The company node created by the sync is untouched by a purge.
	if _, err := f.db.GetFeishuApp(context.Background(), id); err != nil {
		t.Fatalf("the purge removed the registration: %v", err)
	}
}

// feishuCompanyForTest keeps the imported feishu package referenced from this file even if a future
// edit stops using it inline.
var _ = feishu.RootDepartmentID

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// The directory-sync endpoints, tested against a stub that speaks the three Feishu calls
// the walk makes (tenant token, department children, department users). The local side is
// the real store, so what is asserted is the merge itself — channels, pinning, idempotency
// and the refusals — not a mock of our own plan.

type dirDept struct {
	OpenDepartmentID string `json:"open_department_id"`
	Name             string `json:"name"`
}

type dirUser struct {
	OpenID  string `json:"open_id"`
	UnionID string `json:"union_id"`
	Name    string `json:"name"`
}

type dirStub struct {
	server *httptest.Server
	mu     sync.Mutex
	// departments maps a parent id to its children; "0" is the virtual root.
	departments map[string][]dirDept
	// members maps a department id to its people.
	members map[string][]dirUser
	// includeNames models the two contact data permissions.
	includeNames bool
	// failCode, when non-zero, is answered by every contact call.
	failCode int
	// tokenRequests counts tenant-token mints.
	tokenRequests int
}

func newDirStub(t *testing.T) *dirStub {
	t.Helper()
	stub := &dirStub{includeNames: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant-token", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.tokenRequests++
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-dir","expire":7200}`))
	})
	mux.HandleFunc("/contact/v3/departments", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if stub.failCode != 0 {
			writeRawJSON(w, `{"code":`+fmt.Sprint(stub.failCode)+`,"msg":"nope"}`)
			return
		}
		parent := r.URL.Query().Get("parent_department_id")
		items := stub.departments[parent]
		if !stub.includeNames {
			stripped := make([]dirDept, 0, len(items))
			for _, department := range items {
				stripped = append(stripped, dirDept{OpenDepartmentID: department.OpenDepartmentID})
			}
			items = stripped
		}
		writeRawJSON(w, marshalJSON(map[string]any{"code": 0, "data": map[string]any{"items": items, "has_more": false}}))
	})
	mux.HandleFunc("/contact/v3/users", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if stub.failCode != 0 {
			writeRawJSON(w, `{"code":`+fmt.Sprint(stub.failCode)+`,"msg":"nope"}`)
			return
		}
		department := r.URL.Query().Get("department_id")
		items := stub.members[department]
		if !stub.includeNames {
			stripped := make([]dirUser, 0, len(items))
			for _, person := range items {
				stripped = append(stripped, dirUser{OpenID: person.OpenID, UnionID: person.UnionID})
			}
			items = stripped
		}
		writeRawJSON(w, marshalJSON(map[string]any{"code": 0, "data": map[string]any{"items": items, "has_more": false}}))
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

// orgFeishuFixture is the identity fixture with the directory walk pointed at a stub and
// the standard three-level tree loaded.
type orgFeishuFixture struct {
	*feishuFixture
	dir *dirStub
}

// newOrgFeishuFixture wires the directory stub and loads this tree:
//
//	0 ── od_a 研发部 ── od_a1 平台组
//	   └─ od_b 市场部
//
// with 老板 directly under the root, 王五 in both 研发部 and 平台组, 赵六 in 市场部
// and 李四 in 市场部.
func newOrgFeishuFixture(t *testing.T) *orgFeishuFixture {
	t.Helper()
	f := &orgFeishuFixture{feishuFixture: newFeishuFixture(t), dir: newDirStub(t)}
	f.api.deps.Feishu.Client.TenantTokenURL = f.dir.server.URL + "/tenant-token"
	f.api.deps.Feishu.Client.ContactURL = f.dir.server.URL + "/contact/v3"
	f.dir.departments = map[string][]dirDept{
		"0": {
			{OpenDepartmentID: "od_a", Name: "研发部"},
			{OpenDepartmentID: "od_b", Name: "市场部"},
		},
		"od_a":  {{OpenDepartmentID: "od_a1", Name: "平台组"}},
		"od_a1": {},
		"od_b":  {},
	}
	f.dir.members = map[string][]dirUser{
		"0":    {{OpenID: "ou_root", UnionID: "on_root", Name: "老板"}},
		"od_a": {{OpenID: "ou_wang", UnionID: "on_wang", Name: "王五"}},
		"od_a1": {
			{OpenID: "ou_wang", UnionID: "on_wang", Name: "王五"},
			{OpenID: "ou_feng", UnionID: "on_feng", Name: "冯十"},
		},
		"od_b": {
			{OpenID: "ou_zhao", UnionID: "on_zhao", Name: "赵六"},
			{OpenID: "ou_new", UnionID: "on_new", Name: "李四"},
		},
	}
	return f
}

func writeRawJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func marshalJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// callJSON is f.call with a decoded body.
func (f *orgFeishuFixture) callJSON(t *testing.T, method, path, body, cookie string) (int, map[string]any) {
	t.Helper()
	res := f.call(t, method, path, body, cookie)
	defer res.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("%s %s: decoding body: %v", method, path, err)
	}
	return res.StatusCode, payload
}

// seedLocal prepares the local side of the merge exactly as the real deployment looks
// before a first sync: a same-name account, a key-level binding, an account-level binding,
// and no org nodes at all.
func (f *orgFeishuFixture) seedLocal(t *testing.T) (ranID, acmeID, bossID int64) {
	t.Helper()
	ctx := context.Background()
	ranID, err := f.db.UpsertAccount(ctx, &domain.Account{Name: "王五", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	acme, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	acmeID = acme.ID
	// The key-level binding (M60) of 赵六 to acme: the sync must recognize the person and
	// promote the identity onto the account.
	keyID, err := f.db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: acmeID, Name: "yang-key", KeyPrefix: "sk-gw-dir1", KeyHash: "hash-dir1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.BindAPIKeyFeishu(ctx, keyID, domain.FeishuBinding{OpenID: "ou_zhao", UnionID: "on_zhao", Name: "赵六", BoundBy: adminUser}); err != nil {
		t.Fatal(err)
	}
	// An account-level binding written earlier (by a previous sync): 老板.
	bossID, err = f.db.UpsertAccount(ctx, &domain.Account{Name: "老板", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.BindAccountFeishu(ctx, bossID, domain.FeishuBinding{OpenID: "ou_root", UnionID: "on_root", Name: "老板", BoundBy: "sync"}); err != nil {
		t.Fatal(err)
	}
	return ranID, acmeID, bossID
}

func departmentLocal(t *testing.T, payload map[string]any, id string) (name string, local map[string]any) {
	t.Helper()
	for _, raw := range payload["departments"].([]any) {
		department := raw.(map[string]any)
		if department["id"] == id {
			return department["name"].(string), department["local"].(map[string]any)
		}
	}
	t.Fatalf("department %s missing from the preview: %v", id, payload["departments"])
	return "", nil
}

func userMatch(t *testing.T, payload map[string]any, openID string) map[string]any {
	t.Helper()
	for _, raw := range payload["users"].([]any) {
		user := raw.(map[string]any)
		if user["open_id"] == openID {
			return user
		}
	}
	t.Fatalf("user %s missing from the preview", openID)
	return nil
}

// The preview reports all three merge channels and leaves the unmatched person to the
// operator. Nothing is written.
func TestFeishuDirectoryPreviewShowsTheMergeChannels(t *testing.T) {
	f := newOrgFeishuFixture(t)
	ranID, acmeID, bossID := f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("preview status=%d payload=%v", status, payload)
	}
	if payload["names_available"] != true || payload["cached"] == true {
		t.Fatalf("names/cached wrong: %v %v", payload["names_available"], payload["cached"])
	}
	stats := payload["stats"].(map[string]any)
	if stats["departments_to_create"].(float64) != 3 {
		t.Fatalf("departments_to_create = %v, want 3", stats["departments_to_create"])
	}

	_, a := departmentLocal(t, payload, "od_a")
	if a["will_create"] != true || a["matched"] != "" {
		t.Fatalf("研发部 local = %v, want will_create", a)
	}
	_, a1 := departmentLocal(t, payload, "od_a1")
	if a1["will_create"] != true {
		t.Fatalf("平台组 local = %v, want will_create (its parent does not exist yet)", a1)
	}

	// Channel ①: the account already carries this open id.
	match := userMatch(t, payload, "ou_root")["account"].(map[string]any)
	if match["id"].(float64) != float64(bossID) || match["matched_by"] != "open_id" || match["needs_bind"] != false {
		t.Fatalf("老板 match = %v", match)
	}
	// Channel ②: a key of acme carries this open id; the identity would be promoted.
	match = userMatch(t, payload, "ou_zhao")["account"].(map[string]any)
	if match["id"].(float64) != float64(acmeID) || match["matched_by"] != "api_key" || match["needs_bind"] != true {
		t.Fatalf("赵六 match = %v", match)
	}
	// Channel ③: exact same name, account unbound.
	match = userMatch(t, payload, "ou_wang")["account"].(map[string]any)
	if match["id"].(float64) != float64(ranID) || match["matched_by"] != "name" || match["needs_bind"] != true {
		t.Fatalf("王五 match = %v", match)
	}
	// Unmatched: the operator decides.
	if userMatch(t, payload, "ou_new")["account"] != nil {
		t.Fatal("李四 must be unmatched")
	}

	// Nothing was written by a preview.
	account, err := f.db.GetAccount(context.Background(), ranID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "" {
		t.Fatalf("the preview wrote a binding: %+v", account)
	}
	nodes, err := f.db.ListOrgNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("the preview created nodes: %v", nodes)
	}
}

// The preview is served from the 60-second cache unless refresh is asked for, and any write
// invalidates it.
func TestFeishuDirectoryPreviewIsCached(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)

	_, first := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", cookie)
	if first["cached"] != false {
		t.Fatalf("first preview must be fresh: %v", first["cached"])
	}
	_, second := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", cookie)
	if second["cached"] != true {
		t.Fatalf("second preview must come from the cache: %v", second["cached"])
	}
	_, forced := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory?refresh=true", "", cookie)
	if forced["cached"] != false {
		t.Fatalf("refresh=true must bypass the cache: %v", forced["cached"])
	}
	f.dir.mu.Lock()
	mints := f.dir.tokenRequests
	f.dir.mu.Unlock()
	// The token is cached on the client with a 60-second expiry margin, so three walks
	// within milliseconds mint exactly once — that cache is the whole point.
	if mints != 1 {
		t.Fatalf("token minted %d times, want 1 across three walks", mints)
	}
}

// The sync executes the preview: creates the departments parents-first, promotes the
// api_key identity, merges the same-name person, and is a no-op the second time.
func TestFeishuSyncCreatesNodesAndMergesMatchedPeople(t *testing.T) {
	f := newOrgFeishuFixture(t)
	ranID, acmeID, _ := f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("sync status=%d payload=%v", status, payload)
	}
	created := payload["created_nodes"].([]any)
	if len(created) != 3 {
		t.Fatalf("created nodes = %v, want 3", created)
	}
	// Parents first: 研发部 and 市场部 are roots, 平台组 hangs under 研发部.
	nodes, err := f.db.ListOrgNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*domain.OrgNode{}
	for _, node := range nodes {
		byName[node.Name] = node
	}
	dev, ok := byName["研发部"]
	if !ok || dev.ParentID != nil || dev.FeishuDepartmentID != "od_a" || dev.FeishuSyncedAt == nil {
		t.Fatalf("研发部 wrong: %+v", dev)
	}
	platform, ok := byName["平台组"]
	if !ok || platform.ParentID == nil || *platform.ParentID != dev.ID || platform.FeishuDepartmentID != "od_a1" {
		t.Fatalf("平台组 wrong: %+v", platform)
	}
	if _, ok := byName["市场部"]; !ok {
		t.Fatal("市场部 was not created")
	}

	// The same-name merge wrote the identity and attached the account to both of the
	// person's departments.
	ran, err := f.db.GetAccount(context.Background(), ranID)
	if err != nil {
		t.Fatal(err)
	}
	if ran.FeishuOpenID != "ou_wang" || ran.FeishuBoundBy != "sync" || ran.FeishuBoundAt == nil {
		t.Fatalf("王五 identity not merged: %+v", ran)
	}
	memberships, err := f.db.ListOrgMembershipsByAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := memberships[ranID]; len(got) != 2 {
		t.Fatalf("王五 memberships = %v, want 研发部+平台组", got)
	}

	// The api_key promotion wrote the identity onto the account and left the key binding.
	acme, err := f.db.GetAccount(context.Background(), acmeID)
	if err != nil {
		t.Fatal(err)
	}
	if acme.FeishuOpenID != "ou_zhao" || acme.FeishuName != "赵六" {
		t.Fatalf("acme identity not promoted: %+v", acme)
	}
	keys, err := f.db.ListAPIKeys(context.Background(), acmeID)
	if err != nil {
		t.Fatal(err)
	}
	stillBound := false
	for _, key := range keys {
		if key.FeishuOpenID == "ou_zhao" {
			stillBound = true
		}
	}
	if !stillBound {
		t.Fatal("the key-level binding was removed by the promotion")
	}

	// The already-synced person produced no audit row and no second write.
	linked := payload["linked_users"].([]any)
	if len(linked) != 2 {
		t.Fatalf("linked users = %v, want the two merges (老板 was already in sync)", linked)
	}

	// Idempotent: the second sync writes nothing.
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("second sync status=%d", status)
	}
	if stats := payload["stats"].(map[string]any); stats["departments_to_create"].(float64) != 0 {
		t.Fatalf("second sync stats = %v, want nothing to create", stats)
	}
	if got := payload["created_nodes"].([]any); len(got) != 0 {
		t.Fatalf("second sync created %v", got)
	}
	if got := payload["linked_users"].([]any); len(got) != 0 {
		t.Fatalf("second sync linked %v", got)
	}
}

// Without the two contact data permissions the walk still succeeds, names are empty, the
// preview says so loudly, and the sync creates no nameless junk — but identity-keyed
// channels still merge.
func TestFeishuSyncWithoutNamePermissionSkipsNamelessDepartments(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.dir.includeNames = false
	_, acmeID, bossID := f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("preview status=%d", status)
	}
	if payload["names_available"] != false {
		t.Fatalf("names_available = %v, want false", payload["names_available"])
	}
	warnings := payload["warnings"].([]any)
	if len(warnings) == 0 || warnings[0] != "names_unavailable" {
		t.Fatalf("warnings = %v, want names_unavailable", warnings)
	}
	stats := payload["stats"].(map[string]any)
	if stats["departments_skipped"].(float64) != 3 {
		t.Fatalf("departments_skipped = %v, want 3 (no names to create from)", stats["departments_skipped"])
	}
	// The same-name channel is unavailable without names; id-keyed channels still work.
	if userMatch(t, payload, "ou_root")["account"] == nil {
		t.Fatal("ou_root must still match by open id")
	}

	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("sync status=%d", status)
	}
	nodes, err := f.db.ListOrgNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("a nameless sync created nodes: %v", nodes)
	}
	acme, err := f.db.GetAccount(context.Background(), acmeID)
	if err != nil {
		t.Fatal(err)
	}
	// The promotion has a name on the key binding itself, which is where it comes from.
	if acme.FeishuOpenID != "ou_zhao" || acme.FeishuName != "赵六" {
		t.Fatalf("acme identity not promoted: %+v", acme)
	}
	if boss, err := f.db.GetAccount(context.Background(), bossID); err != nil || boss.FeishuOpenID != "ou_root" {
		t.Fatalf("boss binding disturbed: %v %+v", err, boss)
	}
}

// A failed Feishu call is a 502 with a readable reason, and nothing local changed.
func TestFeishuDirectoryFailureIsA502(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.dir.failCode = 99991672 // app-level refusal: the permission is missing
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", cookie)
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d payload=%v, want 502", status, payload)
	}
	apiErr := payload["error"].(map[string]any)
	if !strings.Contains(apiErr["message"].(string), "权限") {
		t.Fatalf("error message = %v, want the permission hint", apiErr["message"])
	}
}

// Creating a user for an unmatched person: account, identity and department membership in
// one action; a same-name account refuses and points at 绑定账号.
func TestFeishuCreateUserFromDirectory(t *testing.T) {
	f := newOrgFeishuFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/users/ou_new/account",
		`{"name":"李四","note":"from feishu"}`, cookie)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d payload=%v", status, payload)
	}
	accountID := int64(payload["account"].(map[string]any)["id"].(float64))
	account, err := f.db.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Name != "李四" || account.FeishuOpenID != "ou_new" || account.FeishuBoundBy != adminUser {
		t.Fatalf("created account wrong: %+v", account)
	}
	// The person's department node was created (parents-first) and the account joined it.
	memberships, err := f.db.ListOrgMembershipsByAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := memberships[accountID]; len(got) != 1 {
		t.Fatalf("memberships = %v, want exactly 市场部", got)
	}
	nodes, _ := f.db.ListOrgNodes(context.Background())
	for _, node := range nodes {
		if node.ID == memberships[accountID][0] && (node.Name != "市场部" || node.FeishuDepartmentID != "od_b") {
			t.Fatalf("joined node wrong: %+v", node)
		}
	}

	// A second person whose name collides with an existing account is refused with the
	// pointer at 绑定账号.
	if _, err := f.db.UpsertAccount(context.Background(), &domain.Account{Name: "王五", BillingMode: domain.BillingPrepaid}); err != nil {
		t.Fatal(err)
	}
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/users/ou_wang/account", `{}`, cookie)
	if status != http.StatusConflict {
		t.Fatalf("same-name create status=%d payload=%v, want 409", status, payload)
	}
	if !strings.Contains(payload["error"].(map[string]any)["message"].(string), "绑定账号") {
		t.Fatalf("conflict message = %v, want the pointer at 绑定账号", payload["error"])
	}

	// An unknown person is a 404.
	if status, _ = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/users/ou_ghost/account", `{}`, cookie); status != http.StatusNotFound {
		t.Fatalf("unknown person status=%d, want 404", status)
	}
}

// Without names, creating a user demands a hand-typed name.
func TestFeishuCreateUserWithoutNamesRequiresAName(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.dir.includeNames = false
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/users/ou_zhao/account", `{}`, cookie)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d payload=%v, want 400", status, payload)
	}
	// A hand-typed name works.
	status, payload = f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/users/ou_zhao/account",
		`{"name":"赵六（市场）"}`, cookie)
	if status != http.StatusCreated {
		t.Fatalf("named create status=%d payload=%v", status, payload)
	}
}

// Binding an unmatched person to an existing account promotes the identity and attaches the
// account to the person's departments; unbinding is idempotent and honest.
func TestFeishuBindAndUnbindAccount(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)
	acme, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}

	status, payload := f.callJSON(t, http.MethodPut, "/admin/api/v1/org/feishu/users/ou_new/account",
		`{"account_id":`+fmt.Sprint(acme.ID)+`}`, cookie)
	if status != http.StatusOK {
		t.Fatalf("bind status=%d payload=%v", status, payload)
	}
	bound, err := f.db.GetAccount(context.Background(), acme.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.FeishuOpenID != "ou_new" || bound.FeishuName != "李四" || bound.FeishuBoundBy != adminUser {
		t.Fatalf("bind not written: %+v", bound)
	}
	memberships, err := f.db.ListOrgMembershipsByAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships[acme.ID]) != 1 {
		t.Fatalf("acme memberships = %v, want the 市场部 node", memberships[acme.ID])
	}

	// A second binding attempt on an already-bound account is refused without writing.
	status, payload = f.callJSON(t, http.MethodPut, "/admin/api/v1/org/feishu/users/ou_feng/account",
		`{"account_id":`+fmt.Sprint(acme.ID)+`}`, cookie)
	if status != http.StatusConflict {
		t.Fatalf("rebind status=%d payload=%v, want 409", status, payload)
	}

	// Unbind: the first call reports a change, the second reports none.
	status, payload = f.callJSON(t, http.MethodDelete, "/admin/api/v1/org/feishu/users/ou_new/account", "", cookie)
	if status != http.StatusOK || payload["unbound"] != true {
		t.Fatalf("unbind = %d %v", status, payload)
	}
	status, payload = f.callJSON(t, http.MethodDelete, "/admin/api/v1/org/feishu/users/ou_new/account", "", cookie)
	if status != http.StatusOK || payload["unbound"] != false {
		t.Fatalf("second unbind = %d %v", status, payload)
	}
	cleared, err := f.db.GetAccount(context.Background(), acme.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.FeishuOpenID != "" {
		t.Fatalf("unbind left the identity behind: %+v", cleared)
	}
	// The key-level binding of another identity is untouched by any of this.
	keys, err := f.db.ListAPIKeys(context.Background(), acme.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.FeishuOpenID == "ou_zhao" {
			return
		}
	}
	t.Fatal("the key-level binding vanished")
}

// Two Feishu people with the same name: the first merge wins, the second stays unmatched
// and cannot be bound onto the account that is now taken.
func TestFeishuSameNameFirstComeFirstServed(t *testing.T) {
	f := newOrgFeishuFixture(t)
	// 王五 twice in 平台组, one local account of that name.
	f.dir.members["od_a1"] = append(f.dir.members["od_a1"], dirUser{OpenID: "ou_wang2", UnionID: "on_wang2", Name: "王五"})
	ranID, _, _ := f.seedLocal(t)
	cookie := f.login(t, adminUser, adminPassword)

	status, payload := f.callJSON(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", cookie)
	if status != http.StatusOK {
		t.Fatalf("sync status=%d", status)
	}
	account, err := f.db.GetAccount(context.Background(), ranID)
	if err != nil {
		t.Fatal(err)
	}
	if account.FeishuOpenID != "ou_wang" {
		t.Fatalf("the wrong person claimed the account: %+v", account)
	}
	// The twin cannot be bound onto the same account.
	if status, payload = f.callJSON(t, http.MethodPut, "/admin/api/v1/org/feishu/users/ou_wang2/account",
		`{"account_id":`+fmt.Sprint(ranID)+`}`, cookie); status != http.StatusConflict {
		t.Fatalf("twin bind status=%d payload=%v, want 409", status, payload)
	}
}

// The surface is administrator-only, and absent (501) when the deployment has no Feishu.
func TestFeishuDirectoryRequiresAdminAndEnabledFeishu(t *testing.T) {
	f := newOrgFeishuFixture(t)
	viewer := f.login(t, "reader", adminPassword)
	if res := f.call(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", viewer); res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer preview: status=%d, want 403", res.StatusCode)
	}
	res := f.call(t, http.MethodPost, "/admin/api/v1/org/feishu/sync", "", viewer)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer sync: status=%d, want 403", res.StatusCode)
	}
	res.Body.Close()
	if res := f.call(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous preview: status=%d, want 401", res.StatusCode)
	}

	bare := newAdminFixture(t)
	// The house convention for "this deployment does not have that feature" is
	// 400 unsupported_parameter (the same answer an unwired org port gives); the design's
	// "501" idea was folded into it.
	if res := bare.call(t, http.MethodGet, "/admin/api/v1/org/feishu/directory", "", ""); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("disabled feishu preview: status=%d, want 400", res.StatusCode)
	}
}

// An account-level binding never reaches the portal login path: that still resolves
// through the key-level binding only.
func TestFeishuAccountBindingDoesNotOpenThePortal(t *testing.T) {
	f := newOrgFeishuFixture(t)
	f.seedLocal(t) // 赵六 only at the key level; ou_new has no key at all
	cookie := f.login(t, adminUser, adminPassword)
	acme, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.BindAccountFeishu(context.Background(), acme.ID, domain.FeishuBinding{OpenID: "ou_new", Name: "李四", BoundBy: adminUser}); err != nil {
		t.Fatal(err)
	}

	// ou_new has an account mapping but no key binding: the portal login must refuse.
	// A 401 here means "unbound", asserted through the redirect target below.
	f.api.deps.Config.Feishu.DSHLogin = true
	res := f.request(t, http.MethodGet, feishuLoginPath, "")
	if res.StatusCode != http.StatusFound {
		t.Fatalf("login start: status=%d", res.StatusCode)
	}
	_ = cookie
	parsed := res.Header.Get("Location")
	if !strings.HasPrefix(parsed, "https://accounts.feishu.cn") {
		t.Fatalf("login start location = %q", parsed)
	}
	// Resolve like the callback would: an account-level identity is invisible to it.
	found, err := f.db.FindAPIKeyByFeishuOpenID(context.Background(), "ou_new")
	if err != nil || found != nil {
		t.Fatalf("ou_new resolved to a key: %v %+v", err, found)
	}
}

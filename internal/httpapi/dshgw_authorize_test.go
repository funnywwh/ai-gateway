package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/localdshgw"
	"github.com/winger/ai-gateway/internal/secret"
)

// M52: the dshgw authorize endpoint and the console's per-account dsh toggle share one
// truth (accounts.dsh_enabled). These tests pin the contract dshgw depends on: who is
// admitted, how a disabled account is refused, and why the refusal is distinguishable.

const dshgwTestToken = "sk-gw-m52-token-abcdef123456"

func seedDSHGWKey(t *testing.T, f *adminFixture, accountID int64) {
	t.Helper()
	if _, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
		AccountID: accountID, Name: "dshgw-probe",
		KeyPrefix: secret.Prefix(dshgwTestToken), KeyHash: secret.Hash(dshgwTestToken),
		Status: "active", RecordInputMode: "inherit",
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
}

func authorizeCall(t *testing.T, f *adminFixture, header string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/dshgw/authorize", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil && res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("decode authorize body: %v", err)
	}
	return res.StatusCode, payload
}

func TestDSHGWAuthorizeAllowsOnlyOptedInActiveAccounts(t *testing.T) {
	f := newAdminFixture(t)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	seedDSHGWKey(t, f, account.ID)

	// Default rows start disabled: the gateway must not admit the login.
	status, payload := authorizeCall(t, f, "Bearer "+dshgwTestToken)
	if status != http.StatusForbidden || payload["allowed"] != false || payload["reason"] != "dsh_disabled" {
		t.Fatalf("disabled account: status=%d payload=%v", status, payload)
	}

	// X-API-Key is an accepted alias for the same check (dshgw sends Bearer; the
	// equivalence mirrors the data plane).
	status, _ = authorizeCall(t, f, "")
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/dshgw/authorize", strings.NewReader("{}"))
	req.Header.Set("X-API-Key", dshgwTestToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("x-api-key alias: status=%d", res.StatusCode)
	}

	// The toggle goes through the admin endpoint (the console button's exact path): the
	// handler must invalidate the verifier's account cache, otherwise the authorize check
	// would keep answering from the pre-toggle snapshot for one TTL.
	cookie := f.login(t, adminUser, adminPassword)
	res = f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin enable: status=%d", res.StatusCode)
	}
	status, payload = authorizeCall(t, f, "Bearer "+dshgwTestToken)
	if status != http.StatusOK || payload["allowed"] != true {
		t.Fatalf("enabled account: status=%d payload=%v", status, payload)
	}

	// An unknown key is a plain 401 — the gateway maps it to its own invalid-key page.
	status, _ = authorizeCall(t, f, "Bearer sk-gw-unknown-key-000000000000")
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown key: status=%d", status)
	}

	// A suspended account is refused with reason account_status (403, not 402/503): the
	// gateway must be able to tell "admin paused this account" from "auth is down".
	res = f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID), `{"status":"suspended"}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("suspend account: status=%d", res.StatusCode)
	}
	status, payload = authorizeCall(t, f, "Bearer "+dshgwTestToken)
	if status != http.StatusForbidden || payload["reason"] != "account_status" {
		t.Fatalf("suspended account: status=%d payload=%v", status, payload)
	}
}

func TestAdminAccountDSHFlagEndpoints(t *testing.T) {
	f := newAdminFixture(t)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID)
	cookie := f.login(t, adminUser, adminPassword)

	res := f.call(t, http.MethodGet, path, "", cookie)
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got["enabled"] != false || got["name"] != "acme" {
		t.Fatalf("initial read: %v", got)
	}

	// Enable: provisioning runs once and the enable is audited exactly once. (A repeated
	// enable is a deliberate re-provision — rotate + start — and is covered separately.)
	res = f.call(t, http.MethodPost, path, `{"enabled":true}`, cookie)
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if payload["enabled"] != true || payload["changed"] != true {
		t.Fatalf("enable: %v", payload)
	}
	audit, err := f.db.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	enables := 0
	for _, entry := range audit {
		if entry.Action == "dsh_enable" {
			enables++
		}
	}
	if enables != 1 {
		t.Fatalf("expected exactly one dsh_enable audit entry, got %d", enables)
	}

	// The list contract carries the flag so the console can render the badge.
	res = f.call(t, http.MethodGet, "/admin/api/v1/accounts?limit=10", "", cookie)
	var listed struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(listed.Data) != 1 || listed.Data[0]["dsh_enabled"] != true {
		t.Fatalf("accounts list dsh_enabled: %v", listed.Data)
	}

	// Disabling is what the 停用 DSH button does.
	res = f.call(t, http.MethodPost, path, `{"enabled":false}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable: status=%d", res.StatusCode)
	}

	// A missing body field is a 400, not a silent toggle.
	res = f.call(t, http.MethodPost, path, `{}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body: status=%d", res.StatusCode)
	}

	// A viewer may read but not write.
	viewer := f.login(t, "reader", adminPassword)
	res = f.call(t, http.MethodGet, path, "", viewer)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("viewer read: status=%d", res.StatusCode)
	}
	res = f.call(t, http.MethodPost, path, `{"enabled":true}`, viewer)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer write: status=%d", res.StatusCode)
	}
}

// fakeDshgwAdmin records the provisioning calls without touching a socket. Existing
// tenants start empty unless a test seeds them.
type fakeDshgwAdmin struct {
	Created  []string
	Started  []string
	Stopped  []string
	Keys     []string
	Tenants  []localdshgw.TenantInfo
	FailWith error
}

func (f *fakeDshgwAdmin) CreateTenant(_ context.Context, name, key string) error {
	if f.FailWith != nil {
		return f.FailWith
	}
	f.Created = append(f.Created, name)
	f.Keys = append(f.Keys, key)
	f.Tenants = append(f.Tenants, localdshgw.TenantInfo{Name: name, PublicPort: 32601})
	return nil
}
func (f *fakeDshgwAdmin) StartTenant(_ context.Context, name string) error {
	if f.FailWith != nil {
		return f.FailWith
	}
	f.Started = append(f.Started, name)
	return nil
}
func (f *fakeDshgwAdmin) StopTenant(_ context.Context, name string) error {
	f.Stopped = append(f.Stopped, name)
	return nil
}
func (f *fakeDshgwAdmin) SetTenantKey(_ context.Context, name, key string) error {
	f.Keys = append(f.Keys, key)
	return nil
}
func (f *fakeDshgwAdmin) ListTenants(context.Context) ([]localdshgw.TenantInfo, error) {
	return f.Tenants, nil
}

// Enabling an account provisions a tenant and records the mapping; every key of the
// account then logs into it — including keys created after the enable, which is the whole
// point of the account-level model.
func TestAdminEnableDSHProvisionsTenantAndMapsAccount(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	seedDSHGWKey(t, f, account.ID)

	res := f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true}`, cookie)
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || payload["enabled"] != true {
		t.Fatalf("enable: status=%d payload=%v", res.StatusCode, payload)
	}
	tenant, _ := payload["tenant"].(string)
	if tenant == "" || !strings.HasPrefix(tenant, "dsh-") {
		t.Fatalf("tenant slug expected, got %q", tenant)
	}
	if len(f.dshgwAdmin.Created) != 1 || f.dshgwAdmin.Created[0] != tenant {
		t.Fatalf("provisioned tenants: %v", f.dshgwAdmin.Created)
	}
	if len(f.dshgwAdmin.Keys) != 1 || !strings.HasPrefix(f.dshgwAdmin.Keys[0], "sk-gw") {
		t.Fatalf("worker key must be minted: %v", f.dshgwAdmin.Keys)
	}

	// The authorize answer now carries the tenant: a brand-new key of the same account —
	// one dshgw has never seen — is admitted without any binding step.
	fresh := "sk-gwfreshkey9876543210abcdefgh"
	if _, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
		AccountID: account.ID, Name: "newcomer",
		KeyPrefix: secret.Prefix(fresh), KeyHash: secret.Hash(fresh),
		Status: "active", RecordInputMode: "inherit",
	}); err != nil {
		t.Fatal(err)
	}
	status, body := authorizeCall(t, f, "Bearer "+fresh)
	if status != http.StatusOK || body["allowed"] != true || body["tenant"] != tenant {
		t.Fatalf("authorize after enable: status=%d body=%v", status, body)
	}

	// The enable is audited with the tenant name, never the worker key.
	audit, err := f.db.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range audit {
		if entry.Action == "dsh_enable" && strings.Contains(entry.ChangesJSON, f.dshgwAdmin.Keys[0]) {
			t.Fatal("worker key material leaked into the audit log")
		}
	}

	// Disabling stops the worker, revokes the worker keys and keeps the mapping.
	res = f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":false}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable: status=%d", res.StatusCode)
	}
	if len(f.dshgwAdmin.Stopped) != 1 || f.dshgwAdmin.Stopped[0] != tenant {
		t.Fatalf("worker stop calls: %v", f.dshgwAdmin.Stopped)
	}
	keys, err := f.db.ListAPIKeys(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	revoked := 0
	for _, k := range keys {
		if strings.HasPrefix(k.Name, "dshgw-"+tenant) && k.Status == "active" {
			t.Fatalf("worker key %q stayed active", k.Name)
		}
		if strings.HasPrefix(k.Name, "dshgw-"+tenant) {
			revoked++
		}
	}
	if revoked == 0 {
		t.Fatal("no worker key found to revoke")
	}
	stored, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DSHEnabled {
		t.Fatal("account still enabled after disable")
	}
	if stored.DshTenant != tenant {
		t.Fatalf("disable must keep the mapping for re-enable, got %q", stored.DshTenant)
	}

	// After disable the authorize check is a plain denial again.
	status, body = authorizeCall(t, f, "Bearer "+fresh)
	if status != http.StatusForbidden || body["reason"] != "dsh_disabled" {
		t.Fatalf("authorize after disable: status=%d body=%v", status, body)
	}
}

// Re-enabling reuses the mapping, rotates the worker key and starts the stopped worker
// instead of creating a second tenant.
func TestAdminReEnableDSHRotatesKeyAndStartsWorker(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID)
	res := f.call(t, http.MethodPost, path, `{"enabled":true}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatal("initial enable failed")
	}
	tenant := f.dshgwAdmin.Created[0]
	f.dshgwAdmin.Tenants = []localdshgw.TenantInfo{{Name: tenant}}

	res = f.call(t, http.MethodPost, path, `{"enabled":true}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-enable: status=%d", res.StatusCode)
	}
	if len(f.dshgwAdmin.Created) != 1 {
		t.Fatalf("re-enable must not create a second tenant: %v", f.dshgwAdmin.Created)
	}
	if len(f.dshgwAdmin.Keys) != 2 {
		t.Fatalf("re-enable must rotate the worker key: %v", f.dshgwAdmin.Keys)
	}
	if len(f.dshgwAdmin.Started) != 1 || f.dshgwAdmin.Started[0] != tenant {
		t.Fatalf("re-enable must start the worker: %v", f.dshgwAdmin.Started)
	}
	stored, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DshTenant != tenant {
		t.Fatalf("mapping changed on re-enable: %q", stored.DshTenant)
	}
}

// A provisioning failure must not half-flip the account flag: the console can retry and
// the account stays consistently disabled.
func TestAdminEnableDSHFailureKeepsFlagConsistent(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	f.dshgwAdmin.FailWith = errors.New("dshgw admin channel unavailable")
	res := f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true}`, cookie)
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("failed provisioning must not answer 200")
	}
	stored, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DSHEnabled || stored.DshTenant != "" {
		t.Fatalf("flag/mapping changed on failed enable: %+v", stored)
	}
}

// Enabling a suspended account is refused: the entitlement must not outlive the account.
func TestAdminEnableDSHRefusesSuspendedAccount(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	res := f.call(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/accounts/%d", account.ID), `{"status":"suspended"}`, cookie)
	res.Body.Close()
	res = f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true}`, cookie)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("suspended enable: status=%d", res.StatusCode)
	}
}

// An explicitly requested tenant name is honored when it is well-formed and unclaimed.
func TestAdminEnableDSHHonorsRequestedTenantName(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	res := f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true,"tenant":"research"}`, cookie)
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || payload["tenant"] != "research" {
		t.Fatalf("requested tenant: status=%d payload=%v", res.StatusCode, payload)
	}
	stored, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if stored.DshTenant != "research" {
		t.Fatalf("mapping %q", stored.DshTenant)
	}
}

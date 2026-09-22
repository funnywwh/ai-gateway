package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	dshgwconfig "github.com/winger/ai-gateway/internal/dshgw/config"
)

// The name an account gets when nobody supplies one (M74). The table is the specification: every row
// is a name an operator reads in a URL, in a dialog and in the gateway's own state directory, so a
// change here is a change people notice.
func TestDSHTenantNameForAccount(t *testing.T) {
	cases := []struct {
		label    string
		account  string
		id       int64
		expected string
	}{
		{"chinese name becomes pinyin", "陈景峰", 10, "dsh-chenjingfeng-10"},
		{"another chinese name", "杨妙", 36, "dsh-yangmiao-36"},
		{"ascii name keeps its shape", "acme", 7, "dsh-acme-7"},
		{"mixed name keeps both halves", "李智超(colin)", 8, "dsh-lizhichao-colin-8"},
		{"digits and letters survive", "E26Q", 30, "dsh-e26q-30"},
		{"hyphens survive", "m51-test-a", 98, "dsh-m51-test-a-98"},
		{"the same pinyin, different accounts", "张伟", 41, "dsh-zhangwei-41"},
		{"polyphones take the table's first reading", "长伟", 5, "dsh-zhangwei-5"},
		{"a name with no letters at all", "!!!", 12, "dsh-tenant-12"},
		{"an emoji-only name", "🙂", 13, "dsh-tenant-13"},
		{"surrounding space is one separator", " 王 芳 ", 14, "dsh-wang-fang-14"},
		{"a script outside the covered blocks", "\U00010000\U00010001", 15, "dsh-tenant-15"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := dshTenantNameForAccount(&domain.Account{ID: tc.id, Name: tc.account})
			if got != tc.expected {
				t.Fatalf("dshTenantNameForAccount(%q, %d) = %q, want %q", tc.account, tc.id, got, tc.expected)
			}
		})
	}
}

// A generated name is handed to dshgw as-is, so it has to satisfy the daemon's own grammar — not a
// copy of it that somebody believed was equivalent. This is also what keeps dshTenantNameRE honest:
// if dshgw's expression changes, this test fails on the day it changes.
func TestDSHTenantNameAlwaysSatisfiesDshgwGrammar(t *testing.T) {
	accounts := []*domain.Account{
		{ID: 1, Name: "acme"},
		{ID: 10, Name: "陈景峰"},
		{ID: 415, Name: "张伟"},
		{ID: 12, Name: "!!!"},
		{ID: 16, Name: strings.Repeat("王", 30)},
		{ID: 17, Name: strings.Repeat("x", 200)},
		{ID: math.MaxInt64, Name: "陈景峰"},
		{ID: 18, Name: "李智超(colin) 研发-1"},
		{ID: 19, Name: "Ünïcödé"},
		{ID: 20, Name: ""},
	}
	for _, account := range accounts {
		name := dshTenantNameForAccount(account)
		// Imported in a test file only: the transport must not grow a runtime dependency on the
		// daemon's packages (the layering test enforces that).
		if !dshgwconfig.ValidTenantName(name) {
			t.Errorf("dshgw would refuse %q (account %d, name %q)", name, account.ID, account.Name)
		}
		if !dshTenantNameRE.MatchString(name) {
			t.Errorf("the console's own expression refuses %q (account %d)", name, account.ID)
		}
		if len(name) > dshTenantNameMax {
			t.Errorf("%q is %d characters, over the %d the grammar allows", name, len(name), dshTenantNameMax)
		}
	}
}

// A long name gives way at a syllable boundary — never in the middle of one, which would read as a
// different name — and the id suffix always survives: it is the part that carries uniqueness.
func TestDSHTenantNameTruncatesAtSyllablesAndKeepsTheID(t *testing.T) {
	name := dshTenantNameForAccount(&domain.Account{ID: 42, Name: "欧阳娜娜诸葛小明明"})
	if !strings.HasSuffix(name, "-42") {
		t.Fatalf("the account id must survive truncation: %q", name)
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, "dsh-"), "-42")
	// 欧阳娜娜诸葛小明明, one unit per character: the cut has to land on one of these boundaries, not
	// inside a reading ("ouyangnanazhu" would be a different, wrong name).
	syllables := []string{"ouyang", "nana", "zhuge", "xiao", "ming", "ming"}
	whole := false
	prefix := ""
	for _, syllable := range syllables {
		prefix += syllable
		whole = whole || stem == prefix
	}
	if !whole {
		t.Fatalf("stem %q is not a whole-syllable prefix of ouyangnanazhugexiaomingming", stem)
	}
}

// Every path that needs a name has to agree: the console button (request without a tenant name), the
// automatic enable a Feishu login performs, and the JSON the dialog pre-fills from.
func TestAdminEnableDSHUsesTheDerivedNameAndAdvertisesIt(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	// A Chinese account name is the case that used to collapse into a shared dsh-tenant stem.
	account.Name = "陈景峰"
	if _, err := f.db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	seedDSHGWKey(t, f, account.ID)
	want := fmt.Sprintf("dsh-chenjingfeng-%d", account.ID)

	// The account list advertises the name the dialog should pre-fill.
	row := findAccountRow(t, f, cookie, account.ID)
	if row["dsh_tenant_suggested"] != want {
		t.Fatalf("dsh_tenant_suggested = %v, want %q", row["dsh_tenant_suggested"], want)
	}
	if row["dsh_tenant"] != "" {
		t.Fatalf("the fixture account should have no stored tenant, got %v", row["dsh_tenant"])
	}

	// Enabling with no tenant name in the request takes exactly that name.
	answer := decodeJSONBody(t, f.call(t, http.MethodPost,
		fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true}`, cookie))
	if answer["tenant"] != want {
		t.Fatalf("enable answered tenant=%v, want %q", answer["tenant"], want)
	}
	if len(f.dshgwAdmin.Created) != 1 || f.dshgwAdmin.Created[0] != want {
		t.Fatalf("provisioned tenants = %v, want [%s]", f.dshgwAdmin.Created, want)
	}

	// The automatic enable of a Feishu login runs the same function, so a tenant created with no
	// operator in the loop carries the same kind of name.
	second, err := f.db.UpsertAccount(ctx, &domain.Account{Name: "陈景峰二号", BillingMode: domain.BillingPostpaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	seedDSHGWKey(t, f, second)
	f.api.deps.Feishu = &FeishuDeps{AutoEnableDSH: true}
	outcome := f.api.autoEnableDSHForBinding(ctx, "dshgw-auto", &domain.APIKey{AccountID: second})
	wantSecond := fmt.Sprintf("dsh-chenjingfengerhao-%d", second)
	if outcome.State != "enabled" || outcome.Tenant != wantSecond {
		t.Fatalf("auto enable = %+v, want tenant %q", outcome, wantSecond)
	}
}

func TestDSHOrgMemberRowCarriesTheSuggestedName(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	node := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/org/nodes", `{"name":"研发部"}`, cookie))
	nodeID := int64(node["id"].(float64))
	f.call(t, http.MethodPut, fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", nodeID),
		fmt.Sprintf(`{"account_ids":[%d]}`, account.ID), cookie).Body.Close()

	payload := decodeJSONBody(t, f.call(t, http.MethodGet,
		fmt.Sprintf("/admin/api/v1/org/nodes/%d/accounts", nodeID), "", cookie))
	rows, _ := payload["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("member rows = %v", payload["data"])
	}
	row, _ := rows[0].(map[string]any)
	want := fmt.Sprintf("dsh-acme-%d", account.ID)
	if row["dsh_tenant_suggested"] != want {
		t.Fatalf("dsh_tenant_suggested = %v, want %q", row["dsh_tenant_suggested"], want)
	}
}

// A stored mapping still wins over the rule (M74 renames nobody), both in what the dialog pre-fills
// from and in what an enable without a requested name provisions.
func TestDSHTenantSuggestionDoesNotReplaceAStoredMapping(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	// A tenant name from before the rule existed: the pinyin of the operator's own choosing.
	account.DshTenant = "dsh-colin"
	account.DSHEnabled = false
	if _, err := f.db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	seedDSHGWKey(t, f, account.ID)

	row := findAccountRow(t, f, cookie, account.ID)
	if row["dsh_tenant"] != "dsh-colin" {
		t.Fatalf("stored mapping = %v", row["dsh_tenant"])
	}
	if row["dsh_tenant_suggested"] == "dsh-colin" {
		t.Fatalf("the suggestion must be the rule's answer; the console decides what to pre-fill (got %v)",
			row["dsh_tenant_suggested"])
	}

	answer := decodeJSONBody(t, f.call(t, http.MethodPost,
		fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID), `{"enabled":true}`, cookie))
	if answer["tenant"] != "dsh-colin" {
		t.Fatalf("re-enable renamed the tenant: %v", answer["tenant"])
	}
}

// A name ending in a digit is what the rule produces and what dshgw accepts; the console has to
// accept it too, or an operator re-submitting the pre-filled value would be refused by the console
// itself while the daemon was perfectly happy with it.
func TestAdminEnableDSHAcceptsATrailingDigit(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	seedDSHGWKey(t, f, account.ID)

	res := f.call(t, http.MethodPost, fmt.Sprintf("/admin/api/v1/accounts/%d/dsh", account.ID),
		`{"enabled":true,"tenant":"research-2"}`, cookie)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a name ending in a digit must be accepted: status=%d", res.StatusCode)
	}
	if !dshgwconfig.ValidTenantName("research-2") {
		t.Fatal("this test only means something while dshgw accepts such a name")
	}
}

func findAccountRow(t *testing.T, f *adminFixture, cookie string, id int64) map[string]any {
	t.Helper()
	payload := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/accounts", "", cookie))
	rows, _ := payload["data"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if rowID, _ := row["id"].(float64); int64(rowID) == id {
			return row
		}
	}
	t.Fatalf("account %d is not in the list", id)
	return nil
}

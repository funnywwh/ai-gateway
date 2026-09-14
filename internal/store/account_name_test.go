package store

import (
	"context"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

// TestAccountNameUnicodeRoundTrip pins the store side of the rule: an email address or a
// Chinese name is an ordinary account name, and the lookup path resolves exactly what the
// write path stored.
func TestAccountNameUnicodeRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, name := range []string{"ops@example.com", "北京研发", "客户 A 组 🚀"} {
		id, err := db.UpsertAccount(ctx, &domain.Account{Name: name, BillingMode: domain.BillingPostpaid})
		if err != nil {
			t.Fatalf("UpsertAccount(%q): %v", name, err)
		}
		byName, err := db.GetAccountByName(ctx, name)
		if err != nil {
			t.Fatalf("GetAccountByName(%q): %v", name, err)
		}
		if byName.ID != id || byName.Name != name {
			t.Fatalf("round trip %q = %+v (id %d)", name, byName, id)
		}
	}
}

// TestAccountNameLookupNormalizesWhitespace proves both directions of the trim: a padded
// name is stored trimmed, and a padded lookup finds it. Without the second half, an
// operator typing " acme " in a console prompt would get a spurious not-found.
func TestAccountNameLookupNormalizesWhitespace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	id, err := db.UpsertAccount(ctx, &domain.Account{Name: "  ops@example.com\t", BillingMode: domain.BillingPostpaid})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAccountByName(ctx, "\u3000ops@example.com ")
	if err != nil {
		t.Fatalf("padded lookup: %v", err)
	}
	if got.ID != id {
		t.Fatalf("padded lookup id = %d, want %d", got.ID, id)
	}
	if got.Name != "ops@example.com" {
		t.Fatalf("stored name = %q, want the trimmed form", got.Name)
	}
	// The near-duplicate must not exist as its own row.
	if _, err := db.GetAccountByName(ctx, "  ops@example.com"); err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	list, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("accounts = %+v, want a single row", list)
	}
}

// TestAccountNameRejectsBlankAndOverlong walks the store-level guard: anything the API
// refuses must also be refused here, because bootstrap and tests call the DAL directly.
func TestAccountNameRejectsBlankAndOverlong(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"spaces", "\u3000 \t"},
		{"nil name", ""},
	}
	for _, tc := range cases {
		if _, err := db.UpsertAccount(ctx, &domain.Account{Name: tc.in}); err == nil {
			t.Errorf("%s: UpsertAccount(%q) succeeded, want an error", tc.name, tc.in)
		} else if !domain.HasStatus(err, 400) {
			t.Errorf("%s: error = %v, want a 400 invalid request", tc.name, err)
		}
	}
	if _, err := db.UpsertAccount(ctx, nil); err == nil {
		t.Error("UpsertAccount(nil) succeeded, want an error")
	}
	overlong := strings.Repeat("中", domain.MaxAccountNameRunes+1)
	if _, err := db.UpsertAccount(ctx, &domain.Account{Name: overlong}); err == nil {
		t.Error("overlong name accepted, want an error")
	}
	if _, err := db.GetAccountByName(ctx, overlong); err == nil {
		t.Error("overlong lookup accepted, want an error")
	}
	if _, err := db.GetAccountByName(ctx, "   "); err == nil {
		t.Error("blank lookup accepted, want an error")
	}
}

// TestBootstrapSeedsUnicodeAccountNames covers the configuration-file path: a YAML name
// may be an email address or Chinese, and its whitespace is trimmed so api_keys resolves
// the same account the console shows.
func TestBootstrapSeedsUnicodeAccountNames(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	stateDir := t.TempDir()

	cfg := config.Bootstrap{
		Mode: "upsert",
		Accounts: []config.BootstrapAccount{
			{Name: "ops@example.com", BillingMode: "postpaid"},
			{Name: "  北京研发  ", BillingMode: "prepaid"},
		},
		APIKeys: []config.BootstrapAPIKey{
			{Name: "dev", Key: "sk-gw-unicode-account-key", Account: "北京研发"},
		},
	}
	res, err := db.Bootstrap(ctx, cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountsCreated != 2 {
		t.Fatalf("accounts created = %d, want 2", res.AccountsCreated)
	}
	cn, err := db.GetAccountByName(ctx, "北京研发")
	if err != nil {
		t.Fatal(err)
	}
	if cn.Name != "北京研发" {
		t.Fatalf("stored chinese name = %q, want the trimmed form", cn.Name)
	}
	if _, err := db.GetAccountByName(ctx, "ops@example.com"); err != nil {
		t.Fatal(err)
	}
	// The key must have landed on the Chinese account rather than failing to resolve it.
	keys, err := db.ListAPIKeys(ctx, cn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "dev" {
		t.Fatalf("api keys for the account = %+v, want one dev key", keys)
	}

	// A second run must find the same rows rather than creating near-duplicates, and
	// merge mode must be able to update them through the same trimmed name.
	cfg.Mode = "merge"
	res, err = db.Bootstrap(ctx, cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountsCreated != 0 {
		t.Fatalf("second bootstrap created %d accounts, want 0 (names must match what was seeded)", res.AccountsCreated)
	}
	if res.AccountsUpdated != 2 {
		t.Fatalf("second bootstrap updated %d accounts, want 2", res.AccountsUpdated)
	}
	list, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("accounts after two bootstraps = %d, want 2", len(list))
	}
}

// TestBootstrapNamesDifferingOnlyByPaddingShareOneAccount pins the duplicate rule: trimming
// is a normalization, not a new identity, so two YAML entries that differ only by surrounding
// whitespace are one account — the second must not create a near-duplicate row.
func TestBootstrapNamesDifferingOnlyByPaddingShareOneAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	stateDir := t.TempDir()

	cfg := config.Bootstrap{
		Mode: "upsert",
		Accounts: []config.BootstrapAccount{
			{Name: "运维组", BillingMode: "postpaid", CreditLimitUSD: 10},
			{Name: "  运维组  ", BillingMode: "postpaid", CreditLimitUSD: 999},
		},
	}
	res, err := db.Bootstrap(ctx, cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountsCreated != 1 {
		t.Fatalf("accounts created = %d, want 1 (padded duplicate is the same account)", res.AccountsCreated)
	}
	list, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("accounts = %d rows (%+v), want 1", len(list), list)
	}
	// upsert mode never overwrites live data, so the first entry's credit limit stands.
	if list[0].CreditLimitMicros != 10_000_000 {
		t.Fatalf("credit limit = %d, want the first entry's 10000000", list[0].CreditLimitMicros)
	}

	// Case and composed/decomposed forms stay distinct: trimming is not case folding and
	// not Unicode normalization.
	cfg = config.Bootstrap{Mode: "upsert", Accounts: []config.BootstrapAccount{
		{Name: "Ops"}, {Name: "ops"}, {Name: "\u00e9quipe"}, {Name: "e\u0301quipe"},
	}}
	if _, err := db.Bootstrap(ctx, cfg, stateDir); err != nil {
		t.Fatal(err)
	}
	list, err = db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("accounts = %d rows, want 5 (case and combining marks are distinct identities)", len(list))
	}
}

// TestUpsertAccountPaddedNameIsTheSameAccount pins the DAL duplicate rule without going
// through bootstrap: two upserts of the same name, one padded, are one row with one id, so
// a caller that re-reads the id it was handed cannot end up on a second account.
func TestUpsertAccountPaddedNameIsTheSameAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	firstID, err := db.UpsertAccount(ctx, &domain.Account{Name: "运维@example.com", BillingMode: domain.BillingPostpaid})
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := db.UpsertAccount(ctx, &domain.Account{
		Name: "  运维@example.com  ", BillingMode: domain.BillingPostpaid, CreditLimitMicros: 5_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondID != firstID {
		t.Fatalf("padded upsert id = %d, want the existing %d", secondID, firstID)
	}
	list, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("accounts = %d rows, want 1", len(list))
	}
	if list[0].Name != "运维@example.com" || list[0].CreditLimitMicros != 5_000_000 {
		t.Fatalf("stored account = %+v, want the trimmed name with the updated limit", list[0])
	}
}

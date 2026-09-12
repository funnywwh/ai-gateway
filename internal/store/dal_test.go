package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

func TestAccountRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	acc := &domain.Account{
		Name:              "acme",
		BillingMode:       domain.BillingPrepaid,
		CreditLimitMicros: 5_000_000,
		AutoSuspend:       true,
	}
	id, err := db.UpsertAccount(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "acme" || got.BillingMode != domain.BillingPrepaid {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !got.AutoSuspend {
		t.Error("auto_suspend lost")
	}

	byName, err := db.GetAccountByName(ctx, "acme")
	if err != nil || byName.ID != id {
		t.Fatalf("by-name lookup failed: %v %+v", err, byName)
	}

	// Upsert must not clobber the ledger-owned balance.
	if _, err := db.AppendLedger(ctx, []*domain.LedgerEntry{{
		AccountID: id, Kind: "topup", AmountMicros: 1_000_000, IdemKey: "topup:manual:1",
	}}); err != nil {
		t.Fatal(err)
	}
	acc.CreditLimitMicros = 9_000_000
	if _, err := db.UpsertAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.BalanceMicros != 1_000_000 {
		t.Fatalf("balance must survive upsert, got %d", after.BalanceMicros)
	}
	if after.CreditLimitMicros != 9_000_000 {
		t.Fatalf("credit limit not updated: %d", after.CreditLimitMicros)
	}

	if _, err := db.GetAccount(ctx, 999999); !domain.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestLedgerIsIdempotentAndTracksBalance(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	id, err := db.UpsertAccount(ctx, &domain.Account{Name: "ledger-acc", BillingMode: domain.BillingPostpaid})
	if err != nil {
		t.Fatal(err)
	}

	entries := []*domain.LedgerEntry{
		{AccountID: id, Kind: "credit_grant", AmountMicros: 2_000_000, IdemKey: "grant:1"},
		{AccountID: id, Kind: "charge", AmountMicros: -500_000, IdemKey: "charge:usage:1", RefType: "usage", RefID: "1"},
	}
	if _, err := db.AppendLedger(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if entries[0].BalanceAfterMicros != 2_000_000 || entries[1].BalanceAfterMicros != 1_500_000 {
		t.Fatalf("balance_after wrong: %d / %d", entries[0].BalanceAfterMicros, entries[1].BalanceAfterMicros)
	}

	// Replaying the same entries must not double-charge.
	if _, err := db.AppendLedger(ctx, entries); err != nil {
		t.Fatalf("replay must be a no-op: %v", err)
	}
	balance, err := db.GetBalance(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 1_500_000 {
		t.Fatalf("balance after replay = %d, want 1500000", balance)
	}

	rows, err := db.ListLedger(ctx, id, time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(rows))
	}
}

func TestAPIKeyRoundTripAndRecordingToggles(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	accID, err := db.UpsertAccount(ctx, &domain.Account{Name: "key-acc"})
	if err != nil {
		t.Fatal(err)
	}
	token := "sk-gw-test-token-abcdef"
	key := &domain.APIKey{
		AccountID: accID,
		Name:      "dev",
		KeyPrefix: secret.Prefix(token),
		KeyHash:   secret.Hash(token),
	}
	id, err := db.UpsertAPIKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAPIKeyByPrefix(ctx, secret.Prefix(token))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || !secret.Equal(got.KeyHash, secret.Hash(token)) {
		t.Fatalf("key round trip mismatch: %+v", got)
	}
	// Defaults: thinking and final-output recording are OFF.
	if got.RecordReasoning || got.RecordOutputText {
		t.Fatal("recording switches must default to off")
	}

	if err := db.SetAPIKeyRecording(ctx, id, true, false, "full"); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetAPIKeyByPrefix(ctx, secret.Prefix(token))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.RecordOutputText || updated.RecordReasoning {
		t.Fatalf("toggles not independent: %+v", updated)
	}
	if updated.RecordInputMode != "full" {
		t.Fatalf("input mode = %q", updated.RecordInputMode)
	}

	if _, err := db.GetAPIKeyByPrefix(ctx, "sk-gw-nope"); !domain.IsUnauthorized(err) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestProviderModelRouteGraph(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	provID, err := db.UpsertProvider(ctx, &domain.Provider{
		Name: "openai-main", Kind: "openai-responses", Enabled: true,
		MetaJSON: `{"pool":"main"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: provID, PublicModel: "gpt-x", UpstreamModel: "gpt-x-2026-01-01",
		CapabilitiesJSON: `{"stream":true,"tools":true}`, MaxOutputTokens: 4096,
	}); err != nil {
		t.Fatal(err)
	}
	models, err := db.ListProviderModels(ctx, provID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].UpstreamModel != "gpt-x-2026-01-01" {
		t.Fatalf("provider model mismatch: %+v", models)
	}

	modelID, err := db.UpsertModel(ctx, &domain.Model{PublicName: "gpt-x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	routeID, err := db.UpsertRoute(ctx, &domain.Route{
		ModelID: modelID, ProviderID: provID, UpstreamModel: "gpt-x-2026-01-01", Priority: 10, Weight: 100, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Priority != 10 {
		t.Fatalf("route mismatch: %+v", routes)
	}

	until := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	if err := db.SetRouteCooldown(ctx, routeID, &until); err != nil {
		t.Fatal(err)
	}
	routes, err = db.ListRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].CooldownUntil == nil || !routes[0].CooldownUntil.Equal(until) {
		t.Fatalf("cooldown not persisted: %+v", routes[0].CooldownUntil)
	}

	// Idempotent upsert on the same (model, provider) pair.
	if _, err := db.UpsertRoute(ctx, &domain.Route{
		ModelID: modelID, ProviderID: provID, UpstreamModel: "gpt-x-2026-02-01", Priority: 20, Weight: 50, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	routes, err = db.ListRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].UpstreamModel != "gpt-x-2026-02-01" || routes[0].Priority != 20 {
		t.Fatalf("route upsert failed: %+v", routes)
	}
}

func TestModelMappingsAndTags(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.UpsertModelMapping(ctx, &domain.ModelMapping{
		Kind: "prefix", Pattern: "gpt-*", TargetModel: "gpt-x", Priority: 20, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertModelMapping(ctx, &domain.ModelMapping{
		Kind: "glob", Pattern: "*", TargetModel: "fallback", Priority: 900, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertModelMapping(ctx, &domain.ModelMapping{
		Kind: "wat", Pattern: "x", TargetModel: "y",
	}); err == nil {
		t.Fatal("invalid mapping kind must be rejected")
	}
	if _, err := db.UpsertModelMapping(ctx, &domain.ModelMapping{
		Kind: "exact", Pattern: "no-target",
	}); err == nil {
		t.Fatal("mapping without target must be rejected")
	}

	mappings, err := db.ListModelMappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 || mappings[0].Kind != "prefix" {
		t.Fatalf("mappings order/content wrong: %+v", mappings)
	}

	if _, err := db.UpsertTag(ctx, &domain.Tag{
		Name: "free", GrantsJSON: `{"models":["*"],"providers":["*"]}`, Priority: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertTag(ctx, &domain.Tag{Name: "free", Priority: 20}); err != nil {
		t.Fatal(err)
	}
	tags, err := db.ListTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Priority != 20 {
		t.Fatalf("tag upsert failed: %+v", tags)
	}
}

func TestBootstrapSeedsAccountsAndKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cfg := config.Bootstrap{
		Mode: "upsert",
		Accounts: []config.BootstrapAccount{
			{Name: "internal", BillingMode: "postpaid", CreditLimitUSD: 1000},
		},
		APIKeys: []config.BootstrapAPIKey{
			{Name: "dev", Key: "sk-gw-bootstrap-token", Account: "internal", Tags: []string{"free"}},
		},
	}
	res, err := db.Bootstrap(ctx, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountsCreated != 1 || res.APIKeysCreated != 1 {
		t.Fatalf("bootstrap result: %+v", res)
	}

	// Running again must not duplicate anything.
	res2, err := db.Bootstrap(ctx, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if res2.AccountsCreated != 0 || res2.APIKeysCreated != 0 {
		t.Fatalf("bootstrap not idempotent: %+v", res2)
	}
	keys, err := db.ListAPIKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("api keys = %d, want 1", len(keys))
	}
	if keys[0].KeyHash != secret.Hash("sk-gw-bootstrap-token") {
		t.Fatal("key must be stored hashed")
	}
	if keys[0].TagsJSON != `["free"]` {
		t.Fatalf("tags json = %q", keys[0].TagsJSON)
	}
}

func TestListRequestLogsTreatsZeroAsEveryAccount(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	first, err := db.UpsertAccount(ctx, &domain.Account{Name: "a", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.UpsertAccount(ctx, &domain.Account{Name: "b", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for index, accountID := range []int64{first, second} {
		if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{
			RequestID: "req_log_" + string(rune('a'+index)), AccountID: accountID, APIKeyID: 1,
			Endpoint: "/v1/responses", RequestJSON: `{"input":"hi"}`, Status: "200",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.ListRequestLogs(ctx, domain.RequestLogFilter{AccountID: 0, From: time.Time{}, To: time.Time{}}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("account_id 0 returned %d rows, want every account's 2", len(all))
	}

	one, err := db.ListRequestLogs(ctx, domain.RequestLogFilter{AccountID: first, From: time.Time{}, To: time.Time{}}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].AccountID != first {
		t.Fatalf("account filter returned %+v", one)
	}
}

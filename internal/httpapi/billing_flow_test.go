package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/apikey"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/secret"
	"github.com/winger/ai-gateway/internal/store"
	"github.com/winger/ai-gateway/internal/usage"
)

const billingToken = "sk-gw-billing-path-token-0001"

type billingFixture struct {
	server  *httptest.Server
	db      *store.DB
	service *billing.Service
	account *domain.Account
}

// newBillingFixture wires the request path with billing enabled: the account is a
// prepaid tenant with a starting balance and a priced model.
func newBillingFixture(t *testing.T, balanceMicros int64) *billingFixture {
	t.Helper()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "billing-path.db")
	cfg.Billing.DefaultMarkupBP = 10000
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accountID, err := db.UpsertAccount(ctx, &domain.Account{
		Name: "payer", BillingMode: domain.BillingPrepaid, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if balanceMicros > 0 {
		if err := db.AppendLedger(ctx, []*domain.LedgerEntry{{
			AccountID: accountID, Kind: "topup", AmountMicros: balanceMicros, IdemKey: "topup:test",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}

	key := &domain.APIKey{
		AccountID: accountID, Name: "dev",
		KeyPrefix: secret.Prefix(billingToken), KeyHash: secret.Hash(billingToken),
		Status: "active", RecordInputMode: "inherit",
	}
	if _, err := db.UpsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}

	providerID, err := db.UpsertProvider(ctx, &domain.Provider{
		Name: "echo", Kind: "testecho", Enabled: true, Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: providerID, PublicModel: "priced-echo", UpstreamModel: "priced-echo", Enabled: true,
		MaxOutputTokens: 1024, CapabilitiesJSON: capabilitiesJSON,
		PricingRulesJSON: `{"rules":[{"id":"cost","order":10,"when":{},"rates":{"input":100000,"output":2000000}}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel(ctx, &domain.Model{
		PublicName: "priced-echo", Enabled: true,
		SalePricingJSON: `{"basis":"cost_follow","markup_bp":20000}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRoute(ctx, &domain.Route{
		ModelID: modelID, ProviderID: providerID, UpstreamModel: "priced-echo",
		Priority: 10, Weight: 100, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertTag(ctx, &domain.Tag{Name: "free", GrantsJSON: grantsAll, Priority: 10}); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(db)
	if _, err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	bal := balancer.New(balancer.DefaultConfig())
	router := routing.New(routing.Config{DefaultGrant: "all", Degradation: "strip"}, reg, bal)
	dispatcher := runtime.New(runtime.Config{}, db, reg, nil, bal, nil)
	service := billing.NewService(ctx, db, billing.ServiceConfig{
		Writer:         billing.Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond},
		ReservationTTL: time.Minute,
	}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	srv := New(Deps{
		Config:     &cfg,
		Registry:   reg,
		Router:     router,
		Dispatcher: dispatcher,
		Verifier:   apikey.New(db, apikey.DefaultConfig()),
		Limiter:    quota.New(4),
		Meter:      usage.New(db),
		Records:    db,
		Billing:    service,
		Ledger:     service,
		Version:    "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &billingFixture{server: ts, db: db, service: service, account: account}
}

func (f *billingFixture) call(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+billingToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// waitForSettlement gives the batching writer a moment to commit.
func (f *billingFixture) waitForSettlement(t *testing.T, want int64) billing.Stats {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stats := f.service.Stats()
		if stats.Settled >= want {
			return stats
		}
		time.Sleep(5 * time.Millisecond)
	}
	return f.service.Stats()
}

func TestRequestIsPricedAndCharged(t *testing.T) {
	ctx := context.Background()
	f := newBillingFixture(t, 5_000_000)

	resp := f.call(t, `{"model":"priced-echo","input":"ping"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	stats := f.waitForSettlement(t, 1)
	if stats.Settled != 1 {
		t.Fatalf("settlement did not happen: %+v", stats)
	}

	rows, err := f.db.ListUsageAsc(ctx, f.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	record := rows[0]
	if record.ChargeMicros <= 0 {
		t.Fatalf("charge = %d, want a positive charge (cost %d)", record.ChargeMicros, record.CostMicros)
	}
	if record.ChargeMicros != record.CostMicros*2 {
		t.Fatalf("charge = %d, want twice the cost %d (markup 20000 bp)", record.ChargeMicros, record.CostMicros)
	}
	if record.PricingSnapshot == "" {
		t.Fatal("the usage row must carry the pricing snapshot")
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(record.PricingSnapshot), &snapshot); err != nil {
		t.Fatalf("pricing snapshot is not JSON: %v", err)
	}
	if snapshot["cost_rule"] == nil {
		t.Fatalf("the snapshot must inline the matched cost rule: %v", snapshot)
	}

	balance, err := f.service.Balance(ctx, f.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 5_000_000-record.ChargeMicros {
		t.Fatalf("balance = %d, want %d", balance, 5_000_000-record.ChargeMicros)
	}

	report, err := f.service.Invariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("the billing invariants must hold after a charged request: %+v", report)
	}

	// The reservation must be gone once the request is done.
	if reservations := f.service.Reservations(); len(reservations) != 0 {
		t.Fatalf("reservations left behind: %+v", reservations)
	}
}

func TestInsufficientBalanceIsRejectedBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	f := newBillingFixture(t, 0)

	resp := f.call(t, `{"model":"priced-echo","input":"ping","max_output_tokens":1000}`)
	status, code := decodeError(t, resp)
	if status != http.StatusPaymentRequired {
		t.Fatalf("status = %d code=%s, want 402", status, code)
	}

	// A refused request must leave no usage row: it never reached a provider.
	rows, err := f.db.ListUsageAsc(ctx, f.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a rejected request must not be metered: %+v", rows)
	}
	if reservations := f.service.Reservations(); len(reservations) != 0 {
		t.Fatalf("a rejected request must not hold a reservation: %+v", reservations)
	}
}

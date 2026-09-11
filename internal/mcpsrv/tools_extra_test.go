package mcpsrv

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/store"
)

func newMCPFixture(t *testing.T) (*Service, *store.DB, int64) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "mcp.db")
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: accountID, Name: "dev", KeyPrefix: "sk-gw-mcp", KeyHash: "hash", Status: "active",
		TagsJSON: `["free"]`, PolicyJSON: `{"rate_limit":{"rpm":60,"tpm":100000}}`,
	}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		if _, err := db.SettleAttempt(ctx, &domain.UsageRecord{
			RequestID: "req_mcp_" + string(rune('a'+index)), AttemptNo: 1,
			AccountID: accountID, APIKeyID: 1, Model: "echo", ProviderID: 1,
			DimensionsJSON: `{"input":100,"output":50}`,
			CostMicros:     200, ChargeMicros: 300, Status: "completed",
			UsageSource: "provider", CreatedAt: time.Now().UTC(), TTFTMS: 10 + index,
		}, []*domain.LedgerEntry{{
			AccountID: accountID, Kind: "charge", AmountMicros: -300, IdemKey: "charge:req_mcp_" + string(rune('a'+index)) + ":1",
		}}, []store.UsageCounter{{AccountID: accountID, APIKeyID: 1, Period: CounterPeriod(time.Now()), Requests: 1, Tokens: 150, CostMicros: 200, ChargeMicros: 300}}); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New(db)
	if _, err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	service := New(db, reg, Config{MaxRows: 2, WindowDays: 30, Currency: "USD"})
	service.SetReservationReporter(func(accountID int64) int64 { return 1234 })
	return service, db, accountID
}

func TestDashboardAggregatesIndependentlyOfRowLimits(t *testing.T) {
	service, _, accountID := newMCPFixture(t)
	payload, err := service.Call(context.Background(), accountID, "get_dashboard", map[string]any{"period": "today"})
	if err != nil {
		t.Fatalf("get_dashboard: %v", err)
	}
	data := payload.(map[string]any)
	requests := data["requests"].(map[string]any)
	if requests["attempts"] != int64(5) {
		t.Fatalf("attempts = %v, want 5 even though max_rows is 2", requests["attempts"])
	}
	money := data["money"].(map[string]any)
	if money["charge_micros"] != int64(1500) || money["cost_micros"] != int64(1000) {
		t.Fatalf("money = %v", money)
	}
	if money["margin_micros"] != int64(500) {
		t.Fatalf("margin = %v, want 500", money["margin_micros"])
	}
	balance := data["balance"].(map[string]any)
	if balance["in_flight_micros"] != int64(1234) {
		t.Fatalf("in-flight = %v, want the injected 1234", balance["in_flight_micros"])
	}
	tokens := data["tokens"].(map[string]any)
	if tokens["input"] != int64(500) || tokens["output"] != int64(250) {
		t.Fatalf("tokens = %v", tokens)
	}
	latency := data["latency"].(map[string]any)
	if latency["ttft_p95_ms"] == nil {
		t.Fatalf("latency = %v", latency)
	}
}

func TestUsageBreakdownGroups(t *testing.T) {
	service, _, accountID := newMCPFixture(t)
	payload, err := service.Call(context.Background(), accountID, "get_usage_breakdown", map[string]any{
		"period": "today", "group_by": "day",
	})
	if err != nil {
		t.Fatalf("get_usage_breakdown: %v", err)
	}
	data := payload.(map[string]any)
	groups := data["groups"].([]map[string]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v, want one day", groups)
	}
	if groups[0]["requests"] != int64(5) || groups[0]["charge_micros"] != int64(1500) {
		t.Fatalf("group = %v", groups[0])
	}

	if _, err := service.Call(context.Background(), accountID, "get_usage_breakdown", map[string]any{
		"group_by": "nonsense",
	}); err == nil {
		t.Fatal("an unsupported group_by must fail")
	}
}

func TestRateLimitsAndInvoices(t *testing.T) {
	service, db, accountID := newMCPFixture(t)
	ctx := context.Background()

	payload, err := service.Call(ctx, accountID, "get_rate_limits", nil)
	if err != nil {
		t.Fatalf("get_rate_limits: %v", err)
	}
	keys := payload.(map[string]any)["keys"].([]map[string]any)
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	limits := keys[0]["configured_limits"].(map[string]any)
	if limits["rpm"] != float64(60) {
		t.Fatalf("rpm = %v, want 60 from the key policy", limits["rpm"])
	}
	used := keys[0]["used_this_period"].(map[string]any)
	if used["requests"] != int64(5) {
		t.Fatalf("used requests = %v, want 5", used["requests"])
	}

	// One invoice for this account, one for another: only the first is visible.
	now := time.Now().UTC()
	if _, _, err := db.PutInvoice(ctx, &domain.Invoice{
		AccountID: accountID, PeriodStart: now.Add(-time.Hour), PeriodEnd: now,
		Status: "issued", Currency: "USD", TotalChargeMicros: 1500, TotalCostMicros: 1000,
	}, []domain.InvoiceLine{{GroupType: "model", GroupKey: "echo", Requests: 5, ChargeMicros: 1500}}, false); err != nil {
		t.Fatal(err)
	}
	otherID, err := db.UpsertAccount(ctx, &domain.Account{Name: "other", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PutInvoice(ctx, &domain.Invoice{
		AccountID: otherID, PeriodStart: now.Add(-time.Hour), PeriodEnd: now,
		Status: "draft", Currency: "USD", TotalChargeMicros: 999,
	}, nil, false); err != nil {
		t.Fatal(err)
	}

	listed, err := service.Call(ctx, accountID, "list_invoices", nil)
	if err != nil {
		t.Fatalf("list_invoices: %v", err)
	}
	invoices := listed.(map[string]any)["invoices"].([]map[string]any)
	if len(invoices) != 1 || invoices[0]["total_charge_micros"] != int64(1500) {
		t.Fatalf("invoices = %v, want only this account's", invoices)
	}

	detail, err := service.Call(ctx, accountID, "get_invoice", map[string]any{"id": float64(invoices[0]["id"].(int64))})
	if err != nil {
		t.Fatalf("get_invoice: %v", err)
	}
	lines := detail.(map[string]any)["lines"].([]map[string]any)
	if len(lines) != 1 || lines[0]["group"] != "echo" {
		t.Fatalf("lines = %v", lines)
	}

	// Another account's invoice is reported as missing, not as forbidden.
	if _, err := service.Call(ctx, accountID, "get_invoice", map[string]any{"id": float64(2)}); err == nil {
		t.Fatal("another account's invoice must not be readable")
	}
}

func TestStdioHandleServesTheSameTools(t *testing.T) {
	service, _, accountID := newMCPFixture(t)
	ctx := context.Background()
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	response := service.Handle(ctx, Principal{AccountID: accountID, Scope: ScopeQuery}, raw)
	if response == nil || response.Error != nil {
		t.Fatalf("tools/list failed: %+v", response)
	}
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"get_dashboard", "get_usage_breakdown", "get_rate_limits", "list_invoices", "get_invoice"} {
		if !strings.Contains(string(encoded), name) {
			t.Errorf("tool %s is missing from tools/list", name)
		}
	}
}

// Amounts are micros of the ledger currency, which a deployment may keep in CNY.
// The payload must then say so and must not call the number "usd".
func TestMoneyFieldsFollowTheLedgerCurrency(t *testing.T) {
	ctx := context.Background()
	service, _, accountID := newMCPFixture(t)

	payload, err := service.Call(ctx, accountID, "get_balance", map[string]any{})
	if err != nil {
		t.Fatalf("get_balance: %v", err)
	}
	usd := payload.(map[string]any)
	if usd["balance"] == nil {
		t.Fatalf("payload must carry a currency-neutral balance: %v", usd)
	}
	if _, ok := usd["balance_usd"]; !ok {
		t.Fatalf("a USD ledger keeps the historical alias: %v", usd)
	}

	// The same deployment with a CNY ledger: the amount is a CNY amount, so the
	// dollar-suffixed key would be a lie.
	service.cfg.Currency = "CNY"
	payload, err = service.Call(ctx, accountID, "get_balance", map[string]any{})
	if err != nil {
		t.Fatalf("get_balance (CNY): %v", err)
	}
	cny := payload.(map[string]any)
	if cny["currency"] != "CNY" {
		t.Fatalf("currency = %v, want CNY", cny["currency"])
	}
	if _, ok := cny["balance_usd"]; ok {
		t.Fatalf("a CNY ledger must not report balance_usd: %v", cny)
	}
	if cny["balance"] != usd["balance"] {
		t.Fatalf("the neutral key must carry the same amount: %v vs %v", cny["balance"], usd["balance"])
	}
}

// A model priced in its own currency advertises that currency, not the ledger's.
func TestGetModelsAdvertisesTheModelCurrency(t *testing.T) {
	ctx := context.Background()
	service, db, accountID := newMCPFixture(t)
	if _, err := db.UpsertModel(ctx, &domain.Model{
		PublicName: "cny-echo", Enabled: true,
		SalePricingJSON: `{"currency":"CNY","basis":"absolute","rules":[{"order":1,"when":{},"rates":{"input":1000000}}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	payload, err := service.Call(ctx, accountID, "get_models", map[string]any{})
	if err != nil {
		t.Fatalf("get_models: %v", err)
	}
	models := payload.(map[string]any)["models"].([]map[string]any)
	for _, entry := range models {
		if entry["id"] != "cny-echo" {
			continue
		}
		if entry["currency"] != "CNY" {
			t.Fatalf("cny-echo currency = %v, want CNY", entry["currency"])
		}
		return
	}
	t.Fatalf("cny-echo missing from %v", models)
}

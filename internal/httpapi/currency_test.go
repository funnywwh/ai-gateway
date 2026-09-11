package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
)

// The multi-currency surface has to be checkable in three places: what the console
// is told (currencies, rates, missing rates), what the write APIs refuse (a
// currency the gateway could not convert), and what actually lands in the ledger
// (converted amounts, native amounts kept in the snapshot).

func getJSON(t *testing.T, f *adminFixture, path, cookie string) map[string]any {
	t.Helper()
	resp := f.call(t, http.MethodGet, path, "", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d body=%s", path, resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func currencyCodes(t *testing.T, payload map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := payload["currencies"].([]any)
	for _, entry := range list {
		item, _ := entry.(map[string]any)
		code, _ := item["code"].(string)
		out[code] = item
	}
	return out
}

func stringList(t *testing.T, value any) []string {
	t.Helper()
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

func TestBillingCurrencyEndpointListsRatesAndGaps(t *testing.T) {
	f := newAdminFixture(t)
	f.fx.Replace("USD", map[string]int64{"CNY": 141000})
	cookie := f.login(t, adminUser, adminPassword)

	// One model priced in a currency with a rate, one in a currency without.
	f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"cny-model","sale_pricing":{"currency":"CNY","basis":"absolute","rules":[{"order":1,"when":{},"rates":{"input":1}}]}}`,
		cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"eur-model","sale_pricing":{"currency":"CNY","basis":"absolute","rules":[{"order":1,"when":{},"rates":{"input":1}}]}}`,
		cookie).Body.Close()
	// The EUR provider model cannot be written through the API (no rate), so it is
	// seeded directly: this is the hand-edited-database case the console must flag.
	providerID, err := f.db.UpsertProvider(context.Background(), &domain.Provider{
		Name: "p", Kind: "testecho", Enabled: true, Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertProviderModel(context.Background(), &domain.ProviderModel{
		ProviderID: providerID, PublicModel: "eur-model", UpstreamModel: "eur-model", Enabled: true,
		PricingRulesJSON: `{"currency":"EUR","rules":[{"order":1,"when":{},"rates":{"input":1}}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	payload := getJSON(t, f, "/admin/api/v1/billing/currency", cookie)
	if payload["ledger_currency"] != "USD" || payload["display_currency"] != "USD" {
		t.Fatalf("currencies = %v / %v", payload["ledger_currency"], payload["display_currency"])
	}
	if payload["fx_source"] != "config" {
		t.Fatalf("fx_source = %v, want config", payload["fx_source"])
	}
	currencies := currencyCodes(t, payload)
	if len(currencies) != 2 {
		t.Fatalf("currencies = %v, want USD and CNY", currencies)
	}
	if currencies["USD"]["rate_micros"] != float64(pricing.RateScale) || currencies["USD"]["rate_source"] != "ledger" {
		t.Fatalf("USD entry = %v", currencies["USD"])
	}
	if currencies["CNY"]["rate_micros"] != float64(141000) {
		t.Fatalf("CNY entry = %v", currencies["CNY"])
	}
	if missing := stringList(t, payload["missing_rates"]); len(missing) != 1 || missing[0] != "EUR" {
		t.Fatalf("missing_rates = %v, want [EUR]", missing)
	}
}

func TestModelSaleCurrencyNeedsARate(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	// No rate for CNY yet: the write must be refused with a pointer to the fix.
	resp := f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"cny-model","sale_pricing":{"currency":"CNY","rules":[{"order":1,"when":{},"rates":{"input":100}}]}}`,
		cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "fx_rates") {
		t.Fatalf("the error must say how to fix it: %s", body)
	}

	// A malformed code is refused regardless of the table.
	resp = f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"bad-model","sale_pricing":{"currency":"RMB!","rules":[{"order":1,"when":{},"rates":{"input":100}}]}}`,
		cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed currency", resp.StatusCode)
	}

	// With the rate configured the same write is accepted (and lower case is
	// normalized rather than rejected).
	f.fx.Replace("USD", map[string]int64{"CNY": 141000})
	resp = f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"cny-model","sale_pricing":{"currency":"cny","rules":[{"order":1,"when":{},"rates":{"input":100}}]}}`,
		cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 once the rate exists", resp.StatusCode)
	}
	model, err := f.db.GetModelByName(context.Background(), "cny-model")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.SalePricingJSON, `"currency":"CNY"`) {
		t.Fatalf("stored sale pricing = %s", model.SalePricingJSON)
	}
}

func TestPatchMarkupCarriesTheSaleCurrency(t *testing.T) {
	f := newAdminFixture(t)
	f.fx.Replace("USD", map[string]int64{"CNY": 141000})
	cookie := f.login(t, adminUser, adminPassword)
	f.call(t, http.MethodPost, "/admin/api/v1/models", `{"public_name":"m1"}`, cookie).Body.Close()

	resp := f.call(t, http.MethodPatch, "/admin/api/v1/pricing/markup",
		`{"model":"m1","basis":"cost_follow","markup_bp":15000,"currency":"CNY"}`, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["currency"] != "CNY" {
		t.Fatalf("response currency = %v, want CNY", payload["currency"])
	}
	model, err := f.db.GetModelByName(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.SalePricingJSON, `"currency":"CNY"`) {
		t.Fatalf("stored sale pricing = %s", model.SalePricingJSON)
	}

	// A currency without a rate is refused, and the document is left untouched.
	resp = f.call(t, http.MethodPatch, "/admin/api/v1/pricing/markup",
		`{"model":"m1","markup_bp":15000,"currency":"EUR"}`, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a currency without a rate", resp.StatusCode)
	}
	model, _ = f.db.GetModelByName(context.Background(), "m1")
	if !strings.Contains(model.SalePricingJSON, `"currency":"CNY"`) {
		t.Fatalf("a rejected write must not change the document: %s", model.SalePricingJSON)
	}

	// An empty currency clears it back to "inherit the ledger currency".
	resp = f.call(t, http.MethodPatch, "/admin/api/v1/pricing/markup",
		`{"model":"m1","markup_bp":15000,"currency":""}`, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 when clearing the currency", resp.StatusCode)
	}
	model, _ = f.db.GetModelByName(context.Background(), "m1")
	if strings.Contains(model.SalePricingJSON, "currency") {
		t.Fatalf("cleared sale pricing still declares a currency: %s", model.SalePricingJSON)
	}
}

func TestFXRatesSettingOverridesConfiguration(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	resp := f.call(t, http.MethodPut, "/admin/api/v1/settings/billing.fx_rates",
		`{"value":{"CNY":150000}}`, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if f.fxReloads == 0 {
		t.Fatal("writing the fx rates must reload the live table")
	}
	payload := getJSON(t, f, "/admin/api/v1/billing/currency", cookie)
	if payload["fx_source"] != "settings" {
		t.Fatalf("fx_source = %v, want settings", payload["fx_source"])
	}
	currencies := currencyCodes(t, payload)
	if currencies["CNY"]["rate_micros"] != float64(150000) || currencies["CNY"]["rate_source"] != "settings" {
		t.Fatalf("CNY entry = %v", currencies["CNY"])
	}

	// Decimals are rejected: micros are the contract, and 0.15 would be ambiguous.
	resp = f.call(t, http.MethodPut, "/admin/api/v1/settings/billing.fx_rates", `{"value":{"CNY":0.15}}`, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "integer") {
		t.Fatalf("status = %d body=%s, want 400 explaining micros", resp.StatusCode, body)
	}
	// The ledger currency must not appear in the table.
	resp = f.call(t, http.MethodPut, "/admin/api/v1/settings/billing.fx_rates", `{"value":{"USD":1000000}}`, cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for the ledger currency in the table", resp.StatusCode)
	}
}

func TestPricingTargetsCarryCurrencyInformation(t *testing.T) {
	f := newAdminFixture(t)
	f.fx.Replace("USD", map[string]int64{"CNY": 141000})
	cookie := f.login(t, adminUser, adminPassword)
	f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"cny-model","sale_pricing":{"currency":"CNY","basis":"cost_follow","markup_bp":15000}}`,
		cookie).Body.Close()

	payload := getJSON(t, f, "/admin/api/v1/pricing/targets", cookie)
	if payload["ledger_currency"] != "USD" {
		t.Fatalf("ledger_currency = %v", payload["ledger_currency"])
	}
	var found map[string]any
	targets, _ := payload["targets"].([]any)
	for _, entry := range targets {
		item, _ := entry.(map[string]any)
		if item["model"] == "cny-model" && item["kind"] == "sale" {
			found = item
		}
	}
	if found == nil {
		t.Fatalf("the sale target is missing from %v", targets)
	}
	if found["currency"] != "CNY" || found["currency_source"] != "declared" {
		t.Fatalf("target currency = %v / %v", found["currency"], found["currency_source"])
	}
	if found["fx_rate_known"] != true || found["fx_rate_micros"] != float64(141000) {
		t.Fatalf("target rate = %v / %v", found["fx_rate_micros"], found["fx_rate_known"])
	}
}

func TestSimulateReportsNativeAndLedgerAmounts(t *testing.T) {
	f := newAdminFixture(t)
	f.fx.Replace("USD", map[string]int64{"CNY": 141000})
	cookie := f.login(t, adminUser, adminPassword)
	f.call(t, http.MethodPost, "/admin/api/v1/models", `{"public_name":"sim-model"}`, cookie).Body.Close()

	body := `{"model":"sim-model","dimensions":{"input":1000000},
		"cost_rules":{"currency":"CNY","rules":[{"order":1,"when":{},"rates":{"input":1000000}}]},
		"sale_rules":{"currency":"USD","basis":"cost_follow","markup_bp":15000}}`
	resp := f.call(t, http.MethodPost, "/admin/api/v1/pricing/simulate", body, cookie)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	// 1M tokens at 1e6 micros CNY per million = 1 CNY of cost; at 0.141 USD per CNY
	// that is 141000 micros, and x1.5 -> 211500 micros of the sale currency.
	if payload["cost_currency"] != "CNY" || payload["sale_currency"] != "USD" || payload["ledger_currency"] != "USD" {
		t.Fatalf("currencies = %v/%v/%v", payload["cost_currency"], payload["sale_currency"], payload["ledger_currency"])
	}
	if payload["cost_micros"] != float64(1_000_000) || payload["charge_micros"] != float64(211_500) {
		t.Fatalf("native amounts = %v / %v", payload["cost_micros"], payload["charge_micros"])
	}
	if payload["ledger_cost_micros"] != float64(141_000) || payload["ledger_charge_micros"] != float64(211_500) {
		t.Fatalf("ledger amounts = %v / %v", payload["ledger_cost_micros"], payload["ledger_charge_micros"])
	}
	if payload["fx_cost_sale"] != float64(141_000) {
		t.Fatalf("fx_cost_sale = %v, want 141000", payload["fx_cost_sale"])
	}
}

func TestSalePricingAdvertisesTheModelCurrency(t *testing.T) {
	cfg := config.Default()
	cfg.Billing.Currency = "USD"
	pricingHint := salePricing(`{"currency":"CNY","basis":"absolute","rules":[{"order":1,"when":{},"rates":{"input":1}}]}`, cfg.Billing)
	if pricingHint == nil || pricingHint.Currency != "CNY" {
		t.Fatalf("advertised pricing = %+v, want currency CNY", pricingHint)
	}
	// A model without a currency keeps advertising the ledger currency.
	ledger := salePricing(`{"basis":"cost_follow","markup_bp":12000}`, cfg.Billing)
	if ledger == nil || ledger.Currency != "USD" {
		t.Fatalf("advertised pricing = %+v, want currency USD", ledger)
	}
}

// End to end: a CNY-priced model is charged in the ledger currency, and the
// snapshot keeps both the native amount and the rate that produced the charge.
func TestCrossCurrencyAttemptSettlesInTheLedgerCurrency(t *testing.T) {
	ctx := context.Background()
	cf := newBillingFixtureWith(t, 5_000_000, func(cfg *config.Config, providerModel *domain.ProviderModel, model *domain.Model) {
		cfg.Billing.FXRates = map[string]int64{"CNY": 141000}
		providerModel.PricingRulesJSON = `{"currency":"CNY","rules":[{"id":"cost","order":10,"when":{},"rates":{"input":100000,"output":2000000}}]}`
		model.SalePricingJSON = `{"currency":"CNY","basis":"cost_follow","markup_bp":20000}`
	})

	resp := cf.call(t, `{"model":"priced-echo","input":"ping"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if stats := cf.waitForSettlement(t, 1); stats.Settled != 1 {
		t.Fatalf("settlement did not happen: %+v", stats)
	}
	rows, err := cf.db.ListUsageAsc(ctx, cf.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	row := rows[0]

	var snapshot pricing.Snapshot
	if err := json.Unmarshal([]byte(row.PricingSnapshot), &snapshot); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.SaleCurrency != "CNY" || snapshot.LedgerCurrency != "USD" || snapshot.FXSaleLedger != 141000 {
		t.Fatalf("snapshot currencies = %s/%s rate=%d", snapshot.SaleCurrency, snapshot.LedgerCurrency, snapshot.FXSaleLedger)
	}
	// The charged amount is the CNY total converted into USD, never the raw CNY
	// number: 1 CNY is 0.141 USD, not 1 USD.
	wantCharge := ceilScale(snapshot.ChargeMicrosNative, snapshot.FXSaleLedger)
	if snapshot.ChargeMicrosNative == 0 || row.ChargeMicros != wantCharge {
		t.Fatalf("charge = %d, want ceil(%d x 141000 / 1e6) = %d", row.ChargeMicros, snapshot.ChargeMicrosNative, wantCharge)
	}
	if row.ChargeMicros >= snapshot.ChargeMicrosNative {
		t.Fatalf("the charge was not converted (native %d, ledger %d)", snapshot.ChargeMicrosNative, row.ChargeMicros)
	}
	if snapshot.LedgerChargeMicros != row.ChargeMicros {
		t.Fatalf("snapshot ledger charge = %d, usage = %d", snapshot.LedgerChargeMicros, row.ChargeMicros)
	}

	// The ledger is single-currency: the charge entry equals the converted amount.
	entries, err := cf.db.ListLedger(ctx, cf.account.ID, row.CreatedAt.Add(-time.Minute), row.CreatedAt.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	charge := int64(0)
	for _, entry := range entries {
		if entry.Kind == "charge" {
			charge += -entry.AmountMicros
		}
	}
	if charge != row.ChargeMicros {
		t.Fatalf("ledger charge = %d, usage charge = %d", charge, row.ChargeMicros)
	}
}

// ceilScale mirrors the conversion rounding (up) so the test states the rule rather
// than a magic number.
func ceilScale(value, rate int64) int64 {
	if value <= 0 || rate <= 0 {
		return 0
	}
	product := value * rate
	if product%pricing.RateScale == 0 {
		return product / pricing.RateScale
	}
	return product/pricing.RateScale + 1
}

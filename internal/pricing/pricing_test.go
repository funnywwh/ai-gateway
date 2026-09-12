package pricing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) *RuleSet {
	t.Helper()
	set, err := ParseRuleSet(raw)
	if err != nil {
		t.Fatalf("ParseRuleSet: %v", err)
	}
	return set
}

// ceilMicros is an independent restatement of the documented rounding rule (each
// line rounds up, so non-zero usage is never free). Tests spell it out rather than
// calling the engine's own helper, so a broken rounding rule cannot hide behind it.
func ceilMicros(units, rate int64) int64 {
	if units <= 0 || rate <= 0 {
		return 0
	}
	return (units*rate + 999_999) / 1_000_000
}

const deepseekCost = `{
  "rules": [
    {
      "id": "offpeak", "order": 10,
      "when": {"time_windows": [{"start": "16:30", "end": "00:30", "tz": "UTC"}]},
      "rates": {"input_cache_hit": 35000, "input_cache_miss": 135000, "output": 550000}
    },
    {
      "id": "long-input", "order": 20,
      "when": {"tier": {"basis": "input", "gte": 32768}},
      "rates": {"input_cache_hit": 140000, "input_cache_miss": 540000, "output": 2200000}
    },
    {
      "id": "standard", "order": 100, "when": {},
      "rates": {"input_cache_hit": 70000, "input_cache_miss": 270000, "output": 1100000}
    }
  ]
}`

func TestCatchAllPricesEveryDimension(t *testing.T) {
	set := mustParse(t, deepseekCost)
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) // Monday, standard hours
	result := Evaluate(Input{
		Cost: set, At: at,
		Dimensions: map[string]int64{"input_cache_hit": 2000, "input_cache_miss": 3000, "output": 5000},
	})
	if result.CostRuleID != "standard" {
		t.Fatalf("matched %q, want standard", result.CostRuleID)
	}
	want := int64(2000*70000/1e6 + 3000*270000/1e6 + 5000*1100000/1e6)
	if result.CostMicros != want {
		t.Fatalf("cost = %d, want %d", result.CostMicros, want)
	}
	if len(result.CostLines) != 3 {
		t.Fatalf("cost lines = %d, want 3", len(result.CostLines))
	}
}

func TestOffPeakWindowWinsInsideWindow(t *testing.T) {
	set := mustParse(t, deepseekCost)
	dimensions := map[string]int64{"input_cache_miss": 10000}
	inside := Evaluate(Input{Cost: set, At: time.Date(2026, 3, 2, 17, 0, 0, 0, time.UTC), Dimensions: dimensions})
	if inside.CostRuleID != "offpeak" {
		t.Fatalf("17:00 matched %q, want offpeak", inside.CostRuleID)
	}
	outside := Evaluate(Input{Cost: set, At: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), Dimensions: dimensions})
	if outside.CostRuleID != "standard" {
		t.Fatalf("12:00 matched %q, want standard", outside.CostRuleID)
	}
	// 00:30 is exclusive: the window is half-open [16:30, 00:30).
	edge := Evaluate(Input{Cost: set, At: time.Date(2026, 3, 3, 0, 30, 0, 0, time.UTC), Dimensions: dimensions})
	if edge.CostRuleID != "standard" {
		t.Fatalf("00:30 matched %q, want standard (half-open window)", edge.CostRuleID)
	}
}

func TestTierBoundariesAreHalfOpen(t *testing.T) {
	set := mustParse(t, deepseekCost)
	below := Evaluate(Input{Cost: set, At: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
		Dimensions: map[string]int64{"input_cache_miss": 32767}})
	if below.CostRuleID != "standard" {
		t.Fatalf("32767 matched %q, want standard", below.CostRuleID)
	}
	at := Evaluate(Input{Cost: set, At: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
		Dimensions: map[string]int64{"input_cache_miss": 32768}})
	if at.CostRuleID != "long-input" {
		t.Fatalf("32768 matched %q, want long-input", at.CostRuleID)
	}
}

func TestRoundingRoundsUp(t *testing.T) {
	set := mustParse(t, `{"rules": [{"id": "c", "when": {}, "rates": {"output": 270000}}]}`)
	result := Evaluate(Input{Cost: set, At: time.Now().UTC(), Dimensions: map[string]int64{"output": 1}})
	if result.CostMicros != 1 {
		t.Fatalf("one token at 270000 per million = %d micros, want 1 (rounded up)", result.CostMicros)
	}
	zero := Evaluate(Input{Cost: set, At: time.Now().UTC(), Dimensions: map[string]int64{"output": 0}})
	if zero.CostMicros != 0 {
		t.Fatalf("zero usage charged %d", zero.CostMicros)
	}
}
func TestCrossMidnightAttributesStartWeekday(t *testing.T) {
	set := mustParse(t, `{"rules": [`+
		`{"id": "monday-night", "order": 10, "when": {"time_windows": [{"days": ["mon"], "start": "23:00", "end": "01:00", "tz": "UTC"}]}, "rates": {"output": 1000000}},`+
		`{"id": "catchall", "order": 100, "when": {}, "rates": {"output": 2000000}}]}`)
	tuesdayEarly := time.Date(2026, 3, 3, 0, 30, 0, 0, time.UTC) // tail of Monday's window
	result := Evaluate(Input{Cost: set, At: tuesdayEarly, Dimensions: map[string]int64{"output": 1000}})
	if result.CostRuleID != "monday-night" {
		t.Fatalf("Tuesday 00:30 matched %q, want monday-night", result.CostRuleID)
	}
	wednesdayEarly := time.Date(2026, 3, 4, 0, 30, 0, 0, time.UTC)
	other := Evaluate(Input{Cost: set, At: wednesdayEarly, Dimensions: map[string]int64{"output": 1000}})
	if other.CostRuleID != "catchall" {
		t.Fatalf("Wednesday 00:30 matched %q, want catchall", other.CostRuleID)
	}
}

func TestValidFromAndVariant(t *testing.T) {
	set := mustParse(t, `{"rules": [`+
		`{"id": "promo", "order": 10, "when": {"valid_from": "2026-06-01T00:00:00Z", "valid_to": "2026-07-01T00:00:00Z", "model_variant": "chat-v2"}, "rates": {"output": 100}},`+
		`{"id": "catchall", "order": 100, "when": {}, "rates": {"output": 500}}]}`)
	inside := Evaluate(Input{Cost: set, At: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), Variant: "chat-v2", Dimensions: map[string]int64{"output": 10}})
	if inside.CostRuleID != "promo" {
		t.Fatalf("inside promo matched %q", inside.CostRuleID)
	}
	before := Evaluate(Input{Cost: set, At: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), Variant: "chat-v2", Dimensions: map[string]int64{"output": 10}})
	if before.CostRuleID != "catchall" {
		t.Fatalf("before promo matched %q", before.CostRuleID)
	}
	wrongVariant := Evaluate(Input{Cost: set, At: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), Variant: "chat-v1", Dimensions: map[string]int64{"output": 10}})
	if wrongVariant.CostRuleID != "catchall" {
		t.Fatalf("wrong variant matched %q", wrongVariant.CostRuleID)
	}
}

func TestCostFollowAndAbsoluteSale(t *testing.T) {
	cost := mustParse(t, `{"rules": [{"id": "c", "when": {}, "rates": {"input": 1000000, "output": 2000000}}]}`)
	dimensions := map[string]int64{"input": 1000000, "output": 1000000}
	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

	follow := Evaluate(Input{Cost: cost, At: at, Dimensions: dimensions, MarkupBP: 15000, MarkupSet: true})
	if follow.CostMicros != 3000000 {
		t.Fatalf("cost = %d, want 3000000", follow.CostMicros)
	}
	if follow.ChargeMicros != 4500000 {
		t.Fatalf("charge = %d, want 4500000 (1.5x)", follow.ChargeMicros)
	}

	override := mustParse(t, `{"basis": "cost_follow", "markup_bp": 10000, "dimension_markup_bp": {"output": 30000}}`)
	mixed := Evaluate(Input{Cost: cost, At: at, Dimensions: dimensions, Sale: override})
	if mixed.ChargeMicros != 7000000 {
		t.Fatalf("charge = %d, want 7000000 (input 1x, output 3x)", mixed.ChargeMicros)
	}

	absolute := mustParse(t, `{"basis": "absolute", "rules": [{"id": "flat", "when": {}, "rates": {"input": 500000, "output": 500000}, "per_request_fee_micros": 1000}]}`)
	flat := Evaluate(Input{Cost: cost, At: at, Dimensions: dimensions, Sale: absolute})
	if flat.ChargeMicros != 1001000 {
		t.Fatalf("absolute charge = %d, want 1001000", flat.ChargeMicros)
	}
	if flat.CostMicros != 3000000 {
		t.Fatalf("cost must not depend on the sale side: %d", flat.CostMicros)
	}
}

func TestMinChargeAndUnpricedDimensions(t *testing.T) {
	set := mustParse(t, `{"rules": [{"id": "partial", "when": {}, "rates": {"input": 1000}}]}`)
	result := Evaluate(Input{
		Cost: set, At: time.Now().UTC(), MinChargeMicros: 500,
		Dimensions: map[string]int64{"input": 1, "image": 3},
	})
	if result.ChargeMicros != 500 {
		t.Fatalf("charge = %d, want the 500 minimum", result.ChargeMicros)
	}
	if !result.MinChargeApplied {
		t.Fatal("min charge should be flagged")
	}
	if len(result.UnpricedDimensions) != 1 || result.UnpricedDimensions[0] != "image" {
		t.Fatalf("unpriced = %v, want [image]", result.UnpricedDimensions)
	}
}
func TestValidationRejectsBadRuleSets(t *testing.T) {
	cases := map[string]string{
		"missing catch-all": `{"rules": [{"id": "a", "order": 10, "when": {"model_variant": "x"}, "rates": {"input": 1}}]}`,
		"duplicate order":   `{"rules": [{"id": "a", "order": 10, "when": {}, "rates": {"input": 1}}, {"id": "b", "order": 10, "when": {}, "rates": {"input": 2}}]}`,
		"bad clock":         `{"rules": [{"id": "a", "order": 10, "when": {"time_windows": [{"start": "25:00", "end": "01:00"}]}, "rates": {"input": 1}}, {"id": "c", "order": 100, "when": {}, "rates": {"input": 1}}]}`,
		"equal clock":       `{"rules": [{"id": "a", "order": 10, "when": {"time_windows": [{"start": "10:00", "end": "10:00"}]}, "rates": {"input": 1}}, {"id": "c", "order": 100, "when": {}, "rates": {"input": 1}}]}`,
		"bad tier":          `{"rules": [{"id": "a", "order": 10, "when": {"tier": {"basis": "input", "gte": 100, "lt": 100}}, "rates": {"input": 1}}, {"id": "c", "order": 100, "when": {}, "rates": {"input": 1}}]}`,
		"negative rate":     `{"rules": [{"id": "a", "order": 10, "when": {}, "rates": {"input": -1}}]}`,
		"bad dimension":     `{"rules": [{"id": "a", "order": 10, "when": {}, "rates": {"Input": 1}}]}`,
		"bad timezone":      `{"rules": [{"id": "a", "order": 10, "when": {"time_windows": [{"start": "10:00", "end": "11:00", "tz": "Mars/Olympus"}]}, "rates": {"input": 1}}, {"id": "c", "order": 100, "when": {}, "rates": {"input": 1}}]}`,
		"empty rule":        `{"rules": [{"id": "a", "order": 10, "when": {}}]}`,
		"unknown field":     `{"rules": [{"id": "a", "order": 10, "when": {}, "rates": {"input": 1}, "nope": 2}]}`,
	}
	for name, raw := range cases {
		if _, err := ParseRuleSet(raw); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestShadowingWarnings(t *testing.T) {
	set := mustParse(t, `{"rules": [`+
		`{"id": "catchall", "order": 10, "when": {}, "rates": {"input": 100}},`+
		`{"id": "never", "order": 20, "when": {"model_variant": "x"}, "rates": {"input": 50}}]}`)
	shadows := DetectShadowing(set)
	if len(shadows) != 1 || shadows[0].RuleID != "never" || shadows[0].ShadowedBy != "catchall" {
		t.Fatalf("shadowing = %+v", shadows)
	}
	clean := mustParse(t, `{"rules": [{"id": "a", "order": 10, "when": {"model_variant": "x"}, "rates": {"input": 1}}, {"id": "b", "order": 20, "when": {}, "rates": {"input": 2}}]}`)
	if len(DetectShadowing(clean)) != 0 {
		t.Fatalf("specific-before-general must not be flagged: %+v", DetectShadowing(clean))
	}
}

func TestSnapshotIsReplayable(t *testing.T) {
	set := mustParse(t, deepseekCost)
	sale := mustParse(t, `{"basis": "absolute", "rules": [{"id": "sale", "when": {}, "rates": {"output": 3000000}}]}`)
	at := time.Date(2026, 3, 2, 17, 0, 0, 0, time.UTC)
	result := Evaluate(Input{Cost: set, Sale: sale, At: at,
		Dimensions: map[string]int64{"input_cache_miss": 1000, "output": 2000}})

	raw, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.CostRule == nil || snapshot.CostRule.ID != "offpeak" {
		t.Fatalf("snapshot must inline the matched cost rule: %+v", snapshot.CostRule)
	}
	if snapshot.SaleRule == nil || snapshot.SaleRule.ID != "sale" {
		t.Fatalf("snapshot must inline the matched sale rule: %+v", snapshot.SaleRule)
	}
	if len(snapshot.MatchedWindows) != 1 || !strings.Contains(snapshot.MatchedWindows[0], "16:30-00:30") {
		t.Fatalf("matched windows = %v", snapshot.MatchedWindows)
	}
	// Replaying from the snapshot alone must reproduce the same numbers.
	replay := Evaluate(Input{Cost: &RuleSet{Rules: []Rule{*snapshot.CostRule}}, At: at,
		Dimensions: snapshot.Dimensions})
	if replay.CostMicros != result.CostMicros {
		t.Fatalf("replay cost = %d, want %d", replay.CostMicros, result.CostMicros)
	}
}

// ---------------------------------------------------------------------------
// M22: per-model currencies
// ---------------------------------------------------------------------------

func mustFX(t *testing.T, ledger string, rates map[string]int64) FXTable {
	t.Helper()
	table, err := NewFXTable(ledger, rates)
	if err != nil {
		t.Fatalf("NewFXTable: %v", err)
	}
	return table
}

func TestRuleSetCurrencyIsValidated(t *testing.T) {
	if _, err := ParseRuleSet(`{"currency": "CN-Y", "rules": [{"order": 1, "when": {}, "rates": {"input": 1}}]}`); err == nil {
		t.Fatal("a malformed currency code must be rejected")
	}
	// Lower case is normalized rather than rejected, and the normalized code is
	// what gets stored and later compared.
	set := mustParse(t, `{"currency": "cny", "rules": [{"order": 1, "when": {}, "rates": {"input": 100}}]}`)
	if set.Currency != "CNY" {
		t.Fatalf("currency = %q, want CNY", set.Currency)
	}
}

// A CNY cost table with a USD sale table must convert before applying the markup:
// multiplying a CNY amount by 1.5 and calling it USD would overcharge by ~7x.
func TestCostFollowConvertsCostIntoTheSaleCurrency(t *testing.T) {
	cost := mustParse(t, `{"currency": "CNY", "rules": [{"id": "c", "order": 1, "when": {}, "rates": {"input": 1000000}}]}`)
	sale := mustParse(t, `{"currency": "USD", "basis": "cost_follow", "rules": [{"id": "s", "order": 1, "when": {}, "rates": {"input": 0}}]}`)
	fx := mustFX(t, "USD", map[string]int64{"CNY": 141000})
	result := Evaluate(Input{
		Cost: cost, Sale: sale, At: time.Now().UTC(),
		Dimensions: map[string]int64{"input": 1_000_000},
		MarkupBP:   15000, MarkupSet: true, MarkupSource: "model",
		Ledger: "USD", FX: fx,
	})

	// 1M tokens at 1e6 micros CNY per 1M = 1e6 micros CNY (= 1 CNY) of cost.
	if result.CostMicros != 1_000_000 {
		t.Fatalf("native cost = %d, want 1000000", result.CostMicros)
	}
	// 1 CNY = 141000 micros USD; x1.5 -> 211500 micros, and the sale line repeats it.
	if result.ChargeMicros != 211_500 {
		t.Fatalf("native charge = %d, want 211500", result.ChargeMicros)
	}
	if len(result.SaleLines) != 1 || result.SaleLines[0].Rate != 141_000 {
		t.Fatalf("sale lines = %+v, want one line at the converted rate 141000", result.SaleLines)
	}
	if got := applyMarkup(mulDivCeil(result.SaleLines[0].Units, result.SaleLines[0].Rate, RateScale), 15000); result.SaleLines[0].AmountMicros != got {
		t.Fatalf("sale amount %d is not reproducible from units x rate x markup (%d)", result.SaleLines[0].AmountMicros, got)
	}
	if result.LedgerChargeMicros != 211_500 {
		t.Fatalf("ledger charge = %d, want 211500", result.LedgerChargeMicros)
	}
	if result.FXCostSale != 141_000 || result.FXSaleLedger != RateScale || result.FXCostLedger != 141_000 {
		t.Fatalf("rates = cost->sale %d, cost->ledger %d, sale->ledger %d",
			result.FXCostSale, result.FXCostLedger, result.FXSaleLedger)
	}
	if len(result.FXUnavailable) != 0 {
		t.Fatalf("fx_unavailable = %v, want none", result.FXUnavailable)
	}
}

// Same-currency pricing must stay bit-for-bit identical to the pre-M22 engine.
func TestSameCurrencyPricingIsUnchanged(t *testing.T) {
	cost := mustParse(t, `{"rules": [{"id": "c", "order": 1, "when": {}, "rates": {"input": 270000, "output": 1100000}, "per_request_fee_micros": 7}]}`)
	sale := mustParse(t, `{"basis": "cost_follow", "markup_bp": 15000, "dimension_markup_bp": {"output": 25000}, "rules": [{"id": "s", "order": 1, "when": {}, "rates": {"input": 0}}]}`)
	dimensions := map[string]int64{"input": 33, "output": 7}
	at := time.Now().UTC()

	plain := Evaluate(Input{Cost: cost, Sale: sale, At: at, Dimensions: dimensions})
	withFX := Evaluate(Input{Cost: cost, Sale: sale, At: at, Dimensions: dimensions,
		Ledger: "USD", FX: mustFX(t, "USD", map[string]int64{"CNY": 141000})})

	if plain.CostMicros != withFX.CostMicros || plain.ChargeMicros != withFX.ChargeMicros {
		t.Fatalf("wiring the FX table changed same-currency pricing: %d/%d vs %d/%d",
			plain.CostMicros, plain.ChargeMicros, withFX.CostMicros, withFX.ChargeMicros)
	}
	if withFX.LedgerCostMicros != plain.CostMicros || withFX.LedgerChargeMicros != plain.ChargeMicros {
		t.Fatalf("same-currency ledger amounts = %d/%d, want the native ones %d/%d",
			withFX.LedgerCostMicros, withFX.LedgerChargeMicros, plain.CostMicros, plain.ChargeMicros)
	}
	if withFX.FXSaleLedger != RateScale {
		t.Fatalf("fx_sale_ledger = %d, want %d", withFX.FXSaleLedger, RateScale)
	}
}

// An absolute sale price in CNY is charged in CNY and only converted for the
// ledger, so the customer price never depends on the ledger currency.
func TestAbsoluteSaleInAnotherCurrency(t *testing.T) {
	sale := mustParse(t, `{"currency": "CNY", "basis": "absolute", "rules": [{"id": "s", "order": 1, "when": {}, "rates": {"input": 2000000}}]}`)
	result := Evaluate(Input{
		Sale: sale, At: time.Now().UTC(), Dimensions: map[string]int64{"input": 500_000},
		Ledger: "USD", FX: mustFX(t, "USD", map[string]int64{"CNY": 141000}),
	})
	if result.ChargeMicros != 1_000_000 {
		t.Fatalf("native charge = %d, want 1000000 micros CNY", result.ChargeMicros)
	}
	if result.LedgerChargeMicros != 141_000 {
		t.Fatalf("ledger charge = %d, want 141000 micros USD", result.LedgerChargeMicros)
	}
	if result.SaleCurrency != "CNY" || result.LedgerCurrency != "USD" {
		t.Fatalf("currencies = %q / %q", result.SaleCurrency, result.LedgerCurrency)
	}
}

// A missing rate must never turn into a wrong number: the affected amounts stay
// zero and the currency is reported.
func TestMissingRateIsReportedInsteadOfGuessed(t *testing.T) {
	cost := mustParse(t, `{"currency": "EUR", "rules": [{"id": "c", "order": 1, "when": {}, "rates": {"input": 1000000}}]}`)
	sale := mustParse(t, `{"currency": "USD", "basis": "cost_follow", "rules": [{"id": "s", "order": 1, "when": {}, "rates": {"input": 0}}]}`)
	result := Evaluate(Input{
		Cost: cost, Sale: sale, At: time.Now().UTC(), Dimensions: map[string]int64{"input": 1000},
		MarkupBP: 15000, MarkupSet: true, Ledger: "USD", FX: mustFX(t, "USD", map[string]int64{"CNY": 141000}),
	})
	if result.ChargeMicros != 0 || result.LedgerChargeMicros != 0 {
		t.Fatalf("charge = %d / ledger %d, want 0 and 0", result.ChargeMicros, result.LedgerChargeMicros)
	}
	if len(result.SaleLines) != 0 {
		t.Fatalf("sale lines = %+v, want none when the conversion is impossible", result.SaleLines)
	}
	if result.CostMicros == 0 {
		t.Fatal("the native cost is known even when it cannot be converted")
	}
	if len(result.FXUnavailable) != 1 || result.FXUnavailable[0] != "EUR" {
		t.Fatalf("fx_unavailable = %v, want [EUR]", result.FXUnavailable)
	}
	if result.MinChargeApplied || result.ChargeMicros != 0 {
		t.Fatal("a minimum charge must not apply to a request the gateway could not price")
	}
}

// The snapshot must describe the conversion well enough to replay it after the
// operator changed (or removed) the rate table.
func TestSnapshotRecordsTheRatesUsed(t *testing.T) {
	cost := mustParse(t, `{"currency": "CNY", "rules": [{"id": "c", "order": 1, "when": {}, "rates": {"input": 1000000}}]}`)
	sale := mustParse(t, `{"currency": "USD", "basis": "cost_follow", "markup_bp": 20000, "rules": [{"id": "s", "order": 1, "when": {}, "rates": {"input": 0}}]}`)
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	result := Evaluate(Input{Cost: cost, Sale: sale, At: at, Dimensions: map[string]int64{"input": 1_000_000},
		Ledger: "USD", FX: mustFX(t, "USD", map[string]int64{"CNY": 141000})})

	raw, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.CostCurrency != "CNY" || snapshot.SaleCurrency != "USD" || snapshot.LedgerCurrency != "USD" {
		t.Fatalf("snapshot currencies = %q/%q/%q", snapshot.CostCurrency, snapshot.SaleCurrency, snapshot.LedgerCurrency)
	}
	if snapshot.CostMicrosNative != 1_000_000 || snapshot.ChargeMicrosNative != 282_000 {
		t.Fatalf("native amounts = %d / %d, want 1000000 / 282000",
			snapshot.CostMicrosNative, snapshot.ChargeMicrosNative)
	}
	if snapshot.LedgerChargeMicros != 282_000 || snapshot.FXCostSale != 141_000 {
		t.Fatalf("ledger charge = %d, fx_cost_sale = %d", snapshot.LedgerChargeMicros, snapshot.FXCostSale)
	}

	// Replay from the snapshot alone, with a rate table that no longer knows CNY:
	// the recorded rates are what make the historical number reproducible.
	replay := Evaluate(Input{
		Cost:       &RuleSet{Currency: snapshot.CostCurrency, Rules: []Rule{*snapshot.CostRule}},
		Sale:       sale,
		At:         at,
		Dimensions: snapshot.Dimensions,
		Ledger:     snapshot.LedgerCurrency,
		FX:         mustFX(t, snapshot.LedgerCurrency, map[string]int64{snapshot.CostCurrency: snapshot.FXCostSale}),
	})
	if replay.LedgerChargeMicros != snapshot.LedgerChargeMicros {
		t.Fatalf("replayed ledger charge = %d, want %d", replay.LedgerChargeMicros, snapshot.LedgerChargeMicros)
	}
}

// The minimum charge is a floor in the model's own sale currency, applied before
// the ledger conversion.
func TestMinChargeAppliesInTheSaleCurrency(t *testing.T) {
	sale := mustParse(t, `{"currency": "CNY", "basis": "absolute", "rules": [{"id": "s", "order": 1, "when": {}, "rates": {"input": 1000}}]}`)
	result := Evaluate(Input{
		Sale: sale, At: time.Now().UTC(), Dimensions: map[string]int64{"input": 10},
		MinChargeMicros: 10_000, // 0.01 CNY
		Ledger:          "USD", FX: mustFX(t, "USD", map[string]int64{"CNY": 141000}),
	})
	if !result.MinChargeApplied || result.ChargeMicros != 10_000 {
		t.Fatalf("charge = %d (applied=%v), want the 10000 micro CNY floor", result.ChargeMicros, result.MinChargeApplied)
	}
	if result.LedgerChargeMicros != 1_410 {
		t.Fatalf("ledger charge = %d, want 1410 micros USD", result.LedgerChargeMicros)
	}
}

// A bare `input` dimension is input the upstream did not break down by cache
// status. docs/pricing.md §1 requires it to be priced as a cache miss, so a rule
// set that only names the cache-split dimensions must still bill it. Before this
// was implemented the dimension matched no rate and the entire prompt was charged
// at zero (observed on real traffic as unpriced_dimensions=["input"]).
func TestBareInputFallsBackToTheCacheMissRate(t *testing.T) {
	set := mustParse(t, deepseekCost)
	result := Evaluate(Input{
		Cost: set, At: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		Dimensions: map[string]int64{"input": 13, "output": 5},
	})
	// 13 tokens at the standard miss rate (270000) plus 5 output tokens.
	want := ceilMicros(13, 270000) + ceilMicros(5, 1100000)
	if result.CostMicros != want {
		t.Fatalf("cost = %d, want %d (bare input must not be free)", result.CostMicros, want)
	}
	if len(result.UnpricedDimensions) != 0 {
		t.Fatalf("unpriced = %v, want none: the fallback priced the input", result.UnpricedDimensions)
	}
	if !result.UsageDimensionsIncomplete {
		t.Fatal("usage_dimensions_incomplete = false, want true when the engine buckets input itself")
	}
	if got := strings.Join(result.BucketedDimensions, ","); got != "input->input_cache_miss" {
		t.Fatalf("bucketed = %q, want input->input_cache_miss", got)
	}
	if got := strings.Join(result.Snapshot.BucketedDimensions, ","); got != "input->input_cache_miss" {
		t.Fatalf("snapshot bucketed = %q, want the same (the snapshot must explain the charge)", got)
	}
	if !result.Snapshot.UsageDimensionsIncomplete {
		t.Fatal("snapshot usage_dimensions_incomplete = false, want true")
	}
}

// An explicit rate for the dimension always wins, including an explicit zero, so a
// rule set can keep pricing bare input on its own terms — which is what the codex
// rules do — without the fallback double-counting it.
func TestExplicitRateBeatsTheFallback(t *testing.T) {
	set := mustParse(t, `{"rules": [{"id": "s", "order": 1, "when": {},
		"rates": {"input": 200000, "input_cache_hit": 20000, "input_cache_miss": 200000, "output": 1200000}}]}`)
	result := Evaluate(Input{
		Cost: set, At: time.Now().UTC(),
		Dimensions: map[string]int64{"input": 10, "output": 1},
	})
	want := ceilMicros(10, 200000) + ceilMicros(1, 1200000)
	if result.CostMicros != want {
		t.Fatalf("cost = %d, want %d", result.CostMicros, want)
	}
	if result.UsageDimensionsIncomplete || len(result.BucketedDimensions) != 0 {
		t.Fatalf("explicit rate must not be reported as bucketed: %v", result.BucketedDimensions)
	}

	// An explicit zero is a deliberate "this dimension is free", not a gap.
	free := mustParse(t, `{"rules": [{"id": "s", "order": 1, "when": {},
		"rates": {"input": 0, "input_cache_miss": 500000, "output": 1000000}}]}`)
	priced := Evaluate(Input{
		Cost: free, At: time.Now().UTC(), Dimensions: map[string]int64{"input": 1000},
	})
	if priced.CostMicros != 0 || len(priced.UnpricedDimensions) != 0 {
		t.Fatalf("explicit zero rate: cost = %d unpriced = %v, want 0 with nothing unpriced",
			priced.CostMicros, priced.UnpricedDimensions)
	}
}

// The cache-split dimensions are priced by name and never through the fallback, so
// a request that reports a breakdown is not charged twice.
func TestCacheSplitDimensionsAreNotBucketed(t *testing.T) {
	set := mustParse(t, deepseekCost)
	result := Evaluate(Input{
		Cost: set, At: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		Dimensions: map[string]int64{"input_cache_hit": 2000, "input_cache_miss": 3000, "output": 5000},
	})
	if result.UsageDimensionsIncomplete || len(result.BucketedDimensions) != 0 {
		t.Fatalf("a reported breakdown must not be flagged incomplete: %v", result.BucketedDimensions)
	}
	if len(result.CostLines) != 3 {
		t.Fatalf("cost lines = %d, want 3", len(result.CostLines))
	}
}

// `reasoning` is documented as billed inside `output` unless a rule lists it
// separately. Plugins split reasoning out of output before reporting, so without
// this fallback the reasoning half of the answer was charged at zero.
func TestReasoningFallsBackToTheOutputRate(t *testing.T) {
	set := mustParse(t, deepseekCost)
	result := Evaluate(Input{
		Cost: set, At: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		Dimensions: map[string]int64{"input_cache_miss": 100, "output": 40, "reasoning": 60},
	})
	// The whole 100 output-side tokens are billed at the output rate: 40 by name
	// plus 60 through the fallback.
	want := ceilMicros(100, 270000) + ceilMicros(100, 1100000)
	if result.CostMicros != want {
		t.Fatalf("cost = %d, want %d", result.CostMicros, want)
	}
	if len(result.UnpricedDimensions) != 0 {
		t.Fatalf("unpriced = %v, want none", result.UnpricedDimensions)
	}
	if got := strings.Join(result.BucketedDimensions, ","); got != "reasoning->output" {
		t.Fatalf("bucketed = %q, want reasoning->output", got)
	}

	// A rule that prices reasoning on its own keeps that rate.
	own := mustParse(t, `{"rules": [{"id": "s", "order": 1, "when": {},
		"rates": {"input_cache_miss": 1000000, "output": 2000000, "reasoning": 500000}}]}`)
	separate := Evaluate(Input{
		Cost: own, At: time.Now().UTC(),
		Dimensions: map[string]int64{"output": 10, "reasoning": 10},
	})
	wantSeparate := ceilMicros(10, 2000000) + ceilMicros(10, 500000)
	if separate.CostMicros != wantSeparate {
		t.Fatalf("cost = %d, want %d (an explicit reasoning rate must win)", separate.CostMicros, wantSeparate)
	}
	if len(separate.BucketedDimensions) != 0 {
		t.Fatalf("bucketed = %v, want none when reasoning has its own rate", separate.BucketedDimensions)
	}
}

// The caller can declare the usage incomplete upfront; the flag survives even when
// no dimension needed bucketing.
func TestCallerDeclaredIncompleteUsageIsPreserved(t *testing.T) {
	set := mustParse(t, deepseekCost)
	result := Evaluate(Input{
		Cost: set, At: time.Now().UTC(), UsageDimensionsIncomplete: true,
		Dimensions: map[string]int64{"input_cache_miss": 10},
	})
	if !result.UsageDimensionsIncomplete || !result.Snapshot.UsageDimensionsIncomplete {
		t.Fatal("caller-declared incompleteness was dropped")
	}
}

// A dimension with no rate and no fallback is still reported as unpriced rather
// than silently dropped.
func TestUnknownDimensionStaysUnpriced(t *testing.T) {
	set := mustParse(t, `{"rules": [{"id": "s", "order": 1, "when": {}, "rates": {"output": 1000000}}]}`)
	result := Evaluate(Input{
		Cost: set, At: time.Now().UTC(), Dimensions: map[string]int64{"output": 1, "image": 3},
	})
	if got := strings.Join(result.UnpricedDimensions, ","); got != "image" {
		t.Fatalf("unpriced = %q, want image", got)
	}
	if result.UsageDimensionsIncomplete {
		t.Fatal("an unpriced dimension is not bucketed usage; the flag must stay off")
	}
}

// The reservation estimate asks for the worst rate per dimension by name, so a
// fallback dimension must be present there too: a cache-split rule set names no
// `input`, and a hold that resolved it by name alone reserved nothing for the prompt.
func TestWorstCaseRatesCarryTheFallbackDimensions(t *testing.T) {
	set := mustParse(t, deepseekCost)
	worst := WorstCaseRates(set)
	// The most expensive miss rate across the three rules is the long-input tier.
	if worst["input"] != 540000 {
		t.Fatalf("worst input = %d, want the 540000 cache-miss rate", worst["input"])
	}
	if worst["reasoning"] != worst["output"] {
		t.Fatalf("worst reasoning = %d, want the output rate %d", worst["reasoning"], worst["output"])
	}

	// An explicit rate still wins, so a rule set that prices `input` itself is
	// unchanged by the fallback.
	explicit := mustParse(t, `{"rules": [{"id": "s", "order": 1, "when": {},
		"rates": {"input": 300000, "input_cache_miss": 900000, "output": 1000000}}]}`)
	if got := WorstCaseRates(explicit)["input"]; got != 900000 {
		t.Fatalf("worst input = %d, want the 900000 maximum of the explicit and fallback rates", got)
	}

	// A nil set stays empty rather than inventing rates.
	if got := WorstCaseRates(nil); len(got) != 0 {
		t.Fatalf("worst of nil = %v, want empty", got)
	}
}

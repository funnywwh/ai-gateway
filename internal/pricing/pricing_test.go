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

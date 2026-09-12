package pricing

import "time"

// Snapshot is the self-contained record stored with usage so a historical charge
// can be replayed even after the rules changed or were deleted.
//
// Since M22 it also carries the currencies, the native amounts and the rates used,
// so a historical charge replays identically even after the operator edits the FX
// table.
type Snapshot struct {
	EvaluatedAt               time.Time        `json:"evaluated_at"`
	Variant                   string           `json:"variant,omitempty"`
	Dimensions                map[string]int64 `json:"dimensions"`
	CostRule                  *Rule            `json:"cost_rule,omitempty"`
	SaleRule                  *Rule            `json:"sale_rule,omitempty"`
	SaleBasis                 string           `json:"sale_basis"`
	SaleMarkupBP              int              `json:"sale_markup_bp,omitempty"`
	MarkupSource              string           `json:"markup_source,omitempty"`
	DimensionMarkupBP         map[string]int   `json:"dimension_markup_bp,omitempty"`
	CostLines                 []Line           `json:"cost_lines"`
	SaleLines                 []Line           `json:"sale_lines"`
	MatchedWindows            []string         `json:"matched_windows,omitempty"`
	MatchedTier               string           `json:"matched_tier,omitempty"`
	UnpricedDimensions        []string         `json:"unpriced_dimensions,omitempty"`
	BucketedDimensions        []string         `json:"bucketed_dimensions,omitempty"`
	UsageDimensionsIncomplete bool             `json:"usage_dimensions_incomplete,omitempty"`
	PerRequestFeeScope        string           `json:"per_request_fee_scope,omitempty"`
	MinChargeMicros           int64            `json:"min_charge_micros,omitempty"`
	CostCurrency              string           `json:"cost_currency,omitempty"`
	SaleCurrency              string           `json:"sale_currency,omitempty"`
	LedgerCurrency            string           `json:"ledger_currency,omitempty"`
	CostMicrosNative          int64            `json:"cost_micros_native,omitempty"`
	ChargeMicrosNative        int64            `json:"charge_micros_native,omitempty"`
	LedgerCostMicros          int64            `json:"ledger_cost_micros,omitempty"`
	LedgerChargeMicros        int64            `json:"ledger_charge_micros,omitempty"`
	// FXCostLedger / FXSaleLedger: micros of the ledger currency per whole unit of
	// the cost / sale currency. FXCostSale is the cross rate a cross-currency
	// cost_follow sale used, and is zero when the two sides share a currency.
	FXCostLedger  int64    `json:"fx_cost_ledger,omitempty"`
	FXSaleLedger  int64    `json:"fx_sale_ledger,omitempty"`
	FXCostSale    int64    `json:"fx_cost_sale,omitempty"`
	FXUnavailable []string `json:"fx_unavailable,omitempty"`
}

func buildSnapshot(in Input, dimensions map[string]int64, result *Result, costRule *Rule, sale *RuleSet) Snapshot {
	snapshot := Snapshot{
		EvaluatedAt:               in.At.UTC(),
		Variant:                   in.Variant,
		Dimensions:                dimensions,
		CostRule:                  cloneRule(costRule),
		SaleBasis:                 result.SaleBasis,
		SaleMarkupBP:              result.MarkupBP,
		MarkupSource:              result.MarkupSource,
		CostLines:                 result.CostLines,
		SaleLines:                 result.SaleLines,
		UnpricedDimensions:        result.UnpricedDimensions,
		BucketedDimensions:        result.BucketedDimensions,
		UsageDimensionsIncomplete: result.UsageDimensionsIncomplete,
		PerRequestFeeScope:        in.PerRequestFeeScope,
		MinChargeMicros:           in.MinChargeMicros,
		CostCurrency:              result.CostCurrency,
		SaleCurrency:              result.SaleCurrency,
		LedgerCurrency:            result.LedgerCurrency,
		CostMicrosNative:          result.CostMicros,
		ChargeMicrosNative:        result.ChargeMicros,
		LedgerCostMicros:          result.LedgerCostMicros,
		LedgerChargeMicros:        result.LedgerChargeMicros,
		FXCostLedger:              result.FXCostLedger,
		FXSaleLedger:              result.FXSaleLedger,
		FXCostSale:                result.FXCostSale,
		FXUnavailable:             result.FXUnavailable,
	}
	if sale != nil && sale.Basis == BasisAbsolute {
		matched := matchRule(sale, in.At, dimensions, in.Variant)
		snapshot.SaleRule = cloneRule(matched)
	}
	if sale != nil && len(sale.DimensionMarkupBP) > 0 {
		snapshot.DimensionMarkupBP = sale.DimensionMarkupBP
	}
	snapshot.MatchedWindows = matchedWindowLabels(costRule, in.At)
	snapshot.MatchedTier = tierLabel(costRule)
	return snapshot
}

func cloneRule(rule *Rule) *Rule {
	if rule == nil {
		return nil
	}
	copyRule := *rule
	if rule.Rates != nil {
		copyRule.Rates = make(map[string]int64, len(rule.Rates))
		for dimension, rate := range rule.Rates {
			copyRule.Rates[dimension] = rate
		}
	}
	if rule.When.TimeWindows != nil {
		copyRule.When.TimeWindows = append([]TimeWindow(nil), rule.When.TimeWindows...)
	}
	return &copyRule
}

func matchedWindowLabels(rule *Rule, at time.Time) []string {
	if rule == nil {
		return nil
	}
	labels := []string{}
	for _, window := range rule.When.TimeWindows {
		if matchesTimeWindow(at, window) {
			labels = append(labels, windowLabel(window))
		}
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

func tierLabel(rule *Rule) string {
	if rule == nil || rule.When.Tier == nil {
		return ""
	}
	tier := rule.When.Tier
	label := tier.Basis + ">=" + itoa(tier.Gte)
	if tier.Lt != 0 {
		label += " && <" + itoa(tier.Lt)
	}
	return label
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

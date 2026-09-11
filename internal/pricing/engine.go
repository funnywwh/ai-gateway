package pricing

import (
	"sort"
	"time"
)

// Input is one pricing evaluation. The engine never reads the clock: the caller
// decides which instant applies (request start or completion).
type Input struct {
	// Dimensions are the metered quantities (tokens, images, seconds, ...).
	Dimensions map[string]int64
	// At is the evaluation instant in UTC.
	At time.Time
	// Variant is the upstream model variant, matched against when.model_variant.
	Variant string
	// Cost is the provider-side rule set (what we pay upstream).
	Cost *RuleSet
	// Sale is the customer-side rule set. A nil or basis-less set means
	// "cost plus the default markup".
	Sale *RuleSet
	// MarkupBP overrides the sale mark-up (basis points, 10000 = 1.0x).
	MarkupBP int
	// MarkupSet distinguishes "use 0" from "use the default".
	MarkupSet bool
	// MarkupSource names where the multiplier came from (key|tag|account|model|default)
	// so a charge can be explained after the fact.
	MarkupSource string
	// MinChargeMicros applies to the sale side only.
	MinChargeMicros int64
	// PerRequestFeeScope is "attempt" or "request"; it only affects the snapshot
	// (the caller decides how many attempts to charge).
	PerRequestFeeScope string

	// UsageDimensionsIncomplete marks usage that had to be bucketed by the caller
	// (for example the upstream did not report cache hits).
	UsageDimensionsIncomplete bool
}

// Line is one dimension's contribution.
type Line struct {
	Dimension    string `json:"dimension"`
	Units        int64  `json:"units"`
	Rate         int64  `json:"rate"`
	AmountMicros int64  `json:"amount_micros"`
	// MarkupBP is set on sale lines that were derived from the cost line.
	MarkupBP int `json:"markup_bp,omitempty"`
}

// Result is the priced outcome plus everything needed to reproduce it.
type Result struct {
	CostMicros       int64  `json:"cost_micros"`
	ChargeMicros     int64  `json:"charge_micros"`
	CostRuleID       string `json:"cost_rule_id,omitempty"`
	SaleRuleID       string `json:"sale_rule_id,omitempty"`
	SaleBasis        string `json:"sale_basis,omitempty"`
	MarkupBP         int    `json:"markup_bp,omitempty"`
	MarkupSource     string `json:"markup_source,omitempty"`
	RequestFeeMicros int64  `json:"request_fee_micros,omitempty"`
	MinChargeApplied bool   `json:"min_charge_applied,omitempty"`
	CostLines        []Line `json:"cost_lines"`
	SaleLines        []Line `json:"sale_lines"`
	// UnpricedDimensions lists metered dimensions the matched rule gave no rate
	// for. They are charged at zero, but the caller should log them.
	UnpricedDimensions []string `json:"unpriced_dimensions,omitempty"`
	// Snapshot is the self-contained, replayable record stored with usage.
	Snapshot Snapshot `json:"snapshot"`
}

// Evaluate prices one usage dimension set. It is pure: same input, same output.
func Evaluate(in Input) *Result {
	dimensions := normalizeDimensions(in.Dimensions)
	result := &Result{CostLines: []Line{}, SaleLines: []Line{}}

	costRule := matchRule(in.Cost, in.At, dimensions, in.Variant)
	if costRule != nil {
		result.CostRuleID = costRule.ID
		result.CostLines, result.UnpricedDimensions = priceDimensions(dimensions, costRule.Rates)
		for _, line := range result.CostLines {
			result.CostMicros += line.AmountMicros
		}
		result.CostMicros += costRule.PerRequestFeeMicros
		result.RequestFeeMicros += costRule.PerRequestFeeMicros
	}

	sale := in.Sale
	basis := ""
	if sale != nil {
		basis = sale.Basis
	}
	if basis == "" {
		basis = BasisCostFollow
	}

	switch basis {
	case BasisAbsolute:
		rule := matchRule(sale, in.At, dimensions, in.Variant)
		if rule != nil {
			result.SaleRuleID = rule.ID
			lines, unpriced := priceDimensions(dimensions, rule.Rates)
			result.SaleLines = lines
			result.UnpricedDimensions = mergeDimensions(result.UnpricedDimensions, unpriced)
			for _, line := range lines {
				result.ChargeMicros += line.AmountMicros
			}
			result.ChargeMicros += rule.PerRequestFeeMicros
			result.RequestFeeMicros += rule.PerRequestFeeMicros
		}
	default:
		markupBP := 10000
		if in.MarkupSet {
			markupBP = in.MarkupBP
		} else if sale != nil && sale.MarkupBP > 0 {
			markupBP = sale.MarkupBP
		}
		result.MarkupBP = markupBP
		result.MarkupSource = in.MarkupSource
		if sale != nil {
			result.SaleRuleID = sale.Basis
		}
		for _, line := range result.CostLines {
			effective := markupBP
			if sale != nil {
				if override, ok := sale.DimensionMarkupBP[line.Dimension]; ok {
					effective = override
				}
			}
			amount := applyMarkup(line.AmountMicros, effective)
			result.SaleLines = append(result.SaleLines, Line{
				Dimension: line.Dimension, Units: line.Units, Rate: line.Rate,
				AmountMicros: amount, MarkupBP: effective,
			})
			result.ChargeMicros += amount
		}
		// A per-request fee follows the same mark-up in cost_follow mode.
		if result.RequestFeeMicros > 0 {
			fee := applyMarkup(result.RequestFeeMicros, markupBP)
			result.ChargeMicros += fee - result.RequestFeeMicros
			result.RequestFeeMicros = fee
		}
	}
	result.SaleBasis = basis

	if in.MinChargeMicros > 0 && result.ChargeMicros < in.MinChargeMicros {
		result.ChargeMicros = in.MinChargeMicros
		result.MinChargeApplied = true
	}

	result.Snapshot = buildSnapshot(in, dimensions, result, costRule, sale)
	return result
}

// matchRule returns the first rule (ascending order) whose conditions all hold.
func matchRule(set *RuleSet, at time.Time, dimensions map[string]int64, variant string) *Rule {
	if set == nil || len(set.Rules) == 0 {
		return nil
	}
	rules := append([]Rule(nil), set.Rules...)
	sortRules(rules)
	for index := range rules {
		if ruleMatches(rules[index], at, dimensions, variant) {
			return &rules[index]
		}
	}
	return nil
}

func ruleMatches(rule Rule, at time.Time, dimensions map[string]int64, variant string) bool {
	when := rule.When
	if when.ModelVariant != "" && when.ModelVariant != variant {
		return false
	}
	if when.ValidFrom != nil && at.Before(*when.ValidFrom) {
		return false
	}
	if when.ValidTo != nil && !at.Before(*when.ValidTo) {
		return false
	}
	if when.Tier != nil && !tierMatches(*when.Tier, dimensions) {
		return false
	}
	if len(when.TimeWindows) > 0 {
		matched := false
		for _, window := range when.TimeWindows {
			if matchesTimeWindow(at, window) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func tierMatches(tier Tier, dimensions map[string]int64) bool {
	var value int64
	switch tier.Basis {
	case TierInput:
		value = dimensions["input"] + dimensions["input_cache_hit"] + dimensions["input_cache_miss"]
	case TierOutput:
		value = dimensions["output"] + dimensions["reasoning"]
	default:
		for _, units := range dimensions {
			value += units
		}
	}
	if value < tier.Gte {
		return false
	}
	if tier.Lt != 0 && value >= tier.Lt {
		return false
	}
	return true
}

// priceDimensions multiplies each dimension by its rate and rounds up, so a
// non-zero usage never becomes a free request.
func priceDimensions(dimensions map[string]int64, rates map[string]int64) ([]Line, []string) {
	names := make([]string, 0, len(dimensions))
	for name := range dimensions {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := []Line{}
	unpriced := []string{}
	for _, name := range names {
		units := dimensions[name]
		rate, ok := rates[name]
		if !ok {
			if units > 0 {
				unpriced = append(unpriced, name)
			}
			continue
		}
		lines = append(lines, Line{Dimension: name, Units: units, Rate: rate, AmountMicros: mulDivCeil(units, rate, RateScale)})
	}
	return lines, unpriced
}

// mulDivCeil computes ceil(a*b/scale) in int64 without overflowing for realistic
// inputs (a <= 1e12 units, b <= 1e12 micros per million).
func mulDivCeil(a, b, scale int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	product := a * b
	if product/scale != (product-1)/scale || product%scale == 0 {
		return product / scale
	}
	return product/scale + 1
}

// applyMarkup scales an amount by basis points, rounding up.
func applyMarkup(amount int64, markupBP int) int64 {
	if amount <= 0 || markupBP <= 0 {
		return 0
	}
	return mulDivCeil(amount, int64(markupBP), 10000)
}

func normalizeDimensions(dimensions map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(dimensions))
	for name, units := range dimensions {
		if units > 0 {
			out[name] = units
		}
	}
	return out
}

func mergeDimensions(existing, extra []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, list := range [][]string{existing, extra} {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

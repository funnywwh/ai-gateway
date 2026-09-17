package domain

import (
	"fmt"
	"time"
)

// Provider cost cap (M56): what the gateway may spend on one upstream before the router
// stops choosing it, and when that accumulation restarts. The rules live here, in one pure
// place, because three callers have to agree on them: the background reader that sums the
// metering table, the router that drops the provider, and the console that shows the
// operator the same number the router used. See docs/design/m56-provider-cost-cap.md.
const (
	// CostPeriodNone counts from the last reset (or from the beginning of the records
	// when it was never reset). It is the default: an operator who wants a budget period
	// says so explicitly.
	CostPeriodNone = "none"
	// CostPeriodDaily re-anchors at 00:00 UTC.
	CostPeriodDaily = "daily"
	// CostPeriodMonthly re-anchors on the first day of the month, 00:00 UTC — the same
	// calendar the usage counters ("YYYY-MM") already use.
	CostPeriodMonthly = "monthly"
)

// NormalizeCostPeriod maps the API spelling of a cost period onto the stored one. The empty
// string is accepted as "none" so a create body may omit the field, and anything else is
// rejected rather than silently treated as unlimited: a typo like "montly" would otherwise
// turn a monthly budget into a permanently accumulating one.
func NormalizeCostPeriod(raw string) (string, error) {
	switch raw {
	case "", CostPeriodNone:
		return CostPeriodNone, nil
	case CostPeriodDaily, CostPeriodMonthly:
		return raw, nil
	default:
		return "", fmt.Errorf("cost_period must be none|daily|monthly (got %q)", raw)
	}
}

// ValidCostPeriod reports whether raw is a period the gateway stores and reads back.
func ValidCostPeriod(raw string) bool {
	_, err := NormalizeCostPeriod(raw)
	return err == nil
}

// CostCapped reports whether this provider has a cost cap at all. A provider without one
// costs the reader nothing: no query, no map entry, no field in the API payload.
func (p *Provider) CostCapped() bool {
	return p != nil && p.CostLimitMicros > 0
}

// ProviderCostWindowStart is the instant the provider's cost accumulation counts from:
// max(period start, last manual reset). The zero time means "everything on record", which is
// what a never-reset provider with no period accumulates over.
//
// The period start is computed in UTC on purpose: "reset at the beginning of the month" must
// not depend on where the gateway happens to run.
func ProviderCostWindowStart(p *Provider, now time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	start := time.Time{}
	at := now.UTC()
	switch p.CostPeriod {
	case CostPeriodDaily:
		start = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	case CostPeriodMonthly:
		start = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	// A manual reset after the period start wins: it is the operator saying "count from
	// here". Once the next period starts, the period start overtakes it again on its own,
	// so an early reset does not have to be undone by hand.
	if p.CostWindowStart != nil && p.CostWindowStart.After(start) {
		start = p.CostWindowStart.UTC()
	}
	return start
}

// ProviderCostExceeded reports whether a provider's accumulated cost has reached its cap.
// Comparison is >= : "达到上限" means the budget is spent, and the next attempt would be
// money the operator did not authorize.
func ProviderCostExceeded(p *Provider, usedMicros int64) bool {
	if p == nil || p.CostLimitMicros <= 0 {
		return false
	}
	return usedMicros >= p.CostLimitMicros
}

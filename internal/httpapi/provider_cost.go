package httpapi

import (
	"strconv"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/runtime"
)

// ProviderCost reports the per-provider cost cap state (M56): how much each capped upstream has
// spent in its current window, and whether that is over the configured limit.
//
// It is a narrow port like Capacity and Prober: a nil port omits the per-provider field and the
// /stats block, which is what a deployment without a metering store (and most tests) wants.
type ProviderCost interface {
	Stats() map[int64]runtime.ProviderCostStat
	Status() runtime.CostStatus
	// MarkReset zeroes one provider's reading after the write path moved its window start.
	MarkReset(providerID int64, at time.Time)
}

// costStats reads the readings once, so a provider list can look each row up without taking the
// tracker's lock per row.
func (s *Server) costStats() map[int64]runtime.ProviderCostStat {
	if s.deps.ProviderCost == nil {
		return nil
	}
	return s.deps.ProviderCost.Stats()
}

// costStatus describes the reader itself (interval, freshness, last error).
func (s *Server) costStatus() (runtime.CostStatus, bool) {
	if s.deps.ProviderCost == nil {
		return runtime.CostStatus{}, false
	}
	return s.deps.ProviderCost.Status(), true
}

// costBlock is the /stats (and therefore MCP admin_stats) payload: the reader's own state
// first — "how fresh is this number, and is it readable at all" is what an operator or an agent
// asks before trusting any per-provider figure — then the reading of every capped provider.
//
// It is omitted entirely when no tracker is wired, so the block never claims "nobody is capped"
// on no evidence.
func (s *Server) costBlock() map[string]any {
	if s.deps.ProviderCost == nil {
		return nil
	}
	stats := s.costStats()
	status := s.deps.ProviderCost.Status()
	providers := make(map[string]any, len(stats))
	for id, stat := range stats {
		providers[strconv.FormatInt(id, 10)] = stat
	}
	block := map[string]any{
		"currency":  s.ledgerCurrency(),
		"refresh_s": status.IntervalS,
		"tracked":   status.Tracked,
		"providers": providers,
	}
	// as_of and last_error carry the guard's honesty: the reading can be a few seconds old, and
	// a failed read keeps the last one instead of stopping traffic.
	if !status.AsOf.IsZero() {
		block["as_of"] = status.AsOf.UTC().Format(time.RFC3339)
	}
	if status.LastError != "" {
		block["last_error"] = status.LastError
	}
	return block
}

// costPeriodJSON renders the stored period for the API. The store normalizes an empty value to
// "none", so this only covers a row written outside the gateway — and it matters there: the
// console renders the value into a select whose options are none/daily/monthly, and an empty
// string would silently match none of them.
func costPeriodJSON(period string) string {
	if period == "" {
		return domain.CostPeriodNone
	}
	return period
}

// attachCost adds one provider's live cost reading to its API row, but only when the provider
// actually has a cap: a provider with no limit has nothing to report, and "limit_micros: 0,
// used_micros: 0" would suggest the gateway were counting something for it. The configuration
// itself (cost_limit_micros / cost_period / cost_window_start) is always in the row, because the
// console's edit form has to render it for uncapped providers too.
//
// The used figure is exactly the one the router compared against, so the console can never show
// a number that disagrees with the routing decision.
func attachCost(row map[string]any, p *domain.Provider, stats map[int64]runtime.ProviderCostStat, currency string) {
	if row == nil || p == nil || stats == nil || !p.CostCapped() {
		return
	}
	stat, ok := stats[p.ID]
	if !ok {
		return
	}
	block := map[string]any{
		"limit_micros": stat.LimitMicros,
		"used_micros":  stat.UsedMicros,
		"period":       stat.Period,
		"exceeded":     stat.Exceeded,
		"currency":     currency,
	}
	if !stat.WindowStart.IsZero() {
		block["window_start"] = stat.WindowStart.UTC().Format(time.RFC3339)
	}
	row["cost"] = block
}

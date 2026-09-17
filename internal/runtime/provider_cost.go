// Provider cost cap enforcement (M56): how much each upstream has cost us, kept current in
// process so the router can drop a provider without touching the database per request.
//
// The reading is deliberately *derived* from the metering table instead of accumulated in a
// counter: a counter would have to be right about replays, batched writes and several
// instances, and being wrong there means either billing-grade drift or a provider that stays
// blocked after a reset. The price of deriving it is a periodic read — one indexed range scan
// per distinct window start, every CostRefreshInterval — which is why the guard can overshoot
// by at most the traffic of one interval. docs/design/m56-provider-cost-cap.md §2 (D3, D7, D8)
// records that trade-off.
package runtime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// CostSource is the aggregation read the tracker needs. *store.DB implements it; keeping it
// this narrow is what lets the tracker live in this package without importing the store
// (internal/arch/layering_test.go).
type CostSource interface {
	// ProviderCostsSince returns what each provider cost since its own window start, in
	// ledger micro-units. Providers with no rows in their window come back as 0.
	ProviderCostsSince(ctx context.Context, windows map[int64]time.Time) (map[int64]int64, error)
}

// CostRefreshInterval is how often the readings are re-derived from the metering table. It is
// a constant rather than a setting on purpose: the promise made to the operator is "the cap
// is a guard that can overshoot by a few seconds of traffic", and a knob would invite the
// belief that a tighter value makes it a ledger.
const CostRefreshInterval = 5 * time.Second

// ProviderCostStat is one capped provider's state, as the console and /stats read it back.
// UsedMicros is the same number the router compared against, so the two can never disagree.
type ProviderCostStat struct {
	LimitMicros int64     `json:"limit_micros"`
	UsedMicros  int64     `json:"used_micros"`
	Period      string    `json:"period"`
	WindowStart time.Time `json:"window_start"`
	Exceeded    bool      `json:"exceeded"`
}

// CostStatus describes the reader itself: how fresh the readings are and whether they are
// readable at all. A failed read does not stop traffic (see Exceeded), so the error has to be
// visible here instead.
type CostStatus struct {
	IntervalS int       `json:"interval_s"`
	AsOf      time.Time `json:"as_of"`
	LastError string    `json:"last_error"`
	Tracked   int       `json:"tracked"`
}

// CostTracker holds the last reading of every capped provider.
type CostTracker struct {
	src      CostSource
	reg      *registry.Registry
	log      *slog.Logger
	interval time.Duration

	// refreshing makes concurrent refreshes collapse into one: the periodic one and a
	// caller that wants a fresh number must not scan the metering table in parallel.
	refreshing sync.Mutex

	mu        sync.RWMutex
	used      map[int64]int64
	windows   map[int64]time.Time
	asOf      time.Time
	lastError string
	// reported is the state each provider was last announced with, so the transition log
	// fires on the crossing rather than on every interval.
	reported map[int64]bool
	lastWarn time.Time
}

// NewCostTracker builds a tracker over the registry snapshot and one aggregation source. A nil
// source disables it (a deployment without a metering store, and most tests).
func NewCostTracker(src CostSource, reg *registry.Registry, log *slog.Logger) *CostTracker {
	return &CostTracker{
		src:      src,
		reg:      reg,
		log:      log,
		interval: CostRefreshInterval,
		used:     map[int64]int64{},
		windows:  map[int64]time.Time{},
		reported: map[int64]bool{},
	}
}

// Start seeds the readings once, then keeps them current until the returned stop function is
// called. Seeding synchronously matters: without it the guard would be blind for one interval
// after every restart, and a provider that was already over its cap would serve traffic.
func (t *CostTracker) Start(ctx context.Context) func() {
	if t == nil {
		return func() {}
	}
	if err := t.Refresh(ctx); err != nil {
		t.noteFailure(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = t.Refresh(ctx) // Refresh reports its own failures
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(cancel)
		<-done
	}
}

// Refresh re-derives every capped provider's reading. It is safe to call concurrently.
//
// A provider whose cap was removed simply disappears from the reading: the map is replaced
// wholesale rather than merged, so a deleted provider cannot linger.
func (t *CostTracker) Refresh(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if t.src == nil || t.reg == nil {
		return nil
	}
	if !t.refreshing.TryLock() {
		return nil // another refresh is already scanning; its result is the same age or newer
	}
	defer t.refreshing.Unlock()

	now := time.Now().UTC()
	snap := t.reg.Snapshot()
	var capped []*domain.Provider
	windows := map[int64]time.Time{}
	if snap != nil {
		for _, p := range snap.Providers {
			if !p.CostCapped() {
				continue
			}
			capped = append(capped, p)
			windows[p.ID] = domain.ProviderCostWindowStart(p, now)
		}
	}

	used := map[int64]int64{}
	if len(windows) > 0 {
		read, err := t.src.ProviderCostsSince(ctx, windows)
		if err != nil {
			// Keep the previous reading: stale is better than blind, and Exceeded keeps
			// using it. The error is surfaced through Status so it is not silent.
			t.noteFailure(err)
			return err
		}
		for id := range windows {
			used[id] = read[id]
		}
	}

	t.publish(capped, used, windows, now)
	return nil
}

// publish swaps in the new reading and reports the crossings. Both halves happen under the
// same lock so a log line can never describe a state the router was not using.
//
// The lines are emitted after the lock is released: Exceeded and Stats take the same mutex on
// the request path, and a slow log sink must never become a routing delay.
func (t *CostTracker) publish(capped []*domain.Provider, used map[int64]int64, windows map[int64]time.Time, now time.Time) {
	type crossing struct {
		provider    *domain.Provider
		exceeded    bool
		usedMicros  int64
		windowStart time.Time
	}
	var crossings []crossing

	t.mu.Lock()
	t.used = used
	t.windows = windows
	t.asOf = now
	t.lastError = ""
	live := make(map[int64]bool, len(capped))
	for _, p := range capped {
		id := p.ID
		live[id] = true
		exceeded := domain.ProviderCostExceeded(p, used[id])
		previous, seen := t.reported[id]
		switch {
		case exceeded && (!seen || !previous):
			t.reported[id] = true
			crossings = append(crossings, crossing{provider: p, exceeded: true, usedMicros: used[id], windowStart: windows[id]})
		case !exceeded && seen && previous:
			t.reported[id] = false
			crossings = append(crossings, crossing{provider: p, exceeded: false, usedMicros: used[id]})
		default:
			t.reported[id] = exceeded
		}
	}
	for id := range t.reported {
		if !live[id] {
			delete(t.reported, id)
		}
	}
	log := t.log
	t.mu.Unlock()

	if log == nil {
		return
	}
	for _, c := range crossings {
		if c.exceeded {
			log.Warn("provider reached its cost cap and will not be chosen",
				"provider", c.provider.Name, "provider_id", c.provider.ID,
				"used_micros", c.usedMicros, "limit_micros", c.provider.CostLimitMicros,
				"period", c.provider.CostPeriod,
				"window_start", c.windowStart.UTC().Format(time.RFC3339))
			continue
		}
		log.Info("provider is back under its cost cap",
			"provider", c.provider.Name, "provider_id", c.provider.ID,
			"used_micros", c.usedMicros, "limit_micros", c.provider.CostLimitMicros)
	}
}

// noteFailure records a failed read and logs it at most once a minute: a metering read can
// fail for every interval while a database is briefly unavailable, and one line per five
// seconds would bury whatever else the operator is looking at.
func (t *CostTracker) noteFailure(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.lastError = err.Error()
	logIt := t.log != nil && (t.lastWarn.IsZero() || time.Since(t.lastWarn) >= time.Minute)
	if logIt {
		t.lastWarn = time.Now()
	}
	l := t.log
	t.mu.Unlock()
	if logIt {
		l.Warn("reading provider costs failed; the cost cap keeps using its last reading",
			"err", err)
	}
}

// Exceeded reports whether this provider has spent its budget. It is the routing gate.
//
// Three deliberate properties:
//
//   - a provider without a cap (the default) is never checked;
//   - no reading yet means *not* exceeded — fail open. Newly capped providers are anchored at
//     "now" (the write path does that), so a missing reading is genuinely a zero, and a guard
//     must not stop traffic on a number it does not have;
//   - the comparison uses the provider's *current* limit, so raising the cap takes effect
//     immediately, while the used figure stays as fresh as the last refresh.
//
// now is taken for symmetry with the other candidate filters; the window it would select is
// already folded into the reading (a rolled-over window keeps the older, larger number, which
// is the safe direction — see the design doc D11).
func (t *CostTracker) Exceeded(p *domain.Provider, now time.Time) bool {
	_ = now
	if t == nil || !p.CostCapped() {
		return false
	}
	t.mu.RLock()
	used, ok := t.used[p.ID]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	return domain.ProviderCostExceeded(p, used)
}

// MarkReset zeroes one provider's reading after the write path moved its window start to now.
// That value is exact rather than optimistic: nothing metered before the reset counts any
// more, and anything metered after it will be picked up by the next refresh.
func (t *CostTracker) MarkReset(providerID int64, at time.Time) {
	if t == nil || providerID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, tracked := t.windows[providerID]; !tracked {
		return
	}
	t.used[providerID] = 0
	t.windows[providerID] = at.UTC()
	// A later crossing must be announced again.
	t.reported[providerID] = false
}

// Stats returns every capped provider's state, computed against the *current* configuration in
// the snapshot and the last reading. An entry can therefore appear before the first refresh
// (used 0) and disappear the moment the cap is removed.
func (t *CostTracker) Stats() map[int64]ProviderCostStat {
	out := map[int64]ProviderCostStat{}
	if t == nil || t.reg == nil {
		return out
	}
	snap := t.reg.Snapshot()
	if snap == nil {
		return out
	}
	t.mu.RLock()
	used := make(map[int64]int64, len(t.used))
	for id, micros := range t.used {
		used[id] = micros
	}
	windows := make(map[int64]time.Time, len(t.windows))
	for id, start := range t.windows {
		windows[id] = start
	}
	t.mu.RUnlock()

	for _, p := range snap.Providers {
		if !p.CostCapped() {
			continue
		}
		out[p.ID] = ProviderCostStat{
			LimitMicros: p.CostLimitMicros,
			UsedMicros:  used[p.ID],
			Period:      costPeriodDisplay(p.CostPeriod),
			WindowStart: windows[p.ID],
			Exceeded:    domain.ProviderCostExceeded(p, used[p.ID]),
		}
	}
	return out
}

// Status describes the reader (interval, freshness, last error, how many providers it tracks).
func (t *CostTracker) Status() CostStatus {
	interval := CostRefreshInterval
	if t == nil {
		return CostStatus{IntervalS: int(interval / time.Second)}
	}
	if t.interval > 0 {
		interval = t.interval
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return CostStatus{
		IntervalS: int(interval / time.Second),
		AsOf:      t.asOf,
		LastError: t.lastError,
		Tracked:   len(t.windows),
	}
}

// costPeriodDisplay renders the stored period for readers. An empty value can only come from a
// row written outside this gateway; reporting "none" is what that row behaves as.
func costPeriodDisplay(period string) string {
	if period == "" {
		return domain.CostPeriodNone
	}
	return period
}

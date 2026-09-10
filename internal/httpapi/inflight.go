package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// errQuotaAborted ends a stream because the account ran out of quota mid-call.
var errQuotaAborted = errors.New("httpapi: aborted because the account ran out of quota")

// inflightOutcome records what a guarded attempt did about quota.
type inflightOutcome struct {
	// dims is the usage that may be charged (frozen at the abort decision point).
	dims map[string]int64
	// overshootCostMicros is the cost of everything metered after that point. It is
	// recorded as cost but never charged, which is what keeps a prepaid balance from
	// going negative because a connection took a moment to die.
	overshootCostMicros int64
	terminatedReason    string
	aborted             bool
	accruedChargeMicros int64
}

// inflightGuard watches a streaming call against its reservation. It is the only
// place that can stop a call already in progress, so it deliberately holds no locks:
// it prices the metered deltas with the same pure function the settlement uses.
type inflightGuard struct {
	server     *Server
	account    *domain.Account
	canonical  string
	upstream   string
	providerID int64
	requestID  string
	cost       *pricing.RuleSet
	sale       *pricing.RuleSet

	policy string
	limit  int64
	soft   float64
	hard   float64

	interval time.Duration
	grace    time.Duration
	ctx      context.Context
	cancel   context.CancelFunc

	lastCheck time.Time
	dims      map[string]int64
	accrued   int64
	warned    bool
	paused    bool
	outcome   inflightOutcome

	reservation *billing.Reservation
	heartbeat   time.Duration
	lastTouch   time.Time
}

// newInflightGuard builds a guard for one streaming attempt.
func (s *Server) newInflightGuard(
	ctx context.Context,
	cancel context.CancelFunc,
	account *domain.Account,
	requestID, canonical, upstream string,
	providerID int64,
	cost, sale *pricing.RuleSet,
	reservation *billing.Reservation,
) *inflightGuard {
	cfg := s.deps.Config.Billing
	policy := s.inflightPolicy(account)
	available := int64(0)
	if account != nil {
		balance := account.BalanceMicros
		floor := int64(0)
		if account.BillingMode == domain.BillingPostpaid {
			floor = -account.CreditLimitMicros
		}
		available = balance - floor
	}
	reserved := int64(0)
	if reservation != nil {
		reserved = reservation.AmountMicros
	}
	interval := time.Duration(cfg.InflightCheckMS) * time.Millisecond
	grace := time.Duration(cfg.CancelGraceMS) * time.Millisecond
	if grace <= 0 {
		grace = 2 * time.Second
	}
	return &inflightGuard{
		server: s, account: account, canonical: canonical, upstream: upstream,
		providerID: providerID, requestID: requestID,
		cost: cost, sale: sale,
		policy:   policy,
		limit:    billing.InflightLimit(policy, reserved, available),
		soft:     cfg.InflightSoftRatio,
		hard:     cfg.InflightHardRatio,
		interval: interval, grace: grace,
		reservation: reservation,
		heartbeat:   time.Duration(cfg.ReservationHeartbeatS) * time.Second,
		ctx:         ctx, cancel: cancel,
		dims: map[string]int64{},
	}
}

// Observe wraps the assembler callback: it meters the event, forwards it, and checks
// the budget on the configured cadence.
func (g *inflightGuard) Observe(ev pluginapi.Event, next func(pluginapi.Event) error) error {
	g.accumulate(ev)
	if err := next(ev); err != nil {
		return err
	}
	now := time.Now()
	// A long call must keep its hold alive, otherwise the GC would release the money
	// while the request is still running and the reservation would stop protecting it.
	if g.reservation != nil && g.heartbeat > 0 && now.Sub(g.lastTouch) >= g.heartbeat {
		g.lastTouch = now
		g.server.deps.Billing.Touch(g.reservation, now)
	}
	if g.interval > 0 && now.Sub(g.lastCheck) < g.interval {
		return nil
	}
	g.lastCheck = now
	return g.check()
}

func (g *inflightGuard) accumulate(ev pluginapi.Event) {
	switch ev.Type {
	case pluginapi.EventUsage:
		if ev.Usage == nil {
			return
		}
		g.dims = map[string]int64{}
		for dimension, units := range ev.Usage.Dimensions {
			g.dims[dimension] = units
		}
	case pluginapi.EventUsageDelta:
		if ev.Usage == nil {
			return
		}
		for dimension, units := range ev.Usage.Dimensions {
			g.dims[dimension] += units
		}
	}
}

func (g *inflightGuard) check() error {
	if g.cancel == nil || g.limit <= 0 || len(g.dims) == 0 {
		return nil
	}
	result := g.server.priceAttempt(g.dims, time.Now(), g.upstream, g.cost, g.sale)
	g.accrued = result.ChargeMicros
	action := billing.DecideInflight(billing.InflightState{
		Policy: g.policy, AccruedMicros: g.accrued,
		LimitMicros: g.limit, SoftRatio: g.soft, HardRatio: g.hard,
	})
	switch action {
	case billing.InflightWarn:
		if !g.warned {
			g.warned = true
			g.server.deps.Log.Warn("request is close to its quota reservation",
				"request_id", g.requestID, "policy", g.policy,
				"accrued_micros", g.accrued, "limit_micros", g.limit)
			g.emit("billing.inflight_warn")
		}
		return nil
	case billing.InflightThrottle:
		if !g.paused {
			g.paused = true
			g.server.deps.Log.Warn("throttling a request that reached its soft quota limit",
				"request_id", g.requestID, "accrued_micros", g.accrued, "limit_micros", g.limit)
			g.emit("billing.inflight_throttle")
		}
		// Backpressure: not reading the upstream pipe stops the provider from making
		// progress. The balance cannot grow while the request runs, so a pause that
		// outlives the grace period means the call must end.
		g.pause(g.grace)
		return g.abort()
	case billing.InflightAbort:
		return g.abort()
	}
	return nil
}

func (g *inflightGuard) pause(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-g.ctx.Done():
	case <-timer.C:
	}
}

func (g *inflightGuard) abort() error {
	if g.outcome.aborted {
		return errQuotaAborted
	}
	g.outcome.aborted = true
	g.outcome.terminatedReason = "aborted_quota"
	g.outcome.accruedChargeMicros = g.accrued
	g.outcome.dims = copyDimensions(g.dims)
	g.server.deps.Log.Warn("aborting a request that exhausted its quota reservation",
		"request_id", g.requestID, "policy", g.policy,
		"accrued_micros", g.accrued, "limit_micros", g.limit)
	g.emit("billing.inflight_abort")
	// Cancelling the attempt context reaches the plugin as provider.cancel{reason}.
	g.cancel()
	return errQuotaAborted
}

func (g *inflightGuard) emit(name string) {
	if g.server.deps.Hooks == nil {
		return
	}
	payload := map[string]any{
		"request_id": g.requestID, "model": g.canonical,
		"policy": g.policy, "accrued_micros": g.accrued, "limit_micros": g.limit,
	}
	if g.account != nil {
		payload["account"] = g.account.Name
	}
	g.server.deps.Hooks.Emit(g.ctx, &domain.Event{
		Name: name, Timestamp: time.Now().UTC(), Payload: payload,
	})
}

// Outcome finalises the guard: usage metered after an abort becomes overshoot cost.
func (g *inflightGuard) Outcome(finalDims map[string]int64) *inflightOutcome {
	if !g.outcome.aborted {
		return nil
	}
	if len(finalDims) > 0 {
		delta := subtractDimensions(finalDims, g.outcome.dims)
		if len(delta) > 0 {
			cost := g.server.priceAttempt(delta, time.Now(), g.upstream, g.cost, g.sale)
			g.outcome.overshootCostMicros = cost.CostMicros
		}
	}
	return &g.outcome
}

func copyDimensions(dims map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(dims))
	for dimension, units := range dims {
		out[dimension] = units
	}
	return out
}

// subtractDimensions returns the units in full that are not in partial.
func subtractDimensions(full, partial map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for dimension, units := range full {
		if rest := units - partial[dimension]; rest > 0 {
			out[dimension] = rest
		}
	}
	return out
}

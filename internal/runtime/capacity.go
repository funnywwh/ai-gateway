// Provider capacity gate: a per-provider ceiling on concurrent upstream attempts, with a
// FIFO queue in front of it.
//
// It exists because `providers.max_inflight` was stored, editable and displayed but never
// read: an operator could set "at most 2 requests to this upstream" and nothing happened.
// The gate makes it real, and makes the excess *wait* rather than fail — the common case
// is a burst that the upstream would have served a little later anyway.
//
// State is in-process on purpose (like the balancer's): no migration, no cross-process
// coordination to get wrong. A multi-instance deployment therefore counts per instance.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// CapacityStat is the live state of one provider's gate.
type CapacityStat struct {
	// Limit is the maximum number of concurrent attempts; 0 means unlimited.
	Limit int `json:"limit"`
	// Inflight is how many attempts hold a slot right now.
	Inflight int `json:"inflight"`
	// Waiting is how many attempts are queued for one.
	Waiting int `json:"waiting"`
	// Admitted counts attempts that got a slot (immediately or after waiting).
	Admitted int64 `json:"admitted"`
	// QueueFull counts attempts refused because the queue was at its depth limit.
	QueueFull int64 `json:"queue_full"`
	// TimedOut counts attempts that gave up waiting.
	TimedOut int64 `json:"timed_out"`
	// Cancelled counts attempts whose context ended while they waited.
	Cancelled int64 `json:"cancelled"`
	// WaitTotalMS is the total time spent waiting, for an average alongside Admitted.
	WaitTotalMS int64 `json:"wait_total_ms"`
}

// CapacityPolicy is the queue policy the data path actually applies. It is gateway-wide
// (a deployment setting), not per provider: only the ceiling is per provider.
type CapacityPolicy struct {
	QueueWaitS      int `json:"queue_wait_s"`
	QueueMaxWaiters int `json:"queue_max_waiters"`
}

// Capacity refusal reasons, reported to the caller and in metrics.
const (
	// CapacityWaitTimeout: the attempt waited its whole budget and no slot appeared.
	CapacityWaitTimeout = "wait_timeout"
	// CapacityQueueFull: the provider's queue was already at its depth limit.
	CapacityQueueFull = "queue_full"
	// CapacityLimitReached: the provider is full and queueing is disabled.
	CapacityLimitReached = "limit_reached"
)

// ErrProviderBusy is the sentinel every capacity refusal wraps. It is retryable: the
// refusal says nothing about the upstream's health, and another candidate may have room.
var ErrProviderBusy = errors.New("provider at its concurrency limit")

// CapacityError reports that an attempt could not get a provider capacity slot.
type CapacityError struct {
	ProviderID int64
	Provider   string
	// Reason is one of the Capacity* constants.
	Reason    string
	Limit     int
	Waiters   int
	Waited    time.Duration
	WaitLimit time.Duration
}

func (e *CapacityError) Error() string {
	name := e.Provider
	if name == "" {
		name = fmt.Sprintf("provider#%d", e.ProviderID)
	}
	switch e.Reason {
	case CapacityQueueFull:
		return fmt.Sprintf("provider %s is at its concurrency limit (%d) and its queue is full (%d waiting)",
			name, e.Limit, e.Waiters)
	case CapacityWaitTimeout:
		return fmt.Sprintf("provider %s is at its concurrency limit (%d): waited %s for a slot out of %s",
			name, e.Limit, e.Waited.Round(time.Millisecond), e.WaitLimit)
	default:
		return fmt.Sprintf("provider %s is at its concurrency limit (%d, %d waiting) and queueing is disabled",
			name, e.Limit, e.Waiters)
	}
}

// Unwrap makes errors.Is(err, ErrProviderBusy) true for every refusal.
func (e *CapacityError) Unwrap() error { return ErrProviderBusy }

// RetryAfterSeconds is the hint handed to the client as Retry-After once every candidate
// has been refused. It is the configured wait budget, not a promise: the gate cannot know
// when a slot frees.
func (e *CapacityError) RetryAfterSeconds() int {
	seconds := int(e.WaitLimit / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// gate holds one slot state per provider. A nil gate (the Dispatcher built without one)
// admits everything.
type gate struct {
	cfg Config

	mu    sync.Mutex
	slots map[int64]*slotState
}

// slotState is one provider's ceiling, occupancy and waiting line.
type slotState struct {
	limit    int
	inflight int
	waiters  []*waiter

	admitted    int64
	queueFull   int64
	timedOut    int64
	cancelled   int64
	waitTotalMS int64
}

// waiter is one queued attempt. ready is closed when a slot is handed to it, and handed
// records that hand-off under the gate's lock so a giving-up waiter can pass the slot on
// instead of leaking it.
type waiter struct {
	ready  chan struct{}
	handed bool
	booked bool
}

// permit is one held slot. Release is idempotent and nil-safe, like quota.Ticket.
type permit struct {
	gate   *gate
	id     int64
	booked bool
	waited time.Duration
	once   sync.Once
}

func newGate(cfg Config) *gate {
	return &gate{cfg: cfg, slots: map[int64]*slotState{}}
}

// waited is how long the attempt waited for its slot (0 when it went straight through).
func (p *permit) waitedFor() time.Duration {
	if p == nil {
		return 0
	}
	return p.waited
}

// Release returns the slot, handing it to the longest-waiting attempt.
func (p *permit) Release() {
	if p == nil || p.gate == nil {
		return
	}
	p.once.Do(func() {
		g := p.gate
		g.mu.Lock()
		defer g.mu.Unlock()
		st := g.slots[p.id]
		if st == nil {
			return
		}
		if p.booked && st.inflight > 0 {
			st.inflight--
		}
		g.handOffLocked(st)
	})
}

// acquire waits for a slot on provider id. limit <= 0 means unlimited and is admitted
// without any bookkeeping. The returned error is a *CapacityError (or the context's own
// error when the caller gave up).
func (g *gate) acquire(ctx context.Context, id int64, limit int, provider string) (*permit, error) {
	if g == nil || limit <= 0 {
		return &permit{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	started := time.Now()

	g.mu.Lock()
	st := g.slots[id]
	if st == nil {
		st = &slotState{}
		g.slots[id] = st
	}
	// Last observed wins: the limit comes from the registry snapshot, and a request that
	// reads a newer snapshot must not be overruled by an older one that arrived later.
	st.limit = limit
	if st.inflight < st.limit {
		st.inflight++
		st.admitted++
		g.mu.Unlock()
		return &permit{gate: g, id: id, booked: true}, nil
	}
	waiting := len(st.waiters)
	if g.cfg.QueueMaxWaiters > 0 && waiting >= g.cfg.QueueMaxWaiters {
		st.queueFull++
		g.mu.Unlock()
		return nil, &CapacityError{
			ProviderID: id, Provider: provider, Reason: CapacityQueueFull,
			Limit: limit, Waiters: waiting, Waited: time.Since(started), WaitLimit: g.cfg.QueueWait,
		}
	}
	if g.cfg.QueueWait <= 0 {
		g.mu.Unlock()
		return nil, &CapacityError{
			ProviderID: id, Provider: provider, Reason: CapacityLimitReached,
			Limit: limit, Waiters: waiting, WaitLimit: g.cfg.QueueWait,
		}
	}
	w := &waiter{ready: make(chan struct{})}
	st.waiters = append(st.waiters, w)
	g.mu.Unlock()

	timer := time.NewTimer(g.cfg.QueueWait)
	defer timer.Stop()
	select {
	case <-w.ready:
		waited := time.Since(started)
		g.countWait(id, waited, func(st *slotState) { st.admitted++ })
		return &permit{gate: g, id: id, booked: w.booked, waited: waited}, nil
	case <-ctx.Done():
		waited := time.Since(started)
		g.retract(id, w)
		g.countWait(id, waited, func(st *slotState) { st.cancelled++ })
		return nil, ctx.Err()
	case <-timer.C:
		waited := time.Since(started)
		g.retract(id, w)
		g.countWait(id, waited, func(st *slotState) { st.timedOut++ })
		return nil, &CapacityError{
			ProviderID: id, Provider: provider, Reason: CapacityWaitTimeout,
			Limit: limit, Waiters: g.waiters(id), Waited: waited, WaitLimit: g.cfg.QueueWait,
		}
	}
}

// retract takes a giving-up waiter out of the line. When a slot was already handed to it
// (the hand-off and the give-up raced), the slot is passed on rather than dropped: the
// alternative is a permanently missing slot on a provider nobody can restart.
func (g *gate) retract(id int64, w *waiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.slots[id]
	if st == nil {
		return
	}
	if w.handed {
		if w.booked && st.inflight > 0 {
			st.inflight--
		}
		g.handOffLocked(st)
		return
	}
	for i, other := range st.waiters {
		if other == w {
			st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
			return
		}
	}
}

// handOffLocked gives free slots to the attempts at the head of the line. It hands over at
// most what the current limit allows, so a limit lowered while requests wait keeps the
// ceiling. A limit of 0 (unlimited) releases everyone without booking a slot.
func (g *gate) handOffLocked(st *slotState) {
	if st.limit <= 0 {
		for len(st.waiters) > 0 {
			w := st.waiters[0]
			st.waiters = st.waiters[1:]
			w.handed, w.booked = true, false
			close(w.ready)
		}
		return
	}
	for len(st.waiters) > 0 && st.inflight < st.limit {
		w := st.waiters[0]
		st.waiters = st.waiters[1:]
		w.handed, w.booked = true, true
		st.inflight++
		close(w.ready)
	}
}

// setLimit records a provider's ceiling and releases whoever now fits. It is called after
// a registry reload, so an operator raising a limit frees the requests already queued
// instead of leaving them to wait for a release that already happened.
func (g *gate) setLimit(id int64, limit int) {
	if g == nil || limit < 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.slots[id]
	if st == nil {
		if limit <= 0 {
			return // an unlimited provider needs no state at all
		}
		st = &slotState{}
		g.slots[id] = st
	}
	if st.limit == limit {
		return
	}
	st.limit = limit
	g.handOffLocked(st)
}

// countWait records how one queued attempt ended and what the queue cost it. Waiting time
// is accumulated for every outcome — including the attempts that gave up — so the counter
// measures the queue a provider caused rather than only the part an answer paid for.
func (g *gate) countWait(id int64, waited time.Duration, apply func(*slotState)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.slots[id]; st != nil {
		apply(st)
		st.waitTotalMS += waited.Milliseconds()
	}
}

// waiters reports the current queue depth (used for the error message after a timeout).
func (g *gate) waiters(id int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st := g.slots[id]; st != nil {
		return len(st.waiters)
	}
	return 0
}

// stats snapshots every provider that has a gate.
func (g *gate) stats() map[int64]CapacityStat {
	if g == nil {
		return map[int64]CapacityStat{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[int64]CapacityStat, len(g.slots))
	for id, st := range g.slots {
		out[id] = CapacityStat{
			Limit:       st.limit,
			Inflight:    st.inflight,
			Waiting:     len(st.waiters),
			Admitted:    st.admitted,
			QueueFull:   st.queueFull,
			TimedOut:    st.timedOut,
			Cancelled:   st.cancelled,
			WaitTotalMS: st.waitTotalMS,
		}
	}
	return out
}

// Package quota enforces request, token and concurrency limits with sharded,
// in-memory sliding windows. Sharding keeps lock contention low on the hot path.
package quota

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// Slots is the number of one-second buckets in a sliding window (60s).
const Slots = 60

// Limits describe the effective limits for one scope (0 means "not set").
type Limits struct {
	RPM               int
	TPM               int64
	Concurrency       int
	MonthlyRequests   int64
	MonthlyTokens     int64
	MonthlyCostMicros int64
}

// Merge returns the strictest combination of two limit sets.
func Merge(a, b Limits) Limits {
	return Limits{
		RPM:               minPositive(a.RPM, b.RPM),
		TPM:               minPositive64(a.TPM, b.TPM),
		Concurrency:       minPositive(a.Concurrency, b.Concurrency),
		MonthlyRequests:   minPositive64(a.MonthlyRequests, b.MonthlyRequests),
		MonthlyTokens:     minPositive64(a.MonthlyTokens, b.MonthlyTokens),
		MonthlyCostMicros: minPositive64(a.MonthlyCostMicros, b.MonthlyCostMicros),
	}
}

func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func minPositive64(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// LimitKind names the exceeded dimension.
type LimitKind string

// Limit kinds.
const (
	LimitRequests    LimitKind = "requests"
	LimitTokens      LimitKind = "tokens"
	LimitConcurrency LimitKind = "concurrency"
)

// ExceededError describes which limit was hit, so the HTTP layer can emit
// x-ratelimit-* headers and Retry-After.
type ExceededError struct {
	Scope     string
	Kind      LimitKind
	Limit     int64
	Used      int64
	Remaining int64
	ResetAt   time.Time
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("quota: %s limit exceeded for %s (limit=%d used=%d)", e.Kind, e.Scope, e.Limit, e.Used)
}

// RetryAfter returns the seconds to wait before retrying.
func (e *ExceededError) RetryAfter(now time.Time) int {
	if e.ResetAt.IsZero() {
		return 1
	}
	secs := int(e.ResetAt.Sub(now).Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// ToAPIError maps the limit violation onto the OpenAI-compatible error envelope.
func (e *ExceededError) ToAPIError() *domain.APIError {
	return domain.ErrRateLimited(e.Error())
}

// Limiter is a sharded sliding-window limiter.
type Limiter struct {
	shards []*shard
	now    func() time.Time
}

type shard struct {
	mu     sync.Mutex
	scopes map[string]*scopeState
}

type bucket struct {
	sec   int64
	value int64
}

type scopeState struct {
	reqs     [Slots]bucket
	toks     [Slots]bucket
	inflight int
}

// New builds a limiter with the given shard count (<=0 uses 64).
func New(shards int) *Limiter {
	if shards <= 0 {
		shards = 64
	}
	l := &Limiter{shards: make([]*shard, shards), now: func() time.Time { return time.Now().UTC() }}
	for i := range l.shards {
		l.shards[i] = &shard{scopes: map[string]*scopeState{}}
	}
	return l
}

// SetClock overrides the clock (tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

func (l *Limiter) shardFor(scope string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(scope))
	return l.shards[h.Sum32()%uint32(len(l.shards))]
}

// Ticket represents a granted request slot. Release must always be called.
type Ticket struct {
	limiter *Limiter
	scope   string
	shard   *shard
	once    sync.Once
}

// Reserve checks the limits and books a concurrency slot.
func (l *Limiter) Reserve(ctx context.Context, scope string, limits Limits) (*Ticket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := l.now()
	sec := now.Unix()
	sh := l.shardFor(scope)

	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.scopes[scope]
	if st == nil {
		st = &scopeState{}
		sh.scopes[scope] = st
	}
	st.expire(sec)

	reqs := st.sumReqs(sec)
	toks := st.sumToks(sec)

	if limits.Concurrency > 0 && st.inflight >= limits.Concurrency {
		return nil, &ExceededError{
			Scope: scope, Kind: LimitConcurrency,
			Limit: int64(limits.Concurrency), Used: int64(st.inflight),
			Remaining: 0, ResetAt: time.Unix(sec+1, 0).UTC(),
		}
	}
	if limits.RPM > 0 && reqs >= int64(limits.RPM) {
		return nil, &ExceededError{
			Scope: scope, Kind: LimitRequests,
			Limit: int64(limits.RPM), Used: reqs,
			Remaining: 0, ResetAt: st.resetAt(sec, &st.reqs),
		}
	}
	if limits.TPM > 0 && toks >= limits.TPM {
		return nil, &ExceededError{
			Scope: scope, Kind: LimitTokens,
			Limit: limits.TPM, Used: toks,
			Remaining: 0, ResetAt: st.resetAt(sec, &st.toks),
		}
	}

	st.addReq(sec, 1)
	st.inflight++
	return &Ticket{limiter: l, scope: scope, shard: sh}, nil
}

// Release frees the concurrency slot (idempotent).
func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		sec := t.limiter.now().Unix()
		t.shard.mu.Lock()
		if st := t.shard.scopes[t.scope]; st != nil && st.inflight > 0 {
			st.inflight--
		}
		t.shard.mu.Unlock()
		_ = sec
	})
}

// Settle records the tokens consumed by the finished request (tpm accounting).
func (t *Ticket) Settle(tokens int64) {
	if t == nil || tokens <= 0 {
		return
	}
	sec := t.limiter.now().Unix()
	t.shard.mu.Lock()
	if st := t.shard.scopes[t.scope]; st != nil {
		st.expire(sec)
		st.addTok(sec, tokens)
	}
	t.shard.mu.Unlock()
}

// Snapshot reports the current window usage for one scope (diagnostics and headers).
func (l *Limiter) Snapshot(scope string) (requests int64, tokens int64, inflight int) {
	now := l.now()
	sec := now.Unix()
	sh := l.shardFor(scope)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	st := sh.scopes[scope]
	if st == nil {
		return 0, 0, 0
	}
	st.expire(sec)
	return st.sumReqs(sec), st.sumToks(sec), st.inflight
}

func (s *scopeState) expire(sec int64) {
	for i := 0; i < Slots; i++ {
		if s.reqs[i].sec != 0 && sec-s.reqs[i].sec >= Slots {
			s.reqs[i] = bucket{}
		}
		if s.toks[i].sec != 0 && sec-s.toks[i].sec >= Slots {
			s.toks[i] = bucket{}
		}
	}
}

func (s *scopeState) addReq(sec int64, n int64) { addTo(&s.reqs, sec, n) }
func (s *scopeState) addTok(sec int64, n int64) { addTo(&s.toks, sec, n) }

func addTo(slots *[Slots]bucket, sec int64, n int64) {
	idx := int(sec % Slots)
	if (*slots)[idx].sec != sec {
		(*slots)[idx] = bucket{sec: sec}
	}
	(*slots)[idx].value += n
}

func (s *scopeState) sumReqs(sec int64) int64 { return sumWindow(&s.reqs, sec) }
func (s *scopeState) sumToks(sec int64) int64 { return sumWindow(&s.toks, sec) }

func sumWindow(slots *[Slots]bucket, sec int64) int64 {
	var total int64
	for i := 0; i < Slots; i++ {
		if sec-slots[i].sec < Slots && slots[i].sec != 0 {
			total += slots[i].value
		}
	}
	return total
}

// resetAt returns when the oldest contributing bucket leaves the window.
func (s *scopeState) resetAt(sec int64, slots *[Slots]bucket) time.Time {
	oldest := sec
	for i := 0; i < Slots; i++ {
		if slots[i].sec != 0 && slots[i].value > 0 && slots[i].sec < oldest {
			oldest = slots[i].sec
		}
	}
	return time.Unix(oldest+Slots, 0).UTC()
}

// LimitsFromPolicy converts a key/tag policy JSON into Limits.
//
// The document is decoded by domain.ParsePolicy so enforcement, the management console
// and the MCP report can never disagree about the shape: only *top-level* quota fields
// count. Recognised fields: rpm, tpm, concurrency, monthly_requests, monthly_tokens,
// monthly_cost_micros — of which this limiter checks rpm, tpm and concurrency (the
// monthly caps are parsed but not enforced yet).
func LimitsFromPolicy(raw string) Limits {
	rl, _, err := domain.ParsePolicy(raw)
	if err != nil {
		return Limits{}
	}
	return Limits{
		RPM: rl.RPM, TPM: rl.TPM, Concurrency: rl.Concurrency,
		MonthlyRequests: rl.MonthlyRequests, MonthlyTokens: rl.MonthlyTokens,
		MonthlyCostMicros: rl.MonthlyCostMicros,
	}
}

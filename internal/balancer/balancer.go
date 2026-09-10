// Package balancer implements the layer-internal load-balancing strategies and their
// runtime state: in-flight counts, per-token latency EWMA, round-robin cursors and
// per-route circuit breakers. All state is safe for concurrent use.
package balancer

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Strategy names (mirrors routing.default_strategy).
type Strategy string

// Supported strategies.
const (
	WeightedRandom Strategy = "weighted_random"
	RoundRobin     Strategy = "round_robin"
	LeastInflight  Strategy = "least_inflight"
	LeastLatency   Strategy = "least_latency"
	StrictOrder    Strategy = "strict_order"
)

// Strategies lists the supported strategies (sorted).
func Strategies() []string {
	return []string{
		string(LeastInflight), string(LeastLatency), string(RoundRobin),
		string(StrictOrder), string(WeightedRandom),
	}
}

// Valid reports whether s is a supported strategy.
func Valid(s string) bool {
	switch Strategy(s) {
	case WeightedRandom, RoundRobin, LeastInflight, LeastLatency, StrictOrder:
		return true
	default:
		return false
	}
}

// Target is one candidate inside a priority tier.
type Target struct {
	// Key identifies the route (used for state bookkeeping).
	Key string
	// Weight is route.weight × provider.weight (>= 0).
	Weight int
	// MaxOutputTokens normalises latency for least_latency (0 means unknown).
	MaxOutputTokens int
	// Order is the configured route priority (smaller first, used by strict_order).
	Order int
	// Reasoning marks reasoning models, which are de-prioritised for least_latency.
	Reasoning bool
}

// Config tunes the circuit breaker.
type Config struct {
	BreakerFailures int
	BreakerWindow   time.Duration
	BreakerCooldown time.Duration
	MinSamples      int
	FailureRate     float64
}

// DefaultConfig mirrors the YAML defaults.
func DefaultConfig() Config {
	return Config{
		BreakerFailures: 5,
		BreakerWindow:   60 * time.Second,
		BreakerCooldown: 30 * time.Second,
		MinSamples:      10,
		FailureRate:     0.6,
	}
}

// Metrics is a point-in-time view of one target (used by /stats).
type Metrics struct {
	Inflight    int
	Samples     int
	LatencyEWMA float64
	Total       int
	Failures    int
	OpenUntil   time.Time
}

// State holds per-target runtime state.
type State struct {
	cfg Config

	mu      sync.Mutex
	targets map[string]*targetState
	cursor  map[string]int
	rnd     *rand.Rand
}

type targetState struct {
	inflight int
	samples  int
	ewma     float64

	windowStart time.Time
	total       int
	failures    int
	consecutive int
	openUntil   time.Time
	halfOpen    bool
}

// New creates the state holder.
func New(cfg Config) *State {
	if cfg.BreakerFailures <= 0 {
		cfg.BreakerFailures = DefaultConfig().BreakerFailures
	}
	if cfg.BreakerWindow <= 0 {
		cfg.BreakerWindow = DefaultConfig().BreakerWindow
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = DefaultConfig().BreakerCooldown
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = DefaultConfig().MinSamples
	}
	if cfg.FailureRate <= 0 {
		cfg.FailureRate = DefaultConfig().FailureRate
	}
	return &State{
		cfg:     cfg,
		targets: map[string]*targetState{},
		cursor:  map[string]int{},
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *State) target(key string) *targetState {
	ts, ok := s.targets[key]
	if !ok {
		ts = &targetState{windowStart: time.Now()}
		s.targets[key] = ts
	}
	return ts
}

// Order returns the tier ordered according to strategy.
func (s *State) Order(strategy Strategy, tier []Target) []Target {
	if len(tier) <= 1 {
		return append([]Target(nil), tier...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := append([]Target(nil), tier...)
	switch strategy {
	case RoundRobin:
		s.cursor["rr"] = (s.cursor["rr"] + 1) % len(out)
		shift := s.cursor["rr"]
		out = append(out[shift:], out[:shift]...)
	case LeastInflight:
		sort.SliceStable(out, func(i, j int) bool {
			ti, tj := s.target(out[i].Key), s.target(out[j].Key)
			if ti.inflight != tj.inflight {
				return ti.inflight < tj.inflight
			}
			return out[i].Weight > out[j].Weight
		})
	case LeastLatency:
		out = s.orderByLatency(out)
	case StrictOrder:
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Order != out[j].Order {
				return out[i].Order < out[j].Order
			}
			return out[i].Weight > out[j].Weight
		})
	default: // WeightedRandom
		out = s.weightedPermutation(out)
	}
	return out
}

// orderByLatency sorts by per-token latency EWMA, falling back to weight when the
// sample count is insufficient. Reasoning models receive a penalty so that long-running
// models are not starved by short-answer models.
func (s *State) orderByLatency(tier []Target) []Target {
	type scored struct {
		target Target
		score  float64
		known  bool
	}
	scoredTier := make([]scored, 0, len(tier))
	var sum float64
	var known int
	for _, t := range tier {
		ts := s.target(t.Key)
		if ts.samples >= s.cfg.MinSamples && ts.ewma > 0 {
			score := ts.ewma
			if t.MaxOutputTokens > 0 {
				score = ts.ewma / float64(t.MaxOutputTokens)
			}
			if t.Reasoning {
				score *= 1.5 // de-prioritise reasoning models
			}
			scoredTier = append(scoredTier, scored{target: t, score: score, known: true})
			sum += score
			known++
			continue
		}
		scoredTier = append(scoredTier, scored{target: t})
	}
	// Unmeasured targets take the tier average so they neither dominate nor starve.
	avg := 0.0
	if known > 0 {
		avg = sum / float64(known)
	}
	sort.SliceStable(scoredTier, func(i, j int) bool {
		si, sj := scoredTier[i], scoredTier[j]
		if !si.known && !sj.known {
			return si.target.Weight > sj.target.Weight
		}
		if !si.known {
			return avg < sj.score
		}
		if !sj.known {
			return si.score <= avg
		}
		return si.score < sj.score
	})
	out := make([]Target, 0, len(scoredTier))
	for _, sc := range scoredTier {
		out = append(out, sc.target)
	}
	return out
}

// weightedPermutation draws targets without replacement, proportional to weight.
func (s *State) weightedPermutation(tier []Target) []Target {
	remaining := append([]Target(nil), tier...)
	out := make([]Target, 0, len(tier))
	for len(remaining) > 0 {
		total := 0
		for _, t := range remaining {
			if t.Weight > 0 {
				total += t.Weight
			}
		}
		if total <= 0 {
			// All weights are zero: fall back to a uniform shuffle.
			s.rnd.Shuffle(len(remaining), func(i, j int) { remaining[i], remaining[j] = remaining[j], remaining[i] })
			out = append(out, remaining...)
			break
		}
		pick := s.rnd.Intn(total)
		idx := 0
		acc := 0
		for i, t := range remaining {
			w := t.Weight
			if w < 0 {
				w = 0
			}
			acc += w
			if pick < acc {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx])
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	return out
}

// Acquire records that a request started against key.
func (s *State) Acquire(key string) {
	s.mu.Lock()
	s.target(key).inflight++
	s.mu.Unlock()
}

// Release records that a request against key finished.
func (s *State) Release(key string) {
	s.mu.Lock()
	ts := s.target(key)
	if ts.inflight > 0 {
		ts.inflight--
	}
	s.mu.Unlock()
}

// Observe records the outcome of one attempt (ok=false counts as a failure).
func (s *State) Observe(key string, latencyMS float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.target(key)
	now := time.Now()

	if latencyMS > 0 {
		if ts.samples == 0 {
			ts.ewma = latencyMS
		} else {
			// Exponential moving average with alpha = 0.2.
			ts.ewma = 0.8*ts.ewma + 0.2*latencyMS
		}
		ts.samples++
	}

	if now.Sub(ts.windowStart) > s.cfg.BreakerWindow {
		ts.windowStart = now
		ts.total = 0
		ts.failures = 0
	}
	ts.total++

	if ok {
		ts.consecutive = 0
		if ts.halfOpen {
			// Probe succeeded: close the breaker and reset the window.
			ts.halfOpen = false
			ts.openUntil = time.Time{}
			ts.total = 0
			ts.failures = 0
		}
		return
	}

	ts.failures++
	ts.consecutive++
	if ts.halfOpen {
		ts.openUntil = now.Add(s.cfg.BreakerCooldown)
		ts.halfOpen = false
		return
	}
	if ts.consecutive >= s.cfg.BreakerFailures {
		ts.openUntil = now.Add(s.cfg.BreakerCooldown)
		return
	}
	if ts.total >= s.cfg.MinSamples {
		rate := float64(ts.failures) / float64(ts.total)
		if rate > s.cfg.FailureRate {
			ts.openUntil = now.Add(s.cfg.BreakerCooldown)
		}
	}
}

// Allow reports whether key may be used now (circuit breaker state).
// When the breaker is open and the cooldown elapsed, exactly one probe is allowed.
func (s *State) Allow(key string, now time.Time) (allowed bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.targets[key]
	if !ok || ts.openUntil.IsZero() {
		return true, ""
	}
	if now.Before(ts.openUntil) {
		return false, "circuit_open"
	}
	if !ts.halfOpen {
		ts.halfOpen = true
	}
	return true, "half_open_probe"
}

// Open reports whether the breaker is currently open for key.
func (s *State) Open(key string, now time.Time) bool {
	allowed, _ := s.Allow(key, now)
	return !allowed
}

// Metrics returns a snapshot of one target.
func (s *State) Metrics(key string) Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.targets[key]
	if !ok {
		return Metrics{}
	}
	return Metrics{
		Inflight:    ts.inflight,
		Samples:     ts.samples,
		LatencyEWMA: ts.ewma,
		Total:       ts.total,
		Failures:    ts.failures,
		OpenUntil:   ts.openUntil,
	}
}

// SnapshotMetrics returns metrics for every known target.
func (s *State) SnapshotMetrics() map[string]Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Metrics, len(s.targets))
	for k, ts := range s.targets {
		out[k] = Metrics{
			Inflight:    ts.inflight,
			Samples:     ts.samples,
			LatencyEWMA: ts.ewma,
			Total:       ts.total,
			Failures:    ts.failures,
			OpenUntil:   ts.openUntil,
		}
	}
	return out
}

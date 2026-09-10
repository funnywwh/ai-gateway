package balancer

import (
	"testing"
	"time"
)

func targets(weights ...int) []Target {
	out := make([]Target, 0, len(weights))
	for i, w := range weights {
		out = append(out, Target{Key: string(rune('a' + i)), Weight: w, MaxOutputTokens: 1000, Order: i})
	}
	return out
}

func TestWeightedRandomRespectsWeights(t *testing.T) {
	s := New(DefaultConfig())
	tier := targets(90, 10)

	firstA := 0
	const runs = 6000
	for i := 0; i < runs; i++ {
		if got := s.Order(WeightedRandom, tier); got[0].Key == "a" {
			firstA++
		}
	}
	ratio := float64(firstA) / float64(runs)
	if ratio < 0.85 || ratio > 0.95 {
		t.Fatalf("weighted random first-pick ratio = %.3f, want ~0.90", ratio)
	}
}

func TestWeightedRandomReturnsPermutation(t *testing.T) {
	s := New(DefaultConfig())
	got := s.Order(WeightedRandom, targets(1, 1, 1))
	if len(got) != 3 {
		t.Fatalf("permutation length = %d", len(got))
	}
	seen := map[string]bool{}
	for _, tgt := range got {
		if seen[tgt.Key] {
			t.Fatalf("duplicate entry in permutation: %+v", got)
		}
		seen[tgt.Key] = true
	}
}

func TestRoundRobinCycles(t *testing.T) {
	s := New(DefaultConfig())
	tier := targets(1, 1, 1)

	first := s.Order(RoundRobin, tier)[0].Key
	second := s.Order(RoundRobin, tier)[0].Key
	third := s.Order(RoundRobin, tier)[0].Key
	fourth := s.Order(RoundRobin, tier)[0].Key

	if first == second || second == third || third == fourth {
		t.Fatalf("round robin did not rotate: %s %s %s %s", first, second, third, fourth)
	}
	if first != fourth {
		t.Fatalf("round robin must wrap around: %s vs %s", first, fourth)
	}
}

func TestLeastInflightPrefersIdleTarget(t *testing.T) {
	s := New(DefaultConfig())
	tier := targets(100, 100)
	s.Acquire("a")
	defer s.Release("a")

	if got := s.Order(LeastInflight, tier)[0].Key; got != "b" {
		t.Fatalf("least_inflight picked %q, want b", got)
	}
	s.Release("a")
}

func TestLeastLatencyNormalisesPerToken(t *testing.T) {
	s := New(DefaultConfig())
	tier := []Target{
		{Key: "slow-big", Weight: 100, MaxOutputTokens: 4000},
		{Key: "fast-small", Weight: 100, MaxOutputTokens: 1000},
	}
	for i := 0; i < 10; i++ {
		s.Observe("slow-big", 2000, true)  // 0.5 ms per token
		s.Observe("fast-small", 1000, true) // 1.0 ms per token
	}
	if got := s.Order(LeastLatency, tier)[0].Key; got != "slow-big" {
		t.Fatalf("per-token normalisation must favour the big-but-cheaper model, got %q", got)
	}
}

func TestLeastLatencyPenalisesReasoningModels(t *testing.T) {
	s := New(DefaultConfig())
	tier := []Target{
		{Key: "thinker", Weight: 100, MaxOutputTokens: 1000, Reasoning: true},
		{Key: "plain", Weight: 100, MaxOutputTokens: 1000},
	}
	for i := 0; i < 10; i++ {
		s.Observe("thinker", 400, true) // 0.4 * 1.5 penalty = 0.6
		s.Observe("plain", 500, true)   // 0.5
	}
	if got := s.Order(LeastLatency, tier)[0].Key; got != "plain" {
		t.Fatalf("reasoning models must be de-prioritised, got %q", got)
	}
}

func TestLeastLatencyFallsBackToWeightWhenUnmeasured(t *testing.T) {
	s := New(DefaultConfig())
	tier := []Target{
		{Key: "unmeasured-light", Weight: 10, MaxOutputTokens: 1000},
		{Key: "unmeasured-heavy", Weight: 90, MaxOutputTokens: 1000},
	}
	if got := s.Order(LeastLatency, tier)[0].Key; got != "unmeasured-heavy" {
		t.Fatalf("with no samples the heavier target must lead, got %q", got)
	}
}

func TestStrictOrderIsDeterministic(t *testing.T) {
	s := New(DefaultConfig())
	tier := []Target{
		{Key: "c", Weight: 100, Order: 30},
		{Key: "a", Weight: 10, Order: 10},
		{Key: "b", Weight: 500, Order: 20},
	}
	for i := 0; i < 5; i++ {
		got := s.Order(StrictOrder, tier)
		if got[0].Key != "a" || got[1].Key != "b" || got[2].Key != "c" {
			t.Fatalf("strict order mismatch: %+v", got)
		}
	}
}

func TestBreakerOpensAfterConsecutiveFailuresAndRecovers(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BreakerFailures = 3
	cfg.BreakerCooldown = 50 * time.Millisecond
	s := New(cfg)

	for i := 0; i < 3; i++ {
		if allowed, _ := s.Allow("k", time.Now()); !allowed {
			t.Fatalf("breaker must stay closed before the threshold (i=%d)", i)
		}
		s.Observe("k", 10, false)
	}
	if allowed, reason := s.Allow("k", time.Now()); allowed {
		t.Fatalf("breaker must open after %d consecutive failures (reason=%q)", cfg.BreakerFailures, reason)
	}

	// After the cooldown exactly one half-open probe is allowed.
	time.Sleep(cfg.BreakerCooldown + 10*time.Millisecond)
	allowed, reason := s.Allow("k", time.Now())
	if !allowed || reason != "half_open_probe" {
		t.Fatalf("expected a half-open probe, got allowed=%t reason=%q", allowed, reason)
	}
	// A successful probe closes the breaker.
	s.Observe("k", 10, true)
	if allowed, _ := s.Allow("k", time.Now()); !allowed {
		t.Fatal("breaker must close after a successful probe")
	}
}

func TestBreakerOpensOnFailureRate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSamples = 10
	cfg.FailureRate = 0.6
	cfg.BreakerFailures = 100 // isolate the rate rule
	cfg.BreakerCooldown = time.Minute
	s := New(cfg)

	for i := 0; i < 10; i++ {
		ok := i%2 == 0 // 5 failures out of 10 = 50% (below the threshold)
		s.Observe("k", 5, ok)
	}
	if allowed, _ := s.Allow("k", time.Now()); !allowed {
		t.Fatal("50% failure rate must not open the breaker")
	}
	for i := 0; i < 5; i++ {
		s.Observe("k", 5, false) // now 10/15 = 66% > 60%
	}
	if allowed, _ := s.Allow("k", time.Now()); allowed {
		t.Fatal("failure rate above the threshold must open the breaker")
	}
}

func TestFailingProbeReopensBreaker(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BreakerFailures = 2
	cfg.BreakerCooldown = 20 * time.Millisecond
	s := New(cfg)

	s.Observe("k", 1, false)
	s.Observe("k", 1, false)
	time.Sleep(cfg.BreakerCooldown + 10*time.Millisecond)

	if allowed, reason := s.Allow("k", time.Now()); !allowed || reason != "half_open_probe" {
		t.Fatalf("expected a probe, got %t/%q", allowed, reason)
	}
	s.Observe("k", 1, false) // probe fails
	if allowed, _ := s.Allow("k", time.Now()); allowed {
		t.Fatal("a failed probe must reopen the breaker")
	}
}

func TestMetricsSnapshot(t *testing.T) {
	s := New(DefaultConfig())
	s.Acquire("k")
	s.Observe("k", 100, true)

	m := s.Metrics("k")
	if m.Inflight != 1 || m.Samples != 1 || m.Total != 1 {
		t.Fatalf("metrics mismatch: %+v", m)
	}
	if all := s.SnapshotMetrics(); len(all) != 1 {
		t.Fatalf("snapshot metrics size = %d", len(all))
	}
	s.Release("k")
	if s.Metrics("k").Inflight != 0 {
		t.Fatal("release must decrement in-flight")
	}
}

func TestValidStrategies(t *testing.T) {
	for _, s := range Strategies() {
		if !Valid(s) {
			t.Errorf("%s must be valid", s)
		}
	}
	if Valid("random") {
		t.Error("unknown strategy must be invalid")
	}
}

package quota

import (
	"context"
	"testing"
	"time"
)

func newLimiter() (*Limiter, *time.Time) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := New(8)
	l.SetClock(func() time.Time { return now })
	return l, &now
}

func TestMergeTakesStrictest(t *testing.T) {
	a := Limits{RPM: 60, TPM: 1000, Concurrency: 8}
	b := Limits{RPM: 30, TPM: 0, Concurrency: 4}
	got := Merge(a, b)
	if got.RPM != 30 || got.TPM != 1000 || got.Concurrency != 4 {
		t.Fatalf("merge mismatch: %+v", got)
	}
	if zero := Merge(Limits{}, Limits{}); zero != (Limits{}) {
		t.Fatalf("merging empty limits must stay empty: %+v", zero)
	}
	if one := Merge(Limits{RPM: 5}, Limits{}); one.RPM != 5 {
		t.Fatalf("single-sided limit lost: %+v", one)
	}
}

func TestRPMBoundary(t *testing.T) {
	l, _ := newLimiter()
	ctx := context.Background()
	limits := Limits{RPM: 2}

	for i := 0; i < 2; i++ {
		ticket, err := l.Reserve(ctx, "key:1", limits)
		if err != nil {
			t.Fatalf("reserve %d must succeed: %v", i, err)
		}
		ticket.Release()
	}

	_, err := l.Reserve(ctx, "key:1", limits)
	exceeded, ok := err.(*ExceededError)
	if !ok {
		t.Fatalf("expected an ExceededError, got %v", err)
	}
	if exceeded.Kind != LimitRequests || exceeded.Limit != 2 || exceeded.Used != 2 {
		t.Fatalf("limit details wrong: %+v", exceeded)
	}
	if exceeded.RetryAfter(time.Now()) < 1 {
		t.Fatalf("RetryAfter must be >= 1, got %d", exceeded.RetryAfter(time.Now()))
	}
	if apiErr := exceeded.ToAPIError(); apiErr.Status != 429 {
		t.Fatalf("expected 429, got %d", apiErr.Status)
	}
}

func TestConcurrencyLimitAndRelease(t *testing.T) {
	l, _ := newLimiter()
	ctx := context.Background()
	limits := Limits{Concurrency: 1}

	first, err := l.Reserve(ctx, "key:2", limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(ctx, "key:2", limits); err == nil {
		t.Fatal("second concurrent request must be rejected")
	} else if exceeded, ok := err.(*ExceededError); !ok || exceeded.Kind != LimitConcurrency {
		t.Fatalf("expected a concurrency error, got %v", err)
	}

	first.Release()
	second, err := l.Reserve(ctx, "key:2", limits)
	if err != nil {
		t.Fatalf("reserve after release must succeed: %v", err)
	}
	second.Release()

	// Release is idempotent.
	first.Release()
	if _, _, inflight := l.Snapshot("key:2"); inflight != 0 {
		t.Fatalf("in-flight should be zero, got %d", inflight)
	}
}

func TestTPMSettlesAfterCompletion(t *testing.T) {
	l, now := newLimiter()
	ctx := context.Background()
	limits := Limits{TPM: 100}

	ticket, err := l.Reserve(ctx, "key:3", limits)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Settle(100)
	ticket.Release()

	if _, err := l.Reserve(ctx, "key:3", limits); err == nil {
		t.Fatal("token limit must apply after settlement")
	} else if exceeded, ok := err.(*ExceededError); !ok || exceeded.Kind != LimitTokens {
		t.Fatalf("expected a token error, got %v", err)
	}

	// After the window slides the limit relaxes.
	*now = now.Add(61 * time.Second)
	next, err := l.Reserve(ctx, "key:3", limits)
	if err != nil {
		t.Fatalf("window must slide: %v", err)
	}
	next.Release()
}

func TestScopesAreIsolated(t *testing.T) {
	l, _ := newLimiter()
	ctx := context.Background()
	limits := Limits{RPM: 1}

	a, err := l.Reserve(ctx, "key:a", limits)
	if err != nil {
		t.Fatal(err)
	}
	a.Release()

	if _, err := l.Reserve(ctx, "key:a", limits); err == nil {
		t.Fatal("scope a must be exhausted")
	}
	b, err := l.Reserve(ctx, "key:b", limits)
	if err != nil {
		t.Fatalf("scope b must be independent: %v", err)
	}
	b.Release()
}

func TestSnapshotCountsRequestsAndTokens(t *testing.T) {
	l, _ := newLimiter()
	ctx := context.Background()

	t1, err := l.Reserve(ctx, "key:snap", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t1.Settle(42)
	t2, err := l.Reserve(ctx, "key:snap", Limits{RPM: 10})
	if err != nil {
		t.Fatal(err)
	}

	requests, tokens, inflight := l.Snapshot("key:snap")
	if requests != 2 || tokens != 42 || inflight != 2 {
		t.Fatalf("snapshot mismatch: req=%d tok=%d inflight=%d", requests, tokens, inflight)
	}
	t1.Release()
	t2.Release()
}

func TestCancelledContextIsRejected(t *testing.T) {
	l, _ := newLimiter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Reserve(ctx, "key:ctx", Limits{}); err == nil {
		t.Fatal("a cancelled context must not be admitted")
	}
}

func TestLimitsFromPolicy(t *testing.T) {
	got := LimitsFromPolicy(`{"rpm":60,"tpm":9000,"concurrency":4,"monthly_requests":1000,"monthly_tokens":500000,"monthly_cost_micros":25000000}`)
	want := Limits{RPM: 60, TPM: 9000, Concurrency: 4, MonthlyRequests: 1000, MonthlyTokens: 500000, MonthlyCostMicros: 25000000}
	if got != want {
		t.Fatalf("policy parsing mismatch: %+v", got)
	}
	if empty := LimitsFromPolicy("not-json"); empty != (Limits{}) {
		t.Fatalf("invalid policy must yield empty limits: %+v", empty)
	}
	if none := LimitsFromPolicy(""); none != (Limits{}) {
		t.Fatalf("empty policy must yield empty limits: %+v", none)
	}
}

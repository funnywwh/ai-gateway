package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// These tests drive the real dispatcher against the builtin echo provider, so what is being
// checked is the wiring an operator's max_inflight actually goes through: the ceiling is
// read from the registry snapshot, the queue lives in front of the upstream call, and a
// queued attempt is not counted as load on the upstream.

type stubStore struct{ provider *domain.Provider }

func (s stubStore) GetProvider(context.Context, int64) (*domain.Provider, error) {
	return s.provider, nil
}

func (s stubStore) SetRouteCooldown(context.Context, int64, *time.Time) error { return nil }

// newTestDispatcher builds a dispatcher over a static snapshot holding one echo provider.
// configJSON configures the echo provider (chunks/delay_ms make an attempt take a known
// amount of time).
func newTestDispatcher(t *testing.T, maxInflight int, configJSON string, cfg Config) (*Dispatcher, *domain.Provider, *balancer.State) {
	t.Helper()
	provider := &domain.Provider{
		ID: 1, Name: "echo", Kind: "testecho", Enabled: true,
		ConfigJSON: configJSON, MaxInflight: maxInflight,
	}
	snap := registry.NewSnapshot(nil, []*domain.Provider{provider}, nil, nil, nil, nil, nil)
	bal := balancer.New(balancer.DefaultConfig())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := New(cfg, stubStore{provider: provider}, registry.NewStatic(snap), nil, bal, log)
	return d, provider, bal
}

func echoStreamRequest() *pluginapi.Request {
	return &pluginapi.Request{
		Model:  "testecho",
		Stream: true,
		Input:  []pluginapi.Item{{Type: "message", Role: "user", Content: []byte(`"ping"`)}},
	}
}

func noopEmit(pluginapi.Event) error { return nil }

type streamResult struct {
	attempt Attempt
	err     error
}

// streamAsync starts one attempt and reports its outcome on the returned channel.
func streamAsync(d *Dispatcher, ctx context.Context) <-chan streamResult {
	out := make(chan streamResult, 1)
	go func() {
		_, attempt, err := d.Stream(ctx, 1, echoStreamRequest(), noopEmit)
		out <- streamResult{attempt: attempt, err: err}
	}()
	return out
}

func TestDispatcherQueuesAttemptsBeyondTheProviderCeiling(t *testing.T) {
	// Two chunks 80ms apart: one attempt costs about 160ms of upstream time.
	d, _, bal := newTestDispatcher(t, 1, `{"chunks":2,"delay_ms":80}`, Config{QueueWait: 3 * time.Second})

	first := streamAsync(d, context.Background())
	waitFor(t, func() bool { return d.CapacityStats()[1].Inflight == 1 }, "the first attempt to hold the slot")

	second := streamAsync(d, context.Background())
	waitFor(t, func() bool {
		stats, ok := d.CapacityStats()[1]
		return ok && stats.Waiting == 1
	}, "the second attempt to queue")

	// A queued attempt has not touched the upstream, so least_inflight and the latency
	// EWMA must not see it as load.
	if inflight := bal.Metrics(ProviderKey(1)).Inflight; inflight != 1 {
		t.Fatalf("balancer in-flight = %d while one attempt ran and one queued, want 1", inflight)
	}

	started := time.Now()
	var attempts []Attempt
	for i := 0; i < 2; i++ {
		select {
		case result := <-first:
			if result.err != nil {
				t.Fatalf("the first attempt failed: %v", result.err)
			}
			attempts = append(attempts, result.attempt)
		case result := <-second:
			if result.err != nil {
				t.Fatalf("the queued attempt failed: %v", result.err)
			}
			attempts = append(attempts, result.attempt)
		case <-time.After(5 * time.Second):
			t.Fatal("attempts did not finish")
		}
	}
	elapsed := time.Since(started)

	queued := 0
	for _, attempt := range attempts {
		if attempt.QueueWaitMS > 0 {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("%d of 2 attempts reported waiting, want exactly 1 (a ceiling of 1 serialises them)", queued)
	}
	// The first attempt started ~160ms before this timer, so two serialised attempts need
	// at least one more upstream call's worth of time.
	if elapsed < 100*time.Millisecond {
		t.Fatalf("the queued attempt finished %v after the first: it did not wait for the slot", elapsed)
	}
	if stats := d.CapacityStats()[1]; stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("the gate must be idle once both attempts finished: %+v", stats)
	}
}

func TestDispatcherRunsFreelyWithoutACeiling(t *testing.T) {
	d, _, _ := newTestDispatcher(t, 0, `{"chunks":2,"delay_ms":80}`, Config{QueueWait: 3 * time.Second})

	results := []<-chan streamResult{streamAsync(d, context.Background()), streamAsync(d, context.Background())}
	started := time.Now()
	for i, ch := range results {
		select {
		case result := <-ch:
			if result.err != nil {
				t.Fatalf("attempt %d failed: %v", i, result.err)
			}
			if result.attempt.QueueWaitMS != 0 {
				t.Fatalf("max_inflight = 0 must never make an attempt wait, got %dms", result.attempt.QueueWaitMS)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("attempts did not finish")
		}
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("two unlimited attempts took %v, so they did not run concurrently", elapsed)
	}
	if stats := d.CapacityStats(); len(stats) != 0 {
		t.Fatalf("an unlimited provider must not be tracked: %+v", stats)
	}
}

func TestDispatcherRefusesImmediatelyWhenQueueingIsDisabled(t *testing.T) {
	d, _, _ := newTestDispatcher(t, 1, `{"chunks":2,"delay_ms":120}`, Config{QueueWait: 0})

	holder := streamAsync(d, context.Background())
	waitFor(t, func() bool { return d.CapacityStats()[1].Inflight == 1 }, "the first attempt to hold the slot")

	_, attempt, err := d.Stream(context.Background(), 1, echoStreamRequest(), noopEmit)
	var busy *CapacityError
	if !errors.As(err, &busy) {
		t.Fatalf("a full provider with queueing off must refuse, got %v", err)
	}
	if busy.Reason != CapacityLimitReached {
		t.Fatalf("reason = %q, want %q", busy.Reason, CapacityLimitReached)
	}
	if attempt.QueueWaitMS != 0 {
		t.Fatalf("a refusal must not report a wait, got %dms", attempt.QueueWaitMS)
	}
	// The refusal must stay retryable: it says nothing about the upstream's health, and the
	// request still has candidates to try.
	if !Retryable(err) {
		t.Fatal("a capacity refusal must be retryable so the request can fail over")
	}
	if stats := d.CapacityStats()[1]; stats.QueueFull != 0 || stats.TimedOut != 0 {
		t.Fatalf("an immediate refusal is neither a timeout nor a full queue: %+v", stats)
	}

	select {
	case result := <-holder:
		if result.err != nil {
			t.Fatalf("the holding attempt failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the holding attempt did not finish")
	}
}

func TestDispatcherPicksUpARaisedCeilingWithoutRestart(t *testing.T) {
	d, provider, _ := newTestDispatcher(t, 1, `{"chunks":2,"delay_ms":200}`, Config{QueueWait: 3 * time.Second})

	holder := streamAsync(d, context.Background())
	waitFor(t, func() bool { return d.CapacityStats()[1].Inflight == 1 }, "the first attempt to hold the slot")
	queued := streamAsync(d, context.Background())
	waitFor(t, func() bool {
		stats, ok := d.CapacityStats()[1]
		return ok && stats.Waiting == 1
	}, "the second attempt to queue")

	// What a registry reload after an operator raises max_inflight does. The swap happens
	// while the queued attempt sits in the gate (which holds no reference to the registry),
	// so this mirrors the reload path without racing a reader.
	raised := *provider
	raised.MaxInflight = 3
	d.reg = registry.NewStatic(registry.NewSnapshot(nil, []*domain.Provider{&raised}, nil, nil, nil, nil, nil))
	d.SyncLimits()

	waitFor(t, func() bool {
		stats, ok := d.CapacityStats()[1]
		return ok && stats.Limit == 3
	}, "the raised ceiling to reach the gate")

	// The queue must drain without waiting for the holder to finish.
	select {
	case result := <-queued:
		if result.err != nil {
			t.Fatalf("the queued attempt failed after the ceiling was raised: %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("raising the ceiling must release the attempts already waiting")
	}
	select {
	case result := <-holder:
		if result.err != nil {
			t.Fatalf("the holding attempt failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the holding attempt did not finish")
	}
	if policy := d.CapacityPolicy(); policy.QueueWaitS != 3 || policy.QueueMaxWaiters != 0 {
		t.Fatalf("policy = %+v, want the configured 3s with no depth limit", policy)
	}
}

func TestDispatcherReportsAQueueTimeoutAsARetryableRefusal(t *testing.T) {
	d, _, _ := newTestDispatcher(t, 1, `{"chunks":2,"delay_ms":400}`, Config{QueueWait: 40 * time.Millisecond})

	holder := streamAsync(d, context.Background())
	waitFor(t, func() bool { return d.CapacityStats()[1].Inflight == 1 }, "the first attempt to hold the slot")

	_, attempt, err := d.Stream(context.Background(), 1, echoStreamRequest(), noopEmit)
	var busy *CapacityError
	if !errors.As(err, &busy) || busy.Reason != CapacityWaitTimeout {
		t.Fatalf("a wait that runs out must report %q, got %v", CapacityWaitTimeout, err)
	}
	if !errors.Is(err, ErrProviderBusy) || !Retryable(err) {
		t.Fatal("a queue timeout must stay recognisable and retryable")
	}
	// A refused attempt never dispatched, so its Attempt is empty: the wait it paid is
	// reported on the refusal itself (which is what the request path logs).
	if attempt.QueueWaitMS != 0 {
		t.Fatalf("a refused attempt has no dispatch to describe, got %dms", attempt.QueueWaitMS)
	}
	if busy.Waited < 40*time.Millisecond {
		t.Fatalf("the refusal must report the wait it paid, got %v", busy.Waited)
	}
	if stats := d.CapacityStats()[1]; stats.TimedOut != 1 {
		t.Fatalf("the timeout must be counted: %+v", stats)
	}

	select {
	case result := <-holder:
		if result.err != nil {
			t.Fatalf("the holding attempt failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the holding attempt did not finish")
	}
}

func TestDispatcherReleasesTheSlotWhenTheClientGoesAway(t *testing.T) {
	d, _, _ := newTestDispatcher(t, 1, `{"chunks":2,"delay_ms":100}`, Config{QueueWait: 3 * time.Second})

	holder := streamAsync(d, context.Background())
	waitFor(t, func() bool { return d.CapacityStats()[1].Inflight == 1 }, "the first attempt to hold the slot")

	waitCtx, cancel := context.WithCancel(context.Background())
	queued := make(chan streamResult, 1)
	go func() {
		_, attempt, err := d.Stream(waitCtx, 1, echoStreamRequest(), noopEmit)
		queued <- streamResult{attempt: attempt, err: err}
	}()
	waitFor(t, func() bool {
		stats, ok := d.CapacityStats()[1]
		return ok && stats.Waiting == 1
	}, "the second attempt to queue")

	cancel()
	select {
	case result := <-queued:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("a cancelled waiter must report the context error, got %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter must return at once instead of waiting out its budget")
	}

	select {
	case result := <-holder:
		if result.err != nil {
			t.Fatalf("the holding attempt failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the holding attempt did not finish")
	}

	// The slot must be reusable: a cancellation must not leak one.
	after := streamAsync(d, context.Background())
	select {
	case result := <-after:
		if result.err != nil {
			t.Fatalf("the attempt after a cancellation failed: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the slot was lost after a cancelled waiter")
	}
	if stats := d.CapacityStats()[1]; stats.Cancelled != 1 || stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("gate state after a cancellation: %+v", stats)
	}
}

// TestDispatcherServesQueuedAttemptsSerially pins the ceiling itself: with a ceiling of 2 and
// six attempts, the provider must never see more than two at once.
func TestDispatcherServesQueuedAttemptsSerially(t *testing.T) {
	d, _, _ := newTestDispatcher(t, 2, `{"chunks":2,"delay_ms":30}`, Config{QueueWait: 10 * time.Second})

	var live, peak int64
	results := make([]<-chan streamResult, 0, 6)
	for i := 0; i < 6; i++ {
		out := make(chan streamResult, 1)
		results = append(results, out)
		go func() {
			_, attempt, err := d.Stream(context.Background(), 1, echoStreamRequest(), func(pluginapi.Event) error {
				current := atomic.AddInt64(&live, 1)
				for {
					observed := atomic.LoadInt64(&peak)
					if current <= observed || atomic.CompareAndSwapInt64(&peak, observed, current) {
						break
					}
				}
				atomic.AddInt64(&live, -1)
				return nil
			})
			out <- streamResult{attempt: attempt, err: err}
		}()
	}
	for i, ch := range results {
		select {
		case result := <-ch:
			if result.err != nil {
				t.Fatalf("attempt %d failed: %v", i, result.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("attempts did not finish")
		}
	}
	if peak > 2 {
		t.Fatalf("the provider saw %d concurrent attempts, above its ceiling of 2", peak)
	}
}

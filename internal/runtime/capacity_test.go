package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the gate's two promises: the ceiling is never exceeded, and a request
// that finds it full waits its turn (FIFO) instead of failing.

func testGate(wait time.Duration, maxWaiters int) *gate {
	return newGate(Config{QueueWait: wait, QueueMaxWaiters: maxWaiters})
}

// waitFor polls cond until it holds. The gate is a concurrent state machine, so the tests
// observe it (a queue depth, a refusal counter) rather than reaching into it.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestGateUnlimitedAdmitsEverythingWithoutState(t *testing.T) {
	g := testGate(time.Second, 4)
	for i := 0; i < 5; i++ {
		p, err := g.acquire(context.Background(), 7, 0, "unlimited")
		if err != nil {
			t.Fatalf("attempt %d on an unlimited provider was refused: %v", i, err)
		}
		if p.waitedFor() != 0 {
			t.Fatalf("an unlimited provider must never make an attempt wait")
		}
		p.Release()
	}
	if stats := g.stats(); len(stats) != 0 {
		t.Fatalf("an unlimited provider must not be tracked at all: %+v", stats)
	}
}

func TestGateAdmitsExactlyTheCeilingAndQueuesTheRest(t *testing.T) {
	g := testGate(2*time.Second, 0)
	ctx := context.Background()
	held := make([]*permit, 0, 2)
	for i := 0; i < 2; i++ {
		p, err := g.acquire(ctx, 1, 2, "p")
		if err != nil {
			t.Fatalf("attempt %d inside the ceiling was admitted: %v", i, err)
		}
		held = append(held, p)
	}

	admitted := make(chan *permit, 1)
	waited := make(chan time.Duration, 1)
	go func() {
		p, err := g.acquire(ctx, 1, 2, "p")
		if err != nil {
			t.Errorf("a queued attempt must not fail: %v", err)
			admitted <- nil
			return
		}
		waited <- p.waitedFor()
		admitted <- p
	}()
	waitFor(t, func() bool { return g.stats()[1].Waiting == 1 }, "the excess attempt to queue")

	select {
	case p := <-admitted:
		if p != nil {
			p.Release()
		}
		t.Fatal("a third attempt must not run while both slots are held")
	case <-time.After(50 * time.Millisecond):
	}

	held[0].Release()
	var queued *permit
	select {
	case queued = <-admitted:
	case <-time.After(time.Second):
		t.Fatal("releasing a slot must admit the longest-waiting attempt")
	}
	if queued == nil {
		t.Fatal("the queued attempt failed instead of waiting")
	}
	if w := <-waited; w <= 0 {
		t.Fatalf("a queued attempt must report how long it waited, got %v", w)
	}
	queued.Release()
	held[1].Release()

	if stats := g.stats()[1]; stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("the gate must be empty once every permit is released: %+v", stats)
	}
}

func TestGateHandsTheSlotToTheLongestWaiterFirst(t *testing.T) {
	g := testGate(2*time.Second, 0)
	ctx := context.Background()
	first, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	order := []int{}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := g.acquire(ctx, 1, 1, "p")
			if err != nil {
				t.Errorf("waiter %d was refused: %v", i, err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			p.Release()
		}(i)
		// Wait until this waiter is queued, so arrival order is a fact of the test rather
		// than a hope about goroutine scheduling.
		want := i + 1
		waitFor(t, func() bool {
			stats, ok := g.stats()[1]
			return ok && stats.Waiting == want
		}, "each waiter to join the queue in order")
	}

	first.Release()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Fatalf("queued attempts must be served in arrival order, got %v", order)
	}
}

func TestGateTimesOutAWaitThatFindsNoSlot(t *testing.T) {
	g := testGate(30*time.Millisecond, 0)
	held, err := g.acquire(context.Background(), 1, 1, "busy")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	started := time.Now()
	_, err = g.acquire(context.Background(), 1, 1, "busy")
	elapsed := time.Since(started)

	var busy *CapacityError
	if !errors.As(err, &busy) {
		t.Fatalf("a wait that runs out must return a CapacityError, got %v", err)
	}
	if busy.Reason != CapacityWaitTimeout {
		t.Fatalf("reason = %q, want %q", busy.Reason, CapacityWaitTimeout)
	}
	if busy.Provider != "busy" || busy.Limit != 1 {
		t.Fatalf("the refusal must name the provider and its limit: %+v", busy)
	}
	if !errors.Is(err, ErrProviderBusy) {
		t.Fatal("every capacity refusal must wrap ErrProviderBusy so it stays retryable")
	}
	if busy.RetryAfterSeconds() < 1 {
		t.Fatalf("Retry-After hint = %d, want at least 1", busy.RetryAfterSeconds())
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("the attempt gave up after %v, before its budget of 30ms", elapsed)
	}
	if stats := g.stats()[1]; stats.Waiting != 0 || stats.TimedOut != 1 {
		t.Fatalf("a timed-out waiter must leave the queue and be counted: %+v", stats)
	}
}

func TestGateRefusesImmediatelyWhenTheQueueIsFull(t *testing.T) {
	g := testGate(2*time.Second, 2)
	ctx := context.Background()
	held, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}

	queued := make(chan *permit, 2)
	for i := 0; i < 2; i++ {
		go func() {
			p, err := g.acquire(ctx, 1, 1, "p")
			if err != nil {
				t.Errorf("a queued attempt must not fail: %v", err)
			}
			queued <- p
		}()
	}
	waitFor(t, func() bool { return g.stats()[1].Waiting == 2 }, "the queue to fill up")

	_, err = g.acquire(ctx, 1, 1, "p")
	var busy *CapacityError
	if !errors.As(err, &busy) || busy.Reason != CapacityQueueFull {
		t.Fatalf("a full queue must refuse with %q, got %v", CapacityQueueFull, err)
	}
	if busy.Waiters != 2 {
		t.Fatalf("the refusal must report the queue depth, got %d", busy.Waiters)
	}
	if stats := g.stats()[1]; stats.QueueFull != 1 {
		t.Fatalf("the refusal must be counted: %+v", stats)
	}

	// The attempts that were queued when it was their turn still get served.
	held.Release()
	for i := 0; i < 2; i++ {
		select {
		case p := <-queued:
			if p == nil {
				t.Fatal("a queued attempt failed")
			}
			p.Release()
		case <-time.After(time.Second):
			t.Fatal("queued attempts must still be served after a refusal")
		}
	}
}

func TestGateRefusesImmediatelyWhenQueueingIsDisabled(t *testing.T) {
	g := testGate(0, 10)
	ctx := context.Background()
	held, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	_, err = g.acquire(ctx, 1, 1, "p")
	var busy *CapacityError
	if !errors.As(err, &busy) || busy.Reason != CapacityLimitReached {
		t.Fatalf("with queueing off a full provider must refuse at once, got %v", err)
	}
	if busy.Waited != 0 {
		t.Fatalf("an immediate refusal must not report a wait, got %v", busy.Waited)
	}
}

func TestGatePassesACancelledWaitersSlotToTheNextInLine(t *testing.T) {
	g := testGate(2*time.Second, 0)
	ctx := context.Background()
	held, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	firstErr := make(chan error, 1)
	go func() {
		_, err := g.acquire(cancelCtx, 1, 1, "p")
		firstErr <- err
	}()
	waitFor(t, func() bool { return g.stats()[1].Waiting == 1 }, "the first waiter to queue")

	second := make(chan *permit, 1)
	go func() {
		p, err := g.acquire(ctx, 1, 1, "p")
		if err != nil {
			t.Errorf("the second waiter was refused: %v", err)
		}
		second <- p
	}()
	waitFor(t, func() bool { return g.stats()[1].Waiting == 2 }, "the second waiter to queue")

	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled waiter must report the context error, got %v", err)
	}
	held.Release()

	select {
	case p := <-second:
		if p == nil {
			t.Fatal("the slot released after a cancellation was lost")
		}
		p.Release()
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter's slot must go to the next attempt in line")
	}
	stats := g.stats()[1]
	if stats.Cancelled != 1 || stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("gate state after a cancellation: %+v", stats)
	}
}

func TestGateReleasesQueuedAttemptsWhenTheCeilingRises(t *testing.T) {
	g := testGate(2*time.Second, 0)
	ctx := context.Background()
	held, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}

	admitted := make(chan *permit, 2)
	for i := 0; i < 2; i++ {
		go func() {
			p, err := g.acquire(ctx, 1, 1, "p")
			if err != nil {
				t.Errorf("a queued attempt failed: %v", err)
			}
			admitted <- p
		}()
	}
	waitFor(t, func() bool { return g.stats()[1].Waiting == 2 }, "both attempts to queue")

	// This is what a registry reload does after an operator raises max_inflight: the queue
	// must drain without waiting for a release that already happened.
	g.setLimit(1, 3)
	for i := 0; i < 2; i++ {
		select {
		case p := <-admitted:
			if p == nil {
				t.Fatal("a queued attempt failed")
			}
			p.Release()
		case <-time.After(time.Second):
			t.Fatal("raising the ceiling must release the attempts already waiting")
		}
	}
	held.Release()
	if stats := g.stats()[1]; stats.Limit != 3 || stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("stat after the ceiling was raised: %+v", stats)
	}
}

func TestGateLoweredCeilingHoldsNewAttemptsUntilItIsRespected(t *testing.T) {
	g := testGate(2*time.Second, 0)
	ctx := context.Background()
	held := make([]*permit, 0, 3)
	for i := 0; i < 3; i++ {
		p, err := g.acquire(ctx, 1, 3, "p")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, p)
	}

	g.setLimit(1, 1) // the operator drops the ceiling to 1 while three attempts run
	admitted := make(chan *permit, 1)
	go func() {
		p, err := g.acquire(ctx, 1, 1, "p")
		if err != nil {
			t.Errorf("queued attempt failed: %v", err)
		}
		admitted <- p
	}()
	waitFor(t, func() bool { return g.stats()[1].Waiting == 1 }, "the attempt to queue behind a lowered ceiling")

	held[1].Release()
	held[2].Release()
	// One attempt still runs, which is exactly the lowered ceiling: the queued attempt
	// must keep waiting rather than bring the count back up.
	select {
	case p := <-admitted:
		if p != nil {
			p.Release()
		}
		t.Fatal("an attempt must not run while the in-flight count still fills the lowered ceiling")
	case <-time.After(50 * time.Millisecond):
	}
	held[0].Release()
	select {
	case p := <-admitted:
		if p == nil {
			t.Fatal("attempt failed")
		}
		p.Release()
	case <-time.After(time.Second):
		t.Fatal("the queued attempt must run once the lowered ceiling is respected")
	}
}

func TestGateReleaseIsIdempotentAndNilSafe(t *testing.T) {
	g := testGate(time.Second, 0)
	ctx := context.Background()
	p, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	p.Release()
	var nilPermit *permit
	nilPermit.Release()

	if stats := g.stats()[1]; stats.Inflight != 0 {
		t.Fatalf("releasing twice must not corrupt the count: %+v", stats)
	}
	again, err := g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatalf("the slot must be reusable after a double release: %v", err)
	}
	again.Release()
}

func TestGateCountsWhatItDid(t *testing.T) {
	g := testGate(20*time.Millisecond, 1)
	ctx := context.Background()
	held, err := g.acquire(ctx, 1, 1, "p") // admitted immediately
	if err != nil {
		t.Fatal(err)
	}

	// A queued attempt that times out.
	if _, err := g.acquire(ctx, 1, 1, "p"); !errors.Is(err, ErrProviderBusy) {
		t.Fatalf("expected a capacity refusal, got %v", err)
	}
	// A queued attempt that is refused because the queue (depth 1) is occupied: the first
	// refusal already emptied it, so this one queues and times out again.
	if _, err := g.acquire(ctx, 1, 1, "p"); !errors.Is(err, ErrProviderBusy) {
		t.Fatalf("expected a capacity refusal, got %v", err)
	}
	held.Release()

	// A queued attempt that the caller cancels.
	held, err = g.acquire(ctx, 1, 1, "p")
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.acquire(cancelCtx, 1, 1, "p")
	}()
	waitFor(t, func() bool { return g.stats()[1].Waiting == 1 }, "the waiter to queue")
	cancel()
	<-done
	held.Release()

	stats := g.stats()[1]
	if stats.Admitted != 2 {
		// Two acquisitions went straight through; the two that queued timed out and the
		// one that was cancelled never ran.
		t.Fatalf("admitted = %d, want 2", stats.Admitted)
	}
	if stats.TimedOut != 2 {
		t.Fatalf("timed_out = %d, want 2", stats.TimedOut)
	}
	if stats.Cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", stats.Cancelled)
	}
	if stats.WaitTotalMS <= 0 {
		t.Fatalf("waiting time must be accumulated, got %d", stats.WaitTotalMS)
	}
	if stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("gate must be empty at the end: %+v", stats)
	}
}

func TestGateNeverExceedsTheCeilingUnderConcurrency(t *testing.T) {
	g := testGate(5*time.Second, 0)
	const workers, ceiling = 200, 8

	var live, peak int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := g.acquire(context.Background(), 1, ceiling, "p")
			if err != nil {
				t.Errorf("attempt refused: %v", err)
				return
			}
			current := atomic.AddInt64(&live, 1)
			for {
				observed := atomic.LoadInt64(&peak)
				if current <= observed || atomic.CompareAndSwapInt64(&peak, observed, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&live, -1)
			p.Release()
		}()
	}
	wg.Wait()

	if peak > ceiling {
		t.Fatalf("peak concurrency %d exceeded the ceiling %d", peak, ceiling)
	}
	if peak < 2 {
		t.Fatalf("the test never exercised concurrency (peak %d)", peak)
	}
	if stats := g.stats()[1]; stats.Inflight != 0 || stats.Waiting != 0 {
		t.Fatalf("gate must be empty after the burst: %+v", stats)
	}
}

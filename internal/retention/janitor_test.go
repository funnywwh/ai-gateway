package retention

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore serves a fixed number of due rows and records how it was called.
type fakeStore struct {
	logsLeft      int
	responsesLeft int
	calls         []string
	batchSizes    []int
	failWith      error
	// seenCutoff records the cutoff the janitor asked for, so the window itself is
	// asserted and not just the row counts.
	seenCutoff time.Time
	seenNow    time.Time
}

func (f *fakeStore) PruneRequestLogs(_ context.Context, before time.Time, limit int) (int, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.calls = append(f.calls, "logs")
	f.batchSizes = append(f.batchSizes, limit)
	f.seenCutoff = before
	deleted := min(f.logsLeft, limit)
	f.logsLeft -= deleted
	return deleted, nil
}

func (f *fakeStore) PruneExpiredResponses(_ context.Context, now time.Time, limit int) (int, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.calls = append(f.calls, "responses")
	f.batchSizes = append(f.batchSizes, limit)
	f.seenNow = now
	deleted := min(f.responsesLeft, limit)
	f.responsesLeft -= deleted
	return deleted, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunDeletesInBatchesUntilDrained(t *testing.T) {
	store := &fakeStore{logsLeft: 1200, responsesLeft: 3}
	j := New(store, Config{RetentionDays: 30, BatchSize: 500}, quiet())

	result, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.RequestLogs != 1200 || result.Responses != 3 {
		t.Fatalf("result = %+v, want 1200 logs and 3 responses", result)
	}
	// 500 + 500 + 200 for the logs, then one short batch for the responses.
	if result.Batches != 4 {
		t.Fatalf("batches = %d, want 4", result.Batches)
	}
	if result.Exhausted || result.Disabled || result.Busy {
		t.Fatalf("flags = %+v, want a completed pass", result)
	}
	if got := j.PrunedTotal(); got != 1203 {
		t.Fatalf("pruned total = %d, want 1203", got)
	}
	// The window is now - retention_days, not "now".
	age := time.Since(store.seenCutoff)
	if age < 29*24*time.Hour || age > 31*24*time.Hour {
		t.Fatalf("cutoff is %v old, want ~30 days", age)
	}
	if time.Since(store.seenNow) > time.Minute {
		t.Fatalf("responses were pruned against %v, want now", store.seenNow)
	}
}

func TestRunStopsAtTheBatchBudget(t *testing.T) {
	store := &fakeStore{logsLeft: 10000, responsesLeft: 10000}
	j := New(store, Config{RetentionDays: 7, BatchSize: 100, MaxBatches: 3}, quiet())

	result, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Exhausted {
		t.Fatalf("a pass that spent its budget with rows left must say so: %+v", result)
	}
	if result.RequestLogs != 300 || result.Responses != 300 {
		t.Fatalf("each table gets its own budget: %+v", result)
	}
	if result.Batches != 6 {
		t.Fatalf("batches = %d, want 6 (3 + 3)", result.Batches)
	}
}

func TestRunIsDisabledWithoutARetentionWindow(t *testing.T) {
	store := &fakeStore{logsLeft: 100}
	j := New(store, Config{RetentionDays: 0}, quiet())

	result, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Disabled {
		t.Fatalf("retention_days <= 0 must disable cleanup: %+v", result)
	}
	if len(store.calls) != 0 {
		t.Fatalf("a disabled janitor must not touch the store: %v", store.calls)
	}
	if j.RetentionDays() != 0 {
		t.Fatalf("retention days = %d", j.RetentionDays())
	}
}

func TestRunReportsFailures(t *testing.T) {
	store := &fakeStore{failWith: errors.New("database is locked")}
	j := New(store, Config{RetentionDays: 30}, quiet())

	if _, err := j.Run(context.Background()); err == nil {
		t.Fatal("a failing store must surface the error")
	}
	if !strings.Contains(j.LastError(), "database is locked") {
		t.Fatalf("last error = %q", j.LastError())
	}
	if j.PrunedTotal() != 0 {
		t.Fatalf("a failed pass must not count rows")
	}

	// A later successful pass clears the error and records when it ran.
	store.failWith = nil
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j.LastError() != "" {
		t.Fatalf("last error = %q, want empty after a good pass", j.LastError())
	}
	if j.LastRun().IsZero() {
		t.Fatal("a completed pass must record its timestamp")
	}
}

func TestRunDoesNotOverlapItself(t *testing.T) {
	store := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	j := New(store, Config{RetentionDays: 30, BatchSize: 1, MaxBatches: 1}, quiet())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = j.Run(context.Background())
	}()
	<-store.entered

	result, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Busy {
		t.Fatalf("a second concurrent pass must report Busy instead of queueing: %+v", result)
	}
	close(store.release)
	<-done
}

// blockingStore holds its first call until the test releases it, so a pass can be caught
// in flight.
type blockingStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStore) PruneRequestLogs(_ context.Context, _ time.Time, _ int) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, nil
}

func (b *blockingStore) PruneExpiredResponses(_ context.Context, _ time.Time, _ int) (int, error) {
	return 0, nil
}

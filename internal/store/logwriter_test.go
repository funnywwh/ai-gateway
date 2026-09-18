package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

// batchFailingStore fails the batched write and delegates the per-row write, which is what
// a real batch failure looks like from the writer's side.
type batchFailingStore struct {
	*DB
	failBatch bool
}

func (s *batchFailingStore) PutAuditBatch(ctx context.Context, batch []pendingRecord) error {
	if s.failBatch {
		return errors.New("store: commit audit batch: database is locked")
	}
	return s.DB.PutAuditBatch(ctx, batch)
}

func TestBatchFailureRetriesRowByRow(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "batch.db")
	db, err := Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := &batchFailingStore{DB: db, failBatch: true}
	var reported []string
	w := NewLogWriter(store, LogWriterConfig{
		FlushInterval: 10 * time.Millisecond, MaxBatch: 8, MaxBytes: 1 << 20,
	}, nil, func(rec *domain.RequestLogRecord, err error) {
		reported = append(reported, rec.RequestID)
	})
	defer w.Close(ctx)

	for _, id := range []string{"req_a", "req_b", "req_c"} {
		w.EnqueueRecording(nil, &domain.RequestLogRecord{
			RequestID: id, Status: "completed", CreatedAt: time.Now().UTC(),
		})
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Every row must survive the failed batch through the per-row retry.
	for _, id := range []string{"req_a", "req_b", "req_c"} {
		if _, err := db.GetRequestLog(ctx, id); err != nil {
			t.Fatalf("row %s lost after a failed batch: %v", id, err)
		}
	}
	if len(reported) != 0 {
		t.Fatalf("no row needed the skeleton fallback, got %v", reported)
	}
	if got := w.Stats()["failed_rows"].(int64); got != 0 {
		t.Fatalf("failed rows = %d, want 0", got)
	}
	if got := w.Stats()["batched_requests"].(int64); got != 3 {
		t.Fatalf("written = %d, want 3", got)
	}
}

// slowStore blocks the batched write so the queue can be seen filling up. started reports
// that a write is in flight, which is the state the test needs before it can fill the
// queue: the flusher takes the whole queue at once, so the queue is only full while a
// write is stuck.
type slowStore struct {
	*DB
	started chan struct{}
	release chan struct{}
}

func (s *slowStore) PutAuditBatch(ctx context.Context, batch []pendingRecord) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.DB.PutAuditBatch(ctx, batch)
}

// TestFullQueueAppliesBackpressure pins the limit that keeps the queue from growing
// without bound: once QueueRows is reached Enqueue waits for the writer, and a row that
// still cannot be queued is reported rather than silently held in memory.
func TestFullQueueAppliesBackpressure(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "backpressure.db")
	db, err := Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	slow := &slowStore{DB: db, started: make(chan struct{}, 1), release: make(chan struct{})}
	var reported int
	w := NewLogWriter(slow, LogWriterConfig{
		FlushInterval: 10 * time.Millisecond,
		MaxBatch:      1, // the shrink-wrapped version of a writer that cannot keep up
		MaxBytes:      1 << 20,
		QueueRows:     2,
		QueueBytes:    1 << 20,
		MaxWait:       150 * time.Millisecond,
	}, nil, func(*domain.RequestLogRecord, error) { reported++ })
	defer func() {
		close(slow.release)
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	// Get the writer stuck first, then fill the queue behind it.
	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "req_bp0", CreatedAt: time.Now().UTC()})
	select {
	case <-slow.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the writer never started its batch")
	}

	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "req_bp1", CreatedAt: time.Now().UTC()})
	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "req_bp2", CreatedAt: time.Now().UTC()})

	started := time.Now()
	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "req_bp3", CreatedAt: time.Now().UTC()})
	waited := time.Since(started)

	if waited < 100*time.Millisecond {
		t.Fatalf("a full queue must make the producer wait, waited only %s", waited)
	}
	if reported != 1 {
		t.Fatalf("the row that could not be queued must be reported, got %d", reported)
	}
	if got := w.Stats()["backpressure"].(int64); got != 1 {
		t.Fatalf("backpressure episodes = %d, want 1", got)
	}
	if got := w.Stats()["queued_requests"].(int); got > 2 {
		t.Fatalf("queue must stay bounded, holds %d", got)
	}
}

func TestStatsReportQueueDepth(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "depth.db")
	db, err := Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A long interval keeps the rows queued so the depth is observable.
	w := NewLogWriter(db, LogWriterConfig{
		FlushInterval: time.Hour, MaxBatch: 64, MaxBytes: 1 << 20,
	}, nil, nil)
	defer w.Close(ctx)

	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "req_q1", CreatedAt: time.Now().UTC()})
	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "req_q2", CreatedAt: time.Now().UTC()})
	st := w.Stats()
	if st["queued_requests"].(int) != 2 {
		t.Fatalf("queued = %v, want 2", st["queued_requests"])
	}
}

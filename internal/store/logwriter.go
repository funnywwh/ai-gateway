package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// LogWriter batches the two per-request audit writes (the `responses` row and the
// `request_logs` row) into one background transaction instead of two synchronous ones.
//
// Why: profiling the running 8088 instance showed the request path spending about half of
// the gateway's CPU in the persistence layer, and — more importantly — waiting on it. The
// writer pool is deliberately a single connection (`store/db.go`), so every request paid
// one round trip through SQLite's write lock plus an fsync per row; a goroutine dump taken
// under load caught six request goroutines parked in `database/sql.(*DB).conn` waiting for
// that connection. Batching turns N transactions into one per flush, so the fsync is
// amortised 256-fold and the write lock is held once instead of N times.
//
// Durability: the response has already been sent to the client before these rows are
// queued, so nothing here delays a client. What changes is when the audit row lands: up to
// one flush interval later (default 250ms). A hard kill inside that window loses the last
// interval's rows, which is the accepted trade for the throughput — and the alternative it
// replaced (a synchronous write that blocks the whole process's write lock) could lose
// them anyway by timing out. `Close` drains everything before the store closes, so an
// orderly shutdown loses nothing.
//
// Ordering: a request's responses row is written before its request_logs row, inside the
// same transaction, so a reader never sees a request log whose stored response is missing.
// auditStore is the persistence the writer needs. *DB implements it; tests inject a
// wrapper that fails the batched write so the per-row fallback can be exercised.
type auditStore interface {
	PutAuditBatch(ctx context.Context, batch []pendingRecord) error
	putAuditSync(ctx context.Context, resp *domain.ResponseRecord, log *domain.RequestLogRecord) error
}

type LogWriter struct {
	db  auditStore
	cfg LogWriterConfig
	log Logger
	// onFailure is called for every row that could not be written in a batch. The HTTP
	// layer owns the retry policy (it writes a content-free skeleton row and counts the
	// failures), so the batch layer reports instead of deciding.
	onFailure func(*domain.RequestLogRecord, error)

	mu      sync.Mutex
	notify  chan struct{}
	queue   []pendingRecord
	bytes   int
	closed  bool
	started bool
	done    chan struct{}
	// pendingResp holds the response ids still queued (or being written). A client that
	// POSTs and then immediately GETs /v1/responses/{id} must not see a 404 just because
	// the row is still in this queue: AwaitResponse closes that window for exactly the
	// ids a reader asks about, instead of making every write synchronous again.
	pendingResp map[string]int

	// counters, for /stats
	written  int64
	batches  int64
	failures int64
	// waiting is set while producers are blocked on a full queue; blocked counts the
	// episodes so an operator can see backpressure without reading logs.
	waiting bool
	blocked int64
}

// pendingRecord is one request's audit work.
type pendingRecord struct {
	resp *domain.ResponseRecord
	log  *domain.RequestLogRecord
	size int
}

// LogWriterConfig bounds the batching.
type LogWriterConfig struct {
	// FlushInterval is how long a queued row may wait before it is written. Smaller
	// means fresher audit rows and more transactions.
	FlushInterval time.Duration
	// MaxBatch caps how many requests share one transaction.
	MaxBatch int
	// MaxBytes caps the payload a single transaction carries. The local deployment's
	// request bodies average ~700KB, so a record cap alone would let one transaction
	// hold hundreds of megabytes.
	MaxBytes int
	// QueueRows and QueueBytes bound what may wait in memory. When either limit is hit,
	// Enqueue applies backpressure (it waits for the flusher) instead of growing without
	// limit: a queue that grows faster than the writer drains it is unbounded memory, and
	// it also means an orderly shutdown cannot finish inside its timeout. Measured on the
	// local deployment with 700KB bodies: sustained 116 rps produced 350MB of queued rows
	// and a drain that timed out.
	QueueRows  int
	QueueBytes int
	// MaxWait bounds how long Enqueue blocks on a full queue before it gives up and
	// reports the row to the failure handler. Zero waits as long as the context allows.
	MaxWait time.Duration
}

// DefaultLogWriterConfig is the batching used when the caller does not override it.
func DefaultLogWriterConfig() LogWriterConfig {
	return LogWriterConfig{
		FlushInterval: 250 * time.Millisecond,
		MaxBatch:      256,
		MaxBytes:      16 << 20,
		QueueRows:     4096,
		QueueBytes:    32 << 20,
	}
}

// Logger is the subset of slog this layer needs; a nil logger is allowed.
type Logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// NewLogWriter starts the background flusher. Note the interface parameter: callers pass
// *DB, tests pass a wrapper that fails the batched write to exercise the per-row fallback.
func NewLogWriter(db auditStore, cfg LogWriterConfig, log Logger, onFailure func(*domain.RequestLogRecord, error)) *LogWriter {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultLogWriterConfig().FlushInterval
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = DefaultLogWriterConfig().MaxBatch
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultLogWriterConfig().MaxBytes
	}
	if cfg.QueueRows <= 0 {
		cfg.QueueRows = DefaultLogWriterConfig().QueueRows
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = DefaultLogWriterConfig().QueueBytes
	}
	w := &LogWriter{
		db: db, cfg: cfg, log: log, onFailure: onFailure,
		done: make(chan struct{}), notify: make(chan struct{}, 1),
		pendingResp: map[string]int{},
	}
	w.started = true
	go w.loop()
	return w
}

// SetFailureHandler installs the callback invoked for every row the writer could not
// store. The composition root calls it once both the store and the transport exist.
func (w *LogWriter) SetFailureHandler(fn func(*domain.RequestLogRecord, error)) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.onFailure = fn
	w.mu.Unlock()
}

// EnqueueRecording queues one request's audit rows (the optional stored response and the
// request log). It satisfies the transport layer's LogRecorder interface.
func (w *LogWriter) EnqueueRecording(resp *domain.ResponseRecord, log *domain.RequestLogRecord) {
	w.Enqueue(resp, log)
}

// WriteNow writes one request's audit rows immediately, bypassing the queue.
//
// The fallback row uses this: queuing the skeleton behind the very batch that failed would
// put it back in line for the same failure. It reports what it wrote to the failure
// callback like any other batch failure, so the counters keep one owner.
func (w *LogWriter) WriteNow(ctx context.Context, resp *domain.ResponseRecord, log *domain.RequestLogRecord) error {
	if w == nil {
		return fmt.Errorf("store: no log writer")
	}
	if err := w.db.putAuditSync(ctx, resp, log); err != nil {
		w.mu.Lock()
		w.failures++
		w.mu.Unlock()
		return err
	}
	w.mu.Lock()
	w.written++
	w.mu.Unlock()
	return nil
}

// Enqueue queues one request's audit rows.
//
// The data plane calls this after the response has already been written to the client, so
// it does no I/O and normally returns at once. The exception is a full queue: rather than
// growing without limit (unbounded memory, and a shutdown that cannot drain), it waits for
// the flusher to make room, bounded by QueueWaitTimeout. A row that cannot be queued even
// then is reported to the failure handler, which is the same path a failed batch takes.
func (w *LogWriter) Enqueue(resp *domain.ResponseRecord, log *domain.RequestLogRecord) {
	if w == nil {
		return
	}
	size := 0
	if log != nil {
		size += len(log.RequestJSON) + len(log.ResponseText) + len(log.ResponseReasoning)
	}
	if resp != nil {
		size += len(resp.RequestJSON) + len(resp.OutputJSON) + len(resp.UsageJSON)
	}

	deadline := time.Now().Add(w.waitLimit())
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			// Shutting down: write it through so the row is not silently lost.
			if err := w.db.putAuditSync(context.Background(), resp, log); err != nil {
				w.report(log, err)
			}
			return
		}
		if len(w.queue) < w.cfg.QueueRows && w.bytes+size <= w.cfg.QueueBytes {
			w.queue = append(w.queue, pendingRecord{resp: resp, log: log, size: size})
			w.bytes += size
			if resp != nil {
				w.pendingResp[resp.ID]++
			}
			full := len(w.queue) >= w.cfg.MaxBatch || w.bytes >= w.cfg.MaxBytes
			// This producer got in: the backpressure episode is over.
			w.waiting = false
			w.mu.Unlock()
			if full {
				w.wake()
			}
			return
		}
		// Queue full: the writer is behind. Wait for it, counting the episode once.
		if !w.waiting {
			w.waiting = true
			w.blocked++
			if w.log != nil {
				w.log.Warn("audit write queue is full; request is waiting for the writer",
					"queued_requests", len(w.queue), "queued_bytes", w.bytes)
			}
		}
		queued, queuedBytes := len(w.queue), w.bytes
		w.mu.Unlock()
		w.wake()

		if w.cfg.MaxWait > 0 && time.Now().After(deadline) {
			err := fmt.Errorf("store: audit queue full (queued %d requests, %d bytes) and wait limit reached", queued, queuedBytes)
			w.report(log, err)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitLimit is how long Enqueue may wait on a full queue.
func (w *LogWriter) waitLimit() time.Duration {
	if w.cfg.MaxWait > 0 {
		return w.cfg.MaxWait
	}
	return 30 * time.Second
}

// AwaitResponse returns once the stored response with this id has been written, or
// immediately when nothing is queued for it. The read path calls it so a POST followed by
// an immediate GET of the same response id cannot observe the write lag.
func (w *LogWriter) AwaitResponse(ctx context.Context, id string) error {
	if w == nil || id == "" {
		return nil
	}
	w.mu.Lock()
	waiting := w.pendingResp[id] > 0
	w.mu.Unlock()
	if !waiting {
		return nil
	}
	w.wake()
	for {
		w.mu.Lock()
		still := w.pendingResp[id] > 0
		w.mu.Unlock()
		if !still {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// AwaitAll is AwaitResponse for every queued row; used when a reader cannot name the ids
// it is about to look for (admin listings) and on shutdown.
func (w *LogWriter) AwaitAll(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.wake()
	for {
		w.mu.Lock()
		empty := len(w.queue) == 0
		w.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// wake nudges the flusher without blocking the caller.
func (w *LogWriter) wake() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// Flush writes everything queued right now and waits for it to land. Callers that need
// read-after-write (tests, an operator asking for the newest row) use it; the data plane
// does not.
func (w *LogWriter) Flush(ctx context.Context) error { return w.AwaitAll(ctx) }

// Close drains the queue and stops the flusher. It is safe to call twice.
func (w *LogWriter) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	w.wake()
	if w.started {
		select {
		case <-w.done:
		case <-ctx.Done():
			return fmt.Errorf("store: log writer drain timed out with work left: %w", ctx.Err())
		}
	}
	return nil
}

// Stats reports batching health for /stats and the console.
func (w *LogWriter) Stats() map[string]any {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	queued, bytes := len(w.queue), w.bytes
	w.mu.Unlock()
	return map[string]any{
		// One "request" here carries its stored response (when the model stores one) and
		// its request log in the same transaction.
		"batched_requests": w.written,
		"batches":          w.batches,
		"failed_rows":      w.failures,
		"queued_requests":  queued,
		"queued_bytes":     bytes,
		"flush_ms":         w.cfg.FlushInterval.Milliseconds(),
		"max_batch":        w.cfg.MaxBatch,
		"queue_rows":       w.cfg.QueueRows,
		"queue_bytes":      w.cfg.QueueBytes,
		"backpressure":     w.blocked,
	}
}

// batchWriteTimeout bounds one batched transaction. It is longer than the single-row
// budget (auditWriteTimeout, 5s, lives in the transport layer) because a batch carries up
// to MaxBatch requests, several of which may be hundreds of kilobytes.
const batchWriteTimeout = 30 * time.Second

// loop writes a batch whenever the queue is non-empty and either a batch is full or the
// flush interval since it became non-empty has passed.
func (w *LogWriter) loop() {
	defer close(w.done)

	for {
		w.mu.Lock()
		for len(w.queue) == 0 && !w.closed {
			w.mu.Unlock()
			select {
			case <-w.notify:
			case <-time.After(w.cfg.FlushInterval):
			}
			w.mu.Lock()
		}
		if len(w.queue) == 0 && w.closed {
			w.mu.Unlock()
			return
		}
		batch := w.takeLocked()
		more := len(w.queue) > 0
		w.mu.Unlock()

		w.writeBatch(batch)

		// A full batch means more work is already waiting: write it back to back rather
		// than sleeping an interval per batch.
		if more {
			w.wake()
			continue
		}
		w.mu.Lock()
		closed := w.closed
		empty := len(w.queue) == 0
		w.mu.Unlock()
		if closed && empty {
			return
		}
	}
}

// takeLocked removes and returns the next batch. The caller holds w.mu.
func (w *LogWriter) takeLocked() []pendingRecord {
	batch := make([]pendingRecord, 0, min(len(w.queue), w.cfg.MaxBatch))
	bytes := 0
	for len(w.queue) > 0 && len(batch) < w.cfg.MaxBatch {
		next := w.queue[0]
		if len(batch) > 0 && bytes+next.size > w.cfg.MaxBytes {
			break
		}
		w.queue = w.queue[1:]
		w.bytes -= next.size
		if next.resp != nil {
			if n := w.pendingResp[next.resp.ID]; n <= 1 {
				delete(w.pendingResp, next.resp.ID)
			} else {
				w.pendingResp[next.resp.ID] = n - 1
			}
		}
		bytes += next.size
		batch = append(batch, next)
	}
	if len(w.queue) == 0 {
		w.queue = nil
	}
	return batch
}

func (w *LogWriter) writeBatch(batch []pendingRecord) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), batchWriteTimeout)
	defer cancel()

	if err := w.db.PutAuditBatch(ctx, batch); err != nil {
		// The batch failed as a unit. Retry each row on its own: one bad row (a
		// constraint violation, an oversized payload) must not take its neighbours'
		// audit rows down with it, and the per-row error is what the operator needs.
		if w.log != nil {
			w.log.Warn("batched audit write failed; retrying row by row",
				"err", err, "rows", len(batch))
		}
		for _, item := range batch {
			if err := w.db.putAuditSync(ctx, item.resp, item.log); err != nil {
				w.mu.Lock()
				w.failures++
				w.mu.Unlock()
				w.report(item.log, err)
				continue
			}
			w.mu.Lock()
			w.written++
			w.mu.Unlock()
		}
		return
	}

	w.mu.Lock()
	w.written += int64(len(batch))
	w.batches++
	w.mu.Unlock()
}

func (w *LogWriter) report(log *domain.RequestLogRecord, err error) {
	if w.onFailure != nil && log != nil {
		w.onFailure(log, err)
		return
	}
	if w.log != nil {
		w.log.Error("request log lost entirely", "err", err)
	}
}

// PutAuditBatch writes one batch of audit rows in a single transaction: every request's
// stored response first, then its request log.
func (db *DB) PutAuditBatch(ctx context.Context, batch []pendingRecord) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin audit batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, item := range batch {
		if item.resp != nil {
			if err := db.putResponseTx(ctx, tx, item.resp); err != nil {
				return err
			}
		}
		if item.log != nil {
			if err := db.putRequestLogTx(ctx, tx, item.log); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit audit batch of %d rows: %w", len(batch), err)
	}
	return nil
}

// putAuditSync is the per-row fallback: one transaction per record, used when the batch
// failed and while shutting down.
func (db *DB) putAuditSync(ctx context.Context, resp *domain.ResponseRecord, log *domain.RequestLogRecord) error {
	if resp == nil && log == nil {
		return nil
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin audit write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if resp != nil {
		if err := db.putResponseTx(ctx, tx, resp); err != nil {
			return err
		}
	}
	if log != nil {
		if err := db.putRequestLogTx(ctx, tx, log); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit audit write: %w", err)
	}
	return nil
}

// inTx adapts the statement cache to a transaction.
type inTx struct {
	db  *DB
	tx  *sql.Tx
	ctx context.Context
}

func (t inTx) exec(sqlText string, args ...any) (sql.Result, error) {
	return t.db.stmts.execInTx(t.ctx, t.db.write, t.tx, sqlText, args...)
}

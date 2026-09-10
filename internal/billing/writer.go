package billing

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/internal/store"
)

// Config configures the settlement writer.
type Config struct {
	// BatchSize is the number of settlements applied per transaction.
	BatchSize int
	// FlushInterval bounds how long a settlement can wait for batch mates.
	FlushInterval time.Duration
	// QueueSize overrides the queue capacity (defaults to 4x BatchSize).
	QueueSize int
	// FallbackFile receives settlements the database refused, one JSON object per line.
	FallbackFile string
	// OnFallback is called for every settlement that had to be written to disk.
	OnFallback func(settlement *Settlement, cause error)
}

// DefaultConfig mirrors the documented defaults.
func DefaultConfig() Config {
	return Config{BatchSize: 32, FlushInterval: 20 * time.Millisecond, FallbackFile: "billing-fallback.jsonl"}
}

// Stats reports writer counters for /metrics and the console.
type Stats struct {
	Queued     int64 `json:"queued"`
	Batches    int64 `json:"batches"`
	Settled    int64 `json:"settled"`
	Replayed   int64 `json:"replayed"`
	SyncWrites int64 `json:"sync_writes"`
	Fallbacks  int64 `json:"fallbacks"`
	Dropped    int64 `json:"dropped"`
}

// Writer is the single writer that turns settlements into committed transactions.
//
// SQLite has exactly one writer, so serialising here is not a bottleneck but the
// opposite: it lets many concurrent requests share one fsync per batch.
type Writer struct {
	cfg     Config
	log     *slog.Logger
	applier Batching

	mu     sync.Mutex
	queue  chan *Settlement
	closed bool
	stop   chan struct{}
	done   chan struct{}
	wake   chan struct{}

	queued     atomic.Int64
	batches    atomic.Int64
	settled    atomic.Int64
	replayed   atomic.Int64
	syncWrites atomic.Int64
	fallbacks  atomic.Int64
}

// NewWriter starts the writer goroutine.
func NewWriter(cfg Config, applier Batching, log *slog.Logger) *Writer {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultConfig().BatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultConfig().FlushInterval
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = cfg.BatchSize * 4
	}
	if log == nil {
		log = slog.Default()
	}
	w := &Writer{
		cfg:     cfg,
		log:     log,
		applier: applier,
		queue:   make(chan *Settlement, cfg.QueueSize),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
	}
	go w.loop()
	return w
}

// Submit queues a settlement. When the queue is full it degrades to a synchronous
// write so a burst can never silently drop money.
func (w *Writer) Submit(settlement *Settlement) {
	if settlement == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.writeDirect(settlement)
		return
	}
	w.mu.Unlock()

	select {
	case w.queue <- settlement:
		w.queued.Add(1)
		select {
		case w.wake <- struct{}{}:
		default:
		}
	default:
		// Backpressure: the queue is full, so pay the latency here instead of losing it.
		w.syncWrites.Add(1)
		w.writeDirect(settlement)
	}
}

// writeDirect applies one settlement outside the batch.
func (w *Writer) writeDirect(settlement *Settlement) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.mu.Lock()
	applier := w.applier
	w.mu.Unlock()
	if applier == nil {
		w.fallback(settlement, errors.New("writer is closed"))
		return
	}
	applied, err := applier.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters)
	if err != nil {
		w.fallback(settlement, err)
		return
	}
	w.count(applied)
}

func (w *Writer) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()
	batch := make([]*Settlement, 0, w.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.flushBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case settlement := <-w.queue:
			batch = append(batch, settlement)
			if len(batch) >= w.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.wake:
			// Drain what is already queued instead of waiting for the next tick.
		drain:
			for len(batch) < w.cfg.BatchSize {
				select {
				case settlement := <-w.queue:
					batch = append(batch, settlement)
				default:
					break drain
				}
			}
			flush()
		case <-w.stop:
			flush()
			return
		}
	}
}

// flushBatch applies a batch in one transaction, degrading to per-row writes and
// then to the fallback file.
func (w *Writer) flushBatch(batch []*Settlement) {
	w.batches.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w.mu.Lock()
	applier := w.applier
	w.mu.Unlock()
	if applier == nil {
		for _, settlement := range batch {
			w.fallback(settlement, errors.New("writer is closed"))
		}
		return
	}

	applied, err := settleBatch(ctx, applier, batch)
	if err == nil {
		w.settled.Add(int64(applied))
		w.replayed.Add(int64(len(batch) - applied))
		return
	}
	w.log.Warn("batched settlement failed; retrying row by row", "err", err, "rows", len(batch))
	for _, settlement := range batch {
		one, err := applier.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters)
		if err != nil {
			w.fallback(settlement, err)
			continue
		}
		w.count(one)
	}
}

// settleBatch is only correct because the store applies the whole slice in one
// transaction; batches are applied through the same code path as single rows.
func settleBatch(ctx context.Context, applier Batching, batch []*Settlement) (int, error) {
	// The store exposes a per-attempt primitive; a batch is a sequence of them inside
	// one transaction, which store.SettleBatch provides.
	batching, ok := applier.(interface {
		SettleBatch(ctx context.Context, settlements []*store.SettlementInput) (int, error)
	})
	if !ok {
		return 0, errors.New("billing: store does not support batched settlement")
	}
	inputs := make([]*store.SettlementInput, 0, len(batch))
	for _, settlement := range batch {
		inputs = append(inputs, &store.SettlementInput{
			Usage: settlement.Usage, Entries: settlement.Entries, Counters: settlement.Counters,
		})
	}
	return batching.SettleBatch(ctx, inputs)
}

func (w *Writer) count(applied bool) {
	if applied {
		w.settled.Add(1)
		return
	}
	w.replayed.Add(1)
}

// fallback persists a settlement the database refused, then reports it.
func (w *Writer) fallback(settlement *Settlement, cause error) {
	w.fallbacks.Add(1)
	w.log.Error("settlement could not be written; falling back to disk",
		"err", cause, "request_id", settlement.Usage.RequestID)
	if w.cfg.FallbackFile != "" {
		if err := AppendFallback(w.cfg.FallbackFile, settlement, cause); err != nil {
			w.log.Error("writing the billing fallback file failed", "err", err,
				"file", w.cfg.FallbackFile, "request_id", settlement.Usage.RequestID)
		}
	}
	if w.cfg.OnFallback != nil {
		w.cfg.OnFallback(settlement, cause)
	}
}

// Stats returns a snapshot of the writer counters.
func (w *Writer) Stats() Stats {
	return Stats{
		Queued:     w.queued.Load(),
		Batches:    w.batches.Load(),
		Settled:    w.settled.Load(),
		Replayed:   w.replayed.Load(),
		SyncWrites: w.syncWrites.Load(),
		Fallbacks:  w.fallbacks.Load(),
	}
}

// Close drains the queue and stops the writer. It returns the number of settlements
// that could not be persisted in time.
func (w *Writer) Close(timeout time.Duration) int {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0
	}
	w.closed = true
	w.mu.Unlock()

	close(w.stop)
	select {
	case <-w.done:
	case <-time.After(timeout):
	}

	// Anything still queued after the writer stopped must not be lost silently.
	remaining := 0
	for {
		select {
		case settlement := <-w.queue:
			remaining++
			w.fallback(settlement, errors.New("writer stopped before the settlement was applied"))
		default:
			return remaining
		}
	}
}

// MarshalSettlement is the fallback file's line format.
func MarshalSettlement(settlement *Settlement) ([]byte, error) {
	return json.Marshal(settlement)
}

var _ = os.O_APPEND

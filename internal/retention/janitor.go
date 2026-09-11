// Package retention prunes recorded observability data (request logs and stored
// responses) once it is older than the configured retention window.
//
// It exists because recording.retention_days was configuration nobody read: request_logs
// grew forever, and responses.expires_at was written but never honoured. Billing and
// audit tables are deliberately out of scope — they are the ledger and the compliance
// trail, not droppable observations.
//
// The package is a leaf: persistence arrives through a port, so the janitor can be
// driven by a fake in tests and by internal/store in production.
package retention

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Store is the persistence the janitor needs. Both methods delete at most limit rows and
// report how many they deleted.
type Store interface {
	PruneRequestLogs(ctx context.Context, before time.Time, limit int) (int, error)
	PruneExpiredResponses(ctx context.Context, now time.Time, limit int) (int, error)
}

// Config tunes one janitor.
type Config struct {
	// RetentionDays is the window from recording.retention_days. A non-positive value
	// disables cleanup entirely: nothing is deleted, and the gateway stops writing
	// response expiry timestamps, so a stored response lives as long as the database.
	RetentionDays int
	// BatchSize bounds one DELETE; MaxBatches bounds one pass. Together they keep the
	// single writer connection available to request traffic while old rows drain away
	// over several passes.
	BatchSize  int
	MaxBatches int
}

// Defaults for Config.
const (
	DefaultBatchSize  = 500
	DefaultMaxBatches = 200
)

// Result summarises one pass.
type Result struct {
	RequestLogs int `json:"request_logs"`
	Responses   int `json:"responses"`
	Batches     int `json:"batches"`
	// Exhausted means the pass stopped at MaxBatches with rows still due; the next pass
	// (or the next tick) continues where this one left off.
	Exhausted bool `json:"exhausted"`
	// Disabled means retention_days <= 0: nothing was looked at.
	Disabled bool `json:"disabled"`
	// Busy means another pass was already running and this call did nothing, so an
	// operator hitting the manual endpoint never queues behind the daily job.
	Busy bool `json:"busy"`
}

// Janitor deletes expired recordings.
type Janitor struct {
	store     Store
	cfg       Config
	log       *slog.Logger
	pruned    atomic.Int64
	batches   atomic.Int64
	lastRun   atomic.Int64 // unix seconds of the last completed pass
	lastError atomic.Value // string
	running   sync.Mutex   // one pass at a time: both the ticker and the manual endpoint
}

// New builds a janitor. A nil logger is replaced so callers need not care.
func New(store Store, cfg Config, log *slog.Logger) *Janitor {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.MaxBatches <= 0 {
		cfg.MaxBatches = DefaultMaxBatches
	}
	if log == nil {
		log = slog.Default()
	}
	j := &Janitor{store: store, cfg: cfg, log: log}
	j.lastError.Store("")
	return j
}

// RetentionDays reports the configured window (0 when cleanup is off).
func (j *Janitor) RetentionDays() int { return j.cfg.RetentionDays }

// PrunedTotal reports how many rows this process has deleted since it started.
func (j *Janitor) PrunedTotal() int64 { return j.pruned.Load() }

// LastError reports the most recent failure, or "" — surfaced on /stats so a silent
// cleanup failure is visible without grepping logs.
func (j *Janitor) LastError() string {
	if value, ok := j.lastError.Load().(string); ok {
		return value
	}
	return ""
}

// LastRun reports when the last pass finished (zero if none has).
func (j *Janitor) LastRun() time.Time {
	if secs := j.lastRun.Load(); secs > 0 {
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}

// Run performs one pass: it deletes request logs older than the window and stored
// responses whose expiry has passed, in batches, and stops when nothing is left or the
// per-pass batch budget is spent.
func (j *Janitor) Run(ctx context.Context) (Result, error) {
	result := Result{}
	if j.cfg.RetentionDays <= 0 || j.store == nil {
		result.Disabled = true
		return result, nil
	}
	if !j.running.TryLock() {
		result.Busy = true
		return result, nil
	}
	defer j.running.Unlock()
	cutoff := time.Now().UTC().AddDate(0, 0, -j.cfg.RetentionDays)

	for batch := 0; batch < j.cfg.MaxBatches; batch++ {
		deleted, err := j.store.PruneRequestLogs(ctx, cutoff, j.cfg.BatchSize)
		if err != nil {
			j.recordError(err)
			return result, err
		}
		result.Batches++
		result.RequestLogs += deleted
		if deleted < j.cfg.BatchSize {
			break
		}
		if batch == j.cfg.MaxBatches-1 {
			result.Exhausted = true
		}
	}
	for batch := 0; batch < j.cfg.MaxBatches; batch++ {
		deleted, err := j.store.PruneExpiredResponses(ctx, time.Now().UTC(), j.cfg.BatchSize)
		if err != nil {
			j.recordError(err)
			return result, err
		}
		result.Batches++
		result.Responses += deleted
		if deleted < j.cfg.BatchSize {
			break
		}
		if batch == j.cfg.MaxBatches-1 {
			result.Exhausted = true
		}
	}

	j.pruned.Add(int64(result.RequestLogs + result.Responses))
	j.batches.Add(int64(result.Batches))
	j.lastRun.Store(time.Now().Unix())
	j.lastError.Store("")
	return result, nil
}

func (j *Janitor) recordError(err error) {
	j.lastError.Store(err.Error())
	j.log.Error("pruning expired recordings failed", "err", err)
}

// Start runs one pass immediately and then daily until the context is cancelled.
// Retention is a daily policy, not a real-time reaction.
func (j *Janitor) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			result, err := j.Run(ctx)
			switch {
			case err != nil:
				// Run already logged and recorded the failure.
			case result.Disabled:
				j.log.Info("retention cleanup disabled", "retention_days", j.cfg.RetentionDays)
			case result.RequestLogs > 0 || result.Responses > 0 || result.Exhausted:
				j.log.Info("retention pass finished",
					"request_logs", result.RequestLogs, "responses", result.Responses,
					"batches", result.Batches, "exhausted", result.Exhausted,
					"retention_days", j.cfg.RetentionDays)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

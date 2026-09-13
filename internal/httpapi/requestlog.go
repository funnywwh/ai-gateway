package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/retention"
)

// skeletonWriteTimeout bounds the fallback write of a content-free request-log row.
//
// The first attempt gets auditWriteTimeout (5s). When it fails, the cause is usually that
// the single writer connection was busy — so the retry needs its own budget rather than
// the leftovers of the first one. It is generous because the row is tiny (a few hundred
// bytes at most): a stall that long means the database is in real trouble, and the
// dropped counter is then the honest signal.
const skeletonWriteTimeout = 15 * time.Second

// storeRequestLog records one request-log row.
//
// Two paths, same audit contract:
//
//   - Batched (Deps.LogRecorder set): the row is queued and written by the background
//     flusher in a shared transaction. The handler does not wait, which is the whole
//     point — the write lock is the process's bottleneck, and a request has no reason to
//     hold it. A row the batch could not write comes back through
//     handleFailedRecording, which is the same skeleton fallback as below.
//   - Synchronous (no LogRecorder): write now, and on failure write the row again
//     without its content.
//
// The fallback exists because the audit trail has to outlive both the client and a busy
// writer. Before it, a failed insert meant the row simply did not exist: an operator saw
// the request nowhere, which is exactly the blind spot M19d/M19e were found through (20
// such losses were logged in one day on the local deployment, two of them losing the
// request entirely). A skeleton row — which request, whose, when, how it ended, how big it
// was — keeps the request visible and still says, honestly, that its content is not there.
func (s *Server) storeRequestLog(ctx context.Context, rec *domain.RequestLogRecord) {
	if s.deps.Records == nil {
		return
	}
	if s.deps.LogRecorder != nil {
		s.deps.LogRecorder.EnqueueRecording(nil, rec)
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	err := s.deps.Records.PutRequestLog(writeCtx, rec)
	cancel()
	if err != nil {
		s.handleFailedRecording(ctx, rec, err)
	}
}

// handleFailedRecording is the fallback for a content write that did not land, whether it
// failed synchronously or in a batch. It writes a content-free skeleton row through
// WriteNow when the recorder is batching (queuing the fallback behind the failed batch
// would just fail again), and counts the loss when even that fails.
func (s *Server) handleFailedRecording(ctx context.Context, rec *domain.RequestLogRecord, err error) {
	s.requestLogWriteFailures.Add(1)
	s.deps.Log.Warn("recording request content failed",
		"err", err, "request_id", rec.RequestID, "request_bytes", rec.RequestBytes)

	retryCtx, cancelRetry := context.WithTimeout(context.WithoutCancel(ctx), skeletonWriteTimeout)
	defer cancelRetry()

	bare := skeletonLog(rec)
	var retryErr error
	switch {
	case s.deps.LogRecorder != nil:
		retryErr = s.deps.LogRecorder.WriteNow(retryCtx, nil, bare)
	case s.deps.Records != nil:
		retryErr = s.deps.Records.PutRequestLog(retryCtx, bare)
	default:
		retryErr = errors.New("no record store configured")
	}
	if retryErr != nil {
		s.requestLogDropped.Add(1)
		s.deps.Log.Error("request log lost entirely",
			"err", retryErr, "request_id", rec.RequestID, "status", rec.Status)
		return
	}
	s.deps.Log.Info("request log stored without content after a failed write",
		"request_id", rec.RequestID, "request_bytes", rec.RequestBytes, "status", rec.Status)
}

// skeletonLog keeps the facts that must survive and drops the content. The stored record
// still names the real output mode, so a reader can tell "recording was on, the body did
// not make it" from "recording was off".
func skeletonLog(rec *domain.RequestLogRecord) *domain.RequestLogRecord {
	bare := *rec
	bare.RequestJSON = ""
	bare.ResponseReasoning = ""
	bare.ResponseText = ""
	bare.ReasoningRecorded = false
	bare.OutputTextRecorded = false
	bare.ResponseBytes = 0
	return &bare
}

// retentionWindow is recording.retention_days as a duration. ok=false means retention is
// switched off: nothing is pruned and stored responses never expire.
func (s *Server) retentionWindow() (time.Duration, bool) {
	days := s.deps.Config.Recording.RetentionDays
	if days <= 0 {
		return 0, false
	}
	return time.Duration(days) * 24 * time.Hour, true
}

// SetRecordingFailureHandler installs the callback the batched recorder calls when it
// could not write a queued row. It exists because the two halves own different things:
// the store owns the queue, the transport owns the retry policy (skeleton fallback plus
// the counters an operator reads). The composition root wires them after both exist.
func (s *Server) SetRecordingFailureHandler(install func(func(*domain.RequestLogRecord, error))) {
	if install == nil {
		return
	}
	install(func(rec *domain.RequestLogRecord, err error) {
		// The batch has already given up on this row; ctx only carries the request id
		// and is used for the detached fallback write.
		s.handleFailedRecording(context.Background(), rec, err)
	})
}

// requestLogStats reports the write health of the request log plus the retention policy,
// for /stats and the console.
func (s *Server) requestLogStats(ctx context.Context) map[string]any {
	stats := map[string]any{
		"write_failures": s.requestLogWriteFailures.Load(),
		"dropped":        s.requestLogDropped.Load(),
		"retention_days": s.deps.Config.Recording.RetentionDays,
	}
	if rec := s.deps.LogRecorder; rec != nil {
		stats["batching"] = rec.Stats()
	}
	if rollups := s.deps.DimensionRollups; rollups != nil {
		if status, err := rollups.DimensionRollupStats(ctx); err == nil {
			stats["dimension_rollup"] = status
		} else {
			stats["dimension_rollup"] = map[string]any{"last_error": err.Error()}
		}
	}
	if janitor := s.deps.LogJanitor; janitor != nil {
		stats["pruned"] = janitor.PrunedTotal()
		stats["enabled"] = janitor.RetentionDays() > 0
		if lastErr := janitor.LastError(); lastErr != "" {
			stats["last_error"] = lastErr
		}
		if at := janitor.LastRun(); !at.IsZero() {
			stats["last_run"] = at.Format(time.RFC3339)
		}
	}
	return stats
}

// handleAdminPruneRequests runs the retention cleanup on demand. The same job runs at
// startup and daily; this endpoint exists so an operator can reclaim space without
// waiting for the schedule (and see exactly how much went).
func (s *Server) handleAdminPruneRequests(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	janitor, ok := portReady(w, s.deps.LogJanitor, "retention cleanup")
	if !ok {
		return
	}
	result, err := janitor.Run(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "prune", "request_log", "", map[string]any{
		"request_logs": result.RequestLogs, "responses": result.Responses,
		"batches": result.Batches, "exhausted": result.Exhausted, "disabled": result.Disabled,
	}, "ok")
	writeJSON(w, http.StatusOK, result)
}

// LogJanitor is the retention cleanup as the transport layer sees it.
type LogJanitor interface {
	Run(ctx context.Context) (retention.Result, error)
	RetentionDays() int
	PrunedTotal() int64
	LastRun() time.Time
	LastError() string
}

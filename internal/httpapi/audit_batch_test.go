package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

// installBatching switches a fixture's server onto the batched audit path, exactly the way
// cmd/aigw wires it: the composition root owns the writer and hands the transport a way to
// report rows the batch could not store.
func installBatching(t *testing.T, f *fixture) *store.LogWriter {
	t.Helper()
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 20 * time.Millisecond,
		MaxBatch:      4,
		MaxBytes:      1 << 20,
	}, nil, nil)
	f.srv.deps.LogRecorder = w
	f.srv.SetRecordingFailureHandler(w.SetFailureHandler)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(ctx)
	})
	// The test reads the rows it just wrote, so it asks for read-after-write explicitly;
	// the data plane never does.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Flush(ctx)
	})
	return w
}

// TestBatchedRecordingReachesTheSameRows pins the equivalence that matters: switching the
// write path to the background batcher must not change what an operator can read back.
func TestBatchedRecordingReachesTheSameRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := installBatching(t, f)

	resp := f.do(t, "POST", "/v1/responses", agentBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	requestID := resp.Header.Get("x-request-id")

	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	log, err := f.db.GetRequestLog(ctx, requestID)
	if err != nil {
		t.Fatalf("the batched request log must be readable: %v", err)
	}
	if log.Status != "completed" || log.RecordInputMode != "user" {
		t.Fatalf("batched row lost its facts: %+v", log)
	}
	if log.RequestBytes <= 0 {
		t.Fatalf("batched row must keep request_bytes: %+v", log)
	}

	// The stored response of the same request must be there too: the batch writes both
	// rows in one transaction, so one can never exist without the other.
	var stored int
	if err := f.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM responses WHERE request_json <> ''`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == 0 {
		t.Fatal("the stored response must be written by the same batch as its request log")
	}

	if stats := w.Stats(); stats["batched_requests"].(int64) < 1 {
		t.Fatalf("the batcher must report what it wrote: %+v", stats)
	}
	if got := f.srv.requestLogWriteFailures.Load(); got != 0 {
		t.Fatalf("a healthy batch must not count failures: %d", got)
	}
}

// TestBatchedRecordingKeepsTheRowOffTheRequestPath is the point of the change: a served
// request must not have written its audit rows yet.
func TestBatchedRecordingKeepsTheRowOffTheRequestPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A long interval so the flusher cannot have run by the time the handler returns.
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Second, MaxBatch: 64, MaxBytes: 1 << 20,
	}, nil, nil)
	f.srv.deps.LogRecorder = w
	f.srv.SetRecordingFailureHandler(w.SetFailureHandler)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	resp := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	resp.Body.Close()
	requestID := resp.Header.Get("x-request-id")

	if _, err := f.db.GetRequestLog(ctx, requestID); err == nil {
		t.Fatal("the row must still be queued when the handler returns, not written")
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := f.db.GetRequestLog(ctx, requestID); err != nil {
		t.Fatalf("flush must write the queued row: %v", err)
	}
}

// TestImmediateGetSeesItsOwnResponse pins the client-visible contract that batching could
// have broken: POST returns a response id, and a GET of that id must find it even though the
// row is written by the background flusher.
func TestImmediateGetSeesItsOwnResponse(t *testing.T) {
	f := newFixture(t)
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Second, MaxBatch: 64, MaxBytes: 1 << 20,
	}, nil, nil)
	f.srv.deps.LogRecorder = w
	f.srv.SetRecordingFailureHandler(w.SetFailureHandler)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	body := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	defer body.Body.Close()
	if body.StatusCode != 200 {
		t.Fatalf("status = %d", body.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(body.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("no response id returned")
	}

	// No Flush call anywhere: the GET path waits for the id it was asked about.
	got := f.do(t, "GET", "/v1/responses/"+created.ID, "", nil)
	defer got.Body.Close()
	if got.StatusCode != 200 {
		t.Fatalf("immediate GET status = %d, want 200 (the row is queued, not missing)", got.StatusCode)
	}
}

// TestBatchFailureFallsBackToSkeletonRows: a row the schema refuses fails its whole batch.
// The transport must then see it (and write the content-free skeleton for it) while every
// other row in that batch still lands — one bad row may not take its neighbours down.
func TestBatchFailureFallsBackToSkeletonRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var reported []string
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Millisecond, MaxBatch: 4, MaxBytes: 1 << 20,
	}, nil, func(rec *domain.RequestLogRecord, err error) {
		reported = append(reported, rec.RequestID)
		f.srv.handleFailedRecording(context.Background(), rec, err)
	})
	f.srv.deps.LogRecorder = w
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	// request_id is NOT NULL, so an empty id makes the driver reject the row itself.
	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "", Status: "completed"})
	w.EnqueueRecording(nil, &domain.RequestLogRecord{
		RequestID: "req_good_for_batch", Status: "completed", CreatedAt: time.Now().UTC(),
	})

	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := f.db.GetRequestLog(ctx, "req_good_for_batch"); err != nil {
		t.Fatalf("a sibling row must survive a failed batch: %v", err)
	}
	if len(reported) == 0 {
		t.Fatal("the failed row must be reported to the failure handler")
	}
	if got := f.srv.requestLogWriteFailures.Load(); got == 0 {
		t.Fatal("the failed row must count as a write failure")
	}
}

// TestBatchDrainsOnClose pins the shutdown contract: an orderly stop writes every queued
// row before the store closes.
func TestBatchDrainsOnClose(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Second, MaxBatch: 64, MaxBytes: 1 << 20,
	}, nil, nil)

	w.EnqueueRecording(nil, &domain.RequestLogRecord{
		RequestID: "req_closedrain", Status: "completed", CreatedAt: time.Now().UTC(),
	})
	if _, err := f.db.GetRequestLog(ctx, "req_closedrain"); err == nil {
		t.Fatal("the row must still be queued")
	}
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(cctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := f.db.GetRequestLog(ctx, "req_closedrain"); err != nil {
		t.Fatalf("close must drain the queue: %v", err)
	}
}

// TestWriteNowBypassesTheQueue pins the fallback path: the skeleton row must not go back
// into the queue that just failed.
func TestWriteNowBypassesTheQueue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Second, MaxBatch: 64, MaxBytes: 1 << 20,
	}, nil, nil)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	if err := w.WriteNow(ctx, nil, &domain.RequestLogRecord{
		RequestID: "req_writenow", Status: "completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write now: %v", err)
	}
	if _, err := f.db.GetRequestLog(ctx, "req_writenow"); err != nil {
		t.Fatalf("WriteNow must write immediately: %v", err)
	}
}

// TestRecorderErrorsStillCountOnce keeps the counter contract from the synchronous path:
// a write failure counts one failure, and a failed fallback counts one drop.
func TestRecorderErrorsStillCountOnce(t *testing.T) {
	f := newFixture(t)
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Millisecond, MaxBatch: 1, MaxBytes: 1 << 20,
	}, nil, func(rec *domain.RequestLogRecord, err error) {
		f.srv.handleFailedRecording(context.Background(), rec, err)
	})
	f.srv.deps.LogRecorder = w
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	// An empty request id cannot be stored: the batch fails, the skeleton fallback fails
	// the same way, and exactly one failure plus one drop must be counted.
	w.EnqueueRecording(nil, &domain.RequestLogRecord{RequestID: "", Status: "completed"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = w.Flush(ctx)

	if got := f.srv.requestLogWriteFailures.Load(); got != 1 {
		t.Fatalf("write failures = %d, want 1", got)
	}
	if got := f.srv.requestLogDropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

// TestImmediateContinuationSeesItsPreviousResponse: an agent loop sends the tool result
// back in the very next request, naming the response it was just handed as
// previous_response_id. That lookup reads a row written by the background batcher, so
// without the same wait the GET path performs, the continuation races the write and the
// turn dies as "response not found" — the loop cannot continue at all, and with a
// thinking-mode upstream it cannot even be retried differently (the tool result has to
// travel back in this shape).
func TestImmediateContinuationSeesItsPreviousResponse(t *testing.T) {
	f := newFixture(t)
	w := store.NewLogWriter(f.db, store.LogWriterConfig{
		FlushInterval: 10 * time.Second, MaxBatch: 64, MaxBytes: 1 << 20,
	}, nil, nil)
	f.srv.deps.LogRecorder = w
	f.srv.SetRecordingFailureHandler(w.SetFailureHandler)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(cctx)
	}()

	first := f.do(t, "POST", "/v1/responses", agentBody, nil)
	defer first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first turn status = %d", first.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(first.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("no response id returned")
	}

	// No Flush call anywhere: the continuation path waits for the id it names.
	next := f.do(t, "POST", "/v1/responses", `{"model":"echo-model","previous_response_id":"`+
		created.ID+`","input":[{"type":"function_call_output","call_id":"call_1","output":"42"}]}`, nil)
	defer next.Body.Close()
	if next.StatusCode != 200 {
		t.Fatalf("continuation status = %d, want 200 (the row is queued, not missing)", next.StatusCode)
	}
}

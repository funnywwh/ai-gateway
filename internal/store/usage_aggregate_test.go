package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// seedTTFTUsage writes one metered attempt carrying an explicit TTFT.
func seedTTFTUsage(t *testing.T, db *DB, requestID string, attempt, ttftMS int) {
	t.Helper()
	if _, err := db.InsertUsage(context.Background(), &domain.UsageRecord{
		RequestID: requestID, AttemptNo: attempt, AccountID: 1, APIKeyID: 1,
		Model: "m", ResolvedModel: "m", DimensionsJSON: `{"input":1,"output":1}`,
		CostMicros: 1, ChargeMicros: 1, LatencyMS: ttftMS, TTFTMS: ttftMS,
		Status: "completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

// A fractional mean must round to an integer instead of failing the scan.
//
// SQLite's AVG() always yields REAL (here 101.33333333333333), and the aggregate is
// scanned into the int64 field domain.UsageTotals.TTFTAvgMS, so the unconverted
// expression made UsageWindowTotals fail outright — which is what broke the MCP
// get_dashboard / get_usage_summary tools and the console period summary. Exact means
// always worked, so the defect only appeared once a window held fractional samples.
func TestUsageWindowTotalsRoundsFractionalTTFT(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	window := func() (*UsageWindowTotals, error) {
		return db.UsageWindowTotals(ctx, 1, now.Add(-time.Hour), now.Add(time.Hour))
	}

	// 100, 101, 103 -> mean 101.333..., previously a scan error.
	seedTTFTUsage(t, db, "req-t1", 1, 100)
	seedTTFTUsage(t, db, "req-t2", 1, 101)
	seedTTFTUsage(t, db, "req-t3", 1, 103)

	got, err := window()
	if err != nil {
		t.Fatalf("a fractional mean must not fail the aggregate: %v", err)
	}
	if got.TTFTAvgMS != 101 {
		t.Fatalf("TTFTAvgMS = %d, want 101 (rounded mean of 100/101/103)", got.TTFTAvgMS)
	}
	if got.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", got.Attempts)
	}

	// Zero samples are excluded by the CASE, so they must not drag the mean toward 0.
	seedTTFTUsage(t, db, "req-t4", 1, 0)
	if got, err = window(); err != nil {
		t.Fatalf("zero sample broke the aggregate: %v", err)
	}
	if got.Attempts != 4 || got.TTFTAvgMS != 101 {
		t.Fatalf("a ttft_ms=0 row must not count as a sample: %+v", got)
	}
}

// No TTFT samples at all must yield 0, not a NULL scan error: the empty window is a
// normal state for these tools (a fresh account, or a period with only failures).
func TestUsageWindowTotalsWithoutTTFTSamples(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	empty, err := db.UsageWindowTotals(ctx, 1, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("empty window: %v", err)
	}
	if empty.TTFTAvgMS != 0 || empty.Attempts != 0 {
		t.Fatalf("empty window = %+v, want a zero aggregate", empty)
	}

	// A row with no first token recorded (a failure) still averages to 0.
	seedTTFTUsage(t, db, "req-t5", 1, 0)
	got, err := db.UsageWindowTotals(ctx, 1, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("zero-only window: %v", err)
	}
	if got.Attempts != 1 || got.TTFTAvgMS != 0 {
		t.Fatalf("zero-only window = %+v, want attempts 1 and TTFT 0", got)
	}
}

// An exact mean must keep working: this is the case the old expression already handled,
// and the regression test pins that the rounding fix did not disturb it.
func TestUsageWindowTotalsExactTTFTMean(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedTTFTUsage(t, db, "req-t6", 1, 100)
	seedTTFTUsage(t, db, "req-t7", 1, 200)

	got, err := db.UsageWindowTotals(ctx, 1, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.TTFTAvgMS != 150 {
		t.Fatalf("TTFTAvgMS = %d, want 150", got.TTFTAvgMS)
	}
}

// The average is reported alongside a p95 computed from integer samples. The two use
// different conventions — AVG over every sample, versus percentile's nearest-rank index
// int((n-1)*fraction). This pins that the rounding fix leaves both statistics inside the
// observed range and that they answer different questions: with a long fast block and one
// slow outlier, AVG is dragged up by the outlier while the nearest-rank p95 stays in the
// fast block (it would need n-1 samples before its index reached the maximum).
func TestUsageWindowTotalsTTFTAverageAndP95(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 1; i <= 40; i++ {
		seedTTFTUsage(t, db, "req-skew", i, 100+i)
	}
	seedTTFTUsage(t, db, "req-skew", 41, 4000) // the slow outlier

	got, err := db.UsageWindowTotals(ctx, 1, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("skewed window: %v", err)
	}
	if got.Attempts != 41 {
		t.Fatalf("attempts = %d, want 41", got.Attempts)
	}
	// Every sample is within 101..4000, so both statistics must land in that range.
	if got.TTFTAvgMS < 101 || got.TTFTAvgMS > 4000 {
		t.Fatalf("TTFTAvgMS = %d, outside the observed 101..4000 range", got.TTFTAvgMS)
	}
	if got.TTFTP95MS < 101 || got.TTFTP95MS > 4000 {
		t.Fatalf("TTFTP95MS = %d, outside the observed 101..4000 range", got.TTFTP95MS)
	}
	// The mean feels the outlier: 40 samples near 120 plus 4000 gives roughly 215.
	if got.TTFTAvgMS < 200 {
		t.Fatalf("TTFTAvgMS = %d, want the outlier to pull the mean above 200", got.TTFTAvgMS)
	}
	// The nearest-rank p95 at n=41 indexes position 38, still inside the fast block, so it
	// must sit below the outlier and below the mean it does not track.
	if got.TTFTP95MS >= 4000 {
		t.Fatalf("TTFTP95MS = %d, nearest-rank p95 at n=41 must not select the maximum", got.TTFTP95MS)
	}
	if got.TTFTP95MS >= got.TTFTAvgMS {
		t.Fatalf("p95 %d should stay in the fast block below the outlier-dragged mean %d",
			got.TTFTP95MS, got.TTFTAvgMS)
	}
	// The p95 must still be a converged integer, not a truncated average.
	if got.TTFTP95MS < 130 || got.TTFTP95MS > 145 {
		t.Fatalf("TTFTP95MS = %d, want the fast-block sample near 139", got.TTFTP95MS)
	}
}

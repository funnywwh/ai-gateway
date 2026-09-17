package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

// seedProviderCostUsage writes one metered attempt carrying an explicit provider, cost and
// timestamp — the three facts the cost cap is computed from.
func seedProviderCostUsage(t *testing.T, db *DB, requestID string, providerID, costMicros int64, at time.Time) {
	t.Helper()
	if _, err := db.InsertUsage(context.Background(), &domain.UsageRecord{
		RequestID: requestID, AttemptNo: 1, AccountID: 1, APIKeyID: 1, ProviderID: providerID,
		Model: "m", ResolvedModel: "m", DimensionsJSON: `{"output":1}`,
		CostMicros: costMicros, ChargeMicros: costMicros * 2,
		Status: "completed", CreatedAt: at,
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

// A provider that never touched the cost fields carries the migration's defaults, and the
// three fields survive a round trip: the router reads them from the registry snapshot, so a
// lost value here would silently disable the cap.
func TestProviderCostColumnsRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	id, err := db.UpsertProvider(ctx, &domain.Provider{Name: "plain", Kind: "testecho"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := db.GetProvider(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if plain.CostLimitMicros != 0 || plain.CostPeriod != domain.CostPeriodNone || plain.CostWindowStart != nil {
		t.Fatalf("defaults = %d/%q/%v, want 0/none/nil", plain.CostLimitMicros, plain.CostPeriod, plain.CostWindowStart)
	}

	reset := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)
	capped := &domain.Provider{
		Name: "capped", Kind: "testecho",
		CostLimitMicros: 50_000_000, CostPeriod: domain.CostPeriodMonthly, CostWindowStart: &reset,
	}
	cappedID, err := db.UpsertProvider(ctx, capped)
	if err != nil {
		t.Fatal(err)
	}
	read, err := db.GetProvider(ctx, cappedID)
	if err != nil {
		t.Fatal(err)
	}
	if read.CostLimitMicros != 50_000_000 || read.CostPeriod != domain.CostPeriodMonthly {
		t.Fatalf("cost fields = %d/%q", read.CostLimitMicros, read.CostPeriod)
	}
	if read.CostWindowStart == nil || !read.CostWindowStart.Equal(reset) {
		t.Fatalf("window start = %v, want %s", read.CostWindowStart, reset)
	}

	// An empty period must be stored as the single spelling of "no period": a second
	// representation would show up as an unmatched option in the console's select.
	if _, err := db.UpsertProvider(ctx, &domain.Provider{Name: "empty-period", Kind: "testecho"}); err != nil {
		t.Fatal(err)
	}
	empty, err := db.GetProviderByName(ctx, "empty-period")
	if err != nil {
		t.Fatal(err)
	}
	if empty.CostPeriod != domain.CostPeriodNone {
		t.Fatalf("empty period stored as %q, want %q", empty.CostPeriod, domain.CostPeriodNone)
	}
}

// The bootstrap runner upserts providers by name, so a merge pass must carry the operator's
// cost cap through untouched. It does that by reading the existing row first; this test is what
// keeps a future rewrite of UpsertProvider from introducing "restart the gateway and the
// budget silently resets".
func TestBootstrapMergeKeepsTheProviderCostCap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	stateDir := t.TempDir()
	cfg := config.Bootstrap{
		Mode:      "upsert",
		Providers: []config.BootstrapProvider{{Name: "local", Kind: "testecho", Config: map[string]any{}}},
	}
	if _, err := db.Bootstrap(ctx, cfg, stateDir); err != nil {
		t.Fatal(err)
	}
	provider, err := db.GetProviderByName(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)
	provider.CostLimitMicros = 12_345_678
	provider.CostPeriod = domain.CostPeriodDaily
	provider.CostWindowStart = &reset
	if _, err := db.UpsertProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}

	cfg.Mode = "merge"
	if _, err := db.Bootstrap(ctx, cfg, stateDir); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetProviderByName(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if after.CostLimitMicros != 12_345_678 || after.CostPeriod != domain.CostPeriodDaily {
		t.Fatalf("bootstrap merge reset the cost cap to %d/%q", after.CostLimitMicros, after.CostPeriod)
	}
	if after.CostWindowStart == nil || !after.CostWindowStart.Equal(reset) {
		t.Fatalf("bootstrap merge reset the window start to %v", after.CostWindowStart)
	}
}

// ProviderCostsSince is the read the cap is enforced from, so its window semantics are the
// feature: only rows at or after each provider's own start instant count, and a provider with
// no rows in its window is 0 rather than absent.
func TestProviderCostsSinceRespectsEachWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	base := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)

	seedProviderCostUsage(t, db, "old-1", 7, 400, base.Add(-72*time.Hour))
	seedProviderCostUsage(t, db, "old-2", 7, 100, base.Add(-1*time.Hour))
	seedProviderCostUsage(t, db, "old-3", 7, 25, base)
	seedProviderCostUsage(t, db, "other-1", 9, 5_000, base.Add(-30*24*time.Hour))
	seedProviderCostUsage(t, db, "other-2", 9, 60, base.Add(-2*time.Hour))
	// A metering row with no provider (a request that never reached an upstream) must not
	// land in any bucket.
	seedProviderCostUsage(t, db, "unmetered", 0, 999, base)

	// 7 was reset a day ago, 9 counts everything, 11 never spent anything.
	got, err := db.ProviderCostsSince(ctx, map[int64]time.Time{
		7:  base.Add(-24 * time.Hour),
		9:  {},
		11: base.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]int64{7: 125, 9: 5_060, 11: 0}
	for id, micros := range want {
		if got[id] != micros {
			t.Fatalf("provider %d cost = %d, want %d (all: %v)", id, got[id], micros, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("result carries %d providers, want %d: %v", len(got), len(want), got)
	}

	// A window that starts after every row sees nothing, which is exactly what a reset
	// produces: the money spent before it stops counting without a single row changing.
	after, err := db.ProviderCostsSince(ctx, map[int64]time.Time{7: base.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if after[7] != 0 {
		t.Fatalf("cost after the reset = %d, want 0", after[7])
	}

	// No providers to look up means no query at all.
	empty, err := db.ProviderCostsSince(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty request returned %v", empty)
	}
	// A non-positive provider id is the "unknown" bucket, never a real provider.
	skipped, err := db.ProviderCostsSince(ctx, map[int64]time.Time{0: {}, -3: {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("non-positive ids must be skipped, got %v", skipped)
	}
}

// Providers whose windows are identical must share one query, and providers whose windows
// differ must not leak into each other's sums. The observable proof is the pair of numbers.
func TestProviderCostsSinceSeparatesWindowsOfTheSameProviderSet(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	base := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	seedProviderCostUsage(t, db, "a-1", 1, 10, base.Add(-2*time.Hour))
	seedProviderCostUsage(t, db, "a-2", 1, 20, base.Add(-1*time.Hour))
	seedProviderCostUsage(t, db, "b-1", 2, 30, base.Add(-2*time.Hour))
	seedProviderCostUsage(t, db, "b-2", 2, 40, base.Add(-1*time.Hour))

	got, err := db.ProviderCostsSince(ctx, map[int64]time.Time{
		1: base.Add(-90 * time.Minute), // sees only a-2
		2: base.Add(-3 * time.Hour),    // sees both
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != 20 || got[2] != 70 {
		t.Fatalf("costs = %v, want {1:20, 2:70}", got)
	}
}

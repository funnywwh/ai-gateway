package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// seedProviderUsage writes one metered attempt served by a named provider. providerID 0 is
// the unmetered/anonymous case: a metering row the gateway could not attribute.
func seedProviderUsage(t *testing.T, db *DB, requestID string, attempt int, providerID, cost, charge int64) {
	t.Helper()
	if _, err := db.InsertUsage(context.Background(), &domain.UsageRecord{
		RequestID: requestID, AttemptNo: attempt, AccountID: 1, APIKeyID: 1,
		Model: "m", ResolvedModel: "m", ProviderID: providerID,
		DimensionsJSON: `{"input":10,"output":5}`,
		CostMicros:     cost, ChargeMicros: charge,
		Status: "completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

func seedProvider(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	id, err := db.UpsertProvider(context.Background(), &domain.Provider{Name: name, Kind: "openai-chat", Enabled: true})
	if err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	return id
}

// The same model served by two providers is the case this dimension exists for: the model
// bucket can only show the sum, while the provider buckets have to keep each provider's own
// cost — that is what the per-provider pricing tables decide. A request that failed over
// carries money on both, and its request count is the number of requests each provider
// handled, so the two buckets do not add up to one request.
func TestProviderDimensionAttributesCostPerProvider(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	f := domain.RequestLogFilter{From: at, To: at.Add(time.Hour - time.Second)}

	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "failover", Model: "same-model", CreatedAt: at})
	seedProviderUsage(t, db, "failover", 1, 11, 100, 200)
	seedProviderUsage(t, db, "failover", 2, 22, 250, 500)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "single", Model: "same-model", CreatedAt: at})
	seedProviderUsage(t, db, "single", 1, 11, 7, 9)

	page, err := db.RequestLogDimensionsPage(ctx, f, "provider", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("provider buckets = %+v, want one per provider", page)
	}
	byKey := map[string]domain.RequestLogDimensionRow{}
	for _, row := range page.Rows {
		byKey[row.Key] = row
	}
	first, second := byKey["11"], byKey["22"]
	if first.CostMicros != 107 || first.ChargeMicros != 209 || first.Requests != 2 || first.Metered != 2 {
		t.Fatalf("provider 11 = %+v, want both of its attempts and only its own money", first)
	}
	if second.CostMicros != 250 || second.ChargeMicros != 500 || second.Requests != 1 || second.Metered != 1 {
		t.Fatalf("provider 22 = %+v, want its one failed-over attempt", second)
	}
	// The model dimension still reports the request-level total: the two dimensions answer
	// different questions about the same window, and the provider view must not have moved
	// money out of the model view.
	byModel, err := db.RequestLogDimensionsPage(ctx, f, "model", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel.Rows) != 1 || byModel.Rows[0].CostMicros != 357 || byModel.Rows[0].Requests != 2 {
		t.Fatalf("model bucket = %+v, want the two requests and all 357 micros", byModel)
	}
}

// A request that never reached an upstream has no provider, and dropping it would make the
// provider view quietly disagree with the request log about the window's size. It lands in
// the bucket whose key is empty, which the console renders as （未知）.
func TestProviderDimensionKeepsUnmeteredRequests(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "rejected", Client: "codex", CreatedAt: at})
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "served", Client: "codex", CreatedAt: at})
	seedProviderUsage(t, db, "served", 1, 5, 40, 80)

	page, err := db.RequestLogDimensionsPage(ctx, ctxFilter(at), "provider", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]domain.RequestLogDimensionRow{}
	for _, row := range page.Rows {
		byKey[row.Key] = row
	}
	if unknown := byKey["0"]; unknown.Requests != 1 || unknown.Metered != 0 || unknown.CostMicros != 0 {
		t.Fatalf("provider 0 bucket = %+v, want the one unmetered request", unknown)
	}
	if served := byKey["5"]; served.Requests != 1 || served.Metered != 1 || served.CostMicros != 40 {
		t.Fatalf("provider 5 bucket = %+v", served)
	}
}

func ctxFilter(at time.Time) domain.RequestLogFilter {
	return domain.RequestLogFilter{From: at, To: at.Add(time.Hour - time.Second)}
}

// The hourly rollups exist to make this page cheap, and they hold one contribution per
// request with the metering summed over its attempts — so they cannot answer "which
// provider". Reading them anyway is exactly how the provider dimension would report a
// plausible zero for a completed hour, which is why the read falls back to the attempt-grain
// source once any hour of the window is rolled up.
func TestProviderDimensionSurvivesRolledUpHours(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	f := ctxFilter(at)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "a", Client: "dsh", CreatedAt: at})
	seedProviderUsage(t, db, "a", 1, 3, 120, 240)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "b", Client: "dsh", CreatedAt: at})
	seedProviderUsage(t, db, "b", 1, 4, 60, 120)

	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := db.DimensionRollupStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.PendingHours != 0 {
		t.Fatalf("the hour is not rolled up, so this test would prove nothing: %+v", status)
	}

	page, err := db.RequestLogDimensionsPage(ctx, f, "provider", "charge", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("provider buckets over a rolled-up hour = %+v, want the two providers", page)
	}
	// Ordered by charge descending: the more expensive provider is first.
	if page.Rows[0].Key != "3" || page.Rows[0].CostMicros != 120 {
		t.Fatalf("first bucket = %+v, want provider 3 with its own cost", page.Rows[0])
	}
	if page.Rows[1].Key != "4" || page.Rows[1].CostMicros != 60 {
		t.Fatalf("second bucket = %+v", page.Rows[1])
	}
	// The same read with the summaries switched off must agree: the fallback is a source
	// change, not a different answer.
	db.SetDimensionRollupsEnabled(false)
	defer db.SetDimensionRollupsEnabled(true)
	raw, err := db.RequestLogDimensionsPage(ctx, f, "provider", "charge", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Total != page.Total || len(raw.Rows) != len(page.Rows) {
		t.Fatalf("raw %+v != rolled %+v", raw, page)
	}
	for i := range raw.Rows {
		if raw.Rows[i] != page.Rows[i] {
			t.Fatalf("bucket %d: raw %+v != rolled %+v", i, raw.Rows[i], page.Rows[i])
		}
	}
}

// The provider filter is a filter on requests, not on buckets: it selects the requests that
// provider served, and the page, the total and the breakdown all have to describe that same
// set — including the requests a second provider also served.
func TestProviderFilterSelectsServedRequests(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "only-1", Model: "m1", CreatedAt: at})
	seedProviderUsage(t, db, "only-1", 1, 1, 10, 20)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "only-2", Model: "m2", CreatedAt: at})
	seedProviderUsage(t, db, "only-2", 1, 2, 30, 60)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "both", Model: "m1", CreatedAt: at})
	seedProviderUsage(t, db, "both", 1, 1, 1, 2)
	seedProviderUsage(t, db, "both", 2, 2, 4, 8)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "none", Model: "m1", CreatedAt: at})

	base := ctxFilter(at)
	filtered := base
	filtered.ProviderID = 1
	rows, err := db.ListRequestLogs(ctx, filtered, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("filtered list = %d rows, want the two requests provider 1 served", len(rows))
	}
	total, err := db.CountRequestLogs(ctx, filtered)
	if err != nil || total != 2 {
		t.Fatalf("filtered total = %d (%v), want the same two requests", total, err)
	}

	// Grouped by model, the filtered breakdown counts requests (2) but sums only the metering
	// of the requests it selected — the failover request's provider-2 attempt included, since
	// the filter selects requests and this view is about those requests.
	page, err := db.RequestLogDimensionsPage(ctx, filtered, "model", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Fatalf("filtered model buckets = %+v, want one", page)
	}
	if page.Rows[0].Key != "m1" || page.Rows[0].Requests != 2 || page.Rows[0].CostMicros != 15 {
		t.Fatalf("filtered model bucket = %+v, want m1 with both requests' metering", page.Rows[0])
	}
	// The provider grouping under the same filter keeps only the selected requests, and then
	// splits all of their metering by provider: provider 1 served both selected requests,
	// while provider 2 appears with the failover attempt of the request it shares with
	// provider 1. The filter selected requests; the grouping is what attributes money.
	byProvider, err := db.RequestLogDimensionsPage(ctx, filtered, "provider", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if byProvider.Total != 2 || len(byProvider.Rows) != 2 {
		t.Fatalf("filtered provider buckets = %+v, want both providers of the two requests", byProvider)
	}
	providerRows := map[string]domain.RequestLogDimensionRow{}
	for _, row := range byProvider.Rows {
		providerRows[row.Key] = row
	}
	if one := providerRows["1"]; one.Requests != 2 || one.CostMicros != 11 {
		t.Fatalf("provider 1 under the filter = %+v, want both selected requests", one)
	}
	if two := providerRows["2"]; two.Requests != 1 || two.CostMicros != 4 {
		t.Fatalf("provider 2 under the filter = %+v, want the failover attempt of the shared request", two)
	}
}

// A provider filter cannot be answered from the hourly summaries at all: they are keyed by
// request_logs columns and keep no request id to correlate a metering row with. The read
// therefore has to go to the raw source, and it has to give the same answer before and after
// the summaries exist.
func TestProviderFilterBypassesRollups(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-5 * time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "kept", Client: "dsh", CreatedAt: at})
	seedProviderUsage(t, db, "kept", 1, 9, 11, 22)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "other", Client: "dsh", CreatedAt: at})
	seedProviderUsage(t, db, "other", 1, 8, 33, 66)

	filtered := ctxFilter(at)
	filtered.ProviderID = 9
	before, err := db.RequestLogDimensionsPage(ctx, filtered, "client", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := db.RequestLogDimensionsPage(ctx, filtered, "client", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflectEqualPage(before, after) {
		t.Fatalf("rolled %+v != raw %+v", after, before)
	}
	if before.Rows[0].Requests != 1 || before.Rows[0].CostMicros != 11 {
		t.Fatalf("bucket = %+v, want only the request provider 9 served", before.Rows[0])
	}
}

func reflectEqualPage(a, b domain.RequestLogDimensionPage) bool {
	if a.Total != b.Total || len(a.Rows) != len(b.Rows) {
		return false
	}
	for i := range a.Rows {
		if a.Rows[i] != b.Rows[i] {
			return false
		}
	}
	return true
}

// The page needs the attempts of its rows to name the providers and to draw the route path,
// and the names come from the providers table as read-time labels. A provider that was deleted
// after it metered traffic is simply absent from the map: the usage rows are billing records
// and outlive the configuration, so the caller keeps the id instead of blanking the cell.
func TestRequestAttemptsAndNames(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cheap := seedProvider(t, db, "cheap-upstream")
	dear := seedProvider(t, db, "dear-upstream")
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "r1"})
	seedProviderUsage(t, db, "r1", 1, dear, 5, 6)
	seedRouteAttempt(t, db, "r1", 2, cheap, 77, "deepseek-chat")
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "r2"})
	// A row metered before migration 0027 knows neither the route nor the upstream model; it
	// must come back as the zero value rather than being dropped from the path.
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "r3"})
	seedProviderUsage(t, db, "r3", 1, dear, 5, 6)

	attempts, err := db.RequestAttempts(ctx, []string{"r1", "r2", "r3", "missing", ""})
	if err != nil {
		t.Fatal(err)
	}
	path := attempts["r1"]
	want := []int64{dear, cheap}
	if len(path) != 2 {
		t.Fatalf("r1 attempts = %v, want both hops", path)
	}
	// The order is the order they were tried, which is what makes the list a route path.
	for i, attempt := range path {
		if attempt.AttemptNo != i+1 || attempt.ProviderID != want[i] {
			t.Fatalf("r1 attempt %d = %+v, want attempt_no %d on provider %d", i, attempt, i+1, want[i])
		}
	}
	if path[1].RouteID != 77 || path[1].UpstreamModel != "deepseek-chat" {
		t.Fatalf("r1 second hop = %+v, want the recorded route 77 and upstream model", path[1])
	}
	if path[1].Status != "completed" || path[1].CostMicros != 12 {
		t.Fatalf("r1 second hop result = %+v, want the metered result carried through", path[1])
	}
	if len(attempts["r3"]) != 1 || attempts["r3"][0].RouteID != 0 || attempts["r3"][0].UpstreamModel != "" {
		t.Fatalf("a pre-M78 attempt = %+v, want zero route and empty upstream model", attempts["r3"])
	}
	if _, ok := attempts["r2"]; ok {
		t.Fatal("a request with no metering row must have no attempt, not an empty entry")
	}
	if _, ok := attempts["missing"]; ok {
		t.Fatal("an unknown id must stay absent")
	}

	names, err := db.ProviderNames(ctx, []int64{dear, 4242, 0})
	if err != nil {
		t.Fatal(err)
	}
	if names[dear] != "dear-upstream" {
		t.Fatalf("names = %v, want the provider's name", names)
	}
	if _, ok := names[4242]; ok {
		t.Fatal("an id with no row must stay absent so the caller can show the id")
	}
}

// seedRouteAttempt writes one metered attempt that knows its route, the way the data plane
// records it (M78). routeID 0 and an empty upstream model are the pre-migration shape.
func seedRouteAttempt(t *testing.T, db *DB, requestID string, attempt int, providerID, routeID int64, upstream string) {
	t.Helper()
	if _, err := db.InsertUsage(context.Background(), &domain.UsageRecord{
		RequestID: requestID, AttemptNo: attempt, AccountID: 1, APIKeyID: 1,
		Model: "m", ResolvedModel: "m", ProviderID: providerID,
		RouteID: routeID, UpstreamModel: upstream,
		DimensionsJSON: `{"input":10,"output":5}`,
		CostMicros:     12, ChargeMicros: 24,
		Status: "completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

// A window that ends before it starts is a legal query the API can receive, and the provider
// dimension answers it like every other one: no rows, no error. It is worth a test of its own
// because this is the one path that builds a filter clause and then discards the selection —
// keeping the clause while dropping its arguments would surface as a bind error, not as an
// empty page.
func TestProviderDimensionEmptyWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "r", Client: "dsh", Model: "m", CreatedAt: at})
	seedProviderUsage(t, db, "r", 1, 4, 10, 20)

	// Both a filter and an inverted window: the two together are what would have left a
	// placeholder without an argument.
	empty := domain.RequestLogFilter{From: at.Add(time.Hour), To: at, Client: "dsh", ProviderID: 4}
	page, err := db.RequestLogDimensionsPage(ctx, empty, "provider", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil {
		t.Fatalf("empty window: %v", err)
	}
	if page.Total != 0 || len(page.Rows) != 0 {
		t.Fatalf("empty window returned %+v", page)
	}
	// The same window grouped by a request-level dimension is unaffected, and so is the
	// convenience count path (which builds its own reference query).
	byClient, err := db.RequestLogDimensionsPage(ctx, empty, "client", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil || byClient.Total != 0 || len(byClient.Rows) != 0 {
		t.Fatalf("empty window by client = %+v (%v)", byClient, err)
	}
	total, err := db.CountRequestLogDimensionGroups(ctx, empty, "provider")
	if err != nil || total != 0 {
		t.Fatalf("empty window bucket count = %d (%v)", total, err)
	}
	// And the window that does contain the row still answers, so the empty case is not
	// passing because everything returns nothing.
	full := domain.RequestLogFilter{From: at, To: at.Add(time.Hour - time.Second), Client: "dsh"}
	page, err = db.RequestLogDimensionsPage(ctx, full, "provider", RequestLogDimensionDefaultSort, 20, 0)
	if err != nil || page.Total != 1 || page.Rows[0].Key != "4" {
		t.Fatalf("non-empty window = %+v (%v)", page, err)
	}
}

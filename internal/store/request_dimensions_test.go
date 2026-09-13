package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// seedDimensionRow writes one recorded request with an identity.
func seedDimensionRow(t *testing.T, db *DB, rec *domain.RequestLogRecord) {
	t.Helper()
	if rec.RequestJSON == "" {
		rec.RequestJSON = `{"model":"m","input":"ping"}`
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if err := db.PutRequestLog(context.Background(), rec); err != nil {
		t.Fatalf("put request log: %v", err)
	}
}

// seedUsage writes one metered attempt for a request.
func seedUsage(t *testing.T, db *DB, requestID string, attempt int, dims string, cost, charge int64) {
	t.Helper()
	if _, err := db.InsertUsage(context.Background(), &domain.UsageRecord{
		RequestID: requestID, AttemptNo: attempt, AccountID: 1, APIKeyID: 1,
		Model: "m", ResolvedModel: "m", DimensionsJSON: dims,
		CostMicros: cost, ChargeMicros: charge, LatencyMS: 100 * attempt, TTFTMS: 10 * attempt,
		Status: "completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

// The identity columns survive a write/read round trip, on both the page and the detail
// projection (they share requestLogColumns, and this is what pins that they agree).
func TestRequestLogIdentityRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedDimensionRow(t, db, &domain.RequestLogRecord{
		RequestID: "req_identity1", AccountID: 7, APIKeyID: 3, Endpoint: "/v1/responses",
		Status: "completed", RecordInputMode: "user",
		Client: "dsh", Model: "luna", ResolvedModel: "gpt-5.6-luna",
		Workspace: "/home/winger/work/ai_gateway", SessionID: "session-abc",
		CallKind: "title", Title: "从 dsh 和 codex 请求解析 workspace",
	})

	page, err := db.ListRequestLogsPage(ctx, domain.RequestLogFilter{AccountID: 7}, 10, 0)
	if err != nil || len(page) != 1 {
		t.Fatalf("page = %v (err %v)", page, err)
	}
	got := page[0]
	if got.Client != "dsh" || got.Model != "luna" || got.ResolvedModel != "gpt-5.6-luna" ||
		got.Workspace != "/home/winger/work/ai_gateway" || got.SessionID != "session-abc" ||
		got.CallKind != "title" || got.Title != "从 dsh 和 codex 请求解析 workspace" {
		t.Fatalf("page row lost the identity: %+v", got)
	}

	detail, err := db.GetRequestLog(ctx, "req_identity1")
	if err != nil {
		t.Fatal(err)
	}
	if *detail != *got {
		t.Fatalf("detail and page disagree:\n detail=%+v\n page  =%+v", detail, got)
	}
}

// A second write of the same request id (the skeleton retry) must not blank the identity
// the first write captured.
func TestRequestLogConflictKeepsIdentity(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedDimensionRow(t, db, &domain.RequestLogRecord{
		RequestID: "req_identity2", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", Model: "luna", Workspace: "/w/a", SessionID: "session-abc",
	})
	// The skeleton fallback: same request, content stripped, no identity computed again.
	if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{
		RequestID: "req_identity2", AccountID: 1, APIKeyID: 1, Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetRequestLog(ctx, "req_identity2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %q, want the retry's", got.Status)
	}
	if got.Client != "dsh" || got.Model != "luna" || got.SessionID != "session-abc" {
		t.Fatalf("the conflict update must not blank the identity: %+v", got)
	}
}

func TestRequestLogDimensionFiltersNarrowPageAndCount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rows := []*domain.RequestLogRecord{
		{RequestID: "req-f1", AccountID: 1, Client: "dsh", Model: "luna", ResolvedModel: "gpt-5.6-luna",
			Workspace: "/w/a", SessionID: "s1", CallKind: "agent"},
		{RequestID: "req-f2", AccountID: 1, Client: "dsh", Model: "luna", ResolvedModel: "gpt-5.6-luna",
			Workspace: "/w/a", SessionID: "s1", CallKind: "title"},
		{RequestID: "req-f3", AccountID: 1, Client: "codex", Model: "luna", ResolvedModel: "gpt-5.6-luna",
			Workspace: "/w/b", SessionID: "s2", CallKind: "agent"},
		{RequestID: "req-f4", AccountID: 1, Client: "codex", Model: "other", ResolvedModel: "other",
			Workspace: "/w/b", SessionID: "s2", CallKind: "agent"},
	}
	for _, rec := range rows {
		seedDimensionRow(t, db, rec)
	}

	cases := []struct {
		name   string
		filter domain.RequestLogFilter
		want   int
	}{
		{"client", domain.RequestLogFilter{Client: "dsh"}, 2},
		{"model", domain.RequestLogFilter{Model: "luna"}, 3},
		{"resolved_model", domain.RequestLogFilter{ResolvedModel: "other"}, 1},
		{"workspace", domain.RequestLogFilter{Workspace: "/w/a"}, 2},
		{"session", domain.RequestLogFilter{SessionID: "s2"}, 2},
		{"call_kind", domain.RequestLogFilter{CallKind: "title"}, 1},
		{"client+session", domain.RequestLogFilter{Client: "codex", SessionID: "s2"}, 2},
		{"no match", domain.RequestLogFilter{Client: "claude"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := db.ListRequestLogsPage(ctx, tc.filter, 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			total, err := db.CountRequestLogs(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) != tc.want || total != tc.want {
				t.Fatalf("page=%d total=%d, want %d for both", len(page), total, tc.want)
			}
		})
	}
}

// Consumption is summed over a request's attempts and read from the metering table.
func TestRequestUsagesSumAttemptsAndReportMetering(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedUsage(t, db, "req-u1", 1, `{"input_cache_hit":100,"input_cache_miss":20,"output":7,"reasoning":3}`, 10, 20)
	seedUsage(t, db, "req-u2", 1, `{"input":50,"output":5}`, 3, 6)
	// A failover retry: the second attempt is metered too and both are counted.
	seedUsage(t, db, "req-u2", 2, `{"input":10,"output":1}`, 1, 2)

	got, err := db.RequestUsages(ctx, []string{"req-u1", "req-u2", "req-unmetered", ""})
	if err != nil {
		t.Fatal(err)
	}
	u1 := got["req-u1"]
	if u1 == nil || !u1.Metered || u1.Attempts != 1 {
		t.Fatalf("req-u1 = %+v", u1)
	}
	if u1.InputTokens != 120 || u1.CachedTokens != 100 || u1.OutputTokens != 7 || u1.ReasoningTokens != 3 {
		t.Fatalf("req-u1 tokens = %+v, want input 120 (hit+miss), output 7, reasoning 3", u1)
	}
	if u1.CostMicros != 10 || u1.ChargeMicros != 20 {
		t.Fatalf("req-u1 money = %+v", u1)
	}
	seedUsage(t, db, "req-u1", 2, `{"input_cache_hit":30,"input_cache_miss":5}`, 0, 0)
	retried, err := db.RequestUsage(ctx, "req-u1")
	if err != nil || retried.CachedTokens != 130 || retried.InputTokens != 155 {
		t.Fatalf("cached tokens across attempts = %+v (err %v)", retried, err)
	}
	u2 := got["req-u2"]
	if u2.Attempts != 2 || u2.InputTokens != 60 || u2.CachedTokens != 0 || u2.OutputTokens != 6 || u2.ChargeMicros != 8 {
		t.Fatalf("req-u2 = %+v, want both attempts summed", u2)
	}
	if u2.LatencyMS != 200 || u2.TTFTMS != 20 {
		t.Fatalf("req-u2 latency = %+v, want the worst attempt", u2)
	}
	if _, ok := got["req-unmetered"]; ok {
		t.Fatal("a request with no usage row must not appear in the map")
	}
	if _, ok := got[""]; ok {
		t.Fatal("empty ids must be ignored")
	}

	single, err := db.RequestUsage(ctx, "req-unmetered")
	if err != nil {
		t.Fatal(err)
	}
	if single.Metered || single.RequestID != "req-unmetered" {
		t.Fatalf("unmetered single lookup = %+v", single)
	}
	if empty, err := db.RequestUsage(ctx, ""); err != nil || empty.Metered {
		t.Fatalf("empty id lookup = %+v (err %v)", empty, err)
	}
}

func TestRequestLogDimensionsGroupAndSum(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedDimensionRow(t, db, &domain.RequestLogRecord{
		RequestID: "req-d1", AccountID: 1, Client: "dsh", Model: "deepseek-flash",
		Workspace: "/w/a", SessionID: "s1", CallKind: "agent",
	})
	seedDimensionRow(t, db, &domain.RequestLogRecord{
		RequestID: "req-d2", AccountID: 1, Client: "dsh", Model: "deepseek-flash",
		Workspace: "/w/a", SessionID: "s1", CallKind: "title", Title: "会话标题",
	})
	seedDimensionRow(t, db, &domain.RequestLogRecord{
		RequestID: "req-d3", AccountID: 1, Client: "codex", Model: "gpt-5.6-luna",
		Workspace: "/w/b", SessionID: "s2", CallKind: "agent",
	})
	seedUsage(t, db, "req-d1", 1, `{"input_cache_hit":6,"input_cache_miss":4,"output":2}`, 5, 9)
	seedUsage(t, db, "req-d2", 1, `{"input":4,"output":1}`, 2, 4)

	rows, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "requests", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("client buckets = %+v, want 2", rows)
	}
	// Ordered by request count, so dsh (2) leads.
	dsh := rows[0]
	if dsh.Key != "dsh" || dsh.Requests != 2 || dsh.Metered != 2 {
		t.Fatalf("dsh bucket = %+v", dsh)
	}
	if dsh.InputTokens != 14 || dsh.CachedTokens != 6 || dsh.OutputTokens != 3 || dsh.CostMicros != 7 || dsh.ChargeMicros != 13 {
		t.Fatalf("dsh bucket totals = %+v", dsh)
	}
	if dsh.FirstSeen.IsZero() || dsh.LastSeen.IsZero() {
		t.Fatalf("bucket window = %+v", dsh)
	}
	if codex := rows[1]; codex.Key != "codex" || codex.Metered != 0 || codex.CachedTokens != 0 {
		t.Fatalf("codex bucket = %+v, want an unmetered bucket", codex)
	}

	sessions, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "session", RequestLogDimensionDefaultSort, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var s1 *domain.RequestLogDimensionRow
	for i := range sessions {
		if sessions[i].Key == "s1" {
			s1 = &sessions[i]
		}
	}
	if s1 == nil {
		t.Fatalf("session bucket s1 missing: %+v", sessions)
	}
	if s1.Title != "会话标题" || s1.Workspace != "/w/a" {
		t.Fatalf("session bucket lost its title/workspace: %+v", s1)
	}

	// A metered row without a cache dimension contributes zero cached tokens.
	uncached, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{CallKind: "title"}, "client", RequestLogDimensionDefaultSort, 10, 0)
	if err != nil || len(uncached) != 1 {
		t.Fatalf("uncached breakdown = %+v (err %v)", uncached, err)
	}
	if uncached[0].CachedTokens != 0 || uncached[0].InputTokens != 4 || uncached[0].Metered != 1 {
		t.Fatalf("missing cache dimension must aggregate to zero: %+v", uncached[0])
	}

	// A filter narrows the breakdown the same way it narrows the list.
	filtered, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{Client: "codex"}, "client", RequestLogDimensionDefaultSort, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Key != "codex" || filtered[0].Requests != 1 {
		t.Fatalf("filtered breakdown = %+v", filtered)
	}
}

// A page and its total must describe the same buckets, and the only reason LIMIT/OFFSET is
// exact here is that the order is total: these 201 buckets were all written in the same
// second, so they tie on time, and paging them without a repeat or a gap is the group-key
// tiebreaker doing its job (docs/design/m31-request-log-stats-pagination.md D6).
func TestRequestLogDimensionsPaging(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const buckets = 201
	stamp := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < buckets; i++ {
		seedDimensionRow(t, db, &domain.RequestLogRecord{
			RequestID: fmt.Sprintf("req-page-%03d", i),
			AccountID: 1, APIKeyID: 1, Client: fmt.Sprintf("c%03d", i), CreatedAt: stamp,
		})
	}

	total, err := db.CountRequestLogDimensionGroups(ctx, domain.RequestLogFilter{}, "client")
	if err != nil {
		t.Fatal(err)
	}
	if total != buckets {
		t.Fatalf("group count = %d, want %d", total, buckets)
	}

	// A caller that forgets limit gets the endpoint default; one asking above the ceiling
	// gets the ceiling rather than an error, and never the whole window.
	byDefault, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", RequestLogDimensionDefaultSort, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byDefault) != 20 {
		t.Fatalf("limit 0 returned %d buckets, want the default 20", len(byDefault))
	}
	capped, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", RequestLogDimensionDefaultSort, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 200 {
		t.Fatalf("limit 1000 returned %d buckets, want the cap 200", len(capped))
	}
	// Every bucket ties on both the sort key and the time, so the order can only come from
	// the group key — and the first page is therefore the first twenty keys, in order.
	if byDefault[0].Key != "c000" || byDefault[19].Key != "c019" {
		t.Fatalf("tied buckets are not ordered by group key: %q … %q", byDefault[0].Key, byDefault[19].Key)
	}

	// Walk every page: the union must be exactly the seeded keys, each exactly once.
	seen := map[string]int{}
	pages := 0
	for offset := 0; offset <= buckets+20; offset += 20 {
		page, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "requests", 20, offset)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for _, row := range page {
			seen[row.Key]++
		}
	}
	if len(seen) != buckets {
		t.Fatalf("paging reached %d distinct buckets, want %d", len(seen), buckets)
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("bucket %q appeared %d times across pages", key, count)
		}
	}
	if pages != 11 {
		t.Fatalf("walked %d pages of 20, want 11 (10 full + 1)", pages)
	}

	// An offset past the end is an empty page, not an error; a negative one is the first
	// page; and neither moves the total.
	beyond, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "requests", 20, buckets+50)
	if err != nil {
		t.Fatal(err)
	}
	if len(beyond) != 0 {
		t.Fatalf("an offset past the end returned %d buckets, want none", len(beyond))
	}
	negative, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "requests", 20, -5)
	if err != nil {
		t.Fatal(err)
	}
	if len(negative) != 20 || negative[0].Key != "c000" {
		t.Fatalf("a negative offset must clamp to the first page: %d buckets starting at %q", len(negative), negative[0].Key)
	}
	stillTotal, err := db.CountRequestLogDimensionGroups(ctx, domain.RequestLogFilter{}, "client")
	if err != nil {
		t.Fatal(err)
	}
	if stillTotal != buckets {
		t.Fatalf("group count changed after paging: %d", stillTotal)
	}

	// A filter narrows the page and the count together, so "共 N 个分组" keeps describing
	// the buckets the pages actually hold.
	filtered, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{Client: "c000"}, "client", "requests", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	filteredTotal, err := db.CountRequestLogDimensionGroups(ctx, domain.RequestLogFilter{Client: "c000"}, "client")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filteredTotal != 1 || filtered[0].Key != "c000" {
		t.Fatalf("filtered page=%+v total=%d, want the single c000 bucket", filtered, filteredTotal)
	}
}

// The three sort keys are the three questions the console's card can be asked, and each
// one has to put a different bucket first — otherwise the switch would look broken while
// every assertion on "a table rendered" still passed.
func TestRequestLogDimensionSorts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	// a: 3 requests, most recent, cheapest · b: 2 requests, oldest, most expensive ·
	// c: 1 request, in between. So last_seen → a,c,b · requests → a,b,c · charge → b,c,a.
	buckets := []struct {
		key     string
		count   int
		minutes int
		charge  int64
	}{
		{"a", 3, 1, 100},
		{"b", 2, 3, 450},
		{"c", 1, 2, 500},
	}
	for _, bucket := range buckets {
		for i := 0; i < bucket.count; i++ {
			id := fmt.Sprintf("req-sort-%s-%d", bucket.key, i)
			seedDimensionRow(t, db, &domain.RequestLogRecord{
				RequestID: id, AccountID: 1, APIKeyID: 1, Client: bucket.key,
				// The newest request of the bucket is what last_seen reports.
				CreatedAt: base.Add(-time.Duration(bucket.minutes) * time.Minute).Add(time.Duration(i) * time.Second),
			})
			seedUsage(t, db, id, 1, `{"input":10,"output":2}`, 1, bucket.charge)
		}
	}

	if RequestLogDimensionSorts[0] != RequestLogDimensionDefaultSort {
		t.Fatalf("the whitelist must list the default first: %v vs %q", RequestLogDimensionSorts, RequestLogDimensionDefaultSort)
	}
	// Every key must end on the group key: an ORDER BY without a unique tail cannot be
	// paged, and this is the cheapest place to say so (the behavioral half is
	// TestRequestLogDimensionsPaging, which pages 201 tied buckets).
	for _, key := range RequestLogDimensionSorts {
		order, err := requestLogDimensionSortExpr(key)
		if err != nil {
			t.Fatalf("sort %q: %v", key, err)
		}
		if !strings.HasSuffix(order, "group_key ASC") {
			t.Fatalf("sort %q does not tie-break on the group key: %q", key, order)
		}
	}
	cases := []struct {
		sort string
		want string
	}{
		{RequestLogDimensionDefaultSort, "a,c,b"},
		{"", "a,c,b"}, // an absent sort is the default
		{"requests", "a,b,c"},
		{"charge", "b,c,a"},
	}
	for _, tc := range cases {
		rows, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", tc.sort, 10, 0)
		if err != nil {
			t.Fatalf("sort %q: %v", tc.sort, err)
		}
		got := make([]string, 0, len(rows))
		for _, row := range rows {
			got = append(got, row.Key)
		}
		if joined := strings.Join(got, ","); joined != tc.want {
			t.Fatalf("sort %q ordered %s, want %s", tc.sort, joined, tc.want)
		}
		// The bucket count is a property of the set, not of the order it is read in.
		total, err := db.CountRequestLogDimensionGroups(ctx, domain.RequestLogFilter{}, "client")
		if err != nil {
			t.Fatal(err)
		}
		if total != len(buckets) {
			t.Fatalf("sort %q changed the group count: %d", tc.sort, total)
		}
	}

	// An unknown key is rejected by name, the same shape as an unknown group_by: a silently
	// ignored sort answers the question that was not asked.
	_, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "tokens", 10, 0)
	if err == nil || !strings.Contains(err.Error(), "sort must be") {
		t.Fatalf("err = %v, want a sort error naming the accepted values", err)
	}
	for _, name := range RequestLogDimensionSorts {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the rejection must name %q: %v", name, err)
		}
	}
}

func TestRequestLogDimensionsRejectsUnknownGrouping(t *testing.T) {
	db := testDB(t)
	_, err := db.ListRequestLogDimensionsPage(context.Background(), domain.RequestLogFilter{}, "password", RequestLogDimensionDefaultSort, 10, 0)
	if err == nil || !strings.Contains(err.Error(), "group_by must be") {
		t.Fatalf("err = %v, want a group_by error naming the accepted values", err)
	}
	// The message is what a caller reads after a typo, so it has to name every accepted
	// value — including the two credential dimensions (M30).
	for _, name := range RequestLogDimensionNames {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the rejection must name %q: %v", name, err)
		}
	}
}

// The credential filters (M30) are exact matches on the columns the request authenticated
// with, and the credential groupings bucket on those columns' ids.
func TestRequestLogFiltersAndGroupsByOwner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rows := []*domain.RequestLogRecord{
		{RequestID: "req-o1", AccountID: 1, APIKeyID: 11, Client: "dsh", Model: "m"},
		{RequestID: "req-o2", AccountID: 1, APIKeyID: 11, Client: "dsh", Model: "m"},
		{RequestID: "req-o3", AccountID: 1, APIKeyID: 12, Client: "codex", Model: "m"},
		{RequestID: "req-o4", AccountID: 2, APIKeyID: 21, Client: "dsh", Model: "m"},
		// A row written without a credential (the unknown bucket).
		{RequestID: "req-o5", AccountID: 0, APIKeyID: 0, Client: "unknown", Model: "m"},
	}
	for _, rec := range rows {
		seedDimensionRow(t, db, rec)
	}

	cases := []struct {
		name   string
		filter domain.RequestLogFilter
		want   int
	}{
		{"api_key", domain.RequestLogFilter{APIKeyID: 11}, 2},
		{"account", domain.RequestLogFilter{AccountID: 1}, 3},
		{"api_key+account", domain.RequestLogFilter{AccountID: 1, APIKeyID: 12}, 1},
		{"api_key of another account", domain.RequestLogFilter{AccountID: 2, APIKeyID: 11}, 0},
		{"unknown credential", domain.RequestLogFilter{AccountID: 0, APIKeyID: 0}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := db.ListRequestLogsPage(ctx, tc.filter, 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			total, err := db.CountRequestLogs(ctx, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) != tc.want || total != tc.want {
				t.Fatalf("page=%d total=%d, want %d for both", len(page), total, tc.want)
			}
		})
	}

	// The account grouping returns the id as text (the caller resolves the name); the
	// counts are what this case is about, so the order it reads them in does not matter.
	byAccount, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "account", RequestLogDimensionDefaultSort, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, row := range byAccount {
		counts[row.Key] = row.Requests
	}
	if counts["1"] != 3 || counts["2"] != 1 || counts["0"] != 1 {
		t.Fatalf("account buckets = %v, want 1→3, 2→1, 0→1 (the unknown bucket is kept)", counts)
	}

	byKey, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "api_key", RequestLogDimensionDefaultSort, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	keyCounts := map[string]int{}
	for _, row := range byKey {
		keyCounts[row.Key] = row.Requests
	}
	if keyCounts["11"] != 2 || keyCounts["12"] != 1 || keyCounts["21"] != 1 || keyCounts["0"] != 1 {
		t.Fatalf("api key buckets = %v", keyCounts)
	}

	// A credential filter narrows the breakdown the same way it narrows the list.
	filtered, err := db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{APIKeyID: 11}, "api_key", RequestLogDimensionDefaultSort, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Key != "11" || filtered[0].Requests != 2 {
		t.Fatalf("filtered breakdown = %+v", filtered)
	}
}

// Names are read-time labels: one batched point lookup per page or per breakdown, keyed by
// id. An id with no row is absent (not an error) — the log row keeps its id and the
// console shows the id instead of a blank.
func TestOwnerLabelLookupsAreBatchedAndTolerant(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.UpsertAccount(ctx, &domain.Account{
		Name: "acme", BillingMode: domain.BillingPostpaid, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAPIKey(ctx, &domain.APIKey{
		AccountID: 1, Name: "dev-key", KeyPrefix: "sk-gw-abcdef", KeyHash: "hash",
	}); err != nil {
		t.Fatal(err)
	}

	names, err := db.AccountNames(ctx, []int64{1, 1, 0, -3, 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[1] != "acme" {
		t.Fatalf("account names = %v, want only id 1 (duplicates and unknown ids dropped)", names)
	}

	labels, err := db.APIKeyLabels(ctx, []int64{1, 1, 0, 42})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 {
		t.Fatalf("api key labels = %v, want only the key that exists", labels)
	}
	if got := labels[1]; got.Name != "dev-key" || got.Prefix != "sk-gw-abcdef" {
		t.Fatalf("label = %+v", got)
	}

	// Empty and all-invalid input answer with empty maps rather than nil: callers index
	// them without a nil check.
	if empty, err := db.AccountNames(ctx, nil); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty lookup = %v (err %v)", empty, err)
	}
	if empty, err := db.APIKeyLabels(ctx, []int64{0}); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("invalid-only lookup = %v (err %v)", empty, err)
	}
}

// The credential dimensions travel with the row and, like the identity columns, are never
// refreshed by a conflict update: the content-free skeleton retry writes the same request
// id with nothing recomputed, and it must not blank the account or the key (M30).
func TestRequestLogConflictKeepsOwner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedDimensionRow(t, db, &domain.RequestLogRecord{
		RequestID: "req-owner-keep", AccountID: 7, APIKeyID: 3, Status: "completed", Client: "dsh",
	})
	if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{
		RequestID: "req-owner-keep", Status: "failed",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetRequestLog(ctx, "req-owner-keep")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != 7 || got.APIKeyID != 3 {
		t.Fatalf("the conflict update blanked the credential dimensions: %+v", got)
	}
}

// The breakdown must never read the recorded bodies: selecting request_json would make
// SQLite pull every row's overflow pages into the window scan, which is the cost M24
// removed from the console's list (docs/design/m24-console-pagination.md §8.10). The
// assertion runs against the store's own builders so it cannot drift from production.
func TestRequestLogDimensionQueryAvoidsBodies(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "req-q1", AccountID: 1, Client: "dsh"})
	since := time.Now().UTC().AddDate(0, 0, -7)

	where, args := requestLogFilter("r.", domain.RequestLogFilter{From: since})
	query := requestLogDimensionsSQL("r.client", "MAX(r.created_at) DESC, group_key ASC", where)
	if strings.Contains(query, "request_json") {
		t.Fatalf("the dimension breakdown must not select request_json:\n%s", query)
	}

	// And the statement must actually run — with the paging tail it now carries.
	rows, err := db.read.QueryContext(ctx, query, append(args, 10, 0)...)
	if err != nil {
		t.Fatalf("run breakdown: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the breakdown returned no bucket for one recorded row")
	}
	rows.Close()

	// The bucket count is what the pager shows as "共 N 个分组", so it must not pay for the
	// usage join the paged query needs: the WHERE names only request_logs columns and a LEFT
	// JOIN can neither add nor drop a left-hand row, so the counting statement drops it
	// (docs/design/m31-request-log-stats-pagination.md D9).
	countSQL := requestLogDimensionCountSQL("r.client", where)
	for _, banned := range []string{"request_json", "usage_records"} {
		if strings.Contains(countSQL, banned) {
			t.Fatalf("the bucket count must not touch %s:\n%s", banned, countSQL)
		}
	}
	total, err := db.CountRequestLogDimensionGroups(ctx, domain.RequestLogFilter{From: since}, "client")
	if err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	if total != 1 {
		t.Fatalf("bucket count = %d, want the one recorded bucket", total)
	}
}

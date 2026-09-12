package store

import (
	"context"
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
	if u1.InputTokens != 120 || u1.OutputTokens != 7 || u1.ReasoningTokens != 3 {
		t.Fatalf("req-u1 tokens = %+v, want input 120 (hit+miss), output 7, reasoning 3", u1)
	}
	if u1.CostMicros != 10 || u1.ChargeMicros != 20 {
		t.Fatalf("req-u1 money = %+v", u1)
	}
	u2 := got["req-u2"]
	if u2.Attempts != 2 || u2.InputTokens != 60 || u2.OutputTokens != 6 || u2.ChargeMicros != 8 {
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
	seedUsage(t, db, "req-d1", 1, `{"input":10,"output":2}`, 5, 9)
	seedUsage(t, db, "req-d2", 1, `{"input":4,"output":1}`, 2, 4)

	rows, err := db.RequestLogDimensions(ctx, domain.RequestLogFilter{}, "client", 10)
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
	if dsh.InputTokens != 14 || dsh.OutputTokens != 3 || dsh.CostMicros != 7 || dsh.ChargeMicros != 13 {
		t.Fatalf("dsh bucket totals = %+v", dsh)
	}
	if dsh.FirstSeen.IsZero() || dsh.LastSeen.IsZero() {
		t.Fatalf("bucket window = %+v", dsh)
	}
	if codex := rows[1]; codex.Key != "codex" || codex.Metered != 0 {
		t.Fatalf("codex bucket = %+v, want an unmetered bucket", codex)
	}

	sessions, err := db.RequestLogDimensions(ctx, domain.RequestLogFilter{}, "session", 10)
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

	// A filter narrows the breakdown the same way it narrows the list.
	filtered, err := db.RequestLogDimensions(ctx, domain.RequestLogFilter{Client: "codex"}, "client", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Key != "codex" || filtered[0].Requests != 1 {
		t.Fatalf("filtered breakdown = %+v", filtered)
	}
}

func TestRequestLogDimensionsRejectsUnknownGrouping(t *testing.T) {
	db := testDB(t)
	_, err := db.RequestLogDimensions(context.Background(), domain.RequestLogFilter{}, "password", 10)
	if err == nil || !strings.Contains(err.Error(), "group_by must be") {
		t.Fatalf("err = %v, want a group_by error naming the accepted values", err)
	}
}

// The breakdown must never read the recorded bodies: selecting request_json would make
// SQLite pull every row's overflow pages into the window scan, which is the cost M24
// removed from the console's list (docs/design/m24-console-pagination.md §8.10). The
// assertion runs against the store's own builder so it cannot drift from production.
func TestRequestLogDimensionQueryAvoidsBodies(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "req-q1", AccountID: 1, Client: "dsh"})

	where, args := requestLogFilter("r.", domain.RequestLogFilter{From: time.Now().UTC().AddDate(0, 0, -7)})
	query := requestLogDimensionsSQL("r.client", where)
	if strings.Contains(query, "request_json") {
		t.Fatalf("the dimension breakdown must not select request_json:\n%s", query)
	}

	// And the statement must actually run.
	rows, err := db.read.QueryContext(ctx, query, append(args, 10)...)
	if err != nil {
		t.Fatalf("run breakdown: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the breakdown returned no bucket for one recorded row")
	}
}

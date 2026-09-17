package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

func TestDimensionRollupCountsRequestsAndLateUsage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "retry", Client: "codex", CreatedAt: at})
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "rejected", Client: "codex", CreatedAt: at})
	seedUsage(t, db, "retry", 1, `{"input":10,"input_cache_hit":3}`, 5, 7)
	seedUsage(t, db, "retry", 2, `{"input":20,"output":4}`, 6, 8)
	f := domain.RequestLogFilter{From: at, To: at.Add(time.Hour - time.Second)}
	read := func() domain.RequestLogDimensionPage {
		t.Helper()
		p, err := db.RequestLogDimensionsPage(ctx, f, "client", "requests", 20, 0)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	want := read()
	if want.Total != 1 || len(want.Rows) != 1 || want.Rows[0].Requests != 2 || want.Rows[0].Metered != 1 || want.Rows[0].InputTokens != 33 {
		t.Fatalf("request-level counts: %+v", want)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rollup: %+v, want %+v", got, want)
	}
	seedUsage(t, db, "retry", 3, `{"input":1}`, 1, 1)
	if got := read(); got.Rows[0].InputTokens != 34 || got.Rows[0].Requests != 2 {
		t.Fatalf("late usage: %+v", got)
	}
	if _, err := db.PruneRequestLogs(ctx, at.Add(time.Second), 1); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Rows[0].Requests != 1 {
		t.Fatalf("prune must immediately invalidate: %+v", got)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Rows[0].Requests != 1 {
		t.Fatalf("rebuilt prune: %+v", got)
	}
}

// The oracle is the original join query, with DISTINCT request counters. It does not
// share the contribution/rollup implementation, so agreement checks actual semantics. It
// resolves the bucket expression itself, exactly like the reference query it drives (the
// provider dimension's key is spelled differently on the two paths).
func dimensionOracle(t *testing.T, db *DB, f domain.RequestLogFilter, group, sortKey string, limit, offset int) domain.RequestLogDimensionPage {
	t.Helper()
	order, err := requestLogDimensionSortExpr(sortKey)
	if err != nil {
		t.Fatal(err)
	}
	where, args := requestLogFilter("r.", f)
	query, err := requestLogDimensionsSQL(group, order, where)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.read.QueryContext(context.Background(), query, append(args, limit, offset)...)
	if err != nil {
		t.Fatal(err)
	}
	p := domain.RequestLogDimensionPage{Rows: []domain.RequestLogDimensionRow{}}
	for rows.Next() {
		var row domain.RequestLogDimensionRow
		var first, last int64
		if err := rows.Scan(&row.Key, &row.Requests, &row.Metered, &first, &last, &row.Title, &row.Workspace, &row.InputTokens, &row.CachedTokens, &row.OutputTokens, &row.ReasoningTokens, &row.CostMicros, &row.ChargeMicros); err != nil {
			t.Fatal(err)
		}
		row.FirstSeen = timeFromUnix(first)
		row.LastSeen = timeFromUnix(last)
		p.Rows = append(p.Rows, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	p.Total, err = db.CountRequestLogDimensionGroups(context.Background(), f, group)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDimensionRollupAllFiltersAndBoundaries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	times := []time.Time{h.Add(-time.Second), h, h.Add(time.Second), h.Add(3599 * time.Second), h.Add(time.Hour), h.Add(2 * time.Hour), h.Add(3 * time.Hour)}
	for i := 0; i < 63; i++ {
		id := fmt.Sprintf("r-%d", i)
		rec := &domain.RequestLogRecord{RequestID: id, AccountID: int64(i % 3), APIKeyID: int64(i % 5), Client: []string{"", "codex", "dsh"}[i%3], Model: fmt.Sprintf("m%d", i%2), ResolvedModel: fmt.Sprintf("r%d", i%3), Workspace: fmt.Sprintf("/w/%d", i%4), SessionID: fmt.Sprintf("s%d", i%7), CallKind: []string{"agent", "title"}[i%2], Title: fmt.Sprintf("title-%d", i), CreatedAt: times[i%len(times)]}
		// Half arrive with their metering already present.
		if i%2 == 0 {
			seedUsage(t, db, id, 1, `{"input":2,"input_cache_hit":3,"input_cache_miss":4,"output":5,"reasoning":1}`, int64(i), int64(2*i))
		}
		seedDimensionRow(t, db, rec)
		if i%3 == 0 {
			seedUsage(t, db, id, 2, `{"input":6,"output":1}`, 7, 8)
		}
	}
	filters := []domain.RequestLogFilter{
		{}, {From: h, To: h.Add(3*time.Hour - time.Second)},
		{From: h.Add(time.Second), To: h.Add(time.Hour)},
		{From: h, To: h}, {From: h.Add(24 * time.Hour)},
		{From: h.Add(time.Second), To: h},
		{AccountID: 1}, {APIKeyID: 2}, {Client: "codex"}, {Model: "m1"},
		{ResolvedModel: "r1"}, {Workspace: "/w/1"}, {SessionID: "s1"}, {CallKind: "title"},
		{AccountID: 1, Client: "codex", CallKind: "title", From: h, To: h.Add(2 * time.Hour)},
	}
	for _, mode := range []string{"raw", "rolled", "dirty", "disabled"} {
		if mode == "rolled" {
			if err := db.RefreshDimensionRollups(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "dirty" {
			seedUsage(t, db, "r-1", 3, `{"input_cache_hit":9}`, 11, 12)
		}
		if mode == "disabled" {
			db.SetDimensionRollupsEnabled(false)
		}
		for _, f := range filters {
			for _, group := range RequestLogDimensionNames {
				for _, sortKey := range RequestLogDimensionSorts {
					want := dimensionOracle(t, db, f, group, sortKey, 2, 1)
					got, err := db.RequestLogDimensionsPage(ctx, f, group, sortKey, 2, 1)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s %s %s %+v:\ngot %+v\nwant %+v", mode, group, sortKey, f, got, want)
					}
				}
			}
		}
	}
}

func TestDimensionRollupInvalidationAndPublication(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "a", Client: "before", CreatedAt: h})
	seedUsage(t, db, "a", 1, `{"input":2}`, 3, 4)
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	version := func() (int64, int64) {
		t.Helper()
		var v, p int64
		if err := db.read.QueryRow(`SELECT version,published_version FROM request_dimension_hours WHERE hour=?`, h.Unix()).Scan(&v, &p); err != nil {
			t.Fatal(err)
		}
		return v, p
	}
	v, p := version()
	if v != p {
		t.Fatal("not published")
	}
	// Content-only updates, skeleton retries, duplicate usage, and rolled-back writes
	// must not invalidate an otherwise complete hour.
	rec, _ := db.GetRequestLog(ctx, "a")
	rec.ResponseText = "new body"
	if err := db.PutRequestLog(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SettleAttempt(ctx, &domain.UsageRecord{RequestID: "a", AttemptNo: 1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE usage_records SET cost_micros=99 WHERE request_id='a'`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if next, _ := version(); next != v {
		t.Fatal("no-op/rollback invalidated")
	}
	// A staging generation is never eligible, and CAS refuses to publish its old version.
	res, err := db.write.Exec(`INSERT INTO request_dimension_generations(hour,source_version) VALUES(?,?)`, h.Unix(), v)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := res.LastInsertId()
	if _, err = db.write.Exec(`UPDATE request_logs SET client='after' WHERE request_id='a'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.publishDimensionGeneration(ctx, h.Unix(), v, generation); err != nil || ok {
		t.Fatalf("published stale version: %v %v", ok, err)
	}
	got, err := db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "", 20, 0)
	if err != nil || got.Total != 1 || got.Rows[0].Key != "after" {
		t.Fatalf("dirty: %+v %v", got, err)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	// Usage changes and deletes both invalidate; metadata movement invalidates both hours.
	for _, statement := range []string{
		`UPDATE usage_records SET dimensions_json='{"input":9}',charge_micros=12 WHERE request_id='a'`,
		`DELETE FROM usage_records WHERE request_id='a'`,
		fmt.Sprintf(`UPDATE request_logs SET created_at=%d,session_id='moved',title='new title' WHERE request_id='a'`, h.Add(-time.Hour).Unix()),
		`DELETE FROM request_logs WHERE request_id='a'`,
	} {
		if _, err := db.write.Exec(statement); err != nil {
			t.Fatal(err)
		}
		for _, group := range []string{"client", "session"} {
			want := dimensionOracle(t, db, domain.RequestLogFilter{}, group, "", 20, 0)
			got, err := db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, group, "", 20, 0)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %+v != %+v (%v)", statement, got, want, err)
			}
		}
		if err := db.RefreshDimensionRollups(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.read.QueryRow(`SELECT COUNT(*) FROM request_dimension_rollups`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("obsolete contributions remain: %d %v", n, err)
	}
}

func TestDimensionRollupSnapshotAndIndexPlan(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "a", Client: "old", CreatedAt: h})
	seedUsage(t, db, "a", 1, `{"input":1}`, 2, 3)
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	f := domain.RequestLogFilter{From: h, To: h.Add(time.Hour - time.Second)}
	tx, err := db.read.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	selected, err := db.selectDimensionSources(ctx, tx, f, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.ranges) != 0 || len(selected.generations) != 1 {
		t.Fatalf("complete hour scans raw: %+v", selected)
	}
	source, args := dimensionSourceSQL(selected, f, false, false)
	if strings.Contains(source, "usage_records") || strings.Contains(source, "request_logs") {
		t.Fatal(source)
	}
	// Changes after snapshot selection cannot change its page or count.
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "b", Client: "new", CreatedAt: h})
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+source+`)`, args...).Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot count: %d %v", count, err)
	}
	countSource, countArgs := dimensionSourceSQL(selected, f, true, false)
	if strings.Contains(countSource, "usage_records") {
		t.Fatal("count joins usage")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+countSource+`)`, countArgs...).Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot group count: %d %v", count, err)
	}
	tx.Rollback()
	rawSource, rawArgs := dimensionSourceSQL(dimensionSelection{ranges: [][2]int64{{h.Unix(), h.Unix() + 3599}}}, f, false, false)
	rows, err := db.read.QueryContext(ctx, `EXPLAIN QUERY PLAN `+rawSource, rawArgs...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "SEARCH r USING INDEX idx_request_logs_time") || strings.Contains(plan.String(), "SCAN r ") {
		t.Fatalf("raw ranges must seek: %s", plan.String())
	}
	if !strings.Contains(plan.String(), "SEARCH u USING INDEX") {
		t.Fatalf("usage must point-lookup: %s", plan.String())
	}
}

func TestDimensionRollupMigrationBackfillAndRestart(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", buildDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	legacy := &DB{write: old, read: old}
	if _, err = old.Exec(createMigrationsTable); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version < 13 {
			if err = legacy.applyMigration(ctx, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	tx, err := old.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1101; i++ {
		if _, err = tx.Exec(`INSERT INTO request_logs(request_id,created_at,client) VALUES(?,?,?)`, fmt.Sprintf("legacy-%d", i), h.Unix()+int64(i%2)*3600, "legacy"); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var hours int
	if err = db.read.QueryRow(`SELECT COUNT(*) FROM request_dimension_hours`).Scan(&hours); err != nil || hours != 0 {
		t.Fatalf("migration scanned history: %d %v", hours, err)
	}
	done, err := db.discoverDimensionHours(ctx)
	if err != nil || done {
		t.Fatalf("first discovery: %v %v", done, err)
	}
	status, err := db.DimensionRollupStats(ctx)
	if err != nil || status.BackfillCursor != 500 || status.BackfillComplete {
		t.Fatalf("progress: %+v %v", status, err)
	}
	// A pending hour is complete only after rebuilding all its rows, including those
	// beyond the discovery cursor. Unknown hours still use the raw complement.
	if err = db.rebuildDimensionHour(ctx, h.Unix()); err != nil {
		t.Fatal(err)
	}
	p, err := db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "", 20, 0)
	if err != nil || p.Rows[0].Requests != 1101 {
		t.Fatalf("partial discovery: %+v %v", p, err)
	}
	// Create an interrupted staging generation containing bogus totals. It cannot leak.
	res, err := db.write.Exec(`INSERT INTO request_dimension_generations(hour,source_version) VALUES(?,999)`, h.Unix())
	if err != nil {
		t.Fatal(err)
	}
	g, _ := res.LastInsertId()
	if err = db.stageDimensionRows(ctx, [][]any{{g, h.Unix(), 0, 0, "bogus", "", "", "", "", "", "", 999, 0, h.Unix(), h.Unix(), 0, 0, 0, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = db.RefreshDimensionRollups(cancelled); err == nil {
		t.Fatal("cancelled refresh succeeded")
	}
	status, _ = db.DimensionRollupStats(ctx)
	if status.LastError == "" {
		t.Fatal("missing refresh error")
	}
	if err = db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = db.DimensionRollupStats(ctx)
	if err != nil || !status.BackfillComplete || status.BackfillCursor != 1101 || status.PendingHours != 0 || status.LastError != "" || status.LastSuccess.IsZero() {
		t.Fatalf("resume: %+v %v", status, err)
	}
	p, err = db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "", 20, 0)
	if err != nil || p.Total != 1 || p.Rows[0].Requests != 1101 {
		t.Fatalf("resume totals: %+v %v", p, err)
	}
	var n int
	if err = db.read.QueryRow(`SELECT COUNT(*) FROM request_dimension_rollups WHERE generation=?`, g).Scan(&n); err != nil || n != 0 {
		t.Fatalf("staging not cleaned: %d %v", n, err)
	}
	stop := db.StartDimensionRollups(ctx, nil)
	stop()
	stop()
}

func TestDimensionRollupWithoutWALUsesRaw(t *testing.T) {
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "rollback.db")
	cfg.WAL = false
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "raw", Client: "c", CreatedAt: time.Now().Add(-2 * time.Hour)})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := db.DimensionRollupStats(ctx)
	if err != nil || st.Enabled || st.LastError == "" {
		t.Fatalf("WAL fallback: %+v %v", st, err)
	}
	p, err := db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "", 20, 0)
	if err != nil || p.Total != 1 || p.Rows[0].Requests != 1 {
		t.Fatalf("raw: %+v %v", p, err)
	}
}

func TestDimensionRollupIdleCleanupAvoidsLiveContributionScan(t *testing.T) {
	db := testDB(t)
	rows, err := db.read.Query(`EXPLAIN QUERY PLAN ` + cleanDimensionRowsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.String(), "SCAN r ") || !strings.Contains(plan.String(), "SEARCH r USING COVERING INDEX idx_dimension_rollups_generation") {
		t.Fatalf("cleanup must seek obsolete generations: %s", plan.String())
	}
}

func TestDimensionRollupProductionBatchPaths(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	batch := []pendingRecord{{log: &domain.RequestLogRecord{RequestID: "batch", Client: "c", CreatedAt: h}}, {log: &domain.RequestLogRecord{RequestID: "usage-first", Client: "c", CreatedAt: h}}}
	first := &SettlementInput{Usage: &domain.UsageRecord{RequestID: "usage-first", AttemptNo: 1, DimensionsJSON: `{"input":3}`}}
	if _, err := db.SettleBatch(ctx, []*SettlementInput{first}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutAuditBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	// INSERT OR IGNORE used by settlement must still execute the trigger's UPSERT
	// when a newly inserted attempt invalidates an existing published hour.
	input := &SettlementInput{Usage: &domain.UsageRecord{RequestID: "batch", AttemptNo: 1, DimensionsJSON: `{"input":4}`}}
	if n, err := db.SettleBatch(ctx, []*SettlementInput{input}); err != nil || n != 1 {
		t.Fatalf("settle: %d %v", n, err)
	}
	p, err := db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "client", "", 20, 0)
	if err != nil || p.Rows[0].Requests != 2 || p.Rows[0].Metered != 2 || p.Rows[0].InputTokens != 7 {
		t.Fatalf("settlement invalidation: %+v %v", p, err)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := db.SettleBatch(ctx, []*SettlementInput{input, first}); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
	if err := db.PutAuditBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	st, err := db.DimensionRollupStats(ctx)
	if err != nil || st.PendingHours != 0 {
		t.Fatalf("replay dirtied hour: %+v %v", st, err)
	}
}

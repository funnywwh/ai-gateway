package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// The windowed reads behind the management console's paged lists
// (docs/design/m24-console-pagination.md §2.2): every Page query has a Count that
// shares its WHERE clause, and the legacy non-paged method stays a first-page wrapper.

func seedPagedAccount(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.UpsertAccount(context.Background(), &domain.Account{
		Name: "paged", BillingMode: domain.BillingPostpaid, Status: "active",
	})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

func TestListAuditPageWindowsAndCounts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		if err := db.InsertAudit(ctx, &AuditEntry{
			Actor: "tester", Action: fmt.Sprintf("action-%02d", i), TargetType: "test",
			TargetID: "1", Result: "ok", CreatedAt: now,
		}); err != nil {
			t.Fatalf("insert audit %d: %v", i, err)
		}
	}

	first, err := db.ListAuditPage(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Action != "action-04" {
		t.Fatalf("first page = %+v, want the two newest rows", first)
	}
	second, err := db.ListAuditPage(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].Action != "action-02" {
		t.Fatalf("second page = %+v, want action-02 first", second)
	}
	last, err := db.ListAuditPage(ctx, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 {
		t.Fatalf("last page holds %d rows, want 1", len(last))
	}
	beyond, err := db.ListAuditPage(ctx, 2, 50)
	if err != nil || len(beyond) != 0 {
		t.Fatalf("offset past the end must be an empty page, got %d rows (err=%v)", len(beyond), err)
	}
	total, err := db.CountAudit(ctx)
	if err != nil || total != 5 {
		t.Fatalf("CountAudit = %d (err=%v), want 5", total, err)
	}

	// The non-paged method keeps its old meaning: the newest `limit` rows.
	legacy, err := db.ListAudit(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 3 || legacy[0].Action != "action-04" {
		t.Fatalf("ListAudit = %+v, want it to stay the first page", legacy)
	}
}

func TestListRequestLogsPageRespectsFiltersAndCounts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID := seedPagedAccount(t, db)
	now := time.Now().UTC()

	// Three rows for our account (newest first) plus one for another account and one
	// outside the time window: neither may leak into the page or the total. created_at
	// rises with the insertion order, the way it does in production, where the row is
	// stamped when it is written (v1.recordRequestLog) — a fixture with the two orders
	// reversed would pin an order the list never sees.
	for i := 0; i < 3; i++ {
		if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{
			RequestID: fmt.Sprintf("req-%02d", i), AccountID: accountID, APIKeyID: 1,
			Endpoint: "/v1/responses", Status: "ok", CreatedAt: now.Add(-time.Duration(2-i) * time.Minute),
		}); err != nil {
			t.Fatalf("put request log: %v", err)
		}
	}
	if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{
		RequestID: "req-other", AccountID: accountID + 99, APIKeyID: 1,
		Endpoint: "/v1/responses", Status: "ok", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{
		RequestID: "req-old", AccountID: accountID, APIKeyID: 1,
		Endpoint: "/v1/responses", Status: "ok", CreatedAt: now.AddDate(0, 0, -30),
	}); err != nil {
		t.Fatal(err)
	}
	from, to := now.Add(-time.Hour), now.Add(time.Minute)

	page, err := db.ListRequestLogsPage(ctx, domain.RequestLogFilter{AccountID: accountID, From: from, To: to}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first is the window's newest created_at, with id as the tiebreaker (see
	// historyPageOrder), so the last row written — which carries the latest stamp — is
	// the first one a console shows.
	if len(page) != 2 || page[0].RequestID != "req-02" {
		t.Fatalf("page = %+v, want req-02 first", page)
	}
	second, err := db.ListRequestLogsPage(ctx, domain.RequestLogFilter{AccountID: accountID, From: from, To: to}, 2, 2)
	if err != nil || len(second) != 1 || second[0].RequestID != "req-00" {
		t.Fatalf("second page = %+v (err=%v), want the remaining req-00", second, err)
	}
	total, err := db.CountRequestLogs(ctx, domain.RequestLogFilter{AccountID: accountID, From: from, To: to})
	if err != nil || total != 3 {
		t.Fatalf("CountRequestLogs = %d (err=%v), want 3 (filtered by account and window)", total, err)
	}
	// accountID 0 means "every account", which is the console's cross-tenant view.
	total, err = db.CountRequestLogs(ctx, domain.RequestLogFilter{AccountID: 0, From: from, To: to})
	if err != nil || total != 4 {
		t.Fatalf("CountRequestLogs(all) = %d (err=%v), want 4", total, err)
	}
}

func TestListLedgerPageExcludesKindsInSQL(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID := seedPagedAccount(t, db)
	now := time.Now().UTC()

	entries := []*domain.LedgerEntry{{
		AccountID: accountID, Kind: "topup", AmountMicros: 5_000_000,
		IdemKey: "seed:topup", CreatedAt: now,
	}}
	for i := 0; i < 4; i++ {
		entries = append(entries, &domain.LedgerEntry{
			AccountID: accountID, Kind: "charge", AmountMicros: -100,
			IdemKey: fmt.Sprintf("seed:charge:%02d", i), CreatedAt: now,
		})
	}
	if _, err := db.AppendLedger(ctx, entries); err != nil {
		t.Fatalf("append ledger: %v", err)
	}

	all := LedgerWindow{AccountID: accountID, Limit: 3, Offset: 0}
	page, err := db.ListLedgerPage(ctx, all)
	if err != nil || len(page) != 3 {
		t.Fatalf("ledger page = %d rows (err=%v), want 3", len(page), err)
	}
	if total, err := db.CountLedger(ctx, all); err != nil || total != 5 {
		t.Fatalf("CountLedger = %d (err=%v), want 5", total, err)
	}

	// The credits view: charges are dropped by the WHERE clause, so the page size and
	// the total describe the same row set.
	creditsOnly := LedgerWindow{AccountID: accountID, ExcludeKinds: []string{"charge"}, Limit: 3, Offset: 0}
	page, err = db.ListLedgerPage(ctx, creditsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Kind != "topup" {
		t.Fatalf("credits page = %+v, want only the topup", page)
	}
	if total, err := db.CountLedger(ctx, creditsOnly); err != nil || total != 1 {
		t.Fatalf("CountLedger(credits) = %d (err=%v), want 1", total, err)
	}

	// The legacy read stays the first page of every kind.
	legacy, err := db.ListLedger(ctx, accountID, time.Time{}, time.Time{}, 2)
	if err != nil || len(legacy) != 2 {
		t.Fatalf("ListLedger = %d rows (err=%v), want 2", len(legacy), err)
	}
}

func TestListInvoicesCodesAndReconciliationsPage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID := seedPagedAccount(t, db)
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		start := now.AddDate(0, -i-1, 0)
		if _, _, err := db.PutInvoice(ctx, &domain.Invoice{
			AccountID: accountID, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
			Status: "draft", Currency: "USD", CreatedAt: start,
		}, nil, false); err != nil {
			t.Fatalf("put invoice: %v", err)
		}
	}
	invoices, err := db.ListInvoicesPage(ctx, accountID, 2, 0)
	if err != nil || len(invoices) != 2 {
		t.Fatalf("invoice page = %d rows (err=%v), want 2", len(invoices), err)
	}
	if total, err := db.CountInvoices(ctx, accountID); err != nil || total != 3 {
		t.Fatalf("CountInvoices = %d (err=%v), want 3", total, err)
	}
	if total, err := db.CountInvoices(ctx, 0); err != nil || total != 3 {
		t.Fatalf("CountInvoices(all) = %d (err=%v), want 3", total, err)
	}

	codes := []*domain.RedemptionCode{}
	for i := 0; i < 3; i++ {
		codes = append(codes, &domain.RedemptionCode{
			CodeHash: fmt.Sprintf("hash-%02d", i), AmountMicros: 100,
			BatchID: "batch-a", CreatedBy: "tester", CreatedAt: now,
		})
	}
	if err := db.InsertRedemptionCodes(ctx, codes); err != nil {
		t.Fatalf("insert codes: %v", err)
	}
	page, err := db.ListRedemptionCodesPage(ctx, "batch-a", 2, 2)
	if err != nil || len(page) != 1 {
		t.Fatalf("code page = %d rows (err=%v), want 1", len(page), err)
	}
	if total, err := db.CountRedemptionCodes(ctx, "batch-a"); err != nil || total != 3 {
		t.Fatalf("CountRedemptionCodes = %d (err=%v), want 3", total, err)
	}
	if total, err := db.CountRedemptionCodes(ctx, "batch-b"); err != nil || total != 0 {
		t.Fatalf("CountRedemptionCodes(other batch) = %d (err=%v), want 0", total, err)
	}

	for i := 0; i < 3; i++ {
		if _, err := db.InsertReconciliation(ctx, &domain.Reconciliation{
			PeriodStart: now.Add(-time.Hour), PeriodEnd: now, Kind: "manual", CreatedAt: now,
		}); err != nil {
			t.Fatalf("insert reconciliation: %v", err)
		}
	}
	reconciliations, err := db.ListReconciliationsPage(ctx, 2, 2)
	if err != nil || len(reconciliations) != 1 {
		t.Fatalf("reconciliation page = %d rows (err=%v), want 1", len(reconciliations), err)
	}
	if total, err := db.CountReconciliations(ctx); err != nil || total != 3 {
		t.Fatalf("CountReconciliations = %d (err=%v), want 3", total, err)
	}
}

// TestHistoryListsAreOrderedByAnIndexNotASorter pins *why* historyPageOrder is
// "created_at DESC, id DESC" rather than "id DESC". These lists filter on a created_at
// window, so with "ORDER BY id DESC" SQLite answers them with "USE TEMP B-TREE FOR ORDER
// BY" — it buffers every row of the window before LIMIT applies. On request_logs that
// buffer carries the recorded request bodies, so the console's 50-row page sorted ~700 MB
// to show 50 rows (1.23s measured against a real 1.8k-row database; 0.00s once the sorter
// is gone — docs/design/m24-console-pagination.md §8.10). Ordering by the filtered column
// first lets the created_at index satisfy the order and no sorter is built.
//
// The plan is asserted instead of the timing because a plan choice is deterministic while
// a duration is not, and because M13 keeps absolute numbers out of CI. The queries come
// from the store's own builders so this test cannot drift away from what production runs.
func TestHistoryListsAreOrderedByAnIndexNotASorter(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from, to := now.AddDate(0, 0, -7), now

	requestWhere, requestArgs := requestLogFilter("", domain.RequestLogFilter{From: from, To: to})
	ledgerWhere, ledgerArgs := ledgerFilter(LedgerWindow{AccountID: 1, From: from, To: to})

	// The identity filters (M27) each have an index, and that index carries the id column
	// so the same ORDER BY is satisfied by a reverse scan of an equality-constrained
	// prefix. Without the id column SQLite sorts the window again — which is the whole
	// reason the three indexes are shaped the way they are.
	clientWhere, clientArgs := requestLogFilter("", domain.RequestLogFilter{From: from, To: to, Client: "dsh"})
	modelWhere, modelArgs := requestLogFilter("", domain.RequestLogFilter{From: from, To: to, Model: "deepseek-flash"})
	sessionWhere, sessionArgs := requestLogFilter("", domain.RequestLogFilter{From: from, To: to, SessionID: "session-abc"})
	// The credential filters (M30) are indexed the same way: the console offers both, and
	// an unindexed one would read every row of the window (bodies included) to show a page.
	accountWhere, accountArgs := requestLogFilter("", domain.RequestLogFilter{From: from, To: to, AccountID: 1})
	keyWhere, keyArgs := requestLogFilter("", domain.RequestLogFilter{From: from, To: to, APIKeyID: 3})

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{"request_logs", requestLogListSQL(requestWhere), append(requestArgs, 50, 0)},
		{"request_logs_by_client", requestLogListSQL(clientWhere), append(clientArgs, 50, 0)},
		{"request_logs_by_model", requestLogListSQL(modelWhere), append(modelArgs, 50, 0)},
		{"request_logs_by_session", requestLogListSQL(sessionWhere), append(sessionArgs, 50, 0)},
		{"request_logs_by_account", requestLogListSQL(accountWhere), append(accountArgs, 50, 0)},
		{"request_logs_by_api_key", requestLogListSQL(keyWhere), append(keyArgs, 50, 0)},
		{"ledger_entries", ledgerListSQL(ledgerWhere), append(ledgerArgs, 100, 0)},
		{"usage_records", ttftSamplesSQL(), []any{int64(1), unix(from), unix(to), 2000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.read.QueryContext(ctx, "EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatalf("explain %s: %v", tc.name, err)
			}
			defer rows.Close()
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatalf("scan plan row: %v", err)
				}
				if strings.Contains(detail, "TEMP B-TREE") {
					t.Fatalf("%s: SQLite sorts the whole window (%q); historyPageOrder must be an "+
						"order an index already provides — see pagination.go", tc.name, detail)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate plan rows: %v", err)
			}
		})
	}
}

// The index a filter relies on has to exist in the schema: the plan assertions above can
// only prove "no sorter", which a full-then-filter scan satisfies too. Migration 0009 adds
// the two credential indexes; this pins them by name so a later migration cannot quietly
// drop one and leave the console reading whole windows (M30).
func TestCredentialDimensionIndexesExist(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, name := range []string{"idx_request_logs_account", "idx_request_logs_key"} {
		var sql string
		err := db.read.QueryRowContext(ctx,
			"SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&sql)
		if err != nil {
			t.Fatalf("index %s is missing (err %v); migration 0009 adds it", name, err)
		}
		// The shape is the contract: the id column is what keeps the paged ORDER BY
		// index-satisfied on an equality-constrained prefix (docs/design/m27 §2.2).
		if !strings.Contains(sql, "created_at") || !strings.Contains(strings.ToLower(sql), "id)") {
			t.Fatalf("index %s has the wrong shape: %s", name, sql)
		}
	}
}

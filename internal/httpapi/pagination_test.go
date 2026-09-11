package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// The pagination contract of the management surface: every list endpoint takes
// limit + offset and answers with {data, count, total, limit, offset, has_more}.
// See docs/design/m24-console-pagination.md §3.

func mustPage(t *testing.T, f *adminFixture, cookie, path string) map[string]any {
	t.Helper()
	resp := f.call(t, http.MethodGet, path, "", cookie)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
	}
	return decodeJSONBody(t, resp)
}

func pageRows(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, _ := payload["data"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("list payload holds a non-object row: %+v", item)
		}
		rows = append(rows, row)
	}
	return rows
}

func wantWindow(t *testing.T, payload map[string]any, count, total, limit, offset int, hasMore bool) {
	t.Helper()
	if got := len(pageRows(t, payload)); got != count {
		t.Fatalf("page holds %d rows, want %d (%+v)", got, count, payload)
	}
	for field, want := range map[string]float64{
		"count": float64(count), "total": float64(total),
		"limit": float64(limit), "offset": float64(offset),
	} {
		if got, _ := payload[field].(float64); got != want {
			t.Errorf("%s = %v, want %v", field, payload[field], want)
		}
	}
	if got, _ := payload["has_more"].(bool); got != hasMore {
		t.Errorf("has_more = %v, want %v", payload["has_more"], hasMore)
	}
}

// seedAccounts creates n accounts and returns their ids in creation order.
func seedAccounts(t *testing.T, f *adminFixture, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := f.db.UpsertAccount(ctx, &domain.Account{
			Name: fmt.Sprintf("acct-%02d", i), BillingMode: domain.BillingPostpaid, Status: "active",
		}); err != nil {
			t.Fatalf("seed account %d: %v", i, err)
		}
	}
}

// seedLedger appends n charges plus one topup to one account and returns the account id.
func seedLedger(t *testing.T, f *adminFixture, n int) int64 {
	t.Helper()
	ctx := context.Background()
	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatalf("load acme: %v", err)
	}
	entries := []*domain.LedgerEntry{{
		AccountID: account.ID, Kind: "topup", AmountMicros: 10_000_000,
		BalanceAfterMicros: 10_000_000, RefID: "seed-topup", IdemKey: "seed:topup",
		Actor: "test", CreatedAt: time.Now().UTC(),
	}}
	for i := 0; i < n; i++ {
		entries = append(entries, &domain.LedgerEntry{
			AccountID: account.ID, Kind: "charge", AmountMicros: -int64(1000 + i),
			BalanceAfterMicros: 10_000_000 - int64(1000+i),
			RefID:              fmt.Sprintf("req-%02d", i), IdemKey: fmt.Sprintf("seed:charge:%02d", i),
			Actor: "test", CreatedAt: time.Now().UTC(),
		})
	}
	if _, err := f.db.AppendLedger(ctx, entries); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	return account.ID
}

func TestAdminListPagesThroughConfigurationTables(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	seedAccounts(t, f, 4) // plus the fixture's own "acme" = 5 accounts

	first := mustPage(t, f, cookie, "/admin/api/v1/accounts?limit=2&offset=0")
	wantWindow(t, first, 2, 5, 2, 0, true)
	middle := mustPage(t, f, cookie, "/admin/api/v1/accounts?limit=2&offset=2")
	wantWindow(t, middle, 2, 5, 2, 2, true)
	last := mustPage(t, f, cookie, "/admin/api/v1/accounts?limit=2&offset=4")
	wantWindow(t, last, 1, 5, 2, 4, false)

	// Windows must not overlap: this is what makes "next page" mean something.
	seen := map[any]bool{}
	for _, page := range []map[string]any{first, middle, last} {
		for _, row := range pageRows(t, page) {
			if seen[row["id"]] {
				t.Fatalf("account %v appeared on two pages", row["id"])
			}
			seen[row["id"]] = true
		}
	}

	// An offset past the end is an empty page, not an error: the console can land there
	// after a deletion and must be able to walk back.
	beyond := mustPage(t, f, cookie, "/admin/api/v1/accounts?limit=2&offset=99")
	wantWindow(t, beyond, 0, 5, 2, 99, false)
}

func TestAdminListPageSizeIsCapped(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	// Configuration lists cap at 1000 and default to 200; the console asks for the cap
	// when it fills a selector, and an oversized request is clamped rather than refused.
	payload := mustPage(t, f, cookie, "/admin/api/v1/accounts?limit=99999")
	if got, _ := payload["limit"].(float64); got != 1000 {
		t.Fatalf("limit = %v, want the 1000 cap", payload["limit"])
	}
	payload = mustPage(t, f, cookie, "/admin/api/v1/accounts")
	if got, _ := payload["limit"].(float64); got != 200 {
		t.Fatalf("limit = %v, want the 200 default", payload["limit"])
	}
}

func TestAdminListRejectsMalformedOffset(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	// Answering a request for page 5 with page 1 would be worse than an error, so a
	// malformed offset is refused instead of being read as 0.
	for _, bad := range []string{"abc", "-1", "1.5"} {
		resp := f.call(t, http.MethodGet, "/admin/api/v1/accounts?offset="+bad, "", cookie)
		status, code := decodeError(t, resp)
		if status != http.StatusBadRequest || code != "invalid_request" {
			t.Fatalf("offset=%s status=%d code=%s, want 400 invalid_request", bad, status, code)
		}
	}
}

func TestAdminListPagesThroughRequestLogs(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := f.db.PutRequestLog(ctx, &domain.RequestLogRecord{
			RequestID: fmt.Sprintf("req_page_%02d", i), AccountID: account.ID, APIKeyID: 1,
			Endpoint: "/v1/responses", Status: "ok", RequestJSON: `{"input":"hi"}`,
			RequestBytes: 14, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed request log: %v", err)
		}
	}

	first := mustPage(t, f, cookie, "/admin/api/v1/requests?limit=2&offset=0")
	wantWindow(t, first, 2, 5, 2, 0, true)
	second := mustPage(t, f, cookie, "/admin/api/v1/requests?limit=2&offset=2")
	wantWindow(t, second, 2, 5, 2, 2, true)

	// Newest first: the second page must hold older rows than the first.
	newest, _ := pageRows(t, first)[0]["request_id"].(string)
	if newest != "req_page_04" {
		t.Fatalf("first page starts at %q, want the newest row", newest)
	}
	older, _ := pageRows(t, second)[0]["request_id"].(string)
	if older != "req_page_02" {
		t.Fatalf("second page starts at %q, want req_page_02", older)
	}
}

func TestAdminListPagesThroughLedgerAndCredits(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	accountID := seedLedger(t, f, 5) // 5 charges + 1 topup

	// The ledger lists every kind: 6 rows, newest (the last charge) first.
	ledger := mustPage(t, f, cookie, fmt.Sprintf("/admin/api/v1/accounts/%d/ledger?limit=4&offset=0", accountID))
	wantWindow(t, ledger, 4, 6, 4, 0, true)
	ledgerTail := mustPage(t, f, cookie, fmt.Sprintf("/admin/api/v1/accounts/%d/ledger?limit=4&offset=4", accountID))
	wantWindow(t, ledgerTail, 2, 6, 4, 4, false)

	// The credits view excludes charges in SQL: 1 row, and the total must count only
	// what the caller can actually page through (the old implementation filtered after
	// the LIMIT, so a page of 100 could render 12 rows and the total was meaningless).
	credits := mustPage(t, f, cookie, fmt.Sprintf("/admin/api/v1/accounts/%d/credits?limit=4&offset=0", accountID))
	wantWindow(t, credits, 1, 1, 4, 0, false)
	if kind, _ := pageRows(t, credits)[0]["kind"].(string); kind != "topup" {
		t.Fatalf("credits page holds a %q row, want only non-charge kinds", kind)
	}
}

func TestAdminListPagesThroughInvoicesCodesAndReconciliations(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		start := now.AddDate(0, -i-1, 0).Truncate(time.Hour)
		if _, _, err := f.db.PutInvoice(ctx, &domain.Invoice{
			AccountID: account.ID, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
			Status: "draft", Currency: "USD", CreatedAt: start,
		}, nil, false); err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
	}
	codes := []*domain.RedemptionCode{}
	for i := 0; i < 3; i++ {
		codes = append(codes, &domain.RedemptionCode{
			CodeHash: fmt.Sprintf("hash-%02d", i), AmountMicros: 1000,
			BatchID: "batch-a", CreatedBy: "test", CreatedAt: now,
		})
	}
	if err := f.db.InsertRedemptionCodes(ctx, codes); err != nil {
		t.Fatalf("seed codes: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.db.InsertReconciliation(ctx, &domain.Reconciliation{
			PeriodStart: now.Add(-time.Hour), PeriodEnd: now,
			Kind: "manual", CreatedAt: now,
		}); err != nil {
			t.Fatalf("seed reconciliation: %v", err)
		}
	}

	invoices := mustPage(t, f, cookie, "/admin/api/v1/invoices?limit=2&offset=0")
	wantWindow(t, invoices, 2, 3, 2, 0, true)
	invoicesTail := mustPage(t, f, cookie, "/admin/api/v1/invoices?limit=2&offset=2")
	wantWindow(t, invoicesTail, 1, 3, 2, 2, false)

	// The account-scoped path shares the window and the total with the cross-account one.
	scoped := mustPage(t, f, cookie, fmt.Sprintf("/admin/api/v1/accounts/%d/invoices?limit=2&offset=0", account.ID))
	wantWindow(t, scoped, 2, 3, 2, 0, true)
	// The query form of the same filter is what the console's account picker sends; it is
	// documented in the route table, so it has to actually filter.
	byQuery := mustPage(t, f, cookie, fmt.Sprintf("/admin/api/v1/invoices?account_id=%d&limit=2&offset=0", account.ID))
	wantWindow(t, byQuery, 2, 3, 2, 0, true)
	otherAccount := mustPage(t, f, cookie, "/admin/api/v1/invoices?account_id=9999&limit=2&offset=0")
	wantWindow(t, otherAccount, 0, 0, 2, 0, false)
	badAccount := f.call(t, http.MethodGet, "/admin/api/v1/invoices?account_id=abc", "", cookie)
	if status, _ := decodeError(t, badAccount); status != http.StatusBadRequest {
		t.Fatalf("account_id=abc status = %d, want 400", status)
	}

	batched := mustPage(t, f, cookie, "/admin/api/v1/redemption-codes?batch_id=batch-a&limit=2&offset=0")
	wantWindow(t, batched, 2, 3, 2, 0, true)
	otherBatch := mustPage(t, f, cookie, "/admin/api/v1/redemption-codes?batch_id=batch-b&limit=2&offset=0")
	wantWindow(t, otherBatch, 0, 0, 2, 0, false)

	reconciliations := mustPage(t, f, cookie, "/admin/api/v1/billing/reconciliations?limit=2&offset=2")
	wantWindow(t, reconciliations, 1, 3, 2, 2, false)
}

func TestAdminListPagesThroughAuditAndBackups(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// The login above already wrote audit rows; add a known number of our own and page
	// to the end of the trail, whatever the fixture did before.
	for i := 0; i < 5; i++ {
		if err := f.db.InsertAudit(ctx, &AuditEntry{
			Actor: "test", Action: fmt.Sprintf("seed-%02d", i), TargetType: "test",
			TargetID: "1", Result: "ok", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	audit := mustPage(t, f, cookie, "/admin/api/v1/audit-logs?limit=3&offset=0")
	if total, _ := audit["total"].(float64); total < 6 {
		t.Fatalf("audit total = %v, want at least the 6 rows written so far", audit["total"])
	}
	wantWindow(t, audit, 3, int(audit["total"].(float64)), 3, 0, true)

	for i := 0; i < 3; i++ {
		started := time.Now().UTC().Add(-time.Duration(i) * time.Minute)
		if _, err := f.db.InsertBackupJob(ctx, &domain.BackupJob{
			Path: fmt.Sprintf("/tmp/backup-%02d.db", i), SizeBytes: int64(1000 * (i + 1)),
			Status: "ok", QuickCheck: "ok", TriggeredBy: "test", StartedAt: started,
		}); err != nil {
			t.Fatalf("seed backup job: %v", err)
		}
	}
	backups := mustPage(t, f, cookie, "/admin/api/v1/backups?limit=2&offset=0")
	wantWindow(t, backups, 2, 3, 2, 0, true)
	// total_bytes describes the whole backup directory, not the page: the console shows
	// it as "占用", and a page-scoped sum would make the number move when paging.
	if got, _ := backups["total_bytes"].(float64); got != 6000 {
		t.Fatalf("total_bytes = %v, want the sum over all 3 jobs (6000)", backups["total_bytes"])
	}
	backupsTail := mustPage(t, f, cookie, "/admin/api/v1/backups?limit=2&offset=2")
	wantWindow(t, backupsTail, 1, 3, 2, 2, false)
	if got, _ := backupsTail["total_bytes"].(float64); got != 6000 {
		t.Fatalf("total_bytes on the last page = %v, want 6000", backupsTail["total_bytes"])
	}
}

func TestAdminListPagesThroughKeysAndHooks(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	account, err := f.db.GetAccountByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.db.UpsertAPIKey(ctx, &domain.APIKey{
			AccountID: account.ID, Name: fmt.Sprintf("key-%02d", i),
			KeyPrefix: fmt.Sprintf("sk-page%02d", i), KeyHash: fmt.Sprintf("hash-%02d", i),
			Status: "active",
		}); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}
	keys := mustPage(t, f, cookie, "/admin/api/v1/keys?limit=2&offset=0")
	wantWindow(t, keys, 2, 3, 2, 0, true)
	keysTail := mustPage(t, f, cookie, "/admin/api/v1/keys?limit=2&offset=2")
	wantWindow(t, keysTail, 1, 3, 2, 2, false)
}

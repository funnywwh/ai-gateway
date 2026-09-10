package billing

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

func TestPeriodForNaturalMonthAndShiftedStart(t *testing.T) {
	at := time.Date(2026, 3, 20, 15, 0, 0, 0, time.UTC)
	start, end, err := PeriodFor(at, PeriodConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if start.Format("2006-01-02") != "2026-03-01" || end.Format("2006-01-02") != "2026-04-01" {
		t.Fatalf("natural month = %s..%s", start, end)
	}

	shiftedStart, shiftedEnd, err := PeriodFor(at, PeriodConfig{StartDay: 15})
	if err != nil {
		t.Fatal(err)
	}
	if shiftedStart.Format("2006-01-02") != "2026-03-15" || shiftedEnd.Format("2006-01-02") != "2026-04-15" {
		t.Fatalf("shifted period = %s..%s", shiftedStart, shiftedEnd)
	}

	// Before the boundary the period belongs to the previous month.
	early := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	earlyStart, earlyEnd, err := PeriodFor(early, PeriodConfig{StartDay: 15})
	if err != nil {
		t.Fatal(err)
	}
	if earlyStart.Format("2006-01-02") != "2026-02-15" || earlyEnd.Format("2006-01-02") != "2026-03-15" {
		t.Fatalf("early period = %s..%s", earlyStart, earlyEnd)
	}

	if _, _, err := PeriodFor(at, PeriodConfig{Timezone: "Mars/Olympus"}); err == nil {
		t.Fatal("an unknown timezone must be rejected")
	}
}

func TestPeriodForRespectsTimezone(t *testing.T) {
	// 2026-04-01T00:30 in +08:00 is still 2026-03-31T16:30 UTC, so the period is March.
	at := time.Date(2026, 4, 1, 0, 30, 0, 0, time.FixedZone("CST", 8*3600))
	start, _, err := PeriodFor(at, PeriodConfig{Timezone: "+08:00"})
	if err != nil {
		t.Fatal(err)
	}
	// Local midnight on 2026-04-01 in +08:00 is 2026-03-31T16:00Z.
	wantStart := time.Date(2026, 3, 31, 16, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Fatalf("start = %s, want %s", start, wantStart)
	}
	utcStart, _, err := PeriodFor(at.UTC(), PeriodConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if utcStart.Format("2006-01-02") != "2026-03-01" {
		t.Fatalf("utc start = %s, want 2026-03-01", utcStart)
	}
}

func TestInvoiceLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 5_000_000)
	service := NewService(ctx, db, ServiceConfig{Writer: Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond}}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	for index := 0; index < 3; index++ {
		settlement := NewCharge(usageFor(accountID, fmt.Sprintf("inv_%d", index), 100_000), nil, true)
		if _, err := db.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().UTC().Add(-time.Hour)
	end := time.Now().UTC().Add(time.Hour)
	invoice, created, err := service.BuildInvoice(ctx, accountID, start, end, "model", false, "USD", "test period")
	if err != nil {
		t.Fatalf("build invoice: %v", err)
	}
	if !created {
		t.Fatal("the first build must create the invoice")
	}
	if invoice.TotalChargeMicros != 300_000 || invoice.TotalCostMicros != 150_000 {
		t.Fatalf("invoice totals = %d/%d, want 300000/150000", invoice.TotalChargeMicros, invoice.TotalCostMicros)
	}
	if len(invoice.Lines) != 1 || invoice.Lines[0].Requests != 3 {
		t.Fatalf("invoice lines = %+v", invoice.Lines)
	}

	// Rebuilding without force returns the same draft instead of a second invoice.
	again, created, err := service.BuildInvoice(ctx, accountID, start, end, "model", false, "USD", "")
	if err != nil {
		t.Fatal(err)
	}
	if created || again.ID != invoice.ID {
		t.Fatalf("rebuild created a duplicate invoice: %+v", again)
	}

	issued, err := service.InvoiceAction(ctx, invoice.ID, "issue", "tester")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.Status != "issued" || issued.IssuedAt == nil {
		t.Fatalf("issue did not move the invoice: %+v", issued)
	}
	// An issued invoice is frozen.
	if _, _, err := service.BuildInvoice(ctx, accountID, start, end, "model", true, "USD", ""); err == nil {
		t.Fatal("an issued invoice must not be recomputed")
	}
	if _, err := service.InvoiceAction(ctx, invoice.ID, "void", "tester"); err != nil {
		t.Fatalf("void: %v", err)
	}

	csv := InvoiceCSV(invoice)
	if !strings.Contains(csv, "total,,,,") {
		t.Fatalf("csv export looks wrong:\n%s", csv)
	}
}

func TestCreditsAreIdempotentAndResumeAccounts(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 0)
	service := NewService(ctx, db, ServiceConfig{Writer: Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond}}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	entry, applied, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "topup", AmountMicros: 2_000_000, RefID: "pay_1",
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !applied || entry.BalanceAfterMicros != 2_000_000 {
		t.Fatalf("grant result = %+v applied=%v", entry, applied)
	}

	// The same external reference must not credit twice.
	second, applied, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "topup", AmountMicros: 2_000_000, RefID: "pay_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatalf("a replayed credit was applied twice: %+v", second)
	}
	balance, _ := db.GetBalance(ctx, accountID)
	if balance != 2_000_000 {
		t.Fatalf("balance = %d, want 2000000", balance)
	}

	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "topup", AmountMicros: -5, RefID: "pay_2",
	}); err == nil {
		t.Fatal("a negative topup must be rejected")
	}
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "topup", AmountMicros: 5, RefID: "",
	}); err == nil {
		t.Fatal("a credit without a reference must be rejected")
	}

	// Auto-resume: a suspended account with credit comes back when policy allows.
	if err := db.SetAccountStatus(ctx, accountID, "suspended"); err != nil {
		t.Fatal(err)
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	account.AutoResume = true
	if _, err := db.UpsertAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "topup", AmountMicros: 500_000, RefID: "pay_3",
	}); err != nil {
		t.Fatal(err)
	}
	resumed, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "active" {
		t.Fatalf("account status = %s, want active after an auto-resume credit", resumed.Status)
	}
}

func TestRedemptionCodeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 0)
	otherAccount, err := db.UpsertAccount(ctx, &domain.Account{Name: "other", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(ctx, db, ServiceConfig{Writer: Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond}}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	codes, err := service.GenerateCodes(ctx, CodeBatchRequest{Count: 2, AmountMicros: 100_000, Actor: "tester"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(codes) != 2 || !strings.HasPrefix(codes[0], "gwrc") {
		t.Fatalf("codes = %v", codes)
	}
	stored, err := db.GetRedemptionCodeByHash(ctx, secretHash(codes[0]))
	if err != nil {
		t.Fatal(err)
	}
	if stored.CodeHash == codes[0] {
		t.Fatal("the plaintext code must never be stored")
	}

	if _, _, err := service.RedeemCode(ctx, codes[0], accountID, "tester"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	balance, _ := db.GetBalance(ctx, accountID)
	if balance != 100_000 {
		t.Fatalf("balance = %d, want 100000", balance)
	}

	if _, _, err := service.RedeemCode(ctx, codes[0], otherAccount, "tester"); err == nil {
		t.Fatal("a code must only be redeemable once")
	}
	otherBalance, _ := db.GetBalance(ctx, otherAccount)
	if otherBalance != 0 {
		t.Fatalf("the second redemption must not credit anything: %d", otherBalance)
	}
	if _, _, err := service.RedeemCode(ctx, "gwrc-nope", accountID, "tester"); err == nil {
		t.Fatal("an unknown code must be rejected")
	}
}

func TestReconcileDetectsAndReportsDifferences(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 5_000_000)
	service := NewService(ctx, db, ServiceConfig{Writer: Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond}}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	for index := 0; index < 2; index++ {
		settlement := NewCharge(usageFor(accountID, fmt.Sprintf("rec_%d", index), 100_000), nil, true)
		if _, err := db.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters); err != nil {
			t.Fatal(err)
		}
	}
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().Add(time.Hour)
	clean, err := service.Reconcile(ctx, ReconcileRequest{From: from, To: to})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if clean.DiffMicros != 0 {
		t.Fatalf("a freshly settled ledger must reconcile: %+v", clean)
	}
	if clean.UsageChargeMicros != 200_000 {
		t.Fatalf("usage total = %d, want 200000", clean.UsageChargeMicros)
	}

	// Remove one charge entry behind the ledger's back: the next run must see it.
	if _, err := db.RawExec(ctx, "DELETE FROM ledger_entries WHERE kind = 'charge' AND id = (SELECT MIN(id) FROM ledger_entries WHERE kind = 'charge')"); err != nil {
		t.Fatal(err)
	}
	broken, err := service.Reconcile(ctx, ReconcileRequest{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if broken.DiffMicros != 100_000 {
		t.Fatalf("diff = %d, want 100000", broken.DiffMicros)
	}
	if !strings.Contains(broken.DetailsJSON, "differences") || !strings.Contains(broken.DetailsJSON, "invariants") {
		t.Fatalf("details must carry the differences and the invariant report: %s", broken.DetailsJSON)
	}
	runs, err := service.Reconciliations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("reconciliation runs = %d, want 2", len(runs))
	}
}

func secretHash(code string) string { return secret.Hash(code) }

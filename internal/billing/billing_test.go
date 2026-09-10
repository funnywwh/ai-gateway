package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/store"
)

func newStore(t *testing.T) *store.DB {
	t.Helper()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "billing.db")
	db, err := store.Open(context.Background(), cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedAccount(t *testing.T, db *store.DB, balanceMicros int64) int64 {
	t.Helper()
	id, err := db.UpsertAccount(context.Background(), &domain.Account{
		Name: "acme", BillingMode: domain.BillingPrepaid, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if balanceMicros != 0 {
		if _, err := db.AppendLedger(context.Background(), []*domain.LedgerEntry{{
			AccountID: id, Kind: "topup", AmountMicros: balanceMicros, IdemKey: "topup:seed",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func usageFor(accountID int64, requestID string, chargeMicros int64) *domain.UsageRecord {
	return &domain.UsageRecord{
		RequestID: requestID, AttemptNo: 1, AccountID: accountID, APIKeyID: 7,
		Model: "m", DimensionsJSON: `{"output":1000}`, CostMicros: chargeMicros / 2,
		ChargeMicros: chargeMicros, Status: "completed", CreatedAt: time.Now().UTC(),
	}
}

func TestSettlementIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 10_000_000)

	settlement := NewCharge(usageFor(accountID, "req_1", 250_000), nil, true)
	applied, err := db.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !applied {
		t.Fatal("first settlement should insert the usage row")
	}
	balance, err := db.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 9_750_000 {
		t.Fatalf("balance = %d, want 9750000", balance)
	}

	// Replaying the same attempt must not charge twice.
	replay := NewCharge(usageFor(accountID, "req_1", 250_000), nil, true)
	applied, err = db.SettleAttempt(ctx, replay.Usage, replay.Entries, replay.Counters)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if applied {
		t.Fatal("replay must not insert a second usage row")
	}
	balance, _ = db.GetBalance(ctx, accountID)
	if balance != 9_750_000 {
		t.Fatalf("balance after replay = %d, want 9750000", balance)
	}
	entries, err := db.ListLedger(ctx, accountID, time.Time{}, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger entries = %d, want 2 (topup + one charge)", len(entries))
	}
}

func TestSettleBatchIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 10_000_000)

	inputs := []*store.SettlementInput{}
	for index := 0; index < 5; index++ {
		settlement := NewCharge(usageFor(accountID, fmt.Sprintf("req_%d", index), 100_000), nil, true)
		inputs = append(inputs, &store.SettlementInput{
			Usage: settlement.Usage, Entries: settlement.Entries, Counters: settlement.Counters,
		})
	}
	applied, err := db.SettleBatch(ctx, inputs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if applied != 5 {
		t.Fatalf("applied = %d, want 5", applied)
	}
	balance, _ := db.GetBalance(ctx, accountID)
	if balance != 9_500_000 {
		t.Fatalf("balance = %d, want 9500000", balance)
	}

	// A failing row must roll the whole batch back, leaving no partial charge.
	bad := &store.SettlementInput{Usage: usageFor(accountID, "req_bad", 100_000),
		Entries: []*domain.LedgerEntry{{AccountID: 999999, Kind: "charge", AmountMicros: -1, IdemKey: "charge:bad"}}}
	second := NewCharge(usageFor(accountID, "req_second", 100_000), nil, true)
	if _, err := db.SettleBatch(ctx, []*store.SettlementInput{bad, {Usage: second.Usage, Entries: second.Entries}}); err == nil {
		t.Fatal("batch with an unknown account must fail")
	}
	balance, _ = db.GetBalance(ctx, accountID)
	if balance != 9_500_000 {
		t.Fatalf("balance after failed batch = %d, want 9500000 (rollback)", balance)
	}
}

func TestWriterBatchesAndFallsBackToDisk(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 10_000_000)

	fallbackPath := filepath.Join(t.TempDir(), "billing-fallback.jsonl")
	writer := NewWriter(Config{BatchSize: 4, FlushInterval: 5 * time.Millisecond, FallbackFile: fallbackPath}, db, nil)
	for index := 0; index < 10; index++ {
		writer.Submit(NewCharge(usageFor(accountID, fmt.Sprintf("w_%d", index), 50_000), nil, true))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if writer.Stats().Settled >= 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stats := writer.Stats()
	if stats.Settled != 10 {
		t.Fatalf("settled = %d, want 10 (stats %+v)", stats.Settled, stats)
	}
	if stats.Batches == 0 {
		t.Fatalf("expected batched writes, stats %+v", stats)
	}
	balance, _ := db.GetBalance(ctx, accountID)
	if balance != 9_500_000 {
		t.Fatalf("balance = %d, want 9500000", balance)
	}
	if remaining := writer.Close(2 * time.Second); remaining != 0 {
		t.Fatalf("close reported %d unsettled settlements", remaining)
	}

	// A settlement the database could not accept (a transient failure here) must land
	// in the fallback file and be applied by a later replay.
	deferred := NewCharge(usageFor(accountID, "req_deferred", 70_000), nil, true)
	writer.fallback(deferred, errors.New("simulated transient database failure"))
	if _, err := os.Stat(fallbackPath); err != nil {
		t.Fatalf("fallback file was not written: %v", err)
	}
	if balance, _ := db.GetBalance(ctx, accountID); balance != 9_500_000 {
		t.Fatalf("a deferred settlement must not be charged yet: %d", balance)
	}
	result, err := writer.ReplayFile(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if result.Replayed != 1 || result.Failed != 0 {
		t.Fatalf("replay result = %+v, want 1 replayed", result)
	}
	balance, _ = db.GetBalance(ctx, accountID)
	if balance != 9_430_000 {
		t.Fatalf("balance after replay = %d, want 9430000", balance)
	}
	if _, err := os.Stat(fallbackPath); !os.IsNotExist(err) {
		t.Fatalf("a fully replayed fallback file should be removed: %v", err)
	}
}

func TestReservationsAndAdmission(t *testing.T) {
	table := NewReservationTable(30 * time.Second)
	now := time.Now()
	table.Reserve("req_1", 1, 500, now)
	table.Reserve("req_2", 1, 300, now)
	if got := table.InFlight(1); got != 800 {
		t.Fatalf("in flight = %d, want 800", got)
	}
	// Re-reserving the same id replaces the amount instead of adding a second hold.
	table.Reserve("req_1", 1, 600, now)
	if got := table.InFlight(1); got != 900 {
		t.Fatalf("in flight after replace = %d, want 900", got)
	}
	table.Release("req_1", 1)
	if got := table.InFlight(1); got != 300 {
		t.Fatalf("in flight after release = %d, want 300", got)
	}
	if removed := table.GC(now.Add(time.Minute)); removed != 1 {
		t.Fatalf("gc removed %d, want 1", removed)
	}

	prepaid := &domain.Account{ID: 1, BillingMode: domain.BillingPrepaid, BalanceMicros: 1000, Status: "active"}
	allowed := DecideAdmission(AdmissionInput{Account: prepaid, Reserve: 1000, InFlight: 0})
	if !allowed.Allowed {
		t.Fatalf("exact balance must be admitted: %+v", allowed)
	}
	denied := DecideAdmission(AdmissionInput{Account: prepaid, Reserve: 1001, InFlight: 0})
	if denied.Allowed || denied.Reason != "insufficient_quota" {
		t.Fatalf("over-reservation must be denied: %+v", denied)
	}
	withInflight := DecideAdmission(AdmissionInput{Account: prepaid, Reserve: 500, InFlight: 600})
	if withInflight.Allowed {
		t.Fatalf("in-flight holds must reduce what is available: %+v", withInflight)
	}
	suspended := &domain.Account{ID: 1, Status: "suspended", BalanceMicros: 1000000}
	if decision := DecideAdmission(AdmissionInput{Account: suspended, Reserve: 1}); decision.Allowed {
		t.Fatalf("a suspended account must be denied: %+v", decision)
	}
	postpaid := &domain.Account{ID: 2, BillingMode: domain.BillingPostpaid, BalanceMicros: 0, CreditLimitMicros: 5000, Status: "active"}
	if decision := DecideAdmission(AdmissionInput{Account: postpaid, Reserve: 4000}); !decision.Allowed {
		t.Fatalf("postpaid within its credit limit must be admitted: %+v", decision)
	}
	if decision := DecideAdmission(AdmissionInput{Account: postpaid, Reserve: 6000}); decision.Allowed {
		t.Fatalf("postpaid beyond its credit limit must be denied: %+v", decision)
	}
}

func TestEstimateReserveUsesTheMostExpensiveRule(t *testing.T) {
	cost, err := pricing.ParseRuleSet(`{"rules": [` +
		`{"id": "offpeak", "order": 10, "when": {"time_windows": [{"start": "16:30", "end": "00:30"}]}, "rates": {"input": 100000, "output": 400000}},` +
		`{"id": "peak", "order": 100, "when": {}, "rates": {"input": 300000, "output": 1200000}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	reserve := EstimateReserve(EstimateInput{Cost: cost, MaxOutputTokens: 1000, EstInputTokens: 2000})
	// Worst case: 1000 * 1.2 + 2000 * 0.3 = 1800 micros, rounded up per unit.
	if reserve != 1000*1200000/pricing.RateScale+2000*300000/pricing.RateScale {
		t.Fatalf("reserve = %d, want the peak-tier amount", reserve)
	}
	withMarkup := EstimateReserve(EstimateInput{Cost: cost, MaxOutputTokens: 1000, EstInputTokens: 0, DefaultMarkupBP: 20000})
	if withMarkup != 1000*1200000/pricing.RateScale*2 {
		t.Fatalf("reserve with markup = %d, want double the cost", withMarkup)
	}
}

func TestInvariantsAndRebuild(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 1_000_000)

	for index := 0; index < 3; index++ {
		settlement := NewCharge(usageFor(accountID, fmt.Sprintf("r_%d", index), 100_000), nil, true)
		if _, err := db.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters); err != nil {
			t.Fatal(err)
		}
	}

	report, err := CheckInvariants(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("freshly settled ledger must satisfy every invariant: %+v", report)
	}

	// Damaging the materialised balance must be detected, then repaired by a rebuild.
	if _, err := db.RawExec(ctx, "UPDATE accounts SET balance_micros = balance_micros - 5 WHERE id = ?", accountID); err != nil {
		t.Fatal(err)
	}
	broken, err := CheckInvariants(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if broken.OK || len(broken.BalanceSumMismatch) != 1 {
		t.Fatalf("expected a balance mismatch: %+v", broken)
	}

	plan, err := RebuildAccount(ctx, db, accountID, true, time.Now())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !plan.Applied || plan.Attempts != 3 {
		t.Fatalf("rebuild plan = %+v", plan)
	}
	repaired, err := CheckInvariants(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !repaired.OK {
		t.Fatalf("rebuild must restore the invariants: %+v", repaired)
	}
}

package billing

import (
	"context"
	"testing"
	"time"
)

func TestGiftCreditExpiresOnlyTheUnusedPart(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 1_000_000) // paid top-up
	service := NewService(ctx, db, ServiceConfig{Writer: Config{BatchSize: 2}, ReservationTTL: 0}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "credit_grant", AmountMicros: 400_000,
		RefID: "welcome", ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "credit_grant", AmountMicros: 900_000,
		RefID: "later", ExpiresAt: &future,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := service.ExpireGiftCredit(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if result.Expired != 1 || result.Micros != 400_000 {
		t.Fatalf("expiry result = %+v, want the matured 400000 expired once", result)
	}
	balance, _ := db.GetBalance(ctx, accountID)
	// 1,000,000 top-up + 900,000 future grant remain; only the matured grant went away.
	if balance != 1_900_000 {
		t.Fatalf("balance = %d, want 1900000: the future grant and the top-up stay", balance)
	}

	// Re-running must not expire anything a second time.
	again, err := service.ExpireGiftCredit(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if again.Expired != 0 {
		t.Fatalf("second pass expired %d entries, want 0", again.Expired)
	}
	balance, _ = db.GetBalance(ctx, accountID)
	if balance != 1_900_000 {
		t.Fatalf("balance after the second pass = %d, want 1900000", balance)
	}
}

func TestGiftCreditNeverTakesTheBalanceNegative(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 0)
	service := NewService(ctx, db, ServiceConfig{Writer: Config{BatchSize: 2}, ReservationTTL: 0}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	past := time.Now().UTC().Add(-time.Hour)
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "credit_grant", AmountMicros: 500_000,
		RefID: "gift-a", ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "credit_grant", AmountMicros: 500_000,
		RefID: "gift-b", ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	// Spend most of the credit before it matures.
	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "adjustment", AmountMicros: -700_000, RefID: "spend",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := service.ExpireGiftCredit(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if result.Micros != 300_000 {
		t.Fatalf("expired %d micros, want 300000 (what is left on the account)", result.Micros)
	}
	balance, _ := db.GetBalance(ctx, accountID)
	if balance != 0 {
		t.Fatalf("balance = %d, want 0: expiry must never go below zero", balance)
	}
	report, err := service.Invariants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("invariants must hold after an expiry: %+v", report)
	}
}

package billing

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

// RebuildStore is the persistence the ledger rebuild needs.
type RebuildStore interface {
	ListUsageAsc(ctx context.Context, accountID int64) ([]*domain.UsageRecord, error)
	RebuildCharges(ctx context.Context, accountID int64, charges []*domain.LedgerEntry) (store.RebuildOutcome, error)
}

// RebuildPlan is the dry-run preview.
type RebuildPlan struct {
	AccountID       int64    `json:"account_id"`
	Attempts        int      `json:"attempts"`
	ChargeMicros    int64    `json:"charge_micros"`
	LedgerChargeSum int64    `json:"ledger_charge_sum"`
	DiffMicros      int64    `json:"diff_micros"`
	Mismatched      int      `json:"mismatched"`
	Samples         []string `json:"samples,omitempty"`
	Applied         bool     `json:"applied"`
}

// RebuildAccount replays every attempt of one account into charge ledger entries.
// The usage rows are the source of truth: charges are recomputed from the pricing
// snapshot, never from the current rule tables, so the result matches the original
// numbers even after prices changed.
func RebuildAccount(ctx context.Context, store RebuildStore, accountID int64, apply bool, now time.Time) (*RebuildPlan, error) {
	usage, err := store.ListUsageAsc(ctx, accountID)
	if err != nil {
		return nil, err
	}
	plan := &RebuildPlan{AccountID: accountID, Attempts: len(usage), Samples: []string{}}

	charges := make([]*domain.LedgerEntry, 0, len(usage))
	for _, record := range usage {
		plan.ChargeMicros += record.ChargeMicros
		if record.ChargeMicros <= 0 {
			continue
		}
		keyID := record.APIKeyID
		charges = append(charges, &domain.LedgerEntry{
			AccountID:    record.AccountID,
			APIKeyID:     &keyID,
			Kind:         "charge",
			AmountMicros: -record.ChargeMicros,
			RefType:      "usage",
			RefID:        fmt.Sprintf("%s:%d", record.RequestID, record.AttemptNo),
			IdemKey:      ChargeIdemKey(record.RequestID, record.AttemptNo),
			Note:         record.Model,
			CreatedAt:    record.CreatedAt,
		})
	}
	sort.SliceStable(charges, func(i, j int) bool {
		if charges[i].CreatedAt.Equal(charges[j].CreatedAt) {
			return charges[i].IdemKey < charges[j].IdemKey
		}
		return charges[i].CreatedAt.Before(charges[j].CreatedAt)
	})
	plan.LedgerChargeSum = -plan.ChargeMicros

	if !apply {
		plan.DiffMicros = plan.LedgerChargeSum - plan.ChargeMicros
		return plan, nil
	}
	outcome, err := store.RebuildCharges(ctx, accountID, charges)
	if err != nil {
		return nil, err
	}
	plan.Applied = true
	plan.DiffMicros = outcome.AfterMicros - outcome.BeforeMicros
	plan.Samples = append(plan.Samples, fmt.Sprintf("balance %d -> %d over %d entries",
		outcome.BeforeMicros, outcome.AfterMicros, outcome.Entries))
	return plan, nil
}

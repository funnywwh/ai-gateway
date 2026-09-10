// Package billing turns metered attempts into ledger movements without ever losing
// money: settlements are batched by a single writer, fall back to disk when the
// database refuses, and are replayed idempotently afterwards.
package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/store"
)

// Settlement is one priced attempt plus the money movement it implies.
type Settlement struct {
	Usage    *domain.UsageRecord
	Entries  []*domain.LedgerEntry
	Counters []store.UsageCounter
	// CreatedAt is when the attempt finished; it drives the fallback file ordering.
	CreatedAt time.Time
}

// ChargeIdemKey is the idempotency key of the charge for one attempt.
func ChargeIdemKey(requestID string, attemptNo int) string {
	return fmt.Sprintf("charge:%s:%d", requestID, attemptNo)
}

// NewCharge builds the settlement for one priced attempt: the usage row plus, when
// the account is charged, a single negative ledger entry.
func NewCharge(usage *domain.UsageRecord, result *pricing.Result, charge bool) *Settlement {
	settlement := &Settlement{Usage: usage, CreatedAt: usage.CreatedAt}
	if settlement.CreatedAt.IsZero() {
		settlement.CreatedAt = time.Now().UTC()
		settlement.Usage.CreatedAt = settlement.CreatedAt
	}
	if result != nil {
		settlement.Usage.CostMicros = result.CostMicros
		settlement.Usage.ChargeMicros = result.ChargeMicros
		if raw, err := json.Marshal(result.Snapshot); err == nil {
			settlement.Usage.PricingSnapshot = string(raw)
		}
	}
	if charge && usage.ChargeMicros > 0 && usage.AccountID != 0 {
		keyID := usage.APIKeyID
		settlement.Entries = append(settlement.Entries, &domain.LedgerEntry{
			AccountID:    usage.AccountID,
			APIKeyID:     &keyID,
			Kind:         "charge",
			AmountMicros: -usage.ChargeMicros,
			RefType:      "usage",
			RefID:        fmt.Sprintf("%s:%d", usage.RequestID, usage.AttemptNo),
			IdemKey:      ChargeIdemKey(usage.RequestID, usage.AttemptNo),
			Note:         usage.Model,
			CreatedAt:    settlement.CreatedAt,
		})
	}
	return settlement
}

// CounterPeriod is the "YYYY-MM" bucket a settlement rolls up into.
func CounterPeriod(at time.Time) string {
	return at.UTC().Format("2006-01")
}

// Apply writes one settlement directly, bypassing the batch writer. It returns
// whether the usage row was newly inserted.
func (w *Writer) Apply(ctx context.Context, settlement *Settlement) (bool, error) {
	w.mu.Lock()
	applier := w.applier
	w.mu.Unlock()
	if applier == nil {
		return false, fmt.Errorf("billing: writer is closed")
	}
	return applier.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters)
}

// Batching is the persistence port the writer needs.
type Batching interface {
	SettleAttempt(ctx context.Context, usage *domain.UsageRecord, entries []*domain.LedgerEntry, counters []store.UsageCounter) (bool, error)
}

// FailureRef identifies a settlement for the billing_failures table.
func (s *Settlement) FailureRef() (string, int, int64) {
	if s == nil || s.Usage == nil {
		return "", 0, 0
	}
	return s.Usage.RequestID, s.Usage.AttemptNo, s.Usage.AccountID
}

// WithCounter rolls the settlement up into the usage counters of a period.
func (s *Settlement) WithCounter(period string, tokens int64) *Settlement {
	if s == nil || s.Usage == nil || period == "" {
		return s
	}
	s.Counters = append(s.Counters, store.UsageCounter{
		AccountID:    s.Usage.AccountID,
		APIKeyID:     s.Usage.APIKeyID,
		Period:       period,
		Requests:     1,
		Tokens:       tokens,
		CostMicros:   s.Usage.CostMicros,
		ChargeMicros: s.Usage.ChargeMicros,
	})
	return s
}

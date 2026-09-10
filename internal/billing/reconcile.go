package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// ReconcileRequest describes one reconciliation run.
type ReconcileRequest struct {
	From      time.Time
	To        time.Time
	AccountID int64
	Kind      string

	CreatedAt time.Time
}

// Reconcile compares what the usage rows say was charged with what the ledger
// actually recorded, and stores the comparison. It never writes to the ledger: a
// correction has to be an explicit adjustment so the paper trail survives.
func (s *Service) Reconcile(ctx context.Context, req ReconcileRequest) (*domain.Reconciliation, error) {
	from := req.From
	to := req.To
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -1)
	}
	if !to.After(from) {
		return nil, domain.ErrInvalidRequest("the reconciliation window is empty")
	}
	kind := req.Kind
	if kind == "" {
		kind = "daily"
	}

	ledgerByAccount, usageByAccount, err := s.store.LedgerChargeTotals(ctx, from, to)
	if err != nil {
		return nil, err
	}
	accounts := map[int64]bool{}
	for accountID := range ledgerByAccount {
		accounts[accountID] = true
	}
	for accountID := range usageByAccount {
		accounts[accountID] = true
	}
	if req.AccountID != 0 {
		accounts = map[int64]bool{req.AccountID: true}
	}
	ordered := make([]int64, 0, len(accounts))
	for accountID := range accounts {
		ordered = append(ordered, accountID)
	}
	sortInt64(ordered)

	record := &domain.Reconciliation{
		PeriodStart: from.UTC(), PeriodEnd: to.UTC(), Kind: kind,
		CreatedAt: req.CreatedAt,
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	type accountDiff struct {
		AccountID    int64    `json:"account_id"`
		UsageMicros  int64    `json:"usage_micros"`
		LedgerMicros int64    `json:"ledger_micros"`
		DiffMicros   int64    `json:"diff_micros"`
		Samples      []string `json:"samples,omitempty"`
	}
	diffs := []accountDiff{}
	for _, accountID := range ordered {
		usageTotal := usageByAccount[accountID]
		ledgerTotal := ledgerByAccount[accountID]
		record.UsageChargeMicros += usageTotal
		record.LedgerChargeMicros += ledgerTotal
		diff := usageTotal - ledgerTotal
		if diff == 0 {
			continue
		}
		record.DiffMicros += diff
		entry := accountDiff{
			AccountID: accountID, UsageMicros: usageTotal,
			LedgerMicros: ledgerTotal, DiffMicros: diff,
		}
		if samples, err := s.store.UsageSamplesForWindow(ctx, accountID, from, to, 100); err == nil {
			entry.Samples = samples
		}
		diffs = append(diffs, entry)
	}
	if ratio, err := s.store.EstimatedUsageRatio(ctx, from, to); err == nil {
		record.EstimatedRatioBP = ratio
	}
	record.MissingUsageCount = int64(len(diffs))

	details := map[string]any{
		"kind": kind, "window_start": from.UTC(), "window_end": to.UTC(),
		"accounts": len(ordered), "differences": diffs,
	}
	if report, err := s.Invariants(ctx); err == nil {
		details["invariants"] = report
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("billing: marshal reconciliation details: %w", err)
	}
	record.DetailsJSON = string(raw)
	if _, err := s.store.InsertReconciliation(ctx, record); err != nil {
		return nil, err
	}

	if record.DiffMicros != 0 {
		s.log.Error("reconciliation found a charge mismatch",
			"diff_micros", record.DiffMicros, "usage_micros", record.UsageChargeMicros,
			"ledger_micros", record.LedgerChargeMicros, "accounts", len(diffs))
		if s.onMismatch != nil {
			s.onMismatch(record)
		}
	}
	return record, nil
}

// ReplayFailures re-applies settlements that could not be written earlier. The amounts
// come from the stored payload (which carries the pricing snapshot), never from the
// current rule tables, so a replay reproduces the original charge.
func (s *Service) ReplayFailures(ctx context.Context, limit int) (ReplayResult, error) {
	result, err := s.writer.ReplayFile(ctx)
	if err != nil {
		return result, err
	}
	failures, err := s.store.ListBillingFailures(ctx, limit)
	if err != nil {
		return result, err
	}
	for _, failure := range failures {
		result.Scanned++
		var settlement Settlement
		if err := json.Unmarshal([]byte(failure.PayloadJSON), &settlement); err != nil || settlement.Usage == nil {
			result.Failed++
			continue
		}
		applied, err := s.store.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters)
		if err != nil {
			result.Failed++
			if err := s.store.MarkBillingFailureRetry(ctx, failure.ID, err.Error()); err != nil {
				s.log.Warn("marking a billing failure retry failed", "err", err, "id", failure.ID)
			}
			continue
		}
		result.Replayed++
		if applied {
			s.writer.settled.Add(1)
		} else {
			s.writer.replayed.Add(1)
		}
		if err := s.store.ResolveBillingFailure(ctx, failure.ID); err != nil {
			s.log.Warn("resolving a billing failure failed", "err", err, "id", failure.ID)
		}
	}
	return result, nil
}

func sortInt64(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

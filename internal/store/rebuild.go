package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// ListUsageAsc returns every usage row of an account oldest first (the ledger rebuild
// needs chronological order, unlike the reporting queries).
func (db *DB) ListUsageAsc(ctx context.Context, accountID int64) ([]*domain.UsageRecord, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+usageCols+" FROM usage_records WHERE account_id = ? ORDER BY created_at, id", accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list usage asc: %w", err)
	}
	defer rows.Close()
	out := []*domain.UsageRecord{}
	for rows.Next() {
		var (
			r         domain.UsageRecord
			createdAt int64
		)
		if err := rows.Scan(&r.ID, &r.RequestID, &r.AttemptNo, &r.AccountID, &r.APIKeyID,
			&r.Model, &r.ResolvedModel, &r.ProviderID, &r.DimensionsJSON, &r.CostMicros,
			&r.ChargeMicros, &r.OvershootCost, &r.PricingSnapshot, &r.LatencyMS, &r.TTFTMS,
			&r.Status, &r.ErrorCode, &r.DegradedFeatures, &r.UsageSource, &r.TerminatedReason,
			&createdAt); err != nil {
			return nil, fmt.Errorf("store: scan usage asc: %w", err)
		}
		r.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate usage asc: %w", err)
	}
	return out, nil
}

// RebuildCharges replaces every charge ledger entry of one account with the supplied
// list, recomputes balance_after for the whole account and updates the materialised
// balance - all inside one transaction. Non-charge entries (topups, grants,
// adjustments, refunds) are preserved verbatim.
func (db *DB) RebuildCharges(ctx context.Context, accountID int64, charges []*domain.LedgerEntry) (RebuildOutcome, error) {
	outcome := RebuildOutcome{}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return outcome, fmt.Errorf("store: begin rebuild tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var balance int64
	if err := tx.QueryRowContext(ctx,
		"SELECT balance_micros FROM accounts WHERE id = ?", accountID).Scan(&balance); err != nil {
		return outcome, fmt.Errorf("store: read balance for rebuild: %w", err)
	}
	outcome.BeforeMicros = balance

	type row struct {
		entry   domain.LedgerEntry
		keyID   *int64
		created int64
	}
	keep := []row{}
	rows, err := tx.QueryContext(ctx, "SELECT "+ledgerCols+" FROM ledger_entries WHERE account_id = ? AND kind <> 'charge'", accountID)
	if err != nil {
		return outcome, fmt.Errorf("store: read non-charge ledger rows: %w", err)
	}
	for rows.Next() {
		var (
			entry     domain.LedgerEntry
			apiKeyID  sql.NullInt64
			createdAt int64
		)
		if err := rows.Scan(&entry.ID, &entry.AccountID, &apiKeyID, &entry.Kind, &entry.AmountMicros,
			&entry.BalanceAfterMicros, &entry.RefType, &entry.RefID, &entry.IdemKey,
			&entry.RebuildSeq, &entry.Note, &entry.Actor, &createdAt); err != nil {
			rows.Close()
			return outcome, fmt.Errorf("store: scan non-charge ledger row: %w", err)
		}
		keep = append(keep, row{entry: entry, keyID: nullInt64Ptr(apiKeyID), created: createdAt})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return outcome, fmt.Errorf("store: iterate non-charge ledger rows: %w", err)
	}
	rows.Close()

	merged := make([]row, 0, len(keep)+len(charges))
	merged = append(merged, keep...)
	for _, charge := range charges {
		merged = append(merged, row{entry: *charge, keyID: charge.APIKeyID, created: unix(charge.CreatedAt)})
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].created == merged[j].created {
			return merged[i].entry.IdemKey < merged[j].entry.IdemKey
		}
		return merged[i].created < merged[j].created
	})

	if _, err := tx.ExecContext(ctx, "DELETE FROM ledger_entries WHERE account_id = ?", accountID); err != nil {
		return outcome, fmt.Errorf("store: clear ledger for rebuild: %w", err)
	}

	running := int64(0)
	for _, item := range merged {
		running += item.entry.AmountMicros
		entry := item.entry
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ledger_entries(account_id, api_key_id, kind, amount_micros, balance_after_micros,
  ref_type, ref_id, idem_key, rebuild_seq, note, actor, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			accountID, item.keyID, entry.Kind, entry.AmountMicros, running, entry.RefType,
			entry.RefID, entry.IdemKey, entry.RebuildSeq+1, entry.Note, entry.Actor, item.created); err != nil {
			return outcome, fmt.Errorf("store: reinsert ledger entry %s: %w", entry.IdemKey, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE accounts SET balance_micros = ?, updated_at = ? WHERE id = ?",
		running, unix(time.Now()), accountID); err != nil {
		return outcome, fmt.Errorf("store: update balance after rebuild: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return outcome, fmt.Errorf("store: commit rebuild: %w", err)
	}
	outcome.AfterMicros = running
	outcome.Entries = len(merged)
	return outcome, nil
}

// RebuildOutcome mirrors billing.RebuildOutcome; it is declared here so the store does
// not have to import the billing package.
type RebuildOutcome struct {
	BeforeMicros int64
	AfterMicros  int64
	Entries      int
}

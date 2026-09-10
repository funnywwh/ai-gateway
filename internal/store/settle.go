package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// UsageCounter is the domain rollup type; the alias keeps call sites readable while the
// type itself lives in the dependency-free layer (see docs/design/m15-decoupling.md).
type UsageCounter = domain.UsageCounter

// SettlementInput is one attempt's worth of persistence work.
type SettlementInput struct {
	Usage    *domain.UsageRecord
	Entries  []*domain.LedgerEntry
	Counters []UsageCounter
}

// SettleAttempt writes one metered attempt and its money movement in a single
// transaction. It reports whether the usage row was newly inserted, so callers can
// tell "settled" from "already settled by an earlier attempt".
func (db *DB) SettleAttempt(ctx context.Context, usage *domain.UsageRecord, entries []*domain.LedgerEntry, counters []UsageCounter) (bool, error) {
	applied, err := db.SettleBatch(ctx, []*SettlementInput{{Usage: usage, Entries: entries, Counters: counters}})
	if err != nil {
		return false, err
	}
	return applied > 0, nil
}

// SettleBatch writes many attempts in ONE transaction: either every usage row, ledger
// entry, balance update and counter lands, or none of them do. A replay after a crash
// is a no-op because the usage row and every ledger entry carry a unique key.
//
// It returns how many usage rows were newly inserted (replays count as zero).
func (db *DB) SettleBatch(ctx context.Context, inputs []*SettlementInput) (int, error) {
	if len(inputs) == 0 {
		return 0, nil
	}
	now := unix(time.Now())
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin settlement tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	applied := 0
	for _, input := range inputs {
		if input == nil || input.Usage == nil {
			continue
		}
		usage := input.Usage
		if usage.RequestID == "" {
			return 0, domain.ErrInvalidRequest("settlement requires a request_id")
		}
		if usage.AttemptNo == 0 {
			usage.AttemptNo = 1
		}
		if usage.CreatedAt.IsZero() {
			usage.CreatedAt = time.Now().UTC()
		}

		res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO usage_records(request_id, attempt_no, account_id, api_key_id, model, resolved_model,
  provider_id, dimensions_json, cost_micros, charge_micros, overshoot_cost_micros,
  pricing_snapshot_json, latency_ms, ttft_ms, status, error_code, degraded_features_json,
  usage_source, terminated_reason, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			usage.RequestID, usage.AttemptNo, usage.AccountID, usage.APIKeyID, usage.Model,
			usage.ResolvedModel, usage.ProviderID, usage.DimensionsJSON, usage.CostMicros,
			usage.ChargeMicros, usage.OvershootCost, usage.PricingSnapshot, usage.LatencyMS,
			usage.TTFTMS, usage.Status, usage.ErrorCode, usage.DegradedFeatures, usage.UsageSource,
			usage.TerminatedReason, unix(usage.CreatedAt))
		if err != nil {
			return 0, fmt.Errorf("store: insert usage in settlement: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: settlement rows affected: %w", err)
		}
		if affected > 0 {
			applied++
			if id, err := res.LastInsertId(); err == nil {
				usage.ID = id
			}
		}

		for _, entry := range input.Entries {
			if entry == nil {
				continue
			}
			if entry.AccountID == 0 || entry.IdemKey == "" {
				return 0, domain.ErrInvalidRequest("ledger entry requires account_id and idem_key")
			}
			var balance int64
			if err := tx.QueryRowContext(ctx,
				"SELECT balance_micros FROM accounts WHERE id = ?", entry.AccountID).Scan(&balance); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return 0, domain.ErrNotFound(fmt.Sprintf("account %d", entry.AccountID))
				}
				return 0, fmt.Errorf("store: read balance of account %d: %w", entry.AccountID, err)
			}
			if entry.CreatedAt.IsZero() {
				entry.CreatedAt = usage.CreatedAt
			}
			newBalance := balance + entry.AmountMicros
			res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO ledger_entries(account_id, api_key_id, kind, amount_micros, balance_after_micros,
  ref_type, ref_id, idem_key, rebuild_seq, note, actor, created_at, expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				entry.AccountID, entry.APIKeyID, entry.Kind, entry.AmountMicros, newBalance,
				entry.RefType, entry.RefID, entry.IdemKey, entry.RebuildSeq, entry.Note,
				entry.Actor, unix(entry.CreatedAt), unixPtr(entry.ExpiresAt))
			if err != nil {
				return 0, fmt.Errorf("store: insert ledger entry %s: %w", entry.IdemKey, err)
			}
			inserted, err := res.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("store: ledger rows affected: %w", err)
			}
			if inserted == 0 {
				continue
			}
			entry.BalanceAfterMicros = newBalance
			if _, err := tx.ExecContext(ctx,
				"UPDATE accounts SET balance_micros = ?, updated_at = ? WHERE id = ?",
				newBalance, now, entry.AccountID); err != nil {
				return 0, fmt.Errorf("store: update balance of account %d: %w", entry.AccountID, err)
			}
		}

		for _, counter := range input.Counters {
			if counter.Period == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO usage_counters(account_id, api_key_id, tag, period, requests, tokens, cost_micros, charge_micros)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(account_id, api_key_id, tag, period) DO UPDATE SET
  requests = requests + excluded.requests,
  tokens = tokens + excluded.tokens,
  cost_micros = cost_micros + excluded.cost_micros,
  charge_micros = charge_micros + excluded.charge_micros`,
				counter.AccountID, counter.APIKeyID, counter.Tag, counter.Period, counter.Requests,
				counter.Tokens, counter.CostMicros, counter.ChargeMicros); err != nil {
				return 0, fmt.Errorf("store: upsert usage counter: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit settlement: %w", err)
	}
	return applied, nil
}

// LedgerTotals sums ledger amounts per account and per kind, for invariant checks.
func (db *DB) LedgerTotals(ctx context.Context) (map[int64]int64, map[string]int64, error) {
	byAccount := map[int64]int64{}
	byKind := map[string]int64{}
	rows, err := db.read.QueryContext(ctx,
		"SELECT account_id, kind, SUM(amount_micros) FROM ledger_entries GROUP BY account_id, kind")
	if err != nil {
		return nil, nil, fmt.Errorf("store: ledger totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			accountID int64
			kind      string
			total     int64
		)
		if err := rows.Scan(&accountID, &kind, &total); err != nil {
			return nil, nil, fmt.Errorf("store: scan ledger totals: %w", err)
		}
		byAccount[accountID] += total
		byKind[kind] += total
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate ledger totals: %w", err)
	}
	return byAccount, byKind, nil
}

// LastLedgerBalances returns each account's final balance_after_micros.
func (db *DB) LastLedgerBalances(ctx context.Context) (map[int64]int64, error) {
	out := map[int64]int64{}
	rows, err := db.read.QueryContext(ctx, `
SELECT account_id, balance_after_micros FROM ledger_entries le
WHERE id = (SELECT MAX(id) FROM ledger_entries WHERE account_id = le.account_id)`)
	if err != nil {
		return nil, fmt.Errorf("store: last ledger balances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			accountID int64
			balance   int64
		)
		if err := rows.Scan(&accountID, &balance); err != nil {
			return nil, fmt.Errorf("store: scan last ledger balance: %w", err)
		}
		out[accountID] = balance
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate last ledger balances: %w", err)
	}
	return out, nil
}

// UsageCharges sums the charge side of usage per account and overall.
func (db *DB) UsageCharges(ctx context.Context) (map[int64]int64, int64, error) {
	byAccount := map[int64]int64{}
	var total int64
	rows, err := db.read.QueryContext(ctx,
		"SELECT account_id, SUM(charge_micros) FROM usage_records GROUP BY account_id")
	if err != nil {
		return nil, 0, fmt.Errorf("store: usage charges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			accountID int64
			sum       int64
		)
		if err := rows.Scan(&accountID, &sum); err != nil {
			return nil, 0, fmt.Errorf("store: scan usage charges: %w", err)
		}
		byAccount[accountID] += sum
		total += sum
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate usage charges: %w", err)
	}
	return byAccount, total, nil
}

// RawExec runs a maintenance statement. It exists for tests that need to damage the
// materialised state on purpose before checking that an audit or rebuild repairs it.
func (db *DB) RawExec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := db.write.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: raw exec: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: raw exec rows: %w", err)
	}
	return affected, nil
}

// RecordBillingFailure stores a settlement that could not be written, so the replay
// job can find it even if the fallback file is rotated away.
func (db *DB) RecordBillingFailure(ctx context.Context, settlement any, cause error) error {
	payload, err := json.Marshal(settlement)
	if err != nil {
		return fmt.Errorf("store: marshal billing failure: %w", err)
	}
	requestID := ""
	attemptNo := 0
	accountID := int64(0)
	if typed, ok := settlement.(interface {
		FailureRef() (string, int, int64)
	}); ok {
		requestID, attemptNo, accountID = typed.FailureRef()
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO billing_failures(request_id, attempt_no, account_id, payload_json, error, retries, created_at)
VALUES(?,?,?,?,?,0,?)`,
		requestID, attemptNo, accountID, string(payload), message, unix(time.Now())); err != nil {
		return fmt.Errorf("store: record billing failure: %w", err)
	}
	return nil
}

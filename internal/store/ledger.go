package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// AppendLedger applies ledger entries atomically: each entry is inserted at most once
// (idem_key is unique) and the account balance is updated in the same transaction, so a
// replay of an already-applied entry is a no-op instead of a double charge.
// It returns how many entries were newly applied, so a replay can be told apart
// from a first write.
func (db *DB) AppendLedger(ctx context.Context, entries []*domain.LedgerEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin ledger tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	applied := 0

	for _, e := range entries {
		if e == nil || e.AccountID == 0 || e.IdemKey == "" {
			return 0, domain.ErrInvalidRequest("ledger entry requires account_id and idem_key")
		}
		var balance int64
		if err := tx.QueryRowContext(ctx,
			"SELECT balance_micros FROM accounts WHERE id = ?", e.AccountID).Scan(&balance); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, domain.ErrNotFound(fmt.Sprintf("account %d", e.AccountID))
			}
			return 0, fmt.Errorf("store: read balance of account %d: %w", e.AccountID, err)
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now().UTC()
		}
		newBalance := balance + e.AmountMicros

		res, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO ledger_entries(account_id, api_key_id, kind, amount_micros, balance_after_micros,
  ref_type, ref_id, idem_key, rebuild_seq, note, actor, created_at, expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.AccountID, e.APIKeyID, e.Kind, e.AmountMicros, newBalance, e.RefType, e.RefID,
			e.IdemKey, e.RebuildSeq, e.Note, e.Actor, unix(e.CreatedAt), unixPtr(e.ExpiresAt))
		if err != nil {
			return 0, fmt.Errorf("store: insert ledger entry %s: %w", e.IdemKey, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: ledger rows affected: %w", err)
		}
		if affected == 0 {
			// Already applied: report where the account stands, but do not
			// count it as applied so callers can detect a replay.
			e.BalanceAfterMicros = balance
			continue
		}
		applied++
		e.BalanceAfterMicros = newBalance
		if _, err := tx.ExecContext(ctx,
			"UPDATE accounts SET balance_micros = ?, updated_at = ? WHERE id = ?",
			newBalance, unix(time.Now()), e.AccountID); err != nil {
			return 0, fmt.Errorf("store: update balance of account %d: %w", e.AccountID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit ledger: %w", err)
	}
	return applied, nil
}

// ListExpiringGrants returns matured gift grants that have not been expired yet.
func (db *DB) ListExpiringGrants(ctx context.Context, now time.Time, limit int) ([]*domain.LedgerEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.read.QueryContext(ctx, `
SELECT `+ledgerCols+` FROM ledger_entries le
WHERE le.kind = 'credit_grant' AND le.expires_at IS NOT NULL AND le.expires_at <= ?
  AND le.amount_micros > 0
  AND NOT EXISTS (SELECT 1 FROM ledger_entries x WHERE x.kind = 'expire' AND x.ref_id = le.idem_key)
ORDER BY le.expires_at, le.id LIMIT ?`, unix(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list expiring grants: %w", err)
	}
	defer rows.Close()
	out := []*domain.LedgerEntry{}
	for rows.Next() {
		var (
			e         domain.LedgerEntry
			apiKeyID  sql.NullInt64
			createdAt int64
			expiresAt sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &e.AccountID, &apiKeyID, &e.Kind, &e.AmountMicros,
			&e.BalanceAfterMicros, &e.RefType, &e.RefID, &e.IdemKey, &e.RebuildSeq,
			&e.Note, &e.Actor, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("store: scan expiring grant: %w", err)
		}
		e.APIKeyID = nullInt64Ptr(apiKeyID)
		e.ExpiresAt = timePtrFromNull(expiresAt)
		e.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate expiring grants: %w", err)
	}
	return out, nil
}

const ledgerCols = `id, account_id, api_key_id, kind, amount_micros, balance_after_micros,
	ref_type, ref_id, idem_key, rebuild_seq, note, actor, created_at, expires_at`

// LedgerWindow describes one window over an account's ledger. ExcludeKinds, when
// non-empty, drops those entry kinds in SQL: the console's credits view excludes
// charges, and filtering after the LIMIT would return uneven pages with a total that
// describes a different row set than the page shows. It is an exclusion list rather
// than an allow-list so a kind added later still shows up in that view.
type LedgerWindow struct {
	AccountID     int64
	From, To      time.Time
	ExcludeKinds  []string
	Limit, Offset int
}

// ledgerFilter builds the WHERE clause shared by the ledger page query and its count.
func ledgerFilter(w LedgerWindow) (string, []any) {
	where := " WHERE account_id = ?"
	args := []any{w.AccountID}
	if !w.From.IsZero() {
		where += " AND created_at >= ?"
		args = append(args, unix(w.From))
	}
	if !w.To.IsZero() {
		where += " AND created_at <= ?"
		args = append(args, unix(w.To))
	}
	if len(w.ExcludeKinds) > 0 {
		where += " AND kind NOT IN (" + placeholders(len(w.ExcludeKinds)) + ")"
		for _, kind := range w.ExcludeKinds {
			args = append(args, kind)
		}
	}
	return where, args
}

// placeholders renders "?, ?, ?" for an IN clause with n values.
func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ", "
		}
		out += "?"
	}
	return out
}

// ListLedger returns the first page of an account's ledger entries (newest first).
func (db *DB) ListLedger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.LedgerEntry, error) {
	return db.ListLedgerPage(ctx, LedgerWindow{AccountID: accountID, From: from, To: to, Limit: limit})
}

// ListLedgerPage returns one window of an account's ledger entries (newest first).
func (db *DB) ListLedgerPage(ctx context.Context, w LedgerWindow) ([]*domain.LedgerEntry, error) {
	limit := normalizeLimit(w.Limit, 100, 1000)
	offset := w.Offset
	if offset < 0 {
		offset = 0
	}
	where, args := ledgerFilter(w)
	query := "SELECT " + ledgerCols + " FROM ledger_entries" + where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list ledger: %w", err)
	}
	defer rows.Close()

	out := []*domain.LedgerEntry{}
	for rows.Next() {
		var (
			e         domain.LedgerEntry
			apiKeyID  sql.NullInt64
			createdAt int64
			expiresAt sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &e.AccountID, &apiKeyID, &e.Kind, &e.AmountMicros,
			&e.BalanceAfterMicros, &e.RefType, &e.RefID, &e.IdemKey, &e.RebuildSeq,
			&e.Note, &e.Actor, &createdAt, &expiresAt); err != nil {
			return nil, fmt.Errorf("store: scan ledger entry: %w", err)
		}
		e.APIKeyID = nullInt64Ptr(apiKeyID)
		e.CreatedAt = timeFromUnix(createdAt)
		e.ExpiresAt = timePtrFromNull(expiresAt)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate ledger: %w", err)
	}
	return out, nil
}

// CountLedger counts the rows the same window selects (without limit/offset).
func (db *DB) CountLedger(ctx context.Context, w LedgerWindow) (int, error) {
	where, args := ledgerFilter(w)
	return db.countRows(ctx, "ledger_entries", where, args, "ledger entries")
}

// GetBalance returns the materialised balance of an account.
func (db *DB) GetBalance(ctx context.Context, accountID int64) (int64, error) {
	var balance int64
	if err := db.read.QueryRowContext(ctx,
		"SELECT balance_micros FROM accounts WHERE id = ?", accountID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, domain.ErrNotFound(fmt.Sprintf("account %d", accountID))
		}
		return 0, fmt.Errorf("store: get balance of account %d: %w", accountID, err)
	}
	return balance, nil
}

const usageCols = `id, request_id, attempt_no, account_id, api_key_id, model, resolved_model,
	provider_id, dimensions_json, cost_micros, charge_micros, overshoot_cost_micros,
	pricing_snapshot_json, latency_ms, ttft_ms, status, error_code, degraded_features_json,
	usage_source, terminated_reason, created_at`

// InsertUsage appends one metered upstream attempt.
func (db *DB) InsertUsage(ctx context.Context, rec *domain.UsageRecord) (int64, error) {
	if rec == nil || rec.RequestID == "" {
		return 0, domain.ErrInvalidRequest("usage record requires request_id")
	}
	if rec.AttemptNo == 0 {
		rec.AttemptNo = 1
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	res, err := db.write.ExecContext(ctx, `
INSERT INTO usage_records(request_id, attempt_no, account_id, api_key_id, model, resolved_model,
  provider_id, dimensions_json, cost_micros, charge_micros, overshoot_cost_micros,
  pricing_snapshot_json, latency_ms, ttft_ms, status, error_code, degraded_features_json,
  usage_source, terminated_reason, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.RequestID, rec.AttemptNo, rec.AccountID, rec.APIKeyID, rec.Model, rec.ResolvedModel,
		rec.ProviderID, rec.DimensionsJSON, rec.CostMicros, rec.ChargeMicros, rec.OvershootCost,
		rec.PricingSnapshot, rec.LatencyMS, rec.TTFTMS, rec.Status, rec.ErrorCode,
		rec.DegradedFeatures, rec.UsageSource, rec.TerminatedReason, unix(rec.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("store: insert usage record: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: usage record id: %w", err)
	}
	rec.ID = id
	return id, nil
}

// ListUsage returns usage rows of one account inside a window (newest first).
func (db *DB) ListUsage(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.UsageRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := "SELECT " + usageCols + " FROM usage_records WHERE account_id = ?"
	args := []any{accountID}
	if !from.IsZero() {
		query += " AND created_at >= ?"
		args = append(args, unix(from))
	}
	if !to.IsZero() {
		query += " AND created_at <= ?"
		args = append(args, unix(to))
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list usage: %w", err)
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
			return nil, fmt.Errorf("store: scan usage record: %w", err)
		}
		r.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate usage: %w", err)
	}
	return out, nil
}

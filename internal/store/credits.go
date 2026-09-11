package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const codeCols = `id, code_hash, amount_micros, expires_at, redeemed_by_account_id, redeemed_at,
	batch_id, created_by, note, created_at`

func scanCode(row rowScanner) (*domain.RedemptionCode, error) {
	var (
		code       domain.RedemptionCode
		expiresAt  sql.NullInt64
		redeemedBy sql.NullInt64
		redeemedAt sql.NullInt64
		createdAt  int64
	)
	if err := row.Scan(&code.ID, &code.CodeHash, &code.AmountMicros, &expiresAt, &redeemedBy,
		&redeemedAt, &code.BatchID, &code.CreatedBy, &code.Note, &createdAt); err != nil {
		return nil, err
	}
	code.ExpiresAt = timePtrFromNull(expiresAt)
	code.RedeemedAt = timePtrFromNull(redeemedAt)
	if redeemedBy.Valid {
		value := redeemedBy.Int64
		code.RedeemedByAccountID = &value
	}
	code.CreatedAt = timeFromUnix(createdAt)
	return &code, nil
}

// InsertRedemptionCodes stores a freshly generated batch. Only hashes are stored.
func (db *DB) InsertRedemptionCodes(ctx context.Context, codes []*domain.RedemptionCode) error {
	if len(codes) == 0 {
		return nil
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin redemption batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, code := range codes {
		if code == nil || code.CodeHash == "" {
			return domain.ErrInvalidRequest("redemption code requires a hash")
		}
		if code.CreatedAt.IsZero() {
			code.CreatedAt = time.Now().UTC()
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO redemption_codes(code_hash, amount_micros, expires_at, batch_id, created_by, note, created_at)
VALUES(?,?,?,?,?,?,?)`,
			code.CodeHash, code.AmountMicros, unixPtr(code.ExpiresAt), code.BatchID,
			code.CreatedBy, code.Note, unix(code.CreatedAt))
		if err != nil {
			return fmt.Errorf("store: insert redemption code: %w", err)
		}
		if id, err := res.LastInsertId(); err == nil {
			code.ID = id
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit redemption batch: %w", err)
	}
	return nil
}

// ListRedemptionCodes returns the first page of codes (optionally one batch).
func (db *DB) ListRedemptionCodes(ctx context.Context, batchID string, limit int) ([]*domain.RedemptionCode, error) {
	return db.ListRedemptionCodesPage(ctx, batchID, limit, 0)
}

// ListRedemptionCodesPage returns one page of codes (optionally one batch), newest first.
func (db *DB) ListRedemptionCodesPage(ctx context.Context, batchID string, limit, offset int) ([]*domain.RedemptionCode, error) {
	limit = normalizeLimit(limit, 100, 500)
	if offset < 0 {
		offset = 0
	}
	query := "SELECT " + codeCols + " FROM redemption_codes"
	args := []any{}
	if batchID != "" {
		query += " WHERE batch_id = ?"
		args = append(args, batchID)
	}
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list redemption codes: %w", err)
	}
	defer rows.Close()
	out := []*domain.RedemptionCode{}
	for rows.Next() {
		code, err := scanCode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan redemption code: %w", err)
		}
		out = append(out, code)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate redemption codes: %w", err)
	}
	return out, nil
}

// CountRedemptionCodes counts the codes the same batch filter selects.
func (db *DB) CountRedemptionCodes(ctx context.Context, batchID string) (int, error) {
	where := ""
	args := []any{}
	if batchID != "" {
		where = " WHERE batch_id = ?"
		args = append(args, batchID)
	}
	return db.countRows(ctx, "redemption_codes", where, args, "redemption codes")
}

// RedeemCode claims one code for an account. The conditional UPDATE is the whole
// concurrency story: exactly one caller can move the row from unclaimed to claimed.
func (db *DB) RedeemCode(ctx context.Context, codeHash string, accountID int64, now time.Time) (*domain.RedemptionCode, error) {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin redeem tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, "SELECT "+codeCols+" FROM redemption_codes WHERE code_hash = ?", codeHash)
	code, err := scanCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("redemption code")
	}
	if err != nil {
		return nil, fmt.Errorf("store: lookup redemption code: %w", err)
	}
	if code.RedeemedAt != nil {
		return nil, domain.ErrConflict("this redemption code has already been used")
	}
	if code.ExpiresAt != nil && code.ExpiresAt.Before(now) {
		return nil, domain.ErrInvalidRequest("this redemption code has expired")
	}

	res, err := tx.ExecContext(ctx, `
UPDATE redemption_codes SET redeemed_by_account_id = ?, redeemed_at = ?
WHERE code_hash = ? AND redeemed_by_account_id IS NULL`,
		accountID, unix(now), codeHash)
	if err != nil {
		return nil, fmt.Errorf("store: claim redemption code: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: redeem rows affected: %w", err)
	}
	if affected != 1 {
		return nil, domain.ErrConflict("this redemption code has already been used")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit redemption: %w", err)
	}
	code.RedeemedByAccountID = &accountID
	code.RedeemedAt = &now
	return code, nil
}

// GetRedemptionCodeByHash finds a code by its hash (used to detect duplicates).
func (db *DB) GetRedemptionCodeByHash(ctx context.Context, codeHash string) (*domain.RedemptionCode, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+codeCols+" FROM redemption_codes WHERE code_hash = ?", codeHash)
	code, err := scanCode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("redemption code")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get redemption code: %w", err)
	}
	return code, nil
}

// InsertReconciliation stores one reconciliation run.
func (db *DB) InsertReconciliation(ctx context.Context, rec *domain.Reconciliation) (int64, error) {
	if rec == nil {
		return 0, domain.ErrInvalidRequest("reconciliation record is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	res, err := db.write.ExecContext(ctx, `
INSERT INTO billing_reconciliations(period_start, period_end, kind, usage_charge_micros,
  ledger_charge_micros, diff_micros, missing_usage_count, estimated_ratio_bp, details_json, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		unix(rec.PeriodStart), unix(rec.PeriodEnd), rec.Kind,
		rec.UsageChargeMicros, rec.LedgerChargeMicros, rec.DiffMicros, rec.MissingUsageCount,
		rec.EstimatedRatioBP, rec.DetailsJSON, unix(rec.CreatedAt))
	_ = res
	if err != nil {
		return 0, fmt.Errorf("store: insert reconciliation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: reconciliation id: %w", err)
	}
	rec.ID = id
	return id, nil
}

// ListReconciliations returns the first page of reconciliation runs.
func (db *DB) ListReconciliations(ctx context.Context, limit int) ([]*domain.Reconciliation, error) {
	return db.ListReconciliationsPage(ctx, limit, 0)
}

// ListReconciliationsPage returns one page of reconciliation runs, newest first.
func (db *DB) ListReconciliationsPage(ctx context.Context, limit, offset int) ([]*domain.Reconciliation, error) {
	limit = normalizeLimit(limit, 50, 500)
	if offset < 0 {
		offset = 0
	}
	rows, err := db.read.QueryContext(ctx, `
SELECT id, period_start, period_end, kind, usage_charge_micros, ledger_charge_micros,
  diff_micros, missing_usage_count, estimated_ratio_bp, details_json, created_at
FROM billing_reconciliations ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list reconciliations: %w", err)
	}
	defer rows.Close()
	out := []*domain.Reconciliation{}
	for rows.Next() {
		var (
			rec        domain.Reconciliation
			start, end int64
			createdAt  int64
		)
		if err := rows.Scan(&rec.ID, &start, &end, &rec.Kind, &rec.UsageChargeMicros,
			&rec.LedgerChargeMicros, &rec.DiffMicros, &rec.MissingUsageCount,
			&rec.EstimatedRatioBP, &rec.DetailsJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan reconciliation: %w", err)
		}
		rec.PeriodStart = timeFromUnix(start)
		rec.PeriodEnd = timeFromUnix(end)
		rec.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate reconciliations: %w", err)
	}
	return out, nil
}

// CountReconciliations counts every reconciliation run.
func (db *DB) CountReconciliations(ctx context.Context) (int, error) {
	return db.countRows(ctx, "billing_reconciliations", "", nil, "reconciliations")
}

// LedgerChargeTotals sums charge ledger entries per account inside a window.
func (db *DB) LedgerChargeTotals(ctx context.Context, from, to time.Time) (map[int64]int64, map[int64]int64, error) {
	ledger := map[int64]int64{}
	usage := map[int64]int64{}
	rows, err := db.read.QueryContext(ctx, `
SELECT account_id, COALESCE(SUM(amount_micros), 0) FROM ledger_entries
WHERE kind = 'charge' AND created_at >= ? AND created_at <= ? GROUP BY account_id`,
		unix(from), unix(to))
	if err != nil {
		return nil, nil, fmt.Errorf("store: ledger charge totals: %w", err)
	}
	for rows.Next() {
		var accountID, sum int64
		if err := rows.Scan(&accountID, &sum); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("store: scan ledger charge totals: %w", err)
		}
		ledger[accountID] = -sum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("store: iterate ledger charge totals: %w", err)
	}
	rows.Close()

	usageRows, err := db.read.QueryContext(ctx, `
SELECT account_id, COALESCE(SUM(charge_micros), 0) FROM usage_records
WHERE created_at >= ? AND created_at <= ? GROUP BY account_id`,
		unix(from), unix(to))
	if err != nil {
		return nil, nil, fmt.Errorf("store: usage charge totals: %w", err)
	}
	defer usageRows.Close()
	for usageRows.Next() {
		var accountID, sum int64
		if err := usageRows.Scan(&accountID, &sum); err != nil {
			return nil, nil, fmt.Errorf("store: scan usage charge totals: %w", err)
		}
		usage[accountID] = sum
	}
	if err := usageRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate usage charge totals: %w", err)
	}
	return ledger, usage, nil
}

// UsageSamplesForWindow lists request ids of an account inside a window (bounded).
func (db *DB) UsageSamplesForWindow(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := db.read.QueryContext(ctx, `
SELECT request_id FROM usage_records WHERE account_id = ? AND created_at >= ? AND created_at <= ?
ORDER BY id LIMIT ?`, accountID, unix(from), unix(to), limit)
	if err != nil {
		return nil, fmt.Errorf("store: usage samples: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan usage sample: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate usage samples: %w", err)
	}
	return out, nil
}

// EstimatedUsageRatio returns the share of estimated usage in a window, in basis points.
func (db *DB) EstimatedUsageRatio(ctx context.Context, from, to time.Time) (int64, error) {
	var total, estimated int64
	if err := db.read.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(CASE WHEN usage_source = 'estimated' THEN 1 ELSE 0 END), 0)
FROM usage_records WHERE created_at >= ? AND created_at <= ?`,
		unix(from), unix(to)).Scan(&total, &estimated); err != nil {
		return 0, fmt.Errorf("store: estimated usage ratio: %w", err)
	}
	if total == 0 {
		return 0, nil
	}
	return estimated * 10000 / total, nil
}

// CreditTotals sums credit ledger entries per account inside a window (topup, grant,
// adjustment, refund, expire).
func (db *DB) CreditTotals(ctx context.Context, accountID int64, from, to time.Time) (map[string]int64, error) {
	query := `
SELECT kind, COALESCE(SUM(amount_micros), 0) FROM ledger_entries
WHERE kind <> 'charge' AND created_at >= ? AND created_at <= ?`
	args := []any{unix(from), unix(to)}
	if accountID > 0 {
		query += " AND account_id = ?"
		args = append(args, accountID)
	}
	query += " GROUP BY kind"
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: credit totals: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var kind string
		var sum int64
		if err := rows.Scan(&kind, &sum); err != nil {
			return nil, fmt.Errorf("store: scan credit totals: %w", err)
		}
		out[kind] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate credit totals: %w", err)
	}
	return out, nil
}

// ReleaseRedemptionCode undoes a claim whose credit failed, so the customer can retry.
func (db *DB) ReleaseRedemptionCode(ctx context.Context, codeHash string) error {
	if _, err := db.write.ExecContext(ctx, `
UPDATE redemption_codes SET redeemed_by_account_id = NULL, redeemed_at = NULL
WHERE code_hash = ?`, codeHash); err != nil {
		return fmt.Errorf("store: release redemption code: %w", err)
	}
	return nil
}

// BillingFailure is one settlement the database refused earlier.
type BillingFailure struct {
	ID          int64
	RequestID   string
	AttemptNo   int
	AccountID   int64
	PayloadJSON string
	Error       string
	Retries     int
	CreatedAt   time.Time
}

// ListBillingFailures returns unresolved settlements, oldest first.
func (db *DB) ListBillingFailures(ctx context.Context, limit int) ([]BillingFailure, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.read.QueryContext(ctx, `
SELECT id, request_id, attempt_no, account_id, payload_json, error, retries, created_at
FROM billing_failures WHERE resolved_at IS NULL ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list billing failures: %w", err)
	}
	defer rows.Close()
	out := []BillingFailure{}
	for rows.Next() {
		var (
			failure   BillingFailure
			createdAt int64
		)
		if err := rows.Scan(&failure.ID, &failure.RequestID, &failure.AttemptNo, &failure.AccountID,
			&failure.PayloadJSON, &failure.Error, &failure.Retries, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan billing failure: %w", err)
		}
		failure.CreatedAt = timeFromUnix(createdAt)
		out = append(out, failure)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate billing failures: %w", err)
	}
	return out, nil
}

// ResolveBillingFailure marks a failure as replayed successfully.
func (db *DB) ResolveBillingFailure(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx,
		"UPDATE billing_failures SET resolved_at = ? WHERE id = ?", unix(time.Now()), id); err != nil {
		return fmt.Errorf("store: resolve billing failure: %w", err)
	}
	return nil
}

// MarkBillingFailureRetry records a failed replay attempt.
func (db *DB) MarkBillingFailureRetry(ctx context.Context, id int64, message string) error {
	if _, err := db.write.ExecContext(ctx,
		"UPDATE billing_failures SET retries = retries + 1, error = ? WHERE id = ?", message, id); err != nil {
		return fmt.Errorf("store: mark billing failure retry: %w", err)
	}
	return nil
}

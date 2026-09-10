package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const invoiceCols = `id, account_id, period_start, period_end, status, currency,
	total_cost_micros, total_charge_micros, issued_at, paid_at, voided_at, note, created_at`

func scanInvoice(row rowScanner) (*domain.Invoice, error) {
	var (
		inv       domain.Invoice
		start     int64
		end       int64
		issuedAt  sql.NullInt64
		paidAt    sql.NullInt64
		voidedAt  sql.NullInt64
		createdAt int64
	)
	if err := row.Scan(&inv.ID, &inv.AccountID, &start, &end, &inv.Status, &inv.Currency,
		&inv.TotalCostMicros, &inv.TotalChargeMicros, &issuedAt, &paidAt, &voidedAt,
		&inv.Note, &createdAt); err != nil {
		return nil, err
	}
	inv.PeriodStart = timeFromUnix(start)
	inv.PeriodEnd = timeFromUnix(end)
	inv.IssuedAt = timePtrFromNull(issuedAt)
	inv.PaidAt = timePtrFromNull(paidAt)
	inv.VoidedAt = timePtrFromNull(voidedAt)
	inv.CreatedAt = timeFromUnix(createdAt)
	return &inv, nil
}

// GetInvoiceByPeriod finds the invoice of one account and period.
func (db *DB) GetInvoiceByPeriod(ctx context.Context, accountID int64, start, end time.Time) (*domain.Invoice, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+invoiceCols+" FROM invoices WHERE account_id = ? AND period_start = ? AND period_end = ?",
		accountID, unix(start), unix(end))
	inv, err := scanInvoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("invoice for this period")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get invoice: %w", err)
	}
	return inv, nil
}

// GetInvoice loads one invoice with its lines.
func (db *DB) GetInvoice(ctx context.Context, id int64) (*domain.Invoice, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+invoiceCols+" FROM invoices WHERE id = ?", id)
	inv, err := scanInvoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("invoice %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get invoice: %w", err)
	}
	lines, err := db.invoiceLines(ctx, id)
	if err != nil {
		return nil, err
	}
	inv.Lines = lines
	return inv, nil
}

func (db *DB) invoiceLines(ctx context.Context, invoiceID int64) ([]domain.InvoiceLine, error) {
	rows, err := db.read.QueryContext(ctx, `
SELECT group_type, group_key, requests, prompt_tokens, completion_tokens, cost_micros, charge_micros
FROM invoice_lines WHERE invoice_id = ? ORDER BY charge_micros DESC, group_key`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("store: invoice lines: %w", err)
	}
	defer rows.Close()
	out := []domain.InvoiceLine{}
	for rows.Next() {
		var line domain.InvoiceLine
		if err := rows.Scan(&line.GroupType, &line.GroupKey, &line.Requests, &line.PromptTokens,
			&line.CompletionTokens, &line.CostMicros, &line.ChargeMicros); err != nil {
			return nil, fmt.Errorf("store: scan invoice line: %w", err)
		}
		line.InvoiceID = invoiceID
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate invoice lines: %w", err)
	}
	return out, nil
}

// ListInvoices returns the invoices of one account (accountID <= 0 means all).
func (db *DB) ListInvoices(ctx context.Context, accountID int64, limit int) ([]*domain.Invoice, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := "SELECT " + invoiceCols + " FROM invoices"
	args := []any{}
	if accountID > 0 {
		query += " WHERE account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY period_start DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list invoices: %w", err)
	}
	defer rows.Close()
	out := []*domain.Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan invoice: %w", err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate invoices: %w", err)
	}
	return out, nil
}

// AggregateInvoiceLines computes the invoice lines of one account and period from
// the usage rows, grouped by model, key or day.
func (db *DB) AggregateInvoiceLines(ctx context.Context, accountID int64, start, end time.Time, groupBy string) ([]domain.InvoiceLine, error) {
	expression := "model"
	groupType := "model"
	switch groupBy {
	case "key":
		expression = "CAST(api_key_id AS TEXT)"
		groupType = "key"
	case "day":
		expression = "date(created_at, 'unixepoch')"
		groupType = "day"
	}
	query := fmt.Sprintf(`
SELECT %s AS group_key, COUNT(*) AS requests,
  COALESCE(SUM(json_extract(dimensions_json, '$.input')), 0)
  + COALESCE(SUM(json_extract(dimensions_json, '$.input_cache_hit')), 0)
  + COALESCE(SUM(json_extract(dimensions_json, '$.input_cache_miss')), 0) AS prompt_tokens,
  COALESCE(SUM(json_extract(dimensions_json, '$.output')), 0)
  + COALESCE(SUM(json_extract(dimensions_json, '$.reasoning')), 0) AS completion_tokens,
  COALESCE(SUM(cost_micros), 0) AS cost_micros,
  COALESCE(SUM(charge_micros), 0) AS charge_micros
FROM usage_records WHERE account_id = ? AND created_at >= ? AND created_at < ?
GROUP BY group_key ORDER BY charge_micros DESC`, expression)
	rows, err := db.read.QueryContext(ctx, query, accountID, unix(start), unix(end))
	if err != nil {
		return nil, fmt.Errorf("store: aggregate invoice lines: %w", err)
	}
	defer rows.Close()
	out := []domain.InvoiceLine{}
	for rows.Next() {
		var line domain.InvoiceLine
		if err := rows.Scan(&line.GroupKey, &line.Requests, &line.PromptTokens,
			&line.CompletionTokens, &line.CostMicros, &line.ChargeMicros); err != nil {
			return nil, fmt.Errorf("store: scan invoice aggregate: %w", err)
		}
		line.GroupType = groupType
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate invoice aggregates: %w", err)
	}
	return out, nil
}

// PutInvoice inserts an invoice with its lines in one transaction. An existing invoice
// for the same period is returned instead of being duplicated unless replace is set.
func (db *DB) PutInvoice(ctx context.Context, inv *domain.Invoice, lines []domain.InvoiceLine, replace bool) (int64, bool, error) {
	if inv == nil || inv.AccountID == 0 {
		return 0, false, domain.ErrInvalidRequest("invoice requires account_id")
	}
	if inv.Currency == "" {
		inv.Currency = "USD"
	}
	if inv.Status == "" {
		inv.Status = "draft"
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now().UTC()
	}

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("store: begin invoice tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingID int64
	var existingStatus string
	err = tx.QueryRowContext(ctx,
		"SELECT id, status FROM invoices WHERE account_id = ? AND period_start = ? AND period_end = ?",
		inv.AccountID, unix(inv.PeriodStart), unix(inv.PeriodEnd)).Scan(&existingID, &existingStatus)
	switch {
	case err == nil:
		if !replace {
			return existingID, false, nil
		}
		if existingStatus != "draft" {
			return 0, false, domain.ErrConflict("an invoice that is no longer a draft cannot be recomputed")
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM invoice_lines WHERE invoice_id = ?", existingID); err != nil {
			return 0, false, fmt.Errorf("store: clear invoice lines: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE invoices SET total_cost_micros = ?, total_charge_micros = ?, note = ?, currency = ? WHERE id = ?`,
			inv.TotalCostMicros, inv.TotalChargeMicros, inv.Note, inv.Currency, existingID); err != nil {
			return 0, false, fmt.Errorf("store: update invoice: %w", err)
		}
		inv.ID = existingID
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `
INSERT INTO invoices(account_id, period_start, period_end, status, currency, total_cost_micros,
  total_charge_micros, note, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			inv.AccountID, unix(inv.PeriodStart), unix(inv.PeriodEnd), inv.Status, inv.Currency,
			inv.TotalCostMicros, inv.TotalChargeMicros, inv.Note, unix(inv.CreatedAt))
		if err != nil {
			return 0, false, fmt.Errorf("store: insert invoice: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("store: invoice id: %w", err)
		}
		existingID = id
		inv.ID = id
	default:
		return 0, false, fmt.Errorf("store: lookup invoice: %w", err)
	}

	for _, line := range lines {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO invoice_lines(invoice_id, group_type, group_key, requests, prompt_tokens,
  completion_tokens, cost_micros, charge_micros) VALUES(?,?,?,?,?,?,?,?)`,
			existingID, line.GroupType, line.GroupKey, line.Requests, line.PromptTokens,
			line.CompletionTokens, line.CostMicros, line.ChargeMicros); err != nil {
			return 0, false, fmt.Errorf("store: insert invoice line: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("store: commit invoice: %w", err)
	}
	return existingID, true, nil
}

// SetInvoiceStatus moves an invoice through its state machine.
func (db *DB) SetInvoiceStatus(ctx context.Context, id int64, status string, at time.Time) error {
	column := map[string]string{
		"issued": "issued_at",
		"paid":   "paid_at",
		"void":   "voided_at",
	}[status]
	if column == "" {
		return domain.ErrInvalidRequest("invoice status must be issued, paid or void")
	}
	res, err := db.write.ExecContext(ctx,
		"UPDATE invoices SET status = ?, "+column+" = ? WHERE id = ?", status, unix(at), id)
	if err != nil {
		return fmt.Errorf("store: set invoice status: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return domain.ErrNotFound(fmt.Sprintf("invoice %d", id))
	}
	return nil
}

// InvoiceChargeTotal sums the charge side of one account inside a window, straight
// from the usage rows (the source of truth for reconciliation).
func (db *DB) InvoiceChargeTotal(ctx context.Context, accountID int64, start, end time.Time) (int64, int64, error) {
	var charge, cost int64
	if err := db.read.QueryRowContext(ctx, `
SELECT COALESCE(SUM(charge_micros), 0), COALESCE(SUM(cost_micros), 0)
FROM usage_records WHERE account_id = ? AND created_at >= ? AND created_at < ?`,
		accountID, unix(start), unix(end)).Scan(&charge, &cost); err != nil {
		return 0, 0, fmt.Errorf("store: invoice charge total: %w", err)
	}
	return charge, cost, nil
}

var _ = strings.TrimSpace

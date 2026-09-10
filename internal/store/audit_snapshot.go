package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BillingAudit is every aggregate the billing invariant audit needs.
type BillingAudit struct {
	// BalanceByAccount is accounts.balance_micros.
	BalanceByAccount map[int64]int64
	// LedgerSumByAccount is sum(ledger_entries.amount_micros) per account.
	LedgerSumByAccount map[int64]int64
	// LedgerSumByKind is sum(ledger_entries.amount_micros) per kind.
	LedgerSumByKind map[string]int64
	// LastBalanceByAccount is the newest ledger row's balance_after_micros.
	LastBalanceByAccount map[int64]int64
	// UsageChargeByAccount is sum(usage_records.charge_micros) per account.
	UsageChargeByAccount map[int64]int64
	// UsageChargeTotal is the same sum over every account.
	UsageChargeTotal int64
	// BillingModeByAccount and StatusByAccount describe the accounts.
	BillingModeByAccount map[int64]string
	StatusByAccount      map[int64]string
	NameByAccount        map[int64]string
}

// BillingAuditSnapshot reads all audit aggregates inside ONE read transaction.
//
// This matters under load: settlement commits between two unrelated read queries, so
// summing usage and ledger separately reported a phantom mismatch whenever the writer
// landed a batch in between. A single deferred read transaction in WAL mode sees one
// consistent snapshot.
func (db *DB) BillingAuditSnapshot(ctx context.Context) (*BillingAudit, error) {
	tx, err := db.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: begin audit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	audit := &BillingAudit{
		BalanceByAccount:     map[int64]int64{},
		LedgerSumByAccount:   map[int64]int64{},
		LedgerSumByKind:      map[string]int64{},
		LastBalanceByAccount: map[int64]int64{},
		UsageChargeByAccount: map[int64]int64{},
		BillingModeByAccount: map[int64]string{},
		StatusByAccount:      map[int64]string{},
		NameByAccount:        map[int64]string{},
	}

	rows, err := tx.QueryContext(ctx, "SELECT id, name, balance_micros, billing_mode, status FROM accounts")
	if err != nil {
		return nil, fmt.Errorf("store: audit accounts: %w", err)
	}
	for rows.Next() {
		var (
			id      int64
			name    string
			balance int64
			mode    string
			status  string
		)
		if err := rows.Scan(&id, &name, &balance, &mode, &status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan audit account: %w", err)
		}
		audit.BalanceByAccount[id] = balance
		audit.NameByAccount[id] = name
		audit.BillingModeByAccount[id] = mode
		audit.StatusByAccount[id] = status
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate audit accounts: %w", err)
	}
	rows.Close()

	if err := eachRow(ctx, tx, "SELECT account_id, kind, SUM(amount_micros) FROM ledger_entries GROUP BY account_id, kind",
		func(scan func(...any) error) error {
			var accountID int64
			var kind string
			var total int64
			if err := scan(&accountID, &kind, &total); err != nil {
				return err
			}
			audit.LedgerSumByAccount[accountID] += total
			audit.LedgerSumByKind[kind] += total
			return nil
		}); err != nil {
		return nil, err
	}

	if err := eachRow(ctx, tx, `
SELECT account_id, balance_after_micros FROM ledger_entries le
WHERE id = (SELECT MAX(id) FROM ledger_entries WHERE account_id = le.account_id)`,
		func(scan func(...any) error) error {
			var accountID int64
			var balance int64
			if err := scan(&accountID, &balance); err != nil {
				return err
			}
			audit.LastBalanceByAccount[accountID] = balance
			return nil
		}); err != nil {
		return nil, err
	}

	if err := eachRow(ctx, tx, "SELECT account_id, SUM(charge_micros) FROM usage_records GROUP BY account_id",
		func(scan func(...any) error) error {
			var accountID int64
			var sum int64
			if err := scan(&accountID, &sum); err != nil {
				return err
			}
			audit.UsageChargeByAccount[accountID] += sum
			audit.UsageChargeTotal += sum
			return nil
		}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit audit tx: %w", err)
	}
	return audit, nil
}

func eachRow(ctx context.Context, tx *sql.Tx, query string, handle func(scan func(...any) error) error) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("store: audit query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := handle(rows.Scan); err != nil {
			return fmt.Errorf("store: scan audit row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate audit rows: %w", err)
	}
	return nil
}

var _ = time.Now

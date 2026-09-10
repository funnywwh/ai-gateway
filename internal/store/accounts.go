package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// rowScanner is implemented by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

const accountCols = `id, name, billing_mode, balance_micros, credit_limit_micros,
	low_balance_threshold_micros, price_overrides_json, markup_override_bp, auto_suspend,
	auto_resume, inflight_policy_override, overdraft_limit_micros, status, note, created_at, updated_at`

func scanAccount(row rowScanner) (*domain.Account, error) {
	var (
		a                       domain.Account
		autoSuspend, autoResume int
		createdAt, updatedAt    int64
	)
	if err := row.Scan(&a.ID, &a.Name, &a.BillingMode, &a.BalanceMicros, &a.CreditLimitMicros,
		&a.LowBalanceThresholdMicros, &a.PriceOverridesJSON, &a.MarkupOverrideBP, &autoSuspend,
		&autoResume, &a.InflightPolicyOverride, &a.OverdraftLimitMicros, &a.Status, &a.Note,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	a.AutoSuspend = autoSuspend != 0
	a.AutoResume = autoResume != 0
	a.CreatedAt = timeFromUnix(createdAt)
	a.UpdatedAt = timeFromUnix(updatedAt)
	return &a, nil
}

// GetAccount loads one account by id.
func (db *DB) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+accountCols+" FROM accounts WHERE id = ?", id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("account %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get account %d: %w", id, err)
	}
	return a, nil
}

// GetAccountByName loads one account by unique name.
func (db *DB) GetAccountByName(ctx context.Context, name string) (*domain.Account, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+accountCols+" FROM accounts WHERE name = ?", name)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("account " + name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get account %q: %w", name, err)
	}
	return a, nil
}

// ListAccounts returns all accounts ordered by name.
func (db *DB) ListAccounts(ctx context.Context) ([]*domain.Account, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT "+accountCols+" FROM accounts ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()

	out := []*domain.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate accounts: %w", err)
	}
	return out, nil
}

// UpsertAccount inserts by name or updates the matching row.
// balance_micros is deliberately NOT overwritten: the ledger owns the balance.
func (db *DB) UpsertAccount(ctx context.Context, a *domain.Account) (int64, error) {
	if a == nil || a.Name == "" {
		return 0, domain.ErrInvalidRequest("account name is required")
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.BillingMode == "" {
		a.BillingMode = domain.BillingPostpaid
	}
	if a.Status == "" {
		a.Status = "active"
	}

	if _, err := db.write.ExecContext(ctx, `
INSERT INTO accounts(name, billing_mode, balance_micros, credit_limit_micros,
  low_balance_threshold_micros, price_overrides_json, markup_override_bp, auto_suspend,
  auto_resume, inflight_policy_override, overdraft_limit_micros, status, note, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  billing_mode = excluded.billing_mode,
  credit_limit_micros = excluded.credit_limit_micros,
  low_balance_threshold_micros = excluded.low_balance_threshold_micros,
  price_overrides_json = excluded.price_overrides_json,
  markup_override_bp = excluded.markup_override_bp,
  auto_suspend = excluded.auto_suspend,
  auto_resume = excluded.auto_resume,
  inflight_policy_override = excluded.inflight_policy_override,
  overdraft_limit_micros = excluded.overdraft_limit_micros,
  status = excluded.status,
  note = excluded.note,
  updated_at = excluded.updated_at`,
		a.Name, string(a.BillingMode), a.BalanceMicros, a.CreditLimitMicros,
		a.LowBalanceThresholdMicros, a.PriceOverridesJSON, a.MarkupOverrideBP, boolInt(a.AutoSuspend),
		boolInt(a.AutoResume), a.InflightPolicyOverride, a.OverdraftLimitMicros, a.Status, a.Note,
		unix(a.CreatedAt), unix(a.UpdatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert account %q: %w", a.Name, err)
	}

	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM accounts WHERE name = ?", a.Name).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve account id %q: %w", a.Name, err)
	}
	a.ID = id
	return id, nil
}

// SetAccountStatus updates the account status (active|suspended).
func (db *DB) SetAccountStatus(ctx context.Context, id int64, status string) error {
	_, err := db.write.ExecContext(ctx,
		"UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?", status, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: set account %d status: %w", id, err)
	}
	return nil
}

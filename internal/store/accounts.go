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

// rowScanner is implemented by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

const accountCols = `id, name, tags_json, billing_mode, balance_micros, credit_limit_micros,
	low_balance_threshold_micros, price_overrides_json, markup_override_bp, markup_override_set,
	auto_suspend, auto_resume, dsh_enabled, dsh_tenant, dsh_disabled_at, inflight_policy_override,
	overdraft_limit_micros, status, note, created_at, updated_at,
	feishu_open_id, feishu_union_id, feishu_name, feishu_bound_at, feishu_bound_by`

func scanAccount(row rowScanner) (*domain.Account, error) {
	var (
		a                       domain.Account
		autoSuspend, autoResume int
		dshEnabled              int
		markupOverrideSet       int
		createdAt, updatedAt    int64
		dshDisabledAt           sql.NullInt64
		feishuBoundAt           sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.Name, &a.TagsJSON, &a.BillingMode, &a.BalanceMicros, &a.CreditLimitMicros,
		&a.LowBalanceThresholdMicros, &a.PriceOverridesJSON, &a.MarkupOverrideBP, &markupOverrideSet,
		&autoSuspend, &autoResume, &dshEnabled, &a.DshTenant, &dshDisabledAt, &a.InflightPolicyOverride,
		&a.OverdraftLimitMicros, &a.Status, &a.Note, &createdAt, &updatedAt,
		&a.FeishuOpenID, &a.FeishuUnionID, &a.FeishuName, &feishuBoundAt, &a.FeishuBoundBy); err != nil {
		return nil, err
	}
	a.MarkupOverrideSet = markupOverrideSet != 0
	a.AutoSuspend = autoSuspend != 0
	a.AutoResume = autoResume != 0
	a.DSHEnabled = dshEnabled != 0
	a.CreatedAt = timeFromUnix(createdAt)
	a.UpdatedAt = timeFromUnix(updatedAt)
	if dshDisabledAt.Valid {
		when := timeFromUnix(dshDisabledAt.Int64)
		a.DshDisabledAt = &when
	}
	if feishuBoundAt.Valid {
		boundAt := timeFromUnix(feishuBoundAt.Int64)
		a.FeishuBoundAt = &boundAt
	}
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
	// Normalizing here as well as at the API boundary keeps every caller that resolves
	// an account by name (keys, invoices, credit codes, --account) consistent with what
	// UpsertAccount stored.
	name, err := domain.NormalizeAccountName(name)
	if err != nil {
		return nil, err
	}
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
	if a == nil {
		return 0, domain.ErrInvalidRequest("account name is required")
	}
	// The store is the last gate before the row is written, so it normalizes too: a
	// caller that skipped the API validation would otherwise store a name (trailing
	// space, overlong, invalid UTF-8) that GetAccountByName can never match.
	name, err := domain.NormalizeAccountName(a.Name)
	if err != nil {
		return 0, err
	}
	a.Name = name
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
INSERT INTO accounts(name, tags_json, billing_mode, balance_micros, credit_limit_micros,
  low_balance_threshold_micros, price_overrides_json, markup_override_bp, markup_override_set,
  auto_suspend, auto_resume, dsh_enabled, dsh_tenant, inflight_policy_override,
  overdraft_limit_micros, status, note, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  tags_json = excluded.tags_json,
  billing_mode = excluded.billing_mode,
  credit_limit_micros = excluded.credit_limit_micros,
  low_balance_threshold_micros = excluded.low_balance_threshold_micros,
  price_overrides_json = excluded.price_overrides_json,
  markup_override_bp = excluded.markup_override_bp,
  markup_override_set = excluded.markup_override_set,
  auto_suspend = excluded.auto_suspend,
  auto_resume = excluded.auto_resume,
  dsh_enabled = excluded.dsh_enabled,
  dsh_tenant = excluded.dsh_tenant,
  inflight_policy_override = excluded.inflight_policy_override,
  overdraft_limit_micros = excluded.overdraft_limit_micros,
  status = excluded.status,
  note = excluded.note,
  updated_at = excluded.updated_at`,
		a.Name, a.TagsJSON, string(a.BillingMode), a.BalanceMicros, a.CreditLimitMicros,
		a.LowBalanceThresholdMicros, a.PriceOverridesJSON, a.MarkupOverrideBP,
		boolInt(a.MarkupOverrideSet), boolInt(a.AutoSuspend),
		boolInt(a.AutoResume), boolInt(a.DSHEnabled), a.DshTenant, a.InflightPolicyOverride,
		a.OverdraftLimitMicros, a.Status, a.Note,
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

// SetAccountDSHDisabledAt records (or clears) the moment an administrator explicitly turned
// DSH off for this account (M72).
//
// It is a separate statement because UpsertAccount deliberately does not list the column:
// every ordinary account write — a console edit, the directory sync, a billing update — must
// leave the administrator's decision alone. Passing nil clears it, which is what 启用 DSH does
// so that dshgw.auto_enable can take over again.
func (db *DB) SetAccountDSHDisabledAt(ctx context.Context, id int64, at *time.Time) error {
	var value any
	if at != nil && !at.IsZero() {
		value = unix(*at)
	}
	result, err := db.write.ExecContext(ctx, `UPDATE accounts SET dsh_disabled_at = ? WHERE id = ?`, value, id)
	if err != nil {
		return fmt.Errorf("store: set account %d dsh disabled_at: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set account %d dsh disabled_at: %w", id, err)
	}
	if affected == 0 {
		if _, err := db.GetAccount(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// BindAccountFeishu writes the account's Feishu identity (M70), replacing any previous one.
//
// It mirrors BindAPIKeyFeishu and exists for the same reason: the identity is a sync mapping
// owned by the directory sync, so it is written by one statement no other write path can
// touch — UpsertAccount deliberately does not list the feishu_* columns, which is what keeps
// a console edit from silently clearing the mapping. The unique index over
// feishu_open_id (empty values excluded, so unbound accounts never collide) makes
// "one Feishu person, one account" a database invariant.
func (db *DB) BindAccountFeishu(ctx context.Context, id int64, binding domain.FeishuBinding) error {
	if binding.OpenID == "" {
		return domain.ErrInvalidRequest("a Feishu binding requires an open_id")
	}
	boundAt := binding.BoundAt
	if boundAt.IsZero() {
		boundAt = time.Now().UTC()
	}
	result, err := db.write.ExecContext(ctx, `
UPDATE accounts SET feishu_open_id = ?, feishu_union_id = ?, feishu_name = ?,
  feishu_bound_at = ?, feishu_bound_by = ?
WHERE id = ?`,
		binding.OpenID, binding.UnionID, binding.Name, unix(boundAt), binding.BoundBy, id)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("this Feishu account is already bound to another account")
		}
		return fmt.Errorf("store: bind account %d to Feishu: %w", id, err)
	}
	// A binding that matched no row would otherwise look like a success, and the sync
	// would report a linked account that does not exist.
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: bind account %d to Feishu: %w", id, err)
	}
	if affected == 0 {
		if _, err := db.GetAccount(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// UnbindAccountFeishu clears the account's Feishu identity and reports whether anything
// changed, so the console can answer idempotently instead of guessing.
func (db *DB) UnbindAccountFeishu(ctx context.Context, id int64) (bool, error) {
	result, err := db.write.ExecContext(ctx, `
UPDATE accounts SET feishu_open_id = '', feishu_union_id = '', feishu_name = '',
  feishu_bound_at = NULL, feishu_bound_by = ''
WHERE id = ? AND feishu_open_id <> ''`, id)
	if err != nil {
		return false, fmt.Errorf("store: unbind account %d from Feishu: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unbind account %d from Feishu: %w", id, err)
	}
	return affected > 0, nil
}

// FindAccountByFeishuOpenID resolves a bound Feishu identity to its account. A missing row
// is (nil, nil): an unlinked person is the ordinary answer while merging a directory, not
// an error worth a log line.
func (db *DB) FindAccountByFeishuOpenID(ctx context.Context, openID string) (*domain.Account, error) {
	if strings.TrimSpace(openID) == "" {
		return nil, nil
	}
	row := db.read.QueryRowContext(ctx, "SELECT "+accountCols+" FROM accounts WHERE feishu_open_id = ?", openID)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find account by Feishu open id: %w", err)
	}
	return a, nil
}

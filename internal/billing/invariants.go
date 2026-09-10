package billing

import (
	"context"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// InvariantStore is the read-only view the audit needs.
type InvariantStore interface {
	ListAccounts(ctx context.Context) ([]*domain.Account, error)
	LedgerTotals(ctx context.Context) (map[int64]int64, map[string]int64, error)
	LastLedgerBalances(ctx context.Context) (map[int64]int64, error)
	UsageCharges(ctx context.Context) (map[int64]int64, int64, error)
}

// Mismatch is one account that violates an invariant.
type Mismatch struct {
	AccountID int64  `json:"account_id"`
	Account   string `json:"account,omitempty"`
	Expected  int64  `json:"expected"`
	Actual    int64  `json:"actual"`
	Detail    string `json:"detail,omitempty"`
}

// InvariantReport is the result of one audit pass.
type InvariantReport struct {
	CheckedAt time.Time `json:"checked_at"`
	Accounts  int       `json:"accounts"`
	// BalanceSumMismatch: balance != sum(ledger amounts).
	BalanceSumMismatch []Mismatch `json:"balance_sum_mismatch"`
	// LastEntryMismatch: the newest ledger row's balance_after != the account balance.
	LastEntryMismatch []Mismatch `json:"last_entry_mismatch"`
	// ChargeMismatch: sum of charge ledger entries != sum of usage charges.
	ChargeMismatch []Mismatch `json:"charge_mismatch"`
	// NegativeBalance: a prepaid account below zero.
	NegativeBalance []Mismatch `json:"negative_balance"`
	OK              bool       `json:"ok"`
}

// CheckInvariants verifies the four invariants from docs/billing.md section 3.
func CheckInvariants(ctx context.Context, store InvariantStore, now time.Time) (*InvariantReport, error) {
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	sums, byKind, err := store.LedgerTotals(ctx)
	if err != nil {
		return nil, err
	}
	lastBalances, err := store.LastLedgerBalances(ctx)
	if err != nil {
		return nil, err
	}
	usageByAccount, usageTotal, err := store.UsageCharges(ctx)
	if err != nil {
		return nil, err
	}

	report := &InvariantReport{
		CheckedAt:          now.UTC(),
		Accounts:           len(accounts),
		BalanceSumMismatch: []Mismatch{},
		LastEntryMismatch:  []Mismatch{},
		ChargeMismatch:     []Mismatch{},
		NegativeBalance:    []Mismatch{},
	}

	var ledgerChargeTotal int64
	for _, account := range accounts {
		name := account.Name
		if sum := sums[account.ID]; sum != account.BalanceMicros {
			report.BalanceSumMismatch = append(report.BalanceSumMismatch, Mismatch{
				AccountID: account.ID, Account: name,
				Expected: sum, Actual: account.BalanceMicros,
				Detail: "sum(ledger.amount) must equal accounts.balance_micros",
			})
		}
		if last, ok := lastBalances[account.ID]; !ok {
			if account.BalanceMicros != 0 {
				report.LastEntryMismatch = append(report.LastEntryMismatch, Mismatch{
					AccountID: account.ID, Account: name, Expected: 0,
					Actual: account.BalanceMicros, Detail: "account has a balance but no ledger entries",
				})
			}
		} else if last != account.BalanceMicros {
			report.LastEntryMismatch = append(report.LastEntryMismatch, Mismatch{
				AccountID: account.ID, Account: name,
				Expected: last, Actual: account.BalanceMicros,
				Detail: "the newest ledger entry must carry the current balance",
			})
		}
		if account.BillingMode == domain.BillingPrepaid && account.BalanceMicros < 0 {
			report.NegativeBalance = append(report.NegativeBalance, Mismatch{
				AccountID: account.ID, Account: name, Expected: 0, Actual: account.BalanceMicros,
				Detail: "prepaid balances must never go negative (overshoot is absorbed as cost)",
			})
		}
	}
	ledgerChargeTotal = byKind["charge"]
	if -ledgerChargeTotal != usageTotal {
		report.ChargeMismatch = append(report.ChargeMismatch, Mismatch{
			Expected: -ledgerChargeTotal, Actual: usageTotal,
			Detail: "sum(charge ledger entries) must equal sum(usage.charge_micros)",
		})
	}
	for accountID, usageSum := range usageByAccount {
		if _, ok := sums[accountID]; !ok && usageSum != 0 {
			report.ChargeMismatch = append(report.ChargeMismatch, Mismatch{
				AccountID: accountID, Expected: 0, Actual: usageSum,
				Detail: "usage exists for an account with no ledger entries",
			})
		}
	}

	report.OK = len(report.BalanceSumMismatch) == 0 && len(report.LastEntryMismatch) == 0 &&
		len(report.ChargeMismatch) == 0 && len(report.NegativeBalance) == 0
	return report, nil
}

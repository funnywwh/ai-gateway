package billing

import (
	"context"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

// InvariantStore is the read-only view the audit needs. Reading it must happen in one
// consistent snapshot: on a busy gateway, summing usage and ledger with separate
// queries reports a mismatch whenever the settlement writer commits in between.
type InvariantStore interface {
	BillingAuditSnapshot(ctx context.Context) (*store.BillingAudit, error)
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
//
// Every aggregate comes from one consistent read snapshot, so a concurrent settlement
// can never make the sums disagree by accident.
func CheckInvariants(ctx context.Context, store InvariantStore, now time.Time) (*InvariantReport, error) {
	audit, err := store.BillingAuditSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	report := &InvariantReport{
		CheckedAt:          now.UTC(),
		Accounts:           len(audit.BalanceByAccount),
		BalanceSumMismatch: []Mismatch{},
		LastEntryMismatch:  []Mismatch{},
		ChargeMismatch:     []Mismatch{},
		NegativeBalance:    []Mismatch{},
	}
	for accountID, balance := range audit.BalanceByAccount {
		name := audit.NameByAccount[accountID]
		if sum := audit.LedgerSumByAccount[accountID]; sum != balance {
			report.BalanceSumMismatch = append(report.BalanceSumMismatch, Mismatch{
				AccountID: accountID, Account: name,
				Expected: sum, Actual: balance,
				Detail: "sum(ledger.amount) must equal accounts.balance_micros",
			})
		}
		last, ok := audit.LastBalanceByAccount[accountID]
		switch {
		case !ok:
			if balance != 0 {
				report.LastEntryMismatch = append(report.LastEntryMismatch, Mismatch{
					AccountID: accountID, Account: name, Expected: 0, Actual: balance,
					Detail: "account has a balance but no ledger entries",
				})
			}
		case last != balance:
			report.LastEntryMismatch = append(report.LastEntryMismatch, Mismatch{
				AccountID: accountID, Account: name, Expected: last, Actual: balance,
				Detail: "the newest ledger entry must carry the current balance",
			})
		}
		if audit.BillingModeByAccount[accountID] == string(domain.BillingPrepaid) && balance < 0 {
			report.NegativeBalance = append(report.NegativeBalance, Mismatch{
				AccountID: accountID, Account: name, Expected: 0, Actual: balance,
				Detail: "prepaid balances must never go negative (overshoot is absorbed as cost)",
			})
		}
	}
	if ledgerCharge := audit.LedgerSumByKind["charge"]; -ledgerCharge != audit.UsageChargeTotal {
		report.ChargeMismatch = append(report.ChargeMismatch, Mismatch{
			Expected: -ledgerCharge, Actual: audit.UsageChargeTotal,
			Detail: "sum(charge ledger entries) must equal sum(usage.charge_micros)",
		})
	}
	for accountID, usageSum := range audit.UsageChargeByAccount {
		if _, ok := audit.BalanceByAccount[accountID]; !ok && usageSum != 0 {
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

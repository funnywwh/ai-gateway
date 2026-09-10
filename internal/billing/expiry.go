package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// ExpiryStore is the persistence the gift-credit expiry job needs.
type ExpiryStore interface {
	ListExpiringGrants(ctx context.Context, now time.Time, limit int) ([]*domain.LedgerEntry, error)
	GetBalance(ctx context.Context, accountID int64) (int64, error)
	AppendLedger(ctx context.Context, entries []*domain.LedgerEntry) (int, error)
}

// ExpiryResult summarises one expiry pass.
type ExpiryResult struct {
	Scanned int   `json:"scanned"`
	Expired int   `json:"expired"`
	Skipped int   `json:"skipped"`
	Micros  int64 `json:"expired_micros"`
}

// ExpireGiftCredit writes off gift credit that matured without being used.
//
// Attribution model, stated plainly because it is a policy choice and not an
// observable fact: paid top-ups are treated as consumed LAST, so the earliest-expiring
// gift credit is spent first. Each matured grant therefore expires
// min(grant amount, what is still on the account), walking grants in expiry order and
// never taking the balance below zero. A grant that found no remaining balance expires
// with a zero-amount entry, which still records the decision and keeps the job
// idempotent.
//
// Idempotency: every write-off is keyed expire:<grant idem key>, so running the job
// twice (or after a restart) cannot double-expire.
func (s *Service) ExpireGiftCredit(ctx context.Context, now time.Time, limit int) (ExpiryResult, error) {
	result := ExpiryResult{}
	grants, err := s.store.ListExpiringGrants(ctx, now, limit)
	if err != nil {
		return result, err
	}
	result.Scanned = len(grants)
	if len(grants) == 0 {
		return result, nil
	}

	byAccount := map[int64][]*domain.LedgerEntry{}
	for _, grant := range grants {
		byAccount[grant.AccountID] = append(byAccount[grant.AccountID], grant)
	}
	accounts := make([]int64, 0, len(byAccount))
	for accountID := range byAccount {
		accounts = append(accounts, accountID)
	}
	sortInt64(accounts)

	for _, accountID := range accounts {
		available, err := s.store.GetBalance(ctx, accountID)
		if err != nil {
			return result, err
		}
		if available < 0 {
			available = 0
		}
		pending := make([]*domain.LedgerEntry, 0, len(byAccount[accountID]))
		for _, grant := range byAccount[accountID] {
			amount := grant.AmountMicros
			if amount > available {
				amount = available
			}
			if amount < 0 {
				amount = 0
			}
			available -= amount
			pending = append(pending, &domain.LedgerEntry{
				AccountID:    accountID,
				Kind:         "expire",
				AmountMicros: -amount,
				RefType:      "credit_grant",
				RefID:        grant.IdemKey,
				IdemKey:      "expire:" + grant.IdemKey,
				Note:         fmt.Sprintf("gift credit of %d micros matured on %s", grant.AmountMicros, grant.ExpiresAt.UTC().Format(time.RFC3339)),
				Actor:        "system:expiry",
				CreatedAt:    now.UTC(),
			})
		}
		applied, err := s.store.AppendLedger(ctx, pending)
		if err != nil {
			return result, err
		}
		if applied == 0 {
			// Every entry already existed: this pass re-scanned rows an earlier run handled.
			result.Skipped += len(pending)
			continue
		}
		result.Expired += applied
		for _, entry := range pending {
			result.Micros += -entry.AmountMicros
		}
	}
	if result.Expired > 0 {
		s.log.Info("gift credit expired",
			"grants", result.Expired, "micros", result.Micros, "accounts", len(accounts))
	}
	return result, nil
}

// StartExpiryJob runs the expiry pass at startup and then daily until the context is
// cancelled. Expiry is a daily policy, not a real-time reaction.
func (s *Service) StartExpiryJob(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			result, err := s.ExpireGiftCredit(ctx, time.Now().UTC(), 500)
			if err != nil {
				s.log.Error("expiring gift credit failed", "err", err)
			} else if result.Expired > 0 || result.Skipped > 0 {
				s.log.Info("gift credit expiry pass finished",
					"scanned", result.Scanned, "expired", result.Expired,
					"skipped", result.Skipped, "micros", result.Micros)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

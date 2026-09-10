package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/secret"
)

// Credit kinds that may be written by hand or by an external payment integration.
var creditKinds = map[string]bool{
	"topup": true, "credit_grant": true, "adjustment": true, "refund": true, "expire": true,
}

// CreditRequest is one manual money movement.
type CreditRequest struct {
	AccountID    int64
	Kind         string
	AmountMicros int64
	// RefID is the external reference (payment id, ticket id, ...) and doubles as the
	// idempotency key, so a retried webhook cannot double-credit an account.
	RefID string
	Note  string
	Actor string
	// ExpiresAt applies to credit_grant: unused gift credit is expiring later.
	ExpiresAt *time.Time
}

// Grant writes one credit (or debit, for a negative adjustment) and resumes the
// account if it was auto-suspended and policy allows it. It reports whether the entry
// was newly applied; a replay returns false.
func (s *Service) Grant(ctx context.Context, req CreditRequest) (*domain.LedgerEntry, bool, error) {
	if req.AccountID == 0 {
		return nil, false, domain.ErrInvalidRequest("account_id is required")
	}
	if !creditKinds[req.Kind] {
		return nil, false, domain.ErrInvalidRequest("kind must be topup, credit_grant, adjustment, refund or expire")
	}
	if req.AmountMicros == 0 {
		return nil, false, domain.ErrInvalidRequest("amount_micros must not be zero")
	}
	if req.AmountMicros < 0 && req.Kind != "adjustment" && req.Kind != "expire" {
		return nil, false, domain.ErrInvalidRequest("only an adjustment or an expiry may be negative")
	}
	if strings.TrimSpace(req.RefID) == "" {
		return nil, false, domain.ErrInvalidRequest("ref_id is required: it is the idempotency key of the movement")
	}
	account, err := s.store.GetAccount(ctx, req.AccountID)
	if err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	entry := &domain.LedgerEntry{
		AccountID: req.AccountID, Kind: req.Kind, AmountMicros: req.AmountMicros,
		RefType: "credit", RefID: req.RefID,
		IdemKey: fmt.Sprintf("%s:%s", req.Kind, req.RefID),
		Note:    req.Note, Actor: req.Actor, CreatedAt: now,
	}
	n, err := s.store.AppendLedger(ctx, []*domain.LedgerEntry{entry})
	if err != nil {
		return nil, false, err
	}
	applied := n > 0

	// Auto-resume: a suspended account that can pay again comes back automatically.
	if account.Status == "suspended" && account.AutoResume {
		if balance, err := s.store.GetBalance(ctx, req.AccountID); err == nil && balance > 0 {
			if err := s.store.SetAccountStatus(ctx, req.AccountID, "active"); err != nil {
				s.log.Warn("auto-resuming the account failed", "account", req.AccountID, "err", err)
			} else {
				s.log.Info("account resumed after a credit", "account", account.Name, "balance", balance)
			}
		}
	}
	return entry, applied, nil
}

// CodeBatchRequest describes a batch of redemption codes.
type CodeBatchRequest struct {
	Count        int
	AmountMicros int64
	ExpiresAt    *time.Time
	BatchID      string
	Note         string
	Actor        string
}

// GenerateCodes creates a batch of redemption codes and returns the plaintext exactly
// once. Only the hashes are stored, so a database leak cannot be redeemed.
func (s *Service) GenerateCodes(ctx context.Context, req CodeBatchRequest) ([]string, error) {
	if req.Count <= 0 || req.Count > 1000 {
		return nil, domain.ErrInvalidRequest("count must be between 1 and 1000")
	}
	if req.AmountMicros <= 0 {
		return nil, domain.ErrInvalidRequest("amount_micros must be positive")
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now().UTC()) {
		return nil, domain.ErrInvalidRequest("expires_at must be in the future")
	}
	batchID := strings.TrimSpace(req.BatchID)
	if batchID == "" {
		batchID = ids.New("rcb")
	}

	plaintext := make([]string, 0, req.Count)
	codes := make([]*domain.RedemptionCode, 0, req.Count)
	for index := 0; index < req.Count; index++ {
		code := ids.RedemptionCode()
		plaintext = append(plaintext, code)
		codes = append(codes, &domain.RedemptionCode{
			CodeHash: secret.Hash(code), AmountMicros: req.AmountMicros,
			ExpiresAt: req.ExpiresAt, BatchID: batchID, CreatedBy: req.Actor, Note: req.Note,
		})
	}
	if err := s.store.InsertRedemptionCodes(ctx, codes); err != nil {
		return nil, err
	}
	return plaintext, nil
}

// RedeemCode claims a code for an account and credits it. The claim is a conditional
// update, so two concurrent redemptions of the same code cannot both succeed; if the
// credit itself fails the claim is released so the customer can try again.
func (s *Service) RedeemCode(ctx context.Context, code string, accountID int64, actor string) (*domain.RedemptionCode, *domain.LedgerEntry, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return nil, nil, domain.ErrInvalidRequest("code is required")
	}
	if accountID == 0 {
		return nil, nil, domain.ErrInvalidRequest("account_id is required")
	}
	hash := secret.Hash(trimmed)
	now := time.Now().UTC()

	record, err := s.store.RedeemCode(ctx, hash, accountID, now)
	if err != nil {
		return nil, nil, err
	}
	entry, _, err := s.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "credit_grant", AmountMicros: record.AmountMicros,
		RefID: "code:" + hash[:16], Note: "redemption code", Actor: actor,
		ExpiresAt: record.ExpiresAt,
	})
	if err != nil {
		// Do not leave a code consumed without its money.
		if releaseErr := s.store.ReleaseRedemptionCode(ctx, hash); releaseErr != nil {
			s.log.Error("releasing a code after a failed credit failed",
				"err", releaseErr, "account", accountID)
		}
		return nil, nil, err
	}
	return record, entry, nil
}

// ExpireGiftCredit writes the offsetting entry for a matured gift grant.
func (s *Service) ExpireGiftCredit(ctx context.Context, accountID int64, amount int64, refID, actor string) (*domain.LedgerEntry, error) {
	return s.expire(ctx, accountID, amount, refID, actor)
}

func (s *Service) expire(ctx context.Context, accountID int64, amount int64, refID, actor string) (*domain.LedgerEntry, error) {
	entry, _, err := s.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "expire", AmountMicros: -amount,
		RefID: refID, Note: "gift credit expired", Actor: actor,
	})
	return entry, err
}

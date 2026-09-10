package billing

import (
	"context"

	"github.com/winger/ai-gateway/internal/domain"
)

// Invoices lists the invoices of one account (0 means every account).
func (s *Service) Invoices(ctx context.Context, accountID int64, limit int) ([]*domain.Invoice, error) {
	return s.store.ListInvoices(ctx, accountID, limit)
}

// Invoice loads one invoice with its lines.
func (s *Service) Invoice(ctx context.Context, id int64) (*domain.Invoice, error) {
	return s.store.GetInvoice(ctx, id)
}

// Codes lists redemption codes, optionally restricted to one batch.
func (s *Service) Codes(ctx context.Context, batchID string, limit int) ([]*domain.RedemptionCode, error) {
	return s.store.ListRedemptionCodes(ctx, batchID, limit)
}

// Reconciliations lists recent reconciliation runs.
func (s *Service) Reconciliations(ctx context.Context, limit int) ([]*domain.Reconciliation, error) {
	return s.store.ListReconciliations(ctx, limit)
}

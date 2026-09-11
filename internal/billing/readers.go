package billing

import (
	"context"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

// PageStore is the windowed read side the management console pages through. It is a
// sub-port of ServiceStore next to Batching/InvariantStore/RebuildStore: the reads
// themselves are pure passthroughs, and keeping them here means the HTTP layer never
// touches the store for billing history (docs/design/m24-console-pagination.md).
type PageStore interface {
	ListLedgerPage(ctx context.Context, w store.LedgerWindow) ([]*domain.LedgerEntry, error)
	CountLedger(ctx context.Context, w store.LedgerWindow) (int, error)
	ListInvoicesPage(ctx context.Context, accountID int64, limit, offset int) ([]*domain.Invoice, error)
	CountInvoices(ctx context.Context, accountID int64) (int, error)
	ListRedemptionCodesPage(ctx context.Context, batchID string, limit, offset int) ([]*domain.RedemptionCode, error)
	CountRedemptionCodes(ctx context.Context, batchID string) (int, error)
	ListReconciliationsPage(ctx context.Context, limit, offset int) ([]*domain.Reconciliation, error)
	CountReconciliations(ctx context.Context) (int, error)
}

// Invoices lists the invoices of one account (0 means every account).
func (s *Service) Invoices(ctx context.Context, accountID int64, limit int) ([]*domain.Invoice, error) {
	return s.store.ListInvoices(ctx, accountID, limit)
}

// InvoicesPage returns one page of invoices; CountInvoices counts every invoice the
// same account filter selects, which is what the console's pager shows as 共 N 条.
func (s *Service) InvoicesPage(ctx context.Context, accountID int64, limit, offset int) ([]*domain.Invoice, error) {
	return s.store.ListInvoicesPage(ctx, accountID, limit, offset)
}

// CountInvoices counts the invoices of one account (0 means every account).
func (s *Service) CountInvoices(ctx context.Context, accountID int64) (int, error) {
	return s.store.CountInvoices(ctx, accountID)
}

// Invoice loads one invoice with its lines.
func (s *Service) Invoice(ctx context.Context, id int64) (*domain.Invoice, error) {
	return s.store.GetInvoice(ctx, id)
}

// Codes lists redemption codes, optionally restricted to one batch.
func (s *Service) Codes(ctx context.Context, batchID string, limit int) ([]*domain.RedemptionCode, error) {
	return s.store.ListRedemptionCodes(ctx, batchID, limit)
}

// CodesPage returns one page of redemption codes.
func (s *Service) CodesPage(ctx context.Context, batchID string, limit, offset int) ([]*domain.RedemptionCode, error) {
	return s.store.ListRedemptionCodesPage(ctx, batchID, limit, offset)
}

// CountCodes counts the codes of one batch (empty means every batch).
func (s *Service) CountCodes(ctx context.Context, batchID string) (int, error) {
	return s.store.CountRedemptionCodes(ctx, batchID)
}

// Reconciliations lists recent reconciliation runs.
func (s *Service) Reconciliations(ctx context.Context, limit int) ([]*domain.Reconciliation, error) {
	return s.store.ListReconciliations(ctx, limit)
}

// ReconciliationsPage returns one page of reconciliation runs.
func (s *Service) ReconciliationsPage(ctx context.Context, limit, offset int) ([]*domain.Reconciliation, error) {
	return s.store.ListReconciliationsPage(ctx, limit, offset)
}

// CountReconciliations counts every reconciliation run.
func (s *Service) CountReconciliations(ctx context.Context) (int, error) {
	return s.store.CountReconciliations(ctx)
}

// LedgerPage returns one window of an account's ledger, optionally excluding kinds
// (the credits view excludes charges so its pages stay evenly sized).
func (s *Service) LedgerPage(ctx context.Context, w store.LedgerWindow) ([]*domain.LedgerEntry, error) {
	return s.store.ListLedgerPage(ctx, w)
}

// CountLedger counts the rows the same window selects.
func (s *Service) CountLedger(ctx context.Context, w store.LedgerWindow) (int, error) {
	return s.store.CountLedger(ctx, w)
}

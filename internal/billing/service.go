package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// ServiceStore is everything the billing service needs from persistence.
type ServiceStore interface {
	Batching
	InvariantStore
	RebuildStore
	GetBalance(ctx context.Context, accountID int64) (int64, error)
	ListLedger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.LedgerEntry, error)
}

// Service bundles the settlement writer, the in-flight reservation table and the
// maintenance operations (invariant audit, ledger rebuild). It is the single
// dependency the HTTP layer and the request path both use.
type Service struct {
	store        ServiceStore
	writer       *Writer
	reservations *ReservationTable
	log          *slog.Logger
}

// ServiceConfig configures the billing service.
type ServiceConfig struct {
	Writer         Config
	ReservationTTL time.Duration
	ReplayInterval time.Duration
}

// NewService builds the service and starts its writer and fallback replayer.
func NewService(ctx context.Context, store ServiceStore, cfg ServiceConfig, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	service := &Service{
		store:        store,
		writer:       NewWriter(cfg.Writer, store, log),
		reservations: NewReservationTable(cfg.ReservationTTL),
		log:          log,
	}
	service.writer.StartReplayer(ctx, cfg.ReplayInterval, log)
	return service
}

// Submit hands a settlement to the writer.
func (s *Service) Submit(settlement *Settlement) { s.writer.Submit(settlement) }

// Apply writes a settlement synchronously (used by tests and by the rebuild path).
func (s *Service) Apply(ctx context.Context, settlement *Settlement) (bool, error) {
	return s.store.SettleAttempt(ctx, settlement.Usage, settlement.Entries, settlement.Counters)
}

// Writer exposes the writer for stats and shutdown.
func (s *Service) Writer() *Writer { return s.writer }

// Stats reports writer counters.
func (s *Service) Stats() Stats { return s.writer.Stats() }

// Reservations lists the in-flight holds (for the console).
func (s *Service) Reservations() []Reservation { return s.reservations.Snapshot() }

// Invariants runs the four invariants from docs/billing.md section 3.
func (s *Service) Invariants(ctx context.Context) (*InvariantReport, error) {
	return CheckInvariants(ctx, s.store, time.Now())
}

// Rebuild replays one account's charges from its usage rows.
func (s *Service) Rebuild(ctx context.Context, accountID int64, apply bool) (*RebuildPlan, error) {
	return RebuildAccount(ctx, s.store, accountID, apply, time.Now())
}

// Balance returns an account's materialised balance.
func (s *Service) Balance(ctx context.Context, accountID int64) (int64, error) {
	return s.store.GetBalance(ctx, accountID)
}

// Ledger lists an account's ledger entries inside a window.
func (s *Service) Ledger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.LedgerEntry, error) {
	return s.store.ListLedger(ctx, accountID, from, to, limit)
}

// Admit reserves against an account's balance. It returns the decision plus, when
// admitted, the reservation to release once the attempt settles.
func (s *Service) Admit(account *domain.Account, requestID string, estimate EstimateInput, policy string, now time.Time) (Admission, *Reservation) {
	reserve := EstimateReserve(estimate)
	inFlight := s.reservations.InFlight(account.ID)
	decision := DecideAdmission(AdmissionInput{
		Account: account, Reserve: reserve, InFlight: inFlight, Policy: policy,
	})
	if !decision.Allowed {
		return decision, nil
	}
	reservation := s.reservations.Reserve(requestID, account.ID, reserve, now)
	return decision, reservation
}

// Release drops a reservation after settlement.
func (s *Service) Release(reservation *Reservation) {
	if reservation != nil {
		s.reservations.Release(reservation.ID, reservation.AccountID)
	}
}

// Touch extends a reservation that is still running.
func (s *Service) Touch(reservation *Reservation, now time.Time) {
	if reservation != nil {
		s.reservations.Touch(reservation.ID, reservation.AccountID, now)
	}
}

// StartReservationGC reaps expired holds until the context is cancelled.
func (s *Service) StartReservationGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if removed := s.reservations.GC(now); removed > 0 {
					s.log.Warn("expired in-flight reservations were released", "count", removed)
				}
			}
		}
	}()
}

// Close drains the writer, returning the number of settlements that could not be
// persisted in time.
func (s *Service) Close(timeout time.Duration) int { return s.writer.Close(timeout) }

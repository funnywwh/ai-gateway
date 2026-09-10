package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// LedgerAdmin reads balances and ledger history.
type LedgerAdmin interface {
	Balance(ctx context.Context, accountID int64) (int64, error)
	Ledger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.LedgerEntry, error)
}

func (s *Server) handleAdminInvariants(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	service, ok := portReady(w, s.deps.Billing, "billing")
	if !ok {
		return
	}
	report, err := service.Invariants(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleAdminBillingStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	service, ok := portReady(w, s.deps.Billing, "billing")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"writer":       service.Stats(),
		"reservations": service.Reservations(),
	})
}

// handleAdminRebuildLedger replays one account's charges from its usage rows. It
// defaults to a dry run: applying a rebuild rewrites the whole ledger of the account
// and must be asked for explicitly with "apply": true.
func (s *Server) handleAdminRebuildLedger(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	service, ok := portReady(w, s.deps.Billing, "billing")
	if !ok {
		return
	}
	var body struct {
		AccountID int64  `json:"account_id"`
		Account   string `json:"account"`
		Apply     bool   `json:"apply"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	accountID := body.AccountID
	if accountID == 0 && body.Account != "" {
		accounts, ok := portReady(w, s.deps.Accounts, "account management")
		if !ok {
			return
		}
		account, err := accounts.GetAccountByName(r.Context(), body.Account)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		accountID = account.ID
	}
	if accountID == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("account_id or account is required"))
		return
	}
	plan, err := service.Rebuild(r.Context(), accountID, body.Apply)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	result := "ok"
	if !body.Apply {
		result = "dry_run"
	}
	s.audit(r.Context(), actor.Username, "rebuild", "ledger", strconv.FormatInt(accountID, 10),
		map[string]any{"apply": body.Apply, "attempts": plan.Attempts, "diff_micros": plan.DiffMicros}, result)
	if body.Apply {
		s.reload(r.Context(), "ledger rebuilt", true)
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleAdminAccountBalance(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	ledger, ok := portReady(w, s.deps.Ledger, "ledger queries")
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	balance, err := ledger.Balance(r.Context(), accountID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	payload := map[string]any{"account_id": accountID, "balance_micros": balance}
	if s.deps.Billing != nil {
		for _, reservation := range s.deps.Billing.Reservations() {
			if reservation.AccountID == accountID {
				payload["in_flight_micros"] = toInt64(payload["in_flight_micros"]) + reservation.AmountMicros
			}
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAdminAccountLedger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	ledger, ok := portReady(w, s.deps.Ledger, "ledger queries")
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	from, to := adminWindow(r)
	entries, err := ledger.Ledger(r.Context(), accountID, from, to, adminLimit(r, 100, 1000))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		payload := map[string]any{
			"id": entry.ID, "kind": entry.Kind, "amount_micros": entry.AmountMicros,
			"balance_after_micros": entry.BalanceAfterMicros,
			"ref_type":             entry.RefType, "ref_id": entry.RefID,
			"idem_key": entry.IdemKey, "rebuild_seq": entry.RebuildSeq,
			"note": entry.Note, "actor": entry.Actor,
			"created_at": entry.CreatedAt.UTC().Format(time.RFC3339),
		}
		if entry.APIKeyID != nil {
			payload["api_key_id"] = *entry.APIKeyID
		}
		out = append(out, payload)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func toInt64(value any) int64 {
	if number, ok := value.(int64); ok {
		return number
	}
	return 0
}

// handleAdminExpireCredit runs the gift-credit expiry pass on demand. The same job
// runs at startup and daily; this endpoint exists so an operator can see the outcome
// without waiting for the schedule.
func (s *Server) handleAdminExpireCredit(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	service, ok := portReady(w, s.deps.Billing, "billing")
	if !ok {
		return
	}
	result, err := service.ExpireGiftCredit(r.Context(), time.Now().UTC(), 500)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "expire", "credit_grant", "", map[string]any{
		"scanned": result.Scanned, "expired": result.Expired, "micros": result.Micros,
	}, "ok")
	if result.Expired > 0 {
		s.reload(r.Context(), "gift credit expired", true)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": result.Scanned, "expired": result.Expired,
		"skipped": result.Skipped, "expired_micros": result.Micros,
	})
}

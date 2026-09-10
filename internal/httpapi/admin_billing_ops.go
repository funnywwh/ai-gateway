package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/domain"
)

// InvoiceAdmin is the invoicing and credit surface the management API needs.
type InvoiceAdmin interface {
	BuildInvoice(ctx context.Context, accountID int64, start, end time.Time, groupBy string, replace bool, currency, note string) (*domain.Invoice, bool, error)
	InvoiceAction(ctx context.Context, id int64, action, actor string) (*domain.Invoice, error)
	Invoices(ctx context.Context, accountID int64, limit int) ([]*domain.Invoice, error)
	Invoice(ctx context.Context, id int64) (*domain.Invoice, error)
	Grant(ctx context.Context, req billing.CreditRequest) (*domain.LedgerEntry, bool, error)
	GenerateCodes(ctx context.Context, req billing.CodeBatchRequest) ([]string, error)
}

// ReconciliationAdmin exposes reconciliation runs and failure replay.
type ReconciliationAdmin interface {
	Reconcile(ctx context.Context, req billing.ReconcileRequest) (*domain.Reconciliation, error)
	Reconciliations(ctx context.Context, limit int) ([]*domain.Reconciliation, error)
	ReplayFailures(ctx context.Context, limit int) (billing.ReplayResult, error)
}

// RedemptionAdmin reads and redeems codes.
type RedemptionAdmin interface {
	Codes(ctx context.Context, batchID string, limit int) ([]*domain.RedemptionCode, error)
	RedeemCode(ctx context.Context, code string, accountID int64, actor string) (*domain.RedemptionCode, *domain.LedgerEntry, error)
}

func (s *Server) handleAdminListInvoices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Invoices, "invoicing")
	if !ok {
		return
	}
	accountID := int64(0)
	if raw := r.PathValue("id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
			return
		}
		accountID = parsed
	}
	invoices, err := store.Invoices(r.Context(), accountID, adminLimit(r, 50, 200))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(invoices))
	for _, invoice := range invoices {
		out = append(out, invoiceJSON(invoice, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (s *Server) handleAdminBuildInvoice(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Invoices, "invoicing")
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	var body struct {
		Period      string `json:"period"`
		GroupBy     string `json:"group_by"`
		Force       bool   `json:"force"`
		Note        string `json:"note"`
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}

	periodCfg := billing.PeriodConfig{
		StartDay: s.deps.Config.Billing.PeriodStartDay,
		Timezone: s.deps.Config.Billing.Timezone,
	}
	at := time.Now().UTC()
	switch body.Period {
	case "", "current":
	case "previous":
		at = at.AddDate(0, -1, 0)
	case "last30":
		start := at.AddDate(0, 0, -30)
		periodCfg.StartDay = 0
		periodCfg.Timezone = "UTC"
		periodCfg = billing.PeriodConfig{StartDay: start.Day(), Timezone: "UTC"}
	default:
		writeAPIError(w, domain.ErrInvalidRequest("period must be current, previous or last30"))
		return
	}
	start, end, err := billing.PeriodFor(at, periodCfg)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if body.PeriodStart != "" {
		parsed, err := time.Parse(time.RFC3339, body.PeriodStart)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("period_start must be RFC3339"))
			return
		}
		start = parsed.UTC()
	}
	if body.PeriodEnd != "" {
		parsed, err := time.Parse(time.RFC3339, body.PeriodEnd)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("period_end must be RFC3339"))
			return
		}
		end = parsed.UTC()
	}

	groupBy := body.GroupBy
	if groupBy == "" {
		groupBy = s.deps.Config.Billing.InvoicePeriod
	}
	if groupBy != "model" && groupBy != "key" && groupBy != "day" {
		groupBy = "model"
	}
	currency := "USD"
	if s.deps.Config != nil && s.deps.Config.Billing.Currency != "" {
		currency = s.deps.Config.Billing.Currency
	}
	invoice, created, err := store.BuildInvoice(r.Context(), accountID, start, end, groupBy, body.Force, currency, body.Note)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	action := "update"
	status := http.StatusOK
	if created {
		action = "create"
		status = http.StatusCreated
	}
	s.audit(r.Context(), actor.Username, action, "invoice", strconv.FormatInt(invoice.ID, 10), map[string]any{
		"account_id": accountID, "group_by": groupBy, "force": body.Force,
		"charge_micros": invoice.TotalChargeMicros,
	}, "ok")
	writeJSON(w, status, invoiceJSON(invoice, true))
}

func (s *Server) handleAdminGetInvoice(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Invoices, "invoicing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid invoice id"))
		return
	}
	invoice, err := store.Invoice(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "csv") {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=invoice-"+strconv.FormatInt(id, 10)+".csv")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(billing.InvoiceCSV(invoice)))
		return
	}
	writeJSON(w, http.StatusOK, invoiceJSON(invoice, true))
}

func (s *Server) handleAdminInvoiceAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Invoices, "invoicing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid invoice id"))
		return
	}
	action := r.PathValue("action")
	invoice, err := store.InvoiceAction(r.Context(), id, action, actor.Username)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, action, "invoice", strconv.FormatInt(id, 10),
		map[string]any{"status": invoice.Status}, "ok")
	if action != "issue" {
		s.reload(r.Context(), "invoice "+action, true)
	}
	writeJSON(w, http.StatusOK, invoiceJSON(invoice, true))
}

func invoiceJSON(invoice *domain.Invoice, withLines bool) map[string]any {
	payload := map[string]any{
		"id": invoice.ID, "account_id": invoice.AccountID,
		"period_start": invoice.PeriodStart.UTC().Format(time.RFC3339),
		"period_end":   invoice.PeriodEnd.UTC().Format(time.RFC3339),
		"status":       invoice.Status, "currency": invoice.Currency,
		"total_cost_micros": invoice.TotalCostMicros, "total_charge_micros": invoice.TotalChargeMicros,
		"note": invoice.Note, "created_at": invoice.CreatedAt.UTC().Format(time.RFC3339),
		"issued_at": timeOrNil(invoice.IssuedAt), "paid_at": timeOrNil(invoice.PaidAt),
		"voided_at": timeOrNil(invoice.VoidedAt),
	}
	if withLines {
		lines := make([]map[string]any, 0, len(invoice.Lines))
		for _, line := range invoice.Lines {
			lines = append(lines, map[string]any{
				"group_type": line.GroupType, "group_key": line.GroupKey,
				"requests": line.Requests, "prompt_tokens": line.PromptTokens,
				"completion_tokens": line.CompletionTokens,
				"cost_micros":       line.CostMicros, "charge_micros": line.ChargeMicros,
			})
		}
		payload["lines"] = lines
	}
	return payload
}

func (s *Server) handleAdminAccountCredits(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Invoices, "invoicing")
	if !ok {
		return
	}
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	var body struct {
		Kind         string `json:"kind"`
		AmountMicros int64  `json:"amount_micros"`
		AmountUSD    string `json:"amount_usd"`
		RefID        string `json:"ref_id"`
		Note         string `json:"note"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	kind := body.Kind
	if kind == "" {
		kind = "topup"
	}
	amount := body.AmountMicros
	if amount == 0 && body.AmountUSD != "" {
		parsed, err := parseUSDToMicros(body.AmountUSD)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
			return
		}
		amount = parsed
	}
	var expiresAt *time.Time
	if body.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("expires_at must be RFC3339"))
			return
		}
		expiresAt = &parsed
	}
	entry, applied, err := store.Grant(r.Context(), billing.CreditRequest{
		AccountID: accountID, Kind: kind, AmountMicros: amount,
		RefID: body.RefID, Note: body.Note, Actor: actor.Username, ExpiresAt: expiresAt,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "credit", "account", strconv.FormatInt(accountID, 10),
		map[string]any{"kind": kind, "amount_micros": amount, "ref_id": body.RefID, "applied": applied}, "ok")
	s.reload(r.Context(), "credit granted", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": accountID, "kind": entry.Kind, "amount_micros": entry.AmountMicros,
		"balance_after_micros": entry.BalanceAfterMicros, "idem_key": entry.IdemKey,
		"applied": applied,
	})
}

// parseUSDToMicros converts a decimal dollar amount to micro-USD without floats.
func parseUSDToMicros(value string) (int64, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "$"))
	if trimmed == "" {
		return 0, nil
	}
	negative := strings.HasPrefix(trimmed, "-")
	trimmed = strings.TrimPrefix(trimmed, "-")
	parts := strings.SplitN(trimmed, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, domain2Err("amount_usd is not a number")
	}
	fraction := int64(0)
	if len(parts) == 2 {
		digits := parts[1]
		if len(digits) > 6 {
			digits = digits[:6]
		}
		for len(digits) < 6 {
			digits += "0"
		}
		fraction, err = strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return 0, domain2Err("amount_usd has an invalid fraction")
		}
	}
	micros := whole*1_000_000 + fraction
	if negative {
		micros = -micros
	}
	return micros, nil
}

func domain2Err(message string) error { return domain.ErrInvalidRequest(message) }

func (s *Server) handleAdminAccountCreditList(w http.ResponseWriter, r *http.Request) {
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
	out := []map[string]any{}
	for _, entry := range entries {
		if entry.Kind == "charge" {
			continue
		}
		out = append(out, map[string]any{
			"id": entry.ID, "kind": entry.Kind, "amount_micros": entry.AmountMicros,
			"balance_after_micros": entry.BalanceAfterMicros, "ref_id": entry.RefID,
			"note": entry.Note, "actor": entry.Actor, "idem_key": entry.IdemKey,
			"created_at": entry.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (s *Server) handleAdminGenerateCodes(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Invoices, "invoicing")
	if !ok {
		return
	}
	var body struct {
		Count        int    `json:"count"`
		AmountMicros int64  `json:"amount_micros"`
		AmountUSD    string `json:"amount_usd"`
		ExpiresAt    string `json:"expires_at"`
		BatchID      string `json:"batch_id"`
		Note         string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	amount := body.AmountMicros
	if amount == 0 && body.AmountUSD != "" {
		parsed, err := parseUSDToMicros(body.AmountUSD)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
			return
		}
		amount = parsed
	}
	var expiresAt *time.Time
	if body.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("expires_at must be RFC3339"))
			return
		}
		expiresAt = &parsed
	}
	codes, err := store.GenerateCodes(r.Context(), billing.CodeBatchRequest{
		Count: body.Count, AmountMicros: amount, ExpiresAt: expiresAt,
		BatchID: body.BatchID, Note: body.Note, Actor: actor.Username,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "create", "redemption_code_batch", body.BatchID,
		map[string]any{"count": len(codes), "amount_micros": amount}, "ok")
	writeJSON(w, http.StatusCreated, map[string]any{
		"batch_id": body.BatchID, "count": len(codes), "codes": codes,
		"note": "these plaintext codes are shown once; only their hashes are stored",
	})
}

func (s *Server) handleAdminListCodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Codes, "redemption codes")
	if !ok {
		return
	}
	codes, err := store.Codes(r.Context(), r.URL.Query().Get("batch_id"), adminLimit(r, 100, 500))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(codes))
	for _, code := range codes {
		hash := code.CodeHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		status := "unused"
		if code.RedeemedAt != nil {
			status = "redeemed"
		} else if code.ExpiresAt != nil && code.ExpiresAt.Before(time.Now().UTC()) {
			status = "expired"
		}
		out = append(out, map[string]any{
			"id": code.ID, "hash_prefix": hash, "amount_micros": code.AmountMicros,
			"status": status, "batch_id": code.BatchID, "note": code.Note,
			"expires_at": timeOrNil(code.ExpiresAt), "redeemed_at": timeOrNil(code.RedeemedAt),
			"redeemed_by": code.RedeemedByAccountID,
			"created_at":  code.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (s *Server) handleAdminRedeemCode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Codes, "redemption codes")
	if !ok {
		return
	}
	var body struct {
		Code      string `json:"code"`
		AccountID int64  `json:"account_id"`
		Account   string `json:"account"`
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
	record, entry, err := store.RedeemCode(r.Context(), body.Code, accountID, actor.Username)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "redeem", "redemption_code", strconv.FormatInt(record.ID, 10),
		map[string]any{"account_id": accountID, "amount_micros": record.AmountMicros}, "ok")
	s.reload(r.Context(), "redemption code redeemed", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"code_id": record.ID, "account_id": accountID, "amount_micros": record.AmountMicros,
		"balance_after_micros": entry.BalanceAfterMicros,
	})
}

func (s *Server) handleAdminReconcile(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Reconciliation, "reconciliation")
	if !ok {
		return
	}
	var body struct {
		Days      int    `json:"days"`
		AccountID int64  `json:"account_id"`
		From      string `json:"from"`
		To        string `json:"to"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -1)
	if body.Days > 0 && body.Days <= 365 {
		from = to.AddDate(0, 0, -body.Days)
	}
	if body.From != "" {
		parsed, err := time.Parse(time.RFC3339, body.From)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("from must be RFC3339"))
			return
		}
		from = parsed.UTC()
	}
	if body.To != "" {
		parsed, err := time.Parse(time.RFC3339, body.To)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("to must be RFC3339"))
			return
		}
		to = parsed.UTC()
	}
	record, err := store.Reconcile(r.Context(), billing.ReconcileRequest{
		From: from, To: to, AccountID: body.AccountID,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	result := "ok"
	if record.DiffMicros != 0 {
		result = "mismatch"
	}
	s.audit(r.Context(), actor.Username, "reconcile", "billing", "", map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"diff_micros": record.DiffMicros,
	}, result)
	writeJSON(w, http.StatusOK, reconciliationJSON(record))
}

func (s *Server) handleAdminReconciliations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Reconciliation, "reconciliation")
	if !ok {
		return
	}
	records, err := store.Reconciliations(r.Context(), adminLimit(r, 30, 200))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(records))
	for _, record := range records {
		out = append(out, reconciliationJSON(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func reconciliationJSON(record *domain.Reconciliation) map[string]any {
	return map[string]any{
		"id": record.ID, "kind": record.Kind,
		"period_start":         record.PeriodStart.UTC().Format(time.RFC3339),
		"period_end":           record.PeriodEnd.UTC().Format(time.RFC3339),
		"usage_charge_micros":  record.UsageChargeMicros,
		"ledger_charge_micros": record.LedgerChargeMicros,
		"diff_micros":          record.DiffMicros,
		"missing_usage_count":  record.MissingUsageCount,
		"estimated_ratio_bp":   record.EstimatedRatioBP,
		"details":              jsonOrNil(record.DetailsJSON),
		"created_at":           record.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleAdminReplayFailures(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Reconciliation, "reconciliation")
	if !ok {
		return
	}
	result, err := store.ReplayFailures(r.Context(), 200)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "replay", "billing_failures", "", map[string]any{
		"scanned": result.Scanned, "replayed": result.Replayed, "failed": result.Failed,
	}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": result.Scanned, "replayed": result.Replayed, "failed": result.Failed,
	})
}

var _ = sort.Strings

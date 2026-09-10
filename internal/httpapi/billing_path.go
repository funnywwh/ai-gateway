package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/usage"
)

// BillingPort is the billing surface the request path needs.
type BillingPort interface {
	Admit(account *domain.Account, requestID string, estimate billing.EstimateInput, policy string, now time.Time) (billing.Admission, *billing.Reservation)
	Release(reservation *billing.Reservation)
	Touch(reservation *billing.Reservation, now time.Time)
	Submit(settlement *billing.Settlement)
	Invariants(ctx context.Context) (*billing.InvariantReport, error)
	Rebuild(ctx context.Context, accountID int64, apply bool) (*billing.RebuildPlan, error)
	Stats() billing.Stats
	Reservations() []billing.Reservation
}

// ruleSetsFor resolves the cost table of one provider's mapping and the model's sale
// table. Missing or unparsable rules degrade to nil (no pricing) with a warning,
// because a pricing mistake must never fail a customer request.
func (s *Server) ruleSetsFor(canonical string, providerID int64) (*pricing.RuleSet, *pricing.RuleSet) {
	if s.deps.Registry == nil {
		return nil, nil
	}
	snap := s.deps.Registry.Snapshot()
	var cost *pricing.RuleSet
	for _, pm := range snap.ProviderModels {
		if pm.ProviderID != providerID || pm.PublicModel != canonical || pm.PricingRulesJSON == "" {
			continue
		}
		parsed, err := pricing.ParseRuleSet(pm.PricingRulesJSON)
		if err != nil {
			s.deps.Log.Warn("provider pricing rules are invalid",
				"provider", providerID, "model", canonical, "err", err)
			break
		}
		cost = parsed
		break
	}
	var sale *pricing.RuleSet
	if entry := snap.ModelByName[canonical]; entry != nil && entry.SalePricingJSON != "" {
		parsed, err := pricing.ParseRuleSet(entry.SalePricingJSON)
		if err != nil {
			s.deps.Log.Warn("model sale pricing is invalid", "model", canonical, "err", err)
		} else {
			sale = parsed
		}
	}
	return cost, sale
}

// priceAttempt evaluates cost and charge for one attempt with the same pure function
// the admin simulator uses.
func (s *Server) priceAttempt(dims map[string]int64, at time.Time, variant string, cost, sale *pricing.RuleSet) *pricing.Result {
	cfg := s.deps.Config.Billing
	input := pricing.Input{
		Dimensions:         dims,
		At:                 at.UTC(),
		Variant:            variant,
		Cost:               cost,
		Sale:               sale,
		MinChargeMicros:    cfg.MinChargeMicros,
		PerRequestFeeScope: cfg.PerRequestFeeScope,
	}
	// The global default markup is a fallback: a mark-up declared by the model's own
	// sale rule set wins, otherwise every model would silently be repriced.
	if cfg.DefaultMarkupBP > 0 && (sale == nil || sale.MarkupBP == 0) {
		input.MarkupBP = cfg.DefaultMarkupBP
		input.MarkupSet = true
	}
	return pricing.Evaluate(input)
}

// effectiveMaxOutput is the output cap used for reservation estimates: the request's
// own limit when given, otherwise the configured default.
func (s *Server) effectiveMaxOutput(req *responses.Request, canonical string) int64 {
	if req != nil && req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		return int64(*req.MaxOutputTokens)
	}
	if limit := s.deps.Config.Billing.DefaultMaxOutputToken; limit > 0 {
		return int64(limit)
	}
	_ = canonical
	return 512
}

// estimateInputTokens is a cheap, deliberately conservative prompt size estimate:
// one token per four bytes of the request body.
func estimateInputTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	return int64(len(body)/4 + 1)
}

// inflightPolicy resolves the effective in-flight policy for an account.
func (s *Server) inflightPolicy(account *domain.Account) string {
	if account != nil && account.InflightPolicyOverride != "" {
		return account.InflightPolicyOverride
	}
	return s.deps.Config.Billing.InflightPolicy
}

// rejectForQuota writes the 402 (or 429) that ends a request the account cannot pay for.
func (s *Server) rejectForQuota(
	w http.ResponseWriter,
	r *http.Request,
	key *domain.APIKey,
	account *domain.Account,
	req *responses.Request,
	decision billing.Admission,
) {
	reason := decision.Reason
	if reason == "" {
		reason = "insufficient_quota"
	}
	message := "insufficient balance for this request; top up the account or lower max_output_tokens"
	if reason == "account_suspended" || reason == "account_closed" {
		message = "account is " + account.Status
	}
	apiErr := domain.ErrInsufficientQuota(message)
	if s.deps.Config.Billing.RejectAs429 {
		apiErr = domain.ErrRateLimited(message)
	}

	if s.deps.Log != nil {
		s.deps.Log.Info("request rejected for quota",
			"request_id", requestIDFrom(r.Context()), "key", key.Name, "account", account.Name,
			"reason", reason, "available_micros", decision.AvailableMicros,
			"reserve_micros", decision.ReserveMicros)
	}
	if s.deps.Hooks != nil {
		s.deps.Hooks.Emit(r.Context(), &domain.Event{
			Name:      "request.denied",
			Timestamp: time.Now().UTC(),
			Payload: map[string]any{
				"request_id": requestIDFrom(r.Context()),
				"account":    account.Name,
				"api_key":    key.Name,
				"model":      req.Model,
				"reason":     reason,
			},
		})
	}
	s.recordDenied(r.Context(), key, account, req, apiErr)
	writeAPIError(w, apiErr)
}

// recordDenied logs a locally rejected request. No usage row is written: the request
// never reached an upstream, so it must not appear in billing at all.
func (s *Server) recordDenied(ctx context.Context, key *domain.APIKey, account *domain.Account, req *responses.Request, apiErr *domain.APIError) {
	if s.deps.Records == nil {
		return
	}
	raw, err := json.Marshal(req)
	if err != nil {
		raw = []byte("{}")
	}
	record := &domain.RequestLogRecord{
		RequestID:       requestIDFrom(ctx),
		APIKeyID:        key.ID,
		AccountID:       account.ID,
		Endpoint:        "/v1/responses",
		RequestJSON:     truncate(string(raw), s.deps.Config.Recording.MaxBytes),
		RecordInputMode: key.RecordInputMode,
		Status:          strconv.Itoa(apiErr.Status),
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.deps.Records.PutRequestLog(ctx, record); err != nil {
		s.deps.Log.Warn("recording a denied request failed", "err", err)
	}
}

// settleAttempt prices one attempt and hands it to the billing writer. The usage row
// and the charge ledger entry travel in the same transaction, so a metered attempt can
// never exist without its money movement (or the other way round).
func (s *Server) settleAttempt(
	ctx context.Context,
	attempt *usage.Attempt,
	resolved *domain.ResolvedModel,
	cand domain.Candidate,
	attemptErr error,
	startedAt time.Time,
	dims map[string]int64,
	chargePartial bool,
) {
	record, err := s.deps.Meter.Build(attempt)
	if err != nil {
		s.deps.Log.Warn("building the usage record failed", "err", err)
		return
	}

	cost, sale := s.ruleSetsFor(resolved.Canonical, cand.ProviderID)
	result := s.priceAttempt(dims, startedAt, cand.UpstreamModel, cost, sale)

	// A failed attempt still costs us money, but charging the customer for it depends
	// on billing.charge_on_error. Cost is always recorded so the waste is visible.
	// An abort because of quota still charges what was metered before the decision;
	// everything after it is absorbed as cost only.
	chargeEnabled := attemptErr == nil || s.deps.Config.Billing.ChargeOnError || chargePartial
	settlement := billing.NewCharge(record, result, chargeEnabled)
	if !chargeEnabled {
		settlement.Usage.ChargeMicros = 0
	}
	settlement.WithCounter(billing.CounterPeriod(record.CreatedAt), totalTokensOf(dims))
	s.deps.Billing.Submit(settlement)
}

// totalTokensOf sums a dimension set, counting reasoning tokens as output.
func totalTokensOf(dims map[string]int64) int64 {
	var total int64
	for _, units := range dims {
		total += units
	}
	return total
}

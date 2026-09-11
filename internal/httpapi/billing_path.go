package httpapi

import (
	"context"
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
	ExpireGiftCredit(ctx context.Context, now time.Time, limit int) (billing.ExpiryResult, error)
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
// the admin simulator uses. Rates stay in the currency their rule set declares, and
// the ledger amounts are converted here, so every caller that persists money sees
// one currency.
func (s *Server) priceAttempt(dims map[string]int64, at time.Time, variant string, cost, sale *pricing.RuleSet, markup billing.MarkupResolution) *pricing.Result {
	cfg := s.deps.Config.Billing
	input := pricing.Input{
		Dimensions:         dims,
		At:                 at.UTC(),
		Variant:            variant,
		Cost:               cost,
		Sale:               sale,
		MinChargeMicros:    cfg.MinChargeMicros,
		PerRequestFeeScope: cfg.PerRequestFeeScope,
		Ledger:             s.ledgerCurrency(),
		FX:                 s.fxTable(),
	}
	switch {
	case markup.Set:
		// key / tag / account / model / default, resolved before the attempt ran.
		input.MarkupBP = markup.BP
		input.MarkupSet = true
		input.MarkupSource = markup.Source
	case cfg.DefaultMarkupBP > 0 && (sale == nil || sale.MarkupBP == 0):
		input.MarkupBP = cfg.DefaultMarkupBP
		input.MarkupSet = true
		input.MarkupSource = billing.MarkupSourceDefault
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
//
// The row goes through the same input policy as a served request (recordInput): a
// rejected request carries exactly the same client content, and this path used to store
// the whole body with no redaction at all — the one place where the recording policy and
// its redaction rules were not applied.
func (s *Server) recordDenied(ctx context.Context, key *domain.APIKey, account *domain.Account, req *responses.Request, apiErr *domain.APIError) {
	if s.deps.Records == nil {
		return
	}
	input := s.recordInput(ctx, key, req)
	record := &domain.RequestLogRecord{
		RequestID:       requestIDFrom(ctx),
		APIKeyID:        key.ID,
		AccountID:       account.ID,
		Endpoint:        "/v1/responses",
		RequestJSON:     input.Payload,
		RequestBytes:    input.Bytes,
		Truncated:       input.Truncated,
		RecordInputMode: input.Mode,
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
	markup billing.MarkupResolution,
) {
	record, err := s.deps.Meter.Build(attempt)
	if err != nil {
		s.deps.Log.Warn("building the usage record failed", "err", err)
		return
	}

	cost, sale := s.ruleSetsFor(resolved.Canonical, cand.ProviderID)
	result := s.priceAttempt(dims, startedAt, cand.UpstreamModel, cost, sale, markup)

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

// resolveMarkup walks the multiplier precedence chain for one request: key policy, tag
// policies, account override, the model's sale rule set, then the configured default.
func (s *Server) resolveMarkup(key *domain.APIKey, account *domain.Account, sale *pricing.RuleSet) billing.MarkupResolution {
	modelMarkup := 0
	if sale != nil {
		modelMarkup = sale.MarkupBP
	}
	var tags []*domain.Tag
	if s.deps.Router != nil && s.deps.Registry != nil && key != nil {
		tags = s.deps.Router.ResolveTags(s.deps.Registry.Snapshot(), key)
	}
	defaultBP := 0
	if s.deps.Config != nil {
		defaultBP = s.deps.Config.Billing.DefaultMarkupBP
	}
	return billing.ResolveMarkup(key, tags, account, modelMarkup, defaultBP)
}

// saleRulesFor returns only the customer-side rule set, which is what the multiplier
// chain needs (the cost side is irrelevant to it).
func (s *Server) saleRulesFor(canonical string, providerID int64) *pricing.RuleSet {
	_, sale := s.ruleSetsFor(canonical, providerID)
	return sale
}

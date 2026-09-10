package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
)

// pricingSimulateBody is the simulator request. Rule sets may be supplied inline so
// an operator can ask "what would this change cost?" without saving anything.
type pricingSimulateBody struct {
	Model           string           `json:"model"`
	At              string           `json:"at"`
	Variant         string           `json:"variant"`
	Dimensions      map[string]int64 `json:"dimensions"`
	MarkupBP        *int             `json:"markup_bp"`
	MinChargeMicros int64            `json:"min_charge_micros"`
	CostRules       json.RawMessage  `json:"cost_rules"`
	SaleRules       json.RawMessage  `json:"sale_rules"`
}

// handleAdminSimulatePricing prices one hypothetical request with the same pure
// function the data plane uses, so the number shown here is the number charged.
func (s *Server) handleAdminSimulatePricing(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	var body pricingSimulateBody
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		writeAPIError(w, domain.ErrInvalidRequest("model is required"))
		return
	}
	at := time.Now().UTC()
	if strings.TrimSpace(body.At) != "" {
		parsed, err := time.Parse(time.RFC3339, body.At)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("at must be RFC3339, for example 2026-03-02T17:00:00Z"))
			return
		}
		at = parsed.UTC()
	}

	costSet, saleSet, sources, err := s.resolveRuleSets(model, body)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	input := pricing.Input{
		Dimensions:      body.Dimensions,
		At:              at,
		Variant:         strings.TrimSpace(body.Variant),
		Cost:            costSet,
		Sale:            saleSet,
		MinChargeMicros: body.MinChargeMicros,
	}
	if body.MarkupBP != nil {
		input.MarkupBP = *body.MarkupBP
		input.MarkupSet = true
	}
	if s.deps.Config != nil {
		input.PerRequestFeeScope = s.deps.Config.Billing.PerRequestFeeScope
		if input.MinChargeMicros == 0 {
			input.MinChargeMicros = s.deps.Config.Billing.MinChargeMicros
		}
		if !input.MarkupSet && s.deps.Config.Billing.DefaultMarkupBP > 0 {
			input.MarkupBP = s.deps.Config.Billing.DefaultMarkupBP
			input.MarkupSet = true
		}
	}

	result := pricing.Evaluate(input)
	writeJSON(w, http.StatusOK, map[string]any{
		"model": model, "at": at.Format(time.RFC3339), "variant": input.Variant,
		"sources":             sources,
		"dimensions":          input.Dimensions,
		"cost_micros":         result.CostMicros,
		"charge_micros":       result.ChargeMicros,
		"cost_rule_id":        result.CostRuleID,
		"sale_rule_id":        result.SaleRuleID,
		"sale_basis":          result.SaleBasis,
		"markup_bp":           result.MarkupBP,
		"cost_lines":          result.CostLines,
		"sale_lines":          result.SaleLines,
		"unpriced_dimensions": result.UnpricedDimensions,
		"min_charge_applied":  result.MinChargeApplied,
		"snapshot":            result.Snapshot,
	})
}

// resolveRuleSets picks the rule sets to price with: explicit ones from the request
// win, otherwise the model's sale table and the routed provider's cost table.
func (s *Server) resolveRuleSets(model string, body pricingSimulateBody) (*pricing.RuleSet, *pricing.RuleSet, map[string]string, error) {
	sources := map[string]string{}
	var costSet, saleSet *pricing.RuleSet
	var err error

	if len(body.CostRules) > 0 {
		costSet, err = pricing.ParseRuleSet(string(body.CostRules))
		if err != nil {
			return nil, nil, nil, err
		}
		sources["cost"] = "request"
	}
	if len(body.SaleRules) > 0 {
		saleSet, err = pricing.ParseRuleSet(string(body.SaleRules))
		if err != nil {
			return nil, nil, nil, err
		}
		sources["sale"] = "request"
	}
	if s.deps.Registry == nil || (costSet != nil && saleSet != nil) {
		return costSet, saleSet, sources, nil
	}

	snap := s.deps.Registry.Snapshot()
	canonical := model
	if entry := snap.ModelByName[model]; entry == nil {
		// A prefix/glob mapping may be the only thing that knows the real model.
		if s.deps.Router != nil {
			if explanation, explainErr := s.deps.Router.Explain(domain.RouteRequest{Model: model}); explainErr == nil && explanation.Canonical != "" {
				canonical = explanation.Canonical
			}
		}
	} else if saleSet == nil {
		saleSet, err = pricing.ParseRuleSet(entry.SalePricingJSON)
		if err != nil {
			return nil, nil, nil, err
		}
		sources["sale"] = "model:" + entry.PublicName
	}

	if costSet == nil {
		candidate := pickProviderModel(snap.ProviderModels, canonical)
		if candidate != nil {
			costSet, err = pricing.ParseRuleSet(candidate.PricingRulesJSON)
			if err != nil {
				return nil, nil, nil, err
			}
			sources["cost"] = "provider_model:" + strconv.FormatInt(candidate.ProviderID, 10) + "/" + candidate.PublicModel
		} else {
			sources["cost"] = "none"
		}
	}
	if saleSet == nil {
		sources["sale"] = "none (cost_follow with the configured default markup)"
	}
	return costSet, saleSet, sources, nil
}

// pickProviderModel chooses a deterministic cost table when several providers serve
// the same public model: the highest-priority mapping wins.
func pickProviderModel(models []*domain.ProviderModel, publicModel string) *domain.ProviderModel {
	matched := []*domain.ProviderModel{}
	for _, candidate := range models {
		if candidate.PublicModel == publicModel && candidate.PricingRulesJSON != "" {
			matched = append(matched, candidate)
		}
	}
	if len(matched) == 0 {
		return nil
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority < matched[j].Priority
		}
		return matched[i].ID < matched[j].ID
	})
	return matched[0]
}

// handleAdminValidatePricing checks a rule set and reports shadowed rules without
// saving anything; the editor calls it while the operator types.
func (s *Server) handleAdminValidatePricing(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("failed to read the request body"))
		return
	}
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		writeAPIError(w, domain.ErrInvalidRequest("request body is empty"))
		return
	}
	if !json.Valid([]byte(raw)) {
		writeAPIError(w, domain.ErrInvalidRequest("request body is not valid JSON"))
		return
	}
	set, err := pricing.ParseRuleSet(raw)
	if err != nil {
		if apiErr, ok := domain.AsAPIError(err); ok {
			writeJSON(w, http.StatusOK, map[string]any{"valid": false, "error": apiErr.Message})
			return
		}
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "rules": len(set.Rules), "shadowed": pricing.DetectShadowing(set),
	})
}

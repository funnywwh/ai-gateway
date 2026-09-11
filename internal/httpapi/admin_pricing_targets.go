package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
)

// handleAdminPricingTargets returns every editable price target in one call: the sale
// table of each model and the cost table of each provider mapping. The console needs
// all of them at once to avoid a request per model and provider.
func (s *Server) handleAdminPricingTargets(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	models, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	providers, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	ctx := r.Context()

	list, err := models.ListModels(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	mappings, err := providers.ListProviderModels(ctx, 0)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	providerList, err := providers.ListProviders(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	providerNames := map[int64]string{}
	for _, provider := range providerList {
		providerNames[provider.ID] = provider.Name
	}

	// A sale price derived from cost is only meaningful when at least one provider
	// mapping actually carries cost rules; without them the multiplier multiplies zero.
	costConfigured := map[string]bool{}
	for _, mapping := range mappings {
		if strings.TrimSpace(mapping.PricingRulesJSON) != "" {
			costConfigured[mapping.PublicModel] = true
		}
	}

	targets := make([]map[string]any, 0, len(list)+len(mappings))
	for _, model := range list {
		payload := map[string]any{
			"kind": "sale", "model": model.PublicName, "enabled": model.Enabled,
			"cost_rules_configured":        costConfigured[model.PublicName],
			"effective_markup_source_hint": "model",
		}
		for key, value := range describeRuleSet(model.SalePricingJSON) {
			payload[key] = value
		}
		if s.deps.Config != nil {
			sale, _ := pricing.ParseRuleSet(model.SalePricingJSON)
			resolved := billing.ResolveMarkup(nil, nil, nil, modelMarkupOf(sale), s.deps.Config.Billing.DefaultMarkupBP)
			payload["effective_markup"] = map[string]any{"bp": resolved.BP, "source": resolved.Source, "set": resolved.Set}
		}
		targets = append(targets, payload)
	}
	for _, mapping := range mappings {
		payload := map[string]any{
			"kind": "cost", "model": mapping.PublicModel,
			"provider_id": mapping.ProviderID, "provider_name": providerNames[mapping.ProviderID],
			"provider_model_id": mapping.ID, "upstream_model": mapping.UpstreamModel,
			"enabled": mapping.Enabled, "source": mapping.Source,
		}
		for key, value := range describeRuleSet(mapping.PricingRulesJSON) {
			payload[key] = value
		}
		targets = append(targets, payload)
	}
	sort.SliceStable(targets, func(i, j int) bool {
		left, _ := targets[i]["model"].(string)
		right, _ := targets[j]["model"].(string)
		if left == right {
			leftKind, _ := targets[i]["kind"].(string)
			rightKind, _ := targets[j]["kind"].(string)
			return leftKind < rightKind
		}
		return left < right
	})
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets, "count": len(targets)})
}

func modelMarkupOf(set *pricing.RuleSet) int {
	if set == nil {
		return 0
	}
	return set.MarkupBP
}

// describeRuleSet parses a stored rule document for the console. A document that does
// not parse is reported as parse_error rather than as a server error: the console has
// to be able to show and repair broken data.
func describeRuleSet(raw string) map[string]any {
	payload := map[string]any{
		"rules": []any{}, "basis": "", "markup_bp": 0,
		"dimension_markup_bp": map[string]int{}, "valid": true, "parse_error": "",
		"shadowed": 0, "catch_all": false,
	}
	if strings.TrimSpace(raw) == "" {
		return payload
	}
	set, err := pricing.ParseRuleSet(raw)
	if err != nil {
		payload["valid"] = false
		payload["parse_error"] = err.Error()
		return payload
	}
	payload["basis"] = set.Basis
	payload["markup_bp"] = set.MarkupBP
	payload["dimension_markup_bp"] = set.DimensionMarkupBP
	catchAll := false
	for _, rule := range set.Rules {
		if rule.When.Empty() {
			catchAll = true
		}
	}
	payload["catch_all"] = catchAll
	payload["shadowed"] = len(pricing.DetectShadowing(set))
	payload["rules"] = set.Rules
	return payload
}

// handleAdminPatchMarkup changes only the multiplier fields of a model's sale pricing,
// keeping the rule array untouched so a concurrent rule edit is not clobbered.
func (s *Server) handleAdminPatchMarkup(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	var body struct {
		Model             string         `json:"model"`
		Basis             string         `json:"basis"`
		MarkupBP          *int           `json:"markup_bp"`
		DimensionMarkupBP map[string]int `json:"dimension_markup_bp"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name := strings.TrimSpace(body.Model)
	if name == "" {
		writeAPIError(w, domain.ErrInvalidRequest("model is required"))
		return
	}
	if body.MarkupBP == nil {
		writeAPIError(w, domain.ErrInvalidRequest("markup_bp is required"))
		return
	}
	if *body.MarkupBP < 0 || *body.MarkupBP > 1_000_000 {
		writeAPIError(w, domain.ErrInvalidRequest("markup_bp must be between 0 and 1000000 (0x to 100x)"))
		return
	}
	model, err := store.GetModelByName(r.Context(), name)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	// Preserve everything else in the document, including the rule array.
	document := map[string]json.RawMessage{}
	if strings.TrimSpace(model.SalePricingJSON) != "" {
		if err := json.Unmarshal([]byte(model.SalePricingJSON), &document); err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("the stored sale pricing is not a JSON object; repair it with the JSON editor first"))
			return
		}
	}
	basis := strings.TrimSpace(body.Basis)
	if basis == "" {
		basis = pricing.BasisCostFollow
	}
	if basis != pricing.BasisCostFollow && basis != pricing.BasisAbsolute {
		writeAPIError(w, domain.ErrInvalidRequest("basis must be cost_follow or absolute"))
		return
	}
	for dimension := range body.DimensionMarkupBP {
		if !dimensionNameRE.MatchString(dimension) {
			writeAPIError(w, domain.ErrInvalidRequest("dimension_markup_bp key "+dimension+" is not a valid dimension name"))
			return
		}
	}
	document["basis"], _ = json.Marshal(basis)
	document["markup_bp"], _ = json.Marshal(*body.MarkupBP)
	if body.DimensionMarkupBP != nil {
		if len(body.DimensionMarkupBP) == 0 {
			delete(document, "dimension_markup_bp")
		} else {
			document["dimension_markup_bp"], _ = json.Marshal(body.DimensionMarkupBP)
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		writeAPIError(w, domain.ErrInternal("cannot encode the sale pricing document"))
		return
	}
	if _, err := pricing.ParseRuleSet(string(encoded)); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	model.SalePricingJSON = string(encoded)
	id, err := store.UpsertModel(r.Context(), model)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "pricing_markup", strconv.FormatInt(id, 10), map[string]any{
		"model": name, "basis": basis, "markup_bp": *body.MarkupBP,
		"dimension_markup_bp": body.DimensionMarkupBP,
	}, "ok")
	s.reload(r.Context(), "sale markup updated", true)
	writeJSON(w, http.StatusOK, describeRuleSet(model.SalePricingJSON))
}

// dimensionNameRE mirrors the pricing package's dimension naming rule so the console
// endpoint can reject a bad key before it reaches the engine.
var dimensionNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

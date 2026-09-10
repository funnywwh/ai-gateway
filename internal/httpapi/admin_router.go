package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// handleAdminExplainRouter runs the same resolution and candidate selection the data
// plane uses, but returns the diagnosis instead of calling a provider. The Mappings
// page needs it: without it, rules could only be edited blind.
func (s *Server) handleAdminExplainRouter(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	if s.deps.Router == nil || s.deps.Registry == nil {
		writeAPIError(w, domain.ErrUnsupported("routing diagnostics are disabled in this deployment"))
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeAPIError(w, domain.ErrInvalidRequest("model is required"))
		return
	}

	accountID := int64(0)
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("account_id must be an integer"))
			return
		}
		accountID = parsed
	}

	// A synthetic key stands in for the caller unless a real one is named, so the
	// simulator can show both "what this account may use" and "what anyone may use".
	key := &domain.APIKey{AccountID: accountID, Status: "active", RecordInputMode: "inherit"}
	if raw := r.URL.Query().Get("api_key_id"); raw != "" {
		keyID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("api_key_id must be an integer"))
			return
		}
		resolved, ok := s.lookupAPIKey(r, keyID)
		if !ok {
			writeAPIError(w, domain.ErrNotFound("api key "+raw))
			return
		}
		key = resolved
	}

	snap := s.deps.Registry.Snapshot()
	tags := s.deps.Router.ResolveTags(snap, key)
	grant := s.deps.Router.Authorize(key, tags)
	explanation, err := s.deps.Router.Explain(domain.RouteRequest{
		Model:       model,
		KeyID:       key.ID,
		Key:         key,
		Tags:        tags,
		Grant:       grant,
		ProviderPin: strings.TrimSpace(r.URL.Query().Get("provider")),
		Strategy:    strings.TrimSpace(r.URL.Query().Get("strategy")),
		Features:    featureQuery(r.URL.Query().Get("features")),
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	order := make([]map[string]any, 0, len(explanation.Order))
	for _, candidate := range explanation.Order {
		order = append(order, map[string]any{
			"route_id": candidate.RouteID, "provider_id": candidate.ProviderID,
			"provider": candidate.ProviderName, "upstream_model": candidate.UpstreamModel,
			"priority": candidate.Priority, "weight": candidate.Weight,
			"strategy": candidate.Strategy, "degraded": candidate.Degraded,
		})
	}
	excluded := make([]map[string]any, 0, len(explanation.Excluded))
	for _, item := range explanation.Excluded {
		excluded = append(excluded, map[string]any{"provider": item.ProviderName, "reason": item.Reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requested": explanation.Requested, "canonical": explanation.Canonical,
		"rule": explanation.Rule, "grant": grant, "failure": explanation.Failure,
		"order": order, "excluded": excluded,
	})
}

// lookupAPIKey scans the key list because the management store exposes listing only;
// the key count is small and this is an operator diagnostic, not a hot path.
func (s *Server) lookupAPIKey(r *http.Request, id int64) (*domain.APIKey, bool) {
	if s.deps.AdminStore == nil {
		return nil, false
	}
	keys, err := s.deps.AdminStore.ListAPIKeys(r.Context(), 0)
	if err != nil {
		return nil, false
	}
	for _, key := range keys {
		if key.ID == id {
			return key, true
		}
	}
	return nil, false
}

// featureQuery turns "tools,stream" into the feature set the router checks.
func featureQuery(raw string) map[string]bool {
	features := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			features[name] = true
		}
	}
	if len(features) == 0 {
		return nil
	}
	return features
}

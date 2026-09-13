package routing

import (
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

func reasoningSnapshot(reasoning string, mappings []*domain.ModelMapping, capabilities string) *registry.Snapshot {
	return registry.NewSnapshot(nil,
		[]*domain.Provider{{ID: 10, Name: "supplier", Kind: "testecho", Enabled: true, Weight: 100}},
		[]*domain.ProviderModel{{ID: 100, ProviderID: 10, PublicModel: "canonical", UpstreamModel: "upstream", Enabled: true, CapabilitiesJSON: capabilities}},
		[]*domain.Model{{ID: 1, PublicName: "canonical", Enabled: true, ReasoningJSON: reasoning}},
		mappings,
		[]*domain.Route{{ID: 200, ModelID: 1, ProviderID: 10, Enabled: true, Weight: 100}}, nil)
}

func TestPlanCapturesCanonicalReasoningForAliasAndPreservesFeatures(t *testing.T) {
	snap := reasoningSnapshot(`{"mode":"default","effort":"high"}`, []*domain.ModelMapping{{
		ID: 1, Kind: "exact", Pattern: "alias", TargetModel: "canonical", Enabled: true,
	}}, `{"reasoning":true,"tools":true}`)
	r := newRouter(Config{Degradation: "reject"}, snap)
	features := map[string]bool{"tools": true}

	res, err := r.Plan(domain.RouteRequest{Model: "alias", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved.Canonical != "canonical" || res.Reasoning == nil || res.Reasoning.Effort != "high" {
		t.Fatalf("canonical reasoning was not captured: resolved=%+v reasoning=%+v", res.Resolved, res.Reasoning)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("reasoning capability route should survive: candidates=%+v exclusions=%+v", res.Candidates, res.Excluded)
	}
	if features["reasoning"] || len(features) != 1 || !features["tools"] {
		t.Fatalf("Plan mutated caller features: %+v", features)
	}
}

func TestPlanModelReasoningPreservesCapabilitySemantics(t *testing.T) {
	for _, policy := range []string{`{"mode":"default","effort":"high"}`, `{"mode":"force","effort":"none"}`} {
		for _, pin := range []string{"", "supplier"} {
			for _, degradation := range []string{"reject", "strip"} {
				r := newRouter(Config{Degradation: degradation}, reasoningSnapshot(policy, nil, `{"reasoning":false}`))
				res, err := r.Plan(domain.RouteRequest{Model: "canonical", ProviderPin: pin})
				if degradation == "reject" {
					if err == nil {
						t.Fatalf("%s pin=%q must reject missing reasoning capability", policy, pin)
					}
					continue
				}
				if err != nil || len(res.Candidates) != 1 || len(res.Candidates[0].Degraded) != 1 || res.Candidates[0].Degraded[0] != "reasoning" || res.Reasoning == nil {
					t.Fatalf("strip must retain existing marker-only behavior: result=%+v error=%v", res, err)
				}
			}
		}
	}
}

func TestPlanAppliesReasoningCapabilityToPinnedRoute(t *testing.T) {
	snap := reasoningSnapshot(`{"mode":"force","effort":"high"}`, nil, `{"reasoning":false}`)
	r := newRouter(Config{Degradation: "reject"}, snap)

	res, err := r.Plan(domain.RouteRequest{Model: "canonical", ProviderPin: "supplier"})
	if err == nil || res == nil {
		t.Fatalf("pinned route lacking reasoning capability must be rejected: res=%+v err=%v", res, err)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Reason != "missing_capability:reasoning" {
		t.Fatalf("exclusions = %+v", res.Excluded)
	}
}

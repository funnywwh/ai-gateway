package routing

import (
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// TestJSONObjectCapabilityIsDistinctFromJSONSchema pins the capability vocabulary that a
// real deployment depends on: DeepSeek's /chat/completions serves json_object and has no
// json_schema support at all.
//
// Reading every structured-output request as "needs json_schema" made such a provider
// invisible to a request it handles perfectly well, and the client got
// "no authorised provider supports the requested features (json_schema)" for asking
// something the gateway could in fact serve. The two levels must gate separately.
func TestJSONObjectCapabilityIsDistinctFromJSONSchema(t *testing.T) {
	snap := jsonObjectSnapshot()
	free := keyWith(`["free"]`, "", "")
	onlyDeepseek := keyWith("", `{"models":["gpt-x"],"providers":["deepseek"]}`, "")

	t.Run("json_object is served by a json_object provider", func(t *testing.T) {
		r := newRouter(Config{Degradation: "reject", DefaultGrant: "none"}, snap)
		res, err := r.Plan(domain.RouteRequest{
			Model: "gpt-x", Key: onlyDeepseek,
			Features: map[string]bool{"json_object": true},
		})
		if err != nil {
			t.Fatalf("a json_object request must reach a json_object provider: %v", err)
		}
		if len(res.Candidates) != 1 || res.Candidates[0].ProviderName != "deepseek" {
			t.Fatalf("candidates = %+v, want exactly the deepseek provider", res.Candidates)
		}
	})

	t.Run("json_schema is refused rather than sent and rejected upstream", func(t *testing.T) {
		r := newRouter(Config{Degradation: "reject", DefaultGrant: "none"}, snap)
		_, err := r.Plan(domain.RouteRequest{
			Model: "gpt-x", Key: onlyDeepseek,
			Features: map[string]bool{"json_schema": true},
		})
		if !domain.HasStatus(err, 400) {
			t.Fatalf("json_schema on a provider that cannot enforce it = %v, want a 400", err)
		}
	})

	t.Run("json_object does not leak into a json_schema-only tier", func(t *testing.T) {
		// The mirror image, and the reason the gate is not simply "any deepseek-ish
		// provider will do": a provider that declares only json_schema must not be handed
		// a json_object request, because it serves structured output by a different
		// mechanism (and DeepSeek's dialect rejects the field outright).
		r := newRouter(Config{Degradation: "reject", DefaultGrant: "none"}, snap)
		schemaOnly := keyWith("", `{"models":["gpt-x"],"providers":["schema-only"]}`, "")
		_, err := r.Plan(domain.RouteRequest{
			Model: "gpt-x", Key: schemaOnly,
			Features: map[string]bool{"json_object": true},
		})
		if !domain.HasStatus(err, 400) {
			t.Fatalf("json_object routed to a json_schema-only provider: %v", err)
		}

		// With both granted, the json_object request must land on the json_object tier
		// rather than being spread over both.
		res, err := r.Plan(domain.RouteRequest{
			Model: "gpt-x", Key: free,
			Features: map[string]bool{"json_object": true},
		})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, cand := range res.Candidates {
			if cand.ProviderName != "deepseek" {
				t.Fatalf("json_object candidate %q cannot serve the level: %+v", cand.ProviderName, res.Candidates)
			}
		}
	})
}

// jsonObjectSnapshot is the routing fixture with deepseek declaring json_object, which is
// what the live deployment declares for deepseek-flash.
func jsonObjectSnapshot() *registry.Snapshot {
	providers := []*domain.Provider{
		{ID: 20, Name: "deepseek", Kind: "openai-chat", Enabled: true, Priority: 20, Weight: 100},
		{ID: 10, Name: "schema-only", Kind: "openai-responses", Enabled: true, Priority: 10, Weight: 100},
	}
	pms := []*domain.ProviderModel{
		{ID: 101, ProviderID: 20, PublicModel: "gpt-x", UpstreamModel: "deepseek-chat", Enabled: true,
			MaxOutputTokens: 8192, CapabilitiesJSON: `{"stream":true,"tools":true,"json_object":true}`},
		{ID: 100, ProviderID: 10, PublicModel: "gpt-x", UpstreamModel: "gpt-x", Enabled: true,
			MaxOutputTokens: 4096, CapabilitiesJSON: `{"stream":true,"json_schema":true}`},
	}
	models := []*domain.Model{{ID: 1, PublicName: "gpt-x", Enabled: true}}
	routes := []*domain.Route{
		{ID: 201, ModelID: 1, ProviderID: 20, Priority: 20, Weight: 100, Enabled: true},
		{ID: 200, ModelID: 1, ProviderID: 10, Priority: 10, Weight: 100, Enabled: true},
	}
	tags := []*domain.Tag{{
		ID: 1, Name: "free", Priority: 10,
		GrantsJSON: `{"models":["gpt-x"],"providers":["deepseek","schema-only"]}`,
	}}
	return registry.NewSnapshot(nil, providers, pms, models, nil, routes, tags)
}

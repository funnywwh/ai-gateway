package routing

import (
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

func factsSnapshot(pms []*domain.ProviderModel) *registry.Snapshot {
	providers := []*domain.Provider{}
	seen := map[int64]bool{}
	for _, pm := range pms {
		if seen[pm.ProviderID] {
			continue
		}
		seen[pm.ProviderID] = true
		providers = append(providers, &domain.Provider{ID: pm.ProviderID, Name: "p" + string(rune('a'+pm.ProviderID)), Enabled: true})
	}
	return registry.NewSnapshot(nil, providers, pms, nil, nil, nil, nil)
}

func TestEffectiveCapabilities(t *testing.T) {
	cases := []struct {
		name string
		pm   *domain.ProviderModel
		want map[string]bool
	}{
		{"nil model", nil, nil},
		{"declaration", &domain.ProviderModel{CapabilitiesJSON: `{"stream":true,"tools":true}`}, map[string]bool{"stream": true, "tools": true}},
		{"override wins", &domain.ProviderModel{CapabilitiesJSON: `{"stream":true}`, CapabilitiesOverride: `{"stream":true,"reasoning":true}`}, map[string]bool{"stream": true, "reasoning": true}},
		// `capabilities_override: inherit` stores a word, not JSON: the model is exempt from
		// capability checks, which is the escape hatch the docs promise.
		{"unreadable override means unknown", &domain.ProviderModel{CapabilitiesJSON: `{"stream":true}`, CapabilitiesOverride: "inherit"}, nil},
		{"empty object means unknown", &domain.ProviderModel{CapabilitiesJSON: `{}`}, nil},
		{"unreadable declaration means unknown", &domain.ProviderModel{CapabilitiesJSON: "not json"}, nil},
		{"nothing declared", &domain.ProviderModel{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveCapabilities(tc.pm)
			if len(got) != len(tc.want) {
				t.Fatalf("capabilities = %v, want %v", got, tc.want)
			}
			for name, want := range tc.want {
				if got[name] != want {
					t.Fatalf("capabilities = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The two aggregates answer opposite questions: a capability is a union (declaring it is what
// lets a request be routed there at all), a capacity is a minimum (the router never looks at
// context length, so the largest declared window would be a promise some route cannot keep).
func TestModelFactsAggregatesCandidates(t *testing.T) {
	snap := factsSnapshot([]*domain.ProviderModel{
		{ID: 1, ProviderID: 1, PublicModel: "m", ContextWindow: 1000000, MaxOutputTokens: 65536, CapabilitiesJSON: `{"stream":true,"reasoning":true}`},
		{ID: 2, ProviderID: 2, PublicModel: "m", ContextWindow: 272000, MaxOutputTokens: 128000, CapabilitiesJSON: `{"stream":true,"tools":true,"image":true}`},
	})
	facts := ModelFactsFor(snap, "m", []domain.Candidate{{ProviderID: 1}, {ProviderID: 2}})
	if facts.ContextWindow != 272000 {
		t.Errorf("context window = %d, want the smallest declared 272000", facts.ContextWindow)
	}
	if facts.MaxOutputTokens != 65536 {
		t.Errorf("max output = %d, want the smallest declared 65536", facts.MaxOutputTokens)
	}
	for _, name := range []string{"stream", "reasoning", "tools", "image"} {
		if !facts.Capabilities[name] {
			t.Errorf("capability %q missing from the union: %v", name, facts.Capabilities)
		}
	}
	if len(facts.Capabilities) != 4 {
		t.Errorf("capabilities = %v, want exactly the declared-true union", facts.Capabilities)
	}
}

// An undeclared capacity is 0, and 0 must never win a minimum: one hand-made mapping row
// without capacities would otherwise erase the only real numbers the model published.
func TestModelFactsIgnoresUndeclaredCapacities(t *testing.T) {
	snap := factsSnapshot([]*domain.ProviderModel{
		{ID: 1, ProviderID: 1, PublicModel: "m", ContextWindow: 1000000, MaxOutputTokens: 65536, CapabilitiesJSON: `{"stream":true}`},
		{ID: 2, ProviderID: 2, PublicModel: "m", CapabilitiesJSON: `{"stream":true}`},
	})
	facts := ModelFactsFor(snap, "m", []domain.Candidate{{ProviderID: 1}, {ProviderID: 2}})
	if facts.ContextWindow != 1000000 || facts.MaxOutputTokens != 65536 {
		t.Fatalf("declared capacities were erased by an undeclared row: %+v", facts)
	}
}

func TestModelFactsWithoutDeclarations(t *testing.T) {
	snap := factsSnapshot([]*domain.ProviderModel{{ID: 1, ProviderID: 1, PublicModel: "m"}})
	facts := ModelFactsFor(snap, "m", []domain.Candidate{{ProviderID: 1}})
	if facts.ContextWindow != 0 || facts.MaxOutputTokens != 0 || facts.Capabilities != nil {
		t.Fatalf("facts = %+v, want the zero value so nothing is disclosed", facts)
	}
	// A candidate whose provider model row is missing (a route to another canonical name)
	// contributes nothing rather than failing the listing.
	if got := ModelFactsFor(snap, "other", []domain.Candidate{{ProviderID: 1}}); !factsEmpty(got) {
		t.Fatalf("facts for an unmapped model = %+v, want the zero value", got)
	}
	if got := ModelFactsFor(nil, "m", []domain.Candidate{{ProviderID: 1}}); !factsEmpty(got) {
		t.Fatalf("facts without a snapshot = %+v, want the zero value", got)
	}
}

// factsEmpty reports whether nothing at all is disclosed (the struct is not comparable
// because of its capability map).
func factsEmpty(facts ModelFacts) bool {
	return facts.ContextWindow == 0 && facts.MaxOutputTokens == 0 && len(facts.Capabilities) == 0
}

// capabilities_override: inherit takes the model out of both the routing check and the
// disclosure, so a model exempted from capability checks is not advertised as capable.
func TestModelFactsTreatInheritOverrideAsUndeclared(t *testing.T) {
	snap := factsSnapshot([]*domain.ProviderModel{
		{ID: 1, ProviderID: 1, PublicModel: "m", ContextWindow: 8192, CapabilitiesJSON: `{"stream":true,"reasoning":true}`, CapabilitiesOverride: "inherit"},
	})
	facts := ModelFactsFor(snap, "m", []domain.Candidate{{ProviderID: 1}})
	if facts.Capabilities != nil {
		t.Fatalf("capabilities = %v, want none disclosed for an inheriting model", facts.Capabilities)
	}
	if facts.ContextWindow != 8192 {
		t.Fatalf("context window = %d, want the declared 8192 (the override is about capabilities)", facts.ContextWindow)
	}
}

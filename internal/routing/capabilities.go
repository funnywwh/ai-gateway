package routing

import (
	"encoding/json"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// EffectiveCapabilities resolves one provider model's effective capability set: the
// per-model override wins over the declaration the provider shipped, and an empty or
// unreadable set answers nil, which every caller reads as "unknown" rather than "nothing
// supported". The unreadable case is deliberate and load-bearing: `capabilities_override:
// inherit` stores a word, not JSON, so a model whose operator wants it exempt from
// capability checks resolves to unknown and passes `checkCapabilities`.
//
// Callers outside this package need it because the same resolution decides both what a
// request is routed by and what a client is told about a model (see ModelFactsFor); one
// truth, two readers.
func EffectiveCapabilities(pm *domain.ProviderModel) map[string]bool {
	if pm == nil {
		return nil
	}
	raw := strings.TrimSpace(pm.CapabilitiesOverride)
	if raw == "" {
		raw = strings.TrimSpace(pm.CapabilitiesJSON)
	}
	if raw == "" {
		return nil
	}
	var caps map[string]bool
	if err := json.Unmarshal([]byte(raw), &caps); err != nil || len(caps) == 0 {
		return nil
	}
	return caps
}

// ModelFacts are the capability facts one canonical model discloses to a client that may
// call it: the capacities its candidate routes agree on, and the union of the capabilities
// they declare.
//
// The two aggregates answer the same question from opposite directions on purpose. A
// capability is a union because declaring it is what lets routing send the request there at
// all (an image request only reaches a route that declares images), while a capacity must be
// the smallest declared value: the router picks candidates by weight and priority with no
// knowledge of context length, so advertising the largest one would let a client build a
// conversation some candidate cannot finish.
type ModelFacts struct {
	// ContextWindow is the smallest declared window among the candidates; 0 = none declared.
	ContextWindow int
	// MaxOutputTokens is the smallest declared output cap among the candidates; 0 = none declared.
	MaxOutputTokens int
	// Capabilities holds the declared-true keys of the candidates' union; nil = nothing declared.
	Capabilities map[string]bool
}

// ModelFactsFor folds the candidate routes of one canonical model into the facts a client
// listing is told. A candidate with no provider model row (or one that is unreadable)
// contributes nothing rather than a zero: "not declared" must not win a minimum, which is
// how a single hand-made mapping row would otherwise erase a real 1M context window.
func ModelFactsFor(snap *registry.Snapshot, canonical string, candidates []domain.Candidate) ModelFacts {
	facts := ModelFacts{}
	for _, candidate := range candidates {
		if snap == nil {
			break
		}
		pm := snap.ProviderModel(candidate.ProviderID, canonical)
		if pm == nil {
			continue
		}
		facts.ContextWindow = minDeclared(facts.ContextWindow, pm.ContextWindow)
		facts.MaxOutputTokens = minDeclared(facts.MaxOutputTokens, pm.MaxOutputTokens)
		for name, declared := range EffectiveCapabilities(pm) {
			if !declared {
				continue
			}
			if facts.Capabilities == nil {
				facts.Capabilities = map[string]bool{}
			}
			facts.Capabilities[name] = true
		}
	}
	return facts
}

// minDeclared folds one declared capacity into a running minimum, ignoring the zero that
// means "not declared" on both sides.
func minDeclared(current, declared int) int {
	if declared <= 0 {
		return current
	}
	if current <= 0 || declared < current {
		return declared
	}
	return current
}

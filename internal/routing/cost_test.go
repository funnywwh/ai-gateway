package routing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// costGateStub marks providers as capped by id, so the filter can be tested without a
// metering table. The tracker's own behaviour is covered in internal/runtime.
type costGateStub struct {
	capped map[int64]bool
	seen   int
}

func (g *costGateStub) Exceeded(p *domain.Provider, _ time.Time) bool {
	g.seen++
	if p == nil {
		return false
	}
	return g.capped[p.ID]
}

func costRouter(capped ...int64) (*Router, *costGateStub) {
	gate := &costGateStub{capped: map[int64]bool{}}
	for _, id := range capped {
		gate.capped[id] = true
	}
	router := newRouter(Config{}, fixture())
	router.SetCostGate(gate)
	return router, gate
}

// A capped provider is dropped with a reason an operator can read in the routing explain, and
// the request still has a candidate: cost is an availability filter, not a failure.
func TestCostCapExcludesTheProviderAndFailsOver(t *testing.T) {
	router, gate := costRouter(10)
	result, err := router.Plan(domain.RouteRequest{Model: "gpt-x", Key: keyWith(`["free"]`, "", "")})
	if err != nil {
		t.Fatalf("a capped provider must not fail the whole plan: %v", err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].ProviderName != "deepseek" {
		t.Fatalf("candidates = %+v, want only deepseek", result.Candidates)
	}
	found := false
	for _, e := range result.Excluded {
		if e.ProviderName == "openai-main" && e.Reason == "cost_cap_reached" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a cost_cap_reached exclusion for openai-main: %+v", result.Excluded)
	}
	if gate.seen == 0 {
		t.Fatal("the gate must be consulted while filtering")
	}
}

// When the cap is the only reason nothing is left, the client gets the dedicated status rather
// than the generic 502: the upstreams are fine, this deployment is out of budget for them.
//
// The fixture here has only capped routes on purpose. A model that also has a disabled or
// draining route keeps the pre-M56 502 — the exclusion set is then mixed, and claiming "every
// provider reached its cap" would be untrue (the reasons are still listed in the explain).
func TestAllCandidatesCappedReportsTheDedicatedError(t *testing.T) {
	snap := registry.NewSnapshot(nil,
		[]*domain.Provider{
			{ID: 1, Name: "primary", Kind: "openai-chat", Enabled: true, Priority: 10},
			{ID: 2, Name: "backup", Kind: "openai-chat", Enabled: true, Priority: 20},
		},
		[]*domain.ProviderModel{
			{ID: 11, ProviderID: 1, PublicModel: "capped-model", UpstreamModel: "a", Enabled: true},
			{ID: 12, ProviderID: 2, PublicModel: "capped-model", UpstreamModel: "b", Enabled: true},
		},
		[]*domain.Model{{ID: 1, PublicName: "capped-model", Enabled: true}},
		nil,
		[]*domain.Route{
			{ID: 101, ModelID: 1, ProviderID: 1, Priority: 10, Weight: 100, Enabled: true},
			{ID: 102, ModelID: 1, ProviderID: 2, Priority: 20, Weight: 100, Enabled: true},
		},
		[]*domain.Tag{{ID: 1, Name: "free", Priority: 10,
			GrantsJSON: `{"models":["capped-model"],"providers":["primary","backup"]}`}},
	)
	router := newRouter(Config{}, snap)
	router.SetCostGate(&costGateStub{capped: map[int64]bool{1: true, 2: true}})

	_, err := router.Plan(domain.RouteRequest{Model: "capped-model", Key: keyWith(`["free"]`, "", "")})
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an APIError, got %v", err)
	}
	if apiErr.Status != 503 || apiErr.Code != "provider_cost_capped" {
		t.Fatalf("error = %d/%s, want 503/provider_cost_capped", apiErr.Status, apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "cost_limit_micros") {
		t.Fatalf("the message must name what to change, got %q", apiErr.Message)
	}
}

// A mix of reasons must keep the existing precedence: an unauthorised key is a permission
// problem, not a budget one.
func TestCostCapDoesNotOverrideAuthorizationErrors(t *testing.T) {
	router, _ := costRouter(10, 20)
	// Only "openai-main" is granted, and it is over its cap, but the other candidate is not
	// granted either: the reason set is mixed, so the answer stays a 502 rather than claiming
	// the whole deployment is out of budget.
	key := keyWith("", `{"models":["gpt-x"],"providers":["openai-main","disabled-prov"]}`, "")
	_, err := router.Plan(domain.RouteRequest{Model: "gpt-x", Key: key})
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an APIError, got %v", err)
	}
	if apiErr.Code == "provider_cost_capped" {
		t.Fatalf("a mixed exclusion set must not report a spent budget: %+v", apiErr)
	}
}

// A pinned request names the upstream it wants; a cap on that upstream still wins, because the
// operator's budget is not something a client can opt out of.
func TestPinnedProviderIsSubjectToItsCap(t *testing.T) {
	router, _ := costRouter(20)
	_, err := router.Plan(domain.RouteRequest{Model: "gpt-x@deepseek", Key: keyWith(`["free"]`, "", "")})
	var apiErr *domain.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an APIError, got %v", err)
	}
	if apiErr.Status != 503 || apiErr.Code != "provider_cost_capped" {
		t.Fatalf("pinned capped provider error = %d/%s", apiErr.Status, apiErr.Code)
	}
}

// An unwired gate is the default: nothing about the candidate list changes.
func TestWithoutACostGateNothingIsExcludedForCost(t *testing.T) {
	router := newRouter(Config{}, fixture())
	result, err := router.Plan(domain.RouteRequest{Model: "gpt-x", Key: keyWith(`["free"]`, "", "")})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want the two usable providers", result.Candidates)
	}
	for _, e := range result.Excluded {
		if e.Reason == "cost_cap_reached" {
			t.Fatalf("a nil gate must not produce cost exclusions: %+v", result.Excluded)
		}
	}
}

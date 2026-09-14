package routing

import (
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

func fixture() *registry.Snapshot {
	cooldown := time.Now().Add(time.Hour).UTC()
	providers := []*domain.Provider{
		{ID: 10, Name: "openai-main", Kind: "openai-responses", Enabled: true, Priority: 10, Weight: 100},
		{ID: 20, Name: "deepseek", Kind: "openai-chat", Enabled: true, Priority: 20, Weight: 100},
		{ID: 30, Name: "disabled-prov", Kind: "openai-chat", Enabled: false},
		{ID: 40, Name: "draining-prov", Kind: "openai-chat", Enabled: true, Draining: true},
		{ID: 50, Name: "cooled", Kind: "openai-chat", Enabled: true, CooldownUntil: &cooldown},
	}
	pms := []*domain.ProviderModel{
		{ID: 100, ProviderID: 10, PublicModel: "gpt-x", UpstreamModel: "gpt-x-2026-01-01", Enabled: true, MaxOutputTokens: 4096, CapabilitiesJSON: capsStreamTools},
		{ID: 101, ProviderID: 20, PublicModel: "gpt-x", UpstreamModel: "deepseek-chat", Enabled: true, MaxOutputTokens: 8192, CapabilitiesJSON: capsStreamOnly},
		{ID: 102, ProviderID: 30, PublicModel: "gpt-x", UpstreamModel: "x", Enabled: true},
		{ID: 103, ProviderID: 40, PublicModel: "gpt-x", UpstreamModel: "x", Enabled: true},
		{ID: 104, ProviderID: 50, PublicModel: "gpt-x", UpstreamModel: "x", Enabled: true},
	}
	models := []*domain.Model{{ID: 1, PublicName: "gpt-x", Enabled: true}}
	routes := []*domain.Route{
		{ID: 200, ModelID: 1, ProviderID: 10, Priority: 10, Weight: 100, Enabled: true},
		{ID: 201, ModelID: 1, ProviderID: 20, Priority: 20, Weight: 100, Enabled: true},
		{ID: 202, ModelID: 1, ProviderID: 30, Priority: 30, Enabled: true},
		{ID: 203, ModelID: 1, ProviderID: 40, Priority: 40, Enabled: true},
		{ID: 204, ModelID: 1, ProviderID: 50, Priority: 50, Enabled: true},
	}
	tags := []*domain.Tag{{
		ID: 1, Name: "free", Priority: 10,
		GrantsJSON: grantAll,
	}}
	return registry.NewSnapshot(nil, providers, pms, models, nil, routes, tags)
}

const (
	capsStreamTools = `{"stream":true,"tools":true}`
	capsStreamOnly  = `{"stream":true}`
	grantAll        = `{"models":["gpt-x"],"providers":["openai-main","deepseek","disabled-prov","draining-prov","cooled"]}`
)

func newRouter(cfg Config, snap *registry.Snapshot) *Router {
	if cfg.DefaultGrant == "" {
		cfg.DefaultGrant = "all"
	}
	if cfg.Degradation == "" {
		cfg.Degradation = "strip"
	}
	return New(cfg, registry.NewStatic(snap), balancer.New(cfg.Breaker))
}

func keyWith(tagsJSON, grantsJSON, policyJSON string) *domain.APIKey {
	return &domain.APIKey{
		ID: 1, AccountID: 1, Name: "test", KeyPrefix: "sk-gw-test", KeyHash: "h",
		TagsJSON: tagsJSON, GrantsJSON: grantsJSON, PolicyJSON: policyJSON, Status: "active",
	}
}

func TestAuthorizeUnionOfKeyAndTags(t *testing.T) {
	r := newRouter(Config{}, fixture())
	key := keyWith(`["free"]`, `{"models":["extra-model"]}`, "")
	tags := r.ResolveTags(fixture(), key)

	grant := r.Authorize(key, tags)
	if !grant.Models["gpt-x"] || !grant.Models["extra-model"] {
		t.Fatalf("union of key and tag model grants expected: %+v", grant.Models)
	}
	if !grant.Providers["deepseek"] || !grant.Providers["disabled-prov"] {
		t.Fatalf("union of provider grants expected: %+v", grant.Providers)
	}
}

func TestResolveTagsUnionsAccountAndKeyTags(t *testing.T) {
	accounts := []*domain.Account{{ID: 1, Name: "acme", TagsJSON: `["account-low","shared","missing"]`}}
	tags := []*domain.Tag{
		{ID: 1, Name: "account-low", Priority: 20},
		{ID: 2, Name: "shared", Priority: 10},
		{ID: 3, Name: "key-high", Priority: 30},
	}
	snap := registry.NewSnapshot(accounts, nil, nil, nil, nil, nil, tags)
	key := keyWith(`["shared","key-high"]`, "", "")

	got := registry.ResolveTagNames(snap, key)
	want := []string{"shared", "account-low", "key-high"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("effective tag order = %v, want %v", got, want)
	}
	if len(got) != 3 {
		t.Fatalf("duplicate account/key tag should appear once: %v", got)
	}
}

func TestAuthorizeWildcardAndDefaultGrantNone(t *testing.T) {
	snap := fixture()
	r := newRouter(Config{DefaultGrant: "none"}, snap)

	bare := keyWith("", "", "")
	grant := r.Authorize(bare, nil)
	if len(grant.Models) != 0 || len(grant.Providers) != 0 {
		t.Fatalf("default_grant=none must deny everything: %+v", grant)
	}
	if _, err := r.Candidates(domain.RouteRequest{Model: "gpt-x", Key: bare}); err == nil {
		t.Fatal("a key without grants must not be routed")
	}

	wildcard := keyWith("", `{"models":["*"],"providers":["*"]}`, "")
	wgrant := r.Authorize(wildcard, nil)
	if !wgrant.Models["*"] || !wgrant.Providers["*"] {
		t.Fatalf("wildcard grants expected: %+v", wgrant)
	}
}

func TestAuthorizeDefaultGrantAll(t *testing.T) {
	r := newRouter(Config{DefaultGrant: "all"}, fixture())
	grant := r.Authorize(keyWith("", "", ""), nil)
	if !grant.Models["*"] || !grant.Providers["*"] {
		t.Fatalf("default_grant=all must grant everything: %+v", grant)
	}
}

func TestCandidatesFilterOutUnusableRoutes(t *testing.T) {
	r := newRouter(Config{}, fixture())
	key := keyWith(`["free"]`, "", "")

	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("expected 2 usable candidates, got %d: %+v", len(res.Candidates), res.Candidates)
	}
	if res.Candidates[0].ProviderName != "openai-main" || res.Candidates[1].ProviderName != "deepseek" {
		t.Fatalf("priority order wrong: %+v", res.Candidates)
	}
	if got := res.Candidates[0].UpstreamModel; got != "gpt-x-2026-01-01" {
		t.Fatalf("upstream model mismatch: %q", got)
	}

	reasons := map[string]string{}
	for _, e := range res.Excluded {
		reasons[e.ProviderName] = e.Reason
	}
	for provider, want := range map[string]string{
		"disabled-prov": "disabled",
		"draining-prov": "draining",
		"cooled":        startswithCooldown,
	} {
		if !strings.HasPrefix(reasons[provider], want) {
			t.Errorf("exclusion for %s = %q, want prefix %q", provider, reasons[provider], want)
		}
	}
}

const startswithCooldown = "cooldown_until="

func TestNotGrantedProviderIsExcluded(t *testing.T) {
	r := newRouter(Config{DefaultGrant: "none"}, fixture())
	key := keyWith("", `{"models":["gpt-x"],"providers":["deepseek"]}`, "")

	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].ProviderName != "deepseek" {
		t.Fatalf("only the granted provider may be used: %+v", res.Candidates)
	}
	if len(res.Excluded) == 0 || res.Excluded[0].Reason != "not_granted" {
		t.Fatalf("expected a not_granted exclusion: %+v", res.Excluded)
	}

	// Nothing granted at all -> 403.
	empty := keyWith("", `{"models":["gpt-x"],"providers":["nope"]}`, "")
	_, err = r.Candidates(domain.RouteRequest{Model: "gpt-x", Key: empty})
	if !domain.IsForbidden(err) {
		t.Fatalf("expected 403 for an unauthorised provider, got %v", err)
	}
}

func TestCapabilityStripVersusReject(t *testing.T) {
	features := map[string]bool{"tools": true, "stream": true}

	strip := newRouter(Config{Degradation: "strip"}, fixture())
	key := keyWith(`["free"]`, "", "")
	res, err := strip.Plan(domain.RouteRequest{Model: "gpt-x", Key: key, Features: features})
	if err != nil {
		t.Fatal(err)
	}
	var deepseek *domain.Candidate
	for i := range res.Candidates {
		if res.Candidates[i].ProviderName == "deepseek" {
			deepseek = &res.Candidates[i]
		}
	}
	if deepseek == nil {
		t.Fatalf("strip mode must keep a candidate that lacks a strippable feature: %+v", res.Candidates)
	}
	if len(deepseek.Degraded) != 1 || deepseek.Degraded[0] != "tools" {
		t.Fatalf("degraded features not recorded: %+v", deepseek.Degraded)
	}

	reject := newRouter(Config{Degradation: "reject", DefaultGrant: "none"}, fixture())
	onlyDeepseek := keyWith("", `{"models":["gpt-x"],"providers":["deepseek"]}`, "")
	_, err = reject.Candidates(domain.RouteRequest{Model: "gpt-x", Key: onlyDeepseek, Features: features})
	if !domain.HasStatus(err, 400) {
		t.Fatalf("reject mode must fail with 400 when a feature is unsupported, got %v", err)
	}
}

func TestStrategyOverrideAndStrictOrder(t *testing.T) {
	r := newRouter(Config{DefaultStrategy: string(balancer.WeightedRandom)}, fixture())
	key := keyWith(`["free"]`, "", "")

	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x", Key: key, Strategy: string(balancer.StrictOrder)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != string(balancer.StrictOrder) {
		t.Fatalf("strategy override ignored: %q", res.Strategy)
	}
	if res.Candidates[0].ProviderName != "openai-main" {
		t.Fatalf("strict order must keep priority order: %+v", res.Candidates)
	}
}

func TestPolicyFromKeyOverridesTag(t *testing.T) {
	snap := fixture()
	// Give the tag a round_robin policy and the key an explicit strict_order override.
	snap.Tags[0].PolicyJSON = `{"strategy":"round_robin"}`
	r := newRouter(Config{DefaultStrategy: string(balancer.WeightedRandom)}, snap)

	tagOnly := keyWith(`["free"]`, "", "")
	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x", Key: tagOnly})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != string(balancer.RoundRobin) {
		t.Fatalf("tag policy not applied: %q", res.Strategy)
	}

	withKey := keyWith(`["free"]`, "", `{"strategy":"strict_order"}`)
	res, err = r.Plan(domain.RouteRequest{Model: "gpt-x", Key: withKey})
	if err != nil {
		t.Fatal(err)
	}
	if res.Strategy != string(balancer.StrictOrder) {
		t.Fatalf("key policy must override the tag policy: %q", res.Strategy)
	}
}

func TestPinnedProviderSkipsRouting(t *testing.T) {
	r := newRouter(Config{}, fixture())
	key := keyWith(`["free"]`, "", "")

	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x@deepseek", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].ProviderName != "deepseek" {
		t.Fatalf("pin must yield exactly one candidate: %+v", res.Candidates)
	}
	if res.Candidates[0].UpstreamModel != "deepseek-chat" {
		t.Fatalf("pinned upstream mismatch: %q", res.Candidates[0].UpstreamModel)
	}
	if !res.Resolved.Pinned {
		t.Fatal("resolved model must be marked pinned")
	}
}

func TestCircuitOpenExcludesRoute(t *testing.T) {
	r := newRouter(Config{}, fixture())
	key := keyWith(`["free"]`, "", "")

	// Trip the breaker for the best route.
	for i := 0; i < 10; i++ {
		r.Balancer().Observe(RouteKey(200), 5, false)
	}
	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Candidates {
		if c.ProviderName == "openai-main" {
			t.Fatalf("a circuit-open route must be excluded: %+v", res.Candidates)
		}
	}
	found := false
	for _, e := range res.Excluded {
		if e.ProviderName == "openai-main" && e.Reason == "circuit_open" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a circuit_open exclusion: %+v", res.Excluded)
	}
}

func TestModelNotFoundAndUnknownModel(t *testing.T) {
	r := newRouter(Config{DefaultGrant: "all"}, fixture())
	key := keyWith("", "", "")
	if _, err := r.Candidates(domain.RouteRequest{Model: "does-not-exist", Key: key}); !domain.IsNotFound(err) {
		t.Fatalf("expected model_not_found, got %v", err)
	}

	// Explain degrades gracefully for unknown models instead of failing.
	expl, err := r.Explain(domain.RouteRequest{Model: "does-not-exist", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if expl.Requested != "does-not-exist" || len(expl.Excluded) == 0 {
		t.Fatalf("explain mismatch: %+v", expl)
	}
}

func TestExplainReportsResolutionChainAndOrder(t *testing.T) {
	r := newRouter(Config{}, fixture())
	key := keyWith(`["free"]`, "", "")

	expl, err := r.Explain(domain.RouteRequest{Model: "gpt-x", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if expl.Canonical != "gpt-x" || !strings.HasPrefix(expl.Rule, "model:") {
		t.Fatalf("resolution chain mismatch: %+v", expl)
	}
	if len(expl.Order) != 2 || len(expl.Excluded) != 3 {
		t.Fatalf("explain order/excluded mismatch: order=%d excluded=%d", len(expl.Order), len(expl.Excluded))
	}
}

func TestNotMappedProviderIsExcluded(t *testing.T) {
	snap := fixture()
	// Remove the deepseek mapping for gpt-x.
	filtered := snap.ProviderModels[:0]
	for _, pm := range snap.ProviderModels {
		if pm.ProviderID == 20 {
			continue
		}
		filtered = append(filtered, pm)
	}
	snap = registry.NewSnapshot(nil, snap.Providers, filtered, snap.Models, nil, snap.Routes, snap.Tags)

	r := newRouter(Config{}, snap)
	key := keyWith(`["free"]`, "", "")
	res, err := r.Plan(domain.RouteRequest{Model: "gpt-x", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Excluded {
		if e.ProviderName == "deepseek" && e.Reason == "not_mapped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a not_mapped exclusion: %+v", res.Excluded)
	}
}

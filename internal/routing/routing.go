// Package routing turns a request into an ordered list of provider candidates.
//
// It is a pure function over a registry snapshot plus balancer state, so the admin
// "simulate routing" tool and the data path share exactly the same decision code.
package routing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/modelmap"
	"github.com/winger/ai-gateway/internal/registry"
)

// Config mirrors the routing section of the configuration file.
type Config struct {
	DefaultStrategy string
	Degradation     string // strip | reject
	DefaultGrant    string // all | none
	ModelFallback   string
	Breaker         balancer.Config

	// SessionAffinity keeps a client session on the route that last served it
	// (docs/routing.md §4.4). AffinityTTL and AffinityMaxEntries fall back to
	// DefaultAffinityTTL / DefaultAffinityMaxEntries when they are left at zero.
	SessionAffinity    bool
	AffinityTTL        time.Duration
	AffinityMaxEntries int
}

// CostGate reports whether a provider has spent its configured cost cap (M56). A nil gate
// means this deployment does not enforce cost caps, which is the default wiring and what keeps
// the filter out of the way of tests.
//
// It is an interface rather than a direct dependency because the implementation reads the
// metering table, and `internal/routing` plans against the in-memory snapshot only
// (internal/arch/layering_test.go forbids importing the store from here).
type CostGate interface {
	Exceeded(p *domain.Provider, now time.Time) bool
}

// Router computes candidate lists.
type Router struct {
	cfg   Config
	reg   *registry.Registry
	bal   *balancer.State
	mm    *modelmap.Resolver
	aff   *affinityStore
	costs CostGate
}

// SetCostGate wires the provider cost cap (M56). It is a setter rather than another New
// parameter because the tracker is built over the same registry, and a nil gate is the correct
// wiring for every deployment (and test) that does not cap provider spend.
func (r *Router) SetCostGate(g CostGate) { r.costs = g }

// New builds a router over the given registry and balancer state.
func New(cfg Config, reg *registry.Registry, state *balancer.State) *Router {
	if cfg.DefaultStrategy == "" {
		cfg.DefaultStrategy = string(balancer.WeightedRandom)
	}
	if cfg.Degradation == "" {
		cfg.Degradation = "strip"
	}
	if cfg.DefaultGrant == "" {
		cfg.DefaultGrant = "all"
	}
	if cfg.SessionAffinity {
		if cfg.AffinityTTL <= 0 {
			cfg.AffinityTTL = DefaultAffinityTTL
		}
		if cfg.AffinityMaxEntries <= 0 {
			cfg.AffinityMaxEntries = DefaultAffinityMaxEntries
		}
	} else {
		// Keep the resolved config honest: with the feature off the bounds are unread.
		cfg.AffinityTTL = 0
		cfg.AffinityMaxEntries = 0
	}
	if state == nil {
		state = balancer.New(cfg.Breaker)
	}
	return &Router{
		cfg: cfg, reg: reg, bal: state, mm: modelmap.New(cfg.ModelFallback),
		aff: newAffinityStore(cfg.AffinityTTL, cfg.AffinityMaxEntries),
	}
}

// Balancer exposes the runtime state (used by /stats and tests).
func (r *Router) Balancer() *balancer.State { return r.bal }

// Result bundles the ordered candidates with the diagnostics that explain them.
type Result struct {
	Resolved   *domain.ResolvedModel
	Grant      *domain.Grant
	Candidates []domain.Candidate
	Excluded   []domain.Exclusion
	Strategy   string
	// Reasoning is the canonical model policy captured from this routing snapshot.
	Reasoning *domain.ModelReasoning
	// Affinity is this request's sticky-routing slot, and "" when the request does not
	// take part in stickiness at all: the feature is off, the client sent no session
	// key, no key was identified, or the request pinned a provider. It is opaque on
	// purpose — a caller reports the route it actually used back through NoteSuccess /
	// NoteFailure and cannot mint a slot of its own.
	Affinity string
}

type grantsWire struct {
	Models    []string `json:"models"`
	Providers []string `json:"providers"`
}

type policyWire struct {
	Strategy      string   `json:"strategy"`
	ProviderOrder []string `json:"provider_order"`
	// MarginBP is the customer-facing multiplier in basis points (10000 = 1.0x).
	MarginBP *int `json:"margin_bp"`
}

// ResolveTags maps account and key tag names onto tag records (ordered by priority).
// Account tags are prepended to key tags before de-duplication, so equal-priority
// policies have a deterministic order while the key's own policy still applies last.
func (r *Router) ResolveTags(snap *registry.Snapshot, key *domain.APIKey) []*domain.Tag {
	return ResolveTagRecords(snap, key)
}

// ResolveTagRecords delegates effective account/key tag resolution to the registry package.
// The result is derived from the immutable snapshot and never queries the database.
func ResolveTagRecords(snap *registry.Snapshot, key *domain.APIKey) []*domain.Tag {
	return registry.ResolveTagRecords(snap, key)
}

// ResolveTagNames returns effective existing tag names in evaluation order.
func ResolveTagNames(snap *registry.Snapshot, key *domain.APIKey) []string {
	return registry.ResolveTagNames(snap, key)
}

// Authorize computes the effective grant as the UNION of the key's own grants and
// the grants of every effective account/key tag.
func (r *Router) Authorize(key *domain.APIKey, tags []*domain.Tag) *domain.Grant {
	models := map[string]bool{}
	providers := map[string]bool{}
	anyGrant := false

	collect := func(raw string) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		var g grantsWire
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			return
		}
		for _, m := range g.Models {
			if m != "" {
				models[m] = true
				anyGrant = true
			}
		}
		for _, p := range g.Providers {
			if p != "" {
				providers[p] = true
				anyGrant = true
			}
		}
	}

	if key != nil {
		collect(key.GrantsJSON)
	}
	for _, t := range tags {
		collect(t.GrantsJSON)
	}

	if !anyGrant && r.cfg.DefaultGrant != "none" {
		models["*"] = true
		providers["*"] = true
	}

	return &domain.Grant{Models: models, Providers: providers, Policy: mergePolicy(key, tags)}
}

// mergePolicy applies tag policies (ascending priority) then the key policy on top.
func mergePolicy(key *domain.APIKey, tags []*domain.Tag) domain.Policy {
	var out domain.Policy
	apply := func(raw string, override bool) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		var pw policyWire
		if err := json.Unmarshal([]byte(raw), &pw); err != nil {
			return
		}
		if pw.Strategy != "" {
			out.Strategy = pw.Strategy
			out.StrategySet = true
		}
		if len(pw.ProviderOrder) > 0 {
			out.ProviderOrder = pw.ProviderOrder
		}
		if pw.MarginBP != nil && *pw.MarginBP >= 0 {
			out.MarginBP = *pw.MarginBP
			out.MarginSet = true
		}
	}
	for _, t := range tags {
		apply(t.PolicyJSON, false)
	}
	if key != nil {
		apply(key.PolicyJSON, true)
	}
	return out
}

// Strategy resolves the effective strategy for a request.
func (r *Router) Strategy(in domain.RouteRequest, grant *domain.Grant) string {
	if in.Strategy != "" && balancer.Valid(in.Strategy) {
		return in.Strategy
	}
	if grant != nil && grant.Policy.StrategySet && balancer.Valid(grant.Policy.Strategy) {
		return grant.Policy.Strategy
	}
	if balancer.Valid(r.cfg.DefaultStrategy) {
		return r.cfg.DefaultStrategy
	}
	return string(balancer.WeightedRandom)
}

// Plan resolves the model, applies authorization and returns ordered candidates.
func (r *Router) Plan(in domain.RouteRequest) (*Result, error) {
	snap := r.reg.Snapshot()

	tags := in.Tags
	if len(tags) == 0 && in.Key != nil {
		tags = r.ResolveTags(snap, in.Key)
	}
	grant := in.Grant
	if grant == nil {
		grant = r.Authorize(in.Key, tags)
	}

	resolved, err := r.mm.Resolve(snap, in.Model, in.ProviderPin)
	if err != nil {
		return nil, err
	}

	res := &Result{Resolved: resolved, Grant: grant, Strategy: r.Strategy(in, grant)}
	if model := snap.ModelByName[resolved.Canonical]; model != nil {
		res.Reasoning, err = domain.ParseModelReasoning(model.ReasoningJSON)
		if err != nil {
			return nil, err
		}
		if res.Reasoning != nil {
			// Both modes produce a nonempty effort: default either preserves the
			// client's explicit effort or supplies the configured one. Copy the
			// caller's feature map rather than leaking this model's policy to it.
			features := make(map[string]bool, len(in.Features)+1)
			for name, enabled := range in.Features {
				features[name] = enabled
			}
			features["reasoning"] = true
			in.Features = features
		}
	}
	now := time.Now()

	if resolved.Pinned {
		// A pinned request states its own provider, so it neither reads nor writes a
		// sticky binding: Affinity stays "" and the Note* calls that follow are no-ops.
		cand, excl := r.buildPinned(snap, resolved, in, grant, res.Strategy, now)
		if excl != "" {
			res.Excluded = append(res.Excluded, domain.Exclusion{ProviderName: resolved.ProviderName, Reason: excl})
			return res, r.noCandidatesError(res)
		}
		res.Candidates = []domain.Candidate{*cand}
		return res, nil
	}

	// Stickiness needs a client session and an identified key; without either the
	// request is routed exactly as it was before this feature existed. strict_order
	// promises a fixed order ("永不打散，确定性"), so such a request does not take part
	// at all rather than binding a route that would never be used to reorder anything.
	if r.aff != nil && in.SessionID != "" && res.Strategy != string(balancer.StrictOrder) {
		keyID := in.KeyID
		if in.Key != nil {
			keyID = in.Key.ID
		}
		if keyID > 0 {
			res.Affinity = affinityKey(keyID, in.SessionID, resolved.Canonical)
		}
	}

	model := snap.ModelByName[resolved.Canonical]
	if model == nil || !model.Enabled {
		return nil, domain.ErrModelNotFound(resolved.Requested)
	}
	routes := snap.RoutesFor(model.ID)
	if len(routes) == 0 {
		return nil, domain.ErrModelNotFound(resolved.Requested)
	}

	type tierEntry struct {
		priority int
		target   balancer.Target
		cand     domain.Candidate
	}
	entries := make([]tierEntry, 0, len(routes))

	for _, route := range routes {
		provider := snap.ProviderByID[route.ProviderID]
		if provider == nil {
			res.Excluded = append(res.Excluded, domain.Exclusion{Reason: "unknown_provider"})
			continue
		}
		if reason := r.filterRoute(snap, provider, route, resolved, in, grant, now); reason != "" {
			res.Excluded = append(res.Excluded, domain.Exclusion{ProviderName: provider.Name, Reason: reason})
			continue
		}

		pm := snap.ProviderModel(provider.ID, resolved.Canonical)
		upstream := strings.TrimSpace(route.UpstreamModel)
		if upstream == "" && pm != nil {
			upstream = pm.UpstreamModel
		}
		if upstream == "" {
			upstream = resolved.Canonical
		}
		upstream = modelmap.ApplyTemplate(upstream, resolved.Requested, resolved.Groups)

		degraded, ok, missing := checkCapabilities(pm, in.Features, r.cfg.Degradation)
		if !ok {
			res.Excluded = append(res.Excluded, domain.Exclusion{
				ProviderName: provider.Name,
				Reason:       "missing_capability:" + strings.Join(missing, ","),
			})
			continue
		}

		weight := normaliseWeight(route.Weight) * normaliseWeight(provider.Weight)
		entries = append(entries, tierEntry{
			priority: route.Priority,
			target: balancer.Target{
				Key:             routeKey(route.ID),
				Weight:          weight,
				MaxOutputTokens: maxOutput(pm),
				Order:           route.Priority,
				Reasoning:       isReasoning(pm),
			},
			cand: domain.Candidate{
				RouteID:       route.ID,
				ProviderID:    provider.ID,
				ProviderName:  provider.Name,
				UpstreamModel: upstream,
				Priority:      route.Priority,
				Weight:        weight,
				Degraded:      degraded,
				Strategy:      res.Strategy,
			},
		})
	}

	if len(entries) == 0 {
		return res, r.noCandidatesError(res)
	}

	// Group by priority tier, order inside each tier, then concatenate tiers.
	byPriority := map[int][]tierEntry{}
	priorities := []int{}
	for _, e := range entries {
		if _, seen := byPriority[e.priority]; !seen {
			priorities = append(priorities, e.priority)
		}
		byPriority[e.priority] = append(byPriority[e.priority], e)
	}
	sort.Ints(priorities)

	for _, p := range priorities {
		tier := byPriority[p]
		if len(grant.Policy.ProviderOrder) > 0 {
			rank := providerRank(grant.Policy.ProviderOrder)
			sort.SliceStable(tier, func(i, j int) bool {
				return rank[tier[i].cand.ProviderName] < rank[tier[j].cand.ProviderName]
			})
		}
		targets := make([]balancer.Target, 0, len(tier))
		byKey := map[string]domain.Candidate{}
		for _, e := range tier {
			targets = append(targets, e.target)
			byKey[e.target.Key] = e.cand
		}
		for _, t := range r.bal.Order(balancer.Strategy(res.Strategy), targets) {
			res.Candidates = append(res.Candidates, byKey[t.Key])
		}
	}

	// Sticky ordering is applied last, over candidates that have already passed every
	// filter: it can reorder what this request may use, never extend it.
	if res.Affinity != "" {
		if routeID, ok := r.aff.get(res.Affinity, now); ok {
			if !promoteWithinTier(res.Candidates, routeID) {
				// The bound route is not usable for this request any more (revoked,
				// disabled, draining, cooling, breaker open, capability lost): forget
				// the binding instead of keeping it to surprise the session later.
				r.aff.dropStale(res.Affinity)
			}
		}
	}
	return res, nil
}

// NoteSuccess binds the session to the route that just served it, refreshing the binding's
// TTL. Call it only for a request whose Result.Affinity was non-empty and whose attempt
// actually succeeded.
func (r *Router) NoteSuccess(affinity string, routeID int64) {
	r.aff.put(affinity, routeID, time.Now())
}

// NoteFailure drops the session's binding, and only while it still points at routeID: a
// retryable failure of the bound route means the next request of this session should not
// walk back into it. It reports whether a binding was dropped, so the caller can log the
// failover. Call it only for retryable failures — a client error says nothing about the
// health of the route, and moving the session for one would be noise.
func (r *Router) NoteFailure(affinity string, routeID int64) bool {
	return r.aff.drop(affinity, routeID)
}

// AffinityStats reports the session stickiness table (used by /stats).
func (r *Router) AffinityStats() AffinityStats { return r.aff.stats() }

// Candidates returns only the ordered candidate list.
func (r *Router) Candidates(in domain.RouteRequest) ([]domain.Candidate, error) {
	res, err := r.Plan(in)
	if err != nil {
		return nil, err
	}
	return res.Candidates, nil
}

// Explain returns the full diagnostic view (used by the simulate-routing UI).
//
// Unlike Plan, a request that resolves but has no usable candidate is *not* an
// error here: the exclusion list is the answer the caller asked for, so it comes
// back with a nil error and the reason in Failure. Only an unresolvable request
// (unknown model, no routes at all) returns an error.
func (r *Router) Explain(in domain.RouteRequest) (*domain.RouteExplanation, error) {
	res, err := r.Plan(in)
	if err != nil && res == nil {
		if apiErr, ok := domain.AsAPIError(err); ok && apiErr.Code == "model_not_found" {
			return &domain.RouteExplanation{Requested: in.Model, Excluded: []domain.Exclusion{{Reason: apiErr.Code}}}, nil
		}
		return nil, err
	}
	out := &domain.RouteExplanation{
		Requested: res.Resolved.Requested,
		Canonical: res.Resolved.Canonical,
		Rule:      res.Resolved.MatchedRule,
		Order:     res.Candidates,
		Excluded:  res.Excluded,
	}
	if err != nil {
		out.Failure = err.Error()
	}
	return out, nil
}

// filterRoute returns "" when the route is usable, or the exclusion reason.
func (r *Router) filterRoute(
	snap *registry.Snapshot,
	provider *domain.Provider,
	route *domain.Route,
	resolved *domain.ResolvedModel,
	in domain.RouteRequest,
	grant *domain.Grant,
	now time.Time,
) string {
	if !provider.Enabled || !route.Enabled {
		return "disabled"
	}
	if provider.Draining {
		return "draining"
	}
	if !grantedName(grant.Providers, provider.Name) {
		return "not_granted"
	}
	if provider.CooldownUntil != nil && provider.CooldownUntil.After(now) {
		return "cooldown_until=" + provider.CooldownUntil.UTC().Format(time.RFC3339)
	}
	if route.CooldownUntil != nil && route.CooldownUntil.After(now) {
		return "cooldown_until=" + route.CooldownUntil.UTC().Format(time.RFC3339)
	}
	if allowed, _ := r.bal.Allow(routeKey(route.ID), now); !allowed {
		return "circuit_open"
	}
	// The provider has spent what the operator allowed it to spend (M56). It is checked with
	// the other availability filters rather than at dispatch time, so the request moves on to
	// the next candidate instead of paying for a doomed attempt.
	if r.costs != nil && r.costs.Exceeded(provider, now) {
		return "cost_cap_reached"
	}
	if pm := snap.ProviderModel(provider.ID, resolved.Canonical); pm == nil || !pm.Enabled {
		return "not_mapped"
	}
	// Authorization on the canonical model: the model must be granted, and either it
	// is reachable as a canonical name or via a mapping that produced it.
	if !grantedName(grant.Models, resolved.Canonical) && !grantedName(grant.Models, resolved.Requested) {
		return "not_granted"
	}
	return ""
}

func (r *Router) buildPinned(
	snap *registry.Snapshot,
	resolved *domain.ResolvedModel,
	in domain.RouteRequest,
	grant *domain.Grant,
	strategy string,
	now time.Time,
) (*domain.Candidate, string) {
	provider := snap.ProviderByID[resolved.ProviderID]
	if provider == nil {
		return nil, "unknown_provider"
	}
	if !provider.Enabled {
		return nil, "disabled"
	}
	if provider.Draining {
		return nil, "draining"
	}
	if !grantedName(grant.Providers, provider.Name) {
		return nil, "not_granted"
	}
	if provider.CooldownUntil != nil && provider.CooldownUntil.After(now) {
		return nil, "cooldown_until=" + provider.CooldownUntil.UTC().Format(time.RFC3339)
	}
	// A pinned provider is a client/operator statement about *which* upstream to use, not an
	// authorization to spend past the cap this deployment put on it (M56).
	if r.costs != nil && r.costs.Exceeded(provider, now) {
		return nil, "cost_cap_reached"
	}
	pm := snap.ProviderModel(provider.ID, resolved.Canonical)
	if pm == nil || !pm.Enabled {
		return nil, "not_mapped"
	}
	degraded, ok, missing := checkCapabilities(pm, in.Features, r.cfg.Degradation)
	if !ok {
		return nil, "missing_capability:" + strings.Join(missing, ",")
	}
	upstream := resolved.Upstream
	if upstream == "" {
		upstream = pm.UpstreamModel
	}
	if upstream == "" {
		upstream = resolved.Canonical
	}
	upstream = modelmap.ApplyTemplate(upstream, resolved.Requested, resolved.Groups)

	return &domain.Candidate{
		ProviderID:    provider.ID,
		ProviderName:  provider.Name,
		UpstreamModel: upstream,
		Priority:      0,
		Weight:        normaliseWeight(provider.Weight),
		Degraded:      degraded,
		Strategy:      strategy,
	}, ""
}

// noCandidatesError maps the exclusion reasons onto a client-visible error.
func (r *Router) noCandidatesError(res *Result) error {
	var notGranted, capability bool
	var firstCapability string
	capped := len(res.Excluded) > 0
	for _, e := range res.Excluded {
		switch {
		case e.Reason == "not_granted":
			notGranted = true
		case strings.HasPrefix(e.Reason, "missing_capability:"):
			capability = true
			if firstCapability == "" {
				firstCapability = strings.TrimPrefix(e.Reason, "missing_capability:")
			}
		}
		if e.Reason != "cost_cap_reached" {
			capped = false
		}
	}
	model := ""
	if res.Resolved != nil {
		model = res.Resolved.Canonical
	}
	// A capability rejection is reported first: it means the key *is* authorised for at
	// least one provider, but none of them can serve the requested features.
	if capability {
		if notGranted {
			return domain.ErrUnsupported("no authorised provider supports the requested features (" +
				firstCapability + ") for model " + model)
		}
		return domain.ErrUnsupported("no provider supports the requested features (" + firstCapability + ") for model " + model)
	}
	if notGranted {
		return domain.ErrForbidden("model or provider not allowed for this API key: " + model)
	}
	// Every candidate was dropped for the same operational reason, so report that reason
	// instead of the generic "no available provider": an operator reading a 502 goes looking
	// at the upstreams, while a spent budget is a deployment fact (M56).
	if capped {
		return domain.ErrProviderCostCapped("every provider for model " + model +
			" has reached its cost cap; raise providers.cost_limit_micros or reset the accumulated cost")
	}
	return domain.ErrUpstream(502, "no available provider for model "+model)
}

// checkCapabilities reports whether pm supports every requested feature.
// Unknown capabilities (empty JSON) are treated as permissive.
func checkCapabilities(pm *domain.ProviderModel, features map[string]bool, degradation string) (degraded []string, ok bool, missing []string) {
	if len(features) == 0 || pm == nil {
		return nil, true, nil
	}
	caps := capabilitiesOf(pm)
	if caps == nil {
		return nil, true, nil // unknown: do not block
	}
	for feature, want := range features {
		if !want {
			continue
		}
		if !caps[feature] {
			missing = append(missing, feature)
		}
	}
	if len(missing) == 0 {
		return nil, true, nil
	}
	sort.Strings(missing)
	if degradation == "strip" {
		return missing, true, missing
	}
	return nil, false, missing
}

// capabilitiesOf is the in-package spelling of EffectiveCapabilities, which lives in
// capabilities.go because the model listing discloses the same resolution to clients.
func capabilitiesOf(pm *domain.ProviderModel) map[string]bool {
	return EffectiveCapabilities(pm)
}

func isReasoning(pm *domain.ProviderModel) bool {
	caps := capabilitiesOf(pm)
	return caps != nil && caps["reasoning"]
}

func maxOutput(pm *domain.ProviderModel) int {
	if pm == nil {
		return 0
	}
	return pm.MaxOutputTokens
}

func grantedName(set map[string]bool, name string) bool {
	if set == nil {
		return false
	}
	return set["*"] || set[name]
}

func normaliseWeight(w int) int {
	if w <= 0 {
		return 100
	}
	return w
}

func providerRank(order []string) map[string]int {
	rank := make(map[string]int, len(order))
	for i, name := range order {
		rank[name] = i
	}
	return rank
}

func routeKey(routeID int64) string { return fmt.Sprintf("route:%d", routeID) }

// RouteKey exposes the balancer key format for tests and admin diagnostics.
func RouteKey(routeID int64) string { return routeKey(routeID) }

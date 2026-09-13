package routing

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// affinityFixture has three providers of one model: two sharing a priority tier and one in
// the next tier down. Promotion inside a tier is only observable when a tier holds more
// than one candidate, and "does not cross tiers" is only observable when something sits
// ahead of the bound route in a higher tier. The M3 fixture gives every provider a tier of
// its own (10/20/30/...), so neither is visible there.
func affinityFixture() *registry.Snapshot {
	providers := []*domain.Provider{
		{ID: 10, Name: "alpha", Kind: "openai-chat", Enabled: true, Weight: 100},
		{ID: 20, Name: "beta", Kind: "openai-chat", Enabled: true, Weight: 100},
		{ID: 30, Name: "gamma", Kind: "openai-chat", Enabled: true, Weight: 100},
	}
	pms := []*domain.ProviderModel{
		{ID: 100, ProviderID: 10, PublicModel: "gpt-x", UpstreamModel: "alpha-x", Enabled: true,
			MaxOutputTokens: 4096, CapabilitiesJSON: capsStreamTools},
		{ID: 101, ProviderID: 20, PublicModel: "gpt-x", UpstreamModel: "beta-x", Enabled: true,
			MaxOutputTokens: 4096, CapabilitiesJSON: capsStreamTools},
		{ID: 102, ProviderID: 30, PublicModel: "gpt-x", UpstreamModel: "gamma-x", Enabled: true,
			MaxOutputTokens: 4096, CapabilitiesJSON: capsStreamTools},
	}
	models := []*domain.Model{{ID: 1, PublicName: "gpt-x", Enabled: true}}
	routes := []*domain.Route{
		{ID: 200, ModelID: 1, ProviderID: 10, Priority: 10, Weight: 100, Enabled: true},
		{ID: 201, ModelID: 1, ProviderID: 20, Priority: 10, Weight: 100, Enabled: true},
		{ID: 202, ModelID: 1, ProviderID: 30, Priority: 20, Weight: 100, Enabled: true},
	}
	return registry.NewSnapshot(nil, providers, pms, models, nil, routes, nil)
}

// newAffinityRouter builds a router with stickiness on and a *deterministic* strategy:
// least_inflight breaks ties by weight and then keeps the registry order, so "was the
// candidate promoted?" is decidable without depending on a random draw.
func newAffinityRouter(t *testing.T, snap *registry.Snapshot) *Router {
	t.Helper()
	return New(Config{
		DefaultStrategy:    string(balancer.LeastInflight),
		DefaultGrant:       "all",
		Degradation:        "strip",
		SessionAffinity:    true,
		AffinityTTL:        time.Hour,
		AffinityMaxEntries: 100,
	}, registry.NewStatic(snap), balancer.New(balancer.DefaultConfig()))
}

func planOne(t *testing.T, r *Router, in domain.RouteRequest) *Result {
	t.Helper()
	res, err := r.Plan(in)
	if err != nil {
		t.Fatalf("Plan(%+v) failed: %v", in, err)
	}
	return res
}

// slotFor plans one request purely for its sticky slot, failing when it has none.
func slotFor(t *testing.T, r *Router, in domain.RouteRequest) string {
	t.Helper()
	res := planOne(t, r, in)
	if res.Affinity == "" {
		t.Fatalf("expected a sticky slot for %+v", in)
	}
	return res.Affinity
}

func firstRoute(res *Result) int64 {
	if len(res.Candidates) == 0 {
		return 0
	}
	return res.Candidates[0].RouteID
}

// routeIDs renders the candidate order, so a test can pin the whole sequence rather than
// only its head.
func routeIDs(res *Result) string {
	parts := make([]string, 0, len(res.Candidates))
	for _, cand := range res.Candidates {
		parts = append(parts, strconv.FormatInt(cand.RouteID, 10))
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// the table itself
// ---------------------------------------------------------------------------

func TestAffinityStoreExpiresAndRefreshesOnHit(t *testing.T) {
	store := newAffinityStore(time.Minute, 10)
	now := time.Now()

	store.put("k", 7, now)
	if id, ok := store.get("k", now.Add(30*time.Second)); !ok || id != 7 {
		t.Fatalf("a live binding must be returned: id=%d ok=%t", id, ok)
	}
	// The hit above moved the deadline to +30s, so 59 seconds later the entry is alive
	// even though more than one TTL has passed since it was created.
	if _, ok := store.get("k", now.Add(89*time.Second)); !ok {
		t.Fatal("a hit must refresh the TTL")
	}
	// ...and dead once a whole TTL has passed since that refresh.
	if id, ok := store.get("k", now.Add(150*time.Second)); ok {
		t.Fatalf("an expired binding must not be returned (id=%d)", id)
	}
	if stats := store.stats(); stats.Entries != 0 {
		t.Fatalf("the expired entry must be dropped on read, entries = %d", stats.Entries)
	}
}

func TestAffinityStoreEvictsTheOldestEntryAtCapacity(t *testing.T) {
	store := newAffinityStore(time.Hour, 2)
	now := time.Now()

	store.put("a", 1, now)
	store.put("b", 2, now.Add(time.Second))
	store.put("c", 3, now.Add(2*time.Second))

	if _, ok := store.get("a", now.Add(3*time.Second)); ok {
		t.Fatal("the oldest binding must be evicted when the table is full")
	}
	if _, ok := store.get("b", now.Add(3*time.Second)); !ok {
		t.Fatal("the newer binding must survive")
	}
	if _, ok := store.get("c", now.Add(3*time.Second)); !ok {
		t.Fatal("the newest binding must survive")
	}
	if stats := store.stats(); stats.Entries > 2 || stats.Evictions != 1 {
		t.Fatalf("capacity must be enforced: %+v", stats)
	}
}

func TestAffinityStoreIsInertWhenDisabled(t *testing.T) {
	if store := newAffinityStore(0, 10); store != nil {
		t.Fatal("a zero TTL must produce no store at all")
	}
	if store := newAffinityStore(time.Minute, 0); store != nil {
		t.Fatal("a zero capacity must produce no store at all")
	}

	var store *affinityStore // the "feature off" value every method has to tolerate
	store.put("k", 1, time.Now())
	if _, ok := store.get("k", time.Now()); ok {
		t.Fatal("a disabled store must never hit")
	}
	if store.drop("k", 1) {
		t.Fatal("a disabled store must not report drops")
	}
	store.dropStale("k") // must not panic
	if stats := store.stats(); stats.Enabled || stats.Entries != 0 {
		t.Fatalf("a disabled store reports disabled: %+v", stats)
	}
}

func TestAffinityStoreIsSafeForConcurrentUse(t *testing.T) {
	store := newAffinityStore(time.Minute, 64)
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			key := affinityKey(int64(worker%4), "sess", "gpt-x")
			for i := 0; i < 200; i++ {
				store.put(key, int64(worker+1), time.Now())
				store.get(key, time.Now())
				store.drop(key, int64(i%8))
				store.stats()
			}
		}(worker)
	}
	wg.Wait()
	if stats := store.stats(); stats.Entries > 64 {
		t.Fatalf("concurrent writes must not exceed the bound: %+v", stats)
	}
}

func TestAffinityKeyIsolatesKeySessionAndModel(t *testing.T) {
	base := affinityKey(1, "sess", "gpt-x")
	for _, other := range []string{
		affinityKey(2, "sess", "gpt-x"),
		affinityKey(1, "other", "gpt-x"),
		affinityKey(1, "sess", "gpt-y"),
	} {
		if other == base {
			t.Fatalf("two different slots collided: %q", base)
		}
	}
}

// ---------------------------------------------------------------------------
// routing behaviour
// ---------------------------------------------------------------------------

func TestPlanPromotesTheBoundRouteInsideItsTier(t *testing.T) {
	r := newAffinityRouter(t, affinityFixture())
	in := domain.RouteRequest{Model: "gpt-x", Key: keyWith("", "", ""), SessionID: "sess-1"}

	first := planOne(t, r, in)
	if len(first.Candidates) != 3 || firstRoute(first) != 200 {
		t.Fatalf("the natural order is the tier order, kept in registry order on a tie: %+v", first.Candidates)
	}

	// The *second* candidate of the top tier is the one that actually served the request.
	r.NoteSuccess(slotFor(t, r, in), 201)

	second := planOne(t, r, in)
	if firstRoute(second) != 201 {
		t.Fatalf("the bound route must be promoted inside its tier: %+v", second.Candidates)
	}
	if routeIDs(second) != "201,200,202" {
		t.Fatalf("promotion must only move the bound candidate: %v", routeIDs(second))
	}
	if stats := r.AffinityStats(); stats.Hits != 1 || stats.Entries != 1 {
		t.Fatalf("the hit must be reported: %+v", stats)
	}
}

func TestAffinityIsScopedToKeySessionAndModel(t *testing.T) {
	r := newAffinityRouter(t, affinityFixture())
	key := keyWith("", "", "")
	in := domain.RouteRequest{Model: "gpt-x", Key: key, SessionID: "sess-1"}
	slot := slotFor(t, r, in)
	r.NoteSuccess(slot, 201)

	if got := firstRoute(planOne(t, r, in)); got != 201 {
		t.Fatalf("the bound session must be promoted, got %d", got)
	}

	otherKey := keyWith("", "", "")
	otherKey.ID = 99
	for name, request := range map[string]domain.RouteRequest{
		"another session": {Model: "gpt-x", Key: key, SessionID: "sess-2"},
		"another key":     {Model: "gpt-x", Key: otherKey, SessionID: "sess-1"},
	} {
		res := planOne(t, r, request)
		if res.Affinity == slot {
			t.Errorf("%s must not share the bound session's slot", name)
		}
		if got := firstRoute(res); got != 200 {
			t.Errorf("%s must not be promoted, got route %d", name, got)
		}
	}
}

func TestPlanTreatsStrictOrderAndPinnedRequestsAsNotParticipating(t *testing.T) {
	r := newAffinityRouter(t, affinityFixture())
	key := keyWith("", "", "")

	strict := planOne(t, r, domain.RouteRequest{
		Model: "gpt-x", Key: key, SessionID: "sess-1", Strategy: string(balancer.StrictOrder),
	})
	if strict.Affinity != "" {
		t.Fatalf("strict_order must not take part in stickiness: %q", strict.Affinity)
	}
	r.NoteSuccess(strict.Affinity, 201) // a no-op, but must not panic

	if r.NoteFailure(strict.Affinity, 201) {
		t.Fatal("an empty slot must never report a drop")
	}

	pinned := planOne(t, r, domain.RouteRequest{Model: "gpt-x@beta", Key: key, SessionID: "sess-1"})
	if pinned.Affinity != "" {
		t.Fatalf("a pinned request must not take part in stickiness: %q", pinned.Affinity)
	}
	if len(pinned.Candidates) != 1 || pinned.Candidates[0].ProviderName != "beta" {
		t.Fatalf("the pin must still select exactly its provider: %+v", pinned.Candidates)
	}

	// Neither of the above wrote anything, so a later plain request is not promoted.
	plain := planOne(t, r, domain.RouteRequest{Model: "gpt-x", Key: key, SessionID: "sess-1"})
	if firstRoute(plain) != 200 {
		t.Fatalf("neither plan may have written a binding: %+v", plain.Candidates)
	}

	// No session key at all: the request is routed exactly as before the feature.
	noSession := planOne(t, r, domain.RouteRequest{Model: "gpt-x", Key: key})
	if noSession.Affinity != "" {
		t.Fatalf("a request without a session key must not take part: %q", noSession.Affinity)
	}

	// The console's "why this provider" view is the same Plan without a session, so it must
	// keep showing the operator's own order rather than a session's preference.
	explained, err := r.Explain(domain.RouteRequest{Model: "gpt-x", Key: key})
	if err != nil {
		t.Fatalf("Explain failed: %v", err)
	}
	if len(explained.Order) != 3 || explained.Order[0].RouteID != 200 {
		t.Fatalf("Explain must show the natural order: %+v", explained.Order)
	}
}

func TestAffinityStaysInsideTheTier(t *testing.T) {
	r := newAffinityRouter(t, affinityFixture())
	in := domain.RouteRequest{Model: "gpt-x", Key: keyWith("", "", ""), SessionID: "sess-1"}

	// gamma is already first in its own (lower-priority) tier, so binding it must change
	// nothing at all: the operator's tier order outranks a session preference.
	r.NoteSuccess(slotFor(t, r, in), 202)

	res := planOne(t, r, in)
	if routeIDs(res) != "200,201,202" {
		t.Fatalf("stickiness must never promote a candidate past a higher tier: %v", routeIDs(res))
	}
	stats := r.AffinityStats()
	if stats.Stale != 0 || stats.Entries != 1 {
		t.Fatalf("a route that is still usable keeps its binding: %+v", stats)
	}
}

func TestAffinityDropsStaleBindingsAndDoesNotResurrectThem(t *testing.T) {
	snap := affinityFixture()
	r := newAffinityRouter(t, snap)
	in := domain.RouteRequest{Model: "gpt-x", Key: keyWith("", "", ""), SessionID: "sess-1"}

	r.NoteSuccess(slotFor(t, r, in), 201)

	// beta goes away: the binding can no longer be honoured.
	snap.Providers[1].Enabled = false
	res := planOne(t, r, in)
	if routeIDs(res) != "200,202" {
		t.Fatalf("only the enabled providers may serve: %v", routeIDs(res))
	}
	stats := r.AffinityStats()
	if stats.Stale != 1 || stats.Entries != 0 {
		t.Fatalf("an unusable binding must be dropped, not kept: %+v", stats)
	}

	// It comes back: the session must not silently return to it.
	snap.Providers[1].Enabled = true
	res = planOne(t, r, in)
	if firstRoute(res) != 200 {
		t.Fatalf("a dropped binding must not be resurrected: %+v", res.Candidates)
	}
	if stats := r.AffinityStats(); stats.Entries != 0 {
		t.Fatalf("nothing may re-bind without a successful attempt: %+v", stats)
	}
}

func TestNoteFailureOnlyDropsTheMatchingRoute(t *testing.T) {
	r := newAffinityRouter(t, affinityFixture())
	in := domain.RouteRequest{Model: "gpt-x", Key: keyWith("", "", ""), SessionID: "sess-1"}
	slot := slotFor(t, r, in)
	r.NoteSuccess(slot, 201)

	if r.NoteFailure(slot, 200) {
		t.Fatal("failing on another route must not drop the binding")
	}
	if got := firstRoute(planOne(t, r, in)); got != 201 {
		t.Fatalf("the binding must survive an unrelated failure, got route %d", got)
	}

	if !r.NoteFailure(slot, 201) {
		t.Fatal("failing on the bound route must drop the binding")
	}
	if got := firstRoute(planOne(t, r, in)); got != 200 {
		t.Fatalf("a failed route must not be preferred again, got route %d", got)
	}
}

func TestAffinityIsOffUnlessConfigured(t *testing.T) {
	r := New(Config{DefaultGrant: "all", Degradation: "strip"},
		registry.NewStatic(affinityFixture()), balancer.New(balancer.DefaultConfig()))

	res := planOne(t, r, domain.RouteRequest{Model: "gpt-x", Key: keyWith("", "", ""), SessionID: "sess-1"})
	if res.Affinity != "" {
		t.Fatalf("the zero-value config must not enable stickiness: %q", res.Affinity)
	}
	if stats := r.AffinityStats(); stats.Enabled {
		t.Fatalf("stats must report the feature as off: %+v", stats)
	}
	// The bounds must not be invented either: an off feature reads back as zero.
	if r.cfg.AffinityTTL != 0 || r.cfg.AffinityMaxEntries != 0 {
		t.Fatalf("an off feature must not resolve bounds: %+v", r.cfg)
	}
}

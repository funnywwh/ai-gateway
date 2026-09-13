package routing

import (
	"strconv"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// Session stickiness (docs/routing.md §4.4): a client session that keeps its requests on
// the upstream that last served it gains prefix-cache locality and a stable upstream
// account, where a per-request weighted draw would scatter it.
//
// The binding is a *hint about ordering*, never a grant: it is applied after the request's
// candidates have been filtered for authorisation, availability and capability, so a stale
// binding can only be ignored — it can never resurrect a route the key may not use.

// Affinity defaults, applied when the feature is on and the config leaves the bound at zero.
const (
	DefaultAffinityTTL        = 30 * time.Minute
	DefaultAffinityMaxEntries = 10000
)

// AffinityStats is the point-in-time view of the table (surfaced on /stats).
type AffinityStats struct {
	Enabled bool `json:"enabled"`
	Entries int  `json:"entries"`
	// Hits counts requests whose binding was promoted; Misses, requests with a session
	// key but no live binding; Stale, bindings dropped because the bound route was no
	// longer usable; Evictions, entries pushed out by the capacity bound.
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Stale     int64 `json:"stale"`
	Evictions int64 `json:"evictions"`
}

type affinityEntry struct {
	routeID int64
	// seen is refreshed on every hit, so the TTL is "time since this session last used
	// the binding" rather than "time since it was first created".
	seen time.Time
}

// affinityStore is a bounded, self-expiring table of successful session routes.
//
// A nil *affinityStore is a valid "feature off" value: every method is a no-op, which is
// what `routing.New` builds when session_affinity is false.
type affinityStore struct {
	mu    sync.Mutex
	items map[string]affinityEntry
	ttl   time.Duration
	max   int

	hits      int64
	misses    int64
	stale     int64
	evictions int64
}

func newAffinityStore(ttl time.Duration, max int) *affinityStore {
	if ttl <= 0 || max <= 0 {
		return nil
	}
	return &affinityStore{items: map[string]affinityEntry{}, ttl: ttl, max: max}
}

// affinityKey is the sticky slot: one API key, one client session, one canonical model.
// The components are length-bounded at the source (the session key is clamped by the
// responses package), so the key cannot grow with client input.
func affinityKey(keyID int64, session, canonical string) string {
	return strconv.FormatInt(keyID, 10) + "\x00" + session + "\x00" + canonical
}

// get returns the bound route and refreshes the entry's TTL. An expired entry is removed
// on the way out: the table cleans itself as it is read.
func (a *affinityStore) get(key string, now time.Time) (int64, bool) {
	if a == nil || key == "" {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.items[key]
	if !ok {
		a.misses++
		return 0, false
	}
	if now.Sub(entry.seen) >= a.ttl {
		delete(a.items, key)
		a.misses++
		return 0, false
	}
	entry.seen = now
	a.items[key] = entry
	a.hits++
	return entry.routeID, true
}

// put records a successful route, purging expired entries first and evicting the oldest
// one when the table is full.
func (a *affinityStore) put(key string, routeID int64, now time.Time) {
	if a == nil || key == "" || routeID <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeLocked(now)
	if _, exists := a.items[key]; !exists && len(a.items) >= a.max {
		oldest, at := "", time.Time{}
		for k, entry := range a.items {
			if oldest == "" || entry.seen.Before(at) {
				oldest, at = k, entry.seen
			}
		}
		if oldest != "" {
			delete(a.items, oldest)
			a.evictions++
		}
	}
	a.items[key] = affinityEntry{routeID: routeID, seen: now}
}

// drop removes the binding, but only while it still points at routeID. A concurrent
// request of the same session may have already re-bound it to a route that is working,
// and that newer fact must win.
func (a *affinityStore) drop(key string, routeID int64) bool {
	if a == nil || key == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.items[key]
	if !ok || entry.routeID != routeID {
		return false
	}
	delete(a.items, key)
	return true
}

// dropStale removes a binding whose route is no longer usable for this request. It is not
// restored when the route becomes usable again: the session re-binds on its next success.
func (a *affinityStore) dropStale(key string) {
	if a == nil || key == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.items[key]; ok {
		delete(a.items, key)
		a.stale++
	}
}

func (a *affinityStore) purgeLocked(now time.Time) {
	for k, entry := range a.items {
		if now.Sub(entry.seen) >= a.ttl {
			delete(a.items, k)
		}
	}
}

func (a *affinityStore) stats() AffinityStats {
	if a == nil {
		return AffinityStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return AffinityStats{
		Enabled: true, Entries: len(a.items),
		Hits: a.hits, Misses: a.misses, Stale: a.stale, Evictions: a.evictions,
	}
}

// promoteWithinTier moves one candidate to the front of its own priority tier and reports
// whether it was in the list at all. Tiers are contiguous (Plan concatenates them in
// ascending priority), so "the front of my tier" is the start of that block.
//
// Tiers are the operator's own statement of "use this before that" (docs/routing.md §4.2),
// so stickiness reorders inside a tier and never promotes across one: a single failure
// must not silently invert the configured priority.
func promoteWithinTier(cands []domain.Candidate, routeID int64) bool {
	idx := -1
	for i := range cands {
		if cands[i].RouteID == routeID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	start := idx
	for start > 0 && cands[start-1].Priority == cands[idx].Priority {
		start--
	}
	if start == idx {
		return true // already first in its tier
	}
	moved := cands[idx]
	copy(cands[start+1:idx+1], cands[start:idx])
	cands[start] = moved
	return true
}

package httpapi

import (
	"strconv"

	"github.com/winger/ai-gateway/internal/runtime"
)

// Capacity reports the provider concurrency gates (M44): how many attempts each provider
// is running, how many are waiting for a slot, and the queue policy the data path applies.
//
// It is a narrow port like Prober: a nil port simply omits the block, which is what a
// deployment without a dispatcher (and most tests) wants.
type Capacity interface {
	CapacityStats() map[int64]runtime.CapacityStat
	CapacityPolicy() runtime.CapacityPolicy
}

// capacityStats reads the live gates once, so a provider list can look each row up without
// taking the gate's lock per row.
func (s *Server) capacityStats() map[int64]runtime.CapacityStat {
	if s.deps.Capacity == nil {
		return nil
	}
	return s.deps.Capacity.CapacityStats()
}

// capacityBlock is the /stats (and therefore MCP admin_stats) payload: the policy first,
// because "how long may a request wait here" is what an operator or agent asks before
// reading any per-provider number, then the live state of every limited provider.
func (s *Server) capacityBlock() map[string]any {
	if s.deps.Capacity == nil {
		return nil
	}
	stats := s.deps.Capacity.CapacityStats()
	providers := make(map[string]any, len(stats))
	for id, stat := range stats {
		providers[strconv.FormatInt(id, 10)] = stat
	}
	policy := s.deps.Capacity.CapacityPolicy()
	return map[string]any{
		"queue_wait_s":      policy.QueueWaitS,
		"queue_max_waiters": policy.QueueMaxWaiters,
		"providers":         providers,
	}
}

// attachCapacity adds one provider's live gate state to its API row. A provider that never
// had a ceiling has no entry and gets no field: reporting a made-up "limit: 0, inflight: 0"
// would suggest the gateway were counting something. A provider whose ceiling was lifted
// back to 0 keeps its entry (attempts may still be running, and the count has to stay
// exact), so its row does carry "limit: 0" — which is the honest answer: unlimited, with
// nobody in flight.
func attachCapacity(row map[string]any, id int64, stats map[int64]runtime.CapacityStat) {
	if row == nil || stats == nil {
		return
	}
	if stat, ok := stats[id]; ok {
		row["capacity"] = stat
	}
}

package pricing

// WorstCaseRates returns the highest rate per dimension across every rule in the set.
// The reservation estimate uses it to assume the most expensive tier and time window,
// which is what keeps a prepaid account from being oversold.
//
// A dimension a rule does not name is still chargeable: Evaluate prices it through
// its fallback (see dimensionFallbacks), so a hold that resolved the dimension by
// name alone reserved nothing for it. Every fallback dimension therefore also
// carries its source's worst rate — an `input` hold can never be cheaper than the
// worst cache-miss rate. Without this, a rule set naming only the cache-split
// dimensions (the DeepSeek shape) reserved zero for the entire prompt.
//
// The two are combined with max, so the hold stays an upper bound even when a rule
// makes a dimension explicitly free: over-holding is released at settlement, whereas
// under-holding is what oversells the account.
func WorstCaseRates(set *RuleSet) map[string]int64 {
	out := map[string]int64{}
	if set == nil {
		return out
	}
	for _, rule := range set.Rules {
		for dimension, rate := range rule.Rates {
			if rate > out[dimension] {
				out[dimension] = rate
			}
		}
		if rule.PerRequestFeeMicros > out["per_request"] {
			out["per_request"] = rule.PerRequestFeeMicros
		}
	}
	// Fold the fallback chains in after the explicit rates. Each chain is walked in
	// full, so a two-hop chain (image_input -> input -> input_cache_miss) resolves
	// the same way whatever order the map happens to iterate in.
	for dimension := range dimensionFallbacks {
		for _, source := range fallbackChain(dimension) {
			if rate := out[source]; rate > out[dimension] {
				out[dimension] = rate
			}
		}
	}
	return out
}

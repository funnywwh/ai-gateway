package pricing

// WorstCaseRates returns the highest rate per dimension across every rule in the set.
// The reservation estimate uses it to assume the most expensive tier and time window,
// which is what keeps a prepaid account from being oversold.
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
	return out
}

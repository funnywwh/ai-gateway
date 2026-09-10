package pricing

// Shadowing describes a rule that can never win because an earlier rule already
// covers everything it matches. It is reported as a warning: operators do prepare
// future promotions, so this must not block a save.
type Shadowing struct {
	RuleID     string `json:"rule_id"`
	RuleTitle  string `json:"rule_title,omitempty"`
	RuleOrder  int    `json:"rule_order"`
	ShadowedBy string `json:"shadowed_by"`
	ByOrder    int    `json:"by_order"`
	Reason     string `json:"reason"`
}

// DetectShadowing reports every rule that an earlier rule makes unreachable.
func DetectShadowing(set *RuleSet) []Shadowing {
	if set == nil || len(set.Rules) < 2 {
		return nil
	}
	ordered := append([]Rule(nil), set.Rules...)
	sortRules(ordered)
	out := []Shadowing{}
	for index := 1; index < len(ordered); index++ {
		later := ordered[index]
		for earlier := 0; earlier < index; earlier++ {
			candidate := ordered[earlier]
			if reason, ok := subsumes(candidate.When, later.When); ok {
				out = append(out, Shadowing{
					RuleID: later.ID, RuleTitle: later.Title, RuleOrder: later.Order,
					ShadowedBy: candidate.ID, ByOrder: candidate.Order, Reason: reason,
				})
				break
			}
		}
	}
	return out
}

// subsumes reports whether every request matched by later is also matched by earlier.
func subsumes(earlier, later When) (string, bool) {
	if earlier.Empty() {
		return "an earlier catch-all rule matches every request", true
	}
	// Time windows: only an identical window set (or no constraint at all on the
	// earlier rule) can be proven to cover the later one without interval algebra.
	if len(earlier.TimeWindows) != 0 {
		if len(later.TimeWindows) == 0 || !sameWindows(earlier.TimeWindows, later.TimeWindows) {
			return "", false
		}
	}
	if earlier.ValidFrom != nil && (later.ValidFrom == nil || later.ValidFrom.Before(*earlier.ValidFrom)) {
		return "", false
	}
	if earlier.ValidTo != nil && (later.ValidTo == nil || later.ValidTo.After(*earlier.ValidTo)) {
		return "", false
	}
	if earlier.Tier != nil {
		if later.Tier == nil || later.Tier.Basis != earlier.Tier.Basis {
			return "", false
		}
		if later.Tier.Gte < earlier.Tier.Gte {
			return "", false
		}
		if earlier.Tier.Lt != 0 && (later.Tier.Lt == 0 || later.Tier.Lt > earlier.Tier.Lt) {
			return "", false
		}
	}
	if earlier.ModelVariant != "" && earlier.ModelVariant != later.ModelVariant {
		return "", false
	}
	if earlier.Region != "" && earlier.Region != later.Region {
		return "", false
	}
	return "an earlier rule with the same or weaker conditions already covers it", true
}

func sameWindows(a, b []TimeWindow) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index].Start != b[index].Start || a[index].End != b[index].End || a[index].TZ != b[index].TZ {
			return false
		}
		if len(a[index].Days) != len(b[index].Days) {
			return false
		}
		for dayIndex := range a[index].Days {
			if normalizeDay(a[index].Days[dayIndex]) != normalizeDay(b[index].Days[dayIndex]) {
				return false
			}
		}
	}
	return true
}

func sortRules(rules []Rule) {
	for i := 1; i < len(rules); i++ {
		for j := i; j > 0 && rules[j].Order < rules[j-1].Order; j-- {
			rules[j], rules[j-1] = rules[j-1], rules[j]
		}
	}
}

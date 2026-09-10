// Package pricing turns usage dimensions into money with integer arithmetic only.
//
// One pure function (Evaluate) serves the data plane, the admin simulator and the
// reconciliation replay, so a number shown in the console is by construction the
// number that gets charged.
package pricing

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "time/tzdata" // the console and the gateway must not depend on host zoneinfo

	"github.com/winger/ai-gateway/internal/domain"
)

// RateScale is the denominator every rate is expressed against: rates are
// micro-USD per RateScale units, for every dimension kind (tokens, images,
// seconds). One scale keeps a single code path and a single rounding rule.
const RateScale int64 = 1_000_000

// Sale pricing bases.
const (
	BasisCostFollow = "cost_follow"
	BasisAbsolute   = "absolute"
)

// Tier bases.
const (
	TierInput  = "input"
	TierOutput = "output"
	TierTotal  = "total"
)

var dimensionRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Tier narrows a rule to a usage band. Gte is inclusive, Lt exclusive; a zero Lt
// means "no upper bound".
type Tier struct {
	Basis string `json:"basis"`
	Gte   int64  `json:"gte"`
	Lt    int64  `json:"lt"`
}

// TimeWindow is one half-open [Start,End) window in a location. End < Start means
// the window crosses midnight and the weekday belongs to the window's start.
type TimeWindow struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
	TZ    string   `json:"tz"`
}

// When holds every condition of a rule. An empty When is the mandatory catch-all.
type When struct {
	TimeWindows  []TimeWindow `json:"time_windows,omitempty"`
	ValidFrom    *time.Time   `json:"valid_from,omitempty"`
	ValidTo      *time.Time   `json:"valid_to,omitempty"`
	Tier         *Tier        `json:"tier,omitempty"`
	ModelVariant string       `json:"model_variant,omitempty"`
	Region       string       `json:"region,omitempty"`
}

// Empty reports whether the condition set matches everything.
func (w When) Empty() bool {
	return len(w.TimeWindows) == 0 && w.ValidFrom == nil && w.ValidTo == nil &&
		w.Tier == nil && w.ModelVariant == "" && w.Region == ""
}

// Rule is one ordered pricing rule.
type Rule struct {
	ID                  string           `json:"id"`
	Title               string           `json:"title,omitempty"`
	Order               int              `json:"order"`
	When                When             `json:"when"`
	Rates               map[string]int64 `json:"rates"`
	PerRequestFeeMicros int64            `json:"per_request_fee_micros,omitempty"`
}

// RuleSet is a full pricing side: a cost table or a sale table.
type RuleSet struct {
	// Basis applies to sale tables only: cost_follow or absolute.
	Basis string `json:"basis,omitempty"`
	// MarkupBP is the default mark-up in basis points (10000 = 1.0x).
	MarkupBP int `json:"markup_bp,omitempty"`
	// DimensionMarkupBP overrides the mark-up per dimension.
	DimensionMarkupBP map[string]int `json:"dimension_markup_bp,omitempty"`
	Currency          string         `json:"currency,omitempty"`
	Rules             []Rule         `json:"rules"`
}

// ParseRuleSet decodes and validates a rule set. An empty document yields an empty
// (valid) set so callers can treat "not configured" uniformly.
func ParseRuleSet(raw string) (*RuleSet, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return &RuleSet{}, nil
	}
	var set RuleSet
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&set); err != nil {
		return nil, domain.ErrInvalidRequest("pricing rules are malformed: " + err.Error())
	}
	if err := Validate(&set); err != nil {
		return nil, err
	}
	return &set, nil
}

// Validate enforces the write-time rules from docs/pricing.md.
func Validate(set *RuleSet) error {
	if set == nil {
		return nil
	}
	switch set.Basis {
	case "", BasisCostFollow, BasisAbsolute:
	default:
		return domain.ErrInvalidRequest("pricing basis must be cost_follow or absolute")
	}
	if set.MarkupBP < 0 {
		return domain.ErrInvalidRequest("markup_bp must not be negative")
	}
	for dimension, bp := range set.DimensionMarkupBP {
		if !dimensionRE.MatchString(dimension) {
			return domain.ErrInvalidRequest("dimension_markup_bp key " + dimension + " is not a valid dimension name")
		}
		if bp < 0 {
			return domain.ErrInvalidRequest("dimension_markup_bp[" + dimension + "] must not be negative")
		}
	}
	if len(set.Rules) == 0 {
		// A cost_follow sale side legitimately carries no rules: the mark-up does
		// the work. An absolute side without rules would charge nothing, which is
		// almost certainly a mistake, so it still requires a catch-all below.
		if set.Basis == BasisAbsolute {
			return domain.ErrInvalidRequest("an absolute pricing rule set needs at least a catch-all rule")
		}
		return nil
	}

	seenOrder := map[int]bool{}
	seenID := map[string]bool{}
	catchAll := false
	for index, rule := range set.Rules {
		where := fmt.Sprintf("rules[%d]", index)
		if rule.ID != "" {
			if seenID[rule.ID] {
				return domain.ErrInvalidRequest(where + ": duplicate rule id " + rule.ID)
			}
			seenID[rule.ID] = true
		}
		if seenOrder[rule.Order] {
			return domain.ErrInvalidRequest(fmt.Sprintf("%s: duplicate order %d; rule order must be unique", where, rule.Order))
		}
		seenOrder[rule.Order] = true
		if len(rule.Rates) == 0 && rule.PerRequestFeeMicros == 0 {
			return domain.ErrInvalidRequest(where + ": a rule needs rates or a per_request_fee_micros")
		}
		for dimension, rate := range rule.Rates {
			if !dimensionRE.MatchString(dimension) {
				return domain.ErrInvalidRequest(where + ": rate key " + dimension + " is not a valid dimension name")
			}
			if rate < 0 {
				return domain.ErrInvalidRequest(where + ": rate for " + dimension + " must not be negative")
			}
		}
		if rule.PerRequestFeeMicros < 0 {
			return domain.ErrInvalidRequest(where + ": per_request_fee_micros must not be negative")
		}
		if rule.When.Tier != nil {
			tier := rule.When.Tier
			switch tier.Basis {
			case TierInput, TierOutput, TierTotal:
			default:
				return domain.ErrInvalidRequest(where + ": tier.basis must be input, output or total")
			}
			if tier.Gte < 0 || tier.Lt < 0 {
				return domain.ErrInvalidRequest(where + ": tier bounds must not be negative")
			}
			if tier.Lt != 0 && tier.Lt <= tier.Gte {
				return domain.ErrInvalidRequest(where + ": tier.lt must be greater than tier.gte")
			}
		}
		if err := validateWindows(where, rule.When.TimeWindows); err != nil {
			return err
		}
		if rule.When.ValidFrom != nil && rule.When.ValidTo != nil && !rule.When.ValidFrom.Before(*rule.When.ValidTo) {
			return domain.ErrInvalidRequest(where + ": valid_from must be before valid_to")
		}
		if rule.When.Empty() {
			catchAll = true
		}
	}
	if !catchAll {
		return domain.ErrInvalidRequest("a pricing rule set must contain a catch-all rule (an empty when object) so every request has a price")
	}
	// Evaluation relies on ascending order, so sort a copy rather than silently
	// depending on whatever order the JSON happened to use.
	sort.SliceStable(set.Rules, func(i, j int) bool { return set.Rules[i].Order < set.Rules[j].Order })
	return nil
}

func validateWindows(where string, windows []TimeWindow) error {
	for index, window := range windows {
		label := fmt.Sprintf("%s.time_windows[%d]", where, index)
		if _, err := parseClock(window.Start); err != nil {
			return domain.ErrInvalidRequest(label + ": start " + err.Error())
		}
		end, err := parseClock(window.End)
		if err != nil {
			return domain.ErrInvalidRequest(label + ": end " + err.Error())
		}
		start, _ := parseClock(window.Start)
		if start == end {
			return domain.ErrInvalidRequest(label + ": start and end must differ (use end < start for a window that crosses midnight)")
		}
		if _, err := windowLocation(window.TZ); err != nil {
			return domain.ErrInvalidRequest(label + ": " + err.Error())
		}
		for _, day := range window.Days {
			if !validDay(day) {
				return domain.ErrInvalidRequest(label + ": day " + day + " must be one of mon,tue,wed,thu,fri,sat,sun")
			}
		}
	}
	return nil
}

// parseClock reads HH:MM into minutes since midnight.
func parseClock(value string) (int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("must look like HH:MM")
	}
	hour, err := parseUint(parts[0], 2)
	if err != nil || hour > 24 {
		return 0, fmt.Errorf("has an invalid hour")
	}
	minute, err := parseUint(parts[1], 2)
	if err != nil || minute > 59 {
		return 0, fmt.Errorf("has an invalid minute")
	}
	if hour == 24 && minute != 0 {
		return 0, fmt.Errorf("24:00 is the only valid hour-24 value")
	}
	return hour*60 + minute, nil
}

func parseUint(value string, width int) (int, error) {
	if len(value) != width {
		return 0, fmt.Errorf("must be zero padded")
	}
	total := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("must be numeric")
		}
		total = total*10 + int(r-'0')
	}
	return total, nil
}

func validDay(day string) bool {
	switch strings.ToLower(day) {
	case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		return true
	}
	return false
}

// LoadLocation resolves UTC, Local and fixed offsets such as +08:00. It is exported
// so the billing period and the pricing engine agree on what a timezone means.
func LoadLocation(tz string) (*time.Location, error) { return windowLocation(tz) }

// windowLocation resolves UTC, Local and fixed offsets such as +08:00.
func windowLocation(tz string) (*time.Location, error) {
	trimmed := strings.TrimSpace(tz)
	switch {
	case trimmed == "", trimmed == "UTC", trimmed == "utc":
		return time.UTC, nil
	case strings.EqualFold(trimmed, "local"):
		return time.Local, nil
	case strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-"):
		if len(trimmed) != 6 || trimmed[3] != ':' {
			return nil, fmt.Errorf("timezone %q must look like +08:00", tz)
		}
		hours, err := parseUint(trimmed[1:3], 2)
		if err != nil || hours > 14 {
			return nil, fmt.Errorf("timezone %q has an invalid offset", tz)
		}
		minutes, err := parseUint(trimmed[4:6], 2)
		if err != nil || minutes > 59 {
			return nil, fmt.Errorf("timezone %q has an invalid offset", tz)
		}
		offset := (hours*60 + minutes) * 60
		if trimmed[0] == '-' {
			offset = -offset
		}
		return time.FixedZone(trimmed, offset), nil
	default:
		location, err := time.LoadLocation(trimmed)
		if err != nil {
			return nil, fmt.Errorf("timezone %q is unknown", tz)
		}
		return location, nil
	}
}

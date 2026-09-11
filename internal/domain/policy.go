package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PolicyFields lists every top-level key the gateway actually reads from an API key or
// tag policy document: the quota fields below plus the routing and margin knobs that
// routing.policyWire and billing.policyMarginBP parse.
//
// The list is an allow-list on purpose. The console used to advertise a nested shape
// ({"rate_limit":{"rpm":60}}) that no code path ever read, so an operator could set a
// limit, see it stored, and keep serving unlimited traffic. A policy that cannot take
// effect must be rejected at write time, not stored and ignored.
var PolicyFields = []string{
	"rpm", "tpm", "concurrency",
	"monthly_requests", "monthly_tokens", "monthly_cost_micros",
	"strategy", "provider_order", "margin_bp",
}

// RateLimits is the quota part of a key/tag policy. Only RPM, TPM and Concurrency are
// enforced (quota.Limiter checks them at admission); the monthly caps are parsed and
// reported so an operator can see what was configured, but nothing consumes them yet —
// see docs/api-responses.md and the M23 follow-up in docs/TODO.md.
type RateLimits struct {
	RPM               int   `json:"rpm"`
	TPM               int64 `json:"tpm"`
	Concurrency       int   `json:"concurrency"`
	MonthlyRequests   int64 `json:"monthly_requests"`
	MonthlyTokens     int64 `json:"monthly_tokens"`
	MonthlyCostMicros int64 `json:"monthly_cost_micros"`
}

// UnenforcedFields returns the names of the quota fields present in rl that admission
// does not check. Callers report them instead of letting a configured cap look active.
func (rl RateLimits) UnenforcedFields() []string {
	out := []string{}
	if rl.MonthlyRequests != 0 {
		out = append(out, "monthly_requests")
	}
	if rl.MonthlyTokens != 0 {
		out = append(out, "monthly_tokens")
	}
	if rl.MonthlyCostMicros != 0 {
		out = append(out, "monthly_cost_micros")
	}
	return out
}

// Configured renders the quota fields that were actually set, so a report shows what the
// operator wrote rather than a wall of zeroes.
func (rl RateLimits) Configured() map[string]any {
	out := map[string]any{}
	if rl.RPM != 0 {
		out["rpm"] = rl.RPM
	}
	if rl.TPM != 0 {
		out["tpm"] = rl.TPM
	}
	if rl.Concurrency != 0 {
		out["concurrency"] = rl.Concurrency
	}
	if rl.MonthlyRequests != 0 {
		out["monthly_requests"] = rl.MonthlyRequests
	}
	if rl.MonthlyTokens != 0 {
		out["monthly_tokens"] = rl.MonthlyTokens
	}
	if rl.MonthlyCostMicros != 0 {
		out["monthly_cost_micros"] = rl.MonthlyCostMicros
	}
	return out
}

// ParsePolicy decodes one key/tag policy document. It returns the quota fields and the
// top-level keys the gateway does not read, so a caller can fail a write loudly instead
// of storing something inert. An empty document yields the zero value and no error.
func ParsePolicy(raw string) (RateLimits, []string, error) {
	if strings.TrimSpace(raw) == "" {
		return RateLimits{}, nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return RateLimits{}, nil, fmt.Errorf("policy must be a JSON object")
	}
	var limits RateLimits
	if err := json.Unmarshal([]byte(raw), &limits); err != nil {
		return RateLimits{}, nil, fmt.Errorf("policy quota fields (rpm, tpm, concurrency, monthly_*) must be numbers")
	}
	known := make(map[string]bool, len(PolicyFields))
	for _, field := range PolicyFields {
		known[field] = true
	}
	unknown := []string{}
	for key := range doc {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return limits, unknown, nil
}

// PolicyFieldList renders the accepted policy fields for an error message.
func PolicyFieldList() string { return strings.Join(PolicyFields, ", ") }

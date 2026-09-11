package domain

import (
	"strings"
	"testing"
)

func TestParsePolicyReadsFlatQuotaFields(t *testing.T) {
	limits, unknown, err := ParsePolicy(`{"rpm":60,"tpm":9000,"concurrency":4,` +
		`"monthly_requests":1000,"monthly_tokens":500000,"monthly_cost_micros":25000000}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := RateLimits{RPM: 60, TPM: 9000, Concurrency: 4, MonthlyRequests: 1000, MonthlyTokens: 500000, MonthlyCostMicros: 25000000}
	if limits != want {
		t.Fatalf("limits = %+v, want %+v", limits, want)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v, want none", unknown)
	}
}

// The nested shape the console used to advertise is not read by any code path; it must be
// reported as unrecognised so a write can be rejected instead of stored and ignored.
func TestParsePolicyReportsFieldsNothingReads(t *testing.T) {
	limits, unknown, err := ParsePolicy(`{"rate_limit":{"rpm":60},"recording":{"output_text":false},"rpm":10}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if limits.RPM != 10 {
		t.Fatalf("flat rpm = %d, want 10 (the nested copy is not a limit)", limits.RPM)
	}
	if strings.Join(unknown, ",") != "rate_limit,recording" {
		t.Fatalf("unknown = %v, want [rate_limit recording] sorted", unknown)
	}
}

func TestParsePolicyAcceptsRoutingKnobs(t *testing.T) {
	if _, unknown, err := ParsePolicy(`{"strategy":"weighted","provider_order":["a"],"margin_bp":11000}`); err != nil || len(unknown) != 0 {
		t.Fatalf("routing knobs must be accepted: unknown=%v err=%v", unknown, err)
	}
}

func TestParsePolicyRejectsMalformedDocuments(t *testing.T) {
	if _, _, err := ParsePolicy(`{"rpm":{"nested":60}}`); err == nil {
		t.Fatal("a non-numeric quota field must fail instead of parsing to zero")
	}
	if _, _, err := ParsePolicy(`["rpm"]`); err == nil {
		t.Fatal("a policy must be a JSON object")
	}
	if limits, unknown, err := ParsePolicy(""); err != nil || limits != (RateLimits{}) || unknown != nil {
		t.Fatalf("empty policy = %+v/%v/%v, want the zero value", limits, unknown, err)
	}
}

func TestRateLimitsReportUnenforcedFields(t *testing.T) {
	limits, _, err := ParsePolicy(`{"rpm":60,"monthly_tokens":500000}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(limits.UnenforcedFields(), ","); got != "monthly_tokens" {
		t.Fatalf("unenforced = %q, want monthly_tokens", got)
	}
	if got := limits.Configured(); got["rpm"] != 60 || len(got) != 2 {
		t.Fatalf("configured = %v, want exactly the two fields that were set", got)
	}
	if none := (RateLimits{}).UnenforcedFields(); len(none) != 0 {
		t.Fatalf("zero limits report nothing unenforced, got %v", none)
	}
}

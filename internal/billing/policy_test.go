package billing

import "testing"

func TestDecideInflight(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		accrued int64
		limit   int64
		want    string
	}{
		{"well under", "abort", 10, 1000, InflightContinue},
		{"soft band warns under the abort policy", "abort", 850, 1000, InflightWarn},
		{"soft band throttles under the throttle policy", "throttle", 850, 1000, InflightThrottle},
		{"soft band is ignored when overdraft is allowed", "allow_overdraft", 850, 1000, InflightContinue},
		{"hard band aborts", "abort", 1000, 1000, InflightAbort},
		{"hard band aborts past the limit", "abort", 1500, 1000, InflightAbort},
		{"throttle ends the call at the hard limit", "throttle", 1000, 1000, InflightAbort},
		{"warn policy never aborts", "warn", 5000, 1000, InflightWarn},
		{"overdraft aborts only past the extended limit", "allow_overdraft", 1000, 1000, InflightAbort},
		{"no limit means no decision", "abort", 5000, 0, InflightContinue},
	}
	for _, tc := range cases {
		got := DecideInflight(InflightState{
			Policy: tc.policy, AccruedMicros: tc.accrued, LimitMicros: tc.limit,
		})
		if got != tc.want {
			t.Errorf("%s: DecideInflight = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestInflightLimitOnlyExtendsForOverdraft(t *testing.T) {
	if got := InflightLimit("abort", 1000, 5000); got != 1000 {
		t.Fatalf("abort policy limit = %d, want the reservation only", got)
	}
	if got := InflightLimit("allow_overdraft", 1000, 5000); got != 6000 {
		t.Fatalf("overdraft policy limit = %d, want reservation plus allowance", got)
	}
}

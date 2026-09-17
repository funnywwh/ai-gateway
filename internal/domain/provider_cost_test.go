package domain

import (
	"testing"
	"time"
)

// The window start is the one rule the reader, the router and the console all depend on, so
// it is pinned here as a table rather than exercised indirectly through those three.
func TestProviderCostWindowStart(t *testing.T) {
	reset := func(ts string) *time.Time {
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", ts, err)
		}
		return &parsed
	}
	now := time.Date(2026, time.September, 17, 15, 4, 5, 0, time.UTC)

	cases := []struct {
		name    string
		period  string
		resetAt *time.Time
		want    time.Time
	}{
		{"no period, never reset counts everything", CostPeriodNone, nil, time.Time{}},
		{"no period counts from the reset", CostPeriodNone, reset("2026-09-10T08:00:00Z"),
			time.Date(2026, time.September, 10, 8, 0, 0, 0, time.UTC)},
		{"daily starts at UTC midnight", CostPeriodDaily, nil,
			time.Date(2026, time.September, 17, 0, 0, 0, 0, time.UTC)},
		{"a reset before today's midnight is overtaken by the period", CostPeriodDaily,
			reset("2026-09-16T20:00:00Z"), time.Date(2026, time.September, 17, 0, 0, 0, 0, time.UTC)},
		{"a reset after today's midnight wins", CostPeriodDaily, reset("2026-09-17T09:30:00Z"),
			time.Date(2026, time.September, 17, 9, 30, 0, 0, time.UTC)},
		{"monthly starts on the first at UTC midnight", CostPeriodMonthly, nil,
			time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)},
		{"a reset in the previous month is overtaken", CostPeriodMonthly, reset("2026-08-20T00:00:00Z"),
			time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)},
		{"a reset inside the month wins", CostPeriodMonthly, reset("2026-09-10T12:00:00Z"),
			time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)},
		{"an unknown period degrades to no period", "weekly", reset("2026-09-10T12:00:00Z"),
			time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &Provider{CostPeriod: tc.period, CostWindowStart: tc.resetAt}
			got := ProviderCostWindowStart(provider, now)
			if !got.Equal(tc.want) {
				t.Fatalf("window start = %s, want %s", got, tc.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				t.Fatalf("window start must be UTC, got %s", got.Location())
			}
		})
	}
}

// The period is computed in UTC, not in the zone the gateway happens to run in: an operator
// in +08:00 must still get the UTC month, because the metering rows are UTC.
func TestProviderCostWindowStartUsesUTCForNonUTCInstants(t *testing.T) {
	zone := time.FixedZone("CST", 8*3600)
	// 2026-09-01T02:00:00+08:00 is 2026-08-31T18:00:00Z: still the previous month in UTC.
	now := time.Date(2026, time.September, 1, 2, 0, 0, 0, zone)
	got := ProviderCostWindowStart(&Provider{CostPeriod: CostPeriodMonthly}, now)
	want := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("window start = %s, want %s", got, want)
	}
}

func TestProviderCostWindowStartIsZeroWithoutProvider(t *testing.T) {
	if got := ProviderCostWindowStart(nil, time.Now()); !got.IsZero() {
		t.Fatalf("nil provider = %s, want the zero time", got)
	}
}

func TestProviderCostExceeded(t *testing.T) {
	cases := []struct {
		name  string
		limit int64
		used  int64
		want  bool
	}{
		{"no cap is never exceeded", 0, 1 << 40, false},
		{"a negative cap behaves as no cap", -1, 1 << 40, false},
		{"under the cap", 100, 99, false},
		{"exactly at the cap is spent", 100, 100, true},
		{"over the cap", 100, 101, true},
		{"zero usage with a cap", 100, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &Provider{CostLimitMicros: tc.limit}
			if got := ProviderCostExceeded(provider, tc.used); got != tc.want {
				t.Fatalf("exceeded = %v, want %v", got, tc.want)
			}
		})
	}
	if ProviderCostExceeded(nil, 1) {
		t.Fatal("a nil provider must never be reported as capped")
	}
}

func TestCostCappedAndPeriodNormalization(t *testing.T) {
	if (&Provider{}).CostCapped() {
		t.Fatal("a provider without a limit is not capped")
	}
	if !(&Provider{CostLimitMicros: 1}).CostCapped() {
		t.Fatal("a positive limit is a cap")
	}
	if (*Provider)(nil).CostCapped() {
		t.Fatal("a nil provider is not capped")
	}

	valid := map[string]string{
		"":        CostPeriodNone,
		"none":    CostPeriodNone,
		"daily":   CostPeriodDaily,
		"monthly": CostPeriodMonthly,
	}
	for raw, want := range valid {
		got, err := NormalizeCostPeriod(raw)
		if err != nil {
			t.Fatalf("NormalizeCostPeriod(%q) failed: %v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeCostPeriod(%q) = %q, want %q", raw, got, want)
		}
		if !ValidCostPeriod(raw) {
			t.Fatalf("%q must be reported valid", raw)
		}
	}
	// A typo must be rejected, not read as "unlimited": that would silently turn a monthly
	// budget into a permanently accumulating one.
	for _, raw := range []string{"montly", "DAILY", " ", "week", "None"} {
		if _, err := NormalizeCostPeriod(raw); err == nil {
			t.Fatalf("NormalizeCostPeriod(%q) must fail", raw)
		}
		if ValidCostPeriod(raw) {
			t.Fatalf("%q must be reported invalid", raw)
		}
	}
}

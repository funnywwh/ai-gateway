package pricing

import (
	"math"
	"testing"
)

// The FX table is the only place money changes currency, so these tests pin the
// direction of every rounding decision: movement rounds up, display rounds to
// nearest, and an unknown rate is always reported rather than guessed.
func TestFXTableConversion(t *testing.T) {
	table, err := NewFXTable("USD", map[string]int64{"CNY": 141000})
	if err != nil {
		t.Fatalf("NewFXTable: %v", err)
	}

	cases := []struct {
		name  string
		value int64
		from  string
		to    string
		round Rounding
		want  int64
	}{
		{name: "ledger to ledger is identity", value: 123456, from: "USD", to: "USD", round: RoundCeil, want: 123456},
		{name: "empty code counts as the ledger", value: 123456, from: "", to: "USD", round: RoundCeil, want: 123456},
		{name: "one yuan in micros", value: 1_000_000, from: "CNY", to: "USD", round: RoundCeil, want: 141000},
		{name: "ceil keeps a fraction of a micro", value: 1, from: "CNY", to: "USD", round: RoundCeil, want: 1},
		{name: "back to yuan", value: 141000, from: "USD", to: "CNY", round: RoundHalfUp, want: 1_000_000},
		{name: "half up rounds the fraction down", value: 140999, from: "USD", to: "CNY", round: RoundHalfUp, want: 999993},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := table.Convert(tc.value, tc.from, tc.to, tc.round)
			if !ok {
				t.Fatalf("Convert(%d, %s, %s) reported an unavailable rate", tc.value, tc.from, tc.to)
			}
			if got != tc.want {
				t.Fatalf("Convert(%d, %s, %s) = %d, want %d", tc.value, tc.from, tc.to, got, tc.want)
			}
		})
	}

	// Rounding up and rounding to nearest differ exactly where it matters: a
	// converted charge must never come out below the exact value.
	if got, _ := table.Convert(142000, "CNY", "USD", RoundCeil); got != 20022 {
		t.Fatalf("ceil conversion = %d, want 20022", got)
	}
	if got, _ := table.Convert(142000, "CNY", "USD", RoundHalfUp); got != 20022 {
		t.Fatalf("half-up conversion = %d, want 20022", got)
	}
}

func TestFXTableMissingRateIsReported(t *testing.T) {
	table, err := NewFXTable("USD", map[string]int64{"CNY": 141000})
	if err != nil {
		t.Fatalf("NewFXTable: %v", err)
	}
	if _, ok := table.Convert(1_000_000, "EUR", "USD", RoundCeil); ok {
		t.Fatal("a currency without a rate must not convert")
	}
	if _, ok := table.Convert(1_000_000, "USD", "EUR", RoundHalfUp); ok {
		t.Fatal("converting into a currency without a rate must report unavailable")
	}
	if rate, ok := table.RateOf("EUR"); ok {
		t.Fatalf("RateOf(EUR) = %d, true; want unavailable", rate)
	}
	// Zero is currency-independent: nothing to convert, so no rate is needed.
	if got, ok := table.Convert(0, "EUR", "USD", RoundCeil); !ok || got != 0 {
		t.Fatalf("Convert(0, EUR) = %d, %v; want 0, true", got, ok)
	}
}

// A zero FXTable must reproduce the pre-M22 numbers exactly: every caller that
// has not wired FX (unit tests, snapshot replay) keeps working unchanged.
func TestZeroFXTableDisablesConversion(t *testing.T) {
	var table FXTable
	if table.Enabled() {
		t.Fatal("the zero table must not report itself as enabled")
	}
	for _, code := range []string{"", "USD", "CNY"} {
		got, ok := table.ToLedger(777_000, code)
		if !ok || got != 777_000 {
			t.Fatalf("ToLedger(%s) = %d, %v; want 777000, true", code, got, ok)
		}
	}
	if codes := table.Codes(); len(codes) != 0 {
		t.Fatalf("Codes() = %v, want empty", codes)
	}
}

func TestNewFXTableValidation(t *testing.T) {
	cases := []struct {
		name   string
		ledger string
		rates  map[string]int64
	}{
		{name: "bad ledger code", ledger: "US", rates: nil},
		{name: "ledger listed in the table", ledger: "USD", rates: map[string]int64{"USD": 1_000_000}},
		{name: "zero rate", ledger: "USD", rates: map[string]int64{"CNY": 0}},
		{name: "negative rate", ledger: "USD", rates: map[string]int64{"CNY": -1}},
		{name: "bad rate key", ledger: "USD", rates: map[string]int64{"CN-Y": 141000}},
		{name: "empty rate key", ledger: "USD", rates: map[string]int64{"": 141000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewFXTable(tc.ledger, tc.rates); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
	// Lower-case input is normalized rather than rejected: YAML keys are often
	// typed by hand and the meaning is unambiguous.
	table, err := NewFXTable("usd", map[string]int64{"cny": 141000})
	if err != nil {
		t.Fatalf("NewFXTable must normalize case: %v", err)
	}
	if table.Ledger != "USD" || table.Rates["CNY"] != 141000 {
		t.Fatalf("normalized table = %+v", table)
	}
}

func TestFXStoreReplaceIsAtomicForReaders(t *testing.T) {
	store := NewFXStore("USD", map[string]int64{"CNY": 141000})
	before := store.Snapshot()
	if rate, _ := before.RateOf("CNY"); rate != 141000 {
		t.Fatalf("initial rate = %d", rate)
	}

	store.Replace("USD", map[string]int64{"CNY": 150000, "EUR": 1080000})
	if rate, _ := before.RateOf("CNY"); rate != 141000 {
		t.Fatalf("the snapshot handed out earlier must not change: rate = %d", rate)
	}
	after := store.Snapshot()
	if rate, _ := after.RateOf("CNY"); rate != 150000 {
		t.Fatalf("rate after Replace = %d, want 150000", rate)
	}
	if got := store.Codes(); len(got) != 3 || got[0] != "USD" || got[1] != "CNY" || got[2] != "EUR" {
		t.Fatalf("Codes() = %v, want [USD CNY EUR]", got)
	}

	// An invalid replacement must not wipe out a usable table.
	store.Replace("USD", map[string]int64{"CNY": 0})
	if rate, _ := store.Snapshot().RateOf("CNY"); rate != 150000 {
		t.Fatalf("an invalid Replace changed the table: rate = %d", rate)
	}
}

// Amounts are capped by the 128-bit intermediate: a huge amount times a huge rate
// saturates instead of wrapping into a smaller number.
func TestFXConversionSaturatesInsteadOfWrapping(t *testing.T) {
	table, err := NewFXTable("USD", map[string]int64{"CNY": 1_000_000_000})
	if err != nil {
		t.Fatalf("NewFXTable: %v", err)
	}
	got, ok := table.ToLedger(math.MaxInt64/2, "CNY")
	if !ok {
		t.Fatal("conversion must still be considered available")
	}
	if got != math.MaxInt64 {
		t.Fatalf("saturated conversion = %d, want MaxInt64", got)
	}
	// 1 CNY (1e6 micros) at 1e9 micros per unit is 1e9 micros of the ledger.
	if got, ok := table.ToLedger(1_000_000, "CNY"); !ok || got != 1_000_000_000 {
		t.Fatalf("ordinary conversion = %d, %v", got, ok)
	}
}

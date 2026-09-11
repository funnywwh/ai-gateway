package pricing

import (
	"encoding/json"
	"math"
	"math/bits"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/winger/ai-gateway/internal/domain"
)

// Money in this gateway is an int64 count of micros of one currency. M22 lets a
// model be priced in a currency of its own (an upstream that bills in CNY, a
// white-label price list in CNY) while the ledger — balances, credit limits,
// reservations, invoices, reconciliation — stays a single currency. This file is
// the whole conversion surface: integer arithmetic only, one rounding rule per
// direction, and no clock or configuration access.

// CurrencyRE is the shape every currency code must have: three uppercase letters
// (ISO-4217 style). The gateway does not embed a currency table, so it validates
// the shape and lets the operator name any currency the FX table knows.
var CurrencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// NormalizeCurrency trims and upper-cases a currency code. An empty code stays
// empty: callers substitute the ledger currency for "not declared".
func NormalizeCurrency(code string) (string, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(code))
	if trimmed == "" {
		return "", nil
	}
	if !CurrencyRE.MatchString(trimmed) {
		return "", domain.ErrInvalidRequest("currency " + code + " must be three uppercase letters, such as USD or CNY")
	}
	return trimmed, nil
}

// Rounding selects how a converted amount is rounded.
type Rounding int

const (
	// RoundCeil rounds any fraction up. Money movement uses it: a charge is never
	// under-stated, and a recorded cost is never under-stated either — the same
	// "rather over-state than under-state" rule the pricing docs apply to usage.
	RoundCeil Rounding = iota
	// RoundHalfUp rounds to nearest. Display conversion uses it, because a shown
	// amount must not be biased. It never touches a stored amount.
	RoundHalfUp
)

// FXTable converts between currencies through the ledger currency.
//
// Rates[X] is how many micros of the ledger currency one whole unit of X is
// worth, so {Ledger: "USD", Rates: {"CNY": 141000}} means 1 CNY = 0.141000 USD.
// The ledger currency itself is implicit (RateScale) and must not appear in Rates.
//
// The zero value (Ledger == "") disables conversion: every currency is then read
// as the ledger currency, which is exactly the pre-M22 behaviour, so callers that
// never wired an FX table (unit tests, replay helpers) keep their old numbers.
type FXTable struct {
	Ledger string
	Rates  map[string]int64
}

// Enabled reports whether the table performs real conversions.
func (t FXTable) Enabled() bool { return t.Ledger != "" }

// RateOf returns micros of the ledger currency per whole unit of code. An empty
// code or the ledger currency itself is RateScale.
func (t FXTable) RateOf(code string) (int64, bool) {
	if !t.Enabled() {
		return RateScale, true
	}
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if normalized == "" || normalized == t.Ledger {
		return RateScale, true
	}
	rate, ok := t.Rates[normalized]
	if !ok || rate <= 0 {
		return 0, false
	}
	return rate, true
}

// Convert moves an amount (or a per-unit rate) from one currency to another:
// value_to = value_from × rate(from) / rate(to). The second return value is false
// when either currency has no usable rate, and callers must treat that as "cannot
// price this" rather than silently using 1:1.
func (t FXTable) Convert(value int64, from, to string, round Rounding) (int64, bool) {
	if value == 0 {
		return 0, true
	}
	fromRate, ok := t.RateOf(from)
	if !ok {
		return 0, false
	}
	toRate, ok := t.RateOf(to)
	if !ok {
		return 0, false
	}
	if fromRate == toRate {
		return value, true
	}
	if round == RoundHalfUp {
		return scaleHalfUp(value, fromRate, toRate), true
	}
	return scaleCeil(value, fromRate, toRate), true
}

// RateBetween returns micros of the `to` currency per one whole unit of `from`,
// i.e. the same unit the configuration uses. It is what gets recorded in a
// pricing snapshot, so a historical conversion can be replayed without the
// operator's current rate table.
func (t FXTable) RateBetween(from, to string) (int64, bool) {
	fromRate, ok := t.RateOf(from)
	if !ok {
		return 0, false
	}
	toRate, ok := t.RateOf(to)
	if !ok {
		return 0, false
	}
	if fromRate == toRate {
		return RateScale, true
	}
	return scaleCeil(fromRate, RateScale, toRate), true
}

// ToLedger converts an amount into the ledger currency, rounding up.
func (t FXTable) ToLedger(value int64, from string) (int64, bool) {
	return t.Convert(value, from, t.Ledger, RoundCeil)
}

// FromLedger converts a ledger amount into another currency for display, rounding
// to nearest.
func (t FXTable) FromLedger(value int64, to string) (int64, bool) {
	return t.Convert(value, t.Ledger, to, RoundHalfUp)
}

// Codes lists every currency the table can convert to or from: the ledger first,
// then the rate keys in sorted order.
func (t FXTable) Codes() []string {
	out := []string{}
	if t.Enabled() {
		out = append(out, t.Ledger)
	}
	keys := make([]string, 0, len(t.Rates))
	for code, rate := range t.Rates {
		if rate <= 0 || code == t.Ledger {
			continue
		}
		keys = append(keys, code)
	}
	sort.Strings(keys)
	return append(out, keys...)
}

// NewFXTable normalizes a ledger currency and rate map into a table.
func NewFXTable(ledger string, rates map[string]int64) (FXTable, error) {
	normalized, err := NormalizeCurrency(ledger)
	if err != nil {
		return FXTable{}, err
	}
	if normalized == "" {
		return FXTable{}, nil
	}
	table := FXTable{Ledger: normalized, Rates: map[string]int64{}}
	for code, rate := range rates {
		key, err := NormalizeCurrency(code)
		if err != nil {
			return FXTable{}, err
		}
		if key == "" {
			return FXTable{}, domain.ErrInvalidRequest("fx_rates contains an empty currency code")
		}
		if key == normalized {
			return FXTable{}, domain.ErrInvalidRequest("fx_rates must not contain the ledger currency " + normalized + " (it is always 1:1)")
		}
		if rate <= 0 {
			return FXTable{}, domain.ErrInvalidRequest("fx_rates[" + key + "] must be a positive number of micros")
		}
		table.Rates[key] = rate
	}
	return table, nil
}

// FXStore holds the live FX table: it is built from configuration at start-up and
// replaced when an operator edits the rates in the console, without a restart.
// Readers take an immutable snapshot, so the request path never locks.
type FXStore struct {
	cur atomic.Pointer[FXTable]
}

// NewFXStore builds a store around one table. Rates are copied so the caller
// cannot mutate a table that readers are using.
func NewFXStore(ledger string, rates map[string]int64) *FXStore {
	store := &FXStore{}
	store.Replace(ledger, rates)
	return store
}

// Snapshot returns the current table. The returned maps are never mutated in
// place, so it is safe to keep for the duration of one request.
func (s *FXStore) Snapshot() FXTable {
	if s == nil {
		return FXTable{}
	}
	current := s.cur.Load()
	if current == nil {
		return FXTable{}
	}
	return *current
}

// Replace swaps in a new ledger currency and rate table.
func (s *FXStore) Replace(ledger string, rates map[string]int64) {
	if s == nil {
		return
	}
	table, err := NewFXTable(ledger, rates)
	if err != nil {
		// A store is only ever built from validated configuration, so this cannot
		// happen; keeping the old table is the least surprising fallback.
		return
	}
	s.cur.Store(&table)
}

// Codes lists the currencies of the current table.
func (s *FXStore) Codes() []string { return s.Snapshot().Codes() }

// Ledger returns the current ledger currency ("" when no table is configured).
func (s *FXStore) Ledger() string { return s.Snapshot().Ledger }

// DeclaredCurrency reads the currency a stored rule document declares; it returns
// "" when the document declares none. It is deliberately lenient: a malformed
// document is reported by the parser that owns it, while this helper only feeds
// currency bookkeeping (which currencies the console must ask about).
func DeclaredCurrency(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var wire struct {
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wire); err != nil {
		return ""
	}
	if code, err := NormalizeCurrency(wire.Currency); err == nil {
		return code
	}
	return strings.ToUpper(strings.TrimSpace(wire.Currency))
}

// SettingFXRates is the runtime settings key that overrides billing.fx_rates from
// the console. The value is the same shape as the configuration: a JSON object of
// currency -> micros of the ledger currency per whole unit.
const SettingFXRates = "billing.fx_rates"

// ParseFXRates reads the console override. Micros are integers by contract, so a
// decimal is rejected with an explanation instead of being silently rounded.
func ParseFXRates(raw string) (map[string]int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return map[string]int64{}, nil
	}
	var wire map[string]json.Number
	if err := json.Unmarshal([]byte(trimmed), &wire); err != nil {
		return nil, domain.ErrInvalidRequest(`fx rates must be a JSON object of currency -> micros, for example {"CNY": 141000}`)
	}
	out := make(map[string]int64, len(wire))
	for code, number := range wire {
		value, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return nil, domain.ErrInvalidRequest("fx rate for " + code + " must be an integer number of micros (1 CNY = 0.141000 USD is written 141000)")
		}
		if value <= 0 {
			return nil, domain.ErrInvalidRequest("fx rate for " + code + " must be positive")
		}
		key, err := NormalizeCurrency(code)
		if err != nil {
			return nil, err
		}
		if key == "" {
			return nil, domain.ErrInvalidRequest("fx rates must not contain an empty currency code")
		}
		out[key] = value
	}
	return out, nil
}

// MergeFXRates overlays the console override onto the configured table.
func MergeFXRates(base, override map[string]int64) map[string]int64 {
	merged := make(map[string]int64, len(base)+len(override))
	for code, rate := range base {
		merged[code] = rate
	}
	for code, rate := range override {
		merged[code] = rate
	}
	return merged
}

// scaleCeil computes ceil(value × numer / denom) in 128 bits so a realistic amount
// (up to ~1e15 micros) times a rate (up to ~1e9) cannot overflow int64. A result
// that would not fit saturates at MaxInt64: over-stating a charge is recoverable,
// silently wrapping it is not.
func scaleCeil(value, numer, denom int64) int64 {
	if value <= 0 || numer <= 0 || denom <= 0 {
		return 0
	}
	quotient, remainder := mulDiv128(value, numer, denom)
	if quotient == math.MaxInt64 {
		return quotient
	}
	if remainder > 0 {
		quotient++
	}
	return quotient
}

// scaleHalfUp computes round-half-up(value × numer / denom) in 128 bits.
func scaleHalfUp(value, numer, denom int64) int64 {
	if value <= 0 || numer <= 0 || denom <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(value), uint64(numer))
	if denom > 1 {
		var carry uint64
		lo, carry = bits.Add64(lo, uint64(denom/2), 0)
		hi += carry
	}
	if hi >= uint64(denom) {
		return math.MaxInt64
	}
	quotient, _ := bits.Div64(hi, lo, uint64(denom))
	if quotient > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(quotient)
}

// mulDiv128 returns value × numer / denom with the remainder, using a 128-bit
// intermediate. MaxInt64 signals saturation.
func mulDiv128(value, numer, denom int64) (int64, uint64) {
	if denom <= 0 {
		return 0, 0
	}
	hi, lo := bits.Mul64(uint64(value), uint64(numer))
	if hi >= uint64(denom) {
		return math.MaxInt64, 0
	}
	quotient, remainder := bits.Div64(hi, lo, uint64(denom))
	if quotient > math.MaxInt64 {
		return math.MaxInt64, 0
	}
	return int64(quotient), remainder
}

package billing

import (
	"sort"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pricing"
)

// Reservation is one in-flight request's hold on an account's balance.
type Reservation struct {
	ID           string    `json:"id"`
	AccountID    int64     `json:"account_id"`
	AmountMicros int64     `json:"amount_micros"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ReservationTable tracks in-flight holds per account. It lives in memory on purpose:
// reservations protect against overselling during the seconds a request is running,
// and a restart has no in-flight requests to protect.
type ReservationTable struct {
	mu        sync.Mutex
	ttl       time.Duration
	byAccount map[int64]map[string]*Reservation
}

// NewReservationTable builds an empty table. A non-positive TTL disables expiry.
func NewReservationTable(ttl time.Duration) *ReservationTable {
	return &ReservationTable{ttl: ttl, byAccount: map[int64]map[string]*Reservation{}}
}

// Reserve records a hold. Re-reserving the same id replaces the amount, which makes
// retries idempotent.
func (t *ReservationTable) Reserve(id string, accountID, amountMicros int64, now time.Time) *Reservation {
	if id == "" || accountID == 0 || amountMicros <= 0 {
		return nil
	}
	reservation := &Reservation{
		ID: id, AccountID: accountID, AmountMicros: amountMicros, CreatedAt: now.UTC(),
	}
	if t.ttl > 0 {
		reservation.ExpiresAt = now.UTC().Add(t.ttl)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	holds := t.byAccount[accountID]
	if holds == nil {
		holds = map[string]*Reservation{}
		t.byAccount[accountID] = holds
	}
	holds[id] = reservation
	return reservation
}

// Release drops a hold. Releasing an unknown id is a no-op.
func (t *ReservationTable) Release(id string, accountID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	holds := t.byAccount[accountID]
	if holds == nil {
		return
	}
	delete(holds, id)
	if len(holds) == 0 {
		delete(t.byAccount, accountID)
	}
}

// Touch extends a long-running request's hold so the GC does not reap it mid-flight.
func (t *ReservationTable) Touch(id string, accountID int64, now time.Time) {
	if t.ttl <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if holds := t.byAccount[accountID]; holds != nil {
		if reservation, ok := holds[id]; ok {
			reservation.ExpiresAt = now.UTC().Add(t.ttl)
		}
	}
}

// InFlight sums the holds of one account (0 means "all accounts").
func (t *ReservationTable) InFlight(accountID int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if accountID != 0 {
		return sumReservations(t.byAccount[accountID])
	}
	var total int64
	for _, holds := range t.byAccount {
		total += sumReservations(holds)
	}
	return total
}

// GC drops expired holds and reports how many it removed.
func (t *ReservationTable) GC(now time.Time) int {
	if t.ttl <= 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	removed := 0
	for accountID, holds := range t.byAccount {
		for id, reservation := range holds {
			if !reservation.ExpiresAt.IsZero() && reservation.ExpiresAt.Before(now.UTC()) {
				delete(holds, id)
				removed++
			}
		}
		if len(holds) == 0 {
			delete(t.byAccount, accountID)
		}
	}
	return removed
}

// Snapshot lists the current holds (oldest first) for the console.
func (t *ReservationTable) Snapshot() []Reservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []Reservation{}
	for _, holds := range t.byAccount {
		for _, reservation := range holds {
			out = append(out, *reservation)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func sumReservations(holds map[string]*Reservation) int64 {
	var total int64
	for _, reservation := range holds {
		total += reservation.AmountMicros
	}
	return total
}

// EstimateInput describes what a request may consume (worst case).
type EstimateInput struct {
	Cost *pricing.RuleSet
	Sale *pricing.RuleSet
	// MaxOutputTokens is the effective output cap (request and model limit combined).
	MaxOutputTokens int64
	// EstInputTokens is the prompt size estimate.
	EstInputTokens int64
	// DefaultMarkupBP applies when the sale side is cost-follow.
	DefaultMarkupBP int
}

// EstimateReserve computes the amount to hold for one request using the most
// expensive rate that could apply, so a prepaid account can never be oversold as
// long as upstream honours max_output_tokens.
func EstimateReserve(in EstimateInput) int64 {
	costRates := pricing.WorstCaseRates(in.Cost)
	saleRates := pricing.WorstCaseRates(in.Sale)

	// Input tokens are not distinguished here: charge both cache buckets to be safe.
	inputUnits := in.EstInputTokens
	total := int64(0)
	total += in.MaxOutputTokens * worstRate(worstOf(costRates["output"], saleRates["output"]), in)
	total += inputUnits * worstRate(worstOf(costRates["input"], saleRates["input"]), in)
	if fee := worstOf(costRates["per_request"], saleRates["per_request"]); fee > 0 {
		total += fee
	}
	return ceilDiv(total, pricing.RateScale)
}

// worstRate applies the sale mark-up when the sale side is cost-follow (or absent).
func worstRate(costRate int64, in EstimateInput) int64 {
	if in.Sale != nil && in.Sale.Basis == pricing.BasisAbsolute {
		return costRate
	}
	markup := in.DefaultMarkupBP
	if markup <= 0 {
		markup = 10000
	}
	return ceilDiv(costRate*int64(markup), 10000)
}

func worstOf(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func ceilDiv(value, divisor int64) int64 {
	if value <= 0 || divisor <= 0 {
		return 0
	}
	if value%divisor == 0 {
		return value / divisor
	}
	return value/divisor + 1
}

// AdmissionInput is the state needed to decide whether a request may start.
type AdmissionInput struct {
	Account  *domain.Account
	Reserve  int64
	InFlight int64
	// Policy is the effective in-flight policy (warn|throttle|abort|allow_overdraft).
	Policy string
}

// Admission is the decision plus the numbers behind it.
type Admission struct {
	Allowed         bool   `json:"allowed"`
	Reason          string `json:"reason,omitempty"`
	AvailableMicros int64  `json:"available_micros"`
	ReserveMicros   int64  `json:"reserve_micros"`
	FloorMicros     int64  `json:"floor_micros"`
}

// DecideAdmission implements docs/billing.md section 4: balance minus in-flight holds
// must cover the reservation, with postpaid accounts allowed down to their credit
// limit and overdraft accounts down to their overdraft limit.
func DecideAdmission(in AdmissionInput) Admission {
	if in.Account == nil {
		return Admission{Allowed: false, Reason: "account_not_found"}
	}
	available := in.Account.BalanceMicros - in.InFlight
	decision := Admission{AvailableMicros: available, ReserveMicros: in.Reserve}

	switch in.Account.Status {
	case "suspended", "closed":
		decision.Reason = "account_" + in.Account.Status
		return decision
	}

	floor := int64(0)
	switch {
	case in.Account.BillingMode == domain.BillingPostpaid:
		floor = -in.Account.CreditLimitMicros
	case in.Policy == "allow_overdraft":
		floor = -in.Account.OverdraftLimitMicros
	}
	decision.FloorMicros = floor
	if available-in.Reserve < floor {
		decision.Reason = "insufficient_quota"
		return decision
	}
	decision.Allowed = true
	return decision
}

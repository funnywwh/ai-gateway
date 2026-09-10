package domain

import "time"

// Invoice is one billing period summary for an account.
type Invoice struct {
	ID                int64
	AccountID         int64
	PeriodStart       time.Time
	PeriodEnd         time.Time
	Status            string // draft|issued|paid|void
	Currency          string
	TotalCostMicros   int64
	TotalChargeMicros int64
	IssuedAt          *time.Time
	PaidAt            *time.Time
	VoidedAt          *time.Time
	Note              string
	CreatedAt         time.Time
	Lines             []InvoiceLine
}

// InvoiceLine is one grouped row of an invoice.
type InvoiceLine struct {
	InvoiceID        int64
	GroupType        string
	GroupKey         string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	CostMicros       int64
	ChargeMicros     int64
}

// RedemptionCode is a prepaid voucher. Only the hash is stored; the plaintext is
// shown once at creation time.
type RedemptionCode struct {
	ID                  int64
	CodeHash            string
	AmountMicros        int64
	ExpiresAt           *time.Time
	RedeemedByAccountID *int64
	RedeemedAt          *time.Time
	BatchID             string
	CreatedBy           string
	Note                string
	CreatedAt           time.Time
}

// Reconciliation is one reconciliation run over a window.
type Reconciliation struct {
	ID                 int64
	PeriodStart        time.Time
	PeriodEnd          time.Time
	Kind               string
	UsageChargeMicros  int64
	LedgerChargeMicros int64
	DiffMicros         int64
	MissingUsageCount  int64
	EstimatedRatioBP   int64
	DetailsJSON        string
	CreatedAt          time.Time
}

// BackupJob is one database snapshot attempt.
type BackupJob struct {
	ID          int64
	StartedAt   time.Time
	FinishedAt  *time.Time
	Path        string
	SizeBytes   int64
	Status      string
	QuickCheck  string
	TriggeredBy string
	Note        string
}

// UsageTotals is a single-row aggregate over one account's usage.
type UsageTotals struct {
	Attempts         int64
	Failed           int64
	Estimated        int64
	PromptTokens     int64
	CompletionTokens int64
	CostMicros       int64
	ChargeMicros     int64
	TTFTAvgMS        int64
	TTFTP95MS        int64
	// TTFTP95Estimated is true when the percentile was computed from a capped sample.
	TTFTP95Estimated bool
}

// UsageBreakdownRow is one group of a usage breakdown.
type UsageBreakdownRow struct {
	GroupKey         string
	Requests         int64
	Failed           int64
	PromptTokens     int64
	CompletionTokens int64
	CostMicros       int64
	ChargeMicros     int64
}

// UsageCounter is the pre-aggregated rollup used for quotas and reporting.
type UsageCounter struct {
	AccountID    int64
	APIKeyID     int64
	Tag          string
	Period       string
	Requests     int64
	Tokens       int64
	CostMicros   int64
	ChargeMicros int64
}

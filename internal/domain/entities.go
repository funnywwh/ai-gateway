package domain

import "time"

// BillingMode selects how an account is charged.
type BillingMode string

const (
	BillingPrepaid  BillingMode = "prepaid"
	BillingPostpaid BillingMode = "postpaid"
)

// Account is the billing subject (tenant). API keys belong to an account.
type Account struct {
	ID                        int64
	Name                      string
	BillingMode               BillingMode
	BalanceMicros             int64
	CreditLimitMicros         int64
	LowBalanceThresholdMicros int64
	PriceOverridesJSON        string
	MarkupOverrideBP          int
	AutoSuspend               bool
	AutoResume                bool
	InflightPolicyOverride    string
	OverdraftLimitMicros      int64
	Status                    string
	Note                      string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// Provider is one upstream provider instance (builtin kind or plugin process).
type Provider struct {
	ID                 int64
	Name               string
	Kind               string
	DisplayName        string
	ConfigJSON         string
	ConfigVersion      int
	CredentialsEnc     []byte
	StateDir           string
	MetaJSON           string
	DiscoveredJSON     string
	HealthJSON         string
	LastError          string
	Enabled            bool
	Priority           int
	Weight             int
	MaxInflight        int
	TimeoutOverrides   string
	Degradation        string
	CooldownUntil      *time.Time
	Draining           bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ProviderModel maps a public model name to an upstream model name for one provider.
type ProviderModel struct {
	ID                   int64
	ProviderID           int64
	PublicModel          string
	UpstreamModel        string
	Enabled              bool
	Priority             int
	Weight               int
	ContextWindow        int
	MaxOutputTokens      int
	PricingRulesJSON     string
	CapabilitiesJSON     string
	CapabilitiesOverride string
	Source               string
	UpdatedAt            time.Time
}

// Model is a client-facing (canonical) model entry.
type Model struct {
	ID              int64
	PublicName      string
	DisplayName     string
	AliasesJSON     string
	Enabled         bool
	SalePricingJSON string
	PolicyJSON      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ModelMapping is a free-form mapping rule from a requested name to a model or a pinned provider model.
type ModelMapping struct {
	ID                  int64
	Kind                string // exact|prefix|glob|regex
	Pattern             string
	TargetModel         string
	TargetProviderID    int64
	TargetUpstreamModel string
	Priority            int
	Enabled             bool
	Note                string
	CreatedAt           time.Time
}

// Route binds a model to a provider (with its upstream model name).
type Route struct {
	ID            int64
	ModelID       int64
	ProviderID    int64
	UpstreamModel string
	Priority      int
	Weight        int
	Enabled       bool
	PolicyJSON    string
	CooldownUntil *time.Time
}

// Tag grants models/providers to a group of API keys and carries policy.
type Tag struct {
	ID          int64
	Name        string
	Description string
	GrantsJSON  string
	PolicyJSON  string
	Priority    int
	CreatedAt   time.Time
}

// APIKey authenticates API traffic. Only the hash is stored.
type APIKey struct {
	ID               int64
	AccountID        int64
	Name             string
	KeyPrefix        string
	KeyHash          string
	TagsJSON         string
	GrantsJSON       string
	PolicyJSON       string
	RecordInputMode  string
	RecordOutputText bool
	RecordReasoning  bool
	Status           string
	ExpiresAt        *time.Time
	LastUsedAt       *time.Time
	CreatedBy        string
	CreatedAt        time.Time
}

// MCPToken authenticates the read-only MCP query endpoint.
type MCPToken struct {
	ID         int64
	AccountID  int64
	Name       string
	TokenHash  string
	TokenPrefix string
	Status     string
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	CreatedBy  string
	Note       string
	CreatedAt  time.Time
}

// ResponseRecord is one stored Responses API response (for GET /v1/responses/{id}
// and previous_response_id continuation).
type ResponseRecord struct {
	ID           string
	APIKeyID     int64
	AccountID    int64
	Model        string
	ProviderID   int64
	Status       string
	RequestJSON  string
	OutputJSON   string
	UsageJSON    string
	Instructions string
	CreatedAt    time.Time
	CompletedAt  *time.Time
	ExpiresAt    *time.Time
}

// RequestLogRecord is one recorded request/response pair. Input text is recorded by
// default; thinking text and final output text are only stored when the key opts in.
type RequestLogRecord struct {
	ID                  int64
	RequestID           string
	APIKeyID            int64
	AccountID           int64
	Endpoint            string
	RequestJSON         string
	ResponseReasoning   string
	ResponseText        string
	ReasoningRecorded   bool
	OutputTextRecorded  bool
	RequestBytes        int
	ResponseBytes       int
	Truncated           bool
	RecordInputMode     string
	RecordReasoning     bool
	RecordOutputText    bool
	Status              string
	CreatedAt           time.Time
}

// LedgerEntry is an append-only balance mutation.
type LedgerEntry struct {
	ID                int64
	AccountID         int64
	APIKeyID          *int64
	Kind              string // credit_grant|topup|charge|adjustment|refund|expire
	AmountMicros      int64
	BalanceAfterMicros int64
	RefType           string
	RefID             string
	IdemKey           string
	RebuildSeq        int64
	Note              string
	Actor             string
	CreatedAt         time.Time
}

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
	// MarkupOverrideSet distinguishes "no account override" from "override to 0".
	MarkupOverrideSet      bool
	AutoSuspend            bool
	AutoResume             bool
	InflightPolicyOverride string
	OverdraftLimitMicros   int64
	Status                 string
	Note                   string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Provider is one upstream provider instance (builtin kind or plugin process).
type Provider struct {
	ID               int64
	Name             string
	Kind             string
	DisplayName      string
	ConfigJSON       string
	ConfigVersion    int
	CredentialsEnc   []byte
	StateDir         string
	MetaJSON         string
	DiscoveredJSON   string
	HealthJSON       string
	LastError        string
	Enabled          bool
	Priority         int
	Weight           int
	MaxInflight      int
	TimeoutOverrides string
	Degradation      string
	CooldownUntil    *time.Time
	Draining         bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
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

// MCPToken authenticates the MCP endpoint. Scope decides whether the token only
// reads its own account (query) or may also drive the management API
// (admin_read / admin).
type MCPToken struct {
	ID          int64
	AccountID   int64
	Name        string
	TokenHash   string
	TokenPrefix string
	Scope       string
	Status      string
	LastUsedAt  *time.Time
	ExpiresAt   *time.Time
	CreatedBy   string
	Note        string
	CreatedAt   time.Time
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
//
// The identity columns (Client .. Title) are extracted from the parsed request and are
// recorded regardless of the input policy — including `off`, which keeps a row with no
// content. They are what makes "who is calling, with which model, from where" answerable
// without reading a body that may have been truncated or pruned.
type RequestLogRecord struct {
	ID                 int64
	RequestID          string
	APIKeyID           int64
	AccountID          int64
	Endpoint           string
	RequestJSON        string
	ResponseReasoning  string
	ResponseText       string
	ReasoningRecorded  bool
	OutputTextRecorded bool
	RequestBytes       int
	ResponseBytes      int
	Truncated          bool
	RecordInputMode    string
	RecordReasoning    bool
	RecordOutputText   bool
	Status             string
	CreatedAt          time.Time

	// Client is dsh | codex | unknown; CallKind is agent | title.
	Client string
	// Model is the model name the client asked for (the billed dimension: usage_records
	// and the invoice breakdown group by it); ResolvedModel is the canonical model the
	// gateway routed to. A locally rejected request never reached routing, so its
	// ResolvedModel is empty.
	Model         string
	ResolvedModel string
	// Workspace is the client's workspace root, "" when it did not send one.
	Workspace string
	// SessionID is the client's session key (the request's prompt_cache_key).
	SessionID string
	CallKind  string
	// Title is the session title produced by a title call; it is only ever set on the row
	// whose CallKind is title.
	Title string
}

// RequestUsage is the metered consumption of one request, summed over its attempts. It is
// read from usage_records rather than duplicated onto the log row: usage is the single
// owner of the money-adjacent facts, and it outlives the log (retention prunes logs, not
// billing).
//
// Metered is false when the request has no usage row at all — a locally rejected request
// deliberately has none (it never reached an upstream). That is a different statement from
// "consumed zero tokens", and the console shows them differently.
type RequestUsage struct {
	RequestID       string
	Attempts        int
	InputTokens     int64
	CachedTokens    int64 // Cache-hit tokens, already included in InputTokens.
	OutputTokens    int64
	ReasoningTokens int64
	CostMicros      int64
	ChargeMicros    int64
	LatencyMS       int
	TTFTMS          int
	Metered         bool
}

// RequestLogFilter selects recorded requests for every read path (the console's page, its
// totals, the MCP query tools and the dimension breakdown). Every field is optional: the
// zero value means "all accounts, no window, no dimension filter".
//
// It lives in the domain layer because the MCP service may not import the store
// (internal/arch), and both have to describe the same query.
type RequestLogFilter struct {
	AccountID int64
	// APIKeyID narrows to one API key; 0 means "every key". It is an exact match on the
	// credential the request authenticated with, so it also covers requests the gateway
	// rejected locally (they never reached a model but they did arrive with a key).
	APIKeyID int64
	From     time.Time
	To       time.Time

	// Dimension filters, each an exact match; "" means "do not filter".
	Client        string
	Model         string
	ResolvedModel string
	Workspace     string
	SessionID     string
	CallKind      string
}

// APIKeyLabel is the read-time label of one API key in the request log's Key dimension:
// the operator-chosen name plus the stable prefix. It deliberately carries no hash.
//
// The label is resolved when a page (or a dimension bucket) is read rather than copied
// onto the log row, for the same reason token and cost stay in usage_records: a name is a
// mutable fact owned by another table, and a second copy would drift silently — a rename
// would split one key into two buckets. api_keys.name is not unique (two keys of one
// account may share a name), which is why grouping keys on the id and only displays this.
type APIKeyLabel struct {
	Name   string
	Prefix string
}

// RequestLogDimensionRow is one bucket of the request log's dimension breakdown. Title and
// Workspace only carry meaning when the grouping is by session, where a group owns one
// title and one workspace; for other groupings they are what the group's rows had.
//
// Key is the dimension's own value, except for the two credential dimensions (account,
// api_key) where it is the numeric id as a string: names belong to another table and are
// resolved by the caller, so that a rename cannot split or merge a bucket. An empty Key on
// those groupings is the unknown bucket (rows whose id is 0 or absent).
type RequestLogDimensionRow struct {
	Key             string
	Requests        int
	Metered         int
	FirstSeen       time.Time
	LastSeen        time.Time
	Title           string
	Workspace       string
	InputTokens     int64
	CachedTokens    int64 // Cache-hit tokens, already included in InputTokens.
	OutputTokens    int64
	ReasoningTokens int64
	CostMicros      int64
	ChargeMicros    int64
}

// LedgerEntry is an append-only balance mutation.
type LedgerEntry struct {
	ID                 int64
	AccountID          int64
	APIKeyID           *int64
	Kind               string // credit_grant|topup|charge|adjustment|refund|expire
	AmountMicros       int64
	BalanceAfterMicros int64
	RefType            string
	RefID              string
	IdemKey            string
	RebuildSeq         int64
	Note               string
	Actor              string
	CreatedAt          time.Time
	// ExpiresAt applies to credit_grant entries: unused gift credit matures here and is
	// written off by the expiry job.
	ExpiresAt *time.Time
}

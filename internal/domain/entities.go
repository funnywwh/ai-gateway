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
	ID   int64
	Name string
	// TagsJSON stores the account-level tag names. API keys inherit these names at
	// request time and union them with their own tags.
	TagsJSON                  string
	BillingMode               BillingMode
	BalanceMicros             int64
	CreditLimitMicros         int64
	LowBalanceThresholdMicros int64
	PriceOverridesJSON        string
	MarkupOverrideBP          int
	// MarkupOverrideSet distinguishes "no account override" from "override to 0".
	MarkupOverrideSet bool
	AutoSuspend       bool
	AutoResume        bool
	// DSHEnabled gates the account's access to the dsh multi-tenant gateway (M52).
	// The console owns this flag; dshgw consults POST /v1/dshgw/authorize.
	DSHEnabled bool
	// DshTenant names the dshgw tenant the account enters once enabled (M52-rev2). All
	// keys of the account — existing and newly created — log into this tenant; no
	// per-key prefix binding is required.
	DshTenant string
	// DshDisabledAt is when an administrator explicitly turned DSH off for this account
	// (M72). Nil means "never explicitly disabled", which is what lets dshgw.auto_enable
	// hand DSH to every active account while an administrator's 停用 still wins — the two
	// states dsh_enabled alone cannot tell apart.
	DshDisabledAt          *time.Time
	InflightPolicyOverride string
	OverdraftLimitMicros   int64
	Status                 string
	Note                   string
	CreatedAt              time.Time
	UpdatedAt              time.Time

	// Feishu identity of the account (M72: this IS the DSH portal login identity; M70 had
	// written it as a directory-sync mapping only). One Feishu person maps to one account and
	// back — the database enforces both directions through a unique index — and the columns
	// mirror the key-level ones from M60 so an audit line reads the same either way.
	// FeishuBoundBy says who wrote it ("sync" for an automatic merge, an admin username for a
	// manual binding, "key-migration" for the M72 backfill of a legacy key-level binding).
	FeishuOpenID  string
	FeishuUnionID string
	FeishuName    string
	FeishuBoundAt *time.Time
	FeishuBoundBy string
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
	// CostLimitMicros caps what this upstream may cost us before the router stops
	// choosing it (M56): 0 = unlimited, which is the default. CostPeriod decides when
	// the accumulation restarts on its own, and CostWindowStart is the last manual
	// reset (nil = never) — a reset only moves that instant forward, so no metering
	// row is ever rewritten. See docs/design/m56-provider-cost-cap.md.
	CostLimitMicros int64
	CostPeriod      string
	CostWindowStart *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
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
	ReasoningJSON   string
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
	// Feishu identity bound to this key (M60). Empty open id means unbound. The
	// identity never authenticates a data-plane request by itself: it only says which
	// person may enter the DSH portal as this key's account (see docs/feishu.md).
	FeishuOpenID  string
	FeishuUnionID string
	FeishuName    string
	FeishuBoundAt *time.Time
	FeishuBoundBy string
}

// FeishuBinding is the read/write projection of a key's Feishu identity, so callers that
// only care about the binding do not have to carry a whole APIKey around.
type FeishuBinding struct {
	OpenID  string
	UnionID string
	Name    string
	BoundAt time.Time
	BoundBy string
}

// Bound reports whether the projection holds an identity.
func (b FeishuBinding) Bound() bool { return b.OpenID != "" }

// KeyFeishuIdentity is one API key's bound Feishu identity together with the account that
// key belongs to. The directory sync (M70) reads them all at once to recognize a Feishu
// person that was bound at the key level before the account-level mapping existed, and
// offers to promote that identity onto the account.
type KeyFeishuIdentity struct {
	KeyID     int64
	AccountID int64
	Binding   FeishuBinding
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
	// TitleFingerprint and StartedAt are private correlation evidence, not API fields.
	TitleFingerprint   string    `json:"-"`
	StartedAt          time.Time `json:"-"`
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
	// MatchedRule names the mapping rule that turned the requested model into ResolvedModel
	// (model:<public> / mapping:<kind>:<pattern> / alias:<x> / fallback:<y>). Overlapping
	// patterns make "which rule fired" unanswerable after the fact, so the request records
	// it. It follows resolved_model's rule: identity metadata, recorded even when content
	// recording is off, and empty for a locally rejected request (which never reached a model).
	MatchedRule string
	// ReasoningEffort is the effort on the canonical provider request after the
	// model-level policy has been applied. Empty means no effort was applied.
	ReasoningEffort string
	// Workspace is the client's workspace root, "" when it did not send one.
	Workspace string
	// SessionID is the explicit root session identity, with prompt_cache_key as fallback.
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

// RequestAttempt is one metered upstream attempt of a recorded request: which route the
// gateway selected, which provider served it, which upstream model name it sent, and how
// that attempt ended. It is the per-attempt half of "which route did this request take",
// read from usage_records because a request can fail over between providers — the log row
// holds one request, and that fact is one-to-many (see docs/design/m78-request-log-route-path.md).
//
// ProviderName is not here: a name is a mutable label owned by the providers table, so the
// caller resolves it for the attempts in hand, exactly as it does for the provider payloads.
type RequestAttempt struct {
	AttemptNo        int
	ProviderID       int64
	RouteID          int64
	UpstreamModel    string
	Status           string
	ErrorCode        string
	TerminatedReason string
	LatencyMS        int
	TTFTMS           int
	CostMicros       int64
	ChargeMicros     int64
	CreatedAt        time.Time
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

	// ProviderID narrows to the requests one upstream provider instance served; 0 means
	// "every provider".
	//
	// It is the one filter that is not an identity column of request_logs: a provider is
	// named by the metering rows, and one request may be served by several of them
	// (failover), so no column on the log row could hold the answer. It is therefore an
	// attempt-grain match — "the request has at least one metered attempt on this provider"
	// — which is what keeps this filter selecting the same rows in the list, in the totals
	// and in the breakdown (docs/design/m53-request-provider-dimension.md §4).
	ProviderID int64
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

// RequestLogDimensionPage contains buckets and their count from one database snapshot.
type RequestLogDimensionPage struct {
	Rows  []RequestLogDimensionRow
	Total int
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

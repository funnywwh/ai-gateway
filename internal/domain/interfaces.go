package domain

import (
	"context"
	"time"
)

// Store is the persistence port. The SQLite implementation lives in internal/store;
// a Postgres implementation can replace it without touching other modules (see docs/architecture).
// The method set grows milestone by milestone (M1 onward).
type Store interface {
	// Accounts
	GetAccount(ctx context.Context, id int64) (*Account, error)
	GetAccountByName(ctx context.Context, name string) (*Account, error)
	ListAccounts(ctx context.Context) ([]*Account, error)
	UpsertAccount(ctx context.Context, a *Account) (int64, error)

	// API keys
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (*APIKey, error)
	ListAPIKeys(ctx context.Context, accountID int64) ([]*APIKey, error)
	UpsertAPIKey(ctx context.Context, k *APIKey) (int64, error)

	// Providers, models, mappings, routes, tags
	ListProviders(ctx context.Context) ([]*Provider, error)
	GetProvider(ctx context.Context, id int64) (*Provider, error)
	UpsertProvider(ctx context.Context, p *Provider) (int64, error)
	ListProviderModels(ctx context.Context, providerID int64) ([]*ProviderModel, error)
	ListModels(ctx context.Context) ([]*Model, error)
	UpsertModel(ctx context.Context, m *Model) (int64, error)
	ListModelMappings(ctx context.Context) ([]*ModelMapping, error)
	UpsertModelMapping(ctx context.Context, m *ModelMapping) (int64, error)
	ListRoutes(ctx context.Context) ([]*Route, error)
	UpsertRoute(ctx context.Context, r *Route) (int64, error)
	ListTags(ctx context.Context) ([]*Tag, error)
	UpsertTag(ctx context.Context, t *Tag) (int64, error)

	// Billing
	AppendLedger(ctx context.Context, entries []*LedgerEntry) (int, error)
	ListLedger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*LedgerEntry, error)
	GetBalance(ctx context.Context, accountID int64) (int64, error)
	InsertUsage(ctx context.Context, rec *UsageRecord) (int64, error)

	Close() error
}

// UsageRecord is one metered upstream attempt (successful or not).
type UsageRecord struct {
	ID               int64
	RequestID        string
	AttemptNo        int
	AccountID        int64
	APIKeyID         int64
	Model            string
	ResolvedModel    string
	ProviderID       int64
	DimensionsJSON   string
	CostMicros       int64
	ChargeMicros     int64
	OvershootCost    int64
	PricingSnapshot  string
	LatencyMS        int
	TTFTMS           int
	Status           string
	ErrorCode        string
	DegradedFeatures string
	UsageSource      string
	TerminatedReason string
	CreatedAt        time.Time
}

// KeyVerifier validates a bearer API key (M4).
type KeyVerifier interface {
	Verify(ctx context.Context, token string) (*APIKey, *Account, error)
}

// Authorizer resolves the effective (union) grants for a key (M3).
type Authorizer interface {
	Effective(ctx context.Context, key *APIKey) (*Grant, error)
}

// Grant is the resolved permission set of an API key.
type Grant struct {
	Models    map[string]bool // "*" means all
	Providers map[string]bool // "*" means all
	Policy    Policy
}

// Policy carries merged routing/billing policy knobs.
type Policy struct {
	Strategy      string
	ProviderOrder []string
	StrategySet   bool
	// MarginBP is the customer-facing multiplier in basis points (10000 = 1.0x) when
	// the key or tag policy sets one.
	MarginBP  int
	MarginSet bool
}

// ModelResolver performs free-form model mapping (M3).
type ModelResolver interface {
	Resolve(ctx context.Context, requested string) (*ResolvedModel, error)
}

// ResolvedModel is the outcome of model resolution.
type ResolvedModel struct {
	Requested    string
	Canonical    string
	Pinned       bool
	ProviderID   int64
	ProviderName string
	Upstream     string
	MatchedRule  string
	// Groups holds capture groups from prefix/glob/regex mapping rules,
	// used to expand {1}/{name} placeholders in upstream model names.
	Groups map[string]string
}

// Router selects provider candidates and can explain the decision (M3).
type Router interface {
	Candidates(ctx context.Context, req RouteRequest) ([]Candidate, error)
	Explain(ctx context.Context, req RouteRequest) (*RouteExplanation, error)
}

// RouteRequest is the routing input.
type RouteRequest struct {
	Model    string
	KeyID    int64
	Features map[string]bool
	// ProviderPin forces a specific provider instance (from model@provider or a header).
	ProviderPin string
	// Strategy overrides the layer-internal strategy for this request.
	Strategy string
	// Grant is the pre-computed permission set (nil means "compute from Key").
	Grant *Grant
	// Key is the authenticated API key.
	Key *APIKey
	// Tags are the resolved tags of the key.
	Tags []*Tag
	// SessionID is the client's session key (the request's prompt_cache_key). It feeds
	// session stickiness only: it is never an authorization input, and an empty value
	// simply means the request does not take part.
	SessionID string
}

// Candidate is one selectable route target.
type Candidate struct {
	RouteID       int64
	ProviderID    int64
	ProviderName  string
	UpstreamModel string
	Priority      int
	Weight        int
	// Degraded lists request features stripped for this candidate (degradation=strip).
	Degraded []string
	// Strategy is the layer-internal strategy that produced ordering.
	Strategy string
}

// RouteExplanation is the diagnostic output used by the "simulate routing" UI.
type RouteExplanation struct {
	Requested string
	Canonical string
	Rule      string
	Order     []Candidate
	Excluded  []Exclusion
	// Failure carries the reason no candidate survived ("no_candidates", a missing
	// capability, and so on). Exclusions are the answer to a diagnostic question, so
	// Explain reports them with a nil error and puts the failure here instead.
	Failure string
}

// Exclusion records why a candidate was filtered out.
type Exclusion struct {
	ProviderName string
	Reason       string
}

// PriceEvaluator resolves cost/charge for a usage dimension set (M11a).
type PriceEvaluator interface {
	Evaluate(ctx context.Context, in PriceInput) (*PriceResult, error)
}

// PriceInput feeds the pricing engine.
type PriceInput struct {
	AccountID       int64
	APIKeyID        int64
	Model           string
	ProviderID      int64
	RequestedAt     time.Time
	Dimensions      map[string]int64
	InputTokensHint int64
}

// PriceResult is the computed charge plus the audit snapshot.
type PriceResult struct {
	CostMicros   int64
	ChargeMicros int64
	SnapshotJSON string
}

// Ledger is the balance/ledger port (M11b).
type Ledger interface {
	Append(ctx context.Context, entries []*LedgerEntry) error
	Balance(ctx context.Context, accountID int64) (int64, error)
}

// ReservationManager tracks in-flight reservations for admission control (M11b).
type ReservationManager interface {
	Reserve(ctx context.Context, in ReserveInput) (*Reservation, error)
	Settle(ctx context.Context, r *Reservation, usedMicros int64) error
	Release(ctx context.Context, r *Reservation)
}

// ReserveInput describes an admission request.
type ReserveInput struct {
	AccountID     int64
	APIKeyID      int64
	ReserveMicros int64
	RequestID     string
}

// Reservation is a granted in-flight reservation.
type Reservation struct {
	ID             string
	AccountID      int64
	APIKeyID       int64
	ReservedMicros int64
	ExpiresAt      time.Time
}

// Recorder persists request/response content per recording policy (M7).
type Recorder interface {
	Record(ctx context.Context, rec *ContentRecord) error
}

// ContentRecord is one recorded request/response pair.
type ContentRecord struct {
	RequestID          string
	APIKeyID           int64
	AccountID          int64
	Endpoint           string
	RequestJSON        string
	ResponseReasoning  string
	ResponseText       string
	ReasoningRecorded  bool
	OutputTextRecorded bool
	Truncated          bool
	CreatedAt          time.Time
}

// HookDispatcher delivers lifecycle events to configured hooks (M7).
type HookDispatcher interface {
	Emit(ctx context.Context, ev *Event)
}

// Event is a hook payload.
type Event struct {
	Name      string
	Timestamp time.Time
	Payload   map[string]any
}

// MCPQuerier serves the MCP tools (M6; administrative tools added in M21).
type MCPQuerier interface {
	Query(ctx context.Context, accountID int64, tool string, args map[string]any) (any, error)
}

// BackupManager performs consistent-point database backups (M16).
type BackupManager interface {
	Run(ctx context.Context, trigger string) (*BackupJob, error)
	List(ctx context.Context) ([]*BackupJob, error)
}

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

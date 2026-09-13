// Package config loads and validates the gateway configuration.
//
// Precedence: built-in defaults < YAML file < GW_* environment variables.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/winger/ai-gateway/internal/logx"
)

// currencyRE mirrors pricing.CurrencyRE. The configuration checks the shape of
// every currency code itself so a typo fails at start-up; the pricing package
// re-checks and normalizes when a rule set is parsed.
var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// Config is the complete gateway configuration.
type Config struct {
	Server         Server      `yaml:"server"`
	Database       Database    `yaml:"database"`
	Auth           Auth        `yaml:"auth"`
	Plugins        Plugins     `yaml:"plugins"`
	Routing        Routing     `yaml:"routing"`
	Billing        Billing     `yaml:"billing"`
	Recording      Recording   `yaml:"recording"`
	MCP            MCP         `yaml:"mcp"`
	Chat           Chat        `yaml:"chat"`
	Hooks          Hooks       `yaml:"hooks"`
	Portal         Portal      `yaml:"portal"`
	RateLimit      RateLimit   `yaml:"ratelimit"`
	Backup         Backup      `yaml:"backup"`
	Log            logx.Config `yaml:"log"`
	CredentialsKey string      `yaml:"credentials_key"`
	Bootstrap      Bootstrap   `yaml:"bootstrap"`
}

// Server holds HTTP listener settings.
type Server struct {
	Listen       string `yaml:"listen"`
	SecretKey    string `yaml:"secret_key"`
	ReadTimeoutS int    `yaml:"read_timeout_s"`
	MaxBodyBytes int64  `yaml:"max_body_bytes"`
	// Pprof exposes /debug/pprof. Off by default: a profiling endpoint should not be
	// open in production.
	Pprof bool `yaml:"pprof"`
	// BasePath mounts the whole surface under one URL prefix, for a reverse proxy that
	// forwards a prefix without stripping it (nginx `location /aigw/ { proxy_pass
	// http://127.0.0.1:8088; }`). Every route — data plane, console and management API —
	// is then served under it, and paths this server generates itself (the session
	// cookie's Path, the console's redirects, preview URLs) carry the prefix too, so the
	// prefix is a deployment choice rather than a hole in the surface.
	BasePath string `yaml:"base_path"`
}

// ReadTimeout returns the HTTP read timeout.
func (s Server) ReadTimeout() time.Duration { return time.Duration(s.ReadTimeoutS) * time.Second }

// NormalizedBasePath returns the mount prefix without a trailing slash: "" for the
// root mount, "/aigw" for `base_path: /aigw/`.
func (s Server) NormalizedBasePath() string {
	path := strings.TrimSpace(s.BasePath)
	for strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" || path == "/" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

// Database holds SQLite settings.
type Database struct {
	Path          string `yaml:"path"`
	BusyTimeoutMS int    `yaml:"busy_timeout_ms"`
	WAL           bool   `yaml:"wal"`
	MaxOpenConns  int    `yaml:"max_open_conns"`
}

// Auth holds API-key authentication settings.
type Auth struct {
	DefaultGrant string `yaml:"default_grant"` // all|none
	KeyCacheTTLS int    `yaml:"key_cache_ttl_s"`
}

// KeyCacheTTL returns the API key cache TTL.
func (a Auth) KeyCacheTTL() time.Duration { return time.Duration(a.KeyCacheTTLS) * time.Second }

// Plugins holds provider-plugin process settings.
type Plugins struct {
	Dir                string   `yaml:"dir"`
	Extra              []string `yaml:"extra"`
	StateDir           string   `yaml:"state_dir"`
	StartTimeoutS      int      `yaml:"start_timeout_s"`
	PingIntervalS      int      `yaml:"ping_interval_s"`
	MaxRestartsPerMin  int      `yaml:"max_restarts_per_min"`
	LogTailLines       int      `yaml:"log_tail_lines"`
	RestartDrainGraceS int      `yaml:"restart_drain_grace_s"`
	RestartForceS      int      `yaml:"restart_force_s"`
}

// Breaker configures the per-route circuit breaker.
type Breaker struct {
	Failures  int `yaml:"failures"`
	WindowS   int `yaml:"window_s"`
	CooldownS int `yaml:"cooldown_s"`
}

// Routing holds routing/retry/timeout policy defaults.
type Routing struct {
	DefaultStrategy       string  `yaml:"default_strategy"`
	MaxAttempts           int     `yaml:"max_attempts"`
	PerAttemptTimeoutS    int     `yaml:"per_attempt_timeout_s"`
	TTFTTimeoutS          int     `yaml:"ttft_timeout_s"`
	TotalBudgetS          int     `yaml:"total_budget_s"`
	Degradation           string  `yaml:"degradation"` // strip|reject
	QuotaCooldownDefaultS int     `yaml:"quota_cooldown_default_s"`
	Breaker               Breaker `yaml:"breaker"`
	ModelFallback         string  `yaml:"model_fallback"`

	// SessionAffinity makes consecutive requests of one client session reuse the route
	// that last served them successfully. It only reorders candidates inside a
	// priority tier (and never for strict_order), so it cannot widen what a key is
	// authorised for; a request without a session key is not affected at all.
	SessionAffinity bool `yaml:"session_affinity"`
	// SessionAffinityTTLS is how long a binding lives without being refreshed by
	// another successful request of the same session.
	SessionAffinityTTLS int `yaml:"session_affinity_ttl_s"`
	// SessionAffinityMaxEntries bounds the in-process table: session keys come from
	// the client, so the table needs a ceiling.
	SessionAffinityMaxEntries int `yaml:"session_affinity_max_entries"`
}

// Billing holds pricing, reservation, in-flight and invoicing policy.
type Billing struct {
	// Currency is the ledger currency: balances, credit limits, reservations,
	// ledger entries, usage amounts and invoices are all kept in it. A model may
	// be priced in another currency (docs/pricing.md §9); those amounts are
	// converted into this one before they are written down.
	Currency string `yaml:"currency"`
	// DisplayCurrency is the currency the console shows amounts in by default. It
	// must be the ledger currency or have an entry in FXRates.
	DisplayCurrency string `yaml:"display_currency"`
	// FXRates maps a currency to how many micros of the ledger currency one whole
	// unit of it is worth: {CNY: 141000} means 1 CNY = 0.141000 USD. Integer micros
	// only; the ledger currency itself must not appear here (it is always 1:1).
	FXRates               map[string]int64 `yaml:"fx_rates"`
	DefaultMarkupBP       int              `yaml:"default_markup_bp"`
	BasisDefault          string           `yaml:"basis_default"` // cost_follow|absolute
	PeakBoundary          string           `yaml:"peak_boundary"` // request_start|completion
	ReasoningCountsAsOut  bool             `yaml:"reasoning_counts_as_output"`
	PerRequestFeeScope    string           `yaml:"per_request_fee_scope"` // attempt|request
	MinChargeMicros       int64            `yaml:"min_charge_micros"`
	ChargeOnError         bool             `yaml:"charge_on_error"`
	ChargeEstimated       bool             `yaml:"charge_estimated"`
	ChargePartial         bool             `yaml:"charge_partial"`
	RecordPartialCost     bool             `yaml:"record_partial_cost"`
	PrepaidEnforce        bool             `yaml:"prepaid_enforce"`
	RejectAs429           bool             `yaml:"reject_as_429"`
	MonthlyUsageConds     bool             `yaml:"monthly_usage_conditions"`
	ReservationMode       string           `yaml:"reservation_mode"` // max_tokens|fixed|hybrid
	ReserveMicrosDefault  int64            `yaml:"reserve_micros_default"`
	DefaultMaxOutputToken int              `yaml:"default_max_output_tokens"`
	ReservationTTLS       int              `yaml:"reservation_ttl_s"`
	ReservationHeartbeatS int              `yaml:"reservation_heartbeat_s"`
	InflightCheckMS       int              `yaml:"inflight_check_interval_ms"`
	InflightPolicy        string           `yaml:"inflight_policy"` // warn|throttle|abort|allow_overdraft
	InflightSoftRatio     float64          `yaml:"inflight_soft_ratio"`
	InflightHardRatio     float64          `yaml:"inflight_hard_ratio"`
	OverdraftLimitMicros  int64            `yaml:"overdraft_limit_micros"`
	OvershootPolicy       string           `yaml:"overshoot_policy"`  // absorb|overdraft
	InflightEstimate      string           `yaml:"inflight_estimate"` // chars4|off
	CancelGraceMS         int              `yaml:"cancel_grace_ms"`
	UnavailableChargePol  string           `yaml:"unavailable_charge_policy"`
	LowBalanceRatio       float64          `yaml:"low_balance_ratio"`
	AutoSuspendDefault    bool             `yaml:"auto_suspend_default"`
	AutoResumeDefault     bool             `yaml:"auto_resume_default"`
	InvoicePeriod         string           `yaml:"invoice_period"`
	PeriodStartDay        int              `yaml:"period_start_day"`
	Timezone              string           `yaml:"timezone"`
	FallbackFile          string           `yaml:"fallback_file"`
	ReconcileCron         string           `yaml:"reconcile_cron"`
	WriterBatchSize       int              `yaml:"writer_batch_size"`
	WriterFlushMS         int              `yaml:"writer_flush_interval_ms"`
	WriterQueueSize       int              `yaml:"writer_queue_size"`
}

// RecordingInputModes are the accepted recording.record_input values, from the most
// detailed capture to none. The console's per-key select offers the same set plus
// "inherit", and internal/config's tests assert the two lists never drift apart.
var RecordingInputModes = []string{"full", "user", "metadata", "off"}

// Recording controls content capture (input text vs thinking/final output text) plus the
// identity dimensions every row carries.
type Recording struct {
	RecordInput      string `yaml:"record_input"` // full|user|metadata|off
	RecordReasoning  bool   `yaml:"record_reasoning"`
	RecordOutputText bool   `yaml:"record_output_text"`
	// RecordTitle keeps the session title produced by a client's title call. It is
	// metadata about the session rather than an answer to the user, so it has its own
	// switch and is on by default; it is only ever written on the title call's own row.
	RecordTitle bool `yaml:"record_title"`
	MaxBytes    int  `yaml:"max_bytes"`
	// RetentionDays is how long recorded content lives: request logs older than the
	// window are pruned daily, and a stored response expires with it. 0 disables cleanup.
	RetentionDays int      `yaml:"retention_days"`
	RedactPaths   []string `yaml:"redact_paths"`
	QueueSize     int      `yaml:"queue_size"`
	// BatchWrites groups the per-request audit rows (stored response + request log) into
	// one background transaction instead of two synchronous ones. On the local deployment
	// the synchronous path took the single writer connection twice per request, and a
	// goroutine dump under load showed request goroutines queued on it.
	// Set false to keep the strictly-synchronous behaviour (the row is written before the
	// handler returns).
	BatchWrites *bool `yaml:"batch_writes"`
	// BatchFlushMS is how long a queued audit row may wait for company. Smaller means
	// fresher rows and more transactions.
	BatchFlushMS int `yaml:"batch_flush_ms"`
	// BatchMaxRows caps how many requests share one transaction.
	BatchMaxRows int `yaml:"batch_max_rows"`
	// BatchMaxBytes caps the payload one transaction carries, which matters because a
	// recorded body can be hundreds of kilobytes.
	BatchMaxBytes int `yaml:"batch_max_bytes"`
	// BatchQueueRows and BatchQueueBytes bound what may wait in memory before the request
	// path applies backpressure instead of queueing more.
	BatchQueueRows  int `yaml:"batch_queue_rows"`
	BatchQueueBytes int `yaml:"batch_queue_bytes"`
}

// BatchingEnabled reports whether audit writes are batched. Absent means the default (on).
func (r Recording) BatchingEnabled() bool {
	return r.BatchWrites == nil || *r.BatchWrites
}

// InputModeFor resolves one API key's record_input_mode against the deployment default.
// "" and "inherit" mean "use the deployment default"; "meta" is the value an older
// console sent for "metadata" and is normalized; anything unrecognised falls back to the
// deployment default rather than silently widening what gets stored. The result is
// always one of RecordingInputModes.
func (r Recording) InputModeFor(keyMode string) string {
	switch mode := strings.ToLower(strings.TrimSpace(keyMode)); mode {
	case "meta":
		return "metadata"
	case "full", "user", "metadata", "off":
		return mode
	}
	// "" and "inherit" mean the deployment default; so does anything unrecognised, which
	// can only come from a hand-edited row. Falling back to the default never widens
	// what gets stored beyond what the operator asked for globally.
	fallback := strings.ToLower(strings.TrimSpace(r.RecordInput))
	for _, known := range RecordingInputModes {
		if fallback == known {
			return known
		}
	}
	return "user"
}

// MCP configures the MCP server (account query tools plus, for tokens whose scope
// allows it, the administrative tool surface).
type MCP struct {
	Enabled           bool `yaml:"enabled"`
	MaxQueryRows      int  `yaml:"max_query_rows"`
	RequestWindowDays int  `yaml:"request_window_days"`
	// AdminTools enables the administrative tool surface (admin_endpoints /
	// admin_describe / admin_request). The real gate is the token scope: only a
	// token issued with scope=admin_read or admin ever sees these tools, so this is
	// the deployment-wide kill switch.
	AdminTools bool `yaml:"admin_tools"`
	// AdminMaxResponseBytes caps one administrative call's response body; a larger
	// answer is truncated and flagged instead of being pasted into the transcript.
	AdminMaxResponseBytes int `yaml:"admin_max_response_bytes"`
}

// Hooks configures async hook delivery.
type Hooks struct {
	QueueSize  int    `yaml:"queue_size"`
	Workers    int    `yaml:"workers"`
	TimeoutS   int    `yaml:"timeout_s"`
	Retries    int    `yaml:"retries"`
	DeadLetter string `yaml:"dead_letter"`
	// AllowInsecure permits http:// webhook targets. Off by default so credentials
	// and payloads cannot be pushed over plaintext by accident.
	AllowInsecure bool `yaml:"allow_insecure"`
}

// Chat configures the console's smart-chat page, its private skill library and the
// isolated HTML/SVG previews. Content from this page is recorded as metadata only (never
// as request/response bodies), because a skill is private to the administrator who wrote
// it and the global request log is readable by every administrator.
type Chat struct {
	Enabled bool `yaml:"enabled"`
	// MaxSteps bounds how many model calls one question may cost; 0 means no limit and is
	// the default. Every step is a separately billed request, so a finite value here is the
	// ceiling on what one click can spend — set it when the account's balance matters more
	// than letting a long investigation finish in one go.
	MaxSteps int `yaml:"max_steps"`
	// MaxToolCalls bounds one question's tool calls; 0 means no limit and is the default.
	MaxToolCalls int `yaml:"max_tool_calls"`
	// MaxToolResultBytes truncates one management tool result before it is fed back to the
	// model; a truncated result says so instead of silently losing the tail.
	MaxToolResultBytes int `yaml:"max_tool_result_bytes"`
	// MaxHistoryMessages / MaxHistoryBytes bound the replayed conversation. History is cut
	// at whole turns so a tool call never loses its output.
	MaxHistoryMessages int `yaml:"max_history_messages"`
	MaxHistoryBytes    int `yaml:"max_history_bytes"`
	// MaxLoadedSkills is a per-conversation limit, not a library limit: the library itself
	// is paged and unlimited.
	MaxLoadedSkills int `yaml:"max_loaded_skills"`
	MaxSkillBytes   int `yaml:"max_skill_bytes"`
	// RecordReasoning keeps the model's thinking with the message so the console can show
	// it collapsed. It never reaches the global request log either way.
	RecordReasoning bool `yaml:"record_reasoning"`
	// ArtifactMaxBytes / ArtifactMaxPerSession bound preview payloads and how many one
	// conversation may keep.
	ArtifactMaxBytes      int `yaml:"artifact_max_bytes"`
	ArtifactMaxPerSession int `yaml:"artifact_max_per_session"`
	// ArtifactTicketTTL is how long a preview URL stays usable. A preview is a short-lived
	// bearer credential for exactly one payload, so the window is deliberately short.
	ArtifactTicketTTL time.Duration `yaml:"artifact_ticket_ttl"`
	// ArtifactAllowNetwork lets a previewed page load external resources (for example a
	// CDN chart library). Off by default: it turns a model-authored page into outbound
	// traffic from the operator's browser.
	ArtifactAllowNetwork bool `yaml:"artifact_allow_network"`
	// UIBridgeEnabled allows an interactive preview: the model-authored page may send a form
	// submission back into the conversation, where it becomes a normally billed question and
	// the answer is patched back into the page. Off means previews are read-only, and a page's
	// buttons produce no model request at all.
	UIBridgeEnabled bool `yaml:"ui_bridge_enabled"`
	// MaxOutputTokens caps one step's answer (0 keeps the provider default).
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// SystemPrompt replaces the built-in instructions when set.
	SystemPrompt string `yaml:"system_prompt"`
}

// Portal configures the customer self-service portal (M14). It is off by default: an
// operator opts in after creating portal users.
type Portal struct {
	Enabled       bool `yaml:"enabled"`
	SessionTTLH   int  `yaml:"session_ttl_h"`
	LoginAttempts int  `yaml:"login_attempts"`
	// AllowedTags limits which tags a customer may attach to their own API keys. Empty
	// means "only the account default tags".
	AllowedTags []string `yaml:"allowed_tags"`
}

// RateLimit configures the sharded rate limiter.
type RateLimit struct {
	Shards int `yaml:"shards"`
}

// Backup configures periodic automatic database backups.
type Backup struct {
	Enabled          bool   `yaml:"enabled"`
	Dir              string `yaml:"dir"`
	Cron             string `yaml:"cron"`
	RetentionDaily   int    `yaml:"retention_daily"`
	RetentionWeekly  int    `yaml:"retention_weekly"`
	RetentionMonthly int    `yaml:"retention_monthly"`
	Verify           bool   `yaml:"verify"`
}

// BootstrapAdmin is the initial admin account seeded on first start.
type BootstrapAdmin struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// BootstrapAccount seeds a billing account.
type BootstrapAccount struct {
	Name           string  `yaml:"name"`
	BillingMode    string  `yaml:"billing_mode"`
	CreditLimitUSD float64 `yaml:"credit_limit_usd"`
}

// BootstrapAPIKey seeds an API key (stored hashed on first start).
type BootstrapAPIKey struct {
	Name    string   `yaml:"name"`
	Key     string   `yaml:"key"`
	Account string   `yaml:"account"`
	Tags    []string `yaml:"tags"`
}

// BootstrapProviderModel maps one public model to an upstream model for a plugin/builtin provider.
type BootstrapProviderModel struct {
	Public          string          `yaml:"public"`
	Upstream        string          `yaml:"upstream"`
	Capabilities    map[string]bool `yaml:"capabilities"`
	PricingRules    string          `yaml:"pricing_rules"` // raw JSON rule set (cost side)
	MaxOutputTokens int             `yaml:"max_output_tokens"`
	ContextWindow   int             `yaml:"context_window"`
	Enabled         *bool           `yaml:"enabled"`
}

// BootstrapProvider declares one provider instance.
type BootstrapProvider struct {
	Name        string                   `yaml:"name"`
	Kind        string                   `yaml:"kind"`
	DisplayName string                   `yaml:"display_name"`
	Enabled     *bool                    `yaml:"enabled"`
	Priority    int                      `yaml:"priority"`
	Weight      int                      `yaml:"weight"`
	Config      map[string]any           `yaml:"config"`
	Models      []BootstrapProviderModel `yaml:"models"`
}

// BootstrapModel declares one canonical (client-facing) model.
type BootstrapModel struct {
	PublicName  string   `yaml:"public_name"`
	Aliases     []string `yaml:"aliases"`
	Enabled     *bool    `yaml:"enabled"`
	SalePricing string   `yaml:"sale_pricing"` // raw JSON rule set (sale side)
}

// BootstrapRoute binds a canonical model to a provider.
type BootstrapRoute struct {
	Model         string `yaml:"model"`
	Provider      string `yaml:"provider"`
	UpstreamModel string `yaml:"upstream_model"`
	Priority      int    `yaml:"priority"`
	Weight        int    `yaml:"weight"`
	Enabled       *bool  `yaml:"enabled"`
}

// BootstrapTag declares a tag with union grants.
type BootstrapTag struct {
	Name      string   `yaml:"name"`
	Models    []string `yaml:"models"`
	Providers []string `yaml:"providers"`
	Priority  int      `yaml:"priority"`
}

// Bootstrap seeds the database when it is empty (or merges, per Mode).
type Bootstrap struct {
	Mode      string              `yaml:"mode"` // upsert|merge|off
	Admin     BootstrapAdmin      `yaml:"admin"`
	Accounts  []BootstrapAccount  `yaml:"accounts"`
	APIKeys   []BootstrapAPIKey   `yaml:"api_keys"`
	Providers []BootstrapProvider `yaml:"providers"`
	Models    []BootstrapModel    `yaml:"models"`
	Routes    []BootstrapRoute    `yaml:"routes"`
	Tags      []BootstrapTag      `yaml:"tags"`
}

// Default returns the built-in configuration (matches config.example.yaml).
func Default() Config {
	return Config{
		Server: Server{
			Listen:       ":8080",
			ReadTimeoutS: 30,
			MaxBodyBytes: 10 * 1024 * 1024,
		},
		Database: Database{
			Path:          "./data/aigw.db",
			BusyTimeoutMS: 5000,
			WAL:           true,
			MaxOpenConns:  16,
		},
		Auth: Auth{DefaultGrant: "all", KeyCacheTTLS: 30},
		Plugins: Plugins{
			Dir:                "./plugins",
			StateDir:           "./data/plugin-state",
			StartTimeoutS:      3,
			PingIntervalS:      15,
			MaxRestartsPerMin:  5,
			LogTailLines:       200,
			RestartDrainGraceS: 30,
			RestartForceS:      600,
		},
		Routing: Routing{
			DefaultStrategy:       "weighted_random",
			MaxAttempts:           3,
			PerAttemptTimeoutS:    120,
			TTFTTimeoutS:          60,
			TotalBudgetS:          600,
			Degradation:           "strip",
			QuotaCooldownDefaultS: 1800,
			Breaker:               Breaker{Failures: 5, WindowS: 60, CooldownS: 30},
			// Sticky sessions are on by default: a session that carries a
			// prompt_cache_key is exactly the traffic that a weighted draw would
			// otherwise scatter across upstreams, and a client that sends no key
			// is not affected at all.
			SessionAffinity:           true,
			SessionAffinityTTLS:       1800,
			SessionAffinityMaxEntries: 10000,
		},
		Billing: Billing{
			Currency:              "USD",
			DisplayCurrency:       "USD",
			FXRates:               map[string]int64{},
			DefaultMarkupBP:       10000,
			BasisDefault:          "cost_follow",
			PeakBoundary:          "request_start",
			ReasoningCountsAsOut:  true,
			PerRequestFeeScope:    "attempt",
			MinChargeMicros:       0,
			ChargeOnError:         false,
			ChargeEstimated:       true,
			ChargePartial:         true,
			RecordPartialCost:     true,
			PrepaidEnforce:        true,
			RejectAs429:           false,
			MonthlyUsageConds:     false,
			ReservationMode:       "max_tokens",
			ReserveMicrosDefault:  20000,
			DefaultMaxOutputToken: 4096,
			ReservationTTLS:       660,
			ReservationHeartbeatS: 15,
			InflightCheckMS:       1000,
			InflightPolicy:        "abort",
			InflightSoftRatio:     0.8,
			InflightHardRatio:     1.0,
			OverdraftLimitMicros:  0,
			OvershootPolicy:       "absorb",
			InflightEstimate:      "chars4",
			CancelGraceMS:         1500,
			UnavailableChargePol:  "charge_estimated",
			LowBalanceRatio:       0.1,
			AutoSuspendDefault:    false,
			AutoResumeDefault:     false,
			InvoicePeriod:         "monthly",
			PeriodStartDay:        1,
			Timezone:              "UTC",
			FallbackFile:          "./data/billing-fallback.jsonl",
			ReconcileCron:         "0 3 * * *",
			WriterBatchSize:       256,
			WriterFlushMS:         20,
			WriterQueueSize:       65536,
		},
		Recording: Recording{
			RecordInput:      "user",
			RecordReasoning:  false,
			RecordOutputText: false,
			// Session titles are on by default: they are the one piece of a client's
			// traffic that makes a request log readable at a glance, and they are
			// metadata about the session rather than the user's own content.
			RecordTitle:     true,
			MaxBytes:        1048576,
			RetentionDays:   30,
			QueueSize:       16384,
			BatchFlushMS:    250,
			BatchMaxRows:    256,
			BatchMaxBytes:   16 << 20,
			BatchQueueRows:  4096,
			BatchQueueBytes: 32 << 20,
		},
		MCP: MCP{
			Enabled: true, MaxQueryRows: 1000, RequestWindowDays: 30,
			AdminTools: true, AdminMaxResponseBytes: 256 * 1024,
		},
		Chat: Chat{
			Enabled: true,
			// 0 = no limit: a question keeps going until the model stops calling tools.
			MaxSteps:              0,
			MaxToolCalls:          0,
			MaxToolResultBytes:    64 * 1024,
			MaxHistoryMessages:    40,
			MaxHistoryBytes:       256 * 1024,
			MaxLoadedSkills:       12,
			MaxSkillBytes:         16 * 1024,
			RecordReasoning:       true,
			ArtifactMaxBytes:      256 * 1024,
			ArtifactMaxPerSession: 50,
			ArtifactTicketTTL:     5 * time.Minute,
			ArtifactAllowNetwork:  false,
			UIBridgeEnabled:       true,
		},
		Hooks:     Hooks{QueueSize: 1024, Workers: 8, TimeoutS: 5, Retries: 5, DeadLetter: "./data/hooks-dead.jsonl"},
		Portal:    Portal{SessionTTLH: 12, LoginAttempts: 10},
		RateLimit: RateLimit{Shards: 64},
		Backup: Backup{
			Enabled:          true,
			Dir:              "./data/backups",
			Cron:             "30 3 * * *",
			RetentionDaily:   7,
			RetentionWeekly:  4,
			RetentionMonthly: 3,
			Verify:           true,
		},
		Log:       logx.Default(),
		Bootstrap: Bootstrap{Mode: "upsert"},
	}
}

// Load reads the configuration from path (a missing file is not an error: defaults are used),
// applies GW_* environment overrides and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// keep defaults
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func envStr(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func envBool(dst *bool, key string) error {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("env %s: %w", key, err)
	}
	*dst = b
	return nil
}

func envInt(dst *int, key string) error {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("env %s: %w", key, err)
	}
	*dst = n
	return nil
}

func applyEnv(cfg *Config) error {
	envStr(&cfg.Server.Listen, "GW_SERVER_LISTEN")
	envStr(&cfg.Server.SecretKey, "GW_SERVER_SECRET_KEY")
	envStr(&cfg.Server.BasePath, "GW_SERVER_BASE_PATH")
	envStr(&cfg.Database.Path, "GW_DATABASE_PATH")
	envStr(&cfg.CredentialsKey, "GW_CREDENTIALS_KEY")
	envStr(&cfg.Log.Level, "GW_LOG_LEVEL")
	envStr(&cfg.Log.Format, "GW_LOG_FORMAT")
	envStr(&cfg.Plugins.Dir, "GW_PLUGINS_DIR")
	envStr(&cfg.Plugins.StateDir, "GW_PLUGINS_STATE_DIR")
	envStr(&cfg.Auth.DefaultGrant, "GW_AUTH_DEFAULT_GRANT")
	envStr(&cfg.Bootstrap.Admin.Username, "GW_ADMIN_USERNAME")
	envStr(&cfg.Bootstrap.Admin.Password, "GW_ADMIN_PASSWORD")
	envStr(&cfg.Backup.Dir, "GW_BACKUP_DIR")
	envStr(&cfg.Backup.Cron, "GW_BACKUP_CRON")

	for _, step := range []error{
		envBool(&cfg.Backup.Enabled, "GW_BACKUP_ENABLED"),
		envBool(&cfg.MCP.Enabled, "GW_MCP_ENABLED"),
		envBool(&cfg.MCP.AdminTools, "GW_MCP_ADMIN_TOOLS"),
		envInt(&cfg.MCP.AdminMaxResponseBytes, "GW_MCP_ADMIN_MAX_RESPONSE_BYTES"),
		envBool(&cfg.Chat.Enabled, "GW_CHAT_ENABLED"),
		envBool(&cfg.Routing.SessionAffinity, "GW_ROUTING_SESSION_AFFINITY"),
		envInt(&cfg.Routing.SessionAffinityTTLS, "GW_ROUTING_SESSION_AFFINITY_TTL_S"),
		envInt(&cfg.Routing.SessionAffinityMaxEntries, "GW_ROUTING_SESSION_AFFINITY_MAX_ENTRIES"),
		envInt(&cfg.Server.ReadTimeoutS, "GW_SERVER_READ_TIMEOUT_S"),
		envInt(&cfg.RateLimit.Shards, "GW_RATELIMIT_SHARDS"),
	} {
		if step != nil {
			return step
		}
	}
	return nil
}

// validateBasePath accepts one or more plain path segments. A prefix that contains a
// space, a query or a wildcard cannot be matched against a request path, so it is
// rejected at start-up rather than turning into a mysterious 404 on every request.
func validateBasePath(path string) error {
	if path == "" {
		return nil
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '-', r == '_', r == '.', r == '~':
		default:
			return fmt.Errorf("server.base_path must be a plain URL path (got %q)", path)
		}
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("server.base_path must not contain an empty segment (got %q)", path)
	}
	if strings.Contains(path, "/.") {
		return fmt.Errorf("server.base_path must not contain a dot segment (got %q)", path)
	}
	return nil
}

func oneOf(field, value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %s (got %q)", field, strings.Join(allowed, "|"), value)
}

// Validate checks the configuration for obvious misconfiguration.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return fmt.Errorf("server.listen must not be empty")
	}
	if err := validateBasePath(c.Server.NormalizedBasePath()); err != nil {
		return err
	}
	if strings.TrimSpace(c.Database.Path) == "" {
		return fmt.Errorf("database.path must not be empty")
	}
	if c.Database.MaxOpenConns < 2 {
		return fmt.Errorf("database.max_open_conns must be >= 2 (one writer + readers)")
	}
	if err := oneOf("auth.default_grant", c.Auth.DefaultGrant, "all", "none"); err != nil {
		return err
	}
	if err := oneOf("routing.default_strategy", c.Routing.DefaultStrategy,
		"weighted_random", "round_robin", "least_inflight", "least_latency", "strict_order"); err != nil {
		return err
	}
	if err := oneOf("routing.degradation", c.Routing.Degradation, "strip", "reject"); err != nil {
		return err
	}
	// Only checked while the feature is on: a deployment that switched sticky sessions
	// off should not be stopped from starting by a stale value it no longer reads.
	if c.Routing.SessionAffinity {
		if c.Routing.SessionAffinityTTLS < 1 {
			return fmt.Errorf("routing.session_affinity_ttl_s must be >= 1 when session_affinity is on")
		}
		if c.Routing.SessionAffinityMaxEntries < 1 {
			return fmt.Errorf("routing.session_affinity_max_entries must be >= 1 when session_affinity is on")
		}
	}
	b := c.Billing
	checks := []error{
		oneOf("billing.basis_default", b.BasisDefault, "cost_follow", "absolute"),
		oneOf("billing.peak_boundary", b.PeakBoundary, "request_start", "completion"),
		oneOf("billing.per_request_fee_scope", b.PerRequestFeeScope, "attempt", "request"),
		oneOf("billing.reservation_mode", b.ReservationMode, "max_tokens", "fixed", "hybrid"),
		oneOf("billing.inflight_policy", b.InflightPolicy, "warn", "throttle", "abort", "allow_overdraft"),
		oneOf("billing.overshoot_policy", b.OvershootPolicy, "absorb", "overdraft"),
		oneOf("billing.inflight_estimate", b.InflightEstimate, "chars4", "off"),
		oneOf("billing.unavailable_charge_policy", b.UnavailableChargePol,
			"charge_estimated", "charge_reserved", "charge_zero"),
		oneOf("billing.invoice_period", b.InvoicePeriod, "monthly", "custom"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	if b.InflightHardRatio < b.InflightSoftRatio {
		return fmt.Errorf("billing.inflight_hard_ratio (%v) must be >= inflight_soft_ratio (%v)",
			b.InflightHardRatio, b.InflightSoftRatio)
	}
	if err := validateCurrencies(c); err != nil {
		return err
	}
	if b.WriterBatchSize <= 0 || b.WriterQueueSize <= 0 {
		return fmt.Errorf("billing.writer_batch_size and writer_queue_size must be positive")
	}
	if err := oneOf("recording.record_input", c.Recording.RecordInput, RecordingInputModes...); err != nil {
		return err
	}
	// 0 switches retention off (nothing is pruned, stored responses never expire);
	// a negative window is a typo, not a policy.
	if c.Recording.RetentionDays < 0 {
		return fmt.Errorf("recording.retention_days must be >= 0 (0 disables cleanup)")
	}
	if c.Recording.BatchingEnabled() {
		// A zero here would silently fall back to the built-in default, which makes the
		// config file lie about what the process does; say so instead.
		if c.Recording.BatchFlushMS <= 0 {
			return fmt.Errorf("recording.batch_flush_ms must be positive when batch_writes is on")
		}
		if c.Recording.BatchMaxRows <= 0 || c.Recording.BatchMaxBytes <= 0 {
			return fmt.Errorf("recording.batch_max_rows and batch_max_bytes must be positive when batch_writes is on")
		}
		if c.Recording.BatchQueueRows <= 0 || c.Recording.BatchQueueBytes <= 0 {
			return fmt.Errorf("recording.batch_queue_rows and batch_queue_bytes must be positive when batch_writes is on")
		}
		if c.Recording.BatchQueueRows < c.Recording.BatchMaxRows {
			// A queue smaller than one batch means every batch starts with producers
			// blocked on a queue that cannot hold what the writer is about to take.
			return fmt.Errorf("recording.batch_queue_rows (%d) must be >= batch_max_rows (%d)",
				c.Recording.BatchQueueRows, c.Recording.BatchMaxRows)
		}
	}
	if c.Backup.Enabled {
		if strings.TrimSpace(c.Backup.Dir) == "" {
			return fmt.Errorf("backup.dir must not be empty when backup is enabled")
		}
		if strings.TrimSpace(c.Backup.Cron) == "" {
			return fmt.Errorf("backup.cron must not be empty when backup is enabled")
		}
	}
	if c.RateLimit.Shards < 1 {
		return fmt.Errorf("ratelimit.shards must be >= 1")
	}
	if err := validateChat(c); err != nil {
		return err
	}
	return nil
}

// validateChat rejects a chat configuration that would make the console lie about what it
// does: a step budget of zero would silently fall back to a default, a non-positive ticket
// TTL would make every preview unusable, and a history bound smaller than one turn would
// cut conversations mid-tool-call.
func validateChat(c *Config) error {
	chat := c.Chat
	if !chat.Enabled {
		return nil
	}
	// 0 disables the ceiling (see the field comments); a negative value is a typo.
	if chat.MaxSteps < 0 {
		return fmt.Errorf("chat.max_steps must be >= 0 (0 disables the step limit)")
	}
	if chat.MaxToolCalls < 0 {
		return fmt.Errorf("chat.max_tool_calls must be >= 0 (0 disables the tool-call limit)")
	}
	if chat.MaxToolResultBytes <= 0 {
		return fmt.Errorf("chat.max_tool_result_bytes must be positive")
	}
	if chat.MaxHistoryMessages < 2 {
		return fmt.Errorf("chat.max_history_messages must be >= 2 (one user message and its answer)")
	}
	if chat.MaxHistoryBytes <= 0 {
		return fmt.Errorf("chat.max_history_bytes must be positive")
	}
	if chat.MaxLoadedSkills < 0 {
		return fmt.Errorf("chat.max_loaded_skills must be >= 0")
	}
	if chat.MaxSkillBytes <= 0 {
		return fmt.Errorf("chat.max_skill_bytes must be positive")
	}
	if chat.ArtifactMaxBytes <= 0 {
		return fmt.Errorf("chat.artifact_max_bytes must be positive")
	}
	if chat.ArtifactMaxPerSession <= 0 {
		return fmt.Errorf("chat.artifact_max_per_session must be positive")
	}
	if chat.ArtifactTicketTTL <= 0 {
		return fmt.Errorf("chat.artifact_ticket_ttl must be positive (previews are short-lived by design)")
	}
	if chat.MaxOutputTokens < 0 {
		return fmt.Errorf("chat.max_output_tokens must be >= 0 (0 keeps the provider default)")
	}
	return nil
}

// validateCurrencies normalizes and checks the ledger currency, the console's
// default display currency and the FX table. It fails at start-up rather than at
// the first request, because a currency typo would otherwise surface as a wrong
// charge (or as a request the gateway claims it cannot price).
func validateCurrencies(c *Config) error {
	ledger := strings.ToUpper(strings.TrimSpace(c.Billing.Currency))
	if !currencyRE.MatchString(ledger) {
		return fmt.Errorf("billing.currency %q must be three uppercase letters, such as USD", c.Billing.Currency)
	}
	c.Billing.Currency = ledger

	rates := make(map[string]int64, len(c.Billing.FXRates))
	for code, rate := range c.Billing.FXRates {
		key := strings.ToUpper(strings.TrimSpace(code))
		if !currencyRE.MatchString(key) {
			return fmt.Errorf("billing.fx_rates key %q must be three uppercase letters, such as CNY", code)
		}
		if key == ledger {
			return fmt.Errorf("billing.fx_rates must not contain the ledger currency %s (it is always 1:1)", ledger)
		}
		if rate <= 0 {
			return fmt.Errorf("billing.fx_rates[%s] must be a positive number of micros of %s", key, ledger)
		}
		rates[key] = rate
	}
	c.Billing.FXRates = rates

	display := strings.ToUpper(strings.TrimSpace(c.Billing.DisplayCurrency))
	if display == "" {
		display = ledger
	}
	if !currencyRE.MatchString(display) {
		return fmt.Errorf("billing.display_currency %q must be three uppercase letters, such as CNY", c.Billing.DisplayCurrency)
	}
	if display != ledger {
		if _, ok := rates[display]; !ok {
			return fmt.Errorf("billing.display_currency %s has no billing.fx_rates entry", display)
		}
	}
	c.Billing.DisplayCurrency = display
	return nil
}

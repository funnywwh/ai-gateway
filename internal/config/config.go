// Package config loads and validates the gateway configuration.
//
// Precedence: built-in defaults < YAML file < GW_* environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/winger/ai-gateway/internal/logx"
)

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
}

// ReadTimeout returns the HTTP read timeout.
func (s Server) ReadTimeout() time.Duration { return time.Duration(s.ReadTimeoutS) * time.Second }

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
}

// Billing holds pricing, reservation, in-flight and invoicing policy.
type Billing struct {
	Currency              string  `yaml:"currency"`
	DefaultMarkupBP       int     `yaml:"default_markup_bp"`
	BasisDefault          string  `yaml:"basis_default"` // cost_follow|absolute
	PeakBoundary          string  `yaml:"peak_boundary"` // request_start|completion
	ReasoningCountsAsOut  bool    `yaml:"reasoning_counts_as_output"`
	PerRequestFeeScope    string  `yaml:"per_request_fee_scope"` // attempt|request
	MinChargeMicros       int64   `yaml:"min_charge_micros"`
	ChargeOnError         bool    `yaml:"charge_on_error"`
	ChargeEstimated       bool    `yaml:"charge_estimated"`
	ChargePartial         bool    `yaml:"charge_partial"`
	RecordPartialCost     bool    `yaml:"record_partial_cost"`
	PrepaidEnforce        bool    `yaml:"prepaid_enforce"`
	RejectAs429           bool    `yaml:"reject_as_429"`
	MonthlyUsageConds     bool    `yaml:"monthly_usage_conditions"`
	ReservationMode       string  `yaml:"reservation_mode"` // max_tokens|fixed|hybrid
	ReserveMicrosDefault  int64   `yaml:"reserve_micros_default"`
	DefaultMaxOutputToken int     `yaml:"default_max_output_tokens"`
	ReservationTTLS       int     `yaml:"reservation_ttl_s"`
	ReservationHeartbeatS int     `yaml:"reservation_heartbeat_s"`
	InflightCheckMS       int     `yaml:"inflight_check_interval_ms"`
	InflightPolicy        string  `yaml:"inflight_policy"` // warn|throttle|abort|allow_overdraft
	InflightSoftRatio     float64 `yaml:"inflight_soft_ratio"`
	InflightHardRatio     float64 `yaml:"inflight_hard_ratio"`
	OverdraftLimitMicros  int64   `yaml:"overdraft_limit_micros"`
	OvershootPolicy       string  `yaml:"overshoot_policy"`  // absorb|overdraft
	InflightEstimate      string  `yaml:"inflight_estimate"` // chars4|off
	CancelGraceMS         int     `yaml:"cancel_grace_ms"`
	UnavailableChargePol  string  `yaml:"unavailable_charge_policy"`
	LowBalanceRatio       float64 `yaml:"low_balance_ratio"`
	AutoSuspendDefault    bool    `yaml:"auto_suspend_default"`
	AutoResumeDefault     bool    `yaml:"auto_resume_default"`
	InvoicePeriod         string  `yaml:"invoice_period"`
	PeriodStartDay        int     `yaml:"period_start_day"`
	Timezone              string  `yaml:"timezone"`
	FallbackFile          string  `yaml:"fallback_file"`
	ReconcileCron         string  `yaml:"reconcile_cron"`
	DisplayCurrencyRate   float64 `yaml:"display_currency_rate"`
	WriterBatchSize       int     `yaml:"writer_batch_size"`
	WriterFlushMS         int     `yaml:"writer_flush_interval_ms"`
	WriterQueueSize       int     `yaml:"writer_queue_size"`
}

// Recording controls content capture (input text vs thinking/final output text).
type Recording struct {
	RecordInput      string   `yaml:"record_input"` // full|metadata|off
	RecordReasoning  bool     `yaml:"record_reasoning"`
	RecordOutputText bool     `yaml:"record_output_text"`
	MaxBytes         int      `yaml:"max_bytes"`
	RetentionDays    int      `yaml:"retention_days"`
	RedactPaths      []string `yaml:"redact_paths"`
	QueueSize        int      `yaml:"queue_size"`
}

// MCP configures the read-only MCP query server.
type MCP struct {
	Enabled           bool `yaml:"enabled"`
	MaxQueryRows      int  `yaml:"max_query_rows"`
	RequestWindowDays int  `yaml:"request_window_days"`
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
		},
		Billing: Billing{
			Currency:              "USD",
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
			DisplayCurrencyRate:   1.0,
			WriterBatchSize:       256,
			WriterFlushMS:         20,
			WriterQueueSize:       65536,
		},
		Recording: Recording{
			RecordInput:      "full",
			RecordReasoning:  false,
			RecordOutputText: false,
			MaxBytes:         1048576,
			RetentionDays:    30,
			QueueSize:        16384,
		},
		MCP:       MCP{Enabled: true, MaxQueryRows: 1000, RequestWindowDays: 30},
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
		envInt(&cfg.Server.ReadTimeoutS, "GW_SERVER_READ_TIMEOUT_S"),
		envInt(&cfg.RateLimit.Shards, "GW_RATELIMIT_SHARDS"),
	} {
		if step != nil {
			return step
		}
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
	if b.WriterBatchSize <= 0 || b.WriterQueueSize <= 0 {
		return fmt.Errorf("billing.writer_batch_size and writer_queue_size must be positive")
	}
	if err := oneOf("recording.record_input", c.Recording.RecordInput, "full", "metadata", "off"); err != nil {
		return err
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
	return nil
}

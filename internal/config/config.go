// Package config loads and validates the gateway configuration.
//
// Precedence: built-in defaults < YAML file < GW_* environment variables.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	Dshgw          Dshgw       `yaml:"dshgw"`
	Feishu         Feishu      `yaml:"feishu"`
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

	// ProviderQueueWaitS is how long one attempt may wait for a provider capacity
	// slot (providers.max_inflight) before it fails retryably and the request moves
	// on to the next candidate. 0 disables queueing: an attempt that finds the
	// provider full fails immediately, which is what a deployment that prefers fast
	// failover wants.
	ProviderQueueWaitS int `yaml:"provider_queue_wait_s"`
	// ProviderQueueMaxWaiters bounds how many attempts may queue for one provider.
	// 0 means the queue depth is bounded only by ProviderQueueWaitS.
	ProviderQueueMaxWaiters int `yaml:"provider_queue_max_waiters"`
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
	RetentionDays int `yaml:"retention_days"`
	// DimensionRollupEnabled controls derived hourly statistics; false uses raw queries.
	DimensionRollupEnabled bool     `yaml:"dimension_rollup_enabled"`
	RedactPaths            []string `yaml:"redact_paths"`
	QueueSize              int      `yaml:"queue_size"`
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

// Dshgw configures the DSH multi-tenant gateway that aigw owns (M58).
//
// Shape: aigw is the long-lived program; dshgw is a sibling executable that aigw
// starts as its own child with the same UID — no root, no systemd unit, no
// separate service account — and stops when aigw exits. Every tenant worker is a
// bubblewrap child of that process.
//
// Enabled defaults to false so an existing deployment is unaffected until an
// operator asks for the child to be supervised.
type Dshgw struct {
	// AdminSocket is the local provisioning socket the console's "启用/停用 DSH"
	// buttons use. In this shape it is owned by aigw's own account (same-UID
	// only); an empty path disables those buttons (they answer 501).
	AdminSocket string `yaml:"admin_socket"`
	// Enabled makes aigw start and supervise the child.
	Enabled bool `yaml:"enabled"`
	// Binary is the dshgw executable. Empty means "the sibling of the running
	// aigw", which is what keeps a moved installation from using a stale copy.
	Binary string `yaml:"binary"`
	// StateDir holds the child's generated config and its registry/sessions.
	// Empty means "<directory of database.path>/dshgw".
	StateDir string `yaml:"state_dir"`
	// TenantRoot holds one directory per tenant (with its .dsh state). Empty means
	// "<state_dir>/tenants". Setting it explicitly is how a migration reuses the
	// data directory an earlier deployment already populated.
	TenantRoot string `yaml:"tenant_root"`
	// WorkspaceRoot holds the tenant workspaces. Empty means
	// "<state_dir>/workspaces"; operators put it on the volume with the space.
	WorkspaceRoot string `yaml:"workspace_root"`
	// AigwBaseURL is the address the child validates tenant keys against. Empty
	// means aigw's own loopback listener, which is the normal case; it exists as
	// an override for tests and for deployments where the child reaches aigw
	// through something other than the plain listener address.
	AigwBaseURL string `yaml:"aigw_base_url"`
	// PublicHost and the port bands describe the tenant-facing surface the child
	// serves directly (there is no edge proxy in this shape).
	PublicHost   string `yaml:"public_host"`
	Listen       string `yaml:"listen"`
	PortalPort   int    `yaml:"portal_port"`
	TenantPortLo int    `yaml:"tenant_port_lo"`
	TenantPortHi int    `yaml:"tenant_port_hi"`
	WorkerPortLo int    `yaml:"worker_port_lo"`
	WorkerPortHi int    `yaml:"worker_port_hi"`
	// DSH runtime the child runs tenant workers from. Empty values fall back to
	// the DSHGW_NODE and DSHGW_DSH_ROOT environment variables, which is how the
	// repository's own scripts already describe a locally staged dsh.
	NodeBin      string `yaml:"node_bin"`
	BinJS        string `yaml:"bin_js"`
	ReleasesRoot string `yaml:"releases_root"`
	CurrentLink  string `yaml:"current_link"`
	// TemplateHome is the prepared dsh web profile copied into each new tenant.
	TemplateHome string `yaml:"template_home"`
	// PluginPath is the directory-picker plugin served to tenant sessions. Empty
	// keeps the child's own default for the installation it ships with.
	PluginPath string `yaml:"plugin_path"`
	// NoStoreAPIs controls whether the child marks its API responses uncacheable.
	// Unset keeps the child's default (never store), which is what a multi-tenant
	// deployment wants; set it false only to let a cache reuse API answers.
	NoStoreAPIs *bool `yaml:"no_store_apis"`
	// PublicBaseURL puts the child in single-domain path mode: public URLs become
	// "https://host/t/<tenant>/" and "https://host/dshgw/" instead of host:port
	// origins. Use it when subdomains are not available.
	PublicBaseURL    string `yaml:"public_base_url"`
	TenantPathPrefix string `yaml:"tenant_path_prefix"`
	PortalPathPrefix string `yaml:"portal_path_prefix"`
	// PublicListen is where the child binds the portal and tenant public ports.
	// Empty means loopback: exposing tenants to a network is an explicit choice.
	PublicListen string `yaml:"public_listen"`
	// PublicScheme is how browsers actually reach that surface: "auto" (the child's
	// own default) presumes https in port mode. It is passed through because the
	// child decides the session cookie's Secure attribute from it, and a browser
	// silently drops a Secure cookie on a plain-HTTP origin — which presents as
	// "login succeeded, then back at the portal".
	PublicScheme string `yaml:"public_scheme"`
	// TLSCertificate/TLSCertificateKey make the child serve HTTPS on those ports.
	// Empty means plain HTTP, which is only appropriate on a trusted network.
	TLSCertificate    string `yaml:"tls_certificate"`
	TLSCertificateKey string `yaml:"tls_certificate_key"`
	// Worker memory/task/CPU ceilings, applied per tenant worker through cgroup v2.
	// Zero means unlimited. They replace the systemd unit's MemoryMax/CPUQuota/
	// TasksMax, which the rootless shape no longer has.
	WorkerMemoryHighBytes int64 `yaml:"worker_memory_high_bytes"`
	WorkerMemoryMaxBytes  int64 `yaml:"worker_memory_max_bytes"`
	WorkerTasksMax        int   `yaml:"worker_tasks_max"`
	WorkerCPUQuotaPercent int   `yaml:"worker_cpu_quota_percent"`
	// PluginBrowserFS is the child's default for the browser filesystem plugin:
	// "on" requires a template prepared with dsh-browser-fs, "off" does not.
	PluginBrowserFS string `yaml:"plugin_browser_fs"`
	// SSHWorkspaces enables ssh workspaces for the accounts this gateway provisions (M64):
	// an account browses and creates directories on a remote host with its own key, and the
	// child mounts the chosen directory inside that account's workspace. Off by default,
	// because enabling it means this deployment will ssh to remote hosts with a key an
	// operator placed here.
	SSHWorkspaces     DshgwSSHWorkspaces     `yaml:"ssh_workspaces"`
	BrowserWorkspaces DshgwBrowserWorkspaces `yaml:"browser_workspaces"`
	// AccountCard is the tenant sidebar's identity row (M67): it shows the signed-in person's
	// Feishu (or account) name and a 退出 button. Off by default, because it adds two routes
	// under every tenant's origin. aigw is the side that knows those names, which is why the
	// switch is configured here and reaches the child as a generated one.
	AccountCard DshgwAccountCard `yaml:"account_card"`
}

// DshgwBrowserWorkspaces is disabled by default.
type DshgwBrowserWorkspaces struct {
	Enabled bool `yaml:"enabled"`
}

// DshgwAccountCard is disabled by default.
type DshgwAccountCard struct {
	Enabled bool `yaml:"enabled"`
}

// DshgwSSHWorkspaces configures the child's ssh-workspace feature (M64) from the
// supervised shape's own configuration file.
//
// Values are passed through as written and validated by the child, which is the side that
// runs ssh: a duration is a Go duration string ("10s"), and paths are resolved against the
// deployment root here so the child never has to guess which directory a relative value
// meant.
type DshgwSSHWorkspaces struct {
	Enabled            bool     `yaml:"enabled"`
	MountSubdir        string   `yaml:"mount_subdir"`
	SSHBin             string   `yaml:"ssh_bin"`
	SSHFSBin           string   `yaml:"sshfs_bin"`
	IdentitySource     string   `yaml:"identity_source"`
	IdentityDir        string   `yaml:"identity_dir"`
	SSHConfigSource    string   `yaml:"ssh_config_source"`
	Hosts              []string `yaml:"hosts"`
	ConnectTimeout     string   `yaml:"connect_timeout"`
	PollInterval       string   `yaml:"poll_interval"`
	MaxEntries         int      `yaml:"max_entries"`
	SSHFSOptions       []string `yaml:"sshfs_options"`
	DisableAutoRemount bool     `yaml:"disable_auto_remount"`
}

// Feishu is the self-built Feishu (Lark) application aigw uses for identity: one app,
// one registered redirect URL, and two flows that share it — an administrator binding
// an API key to a Feishu account from the console, and a person signing in to the DSH
// portal with Feishu instead of pasting a key.
//
// The whole feature is off by default, and while it is off nothing here is read: a
// deployment that never touches Feishu keeps behaving exactly as before.
type Feishu struct {
	Enabled bool `yaml:"enabled"`
	// AppID and AppSecret come from the Feishu developer console (凭证与基础信息).
	// The secret must not be logged, echoed in an error page, or stored anywhere else.
	AppID     string `yaml:"app_id"`
	AppSecret string `yaml:"app_secret"`
	// CallbackURL is the absolute URL registered under 安全设置 → 重定向 URL, as the
	// browser sees it. It is configured rather than derived because aigw cannot know
	// the origin in front of it (a port, a front proxy, or a path prefix), and a wrong
	// redirect_uri is a Feishu error page instead of ours. Its path must be
	// "<base_path>/feishu/callback", which startup also checks.
	CallbackURL string `yaml:"callback_url"`
	// The three endpoints are configurable so tests can point them at a local stub; the
	// defaults are Feishu's documented ones.
	AuthorizeURL string `yaml:"authorize_url"`
	TokenURL     string `yaml:"token_url"`
	UserInfoURL  string `yaml:"userinfo_url"`
	// TenantTokenURL and ContactURL serve the contact-directory read of the org sync
	// (M70) — the tenant access token endpoint and the base of /contact/v3. They follow
	// the same configurability rule as the three above and are validated to be https
	// (or http only for an explicit loopback, like the rest).
	TenantTokenURL string `yaml:"tenant_token_url"`
	ContactURL     string `yaml:"contact_url"`
	// Scopes is a space-separated extra scope list. Empty is the right default: the
	// open id and the display name this feature needs require no permission at all, and
	// asking for more would show the user a consent screen for data we do not read.
	Scopes string `yaml:"scopes"`
	// TimeoutS bounds one call to Feishu.
	TimeoutS int `yaml:"timeout_s"`
	// StateTTLS is how long an authorization attempt stays valid between leaving for
	// Feishu and coming back.
	StateTTLS int `yaml:"state_ttl_s"`
	// StateSecret signs that state; empty derives one from credentials_key.
	StateSecret string `yaml:"state_secret"`
	// DSHLogin opens the DSH portal login flow (M61). It needs PortalURL to be known.
	DSHLogin bool `yaml:"dsh_login"`
	// AdminLogin opens the console login flow (M66): a person whose Feishu identity is bound
	// to an administrator account can sign in by scanning the code on Feishu's consent page.
	// It is harmless when nobody is bound yet — an unbound identity is simply refused — so it
	// defaults to on, exactly like the portal login.
	AdminLogin bool `yaml:"admin_login"`
	// InviteTTLS is how long an administrator invitation link stays usable. It is much
	// longer than StateTTLS because the link travels through a person (chat, mail) rather
	// than through one redirect, and it is bounded because the link is an enrollment
	// credential: whoever opens it first binds their identity to that account.
	InviteTTLS int `yaml:"invite_ttl_s"`
	// InviteSecret signs those invitation links; empty derives one from credentials_key
	// under its own purpose, so the invitation key and the state key never coincide.
	InviteSecret string `yaml:"invite_secret"`
	// ConsoleURL is the console as the operator's browser reaches it, e.g.
	// "http://192.168.190.86:8088/admin/ui/". It is the console's answer to PortalURL, and it
	// exists for one reason: a session cookie belongs to a host name, while the Feishu
	// callback always runs on the origin registered with Feishu. When the two share a host
	// name the callback hands the browser its cookie directly; when they do not (a LAN
	// console and a public callback, say) the callback hands over a one-time ticket and the
	// console redeems it on its own origin. Empty keeps the relative console path, which is
	// right whenever the console is served from the callback's host.
	ConsoleURL string `yaml:"console_url"`
	// AutoEnableDSH makes a successful binding also opt the key's account in to DSH, so the
	// person can log in right away instead of waiting for a second, easily forgotten step in
	// the console. It only ever enables an account that has never been enabled: an account
	// an administrator explicitly disabled stays disabled, and the console is told so.
	AutoEnableDSH bool `yaml:"auto_enable_dsh"`
	// PortalURL is the browser-visible URL of the dshgw portal. Empty derives it from
	// the dshgw block, which is correct whenever aigw owns that child.
	PortalURL string `yaml:"portal_url"`
	// TicketSecret signs the short-lived ticket aigw hands to dshgw; empty derives one
	// from credentials_key, and the supervised child receives the derived value in its
	// generated configuration so the two sides always agree.
	TicketSecret string `yaml:"ticket_secret"`
	// TicketTTLS bounds how long that ticket can be redeemed. It is meant to cover one
	// browser redirect, not to be a session.
	TicketTTLS int `yaml:"ticket_ttl_s"`
}

// FeishuLoginURL is the browser-visible entry point of the authorization flow. It is
// derived from CallbackURL so that a deployment has exactly one public origin to state.
func (c *Config) FeishuLoginURL() string {
	base := strings.TrimSuffix(strings.TrimSpace(c.Feishu.CallbackURL), "/feishu/callback")
	if base == "" {
		return ""
	}
	return base + "/feishu/login"
}

// DSHGWPortalURL is the browser-visible URL of the dshgw portal login page: the page the
// DSH login flow returns the browser to. Explicit configuration wins; otherwise it is
// derived from the dshgw block, mirroring the child's own origin rules (port mode uses
// public_host:portal_port, path mode uses public_base_url plus the portal prefix).
func (c *Config) DSHGWPortalURL() string {
	if explicit := strings.TrimRight(strings.TrimSpace(c.Feishu.PortalURL), "/"); explicit != "" {
		return explicit
	}
	if base := strings.TrimRight(strings.TrimSpace(c.Dshgw.PublicBaseURL), "/"); base != "" {
		prefix := strings.Trim(c.Dshgw.PortalPathPrefix, "/")
		if prefix == "" {
			prefix = "dshgw"
		}
		return base + "/" + prefix
	}
	host := strings.TrimSpace(c.Dshgw.PublicHost)
	if host == "" || c.Dshgw.PortalPort == 0 {
		return ""
	}
	return fmt.Sprintf("%s://%s:%d", c.DshgwPublicScheme(), host, c.Dshgw.PortalPort)
}

// DshgwPublicScheme is how browsers reach the child's public surface: "auto" keeps the
// child's own default (https in port mode), which is what a TLS deployment wants.
func (c *Config) DshgwPublicScheme() string {
	switch scheme := strings.TrimSpace(c.Dshgw.PublicScheme); scheme {
	case "http", "https":
		return scheme
	}
	if base := strings.TrimSpace(c.Dshgw.PublicBaseURL); strings.HasPrefix(base, "http://") {
		return "http"
	}
	return "https"
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
	Name           string   `yaml:"name"`
	BillingMode    string   `yaml:"billing_mode"`
	CreditLimitUSD float64  `yaml:"credit_limit_usd"`
	Tags           []string `yaml:"tags"`
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

// BootstrapModelReasoning is the configuration-file representation of a model's
// reasoning override. The store maps and validates it at the domain boundary.
type BootstrapModelReasoning struct {
	Mode   string `yaml:"mode"`
	Effort string `yaml:"effort"`
}

// UnmarshalYAML accepts only the two reasoning keys so a misspelled bootstrap
// setting cannot be silently ignored by YAML's permissive default decoder.
func (r *BootstrapModelReasoning) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("bootstrap model reasoning must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key != "mode" && key != "effort" {
			return fmt.Errorf("bootstrap model reasoning has unknown field %q", key)
		}
	}
	type plain BootstrapModelReasoning
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*r = BootstrapModelReasoning(decoded)
	return nil
}

// Validate checks the bootstrap reasoning values before the store maps them to the domain type.
func (r BootstrapModelReasoning) Validate() error {
	if r.Mode != "default" && r.Mode != "force" {
		return fmt.Errorf("bootstrap model reasoning mode must be default|force (got %q)", r.Mode)
	}
	switch r.Effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	default:
		return fmt.Errorf("bootstrap model reasoning effort must be none|minimal|low|medium|high|xhigh|max (got %q)", r.Effort)
	}
}

// BootstrapModel declares one canonical (client-facing) model.
type BootstrapModel struct {
	PublicName  string                   `yaml:"public_name"`
	Aliases     []string                 `yaml:"aliases"`
	Enabled     *bool                    `yaml:"enabled"`
	SalePricing string                   `yaml:"sale_pricing"` // raw JSON rule set (sale side)
	Reasoning   *BootstrapModelReasoning `yaml:"reasoning"`
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

// DefaultDataDir is the single runtime data root (M63): every stateful default below
// is derived from it through dataPath, so a deployment has exactly one writable data
// directory and no default names a machine-specific path (/var/lib, /opt, /etc or a
// home directory). It is relative on purpose: paths are resolved against the process
// working directory, which both deployment entry points pin to the deployment root
// (the user unit's WorkingDirectory, and scripts/local-run.sh's cd "$ROOT").
//
// Artifacts that are not runtime state — built binaries (bin/) and provider plugins
// (plugins/) — deliberately stay outside it, and runtime installations (Node, dsh,
// bwrap, TLS certificates) stay absolute: they describe where something is installed
// on this machine, not where data lives.
const DefaultDataDir = "./data"

// dataPath joins one entry of the default data root. filepath.Join is not used
// because it would clean "./data" into "data", and the leading "./" is part of what
// the documentation, the log line and the tests read.
func dataPath(elem string) string { return DefaultDataDir + "/" + elem }

// Default returns the built-in configuration (matches config.example.yaml).
func Default() Config {
	return Config{
		Feishu: Feishu{
			// Default endpoints are the documented ones: the authorization page, the
			// OAuth v3 token endpoint (v2 is historical) and the user-info API. The
			// tenant token + contact base serve the directory read of the org sync.
			AuthorizeURL:   "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
			TokenURL:       "https://accounts.feishu.cn/oauth/v3/token",
			UserInfoURL:    "https://open.feishu.cn/open-apis/authen/v1/user_info",
			TenantTokenURL: "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
			ContactURL:     "https://open.feishu.cn/open-apis/contact/v3",
			TimeoutS:       5,
			StateTTLS:      600,
			TicketTTLS:     120,
			DSHLogin:       true,
			AdminLogin:     true,
			InviteTTLS:     3600,
			AutoEnableDSH:  true,
		},
		Dshgw: Dshgw{
			PublicHost:   "localhost",
			Listen:       "127.0.0.1:31099",
			PortalPort:   31000,
			TenantPortLo: 31001,
			TenantPortHi: 31299,
			WorkerPortLo: 31300,
			WorkerPortHi: 31599,
		},
		Server: Server{
			Listen:       ":8080",
			ReadTimeoutS: 30,
			MaxBodyBytes: 10 * 1024 * 1024,
		},
		Database: Database{
			Path:          dataPath("aigw.db"),
			BusyTimeoutMS: 5000,
			WAL:           true,
			MaxOpenConns:  16,
		},
		Auth: Auth{DefaultGrant: "all", KeyCacheTTLS: 30},
		Plugins: Plugins{
			Dir:                "./plugins",
			StateDir:           dataPath("plugin-state"),
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
			// Queueing is on by default, but only matters for providers whose
			// max_inflight is set: 0 (the default) means unlimited, and no gate is
			// created at all.
			ProviderQueueWaitS:      30,
			ProviderQueueMaxWaiters: 100,
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
			FallbackFile:          dataPath("billing-fallback.jsonl"),
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
			RecordTitle:            true,
			MaxBytes:               1048576,
			RetentionDays:          30,
			DimensionRollupEnabled: true,
			QueueSize:              16384,
			BatchFlushMS:           250,
			BatchMaxRows:           256,
			BatchMaxBytes:          16 << 20,
			BatchQueueRows:         4096,
			BatchQueueBytes:        32 << 20,
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
		Hooks:     Hooks{QueueSize: 1024, Workers: 8, TimeoutS: 5, Retries: 5, DeadLetter: dataPath("hooks-dead.jsonl")},
		Portal:    Portal{SessionTTLH: 12, LoginAttempts: 10},
		RateLimit: RateLimit{Shards: 64},
		Backup: Backup{
			Enabled:          true,
			Dir:              dataPath("backups"),
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
	envStr(&cfg.Feishu.AppID, "GW_FEISHU_APP_ID")
	envStr(&cfg.Feishu.AppSecret, "GW_FEISHU_APP_SECRET")
	envStr(&cfg.Feishu.CallbackURL, "GW_FEISHU_CALLBACK_URL")
	envStr(&cfg.Feishu.StateSecret, "GW_FEISHU_STATE_SECRET")
	envStr(&cfg.Feishu.TicketSecret, "GW_FEISHU_TICKET_SECRET")
	envStr(&cfg.Feishu.PortalURL, "GW_FEISHU_PORTAL_URL")
	envStr(&cfg.Feishu.TenantTokenURL, "GW_FEISHU_TENANT_TOKEN_URL")
	envStr(&cfg.Feishu.ContactURL, "GW_FEISHU_CONTACT_URL")
	envBool(&cfg.Feishu.AdminLogin, "GW_FEISHU_ADMIN_LOGIN")
	envInt(&cfg.Feishu.InviteTTLS, "GW_FEISHU_INVITE_TTL_S")
	envStr(&cfg.Feishu.InviteSecret, "GW_FEISHU_INVITE_SECRET")
	envStr(&cfg.Feishu.ConsoleURL, "GW_FEISHU_CONSOLE_URL")
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
		envBool(&cfg.Recording.DimensionRollupEnabled, "GW_RECORDING_DIMENSION_ROLLUP_ENABLED"),
		envBool(&cfg.MCP.Enabled, "GW_MCP_ENABLED"),
		envBool(&cfg.MCP.AdminTools, "GW_MCP_ADMIN_TOOLS"),
		envInt(&cfg.MCP.AdminMaxResponseBytes, "GW_MCP_ADMIN_MAX_RESPONSE_BYTES"),
		envBool(&cfg.Chat.Enabled, "GW_CHAT_ENABLED"),
		envBool(&cfg.Routing.SessionAffinity, "GW_ROUTING_SESSION_AFFINITY"),
		envInt(&cfg.Routing.SessionAffinityTTLS, "GW_ROUTING_SESSION_AFFINITY_TTL_S"),
		envInt(&cfg.Routing.SessionAffinityMaxEntries, "GW_ROUTING_SESSION_AFFINITY_MAX_ENTRIES"),
		envInt(&cfg.Routing.ProviderQueueWaitS, "GW_ROUTING_PROVIDER_QUEUE_WAIT_S"),
		envInt(&cfg.Routing.ProviderQueueMaxWaiters, "GW_ROUTING_PROVIDER_QUEUE_MAX_WAITERS"),
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

// feishuAppIDRE matches the App ID shape Feishu hands out (cli_ followed by the app's
// identifier). Checking it here turns a copy/paste mistake into a startup error instead
// of a 20027/20002 error page during a user's first login.
var feishuAppIDRE = regexp.MustCompile(`^cli_[A-Za-z0-9]+$`)

// validateFeishu checks the identity block. Everything is checked only while the feature
// is on: a deployment that does not use Feishu must not be stopped from starting by
// stale or half-filled values it never reads.
func (c *Config) validateFeishu() error {
	if !c.Feishu.Enabled {
		return nil
	}
	if !feishuAppIDRE.MatchString(strings.TrimSpace(c.Feishu.AppID)) {
		return fmt.Errorf("feishu.app_id must be the application's App ID (cli_…)")
	}
	if strings.TrimSpace(c.Feishu.AppSecret) == "" {
		return fmt.Errorf("feishu.app_secret must be set (it is read from the developer console's 凭证与基础信息)")
	}
	for label, raw := range map[string]string{
		"feishu.callback_url": c.Feishu.CallbackURL,
	} {
		// Plain http is legitimate for the callback: it is only the address the browser
		// is sent back to, and a LAN deployment served over http has no other option
		// (Feishu accepts http redirect URLs).
		if err := validateFeishuURL(label, raw, true, false); err != nil {
			return err
		}
	}
	for label, raw := range map[string]string{
		"feishu.authorize_url":    c.Feishu.AuthorizeURL,
		"feishu.token_url":        c.Feishu.TokenURL,
		"feishu.userinfo_url":     c.Feishu.UserInfoURL,
		"feishu.tenant_token_url": c.Feishu.TenantTokenURL,
		"feishu.contact_url":      c.Feishu.ContactURL,
	} {
		// These carry the app secret and the user's access token, so https is required
		// outside a loopback stub.
		if err := validateFeishuURL(label, raw, true, true); err != nil {
			return err
		}
	}
	// The callback must be the path aigw actually serves, modulo the mount prefix, which
	// is checked when the server is built (it knows its own base path there).
	if !strings.HasSuffix(strings.TrimRight(c.Feishu.CallbackURL, "/"), "/feishu/callback") {
		return fmt.Errorf("feishu.callback_url must end with /feishu/callback (got %q); register exactly this URL in the Feishu console's 重定向 URL", c.Feishu.CallbackURL)
	}
	if c.Feishu.TimeoutS <= 0 || c.Feishu.TimeoutS > 60 {
		return fmt.Errorf("feishu.timeout_s must be between 1 and 60 (got %d)", c.Feishu.TimeoutS)
	}
	if c.Feishu.StateTTLS < 60 || c.Feishu.StateTTLS > 3600 {
		return fmt.Errorf("feishu.state_ttl_s must be between 60 and 3600 (got %d)", c.Feishu.StateTTLS)
	}
	if strings.TrimSpace(c.Feishu.StateSecret) == "" && strings.TrimSpace(c.CredentialsKey) == "" {
		return fmt.Errorf("feishu.state_secret is empty and credentials_key cannot derive one")
	}
	if c.Feishu.AdminLogin {
		// The invitation window is the part of the console feature that a deployment can get
		// wrong in a way nobody notices: too short and links expire before the person opens
		// them, too long and an unredeemed enrollment link stays a live credential.
		if c.Feishu.InviteTTLS < 300 || c.Feishu.InviteTTLS > 604800 {
			return fmt.Errorf("feishu.invite_ttl_s must be between 300 and 604800 (got %d)", c.Feishu.InviteTTLS)
		}
		if strings.TrimSpace(c.Feishu.InviteSecret) == "" && strings.TrimSpace(c.CredentialsKey) == "" {
			return fmt.Errorf("feishu.invite_secret is empty and credentials_key cannot derive one")
		}
		// The console URL is where a login or an invitation returns the browser. A relative
		// value is what an empty setting means; a stated one must be absolute, because it is
		// also what tells the callback whether the two hosts differ.
		if err := validateFeishuURL("feishu.console_url", c.Feishu.ConsoleURL, false, false); err != nil {
			return err
		}
	}
	if c.Feishu.DSHLogin {
		if c.Feishu.TicketTTLS < 30 || c.Feishu.TicketTTLS > 600 {
			return fmt.Errorf("feishu.ticket_ttl_s must be between 30 and 600 (got %d)", c.Feishu.TicketTTLS)
		}
		if strings.TrimSpace(c.Feishu.TicketSecret) == "" && strings.TrimSpace(c.CredentialsKey) == "" {
			return fmt.Errorf("feishu.ticket_secret is empty and credentials_key cannot derive one")
		}
		if c.DSHGWPortalURL() == "" {
			return fmt.Errorf("feishu.dsh_login is on but the DSH portal URL is unknown: set feishu.portal_url or the dshgw public_host/portal_port block")
		}
		if !c.Dshgw.Enabled && strings.TrimSpace(c.Feishu.PortalURL) == "" {
			// Deriving the portal URL from the dshgw block only makes sense when aigw owns
			// that child: otherwise the defaults for that block describe no running portal,
			// and the login would redirect somewhere that does not answer.
			return fmt.Errorf("feishu.dsh_login is on but dshgw is not supervised by this aigw: set feishu.portal_url explicitly")
		}
		if err := validateFeishuURL("feishu.portal_url", c.DSHGWPortalURL(), false, false); err != nil {
			return err
		}
	}
	return nil
}

// validateFeishuURL rejects anything that could not be used as given. requireHTTPS is set
// for the endpoints that carry the app secret and the user's access token; the browser
// redirect targets are allowed to be plain http, which is what a LAN deployment uses.
func validateFeishuURL(label, raw string, required, requireHTTPS bool) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if required {
			return fmt.Errorf("%s must not be empty", label)
		}
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an absolute URL without credentials, query or fragment (got %q)", label, raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s scheme %q must be http or https", label, parsed.Scheme)
	}
	if requireHTTPS && parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("%s must use https for %q (plain http is only accepted for a loopback stub)", label, parsed.Hostname())
	}
	return nil
}

// isLoopbackHost reports whether a URL host is a loopback address or localhost.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Validate checks the configuration for obvious misconfiguration.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return fmt.Errorf("server.listen must not be empty")
	}
	if err := validateBasePath(c.Server.NormalizedBasePath()); err != nil {
		return err
	}
	if err := c.validateFeishu(); err != nil {
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
	// Provider capacity queueing. The cross-check with the reservation TTL is the one
	// that matters: a request waits for a slot *while* holding its balance reservation,
	// and holds are reaped on a TTL, so queueing longer than the hold would let a
	// prepaid account be oversold by waits that outlive their own reservation.
	if c.Routing.ProviderQueueWaitS < 0 {
		return fmt.Errorf("routing.provider_queue_wait_s must not be negative (0 disables queueing)")
	}
	if c.Routing.ProviderQueueMaxWaiters < 0 {
		return fmt.Errorf("routing.provider_queue_max_waiters must not be negative (0 means no depth limit)")
	}
	if wait := c.Routing.ProviderQueueWaitS; wait > 0 {
		attempts := c.Routing.MaxAttempts
		if attempts < 1 {
			attempts = 1
		}
		worst := wait * attempts
		if ttl := c.Billing.ReservationTTLS; ttl > 0 && worst >= ttl {
			return fmt.Errorf("routing.provider_queue_wait_s (%d) x max_attempts (%d) must stay below "+
				"billing.reservation_ttl_s (%d): a queued request would outlive its balance reservation",
				wait, attempts, ttl)
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
	for _, model := range c.Bootstrap.Models {
		if model.Reasoning != nil {
			if err := model.Reasoning.Validate(); err != nil {
				return fmt.Errorf("bootstrap model %q reasoning: %w", model.PublicName, err)
			}
		}
	}
	// A bootstrap account name is validated rather than silently dropped: a name that the
	// management API would refuse must fail start-up, because the alternative is a gateway
	// that starts "successfully" while the account the operator configured never exists.
	for _, account := range c.Bootstrap.Accounts {
		if err := validateBootstrapAccountName(account.Name); err != nil {
			return err
		}
	}
	for _, key := range c.Bootstrap.APIKeys {
		// Entries the seeder skips outright (no key material or no name) are not validated:
		// they describe no account reference, so holding start-up hostage over one would be
		// a new failure mode rather than a caught typo.
		if key.Key == "" || key.Name == "" {
			continue
		}
		if err := validateBootstrapAccountName(key.Account); err != nil {
			return fmt.Errorf("bootstrap api key %q account: %w", key.Name, err)
		}
	}
	return nil
}

// MaxBootstrapAccountNameRunes mirrors domain.MaxAccountNameRunes. This package stays a
// leaf — it has no module-internal dependency but logx — so the rule is restated here
// instead of imported; TestBootstrapAccountNameValidationMatchesDomain keeps the two
// copies from drifting apart.
const MaxBootstrapAccountNameRunes = 64

// validateBootstrapAccountName applies the rule the domain and the management API enforce,
// so a name that would be rejected on POST /admin/api/v1/accounts cannot slip into the
// database through the configuration file: an account name is a non-empty label of at most
// 64 Unicode characters after trimming (email, Chinese and any other Unicode are all fine).
// A name written in YAML is therefore accepted or refused by the same rule as a name typed
// into the console, and the domain still trims again when the row is written.
func validateBootstrapAccountName(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return fmt.Errorf("bootstrap account name %q: account name is required", name)
	case !utf8.ValidString(trimmed):
		return fmt.Errorf("bootstrap account name %q: account name must be valid UTF-8", name)
	case utf8.RuneCountInString(trimmed) > MaxBootstrapAccountNameRunes:
		return fmt.Errorf("bootstrap account name %q: account name must be at most %d characters",
			name, MaxBootstrapAccountNameRunes)
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

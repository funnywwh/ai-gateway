package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const yamlListenOnly = `server:
  listen: ":9999"
`

const yamlListenAndBackup = `server:
  listen: ":9999"
backup:
  retention_daily: 3
`

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	if cfg.Recording.RecordInput != "user" {
		t.Errorf("record_input default = %q, want user (only the user's own input)", cfg.Recording.RecordInput)
	}
	if cfg.Recording.RecordReasoning || cfg.Recording.RecordOutputText {
		t.Errorf("thinking/final-output recording must default to off")
	}
	// 100 characters per user message: the default policy keeps each user message's head
	// rather than the whole question (M81).
	if cfg.Recording.InputMaxChars != 100 {
		t.Errorf("input_max_chars default = %d, want 100", cfg.Recording.InputMaxChars)
	}
	if cfg.Recording.RetentionDays != 30 {
		t.Errorf("retention default = %d, want 30 days", cfg.Recording.RetentionDays)
	}
	if cfg.Billing.InflightPolicy != "abort" || cfg.Billing.OvershootPolicy != "absorb" {
		t.Errorf("in-flight defaults wrong: %q/%q", cfg.Billing.InflightPolicy, cfg.Billing.OvershootPolicy)
	}
	// Queueing is on by default but inert until a provider sets max_inflight: these two
	// values only decide how long an attempt waits once a gate exists.
	if cfg.Routing.ProviderQueueWaitS != 30 || cfg.Routing.ProviderQueueMaxWaiters != 100 {
		t.Errorf("provider queue defaults wrong: %d/%d, want 30/100",
			cfg.Routing.ProviderQueueWaitS, cfg.Routing.ProviderQueueMaxWaiters)
	}
}

func TestProviderQueueValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults are valid", func(*Config) {}, false},
		{"queueing off is valid", func(c *Config) { c.Routing.ProviderQueueWaitS = 0 }, false},
		{"unlimited depth is valid", func(c *Config) { c.Routing.ProviderQueueMaxWaiters = 0 }, false},
		{"negative wait", func(c *Config) { c.Routing.ProviderQueueWaitS = -1 }, true},
		{"negative depth", func(c *Config) { c.Routing.ProviderQueueMaxWaiters = -1 }, true},
		{
			// One attempt may not wait as long as a balance reservation lives.
			"wait reaching the reservation ttl",
			func(c *Config) { c.Routing.ProviderQueueWaitS = c.Billing.ReservationTTLS },
			true,
		},
		{
			"wait times attempts exceeding the reservation ttl",
			func(c *Config) {
				c.Routing.ProviderQueueWaitS = c.Billing.ReservationTTLS/2 + 1
				c.Routing.MaxAttempts = 2
			},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestProviderQueueEnvOverrides(t *testing.T) {
	t.Setenv("GW_ROUTING_PROVIDER_QUEUE_WAIT_S", "5")
	t.Setenv("GW_ROUTING_PROVIDER_QUEUE_MAX_WAITERS", "7")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.ProviderQueueWaitS != 5 || cfg.Routing.ProviderQueueMaxWaiters != 7 {
		t.Fatalf("env overrides not applied: %d/%d",
			cfg.Routing.ProviderQueueWaitS, cfg.Routing.ProviderQueueMaxWaiters)
	}
}

func TestRecordingInputModeFor(t *testing.T) {
	cases := []struct {
		name    string
		global  string
		keyMode string
		want    string
	}{
		{"key inherit takes the deployment default", "full", "inherit", "full"},
		{"empty key mode takes the deployment default", "full", "", "full"},
		{"key mode wins over the deployment default", "off", "full", "full"},
		{"an older console's meta means metadata", "user", "meta", "metadata"},
		{"unknown key modes fall back to the deployment default", "metadata", "everything", "metadata"},
		{"unknown key mode with an unset default", "", "everything", "user"},
		{"case and padding do not change the mode", "user", " Full ", "full"},
		{"an unset default is user", "", "", "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recording := Recording{RecordInput: tc.global}
			if got := recording.InputModeFor(tc.keyMode); got != tc.want {
				t.Fatalf("InputModeFor(%q) with default %q = %q, want %q", tc.keyMode, tc.global, got, tc.want)
			}
		})
	}
}

func TestMCPAdminDefaultsAndOverrides(t *testing.T) {
	cfg := Default()
	if !cfg.MCP.AdminTools {
		t.Error("the administrative MCP surface must default to enabled; the token scope is the gate")
	}
	if cfg.MCP.AdminMaxResponseBytes != 256*1024 {
		t.Errorf("admin_max_response_bytes default = %d", cfg.MCP.AdminMaxResponseBytes)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mcp:\n  admin_tools: false\n  admin_max_response_bytes: 4096\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MCP.AdminTools {
		t.Error("admin_tools: false must survive loading")
	}
	if loaded.MCP.AdminMaxResponseBytes != 4096 {
		t.Errorf("admin_max_response_bytes = %d", loaded.MCP.AdminMaxResponseBytes)
	}

	t.Setenv("GW_MCP_ADMIN_TOOLS", "true")
	fromEnv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fromEnv.MCP.AdminTools {
		t.Error("GW_MCP_ADMIN_TOOLS must override the file")
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file must not fail: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
}

func TestLoadYAMLOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlListenAndBackup), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":9999" {
		t.Errorf("listen override failed: %q", cfg.Server.Listen)
	}
	if cfg.Backup.RetentionDaily != 3 {
		t.Errorf("nested override failed: %d", cfg.Backup.RetentionDaily)
	}
	// untouched fields keep defaults
	if cfg.Backup.Cron != "30 3 * * *" {
		t.Errorf("default lost: %q", cfg.Backup.Cron)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlListenOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GW_SERVER_LISTEN", ":7777")
	t.Setenv("GW_BACKUP_ENABLED", "false")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":7777" {
		t.Errorf("env override failed: %q", cfg.Server.Listen)
	}
	if cfg.Backup.Enabled {
		t.Errorf("env bool override failed")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty listen", func(c *Config) { c.Server.Listen = "" }},
		{"bad default grant", func(c *Config) { c.Auth.DefaultGrant = "sometimes" }},
		{"bad strategy", func(c *Config) { c.Routing.DefaultStrategy = "random" }},
		{"bad inflight policy", func(c *Config) { c.Billing.InflightPolicy = "explode" }},
		{"bad recording mode", func(c *Config) { c.Recording.RecordInput = "everything" }},
		{"negative input cap", func(c *Config) { c.Recording.InputMaxChars = -1 }},
		{"negative retention", func(c *Config) { c.Recording.RetentionDays = -1 }},
		{"ratios inverted", func(c *Config) { c.Billing.InflightSoftRatio = 0.9; c.Billing.InflightHardRatio = 0.5 }},
		{"backup without dir", func(c *Config) { c.Backup.Enabled = true; c.Backup.Dir = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

// An explicit 0 is a policy ("no cap"), not an absent value: if loading applied the 100
// default over it, the documented escape hatch back to pre-M81 behaviour would be
// unreachable. The other half of the pair is that an omitted key really does get 100.
func TestLoadKeepsExplicitZeroInputCap(t *testing.T) {
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cfg, err := Load(write("zero.yaml", "recording:\n  input_max_chars: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Recording.InputMaxChars != 0 {
		t.Fatalf("input_max_chars = %d, want 0 (unlimited)", cfg.Recording.InputMaxChars)
	}
	if cfg.Recording.RecordInput != "user" {
		t.Fatalf("record_input = %q, want the default user", cfg.Recording.RecordInput)
	}

	loaded, err := Load(write("absent.yaml", "recording:\n  record_input: user\n"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recording.InputMaxChars != 100 {
		t.Fatalf("absent input_max_chars = %d, want the 100 default", loaded.Recording.InputMaxChars)
	}
}

func TestBadEnvIntFails(t *testing.T) {
	t.Setenv("GW_RATELIMIT_SHARDS", "many")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error for non-numeric env int")
	}
}

// Session stickiness ships on (a client that sends no prompt_cache_key is unaffected, and
// one that does is exactly the traffic a weighted draw would scatter), so the defaults and
// both override paths have to be pinned.
func TestSessionAffinityDefaultsAndOverrides(t *testing.T) {
	cfg := Default()
	if !cfg.Routing.SessionAffinity {
		t.Error("session_affinity must default to on")
	}
	if cfg.Routing.SessionAffinityTTLS != 1800 || cfg.Routing.SessionAffinityMaxEntries != 10000 {
		t.Errorf("affinity bounds = %d/%d, want 1800/10000",
			cfg.Routing.SessionAffinityTTLS, cfg.Routing.SessionAffinityMaxEntries)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("routing:\n  session_affinity: false\n  session_affinity_ttl_s: 60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.SessionAffinity {
		t.Error("session_affinity: false must survive loading")
	}
	if loaded.Routing.SessionAffinityTTLS != 60 {
		t.Errorf("ttl = %d, want 60", loaded.Routing.SessionAffinityTTLS)
	}
	if loaded.Routing.SessionAffinityMaxEntries != 10000 {
		t.Errorf("an absent key must keep its default, got %d", loaded.Routing.SessionAffinityMaxEntries)
	}

	t.Setenv("GW_ROUTING_SESSION_AFFINITY", "true")
	t.Setenv("GW_ROUTING_SESSION_AFFINITY_MAX_ENTRIES", "5")
	fromEnv, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fromEnv.Routing.SessionAffinity || fromEnv.Routing.SessionAffinityMaxEntries != 5 {
		t.Errorf("env override failed: %+v", fromEnv.Routing)
	}
}

// The bounds are only read while the feature is on: a deployment that switched it off must
// not be stopped from starting by a value it no longer reads.
func TestSessionAffinityBoundsAreValidatedOnlyWhenOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero ttl", func(c *Config) { c.Routing.SessionAffinityTTLS = 0 }},
		{"negative ttl", func(c *Config) { c.Routing.SessionAffinityTTLS = -1 }},
		{"zero capacity", func(c *Config) { c.Routing.SessionAffinityMaxEntries = 0 }},
	} {
		t.Run("on: "+tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s must be rejected while session_affinity is on", tc.name)
			}
			cfg.Routing.SessionAffinity = false
			if err := cfg.Validate(); err != nil {
				t.Fatalf("an off feature must not validate its bounds: %v", err)
			}
		})
	}
}

// The FX table is what keeps a CNY-priced model from being charged as if CNY were
// USD, so a typo in it must stop the gateway at start-up instead of at the first
// request.
func TestValidateCurrencies(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad ledger currency", func(c *Config) { c.Billing.Currency = "US" }},
		{"ledger currency in the table", func(c *Config) {
			c.Billing.FXRates = map[string]int64{"USD": 1_000_000}
		}},
		{"zero rate", func(c *Config) { c.Billing.FXRates = map[string]int64{"CNY": 0} }},
		{"negative rate", func(c *Config) { c.Billing.FXRates = map[string]int64{"CNY": -141000} }},
		{"bad rate key", func(c *Config) { c.Billing.FXRates = map[string]int64{"CN": 141000} }},
		{"display currency without a rate", func(c *Config) { c.Billing.DisplayCurrency = "CNY" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}

	cfg := Default()
	cfg.Billing.FXRates = map[string]int64{"cny": 141000}
	cfg.Billing.DisplayCurrency = "cny"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("lower-case codes must be normalized, not rejected: %v", err)
	}
	if cfg.Billing.FXRates["CNY"] != 141000 || cfg.Billing.DisplayCurrency != "CNY" {
		t.Fatalf("normalized billing = %+v", cfg.Billing.FXRates)
	}
}

func TestLoadFXRatesFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := yamlListenOnly + `billing:
  currency: USD
  display_currency: CNY
  fx_rates:
    CNY: 141000
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Billing.FXRates["CNY"] != 141000 {
		t.Fatalf("fx_rates = %+v", cfg.Billing.FXRates)
	}
	if cfg.Billing.DisplayCurrency != "CNY" || cfg.Billing.Currency != "USD" {
		t.Fatalf("currencies = %q / %q", cfg.Billing.Currency, cfg.Billing.DisplayCurrency)
	}
}

// TestChatConfigIsValidated covers the console chat's own bounds. Each case is a value that
// would otherwise make the console lie: a zero step budget silently falls back to a default,
// a non-positive ticket TTL makes every preview unusable, and a history bound smaller than
// one turn would cut a conversation in the middle of a tool call.
func TestChatConfigIsValidated(t *testing.T) {
	cases := []struct {
		name   string
		tune   func(*Config)
		broken bool
	}{
		{"defaults are valid", func(c *Config) {}, false},
		{"disabled chat ignores the rest", func(c *Config) { c.Chat.Enabled = false; c.Chat.MaxSteps = 0 }, false},
		{"zero steps means no ceiling", func(c *Config) { c.Chat.MaxSteps = 0 }, false},
		{"zero tool calls means no ceiling", func(c *Config) { c.Chat.MaxToolCalls = 0 }, false},
		{"negative steps", func(c *Config) { c.Chat.MaxSteps = -1 }, true},
		{"negative tool calls", func(c *Config) { c.Chat.MaxToolCalls = -1 }, true},
		{"zero tool result bytes", func(c *Config) { c.Chat.MaxToolResultBytes = 0 }, true},
		{"history below one turn", func(c *Config) { c.Chat.MaxHistoryMessages = 1 }, true},
		{"zero history bytes", func(c *Config) { c.Chat.MaxHistoryBytes = 0 }, true},
		{"negative loaded skills", func(c *Config) { c.Chat.MaxLoadedSkills = -1 }, true},
		{"zero skill bytes", func(c *Config) { c.Chat.MaxSkillBytes = 0 }, true},
		{"zero artifact bytes", func(c *Config) { c.Chat.ArtifactMaxBytes = 0 }, true},
		{"zero artifact count", func(c *Config) { c.Chat.ArtifactMaxPerSession = 0 }, true},
		{"zero ticket ttl", func(c *Config) { c.Chat.ArtifactTicketTTL = 0 }, true},
		{"negative max output", func(c *Config) { c.Chat.MaxOutputTokens = -1 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.tune(&cfg)
			err := cfg.Validate()
			if tc.broken && err == nil {
				t.Fatal("expected a validation error")
			}
			if !tc.broken && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if cfg := Default(); cfg.Chat.ArtifactAllowNetwork {
		t.Fatal("previews must not load external resources unless an operator turns that on")
	}
	// The default is "no ceiling on steps or tool calls": a deployment that never asked for
	// a limit must not silently inherit one.
	if cfg := Default(); cfg.Chat.MaxSteps != 0 || cfg.Chat.MaxToolCalls != 0 {
		t.Fatalf("default ceilings = %d steps / %d calls, want 0 (no limit)",
			cfg.Chat.MaxSteps, cfg.Chat.MaxToolCalls)
	}
}

func TestDimensionRollupConfig(t *testing.T) {
	if !Default().Recording.DimensionRollupEnabled {
		t.Fatal("rollups must default on")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("recording:\n  dimension_rollup_enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Recording.DimensionRollupEnabled {
		t.Fatalf("YAML false: %+v %v", cfg, err)
	}
	t.Setenv("GW_RECORDING_DIMENSION_ROLLUP_ENABLED", "true")
	cfg, err = Load(path)
	if err != nil || !cfg.Recording.DimensionRollupEnabled {
		t.Fatalf("env true: %+v %v", cfg, err)
	}
	t.Setenv("GW_RECORDING_DIMENSION_ROLLUP_ENABLED", "invalid")
	if _, err = Load(path); err == nil {
		t.Fatal("invalid bool accepted")
	}
}

// TestBootstrapAccountNamesAreValidated pins the configuration boundary: a name the
// management API would refuse must not reach the database through YAML, and a name the
// API accepts (email, Chinese, surrounding whitespace) must pass start-up validation.
func TestBootstrapAccountNamesAreValidated(t *testing.T) {
	cases := []struct {
		name    string
		account string
		broken  bool
	}{
		{"ascii", "internal", false},
		{"email", "ops@example.com", false},
		{"chinese", "北京研发", false},
		{"padded", "  internal  ", false},
		{"64 chinese chars", strings.Repeat("中", 64), false},
		{"empty", "", true},
		{"spaces only", "\u3000 \t", true},
		{"65 chinese chars", strings.Repeat("中", 65), true},
		{"invalid utf8", "acme\xff", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Bootstrap.Accounts = []BootstrapAccount{{Name: tc.account, BillingMode: "postpaid"}}
			err := cfg.Validate()
			if tc.broken && err == nil {
				t.Fatalf("bootstrap account %q must be rejected at start-up", tc.account)
			}
			if !tc.broken && err != nil {
				t.Fatalf("bootstrap account %q must be accepted: %v", tc.account, err)
			}
		})
	}

	// The account referenced by a seeded API key is the same label, so it is validated by
	// the same rule: a blank "account:" used to mean "skip the key" and now fails loudly.
	cfg := Default()
	cfg.Bootstrap.APIKeys = []BootstrapAPIKey{{Name: "dev", Key: "sk-x", Account: "  "}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an api key with a blank account must be rejected")
	}
	cfg = Default()
	cfg.Bootstrap.APIKeys = []BootstrapAPIKey{{Name: "dev", Key: "sk-x", Account: "运维组"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a unicode account reference must be accepted: %v", err)
	}
}

// TestBootstrapSkipsEntriesTheSeederIgnores pins the boundary between "validated" and
// "skipped": a placeholder api_keys entry with no key material, and an account entry placed
// in a list the seeder never reads, must not stop start-up. Only entries that would actually
// seed or resolve an account are held to the name rule.
func TestBootstrapSkipsEntriesTheSeederIgnores(t *testing.T) {
	cfg := Default()
	cfg.Bootstrap.APIKeys = []BootstrapAPIKey{{Name: "placeholder", Key: "", Account: ""}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an api key entry with no key material must be skipped, got %v", err)
	}
	cfg = Default()
	cfg.Bootstrap.APIKeys = []BootstrapAPIKey{{Name: "", Key: "sk-x", Account: ""}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an api key entry with no name must be skipped, got %v", err)
	}
	// The same empty account on an entry that does carry key material is a real mistake and
	// must still fail, or the key would seed against nothing.
	cfg = Default()
	cfg.Bootstrap.APIKeys = []BootstrapAPIKey{{Name: "dev", Key: "sk-x", Account: ""}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a seeded api key with no account must be rejected")
	}
}

// TestLoadBootstrapUnicodeAccountNames covers the path a `cfg.Validate()` call cannot:
// the YAML file itself. A unicode account name with surrounding whitespace and an
// api_keys reference to it must load and validate, and the loaded struct must still carry
// the file's bytes verbatim — canonicalization belongs to the store's write path
// (UpsertAccount), so normalizing here would give the composition root a configuration
// that no longer matches the file it read.
func TestLoadBootstrapUnicodeAccountNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `server:
  listen: ":9999"
bootstrap:
  mode: upsert
  accounts:
    - name: "  运维@example.com  "
      billing_mode: postpaid
      credit_limit_usd: 20
  api_keys:
    - name: dev
      key: "sk-gw-unicode-fixture"
      account: "运维@example.com"
    - name: placeholder
      key: ""
      account: ""
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a unicode bootstrap account name must load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a unicode bootstrap account name must validate: %v", err)
	}
	if got := cfg.Bootstrap.Accounts[0].Name; got != "  运维@example.com  " {
		t.Fatalf("loaded name = %q, want the file's bytes (the store trims on write)", got)
	}
	if got := cfg.Bootstrap.APIKeys[0].Account; got != "运维@example.com" {
		t.Fatalf("loaded api key account = %q", got)
	}

	// The same file with a name that is only whitespace must fail at load/validate time:
	// nothing would be seeded, so the gateway must not start as if it had.
	blank := strings.Replace(body, `"  运维@example.com  "`, `"   "`, 1)
	if err := os.WriteFile(path, []byte(blank), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a whitespace-only bootstrap account name must be rejected")
	}
}

package config

import (
	"os"
	"path/filepath"
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
	if cfg.Recording.RetentionDays != 30 {
		t.Errorf("retention default = %d, want 30 days", cfg.Recording.RetentionDays)
	}
	if cfg.Billing.InflightPolicy != "abort" || cfg.Billing.OvershootPolicy != "absorb" {
		t.Errorf("in-flight defaults wrong: %q/%q", cfg.Billing.InflightPolicy, cfg.Billing.OvershootPolicy)
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

func TestBadEnvIntFails(t *testing.T) {
	t.Setenv("GW_RATELIMIT_SHARDS", "many")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error for non-numeric env int")
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

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
	if cfg.Recording.RecordInput != "full" {
		t.Errorf("record_input default = %q, want full", cfg.Recording.RecordInput)
	}
	if cfg.Recording.RecordReasoning || cfg.Recording.RecordOutputText {
		t.Errorf("thinking/final-output recording must default to off")
	}
	if cfg.Billing.InflightPolicy != "abort" || cfg.Billing.OvershootPolicy != "absorb" {
		t.Errorf("in-flight defaults wrong: %q/%q", cfg.Billing.InflightPolicy, cfg.Billing.OvershootPolicy)
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

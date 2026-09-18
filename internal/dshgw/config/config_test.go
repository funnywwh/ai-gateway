package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadStrictDefaultsAndOrigins(t *testing.T) {
	p := writeConfig(t, "public_host: dsh.example.test\nstate_dir: "+filepath.Join(t.TempDir(), "state")+"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DirectoryPicker != "clamp" || cfg.PluginBrowserFS != "on" {
		t.Fatalf("unsafe defaults: %#v", cfg)
	}
	if cfg.ValidateTimeout.Duration() != 5*time.Second {
		t.Fatalf("timeout=%s", cfg.ValidateTimeout.Duration())
	}
	if cfg.RegistryPath != filepath.Join(cfg.StateDir, "registry.json") || cfg.TenantRoot != filepath.Join(cfg.StateDir, "tenants") || cfg.HandshakeDir != filepath.Join(cfg.StateDir, "handshake") {
		t.Fatalf("derived state paths: registry=%q tenant=%q handshake=%q", cfg.RegistryPath, cfg.TenantRoot, cfg.HandshakeDir)
	}
	cfg.SetTenantPorts(map[string]int{"alice": 32601})
	if got := cfg.TenantOrigin("alice"); got != "https://dsh.example.test:32601" {
		t.Fatalf("origin=%q", got)
	}
	if got := cfg.SessionCookieName("alice"); got != "dshgw_s_alice" {
		t.Fatalf("cookie=%q", got)
	}
	if name, ok := cfg.TenantFromPort(32601); !ok || name != "alice" {
		t.Fatalf("reverse=%q,%v", name, ok)
	}
}

func TestLoadRejectsUnknownAndSecondDocument(t *testing.T) {
	for _, body := range []string{"public_host: x.test\nunknown: true\n", "public_host: x.test\n---\n{}\n"} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestValidateSecurityBoundaries(t *testing.T) {
	tests := []string{
		"listen: 0.0.0.0:3099\n",
		"directory_picker: off\n",
		"plugin_browser_fs: maybe\n",
		"portal_port: 32601\n",
		"tenant_port_lo: 32100\ntenant_port_hi: 32150\n",
		"workspace_seed: [../escape]\n",
		"key_revalidate: interval:0\n",
	}
	for _, body := range tests {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("accepted unsafe config %q", body)
		}
	}
}

func TestValidTenantName(t *testing.T) {
	for _, ok := range []string{"a", "alice", "team-2"} {
		if !ValidTenantName(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "Alice", "-a", "a-", "a_b", strings.Repeat("a", 28)} {
		if ValidTenantName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// A Secure session cookie is rejected by browsers on a plain-HTTP origin, so the
// attribute has to follow how clients really reach the gateway: a wrong "true"
// here looks like "login does nothing" — the browser drops the session and the
// next request lands back on the portal.
func TestSecureSessionCookieFollowsTheDeploymentScheme(t *testing.T) {
	cases := []struct {
		name      string
		apply     func(*Config)
		wantSecur bool
	}{
		{"port mode default", func(*Config) {}, true},
		{"path mode over https", func(c *Config) { c.PublicBaseURL = "https://chat.example" }, true},
		{"path mode over http", func(c *Config) { c.PublicBaseURL = "http://192.0.2.10:8090" }, false},
		{"explicit always", func(c *Config) {
			c.PublicBaseURL = "http://192.0.2.10:8090"
			c.SessionCookieSecure = "always"
		}, true},
		{"explicit never", func(c *Config) {
			c.PublicBaseURL = "https://chat.example"
			c.SessionCookieSecure = "never"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{PublicHost: "chat.example", PortalPort: 32600, SessionCookieSecure: "auto"}
			tc.apply(cfg)
			if got := cfg.SecureSessionCookie(); got != tc.wantSecur {
				t.Fatalf("SecureSessionCookie() = %v, want %v", got, tc.wantSecur)
			}
		})
	}
	bad := &Config{PublicHost: "chat.example", PortalPort: 32600, SessionCookieSecure: "sometimes"}
	if err := bad.Validate(); err == nil {
		t.Fatal("an unknown session_cookie_secure value was accepted")
	}
}

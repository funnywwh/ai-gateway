package config

import (
	"os"
	"strings"
	"testing"
)

// feishuFixture is a minimal valid enabled block. The stub endpoints are loopback so the
// http-endpoint rule (https unless loopback) does not get in the way of the other cases.
func feishuFixture() Config {
	cfg := Default()
	cfg.CredentialsKey = "test-credentials-key"
	cfg.Feishu.Enabled = true
	cfg.Feishu.AppID = "cli_a5ca35a685b0x26e"
	cfg.Feishu.AppSecret = "secret-value"
	cfg.Feishu.CallbackURL = "http://192.168.190.86:8090/feishu/callback"
	cfg.Feishu.AuthorizeURL = "http://127.0.0.1:9099/authorize"
	cfg.Feishu.TokenURL = "http://127.0.0.1:9099/token"
	cfg.Feishu.UserInfoURL = "http://127.0.0.1:9099/userinfo"
	cfg.Dshgw.Enabled = true
	cfg.Dshgw.PublicHost = "192.168.190.86"
	cfg.Dshgw.PortalPort = 18300
	cfg.Dshgw.PublicScheme = "http"
	return cfg
}

// The whole feature is off by default, and while it is off a deployment must be able to
// carry any half-filled block it likes without being stopped at startup.
func TestFeishuIsOffByDefaultAndInert(t *testing.T) {
	cfg := Default()
	if cfg.Feishu.Enabled {
		t.Fatal("feishu must be disabled by default")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	// Even nonsense in the block is not read while the feature is off.
	cfg.Feishu.AppID = "not-an-app-id"
	cfg.Feishu.CallbackURL = "://broken"
	cfg.Feishu.TimeoutS = -1
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled feishu block must not be validated: %v", err)
	}
}

func TestFeishuEnabledBlockValidates(t *testing.T) {
	cfg := feishuFixture()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid feishu block was rejected: %v", err)
	}
	// The two derived browser URLs are what the rest of the system states to Feishu and
	// to the user, so they are worth pinning here.
	if got, want := cfg.FeishuLoginURL(), "http://192.168.190.86:8090/feishu/login"; got != want {
		t.Fatalf("login url = %q, want %q", got, want)
	}
	if got, want := cfg.DSHGWPortalURL(), "http://192.168.190.86:18300"; got != want {
		t.Fatalf("portal url = %q, want %q", got, want)
	}
}

func TestFeishuRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"missing app id":      func(c *Config) { c.Feishu.AppID = "" },
		"app id not a cli id": func(c *Config) { c.Feishu.AppID = "a5ca35a685b0x26e" },
		"missing app secret":  func(c *Config) { c.Feishu.AppSecret = "" },
		"callback without the path": func(c *Config) {
			c.Feishu.CallbackURL = "http://192.168.190.86:8090/oauth/return"
		},
		"callback with a query": func(c *Config) {
			c.Feishu.CallbackURL = "http://192.168.190.86:8090/feishu/callback?x=1"
		},
		"callback relative": func(c *Config) { c.Feishu.CallbackURL = "/feishu/callback" },
		"public https endpoint on a non-loopback host": func(c *Config) {
			c.Feishu.TokenURL = "http://open.feishu.cn/oauth/v3/token"
		},
		"token endpoint with a fragment": func(c *Config) {
			c.Feishu.TokenURL = "https://accounts.feishu.cn/oauth/v3/token#frag"
		},
		"empty userinfo url":   func(c *Config) { c.Feishu.UserInfoURL = "" },
		"timeout out of range": func(c *Config) { c.Feishu.TimeoutS = 0 },
		"state ttl too short":  func(c *Config) { c.Feishu.StateTTLS = 5 },
		"state ttl too long":   func(c *Config) { c.Feishu.StateTTLS = 7200 },
		"state secret and credentials key both empty": func(c *Config) {
			c.Feishu.StateSecret = ""
			c.CredentialsKey = ""
		},
		"ticket ttl out of range": func(c *Config) { c.Feishu.TicketTTLS = 5 },
		"ticket secret and credentials key both empty": func(c *Config) {
			c.Feishu.TicketSecret = ""
			c.CredentialsKey = ""
		},
		"portal url unknown": func(c *Config) {
			c.Dshgw.PublicHost = ""
			c.Dshgw.PortalPort = 0
		},
		"portal not owned and not stated": func(c *Config) { c.Dshgw.Enabled = false },
		"portal url relative":             func(c *Config) { c.Feishu.PortalURL = "portal/" },
	}
	for name, mutate := range cases {
		cfg := feishuFixture()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A deployment that only wants the console's key binding must be able to leave the DSH
// login flow off — and then the ticket settings stop mattering.
func TestFeishuDSHLoginIsOptional(t *testing.T) {
	cfg := feishuFixture()
	cfg.Feishu.DSHLogin = false
	cfg.Feishu.TicketSecret = ""
	cfg.Feishu.TicketTTLS = 0
	cfg.Dshgw.Enabled = false
	cfg.Dshgw.PublicHost = ""
	cfg.Dshgw.PortalPort = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("dsh_login=off must not require the ticket or the portal URL: %v", err)
	}
}

// The portal URL follows the same rules the child uses for its own origins: port mode
// uses host:portal_port with the configured scheme, path mode uses the base URL plus the
// portal prefix.
func TestDSHGWPortalURLDerivation(t *testing.T) {
	cfg := Default()
	cfg.Dshgw.PublicHost = "chat.example"
	cfg.Dshgw.PortalPort = 32600

	// Auto keeps https: that is what a TLS deployment wants, and the child's own
	// default agrees.
	if got, want := cfg.DSHGWPortalURL(), "https://chat.example:32600"; got != want {
		t.Fatalf("auto portal url = %q, want %q", got, want)
	}
	cfg.Dshgw.PublicScheme = "http"
	if got, want := cfg.DSHGWPortalURL(), "http://chat.example:32600"; got != want {
		t.Fatalf("http portal url = %q, want %q", got, want)
	}
	// Path mode: the portal lives under the prefix on the one public origin.
	cfg.Dshgw.PublicBaseURL = "https://chat.example"
	cfg.Dshgw.PortalPathPrefix = "/dshgw"
	if got, want := cfg.DSHGWPortalURL(), "https://chat.example/dshgw"; got != want {
		t.Fatalf("path-mode portal url = %q, want %q", got, want)
	}
	// An explicit value always wins, and its trailing slash is not part of it.
	cfg.Feishu.PortalURL = "https://portal.example:9000/"
	if got, want := cfg.DSHGWPortalURL(), "https://portal.example:9000"; got != want {
		t.Fatalf("explicit portal url = %q, want %q", got, want)
	}
}

// The login URL is derived from the callback so a deployment states exactly one public
// origin; a base path in the callback therefore travels with it.
func TestFeishuLoginURLKeepsTheBasePath(t *testing.T) {
	cfg := Default()
	cfg.Feishu.CallbackURL = "https://chat.example/aigw/feishu/callback"
	if got, want := cfg.FeishuLoginURL(), "https://chat.example/aigw/feishu/login"; got != want {
		t.Fatalf("login url = %q, want %q", got, want)
	}
	cfg.Feishu.CallbackURL = ""
	if got := cfg.FeishuLoginURL(); got != "" {
		t.Fatalf("login url = %q, want empty without a callback", got)
	}
}

func TestFeishuEnvOverrides(t *testing.T) {
	cfg := feishuFixture()
	cfg.Feishu.AppID = ""
	cfg.Feishu.AppSecret = ""
	cfg.Feishu.CallbackURL = ""
	t.Setenv("GW_FEISHU_APP_ID", "cli_fromenv123")
	t.Setenv("GW_FEISHU_APP_SECRET", "secret-from-env")
	t.Setenv("GW_FEISHU_CALLBACK_URL", "http://192.168.190.86:8090/feishu/callback")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("env-provided feishu settings were rejected: %v", err)
	}
	if cfg.Feishu.AppID != "cli_fromenv123" || !strings.HasPrefix(cfg.Feishu.CallbackURL, "http://192.168.190.86:8090") {
		t.Fatalf("env overrides not applied: %+v", cfg.Feishu)
	}
}

// The example file is the documentation operators copy; an enabled block there must be
// known to the strict decoder, and a commented-out one must not break loading.
func TestFeishuBlockIsDecodableFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	body := yamlListenOnly + `feishu:
  enabled: true
  app_id: cli_a5ca35a685b0x26e
  app_secret: s3cret
  callback_url: http://192.168.190.86:8090/feishu/callback
  authorize_url: http://127.0.0.1:9099/authorize
  token_url: http://127.0.0.1:9099/token
  userinfo_url: http://127.0.0.1:9099/userinfo
  dsh_login: false
dshgw:
  public_host: 192.168.190.86
  portal_port: 18300
  public_scheme: http
credentials_key: test-credentials-key
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Feishu.Enabled || cfg.Feishu.AppID != "cli_a5ca35a685b0x26e" || cfg.Feishu.DSHLogin {
		t.Fatalf("yaml block not decoded: %+v", cfg.Feishu)
	}
	if cfg.Dshgw.PublicScheme != "http" {
		t.Fatalf("dshgw.public_scheme = %q", cfg.Dshgw.PublicScheme)
	}
}

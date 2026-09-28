package config

import (
	"os"
	"strings"
	"testing"
)

// The extra companies of the organization sync (M89). Every rule here exists because the
// company parameter is human-typed: the app id and the name are both accepted, so both have to
// be unique and a name must not be mistakable for an app id.

func feishuCompanyFixture() Config {
	cfg := feishuFixture()
	cfg.Feishu.Companies = []FeishuCompany{{
		Name: "某某科技", AppID: "cli_1111111111111111", AppSecret: "company-secret",
	}}
	return cfg
}

func TestFeishuCompaniesValidate(t *testing.T) {
	cfg := feishuCompanyFixture()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid company list was rejected: %v", err)
	}
	if got := cfg.IdentityCompanyName(); got != "本公司" {
		t.Fatalf("identity company name = %q, want the default", got)
	}
	cfg.Feishu.CompanyName = "我方公司"
	if got := cfg.IdentityCompanyName(); got != "我方公司" {
		t.Fatalf("identity company name = %q, want the configured one", got)
	}
	// The company node name defaults to the company's own name, and root_node overrides it.
	if got := cfg.Feishu.Companies[0].RootNodeName(); got != "某某科技" {
		t.Fatalf("root node name = %q", got)
	}
	cfg.Feishu.Companies[0].RootNode = "某某科技集团"
	if got := cfg.Feishu.Companies[0].RootNodeName(); got != "某某科技集团" {
		t.Fatalf("root node name = %q, want the override", got)
	}
}

func TestFeishuCompaniesRequireTheBlock(t *testing.T) {
	cfg := feishuCompanyFixture()
	cfg.Feishu.Enabled = false
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "feishu.enabled") {
		t.Fatalf("companies with the block off must be refused, got %v", err)
	}
}

func TestFeishuCompanyValidationMatrix(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing name", func(c *Config) { c.Feishu.Companies[0].Name = "  " }, "name must be set"},
		{"long name", func(c *Config) {
			c.Feishu.Companies[0].Name = strings.Repeat("公", 65)
		}, "at most 64 characters"},
		{"name looks like an app id", func(c *Config) { c.Feishu.Companies[0].Name = "cli_nope" }, "must not start with cli_"},
		{"duplicate name", func(c *Config) {
			c.Feishu.Companies = append(c.Feishu.Companies, FeishuCompany{
				Name: "某某科技", AppID: "cli_2222222222222222", AppSecret: "s",
			})
		}, "already used by"},
		{"name collides with the identity company", func(c *Config) {
			c.Feishu.CompanyName = "某某科技"
		}, "already used by"},
		{"bad app id", func(c *Config) { c.Feishu.Companies[0].AppID = "12345" }, "must be that company's own App ID"},
		{"app id of the identity application", func(c *Config) {
			c.Feishu.Companies[0].AppID = c.Feishu.AppID
		}, "identity application is always the first company"},
		{"duplicate app id", func(c *Config) {
			c.Feishu.Companies = append(c.Feishu.Companies, FeishuCompany{
				Name: "另一家", AppID: "cli_1111111111111111", AppSecret: "s",
			})
		}, "already used by"},
		{"no secret", func(c *Config) { c.Feishu.Companies[0].AppSecret = "" }, "needs app_secret or app_secret_env"},
		{"empty env secret", func(c *Config) {
			c.Feishu.Companies[0].AppSecret = ""
			c.Feishu.Companies[0].AppSecretEnv = "GW_FEISHU_TEST_MISSING_SECRET"
		}, "GW_FEISHU_TEST_MISSING_SECRET"},
		{"long root node", func(c *Config) { c.Feishu.Companies[0].RootNode = strings.Repeat("根", 65) }, "root_node must be at most 64"},
		{"root node collides", func(c *Config) { c.Feishu.Companies[0].RootNode = "本公司" }, "collides with the company node"},
		{"too many companies", func(c *Config) {
			for i := 0; i < maxFeishuCompanies+1; i++ {
				c.Feishu.Companies = append(c.Feishu.Companies, FeishuCompany{
					Name: "公司" + strings.Repeat("x", i+1), AppID: "cli_" + strings.Repeat("9", i+1), AppSecret: "s",
				})
			}
		}, "at most 32 are supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := feishuCompanyFixture()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected a validation error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantSub)
			}
			// The message has to name the setting an operator can go and edit.
			if !strings.Contains(err.Error(), "feishu.") {
				t.Fatalf("error = %v, want it to name the setting", err)
			}
		})
	}
}

// A company secret may come from the environment instead of the file (the same rule as the
// identity application's), and the named variable is resolved while the configuration loads.
func TestFeishuCompanySecretFromEnv(t *testing.T) {
	t.Setenv("GW_FEISHU_TEST_COMPANY_SECRET", "from-env")
	path := t.TempDir() + "/config.yaml"
	body := `
server: {listen: "127.0.0.1:8080"}
database: {path: "test.db"}
feishu:
  enabled: true
  app_id: cli_0000000000000000
  app_secret: secret-value
  company_name: 本公司
  callback_url: http://192.0.2.101:8090/feishu/callback
  authorize_url: http://127.0.0.1:9099/authorize
  token_url: http://127.0.0.1:9099/token
  userinfo_url: http://127.0.0.1:9099/userinfo
  dsh_login: false
  admin_login: false
  companies:
    - name: 某某科技
      app_id: cli_1111111111111111
      app_secret_env: GW_FEISHU_TEST_COMPANY_SECRET
credentials_key: test-credentials-key
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Feishu.Companies[0].AppSecret; got != "from-env" {
		t.Fatalf("company secret = %q, want the environment value", got)
	}
	if got := cfg.Feishu.Companies[0].Name; got != "某某科技" {
		t.Fatalf("company name = %q", got)
	}
	// A literal secret still wins when no variable is named.
	cfg.Feishu.Companies[0].AppSecretEnv = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("literal company secret rejected: %v", err)
	}
}

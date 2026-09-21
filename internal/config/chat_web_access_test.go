package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/webaccess"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

// TestChatWebAccessDefaults pins the shipped defaults: the feature is off, and the values a
// zero-valued block is filled with match the web-access package's own defaults.
func TestChatWebAccessDefaults(t *testing.T) {
	cfg := Default()
	web := cfg.Chat.WebAccess
	if web.Enabled {
		t.Error("console web access must ship disabled")
	}
	if web.Provider != "bing" {
		t.Errorf("provider = %q, want bing (the backend that needs no key)", web.Provider)
	}
	if web.TimeoutS != ChatWebAccessTimeoutS || web.MaxResults != ChatWebAccessMaxResults ||
		web.FetchMaxBytes != ChatWebAccessFetchMaxBytes || web.FetchMaxTextBytes != ChatWebAccessFetchMaxTextBytes ||
		web.MaxCallsPerTurn != ChatWebAccessMaxCallsPerTurn {
		t.Errorf("defaults = %+v", web)
	}
	if web.AllowPrivateHosts {
		t.Error("the SSRF guard must default to on")
	}
}

// TestChatWebAccessDefaultsMatchWebaccess keeps the restated numbers from drifting away from
// the package that actually enforces them.
func TestChatWebAccessDefaultsMatchWebaccess(t *testing.T) {
	if ChatWebAccessTimeoutS != int(webaccess.DefaultTimeout.Seconds()) {
		t.Errorf("timeout drifted: config=%d webaccess=%v", ChatWebAccessTimeoutS, webaccess.DefaultTimeout)
	}
	if ChatWebAccessMaxResults != webaccess.DefaultMaxResults {
		t.Errorf("max_results drifted: config=%d webaccess=%d", ChatWebAccessMaxResults, webaccess.DefaultMaxResults)
	}
	if ChatWebAccessFetchMaxBytes != webaccess.DefaultFetchMaxBytes {
		t.Errorf("fetch_max_bytes drifted: config=%d webaccess=%d", ChatWebAccessFetchMaxBytes, webaccess.DefaultFetchMaxBytes)
	}
	if ChatWebAccessFetchMaxTextBytes != webaccess.DefaultFetchMaxTextBytes {
		t.Errorf("fetch_max_text_bytes drifted: config=%d webaccess=%d",
			ChatWebAccessFetchMaxTextBytes, webaccess.DefaultFetchMaxTextBytes)
	}
	if ChatWebAccessMaxResults > webaccess.MaxResultsLimit {
		t.Errorf("the default max_results %d exceeds the backend limit %d",
			ChatWebAccessMaxResults, webaccess.MaxResultsLimit)
	}
}

// TestChatWebAccessProviderListMatchesWebaccess pins the third restated rule: the provider
// names the configuration accepts are exactly the ones the runtime can build.
func TestChatWebAccessProviderListMatchesWebaccess(t *testing.T) {
	want := map[string]bool{
		webaccess.ProviderSearxNG: true, webaccess.ProviderBocha: true,
		webaccess.ProviderTavily: true, webaccess.ProviderBing: true,
	}
	if len(ChatWebAccessProviders) != len(want) {
		t.Fatalf("provider lists have different sizes: config=%v webaccess=%v", ChatWebAccessProviders, want)
	}
	for _, name := range ChatWebAccessProviders {
		if !want[name] {
			t.Errorf("config accepts provider %q, which internal/webaccess does not implement", name)
		}
	}
}

// TestChatWebAccessProxySchemesMatchProviderkit pins the proxy vocabulary: the console's
// web-access proxy is parsed by the same rule as a provider's.
func TestChatWebAccessProxySchemesMatchProviderkit(t *testing.T) {
	got := ChatWebAccessProxySchemeNames()
	want := providerkit.SupportedProxySchemes()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("proxy schemes drifted: config=%v providerkit=%v", got, want)
	}
	for _, scheme := range want {
		if !ChatWebAccessProxySchemes[scheme] {
			t.Errorf("scheme %q missing from ChatWebAccessProxySchemes", scheme)
		}
		if _, err := providerkit.ParseProxyURL(scheme + "://127.0.0.1:1080"); err != nil {
			t.Errorf("providerkit rejects %s://: %v", scheme, err)
		}
	}
}

// TestValidateWebAccessRejectsBadValues is the start-up contract: every one of these would
// otherwise appear as "the model cannot find anything" during an incident.
func TestValidateWebAccessRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ChatWebAccess)
	}{
		{"unknown provider", func(w *ChatWebAccess) { w.Provider = "google" }},
		{"missing provider", func(w *ChatWebAccess) { w.Provider = "" }},
		{"searxng without base_url", func(w *ChatWebAccess) { w.Provider = "searxng" }},
		{"bocha without key", func(w *ChatWebAccess) { w.Provider = "bocha"; w.APIKey = "" }},
		{"tavily without key", func(w *ChatWebAccess) { w.Provider = "tavily"; w.APIKey = "  " }},
		{"bad base_url scheme", func(w *ChatWebAccess) { w.Provider = "searxng"; w.BaseURL = "searx.example.com" }},
		{"base_url without host", func(w *ChatWebAccess) { w.Provider = "searxng"; w.BaseURL = "https://" }},
		{"zero timeout", func(w *ChatWebAccess) { w.TimeoutS = 0 }},
		{"too many results", func(w *ChatWebAccess) { w.MaxResults = 100 }},
		{"zero results", func(w *ChatWebAccess) { w.MaxResults = 0 }},
		{"zero fetch bytes", func(w *ChatWebAccess) { w.FetchMaxBytes = 0 }},
		{"zero text bytes", func(w *ChatWebAccess) { w.FetchMaxTextBytes = 0 }},
		{"text above bytes", func(w *ChatWebAccess) { w.FetchMaxTextBytes = w.FetchMaxBytes + 1 }},
		{"zero calls per turn", func(w *ChatWebAccess) { w.MaxCallsPerTurn = 0 }},
		{"proxy without scheme", func(w *ChatWebAccess) { w.Proxy = "127.0.0.1:7890" }},
		{"proxy with bad scheme", func(w *ChatWebAccess) { w.Proxy = "ftp://127.0.0.1:7890" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Chat.WebAccess.Enabled = true
			tc.mutate(&cfg.Chat.WebAccess)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
		})
	}
}

// TestValidateWebAccessAcceptsTheDocumentedConfigurations: the four backends as the docs
// describe them, plus the one proxy shape that looks like a private address and is legitimate.
func TestValidateWebAccessAcceptsTheDocumentedConfigurations(t *testing.T) {
	for name, mutate := range map[string]func(*ChatWebAccess){
		"searxng": func(w *ChatWebAccess) { w.Provider = "searxng"; w.BaseURL = "http://127.0.0.1:8888/" },
		"bocha":   func(w *ChatWebAccess) { w.Provider = "bocha"; w.APIKey = "sk-bocha" },
		"tavily":  func(w *ChatWebAccess) { w.Provider = "tavily"; w.APIKey = "tvly-x" },
		"bing":    func(w *ChatWebAccess) { w.Provider = "bing" },
		"proxy": func(w *ChatWebAccess) {
			w.Provider = "tavily"
			w.APIKey = "tvly-x"
			w.Proxy = "socks5://127.0.0.1:1080"
		},
		"env proxy": func(w *ChatWebAccess) {
			w.Provider = "tavily"
			w.APIKey = "tvly-x"
			w.Proxy = "env"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Chat.WebAccess.Enabled = true
			mutate(&cfg.Chat.WebAccess)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("a documented configuration was refused: %v", err)
			}
		})
	}
	// A searxng base URL keeps no trailing slash: the runtime appends its own path.
	cfg := Default()
	cfg.Chat.WebAccess.Enabled = true
	cfg.Chat.WebAccess.Provider = "searxng"
	cfg.Chat.WebAccess.BaseURL = "https://searx.example.com/"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Chat.WebAccess.BaseURL != "https://searx.example.com" {
		t.Errorf("base_url = %q, want the trailing slash trimmed", cfg.Chat.WebAccess.BaseURL)
	}
}

// TestValidateWebAccessRunsWithoutChat: a typo in the block must be caught at start-up even
// while the console itself is off, so turning the console on cannot be the moment it breaks.
func TestValidateWebAccessRunsWithoutChat(t *testing.T) {
	cfg := Default()
	cfg.Chat.Enabled = false
	cfg.Chat.WebAccess.Enabled = true
	cfg.Chat.WebAccess.Provider = "bocha"
	cfg.Chat.WebAccess.APIKey = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("web access must be validated even when the console is disabled")
	}
	// A disabled block still refuses a provider that does not exist, and accepts leftovers
	// such as a key whose provider was switched back to bing.
	cfg = Default()
	cfg.Chat.WebAccess.Provider = "google"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown provider must be refused even when web access is off")
	}
	cfg = Default()
	cfg.Chat.WebAccess.APIKey = "leftover-from-another-backend"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled block must tolerate leftover values: %v", err)
	}
}

// TestLoadWebAccessFromYAMLAndEnv covers both configuration paths, and the one value that has
// an environment variable precisely so it need not live in the file.
func TestLoadWebAccessFromYAMLAndEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `chat:
  web_access:
    enabled: true
    provider: tavily
    api_key: from-yaml
    timeout_s: 20
    max_results: 4
    fetch_max_bytes: 2048
    fetch_max_text_bytes: 1024
    max_calls_per_turn: 3
    allow_private_hosts: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	web := cfg.Chat.WebAccess
	if !web.Enabled || web.Provider != "tavily" || web.APIKey != "from-yaml" || web.TimeoutS != 20 ||
		web.MaxResults != 4 || web.FetchMaxBytes != 2048 || web.FetchMaxTextBytes != 1024 ||
		web.MaxCallsPerTurn != 3 || !web.AllowPrivateHosts {
		t.Fatalf("loaded block = %+v", web)
	}
	if got := cfg.Chat.WebAccess.Provider; got != "tavily" {
		t.Errorf("provider = %q", got)
	}
	// The environment wins, so a key can stay out of the file and out of its backups.
	t.Setenv("GW_CHAT_WEB_API_KEY", "from-env")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Chat.WebAccess.APIKey != "from-env" {
		t.Errorf("api_key = %q, want the environment value", cfg.Chat.WebAccess.APIKey)
	}
}

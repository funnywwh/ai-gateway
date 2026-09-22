// Package webaccess gives the console chat its two web tools: a search backend adapter
// (searxng / bocha / tavily / bing) and a guarded page fetcher with a minimal HTML-to-text
// extractor.
//
// It is a leaf package on purpose: it imports nothing from the gateway, takes its own Config,
// and returns errors whose text is already the sentence a model should read. The transport
// layer owns the policy — whether the tools exist at all (deployment switch), whether this
// conversation may use them (session switch) and how many calls one turn gets — while this
// package only knows how to reach the outside world, and to refuse to reach the inside one.
package webaccess

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Provider names accepted by Config.Provider.
const (
	ProviderSearxNG = "searxng"
	ProviderBocha   = "bocha"
	ProviderTavily  = "tavily"
	ProviderBing    = "bing"
)

// Defaults applied by New for fields left at their zero value. The configuration layer fills
// these in as well (config.example.yaml documents the same numbers); they exist here so a
// caller that builds a Config by hand — a test, a future tool — cannot end up with an
// unbounded download or a request that never times out.
const (
	DefaultTimeout           = 15 * time.Second
	DefaultMaxResults        = 6
	DefaultFetchMaxBytes     = 1 << 20
	DefaultFetchMaxTextBytes = 32 << 10
	MaxResultsLimit          = 20
)

// Freshness is the shared time-range vocabulary the model sees. Each backend maps it to its
// own dialect, and a backend that cannot express it says so in the result note instead of
// silently searching everything.
const (
	FreshnessNone    = "noLimit"
	FreshnessDay     = "oneDay"
	FreshnessWeek    = "oneWeek"
	FreshnessMonth   = "oneMonth"
	FreshnessYear    = "oneYear"
	freshnessDefault = FreshnessNone
)

// FreshnessValues lists the accepted values in the order a schema should present them.
func FreshnessValues() []string {
	return []string{FreshnessNone, FreshnessDay, FreshnessWeek, FreshnessMonth, FreshnessYear}
}

// Config is one deployment's web access setup.
type Config struct {
	// Provider selects the search backend: searxng | bocha | tavily | bing.
	Provider string
	// BaseURL is the backend's own address: required for searxng, optional for the rest
	// (each has a default, see defaultBaseURL).
	BaseURL string
	// APIKey is the backend credential for bocha and tavily. It is never logged, never
	// echoed back and never put into a tool result.
	APIKey string
	// Proxy is the egress proxy, with the same vocabulary as a provider's `proxy` field:
	// http://host:port, https://host:port, socks5://host:port, or the literal "env" to
	// follow HTTPS_PROXY/NO_PROXY. Empty means a direct connection and deliberately does
	// not read the environment, so an upgrade never reroutes an existing deployment.
	Proxy string
	// Timeout bounds one search or fetch request.
	Timeout time.Duration
	// MaxResults is the ceiling for one search (the model may ask for fewer).
	MaxResults int
	// FetchMaxBytes bounds how much of one page is downloaded.
	FetchMaxBytes int
	// FetchMaxTextBytes bounds the extracted text handed to the model. It must not exceed
	// FetchMaxBytes, and should stay below the chat's tool-result cap so the truncation the
	// model sees is this package's explicit flag rather than a silent global cut.
	FetchMaxTextBytes int
	// AllowPrivateHosts permits fetching addresses inside the deployment's own networks.
	// Off by default: a URL chosen by a model must not become a probe into private space.
	AllowPrivateHosts bool
	// UserAgent overrides the request UA (tests use it to pin the header).
	UserAgent string
}

// Validate rejects a configuration that would only fail at the first question. It is called
// by the configuration layer at start-up and again by New, so a hand-built Config cannot skip
// it.
func (c Config) Validate() error {
	switch c.Provider {
	case ProviderSearxNG, ProviderBocha, ProviderTavily, ProviderBing:
	case "":
		return fmt.Errorf("web access requires a provider (searxng, bocha, tavily or bing)")
	default:
		return fmt.Errorf("unknown web access provider %q (want searxng, bocha, tavily or bing)", c.Provider)
	}
	if c.Provider == ProviderSearxNG && strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("provider searxng requires base_url (the instance address)")
	}
	if c.providerNeedsKey() && strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("provider %s requires api_key", c.Provider)
	}
	if raw := strings.TrimSpace(c.BaseURL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("base_url is not a valid URL: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("base_url must be an http or https URL (got %q)", raw)
		}
		if u.Host == "" {
			return fmt.Errorf("base_url must include a host (got %q)", raw)
		}
	}
	if c.Timeout < 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if c.MaxResults < 0 || c.MaxResults > MaxResultsLimit {
		return fmt.Errorf("max_results must be between 1 and %d", MaxResultsLimit)
	}
	if c.FetchMaxBytes < 0 {
		return fmt.Errorf("fetch_max_bytes must be positive")
	}
	if c.FetchMaxTextBytes < 0 {
		return fmt.Errorf("fetch_max_text_bytes must be positive")
	}
	if c.FetchMaxBytes > 0 && c.FetchMaxTextBytes > c.FetchMaxBytes {
		return fmt.Errorf("fetch_max_text_bytes must not exceed fetch_max_bytes")
	}
	return nil
}

// providerNeedsKey reports whether the backend cannot work without a credential. bing is
// parsed from a public result page and searxng is normally self-hosted, so neither has one.
func (c Config) providerNeedsKey() bool {
	return c.Provider == ProviderBocha || c.Provider == ProviderTavily
}

// withDefaults fills the zero values so New can be called with a partial Config.
func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxResults <= 0 {
		c.MaxResults = DefaultMaxResults
	}
	if c.FetchMaxBytes <= 0 {
		c.FetchMaxBytes = DefaultFetchMaxBytes
	}
	if c.FetchMaxTextBytes <= 0 {
		c.FetchMaxTextBytes = DefaultFetchMaxTextBytes
	}
	if c.FetchMaxTextBytes > c.FetchMaxBytes {
		c.FetchMaxTextBytes = c.FetchMaxBytes
	}
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL(c.Provider)
	}
	if strings.TrimSpace(c.UserAgent) == "" {
		c.UserAgent = defaultUserAgent
	}
	return c
}

// defaultBaseURL is the public endpoint of each backend. Only the two key-backed backends have
// an official one; searxng requires the operator's own instance and bing is a result page.
func defaultBaseURL(provider string) string {
	switch provider {
	case ProviderBocha:
		return "https://api.bocha.cn"
	case ProviderTavily:
		return "https://api.tavily.com"
	case ProviderBing:
		return "https://cn.bing.com"
	default:
		return ""
	}
}

// defaultUserAgent identifies the gateway to the sites it fetches. A stable, honest UA is the
// difference between "a tool read one page" and "an anonymous scraper", and bing only answers
// requests that look like a browser.
const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0 Safari/537.36 aigw-webaccess/1"

// Item is one search hit.
type Item struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet,omitempty"`
	Source    string `json:"source,omitempty"`
	Published string `json:"published,omitempty"`
}

// SearchResult is what one web_search call hands back to the model.
type SearchResult struct {
	Provider string `json:"provider"`
	Query    string `json:"query"`
	Count    int    `json:"count"`
	Items    []Item `json:"results"`
	// Note carries the caveats a model must be able to see: a backend that ignored the
	// requested time range, or a backend that answered with no results at all.
	Note string `json:"note,omitempty"`
}

// Page is what one web_fetch call hands back to the model.
type Page struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url,omitempty"`
	Title       string `json:"title,omitempty"`
	Content     string `json:"content"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int    `json:"bytes"`
	Truncated   bool   `json:"truncated"`
	// Note explains a degraded but usable result, such as a page that is not UTF-8.
	Note string `json:"note,omitempty"`
}

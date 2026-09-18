// Package frontproxy is the single-domain path-prefix reverse proxy for this
// project: one public listener that fronts aigw, the dshgw portal and every
// tenant's dsh UI, separated by URL prefix instead of by port.
//
// Why a separate program rather than more routing inside dshgw: aigw and dshgw
// stay independent deployables (M51's decoupling), the proxy owns the public
// surface (TLS, Host, path layout), and a misconfiguration here cannot corrupt
// tenant state. It deliberately depends on nothing but the standard library and
// YAML: it is a thin, auditable edge.
package frontproxy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the proxy's whole configuration surface.
type Config struct {
	// Listen is the public address, e.g. ":443" or "127.0.0.1:8443".
	Listen string `yaml:"listen"`
	// PublicHost is the one domain the proxy serves. Every request's Host must
	// match it, and the proxy derives the upstream Host headers from it.
	PublicHost string `yaml:"public_host"`
	// TLS terminates HTTPS directly. Empty means plain HTTP, which is only
	// appropriate on a trusted network (or behind another TLS terminator).
	TLS TLS `yaml:"tls"`
	// AigwPrefix/AigwUpstream mount aigw under a path prefix. The prefix is
	// forwarded *unchanged*: aigw's own server.base_path serves it, which is what
	// keeps its cookie Path, redirects and console URLs consistent.
	AigwPrefix   string `yaml:"aigw_prefix"`
	AigwUpstream string `yaml:"aigw_upstream"`
	// RootRedirect, when set, sends a request for exactly "/" there. aigw serves
	// nothing at its root (its console lives at /admin/ui/ and its API under /v1),
	// so the useful default for a root-mounted deployment is the console.
	RootRedirect string `yaml:"root_redirect"`
	// AigwStripPrefix removes the prefix before forwarding. It is needed when aigw
	// serves the root (its base_path is exclusive: with a prefix configured, a
	// request without it is a 404). Set it when aigw must *also* stay reachable
	// directly on its own port, and leave it off when aigw itself owns the prefix.
	AigwStripPrefix bool `yaml:"aigw_strip_prefix"`
	// PortalPrefix/PortalUpstream mount the dshgw portal (login page) under a path.
	PortalPrefix   string `yaml:"portal_prefix"`
	PortalUpstream string `yaml:"portal_upstream"`
	// PortalPort is the dshgw portal port: the upstream Host and the edge port
	// header are derived from it, because dshgw routes on exactly those.
	PortalPort int `yaml:"portal_port"`
	// TenantPrefix is where tenant dsh UIs live: <prefix>/<tenant>/...
	TenantPrefix string `yaml:"tenant_prefix"`
	// TenantUpstream is the dshgw listener that fronts the workers (dshgw.Listen).
	TenantUpstream string `yaml:"tenant_upstream"`
	// PublicScheme is the scheme the proxy advertises in the URLs it redirects to
	// ("auto" follows the TLS setting). Redirects exist so one domain can still be
	// the front door when the services themselves live on their own ports.
	PublicScheme string `yaml:"public_scheme"`
	// PortalRedirect/TenantRedirect turn the portal and tenant prefixes into
	// redirects to the service's own port instead of proxied requests.
	//
	// They are the bridge for a deployment that cannot use subdomains *and* cannot
	// use path prefixes for the UI: the dsh client builds its API and stream URLs
	// from location.origin, which never carries a path, so the UI must own an
	// origin — a port. The familiar domain paths then hand the browser over to that
	// origin instead of pretending to serve it.
	PortalRedirect bool `yaml:"portal_redirect"`
	TenantRedirect bool `yaml:"tenant_redirect"`
	// EdgePortHeader is the header dshgw routes on (dshgw.edge_port_header).
	// Empty means the project default.
	EdgePortHeader string `yaml:"edge_port_header"`
	// APIPaths are the request paths whose data must never be cached by this proxy or
	// by anything it forwards to (browsers, corporate proxies, an nginx in front).
	// Matched after the route prefix is stripped, so "/api/" covers a tenant's API and
	// "/v1/" covers aigw's data plane. Empty means the built-in default set.
	APIPaths []string `yaml:"api_paths"`
	// NoStoreAPIs forces "do not store" on responses for those paths. It defaults to
	// true: an API answer that a cache may reuse is a wrong answer waiting to be
	// served, and every caller here is authenticated per request anyway.
	NoStoreAPIs *bool `yaml:"no_store_apis"`
	// RegistryPath is dshgw's registry.json, read to learn each tenant's public
	// port. Reading the same file the gateway reads is what stops the proxy and
	// the gateway from disagreeing about a tenant's identity. The default is the
	// supervised shape's registry inside the single data root; relative paths are
	// resolved against the process working directory (the deployment root).
	RegistryPath string `yaml:"registry_path"`
	// RegistryReload is how often the registry is re-read. Zero means 2s.
	RegistryReload Seconds `yaml:"registry_reload"`
	// UpstreamTimeout bounds one upstream round trip; zero means 5 minutes, which
	// covers long model streams while still releasing abandoned connections.
	UpstreamTimeout Seconds `yaml:"upstream_timeout"`
	// MaxBodyBytes bounds a proxied request body when the upstream has no opinion.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// RewriteTenantHTML fixes the handful of root-absolute references the dsh shell
	// emits (`/plugins/??…`, `href="/"`). dsh has no base-path option, so a path
	// prefix needs this narrow rewrite; see docs/design/m58 §8.
	RewriteTenantHTML *bool `yaml:"rewrite_tenant_html"`
}

// TLS is the certificate the proxy serves. Both fields or neither.
type TLS struct {
	Certificate    string `yaml:"certificate"`
	CertificateKey string `yaml:"certificate_key"`
}

// Seconds is a YAML duration in seconds or a Go duration string.
type Seconds float64

func (s *Seconds) UnmarshalYAML(node *yaml.Node) error {
	var raw any
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch value := raw.(type) {
	case int:
		*s = Seconds(value)
	case float64:
		*s = Seconds(value)
	case string:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", value, err)
		}
		*s = Seconds(parsed.Seconds())
	default:
		return fmt.Errorf("duration must be a number of seconds or a Go duration string")
	}
	return nil
}

// Duration converts to a time.Duration.
func (s Seconds) Duration() time.Duration { return time.Duration(float64(time.Second) * float64(s)) }

// Load reads and validates the proxy configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("front proxy configuration: %w", err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.AigwPrefix) == "" {
		c.AigwPrefix = "/aigw"
	}
	if strings.TrimSpace(c.PortalPrefix) == "" {
		c.PortalPrefix = "/dshgw"
	}
	if strings.TrimSpace(c.TenantPrefix) == "" {
		c.TenantPrefix = "/t"
	}
	// The default points into the single data root (M63): the supervised dshgw writes
	// its registry to <data>/dshgw/registry.json, so an unconfigured proxy in the same
	// deployment reads it without either side naming a machine path.
	if strings.TrimSpace(c.RegistryPath) == "" {
		c.RegistryPath = "./data/dshgw/registry.json"
	}
	if c.RegistryReload <= 0 {
		c.RegistryReload = 2
	}
	if len(c.APIPaths) == 0 {
		// The API surfaces of the services this proxy fronts, plus the status
		// endpoints: aigw sends no cache headers for /version, /healthz or /readyz, and
		// a response with no headers may still be cached heuristically by a shared
		// cache — a stale health check is exactly the thing that hides an outage.
		c.APIPaths = []string{"/api/", "/v1/", "/admin/api/", "/version", "/healthz", "/readyz"}
	}
	if c.UpstreamTimeout <= 0 {
		c.UpstreamTimeout = 300
	}
}

// Validate rejects configurations whose routes would be ambiguous or whose
// prefixes could not be safely matched.
func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	if strings.TrimSpace(c.PublicHost) == "" || strings.ContainsAny(c.PublicHost, "/: ") {
		return errors.New("public_host must be one hostname without scheme, port or spaces")
	}
	if (c.TLS.Certificate == "") != (c.TLS.CertificateKey == "") {
		return errors.New("tls.certificate and tls.certificate_key must be set together")
	}
	for label, value := range map[string]string{"tls.certificate": c.TLS.Certificate, "tls.certificate_key": c.TLS.CertificateKey} {
		if value != "" && !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be an absolute path", label)
		}
	}
	prefixes := map[string]string{}
	for label, prefix := range map[string]string{
		"aigw_prefix": c.AigwPrefix, "portal_prefix": c.PortalPrefix, "tenant_prefix": c.TenantPrefix,
	} {
		// "/" is the fallback route, not a prefix that can overlap: it claims
		// whatever the two specific prefixes do not.
		if path.Clean("/"+strings.Trim(prefix, "/")) == "/" {
			if label != "aigw_prefix" {
				return fmt.Errorf("%s must not be the root path", label)
			}
			continue
		}
		clean := path.Clean("/" + strings.Trim(prefix, "/"))
		if clean == "/" {
			return fmt.Errorf("%s must not be the root path", label)
		}
		for existing, owner := range prefixes {
			if existing == clean || strings.HasPrefix(existing, clean+"/") || strings.HasPrefix(clean, existing+"/") {
				return fmt.Errorf("%s (%s) overlaps %s (%s)", label, clean, owner, existing)
			}
		}
		prefixes[clean] = label
	}
	for label, raw := range map[string]string{"aigw_upstream": c.AigwUpstream, "portal_upstream": c.PortalUpstream, "tenant_upstream": c.TenantUpstream} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" && parsed.Path != "/" {
			return fmt.Errorf("%s must be an http:// URL without a path", label)
		}
	}
	if c.PortalPort < 1 || c.PortalPort > 65535 {
		return errors.New("portal_port must be between 1 and 65535")
	}
	if !filepath.IsAbs(c.RegistryPath) {
		// A relative registry_path is configuration, not an oversight: resolve it against
		// the deployment root (the process working directory) so ./data/dshgw/registry.json
		// keeps working while the checks below still see one canonical form. ".." is
		// refused: a relative path may point deeper into the deployment, not out of it.
		for _, elem := range strings.Split(filepath.ToSlash(c.RegistryPath), "/") {
			if elem == ".." {
				return fmt.Errorf("registry_path %q must not contain \"..\"; use an absolute path to leave the deployment root", c.RegistryPath)
			}
		}
		abs, err := filepath.Abs(c.RegistryPath)
		if err != nil {
			return fmt.Errorf("registry_path %q is not usable: %w", c.RegistryPath, err)
		}
		c.RegistryPath = abs
	}
	if filepath.Clean(c.RegistryPath) != c.RegistryPath {
		return fmt.Errorf("registry_path must be a clean path (got %q)", c.RegistryPath)
	}
	for _, prefix := range c.APIPaths {
		if !strings.HasPrefix(prefix, "/") || prefix == "/" {
			return fmt.Errorf("api_paths entries must be absolute paths and must not be \"/\" (got %q)", prefix)
		}
	}
	return nil
}

// StoreAPIData reports whether responses for APIPaths may be stored by a cache.
// False is the default and means every API answer leaves with "no-store".
func (c *Config) StoreAPIData() bool { return c.NoStoreAPIs != nil && !*c.NoStoreAPIs }

// IsAPIPath reports whether a request path (with any route prefix already stripped,
// or the public path as a fallback) is one of the API surfaces whose data must not
// be cached.
func (c *Config) IsAPIPath(paths ...string) bool {
	for _, path := range paths {
		if path == "" {
			continue
		}
		for _, prefix := range c.APIPaths {
			if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix) {
				return true
			}
		}
	}
	return false
}

// RewriteHTML reports whether tenant HTML should be prefix-rewritten.
func (c *Config) RewriteHTML() bool {
	return c.RewriteTenantHTML == nil || *c.RewriteTenantHTML
}

// NormalizedPrefixes returns the three prefixes in canonical form (leading slash,
// no trailing slash).
func (c *Config) NormalizedPrefixes() (aigw, portal, tenants string) {
	trim := func(value string) string { return path.Clean("/" + strings.Trim(value, "/")) }
	return trim(c.AigwPrefix), trim(c.PortalPrefix), trim(c.TenantPrefix)
}

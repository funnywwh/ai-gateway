// Package dshgwsup supervises the dshgw child process that aigw owns (M58).
//
// Shape: aigw is the long-lived program; dshgw is a sibling executable that aigw
// starts as its own child with the same UID — no root, no systemd unit, no
// separate service account — and stops when aigw exits. This package deliberately
// knows nothing about dshgw's internals: it locates the binary, writes the child's
// configuration, starts it, watches a readiness line, forwards its output, and
// stops it. aigw never links dshgw code (see internal/arch's layering table).
package dshgwsup

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// publicHostRE mirrors the child's own validation: a DNS name without scheme,
// port or path. Checking it here makes a typo in aigw's config fail at aigw
// startup instead of looping the child through a config error.
var publicHostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// DshRuntime points the child at the dsh release it runs tenant workers from.
type DshRuntime struct {
	NodeBin     string `yaml:"node_bin"`
	BinJS       string `yaml:"bin_js"`
	CurrentLink string `yaml:"current_link"`
}

// Deploy is the deployment subset aigw must override: the child's own defaults
// point at a root-owned installation (/etc/dshgw, /var/lib/dshgw, /opt/dshgw),
// which is exactly what a supervised child must not use.
//
// There is no isolation field: bubblewrap is the only mode this shape supports,
// so it is not a setting an operator could get wrong.
type Deploy struct {
	WorkerUser       string `yaml:"worker_user"`
	PluginPath       string `yaml:"plugin_path,omitempty"`
	TemplateHome     string `yaml:"template_home"`
	TenantConfigRoot string `yaml:"tenant_config_root"`
	ConfigPath       string `yaml:"config_path"`
	BackupDir        string `yaml:"backup_dir"`
	// NoStoreAPIs mirrors the child's own switch: false means API responses may be
	// stored by a cache. Unset keeps the child's default (never store).
	NoStoreAPIs *bool `yaml:"no_store_apis,omitempty"`
	// PublicBaseURL switches the child to single-domain path mode: every public URL
	// it generates is built from this base plus a path prefix. Empty keeps the
	// port-based behaviour. It exists because subdomains are not always available.
	PublicBaseURL    string `yaml:"public_base_url,omitempty"`
	TenantPathPrefix string `yaml:"tenant_path_prefix,omitempty"`
	PortalPathPrefix string `yaml:"portal_path_prefix,omitempty"`
	// PublicListen is where the child binds the portal and tenant public ports.
	PublicListen string `yaml:"public_listen,omitempty"`
}

// ChildConfig is the configuration aigw writes for its dshgw child. Only the
// fields a rootless child must not inherit from dshgw's own defaults are listed:
// every other path is derived by the child from state_dir, so there is one place
// (this struct) where the two programs agree about the layout.
type ChildConfig struct {
	PublicHost   string `yaml:"public_host"`
	PortalPort   int    `yaml:"portal_port"`
	TenantPortLo int    `yaml:"tenant_port_lo"`
	TenantPortHi int    `yaml:"tenant_port_hi"`
	WorkerPortLo int    `yaml:"worker_port_lo"`
	WorkerPortHi int    `yaml:"worker_port_hi"`
	// NoStoreAPIs mirrors the child's own switch: false means API responses may be
	// stored by a cache. Unset keeps the child's default (never store).
	NoStoreAPIs *bool `yaml:"no_store_apis,omitempty"`
	// PublicBaseURL switches the child to single-domain path mode: the public URLs
	// it generates become "https://host/t/<tenant>/" and "https://host/dshgw/"
	// instead of host:port origins, and session cookies are scoped to the tenant's
	// path. Empty keeps port mode. It exists because subdomains are not always
	// available: one domain and no wildcard DNS leaves paths as the only option.
	PublicBaseURL    string `yaml:"public_base_url,omitempty"`
	TenantPathPrefix string `yaml:"tenant_path_prefix,omitempty"`
	PortalPathPrefix string `yaml:"portal_path_prefix,omitempty"`
	// Listen is the child's own loopback listener (the edge proxies tenant ports
	// onto it). It must not overlap any of the three port ranges.
	Listen      string `yaml:"listen"`
	AigwBaseURL string `yaml:"aigw_base_url"`
	// PublicScheme is how browsers actually reach the child's public surface:
	// "auto" (the child's own default) presumes https in port mode, which is wrong
	// on a plain-HTTP deployment. It matters beyond cosmetics — the child decides
	// the session cookie's Secure attribute from it, and a browser silently drops a
	// Secure cookie on an http origin, so a wrong value looks exactly like "login
	// succeeded and then bounced me back to the portal". The child has always
	// supported this key; the supervised shape simply never passed it through.
	PublicScheme string `yaml:"public_scheme,omitempty"`
	// AdminSocket is the local provisioning socket the console's DSH buttons use.
	// In this shape it is owned by the same UID as aigw, not by root.
	AdminSocket string `yaml:"admin_socket"`
	// PluginBrowserFS is the child's per-deployment default for the browser
	// filesystem plugin. Empty keeps the child's own default (on), which requires
	// a template prepared with the pinned plugin.
	PluginBrowserFS string     `yaml:"plugin_browser_fs,omitempty"`
	StateDir        string     `yaml:"state_dir"`
	TenantRoot      string     `yaml:"tenant_root,omitempty"`
	WorkspaceRoot   string     `yaml:"workspace_root"`
	Dsh             DshRuntime `yaml:"dsh"`
	// TLS is optional: set both paths to serve HTTPS on the edge listeners, leave
	// them empty for plain HTTP on a trusted network.
	TLS *TLSConfig `yaml:"tls,omitempty"`
	// WorkerLimits are the per-worker cgroup v2 limits; all zero means unlimited.
	WorkerLimits WorkerLimits `yaml:"worker_limits,omitempty"`
	// Feishu is the identity handoff the child needs to accept a Feishu sign-in (M61): where
	// to send a person, and the key that proves the ticket aigw signs is genuine. It is
	// omitted entirely while the feature is off, so a deployment that does not use Feishu
	// generates exactly the configuration it did before.
	Feishu *Feishu `yaml:"feishu,omitempty"`
	Deploy Deploy  `yaml:"deploy"`
}

// Feishu mirrors the child's own feishu block. The child holds no Feishu credential: aigw
// owns the application and the secret, and this secret only authenticates the handoff
// between the two processes (which is why it travels in this generated file rather than
// being something an operator has to keep in sync by hand).
type Feishu struct {
	Enabled      bool   `yaml:"enabled"`
	AigwLoginURL string `yaml:"aigw_login_url"`
	TicketSecret string `yaml:"ticket_secret"`
}

// WorkerLimits mirrors the child's own per-worker limits.
type WorkerLimits struct {
	MemoryHighBytes int64 `yaml:"memory_high_bytes,omitempty"`
	MemoryMaxBytes  int64 `yaml:"memory_max_bytes,omitempty"`
	TasksMax        int   `yaml:"tasks_max,omitempty"`
	CPUQuotaPercent int   `yaml:"cpu_quota_percent,omitempty"`
}

// TLSConfig mirrors the child's own certificate settings: in this shape dshgw
// terminates TLS itself, because there is no nginx in front of it.
type TLSConfig struct {
	Certificate    string `yaml:"certificate"`
	CertificateKey string `yaml:"certificate_key"`
}

// Validate rejects inputs the child could only reject later, in a restart loop,
// with less context than aigw has here.
func (c ChildConfig) Validate() error {
	if !publicHostRE.MatchString(c.PublicHost) {
		return fmt.Errorf("dshgw.public_host %q must be a lowercase DNS name without scheme, port or path", c.PublicHost)
	}
	if c.PublicScheme != "" && c.PublicScheme != "auto" && c.PublicScheme != "http" && c.PublicScheme != "https" {
		return fmt.Errorf("dshgw.public_scheme %q must be auto, http or https", c.PublicScheme)
	}
	if c.Feishu != nil && c.Feishu.Enabled {
		if !strings.HasSuffix(strings.TrimSpace(c.Feishu.AigwLoginURL), "/feishu/login") {
			return fmt.Errorf("dshgw.feishu.aigw_login_url must end with /feishu/login (got %q)", c.Feishu.AigwLoginURL)
		}
		if strings.TrimSpace(c.Feishu.TicketSecret) == "" {
			return errors.New("dshgw.feishu.ticket_secret must be set when the child serves Feishu login")
		}
	}
	if c.PublicScheme == "https" && c.PublicBaseURL != "" && strings.HasPrefix(c.PublicBaseURL, "http://") {
		// The base URL is what every generated link is built from while the scheme
		// decides the cookie's Secure attribute: a deployment cannot be both.
		return fmt.Errorf("dshgw.public_scheme https contradicts public_base_url %q", c.PublicBaseURL)
	}
	if c.PublicBaseURL != "" {
		parsed, err := url.Parse(c.PublicBaseURL)
		if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil {
			return errors.New("dshgw.public_base_url must be a scheme://host URL without a path")
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return fmt.Errorf("dshgw.public_base_url scheme %q must be http or https", parsed.Scheme)
		}
		if !strings.EqualFold(parsed.Hostname(), c.PublicHost) {
			// The Host fence compares a request's host against public_host while the
			// generated links use this base: if they disagree, every link the gateway
			// produces is one it then rejects.
			return fmt.Errorf("dshgw.public_base_url host %q must match public_host %q", parsed.Hostname(), c.PublicHost)
		}
		for label, prefix := range map[string]string{"tenant_path_prefix": c.TenantPathPrefix, "portal_path_prefix": c.PortalPathPrefix} {
			if prefix == "" {
				continue
			}
			if !strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") || strings.ContainsAny(prefix, "?# ") {
				return fmt.Errorf("dshgw.%s %q must start with / and carry no trailing slash", label, prefix)
			}
		}
	}
	ports := []struct {
		label string
		value int
	}{
		{"portal_port", c.PortalPort},
		{"tenant_port_lo", c.TenantPortLo},
		{"tenant_port_hi", c.TenantPortHi},
		{"worker_port_lo", c.WorkerPortLo},
		{"worker_port_hi", c.WorkerPortHi},
	}
	for _, p := range ports {
		if p.value < 1 || p.value > 65535 {
			return fmt.Errorf("dshgw.%s must be between 1 and 65535 (got %d)", p.label, p.value)
		}
	}
	if c.TenantPortLo > c.TenantPortHi || c.WorkerPortLo > c.WorkerPortHi {
		return fmt.Errorf("dshgw port ranges are inverted (tenant %d-%d, worker %d-%d)", c.TenantPortLo, c.TenantPortHi, c.WorkerPortLo, c.WorkerPortHi)
	}
	if c.PortalPort >= c.TenantPortLo && c.PortalPort <= c.TenantPortHi {
		return fmt.Errorf("dshgw.portal_port %d overlaps the tenant port range %d-%d", c.PortalPort, c.TenantPortLo, c.TenantPortHi)
	}
	if c.PortalPort >= c.WorkerPortLo && c.PortalPort <= c.WorkerPortHi {
		return fmt.Errorf("dshgw.portal_port %d overlaps the worker port range %d-%d", c.PortalPort, c.WorkerPortLo, c.WorkerPortHi)
	}
	if c.TenantPortLo <= c.WorkerPortHi && c.WorkerPortLo <= c.TenantPortHi {
		return fmt.Errorf("dshgw tenant and worker port ranges must not overlap")
	}
	host, rawPort, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("dshgw.listen must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("dshgw.listen %q must use a loopback address: the child is reached by the local edge, never directly", c.Listen)
	}
	listenPort, err := strconv.Atoi(rawPort)
	if err != nil || listenPort < 1 || listenPort > 65535 {
		return fmt.Errorf("dshgw.listen %q must contain a numeric port", c.Listen)
	}
	if listenPort == c.PortalPort || listenPort >= c.TenantPortLo && listenPort <= c.TenantPortHi || listenPort >= c.WorkerPortLo && listenPort <= c.WorkerPortHi {
		return fmt.Errorf("dshgw.listen port %d overlaps the portal, tenant or worker port range", listenPort)
	}
	for label, value := range map[string]string{
		"dshgw.state_dir":                 c.StateDir,
		"dshgw.workspace_root":            c.WorkspaceRoot,
		"dshgw.admin_socket":              c.AdminSocket,
		"dshgw.dsh.node_bin":              c.Dsh.NodeBin,
		"dshgw.dsh.bin_js":                c.Dsh.BinJS,
		"dshgw.dsh.current_link":          c.Dsh.CurrentLink,
		"dshgw.deploy.tenant_config_root": c.Deploy.TenantConfigRoot,
		"dshgw.deploy.template_home":      c.Deploy.TemplateHome,
		"dshgw.deploy.config_path":        c.Deploy.ConfigPath,
		"dshgw.deploy.backup_dir":         c.Deploy.BackupDir,
	} {
		if err := checkAbsolute(label, value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Deploy.WorkerUser) == "" {
		return fmt.Errorf("dshgw.deploy.worker_user must name the account the child runs workers as (aigw's own account in the supervised shape)")
	}
	if c.Deploy.WorkerUser == "root" {
		return fmt.Errorf("dshgw.deploy.worker_user must be unprivileged: a root worker could build a wider namespace instead of living inside the tenant profile")
	}
	if c.AigwBaseURL == "" {
		return fmt.Errorf("dshgw.aigw_base_url must not be empty")
	}
	return nil
}

// Render returns the child's YAML configuration.
func (c ChildConfig) Render() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	header := "# Generated by aigw for its dshgw child (M58). Do not edit: aigw rewrites it\n" +
		"# from its own configuration at every start, and the child is not meant to run\n" +
		"# standalone in this shape.\n"
	return append([]byte(header), data...), nil
}

// WriteIfChanged writes the rendered configuration with owner-only permissions and
// reports whether the file changed. Rewriting an unchanged file every start would
// be harmless but noisy: the child watches nothing, yet mtimes are how operators
// notice real configuration changes.
func (c ChildConfig) WriteIfChanged(path string) (bool, error) {
	data, err := c.Render()
	if err != nil {
		return false, err
	}
	if current, readErr := os.ReadFile(path); readErr == nil && string(current) == string(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	// The file names the local socket and every state path; it stays 0600 because
	// the same UID that owns it also runs tenant workers.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return false, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func checkAbsolute(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", label)
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s %q must be an absolute path", label, value)
	}
	if filepath.Clean(value) != value {
		return fmt.Errorf("%s %q must be a clean absolute path", label, value)
	}
	if strings.ContainsAny(value, "\x00\n\r") {
		return fmt.Errorf("%s contains an unsafe character", label)
	}
	return nil
}

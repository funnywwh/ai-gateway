// Package config loads and validates dshgw's standalone configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "/etc/dshgw/config.yaml"

var tenantNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,25}[a-z0-9]$|^[a-z]$`)
var edgeHeaderRE = regexp.MustCompile(`(?i)^x-[a-z0-9]+(?:-[a-z0-9]+)*$`)
var accountNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
var publicHostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Duration is a YAML duration such as 5s or 168h.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration   { return time.Duration(d) }
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// RateLimit controls portal login attempts per source address.
type RateLimit struct {
	Requests int      `yaml:"requests" json:"requests"`
	Window   Duration `yaml:"window" json:"window"`
}

// DshRuntime describes an unpacked dsh release. BinJS is the public launcher
// contract; CurrentLink is switched by upgrade-dsh.
type DshRuntime struct {
	NodeBin     string `yaml:"node_bin" json:"node_bin"`
	BinJS       string `yaml:"bin_js" json:"bin_js"`
	CurrentLink string `yaml:"current_link" json:"current_link"`
}

// PublicBaseURL switches the deployment to single-domain path mode: every public
// URL dshgw generates (portal redirects, tenant redirects, CSP form-action) is
// built from this base plus a path prefix instead of from a host:port origin.
//
// It exists because subdomains are not always available: with one domain and no
// wildcard DNS, the only way to give each tenant its own URL space is a path.
// Empty keeps the port-based behaviour.
type PublicBaseURL string

// WorkerLimits are the per-worker resources, expressed the way cgroup v2 wants
// them. Zero means "no limit". They replace the old unit's MemoryHigh/MemoryMax/
// CPUQuota/TasksMax, which systemd enforced for us.
type WorkerLimits struct {
	MemoryHighBytes int64 `yaml:"memory_high_bytes" json:"memory_high_bytes"`
	MemoryMaxBytes  int64 `yaml:"memory_max_bytes" json:"memory_max_bytes"`
	TasksMax        int   `yaml:"tasks_max" json:"tasks_max"`
	CPUQuotaPercent int   `yaml:"cpu_quota_percent" json:"cpu_quota_percent"`
}

// TLSConfig is the certificate the edge serves directly. Empty values mean plain
// HTTP, which is only appropriate on a trusted network: there is no nginx in this
// shape to terminate TLS for us.
type TLSConfig struct {
	Certificate    string `yaml:"certificate" json:"certificate"`
	CertificateKey string `yaml:"certificate_key" json:"certificate_key"`
}

// DeployConfig holds the paths and identities a dshgw instance needs. Everything
// that described the deleted systemd/nginx/root shape (units, slices, nginx
// directories, per-tenant account prefixes, TLS for an external edge) is gone:
// dshgw now runs as aigw's child, starts tenant workers as its own processes, and
// owns no host-side service configuration.
type DeployConfig struct {
	PluginPath       string `yaml:"plugin_path" json:"plugin_path"`
	TemplateHome     string `yaml:"template_home" json:"template_home"`
	BackupDir        string `yaml:"backup_dir" json:"backup_dir"`
	TenantConfigRoot string `yaml:"tenant_config_root" json:"tenant_config_root"`
	ConfigPath       string `yaml:"config_path" json:"config_path"`
	// GatewayUser is the account the gateway runs as. It is the default for
	// WorkerUser so a deployment does not need a second service account.
	GatewayUser string `yaml:"gateway_user" json:"gateway_user"`
	// WorkerUser is the unprivileged account every tenant worker runs as. It must
	// not be root: a root caller could build a wider namespace instead of living
	// inside the tenant's profile.
	WorkerUser string `yaml:"worker_user" json:"worker_user"`
	// BwrapBin is the bubblewrap executable the sandbox profile and doctor use.
	BwrapBin string `yaml:"bwrap_bin" json:"bwrap_bin"`
	// PublicListen is the address the edge binds the portal and tenant public
	// ports on. Loopback is the safe default; exposing tenants to a network is an
	// explicit decision (and needs TLS, see tls:).
	PublicListen string `yaml:"public_listen" json:"public_listen"`
}

// Config is deliberately independent of aigw's internal configuration types.
type Config struct {
	PublicHost      string   `yaml:"public_host" json:"public_host"`
	PortalPort      int      `yaml:"portal_port" json:"portal_port"`
	TenantPortLo    int      `yaml:"tenant_port_lo" json:"tenant_port_lo"`
	TenantPortHi    int      `yaml:"tenant_port_hi" json:"tenant_port_hi"`
	WorkerPortLo    int      `yaml:"worker_port_lo" json:"worker_port_lo"`
	WorkerPortHi    int      `yaml:"worker_port_hi" json:"worker_port_hi"`
	Listen          string   `yaml:"listen" json:"listen"`
	EdgePortHeader  string   `yaml:"edge_port_header" json:"edge_port_header"`
	MaxHeaderBytes  int      `yaml:"max_header_bytes" json:"max_header_bytes"`
	MaxSessions     int      `yaml:"max_sessions" json:"max_sessions"`
	AigwBaseURL     string   `yaml:"aigw_base_url" json:"aigw_base_url"`
	ValidateTimeout Duration `yaml:"validate_timeout" json:"validate_timeout"`
	SessionTTL      Duration `yaml:"session_ttl" json:"session_ttl"`
	KeyRevalidate   string   `yaml:"key_revalidate" json:"key_revalidate"`
	DSHEnforce      string   `yaml:"dsh_enforce" json:"dsh_enforce"`
	// AdminSocket is the UNIX socket the root admin-serve listens on (M52 provisioning
	// channel). Empty disables the command: the daemon never starts by accident.
	AdminSocket string `yaml:"admin_socket" json:"admin_socket"`
	// AdminAllowedUIDs are the peer UIDs (typically the aigw runtime user) that may talk
	// to the admin socket. UID 0 is always allowed on the local machine.
	AdminAllowedUIDs []int      `yaml:"admin_allowed_uids" json:"admin_allowed_uids"`
	LoginRate        RateLimit  `yaml:"login_rate" json:"login_rate"`
	DirectoryPicker  string     `yaml:"directory_picker" json:"directory_picker"`
	PluginBrowserFS  string     `yaml:"plugin_browser_fs" json:"plugin_browser_fs"`
	WorkspaceSeed    []string   `yaml:"workspace_seed" json:"workspace_seed"`
	ReservedNames    []string   `yaml:"reserved_names" json:"reserved_names"`
	Dsh              DshRuntime `yaml:"dsh" json:"dsh"`
	// PublicBaseURL/scheme+host of the single public entry (e.g.
	// "https://chat.example"). Empty means tenants are addressed by port.
	PublicBaseURL    string `yaml:"public_base_url" json:"public_base_url"`
	TenantPathPrefix string `yaml:"tenant_path_prefix" json:"tenant_path_prefix"`
	PortalPathPrefix string `yaml:"portal_path_prefix" json:"portal_path_prefix"`
	// PublicScheme is how browsers actually reach the public surface: "auto"
	// (default) derives it from public_base_url in path mode and presumes https in
	// port mode, while "http"/"https" state it explicitly.
	//
	// Port mode used to hardcode https in every URL it generated. On a plain-HTTP
	// deployment that turns every redirect into a request to a port nobody serves,
	// which looks exactly like "the button does nothing".
	PublicScheme string `yaml:"public_scheme" json:"public_scheme"`
	// NoStoreAPIs forces "do not store" on every API response dshgw returns. It
	// defaults to true: dsh's API answers carry no cache directives at all, so a
	// shared cache in front (an nginx, a corporate proxy) or a browser is free to
	// reuse them — and every one of those answers belongs to one authenticated
	// tenant, which makes a reused answer a wrong answer.
	NoStoreAPIs *bool `yaml:"no_store_apis" json:"no_store_apis"`
	// SettingsUI decides whether a tenant's dsh settings/models panel is usable from
	// a page the browser does not consider loopback.
	//
	// dsh gates that panel on `transport?.ownsHost === true || isLoopback(page)`:
	// on any non-loopback page the settings scope stays "unavailable", the mirror
	// never loads, and the panel reports "settings are unavailable in this browser".
	// dshgw serves the tenant UI, so it can declare the transport it fronts — the
	// same patch the pre-existing deployment applied in its nginx config.
	//
	// "lan" (default) makes the panel work on the public host; "loopback" keeps
	// dsh's own behaviour. The confused-deputy cases the gate defends against
	// (DNS rebinding, cross-site requests) are already handled in front of it:
	// dshgw refuses any Host other than public_host and any unsafe request whose
	// Origin is not the tenant's own.
	SettingsUI string `yaml:"settings_ui" json:"settings_ui"`
	// SessionCookieSecure controls the session cookie's Secure attribute:
	// "auto" (default) sets it whenever the deployment is actually HTTPS, "always"
	// and "never" override that.
	//
	// It exists because a Secure cookie is *rejected* by browsers on a plain-HTTP
	// origin (localhost excepted): a deployment served over http:// would issue a
	// session the browser silently throws away, and the user would land back on the
	// portal after a successful login instead of inside dsh.
	SessionCookieSecure string       `yaml:"session_cookie_secure" json:"session_cookie_secure"`
	WorkerLimits        WorkerLimits `yaml:"worker_limits" json:"worker_limits"`
	TLS                 TLSConfig    `yaml:"tls" json:"tls"`
	Deploy              DeployConfig `yaml:"deploy" json:"deploy"`
	TenantRoot          string       `yaml:"tenant_root" json:"tenant_root"`
	WorkspaceRoot       string       `yaml:"workspace_root" json:"workspace_root"`
	HandshakeDir        string       `yaml:"handshake_dir" json:"handshake_dir"`
	StateDir            string       `yaml:"state_dir" json:"state_dir"`
	RegistryPath        string       `yaml:"registry_path" json:"registry_path"`
	KeyMapPath          string       `yaml:"key_map_path" json:"key_map_path"`
	SessionPath         string       `yaml:"session_path" json:"session_path"`
	AuditPath           string       `yaml:"audit_path" json:"audit_path"`
	ActivityPath        string       `yaml:"activity_path" json:"activity_path"`

	tenantMu    sync.RWMutex
	tenantPorts map[string]int
	portTenants map[int]string
}

func defaults() Config {
	return Config{
		PublicHost:          "chat.tirisen.hk",
		PortalPort:          32600,
		TenantPortLo:        32601,
		TenantPortHi:        32799,
		WorkerPortLo:        32100,
		WorkerPortHi:        32299,
		Listen:              "127.0.0.1:3099",
		EdgePortHeader:      "X-DSHGW-Port",
		MaxHeaderBytes:      128 << 10,
		MaxSessions:         10000,
		AigwBaseURL:         "http://192.168.190.86:8088",
		ValidateTimeout:     Duration(5 * time.Second),
		SessionTTL:          Duration(7 * 24 * time.Hour),
		KeyRevalidate:       "off",
		SessionCookieSecure: "auto",
		SettingsUI:          "lan",
		PublicScheme:        "auto",
		DSHEnforce:          "login",
		LoginRate:           RateLimit{Requests: 10, Window: Duration(time.Minute)},
		DirectoryPicker:     "clamp",
		PluginBrowserFS:     "on",
		WorkspaceSeed:       []string{"work"},
		ReservedNames:       []string{"login", "dshgw"},
		Dsh: DshRuntime{
			NodeBin:     "/opt/dsh/node/bin/node",
			BinJS:       "/opt/dsh/current/lib/bin.js",
			CurrentLink: "/opt/dsh/current",
		},
		WorkspaceRoot: "/srv/dsh",
		StateDir:      "/var/lib/dshgw",
		Deploy: DeployConfig{
			PluginPath:   "/opt/dshgw/share/dsh-plugin/picker-clamp.js",
			TemplateHome: "/opt/dshgw/share/template-home",
			BackupDir:    "/home/winger/backups/dshgw",
			ConfigPath:   DefaultPath,
			GatewayUser:  "dshgw",
			BwrapBin:     "/usr/bin/bwrap",
			PublicListen: "127.0.0.1",
		},
	}
}

// Load strictly decodes one YAML document, applies safe defaults, and validates it.
func Load(path string) (*Config, error) {
	f, err := openConfig(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("dshgw config %s exceeds 1 MiB", path)
	}

	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("dshgw config %s: multiple YAML documents are not allowed", path)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("dshgw config %s: trailing YAML: %w", path, err)
	}
	cfg.applyDerivedDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("dshgw config %s: %w", path, err)
	}
	return &cfg, nil
}

// osOpen is split out only to keep the strict decoder easy to unit-test through Load.
var openConfig = func(path string) (file, error) { return openOSFile(path) }

type file interface {
	Read([]byte) (int, error)
	Close() error
}

func (c *Config) applyDerivedDefaults() {
	if c.PublicBaseURL != "" {
		c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
		if c.TenantPathPrefix == "" {
			c.TenantPathPrefix = "/t"
		}
		if c.PortalPathPrefix == "" {
			c.PortalPathPrefix = "/dshgw"
		}
	}
	// The bwrap mode's shared worker account is optional in configuration: the
	// documented practice is to reuse the gateway account rather than create a
	// second service account, and the installed worker unit names that account
	// literally. Deriving it here keeps one source of truth for the identity.
	if c.Deploy.WorkerUser == "" {
		c.Deploy.WorkerUser = c.Deploy.GatewayUser
	}
	// The per-tenant configuration (today: the gateway's copy of the tenant key)
	// lives under the state directory. It cannot stay at /etc/dshgw/tenants: that
	// path is root-owned in a host installation, and this shape runs as an
	// ordinary account. It is still a separate directory from the tenant's own
	// .dsh so the sandbox never mounts it (see internal/dshgw/sandbox).
	if c.Deploy.TenantConfigRoot == "" {
		c.Deploy.TenantConfigRoot = filepath.Join(c.StateDir, "tenants")
	}
	if c.RegistryPath == "" {
		c.RegistryPath = filepath.Join(c.StateDir, "registry.json")
	}
	if c.KeyMapPath == "" {
		c.KeyMapPath = filepath.Join(c.StateDir, "keys.map")
	}
	if c.SessionPath == "" {
		c.SessionPath = filepath.Join(c.StateDir, "gateway", "sessions.json")
	}
	if c.AuditPath == "" {
		c.AuditPath = filepath.Join(c.StateDir, "gateway", "audit.jsonl")
	}
	if c.ActivityPath == "" {
		c.ActivityPath = filepath.Join(c.StateDir, "gateway", "activity.json")
	}
	if c.HandshakeDir == "" {
		c.HandshakeDir = filepath.Join(c.StateDir, "handshake")
	}
	if c.TenantRoot == "" {
		c.TenantRoot = filepath.Join(c.StateDir, "tenants")
	}
}

// Validate rejects ambiguous, externally exposed, or overlapping configurations.
func (c *Config) Validate() error {
	if len(c.PublicHost) > 253 || !publicHostRE.MatchString(c.PublicHost) {
		return errors.New("public_host must be one lowercase DNS hostname without a scheme, port, or path")
	}
	for label, p := range map[string]int{"portal_port": c.PortalPort, "tenant_port_lo": c.TenantPortLo, "tenant_port_hi": c.TenantPortHi, "worker_port_lo": c.WorkerPortLo, "worker_port_hi": c.WorkerPortHi} {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%s must be between 1 and 65535", label)
		}
	}
	if c.TenantPortLo > c.TenantPortHi || c.WorkerPortLo > c.WorkerPortHi {
		return errors.New("port range low bound exceeds high bound")
	}
	if c.PortalPort >= c.TenantPortLo && c.PortalPort <= c.TenantPortHi {
		return errors.New("portal_port overlaps the tenant public port range")
	}
	if rangesOverlap(c.TenantPortLo, c.TenantPortHi, c.WorkerPortLo, c.WorkerPortHi) || c.PortalPort >= c.WorkerPortLo && c.PortalPort <= c.WorkerPortHi {
		return errors.New("public and worker port ranges must not overlap")
	}
	host, rawListenPort, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen must use a loopback IP address")
	}
	listenPort, err := strconv.Atoi(rawListenPort)
	if err != nil || listenPort < 1 || listenPort > 65535 || strconv.Itoa(listenPort) != rawListenPort {
		return errors.New("listen must contain a numeric port between 1 and 65535")
	}
	if listenPort == c.PortalPort || listenPort >= c.TenantPortLo && listenPort <= c.TenantPortHi || listenPort >= c.WorkerPortLo && listenPort <= c.WorkerPortHi {
		return errors.New("listen port overlaps a public or worker port")
	}
	header := strings.ToLower(c.EdgePortHeader)
	if !edgeHeaderRE.MatchString(header) || strings.HasPrefix(header, "x-forwarded-") || header == "x-real-ip" || header == "x-api-key" {
		return errors.New("edge_port_header must be a distinct custom X- header using letters, digits and hyphens")
	}
	if c.MaxHeaderBytes < 8192 || c.MaxHeaderBytes > 1<<20 {
		return errors.New("max_header_bytes must be between 8192 and 1048576")
	}
	if c.MaxSessions < 1 || c.MaxSessions > 1_000_000 {
		return errors.New("max_sessions must be between 1 and 1000000")
	}
	u, err := url.Parse(c.AigwBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("aigw_base_url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	c.AigwBaseURL = strings.TrimRight(c.AigwBaseURL, "/")
	if c.ValidateTimeout.Duration() <= 0 || c.SessionTTL.Duration() <= 0 {
		return errors.New("validate_timeout and session_ttl must be positive")
	}
	if _, err := ParseRevalidate(c.KeyRevalidate); err != nil {
		return err
	}
	if _, err := ParseDSHEnforce(c.DSHEnforce); err != nil {
		return err
	}
	if c.LoginRate.Requests < 1 || c.LoginRate.Window.Duration() <= 0 {
		return errors.New("login_rate requests and window must be positive")
	}
	if c.DirectoryPicker != "clamp" && c.DirectoryPicker != "browse" {
		return errors.New(`directory_picker must be "clamp" or "browse"`)
	}
	if c.PluginBrowserFS != "on" && c.PluginBrowserFS != "off" {
		return errors.New(`plugin_browser_fs must be "on" or "off"`)
	}
	for _, name := range c.ReservedNames {
		if !ValidTenantName(name) {
			return fmt.Errorf("reserved tenant name %q is invalid", name)
		}
	}
	for _, seed := range c.WorkspaceSeed {
		if seed == "" || filepath.IsAbs(seed) || filepath.Clean(seed) != seed || seed == "." || strings.HasPrefix(seed, ".."+string(filepath.Separator)) || seed == ".." {
			return fmt.Errorf("workspace_seed %q must be a clean relative path inside the tenant root", seed)
		}
	}
	if c.WorkerLimits.MemoryHighBytes < 0 || c.WorkerLimits.MemoryMaxBytes < 0 || c.WorkerLimits.TasksMax < 0 || c.WorkerLimits.CPUQuotaPercent < 0 {
		return errors.New("worker_limits values must not be negative")
	}
	if c.WorkerLimits.MemoryHighBytes > 0 && c.WorkerLimits.MemoryMaxBytes > 0 && c.WorkerLimits.MemoryHighBytes > c.WorkerLimits.MemoryMaxBytes {
		return errors.New("worker_limits.memory_high_bytes must not exceed memory_max_bytes")
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	switch c.PublicScheme {
	case "", "auto", "http", "https":
	default:
		return fmt.Errorf("public_scheme must be auto, http or https (got %q)", c.PublicScheme)
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	switch c.SessionCookieSecure {
	case "", "auto", "always", "never":
	default:
		return fmt.Errorf("session_cookie_secure must be auto, always or never (got %q)", c.SessionCookieSecure)
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	switch c.PublicScheme {
	case "", "auto", "http", "https":
	default:
		return fmt.Errorf("public_scheme must be auto, http or https (got %q)", c.PublicScheme)
	}
	switch c.SettingsUI {
	case "", "lan", "loopback":
	default:
		return fmt.Errorf("settings_ui must be lan or loopback (got %q)", c.SettingsUI)
	}
	if c.PublicBaseURL != "" {
		parsed, err := url.Parse(c.PublicBaseURL)
		if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return errors.New("public_base_url must be a scheme://host URL without a path")
		}
		tenantPrefix, portalPrefix := normalizePathPrefix(c.TenantPathPrefix), normalizePathPrefix(c.PortalPathPrefix)
		if tenantPrefix == "/" || portalPrefix == "/" {
			return errors.New("tenant_path_prefix and portal_path_prefix must not be the root path")
		}
		if tenantPrefix == portalPrefix || strings.HasPrefix(tenantPrefix, portalPrefix+"/") || strings.HasPrefix(portalPrefix, tenantPrefix+"/") {
			return errors.New("tenant_path_prefix and portal_path_prefix must be distinct and must not overlap")
		}
	}
	if net.ParseIP(c.Deploy.PublicListen) == nil {
		return errors.New("deploy.public_listen must be one IP address")
	}
	if (c.TLS.Certificate == "") != (c.TLS.CertificateKey == "") {
		return errors.New("tls.certificate and tls.certificate_key must be set together")
	}
	for label, value := range map[string]string{"tls.certificate": c.TLS.Certificate, "tls.certificate_key": c.TLS.CertificateKey} {
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be a clean absolute path", label)
		}
	}
	paths := map[string]string{
		"tenant_root": c.TenantRoot, "workspace_root": c.WorkspaceRoot, "handshake_dir": c.HandshakeDir,
		"state_dir": c.StateDir, "registry_path": c.RegistryPath, "key_map_path": c.KeyMapPath,
		"session_path": c.SessionPath, "audit_path": c.AuditPath, "activity_path": c.ActivityPath, "dsh.node_bin": c.Dsh.NodeBin, "dsh.bin_js": c.Dsh.BinJS,
		"dsh.current_link":          c.Dsh.CurrentLink,
		"deploy.plugin_path":        c.Deploy.PluginPath,
		"deploy.template_home":      c.Deploy.TemplateHome,
		"deploy.backup_dir":         c.Deploy.BackupDir,
		"deploy.tenant_config_root": c.Deploy.TenantConfigRoot,
		"deploy.config_path":        c.Deploy.ConfigPath,
		"deploy.bwrap_bin":          c.Deploy.BwrapBin,
	}
	for label, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%s must be a clean absolute path", label)
		}
		if strings.ContainsAny(p, "\r\n;{}") {
			return fmt.Errorf("%s contains unsafe configuration characters", label)
		}
	}
	for label, root := range map[string]string{"tenant_root": c.TenantRoot, "workspace_root": c.WorkspaceRoot, "state_dir": c.StateDir} {
		if root == "/" {
			return fmt.Errorf("%s must not be filesystem root", label)
		}
	}
	for label, account := range map[string]string{"deploy.gateway_user": c.Deploy.GatewayUser, "deploy.worker_user": c.Deploy.WorkerUser} {
		if !accountNameRE.MatchString(account) || len(account) > 32 {
			return fmt.Errorf("%s must be a valid OS account name", label)
		}
	}
	return nil
}

func rangesOverlap(aLo, aHi, bLo, bHi int) bool { return aLo <= bHi && bLo <= aHi }

// RevalidateMode is the parsed key-revalidation policy.
type RevalidateMode struct {
	PerRequest bool
	Interval   time.Duration
}

func ParseRevalidate(raw string) (RevalidateMode, error) {
	switch raw {
	case "off":
		return RevalidateMode{}, nil
	case "per-request":
		return RevalidateMode{PerRequest: true}, nil
	}
	const prefix = "interval:"
	if strings.HasPrefix(raw, prefix) {
		n, err := strconv.ParseInt(strings.TrimPrefix(raw, prefix), 10, 64)
		if err != nil || n < 1 || n > (1<<63-1)/int64(time.Second) {
			return RevalidateMode{}, errors.New("key_revalidate interval must be a positive number of seconds")
		}
		return RevalidateMode{Interval: time.Duration(n) * time.Second}, nil
	}
	return RevalidateMode{}, errors.New(`key_revalidate must be "off", "per-request", or "interval:<seconds>"`)
}

// DSHEnforceMode is the parsed dsh_enforce setting (M52): when the dshgw proxy consults
// aigw's /v1/dshgw/authorize beyond the mandatory login-time check. Zero value = login only.
type DSHEnforceMode struct {
	PerRequest bool
	Interval   time.Duration
}

// ParseDSHEnforce mirrors ParseRevalidate: "login" (default), "per-request", or
// "interval:<seconds>" for a cached near-immediate revocation. Anything else is a
// configuration error, so a typo can never silently disable the entitlement check.
func ParseDSHEnforce(raw string) (DSHEnforceMode, error) {
	switch raw {
	case "", "login":
		return DSHEnforceMode{}, nil
	case "per-request":
		return DSHEnforceMode{PerRequest: true}, nil
	}
	const prefix = "interval:"
	if strings.HasPrefix(raw, prefix) {
		n, err := strconv.ParseInt(strings.TrimPrefix(raw, prefix), 10, 64)
		if err != nil || n < 1 || n > (1<<63-1)/int64(time.Second) {
			return DSHEnforceMode{}, errors.New("dsh_enforce interval must be a positive number of seconds")
		}
		return DSHEnforceMode{Interval: time.Duration(n) * time.Second}, nil
	}
	return DSHEnforceMode{}, errors.New(`dsh_enforce must be "login", "per-request", or "interval:<seconds>"`)
}

func ValidTenantName(name string) bool { return tenantNameRE.MatchString(name) }

func (c *Config) IsReservedTenant(name string) bool {
	for _, v := range c.ReservedNames {
		if name == v {
			return true
		}
	}
	return false
}

// SetTenantPorts installs an immutable registry snapshot used by origin helpers.
func (c *Config) SetTenantPorts(ports map[string]int) {
	names := make(map[string]int, len(ports))
	reverse := make(map[int]string, len(ports))
	for name, port := range ports {
		names[name] = port
		reverse[port] = name
	}
	c.tenantMu.Lock()
	c.tenantPorts = names
	c.portTenants = reverse
	c.tenantMu.Unlock()
}

func (c *Config) TenantOrigin(tenant string) string {
	if c.PathMode() {
		return c.PublicBaseURL + normalizePathPrefix(c.TenantPathPrefix) + "/" + tenant + "/"
	}
	c.tenantMu.RLock()
	port, ok := c.tenantPorts[tenant]
	c.tenantMu.RUnlock()
	if !ok {
		return ""
	}
	return c.OriginForPort(port)
}

// StoreAPIData reports whether API responses may be stored by a cache. False is the
// default and means every API answer leaves with "no-store".
func (c *Config) StoreAPIData() bool { return c.NoStoreAPIs != nil && !*c.NoStoreAPIs }

// LANSettingsUI reports whether the tenant settings panel is enabled for pages the
// browser does not treat as loopback.
func (c *Config) LANSettingsUI() bool { return c.SettingsUI != "loopback" }

// Scheme is the scheme browsers use to reach this deployment.
func (c *Config) Scheme() string {
	switch c.PublicScheme {
	case "http", "https":
		return c.PublicScheme
	}
	if c.PathMode() && strings.HasPrefix(c.PublicBaseURL, "http://") {
		return "http"
	}
	return "https"
}

// SecureSessionCookie reports whether the session cookie should carry Secure. A
// browser refuses to store such a cookie on a plain-HTTP origin, so this must match
// how clients actually reach the gateway — not how it hopes to be reached.
func (c *Config) SecureSessionCookie() bool {
	switch c.SessionCookieSecure {
	case "always":
		return true
	case "never":
		return false
	}
	// The cookie has to match how the browser reaches the site, in both modes:
	// modern browsers refuse to store a Secure cookie on a plain-HTTP origin, so a
	// mismatch silently discards the session right after a successful login.
	return c.Scheme() == "https"
}

// PathMode reports whether public URLs are path-based instead of port-based.
func (c *Config) PathMode() bool { return c.PublicBaseURL != "" }

// PublicOrigin is the origin a browser sends in the Origin header in path mode:
// an origin never carries a path, so every surface of the single domain shares it.
func (c *Config) PublicOrigin() string { return c.PublicBaseURL }

// ExpectedOrigin is the Origin value a browser sends for one surface. In path mode
// that is always the base origin (browsers drop the path); in port mode it is the
// surface's own origin.
func (c *Config) ExpectedOrigin(port int) string {
	if c.PathMode() {
		return c.PublicOrigin()
	}
	return c.OriginForPort(port)
}

// PortalPath is the public path of the login page: the portal prefix in path mode,
// the root otherwise.
func (c *Config) PortalPath() string {
	if !c.PathMode() {
		return "/"
	}
	return normalizePathPrefix(c.PortalPathPrefix) + "/"
}

// SessionCookiePath binds a tenant's session cookie to that tenant's path. In path
// mode every tenant shares one origin, so without this a browser would happily
// attach alice's cookie to a request for /t/bob/: the cookie's path is the only
// thing that keeps two tenants' sessions apart inside one origin.
func (c *Config) SessionCookiePath(tenant string) string {
	if !c.PathMode() {
		return "/"
	}
	return normalizePathPrefix(c.TenantPathPrefix) + "/" + tenant + "/"
}

func (c *Config) OriginForPort(port int) string {
	if !c.PathMode() {
		return fmt.Sprintf("%s://%s:%d", c.Scheme(), c.PublicHost, port)
	}
	if port == c.PortalPort {
		return c.PublicBaseURL + normalizePathPrefix(c.PortalPathPrefix) + "/"
	}
	c.tenantMu.RLock()
	name, known := c.portTenants[port]
	c.tenantMu.RUnlock()
	if known {
		return c.TenantOrigin(name)
	}
	return c.PublicBaseURL + "/"
}

// normalizePathPrefix returns a leading-slash, no-trailing-slash prefix.
func normalizePathPrefix(prefix string) string {
	trimmed := "/" + strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed == "/" {
		return "/"
	}
	return trimmed
}

func (c *Config) TenantFromPort(port int) (string, bool) {
	c.tenantMu.RLock()
	defer c.tenantMu.RUnlock()
	name, ok := c.portTenants[port]
	return name, ok
}

func (c *Config) SessionCookieName(tenant string) string { return "dshgw_s_" + tenant }
func (c *Config) WithTrailingSlash(raw string) string    { return strings.TrimRight(raw, "/") + "/" }

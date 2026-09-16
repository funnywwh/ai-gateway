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
var unitNameRE = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)
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
	NodeBin      string `yaml:"node_bin" json:"node_bin"`
	BinJS        string `yaml:"bin_js" json:"bin_js"`
	ReleasesRoot string `yaml:"releases_root" json:"releases_root"`
	CurrentLink  string `yaml:"current_link" json:"current_link"`
}

// TLSConfig names the existing certificate reused by host nginx.
type TLSConfig struct {
	Certificate    string `yaml:"certificate" json:"certificate"`
	CertificateKey string `yaml:"certificate_key" json:"certificate_key"`
}

// DeployConfig contains paths used only by root-operated provisioning commands.
type DeployConfig struct {
	SystemdDir       string `yaml:"systemd_dir" json:"systemd_dir"`
	NginxDir         string `yaml:"nginx_dir" json:"nginx_dir"`
	NginxIncludePath string `yaml:"nginx_include_path" json:"nginx_include_path"`
	NginxBinary      string `yaml:"nginx_binary" json:"nginx_binary"`
	DshgwBinary      string `yaml:"dshgw_binary" json:"dshgw_binary"`
	PluginPath       string `yaml:"plugin_path" json:"plugin_path"`
	TemplateHome     string `yaml:"template_home" json:"template_home"`
	BackupDir        string `yaml:"backup_dir" json:"backup_dir"`
	PublicListen     string `yaml:"public_listen" json:"public_listen"`
	WorkerUnit       string `yaml:"worker_unit" json:"worker_unit"`
	GatewayUnit      string `yaml:"gateway_unit" json:"gateway_unit"`
	WorkerSlice      string `yaml:"worker_slice" json:"worker_slice"`
	TenantConfigRoot string `yaml:"tenant_config_root" json:"tenant_config_root"`
	ConfigPath       string `yaml:"config_path" json:"config_path"`
	GatewayUser      string `yaml:"gateway_user" json:"gateway_user"`
	GatewayGroup     string `yaml:"gateway_group" json:"gateway_group"`
	DshUserPrefix    string `yaml:"dsh_user_prefix" json:"dsh_user_prefix"`
	CorepackBin      string `yaml:"corepack_bin" json:"corepack_bin"`
}

// Config is deliberately independent of aigw's internal configuration types.
type Config struct {
	PublicHost      string       `yaml:"public_host" json:"public_host"`
	PortalPort      int          `yaml:"portal_port" json:"portal_port"`
	TenantPortLo    int          `yaml:"tenant_port_lo" json:"tenant_port_lo"`
	TenantPortHi    int          `yaml:"tenant_port_hi" json:"tenant_port_hi"`
	WorkerPortLo    int          `yaml:"worker_port_lo" json:"worker_port_lo"`
	WorkerPortHi    int          `yaml:"worker_port_hi" json:"worker_port_hi"`
	Listen          string       `yaml:"listen" json:"listen"`
	EdgePortHeader  string       `yaml:"edge_port_header" json:"edge_port_header"`
	MaxHeaderBytes  int          `yaml:"max_header_bytes" json:"max_header_bytes"`
	MaxSessions     int          `yaml:"max_sessions" json:"max_sessions"`
	AigwBaseURL     string       `yaml:"aigw_base_url" json:"aigw_base_url"`
	ValidateTimeout Duration     `yaml:"validate_timeout" json:"validate_timeout"`
	SessionTTL      Duration     `yaml:"session_ttl" json:"session_ttl"`
	KeyRevalidate   string       `yaml:"key_revalidate" json:"key_revalidate"`
	LoginRate       RateLimit    `yaml:"login_rate" json:"login_rate"`
	DirectoryPicker string       `yaml:"directory_picker" json:"directory_picker"`
	PluginBrowserFS string       `yaml:"plugin_browser_fs" json:"plugin_browser_fs"`
	WorkspaceSeed   []string     `yaml:"workspace_seed" json:"workspace_seed"`
	ReservedNames   []string     `yaml:"reserved_names" json:"reserved_names"`
	Dsh             DshRuntime   `yaml:"dsh" json:"dsh"`
	TLS             TLSConfig    `yaml:"tls" json:"tls"`
	Deploy          DeployConfig `yaml:"deploy" json:"deploy"`
	TenantRoot      string       `yaml:"tenant_root" json:"tenant_root"`
	WorkspaceRoot   string       `yaml:"workspace_root" json:"workspace_root"`
	HandshakeDir    string       `yaml:"handshake_dir" json:"handshake_dir"`
	StateDir        string       `yaml:"state_dir" json:"state_dir"`
	RegistryPath    string       `yaml:"registry_path" json:"registry_path"`
	KeyMapPath      string       `yaml:"key_map_path" json:"key_map_path"`
	SessionPath     string       `yaml:"session_path" json:"session_path"`
	AuditPath       string       `yaml:"audit_path" json:"audit_path"`
	ActivityPath    string       `yaml:"activity_path" json:"activity_path"`

	tenantMu    sync.RWMutex
	tenantPorts map[string]int
	portTenants map[int]string
}

func defaults() Config {
	return Config{
		PublicHost:      "chat.tirisen.hk",
		PortalPort:      32600,
		TenantPortLo:    32601,
		TenantPortHi:    32799,
		WorkerPortLo:    32100,
		WorkerPortHi:    32299,
		Listen:          "127.0.0.1:3099",
		EdgePortHeader:  "X-DSHGW-Port",
		MaxHeaderBytes:  128 << 10,
		MaxSessions:     10000,
		AigwBaseURL:     "http://192.168.190.86:8088",
		ValidateTimeout: Duration(5 * time.Second),
		SessionTTL:      Duration(7 * 24 * time.Hour),
		KeyRevalidate:   "off",
		LoginRate:       RateLimit{Requests: 10, Window: Duration(time.Minute)},
		DirectoryPicker: "clamp",
		PluginBrowserFS: "on",
		WorkspaceSeed:   []string{"work"},
		ReservedNames:   []string{"login", "dshgw"},
		Dsh: DshRuntime{
			NodeBin:      "/opt/dsh/node/bin/node",
			BinJS:        "/opt/dsh/current/lib/bin.js",
			ReleasesRoot: "/opt/dsh/releases",
			CurrentLink:  "/opt/dsh/current",
		},
		WorkspaceRoot: "/srv/dsh",
		StateDir:      "/var/lib/dshgw",
		Deploy: DeployConfig{
			SystemdDir:       "/etc/systemd/system",
			NginxDir:         "/etc/nginx/conf.d/dshgw",
			NginxIncludePath: "/etc/nginx/conf.d/dshgw.conf",
			NginxBinary:      "/usr/sbin/nginx",
			DshgwBinary:      "/opt/dshgw/bin/dshgw",
			PluginPath:       "/opt/dshgw/share/dsh-plugin/picker-clamp.js",
			TemplateHome:     "/opt/dshgw/share/template-home",
			BackupDir:        "/home/winger/backups/dshgw",
			PublicListen:     "0.0.0.0",
			WorkerUnit:       "dsh-worker@.service",
			GatewayUnit:      "dshgw.service",
			WorkerSlice:      "dsh-workers.slice",
			TenantConfigRoot: "/etc/dshgw/tenants",
			ConfigPath:       DefaultPath,
			GatewayUser:      "dshgw",
			GatewayGroup:     "dshgw",
			DshUserPrefix:    "dsh-",
			CorepackBin:      "/opt/dsh/node/bin/corepack",
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
	if net.ParseIP(c.Deploy.PublicListen) == nil {
		return errors.New("deploy.public_listen must be one IP address")
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
	paths := map[string]string{
		"tenant_root": c.TenantRoot, "workspace_root": c.WorkspaceRoot, "handshake_dir": c.HandshakeDir,
		"state_dir": c.StateDir, "registry_path": c.RegistryPath, "key_map_path": c.KeyMapPath,
		"session_path": c.SessionPath, "audit_path": c.AuditPath, "activity_path": c.ActivityPath, "dsh.node_bin": c.Dsh.NodeBin, "dsh.bin_js": c.Dsh.BinJS,
		"dsh.releases_root": c.Dsh.ReleasesRoot, "dsh.current_link": c.Dsh.CurrentLink,
		"deploy.systemd_dir": c.Deploy.SystemdDir, "deploy.nginx_dir": c.Deploy.NginxDir,
		"deploy.nginx_include_path": c.Deploy.NginxIncludePath,
		"deploy.dshgw_binary":       c.Deploy.DshgwBinary, "deploy.plugin_path": c.Deploy.PluginPath,
		"deploy.template_home": c.Deploy.TemplateHome, "deploy.backup_dir": c.Deploy.BackupDir,
		"deploy.tenant_config_root": c.Deploy.TenantConfigRoot,
		"deploy.config_path":        c.Deploy.ConfigPath, "deploy.corepack_bin": c.Deploy.CorepackBin,
		"deploy.nginx_binary": c.Deploy.NginxBinary,
	}
	for label, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%s must be a clean absolute path", label)
		}
		if strings.ContainsAny(p, "\r\n;{}") {
			return fmt.Errorf("%s contains unsafe configuration characters", label)
		}
	}
	relInclude, err := filepath.Rel(c.Deploy.NginxDir, c.Deploy.NginxIncludePath)
	if err != nil {
		return err
	}
	if relInclude == "." || relInclude != ".." && !strings.HasPrefix(relInclude, ".."+string(filepath.Separator)) {
		return errors.New("deploy.nginx_include_path must be outside the managed nginx_dir")
	}
	for label, p := range map[string]string{"tls.certificate": c.TLS.Certificate, "tls.certificate_key": c.TLS.CertificateKey, "deploy.nginx_dir": c.Deploy.NginxDir, "deploy.nginx_include_path": c.Deploy.NginxIncludePath} {
		if p == "" && strings.HasPrefix(label, "tls.") {
			continue // Required by render-nginx, not disposable CLI contracts.
		}
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\r\n\x00;{}\"'\\$") {
			return fmt.Errorf("%s is unsafe for nginx configuration", label)
		}
	}
	for label, root := range map[string]string{"tenant_root": c.TenantRoot, "workspace_root": c.WorkspaceRoot, "state_dir": c.StateDir} {
		if root == "/" {
			return fmt.Errorf("%s must not be filesystem root", label)
		}
	}
	if !accountNameRE.MatchString(c.Deploy.GatewayUser) || len(c.Deploy.GatewayUser) > 32 || !accountNameRE.MatchString(c.Deploy.GatewayGroup) || len(c.Deploy.GatewayGroup) > 32 || !accountNameRE.MatchString(c.Deploy.DshUserPrefix) || len(c.Deploy.DshUserPrefix) > 5 {
		return errors.New("deploy OS account names or tenant user prefix are invalid")
	}
	for _, unit := range []string{c.Deploy.WorkerUnit, c.Deploy.GatewayUnit, c.Deploy.WorkerSlice} {
		if !unitNameRE.MatchString(unit) || strings.HasPrefix(unit, "-") {
			return errors.New("deploy unit names must be safe systemd identifiers")
		}
	}
	if !strings.HasSuffix(c.Deploy.WorkerUnit, "@.service") || strings.Count(c.Deploy.WorkerUnit, "@") != 1 || !strings.HasSuffix(c.Deploy.GatewayUnit, ".service") || !strings.HasSuffix(c.Deploy.WorkerSlice, ".slice") {
		return errors.New("deploy worker_unit, gateway_unit and worker_slice have incorrect suffixes")
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
	c.tenantMu.RLock()
	port, ok := c.tenantPorts[tenant]
	c.tenantMu.RUnlock()
	if !ok {
		return ""
	}
	return c.OriginForPort(port)
}

func (c *Config) OriginForPort(port int) string {
	return fmt.Sprintf("https://%s:%d", c.PublicHost, port)
}

func (c *Config) TenantFromPort(port int) (string, bool) {
	c.tenantMu.RLock()
	defer c.tenantMu.RUnlock()
	name, ok := c.portTenants[port]
	return name, ok
}

func (c *Config) SessionCookieName(tenant string) string { return "dshgw_s_" + tenant }
func (c *Config) WithTrailingSlash(raw string) string    { return strings.TrimRight(raw, "/") + "/" }

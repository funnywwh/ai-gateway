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
	"fmt"
	"net"
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
	NodeBin      string `yaml:"node_bin"`
	BinJS        string `yaml:"bin_js"`
	ReleasesRoot string `yaml:"releases_root"`
	CurrentLink  string `yaml:"current_link"`
}

// Deploy is the deployment subset aigw must override: the child's defaults point
// at a root-owned installation (/etc/dshgw, /var/lib/dshgw, /opt/dshgw), which is
// exactly what a rootless child must not use.
type Deploy struct {
	Isolation        string `yaml:"isolation"`
	WorkerUser       string `yaml:"worker_user"`
	TenantConfigRoot string `yaml:"tenant_config_root"`
	TemplateHome     string `yaml:"template_home"`
	DshgwBinary      string `yaml:"dshgw_binary"`
	ConfigPath       string `yaml:"config_path"`
	BackupDir        string `yaml:"backup_dir"`
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
	// Listen is the child's own loopback listener (the edge proxies tenant ports
	// onto it). It must not overlap any of the three port ranges.
	Listen      string `yaml:"listen"`
	AigwBaseURL string `yaml:"aigw_base_url"`
	// AdminSocket is the local provisioning socket the console's DSH buttons use.
	// In this shape it is owned by the same UID as aigw, not by root.
	AdminSocket   string     `yaml:"admin_socket"`
	StateDir      string     `yaml:"state_dir"`
	WorkspaceRoot string     `yaml:"workspace_root"`
	Dsh           DshRuntime `yaml:"dsh"`
	Deploy        Deploy     `yaml:"deploy"`
}

// Validate rejects inputs the child could only reject later, in a restart loop,
// with less context than aigw has here.
func (c ChildConfig) Validate() error {
	if !publicHostRE.MatchString(c.PublicHost) {
		return fmt.Errorf("dshgw.public_host %q must be a lowercase DNS name without scheme, port or path", c.PublicHost)
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
		"dshgw.dsh.releases_root":         c.Dsh.ReleasesRoot,
		"dshgw.deploy.tenant_config_root": c.Deploy.TenantConfigRoot,
		"dshgw.deploy.template_home":      c.Deploy.TemplateHome,
		"dshgw.deploy.dshgw_binary":       c.Deploy.DshgwBinary,
		"dshgw.deploy.config_path":        c.Deploy.ConfigPath,
		"dshgw.deploy.backup_dir":         c.Deploy.BackupDir,
	} {
		if err := checkAbsolute(label, value); err != nil {
			return err
		}
	}
	if c.Deploy.Isolation != "bwrap" {
		return fmt.Errorf("dshgw.deploy.isolation must be %q in the aigw-supervised shape: the child runs as aigw's own account, so only the bwrap mount namespace separates tenants", "bwrap")
	}
	if strings.TrimSpace(c.Deploy.WorkerUser) == "" {
		return fmt.Errorf("dshgw.deploy.worker_user must name the account the child runs workers as (aigw's own account in the supervised shape)")
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

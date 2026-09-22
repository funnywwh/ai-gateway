package tenancy

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

// sandboxRuntime is the deployment subset a profile needs.
func (m *Manager) sandboxRuntime() sandbox.Runtime {
	return sandbox.Runtime{
		BwrapBin:         m.Config.Deploy.BwrapBin,
		NodeBin:          m.Config.Dsh.NodeBin,
		BinJS:            m.Config.Dsh.BinJS,
		CurrentLink:      m.Config.Dsh.CurrentLink,
		TenantRoot:       m.Config.TenantRoot,
		WorkspaceRoot:    m.Config.WorkspaceRoot,
		TenantConfigRoot: m.Config.Deploy.TenantConfigRoot,
		PluginPath:       m.Config.Deploy.PluginPath,
	}
}

// sandboxTenant is the registry subset a profile needs, including the account's
// ssh-workspace mounts: the profile binds each of them explicitly, because bubblewrap's
// --bind does not carry submounts (M64).
func (m *Manager) sandboxTenant(t registry.Tenant) sandbox.Tenant {
	tenant := sandbox.Tenant{
		Name:        t.Name,
		Workspace:   t.Workspace,
		DshHome:     t.DshHome,
		WorkerPort:  t.WorkerPort,
		Environment: workerArgs(m.Config, t),
	}
	if m.SSHWorkspaces != nil {
		tenant.SSHMounts = m.SSHWorkspaces.MountsFor(t.Name)
	}
	// The operator-declared host directories (M71). Unlike the ssh mounts these are not kernel
	// mounts at all: the profile binds them, so the set only changes when the configuration
	// does (which restarts the workers anyway).
	if m.HostShares != nil {
		// An account the deployment did not name gets neither the container nor a bind: an
		// empty directory in every workspace would be noise, and a bind with nothing in it
		// would be a claim the account has no share to make.
		if shares := m.HostShares.SharesFor(t.Name, t.Workspace); len(shares) > 0 {
			tenant.HostShareRoot = m.HostShares.ContainerFor(t.Workspace)
			tenant.HostShares = shares
		}
	}
	if m.Config.BrowserWorkspaces.Enabled {
		tenant.BrowserMountRoot = filepath.Join(t.Workspace, "browser")
	}
	if m.BrowserWorkspaces != nil {
		tenant.BrowserMounts = m.BrowserWorkspaces.MountsFor(t.Name)
	}
	return tenant
}

// workerArgs is the dsh argv for one worker.
//
// `--trusted-host` names the public authority the worker's /api fence must accept.
// dsh trusts loopback Host values and any authority declared here; without the
// declaration a deployment that forwards the browser's authority (or a proxy that
// does not rewrite Host) gets 403s on every /api call. The port-based deployments
// this project already runs declare exactly this pair, so it is mirrored here
// rather than left to each operator to rediscover.
func workerArgs(cfg *config.Config, t registry.Tenant) []string {
	args := []string{"web", "--port", fmt.Sprintf("%d", t.WorkerPort), "--no-open"}
	seen := map[string]bool{}
	for _, host := range []string{cfg.PublicHost, net.JoinHostPort(cfg.PublicHost, fmt.Sprintf("%d", t.PublicPort))} {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		args = append(args, "--trusted-host", host)
	}
	return args
}

// SandboxProfile renders the bwrap profile for a tenant. It is the argv the worker
// runner spawns, so provisioning, the runner and the acceptance tests all describe
// the same sandbox.
func (m *Manager) SandboxProfile(t registry.Tenant) ([]string, error) {
	if t.EffectiveIsolation() != registry.IsolationBwrap {
		return nil, fmt.Errorf("tenant %s is not in bwrap isolation", t.Name)
	}
	if err := m.prepareBrowserMountRoot(t); err != nil {
		return nil, err
	}
	// The passwd view is rendered here rather than once at provisioning time: the account's
	// home entry is host state that an operator can change under a running deployment, and
	// bubblewrap needs the bind source on disk before it execs.
	passwd, err := m.prepareTenantPasswd(t)
	if err != nil {
		return nil, err
	}
	tenant := m.sandboxTenant(t)
	tenant.PasswdFile = passwd
	return sandbox.Profile(m.sandboxRuntime(), tenant)
}

// SandboxProfileReady validates everything a tenant needs before its worker
// starts: the unprivileged account the worker runs as, the runtime layout, the
// rendered profile, and the "other" bits on the tenant's own roots.
//
// In a shared-account shape the mount namespace is the isolation boundary and the
// host permission bits are the second line of defence, so a tenant leaf that any
// local user could walk through is refused here rather than relied on later.
func (m *Manager) SandboxProfileReady(t registry.Tenant) error {
	if err := m.prepareBrowserMountRoot(t); err != nil {
		return err
	}
	if err := m.checkWorkerAccount(); err != nil {
		return err
	}
	if err := m.checkSandboxRuntime(); err != nil {
		return err
	}
	profile, err := m.SandboxProfile(t)
	if err != nil {
		return err
	}
	if len(profile) == 0 || profile[0] != sandboxRuntimeBin(m.Config.Deploy.BwrapBin) {
		return errors.New("sandbox profile does not start with the configured bubblewrap binary")
	}
	for _, path := range []string{filepath.Dir(t.DshHome), t.Workspace, filepath.Join(m.Config.Deploy.TenantConfigRoot, t.Name)} {
		if err := checkPrivateToOthers(path); err != nil {
			return fmt.Errorf("tenant %s: %w", t.Name, err)
		}
	}
	return nil
}

// checkWorkerAccount verifies the unprivileged account every worker runs as,
// preferring an injected check so the lifecycle is testable without host accounts.
func (m *Manager) checkWorkerAccount() error {
	if m.WorkerAccount != nil {
		return m.WorkerAccount(m.Config.Deploy.WorkerUser)
	}
	return sandbox.ValidateWorkerAccount(m.Config.Deploy.WorkerUser)
}

// checkSandboxRuntime verifies the host-side bindings a profile needs.
func (m *Manager) checkSandboxRuntime() error {
	if m.RuntimeCheck != nil {
		return m.RuntimeCheck(m.sandboxRuntime())
	}
	return sandbox.ValidateRuntime(m.sandboxRuntime())
}

func sandboxRuntimeBin(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return sandbox.DefaultBwrapBin
	}
	return configured
}

// checkPrivateToOthers rejects a tenant-owned directory that grants the "other"
// class any access.
func checkPrivateToOthers(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		return fmt.Errorf("%s mode %04o grants other-user access; tenant roots must stay 0700", path, perm)
	}
	return nil
}

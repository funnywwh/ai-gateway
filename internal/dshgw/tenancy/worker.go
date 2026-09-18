package tenancy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// sandboxTenant is the registry subset a profile needs.
func sandboxTenant(t registry.Tenant) sandbox.Tenant {
	return sandbox.Tenant{
		Name:        t.Name,
		Workspace:   t.Workspace,
		DshHome:     t.DshHome,
		WorkerPort:  t.WorkerPort,
		Environment: []string{"web", "--port", fmt.Sprintf("%d", t.WorkerPort), "--no-open"},
	}
}

// SandboxProfile renders the bwrap profile for a tenant. It is the argv the worker
// runner spawns, so provisioning, the runner and the acceptance tests all describe
// the same sandbox.
func (m *Manager) SandboxProfile(t registry.Tenant) ([]string, error) {
	if t.EffectiveIsolation() != registry.IsolationBwrap {
		return nil, fmt.Errorf("tenant %s is not in bwrap isolation", t.Name)
	}
	return sandbox.Profile(m.sandboxRuntime(), sandboxTenant(t))
}

// SandboxProfileReady validates everything a tenant needs before its worker
// starts: the unprivileged account the worker runs as, the runtime layout, the
// rendered profile, and the "other" bits on the tenant's own roots.
//
// In a shared-account shape the mount namespace is the isolation boundary and the
// host permission bits are the second line of defence, so a tenant leaf that any
// local user could walk through is refused here rather than relied on later.
func (m *Manager) SandboxProfileReady(t registry.Tenant) error {
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

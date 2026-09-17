package tenancy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

// WorkerSpec is the confinement identity of one tenant's worker: which account
// runs it and what executable the unit starts. It mirrors what the installed
// systemd unit must say; `dshgw doctor` cross-checks the deployed unit file
// against this spec so configuration and units cannot drift silently.
type WorkerSpec struct {
	Isolation string
	User      string
	Group     string
	// Exec is the ExecStart command line, already quoted for systemd.
	Exec string
	// UnitTemplate is the unit file name (a systemd template with @).
	UnitTemplate string
}

// workerUnitTemplate returns the unit template the mode uses.
func (m *Manager) workerUnitTemplate(isolation string) (string, error) {
	if isolation == registry.IsolationBwrap {
		if m.Config.Deploy.WorkerUnitBwrap == "" {
			return "", errors.New("bwrap isolation requires deploy.worker_unit_bwrap")
		}
		return m.Config.Deploy.WorkerUnitBwrap, nil
	}
	return m.Config.Deploy.WorkerUnit, nil
}

// sandboxRuntime is the deployment subset the bwrap profile needs.
func (m *Manager) sandboxRuntime() sandbox.Runtime {
	return sandbox.Runtime{
		BwrapBin:         m.Config.Deploy.BwrapBin,
		NodeBin:          m.Config.Dsh.NodeBin,
		BinJS:            m.Config.Dsh.BinJS,
		CurrentLink:      m.Config.Dsh.CurrentLink,
		TenantRoot:       m.Config.TenantRoot,
		WorkspaceRoot:    m.Config.WorkspaceRoot,
		TenantConfigRoot: m.Config.Deploy.TenantConfigRoot,
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

// SandboxProfile renders the bwrap profile for a tenant. It is the same argv the
// worker unit's sandbox-exec launcher produces, which is what lets provisioning
// and the acceptance tests inspect a profile without starting anything.
func (m *Manager) SandboxProfile(t registry.Tenant) ([]string, error) {
	if t.EffectiveIsolation() != registry.IsolationBwrap {
		return nil, fmt.Errorf("tenant %s is not in bwrap isolation", t.Name)
	}
	return sandbox.Profile(m.sandboxRuntime(), sandboxTenant(t))
}

// WorkerSpecFor resolves the account and ExecStart for one tenant. bwrap mode
// uses one shared account for every tenant and takes its executable from the
// root-owned configuration only — never from tenant-owned state.
func (m *Manager) WorkerSpecFor(t registry.Tenant) (WorkerSpec, error) {
	isolation := t.EffectiveIsolation()
	template, err := m.workerUnitTemplate(isolation)
	if err != nil {
		return WorkerSpec{}, err
	}
	if isolation != registry.IsolationBwrap {
		user := m.user(t.Name)
		return WorkerSpec{
			Isolation:    registry.IsolationUser,
			User:         user,
			Group:        user,
			Exec:         fmt.Sprintf("%s %s web --port ${DSH_PORT} --no-open", m.Config.Dsh.NodeBin, m.Config.Dsh.BinJS),
			UnitTemplate: template,
		}, nil
	}
	workerUser := m.Config.Deploy.WorkerUser
	if err := m.checkWorkerAccount(); err != nil {
		return WorkerSpec{}, err
	}
	if _, err := m.SandboxProfile(t); err != nil {
		return WorkerSpec{}, err
	}
	return WorkerSpec{
		Isolation:    registry.IsolationBwrap,
		User:         workerUser,
		Group:        m.Config.Deploy.GatewayGroup,
		Exec:         fmt.Sprintf("%s --config %s sandbox-exec %s", m.Config.Deploy.DshgwBinary, m.Config.Deploy.ConfigPath, t.Name),
		UnitTemplate: template,
	}, nil
}

// workerUnitNameFor expands one systemd template name for a tenant.
func workerUnitNameFor(template, tenant string) string {
	return strings.Replace(template, "@.service", "@"+tenant+".service", 1)
}

// VerifyWorkerAccess proves, before the tenant is published or systemd starts
// anything, that the worker can actually reach the artifacts it needs. The two
// isolation modes prove it differently:
//
//   - user mode runs `test` as the tenant's own UID, which catches a shared
//     parent without the search bit that root's access would hide.
//   - bwrap mode proves the sandbox profile builds and that the tenant's own
//     roots grant nothing to unrelated users (the mounts supply traversal, not
//     host permission bits) — root's ability to read them proves nothing here.
func (m *Manager) VerifyWorkerAccess(ctx context.Context, t registry.Tenant) error {
	if t.EffectiveIsolation() == registry.IsolationBwrap {
		return m.SandboxProfileReady(t)
	}
	user := m.user(t.Name)
	for _, probe := range []struct{ flag, path string }{
		{"-r", filepath.Join(t.DshHome, "settings.yaml")},
		{"-r", filepath.Join(t.DshHome, ".credentials.yaml")},
		{"-w", t.DshHome},
		{"-w", t.Workspace},
	} {
		if _, err := m.run(ctx, "runuser", "-u", user, "--", "/usr/bin/test", probe.flag, probe.path); err != nil {
			return fmt.Errorf("worker UID cannot access %s; check shared parent search permissions: %w", probe.path, err)
		}
	}
	return nil
}

// SandboxProfileReady validates everything a bwrap-mode tenant needs before its
// worker starts: the shared account, the runtime layout, the rendered profile,
// and the "other" bits on the tenant's own roots. In this mode the mount view is
// the isolation boundary, so the tenant's roots must never be reachable by an
// unrelated user through the host filesystem alone.
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

// checkWorkerAccount verifies the shared worker account, preferring an injected
// check so the lifecycle is testable without host accounts.
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

// checkPrivateToOthers rejects a tenant-owned directory that grants the
// "other" class any access, because in shared-account mode the host permission
// bits are the second line of defence behind the mount view.
func checkPrivateToOthers(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		return fmt.Errorf("%s mode %04o grants other-user access; the bwrap isolation mode requires 0700 or stricter", path, perm)
	}
	return nil
}

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

// sandboxFailureExit is the launcher-failure status. Unit files and operators
// key on it: a worker that could not be confined must never be mistaken for a
// worker whose command merely failed.
const sandboxFailureExit = 127

// sandboxExec replaces the current process with the tenant's bubblewrap profile.
//
// It is the worker unit's ExecStart in the bwrap isolation mode: systemd runs it
// as the shared unprivileged worker account, and it takes the tenant name only.
// Everything else — the bubblewrap binary, node, the dsh release, every mount
// point — is derived from the root-owned configuration and the registry. Nothing
// the tenant can write is consulted, so a tenant cannot widen its own profile.
//
// There is deliberately no root requirement: the unit's User= is the shared
// worker account, so requiring root here would make every bwrap worker fail to
// start. The launcher never gains privilege — syscall.Exec replaces this process
// with bubblewrap running as the invoking account — and the files it reads are
// root-owned configuration that only root and the gateway group can read.
func (c *cli) sandboxExec(args []string) error {
	fs := c.flagSet("sandbox-exec")
	printOnly := fs.Bool("print", false, "print the profile argv instead of executing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw sandbox-exec [--print] TENANT")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if err := sandbox.ValidateRuntime(sandboxRuntimeConfig(deps.cfg)); err != nil {
		return sandboxFailure(err)
	}
	if err := sandbox.ValidateWorkerAccount(deps.cfg.Deploy.WorkerUser); err != nil {
		return sandboxFailure(err)
	}
	tenant, err := tenantByName(deps.reg, fs.Arg(0))
	if err != nil {
		return sandboxFailure(err)
	}
	if tenant.EffectiveIsolation() != registry.IsolationBwrap {
		return sandboxFailure(fmt.Errorf("tenant %s is not in bwrap isolation", tenant.Name))
	}
	argv, err := deps.manager.SandboxProfile(tenant)
	if err != nil {
		return sandboxFailure(err)
	}
	if *printOnly {
		// Printed for review and for the acceptance tests; never executed.
		fmt.Fprintln(c.stdout, strings.Join(argv, "\n"))
		return nil
	}
	if err := syscall.Exec(argv[0], argv, os.Environ()); err != nil {
		return sandboxFailure(fmt.Errorf("exec %s: %w", argv[0], err))
	}
	return nil
}

// sandboxFailure marks a launcher-side failure with the reserved exit status and
// the printable signature the seam matches on.
func sandboxFailure(err error) error {
	return &codedError{code: sandboxFailureExit, err: fmt.Errorf("sandbox-exec: %w", err)}
}

// sandboxRuntimeConfig is the deployment subset a profile needs, shared with the
// tenancy manager so the unit and the CLI can never disagree.
func sandboxRuntimeConfig(cfg *config.Config) sandbox.Runtime {
	return sandbox.Runtime{
		BwrapBin:         cfg.Deploy.BwrapBin,
		NodeBin:          cfg.Dsh.NodeBin,
		BinJS:            cfg.Dsh.BinJS,
		CurrentLink:      cfg.Dsh.CurrentLink,
		TenantRoot:       cfg.TenantRoot,
		WorkspaceRoot:    cfg.WorkspaceRoot,
		TenantConfigRoot: cfg.Deploy.TenantConfigRoot,
		PluginPath:       cfg.Deploy.PluginPath,
	}
}

// appArmorRestrictPath is the sysctl that decides whether an unprivileged
// process may create a user namespace on this host. It is the single host
// precondition the bwrap isolation mode cannot supply for itself.
const appArmorRestrictPath = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// checkAppArmorUserNSRestriction is a doctor precondition for the bwrap mode:
// with unprivileged user namespaces unrestricted, a tenant worker could nest a
// second namespace inside its own profile instead of living inside it. The
// sandbox still hides host paths — a nested namespace cannot reveal what was
// never mounted — so this is hardening, not the only boundary, and it is
// therefore reported as a failure with that reasoning rather than silently.
func checkAppArmorUserNSRestriction() error {
	data, err := os.ReadFile(appArmorRestrictPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is absent: unprivileged user namespaces are unrestricted, so a tenant could nest a namespace inside its profile", appArmorRestrictPath)
		}
		return err
	}
	switch value := strings.TrimSpace(string(data)); value {
	case "1":
		return nil
	case "0":
		return fmt.Errorf("%s is 0: set it to 1 so tenant workers cannot nest a wider namespace", appArmorRestrictPath)
	default:
		return fmt.Errorf("%s has unusable value %q", appArmorRestrictPath, value)
	}
}

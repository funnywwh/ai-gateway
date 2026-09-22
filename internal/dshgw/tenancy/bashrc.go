// Per-tenant interactive-shell startup file: what gives a terminal in the sandbox its colour.
//
// The profile's /etc is a whitelist — bubblewrap starts from an empty tmpfs root, so nothing is
// visible unless the profile mounts it — and the host's own bash startup files are not on that
// list. The tenant's HOME, which is the workspace, carries no ~/.bashrc either. An interactive
// shell therefore came up with no aliases, no LS_COLORS and bash's bare default prompt, and a
// terminal that renders colour perfectly (web-tty hands it TERM=xterm-256color and
// COLORTERM=truecolor) still showed a monochrome `ls` and prompt: `ls` only colours when
// something asks it to, and nothing in that view ever did.
//
// The file is a *view*, like the passwd view beside it: it lives in the tenant's own DSH home,
// so the tenant can rewrite it, and the worst a rewrite does is change how its own shell starts.
// The profile never consults it for a mount or a privilege.
package tenancy

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// bashrcViewName is the file name inside the tenant's own sandbox directory.
const bashrcViewName = "bashrc"

// tenantBashrcFile is where a tenant's interactive-shell startup file lives:
// <DshHome>/sandbox/bashrc, next to the passwd view and inside the only tree the worker account
// can write. The sandbox sees it as /etc/bash.bashrc, not at this path.
func tenantBashrcFile(dshHome string) string {
	return filepath.Join(dshHome, "sandbox", bashrcViewName)
}

// prepareTenantBashrc writes the tenant's interactive-shell startup file, returning the host
// path the profile binds at /etc/bash.bashrc.
//
// It runs on every profile build for the same reason the passwd view does: bubblewrap needs the
// bind source on disk before it execs, and re-writing heals a copy the tenant deleted or edited.
// The content is a constant (sandbox.Bashrc), so unlike the passwd view there is nothing to
// render from host state and no injection point to add. A failure is a failure to start the
// worker: a profile that quietly skipped the bind would come up with exactly the colourless
// terminal this file removes.
func (m *Manager) prepareTenantBashrc(t registry.Tenant) (string, error) {
	if err := m.validateTenantPaths(t); err != nil {
		return "", err
	}
	path := tenantBashrcFile(t.DshHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create tenant sandbox directory: %w", err)
	}
	// 0644: the file has to be readable inside the sandbox, where it is /etc/bash.bashrc, and it
	// carries nothing but shell defaults anyone could write for themselves.
	if err := securefile.WriteAtomic(path, []byte(sandbox.Bashrc), 0o644); err != nil {
		return "", fmt.Errorf("write tenant shell startup file: %w", err)
	}
	return path, nil
}

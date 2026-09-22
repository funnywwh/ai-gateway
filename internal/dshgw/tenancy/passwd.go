// Per-tenant passwd view: what the sandbox is told its own home directory is.
//
// The runner exports HOME=<workspace> for every worker (runner.go), while /etc/passwd comes
// from the host and still names the deployment account's own home — a path the profile hides
// behind an empty tmpfs. OpenSSH resolves `~` from passwd rather than from $HOME, so without
// this view the gateway's own per-account ssh alias list (<workspace>/.ssh/config) is
// invisible inside the sandbox and `ssh <alias>` degrades into a DNS lookup of the alias name.
// Rendering the view is what makes getpwuid and HOME agree again.
package tenancy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// hostPasswdPath is the host file every tenant view is rendered from.
const hostPasswdPath = "/etc/passwd"

// passwdViewName is the file name inside the tenant's own sandbox directory.
const passwdViewName = "passwd"

// tenantPasswdFile is where a tenant's passwd view lives: <DshHome>/sandbox/passwd, next to
// the per-tenant gateway state (plugin-state, …) and inside the only tree the worker account
// can write. The sandbox sees it as /etc/passwd, not at this path.
//
// The tenant can rewrite this file — it owns its DSH home — so it is a *view*, not a
// boundary: the worst a rewrite can do is change the name → home mapping inside that tenant's
// own sandbox, and the profile never consults it for a mount or a privilege.
func tenantPasswdFile(dshHome string) string {
	return filepath.Join(dshHome, "sandbox", passwdViewName)
}

// prepareTenantPasswd renders and writes the tenant's passwd view, returning the host path the
// profile binds at /etc/passwd.
//
// It runs on every profile build, which is exactly when the answer can have changed (the
// workspace path is fixed in the registry, but the account's host entry is not), and it
// happens before the worker exists, so the bind source is always there when bubblewrap looks
// for it. A failure is a failure to start the worker: a profile that silently fell back to the
// host's passwd would reintroduce the disagreement this exists to remove.
func (m *Manager) prepareTenantPasswd(t registry.Tenant) (string, error) {
	worker := strings.TrimSpace(m.Config.Deploy.WorkerUser)
	if worker == "" {
		return "", errors.New("passwd view needs deploy.worker_user")
	}
	if err := m.validateTenantPaths(t); err != nil {
		return "", err
	}
	host, err := m.hostPasswd()
	if err != nil {
		return "", err
	}
	view, err := sandbox.RenderPasswd(host, worker, t.Workspace)
	if err != nil {
		return "", err
	}
	path := tenantPasswdFile(t.DshHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create tenant sandbox directory: %w", err)
	}
	// 0644: the file has to be readable inside the sandbox, where it is /etc/passwd, and it
	// carries nothing the account cannot already read on the host.
	if err := securefile.WriteAtomic(path, view, 0o644); err != nil {
		return "", fmt.Errorf("write tenant passwd view: %w", err)
	}
	return path, nil
}

// hostPasswd reads the host file a view is rendered from. Tests inject a fixed file so
// provisioning never depends on the accounts a machine happens to have.
func (m *Manager) hostPasswd() ([]byte, error) {
	if m.HostPasswd != nil {
		return m.HostPasswd()
	}
	host, err := os.ReadFile(hostPasswdPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", hostPasswdPath, err)
	}
	return host, nil
}

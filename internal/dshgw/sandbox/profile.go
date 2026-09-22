// Package sandbox builds the per-tenant bubblewrap profile dshgw workers run
// inside. The profile is the tenant's entire filesystem view: every path a
// tenant can see is either the tenant's own writable root, a read-only runtime
// file needed to start Node and dsh, or device/process plumbing. No host
// directory is exposed wholesale, so one tenant can never observe another
// tenant's data, the gateway's registry, or the operator's home.
//
// Three measured host facts shape the argv this package produces:
//
//   - bubblewrap starts from an empty tmpfs root, so nothing is visible unless
//     this profile mounts it. Hiding /home, /root, /var and friends is
//     therefore belt-and-braces rather than the primary mechanism, and the
//     absence of a mount is what keeps the rest of the host out.
//   - the dynamic loader is reached through the host's top-level symlinks
//     (/bin and /lib are symlinks to usr/bin and usr/lib), and those symlinks
//     live outside the bound trees. Binding only /usr leaves every
//     dynamically linked binary unresolvable ("execvp ...: No such file or
//     directory"), so /usr/lib, /usr/lib64, and the host's own /bin and /sbin
//     are bound explicitly; without the /bin bind, `#!/bin/sh` scripts and
//     Node's default child-process shell do not exist inside the sandbox.
//   - /etc is not bound as a tree: only the named files the runtime reads are
//     mounted, so nginx, systemd, ssh and the gateway's own key directory
//     never appear in the tenant's view. /etc/alternatives is the one directory
//     bound back, because the distro's /usr/bin/{pager,awk,which,vi,…} are
//     symlinks into it: leaving it out makes those names dangle inside the
//     sandbox, and an ordinary interactive `git log` then dies with "cannot run
//     pager: No such file or directory".
//
// This package is deliberately free of filesystem and process side effects: it
// only computes argv, so the profile can be unit-tested and printed for review.
// internal/dshgw/sandbox/staging_test.go re-checks the resulting isolation
// against a real bubblewrap when the host permits it.
package sandbox

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DefaultBwrapBin is the bubblewrap entry point used when the deployment does
// not name one.
const DefaultBwrapBin = "/usr/bin/bwrap"

var tenantNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,25}[a-z0-9]$|^[a-z]$`)

// Runtime is the minimal set of deployment facts a profile needs. It is a
// subset of config.Dsh/config.Deploy on purpose: the sandbox package must not
// depend on the full configuration surface to stay trivially testable.
type Runtime struct {
	// BwrapBin is the bubblewrap executable.
	BwrapBin string
	// NodeBin is the absolute path of the node executable.
	NodeBin string
	// BinJS is the absolute path of dsh's public launcher (…/lib/bin.js).
	BinJS string
	// CurrentLink is the dsh release path (…/current); its resolved directory
	// is what actually gets bound.
	CurrentLink string
	// TenantRoot holds one directory per tenant (…/tenants/<name>).
	TenantRoot string
	// WorkspaceRoot is the parent of every tenant workspace.
	WorkspaceRoot string
	// TenantConfigRoot holds per-tenant configuration (the gateway's copy of the
	// tenant key); it is deliberately NOT mounted.
	TenantConfigRoot string
	// PluginPath is the directory-picker plugin the tenant profile imports. It
	// lives outside every other bound tree, so the sandbox must bind its directory
	// explicitly — without it dsh fails to boot inside the tenant (the profile's
	// cordis patch imports this file by absolute path).
	PluginPath string
}

// Tenant is the subset of a registry tenant a profile needs.
type Tenant struct {
	Name        string
	Workspace   string
	DshHome     string
	WorkerPort  int
	Environment []string
	// SSHMounts are the sshfs mount points the gateway made inside this tenant's workspace
	// (M64). Each one is bound explicitly: bubblewrap's --bind does not carry submounts, so
	// a mount inside the workspace is invisible to the sandbox unless its own path is bound,
	// and a mount that appears after the worker started is invisible until it starts again.
	SSHMounts []string
	// BrowserMounts are explicitly bound browser-backed mount points.
	BrowserMounts []string
	// BrowserMountRoot is gateway-managed and read-only in the tenant namespace.
	BrowserMountRoot string
	// HostShareRoot is the gateway-managed container of the operator-declared host directories
	// (<workspace>/<host_shares.subdir>), read-only in the tenant namespace so the container
	// cannot be replaced through the writable workspace. HostShares is one binding per declared
	// directory: no mount, no sshfs, nothing that can wedge (M71).
	HostShareRoot string
	HostShares    []HostShare
}

// HostShare is one operator-declared host directory as it appears inside one tenant's sandbox.
type HostShare struct {
	// Name is the display name and the last path segment under the container.
	Name string
	// Source is the host directory, already resolved through symlinks by config validation.
	Source string
	// Target is where the tenant sees it: <workspace>/<host_shares.subdir>/<name>.
	Target string
	// ReadOnly decides between --ro-bind and --bind.
	ReadOnly bool
}

// hiddenRoots are host directories replaced by an empty tmpfs, so a stray bind
// of a child path can never expose the parent's contents. The sandbox root is
// already an empty tmpfs, so these mounts matter mainly as a guarantee that a
// later edit cannot accidentally inherit a host tree.
var hiddenRoots = []string{"/home", "/root", "/tmp", "/var", "/srv", "/etc/dshgw"}

// Profile is one tenant's full bubblewrap argv: the profile flags, the `--`
// separator, and the exact command to run.
func Profile(rt Runtime, t Tenant) ([]string, error) {
	nodeBin, binJS, nodeRoot, dshRoot, workspace, dshHome, err := resolve(rt, t)
	if err != nil {
		return nil, err
	}

	argv := []string{bwrapBin(rt.BwrapBin)}

	// Hiding mounts first: each tmpfs replaces a host directory wholesale, so
	// nothing below it survives unless a later bind puts it back.
	for _, hide := range hiddenRoots {
		argv = append(argv, "--tmpfs", hide)
	}

	// Read-only runtime. /usr, /lib, /lib64 and /bin are the loader's and the
	// shell's own paths. On merged-/usr hosts /lib and /bin are symlinks into
	// /usr; the sandbox root is an empty tmpfs, so those symlinks do not exist
	// there and each name must be bound as a real directory — the kernel looks
	// up the ELF interpreter through the literal /lib64 path and the default
	// child-process shell through the literal /bin/sh path.
	argv = append(argv,
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/usr/lib", "/lib",
		"--ro-bind", "/usr/lib64", "/lib64",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
	)
	if nodeRoot != "/usr" {
		argv = append(argv, "--ro-bind", nodeRoot, nodeRoot)
	}
	if dshRoot != nodeRoot && dshRoot != "/usr" {
		argv = append(argv, "--ro-bind", dshRoot, dshRoot)
	}

	// /etc is not a bound tree: only the handful of entries the runtime
	// genuinely reads (name resolution, getpwuid, TLS trust, time zone) is
	// mounted, so nginx, systemd and the gateway's own configuration stay
	// invisible.
	argv = append(argv,
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind-try", "/etc/hosts", "/etc/hosts",
		"--ro-bind-try", "/etc/nsswitch.conf", "/etc/nsswitch.conf",
		"--ro-bind-try", "/etc/passwd", "/etc/passwd",
		"--ro-bind-try", "/etc/group", "/etc/group",
		"--ro-bind-try", "/etc/localtime", "/etc/localtime",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		"--ro-bind-try", "/etc/ca-certificates", "/etc/ca-certificates",
		// The alternatives database is the one /etc *directory* that is bound
		// back, and it is not decoration: the distro's /usr/bin/pager, awk,
		// which, vi and friends are symlinks to /etc/alternatives/<name>, so
		// without this bind they exist as dangling links inside an otherwise
		// complete /usr. The symptom is far from the cause: git's default pager
		// is the literal name `pager` (the alternatives wrapper) on these hosts,
		// so an interactive `git log` on a tty fails with "cannot run pager: No
		// such file or directory" plus "fatal: unable to execute pager 'pager'"
		// while the same command piped through a non-tty works. The directory
		// holds nothing but symlinks into paths the profile already mounts, so it
		// adds no host data a reader of /usr could not infer; --ro-bind-try keeps
		// hosts without it (non-Debian layouts) working unchanged.
		"--ro-bind-try", "/etc/alternatives", "/etc/alternatives",
	)

	// The directory-picker plugin, read-only. Its whole directory is bound because
	// the import resolves the file by absolute path and may bring siblings with it.
	if pluginDir, err := pluginDirectory(rt.PluginPath); err != nil {
		return nil, err
	} else if pluginDir != "" {
		argv = append(argv, "--ro-bind", pluginDir, pluginDir)
	}

	// The tenant's own roots. The workspace is the writable session root and the
	// dsh home keeps sessions, storages and the profile cache. The per-tenant
	// configuration directory is deliberately NOT mounted: systemd reads
	// tenant.env from the host before the sandbox exists, and the directory also
	// holds gateway.key, which a tenant could not read in the per-tenant-account
	// mode. Leaving it out keeps that property instead of exposing the key to
	// the shared worker account for no functional gain.
	argv = append(argv,
		"--bind", workspace, workspace,
		"--bind", filepath.Dir(dshHome), filepath.Dir(dshHome),
	)
	// The account's ssh-workspace mounts, one explicit binding each. --bind-try tolerates a
	// mount point that is not there (a record whose mount has not been re-made yet), and the
	// containment check above already guarantees none of them leaves the workspace.
	for _, mount := range t.SSHMounts {
		if !within(workspace, mount) || mount == workspace {
			return nil, fmt.Errorf("tenant %s ssh mount %s is not inside its workspace %s", t.Name, mount, workspace)
		}
		argv = append(argv, "--bind-try", mount, mount)
	}

	// Protect the mount container even before the first browser mount exists. A
	// read-only bind also makes the container a mountpoint, preventing replacement
	// through the otherwise writable parent workspace. Child binds remain writable.
	if t.BrowserMountRoot != "" {
		root := t.BrowserMountRoot
		if root != filepath.Join(workspace, "browser") {
			return nil, fmt.Errorf("invalid browser mount root %q", root)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve browser mount root: %w", err)
		}
		info, err := os.Lstat(root)
		if err != nil {
			return nil, err
		}
		if resolved != root || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("browser mount root must be a private canonical directory: %s", root)
		}
		argv = append(argv, "--ro-bind", root, root)
	} else if len(t.BrowserMounts) > 0 {
		return nil, fmt.Errorf("browser mounts require a protected browser mount root")
	}
	for _, mount := range t.BrowserMounts {
		root := filepath.Join(workspace, "browser")
		if !filepath.IsAbs(mount) || filepath.Clean(mount) != mount || !within(root, mount) || mount == root {
			return nil, fmt.Errorf("tenant %s browser mount %s is not inside its browser workspace %s", t.Name, mount, root)
		}
		resolved, err := filepath.EvalSymlinks(mount)
		if err != nil {
			return nil, fmt.Errorf("resolve browser mount: %w", err)
		}
		if resolved != mount {
			return nil, fmt.Errorf("browser mount %s contains symlinks", mount)
		}
		argv = append(argv, "--bind", mount, mount)
	}

	// Operator-declared host directories (M71), bound straight in: no sshfs, no FUSE, so
	// nothing here can end up in the uninterruptible wait a wedged mount produces. The
	// container gets the same read-only protection as the browser root — the parent workspace
	// is writable, so an unprotected container could be replaced — and each share is bound at
	// its own path, read-only unless the deployment said otherwise.
	if t.HostShareRoot != "" {
		root := t.HostShareRoot
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || !within(workspace, root) || root == workspace {
			return nil, fmt.Errorf("tenant %s host share root %s is not inside its workspace %s", t.Name, root, workspace)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve host share root: %w", err)
		}
		info, err := os.Lstat(root)
		if err != nil {
			return nil, err
		}
		if resolved != root || !info.IsDir() {
			return nil, fmt.Errorf("host share root must be a canonical directory: %s", root)
		}
		argv = append(argv, "--ro-bind", root, root)
	} else if len(t.HostShares) > 0 {
		return nil, fmt.Errorf("host shares require a protected host share root")
	}
	for _, share := range t.HostShares {
		root := t.HostShareRoot
		if !filepath.IsAbs(share.Source) || filepath.Clean(share.Source) != share.Source || share.Source == string(filepath.Separator) {
			return nil, fmt.Errorf("tenant %s host share %s has an unusable source %q", t.Name, share.Name, share.Source)
		}
		if !filepath.IsAbs(share.Target) || filepath.Clean(share.Target) != share.Target || !within(root, share.Target) || share.Target == root {
			return nil, fmt.Errorf("tenant %s host share %s target %s is not inside its host share root %s", t.Name, share.Name, share.Target, root)
		}
		// A share that no longer exists is skipped rather than fatal: one directory the
		// operator removed must not be the reason an account cannot start.
		if _, err := os.Stat(share.Source); err != nil {
			continue
		}
		flag := "--bind-try"
		if share.ReadOnly {
			flag = "--ro-bind-try"
		}
		argv = append(argv, flag, share.Source, share.Target)
	}
	// Devices and a private process view. The network namespace is deliberately
	// shared: the worker must reach aigw.
	argv = append(argv, "--dev", "/dev", "--proc", "/proc", "--unshare-pid", "--die-with-parent")
	argv = append(argv, "--")
	argv = append(argv, nodeBin, binJS)
	argv = append(argv, t.Environment...)
	return argv, nil
}

// resolve validates the tenant record against the deployment and resolves every
// path the profile will bind.
func resolve(rt Runtime, t Tenant) (nodeBin, binJS, nodeRoot, dshRoot, workspace, dshHome string, err error) {
	if !tenantNameRE.MatchString(t.Name) {
		return "", "", "", "", "", "", fmt.Errorf("invalid tenant name %q", t.Name)
	}
	if t.WorkerPort < 1 || t.WorkerPort > 65535 {
		return "", "", "", "", "", "", fmt.Errorf("tenant %s has invalid worker port %d", t.Name, t.WorkerPort)
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{"bwrap_bin", bwrapBin(rt.BwrapBin)},
		{"node_bin", rt.NodeBin},
		{"bin_js", rt.BinJS},
		{"tenant_root", rt.TenantRoot},
		{"workspace_root", rt.WorkspaceRoot},
		{"tenant_config_root", rt.TenantConfigRoot},
		{"workspace", t.Workspace},
		{"dsh_home", t.DshHome},
	} {
		if err = safeAbsolute(field.value, field.label); err != nil {
			return "", "", "", "", "", "", err
		}
	}
	for _, entry := range t.Environment {
		if entry == "" || strings.ContainsAny(entry, "\x00\n\r") {
			return "", "", "", "", "", "", fmt.Errorf("tenant %s has an unusable argv entry %q", t.Name, entry)
		}
	}
	// A registry record is data, not authority: even a tampered registry.json
	// must not be able to turn the profile into "bind the whole host".
	if !within(rt.WorkspaceRoot, t.Workspace) || t.Workspace == rt.WorkspaceRoot {
		return "", "", "", "", "", "", fmt.Errorf("tenant %s workspace %s is not inside workspace_root %s", t.Name, t.Workspace, rt.WorkspaceRoot)
	}
	if !within(rt.TenantRoot, filepath.Dir(t.DshHome)) || filepath.Dir(t.DshHome) == rt.TenantRoot {
		return "", "", "", "", "", "", fmt.Errorf("tenant %s dsh home %s is not inside tenant_root %s", t.Name, t.DshHome, rt.TenantRoot)
	}

	workspace = t.Workspace
	dshHome = t.DshHome
	if nodeBin, err = mustAbsolute(rt.NodeBin, "node_bin"); err != nil {
		return "", "", "", "", "", "", err
	}
	if binJS, err = mustAbsolute(rt.BinJS, "bin_js"); err != nil {
		return "", "", "", "", "", "", err
	}
	if nodeRoot, err = executableRoot(nodeBin, "node_bin"); err != nil {
		return "", "", "", "", "", "", err
	}
	if dshRoot, err = releaseRoot(rt.CurrentLink, "current_link"); err != nil {
		return "", "", "", "", "", "", err
	}
	return nodeBin, binJS, nodeRoot, dshRoot, workspace, dshHome, nil
}

// ValidateRuntime rejects a deployment whose bwrap mode cannot work, before any
// tenant is created or started.
func ValidateRuntime(rt Runtime) error {
	if err := ValidateBindings(rt); err != nil {
		return err
	}
	// The linker directories are the one mount set whose absence produces an
	// inscrutable "execvp: No such file or directory" instead of a clear error,
	// so they are checked here rather than discovered at first start.
	for _, dir := range []string{"/usr", "/usr/lib", "/usr/lib64"} {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("sandbox runtime requires %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("sandbox runtime requires the directory %s", dir)
		}
	}
	return nil
}

// ValidateBindings checks the deployment-side paths a profile binds: the
// bubblewrap binary, node, the dsh launcher and its release. It is the part of
// the preflight that does not depend on the host's own /usr layout, so tests can
// exercise the lifecycle with a synthetic installation.
func ValidateBindings(rt Runtime) error {
	if err := safeAbsolute(bwrapBin(rt.BwrapBin), "bwrap_bin"); err != nil {
		return err
	}
	if _, err := mustAbsolute(rt.NodeBin, "node_bin"); err != nil {
		return err
	}
	if _, err := mustAbsolute(rt.BinJS, "bin_js"); err != nil {
		return err
	}
	return nil
}

// ValidateWorkerAccount checks the single unprivileged account that runs every
// bwrap-mode worker. bubblewrap needs an unprivileged caller, and a shared
// account must not be root: inside the sandbox a root caller could re-create a
// wider namespace instead of living inside this profile.
func ValidateWorkerAccount(userName string) error {
	if strings.TrimSpace(userName) == "" {
		return fmt.Errorf("the bwrap isolation mode requires deploy.worker_user")
	}
	if userName == "root" || userName == "0" {
		return fmt.Errorf("worker_user must be an unprivileged account, not root")
	}
	account, err := user.Lookup(userName)
	if err != nil {
		return fmt.Errorf("worker_user %q does not exist: %w", userName, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("worker_user %q has unusable uid %q", userName, account.Uid)
	}
	if uid == 0 {
		return fmt.Errorf("worker_user %q is uid 0", userName)
	}
	return nil
}

// pluginDirectory returns the directory to bind for the picker plugin, or "" when
// the deployment did not configure one (then the tenant profile cannot reference
// it either, and dsh boots without the picker).
//
// It refuses directories that would undo the sandbox: binding the filesystem root
// (a plugin path like "/plugin.js") or an ancestor of a hidden tree is exactly the
// "--ro-bind / /" this profile exists to avoid, so a misconfigured path fails
// loudly here instead of silently handing the tenant the host.
func pluginDirectory(configured string) (string, error) {
	trimmed := strings.TrimSpace(configured)
	if trimmed == "" {
		return "", nil
	}
	if err := safeAbsolute(trimmed, "deploy.plugin_path"); err != nil {
		return "", err
	}
	dir := filepath.Dir(trimmed)
	if dir == "/" {
		return "", fmt.Errorf("deploy.plugin_path %s sits in the filesystem root; binding that directory would expose the whole host", trimmed)
	}
	for _, hidden := range hiddenRoots {
		if dir == hidden || within(dir, hidden) {
			return "", fmt.Errorf("deploy.plugin_path %s would bind %s, which contains the hidden tree %s", trimmed, dir, hidden)
		}
	}
	return dir, nil
}

func bwrapBin(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return DefaultBwrapBin
	}
	return configured
}

func safeAbsolute(path, label string) error {
	if path == "" {
		return fmt.Errorf("%s must not be empty", label)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be an absolute path", label)
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("%s must be a clean absolute path", label)
	}
	if strings.ContainsAny(path, "\x00\n\r") {
		return fmt.Errorf("%s contains an unsafe character", label)
	}
	return nil
}

func mustAbsolute(path, label string) (string, error) {
	if err := safeAbsolute(path, label); err != nil {
		return "", err
	}
	return path, nil
}

// releaseRoot resolves an installed dsh release path (…/current, usually a
// symlink) to the directory that must exist inside the sandbox.
func releaseRoot(path, label string) (string, error) {
	if _, err := mustAbsolute(path, label); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s %s is unusable: %w", label, path, err)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("%s %s does not resolve to an absolute path", label, path)
	}
	return resolved, nil
}

// executableRoot returns the directory to bind for an executable: the package
// root above a `bin` directory when there is one, so the executable's siblings
// and its own runtime files stay reachable.
func executableRoot(executable, label string) (string, error) {
	if _, err := mustAbsolute(executable, label); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("%s %s is unusable: %w", label, executable, err)
	}
	dir := filepath.Dir(resolved)
	if filepath.Base(dir) == "bin" {
		return filepath.Dir(dir), nil
	}
	return dir, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

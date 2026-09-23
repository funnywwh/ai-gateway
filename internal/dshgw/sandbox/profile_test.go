package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// profileFixture builds a deployment layout under a temp dir that mimics the
// real host: a node installation under <root>/dsh/node, a release under
// <root>/dsh/releases/v1 with <root>/dsh/current pointing at it, tenant roots
// under <root>/srv and <root>/state/tenants, and per-tenant config under
// <root>/etc/dshgw/tenants.
type profileFixture struct {
	root   string
	rt     Runtime
	alice  Tenant
	tenant string
}

func newProfileFixture(t *testing.T) profileFixture {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dsh/node/bin"), 0o755)
	mustWrite(t, filepath.Join(root, "dsh/node/bin/node"), "#!/bin/sh\n", 0o755)
	mustMkdir(t, filepath.Join(root, "dsh/releases/v1/lib"), 0o755)
	mustWrite(t, filepath.Join(root, "dsh/releases/v1/lib/bin.js"), "// dsh\n", 0o644)
	mustSymlink(t, filepath.Join(root, "dsh/releases/v1"), filepath.Join(root, "dsh/current"))
	mustMkdir(t, filepath.Join(root, "srv/alice/work"), 0o700)
	mustMkdir(t, filepath.Join(root, "srv/bob/work"), 0o700)
	mustMkdir(t, filepath.Join(root, "state/tenants/alice/.dsh"), 0o700)
	mustMkdir(t, filepath.Join(root, "etc/dshgw/tenants/alice"), 0o750)
	mustWrite(t, filepath.Join(root, "etc/dshgw/tenants/alice/gateway.key"), "sk-alice\n", 0o640)

	rt := Runtime{
		NodeBin:          filepath.Join(root, "dsh/node/bin/node"),
		BinJS:            filepath.Join(root, "dsh/current/lib/bin.js"),
		CurrentLink:      filepath.Join(root, "dsh/current"),
		TenantRoot:       filepath.Join(root, "state/tenants"),
		WorkspaceRoot:    filepath.Join(root, "srv"),
		TenantConfigRoot: filepath.Join(root, "etc/dshgw/tenants"),
	}
	return profileFixture{
		root: root,
		rt:   rt,
		alice: Tenant{
			Name:       "alice",
			Workspace:  filepath.Join(root, "srv/alice"),
			DshHome:    filepath.Join(root, "state/tenants/alice/.dsh"),
			WorkerPort: 32100,
		},
	}
}

type mount struct {
	flag string
	src  string
	dst  string
}

// parseMounts splits the profile argv into its mounts, the post-`--` command,
// and any non-mount flags.
func parseMounts(t *testing.T, argv []string) (mounts []mount, flags []string, command []string) {
	t.Helper()
	i := 0
	for i < len(argv) {
		arg := argv[i]
		switch arg {
		case "--":
			return mounts, flags, argv[i+1:]
		case "--ro-bind", "--ro-bind-try", "--bind", "--bind-try", "--dev-bind", "--dev-bind-try":
			if i+2 >= len(argv) {
				t.Fatalf("truncated %s in %v", arg, argv)
			}
			mounts = append(mounts, mount{flag: arg, src: argv[i+1], dst: argv[i+2]})
			i += 3
		case "--tmpfs", "--dir":
			if i+1 >= len(argv) {
				t.Fatalf("truncated %s in %v", arg, argv)
			}
			mounts = append(mounts, mount{flag: arg, dst: argv[i+1]})
			i += 2
		default:
			flags = append(flags, arg)
			i++
		}
	}
	t.Fatalf("profile has no -- separator: %v", argv)
	return nil, nil, nil
}

func TestProfileHidesEveryHostTreeExceptRuntimeAndTenantRoots(t *testing.T) {
	f := newProfileFixture(t)
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != DefaultBwrapBin {
		t.Fatalf("bwrap_bin default not applied: %q", argv[0])
	}
	mounts, flags, command := parseMounts(t, argv)

	// The tenant's own writable roots, and the private temp area, are the only
	// writable destinations in the profile.
	var writable []string
	for _, m := range mounts {
		if m.flag == "--bind" || m.flag == "--dev-bind" || m.flag == "--tmpfs" {
			writable = append(writable, m.dst)
		}
	}
	want := []string{"/home", "/root", "/tmp", "/var", "/srv", "/etc/dshgw", f.alice.Workspace, filepath.Dir(f.alice.DshHome)}
	if strings.Join(writable, " ") != strings.Join(want, " ") {
		t.Fatalf("writable/tmpfs mounts drifted:\n got %v\nwant %v", writable, want)
	}

	// /etc is never exposed as a tree, only as the named runtime files.
	for _, m := range mounts {
		if m.dst == "/etc" {
			t.Fatalf("/etc is mounted wholesale: %+v", m)
		}
	}
	allowedEtc := map[string]bool{
		"/etc/resolv.conf": true, "/etc/hosts": true, "/etc/nsswitch.conf": true,
		"/etc/passwd": true, "/etc/group": true, "/etc/localtime": true,
		"/etc/ssl": true, "/etc/ca-certificates": true,
		// The alternatives database: /usr/bin/{pager,awk,which,…} are symlinks
		// into it, so binding it is what keeps those names resolvable.
		"/etc/alternatives": true,
		// The interactive-shell startup file: /etc does not carry the host's
		// bash startup files, so without this one a terminal inside the sandbox
		// has no aliases, no LS_COLORS and a plain prompt — no colour at all.
		"/etc/bash.bashrc": true,
	}
	for _, m := range mounts {
		if strings.HasPrefix(m.dst, "/etc/") && m.dst != "/etc/dshgw" && !allowedEtc[m.dst] {
			t.Fatalf("unexpected /etc mount %q", m.dst)
		}
	}

	// The loader's and the shell's own directories must be present as real
	// directories, or nothing dynamically linked can start and no `#!/bin/sh`
	// script (including Node's default child-process shell) exists.
	for _, dst := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"} {
		found := false
		for _, m := range mounts {
			if (m.flag == "--ro-bind" || m.flag == "--ro-bind-try") && m.dst == dst {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing read-only runtime mount %s in %v", dst, mounts)
		}
	}

	// The per-tenant configuration directory holds gateway.key and tenant.env.
	// systemd reads the environment file from the host before the sandbox
	// exists, so nothing in that directory needs to be mounted — and mounting it
	// would hand the shared worker account a key it cannot read in the
	// per-tenant-account mode.
	for _, m := range mounts {
		if m.src != "" && (within(f.rt.TenantConfigRoot, m.src) || within(f.rt.TenantConfigRoot, m.dst)) {
			t.Fatalf("tenant configuration is mounted into the sandbox: %+v", m)
		}
	}

	// No host-root passthrough, and no network unshare (the worker dials aigw).
	for _, m := range mounts {
		if m.dst == "/" || m.src == "/" {
			t.Fatalf("profile binds the host root: %+v", m)
		}
	}
	for _, flag := range flags {
		if flag == "--share-net" || flag == "--unshare-net" {
			t.Fatalf("network namespace flag %q is not part of the profile", flag)
		}
	}
	if !hasFlag(flags, "--unshare-pid") || !hasFlag(flags, "--die-with-parent") {
		t.Fatalf("process-view flags missing: %v", flags)
	}

	// The command is exactly node + dsh launcher, and the dsh release symlink is
	// bound by its resolved target so the sandbox never contains a dangling
	// /current link.
	if len(command) != 2 || command[0] != f.rt.NodeBin || command[1] != f.rt.BinJS {
		t.Fatalf("unexpected command: %v", command)
	}
	resolvedRelease := filepath.Join(f.root, "dsh/releases/v1")
	var boundRelease, boundNode bool
	for _, m := range mounts {
		if m.flag == "--ro-bind" && m.dst == resolvedRelease {
			boundRelease = true
		}
		if m.flag == "--ro-bind" && m.dst == filepath.Join(f.root, "dsh/node") {
			boundNode = true
		}
	}
	if !boundRelease || !boundNode {
		t.Fatalf("release/node roots not bound (release=%v node=%v): %v", boundRelease, boundNode, mounts)
	}
}

func TestProfileBindsSharedReleaseOnce(t *testing.T) {
	f := newProfileFixture(t)
	// A deployment where node and dsh share one tree must not emit two identical
	// mounts (the second would shadow the first with the same content, but the
	// profile should stay minimal and reviewable).
	f.rt.CurrentLink = filepath.Join(f.root, "dsh/node")
	f.rt.BinJS = filepath.Join(f.root, "dsh/node/lib/bin.js")
	mustMkdir(t, filepath.Join(f.root, "dsh/node/lib"), 0o755)
	mustWrite(t, filepath.Join(f.root, "dsh/node/lib/bin.js"), "// dsh\n", 0o644)
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	seen := map[string]int{}
	for _, m := range mounts {
		if m.flag == "--ro-bind" {
			seen[m.dst]++
		}
	}
	for dst, count := range seen {
		if count > 1 {
			t.Fatalf("duplicate read-only mount of %s (%d)", dst, count)
		}
	}
}

func TestProfileBindsTheShellStartupFileOnlyWhenItIsRendered(t *testing.T) {
	f := newProfileFixture(t)
	// No view, no bind: unlike /etc/passwd there is no host file to fall back to, so the
	// sandbox simply has no /etc/bash.bashrc — and the profile must not invent one.
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), "/etc/bash.bashrc") {
		t.Fatalf("profile binds a shell startup file nobody rendered:\n%s", strings.Join(argv, " "))
	}

	view := filepath.Join(f.root, "state/sandbox/alice/bashrc")
	mustMkdir(t, filepath.Dir(view), 0o700)
	mustWrite(t, view, Bashrc, 0o644)
	f.alice.BashrcFile = view
	argv, err = Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	var found *mount
	for i := range mounts {
		if mounts[i].dst == "/etc/bash.bashrc" {
			found = &mounts[i]
		}
	}
	if found == nil {
		t.Fatalf("no /etc/bash.bashrc mount in %v", argv)
	}
	// Strict --ro-bind, not --ro-bind-try: the gateway wrote this file moments before the
	// profile was built, so a skipped bind is a real fault — and it is the colourless
	// terminal this mount exists to remove.
	if found.flag != "--ro-bind" || found.src != view {
		t.Fatalf("/etc/bash.bashrc mounted as %s %s, want --ro-bind %s", found.flag, found.src, view)
	}
}

func TestProfileAppendsTenantEnvironmentWithoutShellQuoting(t *testing.T) {
	f := newProfileFixture(t)
	f.alice.Environment = []string{"web", "--port", "32100", "--no-open"}
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	_, _, command := parseMounts(t, argv)
	want := []string{f.rt.NodeBin, f.rt.BinJS, "web", "--port", "32100", "--no-open"}
	if strings.Join(command, " ") != strings.Join(want, " ") {
		t.Fatalf("command = %v, want %v", command, want)
	}
}

func TestProfileRejectsUnsafeInputs(t *testing.T) {
	f := newProfileFixture(t)
	cases := []struct {
		name   string
		mutate func(*Runtime, *Tenant)
	}{
		{"tenant name with separator", func(_ *Runtime, tn *Tenant) { tn.Name = "../evil" }},
		{"tenant name with space", func(_ *Runtime, tn *Tenant) { tn.Name = "al ice" }},
		{"empty tenant name", func(_ *Runtime, tn *Tenant) { tn.Name = "" }},
		{"workspace is the host root", func(_ *Runtime, tn *Tenant) { tn.Workspace = "/" }},
		{"workspace outside workspace_root", func(_ *Runtime, tn *Tenant) { tn.Workspace = "/etc" }},
		{"workspace equals workspace_root", func(rt *Runtime, tn *Tenant) { tn.Workspace = rt.WorkspaceRoot }},
		{"dsh home outside tenant_root", func(_ *Runtime, tn *Tenant) { tn.DshHome = "/var/lib/other/.dsh" }},
		{"dsh home is tenant_root itself", func(rt *Runtime, tn *Tenant) { tn.DshHome = filepath.Join(rt.TenantRoot, ".dsh") }},
		{"relative node bin", func(rt *Runtime, _ *Tenant) { rt.NodeBin = "node" }},
		{"unclean node bin", func(rt *Runtime, _ *Tenant) { rt.NodeBin = rt.NodeBin + "/../bin/node" }},
		{"newline in environment", func(_ *Runtime, tn *Tenant) { tn.Environment = []string{"x\ny"} }},
		{"invalid worker port", func(_ *Runtime, tn *Tenant) { tn.WorkerPort = 70000 }},
		{"relative bashrc view", func(_ *Runtime, tn *Tenant) { tn.BashrcFile = "sandbox/bashrc" }},
		{"unclean bashrc view", func(_ *Runtime, tn *Tenant) { tn.BashrcFile = tn.DshHome + "/sandbox/../bashrc" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, tn := f.rt, f.alice
			tc.mutate(&rt, &tn)
			if _, err := Profile(rt, tn); err == nil {
				t.Fatal("unsafe profile accepted")
			}
		})
	}
}

func TestProfileRejectsMissingInstallations(t *testing.T) {
	f := newProfileFixture(t)
	rt := f.rt
	rt.NodeBin = filepath.Join(f.root, "dsh/node/bin/absent")
	if _, err := Profile(rt, f.alice); err == nil {
		t.Fatal("profile built for a missing node binary")
	}
	rt = f.rt
	rt.CurrentLink = filepath.Join(f.root, "dsh/absent")
	if _, err := Profile(rt, f.alice); err == nil {
		t.Fatal("profile built for a missing release")
	}
}

func TestValidateRuntimeRequiresLoaderDirectories(t *testing.T) {
	f := newProfileFixture(t)
	if err := ValidateRuntime(f.rt); err != nil {
		t.Fatalf("real host layout rejected: %v", err)
	}
	rt := f.rt
	rt.BwrapBin = "bwrap"
	if err := ValidateRuntime(rt); err == nil {
		t.Fatal("relative bwrap_bin accepted")
	}
}

func TestValidateWorkerAccount(t *testing.T) {
	if err := ValidateWorkerAccount(""); err == nil {
		t.Fatal("empty worker_user accepted")
	}
	if err := ValidateWorkerAccount("root"); err == nil {
		t.Fatal("root worker_user accepted")
	}
	if err := ValidateWorkerAccount("dshgw-definitely-absent-account"); err == nil {
		t.Fatal("missing worker_user accepted")
	}
}

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

func TestProfileBindsEverySSHMountInsideTheWorkspace(t *testing.T) {
	f := newProfileFixture(t)
	mountpoint := filepath.Join(f.alice.Workspace, "ssh", "gpt001", "opt", "app")
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	f.alice.SSHMounts = []string{mountpoint}
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	var found *mount
	for i := range mounts {
		if mounts[i].flag == "--bind-try" && mounts[i].dst == mountpoint {
			found = &mounts[i]
		}
	}
	if found == nil {
		t.Fatalf("no --bind-try for the ssh mount in %v", argv)
	}
	if found.src != mountpoint {
		t.Errorf("ssh mount bound %s -> %s, want the mount point on both sides", found.src, found.dst)
	}
	// --bind-try, not --bind: a record whose mount has not been re-made yet must not keep
	// the whole worker from starting.
	for _, m := range mounts {
		if m.flag == "--bind" && m.dst == mountpoint {
			t.Error("the ssh mount is bound with --bind, which fails when the mount is absent")
		}
	}

	// A mount record that points outside the account's workspace is refused outright: the
	// registry is data, not authority.
	f.alice.SSHMounts = []string{filepath.Join(f.root, "srv", "bob", "ssh", "gpt001", "opt")}
	if _, err := Profile(f.rt, f.alice); err == nil {
		t.Fatal("a mount point outside the workspace was accepted")
	}
}

// M79: a deployment may name a short path for the workspace (deploy.sandbox_workspace). The
// workspace is then bound twice — the host path the gateway's own state already names, and the
// view a tenant reads — and every mount inside it is mirrored, because bubblewrap's --bind does
// not carry submounts.
func TestProfileWorkspaceViewBindsBothPathsAndMirrorsItsMounts(t *testing.T) {
	f := newProfileFixture(t)
	browserRoot := filepath.Join(f.alice.Workspace, "browser")
	picked := filepath.Join(browserRoot, "peer")
	mustMkdir(t, picked, 0o700)
	shareRoot := filepath.Join(f.alice.Workspace, "host-shares")
	mustMkdir(t, shareRoot, 0o700)
	shareSource := filepath.Join(f.root, "operator-data")
	mustMkdir(t, shareSource, 0o755)
	sshMount := filepath.Join(f.alice.Workspace, "ssh", "gpt001", "opt", "app")
	mustMkdir(t, sshMount, 0o700)

	alice := f.alice
	alice.WorkspaceView = "/workspace"
	alice.BrowserMountRoot = browserRoot
	alice.BrowserMounts = []string{picked}
	alice.HostShareRoot = shareRoot
	alice.HostShares = []HostShare{{
		Name: "data", Source: shareSource, Target: filepath.Join(shareRoot, "data"), ReadOnly: true,
	}}
	alice.SSHMounts = []string{sshMount}

	argv, err := Profile(f.rt, alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, flags, _ := parseMounts(t, argv)
	byDestination := map[string]mount{}
	for _, m := range mounts {
		byDestination[m.dst] = m
	}
	for _, want := range []struct {
		label string
		flag  string
		src   string
		dst   string
	}{
		{"the host workspace path", "--bind", alice.Workspace, alice.Workspace},
		{"the workspace view", "--bind", alice.Workspace, "/workspace"},
		{"the browser container", "--ro-bind", browserRoot, browserRoot},
		{"the mirrored browser container", "--ro-bind", browserRoot, "/workspace/browser"},
		{"a browser pick", "--bind", picked, picked},
		{"the mirrored browser pick", "--bind", picked, "/workspace/browser/peer"},
		{"the host share container", "--ro-bind", shareRoot, shareRoot},
		{"the mirrored host share container", "--ro-bind", shareRoot, "/workspace/host-shares"},
		{"an operator host share", "--ro-bind-try", shareSource, filepath.Join(shareRoot, "data")},
		{"the mirrored host share", "--ro-bind-try", shareSource, "/workspace/host-shares/data"},
		{"an ssh mount", "--bind-try", sshMount, sshMount},
		{"the mirrored ssh mount", "--bind-try", sshMount, "/workspace/ssh/gpt001/opt/app"},
	} {
		got, ok := byDestination[want.dst]
		if !ok {
			t.Errorf("%s is not mounted at %s:\n%v", want.label, want.dst, argv)
			continue
		}
		if got.flag != want.flag || got.src != want.src {
			t.Errorf("%s: %s %s -> %s, want %s %s", want.label, got.flag, got.src, got.dst, want.flag, want.src)
		}
	}
	// The process starts in the view: without --chdir the worker's own cwd — and everything a
	// fresh dsh derives from a process cwd — would still print the long path.
	if !hasFlag(flags, "--chdir") {
		t.Fatalf("a configured view does not change the working directory:\n%v", argv)
	}
	for i, flag := range flags {
		if flag == "--chdir" && (i+1 >= len(flags) || flags[i+1] != "/workspace") {
			t.Fatalf("--chdir does not name the view:\n%v", argv)
		}
	}
}

// Without a configured view the profile keeps the single-view shape every deployment had before
// M79: no second bind, no --chdir, nothing that names a path the host does not have.
func TestProfileWithoutAWorkspaceViewKeepsTheSingleView(t *testing.T) {
	f := newProfileFixture(t)
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range argv {
		if strings.Contains(arg, "/workspace") {
			t.Fatalf("an unconfigured profile names a view path (%q):\n%v", arg, argv)
		}
		if arg == "--chdir" {
			t.Fatalf("an unconfigured profile changes the working directory:\n%v", argv)
		}
	}
}

func TestValidateWorkspaceView(t *testing.T) {
	for _, value := range []string{
		"/",                                                    // the filesystem root: the whole sandbox would be the workspace
		"/home", "/root", "/tmp", "/var", "/srv", "/etc/dshgw", // the hidden roots themselves
		"/home/winger/workspace", // inside one: the bind would put a tree back into it
		"/etc/passwd",            // a file the profile mounts for itself
		"/usr", "/usr/local/ws", "/bin", "/lib64", "/proc/ws", "/dev/ws",
		"workspace", "./workspace", // relative
		"/workspace/",      // not clean
		"/workspace/../ws", // not clean either
		" /workspace",      // leading space: not the argv element an operator thinks it is
		"/workspace\n/etc", // a newline can never reach an argv element
	} {
		if err := ValidateWorkspaceView(value); err == nil {
			t.Errorf("ValidateWorkspaceView(%q) accepted an unusable view", value)
		}
	}
	for _, value := range []string{"", "/workspace", "/ws", "/dsh-view/work"} {
		if err := ValidateWorkspaceView(value); err != nil {
			t.Errorf("ValidateWorkspaceView(%q) rejected a usable view: %v", value, err)
		}
	}
}

// The view is deployment configuration, but a profile that mounted the workspace onto its own
// runtime would be unusable in a way that blames bubblewrap. The collisions only a profile can
// see (its node root and its dsh release) are refused here; the rest is refused at load time.
func TestProfileRejectsWorkspaceViewsThatOverlapWhatItMounts(t *testing.T) {
	f := newProfileFixture(t)
	for _, view := range []string{
		f.alice.Workspace,                        // the workspace itself
		filepath.Join(f.alice.Workspace, "view"), // inside it
		filepath.Dir(f.alice.Workspace),          // an ancestor: the bind would move the whole tree
		filepath.Join(f.root, "dsh/node"),        // the node runtime
		filepath.Join(f.root, "dsh/releases/v1"), // the dsh release
	} {
		alice := f.alice
		alice.WorkspaceView = view
		if _, err := Profile(f.rt, alice); err == nil {
			t.Errorf("workspace view %s was accepted", view)
		}
	}
}

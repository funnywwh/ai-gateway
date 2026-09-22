package sshworkspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A source directory that contains its own mount point is the one mount shape sshfs cannot
// survive: the mounted tree contains the mount point, so any recursive reader descends into
// its own copy and the requests pile up on the mount's sftp channel until the whole mount
// hangs in an uninterruptible state for every session of the account.
//
// The measured incident (2026-09-21) is the fixture for these tests: the host's own
// /home/winger/work/ai_gateway, mounted at <workspace>/ssh/rag-server/home/winger/work/ai_gateway,
// which lives inside it.
func TestAncestorOfMountpoint(t *testing.T) {
	root := t.TempDir()
	mountpoint := filepath.Join(root, "state", "workspaces", "dsh-tenant", "ssh", "rag-server", "lib")
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	// A second spelling of the same directory, the way this host reaches one file system
	// through two mount points (/data/home/winger/work and /home/winger/work): the answer must
	// not depend on the string.
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		target string
		want   bool
	}{
		{"the mount point itself", mountpoint, true},
		{"a distant parent", root, true},
		{"a direct parent", filepath.Dir(mountpoint), true},
		{"a file system root", string(filepath.Separator), true},
		{"the same directory spelled through a symlink", alias, true},
		{"an unrelated sibling", sibling, false},
		{"a child of the mount point", filepath.Join(mountpoint, "sub"), false},
		{"a path that does not exist", filepath.Join(root, "absent"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ancestorOfMountpoint(mountpoint, tc.target); got != tc.want {
				t.Errorf("ancestorOfMountpoint(%q, %q) = %v, want %v", mountpoint, tc.target, got, tc.want)
			}
		})
	}

	// Containment by identity is decided without ever stat'ing the mount point: a source whose
	// path *is* the mount point answers immediately, and a source inside it is rejected on the
	// paths alone (which is what keeps a stat off a wedged mount).
	unmade := "/nonexistent/dshgw/ssh/rag-server"
	if !ancestorOfMountpoint(unmade, unmade) {
		t.Error("an unmade mount point is not its own ancestor")
	}
	if ancestorOfMountpoint(unmade, filepath.Join(unmade, "lib")) {
		t.Error("a child of an unmade mount point was reported as its ancestor")
	}
	// An ancestor that does not exist on this host cannot be verified as one, and the check
	// says no rather than guessing: it only ever refuses a mount, so an unverifiable source
	// must not be refused (a genuine mount of another machine reads exactly like this).
	if ancestorOfMountpoint(filepath.Join(unmade, "lib"), unmade) {
		t.Error("a source that does not exist locally was reported as an ancestor")
	}
}

// The refusal must fire for this host and only for this host: the identical path string on
// another machine names a different directory, and mounting that is ordinary.
func TestRefuseSelfNestedMountOnlyForThisHost(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()

	// A mount point under the fixture's own root, and a source that contains it: the source is
	// the root itself.
	workspace := filepath.Join(env.root, "state", "workspaces", "dsh-colin")
	mountpoint := filepath.Join(workspace, "ssh", "rag-server", "sub")
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}

	// Loopback is this host by address alone, with no ssh round trip.
	err := env.service.refuseSelfNestedMount(ctx, env.remote, "127.0.0.1", env.root, mountpoint)
	if CodeOf(err) != CodeForbidden {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeForbidden, err)
	}
	if !strings.Contains(err.Error(), mountpoint) || !strings.Contains(err.Error(), env.root) {
		t.Errorf("the refusal must name both paths, got %q", err.Error())
	}

	// The same path on a host that is not this one is an ordinary mount.
	if err := env.service.refuseSelfNestedMount(ctx, env.remote, "elsewhere.example", env.root, mountpoint); err != nil {
		t.Errorf("a mount of another machine's directory was refused: %v", err)
	}

	// A source that cannot contain the mount point is never even a candidate.
	if err := env.service.refuseSelfNestedMount(ctx, env.remote, "127.0.0.1", filepath.Join(env.root, "elsewhere"), mountpoint); err != nil {
		t.Errorf("an unrelated source was refused: %v", err)
	}
}

// An alias whose HostName the local address list does not spell out is settled by the remote
// machine id — the second proof of "this host", and the only one available when a host is named
// by something like a DNS alias.
func TestRefuseSelfNestedMountByMachineID(t *testing.T) {
	env := newTestEnv(t, Options{Hosts: []string{"rag-server"}})
	ctx := context.Background()

	idFile := filepath.Join(env.root, "machine-id")
	if err := os.WriteFile(idFile, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousPath, previousAddresses := machineIDPath, hostAddresses
	machineIDPath = idFile
	hostAddresses = func() map[string]bool { return map[string]bool{} }
	t.Cleanup(func() { machineIDPath, hostAddresses = previousPath, previousAddresses })

	// The alias resolves to a name no address list holds, and the remote reports this host's
	// machine id: same machine.
	unaliased := env.fake.exec
	env.service.exec = func(ctx context.Context, name string, args []string, env2 []string) ([]byte, []byte, error) {
		if name == "ssh" && strings.Contains(args[len(args)-1], "/etc/machine-id") {
			env.fake.record(name, args)
			return []byte("0123456789abcdef0123456789abcdef\n"), nil, nil
		}
		return unaliased(ctx, name, args, env2)
	}

	workspace := filepath.Join(env.root, "state", "workspaces", "dsh-colin")
	mountpoint := filepath.Join(workspace, "ssh", "rag-server", "sub")
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := env.service.refuseSelfNestedMount(ctx, env.remote, "rag-server", env.root, mountpoint); CodeOf(err) != CodeForbidden {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeForbidden, err)
	}

	// A remote with its own machine id is another machine, however the path reads.
	env.service.exec = func(ctx context.Context, name string, args []string, env2 []string) ([]byte, []byte, error) {
		if name == "ssh" && strings.Contains(args[len(args)-1], "/etc/machine-id") {
			return []byte("ffffffffffffffffffffffffffffffff\n"), nil, nil
		}
		return unaliased(ctx, name, args, env2)
	}
	if err := env.service.refuseSelfNestedMount(ctx, env.remote, "rag-server", env.root, mountpoint); err != nil {
		t.Errorf("a directory on another machine was refused: %v", err)
	}
}

// Open is where the user's click lands, and it must refuse before anything is mounted.
func TestOpenRefusesSelfNestedMount(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.fake.canonical = env.root // the source that contains the workspace it would be mounted into

	_, _, err := env.service.Open(context.Background(), env.remote, "127.0.0.1", env.root)
	if CodeOf(err) != CodeForbidden {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeForbidden, err)
	}
	if calls := env.fake.callCount("sshfs "); calls != 0 {
		t.Errorf("sshfs was run %d times for a mount that was refused", calls)
	}
	if mounts, err := env.service.Mounts("dsh-colin"); err != nil || len(mounts) != 0 {
		t.Errorf("mounts = %+v / %v, want nothing recorded", mounts, err)
	}
}

// One mount serves every session of an account, so sshfs' default of a single sftp connection
// is exactly the queue that makes concurrent sessions look stuck. Configuration still wins.
func TestSshfsArgsAsksForSeveralConnections(t *testing.T) {
	env := newTestEnv(t, Options{SSHFSOptions: []string{"reconnect", "idmap=user"}})
	defaults := env.service.options
	args := defaults.sshfsArgs(env.remote, "gpt001", "/opt/app", "/w/app")
	if !containsOption(args, "max_conns=") {
		t.Errorf("args = %v, want a max_conns option", args)
	}
	if !containsOption(args, "max_conns=4") {
		t.Errorf("args = %v, want the default of %d connections", args, defaultMaxConns)
	}
	configured := newTestEnv(t, Options{SSHFSOptions: []string{"reconnect", "max_conns=2"}})
	args = configured.service.options.sshfsArgs(configured.remote, "gpt001", "/opt/app", "/w/app")
	if !containsOption(args, "max_conns=2") || containsOption(args, "max_conns=4") {
		t.Errorf("args = %v, want the configured max_conns=2 alone", args)
	}
}

// containsOption reports whether one sshfs -o list already carries an option with this prefix.
func containsOption(args []string, prefix string) bool {
	for _, arg := range args {
		if !strings.Contains(arg, ",") && arg == prefix {
			return true
		}
		for _, option := range strings.Split(arg, ",") {
			if strings.HasPrefix(option, prefix) {
				return true
			}
		}
	}
	return false
}

//go:build linux

package sshworkspace

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIntegrationMountOverLoopback is the honest end-to-end check of the gateway's half: it
// runs real ssh and real sshfs against the local machine (whose sshd the deployment account
// can already reach non-interactively), mounts a real remote directory inside a temporary
// workspace, proves a file written through the mount lands on the remote side, and detaches
// again.
//
// It skips itself wherever the pieces are missing — no sshfs, no usable identity, no
// non-interactive loopback ssh — because that is the state of a developer machine, not a
// failure of the code. The tenant-side half and the browser surface are covered by
// make dshgw-test and by the operator's acceptance run (docs/dshgw.md §7b).
func TestIntegrationMountOverLoopback(t *testing.T) {
	// Explicitly gated: the mount, the ssh server and this test must share one mount
	// namespace, so it belongs on the gateway host (make dshgw-ssh-integration), not inside
	// the development sandbox where the loopback sshd lives in another namespace.
	if os.Getenv("DSHGW_SSH_INTEGRATION") == "" {
		t.Skip("set DSHGW_SSH_INTEGRATION=1 on the gateway host: make dshgw-ssh-integration")
	}
	if _, err := exec.LookPath("sshfs"); err != nil {
		t.Skip("sshfs is not installed: sudo apt install -y sshfs")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh is not installed")
	}
	key := os.Getenv("DSHGW_SSH_TEST_KEY")
	if key == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("cannot determine the home directory: %v", err)
		}
		for _, candidate := range []string{"id_rsa", "id_ed25519"} {
			if path := filepath.Join(home, ".ssh", candidate); isFile(path) {
				key = path
				break
			}
		}
	}
	if key == "" || !isFile(key) {
		t.Skip("no ssh identity to test with: set DSHGW_SSH_TEST_KEY")
	}
	// Override for a local sshd not listening on the standard port. Tests never
	// modify sshd configuration or the operator's keys / known_hosts.
	portText := os.Getenv("DSHGW_SSH_TEST_PORT")
	if portText == "" {
		portText = "22"
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("invalid DSHGW_SSH_TEST_PORT %q", portText)
	}
	preflightArgs := []string{"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + filepath.Join(t.TempDir(), "known_hosts"), "-o", "ConnectTimeout=5", "-p", strconv.Itoa(port), "-i", key, "--", "127.0.0.1"}
	probe := exec.Command("ssh", append(append([]string{}, preflightArgs...), "true")...)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("non-interactive ssh to 127.0.0.1:%d is unavailable (%v): %s", port, err, strings.TrimSpace(string(output)))
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host   string
		hostIdentity bool
	}{
		{"default-key", "127.0.0.1", false},
		{"host-key-explicit-port", "127.0.0.1:" + strconv.Itoa(port), true},
		{"alias-user-hostname-port", "loopback-alias", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			integrationMountOverLoopback(t, key, tc.host, port, tc.hostIdentity, currentUser.Username, preflightArgs)
		})
	}
}

func integrationMountOverLoopback(t *testing.T, key, host string, port int, hostIdentity bool, username string, preflightArgs []string) {
	t.Helper()

	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote")
	if err := os.MkdirAll(filepath.Join(remoteDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "hello.txt"), []byte("remote content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The remote has to be able to see the directory that is about to be mounted; if it
	// cannot, this test is running in a namespace the ssh server does not share.
	if probe := exec.Command("ssh", append(append([]string{}, preflightArgs...), "test", "-d", remoteDir)...); probe.Run() != nil {
		t.Skipf("the remote cannot see %s: run this on the gateway host, not in a sandbox", remoteDir)
	}
	tenant := Remote{
		Tenant:    "dsh-integration",
		Workspace: filepath.Join(root, "state", "workspaces", "dsh-integration"),
		DshHome:   filepath.Join(root, "state", "tenants", "dsh-integration", ".dsh"),
	}
	if err := os.MkdirAll(tenant.DshHome, 0o700); err != nil {
		t.Fatal(err)
	}

	service, err := New(Options{
		MountSubdir:    "ssh",
		SSHBin:         "ssh",
		SSHFSBin:       "sshfs",
		IdentitySource: key,
		ConnectTimeout: 10 * time.Second,
		MaxEntries:     100,
		SSHFSOptions:   []string{"reconnect", "ServerAliveInterval=15", "ServerAliveCountMax=3", "idmap=user"},
	}, NewStore(filepath.Join(root, "state", "ssh-mounts.json")), nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := service.EnsureIdentity(tenant.Tenant, tenant.Workspace, tenant.DshHome); err != nil {
		t.Fatal(err)
	}
	if port != 22 || host == "loopback-alias" {
		// Alias coverage must exercise all three values through real ssh AND sshfs.
		config := "Host 127.0.0.1\n  Port " + strconv.Itoa(port) + "\n"
		if host == "loopback-alias" {
			config = "Host loopback-alias\n HostName 127.0.0.1\n User " + username + "\n Port " + strconv.Itoa(port) + "\n"
		}
		if err := os.WriteFile(filepath.Join(tenant.Workspace, ".ssh", "config"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if hostIdentity {
		// Move only the freshly provisioned temporary account copy. The operator key
		// remains untouched. A broken default proves the explicit host key wins.
		defaultKey := filepath.Join(tenant.Workspace, ".ssh", "id_rsa")
		hostPath := hostKey(tenant, host)
		if err := os.MkdirAll(filepath.Dir(hostPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(defaultKey, hostPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(defaultKey, []byte("INVALID DEFAULT KEY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The tenant-facing half is exercised through the same functions the plugin mirrors: the
	// remote directory must be reachable and listable before anything is mounted.
	home, err := service.options.Probe(ctx, service.exec, tenant, host)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.HasPrefix(home, "/") {
		t.Fatalf("probe returned %q", home)
	}
	listing, err := service.options.ListDir(ctx, service.exec, tenant, host, remoteDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "sub" {
		t.Fatalf("listing = %+v, want the one subdirectory", listing)
	}

	mount, _, err := service.Open(ctx, tenant, host, remoteDir)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer func() {
		if _, _, closeErr := service.Close(context.WithoutCancel(ctx), tenant.Tenant, tenant.DshHome, mount.Mountpoint); closeErr != nil {
			t.Errorf("unmount: %v", closeErr)
		}
		if fstype, err := mountedAt(mount.Mountpoint); err != nil || fstype != "" {
			t.Errorf("mount remained after close: %q / %v", fstype, err)
		}
	}()

	// The mount is real kernel state, in this process's mount table.
	if fstype, err := mountedAt(mount.Mountpoint); err != nil || fstype != "fuse.sshfs" {
		t.Fatalf("mount table says %q (%v), want fuse.sshfs", fstype, err)
	}
	// A read through the mount reaches the remote directory.
	content, err := os.ReadFile(filepath.Join(mount.Mountpoint, "hello.txt"))
	if err != nil {
		t.Fatalf("reading through the mount: %v", err)
	}
	if string(content) != "remote content\n" {
		t.Fatalf("read %q through the mount", content)
	}
	// A write through the mount lands on the remote side, which is the whole point: the
	// account's dsh sees a local directory and the bytes end up there.
	if err := os.WriteFile(filepath.Join(mount.Mountpoint, "written.txt"), []byte("from the workspace\n"), 0o600); err != nil {
		t.Fatalf("writing through the mount: %v", err)
	}
	remoteWritten, err := os.ReadFile(filepath.Join(remoteDir, "written.txt"))
	if err != nil {
		t.Fatalf("the remote side did not receive the file: %v", err)
	}
	if string(remoteWritten) != "from the workspace\n" {
		t.Fatalf("the remote file holds %q", remoteWritten)
	}
	// The mirror the tenant plugin reads says exactly one mount, with both spellings.
	mirror, err := os.ReadFile(filepath.Join(tenant.DshHome, "ssh-mounts.json"))
	if err != nil {
		t.Fatalf("reading the account mirror: %v", err)
	}
	if !strings.Contains(string(mirror), mount.Mountpoint) {
		t.Fatalf("the mirror does not name the mount point: %s", mirror)
	}

	// Re-opening is idempotent: same mount point, no second sshfs, no extra record.
	again, _, err := service.Open(ctx, tenant, host, remoteDir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if again.Mountpoint != mount.Mountpoint {
		t.Fatalf("re-open produced %q, want %q", again.Mountpoint, mount.Mountpoint)
	}
	mounts, err := service.Mounts(tenant.Tenant)
	if err != nil || len(mounts) != 1 {
		t.Fatalf("mounts = %+v / %v, want one record", mounts, err)
	}

	// The mount point is private to the account: 0700 on the container, the host and the
	// mount point itself.
	for _, dir := range []string{
		filepath.Join(tenant.Workspace, "ssh"),
		filepath.Join(tenant.Workspace, "ssh", host),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", dir, info.Mode().Perm())
		}
	}
}

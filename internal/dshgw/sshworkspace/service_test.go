package sshworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hostFake stands in for ssh, sshfs and fusermount3, and for the kernel's mount table. Every
// decision this package makes about a remote host is therefore exercised without a network,
// a key or a real FUSE mount.
type hostFake struct {
	mu sync.Mutex
	// home and canonical are what the fake remote reports.
	home      string
	canonical string
	listing   []string
	// sshErr/sshStderr force an ssh failure; sshfsErr forces a mount failure.
	sshErr    error
	sshStderr []byte
	sshfsErr  error
	// unmountErr makes "fusermount3 -u" fail so the lazy fallback is exercised.
	unmountErr error
	// mounts is the fake mount table: mount point -> filesystem type.
	mounts map[string]string
	calls  []string
}

func newHostFake() *hostFake {
	return &hostFake{
		home:      "/home/remote",
		canonical: "/opt/app",
		listing:   []string{"app/", "logs/", ".hidden/", "readme.txt"},
		mounts:    map[string]string{},
	}
}

func (f *hostFake) record(name string, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
}

func (f *hostFake) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func (f *hostFake) exec(_ context.Context, name string, args []string, _ []string) ([]byte, []byte, error) {
	f.record(name, args)
	switch name {
	case "ssh":
		if f.sshErr != nil {
			return nil, f.sshStderr, f.sshErr
		}
		script := args[len(args)-1]
		switch {
		case strings.Contains(script, `printf %s "$HOME"`):
			return []byte(f.home), nil, nil
		case strings.Contains(script, "pwd -P"):
			return []byte(f.canonical + "\n"), nil, nil
		case strings.Contains(script, "ls -1ap"):
			return []byte(strings.Join(f.listing, "\n") + "\n"), nil, nil
		case strings.Contains(script, "mkdir --"):
			return nil, nil, nil
		}
		return nil, nil, nil
	case "sshfs":
		if f.sshfsErr != nil {
			return nil, []byte("fuse: bad mount point"), f.sshfsErr
		}
		f.mu.Lock()
		f.mounts[args[len(args)-1]] = "fuse.sshfs"
		f.mu.Unlock()
		return nil, nil, nil
	case "fusermount3":
		mountpoint := args[len(args)-1]
		if args[0] == "-u" && f.unmountErr != nil {
			return nil, []byte("Device or resource busy"), f.unmountErr
		}
		f.mu.Lock()
		delete(f.mounts, mountpoint)
		f.mu.Unlock()
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected command %q", name)
}

func (f *hostFake) mounted(mountpoint string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mounts[mountpoint], nil
}

type testEnv struct {
	service  *Service
	fake     *hostFake
	root     string
	remote   Remote
	restarts []string
}

// newTestEnv builds a service wired to the fake host. The returned restarts slice records
// every worker restart the service asked for — the mount set only becomes visible inside a
// sandbox through one, so it is part of the contract.
func newTestEnv(t *testing.T, options Options) *testEnv {
	t.Helper()
	root := t.TempDir()
	fake := newHostFake()
	if options.MountSubdir == "" {
		options.MountSubdir = "ssh"
	}
	options.SSHBin = "ssh"
	options.SSHFSBin = "sshfs"
	if options.ConnectTimeout == 0 {
		options.ConnectTimeout = 5 * time.Second
	}
	if len(options.SSHFSOptions) == 0 {
		options.SSHFSOptions = []string{"reconnect"}
	}
	if options.IdentitySource == "" {
		key := filepath.Join(root, "operator-id_rsa")
		if err := os.WriteFile(key, []byte("PRIVATE KEY\n"), 0o600); err != nil {
			t.Fatalf("writing the test key: %v", err)
		}
		options.IdentitySource = key
	}
	env := &testEnv{fake: fake, root: root}
	service, err := New(options, NewStore(filepath.Join(root, "ssh-mounts.json")), func(_ context.Context, tenant string) error {
		env.restarts = append(env.restarts, tenant)
		return nil
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	service.exec = fake.exec
	service.mounted = fake.mounted
	service.lookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	service.now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	env.service = service
	env.remote = Remote{
		Tenant:    "dsh-colin",
		Workspace: filepath.Join(root, "state", "workspaces", "dsh-colin"),
		DshHome:   filepath.Join(root, "state", "tenants", "dsh-colin", ".dsh"),
	}
	if err := os.MkdirAll(env.remote.DshHome, 0o700); err != nil {
		t.Fatalf("preparing the tenant home: %v", err)
	}
	return env
}

func TestOpenMountsRecordsAndRestarts(t *testing.T) {
	env := newTestEnv(t, Options{})
	mountpoint := filepath.Join(env.remote.Workspace, "ssh", "gpt001", "opt", "app")

	mount, restarted, err := env.service.Open(context.Background(), env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !restarted {
		t.Error("Open reported no restart, but a new mount is only visible after the worker starts again")
	}
	if mount.Mountpoint != mountpoint || mount.CanonicalRemote != "/opt/app" || mount.Tenant != "dsh-colin" {
		t.Fatalf("mount record = %+v", mount)
	}
	if !mount.CreatedAt.Equal(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, want the injected clock", mount.CreatedAt)
	}
	recorded, err := env.service.Mounts("dsh-colin")
	if err != nil || len(recorded) != 1 {
		t.Fatalf("recorded mounts = %+v / %v, want exactly one", recorded, err)
	}
	if len(env.restarts) != 1 || env.restarts[0] != "dsh-colin" {
		t.Errorf("restarts = %v, want one restart of dsh-colin", env.restarts)
	}
	// Every level of the mount path is private to the account.
	for _, dir := range []string{
		filepath.Join(env.remote.Workspace, "ssh"),
		filepath.Join(env.remote.Workspace, "ssh", "gpt001"),
		mountpoint,
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", dir, info.Mode().Perm())
		}
	}
	// The identity was provisioned from the configured source and is private.
	key, err := os.Stat(filepath.Join(env.remote.Workspace, ".ssh", "id_rsa"))
	if err != nil {
		t.Fatalf("stat provisioned key: %v", err)
	}
	if key.Mode().Perm() != 0o600 {
		t.Errorf("provisioned key mode = %04o, want 0600", key.Mode().Perm())
	}
}

func TestOpenIsIdempotentWhileMounted(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	first, _, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	second, restarted, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if second.Mountpoint != first.Mountpoint {
		t.Fatalf("second Open produced a different mount point: %q vs %q", second.Mountpoint, first.Mountpoint)
	}
	if restarted {
		t.Error("an already-mounted workspace must not restart the worker again")
	}
	if got := env.fake.callCount("sshfs "); got != 1 {
		t.Errorf("sshfs was invoked %d times, want 1", got)
	}
	if len(env.restarts) != 1 {
		t.Errorf("restarts = %v, want exactly the first open", env.restarts)
	}
}

func TestOpenUsesCanonicalRemotePathAsTheKey(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.fake.canonical = "/srv/real-app"
	// Two spellings that the remote reports as one directory must land on one mount point,
	// otherwise the same tree would be mounted twice under two workspaces.
	directories := filepath.Dir(filepath.Join(env.remote.Workspace, "ssh", "gpt001", "srv", "real-app"))
	mount, _, err := env.service.Open(context.Background(), env.remote, "gpt001", "/srv/link")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if filepath.Dir(mount.Mountpoint) != directories {
		t.Fatalf("mount point %q was not built from the canonical remote path", mount.Mountpoint)
	}
	if mount.CanonicalRemote != "/srv/real-app" || mount.Remote != "/srv/link" {
		t.Fatalf("mount record %+v does not keep both spellings", mount)
	}
}

func TestOpenRefusesUnusableInputs(t *testing.T) {
	env := newTestEnv(t, Options{Hosts: []string{"gpt001"}})
	ctx := context.Background()
	if _, _, err := env.service.Open(ctx, env.remote, "elsewhere", "/opt/app"); CodeOf(err) != CodeHostUnknown {
		t.Errorf("off-list host code = %q, want %q", CodeOf(err), CodeHostUnknown)
	}
	if _, _, err := env.service.Open(ctx, env.remote, "-oProxyCommand=x", "/opt/app"); CodeOf(err) != CodeHostUnknown {
		t.Errorf("option-injection code = %q, want %q", CodeOf(err), CodeHostUnknown)
	}
	if _, _, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/../etc"); CodeOf(err) != CodeInvalidPath {
		t.Errorf("traversal code = %q, want %q", CodeOf(err), CodeInvalidPath)
	}
	if _, _, err := env.service.Open(ctx, Remote{Tenant: "dsh-colin"}, "gpt001", "/opt/app"); CodeOf(err) != CodeInvalidState {
		t.Errorf("unresolved workspace code = %q, want %q", CodeOf(err), CodeInvalidState)
	}
	if mounts, _ := env.service.Mounts("dsh-colin"); len(mounts) != 0 {
		t.Errorf("a refused open recorded something: %+v", mounts)
	}
	if got := env.fake.callCount("sshfs "); got != 0 {
		t.Errorf("sshfs ran %d times for refused opens", got)
	}
}

func TestOpenReportsSSHFailures(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.fake.sshErr = errors.New("exit status 255")
	env.fake.sshStderr = []byte("root@gpt001: Permission denied (publickey).")
	if _, _, err := env.service.Open(context.Background(), env.remote, "gpt001", "/opt/app"); CodeOf(err) != CodeAuthFailed {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeAuthFailed, err)
	}

	env = newTestEnv(t, Options{})
	env.fake.sshErr = errors.New("exit status 255")
	env.fake.sshStderr = []byte("ssh: connect to host gpt001 port 22: Connection refused")
	if _, _, err := env.service.Open(context.Background(), env.remote, "gpt001", "/opt/app"); CodeOf(err) != CodeUnreachable {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeUnreachable, err)
	}
}

func TestOpenDetachesTheMountWhenSSHFSLiesBeyondTheObserver(t *testing.T) {
	// sshfs exits 0 but nothing appears in the mount table: the record must not claim a
	// mount that is not there, and the half-made attempt is detached.
	env := newTestEnv(t, Options{})
	env.service.mounted = func(string) (string, error) { return "", nil }
	_, _, err := env.service.Open(context.Background(), env.remote, "gpt001", "/opt/app")
	if CodeOf(err) != CodeMountFailed {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeMountFailed, err)
	}
	if mounts, _ := env.service.Mounts("dsh-colin"); len(mounts) != 0 {
		t.Errorf("a failed mount was recorded: %+v", mounts)
	}
	if got := env.fake.callCount("fusermount3 -u"); got != 1 {
		t.Errorf("fusermount3 -u ran %d times, want 1 cleanup attempt", got)
	}
	if !strings.Contains(fmt.Sprint(env.restarts), "") || len(env.restarts) != 0 {
		t.Errorf("restarts = %v, want none", env.restarts)
	}
}

func TestOpenFailsWhenSSHFSIsMissing(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.service.lookPath = func(file string) (string, error) {
		if file == "sshfs" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + file, nil
	}
	_, _, err := env.service.Open(context.Background(), env.remote, "gpt001", "/opt/app")
	if CodeOf(err) != CodeSSHFSMissing {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeSSHFSMissing, err)
	}
}

func TestOpenRefusesAForeignMountAtTheSamePlace(t *testing.T) {
	env := newTestEnv(t, Options{})
	mountpoint := filepath.Join(env.remote.Workspace, "ssh", "gpt001", "opt", "app")
	env.fake.mounts[mountpoint] = "ext4"
	_, _, err := env.service.Open(context.Background(), env.remote, "gpt001", "/opt/app")
	if CodeOf(err) != CodeBusy {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeBusy, err)
	}
	if got := env.fake.callCount("sshfs "); got != 0 {
		t.Errorf("sshfs ran %d times over a foreign mount", got)
	}
}

func TestCloseUnmountsForgetsAndRestarts(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	mount, _, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lazy, restarted, err := env.service.Close(ctx, "dsh-colin", env.remote.DshHome, mount.Mountpoint)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if lazy {
		t.Error("a clean unmount reported a lazy detach")
	}
	if !restarted {
		t.Error("Close must restart the worker so the binding goes away with the mount")
	}
	if mounts, _ := env.service.Mounts("dsh-colin"); len(mounts) != 0 {
		t.Errorf("mounts after Close = %+v, want none", mounts)
	}
	if _, statErr := os.Stat(mount.Mountpoint); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("mount point still exists after Close: %v", statErr)
	}
}

func TestCloseLazyFallback(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	mount, _, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	env.fake.unmountErr = errors.New("exit status 1")
	lazy, _, err := env.service.Close(ctx, "dsh-colin", env.remote.DshHome, mount.Mountpoint)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !lazy {
		t.Error("a busy mount must be detached lazily, and say so")
	}
}

func TestCloseRefusesForeignAndUnknownMounts(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	mount, _, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := env.service.Close(ctx, "dsh-other", env.remote.DshHome, mount.Mountpoint); CodeOf(err) != CodeForbidden {
		t.Errorf("cross-account close code = %q, want %q", CodeOf(err), CodeForbidden)
	}
	if _, _, err := env.service.Close(ctx, "dsh-colin", env.remote.DshHome, filepath.Join(env.remote.Workspace, "ssh", "nope")); CodeOf(err) != CodeNotMounted {
		t.Errorf("unknown mount code = %q, want %q", CodeOf(err), CodeNotMounted)
	}
	if mounts, _ := env.service.Mounts("dsh-colin"); len(mounts) != 1 {
		t.Errorf("a refused close changed the record: %+v", mounts)
	}
}

func TestDropTenantUnmountsEverything(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	for _, remote := range []string{"/opt/app", "/srv/data"} {
		env.fake.canonical = remote
		if _, _, err := env.service.Open(ctx, env.remote, "gpt001", remote); err != nil {
			t.Fatalf("Open(%s): %v", remote, err)
		}
	}
	if err := env.service.DropTenant(ctx, "dsh-colin"); err != nil {
		t.Fatalf("DropTenant: %v", err)
	}
	if mounts, _ := env.service.Mounts("dsh-colin"); len(mounts) != 0 {
		t.Errorf("mounts after DropTenant = %+v, want none", mounts)
	}
	if got := env.fake.callCount("fusermount3 -u"); got != 2 {
		t.Errorf("fusermount3 -u ran %d times, want 2", got)
	}
}

func TestPollOnceServicesTheMailbox(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	if err := WriteRequest(env.remote.DshHome, Request{ID: "req-open", Op: RequestOpen, Host: "gpt001", Remote: "/opt/app"}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if handled := env.service.PollOnce(ctx, []Remote{env.remote}); handled != 1 {
		t.Fatalf("PollOnce handled %d requests, want 1", handled)
	}
	mounts, _ := env.service.Mounts("dsh-colin")
	if len(mounts) != 1 {
		t.Fatalf("mounts = %+v, want one", mounts)
	}
	replies, err := ReadReplies(env.remote.DshHome)
	if err != nil || len(replies) != 1 {
		t.Fatalf("replies = %+v / %v, want one", replies, err)
	}
	if !replies[0].OK || replies[0].Mountpoint != mounts[0].Mountpoint || !replies[0].Restarted {
		t.Errorf("reply = %+v", replies[0])
	}
	if remaining, _ := ReadRequests(env.remote.DshHome); len(remaining) != 0 {
		t.Errorf("handled request was not removed: %+v", remaining)
	}
	// The close request round-trips the same way.
	if err := WriteRequest(env.remote.DshHome, Request{ID: "req-close", Op: RequestClose, Mountpoint: mounts[0].Mountpoint}); err != nil {
		t.Fatalf("WriteRequest(close): %v", err)
	}
	if handled := env.service.PollOnce(ctx, []Remote{env.remote}); handled != 1 {
		t.Fatalf("PollOnce handled %d close requests, want 1", handled)
	}
	if left, _ := env.service.Mounts("dsh-colin"); len(left) != 0 {
		t.Errorf("mounts after close = %+v, want none", left)
	}
}

func TestPollOnceReportsFailuresWithoutStopping(t *testing.T) {
	env := newTestEnv(t, Options{Hosts: []string{"gpt001"}})
	if err := WriteRequest(env.remote.DshHome, Request{ID: "req-bad", Op: RequestOpen, Host: "elsewhere", Remote: "/opt/app"}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	// A malformed file must be skipped, not wedge the mailbox.
	if err := os.WriteFile(filepath.Join(RequestDir(env.remote.DshHome), "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing a broken request: %v", err)
	}
	if handled := env.service.PollOnce(context.Background(), []Remote{env.remote}); handled != 1 {
		t.Fatalf("PollOnce handled %d requests, want 1", handled)
	}
	replies, _ := ReadReplies(env.remote.DshHome)
	if len(replies) != 1 || replies[0].OK || replies[0].Code != CodeHostUnknown {
		t.Fatalf("replies = %+v, want one refusal with %s", replies, CodeHostUnknown)
	}
}

func TestReconcileRemountsWhatVanished(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	mount, _, err := env.service.Open(ctx, env.remote, "gpt001", "/opt/app")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A gateway restart loses every mount while the record survives.
	env.fake.mounts = map[string]string{}
	env.service.Reconcile(ctx, []Remote{env.remote})
	if fstype, _ := env.fake.mounted(mount.Mountpoint); fstype != "fuse.sshfs" {
		t.Fatalf("Reconcile did not remount %s (fstype %q)", mount.Mountpoint, fstype)
	}
	// An account that is not running is left alone: its mounts cannot be seen by anyone.
	env.fake.mounts = map[string]string{}
	env.service.Reconcile(ctx, nil)
	if fstype, _ := env.fake.mounted(mount.Mountpoint); fstype != "" {
		t.Error("Reconcile mounted for a tenant it was not given")
	}
}

func TestEnsureIdentityProvisionsOnce(t *testing.T) {
	env := newTestEnv(t, Options{})
	if err := env.service.EnsureIdentity("dsh-colin", env.remote.Workspace, env.remote.DshHome); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	keyPath := filepath.Join(env.remote.Workspace, ".ssh", "id_rsa")
	if _, err := os.Stat(filepath.Join(env.remote.Workspace, ".ssh", "known_hosts")); err != nil {
		t.Fatalf("known_hosts was not created: %v", err)
	}
	// A rotated key must survive provisioning: this step only fills a gap.
	if err := os.WriteFile(keyPath, []byte("TENANT ROTATED KEY\n"), 0o600); err != nil {
		t.Fatalf("rotating the key: %v", err)
	}
	if err := env.service.EnsureIdentity("dsh-colin", env.remote.Workspace, env.remote.DshHome); err != nil {
		t.Fatalf("second EnsureIdentity: %v", err)
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading the key: %v", err)
	}
	if string(data) != "TENANT ROTATED KEY\n" {
		t.Errorf("EnsureIdentity overwrote an existing key: %q", data)
	}
}

func TestEnsureIdentityPrefersTheAccountKey(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "dsh-colin"), []byte("ACCOUNT KEY\n"), 0o600); err != nil {
		t.Fatalf("writing the account key: %v", err)
	}
	env := newTestEnv(t, Options{IdentityDir: directory})
	if err := env.service.EnsureIdentity("dsh-colin", env.remote.Workspace, env.remote.DshHome); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(env.remote.Workspace, ".ssh", "id_rsa"))
	if err != nil {
		t.Fatalf("reading the key: %v", err)
	}
	if string(data) != "ACCOUNT KEY\n" {
		t.Errorf("key = %q, want the account-level key", data)
	}
}

func TestEnsureIdentityRefusesWithoutASource(t *testing.T) {
	env := newTestEnv(t, Options{})
	// The helper seeds a key source for every other test; this one is about what happens
	// when a deployment enables the feature and names none.
	env.service.options.IdentitySource = ""
	if err := env.service.EnsureIdentity("dsh-colin", env.remote.Workspace, env.remote.DshHome); CodeOf(err) != CodeInvalidState {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeInvalidState, err)
	}
}

func TestEnsureIdentityRefusesAWorldReadableKey(t *testing.T) {
	directory := t.TempDir()
	key := filepath.Join(directory, "wide-key")
	if err := os.WriteFile(key, []byte("PRIVATE KEY\n"), 0o644); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	env := newTestEnv(t, Options{IdentitySource: key})
	if err := env.service.EnsureIdentity("dsh-colin", env.remote.Workspace, env.remote.DshHome); CodeOf(err) != CodeInvalidState {
		t.Fatalf("code = %q, want %q (%v)", CodeOf(err), CodeInvalidState, err)
	}
}

func TestStoreRoundTripAndRefusals(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "ssh-mounts.json"))
	if mounts, err := store.Load(); err != nil || len(mounts) != 0 {
		t.Fatalf("Load on a fresh store = %+v / %v, want empty", mounts, err)
	}
	first := Mount{Tenant: "dsh-a", Host: "gpt001", Remote: "/a", CanonicalRemote: "/a", Mountpoint: "/w/a"}
	second := Mount{Tenant: "dsh-b", Host: "aipc", Remote: "/b", CanonicalRemote: "/b", Mountpoint: "/w/b"}
	if _, err := store.Add(first); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.Add(second); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.Add(Mount{Tenant: "dsh-a", Host: "gpt001", Remote: "/a2", CanonicalRemote: "/a2", Mountpoint: "/w/a"}); err != nil {
		t.Fatalf("re-adding one mount point: %v", err)
	}
	mounts, err := store.Load()
	if err != nil || len(mounts) != 2 {
		t.Fatalf("Load = %+v / %v, want two records", mounts, err)
	}
	for _, mount := range mounts {
		if mount.Mountpoint == "/w/a" && mount.Remote != "/a2" {
			t.Errorf("re-adding did not replace the record: %+v", mount)
		}
	}
	if info, err := os.Stat(store.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v / %v, want 0600", info.Mode().Perm(), err)
	}
	if _, err := store.Remove("/w/a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if mounts, _ := store.Load(); len(mounts) != 1 {
		t.Fatalf("Load after Remove = %+v, want one", mounts)
	}
	// A document from another version is refused rather than half-read.
	if err := os.WriteFile(store.Path(), []byte(`{"version":99,"mounts":[]}`), 0o600); err != nil {
		t.Fatalf("writing a future document: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("Load accepted a document from another version")
	}
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing a broken document: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("Load accepted a malformed document")
	}
}

func TestMailboxValidation(t *testing.T) {
	home := t.TempDir()
	for _, req := range []Request{
		{ID: "../escape", Op: RequestOpen, Host: "gpt001", Remote: "/a"},
		{ID: "ok", Op: "delete"},
		{ID: "ok", Op: RequestOpen, Host: "gpt001", Remote: "relative"},
		{ID: "ok", Op: RequestClose, Mountpoint: "relative"},
	} {
		if err := WriteRequest(home, req); err == nil {
			t.Errorf("WriteRequest(%+v) = nil, want a refusal", req)
		}
	}
	if entries, err := os.ReadDir(RequestDir(home)); err == nil && len(entries) != 0 {
		t.Errorf("refused requests left files behind: %+v", entries)
	}
	if err := WriteRequest(home, Request{ID: "good", Op: RequestClose, Mountpoint: "/w/a"}); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(RequestDir(home), "good.json"))
	if err != nil {
		t.Fatalf("reading the request: %v", err)
	}
	var decoded Request
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the request is not valid JSON: %v", err)
	}
	if decoded.CreatedAt.IsZero() {
		t.Error("WriteRequest did not stamp CreatedAt")
	}
}

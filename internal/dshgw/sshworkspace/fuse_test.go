package sshworkspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fuseFixtures points the package's /proc and /sys probes at a directory a test
// controls, so daemon discovery and the abort write can be driven without a real ssh or
// a real mount.
func fuseFixtures(t *testing.T, cmdlines map[int]string, names map[int]string) {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(proc, 0o700); err != nil {
		t.Fatal(err)
	}
	for pid, cmdline := range cmdlines {
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// A real cmdline is NUL-separated per argument, terminated by a NUL.
		argv := strings.Split(cmdline, " ")
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		name := names[pid]
		if name == "" {
			name = "sshfs"
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setUnexported(t, &procRoot, proc)
	setUnexported(t, &sysfsFuse, filepath.Join(root, "fuse"))
}

// fuseConnFixtures builds the sysfs tree fuseAbort writes into and states which mount
// points hold a FUSE file system and with which connection id.
func fuseConnFixtures(t *testing.T, types map[string]string, devices map[string]uint64, minors ...int) string {
	t.Helper()
	root := t.TempDir()
	sysfs := filepath.Join(root, "fuse")
	for _, minor := range minors {
		dir := filepath.Join(sysfs, strconv.Itoa(minor))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// The kernel's entry is write-only (0200); a test has to read it back.
		if err := os.WriteFile(filepath.Join(dir, "abort"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setUnexported(t, &sysfsFuse, sysfs)
	previousMounted := serviceMounted
	serviceMounted = func(mountpoint string) (string, error) { return types[mountpoint], nil }
	t.Cleanup(func() { serviceMounted = previousMounted })
	previousStat := statDevice
	statDevice = func(path string) (uint64, error) {
		device, ok := devices[path]
		if !ok {
			return 0, errors.New("no such mount here")
		}
		return device, nil
	}
	t.Cleanup(func() { statDevice = previousStat })
	return sysfs
}

// fuseDevice is the device number of a FUSE connection id, in the kernel's encoding.
func fuseDevice(minor int) uint64 { return uint64(minor&0xff) | uint64(minor&0xfff00)<<12 }

// setUnexported swaps one package-level seam for the duration of a test.
func setUnexported(t *testing.T, target any, root string) {
	t.Helper()
	pointer := reflect.ValueOf(target)
	if pointer.Kind() != reflect.Ptr || pointer.Elem().Kind() != reflect.String {
		t.Fatalf("setUnexported needs a *string, got %T", target)
	}
	previous := pointer.Elem().String()
	pointer.Elem().SetString(root)
	t.Cleanup(func() { pointer.Elem().SetString(previous) })
}

// The daemon is identified by the mount point that ends its command line — the exact
// shape this package builds — and never by a PID alone: a recycled PID must not become
// a killed process.
func TestSSHFSDaemonForMatchesTheMountPointExactly(t *testing.T) {
	mount := "/srv/alice/ssh/host/home"
	other := "/srv/alice/ssh/host/home/other"
	fuseFixtures(t, map[int]string{
		4711: "/usr/bin/sshfs -o reconnect host:/home " + mount,
		4712: "/usr/bin/sshfs -o reconnect host:/ " + other,
		4713: "/usr/bin/sshfs -o reconnect host:/home " + mount + "/nested",
		4714: "/usr/bin/sshfs -o reconnect host:/home " + mount,
	}, map[int]string{
		// The same argv under a different program name is not an sshfs daemon.
		4711: "ssh",
		4712: "sshfs",
		4713: "sshfs",
		4714: "sshfs",
	})

	if got := findSSHFSDaemon(mount); got != 4714 {
		t.Fatalf("daemon for %s = %d, want 4714 (a parent path or a path prefix must not match)", mount, got)
	}
	if got := findSSHFSDaemon(other); got != 4712 {
		t.Fatalf("daemon for %s = %d, want 4712", other, got)
	}
	if got := findSSHFSDaemon("/srv/nobody"); got != 0 {
		t.Fatalf("an unmounted path reported daemon %d", got)
	}
}

// killSSHFSDaemon must reach the daemon the mount point names — this is the operation
// that releases readers parked on it — and must report honestly when there is none.
func TestKillSSHFSDaemonSignalsOnlyTheMatch(t *testing.T) {
	mount := "/srv/bob/ssh/host/home"
	// A real process under the daemon's own name: the signal has to reach an actual PID,
	// and killing the wrong one is the failure this guards against.
	sshfsBin := filepath.Join(t.TempDir(), "sshfs")
	if err := os.Symlink("/bin/sleep", sshfsBin); err != nil {
		t.Skipf("cannot stage an sshfs-named process: %v", err)
	}
	daemon := exec.Command(sshfsBin, "-o", "reconnect", "host:/home", mount)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
		_ = daemon.Wait()
	})
	fuseFixtures(t, map[int]string{
		daemon.Process.Pid: sshfsBin + " -o reconnect host:/home " + mount,
	}, map[int]string{daemon.Process.Pid: "sshfs"})

	if !killSSHFSDaemon(mount) {
		t.Fatal("the daemon serving the mount was not found")
	}
	// Reaping is the test's job: until it waits, the daemon is a zombie and still
	// answerable to kill(pid, 0).
	reaped := make(chan error, 1)
	go func() { reaped <- daemon.Wait() }()
	select {
	case err := <-reaped:
		if err == nil {
			t.Fatal("the daemon exited cleanly; it was not killed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon survived SIGKILL")
	}
	if killSSHFSDaemon("/srv/bob/ssh/absent") {
		t.Fatal("a path with no daemon reported one")
	}
}

// A mount that survives every unmount attempt is a wedge, and the wedge is what keeps a
// reader inside the tenant's sandbox in an uninterruptible wait. The service must break
// it — killing the sshfs daemon whose pending request is the wait — and then unmount.
//
// The fixture is the real thing: a local sshfs mount, its daemon suspended so one
// request can never be answered, and a reader parked on it. Without the fix that reader
// is unkillable and the mount cannot be detached.
func TestBreakWedgeReleasesAWedgedMount(t *testing.T) {
	if _, err := exec.LookPath("sshfs"); err != nil {
		t.Skip("sshfs is not installed")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		t.Skip("fusermount3 is not installed")
	}
	if out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=5", "127.0.0.1", "true").CombinedOutput(); err != nil {
		t.Skipf("localhost ssh is unavailable here: %v %s", err, out)
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote")
	mount := filepath.Join(root, "mount")
	for _, dir := range []string{remote, mount} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(remote, "file"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountCmd := exec.Command("sshfs", "-o", "BatchMode=yes,StrictHostKeyChecking=accept-new", "winger@127.0.0.1:"+remote, mount)
	if out, err := mountCmd.CombinedOutput(); err != nil {
		t.Skipf("cannot make a local sshfs mount here: %v %s", err, out)
	}
	t.Cleanup(func() {
		if daemon := sshfsDaemonFor(mount); daemon > 0 {
			_ = syscall.Kill(daemon, syscall.SIGKILL)
		}
		_ = exec.Command("fusermount3", "-z", mount).Run()
	})

	service, err := New(Options{MountSubdir: "ssh", ConnectTimeout: 5 * time.Second}, NewStore(filepath.Join(root, "mounts.json")), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	daemon := sshfsDaemonFor(mount)
	if daemon == 0 {
		t.Fatalf("no sshfs daemon found for %s", mount)
	}
	// Suspending the daemon wedges the mount: its requests are accepted and never
	// answered, which is the state an unreachable host produces on its own.
	if err := syscall.Kill(daemon, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, _ = os.ReadDir(mount)
	}()
	// The reader must be stuck before the wedge-breaking is worth anything.
	select {
	case <-readerDone:
		t.Skip("a read on the suspended mount returned; this host cannot reproduce a wedged mount")
	case <-time.After(300 * time.Millisecond):
	}
	// A wedged mount does not unmount: the unmount is served by the same connection.
	if _, err := service.options.unmount(context.Background(), service.exec, mount); err == nil {
		t.Skip("the suspended mount still unmounted; this host cannot reproduce a wedged mount")
	}

	broken, err := service.breakWedge(context.Background(), mount)
	if err != nil {
		t.Fatalf("breaking the wedge failed: %v", err)
	}
	if !broken {
		t.Fatal("a wedged mount was not reported as broken")
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader is still waiting after the wedge was broken")
	}
	if _, err := service.options.unmount(context.Background(), service.exec, mount); err != nil {
		t.Fatalf("the mount did not detach after the wedge was broken: %v", err)
	}
	if fstype, _ := service.mounted(mount); fstype != "" {
		t.Fatalf("%s is still mounted as %s", mount, fstype)
	}
}

// The abort is the last resort that reaches waiters the daemon cannot: its sysfs entry
// is write-only, and the connection to abort is named by the device number in the mount
// table.
func TestFuseAbortWritesTheConnectionEntry(t *testing.T) {
	mount := "/srv/alice/ssh/host/home"
	sysfs := fuseConnFixtures(t, map[string]string{mount: "fuse.sshfs"}, map[string]uint64{mount: fuseDevice(834)}, 834)

	if conn, ok := fuseConnAt(serviceMounted, mount); !ok || conn.Minor != 834 {
		t.Fatalf("connection for %s = %+v ok=%v, want minor 834", mount, conn, ok)
	}
	if _, ok := fuseConnAt(serviceMounted, "/srv/absent"); ok {
		t.Fatal("a path that cannot be stat'ed was reported as a FUSE connection")
	}
	if err := fuseAbort(mount); err != nil {
		t.Fatalf("abort failed: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(sysfs, "834", "abort"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "1" {
		t.Fatalf("abort entry holds %q, want \"1\"", written)
	}
	// A connection the kernel has already collected is not a failure: there is nothing
	// left to abort, and the caller's unmount can proceed.
	if err := fuseAbort("/srv/host/home"); err != nil {
		t.Fatalf("aborting a path with no connection failed: %v", err)
	}
}

// A mount the probe cannot read is not a connection, and an unknown connection must not
// become a wrong abort.
func TestFuseAbortIgnoresUntrackedPaths(t *testing.T) {
	fuseConnFixtures(t, map[string]string{"/srv/alice/ssh/host/home": "fuse.sshfs"}, map[string]uint64{"/srv/alice/ssh/host/home": fuseDevice(12)}, 12)
	if err := fuseAbort("/srv/bob/ssh/host/home"); err != nil {
		t.Fatalf("aborting a path with no mount entry failed: %v", err)
	}
}

// A mount whose connection entry is gone leaves the caller with nothing to release, and
// fuseAbort must say so rather than fail an unmount that can now succeed.
func TestFuseAbortReportsAMissingConnection(t *testing.T) {
	mount := "/srv/alice/ssh/host/home"
	fuseConnFixtures(t, map[string]string{mount: "fuse.sshfs"}, map[string]uint64{mount: fuseDevice(99)})
	if err := fuseAbort(mount); err != nil {
		t.Fatalf("a missing connection entry must be tolerated, got %v", err)
	}
}

// The kernel's sysfs entry is the liveness signal for a mount: while it exists the daemon
// holds the connection, and when it is gone every read on the mount fails with ENOTCONN.
func TestConnectionLiveFollowsTheSysfsEntry(t *testing.T) {
	live, dead := "/srv/alice/ssh/host/live", "/srv/alice/ssh/host/dead"
	fuseConnFixtures(t, map[string]string{live: "fuse.sshfs", dead: "fuse.sshfs"},
		map[string]uint64{live: fuseDevice(41), dead: fuseDevice(42)}, 41)
	if !connectionLive(serviceMounted, live) {
		t.Fatal("a mount with a live connection was reported dead")
	}
	if connectionLive(serviceMounted, dead) {
		t.Fatal("a mount whose connection is gone was reported live")
	}
	if !connectionLive(serviceMounted, "/srv/not/a/mount") {
		t.Fatal("a path that is not a FUSE mount was reported dead")
	}
}

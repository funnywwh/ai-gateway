package fusekernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// procFixture writes a mount table a test controls and returns the proc root to read it from.
func procFixture(t *testing.T, lines ...string) string {
	t.Helper()
	root := t.TempDir()
	self := filepath.Join(root, "self")
	if err := os.MkdirAll(self, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(self, "mounts"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The mount table escapes spaces and backslashes; a path that is not decoded would never match
// the mount point the caller asks about, and the mount would look absent.
func TestMountedAtDecodesEscapedPaths(t *testing.T) {
	root := procFixture(t,
		"proc /proc proc rw 0 0",
		`dshgw-browser-workspace /srv/a\040b/browser/key fuse.browser-workspace rw 0 0`,
		"winger@host:/home /srv/ssh/home fuse.sshfs rw 0 0",
	)
	if got, err := MountedAt(root, "/srv/a b/browser/key"); err != nil || got != "fuse.browser-workspace" {
		t.Fatalf("MountedAt(escaped) = %q, %v", got, err)
	}
	if got, err := MountedAt(root, "/srv/ssh/home"); err != nil || got != "fuse.sshfs" {
		t.Fatalf("MountedAt(sshfs) = %q, %v", got, err)
	}
	if got, _ := MountedAt(root, "/srv/a b/browser/other"); got != "" {
		t.Fatalf("a prefix of a mount point reported %q, want nothing", got)
	}
	if got, _ := MountedAt(root, "/srv/absent"); got != "" {
		t.Fatalf("an unmounted path reported %q", got)
	}
	// A table that cannot be read is "no entry", not an error: every caller treats the two the
	// same way, and the table is present on every host this runs on.
	if got, _ := MountedAt(filepath.Join(root, "absent"), "/srv/ssh/home"); got != "" {
		t.Fatalf("MountedAt without a table reported %q", got)
	}
}

func TestDecodeMountField(t *testing.T) {
	cases := map[string]string{
		`/srv/plain`:            "/srv/plain",
		`/srv/a\040b`:           "/srv/a b",
		`/srv/a\011b`:           "/srv/a\tb",
		`/srv/a\134b`:           "/srv/a\\b",
		`/srv/a\04`:             `/srv/a\04`,
		`/srv/trailing\`:        `/srv/trailing\`,
		`/srv/a\040b\040c\012d`: "/srv/a b c\nd",
	}
	for in, want := range cases {
		if got := DecodeMountField(in); got != want {
			t.Errorf("DecodeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}

// fuseDevice is the device number of a FUSE connection id in the kernel's encoding.
func fuseDevice(minor int) uint64 { return uint64(minor&0xff) | uint64(minor&0xfff00)<<12 }

// connFixture builds the sysfs tree Abort writes into and states which mount points hold a
// FUSE filesystem, with which device number.
func connFixture(t *testing.T, types map[string]string, devices map[string]uint64, minors ...int) string {
	t.Helper()
	sysfs := filepath.Join(t.TempDir(), "fuse")
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
	t.Cleanup(func() {})
	return sysfs
}

func deviceTable(devices map[string]uint64) func(string) (uint64, error) {
	return func(path string) (uint64, error) {
		device, ok := devices[path]
		if !ok {
			return 0, errors.New("no such mount here")
		}
		return device, nil
	}
}

func typeTable(types map[string]string) func(string) (string, error) {
	return func(path string) (string, error) { return types[path], nil }
}

// The abort is the last resort that reaches waiters a daemon cannot: its sysfs entry is
// write-only, and the connection to abort is named by the device number in the mount table.
func TestAbortWritesTheConnectionEntry(t *testing.T) {
	mount := "/srv/alice/ssh/host/home"
	sysfs := connFixture(t, nil, nil, 834)
	mounted, device := typeTable(map[string]string{mount: "fuse.sshfs"}), deviceTable(map[string]uint64{mount: fuseDevice(834)})

	if conn, ok := ConnectionAt(mounted, device, mount); !ok || conn.Minor != 834 {
		t.Fatalf("connection for %s = %+v ok=%v, want minor 834", mount, conn, ok)
	}
	if _, ok := ConnectionAt(mounted, device, "/srv/absent"); ok {
		t.Fatal("a path that cannot be stat'ed was reported as a FUSE connection")
	}
	if err := Abort(sysfs, mounted, device, mount); err != nil {
		t.Fatalf("abort failed: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(sysfs, "834", "abort"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "1" {
		t.Fatalf("abort entry holds %q, want \"1\"", written)
	}
	// A connection the kernel has already collected is not a failure: there is nothing left to
	// abort, and the caller's unmount can proceed.
	if err := Abort(sysfs, mounted, device, "/srv/host/home"); err != nil {
		t.Fatalf("aborting a path with no connection failed: %v", err)
	}
}

// The kernel's sysfs entry is the liveness signal for a mount: while it exists the daemon holds
// the connection, and when it is gone every read on the mount fails with ENOTCONN.
func TestConnectionLiveFollowsTheSysfsEntry(t *testing.T) {
	live, dead := "/srv/alice/ssh/host/live", "/srv/alice/ssh/host/dead"
	sysfs := connFixture(t, nil, nil, 41)
	mounted := typeTable(map[string]string{live: "fuse.sshfs", dead: "fuse.sshfs"})
	device := deviceTable(map[string]uint64{live: fuseDevice(41), dead: fuseDevice(42)})
	connDir := func(conn Conn) string { return ConnectionDir(sysfs, conn) }

	if !ConnectionLive(mounted, device, connDir, live) {
		t.Fatal("a mount with a live connection was reported dead")
	}
	if ConnectionLive(mounted, device, connDir, dead) {
		t.Fatal("a mount whose connection is gone was reported live")
	}
	if !ConnectionLive(mounted, device, connDir, "/srv/not/a/mount") {
		t.Fatal("a path that is not a FUSE mount was reported dead")
	}
}

// The daemon is identified by the mount point that ends its command line, and never by a PID
// alone: a recycled PID must not become a killed process.
func TestFindDaemonMatchesTheMountPointExactly(t *testing.T) {
	mount := "/srv/alice/ssh/host/home"
	other := "/srv/alice/ssh/host/home/other"
	proc := t.TempDir()
	stage := func(pid int, comm, cmdline string) {
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		argv := strings.Split(cmdline, " ")
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The same argv under a different program name is not the daemon this package looks for.
	stage(4711, "ssh", "/usr/bin/sshfs -o reconnect host:/home "+mount)
	stage(4712, "sshfs", "/usr/bin/sshfs -o reconnect host:/ "+other)
	stage(4713, "sshfs", "/usr/bin/sshfs -o reconnect host:/home "+mount+"/nested")
	stage(4714, "sshfs", "/usr/bin/sshfs -o reconnect host:/home "+mount)

	if got := FindDaemon(proc, mount, "sshfs"); got != 4714 {
		t.Fatalf("daemon for %s = %d, want 4714 (a parent path or a prefix must not match)", mount, got)
	}
	if got := FindDaemon(proc, other, "sshfs"); got != 4712 {
		t.Fatalf("daemon for %s = %d, want 4712", other, got)
	}
	if got := FindDaemon(proc, mount, "bwrap"); got != 0 {
		t.Fatalf("a mount served by another program reported daemon %d", got)
	}
	if got := FindDaemon(proc, "/srv/nobody", "sshfs"); got != 0 {
		t.Fatalf("an unmounted path reported daemon %d", got)
	}
}

// The ladder: a mount that refuses the ordinary detach is detached lazily, and a mount that
// refuses both reports what fusermount said instead of pretending to be gone.
func TestUnmountFallsBackToLazy(t *testing.T) {
	var calls []string
	run := func(_ context.Context, name string, args []string) ([]byte, []byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if len(args) == 2 && args[0] == "-u" {
			return nil, []byte("Device or resource busy"), errors.New("exit status 1")
		}
		return nil, nil, nil
	}
	lazy, err := Unmount(context.Background(), run, "/srv/mount")
	if err != nil || !lazy {
		t.Fatalf("Unmount = lazy=%v err=%v, want a lazy detach", lazy, err)
	}
	if len(calls) != 2 || calls[0] != "fusermount3 -u /srv/mount" || calls[1] != "fusermount3 -u -z /srv/mount" {
		t.Fatalf("calls = %v", calls)
	}

	// `fusermount3 -z` alone is refused by fusermount ("can only be used with -u"), so the lazy
	// form must carry both flags.
	lazy, err = Unmount(context.Background(), func(_ context.Context, _ string, _ []string) ([]byte, []byte, error) {
		return nil, nil, nil
	}, "/srv/mount")
	if err != nil || lazy {
		t.Fatalf("a plain detach reported lazy=%v err=%v", lazy, err)
	}
}

func TestUnmountReportsBothFailures(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string) ([]byte, []byte, error) {
		return nil, []byte("Device or resource busy"), errors.New("exit status 1")
	}
	lazy, err := Unmount(context.Background(), run, "/srv/mount")
	if err == nil || lazy {
		t.Fatalf("Unmount = lazy=%v err=%v, want an error", lazy, err)
	}
	if !strings.Contains(err.Error(), "Device or resource busy") || !strings.Contains(err.Error(), "/srv/mount") {
		t.Fatalf("the error hides what happened: %v", err)
	}
}

// A host without fusermount3 says so instead of reporting a busy mount.
func TestUnmountReportsAMissingHelper(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string) ([]byte, []byte, error) {
		return nil, nil, fmt.Errorf("exec: %w", os.ErrNotExist)
	}
	if _, err := Unmount(context.Background(), run, "/srv/mount"); err == nil {
		t.Fatal("a missing helper was reported as success")
	}
}

package sshworkspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The wedged-mount escape hatch.
//
// An sshfs mount is a FUSE connection: the kernel forwards every lookup and read to
// the sshfs daemon, and the daemon answers after its ssh round trip. When that round
// trip never finishes — an unreachable host, a black-holed TCP connection, a daemon
// that is itself stopped — the caller waits in an uninterruptible (D) state, which
// no signal can interrupt and no unmount can finish, because the unmount is served by
// the same connection. Inside a worker that caller is the agent's own tool call, and
// its scope then cannot be collected either.
//
// Two operations end that state, and they are the same operation: the FUSE connection
// must be aborted. The kernel then fails every pending request immediately, which
// releases the waiters. Killing the daemon does it (its file descriptor is what keeps
// the connection alive), and so does writing to the connection's sysfs "abort" entry,
// which is the only one of the two that works when the daemon itself is unkillable.
// Measured on this host with a suspended sshfs daemon and a reader parked on it:
// killing the daemon released the reader with ECONNABORTED, and so did the sysfs
// abort.

// Where the kernel state behind a mount is read. They are variables so the probing can be
// tested against fixtures: they are host facts, and a test must be able to state them.
var (
	procRoot  = "/proc"
	sysfsFuse = "/sys/fs/fuse/connections"
	// statDevice numbers one path. It is a seam because the connection id of a FUSE mount
	// is only reachable through the mounted inode's device number.
	statDevice = deviceOf
)

// fuseConn is the FUSE connection behind one mount point.
type fuseConn struct {
	// Major and Minor are the device numbers of the mounted filesystem. The minor
	// number is the FUSE connection id, which is the name of its sysfs entry.
	Major int
	Minor int
}

// fuseConnAt reports the FUSE connection behind a mount point, or ok=false when the
// mount point holds no FUSE filesystem.
//
// The device number is what names the connection, and it comes from the mounted inode:
// /proc/self/mounts cannot be used for this, because a file system that has no block
// device (FUSE included) appears there with its *source* — sshfs writes the remote
// "user@host:/path" — and not with a device. stat on a FUSE mount point reads the kernel's
// cached attributes and does not reach the daemon, so this stays safe on a wedged mount;
// see the note in deviceOf.
func fuseConnAt(mounted func(string) (string, error), mountpoint string) (fuseConn, bool) {
	fstype, _ := mounted(mountpoint)
	if !strings.HasPrefix(fstype, "fuse") {
		return fuseConn{}, false
	}
	device, err := statDevice(mountpoint)
	if err != nil {
		return fuseConn{}, false
	}
	return fuseConn{Major: int(deviceMajor(device)), Minor: int(deviceMinor(device))}, true
}

// The kernel's device encoding is not a plain major<<8|minor: the minor's high bits sit
// above the major (Linux' "huge" dev_t format). Decoding it by hand is what keeps this
// readable without pulling in an x/sys dependency for two shifts.
func deviceMajor(device uint64) uint32 {
	return uint32(device>>8&0xfff) | uint32(device>>32&^uint64(0xfff))
}

func deviceMinor(device uint64) uint32 {
	return uint32(device&0xff) | uint32(device>>12&0xfff00)
}

// deviceOf returns the device number of one mounted path.
//
// A stat on a FUSE mount point is answered from the inode the kernel already has: FUSE
// only passes attribute requests on when the cached entry is stale (attribute timeout,
// default one second in sshfs), so this cannot become the uninterruptible wait that a
// directory read on a wedged mount becomes. It is still a syscall on a mount, which is
// why the whole probing path treats a failure as "no connection" rather than as an error.
func deviceOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no device number in the file info")
	}
	return uint64(stat.Dev), nil
}

// connectionLive reports whether the FUSE connection behind a mount is still there.
//
// The kernel keeps one directory per live connection under
// /sys/fs/fuse/connections/<id>, and it disappears when the daemon's device is released —
// which is what a crashed or killed sshfs leaves behind. So a FUSE mount whose connection
// is gone points at nothing: every read on it fails with ENOTCONN, and no new mount can be
// made over it until the entry is detached.
func connectionLive(mounted func(string) (string, error), mountpoint string) bool {
	conn, ok := fuseConnAt(mounted, mountpoint)
	if !ok {
		// No FUSE file system here at all: nothing to call dead.
		return true
	}
	return fuseConnDir(conn) != ""
}

// fuseConnDir is the kernel's directory for one connection, or "" when the kernel has
// released it. It is a variable for the same reason the mount-table reader is: a test that
// states the mount table has to be able to state the connections too.
var fuseConnDir = func(conn fuseConn) string {
	path := filepath.Join(sysfsFuse, strconv.Itoa(conn.Minor))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// fuseAbort aborts the FUSE connection behind one mount, releasing every process
// waiting on a request that the daemon will never answer. It reads the mount table with
// the package's own reader, so it is only used on the gateway's real mounts.
//
// It is deliberately the last resort of an unmount that has already failed: aborting a
// healthy connection disconnects the mount (reads fail with ECONNABORTED) until the
// daemon reconnects or the gateway mounts it again. A wedged connection has nothing
// left to lose.
func fuseAbort(mountpoint string) error {
	conn, ok := fuseConnAt(serviceMounted, mountpoint)
	if !ok {
		return nil
	}
	path := fuseAbortPath(conn.Minor)
	// O_WRONLY only: the entry is write-only by design.
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			// The connection is already gone; there is no waiter left to release.
			return nil
		}
		return fmt.Errorf("abort FUSE connection %d: %w", conn.Minor, err)
	}
	defer file.Close()
	if _, err := file.WriteString("1"); err != nil {
		return fmt.Errorf("abort FUSE connection %d: %w", conn.Minor, err)
	}
	return nil
}

// fuseAbortPath is the sysfs entry that aborts one FUSE connection.
func fuseAbortPath(minor int) string {
	return filepath.Join(sysfsFuse, strconv.Itoa(minor), "abort")
}

// sshfsDaemonFor finds the sshfs process serving one mount point.
//
// The daemon's own argv ends with the mount point — that is the sshfs command line
// this package builds — so the mount point identifies it exactly, and the process
// name is checked as well so a recycled PID can never be signalled by mistake. Only
// processes of this account are examined: /proc/<pid>/cmdline is unreadable for
// another user's process, and the kill is refused for one.
func sshfsDaemonFor(mountpoint string) int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		if processName(pid) != "sshfs" {
			continue
		}
		if lastArg(pid) == mountpoint {
			return pid
		}
	}
	return 0
}

// processName is the executable name of one process, or "" when it cannot be read.
func processName(pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// lastArg is the final argv element of one process, or "" when it cannot be read.
func lastArg(pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return ""
	}
	parts := bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0})
	if len(parts) == 0 {
		return ""
	}
	return string(parts[len(parts)-1])
}

// killSSHFSDaemon stops the daemon serving one mount, which is what releases the
// pending requests that keep readers in an uninterruptible wait. It reports whether a
// daemon was found; the caller decides what a failure means.
func killSSHFSDaemon(mountpoint string) bool {
	pid := sshfsDaemonFor(mountpoint)
	if pid == 0 {
		return false
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return true
}

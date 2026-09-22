package sshworkspace

import (
	"github.com/winger/ai-gateway/internal/dshgw/fusekernel"
)

// The wedged-mount escape hatch.
//
// An sshfs mount is a FUSE connection: the kernel forwards every lookup and read to the sshfs
// daemon, and the daemon answers after its ssh round trip. When that round trip never finishes
// — an unreachable host, a black-holed TCP connection, a daemon that is itself stopped — the
// caller waits in an uninterruptible (D) state, which no signal can interrupt and no unmount
// can finish, because the unmount is served by the same connection. Inside a worker that caller
// is the agent's own tool call, and its scope then cannot be collected either.
//
// Two operations end that state, and they are the same operation: the FUSE connection must be
// aborted. The kernel then fails every pending request immediately, which releases the waiters.
// Killing the daemon does it (its file descriptor is what keeps the connection alive), and so
// does writing to the connection's sysfs "abort" entry, which is the only one of the two that
// works when the daemon itself is unkillable. Measured on this host with a suspended sshfs
// daemon and a reader parked on it: killing the daemon released the reader with ECONNABORTED,
// and so did the sysfs abort.
//
// The host facts behind all of this live in internal/dshgw/fusekernel, which the browser
// workspace uses too (M76); what stays here is this package's own view of them, which the tests
// state instead of touching the real /proc and /sys.

// Where the kernel state behind a mount is read. They are variables so the probing can be
// tested against fixtures: they are host facts, and a test must be able to state them.
var (
	procRoot  = "/proc"
	sysfsFuse = "/sys/fs/fuse/connections"
	// statDevice numbers one path. It is a seam because the connection id of a FUSE mount is
	// only reachable through the mounted inode's device number.
	statDevice = fusekernel.DeviceOf
)

// fuseConn is the FUSE connection behind one mount point.
type fuseConn = fusekernel.Conn

// fuseConnDir is the kernel's directory for one connection, or "" when the kernel has released
// it. It is a variable for the same reason the mount-table reader is: a test that states the
// mount table has to be able to state the connections too.
var fuseConnDir = func(conn fuseConn) string { return fusekernel.ConnectionDir(sysfsFuse, conn) }

// fuseConnAt reports the FUSE connection behind a mount point, or ok=false when the mount
// point holds no FUSE filesystem.
func fuseConnAt(mounted func(string) (string, error), mountpoint string) (fuseConn, bool) {
	return fusekernel.ConnectionAt(mounted, statDevice, mountpoint)
}

// connectionLive reports whether the FUSE connection behind a mount is still there.
func connectionLive(mounted func(string) (string, error), mountpoint string) bool {
	return fusekernel.ConnectionLive(mounted, statDevice, fuseConnDir, mountpoint)
}

// fuseAbort aborts the FUSE connection behind one mount, releasing every process waiting on a
// request that the daemon will never answer.
func fuseAbort(mountpoint string) error {
	return fusekernel.Abort(sysfsFuse, serviceMounted, statDevice, mountpoint)
}

// sshfsDaemonFor finds the sshfs process serving one mount point.
//
// The seam exists because the worker's profile asks it whether a recorded mount is still
// served: a test has no real daemon to point at, and production must look at the same process
// table the unmount path uses.
var sshfsDaemonFor = findSSHFSDaemon

// findSSHFSDaemon finds the sshfs process serving one mount point: the process whose argv ends
// with the mount point, which is the command line this package builds.
func findSSHFSDaemon(mountpoint string) int {
	return fusekernel.FindDaemon(procRoot, mountpoint, "sshfs")
}

// killSSHFSDaemon stops the daemon serving one mount, which is what releases the pending
// requests that keep readers in an uninterruptible wait. It reports whether a daemon was
// found; the caller decides what a failure means.
func killSSHFSDaemon(mountpoint string) bool {
	return fusekernel.KillDaemon(procRoot, mountpoint, "sshfs")
}

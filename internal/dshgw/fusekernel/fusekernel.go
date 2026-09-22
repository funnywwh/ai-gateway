// Package fusekernel is the single place that reads and controls the kernel side of a FUSE
// mount: the mount table, the per-connection sysfs entries under
// /sys/fs/fuse/connections, the daemon that serves a mount, and the fusermount invocations
// that detach one.
//
// It exists because two services need exactly the same host facts and must not drift apart:
// internal/dshgw/sshworkspace (an sshfs mount an account asked for) and
// internal/dshgw/browserworkspace + browsermount (a browser directory mounted as a real
// workspace). The *ladders* stay in those services, because what counts as "the last resort"
// differs — killing an sshfs daemon on one side, aborting an in-process go-fuse connection on
// the other — but the probing, the abort and the lazy detach are one implementation.
//
// Every path into /proc and /sys is a parameter rather than a constant so a test can state
// the host facts (a fake mount table, a fixture sysfs tree) instead of needing a real mount.
package fusekernel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// RunFunc runs one external command and returns its raw streams. It is a parameter so the
// detach ladder can be driven without a real fusermount3.
type RunFunc func(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error)

// Conn is the FUSE connection behind one mount point. Minor is the connection id, which is
// the name of its sysfs entry.
type Conn struct {
	Major int
	Minor int
}

// MountedAt reports the filesystem type mounted exactly at path, or "" when nothing is
// mounted there.
//
// /proc/self/mounts is read directly instead of shelling out to findmnt: it is always
// present, and it is the same table findmnt would format.
func MountedAt(procRoot, path string) (string, error) {
	fstype := ""
	_ = EachMount(procRoot, func(fields []string) bool {
		if len(fields) < 3 || DecodeMountField(fields[1]) != path {
			return true
		}
		fstype = fields[2]
		return false
	})
	return fstype, nil
}

// EachMount walks one field-split line of /proc/self/mounts at a time. Returning false from
// visit stops the walk. A table that cannot be read is not an error worth propagating: every
// caller treats "no entry" and "no table" the same way, and the table is present on every
// host this runs on.
func EachMount(procRoot string, visit func(fields []string) bool) error {
	file, err := os.Open(filepath.Join(procRoot, "self/mounts"))
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if !visit(fields) {
			return nil
		}
	}
	return scanner.Err()
}

// DecodeMountField undoes the kernel's octal escaping (\040 for a space, \011, \012, \134).
func DecodeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if value, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

// DeviceOf returns the device number of one path. It is the default StatFunc.
//
// A stat on a FUSE mount point is answered from the inode the kernel already has: FUSE only
// passes attribute requests on when the cached entry is stale (attribute timeout, default one
// second in sshfs), so this cannot become the uninterruptible wait a directory read on a
// wedged mount becomes. It is still a syscall on a mount, which is why the whole probing path
// treats a failure as "no connection" rather than as an error.
func DeviceOf(path string) (uint64, error) {
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

// ConnectionAt reports the FUSE connection behind a mount point, or ok=false when the mount
// point holds no FUSE filesystem or its device number cannot be read.
func ConnectionAt(mounted func(string) (string, error), device func(string) (uint64, error), mountpoint string) (Conn, bool) {
	fstype, _ := mounted(mountpoint)
	if !strings.HasPrefix(fstype, "fuse") {
		return Conn{}, false
	}
	dev, err := device(mountpoint)
	if err != nil {
		return Conn{}, false
	}
	return Conn{Major: int(DeviceMajor(dev)), Minor: int(DeviceMinor(dev))}, true
}

// The kernel's device encoding is not a plain major<<8|minor: the minor's high bits sit above
// the major (Linux' "huge" dev_t format). Decoding it by hand is what keeps this readable
// without pulling in an x/sys dependency for two shifts.
func DeviceMajor(device uint64) uint32 {
	return uint32(device>>8&0xfff) | uint32(device>>32&^uint64(0xfff))
}

func DeviceMinor(device uint64) uint32 {
	return uint32(device&0xff) | uint32(device>>12&0xfff00)
}

// ConnectionDir is the kernel's directory for one connection, or "" when the kernel has
// released it.
func ConnectionDir(sysfsFuse string, conn Conn) string {
	path := filepath.Join(sysfsFuse, strconv.Itoa(conn.Minor))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// ConnectionLive reports whether the FUSE connection behind a mount is still there.
//
// The kernel keeps one directory per live connection under /sys/fs/fuse/connections/<id>, and
// it disappears when the daemon's device is released — which is what a crashed or killed
// daemon leaves behind. So a FUSE mount whose connection is gone points at nothing: every read
// on it fails with ENOTCONN, and no new mount can be made over it until the entry is detached.
//
// connDir is a parameter (rather than a path) so a caller can state the sysfs tree — including
// a fixture one — without this package reading /sys behind its back.
func ConnectionLive(mounted func(string) (string, error), device func(string) (uint64, error), connDir func(Conn) string, mountpoint string) bool {
	conn, ok := ConnectionAt(mounted, device, mountpoint)
	if !ok {
		// No FUSE file system here at all: nothing to call dead.
		return true
	}
	return connDir(conn) != ""
}

// Abort aborts the FUSE connection behind one mount, releasing every process waiting on a
// request that the daemon will never answer.
//
// It is deliberately the last resort of an unmount that has already failed: aborting a healthy
// connection disconnects the mount (reads fail with ECONNABORTED) until the daemon reconnects
// or the mount is made again. A wedged connection has nothing left to lose.
func Abort(sysfsFuse string, mounted func(string) (string, error), device func(string) (uint64, error), mountpoint string) error {
	conn, ok := ConnectionAt(mounted, device, mountpoint)
	if !ok {
		return nil
	}
	path := filepath.Join(sysfsFuse, strconv.Itoa(conn.Minor), "abort")
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

// FindDaemon finds the process serving one mount: the process whose executable name is comm
// and whose last argv element is the mount point.
//
// The daemon's own argv ends with the mount point — that is the command line its caller builds
// — so the mount point identifies it exactly, and the program name is checked as well so a
// recycled PID can never be signalled by mistake. Only processes of this account are examined:
// /proc/<pid>/cmdline is unreadable for another user's process, and the kill is refused for
// one.
func FindDaemon(procRoot, mountpoint, comm string) int {
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
		if ProcessName(procRoot, pid) != comm {
			continue
		}
		if LastArg(procRoot, pid) == mountpoint {
			return pid
		}
	}
	return 0
}

// KillDaemon stops the daemon serving one mount, which is what releases the pending requests
// that keep readers in an uninterruptible wait. It reports whether a daemon was found; the
// caller decides what a failure means.
func KillDaemon(procRoot, mountpoint, comm string) bool {
	pid := FindDaemon(procRoot, mountpoint, comm)
	if pid == 0 {
		return false
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return true
}

// ProcessName is the executable name of one process, or "" when it cannot be read.
func ProcessName(procRoot string, pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// LastArg is the final argv element of one process, or "" when it cannot be read.
func LastArg(procRoot string, pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// Unmount detaches one FUSE mount, and falls back to a lazy detach when the filesystem is
// busy.
//
// The lazy form is what detaches a mount that is still in use: the kernel keeps the old
// superblock for whoever holds it (another mount namespace, a sandbox that outlived its
// worker) and drops the entry from our mount table, which is exactly what a caller whose
// mounts must not outlive a session needs. It reports whether the lazy form was the one that
// worked, because that is worth logging and auditing.
func Unmount(ctx context.Context, run RunFunc, mountpoint string) (lazy bool, err error) {
	_, stderr, runErr := fusermount(ctx, run, mountpoint, false)
	if runErr == nil {
		return false, nil
	}
	if errors.Is(runErr, exec.ErrNotFound) {
		return false, fmt.Errorf("fusermount3 is not installed: %w", runErr)
	}
	// The lazy retry needs -u: `fusermount3 -z` alone is refused ("can only be used with -u"),
	// so the two-flag form is what actually detaches a busy mount.
	_, lazyStderr, lazyErr := fusermount(ctx, run, mountpoint, true)
	if lazyErr == nil {
		return true, nil
	}
	return false, fmt.Errorf("unmount %s failed: %s / %s", mountpoint, Tail(stderr), Tail(lazyStderr))
}

// fusermount runs one fusermount invocation. The non-lazy form is tried against fusermount3
// only; the lazy form falls back to a host that still has the version-2 helper, because that
// helper is the one the deployment notes mention as the compatibility path for cleanup.
func fusermount(ctx context.Context, run RunFunc, mountpoint string, lazy bool) (stdout, stderr []byte, err error) {
	args := []string{"-u"}
	if lazy {
		args = append(args, "-z")
	}
	args = append(args, mountpoint)
	stdout, stderr, err = run(ctx, "fusermount3", args)
	if err != nil && errors.Is(err, exec.ErrNotFound) {
		return run(ctx, "fusermount", args)
	}
	return stdout, stderr, err
}

// Tail keeps the end of a command's output: failures put the reason there.
func Tail(data []byte) string {
	const limit = 400
	text := strings.TrimSpace(string(data))
	if len(text) <= limit {
		return text
	}
	return "…" + text[len(text)-limit:]
}

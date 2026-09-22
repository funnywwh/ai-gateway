package browserworkspace

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/fusekernel"
)

// The forced detach of a browser-workspace mount (M76).
//
// The graceful path is go-fuse's Server.Unmount, which runs `fusermount3 -u`. It fails with
// EBUSY whenever something still holds the mount: a sandbox that outlived its worker, or — as
// measured on the deployment host — a *foreign* mount namespace that received a copy of the
// mount by propagation (a snap's private namespace, in that case). Such a mount never
// unmounts gracefully, so a caller that only retries the graceful call retries forever, which
// is exactly what the reaper did for an hour on 2026-09-22 while the tenant's exit reported
// failure.
//
// What removes the mount from OUR mount table is fusermount's lazy flag: the kernel keeps the
// old superblock for whoever still holds it and drops the entry, which is what an account that
// has just signed out needs (its own sandbox is about to be killed anyway). If even that fails,
// the FUSE connection is aborted — the same last resort the ssh workspace uses — which fails
// every pending request instead of letting them wait on a browser that is gone.

// forceLadder is the forced detach, with its host facts and its fusermount invocation as
// fields so a test can drive every branch without a real mount.
type forceLadder struct {
	mounted func(string) (string, error)
	device  func(string) (uint64, error)
	run     fusekernel.RunFunc
	abort   func(string) error
	// pause is how long to wait after aborting a connection before the last unmount attempt:
	// the kernel releases the waiters asynchronously.
	pause time.Duration
	sleep func(time.Duration)
}

func productionForceLadder() forceLadder {
	mounted := func(path string) (string, error) { return fusekernel.MountedAt("/proc", path) }
	device := fusekernel.DeviceOf
	return forceLadder{
		mounted: mounted,
		device:  device,
		run: func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return nil, out, err
			}
			return out, nil, nil
		},
		abort: func(mountpoint string) error {
			return fusekernel.Abort("/sys/fs/fuse/connections", mounted, device, mountpoint)
		},
		pause: 300 * time.Millisecond,
		sleep: time.Sleep,
	}
}

// ForceUnmount detaches one browser-workspace mount that the graceful path could not. It
// returns nil once the mount is gone from the mount table, and an error naming what is still
// attached otherwise.
//
// It is safe to call on a mount that is already gone: that is the first thing it checks.
func ForceUnmount(mountpoint string) error {
	return ForceUnmountContext(context.Background(), mountpoint)
}

// ForceUnmountContext is ForceUnmount under a caller's deadline (the logout path bounds the
// whole teardown, so it must not be able to hang here).
func ForceUnmountContext(ctx context.Context, mountpoint string) error {
	return productionForceLadder().detach(ctx, mountpoint)
}

// detach runs the ladder: lazy detach, then abort the connection and retry, then report.
//
// The mount table is the truth at every step: fusermount can fail to report a detach that did
// happen, and a lazy detach succeeds while another namespace keeps the superblock, so what
// decides is whether the entry is gone rather than what a command returned.
func (l forceLadder) detach(ctx context.Context, mountpoint string) error {
	if !l.attached(mountpoint) {
		return nil
	}
	// The first call is fusermount's ordinary form followed by its lazy one; a mount that is
	// held elsewhere answers EBUSY to the first and succeeds on the second.
	_, err := fusekernel.Unmount(ctx, l.run, mountpoint)
	if !l.attached(mountpoint) {
		return nil
	}
	// Still attached: the kernel is holding the superblock for another namespace, and an abort
	// is what releases it (and every request parked on a browser that is no longer answering)
	// before one last attempt.
	abortErr := l.abort(mountpoint)
	l.sleep(l.pause)
	_, retryErr := fusekernel.Unmount(ctx, l.run, mountpoint)
	if !l.attached(mountpoint) {
		return nil
	}
	fstype, _ := l.mounted(mountpoint)
	if fstype == "" {
		fstype = "unknown filesystem"
	}
	if abortErr != nil {
		return fmt.Errorf("mount %s is still attached (%s) and aborting its FUSE connection failed: %w", mountpoint, fstype, abortErr)
	}
	if retryErr != nil {
		err = retryErr
	}
	if err != nil {
		return fmt.Errorf("mount %s is still attached (%s): %w", mountpoint, fstype, err)
	}
	return fmt.Errorf("mount %s is still attached (%s) after a lazy detach and a FUSE connection abort", mountpoint, fstype)
}

// attached reports whether the mount table still lists a mount exactly at one path.
func (l forceLadder) attached(mountpoint string) bool {
	fstype, err := l.mounted(mountpoint)
	if err != nil {
		// An unreadable table is treated as "attached": the caller then reports a leftover
		// instead of claiming a cleanup that it cannot verify.
		return true
	}
	return fstype != ""
}

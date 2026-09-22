package browserworkspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forceFixture is a fake mount table plus the fusermount and abort outcomes a test states.
type forceFixture struct {
	mounts map[string]string
	// plainOK/lazyOK say whether the ordinary and the lazy fusermount invocation work.
	plainOK, lazyOK bool
	abortErr        error
	calls           []string
	aborts          int
}

func newForceFixture() (*forceFixture, forceLadder) {
	f := &forceFixture{mounts: map[string]string{}}
	ladder := forceLadder{
		mounted: func(path string) (string, error) {
			fstype, ok := f.mounts[path]
			if !ok {
				return "", nil
			}
			return fstype, nil
		},
		device: func(string) (uint64, error) { return 0, errors.New("unused") },
		run: func(_ context.Context, name string, args []string) ([]byte, []byte, error) {
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			path := args[len(args)-1]
			lazy := len(args) > 1 && args[1] == "-z"
			ok := f.plainOK
			if lazy {
				ok = f.lazyOK
			}
			if !ok {
				return nil, []byte("Device or resource busy"), errors.New("exit status 1")
			}
			delete(f.mounts, path)
			return nil, nil, nil
		},
		abort: func(string) error {
			f.aborts++
			return f.abortErr
		},
		pause: 0,
		sleep: func(time.Duration) {},
	}
	return f, ladder
}

// A mount that is already gone is not an error, and must not run fusermount again.
func TestForceUnmountIsIdempotent(t *testing.T) {
	f, ladder := newForceFixture()
	if err := ladder.detach(context.Background(), "/srv/mount"); err != nil {
		t.Fatalf("detaching an unmounted path failed: %v", err)
	}
	if len(f.calls) != 0 || f.aborts != 0 {
		t.Fatalf("an unmounted path still ran %v (aborts=%d)", f.calls, f.aborts)
	}
}

// The mount a graceful unmount cannot take: fusermount's ordinary form answers EBUSY, and the
// lazy form is what removes the entry — without aborting a healthy connection.
func TestForceUnmountUsesTheLazyDetach(t *testing.T) {
	f, ladder := newForceFixture()
	f.mounts["/srv/mount"] = "fuse.browser-workspace"
	f.lazyOK = true
	if err := ladder.detach(context.Background(), "/srv/mount"); err != nil {
		t.Fatalf("a busy mount was not detached lazily: %v", err)
	}
	if f.aborts != 0 {
		t.Fatalf("a lazy detach that worked still aborted the connection (%d aborts)", f.aborts)
	}
	if len(f.calls) != 2 || !strings.HasSuffix(f.calls[1], "-u -z /srv/mount") {
		t.Fatalf("calls = %v, want the lazy form last", f.calls)
	}
}

// When even the lazy detach fails, the connection is aborted and the mount is tried once more:
// aborting is what releases the kernel's hold on a mount another namespace kept.
func TestForceUnmountAbortsTheConnectionWhenNothingDetaches(t *testing.T) {
	f, ladder := newForceFixture()
	f.mounts["/srv/mount"] = "fuse.browser-workspace"
	// Nothing detaches until the abort has happened; the retry after it succeeds.
	ladder.abort = func(string) error {
		f.aborts++
		f.lazyOK = true
		return nil
	}
	if err := ladder.detach(context.Background(), "/srv/mount"); err != nil {
		t.Fatalf("the mount was not detached after the abort: %v", err)
	}
	if f.aborts != 1 {
		t.Fatalf("aborts = %d, want exactly one", f.aborts)
	}
	if len(f.calls) != 4 || !strings.HasSuffix(f.calls[3], "-u -z /srv/mount") {
		t.Fatalf("calls = %v, want two attempts (plain+lazy each) with the lazy retry last", f.calls)
	}
}

// A mount that survives the whole ladder is reported as a leftover, naming the path and what is
// still attached — never as a cleanup that happened.
func TestForceUnmountReportsWhatSurvived(t *testing.T) {
	f, ladder := newForceFixture()
	f.mounts["/srv/mount"] = "fuse.browser-workspace"
	err := ladder.detach(context.Background(), "/srv/mount")
	if err == nil {
		t.Fatal("a mount that survived every step was reported as detached")
	}
	if !strings.Contains(err.Error(), "/srv/mount") || !strings.Contains(err.Error(), "fuse.browser-workspace") {
		t.Fatalf("the error names neither the path nor the filesystem: %v", err)
	}
	if f.aborts != 1 {
		t.Fatalf("aborts = %d, want one attempt", f.aborts)
	}
}

// An abort that fails is its own failure: the caller has to know the connection may still be
// serving requests, not just that the mount is there.
func TestForceUnmountReportsAFailedAbort(t *testing.T) {
	f, ladder := newForceFixture()
	f.mounts["/srv/mount"] = "fuse.browser-workspace"
	f.abortErr = errors.New("permission denied")
	err := ladder.detach(context.Background(), "/srv/mount")
	if err == nil || !strings.Contains(err.Error(), "aborting its FUSE connection failed") {
		t.Fatalf("err = %v, want the failed abort named", err)
	}
}

// An unreadable mount table is not proof that a mount is gone.
func TestForceUnmountTreatsAnUnreadableTableAsAttached(t *testing.T) {
	_, ladder := newForceFixture()
	ladder.mounted = func(string) (string, error) { return "", errors.New("no table") }
	if err := ladder.detach(context.Background(), "/srv/mount"); err == nil {
		t.Fatal("an unverifiable cleanup was reported as success")
	}
}

// The real thing, on a host with /dev/fuse: a mount that a live process holds (its cwd is
// inside the mount) fails the graceful unmount with EBUSY, and the forced ladder still takes it
// out of the mount table. This is the shape the reaper retried forever on 2026-09-22.
func TestRealFUSEForceUnmountTakesABusyMount(t *testing.T) {
	if os.Getenv("BROWSERWORKSPACE_FUSE_TEST") != "1" {
		t.Skip("set BROWSERWORKSPACE_FUSE_TEST=1")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatal(err)
	}
	mount := t.TempDir()
	srv, err := MountFSWithOptions(mount, &diskBackend{root: t.TempDir()}, MountOptions{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ForceUnmount(mount)
		srv.Wait()
	})
	// A process whose cwd is inside the mount makes the ordinary unmount fail with EBUSY; the
	// lazy form is what a caller needs then.
	holder := exec.Command("/bin/sh", "-c", "cd '"+mount+"' && exec sleep 60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := srv.Unmount(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("the mount unmounted cleanly although a process holds it; this host cannot reproduce EBUSY")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := ForceUnmount(mount); err != nil {
		t.Fatalf("the forced detach failed on a busy mount: %v", err)
	}
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mountinfo), filepath.Clean(mount)+" ") {
		t.Fatalf("%s is still in the mount table", mount)
	}
}

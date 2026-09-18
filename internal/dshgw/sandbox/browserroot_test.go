package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
)

func TestBrowserMountRootIsRequiredAndBoundBeforeChildren(t *testing.T) {
	f := newProfileFixture(t)
	root := filepath.Join(f.alice.Workspace, "browser")
	child := filepath.Join(root, "mount")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	f.alice.BrowserMounts = []string{child}
	if _, err := Profile(f.rt, f.alice); err == nil {
		t.Fatal("accepted browser mounts without protected container")
	}
	f.alice.BrowserMountRoot = root
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	rootAt, childAt := -1, -1
	for i, m := range mounts {
		if m.flag == "--ro-bind" && m.src == root && m.dst == root {
			rootAt = i
		}
		if m.flag == "--bind" && m.src == child && m.dst == child {
			childAt = i
		}
	}
	if rootAt < 0 || childAt <= rootAt {
		t.Fatalf("invalid mount order root=%d child=%d", rootAt, childAt)
	}
	f.alice.BrowserMounts = nil
	if _, err := Profile(f.rt, f.alice); err != nil {
		t.Fatal("empty container must be protected", err)
	}
	for _, bad := range []string{f.alice.Workspace, root + "/", filepath.Join(f.alice.Workspace, "other")} {
		f.alice.BrowserMountRoot = bad
		if _, err := Profile(f.rt, f.alice); err == nil {
			t.Errorf("accepted root %s", bad)
		}
	}
	f.alice.BrowserMountRoot = root
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Profile(f.rt, f.alice); err == nil {
		t.Fatal("accepted non-private root")
	}
}

func TestStagingBrowserContainerCannotBeReplaced(t *testing.T) {
	runBrowserContainerStaging(t, false)
}

func TestStagingBrowserFUSEContainerKeepsChildWritable(t *testing.T) {
	runBrowserContainerStaging(t, true)
}

func runBrowserContainerStaging(t *testing.T, fuse bool) {
	t.Helper()
	bwrap := bwrapForStaging(t)
	layout := newStagingLayout(t, bwrap)
	root := filepath.Join(layout.alice.Workspace, "browser")
	child := filepath.Join(root, "mount")
	mustMkdir(t, child, 0700)
	backing := child
	if fuse {
		backing = t.TempDir()
		inode, err := fs.NewLoopbackRoot(backing)
		if err != nil {
			t.Fatal(err)
		}
		server, err := fs.Mount(child, inode, &fs.Options{})
		if err != nil {
			t.Skipf("FUSE staging unavailable: %v", err)
		}
		defer func() {
			if err := server.Unmount(); err != nil {
				t.Error(err)
			}
		}()
	}
	mustWrite(t, filepath.Join(backing, "data"), "before", 0600)
	layout.alice.BrowserMountRoot = root
	layout.alice.BrowserMounts = []string{child}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
root=%q
child=%q
# RO container: cannot create entries or replace/rename mounted objects.
if mkdir "$root/evil" 2>/dev/null; then echo mkdir-allowed; exit 11; fi
if mv "$root" "$root-moved" 2>/dev/null; then echo root-rename-allowed; exit 12; fi
if rmdir "$child" 2>/dev/null; then echo child-remove-allowed; exit 13; fi
if mv "$child" "$root/replaced" 2>/dev/null; then echo child-rename-allowed; exit 14; fi
if ln -s /tmp "$root/new-link" 2>/dev/null; then echo symlink-allowed; exit 15; fi
if rm -rf "$root" 2>/dev/null; then echo root-remove-allowed; exit 16; fi
# A root replacement attempt must not destroy the bind target; recreate data
# after rm -rf (which may legitimately delete writable child contents).
printf after > "$child/data"
mkdir "$child/nested"
printf nested > "$child/nested/file"
test "$(cat "$child/data")" = after
test ! -e /dev/fuse
printf protected-and-writable
`, root, child)
	mustWrite(t, layout.probeNode, script, 0755)
	argv, err := Profile(layout.rt, layout.alice)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox browser root test: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "protected-and-writable") {
		t.Fatalf("unexpected output %s", out)
	}
	data, err := os.ReadFile(filepath.Join(backing, "data"))
	if err != nil || string(data) != "after" {
		t.Fatalf("backing write %q: %v", data, err)
	}
}

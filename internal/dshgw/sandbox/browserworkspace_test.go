package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserMountContainment(t *testing.T) {
	f := newProfileFixture(t)
	mountpoint := filepath.Join(f.alice.Workspace, "browser", "local")
	if err := os.MkdirAll(mountpoint, 0700); err != nil {
		t.Fatal(err)
	}
	f.alice.BrowserMountRoot = filepath.Join(f.alice.Workspace, "browser")
	f.alice.BrowserMounts = []string{mountpoint}
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	found := false
	for _, m := range mounts {
		if m.flag == "--bind" && m.src == mountpoint && m.dst == mountpoint {
			found = true
		}
	}
	if !found {
		t.Fatal("browser mount not explicitly bound")
	}
	link := filepath.Join(f.alice.Workspace, "browser", "escape")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{f.alice.Workspace, filepath.Join(f.alice.Workspace, "browser"), filepath.Join(f.root, "bob"), link, "relative/browser"} {
		f.alice.BrowserMounts = []string{bad}
		if _, err := Profile(f.rt, f.alice); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

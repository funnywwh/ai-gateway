package browsermount

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

// A real tenant profile must see the browser mount via its explicit bind. The
// backend stands in for the browser; this test never modifies a real tenant.
func TestRealBrowserMountInsideSandbox(t *testing.T) {
	if os.Getenv("BROWSERWORKSPACE_SANDBOX_TEST") != "1" {
		t.Skip("set BROWSERWORKSPACE_SANDBOX_TEST=1")
	}
	node, release := os.Getenv("DSHGW_NODE"), os.Getenv("DSHGW_DSH_ROOT")
	if node == "" || release == "" {
		t.Fatal("DSHGW_NODE and DSHGW_DSH_ROOT required")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	workspace := filepath.Join(base, "workspaces", "a")
	home := filepath.Join(base, "tenants", "a", ".dsh")
	container := filepath.Join(workspace, "browser")
	mp := filepath.Join(container, "0123456789abcdef0123456789abcdef0123456789abcdef")
	for _, p := range []string{mp, home, filepath.Join(base, "config", "a")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "hello"), []byte("browser-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	mounted, err := fs.MountFS(mp, fs.BackendFunc(func(_ context.Context, r fs.Request) (fs.Response, error) { return localOperation(local, r), nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mounted.Unmount(); err != nil {
			t.Error(err)
		}
	}()
	rt := sandbox.Runtime{BwrapBin: bwrap, NodeBin: node, BinJS: filepath.Join(release, "lib", "bin.js"), CurrentLink: release, TenantRoot: filepath.Join(base, "tenants"), WorkspaceRoot: filepath.Join(base, "workspaces"), TenantConfigRoot: filepath.Join(base, "config")}
	tenant := sandbox.Tenant{Name: "a", Workspace: workspace, DshHome: home, WorkerPort: 32100, BrowserMountRoot: container, BrowserMounts: []string{mp}}
	args, err := sandbox.Profile(rt, tenant)
	if err != nil {
		t.Fatal(err)
	}
	separator := 0
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == 0 {
		t.Fatal("missing command separator")
	}
	args = append(args[:separator+1], "/bin/sh", "-c", `test ! -e /dev/fuse && ! mkdir "$2/escape" 2>/dev/null && printf sandbox-written > "$1/from-shell" && cat "$1/hello"`, "sh", mp, container)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "browser-bytes" {
		t.Fatalf("sandbox read %q", out)
	}
	data, err := os.ReadFile(filepath.Join(local, "from-shell"))
	if err != nil || string(data) != "sandbox-written" {
		t.Fatalf("browser data = %q, err %v", data, err)
	}
}

// A mount whose browser is gone must leave the worker profile, and the close
// that follows must really unmount it.
//
// Real-host report (M65): closing one mount restarted the worker while a second
// mount of the same tenant had no browser left. The restart resolved that dead
// path and failed with "resolve browser mount: lstat ...: connection timed out",
// so the first mount stayed mounted and the page showed an unconfirmed cleanup.
// Measured on the same host: resolving one clientless mount costs the whole FUSE
// timeout and then fails, and bubblewrap's own stat of a bind source fails the
// same way ("Can't get type of source ...: Input/output error"). Both happen
// inside the worker start, so the only safe answer is to not advertise the mount.
func TestRealDeadBrowserMountLeavesProfile(t *testing.T) {
	if os.Getenv("BROWSERWORKSPACE_SANDBOX_TEST") != "1" {
		t.Skip("set BROWSERWORKSPACE_SANDBOX_TEST=1")
	}
	node, release := os.Getenv("DSHGW_NODE"), os.Getenv("DSHGW_DSH_ROOT")
	if node == "" || release == "" {
		t.Fatal("DSHGW_NODE and DSHGW_DSH_ROOT required")
	}
	base := t.TempDir()
	workspace := filepath.Join(base, "workspaces", "a")
	home := filepath.Join(base, "tenants", "a", ".dsh")
	container := filepath.Join(workspace, "browser")
	for _, p := range []string{container, home, filepath.Join(base, "config", "a")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	tenant := registry.Tenant{Name: "a", Workspace: workspace}
	var server *fuse.Server
	service := NewWithMount(func(context.Context, registry.Tenant) error { return nil }, func(path string, b fs.Backend) (Mounted, error) {
		var err error
		// The browser never answers: every operation runs out the FUSE timeout.
		server, err = fs.MountFSWithOptions(path, b, fs.MountOptions{Timeout: 2 * time.Second})
		return server, err
	})
	sh, err := service.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	if mounted, err := mountInfoPath(sh.path); err != nil || !mounted {
		t.Fatal("fixture is not actually mounted", mounted, err)
	}
	if paths := service.MountsFor("a"); len(paths) != 1 || paths[0] != sh.path {
		t.Fatalf("a served mount must be advertised: %v", paths)
	}
	// The page goes away: its poll connection is canceled while the gateway holds it.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	if _, err := service.dispatch(ctx, "poll", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("poll returned although its client was gone")
	}
	cancel()
	if paths := service.MountsFor("a"); len(paths) != 0 {
		t.Fatalf("a clientless mount is still advertised: %v", paths)
	}
	rt := sandbox.Runtime{BwrapBin: "/usr/bin/bwrap", NodeBin: node, BinJS: filepath.Join(release, "lib", "bin.js"), CurrentLink: release, TenantRoot: filepath.Join(base, "tenants"), WorkspaceRoot: filepath.Join(base, "workspaces"), TenantConfigRoot: filepath.Join(base, "config")}
	profile := sandbox.Tenant{Name: "a", Workspace: workspace, DshHome: home, WorkerPort: 32100, BrowserMountRoot: container, BrowserMounts: service.MountsFor("a")}
	argv, err := sandbox.Profile(rt, profile)
	if err != nil {
		t.Fatalf("worker profile: %v", err)
	}
	for _, arg := range argv {
		if arg == sh.path {
			t.Fatal("profile still binds the clientless mount")
		}
	}
	// The close that follows must clean up instead of reporting an unconfirmed cleanup.
	if _, err := service.dispatch(context.Background(), "close", tenant, "owner", payload{Token: sh.token}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if mounted, err := mountInfoPath(sh.path); err != nil || mounted {
		t.Fatal("host mount survived close", mounted, err)
	}
	if _, err := os.Stat(sh.path); !os.IsNotExist(err) {
		t.Fatal("mount point survived close", err)
	}
	done := make(chan error, 1)
	go func() { server.Wait(); done <- nil }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("FUSE server cleanup exceeded deadline")
	}
}

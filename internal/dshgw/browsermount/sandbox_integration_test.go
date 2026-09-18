package browsermount

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
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

package tenancy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func TestScopeWrapperCarriesEveryConfiguredLimit(t *testing.T) {
	argv := []string{"/usr/bin/bwrap", "--tmpfs", "/home", "--", "/usr/bin/node", "bin.js", "web"}
	limits := WorkerLimits{MemoryHighBytes: 1536 << 20, MemoryMaxBytes: 2 << 30, TasksMax: 512, CPUQuotaPercent: 200}
	wrapped, err := scopeWrapper("dshgw-worker-alice", limits, argv)
	if err != nil {
		t.Skipf("systemd-run unavailable here: %v", err)
	}
	joined := strings.Join(wrapped, " ")
	for _, want := range []string{
		"--user", "--scope", "--unit=dshgw-worker-alice",
		"-p MemoryHigh=1610612736", "-p MemoryMax=2147483648", "-p TasksMax=512", "-p CPUQuota=200%",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapper missing %q:\n%s", want, joined)
		}
	}
	// The worker command is passed through untouched and stays the last element:
	// the wrapper must not reinterpret the sandbox argv.
	if got := wrapped[len(wrapped)-len(argv):]; strings.Join(got, " ") != strings.Join(argv, " ") {
		t.Fatalf("worker argv was rewritten:\n%s", joined)
	}
	if wrapped[len(wrapped)-len(argv)-1] != "--" {
		t.Fatalf("worker argv is not separated by --:\n%s", joined)
	}
}

func TestScopeWrapperIsSkippedWithoutLimits(t *testing.T) {
	wrapped, err := scopeWrapper("dshgw-worker-alice", WorkerLimits{}, []string{"/usr/bin/bwrap"})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped != nil {
		t.Fatalf("unlimited worker was wrapped: %v", wrapped)
	}
}

// A host without a user manager must keep running workers: the runner warns once
// and uses the plain sandbox argv.
func TestApplyLimitsDegradesToNoLimits(t *testing.T) {
	runner := &WorkerRunner{Limits: WorkerLimits{MemoryMaxBytes: 1 << 30}}
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent/runtime/dir")
	argv := []string{"/usr/bin/bwrap", "--", "/usr/bin/node"}
	if got, _ := runner.applyLimits("dshgw-worker-alice", argv); strings.Join(got, " ") != strings.Join(argv, " ") {
		t.Fatalf("argv changed without a user manager: %v", got)
	}
	if !runner.limitsWarned {
		t.Fatal("the degradation was not recorded (it would warn per tenant)")
	}
	// The plain path is also what an unlimited runner always uses.
	plain := &WorkerRunner{}
	if got, scoped := plain.applyLimits("dshgw-worker-alice", argv); scoped || strings.Join(got, " ") != strings.Join(argv, " ") {
		t.Fatalf("unlimited runner wrapped the worker: %v", got)
	}
}

// The sandbox binds the release's resolved directory, so the plugin anchor must be
// the resolved path too: a deployment that keeps the documented
// `…/current -> releases/r1` symlink otherwise starts a worker whose every plugin
// import fails with "Cannot find module".
func TestWorkerEnvAnchorsThroughAResolvedReleasePath(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "dsh", "releases", "r1")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "dsh", "current")
	if err := os.Symlink(release, current); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Dsh: config.DshRuntime{CurrentLink: current, NodeBin: "/usr/bin/node"}}
	tenant := registry.Tenant{Name: "alice", WorkerPort: 32100, DshHome: "/state/alice/.dsh", Workspace: "/srv/alice"}

	var anchor string
	for _, entry := range workerEnv(cfg, tenant) {
		if strings.HasPrefix(entry, "DSHGW_DSH_ANCHOR=") {
			anchor = strings.TrimPrefix(entry, "DSHGW_DSH_ANCHOR=")
		}
	}
	if want := filepath.Join(release, "package.json"); anchor != want {
		t.Fatalf("anchor = %q, want the resolved release %q", anchor, want)
	}
	// A configured path that is not a symlink must be used as-is.
	plain := &config.Config{Dsh: config.DshRuntime{CurrentLink: release, NodeBin: "/usr/bin/node"}}
	if got := dshAnchor(plain); got != filepath.Join(release, "package.json") {
		t.Fatalf("anchor = %q for a plain release path", got)
	}
}

// The worker's /api fence trusts loopback and any authority declared with
// --trusted-host. Deployments in this project already run with that declaration, so
// the worker argv carries it too — and it must never repeat the same authority twice.
func TestWorkerArgsDeclareThePublicAuthorityOnce(t *testing.T) {
	cfg := &config.Config{PublicHost: "chat.example", PortalPort: 18100}
	tenant := registry.Tenant{Name: "alice", WorkerPort: 18200, PublicPort: 18101}
	args := workerArgs(cfg, tenant)

	if args[0] != "web" || !strings.Contains(strings.Join(args, " "), "--port 18200") {
		t.Fatalf("base argv changed: %v", args)
	}
	trusted := []string{}
	for i, arg := range args {
		if arg == "--trusted-host" && i+1 < len(args) {
			trusted = append(trusted, args[i+1])
		}
	}
	want := []string{"chat.example", "chat.example:18101"}
	if strings.Join(trusted, ",") != strings.Join(want, ",") {
		t.Fatalf("trusted hosts = %v, want %v", trusted, want)
	}
	// An empty public host must not produce an empty --trusted-host argument.
	bare := workerArgs(&config.Config{}, registry.Tenant{Name: "bob", WorkerPort: 18201, PublicPort: 18102})
	if strings.Contains(strings.Join(bare, " "), "--trusted-host  ") || strings.HasSuffix(strings.Join(bare, " "), "--trusted-host") {
		t.Fatalf("empty public host produced a dangling flag: %v", bare)
	}
}

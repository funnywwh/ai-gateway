package tenancy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// A 43-character token, the exact shape dsh prints in its startup URL.
const testToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// runnerFixture builds a runner whose "worker" is a stand-in shell script: the
// real profile starts bwrap+node, which needs a host that has both, while the
// runner's own responsibilities (process group, URL capture, handshake file,
// stop semantics) are all observable with any long-lived child.
func runnerFixture(t *testing.T, script string) (*WorkerRunner, *config.Config, string) {
	t.Helper()
	root := t.TempDir()
	handshake := filepath.Join(root, "handshake")
	if err := os.MkdirAll(handshake, 0o700); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(root, "fake-worker.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		HandshakeDir: handshake,
		Dsh:          config.DshRuntime{NodeBin: filepath.Join(root, "node/bin/node"), CurrentLink: filepath.Join(root, "dsh/current")},
	}
	runner := &WorkerRunner{
		Config: cfg,
		Profile: func(t registry.Tenant) ([]string, error) {
			return []string{scriptPath, itoa(t.WorkerPort)}, nil
		},
		StopTimeout: 8 * time.Second,
	}
	return runner, cfg, root
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func runningWorkerScript() string {
	return `#!/bin/sh
port="$1"
echo "dsh web: http://127.0.0.1:$port/?token=` + testToken + `"
trap 'exit 0' TERM INT
while :; do sleep 0.2; done
`
}

// testTenant builds a tenant whose workspace and dsh home exist on disk: the
// runner starts every worker with its workspace as the working directory, so a
// tenant without one is a start failure, not a fixture shortcut.
func testTenant(t *testing.T, root, name string, port int) registry.Tenant {
	t.Helper()
	workspace := filepath.Join(root, "srv", name)
	dshHome := filepath.Join(root, "state", name, ".dsh")
	for _, dir := range []string{workspace, dshHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return registry.Tenant{
		Name: name, WorkerPort: port, UID: 1000,
		Workspace: workspace, DshHome: dshHome,
		Isolation: registry.IsolationBwrap,
	}
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkerRunnerStartsCapturesURLAndStops(t *testing.T) {
	runner, cfg, root := runnerFixture(t, runningWorkerScript())
	tenant := testTenant(t, root, "alice", 32100)
	ctx := context.Background()
	if err := runner.Start(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	status := runner.Status(tenant)
	if !status.Running || status.PID <= 0 {
		t.Fatalf("worker not running: %+v", status)
	}

	// The startup URL is read from the child's own output and published as the
	// handshake file the proxy serves from.
	waitFor(t, "the startup URL", func() bool {
		url, ok := runner.StartURL("alice")
		return ok && strings.Contains(url, ":32100/?token=")
	})
	handshakePath := filepath.Join(cfg.HandshakeDir, "alice.url")
	waitFor(t, "the handshake file", func() bool {
		_, err := os.Stat(handshakePath)
		return err == nil
	})
	data, err := os.ReadFile(handshakePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), testToken) {
		t.Fatalf("handshake file does not carry the worker URL: %q", data)
	}
	info, err := os.Stat(handshakePath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("handshake mode = %04o, want 0600: it carries a session token", perm)
	}

	pid := status.PID
	if err := runner.Stop(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if runner.Status(tenant).Running {
		t.Fatal("worker still reported as running after Stop")
	}
	if _, err := os.Stat(handshakePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale handshake file survived the stop: %v", err)
	}
	if err := processAlive(pid); err == nil {
		t.Fatalf("worker process %d is still alive after Stop", pid)
	}
	// Stopping twice is not an error: the console's disable path may race a
	// worker that already exited on its own.
	if err := runner.Stop(ctx, tenant); err != nil {
		t.Fatalf("second Stop failed: %v", err)
	}
}

func TestWorkerRunnerRejectsDoubleStartAndReportsImmediateExit(t *testing.T) {
	runner, _, root := runnerFixture(t, runningWorkerScript())
	ctx := context.Background()
	tenant := testTenant(t, root, "alice", 32101)
	if err := runner.Start(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runner.Stop(ctx, tenant) }()
	if err := runner.Start(ctx, tenant); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second start accepted: %v", err)
	}

	failing, _, failingRoot := runnerFixture(t, "#!/bin/sh\necho 'boom: no dsh here' >&2\nexit 3\n")
	if err := failing.Start(ctx, testTenant(t, failingRoot, "bob", 32102)); err == nil {
		t.Fatal("a worker that exits immediately was reported as started")
	} else if !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("unexpected error: %v", err)
	} else if !strings.Contains(failing.Output("bob"), "boom") {
		t.Fatalf("worker output not retained for diagnosis: %q", failing.Output("bob"))
	}
}

// A worker that exits cleanly must stop being reported as running. This is a
// regression test for liveness inferred from the exit *error*: exit status 0 is a
// nil error, so that inference called every cleanly-exited worker alive.
func TestWorkerRunnerReportsCleanExitAsStopped(t *testing.T) {
	script := `#!/bin/sh
port="$1"
echo "dsh web: http://127.0.0.1:$port/?token=` + testToken + `"
sleep 0.4
exit 0
`
	runner, _, root := runnerFixture(t, script)
	tenant := testTenant(t, root, "alice", 32107)
	if err := runner.Start(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker to exit on its own", func() bool { return !runner.Status(tenant).Running })
	if detail := runner.Status(tenant).Detail; !strings.Contains(detail, "exited") {
		t.Fatalf("status detail = %q, want an exited description", detail)
	}
	if len(runner.Running()) != 0 {
		t.Fatalf("Running() reports a cleanly exited worker: %+v", runner.Running())
	}
	// Its output stays available until the tenant is started again.
	if !strings.Contains(runner.Output("alice"), "dsh web:") {
		t.Fatalf("output was dropped on exit: %q", runner.Output("alice"))
	}
}

func TestWorkerRunnerStartAllHonoursSuspendedAndCollectsFailures(t *testing.T) {
	runner, _, root := runnerFixture(t, runningWorkerScript())
	ctx := context.Background()
	good := testTenant(t, root, "alice", 32103)
	suspended := testTenant(t, root, "bob", 32104)
	suspended.Suspended = true
	// A tenant whose profile cannot even be built stands in for a broken
	// installation: StartAll must report it and still start the healthy tenant.
	realProfile := runner.Profile
	runner.Profile = func(t registry.Tenant) ([]string, error) {
		if t.Name == "carol" {
			return nil, errors.New("profile is unusable")
		}
		return realProfile(t)
	}
	defer func() { _ = runner.StopAll(ctx) }()
	err := runner.StartAll(ctx, []registry.Tenant{good, suspended, testTenant(t, root, "carol", 32105)})
	if err == nil || !strings.Contains(err.Error(), "carol") {
		t.Fatalf("StartAll did not report the broken tenant: %v", err)
	}
	if !runner.Status(good).Running {
		t.Fatal("healthy tenant was not started")
	}
	if runner.Status(suspended).Running {
		t.Fatal("suspended tenant was started: it must stay down until the operator enables it")
	}

	running := runner.Running()
	if len(running) != 1 || running[0].Name != "alice" {
		t.Fatalf("Running() = %+v, want only alice", running)
	}
	if err := runner.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runner.Running()) != 0 {
		t.Fatalf("StopAll left workers behind: %+v", runner.Running())
	}
}

func TestWorkerRunnerRedactsSessionTokenFromRetainedOutput(t *testing.T) {
	runner, _, root := runnerFixture(t, runningWorkerScript())
	ctx := context.Background()
	tenant := testTenant(t, root, "alice", 32106)
	if err := runner.Start(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runner.Stop(ctx, tenant) }()
	waitFor(t, "worker output", func() bool { return strings.Contains(runner.Output("alice"), "dsh web:") })
	output := runner.Output("alice")
	if strings.Contains(output, testToken) {
		t.Fatalf("worker output kept the tenant session token:\n%s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Fatalf("worker output was not redacted:\n%s", output)
	}
}

func processAlive(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(nil)
}

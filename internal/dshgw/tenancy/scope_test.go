package tenancy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// A scope name is a systemd resource. systemd refuses to create a unit whose name is
// already loaded, so a name shared by two incarnations of one tenant is a landmine:
// an unkillable leftover of the first makes every later start of that tenant fail
// instantly. This is the assertion that keeps that property.
func TestWorkerUnitNamesAreUniquePerIncarnation(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		name := workerUnitName("alice")
		if !strings.HasPrefix(name, workerUnitPrefix+"alice-") {
			t.Fatalf("scope name %q does not identify the tenant", name)
		}
		if seen[name] {
			t.Fatalf("scope name %q was handed out twice", name)
		}
		seen[name] = true
	}
	if a, b := workerUnitName("alice"), workerUnitName("bob"); a[:len(workerUnitPrefix)] != b[:len(workerUnitPrefix)] {
		t.Fatalf("scope names do not share the discoverable prefix: %q, %q", a, b)
	}
}

// fakeSystemdRun writes a systemd-run stand-in that records the scope unit it was
// asked for and answers the way the test script tells it to. It is what lets the
// launch path be exercised without the caller having a user manager: the runner
// spawns the unit name it computed, exactly as it does in production.
func fakeSystemdRun(t *testing.T, behavior string) (binary, record string) {
	t.Helper()
	root := t.TempDir()
	record = filepath.Join(root, "units.log")
	binary = filepath.Join(root, "systemd-run")
	script := `#!/bin/sh
unit=""
while [ $# -gt 0 ]; do
  case "$1" in
    --unit=*) unit="${1#--unit=}" ;;
    --) shift; break ;;
  esac
  shift
done
echo "$unit" >> "` + record + `"
` + behavior + `
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, record
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// scopedRunnerFixture builds a runner that applies limits through the given
// systemd-run stand-in. The scope's cgroup is stubbed out: this test is about launch
// and retry, not about the kernel side (TestTerminateCgroupEndsScopeMembers covers it).
func scopedRunnerFixture(t *testing.T, behavior string) (*WorkerRunner, registry.Tenant, string) {
	t.Helper()
	runner, _, root := runnerFixture(t, runningWorkerScript())
	binary, record := fakeSystemdRun(t, behavior)

	previous := systemdRunBin
	systemdRunBin = func() (string, error) { return binary, nil }
	t.Cleanup(func() { systemdRunBin = previous })

	runner.Limits = WorkerLimits{MemoryMaxBytes: 1 << 30}
	// The runtime dir only has to exist for the environment the wrapper is given.
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	if err := os.MkdirAll(filepath.Join(root, "runtime", "systemd", "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	tenant := testTenant(t, root, "scoped", 32166)
	return runner, tenant, record
}

// A launch that systemd refuses — the reported incident's exact failure, where a
// leftover scope of the same tenant was still loaded — must not leave the tenant
// without a worker. The retry runs under a name nothing else holds.
func TestStartRetriesUnderAFreshScopeWhenTheLaunchIsRefused(t *testing.T) {
	runner, tenant, record := scopedRunnerFixture(t, `if [ ! -f "`+"`dirname \"$0\"`"+`/failed-once" ]; then
  touch "`+"`dirname \"$0\"`"+`/failed-once"
  echo "Failed to start transient scope unit: Unit ${unit}.scope was already loaded or has a fragment file." >&2
  exit 1
fi
exec "$@"`)

	if err := runner.Start(context.Background(), tenant); err != nil {
		t.Fatalf("start did not recover from a refused scope: %v", err)
	}
	status := runner.Status(tenant)
	if !status.Running || status.PID <= 0 {
		t.Fatalf("worker not running after the retry: %+v", status)
	}

	units := readLines(t, record)
	if len(units) != 2 {
		t.Fatalf("systemd-run was called %d times, want 2 (one refused launch, one retry): %v", len(units), units)
	}
	if units[0] == units[1] {
		t.Fatalf("the retry reused the refused scope name %q; a leftover would block it again", units[0])
	}
	if !strings.HasPrefix(units[1], workerUnitPrefix+tenant.Name+"-") {
		t.Fatalf("retry scope %q does not identify the tenant", units[1])
	}
	// The retried worker is the real one: it prints its startup URL and is recorded.
	waitFor(t, "the startup URL", func() bool {
		url, ok := runner.StartURL(tenant.Name)
		return ok && strings.Contains(url, ":32166/?token=")
	})
}

// A worker that fails for its own reasons must be reported, not retried until the
// caller's deadline: the retry exists for launch refusal, not for a broken profile.
func TestStartDoesNotRetryAWorkerThatKeepsFailing(t *testing.T) {
	runner, _, root := runnerFixture(t, "#!/bin/sh\necho 'profile is broken' >&2\nexit 3\n")
	tenant := testTenant(t, root, "broken", 32167)
	err := runner.Start(context.Background(), tenant)
	if err == nil {
		t.Fatal("a worker that exits immediately was reported as started")
	}
	if !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// terminateCgroup is the kernel half of the fix: a process a worker left behind is not
// in the worker's process group, so it survives the SIGTERM that stops the worker and
// holds the scope — and its name — open. Ending the scope's members is what closes that.
//
// The fixture is that shape exactly: a worker inside a real systemd user scope, plus a
// detached descendant that outlives it, which is what an agent's own tool call looks
// like from the outside.
func TestTerminateCgroupEndsScopeMembers(t *testing.T) {
	if !userManagerAvailable() {
		t.Skip("no systemd user manager to create a scope with")
	}
	unit := workerUnitName("cgroup-test")
	// The worker starts a detached descendant and exits: the descendant is reparented, so
	// it is not in the worker's process group, SIGTERM for the worker never reaches it,
	// and the scope stays loaded for as long as it lives.
	worker := "/bin/sh -c 'setsid sleep 300 &'"
	scope, err := scopeWrapper(unit, WorkerLimits{MemoryMaxBytes: 256 << 20}, []string{"/bin/sh", "-c", worker})
	if err != nil {
		t.Skipf("systemd-run unavailable: %v", err)
	}
	cmd := exec.Command(scope[0], scope[1:]...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+userRuntimeDir())
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a scope here: %v", err)
	}
	// Reap the wrapper so the test leaves no zombie behind, whatever happens next.
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	// systemd creates the unit asynchronously, so the cgroup appears a moment after the
	// wrapper is spawned. The poll must not shell out: a `sleep` child of the test
	// process lands in the same cgroup, and the teardown would then be racing the test's
	// own tooling — which is exactly the production shape (a tool call started inside the
	// worker outliving the worker).
	path := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if path = findScopeCgroup(unit); path != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if path == "" {
		t.Skipf("the scope %s did not appear as a cgroup here; candidates: %v", unit, scopeCgroupCandidates(unit))
	}
	t.Cleanup(func() {
		_ = stopScope(context.Background(), unit)
		<-waited
	})
	// Wait for the orphan: the shell has to exit and the kernel reparent the sleep before
	// the fixture is the shape under test.
	var orphan int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		for _, pid := range cgroupProcesses(path) {
			if processIsOrphan(pid, path) {
				orphan = pid
				break
			}
		}
		if orphan != 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if orphan == 0 {
		t.Skipf("no detached descendant appeared in %s", path)
	}
	if err := syscall.Kill(orphan, 0); err != nil {
		t.Fatalf("the fixture's orphan %d is not alive: %v", orphan, err)
	}

	if leftover := terminateCgroup(context.Background(), path, time.Second, 3*time.Second); len(leftover) != 0 {
		t.Fatalf("processes survived the cgroup teardown: %v", leftover)
	}
	// The cgroup being empty is the invariant that matters: it is what lets the kernel
	// collect the scope, and systemd release the name.
	if remaining := cgroupProcesses(path); len(remaining) != 0 {
		t.Fatalf("cgroup still holds %v", remaining)
	}
	// The orphan itself is gone. The wait is short but real: the cgroup can empty before
	// the kernel finishes tearing the process down.
	gone := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(orphan, 0); errors.Is(err, syscall.ESRCH) {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		state, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(orphan), "stat"))
		fields := strings.Fields(string(state))
		t.Fatalf("orphan %d outlived the teardown (state %v)", orphan, fields)
	}
	if err := stopScope(context.Background(), unit); err != nil {
		t.Errorf("stopping the scope failed: %v", err)
	}
}

// processIsOrphan reports whether one process is the fixture's detached descendant: a
// sleep whose parent is outside the scope, meaning the shell that started it is gone
// (the kernel reparents it to the nearest subreaper, which is the systemd-run wrapper,
// not necessarily init).
func processIsOrphan(pid int, cgroupPath string) bool {
	if name, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err != nil || strings.TrimSpace(string(name)) != "sleep" {
		return false
	}
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return false
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent == pid {
			return false
		}
		for _, member := range cgroupProcesses(cgroupPath) {
			if member == parent {
				return false
			}
		}
		return true
	}
	return false
}

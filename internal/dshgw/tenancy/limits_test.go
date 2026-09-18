package tenancy

import (
	"strings"
	"testing"
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

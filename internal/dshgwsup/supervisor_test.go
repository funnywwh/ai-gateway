package dshgwsup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeChild writes a stand-in dshgw: it must behave like the real one in exactly
// two ways — it takes `--config PATH serve`, and it prints the readiness line
// before it is considered up. Everything else about the child is irrelevant to
// what this package owns (process group, readiness wait, restart budget, stop).
func fakeChild(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-dshgw.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func supervisorFor(t *testing.T, binary string, opts ...func(*Options)) *Supervisor {
	t.Helper()
	options := Options{
		Binary:       binary,
		ConfigPath:   filepath.Join(t.TempDir(), "config.yaml"),
		Logger:       quietLogger(),
		ReadyTimeout: 5 * time.Second,
		StopTimeout:  5 * time.Second,
	}
	for _, apply := range opts {
		apply(&options)
	}
	s, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLookupBinaryPrefersTheSiblingNextToAigw(t *testing.T) {
	dir := t.TempDir()
	aigwPath := filepath.Join(dir, "aigw")
	if err := os.WriteFile(aigwPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "dshgw")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := LookupBinary(aigwPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != sibling {
		t.Fatalf("LookupBinary = %q, want the sibling %q", got, sibling)
	}
	// An explicit override is honoured, but only as an absolute path.
	if got, err := LookupBinary(aigwPath, sibling); err != nil || got != sibling {
		t.Fatalf("override rejected: %q / %v", got, err)
	}
	if _, err := LookupBinary(aigwPath, "dshgw"); err == nil {
		t.Fatal("relative override accepted")
	}
	// A missing sibling is an error, not a silent no-op: the operator asked for a
	// DSH surface and there is no program to provide it.
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupBinary(aigwPath, ""); err == nil {
		t.Fatal("missing sibling accepted")
	}
	if _, err := LookupBinary("", ""); err == nil {
		t.Fatal("empty executable path accepted")
	}
}

func TestChildConfigValidatesAndWritesOnce(t *testing.T) {
	valid := ChildConfig{
		PublicHost: "dsh.example", PortalPort: 31000,
		TenantPortLo: 31001, TenantPortHi: 31299,
		WorkerPortLo: 31300, WorkerPortHi: 31599,
		Listen:      "127.0.0.1:31699",
		AigwBaseURL: "http://127.0.0.1:8088",
		AdminSocket: "/run/user/1000/dshgw/admin.sock",
		StateDir:    "/home/u/.local/share/dshgw", WorkspaceRoot: "/home/u/.local/share/dshgw/workspaces",
		Dsh: DshRuntime{
			NodeBin: "/home/u/.local/node/bin/node", BinJS: "/home/u/.local/dsh/lib/bin.js",
			CurrentLink: "/home/u/.local/dsh/current",
		},
		Deploy: Deploy{
			WorkerUser:       "u",
			TenantConfigRoot: "/home/u/.local/share/dshgw/tenants", TemplateHome: "/home/u/.local/share/dshgw/template-home",
			ConfigPath: "/home/u/.local/share/dshgw/config.yaml",
			BackupDir:  "/home/u/.local/share/dshgw/backups",
		},
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	changed, err := valid.WriteIfChanged(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first write reported no change")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"public_host: dsh.example", "worker_user: u", "127.0.0.1:31699"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("generated config missing %q:\n%s", want, data)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", perm)
	}
	// A second start with identical inputs must not touch the file: operators use
	// mtime to notice real configuration changes.
	changed, err = valid.WriteIfChanged(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("unchanged configuration was rewritten")
	}

	// The child would reject these; aigw refuses them first, where the operator can
	// see which setting is wrong instead of reading a restart loop.
	for name, mutate := range map[string]func(*ChildConfig){
		"public host with a port":   func(c *ChildConfig) { c.PublicHost = "dsh.example:8443" },
		"worker user empty":         func(c *ChildConfig) { c.Deploy.WorkerUser = "" },
		"worker user is root":       func(c *ChildConfig) { c.Deploy.WorkerUser = "root" },
		"listen outside loopback":   func(c *ChildConfig) { c.Listen = "0.0.0.0:31699" },
		"listen inside a range":     func(c *ChildConfig) { c.Listen = "127.0.0.1:31050" },
		"relative state dir":        func(c *ChildConfig) { c.StateDir = "state" },
		"overlapping port ranges":   func(c *ChildConfig) { c.WorkerPortLo = 31200 },
		"worker ports above 65535":  func(c *ChildConfig) { c.WorkerPortHi = 70000 },
		"portal inside tenant band": func(c *ChildConfig) { c.PortalPort = 31100 },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSupervisorWaitsForReadinessAndStopsTheChild(t *testing.T) {
	binary := fakeChild(t, "echo 'time=... level=INFO msg=\"dshgw listening\" listen=127.0.0.1:31699'\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.2; done\n")
	s := supervisorFor(t, binary)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.Ready() {
		t.Fatal("Start returned before readiness")
	}
	pid := s.PID()
	if pid <= 0 {
		t.Fatalf("pid = %d", pid)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d survived Stop: %v", pid, err)
	}
	// Stopping twice is not an error: aigw's shutdown path may race a child that
	// already exited.
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("second Stop failed: %v", err)
	}
}

func TestSupervisorReportsAChildThatExitsBeforeReady(t *testing.T) {
	binary := fakeChild(t, "echo 'boom: bad config' >&2\nexit 2\n")
	s := supervisorFor(t, binary)
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("a child that never became ready was reported as started")
	}
	if !strings.Contains(err.Error(), "before becoming ready") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The child's own words are the diagnosis; without them the operator sees only
	// "exit status 2".
	if !strings.Contains(s.RecentOutput(), "boom") {
		t.Fatalf("child output was not retained: %q", s.RecentOutput())
	}
}

func TestSupervisorRestartsWithinBudgetAndThenGivesUp(t *testing.T) {
	// Counts its runs, exits immediately every time: the budget must run out.
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	binary := fakeChild(t, "echo x >> "+counter+"\nexit 1\n")
	s := supervisorFor(t, binary, func(o *Options) {
		o.MaxRestarts = 2
		o.RestartWindow = time.Minute
		o.ReadyTimeout = 3 * time.Second
	})
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("exhausted restart budget reported as success")
	}
	if !strings.Contains(err.Error(), "restart budget") {
		t.Fatalf("unexpected error: %v", err)
	}
	data, readErr := os.ReadFile(counter)
	if readErr != nil {
		t.Fatal(readErr)
	}
	// One initial attempt plus MaxRestarts retries, and no more: a crash loop must
	// not spin.
	if runs := len(strings.Fields(string(data))); runs != 3 {
		t.Fatalf("child ran %d times, want 3 (1 attempt + 2 restarts)", runs)
	}
}

func TestSupervisorRestartsAChildThatDiesAndKeepsServing(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	// First run: become ready, then die. Later runs: stay up.
	binary := fakeChild(t, `echo x >> `+counter+`
echo 'msg="dshgw listening"'
if [ "$(wc -l < `+counter+`)" = "1" ]; then exit 1; fi
trap 'exit 0' TERM INT
while :; do sleep 0.2; done
`)
	s := supervisorFor(t, binary, func(o *Options) { o.MaxRestarts = 2 })
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop(ctx) }()
	// Start waits for the *second* child's readiness, so a live pid plus readiness
	// is the proof that the restart happened and the replacement is serving.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(counter); err == nil && len(strings.Fields(string(data))) >= 2 && s.Ready() && s.PID() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no restart observed: ready=%t pid=%d", s.Ready(), s.PID())
}

func TestSupervisorStopKillsTheWholeProcessGroup(t *testing.T) {
	// A child that starts a grandchild: dshgw does exactly this with tenant
	// workers, and SIGTERM to the group is what keeps them from being orphaned.
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	binary := fakeChild(t, `sh -c 'echo $$ > `+pidfile+`; trap "" TERM; while :; do sleep 0.2; done' &
echo 'msg="dshgw listening"'
trap 'exit 0' TERM INT
while :; do sleep 0.2; done
`)
	s := supervisorFor(t, binary)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var grandchild int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidfile); err == nil && strings.TrimSpace(string(data)) != "" {
			var pid int
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); scanErr == nil {
				grandchild = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("grandchild never started")
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// The grandchild ignores SIGTERM, so it must have received the group's SIGKILL
	// (or died with the group). Either way it must not outlive the supervisor.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(grandchild, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d outlived the supervised child", grandchild)
}

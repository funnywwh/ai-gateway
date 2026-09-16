package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/contract"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

func release(t *testing.T, path, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "package.json"), []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "lib/bin.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeRunner struct {
	statuses    map[string]bool
	statusErrs  map[string]error
	restartErrs map[string][]error
	restartCall map[string]int
	onRestart   func(string, int)
}

func (r *fakeRunner) Run(_ context.Context, command tenancy.Command) ([]byte, error) {
	if len(command.Args) < 2 {
		return nil, fmt.Errorf("unexpected command %#v", command)
	}
	unit := command.Args[len(command.Args)-1]
	name := strings.TrimSuffix(strings.TrimPrefix(unit, "dsh-worker@"), ".service")
	switch command.Args[0] {
	case "show":
		if err := r.statusErrs[name]; err != nil {
			return nil, err
		}
		activeState := "inactive"
		if r.statuses[name] {
			activeState = "active"
		}
		return []byte("LoadState=loaded\nActiveState=" + activeState + "\nUnitFileState=enabled\n"), nil
	case "is-active":
		if err := r.statusErrs[name]; err != nil {
			return nil, err
		}
		if r.statuses[name] {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), nil
	case "is-enabled":
		return []byte("enabled\n"), nil
	case "restart":
		r.restartCall[name]++
		if r.onRestart != nil {
			r.onRestart(name, r.restartCall[name])
		}
		errs := r.restartErrs[name]
		if len(errs) == 0 {
			return nil, nil
		}
		err := errs[0]
		r.restartErrs[name] = errs[1:]
		if err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command %q", command.Args[0])
	}
}

type upgradeFixture struct {
	root      string
	old       string
	next      string
	current   string
	registry  *registry.Registry
	config    *config.Config
	runner    *fakeRunner
	manager   *tenancy.Manager
	probes    map[string][]error
	probeCall map[string]int
}

func newUpgradeFixture(t *testing.T, active ...string) *upgradeFixture {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	if err := os.MkdirAll(releases, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(releases, "old")
	next := filepath.Join(releases, "next")
	release(t, old, "1")
	release(t, next, "2")
	switchDir := filepath.Join(root, "switch")
	if err := os.MkdirAll(switchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(switchDir, "current")
	if err := os.Symlink(old, current); err != nil {
		t.Fatal(err)
	}

	activeSet := make(map[string]bool, len(active))
	for _, name := range active {
		activeSet[name] = true
	}
	runner := &fakeRunner{
		statuses:    activeSet,
		statusErrs:  make(map[string]error),
		restartErrs: make(map[string][]error),
		restartCall: make(map[string]int),
	}
	reg := registry.New(filepath.Join(root, "registry.json"), filepath.Join(root, "keys.map"))
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"alice", "bob"} {
		if !activeSet[name] && len(active) == 0 {
			// Keep the default fixture empty unless callers explicitly request
			// a worker state. Tests add tenants through this branch below.
			_ = i
			continue
		}
		tenant := registry.Tenant{
			Name:       name,
			UID:        1001 + i,
			PublicPort: 32601 + i,
			WorkerPort: 32101 + i,
			KeyPrefix:  fmt.Sprintf("sk-%09d", i+1),
			DshHome:    filepath.Join(root, "tenants", name, ".dsh"),
			Workspace:  filepath.Join(root, "work", name),
			CreatedAt:  created,
			Handshake:  registry.HandshakeOK,
		}
		if err := reg.Put(tenant); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Dsh:      config.DshRuntime{NodeBin: "node", ReleasesRoot: releases, CurrentLink: current},
		Deploy:   config.DeployConfig{PluginPath: "plugin"},
		StateDir: root,
	}
	f := &upgradeFixture{root: root, old: old, next: next, current: current, registry: reg, config: cfg, runner: runner, probes: make(map[string][]error), probeCall: make(map[string]int)}
	f.manager = &tenancy.Manager{Config: cfg, Registry: reg, Runner: runner, Probe: f.probe}
	return f
}

func (f *upgradeFixture) probe(_ context.Context, tenant registry.Tenant) error {
	f.probeCall[tenant.Name]++
	errs := f.probes[tenant.Name]
	if len(errs) == 0 {
		return nil
	}
	err := errs[0]
	f.probes[tenant.Name] = errs[1:]
	return err
}

func (f *upgradeFixture) apply(t *testing.T) (Result, error) {
	t.Helper()
	return Apply(context.Background(), Options{
		Config:    f.config,
		Registry:  f.registry,
		Manager:   f.manager,
		Candidate: f.next,
		Checks:    []contract.Check{{Name: "ok", Required: true, Run: func(context.Context) error { return nil }}},
		RunPicker: func(context.Context, string) error { return nil },
	})
}

func TestApplyRestartsOnlyPreviouslyActiveWorkersOnSuccess(t *testing.T) {
	f := newUpgradeFixture(t, "alice")
	// Add an intentionally stopped registered worker.
	bob := registry.Tenant{Name: "bob", UID: 1003, PublicPort: 32603, WorkerPort: 32103, KeyPrefix: "sk-000000003", DshHome: filepath.Join(f.root, "tenants/bob/.dsh"), Workspace: filepath.Join(f.root, "work/bob"), CreatedAt: time.Now(), Handshake: registry.HandshakeOK}
	if err := f.registry.Put(bob); err != nil {
		t.Fatal(err)
	}
	result, err := f.apply(t)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(result.Restarted, []string{"alice"}) {
		t.Fatalf("restarted=%v", result.Restarted)
	}
	if f.runner.restartCall["alice"] != 1 || f.runner.restartCall["bob"] != 0 {
		t.Fatalf("restart calls=%v", f.runner.restartCall)
	}
	if f.probeCall["alice"] != 1 || f.probeCall["bob"] != 0 {
		t.Fatalf("probe calls=%v", f.probeCall)
	}
}

func TestApplyFailureLeavesInactiveWorkersUntouched(t *testing.T) {
	f := newUpgradeFixture(t, "alice")
	bob := registry.Tenant{Name: "bob", UID: 1003, PublicPort: 32603, WorkerPort: 32103, KeyPrefix: "sk-000000003", DshHome: filepath.Join(f.root, "tenants/bob/.dsh"), Workspace: filepath.Join(f.root, "work/bob"), CreatedAt: time.Now(), Handshake: registry.HandshakeOK}
	if err := f.registry.Put(bob); err != nil {
		t.Fatal(err)
	}
	candidateErr := errors.New("candidate restart failed")
	f.runner.restartErrs["alice"] = []error{candidateErr, nil}
	_, err := f.apply(t)
	if !errors.Is(err, candidateErr) {
		t.Fatalf("error=%v, want candidate restart error", err)
	}
	if f.runner.restartCall["alice"] != 2 || f.runner.restartCall["bob"] != 0 {
		t.Fatalf("restart calls=%v", f.runner.restartCall)
	}
	if f.probeCall["bob"] != 0 {
		t.Fatalf("inactive worker was probed: %v", f.probeCall)
	}
	if target, evalErr := filepath.EvalSymlinks(f.current); evalErr != nil || target != f.old {
		t.Fatalf("current target=%q err=%v", target, evalErr)
	}
}

func TestApplyStatusFailurePreventsLinkChange(t *testing.T) {
	f := newUpgradeFixture(t, "alice")
	statusErr := errors.New("status unavailable")
	f.runner.statusErrs["alice"] = statusErr
	_, err := f.apply(t)
	if !errors.Is(err, statusErr) {
		t.Fatalf("error=%v, want status error", err)
	}
	if f.runner.restartCall["alice"] != 0 {
		t.Fatalf("restart calls=%v", f.runner.restartCall)
	}
	if target, evalErr := filepath.EvalSymlinks(f.current); evalErr != nil || target != f.old {
		t.Fatalf("current target=%q err=%v", target, evalErr)
	}
}

func TestApplyPreservesRollbackRestartAndProbeErrors(t *testing.T) {
	f := newUpgradeFixture(t, "alice", "bob")
	candidateErr := errors.New("candidate restart failed")
	rollbackRestartErr := errors.New("rollback restart failed")
	rollbackProbeErr := errors.New("rollback probe failed")
	f.runner.restartErrs["alice"] = []error{nil, rollbackRestartErr}
	f.runner.restartErrs["bob"] = []error{candidateErr, nil}
	f.probes["alice"] = []error{nil, rollbackProbeErr}
	_, err := f.apply(t)
	for _, want := range []error{candidateErr, rollbackRestartErr, rollbackProbeErr} {
		if !errors.Is(err, want) {
			t.Errorf("error=%v does not preserve %v", err, want)
		}
	}
	if target, evalErr := filepath.EvalSymlinks(f.current); evalErr != nil || target != f.old {
		t.Fatalf("current target=%q err=%v", target, evalErr)
	}
}

func TestApplyDoesNotRestartWhenLinkRestoreFails(t *testing.T) {
	f := newUpgradeFixture(t, "alice")
	candidateErr := errors.New("candidate restart failed")
	f.runner.restartErrs["alice"] = []error{candidateErr}
	f.runner.onRestart = func(name string, call int) {
		if name != "alice" || call != 1 {
			return
		}
		// Make restoration fail with EEXIST: the old link path has become a
		// directory, so workers must not be restarted against the candidate.
		if err := os.Remove(f.current); err != nil {
			t.Fatalf("remove current: %v", err)
		}
		if err := os.Mkdir(f.current, 0o755); err != nil {
			t.Fatalf("replace current with directory: %v", err)
		}
	}
	_, err := f.apply(t)
	if !errors.Is(err, candidateErr) || !strings.Contains(err.Error(), "restore dsh.current_link") {
		t.Fatalf("error=%v, want candidate and link restore errors", err)
	}
	if f.runner.restartCall["alice"] != 1 {
		t.Fatalf("rollback restart happened after link restore failure: %v", f.runner.restartCall)
	}
}

func TestApplyRejectsNestedCandidate(t *testing.T) {
	f := newUpgradeFixture(t)
	nested := filepath.Join(f.next, "nested")
	release(t, nested, "3")
	_, err := Apply(context.Background(), Options{
		Config:    f.config,
		Registry:  f.registry,
		Manager:   f.manager,
		Candidate: nested,
		Checks:    []contract.Check{{Name: "ok", Required: true, Run: func(context.Context) error { return nil }}},
		RunPicker: func(context.Context, string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "one release directory below") {
		t.Fatalf("error=%v, want nested candidate rejection", err)
	}
	if target, evalErr := filepath.EvalSymlinks(f.current); evalErr != nil || target != f.old {
		t.Fatalf("current target=%q err=%v", target, evalErr)
	}
}

func TestApplySwitchesContractedCandidate(t *testing.T) {
	f := newUpgradeFixture(t)
	result, err := f.apply(t)
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(f.current)
	if err != nil {
		t.Fatal(err)
	}
	if target != f.next || result.Version != "2" || result.Current != f.next || result.Previous != f.old {
		t.Fatalf("target=%s result=%#v", target, result)
	}
}

func TestApplyFailureLeavesUntouchedActiveWorkersAlone(t *testing.T) {
	f := newUpgradeFixture(t, "alice", "bob")
	candidateErr := errors.New("candidate restart failed")
	f.runner.restartErrs["alice"] = []error{candidateErr, nil}
	_, err := f.apply(t)
	if !errors.Is(err, candidateErr) {
		t.Fatalf("error=%v, want candidate restart error", err)
	}
	if f.runner.restartCall["bob"] != 0 || f.probeCall["bob"] != 0 {
		t.Fatalf("untouched active worker was operated on: restarts=%v probes=%v", f.runner.restartCall, f.probeCall)
	}
	if target, evalErr := filepath.EvalSymlinks(f.current); evalErr != nil || target != f.old {
		t.Fatalf("current target=%q err=%v", target, evalErr)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

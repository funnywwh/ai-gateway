package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// recordingValidator remembers which key each model lookup used: the whole point of the login
// refresh is that the platform slice comes from the tenant's OWN worker key.
type recordingValidator struct {
	models []aigw.Model
	err    error
	keys   []string
}

func (v *recordingValidator) ValidateKey(_ context.Context, key string) ([]aigw.Model, error) {
	v.keys = append(v.keys, key)
	return v.models, v.err
}

type loginHarness struct {
	ops      managerOps
	manager  *tenancy.Manager
	runner   *tenancy.WorkerRunner
	tenant   registry.Tenant
	cfg      *config.Config
	dshHome  string
	settings string
	creds    string
	stored   string
}

// newLoginHarness builds a provisioned tenant whose worker is a stand-in process, so both halves
// of the login hook are observable: the files it rewrites and the worker it has to bring back.
func newLoginHarness(t *testing.T, validator keyValidator) *loginHarness {
	t.Helper()
	root := t.TempDir()
	name := "alice"
	stored := "sk-aaaaaaaaa-stored"
	cfg := &config.Config{
		AigwBaseURL: "http://aigw", StateDir: filepath.Join(root, "state"),
		TenantRoot: filepath.Join(root, "state/tenants"), WorkspaceRoot: filepath.Join(root, "srv"),
		RegistryPath: filepath.Join(root, "registry.json"), KeyMapPath: filepath.Join(root, "keys.map"),
		DirectoryPicker: "clamp", PluginBrowserFS: "off",
		Deploy: config.DeployConfig{TenantConfigRoot: filepath.Join(root, "etc/tenants")},
	}
	dshHome := filepath.Join(cfg.TenantRoot, name, ".dsh")
	configDir := filepath.Join(cfg.Deploy.TenantConfigRoot, name)
	workspace := filepath.Join(cfg.WorkspaceRoot, name)
	for _, dir := range []string{dshHome, configDir, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configDir, "gateway.key"), []byte(stored+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A provisioned tenant: settings.yaml exists, which is what makes EnsureProvisioned a no-op
	// and leaves SyncModels as the only writer of the platform slice.
	settings := "llm-pi-ai:\n  providers:\n    aigw:\n      models: [{id: stale-model}]\n    tenant-own:\n      apiKeyEnv: TENANT_KEY\n      models: [{id: mine}]\n" +
		"agent-default-model:\n  provider: aigw\n  model: stale-model\n" +
		"permission:\n  defaultPreset: danger-full-access\nui-theme:\n  preference: dark\n"
	settingsPath := filepath.Join(dshHome, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := "version: 1\nrefs:\n  TENANT_KEY: tenant-owned\nrecords:\n  client-connection/browser-session:\n    kind: grant\n    payload:\n      secret: keep-me\n"
	credsPath := filepath.Join(dshHome, ".credentials.yaml")
	if err := os.WriteFile(credsPath, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	tenant := registry.Tenant{
		Name: name, WorkerPort: 32100, PublicPort: 32601, UID: os.Geteuid(), Isolation: registry.IsolationBwrap,
		DshHome: dshHome, Workspace: workspace, CreatedAt: time.Now().UTC(),
		KeyPrefix: stored[:12], Handshake: registry.HandshakeOK,
	}
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(root, "fake-worker.sh")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.2; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &tenancy.WorkerRunner{
		Config:      cfg,
		Profile:     func(tn registry.Tenant) ([]string, error) { return []string{worker, strconv.Itoa(tn.WorkerPort)}, nil },
		StopTimeout: 2 * time.Second,
	}
	manager := &tenancy.Manager{
		Config: cfg, Registry: reg, Workers: runner, RuntimeCheck: nil,
		WorkerAccount: func(string) error { return nil },
		Probe:         func(context.Context, registry.Tenant) error { return nil },
	}
	t.Cleanup(func() { _ = manager.ShutdownWorkers(context.Background()) })
	return &loginHarness{
		ops:     managerOps{m: manager, validator: validator, cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		manager: manager, runner: runner, tenant: tenant, cfg: cfg, dshHome: dshHome,
		settings: settingsPath, creds: credsPath, stored: stored,
	}
}

func (f *loginHarness) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The login refresh applies the platform slice — the model list the tenant's worker key is
// granted, and the credential reference that provider reads — and leaves everything the tenant
// owns exactly as it was (M69).
func TestPrepareLoginAppliesThePlatformSliceFromTheStoredKey(t *testing.T) {
	validator := &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}, {ID: "u2-flash"}}}
	fixture := newLoginHarness(t, validator)

	// The login presented a different key of the same account. It must not become the tenant's
	// worker key (that would restart the worker under whoever logs in), and it must not decide
	// the model list either: the worker key does.
	if err := fixture.ops.PrepareLogin(context.Background(), "alice", "sk-bbbbbbbbb-other"); err != nil {
		t.Fatalf("PrepareLogin: %v", err)
	}
	if len(validator.keys) != 1 || validator.keys[0] != fixture.stored {
		t.Fatalf("the model list was fetched with %q, want the tenant's stored key", validator.keys)
	}
	if key := fixture.read(t, filepath.Join(fixture.cfg.Deploy.TenantConfigRoot, "alice", "gateway.key")); strings.TrimSpace(key) != fixture.stored {
		t.Fatalf("the tenant's stored key was rewritten: %q", key)
	}

	settings := fixture.read(t, fixture.settings)
	for _, want := range []string{"deepseek-flash", "u2-flash"} {
		if !strings.Contains(settings, want) {
			t.Fatalf("settings.yaml is missing the platform model %q:\n%s", want, settings)
		}
	}
	if strings.Contains(settings, "stale-model") {
		t.Fatalf("the platform's own stale entry survived the refresh:\n%s", settings)
	}
	// The tenant's half of the file: their provider, their theme, their permission preset.
	for _, want := range []string{"tenant-own:", "TENANT_KEY", "mine", "preference: dark", "defaultPreset: danger-full-access"} {
		if !strings.Contains(settings, want) {
			t.Fatalf("the tenant's own setting %q was lost:\n%s", want, settings)
		}
	}

	creds := fixture.read(t, fixture.creds)
	for _, want := range []string{"AIGW_API_KEY: " + fixture.stored, "TENANT_KEY: tenant-owned", "secret: keep-me"} {
		if !strings.Contains(creds, want) {
			t.Fatalf("credentials are missing %q:\n%s", want, creds)
		}
	}

	// The worker is signed-out territory: signing in has to bring it back.
	if running := fixture.runner.Running(); len(running) != 1 {
		t.Fatalf("the login did not start the tenant's worker: %+v", running)
	}
}

// A login that has already been authenticated must not be turned into a failure by a refresh
// that could not run; and a refresh that could not run must not half-write the tenant's files.
func TestPrepareLoginLeavesTheFilesAloneWhenAigwFails(t *testing.T) {
	validator := &recordingValidator{err: errors.New("aigw unreachable")}
	fixture := newLoginHarness(t, validator)
	settingsBefore := fixture.read(t, fixture.settings)
	credsBefore := fixture.read(t, fixture.creds)
	err := fixture.ops.PrepareLogin(context.Background(), "alice", "")
	if err == nil || !strings.Contains(err.Error(), "aigw unreachable") {
		t.Fatalf("a failed refresh was not reported: %v", err)
	}
	if got := fixture.read(t, fixture.settings); got != settingsBefore {
		t.Fatalf("settings changed despite the failure:\n%s", got)
	}
	if got := fixture.read(t, fixture.creds); got != credsBefore {
		t.Fatalf("credentials changed despite the failure:\n%s", got)
	}
	if running := fixture.runner.Running(); len(running) != 0 {
		t.Fatalf("a worker was started despite the failure: %+v", running)
	}
}

// The operator's suspension is not something a login may clear.
func TestPrepareLoginDoesNotResurrectASuspendedTenant(t *testing.T) {
	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	tenant, _ := fixture.manager.Registry.Get("alice")
	tenant.Suspended = true
	if err := fixture.manager.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	err := fixture.ops.PrepareLogin(context.Background(), "alice", "")
	if err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("a suspended tenant was prepared as a startable one: %v", err)
	}
	if running := fixture.runner.Running(); len(running) != 0 {
		t.Fatalf("a suspended tenant got a worker: %+v", running)
	}
	// The platform slice still landed: the tenant is configured, just not running.
	if settings := fixture.read(t, fixture.settings); !strings.Contains(settings, "deepseek-flash") {
		t.Fatalf("the platform slice was skipped for a suspended tenant:\n%s", settings)
	}
	current, _ := fixture.manager.Registry.Get("alice")
	if !current.Suspended {
		t.Fatal("the durable suspension was cleared by a login")
	}
}

// The host's own dsh is not dshgw's business: the whole login path must run without reading or
// writing $HOME/.dsh, even though the operator's settings live there (M69, "不要碰宿主机的").
func TestPrepareLoginNeverTouchesTheHostsDshHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostSettings := filepath.Join(home, ".dsh", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(hostSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostSettings, []byte("host-owned: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(hostSettings)
	if err != nil {
		t.Fatal(err)
	}

	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	if err := fixture.ops.PrepareLogin(context.Background(), "alice", "sk-bbbbbbbbb-other"); err != nil {
		t.Fatalf("PrepareLogin: %v", err)
	}
	if got := fixture.read(t, hostSettings); got != "host-owned: true\n" {
		t.Fatalf("the host's settings were rewritten: %q", got)
	}
	after, err := os.Stat(hostSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the host's settings were touched: %v -> %v", before.ModTime(), after.ModTime())
	}
	entries, err := os.ReadDir(filepath.Join(home, ".dsh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dshgw wrote into the host's dsh home: %v", entries)
	}
}

// StopSignedOut is the logout half: the tenant's worker goes away and the durable suspension
// stays clear, so the next login brings it back.
func TestStopSignedOutStopsWithoutSuspending(t *testing.T) {
	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	ctx := context.Background()
	if err := fixture.ops.PrepareLogin(ctx, "alice", ""); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.ops.StopSignedOut(ctx, "alice")
	if err != nil {
		t.Fatalf("StopSignedOut: %v", err)
	}
	if !result.WorkerStopped {
		t.Fatal("StopSignedOut did not verify the worker is gone")
	}
	if running := fixture.runner.Running(); len(running) != 0 {
		t.Fatalf("the worker outlived the logout: %+v", running)
	}
	tenant, _ := fixture.manager.Registry.Get("alice")
	if tenant.Suspended {
		t.Fatal("a logout recorded an operator suspension")
	}
	if err := fixture.ops.PrepareLogin(ctx, "alice", ""); err != nil {
		t.Fatalf("the tenant could not sign in again: %v", err)
	}
	if running := fixture.runner.Running(); len(running) != 1 {
		t.Fatalf("signing in again did not start the worker: %+v", running)
	}
}

// A key source that cannot produce the tenant's key is a configuration problem for the
// operator, reported to the caller rather than papered over with the key the login presented.
func TestPrepareLoginReportsAMissingStoredKey(t *testing.T) {
	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	if err := os.Remove(filepath.Join(fixture.cfg.Deploy.TenantConfigRoot, "alice", "gateway.key")); err != nil {
		t.Fatal(err)
	}
	err := fixture.ops.PrepareLogin(context.Background(), "alice", "")
	if err == nil || !strings.Contains(err.Error(), "stored key") {
		t.Fatalf("a missing stored key was not reported: %v", err)
	}
}

// loginMountHook is the ssh-workspace slice the login path uses (M76). It records the order of
// the calls and reports what the account has attached, so a test can prove that the remount
// happens BEFORE the worker starts — the worker's profile binds the mount points that exist when
// it starts, so the other order would leave the account looking at empty directories.
type loginMountHook struct {
	order         []string
	attached      []string
	restoreErr    error
	duringRestore func()
}

func (h *loginMountHook) MountsFor(string) []string                   { return nil }
func (h *loginMountHook) EnsureIdentity(string, string, string) error { return nil }
func (h *loginMountHook) DropTenant(context.Context, string) error    { return nil }
func (h *loginMountHook) DetachTenant(context.Context, string) error  { return nil }
func (h *loginMountHook) AttachedMounts(string) []string              { return h.attached }
func (h *loginMountHook) Restore(context.Context, string, string, string) error {
	h.order = append(h.order, "restore")
	if h.duringRestore != nil {
		h.duringRestore()
	}
	if h.restoreErr != nil {
		return h.restoreErr
	}
	if len(h.attached) == 0 {
		h.attached = []string{"/srv/state/workspaces/alice/ssh/aipc/home"}
	}
	return nil
}

type auditRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *auditRecorder) Write(event audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *auditRecorder) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	kinds := make([]string, 0, len(s.events))
	for _, event := range s.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// Signing out detached the account's ssh workspaces, so signing in has to put them back — and
// before the worker starts, because a running worker keeps the profile it started with.
func TestPrepareLoginRemountsBeforeStartingTheWorker(t *testing.T) {
	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	ctx := context.Background()
	// The account's mount was detached by an earlier logout.
	hook := &loginMountHook{attached: []string{}}
	hook.duringRestore = func() {
		if running := fixture.runner.Running(); len(running) != 0 {
			t.Error("the mounts were restored after the worker started; its profile would not bind them")
		}
	}
	fixture.manager.SSHWorkspaces = hook

	if err := fixture.ops.PrepareLogin(ctx, "alice", ""); err != nil {
		t.Fatalf("PrepareLogin: %v", err)
	}
	if len(hook.order) != 1 || hook.order[0] != "restore" {
		t.Fatalf("mount calls = %v, want one restore", hook.order)
	}
	if running := fixture.runner.Running(); len(running) != 1 {
		t.Fatalf("the worker was not started: %+v", running)
	}
}

// A mount that cannot come back is reported and audited, and it never costs the person their
// login: the worker still starts, and the page tells them what is missing.
func TestPrepareLoginSurvivesAMountThatCannotComeBack(t *testing.T) {
	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	sink := &auditRecorder{}
	fixture.ops.auditor = sink
	hook := &loginMountHook{restoreErr: errors.New("ssh: connect to host aipc port 22: Connection refused")}
	fixture.manager.SSHWorkspaces = hook

	if err := fixture.ops.PrepareLogin(context.Background(), "alice", ""); err != nil {
		t.Fatalf("a failed remount must not fail the login: %v", err)
	}
	if running := fixture.runner.Running(); len(running) != 1 {
		t.Fatalf("the worker was not started: %+v", running)
	}
	kinds := sink.kinds()
	if len(kinds) != 1 || kinds[0] != "login_mount_restore_failed" {
		t.Fatalf("audit events = %v, want one login_mount_restore_failed", kinds)
	}
}

// A worker that is already running keeps its profile: the restored mount is announced as deferred
// instead of silently restarting a worker that may be in the middle of a turn (M69 D4).
func TestPrepareLoginDefersAMountThatTheRunningWorkerCannotSee(t *testing.T) {
	fixture := newLoginHarness(t, &recordingValidator{models: []aigw.Model{{ID: "deepseek-flash"}}})
	ctx := context.Background()
	sink := &auditRecorder{}
	fixture.ops.auditor = sink
	// A worker that is already serving somebody: the login hook leaves it alone.
	if started, err := fixture.manager.EnsureRunning(ctx, fixture.tenant); err != nil || !started {
		t.Fatalf("preparing a running worker: started=%t err=%v", started, err)
	}
	hook := &loginMountHook{}
	fixture.manager.SSHWorkspaces = hook

	if err := fixture.ops.PrepareLogin(ctx, "alice", ""); err != nil {
		t.Fatalf("PrepareLogin: %v", err)
	}
	if running := fixture.runner.Running(); len(running) != 1 {
		t.Fatalf("the already-running worker was replaced: %+v", running)
	}
	deferred := false
	for _, kind := range sink.kinds() {
		if kind == "ssh_mount_restore_deferred" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatalf("audit events = %v, want ssh_mount_restore_deferred", sink.kinds())
	}
}

package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

type fakeRunner struct {
	commands []Command
	fail     string
	hook     func(context.Context, Command) ([]byte, error, bool)
}

func (f *fakeRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	f.commands = append(f.commands, c)
	if f.hook != nil {
		if out, err, handled := f.hook(ctx, c); handled {
			return out, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	joined := c.Path + " " + strings.Join(c.Args, " ")
	if strings.Contains(joined, f.fail) && f.fail != "" {
		return nil, errors.New("forced")
	}
	if c.Path == "id" {
		return []byte("1234\n"), nil
	}
	if c.Path == "systemctl" && len(c.Args) > 1 && c.Args[0] == "show" && c.Args[1] == "--property=LoadState" {
		return systemdStatus("active", "enabled"), nil
	}
	return nil, nil
}

func systemdStatus(active, enabled string) []byte {
	return []byte(fmt.Sprintf("LoadState=loaded\nActiveState=%s\nUnitFileState=%s\n", active, enabled))
}

func commandsText(commands []Command) string {
	var lines []string
	for _, c := range commands {
		lines = append(lines, c.Path+" "+strings.Join(c.Args, " "))
	}
	return strings.Join(lines, "\n")
}

func template(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, "profiles/web")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"dependencies": map[string]string{"dsh-browser-fs": "0.2.0"}, "dsh": map[string]any{"profile": map[string]any{"bundles": []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", "dsh-browser-fs"}}}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(p, "package.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "cordis.patch.yml"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// managerFixture builds a Manager whose tenant workers are stand-in processes.
//
// The real profile starts bwrap + node, which a unit test neither needs nor can
// rely on: what these tests pin is dshgw's own lifecycle behaviour (directories,
// registry, handshake, rollback, restart), and the runner is the seam that makes
// that observable without a host that can run sandboxes.
func managerFixture(t *testing.T) (*Manager, *WorkerRunner, string) {
	t.Helper()
	root := t.TempDir()
	tpl := filepath.Join(root, "template")
	template(t, tpl)
	// A complete synthetic dsh installation: the profile resolves the release
	// through current_link with EvalSymlinks, so the fixture must look like the
	// deployed layout instead of pointing at host paths.
	release := filepath.Join(root, "dsh", "releases", "r1")
	nodeBin := filepath.Join(root, "dsh", "node", "bin", "node")
	currentLink := filepath.Join(root, "dsh", "current")
	for _, dir := range []string{filepath.Join(release, "lib"), filepath.Dir(nodeBin)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(release, "lib", "bin.js"), []byte("// dsh launcher\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, currentLink); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		PublicHost: "dsh.test", PortalPort: 32600, TenantPortLo: 32601, TenantPortHi: 32605,
		WorkerPortLo: 32100, WorkerPortHi: 32105, Listen: "127.0.0.1:3099", AigwBaseURL: "http://aigw",
		SessionTTL: config.Duration(1), DirectoryPicker: "clamp", PluginBrowserFS: "on", WorkspaceSeed: []string{"work"}, ReservedNames: []string{"login"},
		Dsh:      config.DshRuntime{NodeBin: nodeBin, BinJS: filepath.Join(release, "lib", "bin.js"), CurrentLink: currentLink},
		StateDir: filepath.Join(root, "state"), TenantRoot: filepath.Join(root, "state/tenants"), WorkspaceRoot: filepath.Join(root, "srv"), HandshakeDir: filepath.Join(root, "handshake"),
		RegistryPath: filepath.Join(root, "registry.json"), KeyMapPath: filepath.Join(root, "keys.map"), SessionPath: filepath.Join(root, "sessions.json"),
		Deploy: config.DeployConfig{TemplateHome: tpl, PluginPath: "/opt/dshgw/share/dsh-plugin/picker-clamp.js", ConfigPath: filepath.Join(root, "etc/dshgw.yaml"),
			TenantConfigRoot: filepath.Join(root, "etc/tenants"), BackupDir: filepath.Join(root, "backups"),
			WorkerUser: "dshgw", BwrapBin: "/usr/bin/bwrap"},
	}
	for _, path := range []string{cfg.StateDir, filepath.Dir(cfg.Deploy.ConfigPath), cfg.HandshakeDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.Deploy.ConfigPath, []byte("test: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(root, "fake-worker.sh")
	script := "#!/bin/sh\nport=\"$1\"\necho \"dsh web: http://127.0.0.1:$port/?token=" + testToken + "\"\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.2; done\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &WorkerRunner{
		Config: cfg,
		Profile: func(tn registry.Tenant) ([]string, error) {
			return []string{worker, itoa(tn.WorkerPort)}, nil
		},
		StopTimeout: 5 * time.Second,
	}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	m := &Manager{
		Config: cfg, Registry: reg, Workers: runner,
		WorkerAccount: func(name string) error {
			if name == "" {
				return errors.New("worker_user must name the unprivileged account")
			}
			return nil
		},
		RuntimeCheck: sandbox.ValidateBindings,
		Probe:        func(context.Context, registry.Tenant) error { return nil },
	}
	return m, runner, root
}

// fixtureTenant creates the tenant's directories and returns the registry record
// the lifecycle would hold for it.
func fixtureTenant(t *testing.T, m *Manager, name string, port int) registry.Tenant {
	t.Helper()
	workspace := filepath.Join(m.Config.WorkspaceRoot, name)
	dshHome := filepath.Join(m.Config.TenantRoot, name, ".dsh")
	for _, dir := range []string{workspace, dshHome, filepath.Join(m.Config.Deploy.TenantConfigRoot, name)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return registry.Tenant{
		Name: name, WorkerPort: port, UID: os.Geteuid(), Isolation: registry.IsolationBwrap,
		Workspace: workspace, DshHome: dshHome, PublicPort: port - 100, KeyPrefix: "sk-aaaaaaaaa", CreatedAt: time.Now().UTC(),
		Handshake: registry.HandshakeOK,
	}
}

func TestCreateRejectsRetainedDestinationsBeforeMutation(t *testing.T) {
	for _, location := range []string{"state", "workspace", "config", "handshake"} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/symlink=%t", location, symlink), func(t *testing.T) {
				m, runner, _ := managerFixture(t)
				paths := map[string]string{"state": filepath.Join(m.Config.TenantRoot, "alice"), "workspace": filepath.Join(m.Config.WorkspaceRoot, "alice"), "config": filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice"), "handshake": filepath.Join(m.Config.HandshakeDir, "alice.url")}
				path := paths[location]
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if symlink {
					if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("retained"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", models("m"), CreateOptions{})
				if err == nil || !strings.Contains(err.Error(), "already exists") {
					t.Fatalf("expected preflight refusal, got %v", err)
				}
				if len(runner.Running()) != 0 {
					t.Fatalf("preflight started a worker process: %+v", runner.Running())
				}
				if len(m.Registry.List()) != 0 {
					t.Fatalf("preflight registered a tenant: %+v", m.Registry.List())
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("retained destination removed: %v", err)
				}
				if !symlink {
					if data, err := os.ReadFile(filepath.Join(path, "sentinel")); err != nil || string(data) != "retained" {
						t.Fatalf("retained data changed: %q / %v", data, err)
					}
				}
			})
		}
	}
}

func TestValidateTemplatePinsBrowserFS(t *testing.T) {
	root := t.TempDir()
	template(t, root)
	if err := ValidateTemplate(root, true); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "profiles/web/package.json")
	if err := os.WriteFile(p, []byte(`{"dependencies":{},"dsh":{"profile":{"bundles":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTemplate(root, true); err == nil {
		t.Fatal("missing plugin accepted")
	}
	if err := ValidateTemplate(root, false); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentManagersAllocateDistinctPorts(t *testing.T) {
	first, _, _ := managerFixture(t)
	secondRegistry := registry.New(first.Config.RegistryPath, first.Config.KeyMapPath)
	second := &Manager{Config: first.Config, Registry: secondRegistry, Workers: first.Workers, Probe: func(context.Context, registry.Tenant) error { return nil }}
	type result struct {
		tenant registry.Tenant
		err    error
	}
	results := make(chan result, 2)
	go func() {
		tenant, err := first.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", models("m"), CreateOptions{})
		results <- result{tenant, err}
	}()
	go func() {
		tenant, err := second.Create(context.Background(), "bob", "sk-bbbbbbbbb-rest", models("m"), CreateOptions{})
		results <- result{tenant, err}
	}()
	one, two := <-results, <-results
	if one.err != nil || two.err != nil {
		t.Fatalf("errors: %v / %v", one.err, two.err)
	}
	if one.tenant.PublicPort == two.tenant.PublicPort || one.tenant.WorkerPort == two.tenant.WorkerPort {
		t.Fatalf("colliding allocations: %#v %#v", one.tenant, two.tenant)
	}
	loaded, err := registry.Load(first.Config.RegistryPath, first.Config.KeyMapPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.List()) != 2 {
		t.Fatalf("registry tenants=%v", loaded.List())
	}
}

func TestBindPrefixRollsBackWhenDerivedRegistrySaveFails(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", models("model"), CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.Config.KeyMapPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.Config.KeyMapPath, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix := "sk-bbbbbbbbb"
	if err := m.BindPrefix(tenant.Name, prefix); err == nil {
		t.Fatal("derived registry save failure accepted")
	}
	if _, ok := m.Registry.ByPrefix(prefix); ok {
		t.Fatal("failed prefix remains in memory")
	}
	loaded, err := registry.Load(m.Config.RegistryPath, m.Config.KeyMapPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ByPrefix(prefix); ok {
		t.Fatal("failed prefix remains in canonical registry")
	}
}

func TestSyncModelsRollsBackSettingsAndRegistryWhenDerivedSaveFails(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", models("model"), CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(tenant.DshHome, "settings.yaml")
	oldSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.Config.KeyMapPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.Config.KeyMapPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncModels(tenant, nil); err == nil {
		t.Fatal("derived registry save failure accepted")
	}
	gotSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSettings) != string(oldSettings) {
		t.Fatalf("settings changed after failed sync: got %q want %q", gotSettings, oldSettings)
	}
	current, ok := m.Registry.Get(tenant.Name)
	if !ok {
		t.Fatal("tenant disappeared from in-memory registry")
	}
	if current.ModelsPending {
		t.Fatal("ModelsPending changed in memory after failed sync")
	}
	loaded, err := registry.Load(m.Config.RegistryPath, m.Config.KeyMapPath)
	if err != nil {
		t.Fatal(err)
	}
	current, ok = loaded.Get(tenant.Name)
	if !ok {
		t.Fatal("tenant disappeared from canonical registry")
	}
	if current.ModelsPending {
		t.Fatal("ModelsPending changed on disk after failed sync")
	}
}

func TestRotateAndSyncRejectRegistryPathsOutsideConfiguredRoots(t *testing.T) {
	for _, operation := range []string{"rotate", "sync"} {
		t.Run(operation, func(t *testing.T) {
			m, runner, _ := managerFixture(t)
			tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", models("model"), CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tenant.DshHome = filepath.Join(t.TempDir(), ".dsh")
			if err := m.Registry.Put(tenant); err != nil {
				t.Fatal(err)
			}
			if err := m.Registry.Save(); err != nil {
				t.Fatal(err)
			}
			beforePID := runner.Status(tenant).PID
			if operation == "rotate" {
				err = m.RotateKey(context.Background(), tenant, "sk-bbbbbbbbb-rest", models("model"), false)
			} else {
				err = m.SyncModels(tenant, models("model"))
			}
			if err == nil || !strings.Contains(err.Error(), "paths do not match configured tenant roots") {
				t.Fatalf("unsafe registry path accepted: %v", err)
			}
			// A refused lifecycle call must not touch the running worker: the PID
			// is the observable proof that nothing was restarted or stopped.
			if after := runner.Status(tenant).PID; after != beforePID {
				t.Fatalf("unsafe path changed the worker process: pid %d -> %d", beforePID, after)
			}
		})
	}
}

// A tenant can be in the registry without ever having been provisioned: written by
// hand, restored from a backup, or migrated from the previous deployment. Its dsh
// then has no settings.yaml, which the UI reports as "settings are unavailable in
// this browser" and an empty model list — so starting that worker has to create the
// files from the key the tenant has and the models that key can call.
func TestEnsureProvisionedCreatesMissingArtifactsAndIsIdempotent(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	if err := m.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(tenant.DshHome, "settings.yaml")

	provisioned, err := m.EnsureProvisioned(context.Background(), tenant, "sk-aaaaaaaaa-rest", models("m-a", "m-b"))
	if err != nil {
		t.Fatal(err)
	}
	if !provisioned {
		t.Fatal("an unprovisioned tenant was reported as already provisioned")
	}
	for _, name := range []string{"settings.yaml", ".credentials.yaml"} {
		if _, err := os.Stat(filepath.Join(tenant.DshHome, name)); err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
	}
	// The profile tree is what makes the rendered patch importable: without it dsh
	// exits with "plugin tree failed to load" on startup, which is a worse outcome
	// than the empty settings this provisioning set out to fix.
	profileManifest := filepath.Join(tenant.DshHome, "profiles", "web", "package.json")
	if _, err := os.Stat(profileManifest); err != nil {
		t.Fatalf("the template profile was not installed: %v", err)
	}
	gateway := filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice", "gateway.key")
	key, err := os.ReadFile(gateway)
	if err != nil {
		t.Fatalf("gateway key was not stored: %v", err)
	}
	if strings.TrimSpace(string(key)) != "sk-aaaaaaaaa-rest" {
		t.Fatalf("stored key = %q", strings.TrimSpace(string(key)))
	}
	// dsh reads its model list from settings.yaml; the models this key may use must be
	// in there, or the UI shows an empty catalog again.
	body, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"m-a", "m-b"} {
		if !strings.Contains(string(body), model) {
			t.Errorf("settings.yaml is missing model %q:\n%s", model, body)
		}
	}
	// The key's own prefix becomes authoritative, with the old one kept.
	updated, _ := m.Registry.Get("alice")
	if updated.KeyPrefix != "sk-aaaaaaaaa"[:12] && updated.KeyPrefix != "sk-aaaaaaaaa" {
		t.Fatalf("key prefix = %q", updated.KeyPrefix)
	}
	if updated.ModelsPending {
		t.Error("ModelsPending stayed true although models were configured")
	}

	// A second call must not touch a provisioned tenant: its model list belongs to
	// SyncModels from then on.
	marked := append(body, []byte("# keep\n")...)
	if err := os.WriteFile(settings, marked, 0o600); err != nil {
		t.Fatal(err)
	}
	provisioned, err = m.EnsureProvisioned(context.Background(), tenant, "sk-bbbbbbbbb-other", models("m-c"))
	if err != nil {
		t.Fatal(err)
	}
	if provisioned {
		t.Fatal("an already provisioned tenant was provisioned again")
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(marked) {
		t.Fatalf("settings.yaml was rewritten by EnsureProvisioned:\n%s", after)
	}
}

func TestEnsureProvisionedRequiresAKey(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	if err := m.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	// Without a key there is nothing to configure dsh with; refusing is what keeps
	// the caller from writing a provider block that can never authenticate.
	if _, err := m.EnsureProvisioned(context.Background(), tenant, "", models("m")); err == nil {
		t.Fatal("an empty key was accepted")
	}
}

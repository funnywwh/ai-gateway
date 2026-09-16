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

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
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
func managerFixture(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	tpl := filepath.Join(root, "template")
	template(t, tpl)
	cfg := &config.Config{
		PublicHost: "dsh.test", PortalPort: 32600, TenantPortLo: 32601, TenantPortHi: 32605,
		WorkerPortLo: 32100, WorkerPortHi: 32105, Listen: "127.0.0.1:3099", AigwBaseURL: "http://aigw",
		SessionTTL: config.Duration(1), DirectoryPicker: "clamp", PluginBrowserFS: "on", WorkspaceSeed: []string{"work"}, ReservedNames: []string{"login"},
		Dsh: config.DshRuntime{CurrentLink: "/opt/dsh/current"}, TLS: config.TLSConfig{Certificate: "/cert", CertificateKey: "/key"},
		StateDir: filepath.Join(root, "state"), TenantRoot: filepath.Join(root, "state/tenants"), WorkspaceRoot: filepath.Join(root, "srv"), HandshakeDir: filepath.Join(root, "handshake"),
		RegistryPath: filepath.Join(root, "registry.json"), KeyMapPath: filepath.Join(root, "keys.map"), SessionPath: filepath.Join(root, "sessions.json"),
		Deploy: config.DeployConfig{TemplateHome: tpl, PluginPath: "/plugin.js", ConfigPath: filepath.Join(root, "etc/dshgw.yaml"),
			TenantConfigRoot: filepath.Join(root, "etc/tenants"), BackupDir: filepath.Join(root, "backups"), GatewayGroup: "dshgw", GatewayUnit: "dshgw.service", DshUserPrefix: "dsh-",
			NginxDir: filepath.Join(root, "nginx"), NginxBinary: "nginx", PublicListen: "0.0.0.0"},
	}
	for _, path := range []string{cfg.StateDir, filepath.Dir(cfg.Deploy.ConfigPath)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.Deploy.ConfigPath, []byte("test: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	runner := &fakeRunner{}
	return &Manager{Config: cfg, Registry: reg, Runner: runner, Probe: func(context.Context, registry.Tenant) error { return nil }}, runner
}
func TestCreateRunsIsolationFlow(t *testing.T) {
	m, r := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tenant.UID != 1234 || tenant.PublicPort != 32601 || tenant.WorkerPort != 32100 {
		t.Fatalf("%#v", tenant)
	}
	if _, err := os.Stat(filepath.Join(tenant.DshHome, ".credentials.yaml")); err != nil {
		t.Fatal(err)
	}
	joined := commandsText(r.commands)
	for _, want := range []string{"useradd --system --user-group --no-create-home", "chown -R dsh-alice:dsh-alice", "systemctl start dsh-worker@alice.service", "systemctl enable dsh-worker@alice.service", "nginx -t", "systemctl reload nginx"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
}
func TestCreateRollsBackOnStartFailure(t *testing.T) {
	m, r := managerFixture(t)
	r.fail = "systemctl start"
	_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if _, ok := m.Registry.Get("alice"); ok {
		t.Fatal("registry not rolled back")
	}
	for _, path := range []string{filepath.Join(m.Config.TenantRoot, "alice"), filepath.Join(m.Config.WorkspaceRoot, "alice"), filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice")} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("tenant data remains at %s: %v", path, statErr)
		}
	}
	if strings.Contains(commandsText(r.commands), "userdel --remove") {
		t.Fatal("rollback delegates destructive home deletion to userdel")
	}
}

func TestCreateRejectsRetainedDestinationsBeforeMutation(t *testing.T) {
	for _, location := range []string{"state", "workspace", "config", "handshake"} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/symlink=%t", location, symlink), func(t *testing.T) {
				m, r := managerFixture(t)
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
				_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
				if err == nil || !strings.Contains(err.Error(), "already exists") {
					t.Fatalf("expected preflight refusal, got %v", err)
				}
				if len(r.commands) != 0 {
					t.Fatalf("preflight ran commands: %s", commandsText(r.commands))
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

func TestCreateUseraddFailureRemovesOnlyClaimedPaths(t *testing.T) {
	m, r := managerFixture(t)
	r.fail = "useradd"
	if _, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{}); err == nil {
		t.Fatal("expected useradd failure")
	}
	if strings.Contains(commandsText(r.commands), "userdel") {
		t.Fatal("attempted to delete an account not created by this transaction")
	}
	for _, parent := range []string{m.Config.TenantRoot, m.Config.WorkspaceRoot, m.Config.Deploy.TenantConfigRoot} {
		if _, err := os.Lstat(filepath.Join(parent, "alice")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("claimed directory remains: %s / %v", parent, err)
		}
	}
}

func TestCreateRetainsDataWhenRollbackCannotStopOrDeleteIdentity(t *testing.T) {
	for _, failure := range []string{"systemctl disable", "userdel"} {
		t.Run(failure, func(t *testing.T) {
			m, r := managerFixture(t)
			r.fail = failure
			primary := errors.New("candidate probe failed")
			m.Probe = func(context.Context, registry.Tenant) error { return primary }
			tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
			if !errors.Is(err, primary) || !strings.Contains(err.Error(), "data retained") {
				t.Fatalf("rollback failure hidden: %v", err)
			}
			if _, err := os.Stat(filepath.Join(tenant.DshHome, ".credentials.yaml")); err != nil {
				t.Fatalf("live identity data was destroyed: %v", err)
			}
			if failure == "systemctl disable" && strings.Contains(commandsText(r.commands), "userdel") {
				t.Fatal("deleted identity without stopping worker")
			}
		})
	}
}

func TestStatusFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		runErr       error
		wantErr      bool
		active       bool
		enabled      bool
	}{
		{name: "inactive", output: string(systemdStatus("inactive", "disabled"))},
		{name: "active runtime", output: string(systemdStatus("active", "enabled-runtime")), active: true, enabled: true},
		{name: "failed static", output: string(systemdStatus("failed", "static"))},
		{name: "bus error", output: string(systemdStatus("inactive", "disabled")), runErr: errors.New("bus unavailable"), wantErr: true},
		{name: "missing output", wantErr: true},
		{name: "missing unit", output: "LoadState=not-found\nActiveState=inactive\nUnitFileState=disabled\n", wantErr: true},
		{name: "transition", output: string(systemdStatus("deactivating", "enabled")), wantErr: true},
		{name: "unknown enablement", output: string(systemdStatus("active", "future-state")), wantErr: true},
		{name: "duplicate", output: string(systemdStatus("inactive", "disabled")) + "ActiveState=active\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, r := managerFixture(t)
			r.hook = func(context.Context, Command) ([]byte, error, bool) { return []byte(tc.output), tc.runErr, true }
			status, err := m.Status(context.Background(), registry.Tenant{Name: "alice"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("status=%#v err=%v", status, err)
			}
			if !tc.wantErr && (status.Active != tc.active || status.Enabled != tc.enabled) {
				t.Fatalf("status=%#v", status)
			}
		})
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

func TestNginxFailureRestoresFragmentsAndInclude(t *testing.T) {
	m, runner := managerFixture(t)
	includePath := filepath.Join(filepath.Dir(m.Config.Deploy.NginxDir), "dshgw.conf")
	if err := os.MkdirAll(m.Config.Deploy.NginxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFragment := filepath.Join(m.Config.Deploy.NginxDir, "old.conf")
	if err := os.WriteFile(oldFragment, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(includePath, []byte("old include\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.fail = "nginx -t"
	if err := m.ReconcileNginx(context.Background(), true); err == nil {
		t.Fatal("nginx validation failure accepted")
	}
	fragment, _ := os.ReadFile(oldFragment)
	include, _ := os.ReadFile(includePath)
	if string(fragment) != "old\n" || string(include) != "old include\n" {
		t.Fatalf("fragment=%q include=%q", fragment, include)
	}
	if _, err := os.Stat(filepath.Join(m.Config.Deploy.NginxDir, "01-portal.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated fragment remains: %v", err)
	}
}

func TestNginxReloadRestorationFailureIsReported(t *testing.T) {
	m, r := managerFixture(t)
	firstErr, restoreErr := errors.New("initial reload failed"), errors.New("restoration reload failed")
	reloads := 0
	r.hook = func(_ context.Context, c Command) ([]byte, error, bool) {
		if c.Path == "systemctl" && strings.Join(c.Args, " ") == "reload nginx" {
			reloads++
			if reloads == 1 {
				return nil, firstErr, true
			}
			return nil, restoreErr, true
		}
		return nil, nil, false
	}
	err := m.ReconcileNginx(context.Background(), true)
	if !errors.Is(err, firstErr) || !errors.Is(err, restoreErr) || reloads != 2 {
		t.Fatalf("reloads=%d err=%v", reloads, err)
	}
}

func TestConcurrentManagersAllocateDistinctPorts(t *testing.T) {
	first, _ := managerFixture(t)
	secondRegistry := registry.New(first.Config.RegistryPath, first.Config.KeyMapPath)
	second := &Manager{Config: first.Config, Registry: secondRegistry, Runner: &fakeRunner{}, Probe: func(context.Context, registry.Tenant) error { return nil }}
	type result struct {
		tenant registry.Tenant
		err    error
	}
	results := make(chan result, 2)
	go func() {
		tenant, err := first.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
		results <- result{tenant, err}
	}()
	go func() {
		tenant, err := second.Create(context.Background(), "bob", "sk-bbbbbbbbb-rest", []string{"m"}, CreateOptions{})
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

func TestRotateKeyHoldsWriterLocksThroughRollback(t *testing.T) {
	m, r := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"old"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(tenant.DshHome, ".credentials.yaml"), filepath.Join(tenant.DshHome, "settings.yaml"), filepath.Join(m.Config.Deploy.TenantConfigRoot, tenant.Name, "gateway.key")}
	old := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		old[path] = string(data)
	}
	primary := errors.New("gateway chown failed")
	checked := false
	r.hook = func(_ context.Context, c Command) ([]byte, error, bool) {
		if c.Path == "chown" && len(c.Args) == 2 && c.Args[1] == paths[2] {
			for _, path := range paths[:2] {
				if _, err := os.Stat(path + ".lock"); err != nil {
					t.Errorf("writer lock not held during transaction: %s: %v", path, err)
				}
			}
			checked = true
			return nil, primary, true
		}
		return nil, nil, false
	}
	err = m.RotateKey(context.Background(), tenant, "sk-bbbbbbbbb-rest", []string{"new"}, false)
	if !errors.Is(err, primary) || !checked {
		t.Fatalf("checked=%t err=%v", checked, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != old[path] {
			t.Fatalf("rollback failed for %s: %q / %v", path, data, err)
		}
	}
	for _, path := range paths[:2] {
		if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("writer lock leaked: %s: %v", path, err)
		}
	}
	current, _ := m.Registry.Get(tenant.Name)
	if current.KeyPrefix != tenant.KeyPrefix {
		t.Fatal("key prefix changed on failed rotation")
	}
}

func TestRotateKeyPropagatesRestorationFailure(t *testing.T) {
	m, r := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"old"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(tenant.DshHome, ".credentials.yaml")
	primary := errors.New("gateway ownership failed")
	r.hook = func(_ context.Context, c Command) ([]byte, error, bool) {
		if c.Path == "chown" && len(c.Args) == 2 {
			if err := os.Remove(credentials); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(credentials, 0o700); err != nil {
				t.Fatal(err)
			}
			return nil, primary, true
		}
		return nil, nil, false
	}
	err = m.RotateKey(context.Background(), tenant, "sk-bbbbbbbbb-rest", []string{"new"}, false)
	if !errors.Is(err, primary) || !strings.Contains(err.Error(), "rollback credentials") {
		t.Fatalf("rollback failure hidden: %v", err)
	}
}

func TestCreateDirectoryModes(t *testing.T) {
	m, _ := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{path: filepath.Dir(tenant.DshHome), mode: 0o700},
		{path: tenant.Workspace, mode: 0o700},
		{path: filepath.Join(m.Config.Deploy.TenantConfigRoot, tenant.Name), mode: 0o750},
	} {
		info, statErr := os.Stat(tc.path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", tc.path, statErr)
		}
		if got := info.Mode().Perm(); got != tc.mode {
			t.Errorf("mode %s = %04o, want %04o", tc.path, got, tc.mode)
		}
	}
}

func TestManagerUsesConfiguredWorkerUnit(t *testing.T) {
	m, r := managerFixture(t)
	m.Config.Deploy.WorkerUnit = "custom-worker@.service"
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable(context.Background(), tenant, true); err != nil {
		t.Fatal(err)
	}
	joined := commandsText(r.commands)
	for _, want := range []string{
		"systemctl start custom-worker@alice.service",
		"systemctl enable custom-worker@alice.service",
		"systemctl show --property=LoadState --property=ActiveState --property=UnitFileState custom-worker@alice.service",
		"systemctl restart custom-worker@alice.service",
		"systemctl enable custom-worker@alice.service",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dsh-worker@alice.service") {
		t.Errorf("default worker unit used in\n%s", joined)
	}
}

func TestBindPrefixRollsBackWhenDerivedRegistrySaveFails(t *testing.T) {
	m, _ := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
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
	m, _ := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
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
			m, r := managerFixture(t)
			tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
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
			r.commands = nil
			if operation == "rotate" {
				err = m.RotateKey(context.Background(), tenant, "sk-bbbbbbbbb-rest", []string{"model"}, false)
			} else {
				err = m.SyncModels(tenant, []string{"model"})
			}
			if err == nil || !strings.Contains(err.Error(), "paths do not match configured tenant roots") {
				t.Fatalf("unsafe registry path accepted: %v", err)
			}
			if len(r.commands) != 0 {
				t.Fatalf("unsafe path reached lifecycle commands: %s", commandsText(r.commands))
			}
		})
	}
}

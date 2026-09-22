package tenancy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// repoPluginDir is where this repository ships the tenant plugins, which is the shape a
// deployment has: the packages sit beside deploy.plugin_path's picker-clamp.js.
func repoPluginDir() string {
	return filepath.Join("..", "..", "..", "cmd", "dshgw", "plugin")
}

// pluginFixture is renderFixture with the three tenant plugins switched on and pointing at the
// real packages in this checkout — a made-up path could not tell a deployed plugin from an
// undeployed one, and that distinction is half of what this file pins.
func pluginFixture(t *testing.T) (*config.Config, registry.Tenant) {
	t.Helper()
	cfg, tenant := renderFixture(t)
	cfg.Deploy.PluginPath = filepath.Join(repoPluginDir(), "picker-clamp.js")
	cfg.TenantPlugins = config.TenantPlugins{
		WebTTY:         config.PluginSwitch{Enabled: true},
		WorkspaceFiles: config.PluginSwitch{Enabled: true},
		GitDiff:        config.PluginSwitch{Enabled: true},
	}
	return cfg, tenant
}

// M75: one installed copy serves every account, so the row's `name` must name the package's host
// half and the browser half must be discoverable the way dsh discovers client modules — the same
// axis account_card_test.go pins for M67, now for three packages at once.
func TestTenantPluginPackagesShipBothHalves(t *testing.T) {
	cfg, _ := pluginFixture(t)
	for _, plugin := range tenantPlugins(cfg) {
		row := plugin.row(cfg, registry.Tenant{Name: "alice", DshHome: "/state/alice/.dsh", Workspace: "/srv/alice"})
		name, _ := row["name"].(string)
		if !strings.HasSuffix(name, "/"+plugin.dir+"/index.js") {
			t.Fatalf("%s: row name = %q, want the package's host half (index.js)", plugin.dir, name)
		}
		if strings.Contains(name, "client.js") {
			t.Fatalf("%s: row name = %q: a row must never name the browser bundle, which throws on import", plugin.dir, name)
		}
		dir := filepath.Dir(strings.TrimPrefix(name, "file://"))

		manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			t.Fatalf("%s: read the package manifest: %v", plugin.dir, err)
		}
		var pkg struct {
			Name    string            `json:"name"`
			Exports map[string]string `json:"exports"`
			DSH     struct {
				Client struct {
					Platform string `json:"platform"`
				} `json:"client"`
			} `json:"dsh"`
		}
		if err := json.Unmarshal(manifest, &pkg); err != nil {
			t.Fatalf("%s: parse the package manifest: %v", plugin.dir, err)
		}
		if pkg.Exports["./client"] != "./client.js" || pkg.DSH.Client.Platform != "web" {
			t.Fatalf("%s: exports[./client]=%q platform=%q; dsh discovers the browser half through both", plugin.dir, pkg.Exports["./client"], pkg.DSH.Client.Platform)
		}
		host, err := os.ReadFile(filepath.Join(dir, "index.js"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(host), "export const name") {
			t.Fatalf("%s: the host half must export a plugin name", plugin.dir)
		}
		if strings.Contains(string(host), "__ModuleLoader__.load(") {
			t.Fatalf("%s: the host half must not be the browser bundle", plugin.dir)
		}
	}
}

// The three rows and what each of them carries: the account's own workspace as the clamp root, a
// label that is not the account's directory name, and per-account runtime files inside that
// account's DSH home rather than inside the shared plugin directory.
func TestTenantPluginRowsAreClampedPerAccount(t *testing.T) {
	cfg, tenant := pluginFixture(t)
	rows, err := tenantPluginRows(cfg, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	wantIDs := []string{webTTYRowID, workspaceFilesRowID, gitDiffRowID}
	for i, want := range wantIDs {
		if rows[i]["id"] != want {
			t.Errorf("row %d id = %v, want %s", i, rows[i]["id"], want)
		}
	}

	stateDir := filepath.Join(tenant.DshHome, "plugin-state")
	webTTY := rows[0]["config"].(map[string]any)
	if webTTY["cwd"] != tenant.Workspace || webTTY["cwdRoot"] != tenant.Workspace {
		t.Errorf("web-tty cwd = %v, cwdRoot = %v; both must be the account's workspace %s", webTTY["cwd"], webTTY["cwdRoot"], tenant.Workspace)
	}
	if webTTY["traceFile"] != filepath.Join(stateDir, "web-tty.trace.jsonl") {
		t.Errorf("web-tty traceFile = %v", webTTY["traceFile"])
	}
	for i, want := range []string{"workspace-files", "git-diff"} {
		cfgMap := rows[i+1]["config"].(map[string]any)
		if cfgMap["root"] != tenant.Workspace {
			t.Errorf("%s root = %v, want %s", want, cfgMap["root"], tenant.Workspace)
		}
		if cfgMap["rootLabel"] != config.DefaultPluginRootLabel {
			t.Errorf("%s rootLabel = %v, want %q", want, cfgMap["rootLabel"], config.DefaultPluginRootLabel)
		}
		if cfgMap["traceFile"] != filepath.Join(stateDir, want+".trace.jsonl") {
			t.Errorf("%s traceFile = %v", want, cfgMap["traceFile"])
		}
	}
	gitDiff := rows[2]["config"].(map[string]any)
	if gitDiff["cacheFile"] != filepath.Join(stateDir, "git-diff.cache.json") {
		t.Errorf("git-diff cacheFile = %v", gitDiff["cacheFile"])
	}
	// Nothing may be written into the directory the packages live in: it is shared by every
	// account, so a state file there would mix accounts (and in a root-owned deployment it is
	// not writable at all).
	pluginDir := filepath.Dir(cfg.Deploy.PluginPath)
	for i, row := range rows {
		for key, value := range row["config"].(map[string]any) {
			if path, ok := value.(string); ok && strings.HasPrefix(path, pluginDir) {
				t.Errorf("row %d %s = %q: state must not live in the shared plugin directory", i, key, path)
			}
		}
	}

	// Two accounts must not share a state file.
	other := tenant
	other.Name = "bob"
	other.DshHome = filepath.Join(filepath.Dir(tenant.DshHome), "bob", ".dsh")
	otherRows, err := tenantPluginRows(cfg, other)
	if err != nil {
		t.Fatal(err)
	}
	if otherRows[0]["config"].(map[string]any)["traceFile"] == webTTY["traceFile"] {
		t.Error("two accounts got the same trace file")
	}

	// The label is the operator's when they set one.
	cfg.TenantPlugins.RootLabel = "我的工作区"
	relabelled, err := tenantPluginRows(cfg, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got := relabelled[1]["config"].(map[string]any)["rootLabel"]; got != "我的工作区" {
		t.Errorf("rootLabel = %v, want the configured label", got)
	}
}

// Switching one plugin off removes that row and leaves the other two alone, both in a freshly
// rendered patch and on a tenant that already exists (the every-start refresh).
func TestEnsureTenantPluginsFollowsTheSwitches(t *testing.T) {
	cfg, tenant := pluginFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", models("m"), TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifacts(arts); err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")
	rendered, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{webTTYRowID, workspaceFilesRowID, gitDiffRowID, "root: " + tenant.Workspace, "rootLabel: " + config.DefaultPluginRootLabel} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("rendered patch missing %q:\n%s", want, rendered)
		}
	}

	cfg.TenantPlugins.GitDiff.Enabled = false
	if warnings, err := EnsureTenantPlugins(cfg, tenant); err != nil {
		t.Fatalf("refresh with git_diff off: %v", err)
	} else if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	data, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), gitDiffRowID) {
		t.Fatalf("a disabled plugin kept its row:\n%s", data)
	}
	if !strings.Contains(string(data), webTTYRowID) || !strings.Contains(string(data), workspaceFilesRowID) {
		t.Fatalf("disabling one plugin removed another:\n%s", data)
	}

	// Idempotent: the refresh runs on every worker start, and a second run must not duplicate rows.
	if _, err := EnsureTenantPlugins(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{webTTYRowID, workspaceFilesRowID} {
		// yaml.Marshal writes a row's keys in alphabetical order, so a row that carries a config
		// starts with `- config:` and `id:` follows — count the key, not a list-item prefix.
		if count := strings.Count(string(again), "id: "+id); count != 1 {
			t.Fatalf("%s appears %d times:\n%s", id, count, again)
		}
	}

	cfg.TenantPlugins = config.TenantPlugins{RootLabel: config.DefaultPluginRootLabel}
	if _, err := EnsureTenantPlugins(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	if off, _ := os.ReadFile(patchPath); strings.Contains(string(off), "dshgw-") {
		t.Fatalf("rows survived every switch being off:\n%s", off)
	}
}

// An enabled but undeployed plugin must never render a row: a row whose module cannot be imported
// costs the account its whole plugin tree (measured on a real tenant, M67), so create time fails
// loudly and a worker start drops just that row with a warning.
func TestTenantPluginsThatAreNotDeployed(t *testing.T) {
	cfg, tenant := pluginFixture(t)
	// A deployment whose plugin directory exists but does not carry web-tty: the other two are
	// deployed, so this is the "half-deployed host" case, not a typo in plugin_path.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "picker-clamp.js"), []byte("// picker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"workspace-files", "git-diff"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "index.js"), []byte("export const name = 'x'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Deploy.PluginPath = filepath.Join(dir, "picker-clamp.js")

	rows, err := tenantPluginRows(cfg, tenant)
	if err == nil {
		t.Fatalf("an undeployed plugin was rendered: %v", rows)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "web-tty", "index.js")) {
		t.Fatalf("the error must name the missing path, got: %v", err)
	}
	if _, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", models("m"), TenantOptions{}, time.Now()); err == nil {
		t.Fatal("creating a tenant must fail while an enabled plugin is not deployed")
	}

	// The start-time refresh is the tolerant half. A patch that carries the row from a previous
	// deployment (here: rendered while the plugin was still deployed) must lose exactly that row.
	cfg.TenantPlugins.WebTTY.Enabled = false
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", models("m"), TenantOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifacts(arts); err != nil {
		t.Fatal(err)
	}
	cfg.TenantPlugins.WebTTY.Enabled = true
	warnings, err := EnsureTenantPlugins(cfg, tenant)
	if err != nil {
		t.Fatalf("a start-time refresh must not fail on an undeployed plugin: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "web-tty") {
		t.Fatalf("warnings = %v, want one naming web-tty", warnings)
	}
	patch, err := os.ReadFile(filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patch), webTTYRowID) {
		t.Fatalf("the undeployed plugin kept a row:\n%s", patch)
	}
	if !strings.Contains(string(patch), workspaceFilesRowID) {
		t.Fatalf("the deployed plugins lost their rows:\n%s", patch)
	}
}

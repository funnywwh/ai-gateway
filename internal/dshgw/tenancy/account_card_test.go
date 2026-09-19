package tenancy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M67: the axis that bit once and must not bite again. A loader row's `name` is a module the
// HOST imports, and the browser bundle runs `window.__ModuleLoader__.load` when it is
// imported — so naming client.js in the row crashes the tenant's whole plugin tree
// ("window is not defined", seen on a real tenant before this file existed). The row must
// name the host half, and the browser half must be reachable the way dsh discovers client
// modules: the package's `dsh.client` declaration plus `exports["./client"]`.
func TestAccountCardRowNamesTheHostHalfAndShipsAClientBundle(t *testing.T) {
	cfg, _ := renderFixture(t)
	// The row derives its path from deploy.plugin_path, so the fixture points at the real
	// plugin directory in this repository: the shape being pinned (the package beside the
	// picker) is what a deployment actually ships, and a made-up path could not be checked.
	pluginDir := filepath.Join("..", "..", "..", "cmd", "dshgw", "plugin")
	cfg.Deploy.PluginPath = filepath.Join(pluginDir, "picker-clamp.js")
	cfg.AccountCard.Enabled = true
	row := accountCardRow(cfg)

	name, _ := row["name"].(string)
	if !strings.HasSuffix(name, "/account-card/index.js") {
		t.Fatalf("row name = %q, want the package's host half (index.js)", name)
	}
	if strings.Contains(name, "client.js") {
		t.Fatalf("row name = %q: a row must never point at the browser bundle, which throws on import", name)
	}
	dir := filepath.Dir(strings.TrimPrefix(name, "file://"))

	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read the package manifest: %v", err)
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
		t.Fatalf("parse the package manifest: %v", err)
	}
	if pkg.Name != "dshgw-account-card" {
		t.Fatalf("package name = %q; it must equal the module id the bundle registers", pkg.Name)
	}
	if pkg.Exports["./client"] != "./client.js" {
		t.Fatalf("exports[\"./client\"] = %q; dsh discovers the browser half through it", pkg.Exports["./client"])
	}
	if pkg.DSH.Client.Platform != "web" {
		t.Fatalf("dsh.client.platform = %q, want \"web\"", pkg.DSH.Client.Platform)
	}
	for _, file := range []string{"index.js", "client.js"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	// The host half must be a real module: `export const name` is the one thing the loader
	// requires of it, and a bundle copy would carry the browser marker instead.
	host, err := os.ReadFile(filepath.Join(dir, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(host), "export const name") {
		t.Fatal("the host half must export a plugin name")
	}
	if strings.Contains(string(host), "__ModuleLoader__.load(") {
		t.Fatal("the host half must not be the browser bundle")
	}
}

// The same shape applies to the patch a tenant gets: switching account_card on adds exactly
// one row, and switching it off removes it again — the ssh and browser rows keep their own
// behaviour while this one comes and goes.
func TestEnsureAccountCardRowFollowsTheSwitch(t *testing.T) {
	cfg, tenant := renderFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", []string{"m"}, TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifacts(arts); err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")

	if _, err := EnsureAccountCardRow(cfg, tenant); err != nil {
		t.Fatalf("refresh with the feature off: %v", err)
	}
	if data, _ := os.ReadFile(patchPath); strings.Contains(string(data), accountCardRowID) {
		t.Fatalf("a disabled feature added a row:\n%s", data)
	}

	cfg.AccountCard.Enabled = true
	if _, err := EnsureAccountCardRow(cfg, tenant); err != nil {
		t.Fatalf("refresh with the feature on: %v", err)
	}
	data, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "account-card/index.js") {
		t.Fatalf("the enabled row is missing:\n%s", data)
	}
	// The row must land in the same insert list the other plugins use, not as a new top-level
	// entry: a second root entry would load the plugin twice.
	if strings.Count(string(data), "- id: "+accountCardRowID) != 1 {
		t.Fatalf("expected exactly one account-card row:\n%s", data)
	}

	cfg.AccountCard.Enabled = false
	if _, err := EnsureAccountCardRow(cfg, tenant); err != nil {
		t.Fatalf("refresh with the feature off again: %v", err)
	}
	if after, _ := os.ReadFile(patchPath); strings.Contains(string(after), accountCardRowID) {
		t.Fatalf("the row survived the switch being turned off:\n%s", after)
	}
}

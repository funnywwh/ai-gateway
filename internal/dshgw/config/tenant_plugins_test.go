package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// tenantPluginsOffDoc keeps a fixture free of plugin files: the tenant-side plugins are on by
// default and their rows name files beside deploy.plugin_path (M75), so a fixture that has no
// plugin directory has to switch them off explicitly — and a fixture that wants to test a
// *different* missing-plugin error must too, or that error would be masked by this one.
const tenantPluginsOffDoc = "tenant_plugins:\n  web_tty:\n    enabled: false\n  workspace_files:\n    enabled: false\n  git_diff:\n    enabled: false\n"

// M75: the three tenant-side web plugins are on unless the operator says otherwise. "On" is the
// feature — an account's dsh has the terminal, the file manager and the change review without
// anybody editing that account's profile — so a configuration that stays silent must get them,
// and the child's generated configuration must be able to turn one off for real.
func TestTenantPluginsDefaultOn(t *testing.T) {
	root := t.TempDir()
	p := writeConfig(t, "public_host: dsh.example.test\nstate_dir: "+filepath.Join(root, "state")+"\n"+
		"deploy:\n  plugin_path: "+filepath.Join(root, "picker-clamp.js")+"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TenantPlugins.AnyEnabled() || !cfg.TenantPlugins.WebTTY.Enabled || !cfg.TenantPlugins.WorkspaceFiles.Enabled || !cfg.TenantPlugins.GitDiff.Enabled {
		t.Fatalf("tenant_plugins defaults are not all on: %#v", cfg.TenantPlugins)
	}
	if cfg.TenantPlugins.RootLabel != DefaultPluginRootLabel {
		t.Fatalf("root_label default = %q, want %q", cfg.TenantPlugins.RootLabel, DefaultPluginRootLabel)
	}
}

func TestTenantPluginsCanBeNarrowed(t *testing.T) {
	root := t.TempDir()
	p := writeConfig(t, "public_host: dsh.example.test\nstate_dir: "+filepath.Join(root, "state")+"\n"+
		"deploy:\n  plugin_path: "+filepath.Join(root, "picker-clamp.js")+"\n"+
		"tenant_plugins:\n  web_tty:\n    enabled: false\n  git_diff:\n    enabled: false\n  root_label: 我的工作区\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TenantPlugins.WebTTY.Enabled || cfg.TenantPlugins.GitDiff.Enabled {
		t.Fatalf("an explicit false was ignored: %#v", cfg.TenantPlugins)
	}
	if !cfg.TenantPlugins.WorkspaceFiles.Enabled {
		t.Fatalf("a switch nobody mentioned must stay on: %#v", cfg.TenantPlugins)
	}
	if cfg.TenantPlugins.RootLabel != "我的工作区" {
		t.Fatalf("root_label = %q", cfg.TenantPlugins.RootLabel)
	}
	if !cfg.TenantPlugins.AnyEnabled() {
		t.Fatal("one plugin is still on")
	}
}

// The rows are rendered from files deployed beside deploy.plugin_path, so an enabled plugin without
// that path is a configuration error naming itself — not a file:// URL pointing nowhere.
func TestTenantPluginsRequirePluginPath(t *testing.T) {
	root := t.TempDir()
	// directory_picker: browse keeps the picker's own requirement out of the way, so the error
	// this test reads is the tenant_plugins one.
	p := writeConfig(t, "public_host: dsh.example.test\ndirectory_picker: browse\nstate_dir: "+filepath.Join(root, "state")+"\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("tenant_plugins with no deploy.plugin_path was accepted")
	}
	if !strings.Contains(err.Error(), "tenant_plugins") {
		t.Fatalf("error = %v, want it to name tenant_plugins", err)
	}

	// With every plugin switched off the requirement goes away: a deployment that wants neither
	// the picker nor the panels may leave plugin_path unset.
	p = writeConfig(t, "public_host: dsh.example.test\ndirectory_picker: browse\nstate_dir: "+filepath.Join(root, "state")+"\n"+
		"tenant_plugins:\n  web_tty:\n    enabled: false\n  workspace_files:\n    enabled: false\n  git_diff:\n    enabled: false\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TenantPlugins.AnyEnabled() {
		t.Fatalf("expected every plugin off: %#v", cfg.TenantPlugins)
	}
}

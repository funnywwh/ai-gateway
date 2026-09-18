package config

import (
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/hook"
	"github.com/winger/ai-gateway/internal/pluginhost"
)

// M63: the deployment has exactly one runtime data root, and every stateful default is
// inside it. This test is the executable form of docs/deployment-layout.md §4: a default
// that names /var/lib, /opt, /etc or a home directory is a second data root, and the
// whole point of the milestone was to delete those.
func TestDefaultDataRootCoversEveryStatefulPath(t *testing.T) {
	cfg := Default()
	stateful := map[string]string{
		"database.path":         cfg.Database.Path,
		"plugins.state_dir":     cfg.Plugins.StateDir,
		"billing.fallback_file": cfg.Billing.FallbackFile,
		"hooks.dead_letter":     cfg.Hooks.DeadLetter,
		"backup.dir":            cfg.Backup.Dir,
	}
	for key, path := range stateful {
		if !strings.HasPrefix(path, DefaultDataDir+"/") {
			t.Errorf("%s = %q, want it under %s", key, path, DefaultDataDir)
		}
	}
	// Artifacts are deliberately NOT data: the plugin binaries a deployment copies in and
	// the built binary directory are not runtime state, so they stay outside the data root.
	if cfg.Plugins.Dir != "./plugins" {
		t.Errorf("plugins.dir = %q, want the artifact directory ./plugins", cfg.Plugins.Dir)
	}
	if strings.HasPrefix(cfg.Plugins.Dir, DefaultDataDir) {
		t.Errorf("plugins.dir = %q must not live inside the data root", cfg.Plugins.Dir)
	}
}

// Two packages keep their own copy of the data root's spelling because the layering table
// forbids them from importing internal/config (hook is a leaf; pluginhost sits above
// config). Copies drift, so this test pins them to the same value: a hook dead-letter file
// or a plugin state directory outside the data root is the scattered state M63 removed.
func TestPackageDefaultsAgreeOnTheDataRoot(t *testing.T) {
	cfg := Default()
	if got := hook.DefaultConfig().DeadLetter; got != cfg.Hooks.DeadLetter {
		t.Errorf("hook.DefaultConfig().DeadLetter = %q, want the configured default %q", got, cfg.Hooks.DeadLetter)
	}
	if got := pluginhost.DefaultConfig().StateDir; got != cfg.Plugins.StateDir {
		t.Errorf("pluginhost.DefaultConfig().StateDir = %q, want the configured default %q", got, cfg.Plugins.StateDir)
	}
}

// The data root is spelled as a relative path on purpose, and the leading "./" is part of
// the contract the documentation and the startup log read.
func TestDefaultDataDirIsRelative(t *testing.T) {
	if DefaultDataDir != "./data" {
		t.Fatalf("DefaultDataDir = %q, want ./data", DefaultDataDir)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M63: the standalone shape has one data root, and every default hangs off it. The
// defaults used to be /var/lib/dshgw, /srv/dsh, /opt/dshgw/... and
// /home/winger/backups/dshgw — machine paths from the deleted root install.
func TestDefaultsLiveUnderTheDataRoot(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "data", "dshgw")
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// directory_picker: browse keeps this fixture independent of the picker plugin, which
	// has no default (see TestClampRequiresPluginPath).
	if _, err := f.WriteString("directory_picker: browse\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	derived := map[string]string{
		"state_dir":                 cfg.StateDir,
		"tenant_root":               cfg.TenantRoot,
		"workspace_root":            cfg.WorkspaceRoot,
		"handshake_dir":             cfg.HandshakeDir,
		"admin_socket":              cfg.AdminSocket,
		"registry_path":             cfg.RegistryPath,
		"key_map_path":              cfg.KeyMapPath,
		"session_path":              cfg.SessionPath,
		"audit_path":                cfg.AuditPath,
		"activity_path":             cfg.ActivityPath,
		"deploy.template_home":      cfg.Deploy.TemplateHome,
		"deploy.backup_dir":         cfg.Deploy.BackupDir,
		"deploy.tenant_config_root": cfg.Deploy.TenantConfigRoot,
	}
	for label, path := range derived {
		if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Errorf("%s = %q, want it inside %q", label, path, root)
		}
	}
	// gateway.key must not land inside the tenant root: that tree is what the worker binds
	// into the sandbox, and the key is deliberately not visible there.
	if cfg.Deploy.TenantConfigRoot == cfg.TenantRoot {
		t.Errorf("deploy.tenant_config_root = %q must not be the tenant root", cfg.Deploy.TenantConfigRoot)
	}
	if cfg.Deploy.ConfigPath != filepath.Join(wd, "dshgw.yaml") {
		t.Errorf("deploy.config_path = %q, want the deployment root's ./dshgw.yaml", cfg.Deploy.ConfigPath)
	}
}

// A relative path in the file is resolved against the process working directory, which is
// the deployment root in every documented entry point. Before M63 a relative state_dir was
// rejected outright, so ./data/dshgw — the layout the documentation describes — could not
// be written down at all.
func TestRelativePathsResolveAgainstTheWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "directory_picker: browse\nstate_dir: ./data/dshgw\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(wd, "data", "dshgw"); cfg.StateDir != want {
		t.Fatalf("state_dir = %q, want %q", cfg.StateDir, want)
	}
	if want := filepath.Join(wd, "data", "dshgw", "registry.json"); cfg.RegistryPath != want {
		t.Fatalf("registry_path = %q, want %q", cfg.RegistryPath, want)
	}
	// Resolution must not become a way past the cleanliness checks.
	if _, err := Load(writeConfig(t, "directory_picker: browse\nstate_dir: ./data/../dshgw\n")); err == nil {
		t.Fatal("a relative path containing .. was cleaned instead of rejected")
	}
}

// The dsh runtime is an installation, not data: the environment names it when the file does
// not, and both spellings must end up absolute for the sandbox profile to bind them.
func TestRuntimePathsFallBackToTheEnvironment(t *testing.T) {
	release := t.TempDir()
	t.Setenv("DSHGW_NODE", release+"/node/bin/node")
	t.Setenv("DSHGW_DSH_ROOT", release)

	cfg, err := Load(writeConfig(t, "directory_picker: browse\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dsh.NodeBin != release+"/node/bin/node" || cfg.Dsh.CurrentLink != release {
		t.Fatalf("runtime not taken from the environment: %+v", cfg.Dsh)
	}
	if want := filepath.Join(release, "lib", "bin.js"); cfg.Dsh.BinJS != want {
		t.Fatalf("dsh.bin_js = %q, want it derived from current_link (%q)", cfg.Dsh.BinJS, want)
	}
}

// An unconfigured runtime must load, not fail: dshgw doctor exists to report exactly this
// as a failing precondition, and a configuration error would stop it from running at all.
func TestMissingRuntimeIsADoctorFindingNotAConfigError(t *testing.T) {
	t.Setenv("DSHGW_NODE", "")
	t.Setenv("DSHGW_DSH_ROOT", "")
	cfg, err := Load(writeConfig(t, "directory_picker: browse\n"))
	if err != nil {
		t.Fatalf("a configuration without a dsh runtime was rejected: %v", err)
	}
	if cfg.Dsh.NodeBin != "" || cfg.Dsh.BinJS != "" || cfg.Dsh.CurrentLink != "" {
		t.Fatalf("runtime invented from nothing: %+v", cfg.Dsh)
	}
	// A value that IS given still has to be clean and absolute.
	if _, err := Load(writeConfig(t, "directory_picker: browse\ndsh:\n  node_bin: relative/node\n")); err != nil {
		t.Fatalf("a relative runtime path is configuration, not an error: %v", err)
	}
}

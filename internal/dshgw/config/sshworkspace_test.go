package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseCfg is the smallest valid document: the default directory picker is clamp, and that
// plugin has no default path (M63).
const baseCfg = "deploy:\n  plugin_path: ./cmd/dshgw/plugin/picker-clamp.js\n"

// sshKey writes a usable private key file at the requested mode.
func sshKey(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("PRIVATE KEY\n"), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestSSHWorkspacesDefaultsAndPathResolution(t *testing.T) {
	dir := t.TempDir()
	key := sshKey(t, dir, "id_rsa", 0o600)
	// Relative values resolve against the deployment root (the process working directory),
	// which is what lets a deployment name its own paths without an absolute prefix (M63).
	// "." stands in for the deployment root here; the key stays absolute because it must
	// also pass the 0600 check.
	configPath := writeConfig(t, baseCfg+"ssh_workspaces:\n  enabled: true\n  identity_source: "+key+"\n  identity_dir: .\n")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHWorkspaces.MountSubdir != "ssh" {
		t.Errorf("mount_subdir = %q, want the documented default", cfg.SSHWorkspaces.MountSubdir)
	}
	if cfg.SSHWorkspaces.PollInterval.Duration() != 2*time.Second {
		t.Errorf("poll_interval = %v, want 2s", cfg.SSHWorkspaces.PollInterval.Duration())
	}
	if cfg.SSHWorkspaces.MaxEntries != 1000 {
		t.Errorf("max_entries = %d, want 1000", cfg.SSHWorkspaces.MaxEntries)
	}
	// "." is the directory the test process runs in; the point is that the value arrives
	// absolute, because the gateway later joins account names onto it.
	if !filepath.IsAbs(cfg.SSHWorkspaces.IdentityDir) {
		t.Errorf("identity_dir %q was not resolved", cfg.SSHWorkspaces.IdentityDir)
	}
	// The default mount options are the ones the design calls for, and none of them widens
	// the mount to other users.
	joined := strings.Join(cfg.SSHWorkspaces.SSHFSOptions, ",")
	for _, want := range []string{"reconnect", "ServerAliveInterval=15", "idmap=user"} {
		if !strings.Contains(joined, want) {
			t.Errorf("default sshfs options %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "allow_other") || strings.Contains(joined, "allow_root") {
		t.Errorf("default sshfs options widen the mount: %q", joined)
	}
}

func TestSSHWorkspacesRejectsUnsafeConfiguration(t *testing.T) {
	dir := t.TempDir()
	wide := sshKey(t, dir, "wide-key", 0o644)
	good := sshKey(t, dir, "id_rsa", 0o600)
	notExecutable := filepath.Join(dir, "sshfs")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("writing the fake sshfs: %v", err)
	}
	// A key source must be readable: the gateway copies it into each account.
	missing := filepath.Join(dir, "missing-key")

	for name, body := range map[string]string{
		"world readable key":          "ssh_workspaces:\n  enabled: true\n  identity_source: " + wide + "\n",
		"missing key":                 "ssh_workspaces:\n  enabled: true\n  identity_source: " + missing + "\n",
		"identity_dir is a file":      "ssh_workspaces:\n  enabled: true\n  identity_source: " + good + "\n  identity_dir: " + good + "\n",
		"sshfs is not executable":     "ssh_workspaces:\n  enabled: true\n  identity_source: " + good + "\n  sshfs_bin: " + notExecutable + "\n",
		"host with an option":         "ssh_workspaces:\n  enabled: true\n  identity_source: " + good + "\n  hosts: ['-oProxyCommand=x']\n",
		"host with a slash":           "ssh_workspaces:\n  enabled: true\n  identity_source: " + good + "\n  hosts: ['host/path']\n",
		"zero connect timeout":        "ssh_workspaces:\n  enabled: true\n  identity_source: " + good + "\n  connect_timeout: 0s\n",
		"zero poll interval":          "ssh_workspaces:\n  enabled: true\n  identity_source: " + good + "\n  poll_interval: 0s\n",
		"mount container is hidden":   "ssh_workspaces:\n  mount_subdir: .ssh\n",
		"mount container has a slash": "ssh_workspaces:\n  mount_subdir: a/b\n",
		// Even while the feature is off, a mount container that would break the account's
		// picker or collide with a seeded workspace is a configuration error.
		"mount container collides with a seed": "ssh_workspaces:\n  mount_subdir: work\n",
	} {
		if _, err := Load(writeConfig(t, baseCfg+body)); err == nil {
			t.Errorf("unsafe ssh_workspaces config accepted: %s", name)
		}
	}
}

func TestSSHWorkspacesAcceptsPerAccountKeys(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatalf("preparing the key directory: %v", err)
	}
	sshKey(t, keys, "dsh-colin", 0o600)
	cfg, err := Load(writeConfig(t, baseCfg+"ssh_workspaces:\n  enabled: true\n  identity_dir: "+keys+"\n  hosts: ['gpt001', 'aipc']\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHWorkspaces.IdentityDir != keys {
		t.Errorf("identity_dir = %q, want %q", cfg.SSHWorkspaces.IdentityDir, keys)
	}
	if len(cfg.SSHWorkspaces.Hosts) != 2 {
		t.Errorf("hosts = %v, want two entries", cfg.SSHWorkspaces.Hosts)
	}
}

func TestSSHWorkspacesDisabledStillValidatesShape(t *testing.T) {
	// Turning the feature off must not be a way to ship a configuration that fails on the
	// day it is switched on.
	cfg, err := Load(writeConfig(t, baseCfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHWorkspaces.Enabled {
		t.Error("ssh_workspaces must default to disabled")
	}
	if cfg.SSHWorkspaces.MountSubdir != "ssh" {
		t.Errorf("mount_subdir = %q, want the default even while disabled", cfg.SSHWorkspaces.MountSubdir)
	}
}

func TestSSHWorkspacesAcceptsUserUploadedIdentities(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseCfg+"ssh_workspaces:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("upload-only configuration rejected: %v", err)
	}
	if cfg.SSHWorkspaces.IdentitySource != "" || cfg.SSHWorkspaces.IdentityDir != "" {
		t.Fatal("unexpected shared key source")
	}
}

// The alias source is a directory of per-account files, never a single host-wide file.
func TestSSHWorkspacesConfigDirIsAPerAccountDirectory(t *testing.T) {
	dir := t.TempDir()
	seeds := filepath.Join(dir, "seeds")
	if err := os.Mkdir(seeds, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seeds, "dsh-colin"), []byte("Host aipc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, baseCfg+"ssh_workspaces:\n  enabled: true\n  ssh_config_dir: "+seeds+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHWorkspaces.SSHConfigDir != seeds {
		t.Errorf("ssh_config_dir = %q, want %q", cfg.SSHWorkspaces.SSHConfigDir, seeds)
	}

	// A relative value resolves against the deployment root, like every other path here.
	cfg, err = Load(writeConfig(t, baseCfg+"ssh_workspaces:\n  enabled: true\n  ssh_config_dir: .\n"))
	if err != nil {
		t.Fatalf("relative ssh_config_dir rejected: %v", err)
	}
	if !filepath.IsAbs(cfg.SSHWorkspaces.SSHConfigDir) {
		t.Errorf("ssh_config_dir %q was not resolved", cfg.SSHWorkspaces.SSHConfigDir)
	}
}

func TestSSHWorkspacesRefusesAnUnusableConfigDir(t *testing.T) {
	dir := t.TempDir()
	seeds := filepath.Join(dir, "seeds")
	if err := os.Mkdir(seeds, 0o700); err != nil {
		t.Fatal(err)
	}
	wide := filepath.Join(dir, "wide")
	if err := os.Mkdir(wide, 0o777); err != nil {
		t.Fatal(err)
	}
	// Mkdir is masked by the umask, so the mode has to be forced for the check to be tested.
	if err := os.Chmod(wide, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, []byte("Host aipc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "absent")

	for name, value := range map[string]string{
		"missing directory": missing,
		"a file":            file,
		"world writable":    wide,
	} {
		body := baseCfg + "ssh_workspaces:\n  enabled: true\n  ssh_config_dir: " + value + "\n"
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("unusable ssh_config_dir accepted: %s", name)
		}
	}
}

// The deployment account's own ~/.ssh is not a tenant source: it is the one directory this
// feature exists to stop handing out, and a symlink must not be a way around the check.
func TestSSHWorkspacesRefusesTheDeploymentAccountsOwnSSH(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte("Host mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(sshDir, "seeds")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(sshDir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	for _, value := range []string{sshDir, nested, link} {
		body := baseCfg + "ssh_workspaces:\n  enabled: true\n  ssh_config_dir: " + value + "\n"
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Errorf("ssh_config_dir %s was accepted", value)
			continue
		}
		if !strings.Contains(err.Error(), ".ssh") {
			t.Errorf("the refusal of %s does not name the reason: %v", value, err)
		}
	}

	// A directory of its own, outside that tree, is still accepted.
	ok := filepath.Join(t.TempDir(), "ssh-configs")
	if err := os.Mkdir(ok, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeConfig(t, baseCfg+"ssh_workspaces:\n  enabled: true\n  ssh_config_dir: "+ok+"\n")); err != nil {
		t.Errorf("a dedicated ssh_config_dir was refused: %v", err)
	}
}

// The removed key must be reported as a rename, not as a strict-decoding failure: what it
// named is exactly what this version forbids.
func TestSSHWorkspacesRemovedHostWideSourceNamesItsReplacement(t *testing.T) {
	body := baseCfg + "ssh_workspaces:\n  enabled: true\n  ssh_config_source: /home/ops/.ssh/config\n"
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("the removed ssh_config_source key was accepted")
	}
	for _, want := range []string{"ssh_config_source", "ssh_config_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

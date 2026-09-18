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
		"no identity at all":          "ssh_workspaces:\n  enabled: true\n",
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

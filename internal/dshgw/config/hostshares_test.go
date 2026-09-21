package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostShareConfig renders a host_shares document pointing at one host directory.
func hostShareConfig(t *testing.T, dir, body string) string {
	t.Helper()
	share := filepath.Join(dir, "shared")
	if err := os.MkdirAll(share, 0o700); err != nil {
		t.Fatal(err)
	}
	return writeConfig(t, baseCfg+"host_shares:\n"+strings.ReplaceAll(body, "@SHARE@", share))
}

func TestHostSharesDefaults(t *testing.T) {
	// Off by default, and its container does not collide with the ssh one.
	cfg, err := Load(writeConfig(t, baseCfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HostShares.Enabled {
		t.Error("host_shares is enabled by default")
	}
	if cfg.HostShares.Subdir != "host" {
		t.Errorf("subdir = %q, want the documented default", cfg.HostShares.Subdir)
	}
	if cfg.HostShares.Subdir == cfg.SSHWorkspaces.MountSubdir {
		t.Error("the host-share container collides with the ssh mount container")
	}
}

func TestHostSharesReadOnlyIsTheDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := hostShareConfig(t, dir, "  enabled: true\n  shares:\n    - name: docs\n      path: @SHARE@\n      tenants: [dsh-colin]\n")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.HostShares.Shares) != 1 {
		t.Fatalf("shares = %+v, want one", cfg.HostShares.Shares)
	}
	if !cfg.HostShares.Shares[0].EffectiveReadOnly() {
		t.Error("a share without read_only is writable, want read-only by default")
	}
	// An explicit write grant is a decision, and it survives.
	configPath = hostShareConfig(t, dir, "  enabled: true\n  shares:\n    - name: repo\n      path: @SHARE@\n      read_only: false\n      tenants: [dsh-colin]\n")
	cfg, err = Load(configPath)
	if err != nil {
		t.Fatalf("Load with read_only: false: %v", err)
	}
	if cfg.HostShares.Shares[0].EffectiveReadOnly() {
		t.Error("read_only: false was ignored")
	}
}

// The rules that keep one tenant's data out of another's sandbox.
func TestHostSharesRefusals(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Join(state, "workspaces", "dsh-colin"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(state, "workspaces", "dsh-colin")
	outside := filepath.Join(dir, "shared")
	for _, d := range []string{outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink to the state directory is the same exposure as naming it.
	link := filepath.Join(dir, "link-to-state")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "a share that contains the state directory",
			body: "  enabled: true\n  shares:\n    - name: root\n      path: " + dir + "\n      tenants: [dsh-colin]\n",
			want: "overlaps the state directory",
		},
		{
			name: "a share inside the state directory",
			body: "  enabled: true\n  shares:\n    - name: mine\n      path: " + inside + "\n      tenants: [dsh-tenant]\n",
			want: "overlaps the state directory",
		},
		{
			name: "a share reaching the state directory through a symlink",
			body: "  enabled: true\n  shares:\n    - name: via-link\n      path: " + link + "\n      tenants: [dsh-colin]\n",
			want: "overlaps the state directory",
		},
		{
			name: "the file system root",
			body: "  enabled: true\n  shares:\n    - name: all\n      path: /\n      tenants: [dsh-colin]\n",
			want: "refusing to share the file system root",
		},
		{
			name: "no tenants",
			body: "  enabled: true\n  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: []\n",
			want: "tenants must name the accounts",
		},
		{
			name: "a tenant name that is not one",
			body: "  enabled: true\n  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: [Not A Tenant]\n",
			want: "is not an account name",
		},
		{
			name: "a name with a separator",
			body: "  enabled: true\n  shares:\n    - name: a/b\n      path: " + outside + "\n      tenants: [dsh-colin]\n",
			want: "one visible path segment",
		},
		{
			name: "a hidden name",
			body: "  enabled: true\n  shares:\n    - name: .hidden\n      path: " + outside + "\n      tenants: [dsh-colin]\n",
			want: "one visible path segment",
		},
		{
			name: "a relative path",
			body: "  enabled: true\n  shares:\n    - name: docs\n      path: ./shared\n      tenants: [dsh-colin]\n",
			want: "absolute host directory",
		},
		{
			name: "a path that is not a directory",
			body: "  enabled: true\n  shares:\n    - name: docs\n      path: /etc/hostname\n      tenants: [dsh-colin]\n",
			want: "is not a directory",
		},
		{
			name: "a missing path",
			body: "  enabled: true\n  shares:\n    - name: docs\n      path: " + filepath.Join(dir, "absent") + "\n      tenants: [dsh-colin]\n",
			want: "no such file",
		},
		{
			name: "shares without the switch",
			body: "  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: [dsh-colin]\n",
			want: "host_shares.enabled is not",
		},
		{
			name: "the switch without shares",
			body: "  enabled: true\n",
			want: "requires at least one entry",
		},
		{
			name: "a duplicate name",
			body: "  enabled: true\n  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: [dsh-colin]\n    - name: docs\n      path: " + inside + "\n      tenants: [dsh-colin]\n",
			want: "duplicate share name",
		},
		{
			name: "a container that collides with the ssh mount container",
			body: "  enabled: true\n  subdir: ssh\n  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: [dsh-colin]\n",
			want: "collides with ssh_workspaces.mount_subdir",
		},
		{
			name: "a container that collides with a workspace seed",
			body: "  enabled: true\n  subdir: work\n  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: [dsh-colin]\n",
			want: "collides with a workspace_seed name",
		},
		{
			name: "a container that is not one segment",
			body: "  enabled: true\n  subdir: a/b\n  shares:\n    - name: docs\n      path: " + outside + "\n      tenants: [dsh-colin]\n",
			want: "one visible path segment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeConfig(t, "state_dir: "+state+"\n"+baseCfg+"host_shares:\n"+tc.body)
			_, err := Load(configPath)
			if err == nil {
				t.Fatalf("config was accepted, want a refusal mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A share outside the state directory is accepted, keeps its order, and is shared with exactly
// the accounts it names.
func TestHostSharesAccepted(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(dir, "state")
	configPath := writeConfig(t, "state_dir: "+state+"\n"+baseCfg+
		"host_shares:\n  enabled: true\n  shares:\n"+
		"    - name: first\n      path: "+first+"\n      tenants: [dsh-colin, dsh-tenant]\n"+
		"    - name: second\n      path: "+second+"\n      read_only: false\n      tenants: [dsh-tenant]\n")
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.HostShares.Shares) != 2 || cfg.HostShares.Shares[0].Name != "first" || cfg.HostShares.Shares[1].Name != "second" {
		t.Fatalf("shares = %+v, want both in declaration order", cfg.HostShares.Shares)
	}
	if got := cfg.HostShares.Shares[0].Tenants; len(got) != 2 || got[0] != "dsh-colin" {
		t.Errorf("tenants = %v", got)
	}
}

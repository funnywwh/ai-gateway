package sshworkspace

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putKey(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("PRIVATE TEST KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hostKey(remote Remote, host string) string {
	return filepath.Join(remote.Workspace, ".ssh", "host_keys", fmt.Sprintf("%x", sha256.Sum256([]byte(host))), "id_rsa")
}

func TestSplitHostSpecPorts(t *testing.T) {
	for _, tc := range []struct {
		spec, target string
		port         int
	}{
		{"host", "host", 0}, {"alice@host", "alice@host", 0}, {"alice@host:1", "alice@host", 1}, {"host:65535", "host", 65535}, {"host:0022", "host", 22},
	} {
		target, port, err := SplitHostSpec(tc.spec)
		if err != nil || target != tc.target || port != tc.port {
			t.Errorf("SplitHostSpec(%q) = %q, %d, %v", tc.spec, target, port, err)
		}
	}
	for _, spec := range []string{"host:", "host:0", "host:-1", "host:65536", "host:abc", "host:1:22", "::1", "[::1]:22", "host:99999999999999999999999", ":22"} {
		if _, _, err := SplitHostSpec(spec); CodeOf(err) != CodeHostUnknown {
			t.Errorf("accepted %q: %v", spec, err)
		}
		if err := (Options{Hosts: []string{spec}}).permits(spec); CodeOf(err) != CodeHostUnknown {
			t.Errorf("allowlist bypass for %q", spec)
		}
	}
	options := Options{Hosts: []string{"alice@host:22"}}
	for _, spec := range []string{"host", "host:22", "alice@host", "bob@host:22", "alice@host:0022"} {
		if options.permits(spec) == nil {
			t.Errorf("allowlist accepted %q", spec)
		}
	}
	if err := options.permits("alice@host:22"); err != nil {
		t.Fatal(err)
	}
}

func TestHostIdentityPriorityAndIsolation(t *testing.T) {
	a := Remote{Workspace: t.TempDir()}
	b := Remote{Workspace: t.TempDir()}
	host := "alice@host:2222"
	defaultKey := filepath.Join(a.Workspace, ".ssh", "id_rsa")
	putKey(t, defaultKey)
	putKey(t, hostKey(a, host))
	for _, tc := range []struct{ host, key string }{
		{host, hostKey(a, host)}, {"bob@host:2222", defaultKey}, {"alice@host:22", defaultKey},
	} {
		paths, err := pathsFor(Options{}, a, tc.host)
		if err != nil || paths.key != tc.key {
			t.Fatalf("paths(%s): %+v %v", tc.host, paths, err)
		}
	}
	// Nothing outside this account's own workspace is ever a key source, whatever the
	// deployment configured: another account's key — or a directory an earlier version copied
	// every account's key out of — is not reachable from here.
	if _, err := pathsFor(Options{IdentityDir: filepath.Dir(defaultKey)}, b, host); CodeOf(err) != CodeAuthFailed {
		t.Fatalf("cross-account fallback: %v", err)
	}
	if err := os.Remove(defaultKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pathsFor(Options{}, a, "bob@host:2222"); CodeOf(err) != CodeAuthFailed {
		t.Fatalf("cross-user host key fallback: %v", err)
	}
	for name, args := range map[string][]string{
		"ssh":   (Options{}).sshArgs(a, host, "true"),
		"sshfs": (Options{}).sshfsArgs(a, host, "/app", "/mount"),
	} {
		joined := strings.Join(args, " ")
		for _, want := range []string{"-p 2222", "alice@host", hostKey(a, host), "IdentityAgent=none", "IdentitiesOnly=yes", "-F /dev/null"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s args missing %q: %s", name, want, joined)
			}
		}
		if strings.Contains(joined, "alice@host:2222") {
			t.Errorf("%s did not strip port: %s", name, joined)
		}
	}
}

func TestManagedDefaultDeletionSurvivesRestart(t *testing.T) {
	env := newTestEnv(t, Options{})
	dir := filepath.Join(env.remote.Workspace, ".ssh")
	if err := os.WriteFile(filepath.Join(dir, "identity-managed"), []byte("managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "id_rsa")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := env.service.EnsureIdentity(env.remote.Tenant, env.remote.Workspace, env.remote.DshHome); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "id_rsa")); !os.IsNotExist(err) {
			t.Fatalf("deleted default resurrected: %v", err)
		}
	}
	for _, name := range []string{filepath.Join(dir, "known_hosts"), filepath.Join(dir, "config"), filepath.Join(env.remote.DshHome, "ssh-mounts.json")} {
		if !isFile(name) {
			t.Errorf("missing initialization output %s", name)
		}
	}
	if _, err := env.service.options.ssh(context.Background(), env.fake.exec, env.remote, "host", "true"); CodeOf(err) != CodeAuthFailed {
		t.Fatalf("ssh without key: %v", err)
	}
	if err := env.service.mount(context.Background(), env.remote, "host", "/app", "/mount"); CodeOf(err) != CodeAuthFailed {
		t.Fatalf("sshfs without key: %v", err)
	}
	if len(env.fake.calls) != 0 {
		t.Fatalf("executed without a key: %v", env.fake.calls)
	}
}

func TestIdentityRejectsSymlinksAndPermissions(t *testing.T) {
	for _, kind := range []string{"key", "host-key", "ssh-dir", "host-dir", "wide-key", "wide-dir", "hardlink", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			remote := Remote{Workspace: t.TempDir()}
			victim := filepath.Join(t.TempDir(), "id_rsa")
			putKey(t, victim)
			key := filepath.Join(remote.Workspace, ".ssh", "id_rsa")
			putKey(t, key)
			switch kind {
			case "wide-key":
				if err := os.Chmod(key, 0o644); err != nil {
					t.Fatal(err)
				}
			case "wide-dir":
				if err := os.Chmod(filepath.Dir(key), 0o777); err != nil {
					t.Fatal(err)
				}
			case "key", "hardlink", "dangling":
				if err := os.Remove(key); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "hardlink" {
					err = os.Link(victim, key)
				} else if kind == "dangling" {
					err = os.Symlink(victim+"-missing", key)
				} else {
					err = os.Symlink(victim, key)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "ssh-dir":
				if err := os.RemoveAll(filepath.Dir(key)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(victim), filepath.Dir(key)); err != nil {
					t.Fatal(err)
				}
			case "host-dir":
				if err := os.Symlink(filepath.Dir(victim), filepath.Join(remote.Workspace, ".ssh", "host_keys")); err != nil {
					t.Fatal(err)
				}
			case "host-key":
				hostPath := hostKey(remote, "host")
				if err := os.MkdirAll(filepath.Dir(hostPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, hostPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pathsFor(Options{}, remote, "host"); CodeOf(err) != CodeAuthFailed {
				t.Fatalf("unsafe identity accepted: %v", err)
			}
		})
	}
}

func TestRuntimeConfigCannotAddIdentities(t *testing.T) {
	env := newTestEnv(t, Options{})
	config := "Host alias\n HostName real.example\n User configured\n Port 2200\n IdentityFile /secret/other-key\n IdentityAgent /secret/agent\n Include /secret/config\n ProxyCommand touch /bad\n"
	if err := os.WriteFile(filepath.Join(env.remote.Workspace, ".ssh", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	options := Options{SSHFSOptions: []string{"IdentityFile=/secret/key", "IdentityAgent=agent", "IdentitiesOnly=no", "ssh_command=evil", "reconnect,IdentityFile=/secret/key"}}
	for _, host := range []string{"alias", "explicit@alias:2222"} {
		for i, args := range [][]string{options.sshArgs(env.remote, host, "true"), options.sshfsArgs(env.remote, host, "/app", "/mount")} {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "/secret") || strings.Contains(joined, "evil") || strings.Contains(joined, "/bad") {
				t.Errorf("unsafe config carried into args: %s", joined)
			}
			if i == 1 {
				// Exact alias destination/port behavior is covered by TestSSHFSResolvedAlias.
				if !strings.Contains(joined, "@real.example:/app") {
					t.Errorf("lost sshfs alias: %s", joined)
				}
				continue
			}
			if !strings.Contains(joined, "HostName=real.example") {
				t.Errorf("lost alias: %s", joined)
			}
			if host == "alias" && (!strings.Contains(joined, "User=configured") || !strings.Contains(joined, "Port=2200")) {
				t.Errorf("lost alias settings: %s", joined)
			}
			if host != "alias" && (strings.Contains(joined, "User=configured") || strings.Contains(joined, "Port=2200")) {
				t.Errorf("config overrides explicit user/port: %s", joined)
			}
		}
	}
}

func TestAliasEqualsAndInvalidPort(t *testing.T) {
	env := newTestEnv(t, Options{})
	configPath := filepath.Join(env.remote.Workspace, ".ssh", "config")
	for _, value := range []string{"0", "-1", "+22", "65536", "abc", "99999999999999999999999"} {
		if err := os.WriteFile(configPath, []byte("Host=alias\nHostName=real.example\nUser=configured\nPort="+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := pathsFor(Options{}, env.remote, "alias"); CodeOf(err) != CodeHostUnknown {
			t.Errorf("invalid config port %q accepted: %v", value, err)
		}
		paths, err := pathsFor(Options{}, env.remote, "alice@alias:2222")
		if err != nil {
			t.Fatalf("explicit port must override invalid alias port: %v", err)
		}
		if strings.Join(paths.alias, " ") != "HostName=real.example" {
			t.Errorf("unexpected aliases: %v", paths.alias)
		}
	}
	for _, setting := range []string{"HostName=bad@name", "User=bad:user", "HostName=-option", "User=-option", "HostName=bad,User=evil", "User=bad;command", "User=bad\"quote", "HostName=bad$(command)"} {
		if err := os.WriteFile(configPath, []byte("Host=alias\n"+setting+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := pathsFor(Options{}, env.remote, "alias"); CodeOf(err) != CodeHostUnknown {
			t.Errorf("invalid alias %q accepted: %v", setting, err)
		}
	}
}

func TestEnsureIdentityRejectsSymlinkDefault(t *testing.T) {
	env := newTestEnv(t, Options{})
	path := filepath.Join(env.remote.Workspace, ".ssh", "id_rsa")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "id_rsa")
	putKey(t, victim)
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := env.service.EnsureIdentity(env.remote.Tenant, env.remote.Workspace, env.remote.DshHome); CodeOf(err) != CodeInvalidState {
		t.Fatalf("accepted symlink during init: %v", err)
	}
}

func TestKeylessInitializationCreatesMetadata(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.service.options.IdentityDir = ""
	dir := filepath.Join(env.remote.Workspace, ".ssh")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(env.remote.DshHome, "ssh-mounts.json")
	if err := os.Remove(mirror); err != nil {
		t.Fatal(err)
	}
	if err := env.service.EnsureIdentity(env.remote.Tenant, env.remote.Workspace, env.remote.DshHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "id_rsa")); !os.IsNotExist(err) {
		t.Fatalf("unexpected default key: %v", err)
	}
	for _, name := range []string{filepath.Join(dir, "known_hosts"), filepath.Join(dir, "config"), mirror} {
		if !isFile(name) {
			t.Errorf("missing initialization output %s", name)
		}
	}
}

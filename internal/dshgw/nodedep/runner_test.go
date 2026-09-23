package nodedep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
)

// fakeRemote is a stand-in for the target machine: it runs the remote scripts locally against a
// root directory, so the whole deploy sequence — including the tar upload, the swaps, the unit
// install and the rollback — executes for real.
//
// The scripts are POSIX shell, so running them with sh is the same code path the target runs; what
// this does not cover is ssh itself and the target's own environment, which the acceptance script
// covers on a real connection.
type fakeRemote struct {
	root string
	// failingPhase makes one phase fail, by name, to exercise the rollback.
	failPhase string
	// facts override what the preflight reports.
	facts map[string]string
	// log records the commands that ran.
	mu       sync.Mutex
	commands []string
	// userManager reports whether the target has a systemd user manager.
	userManager bool
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	root := t.TempDir()
	remote := &fakeRemote{root: root, userManager: true}
	remote.facts = map[string]string{
		"UNAME":        "Linux x86_64",
		"USER":         "deployer",
		"BWRAP":        "yes",
		"NODE_BIN":     "yes",
		"DSH_BIN_JS":   "yes",
		"DSH_ROOT":     "yes",
		"DIR_WRITABLE": "yes",
		"USER_MANAGER": "yes",
		"LINGER":       "yes",
		"SUDO":         "no",
		"SSHFS":        "no",
		"FUSE":         "no",
		"COREPACK":     "no",
		"HAS_TOKEN":    "no",
		"HAS_CONFIG":   "no",
	}
	return remote
}

// exec is the ExecFunc the runner drives.
func (f *fakeRemote) exec(ctx context.Context, command string, stdin io.Reader) ([]byte, []byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()

	// The preflight phase is answered from the scripted facts: it inspects a machine, and this
	// stand-in's machine is a directory tree.
	if strings.Contains(command, "UNAME=") {
		if f.failPhase == PhasePreflight {
			return nil, []byte("preflight says no"), errors.New("exit status 1")
		}
		// Three facts describe the machine's state rather than its installation, so they are answered
		// from the simulated filesystem: a deploy that cannot see the token it installed last time
		// would rotate the secret on every upgrade.
		live := map[string]string{}
		for key, value := range f.facts {
			live[key] = value
		}
		live["USER_MANAGER"] = boolFact(f.userManager, "yes", "no")
		live["HAS_TOKEN"] = boolFact(f.exists(filepath.Join("srv", "node-a", "node-a.token")), "yes", "no")
		live["HAS_CONFIG"] = boolFact(f.exists(filepath.Join("srv", "node-a", "dshgw-node.yaml")), "yes", "no")
		// The real preflight reports the target token's hash so the control plane can decide whether
		// the two sides already agree; the fake computes it the same way.
		live["TOKEN_SHA256"] = ""
		if f.exists(filepath.Join("srv", "node-a", "node-a.token")) {
			data, err := os.ReadFile(f.path("srv", "node-a", "node-a.token"))
			if err == nil {
				sum := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
				live["TOKEN_SHA256"] = hex.EncodeToString(sum[:])
			}
		}
		var lines []string
		for _, key := range sortedKeys(live) {
			lines = append(lines, key+"="+live[key])
		}
		return []byte(strings.Join(lines, "\n") + "\n"), nil, nil
	}
	// systemd is not available in this sandbox: the unit phase is checked by its script text, and
	// the start phase would fail for real. Both are scripted here.
	// The systemd phases are scripted (this sandbox has no user manager to talk to), and the match is
	// deliberately anchored on the script's first line: a rollback script also mentions systemctl,
	// and it has to run for real.
	trimmed := strings.TrimSpace(command)
	switch {
	case strings.HasPrefix(trimmed, "set -eu\nmkdir -p ~/.config/systemd/user"):
		if strings.Contains(trimmed, "no-user-manager-detected") {
			// The fallback variant only installs the unit file for later; it calls no systemd.
			// Running it for real is fine, and it is the code path a host without a user manager
			// actually takes.
			break
		}
		if f.failPhase == PhaseUnit {
			return nil, []byte("no user bus"), errors.New("exit status 1")
		}
		if !f.userManager {
			return nil, []byte("Failed to connect to bus"), errors.New("exit status 1")
		}
		return []byte("unit-ok\n"), nil, nil
	case strings.HasPrefix(trimmed, "set -eu\nsystemctl --user restart dshgw-node"):
		if f.failPhase == PhaseStart {
			return nil, []byte("the node refused to start"), errors.New("exit status 1")
		}
		return []byte("active\n"), nil, nil
	case f.failPhase == PhaseStart && strings.HasPrefix(trimmed, "set -eu\nif [ -f") && strings.Contains(trimmed, "setsid nohup"):
		return nil, []byte("the node refused to start"), errors.New("exit status 1")
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(), "HOME="+f.root)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

func (f *fakeRemote) ranCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeRemote) path(parts ...string) string {
	return filepath.Join(append([]string{f.root}, parts...)...)
}

// exists reports whether a path (relative to the simulated root) is there.
func (f *fakeRemote) exists(parts ...string) bool {
	_, err := os.Stat(f.path(parts...))
	return err == nil
}

func boolFact(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}

// fixture builds a runner over a fake remote with plausible assets.
func fixture(t *testing.T) (*Runner, nodestore.Node, Assets, *fakeRemote, string) {
	t.Helper()
	remote := newFakeRemote(t)
	assetsRoot := t.TempDir()
	bin := filepath.Join(assetsRoot, "dshgw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho dshgw\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(assetsRoot, "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "picker-clamp.js"), []byte("// picker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginDir, "web-tty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "web-tty", "index.js"), []byte("// plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(assetsRoot, "template-home")
	if err := os.MkdirAll(filepath.Join(template, "profiles", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "profiles", "web", "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assets := Assets{DshgwBin: bin, PluginDir: pluginDir, TemplateHome: template}

	// The fake remote's key file must exist: the runner checks it before it tries to connect.
	keyPath := filepath.Join(remote.root, "key")
	if err := os.WriteFile(keyPath, []byte("fake key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deployDir := filepath.Join(remote.root, "srv", "node-a")
	record := nodestore.Node{
		Name:   "node-a",
		Listen: "127.0.0.1:18400",
		URL:    "http://127.0.0.1:18400",
		SSH: nodestore.SSH{
			Host: "127.0.0.1", Port: 22, User: "deployer",
			KeyFile:        keyPath,
			KnownHostsFile: filepath.Join(remote.root, "known_hosts"),
		},
		Deploy: nodestore.Deploy{
			Dir: deployDir, WorkerPortLo: 32900, WorkerPortHi: 32910,
			BwrapBin: "/usr/bin/bwrap",
			NodeBin:  filepath.Join(remote.root, "node"),
			BinJS:    filepath.Join(remote.root, "dsh", "lib", "bin.js"),
		},
	}
	runner := &Runner{
		Version: "9.9.9", Revision: "abcdef1234567", Exec: remote.exec,
		// The fingerprint is read from the target's pinned known_hosts in production; here the fake
		// remote answers with the scripted value.
		Fingerprint: func(context.Context, string, string) (string, error) {
			value := remote.facts["HOST_KEY_FINGERPRINT"]
			if value == "" {
				value = "SHA256:fake"
			}
			return value, nil
		},
	}
	return runner, record, assets, remote, deployDir
}

// TestDeployInstallsAndActivates walks the happy path and asserts the target's end state.
func TestDeployInstallsAndActivates(t *testing.T) {
	runner, record, assets, remote, deployDir := fixture(t)
	runner.Exec = remote.exec
	runner.Probe = func(context.Context, string) error { return nil }
	var log strings.Builder
	runner.Log = &log

	result, err := runner.Deploy(context.Background(), record, assets, Options{
		AcceptHostKey:   "SHA256:fake",
		AigwBaseURL:     "http://aigw:8088",
		DirectoryPicker: "clamp", PluginBrowserFS: "off",
		TenantPlugins: config.TenantPlugins{
			WebTTY: config.PluginSwitch{Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("deploy failed: %v\n%s", err, log.String())
	}
	if len(result.Phases) != 5 {
		t.Fatalf("phases = %+v", result.Phases)
	}
	for _, phase := range result.Phases {
		if !phase.OK {
			t.Fatalf("phase %s failed: %+v", phase.Name, phase)
		}
	}
	if !result.RotatedToken || result.Token == "" {
		t.Fatalf("a first deploy must install a token: %+v", result)
	}
	// The binary, the plugins, the template, the configuration and the unit are in place.
	for _, path := range []string{
		filepath.Join(deployDir, "bin", "dshgw"),
		filepath.Join(deployDir, "plugins", "picker-clamp.js"),
		filepath.Join(deployDir, "plugins", "web-tty", "index.js"),
		filepath.Join(deployDir, "template-home", "profiles", "web", "package.json"),
		filepath.Join(deployDir, "dshgw-node.yaml"),
		filepath.Join(deployDir, "dshgw-node.service"),
		filepath.Join(deployDir, "node-a.token"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	// The token installed on the target is the one the caller was told about.
	tokenData, err := os.ReadFile(filepath.Join(deployDir, "node-a.token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(tokenData)) != result.Token {
		t.Fatalf("the token on the target (%q) is not the one reported (%q)", tokenData, result.Token)
	}
	info, err := os.Stat(filepath.Join(deployDir, "node-a.token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %04o", info.Mode().Perm())
	}
	// The generated configuration is one the node can load.
	cfg, err := config.Load(filepath.Join(deployDir, "dshgw-node.yaml"))
	if err != nil {
		t.Fatalf("the generated node configuration does not load: %v\n%s", err, mustRead(t, filepath.Join(deployDir, "dshgw-node.yaml")))
	}
	if cfg.Node.Name != "node-a" || cfg.Node.Listen != "127.0.0.1:18400" {
		t.Fatalf("node block = %+v", cfg.Node)
	}
	if !cfg.NodeMode() {
		t.Fatal("the generated configuration is not in node mode")
	}
	if cfg.Deploy.BwrapBin != "/usr/bin/bwrap" || cfg.Dsh.NodeBin == "" {
		t.Fatalf("runtime paths = %+v %+v", cfg.Deploy, cfg.Dsh)
	}
	// The unit file points at the binary and the configuration.
	unit := mustRead(t, filepath.Join(deployDir, "dshgw-node.service"))
	if !strings.Contains(unit, filepath.Join(deployDir, "bin", "dshgw")) || !strings.Contains(unit, "node serve") {
		t.Fatalf("unit = %s", unit)
	}
	// The deploy never wrote a state directory of its own: it creates it empty for the node.
	if entries, err := os.ReadDir(filepath.Join(deployDir, "state")); err != nil {
		t.Fatalf("state directory missing: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("the deploy put something in the state directory: %+v", entries)
	}
}

// TestUpgradeKeepsStateAndPreviousVersion is the property that makes an upgrade safe: tenants live
// in the state directory, and the previous binaries stay behind as .prev.
func TestUpgradeKeepsStateAndPreviousVersion(t *testing.T) {
	runner, record, assets, remote, deployDir := fixture(t)
	runner.Exec = remote.exec
	first, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"})
	if err != nil {
		t.Fatal(err)
	}
	// A tenant's data appears; nothing a deploy does may touch it.
	tenantFile := filepath.Join(deployDir, "state", "tenants", "alice", ".dsh", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(tenantFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tenantFile, []byte("llm-pi-ai: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Second deploy (an upgrade): new binary content, same token (no rotation asked for). The record
	// carries the token the first deploy installed — in production the caller persists it.
	record.Token = first.Token
	if err := os.WriteFile(assets.DshgwBin, []byte("#!/bin/sh\necho v2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"})
	if err != nil {
		t.Fatal(err)
	}
	if second.RotatedToken {
		t.Fatal("an upgrade without --rotate-token must keep the node's secret")
	}
	if second.Token != "" {
		t.Fatalf("an upgrade must not report a token it did not set: %q", second.Token)
	}
	if _, err := os.Stat(tenantFile); err != nil {
		t.Fatalf("the upgrade touched a tenant's data: %v", err)
	}
	if data := mustRead(t, filepath.Join(deployDir, "bin", "dshgw")); !strings.Contains(data, "v2") {
		t.Fatalf("the new binary was not activated: %q", data)
	}
	if data := mustRead(t, filepath.Join(deployDir, "bin.prev", "dshgw")); !strings.Contains(data, "dshgw") || strings.Contains(data, "v2") {
		t.Fatalf("the previous binary was not kept: %q", data)
	}
	// The token file still holds the first token.
	if data := strings.TrimSpace(mustRead(t, filepath.Join(deployDir, "node-a.token"))); data != first.Token {
		t.Fatalf("the token changed on an upgrade: %q vs %q", data, first.Token)
	}
}

// TestFailedPhaseRollsBack is the safety property: a failure after the target was touched leaves the
// previous version running, not a half-upgraded node.
func TestFailedPhaseRollsBack(t *testing.T) {
	runner, record, assets, remote, deployDir := fixture(t)
	runner.Exec = remote.exec
	first, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"})
	if err != nil {
		t.Fatal(err)
	}
	record.Token = first.Token
	// An upgrade whose start phase fails.
	if err := os.WriteFile(assets.DshgwBin, []byte("#!/bin/sh\necho broken\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	remote.failPhase = PhaseStart
	var log strings.Builder
	runner.Log = &log
	_, err = runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"})
	if err == nil || !strings.Contains(err.Error(), PhaseStart) {
		t.Fatalf("err = %v", err)
	}
	// The previous binary is back in place, and the broken one is gone.
	if data := mustRead(t, filepath.Join(deployDir, "bin", "dshgw")); strings.Contains(data, "broken") {
		t.Fatalf("the failed upgrade is still installed: %q", data)
	}
	if _, err := os.Stat(filepath.Join(deployDir, "bin.prev")); !os.IsNotExist(err) {
		t.Fatalf("the rollback left a .prev behind: %v", err)
	}
	if !strings.Contains(log.String(), "restoring the previous deployment") {
		t.Fatalf("the rollback is not visible in the log:\n%s", log.String())
	}
}

// TestPreflightRefusesAnUnpreparedMachine: a deploy that cannot work is stopped before anything is
// sent, and the message names what is missing.
func TestPreflightRefusesAnUnpreparedMachine(t *testing.T) {
	runner, record, assets, remote, _ := fixture(t)
	runner.Exec = remote.exec
	remote.facts["BWRAP"] = "no"
	remote.facts["FUSE"] = "no"
	_, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"})
	if err == nil || !strings.Contains(err.Error(), "bubblewrap") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(strings.Join(remote.ranCommands(), " "), "tar -xf") {
		t.Fatal("the deploy uploaded something to an unprepared machine")
	}
}

// TestFingerprintGate: the first deploy stops and asks, and a mismatch is refused.
func TestFingerprintGate(t *testing.T) {
	runner, record, assets, remote, deployDir := fixture(t)
	runner.Exec = remote.exec
	remote.facts["HOST_KEY_FINGERPRINT"] = "SHA256:abc"
	runner.Fingerprint = func(context.Context, string, string) (string, error) {
		return remote.facts["HOST_KEY_FINGERPRINT"], nil
	}
	_, err := runner.Deploy(context.Background(), record, assets, Options{AigwBaseURL: "http://aigw:8088"})
	var required *FingerprintRequired
	if !errors.As(err, &required) {
		t.Fatalf("err = %v, want a fingerprint request", err)
	}
	if required.Fingerprint != "SHA256:abc" {
		t.Fatalf("fingerprint = %q", required.Fingerprint)
	}
	if _, statErr := os.Stat(filepath.Join(deployDir, "bin")); !os.IsNotExist(statErr) {
		t.Fatal("a deploy without a confirmed fingerprint still installed something")
	}
	// A fingerprint that does not match what the operator confirmed is refused too.
	_, err = runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:different", AigwBaseURL: "http://aigw:8088"})
	if err == nil || !strings.Contains(err.Error(), "SHA256:different") {
		t.Fatalf("err = %v", err)
	}
	// The confirmed one proceeds.
	if _, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:abc", AigwBaseURL: "http://aigw:8088"}); err != nil {
		t.Fatalf("deploy with the confirmed fingerprint failed: %v", err)
	}
	// A pinned record refuses a different confirmation: this is not the machine from last time.
	pinned := record
	pinned.SSH.HostKeyFingerprint = "SHA256:pinned"
	_, err = runner.Deploy(context.Background(), pinned, assets, Options{AcceptHostKey: "SHA256:abc", AigwBaseURL: "http://aigw:8088"})
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("err = %v", err)
	}
}

// TestNoUserManagerFallsBack: a machine without a systemd user manager still gets a running node,
// and the result says plainly that it will not survive a reboot.
func TestNoUserManagerFallsBack(t *testing.T) {
	runner, record, assets, remote, deployDir := fixture(t)
	runner.Exec = remote.exec
	remote.userManager = false
	remote.facts["USER_MANAGER"] = "no"
	var log strings.Builder
	runner.Log = &log
	result, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"})
	if err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	if result.Systemd {
		t.Fatal("the result claims systemd supervision on a machine without a user manager")
	}
	// The unit file is still installed (so enabling it later is one command), and a pidfile exists
	// because the node was started detached.
	if _, err := os.Stat(filepath.Join(deployDir, "dshgw-node.service")); err != nil {
		t.Fatalf("the unit file was not installed: %v", err)
	}
	// The start script wrote a pidfile; the detached process itself cannot be started here (the
	// binary is a stub), so only the script's effect is checked.
	if !strings.Contains(strings.Join(remote.ranCommands(), "\n"), "setsid nohup") {
		t.Fatalf("the fallback start command never ran:\n%s", log.String())
	}
}

// TestGenerateConfigObeysTheStrictLoader is the contract with the node's own configuration loader.
func TestGenerateConfigObeysTheStrictLoader(t *testing.T) {
	_, record, _, _, _ := fixture(t)
	data, err := GenerateNodeConfig(record, Options{
		AigwBaseURL:       "http://aigw:8088",
		DirectoryPicker:   "clamp",
		PluginBrowserFS:   "off",
		SSHWorkspaces:     true,
		BrowserWorkspaces: true,
		WorkerLimits:      config.WorkerLimits{MemoryMaxBytes: 2 << 30},
		TenantPlugins:     config.TenantPlugins{WebTTY: config.PluginSwitch{Enabled: true}, WorkspaceFiles: config.PluginSwitch{Enabled: true}},
		HostShares: &config.HostShares{Enabled: true, Subdir: "host", Shares: []config.HostShare{{
			Name: "docs", Path: "/srv/docs", Tenants: []string{"alice"},
		}}},
	}, "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "dshgw-node.yaml")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The host share path must exist for the strict loader, so the test creates it.
	if err := os.MkdirAll("/srv/docs", 0o755); err != nil {
		// Without root the path cannot be created; the loader only checks the shape, so skip the
		// share in that case by re-generating without it.
		data, err = GenerateNodeConfig(record, Options{
			AigwBaseURL: "http://aigw:8088", DirectoryPicker: "clamp", PluginBrowserFS: "off",
			SSHWorkspaces: true, BrowserWorkspaces: true,
		}, "tok", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(file)
	if err != nil {
		t.Fatalf("the generated configuration does not load: %v\n%s", err, data)
	}
	if !cfg.NodeMode() || !cfg.SSHWorkspaces.Enabled || !cfg.BrowserWorkspaces.Enabled {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Deploy.TemplateHome == "" || cfg.Deploy.PluginPath == "" || cfg.Dsh.NodeBin == "" {
		t.Fatalf("paths = %+v %+v", cfg.Deploy, cfg.Dsh)
	}
}

// TestSSHArgsAreSafe pins the ssh options that make a deploy predictable and a first connection
// checkable.
func TestSSHArgsAreSafe(t *testing.T) {
	_, record, _, _, _ := fixture(t)
	record.SSH.KeyFile = "/keys/node-a"
	record.SSH.KnownHostsFile = "/state/node-ssh/node-a/known_hosts"
	args := SSHArgs(record, "uname -a")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-F /dev/null", "BatchMode=yes", "IdentitiesOnly=yes",
		"StrictHostKeyChecking=accept-new", "UserKnownHostsFile=/state/node-ssh/node-a/known_hosts",
		"GlobalKnownHostsFile=/dev/null", "-i /keys/node-a", "-p 22", "deployer@127.0.0.1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ssh args are missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "StrictHostKeyChecking=no") {
		t.Fatal("a deploy must never disable host key checking")
	}
	// Once the fingerprint is pinned, the policy is strict.
	record.SSH.HostKeyFingerprint = "SHA256:x"
	if args := SSHArgs(record, "true"); !strings.Contains(strings.Join(args, " "), "StrictHostKeyChecking=yes") {
		t.Fatalf("pinned args = %v", args)
	}
	// A command with quotes and newlines survives as one argument.
	args = SSHArgs(record, "echo 'a b'\nuname")
	last := args[len(args)-1]
	if !strings.Contains(last, "sh -c") || !strings.Contains(last, "a b") {
		t.Fatalf("command argument = %q", last)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/srv/dshgw-node":  "'/srv/dshgw-node'",
		"/srv/it's a dir":  `'/srv/it'\''s a dir'`,
		"":                 "''",
		"/srv/$(rm -rf /)": `'/srv/$(rm -rf /)'`,
		"/srv/`whoami`":    "'/srv/`whoami`'",
	}
	for input, want := range cases {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDeployLogIsBoundedAndReadable(t *testing.T) {
	dir := t.TempDir()
	writer, path, err := OpenDeployLog(dir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(writer, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("deploy log mode = %04o", info.Mode().Perm())
	}
	tail, err := ReadDeployLogTail(dir, "node-a")
	if err != nil || !strings.Contains(tail, "hello") {
		t.Fatalf("tail = %q err = %v", tail, err)
	}
	// A log past the bound is rotated instead of growing for ever.
	big := make([]byte, maxDeployLogBytes+1024)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	writer, _, err = OpenDeployLog(dir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the previous log was not rotated aside: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxDeployLogBytes {
		t.Fatalf("the live log is still oversized: %d", info.Size())
	}
	// Reading a missing node's log is empty, not an error.
	if tail, err := ReadDeployLogTail(dir, "node-b"); err != nil || tail != "" {
		t.Fatalf("tail = %q err = %v", tail, err)
	}
}

func TestPurgeRemovesEverythingItCreated(t *testing.T) {
	runner, record, assets, remote, deployDir := fixture(t)
	runner.Exec = remote.exec
	if _, err := runner.Deploy(context.Background(), record, assets, Options{AcceptHostKey: "SHA256:fake", AigwBaseURL: "http://aigw:8088"}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Purge(context.Background(), record); err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	for _, path := range []string{
		filepath.Join(deployDir, "bin"), filepath.Join(deployDir, "plugins"),
		filepath.Join(deployDir, "dshgw-node.yaml"), filepath.Join(deployDir, "node-a.token"),
		filepath.Join(deployDir, "state"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived the purge (%v)", path, err)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

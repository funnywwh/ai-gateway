package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

// The deployment invariants of the aigw-supervised shape (M58): private state, a
// configuration example that loads as written, and a launcher that must run as an
// ordinary user. The systemd/nginx assertions this file used to carry belonged to
// the deleted shape.

func TestPrivateRootsRejectGroupOrOtherAccess(t *testing.T) {
	root := t.TempDir()
	for _, mode := range []os.FileMode{0o750, 0o755, 0o770, 0o777, 0o701} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkPrivateDirectory(root); err == nil {
			t.Fatalf("directory mode %04o accepted; tenant roots must stay 0700", mode)
		}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDirectory(root); err != nil {
		t.Fatalf("0700 directory refused: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDirectory(link); err == nil {
		t.Fatal("symlinked directory accepted")
	}
	file := filepath.Join(root, "state.json")
	if err := os.WriteFile(file, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateFile(file); err == nil {
		t.Fatal("0640 state file accepted")
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateFile(file); err != nil {
		t.Fatalf("0600 state file refused: %v", err)
	}
}

func deploymentFile(t *testing.T, relative string) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(here)))
	data, err := os.ReadFile(filepath.Join(root, "deploy", "dshgw", relative))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The example must load as written: an example that fails strict decoding or
// validation is discovered only by an operator, on a host, at install time.
func TestConfigExampleLoadsAsWritten(t *testing.T) {
	example := deploymentFile(t, "config.example.yaml")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if cfg.Deploy.WorkerUser == "" {
		t.Fatal("the example leaves deploy.worker_user empty: every worker runs as one unprivileged account")
	}
	if cfg.Deploy.WorkerUser != cfg.Deploy.GatewayUser {
		t.Fatalf("example worker_user = %q, want the gateway account %q so no second account is needed", cfg.Deploy.WorkerUser, cfg.Deploy.GatewayUser)
	}
}

// The worker-node example (M77) must load as written for the same reason: it is copied onto a
// machine nobody is watching, and a strict-decoder complaint there is discovered at install
// time by an operator who has no idea which key is wrong.
func TestNodeExampleLoadsAsWritten(t *testing.T) {
	example := deploymentFile(t, "node.example.yaml")
	path := filepath.Join(t.TempDir(), "dshgw-node.yaml")
	if err := os.WriteFile(path, []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("node.example.yaml does not load: %v", err)
	}
	if !cfg.NodeMode() {
		t.Fatal("node.example.yaml must describe a worker node (its node: block is the switch)")
	}
	if cfg.Node.Name == "" || cfg.Node.Listen == "" {
		t.Fatalf("the example must name the node and the address it binds: %+v", cfg.Node)
	}
	// The example names a token file rather than an inline token (the secret belongs in a
	// 0600 file). It does not exist on a machine that has not been deployed yet, so resolution
	// fails — and that failure has to name the file, which is the operator's next step.
	if cfg.Node.TokenFile == "" && cfg.Node.Token == "" {
		t.Fatal("the example must name a token source")
	}
	if token, err := cfg.NodeSelfToken(); err == nil {
		t.Fatalf("a token file that does not exist yet must not resolve to a usable token (%q)", token)
	} else if !strings.Contains(err.Error(), cfg.Node.TokenFile) {
		t.Fatalf("the error must name the file to create: %v", err)
	}
	// A node carries none of the control plane's own surface.
	if len(cfg.Nodes) != 0 || cfg.DefaultNode != "" {
		t.Fatalf("a node example must not configure the control plane's node list: %+v", cfg.Nodes)
	}
	if cfg.AigwBaseURL == "" {
		t.Fatal("a node needs aigw_base_url: its workers call aigw directly")
	}
}

func TestServerAndLifecycleUseConfiguredSessionCapacity(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfg, []byte("directory_picker: browse\nstate_dir: "+filepath.Join(root, "state")+"\nmax_sessions: 1\n"+tenantPluginsOff), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &cli{configPath: cfg}
	deps, err := app.loadRuntime(true) // shared by serve and lifecycle commands
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.manager.Sessions.Issue("alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.manager.Sessions.Issue("bob", time.Hour); !errors.Is(err, session.ErrCapacity) {
		t.Fatalf("configured session limit ignored: %v", err)
	}
}

func TestDoctorRequiresAppArmorUserNamespaceRestriction(t *testing.T) {
	err := checkAppArmorUserNSRestriction()
	if err == nil {
		// The host restricts unprivileged user namespaces; nothing more to prove.
		return
	}
	if !strings.Contains(err.Error(), "apparmor_restrict_unprivileged_userns") {
		t.Fatalf("unexpected precondition error: %v", err)
	}
}

// The worker process runs the launcher as an ordinary account, so the launcher
// must not require root. This is a regression test for a launcher that would have
// failed every worker at start.
func TestSandboxExecRunsWithoutRoot(t *testing.T) {
	root := t.TempDir()
	cfg := deploymentConfigIn(t, root, "")
	app := &cli{configPath: cfg.Deploy.ConfigPath, stdout: io.Discard, stderr: io.Discard}
	err := app.sandboxExec([]string{"alice"})
	if err == nil {
		t.Fatal("launcher accepted an unknown tenant")
	}
	if strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("sandbox-exec requires root, which the worker process cannot provide: %v", err)
	}
	// Usage is still validated before anything else.
	if usageErr := app.sandboxExec(nil); usageErr == nil || !strings.Contains(usageErr.Error(), "usage: dshgw sandbox-exec") {
		t.Fatalf("missing usage validation: %v", usageErr)
	}
}

// sandbox-exec --print is the operator's profile review command, so it must work
// from configuration alone (no tenant directories, no running worker) and must
// refuse to run outside the bwrap mode.
func TestSandboxExecPrintRendersProfileFromConfiguration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the worker account must be unprivileged; root cannot stand in for it")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// A synthetic dsh installation: the profile resolves the release through
	// current_link, so these paths must exist even for a printed profile.
	mustWriteFile(t, filepath.Join(root, "dsh/node/bin/node"), "#!/bin/sh\n", 0o755)
	mustWriteFile(t, filepath.Join(root, "dsh/releases/r1/lib/bin.js"), "// dsh\n", 0o644)
	if err := os.Symlink(filepath.Join(root, "dsh/releases/r1"), filepath.Join(root, "dsh/current")); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "srv/alice")
	configPath := filepath.Join(root, "config.yaml")
	doc := "directory_picker: browse\n" +
		tenantPluginsOff +
		"state_dir: " + filepath.Join(root, "state") + "\n" +
		"tenant_root: " + filepath.Join(root, "state/tenants") + "\n" +
		"workspace_root: " + filepath.Join(root, "srv") + "\n" +
		"registry_path: " + filepath.Join(root, "registry.json") + "\n" +
		"key_map_path: " + filepath.Join(root, "keys.map") + "\n" +
		"dsh:\n  node_bin: " + filepath.Join(root, "dsh/node/bin/node") + "\n" +
		"  bin_js: " + filepath.Join(root, "dsh/releases/r1/lib/bin.js") + "\n" +
		"  current_link: " + filepath.Join(root, "dsh/current") + "\n" +
		"deploy:\n  worker_user: " + current.Username + "\n"
	mustWriteFile(t, configPath, doc, 0o600)
	registryDoc := fmt.Sprintf(`{"version":1,"tenants":[{"name":"alice","uid":%d,"public_port":32601,"worker_port":32100,`+
		`"key_prefix":"sk-aaaaaaaaa","dsh_home":%q,"workspace":%q,"created_at":"2025-01-01T00:00:00Z",`+
		`"handshake":"ok","isolation":"bwrap"}]}`, os.Getuid(), filepath.Join(root, "state/tenants/alice/.dsh"), workspace)
	mustWriteFile(t, filepath.Join(root, "registry.json"), registryDoc, 0o600)

	var stdout bytes.Buffer
	app := &cli{configPath: configPath, stdout: &stdout, stderr: io.Discard}
	if err := app.sandboxExec([]string{"--print", "alice"}); err != nil {
		t.Fatalf("profile print failed: %v", err)
	}
	// The profile is printed one argv element per line so it can be reviewed flag
	// by flag; the assertions work on the joined form.
	printed := strings.Join(strings.Fields(stdout.String()), " ")
	for _, want := range []string{"--tmpfs /home", "--unshare-pid", workspace, "--die-with-parent"} {
		if !strings.Contains(printed, want) {
			t.Errorf("printed profile is missing %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "--ro-bind / /") {
		t.Errorf("printed profile exposes the host root:\n%s", printed)
	}
}

// deploymentConfigIn writes a configuration the supervised shape expects under
// root, with extraDeploy appended to the deploy block.
// tenantPluginsOff is the fixture sentence that keeps a configuration free of plugin files: the
// picker that needs none, and the tenant-side plugins switched off. They are on by default because
// they ship beside deploy.plugin_path (M75), so a fixture that does not exercise them — and has no
// plugin directory in its temporary tree — has to say so.
const tenantPluginsOff = "tenant_plugins:\n  web_tty:\n    enabled: false\n  workspace_files:\n    enabled: false\n  git_diff:\n    enabled: false\n"

func deploymentConfigIn(t *testing.T, root, extraDeploy string) *config.Config {
	t.Helper()
	path := filepath.Join(root, "config.yaml")
	// directory_picker is pinned to browse because deploy.plugin_path has no default
	// (M63): a fixture that does not exercise the picker should not have to name a
	// plugin file that does not exist in its temporary tree.
	doc := "state_dir: " + filepath.Join(root, "state") + "\n" +
		"directory_picker: browse\n" +
		tenantPluginsOff +
		"deploy:\n" + extraDeploy
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &cli{configPath: path}
	deps, err := app.loadRuntime(true)
	if err != nil {
		t.Fatal(err)
	}
	return deps.cfg
}

func mustWriteFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

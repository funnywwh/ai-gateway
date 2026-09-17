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

func TestSharedRootTraversal(t *testing.T) {
	root := t.TempDir()
	for _, mode := range []os.FileMode{0o700, 0o750, 0o770, 0o777} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkSharedTraversal(root); err == nil {
			t.Fatalf("unsafe/unsearchable shared root %04o accepted", mode)
		}
	}
	for _, mode := range []os.FileMode{0o711, 0o751, 0o755} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkSharedTraversal(root); err != nil {
			t.Fatalf("shared root %04o refused: %v", mode, err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := checkSharedTraversal(link); err == nil {
		t.Fatal("symlink shared root accepted")
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

func TestInstallerGivesWorkersSearchWithoutGatewayMembership(t *testing.T) {
	text := deploymentFile(t, "install.sh")
	for _, want := range []string{
		"install -d -o root -g dshgw -m 0751 /var/lib/dshgw\n",
		"install -d -o root -g root -m 0711 /var/lib/dshgw/tenants\n",
		"install -d -o dshgw -g dshgw -m 0700 /var/lib/dshgw/gateway\n",
		"install -d -o root -g dshgw -m 0750 /var/lib/dshgw/handshake /etc/dshgw /etc/dshgw/tenants\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("installer missing shared/private permission contract: %s", want)
		}
	}
}

// The bwrap isolation mode ships as deployment artifacts. A missing unit in the
// installer or an undocumented config key is a silent deployment failure, so
// both are pinned here.
func TestInstallerAndConfigExampleShipBwrapIsolation(t *testing.T) {
	installer := deploymentFile(t, "install.sh")
	for _, want := range []string{
		`install -m 0644 "$ROOT/deploy/dshgw/dsh-worker-bwrap@.service" /etc/systemd/system/dsh-worker-bwrap@.service`,
	} {
		if !strings.Contains(installer, want) {
			t.Fatalf("installer does not install the bwrap worker unit: %s", want)
		}
	}
	example := deploymentFile(t, "config.example.yaml")
	for _, want := range []string{
		"isolation: user",
		"worker_user:",
		"bwrap_bin:",
		"worker_unit_bwrap:",
	} {
		if !strings.Contains(example, want) {
			t.Fatalf("config example does not document %s", want)
		}
	}
	// The example must load as written: an example that fails strict decoding or
	// validation would be discovered only by an operator.
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if mode, err := cfg.IsolationMode(); err != nil || mode != config.IsolationUser {
		t.Fatalf("example isolation mode = %q (%v), want the compatible default", mode, err)
	}
	if cfg.Deploy.WorkerUser != cfg.Deploy.GatewayUser {
		t.Fatalf("example worker_user = %q, want the gateway account %q", cfg.Deploy.WorkerUser, cfg.Deploy.GatewayUser)
	}
}

func TestServerAndLifecycleUseConfiguredSessionCapacity(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfg, []byte("state_dir: "+filepath.Join(root, "state")+"\nmax_sessions: 1\n"), 0o600); err != nil {
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

func TestGatewayNamespaceDoesNotPinAtomicStateFiles(t *testing.T) {
	unit := deploymentFile(t, "dshgw.service")
	var readOnly, readWrite []string
	for _, line := range strings.Split(unit, "\n") {
		if rest, ok := strings.CutPrefix(line, "ReadOnlyPaths="); ok {
			readOnly = append(readOnly, strings.Fields(rest)...)
		}
		if rest, ok := strings.CutPrefix(line, "ReadWritePaths="); ok {
			readWrite = append(readWrite, strings.Fields(rest)...)
		}
	}
	if strings.Join(readOnly, " ") != "/etc/dshgw /var/lib/dshgw" {
		t.Fatalf("state must be mounted by directory, not pinned per file: %v", readOnly)
	}
	if len(readWrite) != 1 || readWrite[0] != "/var/lib/dshgw/gateway" {
		t.Fatalf("only gateway mutable state should be writable: %v", readWrite)
	}
	if !strings.Contains(unit, "ProtectSystem=strict") {
		t.Fatal("filesystem protection was removed instead of fixing mount granularity")
	}
}

// The bwrap worker unit is installed verbatim (systemd expands %i), so its
// identity lines are the deployment's only record of how a tenant worker runs.
// These tests pin that contract and the cross-check that keeps it honest.
func TestBwrapWorkerUnitIsAStaticTemplate(t *testing.T) {
	unit := deploymentFile(t, "dsh-worker-bwrap@.service")
	if strings.Contains(unit, "{{") {
		t.Fatalf("bwrap unit carries unrendered placeholders; systemd would reject it:\n%s", unit)
	}
	for _, want := range []string{
		"User=dshgw\n",
		"Group=dshgw\n",
		"ExecStart=/opt/dshgw/bin/dshgw --config /etc/dshgw/config.yaml sandbox-exec %i\n",
		"EnvironmentFile=/etc/dshgw/tenants/%i/tenant.env\n",
		"Slice=dsh-workers.slice\n",
		"NoNewPrivileges=yes\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("bwrap unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, `ExecStart=/opt/dsh/node/bin/node`) {
		t.Fatal("bwrap unit starts node directly instead of the sandbox launcher")
	}
}

func TestDoctorCrossChecksBwrapUnitAgainstConfiguration(t *testing.T) {
	root := t.TempDir()
	cfg := deploymentConfigIn(t, root, "")
	unit := deploymentFile(t, "dsh-worker-bwrap@.service")
	if err := os.WriteFile(filepath.Join(cfg.Deploy.SystemdDir, cfg.Deploy.WorkerUnitBwrap), []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBwrapWorkerUnit(cfg); err != nil {
		t.Fatalf("default deployment rejected its own unit: %v", err)
	}
	// A configuration that names another worker account must be reported: the
	// unit is static, so nothing else would notice the drift. config.Config
	// carries a mutex and must never be copied by value, so the drifted state
	// is loaded from its own file.
	drifted := deploymentConfigIn(t, root, "  worker_user: otherworker\n")
	if err := checkBwrapWorkerUnit(drifted); err == nil || !strings.Contains(err.Error(), "User=") {
		t.Fatalf("worker_user drift not reported: %v", err)
	}
	if err := os.Remove(filepath.Join(cfg.Deploy.SystemdDir, cfg.Deploy.WorkerUnitBwrap)); err != nil {
		t.Fatal(err)
	}
	if err := checkBwrapWorkerUnit(cfg); err == nil {
		t.Fatal("missing bwrap unit accepted")
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

// deploymentConfigIn builds the configuration the examples and defaults
// describe under root, with extraDeploy appended to the deploy block, so
// filesystem checks can run offline and two variants can share one systemd
// directory.
func deploymentConfigIn(t *testing.T, root, extraDeploy string) *config.Config {
	t.Helper()
	path := filepath.Join(root, "config.yaml")
	doc := "state_dir: " + filepath.Join(root, "state") + "\n" +
		"deploy:\n  systemd_dir: " + filepath.Join(root, "systemd") + "\n  isolation: bwrap\n" + extraDeploy
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &cli{configPath: path}
	deps, err := app.loadRuntime(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deps.cfg.Deploy.SystemdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return deps.cfg
}

// systemd runs the bwrap unit's ExecStart as the shared unprivileged worker
// account, so the launcher must not require root. This is a regression test for
// a launcher that would have failed every bwrap worker at start.
func TestSandboxExecRunsWithoutRoot(t *testing.T) {
	root := t.TempDir()
	cfg := deploymentConfigIn(t, root, "")
	app := &cli{configPath: cfg.Deploy.ConfigPath, stdout: io.Discard, stderr: io.Discard}
	err := app.sandboxExec([]string{"alice"})
	if err == nil {
		t.Fatal("launcher accepted an unknown tenant")
	}
	if strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("sandbox-exec requires root, which the worker unit cannot provide: %v", err)
	}
	// Usage is still validated before anything else.
	if usageErr := app.sandboxExec(nil); usageErr == nil || !strings.Contains(usageErr.Error(), "usage: dshgw sandbox-exec") {
		t.Fatalf("missing usage validation: %v", usageErr)
	}
}

// sandbox-exec --print is the operator's profile review command before any
// bwrap tenant is created, so it must work from configuration alone (no tenant
// directories, no systemd) and must refuse to run outside the bwrap mode.
func TestSandboxExecPrintRendersProfileFromConfiguration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the shared worker account must be unprivileged; root cannot stand in for it")
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
	doc := "state_dir: " + filepath.Join(root, "state") + "\n" +
		"tenant_root: " + filepath.Join(root, "state/tenants") + "\n" +
		"workspace_root: " + filepath.Join(root, "srv") + "\n" +
		"registry_path: " + filepath.Join(root, "registry.json") + "\n" +
		"key_map_path: " + filepath.Join(root, "keys.map") + "\n" +
		"dsh:\n  node_bin: " + filepath.Join(root, "dsh/node/bin/node") + "\n" +
		"  bin_js: " + filepath.Join(root, "dsh/releases/r1/lib/bin.js") + "\n" +
		"  current_link: " + filepath.Join(root, "dsh/current") + "\n" +
		"deploy:\n  isolation: bwrap\n  worker_user: " + current.Username + "\n" +
		"  systemd_dir: " + filepath.Join(root, "systemd") + "\n"
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
	// The profile is printed one argv element per line so it can be reviewed
	// flag by flag; the assertions work on the joined form.
	printed := strings.Join(strings.Fields(stdout.String()), " ")
	for _, want := range []string{"--tmpfs /home", "--unshare-pid", workspace, "--die-with-parent"} {
		if !strings.Contains(printed, want) {
			t.Errorf("printed profile is missing %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "--ro-bind / /") {
		t.Errorf("printed profile exposes the host root:\n%s", printed)
	}
	// A per-tenant-account deployment has no sandbox to launch.
	if err := os.WriteFile(configPath, []byte(strings.Replace(doc, "isolation: bwrap", "isolation: user", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.sandboxExec([]string{"--print", "alice"}); err == nil || !strings.Contains(err.Error(), "requires deploy.isolation: bwrap") {
		t.Fatalf("sandbox-exec ran outside the bwrap mode: %v", err)
	}
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

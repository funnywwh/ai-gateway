package tenancy

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func renderFixture(t *testing.T) (*config.Config, registry.Tenant) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{EdgePortHeader: "X-DSHGW-Port", AigwBaseURL: "http://aigw:8088", DirectoryPicker: "clamp", PluginBrowserFS: "on", WorkspaceSeed: []string{"work", "projects"}, Dsh: config.DshRuntime{CurrentLink: "/opt/dsh/current"}, Deploy: config.DeployConfig{PluginPath: "/opt/dshgw/share/dsh-plugin/picker-clamp.js", TenantConfigRoot: filepath.Join(root, "etc")}, PublicHost: "dsh.example", PortalPort: 32600, Listen: "127.0.0.1:3099"}
	tenant := registry.Tenant{Name: "alice", WorkerPort: 32100, PublicPort: 32601, DshHome: filepath.Join(root, "state/alice/.dsh"), Workspace: filepath.Join(root, "srv/alice")}
	return cfg, tenant
}
func TestTenantArtifactsCredentialsPatchAndWorkspace(t *testing.T) {
	cfg, tenant := renderFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", []string{"z", "a"}, TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	byBase := map[string]Artifact{}
	for _, a := range arts {
		byBase[filepath.Base(a.Path)] = a
	}
	var credentials map[string]any
	if err := yaml.Unmarshal(byBase[".credentials.yaml"].Data, &credentials); err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 3 || credentials["version"] != 1 {
		t.Fatalf("credentials=%v", credentials)
	}
	patch := string(byBase["cordis.patch.yml"].Data)
	for _, want := range []string{"directory-picker-auto", "picker-clamp.js", "dsh-client-ui-directory-picker-browse", "root: " + tenant.Workspace} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch missing %q:\n%s", want, patch)
		}
	}
	var workspace map[string]any
	if err := json.Unmarshal(byBase["workspace.json"].Data, &workspace); err != nil {
		t.Fatal(err)
	}
	global := workspace["global"].(map[string]any)
	if len(global["workspaceIds"].([]any)) != 2 {
		t.Fatalf("workspace=%v", workspace)
	}
	if byBase["gateway.key"].Mode != 0o640 || byBase[".credentials.yaml"].Mode != 0o600 {
		t.Fatal("secret modes wrong")
	}
}
func TestBrowserFSOffAndBrowsePicker(t *testing.T) {
	cfg, tenant := renderFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "key", nil, TenantOptions{DirectoryPicker: "browse", PluginBrowserFS: "off"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var patch string
	for _, a := range arts {
		if filepath.Base(a.Path) == "cordis.patch.yml" {
			patch = string(a.Data)
		}
	}
	if !strings.Contains(patch, "dsh-host-directory-picker-browse") || !strings.Contains(patch, "browser-fs") || !strings.Contains(patch, "disabled: true") {
		t.Fatalf("%s", patch)
	}
	if strings.Contains(patch, "picker-clamp.js") {
		t.Fatal("browse mode loaded clamp")
	}
}
func TestRotateCredentialsPreservesRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".credentials.yaml")
	if err := os.WriteFile(p, []byte("version: 1\nrefs:\n  AIGW_API_KEY: old\nrecords:\n  browser:\n    token: keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RotateCredentials(p, "new"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "AIGW_API_KEY: new") || !strings.Contains(string(data), "token: keep") {
		t.Fatalf("%s", data)
	}
}
func TestRenderSettingsPreservesOtherProvidersAndDropsEmptyAigw(t *testing.T) {
	existing := []byte("custom: keep\nllm-pi-ai:\n  extra: keep\n  providers:\n    other:\n      enabled: true\n    aigw:\n      models: [{id: old}]\nagent-default-model:\n  provider: aigw\n  model: old\n")
	updated, err := renderSettings(existing, "http://aigw", []string{"new"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, want := range []string{"custom: keep", "other:", "extra: keep", "model: new", "compat:", "supportsStrictMode: true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	empty, err := renderSettings(updated, "http://aigw", nil)
	if err != nil {
		t.Fatal(err)
	}
	text = string(empty)
	if strings.Contains(text, "AIGW_API_KEY") || strings.Contains(text, "agent-default-model") || !strings.Contains(text, "other:") {
		t.Fatalf("empty models rendered invalid provider:\n%s", text)
	}
}

// The plugin row is what makes the feature reachable at all: without it the account's dsh has
// no surface, and with a wrong path dsh fails to boot (the profile imports the file by
// absolute path).
func TestRenderPatchAddsTheSSHWorkspacePluginWhenEnabled(t *testing.T) {
	cfg, tenant := renderFixture(t)
	disabled, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", []string{"m"}, TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	patchOf := func(arts []Artifact) string {
		for _, artifact := range arts {
			if filepath.Base(artifact.Path) == "cordis.patch.yml" {
				return string(artifact.Data)
			}
		}
		t.Fatal("no patch artifact")
		return ""
	}
	if strings.Contains(patchOf(disabled), "ssh-workspace") {
		t.Fatalf("a disabled feature rendered a plugin row:\n%s", patchOf(disabled))
	}

	cfg.SSHWorkspaces.Enabled = true
	cfg.SSHWorkspaces.MountSubdir = "ssh"
	cfg.SSHWorkspaces.Hosts = []string{"gpt001"}
	cfg.SSHWorkspaces.MaxEntries = 25
	cfg.SSHWorkspaces.ConnectTimeout = config.Duration(9000 * time.Millisecond)
	enabled, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", []string{"m"}, TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	patch := patchOf(enabled)
	// The plugin lives beside the picker plugin, which is the directory the sandbox already
	// binds read-only.
	want := "file:///opt/dshgw/share/dsh-plugin/ssh-workspace/index.js"
	if !strings.Contains(patch, want) {
		t.Errorf("the patch does not name the ssh workspace plugin %q:\n%s", want, patch)
	}
	for _, fragment := range []string{"id: ssh-workspace", "mountSubdir: ssh", "gpt001", "maxEntries: 25", "connectTimeoutMs: 9000"} {
		if !strings.Contains(patch, fragment) {
			t.Errorf("the patch lacks %q:\n%s", fragment, patch)
		}
	}
	// The picker keeps working exactly as before: the ssh row is an addition, not a swap.
	if !strings.Contains(patch, "picker-clamp.js") {
		t.Errorf("the picker row disappeared:\n%s", patch)
	}
}

// Enabling the feature must reach accounts that already exist: the artifacts are otherwise
// written only at create/rotate time, so without this a switch that is on would show a UI on
// new tenants and nothing on the ones people actually use. The refresh must touch the patch
// only — rewriting the artifacts would discard the workspaces someone added in the UI.
func TestEnsureSSHWorkspaceRowFollowsTheSwitch(t *testing.T) {
	cfg, tenant := renderFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", []string{"m"}, TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifacts(arts); err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")
	workspacePath := filepath.Join(tenant.DshHome, "storages", "workspace.json")
	// A workspace added through the UI is what a careless re-render would destroy.
	workspaceBefore, err := os.ReadFile(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(workspacePath, append(workspaceBefore, []byte("\n# ui-added\n")...), 0o600)
	uiWorkspace, _ := os.ReadFile(workspacePath)

	// Off: the rendered patch has no row, and a refresh is a no-op.
	if _, err := EnsureSSHWorkspaceRow(cfg, tenant); err != nil {
		t.Fatalf("refresh with the feature off: %v", err)
	}
	if data, _ := os.ReadFile(patchPath); strings.Contains(string(data), "ssh-workspace") {
		t.Fatalf("a disabled feature added a row:\n%s", data)
	}

	// On: the row appears on a tenant that was provisioned before the switch existed.
	cfg.SSHWorkspaces.Enabled = true
	cfg.SSHWorkspaces.Hosts = []string{"gpt001"}
	cfg.SSHWorkspaces.MountSubdir = "ssh"
	if _, err := EnsureSSHWorkspaceRow(cfg, tenant); err != nil {
		t.Fatalf("refresh with the feature on: %v", err)
	}
	patchAfter, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ssh-workspace", "gpt001", "picker-clamp.js"} {
		if !strings.Contains(string(patchAfter), want) {
			t.Errorf("refreshed patch lacks %q:\n%s", want, patchAfter)
		}
	}
	// Refreshing twice must not stack rows.
	if _, err := EnsureSSHWorkspaceRow(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(patchPath)
	if strings.Count(string(again), "ssh-workspace") != strings.Count(string(patchAfter), "ssh-workspace") {
		t.Errorf("a second refresh stacked rows:\n%s", again)
	}
	// A changed configuration propagates.
	cfg.SSHWorkspaces.Hosts = []string{"aipc"}
	if _, err := EnsureSSHWorkspaceRow(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	changed, _ := os.ReadFile(patchPath)
	if strings.Contains(string(changed), "gpt001") || !strings.Contains(string(changed), "aipc") {
		t.Errorf("the refresh did not carry the new host list:\n%s", changed)
	}
	// And the workspaces someone added are untouched.
	if after, _ := os.ReadFile(workspacePath); string(after) != string(uiWorkspace) {
		t.Errorf("the refresh rewrote workspace.json:\n%s", after)
	}

	// Switching it off removes the row again.
	cfg.SSHWorkspaces.Enabled = false
	if _, err := EnsureSSHWorkspaceRow(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(patchPath); strings.Contains(string(data), "ssh-workspace") {
		t.Errorf("the row survived the switch being turned off:\n%s", data)
	}
}

// A tenant whose patch has no insert list (an older shape) is warned about — never blocked:
// an optional feature must not stop an account from starting.
func TestEnsureSSHWorkspaceRowReportsAnUnpatchableTenant(t *testing.T) {
	cfg, tenant := renderFixture(t)
	cfg.SSHWorkspaces.Enabled = true
	patchPath := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")
	if err := os.MkdirAll(filepath.Dir(patchPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patchPath, []byte("- id: directory-picker\n  disabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	warning, err := EnsureSSHWorkspaceRow(cfg, tenant)
	if err != nil {
		t.Fatalf("an unpatchable tenant produced an error instead of a warning: %v", err)
	}
	if warning == "" {
		t.Fatal("an unpatchable tenant was accepted silently")
	}
	// The caller logs that warning through the manager's own logger, which is nil for several
	// entry points; a nil logger must never turn a warning into a panic (it did: every worker
	// start panicked on this host until the live acceptance caught it).
	bare := &Manager{}
	bare.log().Warn(warning)
	// A tenant that was never provisioned is skipped instead: there is nothing to patch yet.
	other := registry.Tenant{Name: "bob", DshHome: filepath.Join(t.TempDir(), "bob/.dsh")}
	if warning, err := EnsureSSHWorkspaceRow(cfg, other); err != nil || warning != "" {
		t.Fatalf("an unprovisioned tenant was not skipped: %q / %v", warning, err)
	}
}

// A writer that died while holding the lock must not block an account forever: the lock file
// records its owner precisely so a later start can tell the difference between "someone is
// working" and "nobody is coming back". Found on the deployment host, where a crashed gateway
// left a lock behind and the affected account could no longer start.
func TestDSHFileLockReclaimsAStaleOwner(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "settings.yaml")
	lockPath := target + ".lock"
	// A pid that is certainly gone: start a process and let it exit.
	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	pid := dead.Process.Pid
	_ = dead.Wait()
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := withDSHFileLock(target, func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("a stale lock was not reclaimed: %v", err)
	}
	if !ran {
		t.Fatal("the operation did not run")
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock survived the operation: %v", err)
	}
	// A live owner is respected: this process holds its own lock, so a second attempt must
	// wait rather than steal it.
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := withDSHFileLockTimeout(target, 200*time.Millisecond, func() error { return nil }); err == nil {
		t.Fatal("a lock held by a live process was stolen")
	}
}

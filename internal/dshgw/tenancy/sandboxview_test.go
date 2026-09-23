package tenancy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"gopkg.in/yaml.v3"
)

// M79: deploy.sandbox_workspace gives every tenant a short path for its workspace. Everything a
// tenant reads has to follow it — HOME, the passwd entry, the picker root, the terminal's cwd, the
// two workspace panels' roots, the seeded workspaces — while the gateway's own state keeps the
// host path it already wrote (registry records, mount records, backup roots).

func TestSandboxWorkspacePathFollowsTheConfiguredView(t *testing.T) {
	cfg, tenant := renderFixture(t)
	if got := sandboxWorkspacePath(cfg, tenant); got != tenant.Workspace {
		t.Fatalf("no view: %q, want the host workspace %q", got, tenant.Workspace)
	}
	cfg.Deploy.SandboxWorkspace = "/workspace"
	if got := sandboxWorkspacePath(cfg, tenant); got != "/workspace" {
		t.Fatalf("view set: %q, want /workspace", got)
	}
	// Whitespace is not a view: the profile treats empty as "one view only", and a blank value
	// would otherwise be handed to bubblewrap as a mount target.
	cfg.Deploy.SandboxWorkspace = "  "
	if got := sandboxWorkspacePath(cfg, tenant); got != tenant.Workspace {
		t.Fatalf("blank view: %q, want the host workspace", got)
	}
}

func TestWorkerEnvHomeFollowsTheView(t *testing.T) {
	cfg, tenant := renderFixture(t)
	cfg.Deploy.SandboxWorkspace = "/workspace"
	env := workerEnv(cfg, tenant)
	if !envHas(env, "HOME=/workspace") {
		t.Fatalf("HOME did not follow the view: %v", env)
	}
	// DSH_HOME is the gateway's own state directory: the view is about the workspace a person
	// works in, and moving the state path would only rename it without shortening anything a
	// tenant reads (M79 scope).
	if !envHas(env, "DSH_HOME="+tenant.DshHome) {
		t.Fatalf("DSH_HOME moved with the view: %v", env)
	}
}

func envHas(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func TestTenantRowsFollowTheWorkspaceView(t *testing.T) {
	cfg, tenant := renderFixture(t)
	cfg.Deploy.SandboxWorkspace = "/workspace"
	for _, row := range []map[string]any{
		webTTYRow(cfg, tenant),
		workspaceFilesRow(cfg, tenant),
		gitDiffRow(cfg, tenant),
	} {
		options, _ := row["config"].(map[string]any)
		for _, key := range []string{"cwd", "cwdRoot", "root"} {
			value, ok := options[key]
			if !ok {
				continue
			}
			if value != "/workspace" {
				t.Errorf("row %v: %s = %v, want the workspace view", row["id"], key, value)
			}
		}
	}
	// Without a view the same rows keep naming the host workspace path.
	plainCfg, plainTenant := renderFixture(t)
	for _, row := range []map[string]any{
		webTTYRow(plainCfg, plainTenant),
		workspaceFilesRow(plainCfg, plainTenant),
		gitDiffRow(plainCfg, plainTenant),
	} {
		options, _ := row["config"].(map[string]any)
		for _, key := range []string{"cwd", "cwdRoot", "root"} {
			value, ok := options[key]
			if !ok {
				continue
			}
			if value != plainTenant.Workspace {
				t.Errorf("row %v without a view: %s = %v, want %s", row["id"], key, value, plainTenant.Workspace)
			}
		}
	}
}

func TestRenderedArtifactsFollowTheWorkspaceView(t *testing.T) {
	cfg, tenant := renderFixture(t)
	cfg.Deploy.SandboxWorkspace = "/workspace"
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", models("m"), TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	byBase := map[string]Artifact{}
	for _, a := range arts {
		byBase[filepath.Base(a.Path)] = a
	}
	if patch := string(byBase["cordis.patch.yml"].Data); !strings.Contains(patch, "root: /workspace") {
		t.Errorf("the picker row does not root at the view:\n%s", patch)
	}
	// Seeded workspaces are the ones a fresh account opens first.
	var workspace struct {
		Global struct {
			WorkspaceIDs []string `json:"workspaceIds"`
		} `json:"global"`
		Tables struct {
			Workspaces map[string]struct {
				Path string `json:"path"`
			} `json:"workspaces"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(byBase["workspace.json"].Data, &workspace); err != nil {
		t.Fatal(err)
	}
	if len(workspace.Global.WorkspaceIDs) != 2 {
		t.Fatalf("seeded workspaces = %v", workspace.Global.WorkspaceIDs)
	}
	for _, id := range workspace.Global.WorkspaceIDs {
		if path := workspace.Tables.Workspaces[id].Path; path != "/workspace/work" && path != "/workspace/projects" {
			t.Errorf("seeded workspace %s = %q, want it under /workspace", id, path)
		}
	}
}

// The picker row is written only when a tenant is created or its key rotates, so without a
// start-time refresh a deployment that starts naming a view would keep offering the long host
// path to every account that already exists — and the picker is exactly where the path a session
// runs in is chosen.
func TestEnsureDirectoryPickerRowFollowsTheWorkspaceView(t *testing.T) {
	cfg, tenant := renderFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", models("m"), TenantOptions{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifacts(arts); err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")
	workspacePath := filepath.Join(tenant.DshHome, "storages", "workspace.json")
	workspaceBefore, err := os.ReadFile(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	idsBefore := patchRowIDs(t, patchPath)
	if root := patchPickerRoot(t, patchPath); root != tenant.Workspace {
		t.Fatalf("fixture picker root = %q, want the host workspace %q", root, tenant.Workspace)
	}

	cfg.Deploy.SandboxWorkspace = "/workspace"
	if warning, err := EnsureDirectoryPickerRow(cfg, tenant); err != nil || warning != "" {
		t.Fatalf("refresh: warning=%q err=%v", warning, err)
	}
	if root := patchPickerRoot(t, patchPath); root != "/workspace" {
		t.Errorf("picker root = %q, want the view", root)
	}
	idsAfter := patchRowIDs(t, patchPath)
	if strings.Join(idsBefore, " ") != strings.Join(idsAfter, " ") {
		t.Errorf("the refresh changed the row set: %v -> %v", idsBefore, idsAfter)
	}
	// Refreshing twice must not stack the pair.
	if _, err := EnsureDirectoryPickerRow(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	if again := patchRowIDs(t, patchPath); strings.Join(again, " ") != strings.Join(idsAfter, " ") {
		t.Errorf("a second refresh stacked rows: %v -> %v", idsAfter, again)
	}
	// The refresh touches the patch only: re-rendering the artifacts would discard the
	// workspaces a person added in the UI.
	if after, _ := os.ReadFile(workspacePath); string(after) != string(workspaceBefore) {
		t.Error("the refresh rewrote workspace.json")
	}

	// A changed picker mode replaces the pair instead of leaving both behind.
	cfg.DirectoryPicker = "browse"
	if _, err := EnsureDirectoryPickerRow(cfg, tenant); err != nil {
		t.Fatal(err)
	}
	ids := patchRowIDs(t, patchPath)
	for _, id := range ids {
		if strings.HasPrefix(id, "picker-clamp") {
			t.Errorf("the clamp row survived a switch to browse: %v", ids)
		}
	}
	if len(ids) == 0 || ids[0] != "picker-browse" || ids[1] != "picker-browse-ui" {
		t.Errorf("the picker pair is not at the head of the insert list: %v", ids)
	}
}

func TestEnsureDirectoryPickerRowSkipsAndWarnsInsteadOfBlocking(t *testing.T) {
	cfg, tenant := renderFixture(t)
	cfg.Deploy.SandboxWorkspace = "/workspace"
	// A tenant that was never provisioned has no patch yet: nothing to do, no error.
	unprovisioned := registry.Tenant{Name: "bob", DshHome: filepath.Join(t.TempDir(), "bob/.dsh")}
	if warning, err := EnsureDirectoryPickerRow(cfg, unprovisioned); err != nil || warning != "" {
		t.Fatalf("an unprovisioned tenant was not skipped: %q / %v", warning, err)
	}
	// A patch from an older shape has no insert list: warn, never block the start.
	patchPath := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")
	if err := os.MkdirAll(filepath.Dir(patchPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patchPath, []byte("- id: directory-picker\n  disabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	warning, err := EnsureDirectoryPickerRow(cfg, tenant)
	if err != nil {
		t.Fatalf("an unpatchable tenant produced an error instead of a warning: %v", err)
	}
	if warning == "" {
		t.Fatal("an unpatchable tenant was accepted silently")
	}
}

// patchRowIDs returns the ids of a patch's insert rows, in order.
func patchRowIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := yaml.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, row := range rows {
		insert, ok := row["insert"].([]any)
		if !ok {
			continue
		}
		for _, entry := range insert {
			record, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := record["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// patchPickerRoot returns the clamp picker's configured root, or "" when the row is absent.
func patchPickerRoot(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := yaml.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		insert, ok := row["insert"].([]any)
		if !ok {
			continue
		}
		for _, entry := range insert {
			record, ok := entry.(map[string]any)
			if !ok || record["id"] != pickerClampID {
				continue
			}
			options, _ := record["config"].(map[string]any)
			root, _ := options["root"].(string)
			return root
		}
	}
	return ""
}

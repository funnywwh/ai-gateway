package tenancy

import (
	"encoding/json"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func renderFixture(t *testing.T) (*config.Config, registry.Tenant) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{EdgePortHeader: "X-DSHGW-Port", AigwBaseURL: "http://aigw:8088", DirectoryPicker: "clamp", PluginBrowserFS: "on", WorkspaceSeed: []string{"work", "projects"}, Dsh: config.DshRuntime{CurrentLink: "/opt/dsh/current"}, TLS: config.TLSConfig{Certificate: "/cert/full.pem", CertificateKey: "/cert/key.pem"}, Deploy: config.DeployConfig{PluginPath: "/opt/dshgw/share/dsh-plugin/picker-clamp.js", TenantConfigRoot: filepath.Join(root, "etc"), PublicListen: "0.0.0.0"}, PublicHost: "dsh.example", PortalPort: 32600, Listen: "127.0.0.1:3099"}
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

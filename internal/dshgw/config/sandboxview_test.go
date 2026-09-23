package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// M79: deploy.sandbox_workspace names the short path every tenant's workspace is also visible at
// inside its sandbox. The path itself needs nothing on the host — bubblewrap creates it under its
// own empty tmpfs root — so what matters is what it must not name: a tree the sandbox mounts for
// itself, or a path this deployment already uses for state.
func TestSandboxWorkspaceViewLoadsAndRejectsUnusableValues(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	plugin := filepath.Join(root, "plugin", "picker-clamp.js")
	doc := func(view string) string {
		return "public_host: dsh.example.test\n" +
			"state_dir: " + stateDir + "\n" +
			"deploy:\n  plugin_path: " + plugin + "\n  sandbox_workspace: " + view + "\n"
	}
	for _, view := range []string{"/workspace", "/dsh-view/work", "/opt/tenant-view"} {
		cfg, err := Load(writeConfig(t, doc(view)))
		if err != nil {
			t.Errorf("view %q was rejected: %v", view, err)
			continue
		}
		if cfg.Deploy.SandboxWorkspace != view {
			t.Errorf("view %q was not kept: %q", view, cfg.Deploy.SandboxWorkspace)
		}
	}
	for _, tc := range []struct{ view, why string }{
		{"/", "the filesystem root"},
		{"/usr", "a tree the profile mounts for itself"},
		{"/usr/local/ws", "inside such a tree"},
		{"/etc/passwd", "a file the profile mounts itself"},
		{"/home/winger/ws", "inside a hidden root"},
		{"/tmp/ws", "inside a hidden root"},
		{"workspace", "relative"},
		{"/workspace/", "not a clean path"},
		{"/workspace/../ws", "not a clean path"},
	} {
		_, err := Load(writeConfig(t, doc(tc.view)))
		if err == nil {
			t.Errorf("view %q (%s) was accepted", tc.view, tc.why)
			continue
		}
		// The operator has to be able to tell which key is wrong from the message alone.
		if !strings.Contains(err.Error(), "sandbox_workspace") {
			t.Errorf("view %q (%s): the error does not name the key: %v", tc.view, tc.why, err)
		}
	}
	// Unset is the shape every deployment had before this option existed.
	cfg, err := Load(writeConfig(t, "public_host: dsh.example.test\nstate_dir: "+stateDir+"\ndeploy:\n  plugin_path: "+plugin+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.SandboxWorkspace != "" {
		t.Fatalf("sandbox_workspace defaulted to %q, want empty (the option is off)", cfg.Deploy.SandboxWorkspace)
	}
}

// The deployment's own paths are the part only this package knows: the sandbox package refuses
// the runtime and hidden trees, but it has never heard of state_dir, tenant_root or the plugin
// directory. None of them may be shadowed by a workspace bind, in either direction.
func TestSandboxWorkspaceViewMustNotOverlapDeploymentPaths(t *testing.T) {
	base := &Config{
		StateDir:      "/data/dshgw/state",
		WorkspaceRoot: "/data/dshgw/state/workspaces",
		TenantRoot:    "/data/dshgw/state/tenants",
		Deploy: DeployConfig{
			PluginPath: "/opt/dshgw/share/dsh-plugin/picker-clamp.js",
		},
	}
	for _, view := range []string{"/workspace", "/data-view", "/opt/tenant-view"} {
		candidate := *base
		candidate.Deploy.SandboxWorkspace = view
		if err := candidate.validateSandboxWorkspace(); err != nil {
			t.Errorf("view %q was rejected: %v", view, err)
		}
	}
	for _, tc := range []struct{ view, why string }{
		{"/data/dshgw/state/workspaces", "the workspace root itself"},
		{"/data/dshgw/state/workspaces/alice", "inside the workspace root"},
		{"/data/dshgw/state", "the state directory"},
		{"/data/dshgw/state/tenants/alice/.dsh", "a tenant's dsh home"},
		{"/data/dshgw", "an ancestor of both roots"},
		{"/data", "a much wider ancestor"},
		{"/opt/dshgw/share/dsh-plugin", "the plugin directory"},
		{"/opt/dshgw", "an ancestor of the plugin directory"},
	} {
		candidate := *base
		candidate.Deploy.SandboxWorkspace = tc.view
		err := candidate.validateSandboxWorkspace()
		if err == nil {
			t.Errorf("view %q (%s) was accepted", tc.view, tc.why)
			continue
		}
		if !strings.Contains(err.Error(), "sandbox_workspace") {
			t.Errorf("view %q (%s): the error does not name the key: %v", tc.view, tc.why, err)
		}
	}
	// A deployment that names no plugin (directory_picker: browse) must not trip over an empty
	// directory name, which filepath.Dir("") turns into ".".
	browse := *base
	browse.Deploy = DeployConfig{SandboxWorkspace: "/workspace"}
	if err := browse.validateSandboxWorkspace(); err != nil {
		t.Errorf("a deployment without a plugin rejected a valid view: %v", err)
	}
}

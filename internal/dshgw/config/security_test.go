package config

import "testing"

func TestRejectMalformedEdgeAndDeploymentConfiguration(t *testing.T) {
	for _, body := range []string{
		"edge_port_header: Origin\n",
		"edge_port_header: X-Real-IP\n",
		"edge_port_header: X-Forwarded-Port\n",
		"edge_port_header: 'X-;break'\n",
		"edge_port_header: 'X-端口'\n",
		"listen: 127.0.0.1:0\n",
		"listen: 127.0.0.1:32100\n",
		"listen: 127.0.0.1:32600\n",
		"listen: 127.0.0.1:32601\n",
		"listen: 127.0.0.1:http\n",
		"tenant_root: /\n",
		"workspace_root: /\n",
		"state_dir: /\n",
		"state_dir: /tmp/../etc\n",
		"deploy:\n  gateway_user: 'root;bad'\n",
		"deploy:\n  dsh_user_prefix: prefix-too-long\n",
		"deploy:\n  bwrap_bin: ../../usr/bin/bwrap\n",
		"deploy:\n  worker_unit: dsh-worker.service\n",
		"deploy:\n  nginx_include_path: /etc/nginx/conf.d/dshgw/recursive.conf\n",
		"key_revalidate: interval:9223372036854775807\n",
		// The clamp picker imports a plugin by absolute file URL; without a path the tenant
		// profile would carry a file:// URL pointing nowhere (M63).
		"directory_picker: clamp\n",
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("unsafe config accepted: %s", body)
		}
	}
	// A relative plugin path is accepted and resolved against the deployment root: this
	// repository ships the picker at cmd/dshgw/plugin/picker-clamp.js, and a deployment
	// should be able to name it without hard-coding its own absolute prefix (M63).
	if _, err := Load(writeConfig(t, "edge_port_header: X-Tenant-Port\ndeploy:\n  plugin_path: ./cmd/dshgw/plugin/picker-clamp.js\n")); err != nil {
		t.Fatalf("safe custom header rejected: %v", err)
	}
	// The picker that needs no plugin is still a valid configuration without one — provided the
	// tenant-side plugins, whose rows name files beside deploy.plugin_path, are switched off
	// (M75). This is the whole shape of a deployment that ships no plugin directory at all.
	if _, err := Load(writeConfig(t, "directory_picker: browse\ntenant_plugins:\n  web_tty:\n    enabled: false\n  workspace_files:\n    enabled: false\n  git_diff:\n    enabled: false\n")); err != nil {
		t.Fatalf("browse picker with the tenant plugins off rejected: %v", err)
	}
	// The same configuration with those plugins left at their default (on) is refused rather than
	// started: their rows would point at a directory this deployment does not have.
	if _, err := Load(writeConfig(t, "directory_picker: browse\n")); err == nil {
		t.Error("a deployment with no plugin directory accepted the default-on tenant plugins")
	}
}

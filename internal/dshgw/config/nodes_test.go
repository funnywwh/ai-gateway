package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseBody is the minimum a configuration must say in this repository: a public host, a data
// root and the picker plugin (which has no default).
func baseBody(root string) string {
	return "public_host: dsh.example.test\nstate_dir: " + filepath.Join(root, "state") + "\n" +
		"deploy:\n  plugin_path: " + filepath.Join(root, "picker-clamp.js") + "\n"
}

// TestSingleMachineStillNeedsNoNodeConfig is the compatibility anchor for M77: a deployment
// that says nothing about nodes keeps today's behaviour, including the data root default.
func TestSingleMachineStillNeedsNoNodeConfig(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(writeConfig(t, "public_host: dsh.example.test\ndeploy:\n  plugin_path: "+filepath.Join(root, "picker-clamp.js")+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeMode() {
		t.Fatal("a configuration without a node block is not a worker node")
	}
	if len(cfg.Nodes) != 0 || cfg.DefaultNodeName() != LocalNodeName {
		t.Fatalf("nodes=%+v default=%q", cfg.Nodes, cfg.DefaultNodeName())
	}
	if filepath.Base(cfg.StateDir) != "dshgw" {
		t.Fatalf("control-plane state dir = %q, want the ./data/dshgw default", cfg.StateDir)
	}
	if cfg.NodeStorePath() != filepath.Join(cfg.StateDir, "nodes.json") ||
		cfg.NodeSSHDir() != filepath.Join(cfg.StateDir, "node-ssh") ||
		cfg.NodeDeployLogDir() != filepath.Join(cfg.StateDir, "node-deploy") {
		t.Fatalf("node paths: %q %q %q", cfg.NodeStorePath(), cfg.NodeSSHDir(), cfg.NodeDeployLogDir())
	}
}

func TestNodeListValidation(t *testing.T) {
	root := t.TempDir()
	tokenFile := filepath.Join(root, "node-a.token")
	if err := os.WriteFile(tokenFile, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := baseBody(root) + "nodes:\n" +
		"  - name: node-a\n    url: http://192.168.190.87:18400\n    token_file: " + tokenFile + "\n" +
		"  - name: node-b\n    url: http://192.168.190.88:18400\n    token: inline-token\n" +
		"default_node: node-b\n"
	cfg, err := Load(writeConfig(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Nodes) != 2 || cfg.DefaultNodeName() != "node-b" {
		t.Fatalf("nodes=%+v default=%q", cfg.Nodes, cfg.DefaultNodeName())
	}
	nodeA, ok := cfg.NodeByName("node-a")
	if !ok {
		t.Fatal("node-a is missing")
	}
	token, err := cfg.NodeToken(nodeA)
	if err != nil {
		t.Fatal(err)
	}
	if token != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("token = %q", token)
	}
	if _, err := cfg.NodeToken(Node{Name: "node-b", Token: "inline-token"}); err != nil {
		t.Fatal(err)
	}

	// A relative token file is resolved against the deployment root (the process working
	// directory), like every other path key: the file the operator wrote is the file the
	// gateway reads.
	t.Chdir(root)
	pt := filepath.Join(root, "relative.token")
	if err := os.WriteFile(pt, []byte("relative-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relative, err := Load(writeConfig(t, baseBody(root)+"nodes:\n  - {name: node-c, url: 'http://10.0.0.3:1', token_file: ./relative.token}\n"))
	if err != nil {
		t.Fatal(err)
	}
	nodeC, _ := relative.NodeByName("node-c")
	if !filepath.IsAbs(nodeC.TokenFile) {
		t.Fatalf("relative token_file was not resolved: %q", nodeC.TokenFile)
	}
	if token, err := relative.NodeToken(nodeC); err != nil || token != "relative-token" {
		t.Fatalf("token = %q err = %v", token, err)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"duplicate names", "nodes:\n  - {name: node-a, url: 'http://10.0.0.1:1', token: t}\n  - {name: node-a, url: 'http://10.0.0.2:1', token: t}\n", "twice"},
		{"reserved name", "nodes:\n  - {name: local, url: 'http://10.0.0.1:1', token: t}\n", "must match"},
		{"uppercase name", "nodes:\n  - {name: Node-A, url: 'http://10.0.0.1:1', token: t}\n", "must match"},
		{"missing url", "nodes:\n  - {name: node-a, token: t}\n", "http:// URL"},
		{"https url", "nodes:\n  - {name: node-a, url: 'https://10.0.0.1:1', token: t}\n", "http:// URL"},
		{"url with path", "nodes:\n  - {name: node-a, url: 'http://10.0.0.1:1/node', token: t}\n", "http:// URL"},
		{"both token sources", "nodes:\n  - {name: node-a, url: 'http://10.0.0.1:1', token: t, token_file: /tmp/x}\n", "not both"},
		{"no token", "nodes:\n  - {name: node-a, url: 'http://10.0.0.1:1'}\n", "token or token_file"},
		{"unknown default node", "nodes:\n  - {name: node-a, url: 'http://10.0.0.1:1', token: t}\ndefault_node: node-z\n", "not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, baseBody(root)+tc.body))
			if err == nil {
				t.Fatalf("expected a rejection for %q", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNodeTokenFileRules(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{}

	t.Run("missing", func(t *testing.T) {
		if _, err := cfg.NodeToken(Node{Name: "node-a", TokenFile: filepath.Join(root, "absent")}); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("too broad", func(t *testing.T) {
		path := filepath.Join(root, "broad.token")
		if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := cfg.NodeToken(Node{Name: "node-a", TokenFile: path})
		if err == nil || !strings.Contains(err.Error(), "broader") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		path := filepath.Join(root, "empty.token")
		if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := cfg.NodeToken(Node{Name: "node-a", TokenFile: path})
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("multi line", func(t *testing.T) {
		path := filepath.Join(root, "multi.token")
		if err := os.WriteFile(path, []byte("one two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := cfg.NodeToken(Node{Name: "node-a", TokenFile: path})
		if err == nil || !strings.Contains(err.Error(), "whitespace-free") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink leaf", func(t *testing.T) {
		target := filepath.Join(root, "real.token")
		if err := os.WriteFile(target, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link.token")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.NodeToken(Node{Name: "node-a", TokenFile: link}); err == nil {
			t.Fatal("a symlinked token file must be refused")
		}
	})
	t.Run("neither source", func(t *testing.T) {
		if _, err := cfg.NodeToken(Node{Name: "node-a"}); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// TestNodeModeValidation covers the worker-node half: the block is required, its listener must
// be explicit and outside the port bands, and a node must not carry the control plane's list.
func TestNodeModeValidation(t *testing.T) {
	root := t.TempDir()
	nodeBody := func(extra string) string {
		return baseBody(root) + "aigw_base_url: http://192.168.190.86:8088\n" +
			"node:\n  name: node-a\n  listen: 192.168.190.87:18400\n  token: node-token\n" + extra
	}
	cfg, err := Load(writeConfig(t, nodeBody("")))
	_ = cfg
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NodeMode() {
		t.Fatal("the node block must switch the process into node mode")
	}
	// The node-mode default data root is its own, so a node and a control plane may share a
	// machine without sharing state.
	bare := "public_host: dsh.example.test\ndeploy:\n  plugin_path: " + filepath.Join(root, "picker-clamp.js") + "\n" +
		"node:\n  name: node-a\n  listen: 192.168.190.87:18400\n  token: node-token\n"
	bareCfg, err := Load(writeConfig(t, bare))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(bareCfg.StateDir) != "dshgw-node" {
		t.Fatalf("a worker node defaults to its own data root, got %q", bareCfg.StateDir)
	}
	if token, err := cfg.NodeSelfToken(); err != nil || token != "node-token" {
		t.Fatalf("token = %q err = %v", token, err)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing listen", baseBody(root) + "node:\n  name: node-a\n  token: t\n", "node.listen is required"},
		{"bare port", baseBody(root) + "node:\n  name: node-a\n  listen: ':18400'\n  token: t\n", "must name the interface"},
		{"wildcard", baseBody(root) + "node:\n  name: node-a\n  listen: 0.0.0.0:18400\n  token: t\n", "wildcard"},
		{"inside the worker band", baseBody(root) + "node:\n  name: node-a\n  listen: 192.168.190.87:32100\n  token: t\n", "worker port range"},
		{"inside the public band", baseBody(root) + "node:\n  name: node-a\n  listen: 192.168.190.87:32601\n  token: t\n", "public portal/tenant port range"},
		{"missing token", baseBody(root) + "node:\n  name: node-a\n  listen: 192.168.190.87:18400\n", "token or token_file"},
		{"reserved name", baseBody(root) + "node:\n  name: local\n  listen: 192.168.190.87:18400\n  token: t\n", "must match"},
		{"node carries a node list", baseBody(root) + "node:\n  name: node-a\n  listen: 192.168.190.87:18400\n  token: t\n" +
			"nodes:\n  - {name: node-b, url: 'http://10.0.0.1:1', token: t}\n", "must not configure nodes"},
		{"node carries a default node", baseBody(root) + "node:\n  name: node-a\n  listen: 192.168.190.87:18400\n  token: t\n" +
			"default_node: node-b\n", "must not configure nodes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected a rejection for %q", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNodeNameHelpers(t *testing.T) {
	for name, want := range map[string]bool{
		"node-a": true, "n": true, "node-1": true,
		"": false, "local": false, "Node-a": false, "-node": false, "node_a": false,
		"node-" + strings.Repeat("x", 30): false,
	} {
		if got := ValidNodeName(name); got != want {
			t.Errorf("ValidNodeName(%q) = %t, want %t", name, got, want)
		}
	}
	for ref, want := range map[string]bool{
		"": true, "local": true, "node-a": true, "Node A": false, "node/other": false,
	} {
		if got := ValidNodeRef(ref); got != want {
			t.Errorf("ValidNodeRef(%q) = %t, want %t", ref, got, want)
		}
	}
	if !IsLocalNodeName("") || !IsLocalNodeName("local") || IsLocalNodeName("node-a") {
		t.Fatal("IsLocalNodeName disagrees with the documented meanings")
	}
	cfg := &Config{}
	if cfg.DefaultNodeName() != LocalNodeName {
		t.Fatalf("an empty default_node must mean local, got %q", cfg.DefaultNodeName())
	}
	if !cfg.IsLocalNode("") || !cfg.IsLocalNode(LocalNodeName) {
		t.Fatal("IsLocalNode must accept both spellings")
	}
}

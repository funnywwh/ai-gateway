package nodestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/config"
)

func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "nodes.json")
}

// loadEmpty proves the single-machine case: no file, no nodes, no error.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	store, err := Load(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("expected an empty store, got %+v", got)
	}
	view, err := store.View(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 0 {
		t.Fatalf("view = %+v", view)
	}
	if name := DefaultNode(view, ""); name != config.LocalNodeName {
		t.Fatalf("DefaultNode = %q, want local", name)
	}
}

func TestPutGetListDeleteRoundTrip(t *testing.T) {
	path := storePath(t)
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	node := Node{
		Name:  "node-a",
		URL:   "http://192.168.190.87:18400",
		Token: "node-a-token",
		SSH: SSH{
			Host:    "192.168.190.87",
			Port:    22,
			User:    "winger",
			KeyFile: "/home/winger/.ssh/id_ed25519",
		},
		Deploy: Deploy{Dir: "/srv/dshgw-node", WorkerPortLo: 32100, WorkerPortHi: 32299},
		Status: Status{State: StatePending},
	}
	if err := store.Put(node); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("node store mode = %04o, want 0600", perm)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("node-a")
	if !ok {
		t.Fatal("node-a is missing after reload")
	}
	if got.URL != node.URL || got.SSH.User != "winger" || got.Deploy.Dir != "/srv/dshgw-node" {
		t.Fatalf("reloaded node = %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps were not stamped: %+v", got)
	}

	if err := reloaded.Delete("node-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("node-a"); ok {
		t.Fatal("node-a survived deletion")
	}
	if err := reloaded.Delete("node-a"); err == nil {
		t.Fatal("deleting a missing node must be an error, not a silent success")
	}
}

func TestPutValidation(t *testing.T) {
	store, err := Load(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		node Node
		want string
	}{
		{"empty name", Node{URL: "http://10.0.0.1:1", Token: "t"}, "node name"},
		{"reserved name", Node{Name: "local", URL: "http://10.0.0.1:1", Token: "t"}, "must match"},
		{"uppercase name", Node{Name: "Node-A", URL: "http://10.0.0.1:1", Token: "t"}, "must match"},
		{"bad url", Node{Name: "node-a", URL: "192.168.190.87:18400", Token: "t"}, "http:// URL"},
		{"https url", Node{Name: "node-a", URL: "https://192.168.190.87", Token: "t"}, "http:// URL"},
		{"url with path", Node{Name: "node-a", URL: "http://192.168.190.87/node", Token: "t"}, "http:// URL"},
		{"both token sources", Node{Name: "node-a", URL: "http://10.0.0.1:1", Token: "t", TokenFile: "/tmp/x"}, "not both"},
		{"relative deploy dir", Node{Name: "node-a", URL: "http://10.0.0.1:1", Token: "t", Deploy: Deploy{Dir: "srv/node"}}, "absolute"},
		{"inverted port band", Node{Name: "node-a", URL: "http://10.0.0.1:1", Token: "t", Deploy: Deploy{WorkerPortLo: 33, WorkerPortHi: 32}}, "low bound"},
		{"unknown state", Node{Name: "node-a", URL: "http://10.0.0.1:1", Token: "t", Status: Status{State: "banana"}}, "unknown status"},
		{"bad ssh port", Node{Name: "node-a", URL: "http://10.0.0.1:1", Token: "t", SSH: SSH{Port: 70000}}, "ssh port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Put(tc.node)
			if err == nil {
				t.Fatalf("expected a rejection for %+v", tc.node)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
	// Registration with no address yet is legal, and so is an address with no secret yet: both are
	// what "registered, not deployed" looks like. The deploy installs the token and records it.
	if err := store.Put(Node{Name: "node-b", SSH: SSH{Host: "10.0.0.2", User: "winger", KeyFile: "/keys/b"}}); err != nil {
		t.Fatalf("a registered-but-undeployed node must be storable: %v", err)
	}
	if err := store.Put(Node{Name: "node-c", URL: "http://10.0.0.3:1", Status: Status{State: StatePending}}); err != nil {
		t.Fatalf("a node with an address but no secret yet must be storable: %v", err)
	}
}

// TestViewMergePrecedence pins the rule the whole package exists for: configuration owns the
// address and the token, the store owns everything else.
func TestViewMergePrecedence(t *testing.T) {
	store, err := Load(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Node{
		Name:      "node-a",
		URL:       "http://127.0.0.1:1",
		Token:     "store-token",
		SSH:       SSH{Host: "192.168.190.87", User: "winger", KeyFile: "/keys/a"},
		Deploy:    Deploy{Dir: "/srv/node-a"},
		Status:    Status{State: StateReady},
		Overrides: Overrides{DirectoryPicker: "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Node{Name: "node-b", URL: "http://192.168.190.88:18400", TokenFile: "/tokens/b"}); err != nil {
		t.Fatal(err)
	}
	static := []config.Node{{Name: "node-a", URL: "http://192.168.190.87:18400", Token: "cfg-token"}}
	view, err := store.View(static)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 2 {
		t.Fatalf("view = %+v", view)
	}
	a, _ := Find(view, "node-a")
	if a.Source != SourceConfig {
		t.Fatalf("node-a source = %q, want config", a.Source)
	}
	if a.URL != "http://192.168.190.87:18400" || a.Token != "cfg-token" {
		t.Fatalf("configuration must win on identity: %+v", a)
	}
	if a.SSH.Host == "" || a.Deploy.Dir != "/srv/node-a" || a.Status.State != StateReady || a.Overrides.DirectoryPicker != "clamp" {
		t.Fatalf("the store must keep contributing deployment metadata: %+v", a)
	}
	b, _ := Find(view, "node-b")
	if b.Source != SourceConsole || b.TokenFile != "/tokens/b" {
		t.Fatalf("node-b = %+v", b)
	}
}

func TestSetDefaultIsExclusive(t *testing.T) {
	store, err := Load(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"node-a", "node-b"} {
		if err := store.Put(Node{Name: name, URL: "http://10.0.0." + strings.TrimPrefix(name, "node-") + ":1", Token: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetDefault("node-b"); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := DefaultNode(view, ""); got != "node-b" {
		t.Fatalf("DefaultNode = %q, want node-b", got)
	}
	if err := store.SetDefault("node-a"); err != nil {
		t.Fatal(err)
	}
	view, err = store.View(nil)
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, n := range view {
		if n.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("exactly one node may be default, got %d: %+v", defaults, view)
	}
	// Clearing falls back to the control plane's own machine, which is not a record at all.
	if err := store.SetDefault(""); err != nil {
		t.Fatal(err)
	}
	view, _ = store.View(nil)
	if got := DefaultNode(view, ""); got != config.LocalNodeName {
		t.Fatalf("DefaultNode after clearing = %q", got)
	}
	if err := store.SetDefault("nope"); err == nil {
		t.Fatal("setting a missing node as default must fail")
	}
}

func TestSetStatusIgnoresDeletedNode(t *testing.T) {
	store, err := Load(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Node{Name: "node-a", URL: "http://10.0.0.1:1", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus("node-a", Status{State: StateReady, Revision: "abc"}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get("node-a")
	if got.Status.State != StateReady || got.Status.Revision != "abc" {
		t.Fatalf("status = %+v", got.Status)
	}
	if err := store.Delete("node-a"); err != nil {
		t.Fatal(err)
	}
	// A probe that finishes after the operator deleted the node must not resurrect it.
	if err := store.SetStatus("node-a", Status{State: StateReady}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("node-a"); ok {
		t.Fatal("SetStatus resurrected a deleted node")
	}
}

func TestNamesKnownAndResolve(t *testing.T) {
	view := []Node{{Name: "node-a"}, {Name: "node-b"}}
	known := NamesKnown(view)
	for _, name := range []string{"node-a", "node-b"} {
		if !known(name) {
			t.Fatalf("%s should be known", name)
		}
	}
	if known("node-c") {
		t.Fatal("node-c must not be known")
	}
	for _, name := range []string{"", config.LocalNodeName} {
		node, ok := Resolve(view, name)
		if !ok || node.Name != config.LocalNodeName {
			t.Fatalf("Resolve(%q) = (%+v, %t)", name, node, ok)
		}
	}
	if _, ok := Resolve(view, "node-c"); ok {
		t.Fatal("Resolve must not invent a node")
	}
}

func TestLoadRejectsUnusableFile(t *testing.T) {
	path := storePath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected a decode error")
	}
	if err := os.WriteFile(path, []byte(`{"version":99,"nodes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected a version error, got %v", err)
	}
	// A world-readable token file is a control-channel downgrade, so the store must refuse it.
	if err := os.WriteFile(path, []byte(`{"version":1,"nodes":[{"name":"node-a","url":"http://10.0.0.1:1","token":"t"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod explicitly: the process umask would otherwise decide the mode, and this test is
	// about the store refusing a documented-too-broad file, not about the umask.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected a permission error for a world-readable node store")
	}
}

// TestAuditCursorOnAConfigurationNode is the case the audit merger hit: a node declared in
// configuration has no record of its own, and the cursor still has to live somewhere — otherwise
// every gateway start either re-imports that node's history or skips everything written while the
// gateway was down.
func TestAuditCursorOnAConfigurationNode(t *testing.T) {
	path := storePath(t)
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.AuditCursor("node-a"); got != "" {
		t.Fatalf("cursor = %q", got)
	}
	if err := store.SetAuditCursor("node-a", "off:128"); err != nil {
		t.Fatal(err)
	}
	if got := store.AuditCursor("node-a"); got != "off:128" {
		t.Fatalf("cursor = %q", got)
	}
	// It survives a restart, which is the whole point.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.AuditCursor("node-a"); got != "off:128" {
		t.Fatalf("cursor after reload = %q", got)
	}
	// The empty record does not take over the node's identity: configuration still decides it.
	static := []config.Node{{Name: "node-a", URL: "http://10.0.0.1:1", Token: "cfg-token"}}
	view, err := reloaded.View(static)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("view = %+v", view)
	}
	if view[0].Source != SourceConfig || view[0].URL != "http://10.0.0.1:1" || view[0].Token != "cfg-token" {
		t.Fatalf("the cursor record changed the node's identity: %+v", view[0])
	}
	if view[0].Status.AuditCursor != "off:128" {
		t.Fatalf("the cursor did not survive the merge: %+v", view[0].Status)
	}
	// Setting the same cursor twice is a no-op, not a rewrite.
	if err := reloaded.SetAuditCursor("node-a", "off:128"); err != nil {
		t.Fatal(err)
	}
}

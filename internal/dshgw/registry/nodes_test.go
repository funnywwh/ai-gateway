package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/config"
)

// TestLegacyRegistryHasNoNode pins the compatibility rule the field was designed around: every
// registry written before M77 lacks the key, and it must keep loading as "this machine".
func TestLegacyRegistryHasNoNode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	legacy := `{"version":1,"tenants":[{"name":"alice","uid":1000,"public_port":32601,"worker_port":32100,` +
		`"key_prefix":"sk-aaaaaaaaa","dsh_home":"/state/alice/.dsh","workspace":"/srv/alice",` +
		`"created_at":"2026-01-02T03:04:05Z","handshake":"ok","isolation":"bwrap"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(path, filepath.Join(dir, "keys.map"))
	if err != nil {
		t.Fatal(err)
	}
	alice, ok := reg.Get("alice")
	if !ok {
		t.Fatal("alice is missing")
	}
	if alice.Node != "" {
		t.Fatalf("legacy tenant node = %q, want empty", alice.Node)
	}
	if !config.IsLocalNodeName(alice.Node) {
		t.Fatal("the empty node value must mean local")
	}
	// The runtime check must accept it without a node list at all.
	if err := reg.ValidateNodes(func(string) bool { return false }); err != nil {
		t.Fatalf("a registry with no node references must validate: %v", err)
	}
	if got := reg.TenantsForNode(config.LocalNodeName); len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("local tenants = %+v", got)
	}
}

func TestNodeFieldRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	reg := New(path, filepath.Join(dir, "keys.map"))
	item := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	item.Node = "node-a"
	if err := reg.Put(item); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"node": "node-a"`) {
		t.Fatalf("registry did not persist the node: %s", data)
	}
	reloaded, err := Load(path, filepath.Join(dir, "keys.map"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("alice")
	if !ok || got.Node != "node-a" {
		t.Fatalf("reloaded tenant = %+v (ok=%t)", got, ok)
	}

	// A stored value that could never be a node name is refused at load time, where it is a
	// file problem an operator can fix, rather than at request time.
	var disk diskRegistry
	if err := json.Unmarshal([]byte(`{"version":1,"tenants":[{"name":"bob","uid":1000,"public_port":32602,`+
		`"worker_port":32101,"key_prefix":"sk-bbbbbbbbb","dsh_home":"/state/bob/.dsh","workspace":"/srv/bob",`+
		`"created_at":"2026-01-02T03:04:05Z","handshake":"ok","isolation":"bwrap","node":"Node A"}]}`), &disk); err != nil {
		t.Fatal(err)
	}
	if err := validateTenant(disk.Tenants[0]); err == nil {
		t.Fatal("a malformed node name must be rejected")
	}
	// The literal "local" is accepted on read: it is the other spelling of "this machine".
	local := tenant("carol", "sk-ccccccccc", 32603, 32102)
	local.Node = config.LocalNodeName
	if err := validateTenant(local); err != nil {
		t.Fatalf("the literal local must be accepted: %v", err)
	}
}

// TestValidateNodesHook pins the two-phase check: the registry loads without a node list (the
// offline CLI paths and the unit tests), and the process that has a configuration installs the
// hook, which then applies to the existing snapshot and to later writes.
func TestValidateNodesHook(t *testing.T) {
	reg := New("x", "y")
	placed := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	placed.Node = "node-a"
	if err := reg.Put(placed); err != nil {
		t.Fatalf("without a hook any well-formed name is accepted: %v", err)
	}
	if err := reg.ValidateNodes(func(name string) bool { return name == "node-b" }); err == nil {
		t.Fatal("installing a hook over an unknown placement must fail")
	}
	if err := reg.ValidateNodes(func(name string) bool { return name == "node-a" }); err != nil {
		t.Fatalf("a known placement must validate: %v", err)
	}
	// Later writes are checked too.
	unknown := tenant("bob", "sk-bbbbbbbbb", 32602, 32101)
	unknown.Node = "node-z"
	if err := reg.Put(unknown); err == nil {
		t.Fatal("Put must refuse a node the deployment does not define")
	}
	if err := reg.SetNode("alice", "node-z"); err == nil {
		t.Fatal("SetNode must refuse a node the deployment does not define")
	}
	if err := reg.SetNode("alice", config.LocalNodeName); err != nil {
		t.Fatalf("moving a tenant back to this machine must work: %v", err)
	}
	if got, _ := reg.Get("alice"); got.Node != config.LocalNodeName {
		t.Fatalf("alice node = %q", got.Node)
	}
}

func TestTenantsForNodeAndWorkerPort(t *testing.T) {
	reg := New("x", "y")
	local := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	remoteA := tenant("bob", "sk-bbbbbbbbb", 32602, 32101)
	remoteA.Node = "node-a"
	remoteB := tenant("carol", "sk-ccccccccc", 32603, 32102)
	remoteB.Node = "node-b"
	for _, item := range []Tenant{local, remoteA, remoteB} {
		if err := reg.Put(item); err != nil {
			t.Fatal(err)
		}
	}
	if got := reg.TenantsForNode(""); len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("local tenants = %+v", got)
	}
	if got := reg.TenantsForNode(config.LocalNodeName); len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("local tenants via the literal = %+v", got)
	}
	if got := reg.TenantsForNode("node-a"); len(got) != 1 || got[0].Name != "bob" {
		t.Fatalf("node-a tenants = %+v", got)
	}
	if got := reg.TenantsForNode("node-b"); len(got) != 1 || got[0].Name != "carol" {
		t.Fatalf("node-b tenants = %+v", got)
	}
	if got := reg.TenantsForNode("node-c"); len(got) != 0 {
		t.Fatalf("node-c tenants = %+v", got)
	}

	// A node may re-allocate a worker port when its agent restarts; the registry follows.
	if err := reg.SetWorkerPort("bob", 32200); err != nil {
		t.Fatal(err)
	}
	if got, _ := reg.Get("bob"); got.WorkerPort != 32200 {
		t.Fatalf("bob worker port = %d", got.WorkerPort)
	}
	if err := reg.SetWorkerPort("bob", 32200); err != nil {
		t.Fatalf("idempotent rewrite must succeed: %v", err)
	}
	if err := reg.SetWorkerPort("bob", 0); err == nil {
		t.Fatal("port 0 must be refused")
	}
	if err := reg.SetWorkerPort("nobody", 32100); err == nil {
		t.Fatal("an unknown tenant must be refused")
	}
	if err := reg.SetNode("nobody", "node-a"); err == nil {
		t.Fatal("an unknown tenant must be refused")
	}
}

// TestAssignWorkerPort is the node's allocator (M77): a node picks its own worker port from its
// own band, and both its registry and the processes already listening make a port unavailable.
func TestAssignWorkerPort(t *testing.T) {
	cfg := &config.Config{WorkerPortLo: 32900, WorkerPortHi: 32902}
	reg := New("x", "y")
	first, err := reg.AssignWorkerPort(cfg, nil)
	if err != nil || first != 32900 {
		t.Fatalf("first = %d, %v", first, err)
	}
	item := tenant("alice", "sk-aaaaaaaaa", 32601, first)
	if err := reg.Put(item); err != nil {
		t.Fatal(err)
	}
	second, err := reg.AssignWorkerPort(cfg, func(port int) bool { return port == 32901 })
	if err != nil || second != 32902 {
		t.Fatalf("second = %d, %v", second, err)
	}
	third := tenant("bob", "sk-bbbbbbbbb", 32602, second)
	if err := reg.Put(third); err != nil {
		t.Fatal(err)
	}
	// Every port is now used either by a record or by a listener.
	if free, err := reg.AssignWorkerPort(cfg, nil); err != nil || free != 32901 {
		t.Fatalf("the remaining free port = %d, %v", free, err)
	}
	if _, err := reg.AssignWorkerPort(cfg, func(port int) bool { return port == 32901 }); err == nil || !strings.Contains(err.Error(), "no free worker port") {
		t.Fatalf("exhausted band err = %v", err)
	}
	if _, err := reg.AssignWorkerPort(&config.Config{WorkerPortLo: 33000, WorkerPortHi: 32999}, nil); err == nil {
		t.Fatal("an inverted band must be refused")
	}
}

func TestSetPlacementRewritesWhatTheNodeOwns(t *testing.T) {
	reg := New("x", "y")
	item := tenant("alice", "sk-aaaaaaaaa", 32601, 32900)
	if err := reg.Put(item); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetPlacement("alice", 32907, "/node/tenants/alice/.dsh", "/node/srv/alice"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Get("alice")
	if got.WorkerPort != 32907 || got.DshHome != "/node/tenants/alice/.dsh" || got.Workspace != "/node/srv/alice" {
		t.Fatalf("got = %+v", got)
	}
	// Idempotent, and it validates what it is given: a relative path would be a record the
	// lifecycle could not use on the node it describes.
	if err := reg.SetPlacement("alice", 32907, "/node/tenants/alice/.dsh", "/node/srv/alice"); err != nil {
		t.Fatalf("idempotent call: %v", err)
	}
	if err := reg.SetPlacement("alice", 0, "/node/x/.dsh", "/node/srv/x"); err == nil {
		t.Fatal("port 0 must be refused")
	}
	if err := reg.SetPlacement("alice", 32908, "relative/.dsh", "/node/srv/x"); err == nil {
		t.Fatal("a relative dsh home must be refused")
	}
	if err := reg.SetPlacement("nobody", 32908, "/node/x/.dsh", "/node/srv/x"); err == nil {
		t.Fatal("an unknown tenant must be refused")
	}
}

// TestPrefixlessRecordRoundTrips is the node's allocation table (M77): a record with no key
// prefix — because the prefix belongs to the control plane — must save, reload and still be a
// valid entry.
func TestPrefixlessRecordRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	keyMap := filepath.Join(dir, "keys.map")
	reg := New(path, keyMap)
	adopted := tenant("alice", "", 32601, 32900)
	if err := reg.Put(adopted); err != nil {
		t.Fatalf("a record without a key prefix must be storable: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path, keyMap)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reloaded.Get("alice")
	if !ok || got.KeyPrefix != "" {
		t.Fatalf("got = %+v (ok=%t)", got, ok)
	}
	if _, ok := reloaded.ByPrefix(""); ok {
		t.Fatal("an empty prefix must never resolve as a key binding")
	}
	// The derived key map has no entry for it, because there is no key to map.
	data, err := os.ReadFile(keyMap)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("key map = %q", data)
	}
}

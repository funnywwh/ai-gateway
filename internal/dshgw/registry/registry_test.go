package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
)

func tenant(name, prefix string, pub, worker int) Tenant {
	return Tenant{Name: name, UID: 1000, PublicPort: pub, WorkerPort: worker, KeyPrefix: prefix, DshHome: "/state/" + name, Workspace: "/srv/" + name, CreatedAt: time.Now().UTC(), Handshake: HandshakePending}
}

func TestSaveLoadModesAndIndexes(t *testing.T) {
	dir := t.TempDir()
	rp := filepath.Join(dir, "registry.json")
	kp := filepath.Join(dir, "keys.map")
	r := New(rp, kp)
	if err := r.Put(tenant("alice", "sk-aaaaaaaaa", 32601, 32100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{rp: 0o600, kp: 0o640} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode=%o", p, info.Mode().Perm())
		}
	}
	got, err := Load(rp, kp)
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := got.ByPrefix("sk-aaaaaaaaa"); !ok || x.Name != "alice" {
		t.Fatalf("lookup=%#v,%v", x, ok)
	}
	km, err := LoadKeyMap(kp)
	if err != nil {
		t.Fatal(err)
	}
	if km["sk-aaaaaaaaa"] != "alice" {
		t.Fatalf("map=%v", km)
	}
}

func TestAssignPortsUsesRegistryAndListeners(t *testing.T) {
	cfg := &config.Config{TenantPortLo: 32601, TenantPortHi: 32603, WorkerPortLo: 32100, WorkerPortHi: 32102}
	r := New("x", "y")
	if err := r.Put(tenant("alice", "sk-aaaaaaaaa", 32601, 32100)); err != nil {
		t.Fatal(err)
	}
	pub, worker, err := r.AssignPorts(cfg, func(p int) bool { return p == 32602 || p == 32101 })
	if err != nil {
		t.Fatal(err)
	}
	if pub != 32603 || worker != 32102 {
		t.Fatalf("got %d/%d", pub, worker)
	}
}

func TestRejectDuplicatePrefixOrPort(t *testing.T) {
	r := New("x", "y")
	if err := r.Put(tenant("alice", "sk-aaaaaaaaa", 32601, 32100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(tenant("bob", "sk-aaaaaaaaa", 32602, 32101)); err == nil {
		t.Fatal("duplicate prefix accepted")
	}
	if err := r.Put(tenant("bob", "sk-bbbbbbbbb", 32601, 32101)); err == nil {
		t.Fatal("duplicate port accepted")
	}
}

func TestListeningPorts(t *testing.T) {
	got := ListeningPorts([]byte("LISTEN 0 4096 127.0.0.1:32100 0.0.0.0:*\nLISTEN 0 10 [::]:32601 [::]:*\n"))
	if !got[32100] || !got[32601] {
		t.Fatalf("%v", got)
	}
}

// The isolation field arrived after the first deployments, so a registry
// written before it existed must keep loading and mean the per-tenant-account
// mode. An unknown value must be rejected instead of silently defaulting.
func TestIsolationFieldCompatibility(t *testing.T) {
	dir := t.TempDir()
	rp := filepath.Join(dir, "registry.json")
	kp := filepath.Join(dir, "keys.map")
	legacy := `{
  "version": 1,
  "tenants": [
    {
      "name": "alice",
      "uid": 1000,
      "public_port": 32601,
      "worker_port": 32100,
      "key_prefix": "sk-aaaaaaaaa",
      "dsh_home": "/state/alice/.dsh",
      "workspace": "/srv/alice",
      "created_at": "2025-01-01T00:00:00Z",
      "handshake": "ok"
    }
  ]
}
`
	if err := os.WriteFile(rp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(rp, kp)
	if err != nil {
		t.Fatalf("legacy registry without an isolation field was rejected: %v", err)
	}
	got, ok := loaded.Get("alice")
	if !ok {
		t.Fatal("legacy tenant missing")
	}
	if got.Isolation != "" || got.EffectiveIsolation() != IsolationUser {
		t.Fatalf("legacy tenant isolation = %q / %q, want empty / user", got.Isolation, got.EffectiveIsolation())
	}
	// A mode this build does not implement must never be read as the default.
	unknown := strings.Replace(legacy, `"handshake": "ok"`, `"handshake": "ok", "isolation": "jail"`, 1)
	if err := os.WriteFile(rp, []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(rp, kp); err == nil || !strings.Contains(err.Error(), "invalid isolation mode") {
		t.Fatalf("unknown isolation mode accepted: %v", err)
	}
}

package registry

import (
	"os"
	"path/filepath"
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

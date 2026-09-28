package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/funnywwh/ai-gateway/internal/dshgw/config"
)

func tenant(name, prefix string, pub, worker int) Tenant {
	return Tenant{Name: name, UID: 1000, PublicPort: pub, WorkerPort: worker, KeyPrefix: prefix, DshHome: "/state/" + name, Workspace: "/srv/" + name, CreatedAt: time.Now().UTC(), Handshake: HandshakePending}
}

func TestSaveLoadModesAndIndexes(t *testing.T) {
	dir := t.TempDir()
	rp := filepath.Join(dir, "registry.json")
	kp := filepath.Join(dir, "keys.map")
	// A file an older release left behind: Save must retire it (M88), because a stale
	// prefix→tenant index is worse than no index at all.
	if err := os.WriteFile(kp, []byte("sk-aaaaaaaaa alice\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	r := New(rp, kp)
	if err := r.Put(tenant("alice", "sk-aaaaaaaaa", 32601, 32100)); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rp)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode=%o", rp, info.Mode().Perm())
	}
	if _, err := os.Stat(kp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("keys.map must be retired: %v", err)
	}
	got, err := Load(rp, kp)
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := got.Get("alice"); !ok || x.KeyPrefix != "sk-aaaaaaaaa" {
		t.Fatalf("lookup=%#v,%v", x, ok)
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

func TestRejectDuplicatePortButAllowSharedKeyLabel(t *testing.T) {
	r := New("x", "y")
	if err := r.Put(tenant("alice", "sk-aaaaaaaaa", 32601, 32100)); err != nil {
		t.Fatal(err)
	}
	// Two tenants may wear the same label (M88): nothing resolves a tenant by it any more, and
	// two credentials sharing their first 12 characters is ordinary (M87).
	if err := r.Put(tenant("bob", "sk-aaaaaaaaa", 32602, 32101)); err != nil {
		t.Fatalf("a shared key label must be allowed: %v", err)
	}
	if err := r.Put(tenant("carol", "sk-ccccccccc", 32601, 32102)); err == nil {
		t.Fatal("duplicate port accepted")
	}
}

func TestByAccountFindsTheTenantWhateverItsLabel(t *testing.T) {
	r := New("x", "y")
	alice := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	alice.Account = "acme"
	if err := r.Put(alice); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(tenant("bob", "sk-bbbbbbbbb", 32602, 32101)); err != nil {
		t.Fatal(err)
	}
	got := r.ByAccount("ACME")
	if len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("ByAccount = %+v", got)
	}
	if len(r.ByAccount("")) != 0 || len(r.ByAccount("nobody")) != 0 {
		t.Fatal("an unknown account must not match")
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

// M67: the account label is what the tenant's sidebar names a signed-in person with. It is
// optional (tenants predate it and the CLI creates tenants without one), it survives a
// save/load round trip, and a value that cannot be rendered safely is refused.
func TestAccountLabelRoundTripAndLimits(t *testing.T) {
	dir := t.TempDir()
	rp := filepath.Join(dir, "registry.json")
	kp := filepath.Join(dir, "keys.map")
	r := New(rp, kp)
	alice := tenant("alice", "sk-aaaaaaaaa", 32601, 32100)
	if err := r.Put(alice); err != nil {
		t.Fatal(err)
	}
	// A tenant recorded without a label is legal and stays that way.
	if got, _ := r.Get("alice"); got.Account != "" {
		t.Fatalf("account = %q, want empty", got.Account)
	}
	if err := r.SetAccount("alice", "  李雷(alex)  "); err != nil {
		t.Fatal(err)
	}
	// SetAccount trims, so a value from a form does not become a second, unequal label.
	if got, _ := r.Get("alice"); got.Account != "李雷(alex)" {
		t.Fatalf("account = %q", got.Account)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(rp, kp)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := loaded.Get("alice"); got.Account != "李雷(alex)" {
		t.Fatalf("account after reload = %q", got.Account)
	}
	// Unknown tenants and unusable labels are refusals, not silent writes.
	if err := r.SetAccount("nobody", "x"); err == nil {
		t.Fatal("setting an account on a missing tenant must fail")
	}
	for _, bad := range []string{"bad\x00label", "line\nbreak", strings.Repeat("x", 129)} {
		if err := r.SetAccount("alice", bad); err == nil {
			t.Fatalf("unusable account label %q accepted", bad)
		}
	}
	// The label is validated on the whole record too, not only through SetAccount: a tenant
	// written by any other path must not carry a value that cannot be rendered.
	alice.Account = "bad\x1f"
	if err := r.Put(alice); err == nil {
		t.Fatal("a tenant carrying a control character in its account label must be refused")
	}
}

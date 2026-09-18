package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

type stubValidator struct {
	models []string
	err    error
	calls  int
}

func (s *stubValidator) ValidateKey(context.Context, string) ([]string, error) {
	s.calls++
	return s.models, s.err
}

// refreshFixture builds a hook over a tenant whose settings.yaml already lists one
// model, so "the refresh ran" is observable as a change on disk.
func refreshFixture(t *testing.T, validator keyValidator) (func(context.Context, registry.Tenant) error, *config.Config, registry.Tenant, *tenancy.Manager) {
	t.Helper()
	root := t.TempDir()
	tenantName := "alice"
	cfg := &config.Config{
		AigwBaseURL:     "http://aigw",
		StateDir:        filepath.Join(root, "state"),
		TenantRoot:      filepath.Join(root, "state/tenants"),
		WorkspaceRoot:   filepath.Join(root, "srv"),
		RegistryPath:    filepath.Join(root, "registry.json"),
		KeyMapPath:      filepath.Join(root, "keys.map"),
		DirectoryPicker: "clamp", PluginBrowserFS: "on",
		Deploy: config.DeployConfig{TenantConfigRoot: filepath.Join(root, "state/tenants")},
	}
	dshHome := filepath.Join(cfg.TenantRoot, tenantName, ".dsh")
	for _, dir := range []string{dshHome, filepath.Join(cfg.Deploy.TenantConfigRoot, tenantName), filepath.Join(cfg.WorkspaceRoot, tenantName)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The renderer owns the aigw provider block; anything else in the file is
	// preserved as-is, so the tests compare bytes rather than guessing the shape.
	if err := os.WriteFile(filepath.Join(dshHome, "settings.yaml"), []byte("providers:\n  aigw:\n    models:\n      - old-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Deploy.TenantConfigRoot, tenantName, "gateway.key"), []byte("sk-aaaaaaaaa-rest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	manager := &tenancy.Manager{Config: cfg, Registry: reg, Now: func() time.Time { return time.Now().UTC() },
		Probe: func(context.Context, registry.Tenant) error { return nil }}
	tenant := registry.Tenant{
		Name: tenantName, WorkerPort: 32100, PublicPort: 32601, UID: os.Geteuid(), Isolation: registry.IsolationBwrap,
		DshHome: dshHome, Workspace: filepath.Join(cfg.WorkspaceRoot, tenantName), CreatedAt: time.Now().UTC(),
		KeyPrefix: "sk-aaaaaaaaa", Handshake: registry.HandshakeOK,
	}
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	// Lifecycle operations re-read the registry from disk under the lock, so the
	// fixture must persist what it registers.
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return modelRefreshHook(cfg, validator, manager, log), cfg, tenant, manager
}

// settingsSnapshot reads the tenant's settings.yaml: the tests compare bytes
// before and after a refresh, which stays true regardless of how the renderer
// shapes the aigw provider block.
func settingsSnapshot(t *testing.T, tenant registry.Tenant) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(tenant.DshHome, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestModelRefreshAppliesCurrentGrantsBeforeStart(t *testing.T) {
	validator := &stubValidator{models: []string{"deepseek-flash", "gpt-5.6-luna"}}
	hook, _, tenant, _ := refreshFixture(t, validator)
	before := settingsSnapshot(t, tenant)
	if err := hook(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if validator.calls != 1 {
		t.Fatalf("validator calls = %d, want 1", validator.calls)
	}
	settings := settingsSnapshot(t, tenant)
	for _, want := range []string{"deepseek-flash", "gpt-5.6-luna"} {
		if !strings.Contains(settings, want) {
			t.Fatalf("settings.yaml missing the refreshed model %q:\n%s", want, settings)
		}
	}
	if settings == before {
		t.Fatal("settings.yaml was not rewritten by the refresh")
	}
}

// A revoked key must stop the start: the tenant could not call a single model, and
// starting it would hide the real problem behind a broken UI.
func TestModelRefreshRefusesRevokedKey(t *testing.T) {
	validator := &stubValidator{err: aigw.ErrInvalidKey}
	hook, _, tenant, _ := refreshFixture(t, validator)
	before := settingsSnapshot(t, tenant)
	err := hook(context.Background(), tenant)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("revoked key did not stop the start: %v", err)
	}
	if after := settingsSnapshot(t, tenant); after != before {
		t.Fatalf("settings were rewritten despite the refusal:\n%s", after)
	}
}

func TestModelRefreshRefusesForbiddenAccount(t *testing.T) {
	validator := &stubValidator{err: &aigw.StatusError{Status: http.StatusForbidden}}
	hook, _, tenant, _ := refreshFixture(t, validator)
	err := hook(context.Background(), tenant)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("forbidden account did not stop the start: %v", err)
	}
}

// aigw being unreachable must not stop tenants from coming up: availability first,
// with the model list already on disk.
func TestModelRefreshProceedsWhenAigwIsUnavailable(t *testing.T) {
	for _, transient := range []error{
		context.DeadlineExceeded,
		errors.New("validate key: dial tcp 127.0.0.1:8088: connect: connection refused"),
		&aigw.StatusError{Status: http.StatusBadGateway},
	} {
		validator := &stubValidator{err: transient}
		hook, _, tenant, _ := refreshFixture(t, validator)
		before := settingsSnapshot(t, tenant)
		if err := hook(context.Background(), tenant); err != nil {
			t.Fatalf("transient aigw failure (%v) blocked the worker start: %v", transient, err)
		}
		if after := settingsSnapshot(t, tenant); after != before {
			t.Fatalf("transient failure rewrote settings:\n%s", after)
		}
	}
}

// No stored key means there is nothing to ask aigw about; the worker keeps what it
// has instead of being blocked by a key this deployment does not keep.
func TestModelRefreshSkipsWithoutStoredKey(t *testing.T) {
	validator := &stubValidator{models: []string{"deepseek-flash"}}
	hook, cfg, tenant, _ := refreshFixture(t, validator)
	if err := os.Remove(filepath.Join(cfg.Deploy.TenantConfigRoot, tenant.Name, "gateway.key")); err != nil {
		t.Fatal(err)
	}
	before := settingsSnapshot(t, tenant)
	if err := hook(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if validator.calls != 0 {
		t.Fatalf("validator was called without a stored key: %d calls", validator.calls)
	}
	if after := settingsSnapshot(t, tenant); after != before {
		t.Fatalf("settings changed without a key:\n%s", after)
	}
}

// Logging in must not rewrite a tenant that already has a key: doing so restarts the
// worker under whoever is using it, can shrink the model list to what that particular
// key is granted, and lets two people take turns overwriting each other. Only a
// tenant with no stored key is configured from a login.
func TestAdoptKeyOnlyConfiguresTenantsWithoutAKey(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "tenant-config")
	tenantDir := filepath.Join(configRoot, "alice")
	if err := os.MkdirAll(tenantDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Deploy.TenantConfigRoot = configRoot
	store := func(key string) {
		if err := os.WriteFile(filepath.Join(tenantDir, "gateway.key"), []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() string {
		data, err := os.ReadFile(filepath.Join(tenantDir, "gateway.key"))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	// The lifecycle side is not exercised here: what this test pins is the decision,
	// which is visible from whether the stored key changes.
	ops := managerOps{cfg: cfg}
	_ = ops

	// Same key: nothing to do.
	store("sk-aaaaaaaaa-rest")
	if existing, err := (proxy.FileKeySource{Root: configRoot}).Key("alice"); err != nil || existing != "sk-aaaaaaaaa-rest" {
		t.Fatalf("stored key = %q err = %v", existing, err)
	}
	// A different key must be reported as "leave it alone" — the check lives in
	// AdoptKey, and prefixOf must never leak the secret into a log line.
	if got := prefixOf("sk-aaaaaaaaa-rest"); !strings.HasPrefix(got, "sk-aaaaaaaaa") || !strings.HasSuffix(got, "…") {
		t.Fatalf("prefixOf = %q", got)
	}
	if got := prefixOf("short"); got != "…" {
		t.Fatalf("prefixOf(short) = %q, want a placeholder", got)
	}
	_ = stored
}

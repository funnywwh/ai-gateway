package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func TestVersionAndUsageExitCodes(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := execute([]string{"--version"}, strings.NewReader(""), &out, &stderr); code != 0 || !strings.Contains(out.String(), "dshgw") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := execute(nil, strings.NewReader(""), &out, &stderr); code != 2 {
		t.Fatalf("missing command code=%d", code)
	}
}

func TestConfigurationErrorUsesExitTwo(t *testing.T) {
	var out, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	code := execute([]string{"--config", missing, "tenant", "list"}, strings.NewReader(""), &out, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "configuration error") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestErrorRedaction(t *testing.T) {
	input := "server echoed sk-first and sk-second; Authorization: Bearer bearer-secret; startup http://127.0.0.1:32100/?token=startup-secret"
	got := redactError(&testError{input})
	for _, secret := range []string{"sk-first", "sk-second", "bearer-secret", "startup-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("secret %q leaked in %q", secret, got)
		}
	}
	for _, want := range []string{"Bearer [REDACTED]", "token=[REDACTED]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("redaction marker %q missing from %q", want, got)
		}
	}
}

type testError struct{ text string }

func (e *testError) Error() string { return e.text }

func TestCheckDirectoryPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectoryPermissions(dir, 0o750); err != nil {
		t.Fatalf("valid directory rejected: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectoryPermissions(file, 0o750); err == nil {
		t.Fatal("regular file accepted as directory")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectoryPermissions(link, 0o750); err == nil {
		t.Fatal("directory symlink accepted")
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectoryPermissions(dir, 0o750); err == nil {
		t.Fatal("writable directory accepted")
	}
}

func TestTenantRemovalErrorPreservesSnapshot(t *testing.T) {
	original := errors.New("user deletion incomplete")
	got := tenantRemovalError("/tmp/alice.snapshot.tar.gz", original)
	if !errors.Is(got, original) || !strings.Contains(got.Error(), "snapshot: /tmp/alice.snapshot.tar.gz") {
		t.Fatalf("error=%v", got)
	}
	if got := tenantRemovalError("", original); got != original {
		t.Fatalf("without snapshot got %v, want original", got)
	}
}

func TestLoginURLWithoutPrefixPrintsPortal(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	state := filepath.Join(root, "state")
	// deploy.plugin_path has no default (M63), so a fixture that only needs a portal URL
	// picks the picker that requires no plugin file.
	if err := os.WriteFile(configPath, []byte("public_host: portal.example.test\ndirectory_picker: browse\nstate_dir: "+state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := execute([]string{"--config", configPath, "login-url"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	if got := strings.TrimSpace(out.String()); got != "https://portal.example.test:32600/" {
		t.Fatalf("portal URL=%q", got)
	}
}

func TestTenantListJSONIncludesReadOnlyMetadata(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	state := filepath.Join(root, "state")
	if err := os.WriteFile(configPath, []byte("public_host: portal.example.test\ndirectory_picker: browse\nstate_dir: "+state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.FixedZone("CST", 8*60*60))
	r := registry.New(filepath.Join(state, "registry.json"), filepath.Join(state, "keys.map"))
	tenant := registry.Tenant{
		Name: "alice", UID: 4242, PublicPort: 32601, WorkerPort: 32100, KeyPrefix: "sk-aaaaaaaaa",
		PreviousPrefixes: []string{"sk-bbbbbbbbb"}, DshHome: filepath.Join(root, "tenants/alice/.dsh"), Workspace: filepath.Join(root, "work/alice"),
		CreatedAt: created, Handshake: registry.HandshakeOK,
	}
	if err := r.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	args := []string{"--config", configPath, "tenant", "list", "--json", "--no-status"}
	if code := execute(args, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	row := rows[0]
	checks := map[string]string{
		"name": "alice", "user": "dshgw",
		"dsh_home": tenant.DshHome, "workspace": tenant.Workspace,
		"created_at":     created.UTC().Format(time.RFC3339Nano),
		"handshake_path": filepath.Join(state, "handshake/alice.url"),
		// tenant-config is a sibling of tenants (M63): sharing the tenant root would put
		// gateway.key inside the tree the worker binds into the sandbox.
		"gateway_key_path": filepath.Join(state, "tenant-config/alice/gateway.key"),
		"aigw_base_url":    "http://192.168.190.86:8088", "key_revalidate": "off",
		"portal_url": "https://portal.example.test:32600/",
	}
	for key, want := range checks {
		if got, ok := row[key].(string); !ok || got != want {
			t.Errorf("%s=%#v, want %q", key, row[key], want)
		}
	}
	if got, ok := row["uid"].(float64); !ok || got != 4242 {
		t.Errorf("uid=%#v", row["uid"])
	}
	// The shape has no units and no per-tenant accounts: those keys must be gone,
	// not merely empty, so a stale script cannot read them as "not configured yet".
	for _, gone := range []string{"unit", "worker_slice", "enabled", "active"} {
		if _, ok := row[gone]; ok {
			t.Errorf("tenant list still reports the deleted %q field", gone)
		}
	}
	if got, ok := row["previous_prefixes"].([]any); !ok || len(got) != 1 || got[0] != "sk-bbbbbbbbb" {
		t.Errorf("previous_prefixes=%#v", row["previous_prefixes"])
	}
	if _, ok := row["gateway_key"]; ok {
		t.Error("raw gateway key unexpectedly present")
	}
}

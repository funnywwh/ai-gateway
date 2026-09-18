package tenancy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func backupFixture(t *testing.T) (*Manager, *WorkerRunner, registry.Tenant) {
	t.Helper()
	m, runner, _ := managerFixture(t)
	m.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{tenant.DshHome, tenant.Workspace, filepath.Join(m.Config.Deploy.TenantConfigRoot, tenant.Name)} {
		if err := os.WriteFile(filepath.Join(path, "marker"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m, runner, tenant
}

func archiveFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := files[header.Name]; duplicate {
			t.Fatalf("duplicate archive member: %s", header.Name)
		}
		files[header.Name] = data
	}
}

func TestBackupTenantAndRemovePurge(t *testing.T) {
	m, runner, tenant := backupFixture(t)
	snapshot, err := m.Remove(context.Background(), tenant, true)
	if err != nil {
		t.Fatal(err)
	}
	names, err := ListArchive(snapshot)
	if err != nil || len(names) == 0 {
		t.Fatalf("names=%v err=%v", names, err)
	}
	files := archiveFiles(t, snapshot)
	var manifest archiveManifest
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || manifest.Tenant == nil || manifest.Tenant.Name != tenant.Name || manifest.Tenant.KeyPrefix != tenant.KeyPrefix || manifest.Tenant.UID != tenant.UID {
		t.Fatalf("missing recovery metadata: %#v", manifest)
	}
	if _, ok := m.Registry.Get("alice"); ok {
		t.Fatal("tenant still registered")
	}
	if _, err := os.Stat(tenant.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains: %v", err)
	}
	// Removal stops the worker process and drops its published startup URL: no
	// per-tenant OS identity exists to delete in this shape (M58).
	if runner.Status(tenant).Running {
		t.Fatal("worker process survived removal")
	}
	if _, err := os.Stat(filepath.Join(m.Config.HandshakeDir, tenant.Name+".url")); !os.IsNotExist(err) {
		t.Fatalf("stale handshake survived removal: %v", err)
	}
}

func TestRemoveWithoutPurgeCannotBeDestroyedByRecreate(t *testing.T) {
	m, _, tenant := backupFixture(t)
	if _, err := m.Remove(context.Background(), tenant, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), "alice", "sk-bbbbbbbbb-rest", []string{"m"}, CreateOptions{}); err == nil {
		t.Fatal("recreate adopted retained tenant data")
	}
	if data, err := os.ReadFile(filepath.Join(tenant.Workspace, "marker")); err != nil || string(data) != "original" {
		t.Fatalf("retained workspace destroyed: %q / %v", data, err)
	}
}

func TestBackupSameTimeKeepsIndependentSnapshots(t *testing.T) {
	m, _, tenant := backupFixture(t)
	one, err := m.BackupTenant(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenant.Workspace, "marker"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	two, err := m.BackupTenant(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if one == two || !strings.HasSuffix(one, ".tar.gz") || !strings.HasSuffix(two, ".tar.gz") {
		t.Fatalf("nonunique archive names: %s / %s", one, two)
	}
	if string(archiveFiles(t, one)["workspace/marker"]) != "original" || string(archiveFiles(t, two)["workspace/marker"]) != "changed" {
		t.Fatal("later snapshot overwrote an earlier snapshot")
	}
	for _, path := range []string{one, two} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("archive permissions: %s: %v / %v", path, info, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(one))
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary archives leaked: %v / %v", entries, err)
	}
}

func TestBackupContainsExternalConfiguredPaths(t *testing.T) {
	m, _, _ := managerFixture(t)
	external := t.TempDir()
	m.Config.TenantRoot = filepath.Join(external, "tenants")
	m.Config.HandshakeDir = filepath.Join(external, "handshake")
	m.Config.Deploy.TenantConfigRoot = filepath.Join(external, "config")
	m.Config.SessionPath = filepath.Join(external, "sessions.json")
	m.Config.ActivityPath = filepath.Join(external, "activity.json")
	m.Config.AuditPath = filepath.Join(external, "audit.jsonl")
	if err := os.MkdirAll(m.Config.HandshakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	markers := map[string]string{
		filepath.Join(tenant.DshHome, "marker"):                            "tenant state",
		filepath.Join(m.Config.HandshakeDir, "alice.url"):                  "handshake token",
		filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice", "marker"): "tenant config",
		m.Config.SessionPath: "session state", m.Config.ActivityPath: "activity state", m.Config.AuditPath: "audit state",
	}
	for path, data := range markers {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := m.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files := archiveFiles(t, snapshot)
	var manifest archiveManifest
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	for path, expected := range markers {
		found := false
		for _, root := range manifest.Roots {
			if root.Present && pathWithin(root.Path, path) {
				rel, _ := filepath.Rel(root.Path, path)
				member := filepath.ToSlash(filepath.Join(root.Name, rel))
				if string(files[member]) == expected {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("configured data missing from archive/manifest: %s", path)
		}
	}
	if _, ok := files["registry.json"]; !ok {
		t.Fatal("external canonical registry omitted")
	}
}

// Remove b after ReadDir has enumerated it but before the walk reaches it.
// This deterministically exercises ENOENT below an existing archive root.
// That error used to be swallowed as if the optional root itself were absent.
type removeDuringArchive struct {
	bytes.Buffer
	onHeader func(string)
}

func (w *removeDuringArchive) Write(data []byte) (int, error) {
	if len(data) == 512 && w.onHeader != nil {
		w.onHeader(strings.TrimRight(string(data[:100]), "\x00"))
	}
	return w.Buffer.Write(data)
}

func TestArchiveRejectsDisappearingDescendant(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed := false
	writer := &removeDuringArchive{onHeader: func(name string) {
		if name == "data/a" {
			if err := os.Remove(filepath.Join(root, "b")); err != nil {
				t.Fatal(err)
			}
			removed = true
		}
	}}
	tw := tar.NewWriter(writer)
	err := writeArchiveContents(tw, []archiveRoot{{root, "data", false}}, nil)
	if !removed || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete snapshot accepted: removed=%t err=%v", removed, err)
	}
}

func TestArchiveRefusesDestinationWithinSource(t *testing.T) {
	root := t.TempDir()
	path, err := writeArchive(filepath.Join(root, "backups"), "test.tar.gz", []archiveRoot{{root, "data", true}}, nil)
	if err == nil || path != "" || !strings.Contains(err.Error(), "inside archive source") {
		t.Fatalf("recursive self-backup accepted: path=%s err=%v", path, err)
	}
}

// Mandatory tenant data that is already gone must stop the removal before any
// purge. The worker cannot be brought back in this scenario — its workspace is
// what disappeared, and a worker without a workspace is not startable — so the
// contract here is: the registry entry survives and the failure is reported
// rather than reported as a clean removal.
func TestMissingTenantDataPreventsPurgeAndKeepsRegistry(t *testing.T) {
	m, _, tenant := backupFixture(t)
	if err := os.RemoveAll(tenant.Workspace); err != nil {
		t.Fatal(err)
	}
	path, err := m.Remove(context.Background(), tenant, true)
	if err == nil || path != "" {
		t.Fatalf("incomplete mandatory snapshot accepted: %s / %v", path, err)
	}
	if _, ok := m.Registry.Get(tenant.Name); !ok {
		t.Fatal("tenant removed without a complete snapshot")
	}
}

// When the snapshot fails but the tenant's data is intact, the rollback must put
// the tenant back the way it was: registered, with its worker running again. A
// failed removal may not leave a live tenant down.
func TestRemovalRollbackRestartsWorkerWhenSnapshotFails(t *testing.T) {
	m, runner, tenant := backupFixture(t)
	// The per-tenant configuration directory is a mandatory archive root: its
	// absence fails the snapshot while the workspace stays usable.
	if err := os.RemoveAll(filepath.Join(m.Config.Deploy.TenantConfigRoot, tenant.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(context.Background(), tenant, false); err == nil {
		t.Fatal("removal succeeded despite an incomplete snapshot")
	}
	if _, ok := m.Registry.Get(tenant.Name); !ok {
		t.Fatal("failed removal dropped the tenant from the registry")
	}
	if !runner.Status(tenant).Running {
		t.Fatal("rollback did not restart the tenant's worker")
	}
	if data, err := os.ReadFile(filepath.Join(tenant.Workspace, "marker")); err != nil || string(data) != "original" {
		t.Fatalf("retained workspace damaged by the failed removal: %q / %v", data, err)
	}
}

func TestRemoveRejectsRegistryPathsOutsideConfiguredTenant(t *testing.T) {
	m, runner, tenant := backupFixture(t)
	tenant.DshHome = filepath.Join(t.TempDir(), ".dsh")
	if err := m.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(context.Background(), tenant, true); err == nil || !strings.Contains(err.Error(), "paths do not match configured tenant roots") {
		t.Fatalf("unsafe registry path accepted: %v", err)
	}
	if !runner.Status(tenant).Running {
		t.Fatal("refused removal still stopped the tenant's worker")
	}
}

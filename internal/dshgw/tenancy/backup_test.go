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

func backupFixture(t *testing.T) (*Manager, *fakeRunner, registry.Tenant) {
	t.Helper()
	m, r := managerFixture(t)
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
	r.commands = nil
	return m, r, tenant
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
	m, r, tenant := backupFixture(t)
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
	joined := commandsText(r.commands)
	if !strings.Contains(joined, "systemctl disable --now dsh-worker@alice.service") || !strings.Contains(joined, "userdel dsh-alice") {
		t.Fatalf("%s", joined)
	}
}

func TestRemoveWithoutPurgeCannotBeDestroyedByRecreate(t *testing.T) {
	m, r, tenant := backupFixture(t)
	if _, err := m.Remove(context.Background(), tenant, false); err != nil {
		t.Fatal(err)
	}
	r.commands = nil
	if _, err := m.Create(context.Background(), "alice", "sk-bbbbbbbbb-rest", []string{"m"}, CreateOptions{}); err == nil {
		t.Fatal("recreate adopted retained tenant data")
	}
	if len(r.commands) != 0 {
		t.Fatalf("recreate ran commands: %s", commandsText(r.commands))
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
	m, _ := managerFixture(t)
	external := t.TempDir()
	m.Config.TenantRoot = filepath.Join(external, "tenants")
	m.Config.HandshakeDir = filepath.Join(external, "handshake")
	m.Config.Deploy.TenantConfigRoot = filepath.Join(external, "config")
	m.Config.SessionPath = filepath.Join(external, "sessions.json")
	m.Config.ActivityPath = filepath.Join(external, "activity.json")
	m.Config.AuditPath = filepath.Join(external, "audit.jsonl")
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Config.HandshakeDir, 0o750); err != nil {
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

func TestMissingTenantDataPreventsPurgeAndRestoresUnit(t *testing.T) {
	m, r, tenant := backupFixture(t)
	if err := os.RemoveAll(tenant.Workspace); err != nil {
		t.Fatal(err)
	}
	path, err := m.Remove(context.Background(), tenant, true)
	if err == nil || path != "" {
		t.Fatalf("incomplete mandatory snapshot accepted: %s / %v", path, err)
	}
	if _, ok := m.Registry.Get(tenant.Name); !ok {
		t.Fatal("tenant removed without complete snapshot")
	}
	joined := commandsText(r.commands)
	if strings.Contains(joined, "userdel") || !strings.Contains(joined, "systemctl enable dsh-worker@alice.service") || !strings.Contains(joined, "systemctl start dsh-worker@alice.service") {
		t.Fatalf("bad rollback: %s", joined)
	}
}

func TestStatusFailurePreventsBackupAndRemoveMutation(t *testing.T) {
	for _, operation := range []string{"backup", "remove"} {
		t.Run(operation, func(t *testing.T) {
			m, r, tenant := backupFixture(t)
			r.fail = "systemctl show"
			var err error
			if operation == "backup" {
				_, err = m.Backup(context.Background())
			} else {
				_, err = m.Remove(context.Background(), tenant, true)
			}
			if err == nil {
				t.Fatal("unknown systemd status accepted")
			}
			for _, c := range r.commands {
				if c.Path != "systemctl" || c.Args[0] != "show" {
					t.Fatalf("mutated despite unknown status: %s", commandsText(r.commands))
				}
			}
			if _, err := os.Stat(m.Config.Deploy.BackupDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("backup performed despite unknown status: %v", err)
			}
		})
	}
}

func TestBackupRestoresPartiallyStoppedGateway(t *testing.T) {
	m, r, _ := backupFixture(t)
	primary := errors.New("gateway stop failed after stopping")
	r.hook = func(ctx context.Context, c Command) ([]byte, error, bool) {
		if c.Path == "systemctl" && strings.Join(c.Args, " ") == "stop dshgw.service" {
			return nil, primary, true
		}
		return nil, nil, false
	}
	path, err := m.Backup(context.Background())
	if !errors.Is(err, primary) || path != "" {
		t.Fatalf("path=%s err=%v", path, err)
	}
	joined := commandsText(r.commands)
	if !strings.Contains(joined, "systemctl start dshgw.service") || strings.Contains(joined, "systemctl stop dsh-worker") || strings.Contains(joined, "systemctl start dsh-worker") {
		t.Fatalf("wrong recovery set: %s", joined)
	}
}

func TestBackupReportsRestartFailureWithSnapshot(t *testing.T) {
	m, r, _ := backupFixture(t)
	r.fail = "systemctl start"
	path, err := m.Backup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restore unit") || path == "" {
		t.Fatalf("successful backup hid failed recovery: path=%s err=%v", path, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestBackupPreservesStoppedWorkers(t *testing.T) {
	m, r, _ := backupFixture(t)
	r.hook = func(_ context.Context, c Command) ([]byte, error, bool) {
		if c.Path == "systemctl" && c.Args[0] == "show" && c.Args[len(c.Args)-1] == "dsh-worker@alice.service" {
			return systemdStatus("inactive", "disabled"), nil, true
		}
		return nil, nil, false
	}
	if _, err := m.Backup(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := commandsText(r.commands)
	if strings.Contains(joined, "stop dsh-worker") || strings.Contains(joined, "start dsh-worker") {
		t.Fatalf("stopped worker was started by backup: %s", joined)
	}
}

func TestRemoveRestoresPartiallyDisabledRuntimeUnit(t *testing.T) {
	m, r, tenant := backupFixture(t)
	primary := errors.New("disable partially failed")
	restore := errors.New("restore start failed")
	r.hook = func(_ context.Context, c Command) ([]byte, error, bool) {
		if c.Path == "systemctl" {
			switch c.Args[0] {
			case "show":
				return systemdStatus("active", "enabled-runtime"), nil, true
			case "disable":
				return nil, primary, true
			case "start":
				return nil, restore, true
			}
		}
		return nil, nil, false
	}
	_, err := m.Remove(context.Background(), tenant, true)
	if !errors.Is(err, primary) || !errors.Is(err, restore) {
		t.Fatalf("rollback errors lost: %v", err)
	}
	joined := commandsText(r.commands)
	if !strings.Contains(joined, "systemctl disable --now --runtime dsh-worker@alice.service") {
		t.Fatalf("runtime disablement not preserved: %s", joined)
	}
	if !strings.Contains(joined, "systemctl enable --runtime dsh-worker@alice.service") {
		t.Fatalf("runtime enablement not preserved: %s", joined)
	}
	if _, ok := m.Registry.Get(tenant.Name); !ok {
		t.Fatal("registry mutated after failed disable")
	}
}

func TestBackupCancellationStillRestoresAttemptedUnits(t *testing.T) {
	m, r, _ := backupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restored := map[string]bool{}
	r.hook = func(runCtx context.Context, c Command) ([]byte, error, bool) {
		if c.Path != "systemctl" {
			return nil, nil, false
		}
		if strings.Join(c.Args, " ") == "stop dsh-worker@alice.service" {
			cancel()
			return nil, ctx.Err(), true
		}
		if c.Args[0] == "start" {
			if runCtx.Err() != nil {
				t.Errorf("recovery reused canceled context: %v", runCtx.Err())
			}
			restored[c.Args[1]] = true
		}
		return nil, nil, false
	}
	_, err := m.Backup(ctx)
	if !errors.Is(err, context.Canceled) || !restored["dsh-worker@alice.service"] || !restored["dshgw.service"] {
		t.Fatalf("recovery after cancellation: %v / %v", restored, err)
	}
}

func TestUserdelFailureDoesNotResurrectPotentiallyDeletedIdentity(t *testing.T) {
	m, r, tenant := backupFixture(t)
	r.fail = "userdel"
	path, err := m.Remove(context.Background(), tenant, true)
	if err == nil || path == "" || !strings.Contains(err.Error(), "data retained") {
		t.Fatalf("path=%s err=%v", path, err)
	}
	if _, ok := m.Registry.Get(tenant.Name); ok {
		t.Fatal("restored a route to a possibly deleted identity")
	}
	if _, err := os.Stat(filepath.Join(tenant.Workspace, "marker")); err != nil {
		t.Fatalf("purged after failed userdel: %v", err)
	}
	joined := commandsText(r.commands)
	if strings.Contains(joined, "systemctl start dsh-worker") || strings.Contains(joined, "systemctl enable dsh-worker") {
		t.Fatalf("restarted potentially deleted identity: %s", joined)
	}
}

func TestRemoveRejectsRegistryPathsOutsideConfiguredTenant(t *testing.T) {
	m, r, tenant := backupFixture(t)
	tenant.DshHome = filepath.Join(t.TempDir(), ".dsh")
	if err := m.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Remove(context.Background(), tenant, true); err == nil {
		t.Fatal("unsafe registry paths accepted")
	}
	if len(r.commands) != 0 {
		t.Fatalf("unsafe path reached lifecycle commands: %s", commandsText(r.commands))
	}
}

package browsermount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func TestPersistentRecordRollsBackFailedMount(t *testing.T) {
	root := t.TempDir()
	mountErr := errors.New("mount failed")
	s := NewWithState(nil, func(string, fs.Backend) (Mounted, error) { return nil, mountErr }, root)
	tenant := registry.Tenant{Name: "alice", Workspace: filepath.Join(root, "workspace")}
	if err := os.MkdirAll(tenant.Workspace, 0700); err != nil { t.Fatal(err) }
	if _, err := s.open(tenant, "owner", "docs", true); !errors.Is(err, mountErr) { t.Fatalf("open error = %v", err) }
	entries, err := os.ReadDir(filepath.Join(root, "browser-mounts"))
	if err != nil { t.Fatal(err) }
	if len(entries) != 0 { t.Fatalf("failed mount left records: %v", entries) }
}

func TestRecordPathValidation(t *testing.T) {
	root := t.TempDir()
	s := NewWithState(nil, func(string, fs.Backend) (Mounted, error) { return &fakeMount{}, nil }, root)
	if err := os.MkdirAll(filepath.Join(root, "browser-mounts"), 0700); err != nil { t.Fatal(err) }
	bad := mountRecord{ID: "x", Workspace: "/tmp/work", Path: "/tmp/work/browser/other"}
	if validRecordPath(bad) { t.Fatal("accepted mismatched record path") }
	_ = s.DropTenant(context.Background(), "none")
}

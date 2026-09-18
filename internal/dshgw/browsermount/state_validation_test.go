package browsermount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func TestCleanupStaleRejectsMaliciousRecords(t *testing.T) {
	for _, kind := range []string{"wrong-tenant", "wrong-workspace", "wrong-path", "wrong-filename", "symlink-record", "symlink-mountpoint", "public-record", "invalid-state", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			workspace := filepath.Join(base, "workspace")
			if err := os.MkdirAll(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("a", 48)
			path, err := prepare(workspace, id)
			if err != nil {
				t.Fatal(err)
			}
			tenant := registry.Tenant{Name: "alice", UID: 1001, PublicPort: 32101, WorkerPort: 32102, Workspace: workspace, DshHome: filepath.Join(base, "home"), CreatedAt: time.Now(), KeyPrefix: "key-prefix12", Handshake: registry.HandshakePending}
			reg := registry.New(filepath.Join(base, "registry.json"), filepath.Join(base, "registry.keys.map"))
			if err := reg.Put(tenant); err != nil {
				t.Fatal(err)
			}
			s := NewWithState(nil, nil, base)
			s.SetRegistry(reg)
			if err := s.AcquireLock(); err != nil {
				t.Fatal(err)
			}
			defer s.ReleaseLock()
			r := mountRecord{ID: id, Tenant: tenant.Name, Workspace: workspace, Path: path, State: "ready"}
			filename := s.recordPath(id)
			outside := filepath.Join(base, "untouched")
			if err := os.WriteFile(outside, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "wrong-tenant":
				r.Tenant = "bob"
			case "wrong-workspace":
				other := filepath.Join(base, "other")
				if err := os.MkdirAll(other, 0700); err != nil {
					t.Fatal(err)
				}
				otherPath, err := prepare(other, id)
				if err != nil {
					t.Fatal(err)
				}
				r.Workspace, r.Path = other, otherPath
			case "wrong-path":
				r.Path = outside
			case "wrong-filename":
				filename = s.recordPath(strings.Repeat("b", 48))
			case "invalid-state":
				r.State = "other"
			case "symlink-mountpoint":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "oversized" {
				b = append(b, []byte(strings.Repeat(" ", 17000))...)
			}
			if kind == "symlink-record" {
				if err := os.Symlink(outside, filename); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filename, b, 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "public-record" {
					if err := os.Chmod(filename, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.CleanupStale(); err == nil {
				t.Fatal("malicious record silently accepted")
			}
			if _, err := os.Lstat(filename); err != nil {
				t.Fatal("rejected record deleted", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("rejected mountpoint deleted", err)
			}
			b, err = os.ReadFile(outside)
			if err != nil || string(b) != "sentinel" {
				t.Fatal("outside content touched", err)
			}
		})
	}
}

func TestCleanupStaleRetriesRecordWithoutMount(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 48)
	path, err := prepare(workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	tenant := registry.Tenant{Name: "alice", UID: 1001, PublicPort: 32101, WorkerPort: 32102, Workspace: workspace, DshHome: filepath.Join(base, "home"), CreatedAt: time.Now(), KeyPrefix: "key-prefix12", Handshake: registry.HandshakePending}
	reg := registry.New(filepath.Join(base, "registry.json"), filepath.Join(base, "registry.keys.map"))
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	s := NewWithState(nil, nil, base)
	s.SetRegistry(reg)
	if err := s.AcquireLock(); err != nil {
		t.Fatal(err)
	}
	defer s.ReleaseLock()
	if err := s.writeRecord(mountRecord{ID: id, Tenant: tenant.Name, Workspace: workspace, Path: path, State: "preparing"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupStale(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale empty directory remains", err)
	}
	if _, err := os.Stat(s.recordPath(id)); !os.IsNotExist(err) {
		t.Fatal("stale record remains", err)
	}
}

func TestRecordDirectoryRejectsSymlinks(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "browser-mounts")); err != nil {
		t.Fatal(err)
	}
	s := NewWithState(nil, nil, base)
	if err := s.AcquireLock(); err == nil {
		defer s.ReleaseLock()
		t.Fatal("symlink state directory accepted")
	}
}

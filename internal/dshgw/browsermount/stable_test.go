package browsermount

import (
	"context"

	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// recordingMounts hands out one independent mount per mount point, so a test can tell which
// mount a teardown actually released instead of seeing one shared flag.
type recordingMounts struct {
	mu     sync.Mutex
	byPath map[string]*fakeMount
}

func (r *recordingMounts) mount(path string, _ fs.Backend) (Mounted, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := &fakeMount{}
	r.byPath[path] = m
	return m, nil
}

func (r *recordingMounts) released(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.byPath[path]
	return m != nil && m.closed
}

func (r *recordingMounts) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byPath)
}

// stableFixture is one account and a mount factory that records per-path teardown.
func stableFixture(t *testing.T) (*Service, registry.Tenant, *recordingMounts) {
	t.Helper()
	mounts := &recordingMounts{byPath: make(map[string]*fakeMount)}
	s := NewWithMount(func(context.Context, registry.Tenant) error { return nil }, mounts.mount)
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	t.Cleanup(func() { _ = s.DropTenant(context.Background(), tenant.Name) })
	return s, tenant, mounts
}

const stableKeyA = "0123456789abcdef0123456789abcdef"
const stableKeyB = "fedcba9876543210fedcba9876543210"

// The whole point of a client-supplied key: the virtual path belongs to the LOCAL directory,
// not to one mount. Disconnecting and mounting again must land on the same path, because that
// path is what DSH's own workspace entry (and the sessions grouped under it) is keyed by.
func TestStableKeyKeepsTheSameVirtualPathAcrossMounts(t *testing.T) {
	s, tenant, mounts := stableFixture(t)
	first, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tenant.Workspace, "browser", stableKeyA)
	if first.path != want {
		t.Fatalf("mount point = %q, want the stable path %q", first.path, want)
	}
	if err := s.close(first); err != nil {
		t.Fatal(err)
	}
	if !mounts.released(want) {
		t.Fatal("close did not unmount the stable mount point")
	}
	// Disconnect is not removal: the empty directory is the virtual path and stays.
	info, err := os.Stat(want)
	if err != nil || !info.IsDir() {
		t.Fatalf("stable mount point did not survive the disconnect: %v", err)
	}
	// Nothing is mounted there any more, so there is nothing left for startup cleanup: the
	// record goes, the directory stays.
	if _, err := os.Stat(s.recordPath(tenant.Name, stableKeyA)); !os.IsNotExist(err) {
		t.Fatalf("record of a closed mount remains: %v", err)
	}
	second, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close(second) }()
	if second.path != want {
		t.Fatalf("reconnected mount point = %q, want the same stable path %q", second.path, want)
	}
}

// A key that a share still holds — serving, or waiting out its reconnect grace — names a path
// that already carries a FUSE mount. Mounting it again would stack a second mount on one
// directory and let two browsers write the same local files at once.
func TestStableKeyIsRefusedWhileAShareHoldsIt(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	first, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA}); err == nil || !strings.Contains(err.Error(), "directory key already mounted") {
		t.Fatalf("a second mount of the same key was allowed: %v", err)
	}
	// The grace window is not an exception: the kernel mount and its mount point are still
	// there, which is exactly what a resume needs.
	first.disconnect()
	if _, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA}); err == nil {
		t.Fatal("a mount waiting out its grace window did not hold its key")
	}
	if err := s.close(first); err != nil {
		t.Fatal(err)
	}
	// Once the mount is gone the key is free again, and only then.
	again, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatalf("the key was not released by a real teardown: %v", err)
	}
	if err := s.close(again); err != nil {
		t.Fatal(err)
	}
	// Another account's identical key is a different path and needs no coordination.
	other := registry.Tenant{Name: "bob", Workspace: t.TempDir()}
	foreign, err := s.openWith(other, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatalf("another account could not use the same key: %v", err)
	}
	if foreign.path == first.path {
		t.Fatalf("two accounts share one mount point: %q", foreign.path)
	}
	if err := s.close(foreign); err != nil {
		t.Fatal(err)
	}
}

// The operator removing a folder for good is the one action that releases the stable path.
func TestPurgeReleasesTheStableMountPoint(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	sh, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.closeContext(context.Background(), sh, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sh.path); !os.IsNotExist(err) {
		t.Fatalf("purged mount point remains: %v", err)
	}
	if _, err := os.Stat(s.recordPath(tenant.Name, stableKeyA)); !os.IsNotExist(err) {
		t.Fatalf("purged mount record remains: %v", err)
	}
	// A share without a stable key keeps the original behaviour: its mount point was
	// per-mount scratch and a plain close still removes it.
	legacy, err := s.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	path := legacy.path
	if err := s.close(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a random-id mount point was kept: %v", err)
	}
}

// The key becomes a directory name, so its shape is validated instead of sanitised. The legacy
// 32/48 hex ids pass the same rule (see TestDirectoryKeys), so a folder saved before directory
// names keeps its path; what is refused here is anything that is not exactly one safe segment.
func TestInvalidDirectoryKeyIsRefused(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	for _, key := range []string{
		"..", ".", "../etc", "/absolute", "nested/name", `back\slash`, ".hidden",
		" padded", "padded ", "new\nline", "nul\x00", "delete\x7f", strings.Repeat("a", 256),
	} {
		if _, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: key}); err == nil || !strings.Contains(err.Error(), "invalid directory key") {
			t.Fatalf("invalid directory key %q was accepted: %v", key, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tenant.Workspace, "browser")); !os.IsNotExist(err) && err != nil {
		t.Fatalf("a refused key touched the workspace: %v", err)
	}
}

// A reused mount point is reused only when it is exactly what prepare itself would create.
func TestStableMountPointMustBeEmptyAndPrivate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"not empty", func(t *testing.T, path string) {
			if err := os.WriteFile(filepath.Join(path, "leftover"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong mode", func(t *testing.T, path string) { _ = os.Chmod(path, 0755) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, tenant, _ := stableFixture(t)
			path := filepath.Join(tenant.Workspace, "browser", stableKeyA)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, path)
			if _, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA}); err == nil {
				t.Fatal("a mount point that is not reusable was mounted over")
			}
		})
	}
}

// Two directories of one account are two mounts: closing one must not disturb the other.
func TestTwoStableMountsOfOneAccountAreIndependent(t *testing.T) {
	s, tenant, mounts := stableFixture(t)
	first, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.openWith(tenant, "owner", openRequest{Name: "photos", Writable: true, Key: stableKeyB})
	if err != nil {
		t.Fatal(err)
	}
	if first.path == second.path {
		t.Fatal("two directories share one mount point")
	}
	paths := s.MountsFor(tenant.Name)
	if len(paths) != 2 {
		t.Fatalf("worker profile would bind %v, want both mounts", paths)
	}
	if err := s.close(second); err != nil {
		t.Fatal(err)
	}
	if !mounts.released(second.path) {
		t.Fatal("the closed mount was not unmounted")
	}
	if mounts.released(first.path) {
		t.Fatal("closing one directory unmounted the other")
	}
	remaining := s.MountsFor(tenant.Name)
	if len(remaining) != 1 || remaining[0] != first.path {
		t.Fatalf("remaining mounts = %v, want only %q", remaining, first.path)
	}
	// The survivor still serves: queue one operation and answer it.
	answered := make(chan fs.Response, 1)
	go func() {
		response, err := first.Call(context.Background(), fs.Request{Op: "stat", Path: ""})
		if err != nil {
			answered <- fs.Response{}
			return
		}
		answered <- response
	}()
	value, err := s.dispatch(context.Background(), "poll", tenant, "owner", payload{Token: first.token})
	if err != nil {
		t.Fatal(err)
	}
	request := value.(map[string]any)["requests"].([]fs.Request)[0]
	if _, err := s.dispatch(context.Background(), "respond", tenant, "owner", payload{Token: first.token, ID: request.ID, Result: fs.Response{OK: true, Value: fs.Value{Kind: "directory"}}}); err != nil {
		t.Fatal(err)
	}
	if response := <-answered; response.Value.Kind != "directory" {
		t.Fatalf("the surviving mount did not answer: %+v", response)
	}
}

// The per-account bound is a user-facing condition with its own message; the global bound and
// shutdown keep theirs.
func TestPerAccountMountLimitIsFour(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	keys := []string{stableKeyA, stableKeyB, "11111111111111111111111111111111", "22222222222222222222222222222222"}
	for _, key := range keys {
		if _, err := s.openWith(tenant, "owner", openRequest{Name: "dir", Writable: true, Key: key}); err != nil {
			t.Fatalf("mount %s refused below the limit: %v", key, err)
		}
	}
	_, err := s.openWith(tenant, "owner", openRequest{Name: "fifth", Writable: true, Key: "33333333333333333333333333333333"})
	if err == nil || !strings.Contains(err.Error(), "per-account browser directory limit reached: 4") {
		t.Fatalf("the fifth mount was not refused with the per-account message: %v", err)
	}
	other := registry.Tenant{Name: "bob", Workspace: t.TempDir()}
	if _, err := s.openWith(other, "owner", openRequest{Name: "dir", Writable: true, Key: stableKeyA}); err != nil {
		t.Fatalf("another account was affected by this account's limit: %v", err)
	}
}

// Startup cleanup after a crash releases a stable mount but keeps its empty mount point, so
// the workspace entry that maps to that local directory still resolves.
func TestCleanupStaleKeepsAStableMountPoint(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := prepare(workspace, stableKeyA)
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
	if err := s.writeRecord(mountRecord{ID: stableKeyA, Tenant: tenant.Name, Workspace: workspace, Path: path, State: "ready", Persistent: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupStale(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("a stable mount point was removed by startup cleanup: %v", err)
	}
	if _, err := os.Stat(s.recordPath(tenant.Name, stableKeyA)); !os.IsNotExist(err) {
		t.Fatalf("the stale record was kept: %v", err)
	}
	// The kept directory is immediately reusable by the next mount of that key.
	if err := reusableMountPoint(path); err != nil {
		t.Fatalf("the kept mount point is not reusable: %v", err)
	}
}

// Deleting a folder AFTER disconnecting it is the ordinary order of operations: the operator
// disconnects, sees the workspace go quiet, then removes the folder. No live share is left to
// purge through, so the tombstoned capability is what still knows which path that key owned.
func TestPurgeAfterDisconnectReleasesTheStableMountPoint(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	sh, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	token, path := sh.token, sh.path
	if err := s.close(sh); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stable mount point did not survive the disconnect: %v", err)
	}
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: token, Purge: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the purge did not release the stable mount point: %v", err)
	}
}

// A key is meant to be reused, so a purge that names an OLD capability must never delete the
// directory a NEWER mount of the same key is serving.
func TestStalePurgeNeverRemovesALiveMountPoint(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	first, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	staleToken, path := first.token, first.path
	if err := s.close(first); err != nil {
		t.Fatal(err)
	}
	second, err := s.openWith(tenant, "owner", openRequest{Name: "docs", Writable: true, Key: stableKeyA})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close(second) }()
	if second.path != path {
		t.Fatalf("the reused key mounted at %q instead of %q", second.path, path)
	}
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: staleToken, Purge: true}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("a stale purge removed the live mount point: %v", err)
	}
}

// Even a tombstone is not authority to delete arbitrary paths: the path must be the private
// mount point of this tenant's own browser container.
func TestPurgeRefusesPathsOutsideTheBrowserContainer(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	const token = "tombstoned-token"
	s.mu.Lock()
	s.tombstones[token] = tombstone{owner: "owner", tenant: tenant.Name, until: time.Now().Add(time.Minute), path: outside, persistent: true}
	s.mu.Unlock()
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: token, Purge: true}); err == nil {
		t.Fatal("a purge outside the browser container was accepted")
	}
	if info, err := os.Stat(outside); err != nil || !info.IsDir() {
		t.Fatalf("a refused purge touched the path: %v", err)
	}
}

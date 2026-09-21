package browsermount

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// The key is the mount point's basename, so what it may be IS the picker's question: the local
// directory's own name (what a person recognises, with spaces and non-ASCII included), or the
// legacy random id an already saved folder still carries.
func TestDirectoryKeys(t *testing.T) {
	for _, key := range []string{
		"aosp", "My Docs", "我的项目", "Downloads-2", "a.b", "-leading-dash",
		stableKeyA, strings.Repeat("a", 48), strings.Repeat("a", 255),
	} {
		if !validDirectoryKey(key) {
			t.Fatalf("valid directory key %q was refused", key)
		}
	}
	for _, key := range []string{
		"", ".", "..", ".hidden", "a/b", `a\b`, "a\nb", " a", "a ", "\ta", "a\x7f", strings.Repeat("a", 256),
	} {
		if validDirectoryKey(key) {
			t.Fatalf("invalid directory key %q was accepted", key)
		}
	}
}

// A local directory name is what the mount point is named after, and that path is the mapping:
// disconnect and mount again must land on the SAME name, because that is the path DSH's own
// workspace entry (and the sessions grouped under it) is keyed by.
func TestNameKeyMountsAtTheLocalDirectoryName(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	first, err := s.openWith(tenant, "owner", openRequest{Name: "aosp", Writable: true, Key: "aosp"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tenant.Workspace, "browser", "aosp")
	if first.path != want {
		t.Fatalf("mount point = %q, want the local directory's own name %q", first.path, want)
	}
	if err := s.close(first); err != nil {
		t.Fatal(err)
	}
	// Disconnect is not removal: the empty directory is the virtual path and stays.
	if err := reusableMountPoint(want); err != nil {
		t.Fatalf("the named mount point did not survive the disconnect: %v", err)
	}
	second, err := s.openWith(tenant, "owner", openRequest{Name: "aosp", Writable: true, Key: "aosp"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close(second) }()
	if second.path != want {
		t.Fatalf("reconnected mount point = %q, want the same named path %q", second.path, want)
	}
}

// `allocate` is the one step before a saved folder's first mount. It is read-only, it never
// hands out a name some local directory of this account already owns, and an unusable proposal
// still answers with something mountable rather than leaving the folder unmountable.
func TestAllocateNameArbitratesWithoutCreatingAnything(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	container := filepath.Join(tenant.Workspace, "browser")
	if got := s.allocateName(tenant, "aosp"); got != "aosp" {
		t.Fatalf("a free name was not answered as asked: %q", got)
	}
	if _, err := os.Stat(container); !os.IsNotExist(err) {
		t.Fatalf("allocate created something: %v", err)
	}
	// A live mount holds its name.
	live, err := s.openWith(tenant, "owner", openRequest{Name: "aosp", Writable: true, Key: "aosp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.allocateName(tenant, "aosp"); got != "aosp-2" {
		t.Fatalf("a mounted name was handed out again: %q", got)
	}
	// A disconnected folder's name is held by its directory, which outlives the mount (and the
	// record): that empty directory is the virtual path another folder must not take over.
	if err := s.close(live); err != nil {
		t.Fatal(err)
	}
	if got := s.allocateName(tenant, "aosp"); got != "aosp-2" {
		t.Fatalf("a kept mount point's name was handed out again: %q", got)
	}
	// Suffixes are not infinite: past the readable ones an opaque tail still names the local
	// directory, and the result is always one valid segment.
	taken := []string{}
	for n := 2; n <= directoryNameSuffixLimit; n++ {
		taken = append(taken, withSuffix("aosp", "-"+strconv.Itoa(n)))
	}
	for _, name := range append([]string{"aosp"}, taken...) {
		if err := os.MkdirAll(filepath.Join(container, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	got := s.allocateName(tenant, "aosp")
	if !validDirectoryKey(got) || !strings.HasPrefix(got, "aosp-") {
		t.Fatalf("an exhausted name did not fall back to a readable tail: %q", got)
	}
	if _, err := os.Stat(filepath.Join(container, got)); !os.IsNotExist(err) {
		t.Fatalf("allocate answered a name that is already a directory: %q", got)
	}
	// Another account's identical name is a different container and needs no coordination.
	other := registry.Tenant{Name: "bob", Workspace: t.TempDir()}
	if got := s.allocateName(other, "aosp"); got != "aosp" {
		t.Fatalf("another account could not use the same name: %q", got)
	}
	// A name that cannot be a directory is still answered with something mountable: the legacy
	// random id, which every client and every record shape already understands.
	for _, desired := range []string{"", ".hidden", "nested/one", strings.Repeat("a", 256)} {
		if got := s.allocateName(tenant, desired); !validDirectoryKey(got) {
			t.Fatalf("an unusable proposal %q answered %q, which cannot be mounted", desired, got)
		}
	}
}

// A suffixed name still has to be one segment of at most 255 bytes, and a long local directory
// name must not be cut into invalid UTF-8 by the suffix.
func TestAllocatedNamesStayOneSegmentWithinTheBound(t *testing.T) {
	for _, base := range []string{strings.Repeat("a", 255), strings.Repeat("我", 85)} {
		for _, suffix := range []string{"-2", "-123456789", "-01234567"} {
			got := withSuffix(base, suffix)
			if !validDirectoryKey(got) {
				t.Fatalf("withSuffix(%q, %q) = %q is not a usable name", base, suffix, got)
			}
			if !strings.HasSuffix(got, suffix) {
				t.Fatalf("withSuffix(%q, %q) = %q lost its suffix", base, suffix, got)
			}
		}
	}
}

// Deleting a SAVED folder is the delete path this exists for: the operator disconnects, the
// capability dies with the mount (a restart, the grace window, a reaped lease), and the empty
// mount point would otherwise stay in the account's container forever. What a stale capability
// must never do, this must never do either: touch a live mount, or anything this service did
// not create.
func TestPurgeByKeyReleasesALeftoverMountPoint(t *testing.T) {
	s, tenant, _ := stableFixture(t)
	sh, err := s.openWith(tenant, "owner", openRequest{Name: "aosp", Writable: true, Key: "aosp"})
	if err != nil {
		t.Fatal(err)
	}
	path := sh.path
	if err := s.close(sh); err != nil {
		t.Fatal(err)
	}
	// The capability is gone: only the key is left to name the path.
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Key: "aosp", Purge: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the leftover mount point was not released: %v", err)
	}
	// Releasing an already released path is success, not an error: the operator's delete must
	// be idempotent.
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Key: "aosp", Purge: true}); err != nil {
		t.Fatalf("an already released path failed: %v", err)
	}
	// The name is free again, which is what makes delete + re-add land on the same path.
	if got := s.allocateName(tenant, "aosp"); got != "aosp" {
		t.Fatalf("the released name was not free again: %q", got)
	}
	// A live mount is never released, whatever the caller claims to know.
	live, err := s.openWith(tenant, "owner", openRequest{Name: "aosp", Writable: true, Key: "aosp"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.close(live) }()
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Key: "aosp", Purge: true}); err == nil || !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("a live mount was released by key: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the refused release touched the live mount point: %v", err)
	}
	// A directory this service could not have created is refused, not removed.
	foreign := filepath.Join(tenant.Workspace, "browser", "mine")
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(foreign, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Key: "mine", Purge: true}); err == nil {
		t.Fatal("a non-empty directory was removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a refused release touched the directory: %v", err)
	}
	// A key is not a removal instruction for anything but one safe segment.
	for _, key := range []string{"../etc", "nested/one", ".hidden"} {
		if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Key: key, Purge: true}); err == nil {
			t.Fatalf("an invalid key %q was accepted as a release", key)
		}
	}
	// Another account's identical key names another path: this one must not be touched.
	other := registry.Tenant{Name: "bob", Workspace: t.TempDir()}
	if _, err := s.dispatch(context.Background(), "close", other, "owner", payload{Key: "aosp", Purge: true}); err != nil {
		t.Fatalf("another account's own key could not be released: %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("another account's release touched this account's mount point: %v", err)
	}
}

// The durable half of the mapping goes with the purge: a record whose mount no longer exists
// would otherwise be replayed by the next startup cleanup.
func TestPurgeByKeyRemovesTheRecord(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	tenant := registry.Tenant{Name: "alice", Workspace: workspace}
	s := NewWithState(nil, nil, base)
	path, err := prepare(workspace, "aosp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeRecord(mountRecord{ID: "aosp", Tenant: tenant.Name, Workspace: workspace, Path: path, State: "ready", Persistent: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Key: "aosp", Purge: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.recordPath(tenant.Name, "aosp")); !os.IsNotExist(err) {
		t.Fatalf("the record survived the release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the mount point survived the release: %v", err)
	}
}

// The record file name carries the account, because a client-chosen directory name is unique
// inside one account and not across the gateway. Two accounts with a folder of the same name
// must not overwrite each other's record, and the name written before directory names existed
// must still be cleaned instead of rejected.
func TestRecordFileNamesArePerAccount(t *testing.T) {
	base := t.TempDir()
	workspaceA := filepath.Join(base, "a")
	workspaceB := filepath.Join(base, "b")
	for _, workspace := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(workspace, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alice := registry.Tenant{Name: "alice", UID: 1001, PublicPort: 32101, WorkerPort: 32102, Workspace: workspaceA, DshHome: filepath.Join(base, "home-a"), CreatedAt: time.Now(), KeyPrefix: "key-prefix12", Handshake: registry.HandshakePending}
	bob := registry.Tenant{Name: "bob", UID: 1002, PublicPort: 32201, WorkerPort: 32202, Workspace: workspaceB, DshHome: filepath.Join(base, "home-b"), CreatedAt: time.Now(), KeyPrefix: "key-prefix34", Handshake: registry.HandshakePending}
	reg := registry.New(filepath.Join(base, "registry.json"), filepath.Join(base, "registry.keys.map"))
	for _, tenant := range []registry.Tenant{alice, bob} {
		if err := reg.Put(tenant); err != nil {
			t.Fatal(err)
		}
	}
	s := NewWithState(nil, nil, base)
	s.SetRegistry(reg)
	if err := s.AcquireLock(); err != nil {
		t.Fatal(err)
	}
	defer s.ReleaseLock()
	paths := map[string]string{}
	for _, tenant := range []registry.Tenant{alice, bob} {
		path, err := prepare(tenant.Workspace, "aosp")
		if err != nil {
			t.Fatal(err)
		}
		paths[tenant.Name] = path
		if err := s.writeRecord(mountRecord{ID: "aosp", Tenant: tenant.Name, Workspace: tenant.Workspace, Path: path, State: "ready", Persistent: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tenant := range []registry.Tenant{alice, bob} {
		if _, err := os.Stat(s.recordPath(tenant.Name, "aosp")); err != nil {
			t.Fatalf("%s lost its own record: %v", tenant.Name, err)
		}
	}
	if aliceRecord, err := os.Stat(s.recordPath(alice.Name, "aosp")); err != nil {
		t.Fatal(err)
	} else if bobRecord, err := os.Stat(s.recordPath(bob.Name, "aosp")); err != nil {
		t.Fatal(err)
	} else if os.SameFile(aliceRecord, bobRecord) {
		t.Fatal("two accounts share one record file")
	}
	// The name a previous build wrote ("<id>.json") is read, not rejected: an upgraded gateway
	// must still clean the mount it crashed on.
	legacy := mountRecord{ID: stableKeyA, Tenant: alice.Name, Workspace: workspaceA, Path: paths[alice.Name], State: "ready", Persistent: true}
	if !recordFileName(stableKeyA+".json", legacy) || !recordFileName(alice.Name+"."+stableKeyA+".json", legacy) {
		t.Fatal("a record file name this service writes is not recognised")
	}
	if recordFileName("bob."+stableKeyA+".json", legacy) || recordFileName("other.json", legacy) {
		t.Fatal("a record file name of another account or shape was accepted")
	}
}

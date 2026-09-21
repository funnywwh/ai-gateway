package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssueStoresOnlyHashAndPersistsMapping(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := newFileStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Issue("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("plaintext token persisted")
	}
	if !strings.Contains(string(data), tokenHash(token)) {
		t.Fatal("token hash missing")
	}
	if err := s.SetUpstream(token, &Upstream{Name: "dsh-auth-x", Value: "secret", Authority: "127.0.0.1:32100"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tenant != "alice" || got.Upstream == nil || got.Upstream.Generation != 1 {
		t.Fatalf("%#v", got)
	}
	again, err := newFileStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Get(token); err != nil {
		t.Fatal(err)
	}
}
func TestExpiryTouchAndCAS(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "s.json")
	s, _ := newFileStore(path, func() time.Time { return now })
	token, _ := s.Issue("alice", time.Minute)
	_ = s.SetUpstream(token, &Upstream{Name: "n", Value: "one", Authority: "a"})
	st, _ := s.Get(token)
	old := st.Upstream.Generation
	_ = s.SetUpstream(token, &Upstream{Name: "n", Value: "two", Authority: "a"})
	cleared, err := s.ClearUpstreamIf(token, old)
	if err != nil || cleared {
		t.Fatalf("stale CAS cleared=%v err=%v", cleared, err)
	}
	st, _ = s.Get(token)
	cleared, err = s.ClearUpstreamIf(token, st.Upstream.Generation)
	if err != nil || !cleared {
		t.Fatalf("current CAS cleared=%v err=%v", cleared, err)
	}
	if err := s.Touch(token, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := s.Get(token); err != ErrNotFound {
		t.Fatalf("expiry err=%v", err)
	}
}
func TestModeRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(p); err == nil {
		t.Fatal("broad permissions accepted")
	}
}

func TestTwoStoresMergeAndObserveChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	first, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	one, err := first.Issue("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	two, err := second.Issue("bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(two); err != nil {
		t.Fatalf("first store did not reload second writer: %v", err)
	}
	if err := second.Delete(one); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(one); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first store did not observe deletion: %v", err)
	}
}

func TestDeleteTenantMissingDoesNotCreateLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTenant("alice"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".lock"} {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s unexpectedly created: %v", candidate, err)
		}
	}
}

func TestSessionCapacity(t *testing.T) {
	store, err := NewFileStoreWithLimit(filepath.Join(t.TempDir(), "sessions.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Issue("alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Issue("alice", time.Hour); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second issue err=%v", err)
	}
}

// Logout stops a tenant's dsh only when its last session left, so the count has to mean
// "live sessions of this tenant" and nothing else: expired records and other tenants' records
// must not keep a worker alive.
func TestCountTenantCountsOnlyLiveSessionsOfThatTenant(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "s.json")
	s, err := newFileStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CountTenant("alice"); err != nil {
		t.Fatalf("empty store: %v", err)
	}
	live, _ := s.Issue("alice", time.Hour)
	second, _ := s.Issue("alice", time.Hour)
	if _, err := s.Issue("bob", time.Hour); err != nil {
		t.Fatal(err)
	}
	expired, _ := s.Issue("alice", time.Minute)
	now = now.Add(2 * time.Minute)
	got, err := s.CountTenant("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("alice has %d live sessions, want 2 (expired and other tenants excluded)", got)
	}
	if got, err := s.CountTenant("bob"); err != nil || got != 1 {
		t.Fatalf("bob = %d, %v", got, err)
	}
	// Revoking the last one is what the logout path does before asking again.
	for _, token := range []string{live, second} {
		if err := s.Delete(token); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.CountTenant("alice"); err != nil || got != 0 {
		t.Fatalf("after revoking both: %d, %v", got, err)
	}
	_ = expired
	if _, err := s.CountTenant(""); err == nil {
		t.Fatal("an empty tenant name was accepted")
	}
}

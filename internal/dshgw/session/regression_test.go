package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemovedStateInvalidatesCachedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.Issue("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(token); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed state left cached session valid: %v", err)
	}
}

func TestSameTimestampReplacementInvalidatesCache(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions.json")
	first, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token, err := first.Issue("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	data := []byte(`{"version":1,"sessions":{}}`)
	for int64(len(data)) < before.Size() {
		data = append(data, ' ')
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Get(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same-metadata replacement retained session: %v", err)
	}
}

func TestResponseCookieCASRejectsStaleAndClearedGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	first, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token, err := first.Issue("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &Upstream{Name: "dsh-auth-test", Value: "one", Authority: "127.0.0.1:32100"}
	if err := first.SetUpstream(token, cookie); err != nil {
		t.Fatal(err)
	}
	old, _ := first.Get(token)
	second, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cookie.Value = "two"
	if ok, err := second.SetUpstreamIf(token, old.Upstream.Generation, cookie); err != nil || !ok {
		t.Fatalf("current cookie CAS: %v, %v", ok, err)
	}
	cookie.Value = "stale"
	if ok, err := first.SetUpstreamIf(token, old.Upstream.Generation, cookie); err != nil || ok {
		t.Fatalf("stale cookie CAS: %v, %v", ok, err)
	}
	current, _ := first.Get(token)
	if current.Upstream.Value != "two" {
		t.Fatalf("stale cookie replaced newer value: %s", current.Upstream.Value)
	}
	if ok, err := second.ClearUpstreamIf(token, current.Upstream.Generation); err != nil || !ok {
		t.Fatalf("clear: %v, %v", ok, err)
	}
	if ok, err := first.SetUpstreamIf(token, current.Upstream.Generation, cookie); err != nil || ok {
		t.Fatalf("cleared cookie was resurrected: %v, %v", ok, err)
	}
	if err := second.Delete(token); err != nil {
		t.Fatal(err)
	}
	if _, err := first.SetUpstreamIf(token, current.Upstream.Generation, cookie); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session was resurrected: %v", err)
	}
}

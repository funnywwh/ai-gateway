package sshworkspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Removing the last mount must leave an empty list, not `null`.
//
// The record file is what an operator opens during an incident — it is how the mounts behind a
// wedged FUSE connection are found — and the account's mirror of the same fact has always
// written `[]`. `null` also reads as "the field is missing" to anything stricter than Go's own
// decoder, so the two documents must not disagree.
func TestStoreWritesAnEmptyListWhenTheLastMountGoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-mounts.json")
	store := NewStore(path)
	mount := Mount{
		Tenant:          "dsh-tenant",
		Host:            "rag-server",
		Remote:          "/srv/docs",
		CanonicalRemote: "/srv/docs",
		Mountpoint:      "/state/workspaces/dsh-tenant/ssh/rag-server/srv/docs",
		CreatedAt:       time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}
	if _, err := store.Add(mount); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.Remove(mount.Mountpoint); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("the emptied record contains null: %s", data)
	}
	var document struct {
		Version int             `json:"version"`
		Mounts  []Mount         `json:"mounts"`
		Extra   json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parsing %s: %v", data, err)
	}
	if document.Mounts == nil {
		t.Errorf("mounts decoded to nil; the document must say []: %s", data)
	}
	if len(document.Mounts) != 0 {
		t.Errorf("mounts = %+v, want none", document.Mounts)
	}
	// The record still loads as an empty list, which is what the lifecycle acts on.
	mounts, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mounts) != 0 {
		t.Errorf("Load = %+v, want none", mounts)
	}
}

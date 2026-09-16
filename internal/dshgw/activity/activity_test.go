package activity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkReloadDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	s := &Store{Path: path}
	at := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkLogin("alice", at); err != nil {
		t.Fatal(err)
	}
	other := &Store{Path: path}
	got, err := other.LastLogin("alice")
	if err != nil || !got.Equal(at) {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if err := other.Delete("alice"); err != nil {
		t.Fatal(err)
	}
	got, err = s.LastLogin("alice")
	if err != nil || !got.IsZero() {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestDeleteMissingDoesNotCreateRootOwnedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	if err := (&Store{Path: path}).Delete("alice"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".lock"} {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s unexpectedly created: %v", candidate, err)
		}
	}
}

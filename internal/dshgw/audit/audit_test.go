package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONLDoesNotInventSecretFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink := &JSONL{Path: path}
	if err := sink.Write(Event{Kind: "edge_reject", Path: "/api", Reason: "origin mismatch", Status: 403}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"kind":"edge_reject"`) || strings.Contains(string(data), "token") {
		t.Fatalf("%s", data)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

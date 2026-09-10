package secret

import (
	"strings"
	"testing"
)

func TestHashIsStableAndHex(t *testing.T) {
	h1 := Hash("sk-gw-abc")
	h2 := Hash("sk-gw-abc")
	if h1 != h2 {
		t.Fatal("hash must be deterministic")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex length = %d", len(h1))
	}
	if Hash("sk-gw-abd") == h1 {
		t.Fatal("different tokens must hash differently")
	}
}

func TestPrefix(t *testing.T) {
	if got := Prefix("sk-gw-1234567890abcdef"); got != "sk-gw-123456" {
		t.Fatalf("prefix = %q", got)
	}
	if got := Prefix("short"); got != "short" {
		t.Fatalf("short prefix = %q", got)
	}
}

func TestEqual(t *testing.T) {
	h := Hash("token")
	if !Equal(h, h) {
		t.Fatal("equal hashes must compare equal")
	}
	if Equal(h, Hash("other")) {
		t.Fatal("different hashes must not compare equal")
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Bearer sk-gw-1": "sk-gw-1",
		"bearer sk-gw-1": "sk-gw-1",
		"  sk-gw-1  ":    "sk-gw-1",
		"sk-gw-1":        "sk-gw-1",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.TrimSpace(Normalize("Bearer ")) != "" {
		t.Error("empty token must normalize to empty")
	}
}

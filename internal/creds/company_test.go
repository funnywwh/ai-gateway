package creds

import (
	"strings"
	"testing"
)

// Company secrets (M93) use a scoped AAD so their ciphertext can never be replayed as a provider
// credential — or the other way round — when the numeric row ids coincide. That property is the
// whole reason the scoped functions exist, so it is what these tests pin.

func TestScopedRoundTrip(t *testing.T) {
	key := DeriveKey("k")
	sealed, err := EncryptScoped(key, "feishu_app", 7, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed) == "secret" {
		t.Fatal("the ciphertext is the plaintext")
	}
	opened, err := DecryptScoped(key, "feishu_app", 7, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "secret" {
		t.Fatalf("opened = %q", opened)
	}
}

func TestScopedIsNotReplayableAcrossScopesOrIDs(t *testing.T) {
	key := DeriveKey("k")
	sealed, err := EncryptScoped(key, "feishu_app", 7, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		scope string
		id    int64
	}{
		{"another scope", "provider", 7},
		{"another id", "feishu_app", 8},
		{"empty scope is refused", "", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecryptScoped(key, tc.scope, tc.id, sealed); err == nil {
				t.Fatalf("ciphertext opened with scope=%q id=%d", tc.scope, tc.id)
			}
		})
	}
	// The legacy provider AAD is a different shape entirely: the same numeric id must not open a
	// company secret.
	if _, err := Decrypt(key, 7, sealed); err == nil {
		t.Fatal("a company secret opened with the legacy provider AAD")
	}
	// And the other direction: a provider credential must not open as a company secret.
	legacy, err := Encrypt(key, 7, []byte("provider-json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptScoped(key, "feishu_app", 7, legacy); err == nil {
		t.Fatal("a provider credential opened as a company secret")
	}
}

func TestScopedRequiresAScope(t *testing.T) {
	key := DeriveKey("k")
	if _, err := EncryptScoped(key, "", 1, []byte("x")); err == nil {
		t.Fatal("an empty scope must be refused")
	}
	if _, err := DecryptScoped(key, "", 1, []byte("x")); err == nil {
		t.Fatal("an empty scope must be refused on the way back too")
	}
}

func TestCompanySealer(t *testing.T) {
	sealer := NewCompanySealer(DeriveKey("k"))
	if !sealer.Ready() {
		t.Fatal("a keyed sealer must be ready")
	}
	sealed, err := sealer.Seal(3, "company-secret")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sealer.Open(3, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened != "company-secret" {
		t.Fatalf("opened = %q", opened)
	}
	// A different row id cannot open it: the AAD binds the row, not just the table.
	if _, err := sealer.Open(4, sealed); err == nil {
		t.Fatal("another row opened the secret")
	}
	// No secret stored is a state, not an error.
	if opened, err := sealer.Open(3, nil); err != nil || opened != "" {
		t.Fatalf("empty secret = %q %v, want empty and no error", opened, err)
	}
	// A row id is required: sealing against id 0 would make every such secret interchangeable.
	if _, err := sealer.Seal(0, "x"); err == nil {
		t.Fatal("sealing without a row id must fail")
	}

	// A deployment without credentials_key cannot seal, and says so instead of storing anything.
	empty := NewCompanySealer(nil)
	if empty.Ready() {
		t.Fatal("a keyless sealer must not be ready")
	}
	if _, err := empty.Seal(1, "x"); err == nil || !strings.Contains(err.Error(), "credentials_key") {
		t.Fatalf("keyless seal = %v, want a credentials_key error", err)
	}
}

package creds

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := DeriveKey("test-passphrase")
	plaintext := []byte(`{"api_key":"sk-secret"}`)

	sealed, err := Encrypt(key, 42, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-secret")) {
		t.Fatal("ciphertext must not contain the plaintext")
	}

	opened, err := Decrypt(key, 42, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("round trip mismatch: %s", opened)
	}
}

func TestNonceIsRandom(t *testing.T) {
	key := DeriveKey("k")
	a, err := Encrypt(key, 1, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt(key, 1, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("ciphertexts must differ across calls (random nonce)")
	}
}

func TestProviderIDIsAuthenticated(t *testing.T) {
	key := DeriveKey("k")
	sealed, err := Encrypt(key, 1, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(key, 2, sealed); err == nil {
		t.Fatal("decryption with a different provider id must fail")
	}
}

func TestWrongKeyFails(t *testing.T) {
	sealed, err := Encrypt(DeriveKey("a"), 1, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(DeriveKey("b"), 1, sealed); err == nil {
		t.Fatal("decryption with the wrong key must fail")
	}
}

func TestEmptyInputs(t *testing.T) {
	if _, err := Encrypt(nil, 1, []byte("x")); err == nil {
		t.Fatal("empty key must be rejected")
	}
	if _, err := Decrypt(DeriveKey("k"), 1, nil); err != nil {
		t.Fatalf("empty ciphertext means no credentials: %v", err)
	}
	if _, err := Decrypt(DeriveKey("k"), 1, []byte{1, 2, 3}); err == nil {
		t.Fatal("truncated ciphertext must be rejected")
	}
}

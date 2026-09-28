// Package creds encrypts secrets at rest with AES-256-GCM.
// The key comes from configuration (credentials_key); the row id the secret belongs to is bound as
// additional authenticated data so one row's ciphertext cannot be replayed for another.
//
// Two AAD shapes coexist, and they are deliberately not interchangeable:
//
//   - Encrypt/Decrypt (no scope) is the original provider-credential form. Providers still use it,
//     and their stored ciphertext is never re-sealed — changing the AAD would make every existing
//     row unreadable.
//   - EncryptScoped/DecryptScoped namespaces the AAD with a scope string (M93, first used by the
//     console-managed Feishu companies). Without it, a provider credential and a company secret
//     whose numeric ids happen to match would be swappable between tables.
package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// DeriveKey turns a passphrase into a 32-byte AES-256 key.
func DeriveKey(passphrase string) []byte {
	sum := sha256.Sum256([]byte(passphrase))
	return sum[:]
}

// Encrypt seals plaintext for one provider (the legacy, unscoped AAD).
func Encrypt(key []byte, providerID int64, plaintext []byte) ([]byte, error) {
	return seal(key, aad(providerID), plaintext)
}

// Decrypt opens ciphertext produced by Encrypt.
func Decrypt(key []byte, providerID int64, ciphertext []byte) ([]byte, error) {
	return open(key, aad(providerID), ciphertext)
}

// EncryptScoped seals plaintext for one row of one scope ("feishu_app", say). Callers that own a
// new kind of secret must use this instead of Encrypt: sharing the AAD space across tables is what
// makes ciphertexts replayable between them.
func EncryptScoped(key []byte, scope string, id int64, plaintext []byte) ([]byte, error) {
	if scope == "" {
		return nil, fmt.Errorf("creds: scope is required")
	}
	return seal(key, scopedAAD(scope, id), plaintext)
}

// DecryptScoped opens ciphertext produced by EncryptScoped for the same scope and id.
func DecryptScoped(key []byte, scope string, id int64, ciphertext []byte) ([]byte, error) {
	if scope == "" {
		return nil, fmt.Errorf("creds: scope is required")
	}
	return open(key, scopedAAD(scope, id), ciphertext)
}

// EncryptScopedKey seals plaintext for one row of one scope, addressed by a *string* handle: the
// console's per-company overrides are keyed by app id, not by a row id (M96).
func EncryptScopedKey(key []byte, scope, handle string, plaintext []byte) ([]byte, error) {
	if scope == "" || handle == "" {
		return nil, fmt.Errorf("creds: scope and handle are required")
	}
	return seal(key, scopedKeyAAD(scope, handle), plaintext)
}

// DecryptScopedKey opens ciphertext produced by EncryptScopedKey for the same scope and handle.
func DecryptScopedKey(key []byte, scope, handle string, ciphertext []byte) ([]byte, error) {
	if scope == "" || handle == "" {
		return nil, fmt.Errorf("creds: scope and handle are required")
	}
	return open(key, scopedKeyAAD(scope, handle), ciphertext)
}

func seal(key, additional []byte, plaintext []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("creds: encryption key is empty")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("creds: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, additional), nil
}

func open(key, additional, ciphertext []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("creds: encryption key is empty")
	}
	if len(ciphertext) == 0 {
		return nil, nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("creds: ciphertext too short")
	}
	nonce, body := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, additional)
	if err != nil {
		return nil, fmt.Errorf("creds: decrypt: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creds: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creds: gcm: %w", err)
	}
	return gcm, nil
}

// aad is the legacy provider form: the row id alone. It is kept byte-for-byte so every provider
// credential written before scopes existed keeps decrypting.
func aad(providerID int64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(providerID))
	return buf[:]
}

// scopedAAD is "scope, 0x00, row id": the scope keeps two tables' ciphertexts apart even when the
// numeric ids coincide, and the NUL separator keeps ("a", 1) from colliding with ("a\x001", 0).
func scopedAAD(scope string, id int64) []byte {
	out := make([]byte, 0, len(scope)+1+8)
	out = append(out, scope...)
	out = append(out, 0)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(id))
	return append(out, buf[:]...)
}

// scopedKeyAAD is the same shape for a string handle ("scope, 0x00, app id"). It is deliberately not
// the numeric form: the two live in different scopes anyway, and keeping them textually distinct
// means a ciphertext cannot be moved between them even if a scope name is ever reused.
func scopedKeyAAD(scope, handle string) []byte {
	out := make([]byte, 0, len(scope)+1+len(handle))
	out = append(out, scope...)
	out = append(out, 0)
	return append(out, handle...)
}

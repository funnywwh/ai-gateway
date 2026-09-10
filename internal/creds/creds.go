// Package creds encrypts provider credentials at rest with AES-256-GCM.
// The key comes from configuration (credentials_key); a provider id is bound as
// additional authenticated data so one provider's ciphertext cannot be replayed for another.
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

// Encrypt seals plaintext for one provider.
func Encrypt(key []byte, providerID int64, plaintext []byte) ([]byte, error) {
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
	return gcm.Seal(nonce, nonce, plaintext, aad(providerID)), nil
}

// Decrypt opens ciphertext produced by Encrypt.
func Decrypt(key []byte, providerID int64, ciphertext []byte) ([]byte, error) {
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
	plaintext, err := gcm.Open(nil, nonce, body, aad(providerID))
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

func aad(providerID int64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(providerID))
	return buf[:]
}

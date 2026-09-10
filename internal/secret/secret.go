// Package secret hashes bearer credentials (API keys, MCP tokens, admin sessions).
// Tokens are high-entropy random values, so an unsalted SHA-256 is sufficient.
package secret

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// PrefixLen is the number of leading characters stored in clear for indexed lookup.
const PrefixLen = 12

// Hash returns the hex-encoded SHA-256 of a bearer token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Prefix returns the lookup prefix stored alongside the hash.
func Prefix(token string) string {
	if len(token) <= PrefixLen {
		return token
	}
	return token[:PrefixLen]
}

// Equal compares two hex hashes in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Normalize strips an optional "Bearer " prefix and surrounding whitespace.
func Normalize(header string) string {
	h := strings.TrimSpace(header)
	if len(h) >= 6 && strings.EqualFold(h[:6], "bearer") {
		return strings.TrimSpace(h[6:])
	}
	return h
}

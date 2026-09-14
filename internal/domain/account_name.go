package domain

import (
	"strings"
	"unicode/utf8"
)

// MaxAccountNameRunes is the maximum number of Unicode code points in an account
// name. Account names are labels rather than machine identifiers, so the limit is
// measured in runes instead of UTF-8 bytes.
const MaxAccountNameRunes = 64

// NormalizeAccountName trims surrounding Unicode whitespace and validates an
// account name. Account names may contain any valid Unicode characters, including
// characters such as '@' used by email addresses; only blank, malformed UTF-8 and
// overlong values are rejected.
func NormalizeAccountName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrInvalidRequest("account name is required")
	}
	if !utf8.ValidString(name) {
		return "", ErrInvalidRequest("account name must be valid UTF-8")
	}
	if utf8.RuneCountInString(name) > MaxAccountNameRunes {
		return "", ErrInvalidRequest("account name must be at most 64 characters")
	}
	return name, nil
}

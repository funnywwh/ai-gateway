package domain

import (
	"fmt"
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
	return normalizeLabel("account name", name, MaxAccountNameRunes)
}

// normalizeLabel is the one rule every human-facing name follows (accounts and tags
// today): trim surrounding Unicode whitespace, refuse a blank or malformed value, and cap
// the length in runes so a Chinese character is not charged three bytes. It exists because
// the two names must not drift apart — a label the writer accepts in one place and refuses
// in the other is how a row becomes uneditable (see NormalizeTagName).
func normalizeLabel(kind, name string, maxRunes int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrInvalidRequest(kind + " is required")
	}
	if !utf8.ValidString(name) {
		return "", ErrInvalidRequest(kind + " must be valid UTF-8")
	}
	if utf8.RuneCountInString(name) > maxRunes {
		return "", ErrInvalidRequest(fmt.Sprintf("%s must be at most %d characters", kind, maxRunes))
	}
	return name, nil
}

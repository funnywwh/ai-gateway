package domain

import (
	"strings"
	"testing"
)

func TestNormalizeAccountNameAcceptsUnicodeLabels(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The point of the change: an account name is a label, so an email address and
		// Chinese text are both valid. "@" must not be read as a character-class escape.
		{"email", "ops@example.com", "ops@example.com"},
		{"email with plus", "billing+prod@example.co.uk", "billing+prod@example.co.uk"},
		{"chinese", "北京研发", "北京研发"},
		{"mixed script", "客户 A 组", "客户 A 组"},
		{"cyrillic", "Аккаунт", "Аккаунт"},
		{"emoji", "订单 🚀", "订单 🚀"},
		{"trim ascii spaces", "  acme  ", "acme"},
		{"trim unicode space", "\u3000北京研发\u3000", "北京研发"},
		{"trim mixed whitespace", " \t\nacme\r\n", "acme"},
		{"inner whitespace kept", "北京 研发", "北京 研发"},
		// 64 runes is the ceiling; multi-byte runes must be counted as characters, not bytes.
		{"64 ascii", strings.Repeat("a", MaxAccountNameRunes), strings.Repeat("a", MaxAccountNameRunes)},
		{"64 chinese", strings.Repeat("中", MaxAccountNameRunes), strings.Repeat("中", MaxAccountNameRunes)},
		// Digits and punctuation that the API used to reject are now ordinary label text.
		{"punctuation", "acme #1 (trial)", "acme #1 (trial)"},
	}
	for _, tc := range cases {
		got, err := NormalizeAccountName(tc.in)
		if err != nil {
			t.Errorf("%s: NormalizeAccountName(%q) = %v, want no error", tc.name, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: NormalizeAccountName(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestNormalizeAccountNameRejectsBlankAndOverlong(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"spaces only", "     "},
		{"unicode spaces only", "\u3000\t\n"},
		{"65 ascii", strings.Repeat("a", MaxAccountNameRunes+1)},
		{"65 chinese", strings.Repeat("中", MaxAccountNameRunes+1)},
		// Invalid UTF-8 would otherwise be stored and never resolvable by name.
		{"invalid utf8", "acme\xff"},
		{"invalid utf8 only", "\xc3\x28"},
	}
	for _, tc := range cases {
		if got, err := NormalizeAccountName(tc.in); err == nil {
			t.Errorf("%s: NormalizeAccountName(%q) = %q, want an error", tc.name, tc.in, got)
		} else if !isInvalidNameError(err) {
			t.Errorf("%s: error = %v, want an invalid_request error", tc.name, err)
		}
	}
}

// isInvalidNameError keeps the assertion explicit: every rejection is the API shape the
// management surface renders, not a bare error.
func isInvalidNameError(err error) bool {
	ae, ok := AsAPIError(err)
	return ok && ae.Code == "invalid_request"
}

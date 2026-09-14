package domain

import (
	"strings"
	"testing"
)

// A tag name is a human-facing label and follows the account-name rule; these tests exist
// because the ASCII-identifier rule that used to guard it made every pre-existing row with
// such a name uneditable (see NormalizeTagName).
func TestNormalizeTagNameIsTheAccountNameRule(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"chinese", "蓝精灵3", "蓝精灵3"},
		{"mixed script", "蓝精灵 3 号（测试）", "蓝精灵 3 号（测试）"},
		{"ascii identifier", "team.a-b", "team.a-b"},
		{"punctuation", "tier #1 / trial", "tier #1 / trial"},
		{"trim ascii spaces", "  deepseek  ", "deepseek"},
		{"trim unicode space", "\u3000蓝精灵1\u3000", "蓝精灵1"},
		{"inner whitespace kept", "蓝精灵 1", "蓝精灵 1"},
		// 64 runes is the ceiling, counted as characters rather than bytes.
		{"64 ascii", strings.Repeat("a", MaxTagNameRunes), strings.Repeat("a", MaxTagNameRunes)},
		{"64 chinese", strings.Repeat("中", MaxTagNameRunes), strings.Repeat("中", MaxTagNameRunes)},
	}
	for _, tc := range cases {
		got, err := NormalizeTagName(tc.in)
		if err != nil {
			t.Errorf("%s: NormalizeTagName(%q) = %v, want no error", tc.name, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: NormalizeTagName(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		// The two labels must not drift apart: a name one writer accepts and the other
		// refuses is exactly how a row becomes uneditable.
		if _, accountErr := NormalizeAccountName(tc.in); accountErr != nil {
			t.Errorf("%s: the account rule rejects %q (%v) while the tag rule accepts it", tc.name, tc.in, accountErr)
		}
	}
}

func TestNormalizeTagNameRejectsBlankAndOverlong(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"spaces only", "     "},
		{"unicode spaces only", "\u3000\t\n"},
		{"65 ascii", strings.Repeat("a", MaxTagNameRunes+1)},
		{"65 chinese", strings.Repeat("中", MaxTagNameRunes+1)},
		{"invalid utf8", "蓝精灵\xff"},
	} {
		if got, err := NormalizeTagName(tc.in); err == nil {
			t.Errorf("%s: NormalizeTagName(%q) = %q, want an error", tc.name, tc.in, got)
		} else if !isInvalidNameError(err) {
			t.Errorf("%s: error = %v, want an invalid_request error", tc.name, err)
		}
	}
}

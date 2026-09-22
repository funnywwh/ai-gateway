package pinyin

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The generated Go table and the console's generated JS table are two copies of one fact. They are
// written by the same script, but a script that is only ever re-run on one side would let the
// console filter by 陈→chen while the gateway names a tenant from a reading that no longer exists —
// and nothing in either build would notice. So the test reads the console asset as text and compares
// the two blobs byte for byte.
func TestTableMatchesTheConsoleAsset(t *testing.T) {
	raw, err := os.ReadFile("../webui/static/js/pinyin.js")
	if err != nil {
		t.Fatalf("read the console asset: %v", err)
	}
	// The declaration is `const READINGS = "…";` with a JSON string literal (scripts/gen-pinyin.py
	// uses json.dumps, which is valid JavaScript and keeps CJK readable in the file).
	match := regexp.MustCompile(`(?s)const READINGS = ("(?:[^"\\]|\\.)*");`).FindSubmatch(raw)
	if match == nil {
		t.Fatal("pinyin.js has no `const READINGS = \"…\";` declaration: the generator's output shape changed")
	}
	var console string
	if err := json.Unmarshal(match[1], &console); err != nil {
		t.Fatalf("decode the console's READINGS literal: %v", err)
	}
	if console != readingsBlob {
		t.Fatalf("the two generated tables differ (console %d bytes, go %d bytes); "+
			"regenerate both with scripts/gen-pinyin.py", len(console), len(readingsBlob))
	}
}

// The row count is the covered codepoint range, and a row that shifted by one would silently rename
// every character after it. 0x9FFF-0x4E00+1 = 20992 rows, 20924 of which carry a reading.
func TestTableCoversTheDocumentedRange(t *testing.T) {
	rows := readings()
	if want := 0x9FFF - 0x4E00 + 1; len(rows) != want {
		t.Fatalf("rows = %d, want %d", len(rows), want)
	}
	readings := 0
	for _, row := range rows {
		if row != "" {
			readings++
		}
	}
	if readings != 20924 {
		t.Fatalf("characters with a reading = %d, want 20924 (the count the generated header states)", readings)
	}
}

func TestFirstReading(t *testing.T) {
	cases := []struct {
		char rune
		want string
	}{
		{'陈', "chen"}, {'景', "jing"}, {'峰', "feng"},
		{'杨', "yang"}, {'妙', "miao"},
		{'李', "li"}, {'智', "zhi"}, {'超', "chao"},
		{'张', "zhang"}, {'伟', "wei"},
		{'文', "wen"}, {'辉', "hui"},
		{'航', "hang"},
		// The generator strips the diacritics with NFD, so ü loses its umlaut instead of becoming
		// v: 女 is "nu" and 吕 is "lu" (which is also 路). Same-kind collisions in the slug are
		// expected — the account id at the end of the tenant name is what keeps them apart.
		{'女', "nu"},
		{'吕', "lu"},
		// 长 is both chang and zhang; a name has to pick one, and it picks the table's first.
		{'长', "zhang"},
		{'曾', "ceng"},
	}
	for _, tc := range cases {
		if got := FirstReading(tc.char); got != tc.want {
			t.Errorf("FirstReading(%q) = %q, want %q", tc.char, got, tc.want)
		}
	}
}

// Characters the table does not cover answer "", which is how the caller knows to treat them as a
// separator instead of inventing a reading for them.
func TestFirstReadingOutsideTheTable(t *testing.T) {
	for _, r := range []rune{'a', 'Z', '0', '-', '(', ' ', 0x20000, '🙂'} {
		if got := FirstReading(r); got != "" {
			t.Errorf("FirstReading(%q) = %q, want \"\"", r, got)
		}
	}
}

// Every reading has to be usable in a tenant name: the caller lowercases nothing and strips nothing,
// so a table entry with an uppercase letter or a tone mark would produce a name dshgw rejects.
func TestEveryFirstReadingIsNameSafe(t *testing.T) {
	safe := regexp.MustCompile(`^[a-z]+$`)
	for _, row := range readings() {
		if row == "" {
			continue
		}
		first := row
		if cut := strings.IndexByte(row, ','); cut >= 0 {
			first = row[:cut]
		}
		if !safe.MatchString(first) {
			t.Fatalf("a generated reading is not name-safe: %q (row %q)", first, row)
		}
	}
}

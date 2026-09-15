package webui

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// readConsoleAsset reads one embedded console file as text.
func readConsoleAsset(name string) (string, error) {
	raw, err := assets.ReadFile("static/" + name)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// The pinyin table is generated (scripts/gen-pinyin.py) and committed, so nothing in the build
// would notice if it were regenerated wrongly, truncated, or replaced with a table whose
// readings are shifted by one codepoint — every symptom of that is "the filter silently stops
// matching some names", which no other test can see. These assertions pin the properties the
// console actually relies on.

// pinyinReadings parses the generated module's READINGS blob the same way the browser does:
// one newline-separated record per codepoint starting at U+4E00, readings comma-separated.
func pinyinReadings(t *testing.T) []string {
	t.Helper()
	raw, err := readConsoleAsset("js/pinyin.js")
	if err != nil {
		t.Fatalf("read js/pinyin.js: %v", err)
	}
	match := regexp.MustCompile(`(?s)const READINGS = (".*?");`).FindStringSubmatch(raw)
	if match == nil {
		t.Fatal("js/pinyin.js has no `const READINGS = \"…\";` declaration: the generator's output shape changed")
	}
	var blob string
	if err := json.Unmarshal([]byte(match[1]), &blob); err != nil {
		t.Fatalf("READINGS is not a valid JS/JSON string literal: %v", err)
	}
	return strings.Split(blob, "\n")
}

// TestPinyinTableCoversTheContract pins the characters the console's own tests and documentation
// use, plus the shape of the data (one record per codepoint from U+4E00).
func TestPinyinTableCoversTheContract(t *testing.T) {
	readings := pinyinReadings(t)
	// The CJK Unified Ideographs block is 0x9FFF-0x4E00+1 = 20992 codepoints; a truncated or
	// off-by-one blob would change this count.
	if len(readings) != 0x9FFF-0x4E00+1 {
		t.Fatalf("table holds %d records, want %d (one per codepoint U+4E00-U+9FFF)",
			len(readings), 0x9FFF-0x4E00+1)
	}

	// The index must be absolute: if the blob started at a different codepoint, these would all
	// be the readings of some *other* character.
	cases := []struct {
		ch      rune
		wantAny []string
		because string
	}{
		{'张', []string{"zhang"}, "the fixture account 张三 is filtered by zhangsan/zs"},
		{'三', []string{"san"}, "same"},
		{'李', []string{"li"}, "the fixture account 李四 is filtered by lisi"},
		{'研', []string{"yan"}, "the fixture node 研发部 is filtered by yanfa"},
		{'发', []string{"fa"}, "same"},
		{'部', []string{"bu"}, "every node name in this system ends in 部"},
		{'财', []string{"cai"}, "the harness树 view filters 财务部 by caiwu/cw"},
		// Polyphones have to keep every reading, or a name is unfindable by one of its own
		// pronunciations.
		{'长', []string{"chang", "zhang"}, "长伟 must answer to changwei and zhangwei"},
		{'重', []string{"zhong", "chong"}, "polyphone"},
	}
	for _, tc := range cases {
		got := readings[tc.ch-0x4E00]
		if got == "" {
			t.Errorf("%c (U+%04X) has no reading; %s", tc.ch, tc.ch, tc.because)
			continue
		}
		list := strings.Split(got, ",")
		for _, want := range tc.wantAny {
			if !contains(list, want) {
				t.Errorf("%c readings = %v, want %q among them (%s)", tc.ch, list, want, tc.because)
			}
		}
	}
}

// TestPinyinTableHasNoTonesOrAccents guards the normalisation step: a tone mark or an accented
// character would make the table unmatchable, because an operator types plain ASCII letters.
func TestPinyinTableHasNoTonesOrAccents(t *testing.T) {
	for index, record := range pinyinReadings(t) {
		if record == "" {
			continue
		}
		for _, reading := range strings.Split(record, ",") {
			for _, r := range reading {
				if r < 'a' || r > 'z' {
					t.Fatalf("reading %q of U+%04X contains %q: readings must be plain lowercase a-z "+
						"(tones stripped, ü written as v) or the console cannot match them",
						reading, 0x4E00+index, r)
				}
			}
		}
	}
}

// TestPinyinModuleRecordsItsProvenance is the licensing half: the file embeds MIT-licensed data
// from someone else's project, so the source, its version and the checksum of the exact input
// have to travel with it.
func TestPinyinModuleRecordsItsProvenance(t *testing.T) {
	raw, err := readConsoleAsset("js/pinyin.js")
	if err != nil {
		t.Fatalf("read js/pinyin.js: %v", err)
	}
	for _, want := range []string{
		"mozillazg/pinyin-data", "MIT", "SHA-256", "gen-pinyin.py",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("js/pinyin.js does not record %q in its header; a generated asset has to say "+
				"where its data came from and how to regenerate it", want)
		}
	}
}

// TestConsoleFiltersUseThePinyinMatcher keeps the wiring honest: the table only helps if the
// filters actually consult it. Both the member list and the tree filter are checked, because
// wiring one and forgetting the other is exactly the kind of half-done change that looks fine
// in a screenshot.
func TestConsoleFiltersUseThePinyinMatcher(t *testing.T) {
	org, err := readConsoleAsset("js/pages/org.js")
	if err != nil {
		t.Fatalf("read js/pages/org.js: %v", err)
	}
	if !strings.Contains(org, "matchesQuery(account.name") {
		t.Error("the member filter does not use matchesQuery: typing pinyin would not find 张三")
	}
	if !strings.Contains(org, "matcher: matchesQuery") {
		t.Error("the node tree is not given the pinyin matcher, so its filter box only matches literally")
	}

	// The control itself must stay free of the table: a consumer that does not want pinyin
	// (any future tree) must not pay for it, and the control's reusability is the reason it
	// takes a matcher at all.
	tree, err := readConsoleAsset("js/tree.js")
	if err != nil {
		t.Fatalf("read js/tree.js: %v", err)
	}
	// Check the import statements, not the text: the file mentions pinyin in a comment
	// explaining what `matcher` is for, and a substring test on "pinyin" flags that comment.
	if regexp.MustCompile(`(?m)^\s*import[^;]*pinyin`).MatchString(tree) {
		t.Error("js/tree.js imports the pinyin table; the matcher must arrive as a callback so the " +
			"control stays dependency-free")
	}
	if !strings.Contains(tree, "matcher") {
		t.Error("js/tree.js has no matcher hook: the pinyin capability cannot be injected")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

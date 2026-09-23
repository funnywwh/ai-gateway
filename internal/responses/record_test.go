package responses

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func parseBody(t *testing.T, body string) *Request {
	t.Helper()
	req, apiErr := Parse([]byte(body))
	if apiErr != nil {
		t.Fatalf("parse %s: %v", body, apiErr)
	}
	return req
}

// userItem renders one user message item with a single text part, so a test body can be
// built from texts without quoting mistakes.
func userItem(text string) string {
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`
}

// The default policy records the LAST user message — the one a human just wrote — and leaves
// the runtime-context snapshot, the tool traffic and the earlier assistant turns out.
func TestUserInputTextKeepsTheLastShortMessage(t *testing.T) {
	boilerplate := "Current runtime context. " + strings.Repeat("context filler ", 20)
	question := "把这段代码改成流式，并且保持错误处理不变"
	req := parseBody(t, `{"model":"m","instructions":"be terse","input":[`+
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"SYSTEM INSTRUCTION"}]},`+
		userItem(boilerplate)+`,`+
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"file\":\"/etc/shadow\"}"},`+
		`{"type":"function_call_output","call_id":"c1","output":"ZZ_TOOL_OUTPUT_SECRET"},`+
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"an earlier answer"}]},`+
		userItem(question)+`]}`)

	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != question {
		t.Fatalf("recorded %q, want the last message %q", got, question)
	}
	// The whole point: what is stored is plain text, not a document.
	for _, forbidden := range []string{"{", "}", `"input"`, `"role"`, "omitted", "request_bytes",
		"ZZ_TOOL_OUTPUT_SECRET", "SYSTEM INSTRUCTION", "an earlier answer", "/etc/shadow",
		"runtime context"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("recorded text leaked %q: %q", forbidden, got)
		}
	}
}

// The boundary is <=: a message of exactly maxChars is kept, one character more is not. This
// is the boundary most easily misread, so it is pinned here.
func TestUserInputTextKeepsAMessageAtTheThreshold(t *testing.T) {
	exactly := strings.Repeat("问", 100)
	req := parseBody(t, `{"model":"m","input":[`+userItem("earlier")+`,`+userItem(exactly)+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != exactly {
		t.Fatalf("a %d-character message must be kept at the threshold, got %q",
			len([]rune(exactly)), got)
	}

	tooLong := strings.Repeat("问", 101)
	req = parseBody(t, `{"model":"m","input":[`+userItem("earlier")+`,`+userItem(tooLong)+`]}`)
	got, err = req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	// The 101-character message is skipped, so the row falls back to the earlier one.
	if got != "earlier" {
		t.Fatalf("a 101-character message must be skipped in favour of an earlier one, got %q", got)
	}
}

// A message that carries no text is not what a log row should hold, and it does not end the
// search either: agent clients end turns with empty continuations and tool-result-only
// messages, and the row should still get the last thing that was actually said.
func TestUserInputTextSkipsMessagesWithoutText(t *testing.T) {
	for name, empty := range map[string]string{
		"empty array":      `{"type":"message","role":"user","content":[]}`,
		"blank string":     `{"type":"message","role":"user","content":"   "}`,
		"image only":       `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,ZZ"}]}`,
		"unreadable":       `{"type":"message","role":"user","content":{"nested":"ZZUNREADABLE"}}`,
		"empty text parts": `{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := parseBody(t, `{"model":"m","input":[`+userItem("the real question")+`,`+empty+`]}`)
			got, err := req.UserInputText(100)
			if err != nil {
				t.Fatal(err)
			}
			if got != "the real question" {
				t.Fatalf("recorded %q, want the earlier message with text", got)
			}
		})
	}

	// Nothing carries text at all: the row gets no body, and that is not an error.
	req := parseBody(t, `{"model":"m","input":[`+
		`{"type":"message","role":"user","content":"   "},`+
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,ZZ"}]}]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("recorded %q, want nothing", got)
	}
}

// An oversized message is skipped too, so a row whose turn ended with a pasted file still
// keeps the newest message that fits.
func TestUserInputTextFallsBackPastAnOversizedMessage(t *testing.T) {
	question := "为什么构建这么慢？"
	req := parseBody(t, `{"model":"m","input":[`+
		userItem(question)+`,`+userItem(strings.Repeat("x", 400))+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != question {
		t.Fatalf("recorded %q, want the newest message that fits", got)
	}
}

// A short message is stored whole: no truncation, so a reader can trust that what is in the
// log is what the user wrote.
func TestUserInputTextDoesNotTruncate(t *testing.T) {
	text := strings.Repeat("ab", 40) // 80 characters, comfortably under the threshold
	req := parseBody(t, `{"model":"m","input":[`+userItem(text)+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != text {
		t.Fatalf("recorded %q, want the message verbatim", got)
	}
	if len([]rune(got)) != 80 {
		t.Fatalf("recorded %d characters, want 80", len([]rune(got)))
	}
}

// Several text parts in one message are all the user's own input: they are joined with a
// newline, and the join counts toward the threshold (the stored text is what was measured).
func TestUserInputTextJoinsTextPartsWithANewline(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":[`+
		`{"type":"input_text","text":"first half"},`+
		`{"type":"input_text","text":"second half"}]}]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "first half\nsecond half" {
		t.Fatalf("recorded %q", got)
	}
}

// An image inside the kept message is not content this policy records: it is left out, and
// (M82) nothing is written to say it was there.
func TestUserInputTextLeavesOutNonTextParts(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":[`+
		`{"type":"input_text","text":"look at this"},`+
		`{"type":"input_image","image_url":"data:image/png;base64,ZZBASE64IMAGE"}]}]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "look at this" {
		t.Fatalf("recorded %q, want just the message's text", got)
	}
	if strings.Contains(got, "ZZBASE64IMAGE") || strings.Contains(got, "data:image") {
		t.Fatalf("the image reached the log: %q", got)
	}
}

// Content nobody can read cannot be measured, so it is not recorded. When it is the only user
// message the row is written without a body (the caller stores an empty string); when an
// earlier message carries text, that message is what the row gets — see
// TestUserInputTextSkipsMessagesWithoutText.
func TestUserInputTextLeavesOutUnreadableContent(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":{"nested":"ZZUNREADABLE"}}]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("unreadable content must not be recorded, got %q", got)
	}
}

// A continuation can carry only tool results: there is no user input to record, and that is
// not an error.
func TestUserInputTextWithNoUserMessages(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":"data"}]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("recorded %q, want nothing", got)
	}
}

// 0 turns the filter off: every user message's text is kept, in order. It is still text only.
func TestUserInputTextWithoutAThresholdKeepsEveryMessage(t *testing.T) {
	long := strings.Repeat("x", 300)
	req := parseBody(t, `{"model":"m","input":[`+
		userItem("first")+`,`+userItem(long)+`,`+
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,ZZ"}]},`+
		userItem("last")+`]}`)
	got, err := req.UserInputText(0)
	if err != nil {
		t.Fatal(err)
	}
	// Messages that carry no text are skipped rather than joined as empty strings: a blank
	// line in the middle of the recorded text would read like the user sent a blank turn.
	want := "first\n" + long + "\nlast"
	if got != want {
		t.Fatalf("recorded %q, want every text-carrying message joined", got)
	}
}

// The gateway's own string shorthand (`"input":"ping"`) arrives as one synthesized user
// message.
func TestUserInputTextAcceptsTheStringShorthand(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":"ping"}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ping" {
		t.Fatalf("recorded %q, want ping", got)
	}
}

// A message whose content is a bare string is read the same way as a part array.
func TestUserInputTextAcceptsStringContent(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":"hello there"}]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello there" {
		t.Fatalf("recorded %q", got)
	}
}

// The search takes the newest message that fits, not the first one it sees: a long message at
// the end does not disqualify an earlier short one (that is the v4.3.2 correction).
func TestUserInputTextTakesTheNewestMessageThatFits(t *testing.T) {
	fits := strings.Repeat("a", 20)
	req := parseBody(t, `{"model":"m","input":[`+
		userItem(fits)+`,`+userItem(strings.Repeat("b", 300))+`,`+userItem("   ")+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != fits {
		t.Fatalf("recorded %q, want the newest message that fits", got)
	}
}

// The recorded text is valid UTF-8 and its length is exactly what the caller would store.
func TestUserInputTextIsValidUTF8(t *testing.T) {
	text := strings.Repeat("重构", 30) // 60 characters, 180 bytes
	req := parseBody(t, `{"model":"m","input":[`+userItem(text)+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("recorded text is not valid UTF-8: %q", got)
	}
	if len([]rune(got)) != 60 || len(got) != 180 {
		t.Fatalf("recorded %d characters / %d bytes, want 60 / 180", len([]rune(got)), len(got))
	}
}

// userItemJSON renders one user message item whose text needs JSON escaping (newlines, quotes).
func userItemJSON(t *testing.T, text string) string {
	t.Helper()
	raw, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(raw) + `}]}`
}

// The case that made this rule: a real prompt is often LONGER than a short boilerplate block and
// shorter than a long one, so the markers — not the length — have to decide. This is the shape a
// browser DSH session sent when every one of its rows came out empty: a 542-character runtime
// context snapshot LAST, and a 117-character question before it (M83).
func TestUserInputTextSkipsBoilerplateAndKeepsTheHumanMessage(t *testing.T) {
	question := strings.Repeat("问", 117)
	runtime := dshRuntimePrefix + " " + strings.Repeat("context. ", 50) // boilerplate, ~540 chars
	req := parseBody(t, `{"model":"m","input":[`+userItemJSON(t, question)+`,`+userItemJSON(t, runtime)+`]}`)

	// The bound is the shipped default: both messages are within it, so only the marker can tell
	// them apart. If the marker were ignored, the runtime snapshot would be what got recorded.
	got, err := req.UserInputText(2000)
	if err != nil {
		t.Fatal(err)
	}
	if got != question {
		t.Fatalf("recorded %d characters, want the 117-character question (boilerplate skipped)",
			len([]rune(got)))
	}
}

// Every client's scaffolding is skipped, whichever marker it uses.
func TestUserInputTextRecognisesEachClientsBoilerplate(t *testing.T) {
	for name, boilerplate := range map[string]string{
		"DSH runtime context":     dshRuntimePrefix + " This snapshot supersedes earlier ones.",
		"DSH system prompt":       dshDeveloperPrefix + "\n\nThe checkout is at /x.",
		"Codex environment block": codexEnvContextTag + "\n  <cwd>/repo</cwd>\n</environment_context>",
		"Codex instructions":      codexInstructionPrefix + ", a terminal-based coding assistant.",
		"system reminder":         "<system-reminder>\nCurrent runtime context …\n</system-reminder>",
		"skills block":            "<skills_instructions>\n## Skills\n</skills_instructions>",
		"DSH title call":          dshTitleUserPrefix + `["hello"]`,
		"Codex title call":        codexTitleUserPrefix + "\n\nUser prompt:\nfix this",
	} {
		t.Run(name, func(t *testing.T) {
			req := parseBody(t, `{"model":"m","input":[`+
				userItemJSON(t, "真正的问题")+`,`+userItemJSON(t, boilerplate)+`]}`)
			got, err := req.UserInputText(2000)
			if err != nil {
				t.Fatal(err)
			}
			if got != "真正的问题" {
				t.Fatalf("recorded %q, want the human message before the boilerplate", got)
			}
		})
	}
}

// A human message beyond the sanity bound is skipped like any other miss, and the search keeps
// going backwards: a pasted file must not become the body, and an older prompt still must.
func TestUserInputTextSkipsABeyondBoundHumanMessage(t *testing.T) {
	older := "先解决构建问题"
	huge := strings.Repeat("x", 2500)
	req := parseBody(t, `{"model":"m","input":[`+
		userItemJSON(t, older)+`,`+userItemJSON(t, huge)+`,`+userItemJSON(t, dshRuntimePrefix+" boilerplate")+`]}`)
	got, err := req.UserInputText(2000)
	if err != nil {
		t.Fatal(err)
	}
	if got != older {
		t.Fatalf("recorded %d characters, want the older prompt", len([]rune(got)))
	}
}

// Boilerplate is never stored, not even with the length bound switched off.
func TestUserInputTextSkipsBoilerplateWithoutABound(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[`+
		userItemJSON(t, "first question")+`,`+userItemJSON(t, dshRuntimePrefix+" boilerplate")+`,`+
		userItemJSON(t, "second question")+`]}`)
	got, err := req.UserInputText(0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "first question\nsecond question" {
		t.Fatalf("recorded %q, want both human messages and no boilerplate", got)
	}
}

// A message that merely CONTAINS a marker (a human quoting the runtime context line) is not
// boilerplate: only the opening decides.
func TestUserInputTextOnlyTheOpeningDecides(t *testing.T) {
	mentioning := "为什么日志里会出现 " + dshRuntimePrefix + " 这一行？"
	req := parseBody(t, `{"model":"m","input":[`+userItemJSON(t, mentioning)+`]}`)
	got, err := req.UserInputText(2000)
	if err != nil {
		t.Fatal(err)
	}
	if got != mentioning {
		t.Fatalf("recorded %q, want the message that merely mentions the marker", got)
	}
}

package responses

import (
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

// The threshold is strict: exactly maxChars is dropped, one character less is kept. This is
// the boundary most easily misread as <=, so it is pinned here.
func TestUserInputTextDropsAMessageAtTheThreshold(t *testing.T) {
	exactly := strings.Repeat("问", 100)
	req := parseBody(t, `{"model":"m","input":[`+userItem("earlier")+`,`+userItem(exactly)+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("a %d-character last message must not be recorded, got %q",
			len([]rune(exactly)), got)
	}

	justUnder := strings.Repeat("问", 99)
	req = parseBody(t, `{"model":"m","input":[`+userItem("earlier")+`,`+userItem(justUnder)+`]}`)
	got, err = req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != justUnder {
		t.Fatalf("a 99-character message must be kept verbatim, got %q", got)
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

// Content nobody can read cannot be measured, so it is not recorded — and the row is still
// written (the caller stores an empty body).
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
	// The image-only message contributes empty text, so the join leaves a blank line.
	want := "first\n" + long + "\n\nlast"
	if got != want {
		t.Fatalf("recorded %q, want every message joined", got)
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

// Only the LAST message decides: an earlier short message does not rescue a request whose
// last message is too long, and a long earlier message does not disqualify a short last one.
func TestUserInputTextOnlyTheLastMessageDecides(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[`+
		userItem(strings.Repeat("a", 20))+`,`+userItem(strings.Repeat("b", 300))+`]}`)
	got, err := req.UserInputText(100)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("a long LAST message must leave no body, got %d characters", len([]rune(got)))
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

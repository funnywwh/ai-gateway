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

func TestUserInputDocumentKeepsOnlyUserMessages(t *testing.T) {
	req := parseBody(t, `{
		"model":"deepseek-flash",
		"instructions":"be terse",
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"SYSTEM INSTRUCTION"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]},
			{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"file\":\"/etc/shadow\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ZZ_TOOL_OUTPUT_SECRET"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"an earlier answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"second question"}]}
		]
	}`)

	doc, err := req.UserInputDocument(210396, 100)
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	if len(doc.Input) != 2 {
		t.Fatalf("kept %d items, want the two user messages: %+v", len(doc.Input), doc.Input)
	}
	for _, item := range doc.Input {
		if item.Role != "user" {
			t.Fatalf("a non-user item was kept: %+v", item)
		}
	}
	if doc.Model != "deepseek-flash" || doc.Bytes != 210396 {
		t.Fatalf("envelope = %+v", doc)
	}
	// The cap is recorded even when nothing was cut, so a reader knows which policy the row
	// was written under; both questions here are far shorter than it.
	if doc.MaxChars != 100 || doc.Truncated {
		t.Fatalf("cap = %d/%v, want 100 with nothing truncated", doc.MaxChars, doc.Truncated)
	}
	want := map[string]int{"message:developer": 1, "function_call": 1, "function_call_output": 1, "reasoning": 1, "message:assistant": 1, "tools": 1, "instructions": 1}
	if len(doc.Omitted) != len(want) {
		t.Fatalf("omitted = %v, want %v", doc.Omitted, want)
	}
	for key, count := range want {
		if doc.Omitted[key] != count {
			t.Fatalf("omitted[%s] = %d, want %d (%v)", key, doc.Omitted[key], count, doc.Omitted)
		}
	}

	// The whole point: nothing but the user's own words is in the recorded document.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	for _, forbidden := range []string{"ZZ_TOOL_OUTPUT_SECRET", "SYSTEM INSTRUCTION", "an earlier answer", "/etc/shadow"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("recorded document leaked %q: %s", forbidden, encoded)
		}
	}
	for _, kept := range []string{"first question", "second question"} {
		if !strings.Contains(encoded, kept) {
			t.Fatalf("recorded document dropped the user's own input %q: %s", kept, encoded)
		}
	}
}

func TestUserInputDocumentAcceptsTheStringShorthand(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":"ping"}`)
	doc, err := req.UserInputDocument(31, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Input) != 1 || doc.Input[0].Role != "user" || !strings.Contains(string(doc.Input[0].Content), "ping") {
		t.Fatalf("string input must be kept as one user message: %+v", doc.Input)
	}
	if doc.Omitted != nil {
		t.Fatalf("nothing was omitted, got %v", doc.Omitted)
	}
}

// A continuation can carry only tool results. The document must still be written, and
// the empty input must serialize as [] so a reader can tell "no user text" from a broken
// record.
func TestUserInputDocumentWithNoUserItemsSerializesAsEmptyArray(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":"data"}]}`)
	doc, err := req.UserInputDocument(64, 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"input":[]`) {
		t.Fatalf("input must serialize as [], got %s", raw)
	}
	if doc.Omitted["function_call_output"] != 1 {
		t.Fatalf("omitted = %v", doc.Omitted)
	}
}

// The default policy keeps each user message's head, not the whole question (M81). The cap
// is per message on purpose: in a real DSH request the first user message is a runtime
// context snapshot, so a budget shared across the document would be spent on that
// boilerplate and hide the prompt the operator opened the log to read.
func TestUserInputDocumentCapsEachUserMessage(t *testing.T) {
	boilerplate := strings.Repeat("问", 150) // 150 characters, 450 bytes
	question := strings.Repeat("ab", 125)   // 250 characters
	req := parseBody(t, `{"model":"m","input":[`+
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"`+boilerplate+`"}]},`+
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"`+question+`"}]}]}`)

	doc, err := req.UserInputDocument(4096, 100)
	if err != nil {
		t.Fatal(err)
	}
	if doc.MaxChars != 100 || !doc.Truncated {
		t.Fatalf("cap = %d/%v, want 100 with a truncation reported", doc.MaxChars, doc.Truncated)
	}
	if len(doc.Input) != 2 {
		t.Fatalf("kept %d messages, want both", len(doc.Input))
	}
	if got := itemText(doc.Input[0]); got != strings.Repeat("问", 100) {
		t.Fatalf("first message kept %q", got)
	}
	if got := itemText(doc.Input[1]); got != strings.Repeat("ab", 50) {
		t.Fatalf("second message kept %q", got)
	}
	for i, item := range doc.Input {
		if got := len([]rune(itemText(item))); got != 100 {
			t.Fatalf("message %d kept %d characters, want 100", i, got)
		}
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// The cap is characters, so the tail of each message must be gone and the document must
	// still be valid UTF-8 (a byte cut would have split a Chinese character).
	if strings.Contains(string(raw), strings.Repeat("问", 101)) ||
		strings.Contains(string(raw), strings.Repeat("ab", 60)) {
		t.Fatalf("text past the cap survived: %s", raw)
	}
	if !utf8.Valid(raw) {
		t.Fatalf("the recorded document is not valid UTF-8: %q", raw)
	}
}

// 0 means no cap: the pre-M81 behaviour the config documents as the way back. The document
// must not claim a cap it did not apply.
func TestUserInputDocumentWithoutACapKeepsWholeMessages(t *testing.T) {
	text := strings.Repeat("x", 300)
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"`+text+`"}]}]}`)

	doc, err := req.UserInputDocument(500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if doc.MaxChars != 0 || doc.Truncated {
		t.Fatalf("a document with no cap claimed one: %d/%v", doc.MaxChars, doc.Truncated)
	}
	if got := itemText(doc.Input[0]); got != text {
		t.Fatalf("message was altered without a cap: %d characters kept", len([]rune(got)))
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "input_max_chars") || strings.Contains(string(raw), "input_truncated") {
		t.Fatalf("uncapped document must not carry cap fields: %s", raw)
	}
}

// A base64 image inside a user message would blow through the character cap while adding
// nothing an operator can read, so non-text parts are dropped and counted instead.
func TestUserInputDocumentDropsNonTextParts(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":[`+
		`{"type":"input_text","text":"look at this"},`+
		`{"type":"input_image","image_url":"data:image/png;base64,ZZBASE64IMAGE"},`+
		`{"type":"input_file","file_id":"ZZFILEID"}]}]}`)

	doc, err := req.UserInputDocument(900, 100)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Omitted["input_image"] != 1 || doc.Omitted["input_file"] != 1 {
		t.Fatalf("omitted = %v, want one image and one file", doc.Omitted)
	}
	if doc.Truncated {
		t.Fatalf("the text fit under the cap; only parts were dropped: %+v", doc)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ZZBASE64IMAGE", "data:image", "ZZFILEID"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("non-text content leaked %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), "look at this") {
		t.Fatalf("the message's own text must survive: %s", raw)
	}
}

// One message's parts share its budget, in order: "at most N characters per user message"
// only holds if it holds across the parts. A part that falls entirely past the cap is
// dropped and counted as over_cap — storing it empty would read like a message the client
// sent with no text at all.
func TestUserInputDocumentSharesTheBudgetAcrossTextParts(t *testing.T) {
	first := strings.Repeat("a", 60)
	question := strings.Repeat("b", 60)
	third := strings.Repeat("c", 60)
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":[`+
		`{"type":"input_text","text":"`+first+`"},`+
		`{"type":"input_text","text":"`+question+`"},`+
		`{"type":"input_text","text":"`+third+`"}]}]}`)

	doc, err := req.UserInputDocument(400, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := first + "\n" + strings.Repeat("b", 40)
	if got := itemText(doc.Input[0]); got != want {
		t.Fatalf("kept %q, want the first 100 characters across the parts", got)
	}
	if doc.Omitted["over_cap"] != 1 {
		t.Fatalf("omitted = %v, want the third part counted as over_cap", doc.Omitted)
	}
	if !doc.Truncated {
		t.Fatal("a cut message must set input_truncated")
	}
}

// A client may send a user message whose content is a bare string (the shape the gateway
// itself synthesizes for `"input":"…"`). It is bounded the same way and keeps its shape, so
// a reader who knows the wire format still recognises it.
func TestUserInputDocumentCapsStringContent(t *testing.T) {
	text := strings.Repeat("z", 250)
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":"`+text+`"}]}`)

	doc, err := req.UserInputDocument(300, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(doc.Input[0].Content); got != `"`+strings.Repeat("z", 100)+`"` {
		t.Fatalf("string content = %s", got)
	}
	if !doc.Truncated {
		t.Fatal("a cut message must set input_truncated")
	}
}

// Content the gateway cannot read (an object, null, malformed JSON) is dropped whole and
// counted. The cap exists to bound what a log row carries; content nobody can bound is
// exactly what must not be carried, and the request still gets its row.
func TestUserInputDocumentCountsUnreadableContent(t *testing.T) {
	req := parseBody(t, `{"model":"m","input":[{"type":"message","role":"user","content":{"nested":"ZZUNREADABLE"}}]}`)

	doc, err := req.UserInputDocument(120, 100)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Omitted[unreadableContentKey] != 1 {
		t.Fatalf("omitted = %v, want the unreadable content counted", doc.Omitted)
	}
	if len(doc.Input) != 1 || len(doc.Input[0].Content) != 0 {
		t.Fatalf("unreadable content must not be kept: %+v", doc.Input)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ZZUNREADABLE") {
		t.Fatalf("unreadable content leaked: %s", raw)
	}
}

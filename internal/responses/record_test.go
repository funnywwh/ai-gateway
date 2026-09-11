package responses

import (
	"encoding/json"
	"strings"
	"testing"
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

	doc, err := req.UserInputDocument(210396)
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
	doc, err := req.UserInputDocument(31)
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
	doc, err := req.UserInputDocument(64)
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

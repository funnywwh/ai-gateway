package responses

import (
	"encoding/json"
	"strings"
)

// UserInputText returns the text the default input policy (recording.record_input=user)
// records for this request, or "" when there is nothing worth keeping.
//
// The rule (M82, revised in v4.3.2): walk the user messages from the end and keep the newest
// one that is BOTH readable text and at most maxChars characters. Messages that are empty or
// too long are stepped over rather than ending the search — the row gets the newest message
// that actually says something, instead of nothing at all.
//
// Why this shape:
//
//   - In an agent loop the request body is not the user's input. One real request carried 67
//     input items, of which 2 were user messages — the rest were tool definitions and
//     function_call_output items holding whole file contents. Recording all of them turned
//     the request log into a second copy of every file an agent read (M23).
//   - The message worth having sits at the end: a DSH turn looks like
//     [813-character runtime-context snapshot][78-character prompt].
//   - Clients also end turns with messages that carry nothing usable — an empty continuation,
//     a tool-result-only turn, a whitespace-only string. Taking the literal last message made
//     those rows bodyless, which is why the search now steps back over them.
//   - A long message is not recorded at all rather than truncated (M81 kept the first 100
//     characters, which made a 1000-character question indistinguishable from a 100-character
//     one — a "full question" that was really a head). Keep it whole or do not keep it.
//   - The payload is plain text, not a JSON document (M82): the structure — items, parts,
//     omission tallies, thresholds — was noise for the person reading the log and for an agent
//     calling get_request. What is left is the one thing worth having.
//
// Want everything? Switching the key to recording.record_input=full keeps the whole request
// body verbatim, JSON and all, and is deliberately exempt from every rule above.
//
// maxChars is recording.input_max_chars: a message of exactly that many characters is kept
// (the boundary is <=). 0 means no length filter: every message that carries text is kept, in
// order, joined by newlines — still text only, never the tool definitions or images.
func (r *Request) UserInputText(maxChars int) (string, error) {
	items, apiErr := r.Items()
	if apiErr != nil {
		return "", apiErr
	}
	// Only the user's own messages: developer instructions, tool definitions, tool calls and
	// their outputs, earlier assistant turns and compaction items are what this policy exists
	// to leave out.
	msgs := make([]recordedUserMessage, 0, len(items))
	for _, item := range items {
		if item.Type != "message" || item.Role != "user" {
			continue
		}
		text, runes, readable := userMessageText(item.Content)
		msgs = append(msgs, recordedUserMessage{text: text, runes: runes, readable: readable})
	}
	if len(msgs) == 0 {
		return "", nil
	}
	if maxChars <= 0 {
		kept := make([]string, 0, len(msgs))
		for _, msg := range msgs {
			if msg.carriesText() {
				kept = append(kept, msg.text)
			}
		}
		return strings.Join(kept, "\n"), nil
	}
	// Newest first: the first message that carries text and fits is the one to record.
	// Stepping back matters — agent clients end a turn with a message that is empty (a
	// tool-result-only turn) or oversized (a whole file pasted in), and neither should cost
	// the row its body when an earlier message still says something useful.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if !msg.carriesText() || msg.runes > maxChars {
			continue
		}
		return msg.text, nil
	}
	return "", nil
}

// recordedUserMessage is one user message reduced to what the recording policy decides on: its text,
// how many characters that text is, and whether the content could be read at all.
type recordedUserMessage struct {
	text     string
	runes    int
	readable bool
}

// carriesText reports whether this message says anything worth recording. A message can be
// present and still carry nothing: an empty or whitespace-only string, an array whose only
// parts are images or files, or content this package cannot read at all. Those are stepped
// over rather than stored, because a log row holding "   " is indistinguishable to a reader
// from a row holding nothing — and because they are how agent clients end a turn that had no
// human input in it.
func (m recordedUserMessage) carriesText() bool {
	return m.readable && strings.TrimSpace(m.text) != ""
}

// userMessageText flattens one user message to text and reports its length in characters.
//
// Content is either a plain string (the shape the gateway itself synthesizes for
// `"input":"…"`) or an array of typed parts (the shape both clients send). A text part's
// "text" field is content; anything else in the array — an image, a file, a part type from a
// newer protocol — is content this package will not guess at and is left out. Content of a
// shape nobody can read (an object, malformed JSON) is reported as unreadable: it cannot be
// measured, so it cannot be bounded, so it is not recorded.
//
// The length counted is the length of the text this function returns, so the message that
// gets stored is exactly the message that passed the threshold. Multiple text parts are
// joined by a newline, and those separators count like any other character.
func userMessageText(content json.RawMessage) (string, int, bool) {
	raw := strings.TrimSpace(string(content))
	if raw == "" || raw == "null" {
		return "", 0, true
	}
	if !strings.HasPrefix(raw, "[") {
		var text string
		if err := json.Unmarshal([]byte(raw), &text); err != nil {
			return "", 0, false
		}
		return text, len([]rune(text)), true
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		return "", 0, false
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if !textPart(jsonString(part["type"])) {
			continue
		}
		var text string
		if err := json.Unmarshal(part["text"], &text); err != nil {
			continue
		}
		texts = append(texts, text)
	}
	joined := strings.Join(texts, "\n")
	return joined, len([]rune(joined)), true
}

// textPart reports whether a content part carries text this package records. These are the
// two carriers the clients actually send (see itemText and TitlePromptFingerprint); every
// other part type — an image, a file, something a newer protocol added — is left out.
func textPart(partType string) bool {
	switch strings.TrimSpace(partType) {
	case "input_text", "text":
		return true
	}
	return false
}

// jsonString reads a JSON string out of a raw field, "" when the field is absent or is not
// a string.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

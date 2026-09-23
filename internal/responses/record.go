package responses

import (
	"encoding/json"
	"strings"
)

// UserInputText returns the text the default input policy (recording.record_input=user)
// records for this request, or "" when there is nothing worth keeping.
//
// The rule (M82, revised in v4.3.2 and again in M83): walk the user messages from the end and
// keep the newest one that (a) carries readable text, (b) is NOT client boilerplate, and
// (c) is at most maxChars characters. Messages that fail any of those are stepped over rather
// than ending the search, so the row gets the newest thing a human actually wrote.
//
// Why each condition:
//
//   - In an agent loop the request body is not the user's input. One real request carried 67
//     input items, of which 2 were user messages — the rest were tool definitions and
//     function_call_output items holding whole file contents. Recording all of them turned
//     the request log into a second copy of every file an agent read (M23).
//   - Most of the user messages are the client talking to itself: a DSH turn is
//     [Current runtime context. … (542 chars)][the question (117 chars)], and a browser DSH
//     session replays its own system prompt in the same list. Those are boilerplate (see
//     boilerplatePrefixes) and are skipped — including when they are the last message, which
//     is the normal case.
//   - Length alone cannot make that distinction, which is what M82/M82.1 got wrong: a real
//     prompt can be longer than a short boilerplate block and shorter than a long one. With a
//     100-character rule, one deployment's every request (prompts of 117–521 characters, next
//     to a 542-character runtime snapshot) recorded nothing at all — see the M83 notes in
//     docs/request-log.md. Hence: markers decide, and the length threshold is only a sanity
//     bound (default 2000) so a pasted file does not become the log's body.
//   - A long message is not truncated (M81 kept the first 100 characters, which made a
//     1000-character question indistinguishable from a 100-character one — a "full question"
//     that was really a head). Keep it whole or do not keep it.
//   - The payload is plain text, not a JSON document (M82): the structure — items, parts,
//     omission tallies, thresholds — was noise for the person reading the log and for an agent
//     calling get_request. What is left is the one thing worth having.
//
// Want everything? Switching the key to recording.record_input=full keeps the whole request
// body verbatim, JSON and all, and is deliberately exempt from every rule above.
//
// maxChars is recording.input_max_chars: a message of exactly that many characters is kept
// (the boundary is <=). 0 means no length filter — every non-boilerplate message that carries
// text is kept, in order, joined by newlines; boilerplate is skipped either way, because
// storing the agent's own scaffolding as if it were the user's words is exactly what this
// policy exists to prevent.
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
		msgs = append(msgs, recordedUserMessage{
			text: text, runes: runes, readable: readable, boilerplate: boilerplateText(text),
		})
	}
	if len(msgs) == 0 {
		return "", nil
	}
	if maxChars <= 0 {
		kept := make([]string, 0, len(msgs))
		for _, msg := range msgs {
			if msg.carriesText() && !msg.boilerplate {
				kept = append(kept, msg.text)
			}
		}
		return strings.Join(kept, "\n"), nil
	}
	// Newest first: the first message that a human actually wrote is the one to record.
	// Stepping back matters — agent clients end a turn with boilerplate (a runtime-context
	// snapshot), nothing at all (a tool-result-only turn) or an oversized paste, and none of
	// those should cost the row its body when an earlier message still says something useful.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if !msg.carriesText() || msg.runes > maxChars || msg.boilerplate {
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
	// boilerplate marks a message the client generated rather than wrote: DSH's runtime-context
	// snapshot, Codex's environment block, either agent's own system prompt, the injected
	// skill/reminder blocks, and the machine-generated title calls.
	boilerplate bool
}

// boilerplatePrefixes are the openings that mark a user message as client-generated rather
// than human. They come from the same vocabulary the identity extractor uses (dimensions.go):
// DSH's runtime-context snapshot and its system prompt, Codex's environment block and its
// instructions, the injected skill/reminder blocks, and the two machine-generated title
// prompts. A message that opens with one of these is the agent scaffolding talking to itself.
//
// This list is why the length threshold is not the whole story: a real prompt can easily be
// longer than the boilerplate around it (a 117-character question next to a 542-character
// runtime snapshot), so length alone cannot tell them apart — the marker can.
var boilerplatePrefixes = []string{
	dshRuntimePrefix,
	codexEnvContextTag,
	dshDeveloperPrefix,
	codexInstructionPrefix,
	dshTitleUserPrefix,
	codexTitleUserPrefix,
	dshTitleSystemPrefix,
	"<system-reminder>",
	"<skills_instructions>",
}

// boilerplateText reports whether a message is client scaffolding. The check is on the opening
// of the message, because that is where every one of these clients puts its tag.
func boilerplateText(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" {
		return false
	}
	for _, prefix := range boilerplatePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
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

package responses

import (
	"encoding/json"
	"strings"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// unreadableContentKey tallies a user message whose content this package cannot read (an
// object, null, malformed JSON). Such content is dropped whole rather than stored: the
// point of the default policy is to bound what a log row carries, and content nobody can
// bound is exactly what must not be stored.
const unreadableContentKey = "message:user:content"

// UserInput is the recorded payload of the default input policy
// (recording.record_input=user): what the user wrote, plus a summary of everything the
// client sent that the gateway deliberately does not store.
//
// The default used to be the whole request body. In an agent loop that body is not the
// user's input: one real request carried 67 input items, of which 2 were user messages —
// the rest were tool definitions and function_call_output items holding whole file
// contents. Storing that by default turned the request log into a second copy of every
// file an agent read, and made "input" mean something the operator never chose.
//
// Since M81 the user's own messages are bounded too: each one keeps at most maxChars
// characters (see UserInputDocument). MaxChars and Truncated describe that cap, because a
// truncated question and a short one are otherwise indistinguishable in the record.
type UserInput struct {
	Model   string           `json:"model,omitempty"`
	Input   []pluginapi.Item `json:"input"`
	Omitted map[string]int   `json:"omitted,omitempty"`
	Bytes   int              `json:"request_bytes,omitempty"`
	// MaxChars is the per-message character cap this document was built under; absent when
	// there was none (recording.input_max_chars=0, i.e. the pre-M81 behaviour).
	MaxChars int `json:"input_max_chars,omitempty"`
	// Truncated reports that at least one user message was cut at the cap.
	Truncated bool `json:"input_truncated,omitempty"`
}

// UserInputDocument builds that document. bodyBytes is the serialized size of the whole
// request, which the caller already has; it is recorded so a reader can still tell a
// 200-byte prompt from a 200 KB one without the body being kept.
//
// maxChars is recording.input_max_chars as the deployment resolved it, and it applies to
// EACH kept user message. Per message rather than per document is deliberate: a DSH request
// usually carries 2-3 user messages and the first one is a runtime-context snapshot, so a
// shared budget would spend itself on that boilerplate and hide the prompt the operator is
// looking for. 0 (or less) means no cap: the whole message, which is what M23 shipped.
func (r *Request) UserInputDocument(bodyBytes, maxChars int) (*UserInput, error) {
	items, apiErr := r.Items()
	if apiErr != nil {
		return nil, apiErr
	}
	doc := &UserInput{
		Model:   r.Model,
		Input:   []pluginapi.Item{},
		Omitted: map[string]int{},
		Bytes:   bodyBytes,
	}
	if maxChars > 0 {
		doc.MaxChars = maxChars
	}
	for _, item := range items {
		if item.Type == "message" && item.Role == "user" {
			content, dropped, truncated := clampUserMessage(item.Content, maxChars)
			if truncated {
				doc.Truncated = true
			}
			for key, count := range dropped {
				doc.Omitted[key] += count
			}
			item.Content = content
			doc.Input = append(doc.Input, item)
			continue
		}
		doc.Omitted[omittedKey(item)]++
	}
	if len(r.Tools) > 0 {
		doc.Omitted["tools"] = len(r.Tools)
	}
	if strings.TrimSpace(r.Instructions) != "" {
		doc.Omitted["instructions"] = 1
	}
	if len(doc.Omitted) == 0 {
		doc.Omitted = nil
	}
	return doc, nil
}

// clampUserMessage bounds one user message's content to budget characters, the cap the
// default input policy applies per message (M81). It returns the rewritten content, the
// tally of what was dropped, and whether any text was cut.
//
// The budget is shared by every text part of the message, in order: "at most N characters
// per user message" only holds if it holds across the parts. A text part that does not fit
// at all is dropped and counted as over_cap rather than stored empty — an empty text part
// reads like the client sent nothing, which is a different fact.
//
// Non-text parts (images, files, anything a newer protocol adds) are dropped and counted by
// type: the cap is a bound on what the log carries, and a base64 image would blow through it
// while adding nothing an operator can read.
func clampUserMessage(content json.RawMessage, budget int) (json.RawMessage, map[string]int, bool) {
	if budget <= 0 || len(content) == 0 {
		return content, nil, false
	}
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return content, nil, false
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return nil, map[string]int{unreadableContentKey: 1}, false
		}
		kept, _, cut := takeText(text, budget)
		encoded, err := json.Marshal(kept)
		if err != nil {
			return nil, map[string]int{unreadableContentKey: 1}, false
		}
		return encoded, nil, cut
	case '[':
		return clampContentParts(content, budget)
	}
	return nil, map[string]int{unreadableContentKey: 1}, false
}

// clampContentParts applies one message's budget across its content parts.
func clampContentParts(content json.RawMessage, budget int) (json.RawMessage, map[string]int, bool) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, map[string]int{unreadableContentKey: 1}, false
	}
	remaining := budget
	truncated := false
	dropped := map[string]int{}
	kept := make([]map[string]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		partType := jsonString(part["type"])
		if !textPart(partType) {
			dropped[partTypeKey(partType)]++
			continue
		}
		rawText, hasText := part["text"]
		if !hasText {
			// Nothing to bound: a text part with no text field is kept as it arrived.
			kept = append(kept, part)
			continue
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			dropped[partTypeKey(partType)]++
			continue
		}
		piece, used, cut := takeText(text, remaining)
		remaining -= used
		if cut {
			truncated = true
		}
		if text != "" && piece == "" {
			// The whole part fell outside the cap.
			dropped["over_cap"]++
			continue
		}
		encoded, err := json.Marshal(piece)
		if err != nil {
			dropped[partTypeKey(partType)]++
			continue
		}
		part["text"] = encoded
		kept = append(kept, part)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, map[string]int{unreadableContentKey: 1}, false
	}
	return encoded, dropped, truncated
}

// takeText keeps at most budget characters of text and reports how many it kept, so the
// caller can spend the rest of a message's budget on later parts.
//
// Characters, not bytes: the cap is specified in characters (recording.input_max_chars) and
// the traffic here is mostly Chinese, where 100 bytes would be ~33 characters. Cutting on a
// rune boundary is also what keeps invalid UTF-8 out of the database. The kept prefix is
// returned verbatim — no trimming — because "the first N characters" is the whole promise.
func takeText(text string, budget int) (kept string, used int, cut bool) {
	if budget <= 0 {
		return "", 0, text != ""
	}
	runes := []rune(text)
	if len(runes) <= budget {
		return text, len(runes), false
	}
	return string(runes[:budget]), budget, true
}

// textPart reports whether a content part carries text this package can bound. These are
// the two carriers the clients actually send (see itemText and TitlePromptFingerprint);
// every other part type — an image, a file, something a newer protocol added — is content
// the gateway will not guess at, so it is dropped and counted instead.
func textPart(partType string) bool {
	switch strings.TrimSpace(partType) {
	case "input_text", "text":
		return true
	}
	return false
}

// partTypeKey names a dropped content part in the tally, falling back to "unknown" for a
// part with no usable type (the same word omittedKey uses for items).
func partTypeKey(partType string) string {
	partType = strings.TrimSpace(partType)
	if partType == "" {
		return "unknown"
	}
	return partType
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

// omittedKey names one dropped item in the document's tally: by type, and for messages
// by role too, because "one assistant turn" and "one developer instruction" are
// different diagnoses.
func omittedKey(item pluginapi.Item) string {
	itemType := strings.TrimSpace(item.Type)
	if itemType == "" {
		itemType = "unknown"
	}
	if itemType == "message" && item.Role != "" {
		return itemType + ":" + item.Role
	}
	return itemType
}

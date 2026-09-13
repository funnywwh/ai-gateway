package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// TitlePromptFingerprint matches only the first plain user prompt, never tool output
// or later turns. The title template embeds that same prompt after a fixed delimiter.
// No normalization beyond outer whitespace: similar prompts must not share an identity.
func (r *Request) TitlePromptFingerprint(d Dimensions) string {
	if d.Client != ClientCodex || d.SessionID == "" || d.Workspace == "" {
		return ""
	}
	// Explicit root metadata takes precedence over inferred associations.
	if d.CallKind == CallKindTitle && d.SessionID != r.SessionKey() {
		return ""
	}
	items, err := r.Items()
	if err != nil {
		return ""
	}
	for _, item := range items {
		if item.Type != "message" || item.Role != "user" {
			continue
		}
		// Mixed image/audio prompts cannot be matched by their text alone.
		var parts []struct {
			Type string `json:"type"`
		}
		if strings.HasPrefix(strings.TrimSpace(string(item.Content)), "[") {
			if json.Unmarshal(item.Content, &parts) != nil {
				return ""
			}
			for _, part := range parts {
				if part.Type != "input_text" && part.Type != "text" {
					return ""
				}
			}
		}
		text := strings.TrimSpace(itemText(item))
		if text == "" || strings.HasPrefix(text, codexEnvContextTag) {
			continue
		}
		// Unrecognized XML context or a tagged user prompt is ambiguous.
		if strings.HasPrefix(text, "<") {
			return ""
		}
		if d.CallKind == CallKindTitle {
			if !strings.HasPrefix(text, codexTitleUserPrefix) {
				return ""
			}
			_, text, _ = strings.Cut(text, "\n\nUser prompt:\n")
			text = strings.TrimSpace(text)
		} else if strings.HasPrefix(text, codexTitleUserPrefix) {
			return ""
		}
		if text == "" {
			return ""
		}
		digest := sha256.Sum256([]byte(text))
		return hex.EncodeToString(digest[:])
	}
	return ""
}

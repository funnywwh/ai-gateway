package httpapi

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/winger/ai-gateway/internal/responses"
)

// TestFeaturesOfMapsFormatLevelToCapability pins the capability a request actually needs.
//
// Every text.format used to be read as "needs json_schema", which excluded providers that
// serve json_object perfectly well (DeepSeek's /chat/completions among them) and blamed
// the client for asking for something the deployment does support. The level and the
// capability share one vocabulary, so they must map one to one.
func TestFeaturesOfMapsFormatLevelToCapability(t *testing.T) {
	request := func(text string) *responses.Request {
		body := `{"model":"m","input":"hi"`
		if text != "" {
			body += `,"text":` + text
		}
		body += `}`
		req, apiErr := responses.Parse([]byte(body))
		if apiErr != nil {
			t.Fatalf("Parse(%s): %v", body, apiErr)
		}
		return req
	}
	names := func(features map[string]bool) []string {
		out := make([]string, 0, len(features))
		for name := range features {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}

	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{name: "no text parameter", text: "", want: nil},
		{name: "explicit text level", text: `{"format":{"type":"text"}}`, want: nil},
		{name: "json_object", text: `{"format":{"type":"json_object"}}`, want: []string{"json_object"}},
		{name: "json_schema", text: `{"format":{"type":"json_schema","schema":{"type":"object"}}}`, want: []string{"json_schema"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(featuresOf(request(tc.text)))
			if len(got) != len(tc.want) {
				t.Fatalf("features = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("features = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestFeaturesOfKeepsTheOtherRequirements guards the mapping above from quietly replacing
// the rest of the derivation: a request carries several requirements at once, and the
// structured-output level is only one of them.
func TestFeaturesOfKeepsTheOtherRequirements(t *testing.T) {
	req, apiErr := responses.Parse([]byte(`{"model":"m","input":"hi","stream":true,` +
		`"parallel_tool_calls":true,"reasoning":{"effort":"high"},` +
		`"tools":[{"type":"function","name":"f","parameters":{}}],` +
		`"text":{"format":{"type":"json_object"}}}`))
	if apiErr != nil {
		t.Fatalf("Parse: %v", apiErr)
	}
	features := featuresOf(req)
	for _, want := range []string{"stream", "tools", "parallel_tools", "reasoning", "json_object"} {
		if !features[want] {
			t.Errorf("feature %q is missing from %v", want, features)
		}
	}
	if features["json_schema"] {
		t.Errorf("a json_object request must not require json_schema: %v", features)
	}
	if len(features) != 5 {
		t.Errorf("features = %v, want exactly the five the request asks for", features)
	}
}

// TestFormatLevelConstantVocabulary locks the two packages to one spelling of the levels:
// a typo on either side would silently change which capability a request requires.
func TestFormatLevelConstantVocabulary(t *testing.T) {
	for level, raw := range map[string]json.RawMessage{
		responses.TextFormatText:       json.RawMessage(`{"type":"text"}`),
		responses.TextFormatJSONObject: json.RawMessage(`{"type":"json_object"}`),
		responses.TextFormatJSONSchema: json.RawMessage(`{"type":"json_schema"}`),
	} {
		got, apiErr := responses.TextFormatLevel(raw)
		if apiErr != nil {
			t.Fatalf("TextFormatLevel(%s): %v", raw, apiErr)
		}
		if got != level {
			t.Errorf("TextFormatLevel(%s) = %q, want %q", raw, got, level)
		}
	}
}

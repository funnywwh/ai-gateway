package responses

import (
	"encoding/json"
	"testing"
)

// TestParseValidatesTextFormat pins the one rule that keeps a structured-output request
// from being silently downgraded: a level this gateway cannot name is refused at parse
// time, because the alternative is answering with plain text a client asked to parse.
func TestParseValidatesTextFormat(t *testing.T) {
	body := func(text string) []byte {
		return []byte(`{"model":"m","input":"hi","text":` + text + `}`)
	}
	for _, tc := range []struct {
		name string
		text string
		want bool // accepted?
	}{
		{name: "absent", text: `{}`, want: true},
		{name: "text", text: `{"format":{"type":"text"}}`, want: true},
		{name: "json_object", text: `{"format":{"type":"json_object"}}`, want: true},
		{name: "json_schema", text: `{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}}`, want: true},
		{name: "empty object", text: `{"format":{}}`, want: true},
		{name: "unknown type", text: `{"format":{"type":"yaml"}}`, want: false},
		{name: "format is a string", text: `{"format":"json"}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(body(tc.text))
			if tc.want {
				if err != nil {
					t.Fatalf("Parse(%s) = %v, want accepted", tc.text, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Parse(%s) accepted an unusable text.format", tc.text)
			}
			if err.Param == "" {
				t.Errorf("rejection carries no param: %+v", err)
			}
		})
	}
}

// TestToProviderRequestNormalisesTheTextLevel keeps "text" and "no format" from being two
// different things downstream: providers would each have to re-interpret "text", and the
// first one to forget would send response_format=text to an upstream that rejects it.
func TestToProviderRequestNormalisesTheTextLevel(t *testing.T) {
	build := func(text string) *Request {
		req, err := Parse([]byte(`{"model":"m","input":"hi","text":` + text + `}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return req
	}

	explicitText := build(`{"format":{"type":"text"}}`)
	out, apiErr := explicitText.ToProviderRequest("upstream-m")
	if apiErr != nil {
		t.Fatalf("ToProviderRequest: %v", apiErr)
	}
	if out.Text == nil {
		t.Fatal("text config was dropped entirely")
	}
	if len(out.Text.Format) != 0 {
		t.Errorf("an explicit text level reached the provider as %s, want no format", out.Text.Format)
	}

	schema := build(`{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}}`)
	out, apiErr = schema.ToProviderRequest("upstream-m")
	if apiErr != nil {
		t.Fatalf("ToProviderRequest: %v", apiErr)
	}
	var level struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out.Text.Format, &level); err != nil {
		t.Fatalf("json_schema level was not carried verbatim: %v", err)
	}
	if level.Type != "json_schema" || level.Name != "r" {
		t.Errorf("json_schema level = %s, want the client's object unchanged", out.Text.Format)
	}
}

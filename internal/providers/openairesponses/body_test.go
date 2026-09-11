package openairesponses

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// TestBodyPassesTheReasoningControlThrough pins the one wire contract the DSH/pi-ai
// effort selector depends on: whatever reasoning control the client sent reaches the
// Responses upstream unchanged inside the request body.
//
// It is the counterpart of pkg/providerkit's chat-completions assertion (a client's
// reasoning.effort becomes upstream reasoning_effort there). Without this, a refactor
// of the payload plumbing could silently drop the parameter and every effort level a
// client offers would look accepted while doing nothing.
func TestBodyPassesTheReasoningControlThrough(t *testing.T) {
	cases := []struct {
		name      string
		reasoning *pluginapi.Reasoning
		want      map[string]any // the reasoning object the upstream must see; nil = absent
	}{
		{name: "absent sends no reasoning object", reasoning: nil, want: nil},
		{
			name:      "effort is forwarded verbatim",
			reasoning: &pluginapi.Reasoning{Effort: "high"},
			want:      map[string]any{"effort": "high"},
		},
		{
			// The harness maps its "off" level to this spelling, so it must survive as-is:
			// it is what turns thinking off for a model whose own default is to think.
			name:      "none is a real value, not an omission",
			reasoning: &pluginapi.Reasoning{Effort: "none"},
			want:      map[string]any{"effort": "none"},
		},
		{
			name:      "a summary request rides along with the effort",
			reasoning: &pluginapi.Reasoning{Effort: "xhigh", Summary: "auto"},
			want:      map[string]any{"effort": "xhigh", "summary": "auto"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New("upstream", `{"base_url":"http://127.0.0.1:1","timeout_s":1}`, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			req := streamRequest()
			req.Reasoning = tc.reasoning

			raw, err := p.body(req, true)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("body is not a JSON object: %v (%s)", err, raw)
			}

			got, present := payload["reasoning"]
			switch {
			case tc.want == nil && present:
				t.Fatalf("reasoning must be absent when the client sent none, got %s", got)
			case tc.want != nil && !present:
				t.Fatalf("reasoning is missing from the upstream body: %s", raw)
			case tc.want != nil:
				var have map[string]any
				if err := json.Unmarshal(got, &have); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(tc.want, have) {
					t.Fatalf("reasoning = %s, want %v", got, tc.want)
				}
			}

			// stream is forced by the call site; a body that lost it would be read as a
			// non-streaming response and stall the attempt.
			if string(payload["stream"]) != "true" {
				t.Fatalf("stream = %s, want true", payload["stream"])
			}
		})
	}
}

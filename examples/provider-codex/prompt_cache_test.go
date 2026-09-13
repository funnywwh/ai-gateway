package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Exercise client parsing, canonical conversion, the plugin JSON boundary and
// Codex serialization: the upstream must receive the client's original key,
// not the trimmed/truncated key used for gateway session affinity.
func TestPromptCacheKeyReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"absent", ""},
		{"empty", ""},
		{"session", "session-123"},
		{"verbatim", "  会话-" + strings.Repeat("x", 160) + "  "},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				client := map[string]any{"model": "codex", "input": "hi", "stream": stream}
				if tc.name != "absent" {
					client["prompt_cache_key"] = tc.key
				}
				body, err := json.Marshal(client)
				if err != nil {
					t.Fatal(err)
				}
				req, apiErr := responses.Parse(body)
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				canonical, apiErr := req.ToProviderRequest("codex")
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				encoded, err := json.Marshal(canonical)
				if err != nil {
					t.Fatal(err)
				}
				var decoded pluginapi.Request
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				p := &provider{}
				upstream, err := p.buildRequest(&decoded, stream)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(upstream, &wire); err != nil {
					t.Fatal(err)
				}
				raw, present := wire["prompt_cache_key"]
				if tc.key == "" {
					if present {
						t.Fatalf("empty key must be omitted, got %s", raw)
					}
					return
				}
				var got string
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("missing or invalid upstream cache key: %s (%v)", upstream, err)
				}
				if got != tc.key {
					t.Fatalf("upstream cache key = %q, want %q", got, tc.key)
				}
			})
		}
	}
}

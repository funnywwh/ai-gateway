package main

import (
	"encoding/json"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestBuildRequestPreservesOptionalToolArguments(t *testing.T) {
	for _, mode := range []string{"omitted", "false", "true"} {
		t.Run(mode, func(t *testing.T) {
			tool := pluginapi.Tool{Type: "function", Name: "bash", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"sandbox_permissions":{"type":"string","enum":["danger-full-access"]}},"required":["command"]}`)}
			if mode != "omitted" {
				v := mode == "true"
				tool.Strict = &v
			}
			req := &pluginapi.Request{Model: "codex", Tools: []pluginapi.Tool{tool, {Type: "custom", Raw: json.RawMessage(`{"type":"custom","name":"echo"}`)}}}
			// Cross the same JSON boundary as the plugin subprocess.
			encoded, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var decoded pluginapi.Request
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			p := &provider{}
			body, err := p.buildRequest(&decoded, true)
			if err != nil {
				t.Fatal(err)
			}
			var wire responsesRequest
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Tools[0].Strict == nil || *wire.Tools[0].Strict != (mode == "true") {
				t.Fatalf("upstream strict = %v, want explicit %v", wire.Tools[0].Strict, mode == "true")
			}
			if string(wire.Tools[0].Parameters) != string(tool.Parameters) {
				t.Fatal("parameter schema changed")
			}
			if string(wire.Tools[1].Raw) != string(req.Tools[1].Raw) {
				t.Fatal("custom tool changed")
			}
			if mode == "omitted" && decoded.Tools[0].Strict != nil {
				t.Fatal("caller mutated")
			}
		})
	}
}

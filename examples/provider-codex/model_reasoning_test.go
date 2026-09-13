package main

import (
	"encoding/json"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func TestBuildRequestPassesModelReasoningAndFallsBackToProviderDefault(t *testing.T) {
	p := &provider{cfg: config{ReasoningEffort: "medium"}}

	for _, effort := range []string{"high", "max"} {
		t.Run(effort, func(t *testing.T) {
			req := &pluginapi.Request{Model: "codex", Reasoning: &pluginapi.Reasoning{Effort: effort, Summary: "brief"}}
			raw, err := p.buildRequest(req, false)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Reasoning *pluginapi.Reasoning `json:"reasoning"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if body.Reasoning == nil || body.Reasoning.Effort != effort || body.Reasoning.Summary != "brief" {
				t.Fatalf("reasoning = %+v, want request value", body.Reasoning)
			}
		})
	}

	raw, err := p.buildRequest(&pluginapi.Request{Model: "codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Reasoning *pluginapi.Reasoning `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Reasoning == nil || body.Reasoning.Effort != "medium" {
		t.Fatalf("provider fallback reasoning = %+v, want medium", body.Reasoning)
	}
}

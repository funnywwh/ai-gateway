package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Codex uses custom tools (including its exec tool), not just JSON function
// tools. Dropping this item makes a working agent silently finish its turn.
func TestCustomToolSurvivesProviderProtocolAndResponse(t *testing.T) {
	frames := []string{
		`{"type":"response.output_text.delta","delta":"I will inspect the files."}`,
		`{"type":"response.output_item.added","item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","input":"","status":"in_progress"}}`,
		`{"type":"response.custom_tool_call_input.delta","delta":"echo hello","item_id":"ctc_1"}`,
		`{"type":"response.output_item.done","item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","namespace":"functions","input":"echo hello","status":"completed"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":12}}}`,
	}
	for i := range frames {
		frames[i] = "data: " + frames[i] + "\n\n"
	}
	server := sseServer(t, 200, frames, nil)
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL, server.URL, map[string]string{"access_token": "test-token"})
	var done []byte
	a := responses.NewAssembler("codex", func(e *responses.Event) error {
		if e.Type == responses.EventOutputItemDone && e.Item.Type == "custom_tool_call" {
			done, _ = json.Marshal(e)
		}
		return nil
	})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(e pluginapi.Event) error {
		// Exercise the subprocess JSON boundary, where unknown item fields were
		// previously lost on the request side too.
		wire, err := json.Marshal(e)
		if err != nil {
			return err
		}
		var decoded pluginapi.Event
		if err := json.Unmarshal(wire, &decoded); err != nil {
			return err
		}
		return a.Add(decoded)
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Complete(a.Usage())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Output) != 2 || r.Output[1].Type != "custom_tool_call" {
		t.Fatalf("lost tool: %+v", r.Output)
	}
	for _, expected := range []string{`"input":"echo hello"`, `"namespace":"functions"`, `"call_id":"call_1"`, `"output_index":1`} {
		if !strings.Contains(string(done), expected) {
			t.Fatalf("missing %s in %s", expected, done)
		}
	}
	stored, err := json.Marshal(r.Output)
	if err != nil {
		t.Fatal(err)
	}
	var restored []responses.OutputItem
	if err := json.Unmarshal(stored, &restored); err != nil {
		t.Fatal(err)
	}
	if restored[1].Raw == nil || string(restored[1].Raw.Extra["input"]) != `"echo hello"` {
		t.Fatalf("stored tool lost: %s", stored)
	}
	if len(a.Items()) != 2 || a.Deltas() < 2 {
		t.Fatal("tool not retained for continuation/failover protection")
	}
	complete, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	replayed := responses.NewAssembler("codex", nil)
	if err := responses.FeedItems(replayed, complete.Items); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range replayed.Items() {
		if item.Type == "custom_tool_call" {
			found = true
		}
	}
	if !found {
		t.Fatal("non-streaming response lost tool")
	}
}

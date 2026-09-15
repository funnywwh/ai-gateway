package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

func compactionItem(encrypted string) pluginapi.Item {
	if encrypted == "" {
		return pluginapi.Item{Type: ItemTypeCompaction}
	}
	return pluginapi.Item{Type: ItemTypeCompaction, Extra: map[string]json.RawMessage{
		"encrypted_content": json.RawMessage(`"` + encrypted + `"`),
	}}
}

func textOf(t *testing.T, item pluginapi.Item) string {
	t.Helper()
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(item.Content, &parts); err != nil {
		t.Fatalf("content is not a content-part array: %v (%s)", err, item.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("content = %s, want a single part", item.Content)
	}
	return parts[0].Text
}

// TestCompactionRequestDetection covers both trigger shapes Codex has shipped and, more
// importantly, the near-misses: a compaction item that carries a payload is history the client
// replays, not a trigger, and treating it as one would run a summarization on every later turn.
func TestCompactionRequestDetection(t *testing.T) {
	cases := []struct {
		name  string
		items []pluginapi.Item
		want  bool
	}{
		{"new trigger", []pluginapi.Item{{Type: "message"}, {Type: ItemTypeCompactionTrigger}}, true},
		{"legacy trigger without payload", []pluginapi.Item{{Type: ItemTypeContextCompaction}}, true},
		{"context_compaction with payload is history", []pluginapi.Item{
			{Type: ItemTypeContextCompaction, Extra: map[string]json.RawMessage{"encrypted_content": json.RawMessage(`"blob"`)}},
		}, false},
		{"compaction item is history", []pluginapi.Item{compactionItem("gw1:abc")}, false},
		{"ordinary turn", []pluginapi.Item{{Type: "message", Role: "user"}}, false},
		{"no items", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCompactionRequest(tc.items); got != tc.want {
				t.Fatalf("IsCompactionRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompactionEnvelopeRoundTrip(t *testing.T) {
	const summary = "line one\n\"quoted\" 中文\n{}"
	encoded := EncodeCompactionSummary(summary)
	if !strings.HasPrefix(encoded, CompactionEnvelopePrefix) {
		t.Fatalf("encoded = %q, want the gateway prefix", encoded)
	}
	decoded, ok := DecodeCompactionSummary(encoded)
	if !ok || decoded != summary {
		t.Fatalf("DecodeCompactionSummary = (%q, %v), want (%q, true)", decoded, ok, summary)
	}
}

// A foreign blob is what a native upstream minted: unreadable here, readable by the upstream
// that minted it, so it must never be decoded, guessed at, or rewritten.
func TestDecodeCompactionSummaryRejectsForeignAndCorrupt(t *testing.T) {
	for _, value := range []string{
		"",
		"gAAAAABm-real-openai-blob",
		CompactionEnvelopePrefix, // prefix with no payload
		CompactionEnvelopePrefix + "not base64 !!", // payload is not base64
		CompactionEnvelopePrefix + "//79",          // base64 of invalid UTF-8 (0xff 0xfe 0xfd)
	} {
		if decoded, ok := DecodeCompactionSummary(value); ok {
			t.Fatalf("DecodeCompactionSummary(%q) = (%q, true), want false", value, decoded)
		}
	}
}

// TestPrepareCompactionRequestShapesTheUpstreamCall: the trigger item is an instruction to the
// client's compact task and no upstream models it, tools would let the model answer with a tool
// call instead of a summary, and the prompt is what makes the turn produce one.
func TestPrepareCompactionRequestShapesTheUpstreamCall(t *testing.T) {
	strict := false
	req := &pluginapi.Request{
		Input: []pluginapi.Item{
			{Type: "message", Role: "user"},
			{Type: ItemTypeCompactionTrigger},
		},
		Tools:             []pluginapi.Tool{{Type: "function", Name: "shell"}},
		ToolChoice:        json.RawMessage(`"auto"`),
		ParallelToolCalls: &strict,
	}
	PrepareCompactionRequest(req)

	if len(req.Input) != 2 {
		t.Fatalf("input = %d items, want the original message plus the prompt", len(req.Input))
	}
	for _, item := range req.Input {
		if item.Type == ItemTypeCompactionTrigger {
			t.Fatal("the trigger item must not reach the upstream")
		}
	}
	last := req.Input[len(req.Input)-1]
	if last.Type != "message" || last.Role != "user" {
		t.Fatalf("last item = %s/%s, want a user message", last.Type, last.Role)
	}
	if got := textOf(t, last); got != CompactionPrompt {
		t.Fatalf("prompt = %q, want the codex compaction prompt", got)
	}
	if len(req.Tools) != 0 || req.ToolChoice != nil || req.ParallelToolCalls != nil {
		t.Fatalf("tools must be cleared: %+v", req)
	}
}

// TestLocalizeCompactionItemsKeepsUpstreamsAbleToReadHistory: a replaying client sends the
// envelope back every turn, and an upstream that cannot read it would lose the compacted
// history entirely — which is the difference between compaction working and silently
// discarding the conversation.
func TestLocalizeCompactionItemsKeepsUpstreamsAbleToReadHistory(t *testing.T) {
	foreign := compactionItem("gAAAAABm-native-blob")
	items := []pluginapi.Item{
		{Type: "message", Role: "user"},
		compactionItem(EncodeCompactionSummary("we fixed the parser")),
		foreign,
	}
	out := LocalizeCompactionItems(items)
	if len(out) != len(items) {
		t.Fatalf("item count changed: %d -> %d", len(items), len(out))
	}
	// Order is untouched: the summary has to stay where the client put it in history.
	if out[0].Type != "message" {
		t.Fatalf("first item = %s, want the untouched message", out[0].Type)
	}
	localized := out[1]
	if localized.Type != "message" || localized.Role != "user" {
		t.Fatalf("envelope item = %s/%s, want a user message", localized.Type, localized.Role)
	}
	if got := textOf(t, localized); got != CompactionSummaryPrefix+"\nwe fixed the parser" {
		t.Fatalf("localized text = %q", got)
	}
	if out[2].Type != ItemTypeCompaction || out[2].Extra["encrypted_content"] == nil {
		t.Fatalf("foreign blob must travel untouched: %+v", out[2])
	}
}

func TestLocalizeCompactionItemsLeavesOrdinaryInputAlone(t *testing.T) {
	items := []pluginapi.Item{{Type: "message", Role: "user"}, {Type: "function_call", CallID: "c1"}}
	out := LocalizeCompactionItems(items)
	for i := range out {
		if out[i].Type != items[i].Type {
			t.Fatalf("item %d changed: %s -> %s", i, items[i].Type, out[i].Type)
		}
	}
}

// TestCompactionItemShapeIsWhatTheClientCounts: the only fields codex reads are type and
// encrypted_content, and the blob has to be an envelope this gateway can decode next turn.
func TestCompactionItemShapeIsWhatTheClientCounts(t *testing.T) {
	encoded, err := json.Marshal(CompactionItem("a summary"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire.Type != "compaction" {
		t.Fatalf("type = %q, want compaction", wire.Type)
	}
	if summary, ok := DecodeCompactionSummary(wire.EncryptedContent); !ok || summary != "a summary" {
		t.Fatalf("encrypted_content = %q (decoded %q, %v)", wire.EncryptedContent, summary, ok)
	}
}

func TestIsNativeCompactionItem(t *testing.T) {
	cases := map[string]bool{
		"compaction":         true,
		"compaction_summary": true,
		"context_compaction": false,
		"message":            false,
	}
	for itemType, want := range cases {
		if got := IsNativeCompactionItem(pluginapi.Item{Type: itemType}); got != want {
			t.Fatalf("IsNativeCompactionItem(%s) = %v, want %v", itemType, got, want)
		}
	}
}

// TestCompactionObserverHidesTheAnswerButKeepsTheCheckpoint: the client asked for a checkpoint,
// so the model's prose and thinking must not arrive as an assistant answer — while an upstream
// that answers natively, and the item the gateway synthesizes at the end, must get through.
func TestCompactionObserverHidesTheAnswerButKeepsTheCheckpoint(t *testing.T) {
	var seen []string
	send := CompactionObserver(func(event *Event) error {
		seen = append(seen, event.Type)
		return nil
	})
	events := []*Event{
		{Type: EventCreated},
		{Type: EventInProgress},
		{Type: EventOutputItemAdded, Item: &OutputItem{Type: "message"}},
		{Type: EventContentPartAdded, Item: &OutputItem{Type: "message"}},
		{Type: EventOutputTextDelta, Delta: "Here is the summary"},
		{Type: EventOutputTextDone},
		{Type: EventContentPartDone},
		{Type: EventOutputItemDone, Item: &OutputItem{Type: "message"}},
		{Type: EventReasoningSummaryDelta, Delta: "thinking"},
		{Type: EventOutputItemAdded, Item: &OutputItem{Type: ItemTypeCompaction}},
		{Type: EventOutputItemDone, Item: &OutputItem{Type: ItemTypeCompaction}},
		{Type: EventCompleted},
	}
	for _, event := range events {
		if err := send(event); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	want := []string{
		EventCreated, EventInProgress,
		EventOutputItemAdded, EventOutputItemDone,
		EventCompleted,
	}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("client saw %v, want %v", seen, want)
	}
}

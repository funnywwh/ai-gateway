package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestItemScopedEventsAlwaysCarryOutputIndex pins the wire contract that was broken by
// omitempty: the index fields are part of the event's identity, and the first item's
// index is 0. Clients key their item tables by output_index (Codex logs
// "OutputTextDelta without active item"; pi-ai, which DSH uses, looks the slot up by
// output_index and drops a delta it cannot place), so a missing zero is not "no
// information" — it is a different item than the one the delta belongs to.
func TestItemScopedEventsAlwaysCarryOutputIndex(t *testing.T) {
	cases := []struct {
		event        Event
		wantIndex    bool
		wantContent  bool
		wantSummary  bool
		wantPresence string
	}{
		{Event{Type: EventOutputItemAdded, OutputIndex: 0}, true, false, false, "output_index"},
		{Event{Type: EventOutputItemDone, OutputIndex: 0}, true, false, false, "output_index"},
		{Event{Type: EventContentPartAdded, OutputIndex: 0, ContentIndex: 0}, true, true, false, "content_index"},
		{Event{Type: EventOutputTextDelta, OutputIndex: 0, ContentIndex: 0, Delta: "x"}, true, true, false, "content_index"},
		{Event{Type: EventOutputTextDone, OutputIndex: 0, ContentIndex: 0, Text: "x"}, true, true, false, "content_index"},
		{Event{Type: EventRefusalDelta, OutputIndex: 0, ContentIndex: 0, Delta: "x"}, true, true, false, "content_index"},
		{Event{Type: EventFunctionArgsDelta, OutputIndex: 1, ItemID: "call_1", Delta: "{"}, true, false, false, "output_index"},
		{Event{Type: EventReasoningSummaryDelta, OutputIndex: 0, SummaryIndex: 0, Delta: "t"}, true, false, true, "summary_index"},
		// Not item-scoped: the index fields must stay out of these payloads.
		{Event{Type: EventCreated}, false, false, false, "output_index"},
		{Event{Type: EventCompleted}, false, false, false, "output_index"},
		{Event{Type: EventFailed}, false, false, false, "output_index"},
		{Event{Type: EventError}, false, false, false, "output_index"},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.event)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.event.Type, err)
		}
		got := string(raw)
		if has := strings.Contains(got, `"output_index"`); has != c.wantIndex {
			t.Errorf("%s: output_index present=%v want %v (%s)", c.event.Type, has, c.wantIndex, got)
		}
		if has := strings.Contains(got, `"content_index"`); has != c.wantContent {
			t.Errorf("%s: content_index present=%v want %v (%s)", c.event.Type, has, c.wantContent, got)
		}
		if has := strings.Contains(got, `"summary_index"`); has != c.wantSummary {
			t.Errorf("%s: summary_index present=%v want %v (%s)", c.event.Type, has, c.wantSummary, got)
		}
		if c.wantIndex && !strings.Contains(got, `"`+c.wantPresence+`":0`) &&
			!strings.Contains(got, `"`+c.wantPresence+`":1`) {
			t.Errorf("%s: expected a concrete %s value in %s", c.event.Type, c.wantPresence, got)
		}
	}
}

// TestEventMarshalKeepsTheOtherFields: custom marshalling must not drop anything the
// protocol carries alongside the indexes.
func TestEventMarshalKeepsTheOtherFields(t *testing.T) {
	ev := Event{
		Type: EventOutputTextDelta, SequenceNumber: 7, ItemID: "msg_1",
		OutputIndex: 0, ContentIndex: 0, Delta: "hello",
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != EventOutputTextDelta || decoded["item_id"] != "msg_1" || decoded["delta"] != "hello" {
		t.Fatalf("field mismatch: %s", raw)
	}
	if decoded["sequence_number"] != float64(7) {
		t.Fatalf("sequence_number lost: %s", raw)
	}
	if decoded["output_index"] != float64(0) {
		t.Fatalf("output_index must be 0, got %v (%s)", decoded["output_index"], raw)
	}
}

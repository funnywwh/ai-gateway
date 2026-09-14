package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// collect runs events through an assembler and returns the client-visible events plus the
// final response.
func collect(t *testing.T, events ...pluginapi.Event) ([]Event, *Response) {
	t.Helper()
	var sent []Event
	a := NewAssembler("codex-test", func(ev *Event) error {
		sent = append(sent, *ev)
		return nil
	})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err := a.Add(ev); err != nil {
			t.Fatalf("Add(%s): %v", ev.Type, err)
		}
	}
	resp, err := a.Complete(pluginapi.Usage{Dimensions: map[string]int64{"input": 5, "output": 3}})
	if err != nil {
		t.Fatal(err)
	}
	return sent, resp
}

// A codex client replays the items this gateway returned. The subscription backend is
// stateless (store=false), so it can only accept a replayed reasoning item when the item
// carries the id and the encrypted blob the backend itself issued. Production failure
// (2026-09-14, gptjp): the gateway published a reasoning item with an id of its own making and
// no blob, and the client's next turn died with
// "Item with id 'rs_…' not found. Items are not persisted when `store` is set to false."
func TestFinishedItemUpgradesTheDeltaBuiltItemInPlace(t *testing.T) {
	blob := "gAAAA_encrypted_reasoning_blob"
	finished := pluginapi.Item{
		Type: "reasoning", ID: "rs_upstream", Status: "completed", Summary: []pluginapi.SummaryPart{},
		Extra: map[string]json.RawMessage{"encrypted_content": json.RawMessage(`"` + blob + `"`)},
	}
	sent, resp := collect(t,
		pluginapi.Event{Type: pluginapi.EventReasoningDelta, ItemID: "rs_upstream", Text: "thinking"},
		pluginapi.Event{Type: pluginapi.EventOutputItemDone, Item: &finished},
		pluginapi.Event{Type: pluginapi.EventTextDelta, ItemID: "msg_upstream", Text: "answer"},
	)

	reasoning := []OutputItem{}
	for _, item := range resp.Output {
		if item.Type == "reasoning" {
			reasoning = append(reasoning, item)
		}
	}
	if len(reasoning) != 1 {
		t.Fatalf("expected exactly one reasoning item, got %d: %+v", len(reasoning), resp.Output)
	}
	if reasoning[0].ID != "rs_upstream" {
		t.Fatalf("reasoning id = %q, want the upstream's own id", reasoning[0].ID)
	}
	encoded, err := json.Marshal(reasoning[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), blob) {
		t.Fatalf("the encrypted blob is missing from the delivered item: %s", encoded)
	}

	// The client sees the upstream's id from the first delta on, in one finish sequence.
	deltas := 0
	dones := 0
	for _, ev := range sent {
		switch ev.Type {
		case EventReasoningSummaryDelta:
			deltas++
			if ev.ItemID != "rs_upstream" {
				t.Errorf("delta item id = %q, want the upstream's", ev.ItemID)
			}
		case EventOutputItemDone:
			if ev.Item != nil && ev.Item.ID != "rs_upstream" {
				continue // the message item finishes through its own done event
			}
			dones++
			if ev.Item == nil || ev.Item.ID != "rs_upstream" {
				t.Errorf("done event carries %+v, want the upgraded item", ev.Item)
			}
		}
	}
	if deltas == 0 {
		t.Error("streaming the chain of thought must keep working")
	}
	if dones != 1 {
		t.Errorf("reasoning item finished %d times, want exactly one done event", dones)
	}
}

// The blob also has to survive into the stored response, which is what a
// previous_response_id continuation replays.
func TestStoredItemsKeepTheUpstreamIdentity(t *testing.T) {
	finished := pluginapi.Item{
		Type: "reasoning", ID: "rs_stored", Status: "completed", Summary: []pluginapi.SummaryPart{},
		Extra: map[string]json.RawMessage{"encrypted_content": json.RawMessage(`"blob"`)},
	}
	_, resp := collect(t,
		pluginapi.Event{Type: pluginapi.EventReasoningDelta, ItemID: "rs_stored", Text: "thought"},
		pluginapi.Event{Type: pluginapi.EventOutputItemDone, Item: &finished},
	)
	stored, err := json.Marshal(resp.Output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), `"encrypted_content":"blob"`) {
		t.Fatalf("stored output lost the blob: %s", stored)
	}
	if !strings.Contains(string(stored), `"id":"rs_stored"`) {
		t.Fatalf("stored output lost the upstream id: %s", stored)
	}
}

// A provider that only sends deltas (no finished item) keeps the gateway-generated id: the
// identity fix must not require an id the upstream never gave.
func TestDeltasWithoutAnItemIDStillWork(t *testing.T) {
	_, resp := collect(t, pluginapi.Event{Type: pluginapi.EventReasoningDelta, Text: "thought"})
	if len(resp.Output) != 1 {
		t.Fatalf("output = %+v", resp.Output)
	}
	if !strings.HasPrefix(resp.Output[0].ID, "rs_") {
		t.Fatalf("reasoning id = %q, want a generated rs_ id", resp.Output[0].ID)
	}
}

// The same finished item arriving twice must not duplicate it for the client.
func TestRepeatedFinishedItemIsNotDuplicated(t *testing.T) {
	finished := pluginapi.Item{Type: "reasoning", ID: "rs_twice", Status: "completed",
		Summary: []pluginapi.SummaryPart{}}
	_, resp := collect(t,
		pluginapi.Event{Type: pluginapi.EventOutputItemDone, Item: &finished},
		pluginapi.Event{Type: pluginapi.EventOutputItemDone, Item: &finished},
	)
	if len(resp.Output) != 1 {
		t.Fatalf("output = %+v, want the item once", resp.Output)
	}
}

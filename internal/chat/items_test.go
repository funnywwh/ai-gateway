package chat

import (
	"strings"
	"testing"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/pkg/pluginapi"
)

// buildHistory is the one decision point about what the model gets to see, so it is tested
// as a pure function: stored messages in, provider items out. The service-level wiring (the
// turn actually replays what this returns) is covered in chat_test.go.

func histUser(turnID, id, text string) *domain.ChatMessage {
	return &domain.ChatMessage{
		ID: id, SessionID: "chat_1", TurnID: turnID, Role: domain.ChatRoleUser,
		Content: text, Status: domain.ChatMessageOK,
	}
}

func histAssistant(turnID, id string, items ...pluginapi.Item) *domain.ChatMessage {
	return &domain.ChatMessage{
		ID: id, SessionID: "chat_1", TurnID: turnID, Role: domain.ChatRoleAssistant,
		Content: "答 " + id, ProviderItems: encodeItems(items), Status: domain.ChatMessageOK,
	}
}

// histConversation is a conversation of three finished turns (one of them with a tool
// exchange) plus the question being asked now — the shape a real session has when the turn
// loop reads it. Seven stored messages, eight provider items.
func histConversation() []*domain.ChatMessage {
	return []*domain.ChatMessage{
		histUser("turn_1", "c1", "第一问：记住 4271"),
		histAssistant("turn_1", "c2", assistantTextItem("好的")),
		histUser("turn_2", "c3", "第二问"),
		histAssistant("turn_2", "c4",
			functionCallItem("call_1", "admin_request", `{"name":"admin_list_accounts"}`),
			functionOutputItem("call_1", `{"result":"2"}`)),
		histUser("turn_3", "c5", "第三问"),
		histAssistant("turn_3", "c6", assistantTextItem("第三答")),
		histUser("turn_4", "c7", "第四问（现在）"),
	}
}

// itemShapes renders the identity of each item as "type:call_id", which is all these tests
// need to compare (Item itself carries a map and slices, so it is not comparable).
func itemShapes(items []pluginapi.Item) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.Type+":"+item.CallID)
	}
	return strings.Join(parts, " ")
}

func TestBuildHistoryWithoutAWindowReplaysEverything(t *testing.T) {
	plan := buildHistory(histConversation(), 0, 0)
	if plan.dropped != 0 || plan.tooLarge {
		t.Fatalf("no window must not drop or refuse anything: %+v", plan)
	}
	if plan.turns != 4 || plan.messages != 7 {
		t.Fatalf("turns/messages = %d/%d, want 4/7", plan.turns, plan.messages)
	}
	if len(plan.items) != 8 {
		t.Fatalf("items = %d, want every stored message: %+v", len(plan.items), plan.items)
	}
	// Order is reading order, oldest first: the first question is the first thing the model
	// sees, which is exactly what the window used to throw away.
	if !strings.Contains(string(plan.items[0].Content), "第一问") {
		t.Fatalf("the oldest message is missing from the replay: %+v", plan.items[0])
	}
	if !strings.Contains(string(plan.items[7].Content), "第四问") {
		t.Fatalf("the newest message is not last: %+v", plan.items[7])
	}
	if !strings.Contains(itemShapes(plan.items), "function_call:call_1 function_call_output:call_1") {
		t.Fatalf("the tool exchange was not replayed in order: %q", itemShapes(plan.items))
	}
}

// TestBuildHistoryBoundsAreIndependent pins the two bounds apart from each other: an
// operator who configures only one of them still gets the whole conversation on the other
// axis. The combined check this replaced treated "maxMessages = 0" as "nothing fits" and
// dropped every turn but the newest.
func TestBuildHistoryBoundsAreIndependent(t *testing.T) {
	// Messages bounded, bytes not: three messages is the newest turn plus the one before it.
	plan := buildHistory(histConversation(), 3, 0)
	if plan.messages != 3 || plan.turns != 2 {
		t.Fatalf("message window = %d messages / %d turns, want 3/2", plan.messages, plan.turns)
	}
	if plan.dropped != 2 {
		t.Fatalf("dropped = %d, want the two older turns", plan.dropped)
	}
	if plan.tooLarge {
		t.Fatal("the newest turn fits the window; nothing may be refused")
	}

	// Bytes bounded, messages not: nothing but the newest question survives, which is a
	// refusal rather than a silently empty request.
	tiny := buildHistory(histConversation(), 0, 1)
	if tiny.turns != 1 || tiny.dropped != 3 {
		t.Fatalf("byte window kept %d turns and dropped %d, want 1/3", tiny.turns, tiny.dropped)
	}
	if !tiny.tooLarge {
		t.Fatal("a byte window that cannot fit the newest turn must refuse that turn")
	}

	// A byte window with room for everything drops nothing, even though maxMessages is 0.
	roomy := buildHistory(histConversation(), 0, 1<<20)
	if roomy.dropped != 0 || roomy.tooLarge || len(roomy.items) != 8 {
		t.Fatalf("a roomy byte window changed the replay: %+v", roomy)
	}
}

func TestBuildHistoryKeepsTheNewestWholeTurnsInsideAWindow(t *testing.T) {
	// Five messages fit the newest three turns, so the oldest turn is dropped as a whole:
	// its question never arrives without its answer.
	plan := buildHistory(histConversation(), 5, 0)
	if plan.messages != 5 || plan.turns != 3 || plan.dropped != 1 {
		t.Fatalf("messages/turns/dropped = %d/%d/%d, want 5/3/1", plan.messages, plan.turns, plan.dropped)
	}
	for _, item := range plan.items {
		if strings.Contains(string(item.Content), "第一问") {
			t.Fatalf("the dropped turn is still in the replay: %+v", item)
		}
	}
}

func TestBuildHistoryOnlyRefusesWhenAWindowCannotFitTheNewestTurn(t *testing.T) {
	big := []*domain.ChatMessage{histUser("turn_1", "c1", strings.Repeat("很长的提问", 100))}
	if plan := buildHistory(big, 0, 0); plan.tooLarge {
		t.Fatal("with no window there is nothing to exceed")
	}
	if plan := buildHistory(big, 2, 8); !plan.tooLarge {
		t.Fatal("a window smaller than the newest turn must refuse it")
	}
}

// TestPairToolItemsDropsHalvesThatLostTheirPartner covers the shapes stored history can
// hold when a turn was interrupted before its tool ran.
func TestPairToolItemsDropsHalvesThatLostTheirPartner(t *testing.T) {
	message := pluginapi.Item{Type: "message", Role: "assistant", Content: []byte(`"ok"`)}
	call := functionCallItem("call_1", "admin_request", `{}`)
	output := functionOutputItem("call_1", `{"result":"2"}`)
	dangling := functionCallItem("call_2", "admin_request", `{}`)
	orphan := functionOutputItem("call_3", `{"result":"stray"}`)
	anonymous := functionCallItem("", "admin_request", `{}`)

	paired := pairToolItems([]pluginapi.Item{message, dangling, call, output, orphan, anonymous})
	if got, want := itemShapes(paired), "message: function_call:call_1 function_call_output:call_1"; got != want {
		t.Fatalf("paired = %q, want %q (order must survive)", got, want)
	}

	// A history that is already consistent is returned untouched.
	clean := []pluginapi.Item{message, call, output}
	if got := itemShapes(pairToolItems(clean)); got != itemShapes(clean) {
		t.Fatalf("a consistent history was rewritten: %q", got)
	}
}

func TestBuildHistoryDropsUnansweredToolCallsFromStoredHistory(t *testing.T) {
	// The shape an aborted turn leaves behind: the model announced a call, the answer was
	// stopped before the tool ran. Providers reject an unmatched call, and with no window
	// there is nothing left to slide it out of, so it must not be replayed.
	messages := []*domain.ChatMessage{
		histUser("turn_1", "c1", "问一"),
		histAssistant("turn_1", "c2",
			assistantTextItem("先查一下"),
			functionCallItem("call_9", "admin_request", `{"name":"x"}`)),
		histUser("turn_2", "c3", "继续"),
	}
	plan := buildHistory(messages, 0, 0)
	for _, item := range plan.items {
		if item.Type == "function_call" {
			t.Fatalf("an unanswered call reached the replay: %+v", item)
		}
	}
	// Three messages survive: the two questions and the answer text that preceded the call.
	if got, want := itemShapes(plan.items), "message: message: message:"; got != want {
		t.Fatalf("shapes = %q, want %q (the call is dropped, nothing is invented)", got, want)
	}
}

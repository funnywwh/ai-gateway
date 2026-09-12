package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
)

// The console chat stores the only rows in this schema owned by an *administrator account*
// rather than by a billing account, so these tests are about ownership: what one
// administrator can read, what a delete takes with it, and what must never be shared.

func chatStoreFixture(t *testing.T) (*DB, int64, int64) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "chat.db")
	db, err := Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	alice, err := db.UpsertAdminUser(ctx, &domain.AdminUser{Username: "alice", PasswordHash: "x", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := db.UpsertAdminUser(ctx, &domain.AdminUser{Username: "bob", PasswordHash: "y", Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	return db, alice, bob
}

func newChatSession(t *testing.T, db *DB, owner int64, id string) *domain.ChatSession {
	t.Helper()
	session := &domain.ChatSession{
		ID: id, OwnerUserID: owner, OwnerName: "alice", Model: "m",
		AccountID: 1, APIKeyID: 2, WriteMode: domain.ChatWriteModeReadOnly, Status: "active",
	}
	if err := db.CreateChatSession(context.Background(), session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func TestChatSessionsAreOwnerScoped(t *testing.T) {
	ctx := context.Background()
	db, alice, bob := chatStoreFixture(t)
	newChatSession(t, db, alice, "chat_a")

	// The owner sees it; the other administrator gets not-found rather than a 403, so the
	// console cannot be used to probe which conversations exist.
	if _, err := db.GetChatSession(ctx, "chat_a", alice); err != nil {
		t.Fatalf("owner read failed: %v", err)
	}
	if _, err := db.GetChatSession(ctx, "chat_a", bob); err == nil {
		t.Fatal("another administrator could read the conversation")
	}
	list, total, err := db.ListChatSessions(ctx, bob, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 || total != 0 {
		t.Fatalf("another administrator's list was not empty: %d/%d", len(list), total)
	}
	// A zero owner matches nothing: there is no "all owners" escape hatch in the queries.
	if list, total, err := db.ListChatSessions(ctx, 0, 10, 0); err != nil || len(list) != 0 || total != 0 {
		t.Fatalf("owner 0 must match nothing: %d/%d err=%v", len(list), total, err)
	}
	titled := *newChatSession(t, db, alice, "chat_b")
	titled.Title = "改过的标题"
	titled.OwnerUserID = bob
	if err := db.UpdateChatSession(ctx, &titled); err == nil {
		t.Fatal("an update carrying another owner must not write")
	}
	if err := db.DeleteChatSession(ctx, "chat_a", bob); err == nil {
		t.Fatal("another administrator could delete the conversation")
	}
}

func TestDeletingASessionTakesItsChildren(t *testing.T) {
	ctx := context.Background()
	db, alice, _ := chatStoreFixture(t)
	session := newChatSession(t, db, alice, "chat_a")

	turn := &domain.ChatTurn{ID: "turn_row", SessionID: session.ID, TurnID: "turn_1", Status: domain.ChatTurnRunning}
	if err := db.CreateChatTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendChatMessage(ctx, &domain.ChatMessage{
		ID: "cmsg_1", SessionID: session.ID, TurnID: "turn_1", Role: domain.ChatRoleUser,
		Content: "问题", Status: domain.ChatMessageOK,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateChatToolCall(ctx, &domain.ChatToolCall{
		ID: "tcall_1", SessionID: session.ID, TurnID: "turn_1", Step: 1, CallID: "call_1",
		Name: "admin_request", Status: domain.ChatToolPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChatArtifact(ctx, &domain.ChatArtifact{
		ID: "art_1", OwnerUserID: alice, SessionID: session.ID, Key: "msg_1:0",
		Format: domain.ChatArtifactHTML, Body: "<h1>x</h1>", SizeBytes: 11,
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteChatSession(ctx, session.ID, alice); err != nil {
		t.Fatal(err)
	}
	if messages, err := db.ListChatMessages(ctx, session.ID, 0); err != nil || len(messages) != 0 {
		t.Fatalf("messages survived the delete: %d err=%v", len(messages), err)
	}
	if calls, err := db.ListChatToolCalls(ctx, session.ID, 10); err != nil || len(calls) != 0 {
		t.Fatalf("tool calls survived the delete: %d err=%v", len(calls), err)
	}
	if _, err := db.GetChatTurn(ctx, session.ID, "turn_1"); err == nil {
		t.Fatal("the turn row survived the delete")
	}
	if _, err := db.GetChatArtifact(ctx, "art_1", alice); err == nil {
		t.Fatal("a preview payload survived the conversation it belongs to")
	}
}

func TestChatTurnIdentityAndRecovery(t *testing.T) {
	ctx := context.Background()
	db, alice, _ := chatStoreFixture(t)
	session := newChatSession(t, db, alice, "chat_a")

	turn := &domain.ChatTurn{ID: "turn_row", SessionID: session.ID, TurnID: "turn_1", Status: domain.ChatTurnRunning}
	if err := db.CreateChatTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	// The client's turn id is the idempotency key: a resend must lose the race, not create
	// a second row that would bill the same question twice.
	duplicate := &domain.ChatTurn{ID: "turn_other", SessionID: session.ID, TurnID: "turn_1"}
	if err := db.CreateChatTurn(ctx, duplicate); err == nil {
		t.Fatal("a duplicate turn id was accepted")
	}

	if err := db.CreateChatToolCall(ctx, &domain.ChatToolCall{
		ID: "tcall_1", SessionID: session.ID, TurnID: "turn_1", Step: 1, CallID: "call_1",
		Name: "admin_request", Status: domain.ChatToolPending,
	}); err != nil {
		t.Fatal(err)
	}
	// The same call id in a different step is a different call: the unique key is
	// (turn, step, call_id), not the call id alone.
	if err := db.CreateChatToolCall(ctx, &domain.ChatToolCall{
		ID: "tcall_2", SessionID: session.ID, TurnID: "turn_1", Step: 2, CallID: "call_1",
		Name: "admin_request", Status: domain.ChatToolPending,
	}); err != nil {
		t.Fatalf("the same call id in another step must be allowed: %v", err)
	}

	if n, err := db.InterruptRunningChatTurns(ctx); err != nil || n != 1 {
		t.Fatalf("interrupted = %d err=%v", n, err)
	}
	recovered, err := db.GetChatTurn(ctx, session.ID, "turn_1")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != domain.ChatTurnInterrupted {
		t.Fatalf("turn status = %q", recovered.Status)
	}
	calls, _ := db.ListChatToolCalls(ctx, session.ID, 10)
	for _, call := range calls {
		if call.Status != domain.ChatToolUnknown {
			t.Fatalf("pending tool call %s became %q, want unknown", call.CallID, call.Status)
		}
	}
}

func TestChatMessagesKeepOrderAndProviderItems(t *testing.T) {
	ctx := context.Background()
	db, alice, _ := chatStoreFixture(t)
	session := newChatSession(t, db, alice, "chat_a")

	for i := 1; i <= 3; i++ {
		if err := db.AppendChatMessage(ctx, &domain.ChatMessage{
			ID: "cmsg_" + string(rune('0'+i)), SessionID: session.ID, TurnID: "turn_1",
			Role: domain.ChatRoleUser, Content: "q", Status: domain.ChatMessageOK,
		}); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := db.ListChatMessages(ctx, session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages = %d", len(messages))
	}
	for i, message := range messages {
		if message.Seq != i+1 {
			t.Fatalf("seq[%d] = %d, want %d", i, message.Seq, i+1)
		}
	}
	// A tail read returns the newest window in reading order.
	tail, err := db.ListChatMessages(ctx, session.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[0].Seq != 2 || tail[1].Seq != 3 {
		t.Fatalf("tail = %+v", tail)
	}

	assistant := &domain.ChatMessage{
		ID: "cmsg_a", SessionID: session.ID, TurnID: "turn_1", Role: domain.ChatRoleAssistant,
		Content: "a", Status: domain.ChatMessageOK,
		Parts:         []domain.ChatPart{{Type: "text", Text: "a"}, {Type: "tool_call", ID: "call_1", Name: "admin_request", Result: "{}"}},
		ProviderItems: `[{"type":"function_call","call_id":"call_1","name":"admin_request","arguments":"{}"}]`,
		RequestIDs:    []string{"req_1", "req_2"},
		Truncated:     true,
	}
	if err := db.AppendChatMessage(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetChatMessage(ctx, session.ID, "cmsg_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Parts) != 2 || stored.Parts[1].Name != "admin_request" {
		t.Fatalf("parts round-trip: %+v", stored.Parts)
	}
	if len(stored.RequestIDs) != 2 || !stored.Truncated {
		t.Fatalf("request ids/truncated round-trip: %+v", stored)
	}
	if stored.ProviderItems == "" {
		t.Fatal("canonical provider items were lost")
	}
	if _, err := db.GetChatMessage(ctx, "chat_other", "cmsg_a"); err == nil {
		t.Fatal("a message must not be readable through another session id")
	}
}

func TestChatSkillsAreOwnerScopedAndUnique(t *testing.T) {
	ctx := context.Background()
	db, alice, bob := chatStoreFixture(t)

	aliceSkill := &domain.ChatSkill{OwnerUserID: alice, Name: "成本排查", Instructions: "步骤"}
	if _, err := db.CreateChatSkill(ctx, aliceSkill); err != nil {
		t.Fatal(err)
	}
	// The name is unique per owner, not globally: two administrators may each keep a skill
	// called "成本排查".
	if _, err := db.CreateChatSkill(ctx, &domain.ChatSkill{OwnerUserID: bob, Name: "成本排查", Instructions: "步骤"}); err != nil {
		t.Fatalf("another owner must be able to reuse the name: %v", err)
	}
	if _, err := db.CreateChatSkill(ctx, &domain.ChatSkill{OwnerUserID: alice, Name: "成本排查", Instructions: "别的"}); err == nil {
		t.Fatal("a duplicate name for the same owner was accepted")
	}
	if _, err := db.GetChatSkill(ctx, aliceSkill.ID, bob); err == nil {
		t.Fatal("another administrator could read the skill")
	}
	if list, total, err := db.ListChatSkills(ctx, bob, 10, 0); err != nil || total != 1 || list[0].OwnerUserID != bob {
		t.Fatalf("bob's library = %d/%d err=%v", len(list), total, err)
	}
	if err := db.DeleteChatSkill(ctx, aliceSkill.ID, bob); err == nil {
		t.Fatal("another administrator could delete the skill")
	}

	// Editing one's own skill updates it.
	aliceSkill.Instructions = "新步骤"
	if err := db.UpdateChatSkill(ctx, aliceSkill); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetChatSkill(ctx, aliceSkill.ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Instructions != "新步骤" {
		t.Fatalf("instructions = %q", updated.Instructions)
	}

	// Deleting the administrator removes their library (admin_users cascade).
	if err := db.DeleteAdminSession(ctx, "none"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.write.ExecContext(ctx, "DELETE FROM admin_users WHERE id = ?", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetChatSkill(ctx, aliceSkill.ID, alice); err == nil {
		t.Fatal("a skill survived its owner")
	}
}

func TestChatArtifactsUpsertAndPrune(t *testing.T) {
	ctx := context.Background()
	db, alice, bob := chatStoreFixture(t)
	session := newChatSession(t, db, alice, "chat_a")

	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		if err := db.UpsertChatArtifact(ctx, &domain.ChatArtifact{
			ID: "art_" + string(rune('a'+i)), OwnerUserID: alice, SessionID: session.ID,
			Key: "block-" + string(rune('a'+i)), Format: domain.ChatArtifactHTML,
			Body: "<h1>x</h1>", SizeBytes: 11, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-previewing the same block replaces the row instead of piling up copies.
	if err := db.UpsertChatArtifact(ctx, &domain.ChatArtifact{
		ID: "art_new", OwnerUserID: alice, SessionID: session.ID, Key: "block-a",
		Format: domain.ChatArtifactSVG, Body: "<svg/>", SizeBytes: 6, CreatedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	existing, err := db.GetChatArtifact(ctx, "art_a", alice)
	if err != nil {
		t.Fatal(err)
	}
	if existing.Format != domain.ChatArtifactSVG || existing.Body != "<svg/>" {
		t.Fatalf("upsert did not replace the payload: %+v", existing)
	}
	// A preview belongs to one administrator only.
	if _, err := db.GetChatArtifact(ctx, "art_a", bob); err == nil {
		t.Fatal("another administrator could read the preview")
	}

	if err := db.PruneChatArtifacts(ctx, session.ID, 2); err != nil {
		t.Fatal(err)
	}
	remaining := 0
	for _, key := range []string{"art_a", "art_b", "art_c", "art_d", "art_e"} {
		if _, err := db.GetChatArtifact(ctx, key, alice); err == nil {
			remaining++
		}
	}
	if remaining != 2 {
		t.Fatalf("artifacts kept = %d, want 2", remaining)
	}
}

func TestChatAdminRoleAndSessionLookup(t *testing.T) {
	ctx := context.Background()
	db, alice, _ := chatStoreFixture(t)

	role, err := db.AdminRole(ctx, alice)
	if err != nil || role != "admin" {
		t.Fatalf("role = %q err=%v", role, err)
	}
	if _, err := db.AdminRole(ctx, 9999); err == nil {
		t.Fatal("an unknown administrator must not resolve")
	}

	if err := db.CreateAdminSession(ctx, "sess_1", alice, "hash", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	owner, err := db.AdminSessionUser(ctx, "sess_1")
	if err != nil || owner != alice {
		t.Fatalf("session owner = %d err=%v", owner, err)
	}
	// An expired session must not resolve: that is what makes a preview ticket die with the
	// login that asked for it.
	if err := db.CreateAdminSession(ctx, "sess_old", alice, "hash", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminSessionUser(ctx, "sess_old"); err == nil {
		t.Fatal("an expired session resolved")
	}
	if err := db.DeleteAdminSession(ctx, "sess_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminSessionUser(ctx, "sess_1"); err == nil {
		t.Fatal("a logged-out session still resolved")
	}
}

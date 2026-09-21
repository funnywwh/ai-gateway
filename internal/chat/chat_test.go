package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// The chat loop is where money, permissions and conversation state meet, so it is tested
// with explicit doubles rather than through a gateway: a scripted runner says exactly what
// each step returned, and the assertions are about what the loop did with it (execute the
// call or not, what it stored, what it charged the operator's trust with).

type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]*domain.ChatSession
	messages map[string][]*domain.ChatMessage
	turns    map[string]*domain.ChatTurn // key: session|turnID
	tools    []*domain.ChatToolCall
	skills   map[int64]*domain.ChatSkill
	tokens   map[int64]*domain.MCPToken
	nextID   int64
	role     string
	seq      map[string]int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions: map[string]*domain.ChatSession{},
		messages: map[string][]*domain.ChatMessage{},
		turns:    map[string]*domain.ChatTurn{},
		skills:   map[int64]*domain.ChatSkill{},
		tokens:   map[int64]*domain.MCPToken{},
		seq:      map[string]int{},
		role:     RoleAdmin,
	}
}

// GetMCPTokenByID is the optional port the service uses to resolve a bound token. It lives
// on the store because that is where the token rows are, not on Store, so a store that does
// not implement it simply cannot validate a binding (see mcpTokenLookup).
func (f *fakeStore) GetMCPTokenByID(_ context.Context, id int64) (*domain.MCPToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, ok := f.tokens[id]
	if !ok {
		return nil, domain.ErrNotFound("MCP token")
	}
	return token, nil
}

// addToken seeds one token row for binding tests.
func (f *fakeStore) addToken(id int64, scope string) *domain.MCPToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := &domain.MCPToken{ID: id, AccountID: 7, Name: "t", Scope: scope, Status: "active"}
	f.tokens[id] = token
	return token
}

func turnKey(sessionID, turnID string) string { return sessionID + "|" + turnID }

func (f *fakeStore) CreateChatSession(ctx context.Context, s *domain.ChatSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
	return nil
}

func (f *fakeStore) GetChatSession(ctx context.Context, id string, ownerUserID int64) (*domain.ChatSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok || s.OwnerUserID != ownerUserID {
		return nil, domain.ErrNotFound("chat session " + id)
	}
	return s, nil
}

func (f *fakeStore) ListChatSessions(ctx context.Context, ownerUserID int64, limit, offset int) ([]*domain.ChatSession, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*domain.ChatSession{}
	for _, s := range f.sessions {
		if s.OwnerUserID == ownerUserID {
			out = append(out, s)
		}
	}
	return out, len(out), nil
}

func (f *fakeStore) UpdateChatSession(ctx context.Context, s *domain.ChatSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.sessions[s.ID]; !ok || existing.OwnerUserID != s.OwnerUserID {
		return domain.ErrNotFound("chat session " + s.ID)
	}
	f.sessions[s.ID] = s
	return nil
}

func (f *fakeStore) UpdateChatSessionAggregates(ctx context.Context, id string, ownerUserID int64, messages, tokensIn, tokensOut, tokensReasoning int, lastMessage time.Time, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok || s.OwnerUserID != ownerUserID {
		return domain.ErrNotFound("chat session " + id)
	}
	if title != "" && s.Title == "" {
		s.Title = title
	}
	s.MessageCount += messages
	s.TokensIn += tokensIn
	s.TokensOut += tokensOut
	s.Reasoning += tokensReasoning
	s.LastMessage = &lastMessage
	return nil
}

func (f *fakeStore) DeleteChatSession(ctx context.Context, id string, ownerUserID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok || s.OwnerUserID != ownerUserID {
		return domain.ErrNotFound("chat session " + id)
	}
	delete(f.sessions, id)
	return nil
}

func (f *fakeStore) CreateChatTurn(ctx context.Context, t *domain.ChatTurn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := turnKey(t.SessionID, t.TurnID)
	if _, exists := f.turns[key]; exists {
		return domain.ErrConflict("this turn was already submitted")
	}
	f.turns[key] = t
	return nil
}

func (f *fakeStore) GetChatTurn(ctx context.Context, sessionID, turnID string) (*domain.ChatTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.turns[turnKey(sessionID, turnID)]
	if !ok {
		return nil, domain.ErrNotFound("chat turn")
	}
	return t, nil
}

func (f *fakeStore) SetChatTurnStatus(ctx context.Context, id, status, errMsg string, requestIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.turns {
		if t.ID == id {
			t.Status = status
			t.Error = errMsg
			t.RequestIDs = requestIDs
			return nil
		}
	}
	return nil
}

func (f *fakeStore) InterruptRunningChatTurns(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, t := range f.turns {
		if t.Status == domain.ChatTurnRunning {
			t.Status = domain.ChatTurnInterrupted
			n++
		}
	}
	for _, call := range f.tools {
		if call.Status == domain.ChatToolPending {
			call.Status = domain.ChatToolUnknown
		}
	}
	return n, nil
}

func (f *fakeStore) AppendChatMessage(ctx context.Context, m *domain.ChatMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq[m.SessionID]++
	m.Seq = f.seq[m.SessionID]
	f.messages[m.SessionID] = append(f.messages[m.SessionID], m)
	return nil
}

func (f *fakeStore) ListChatMessages(ctx context.Context, sessionID string, newest int) ([]*domain.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*domain.ChatMessage{}, f.messages[sessionID]...), nil
}

func (f *fakeStore) ListChatTurnMessages(ctx context.Context, sessionID, turnID string) ([]*domain.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*domain.ChatMessage{}
	for _, m := range f.messages[sessionID] {
		if m.TurnID == turnID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateChatToolCall(ctx context.Context, c *domain.ChatToolCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.tools {
		if existing.TurnID == c.TurnID && existing.Step == c.Step && existing.CallID == c.CallID {
			return domain.ErrConflict("this tool call was already executed")
		}
	}
	f.tools = append(f.tools, c)
	return nil
}

func (f *fakeStore) FinishChatToolCall(ctx context.Context, id, result string, isError bool, status string, durationMS int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.tools {
		if call.ID == id {
			call.Result, call.IsError, call.Status, call.DurationMS = result, isError, status, durationMS
			return nil
		}
	}
	return nil
}

func (f *fakeStore) ListChatToolCalls(ctx context.Context, sessionID string, limit int) ([]*domain.ChatToolCall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*domain.ChatToolCall{}
	for _, call := range f.tools {
		if call.SessionID == sessionID {
			out = append(out, call)
		}
	}
	return out, nil
}

func (f *fakeStore) AdminRole(ctx context.Context, userID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.role == "" {
		return "", domain.ErrNotFound("admin user")
	}
	return f.role, nil
}

func (f *fakeStore) AdminSessionUser(ctx context.Context, sessionID string) (int64, error) {
	return 1, nil
}

func (f *fakeStore) CreateChatSkill(ctx context.Context, s *domain.ChatSkill) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.skills {
		if existing.OwnerUserID == s.OwnerUserID && existing.Name == s.Name {
			return 0, domain.ErrConflict("a skill named " + s.Name + " already exists")
		}
	}
	f.nextID++
	s.ID = f.nextID
	f.skills[s.ID] = s
	return s.ID, nil
}

func (f *fakeStore) GetChatSkill(ctx context.Context, id, ownerUserID int64) (*domain.ChatSkill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.skills[id]
	if !ok || s.OwnerUserID != ownerUserID {
		return nil, domain.ErrNotFound(fmt.Sprintf("chat skill %d", id))
	}
	return s, nil
}

func (f *fakeStore) ListChatSkills(ctx context.Context, ownerUserID int64, limit, offset int) ([]*domain.ChatSkill, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*domain.ChatSkill{}
	for _, s := range f.skills {
		if s.OwnerUserID == ownerUserID {
			out = append(out, s)
		}
	}
	return out, len(out), nil
}

func (f *fakeStore) UpdateChatSkill(ctx context.Context, s *domain.ChatSkill) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.skills[s.ID]
	if !ok || existing.OwnerUserID != s.OwnerUserID {
		return domain.ErrNotFound(fmt.Sprintf("chat skill %d", s.ID))
	}
	f.skills[s.ID] = s
	return nil
}

func (f *fakeStore) DeleteChatSkill(ctx context.Context, id, ownerUserID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.skills[id]
	if !ok || s.OwnerUserID != ownerUserID {
		return domain.ErrNotFound(fmt.Sprintf("chat skill %d", id))
	}
	delete(f.skills, id)
	return nil
}

// fakeRunner plays a scripted list of step results.
type fakeRunner struct {
	steps []*StepResult
	calls []Step
	err   error
	// cursor advances per step, so a test can swap the script between two turns without
	// having to know how many steps the first one consumed.
	cursor   int
	onStep   func(step Step)
	emitText [][]string
}

// script replaces the step script and rewinds, for tests that drive two independent calls.
func (f *fakeRunner) script(steps ...*StepResult) {
	f.steps = steps
	f.cursor = 0
}

func (f *fakeRunner) RunStep(ctx context.Context, step Step, emit func(StepEvent) error) (*StepResult, error) {
	index := f.cursor
	f.cursor++
	f.calls = append(f.calls, step)
	if f.onStep != nil {
		f.onStep(step)
	}
	if index < len(f.emitText) && emit != nil {
		for _, chunk := range f.emitText[index] {
			if err := emit(StepEvent{Kind: "text", Text: chunk}); err != nil {
				return nil, err
			}
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if index >= len(f.steps) {
		return &StepResult{Outcome: OutcomeCompleted, ToolCallsComplete: true}, nil
	}
	result := f.steps[index]
	// Mirror the real adapter: a function call reaches the console as a tool_call event as
	// soon as the provider announces it.
	if emit != nil {
		for _, item := range result.Items {
			if item.Type == "function_call" {
				if err := emit(StepEvent{Kind: "tool_call", CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}); err != nil {
					return nil, err
				}
			}
		}
	}
	return result, nil
}

// fakeTools records what the loop executed.
type fakeTools struct {
	mu       sync.Mutex
	listed   int
	actions  []string
	accesses []Access
	result   ToolResult
	err      error
}

func (f *fakeTools) List(access Access) []Tool {
	f.listed++
	return []Tool{{Name: "admin_request", Description: "run one management endpoint"}}
}

func (f *fakeTools) Call(ctx context.Context, access Access, name string, args map[string]any) (ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, name+":"+Stringer(args))
	f.accesses = append(f.accesses, access)
	if f.err != nil {
		return ToolResult{}, f.err
	}
	return f.result, nil
}

func Stringer(args map[string]any) string {
	raw, _ := json.Marshal(args)
	return string(raw)
}

func (f *fakeTools) executed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.actions...)
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func chatFixture(t *testing.T, cfg Config) (*Service, *fakeStore, *fakeRunner, *fakeTools, *domain.ChatSession) {
	t.Helper()
	store := newFakeStore()
	runner := &fakeRunner{}
	tools := &fakeTools{result: ToolResult{Value: map[string]any{"count": 2}}}
	service := New(cfg, store, runner, tools, nil)
	session := &domain.ChatSession{
		ID: "chat_1", OwnerUserID: 1, OwnerName: "admin", Model: "test-model",
		AccountID: 7, APIKeyID: 9, WriteMode: domain.ChatWriteModeReadOnly, Status: "active",
	}
	if err := store.CreateChatSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	return service, store, runner, tools, session
}

func textStep(text string, calls ...pluginapi.Item) *StepResult {
	items := []pluginapi.Item{}
	if text != "" {
		content, _ := json.Marshal([]map[string]string{{"type": "output_text", "text": text}})
		items = append(items, pluginapi.Item{Type: "message", Role: "assistant", Content: content})
	}
	items = append(items, calls...)
	return &StepResult{
		Items: items, Text: text, Outcome: OutcomeCompleted, ToolCallsComplete: true,
		RequestID: "req_step", Usage: Usage{InputTokens: 10, OutputTokens: 5, Metered: true},
	}
}

func callItem(id, name, args string) pluginapi.Item {
	return pluginapi.Item{Type: "function_call", CallID: id, Name: name, Arguments: args}
}

func collectEvents() (*[]Event, func(Event) error) {
	events := &[]Event{}
	return events, func(ev Event) error {
		*events = append(*events, ev)
		return nil
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestTurnRunsToolLoopAndStoresCanonicalItems(t *testing.T) {
	service, store, runner, tools, session := chatFixture(t, Config{MaxSteps: 4})
	runner.steps = []*StepResult{
		textStep("先查一下", callItem("call_1", "admin_request", `{"name":"admin_list_accounts"}`)),
		textStep("共 2 个账户"),
	}
	events, emit := collectEvents()

	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "有几个账户？",
	}, emit)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if result.Status != domain.ChatTurnCompleted {
		t.Fatalf("status = %q", result.Status)
	}
	if got := tools.executed(); len(got) != 1 || !strings.HasPrefix(got[0], "admin_request:") {
		t.Fatalf("executed = %v", got)
	}
	if runner.calls[0].PromptCacheKey != session.ID {
		t.Fatalf("prompt cache key = %q, want the session id", runner.calls[0].PromptCacheKey)
	}
	if runner.calls[0].AccountID != 7 || runner.calls[0].APIKeyID != 9 {
		t.Fatalf("billing identity not carried: %+v", runner.calls[0])
	}
	// The second step must see the tool call *and* its output, otherwise the model would
	// answer about a call it cannot see the result of.
	second := runner.calls[1]
	var sawCall, sawOutput bool
	for _, item := range second.Items {
		if item.Type == "function_call" && item.CallID == "call_1" {
			sawCall = true
		}
		if item.Type == "function_call_output" && item.CallID == "call_1" && strings.Contains(item.Output, "count") {
			sawOutput = true
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("second step items missing the call/output pair: %+v", second.Items)
	}

	// Stored message: parts in order, provider items persisted, usage summed.
	messages, _ := store.ListChatMessages(context.Background(), session.ID, 0)
	if len(messages) != 2 {
		t.Fatalf("stored messages = %d", len(messages))
	}
	assistant := messages[1]
	if len(assistant.Parts) != 3 || assistant.Parts[0].Type != "text" || assistant.Parts[1].Type != "tool_call" {
		t.Fatalf("parts = %+v", assistant.Parts)
	}
	if !strings.Contains(assistant.Content, "共 2 个账户") {
		t.Fatalf("content = %q", assistant.Content)
	}
	if assistant.TokensIn != 20 || assistant.TokensOut != 10 {
		t.Fatalf("usage = %d/%d", assistant.TokensIn, assistant.TokensOut)
	}
	if len(assistant.RequestIDs) != 2 {
		t.Fatalf("request ids = %v", assistant.RequestIDs)
	}
	if items := decodeItems(assistant.ProviderItems); len(items) == 0 {
		t.Fatal("canonical provider items were not stored")
	}
	// The session title comes from the first question, and the totals are folded in.
	if session.Title != "有几个账户？" || session.MessageCount != 2 {
		t.Fatalf("session aggregates = %q / %d", session.Title, session.MessageCount)
	}
	// The event stream carries the tool call and its result for the UI.
	var sawToolCall, sawToolResult, sawDone bool
	for _, ev := range *events {
		switch ev.Type {
		case EventToolCall:
			sawToolCall = true
		case EventToolResult:
			if ev.CallID == "" || ev.Tool == nil || ev.CallID != ev.Tool.CallID {
				t.Fatalf("tool result must identify its live UI card: %+v", ev)
			}
			if sawDone {
				t.Fatal("tool result must arrive before the turn completes")
			}
			sawToolResult = true
		case EventDone:
			sawDone = true
		}
	}
	if !sawToolCall || !sawToolResult || !sawDone {
		t.Fatalf("events missing tool/done frames: %+v", *events)
	}
}

func TestIncompleteStepDoesNotExecuteTools(t *testing.T) {
	service, store, runner, tools, session := chatFixture(t, Config{MaxSteps: 4})
	step := textStep("被截断的回答", callItem("call_1", "admin_request", `{"name":"admin_prune_requests"}`))
	step.Outcome = OutcomeIncomplete
	runner.steps = []*StepResult{step}
	events, emit := collectEvents()

	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "清理请求日志",
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := tools.executed(); len(got) != 0 {
		t.Fatalf("a truncated step must not execute tools, got %v", got)
	}
	if !result.Assistant.Truncated || result.Status != domain.ChatTurnCompleted {
		t.Fatalf("truncation not reported: status=%q truncated=%v", result.Status, result.Assistant.Truncated)
	}
	var noticed bool
	for _, ev := range *events {
		if ev.Type == EventNotice && strings.Contains(ev.Notice, "截断") {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("no notice about the truncation: %+v", *events)
	}
	messages, _ := store.ListChatMessages(context.Background(), session.ID, 0)
	if len(messages) != 2 || messages[1].Status != domain.ChatMessageOK {
		t.Fatalf("truncated answer must still be stored as ok: %+v", messages[1].Status)
	}
}

func TestIncompleteToolArgumentsAreNotExecuted(t *testing.T) {
	service, _, runner, tools, session := chatFixture(t, Config{MaxSteps: 4})
	step := textStep("让我删除它", callItem("call_1", "admin_request", `{"name":"admin_del`))
	step.ToolCallsComplete = false
	runner.steps = []*StepResult{step}
	_, emit := collectEvents()

	if _, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "删掉它",
	}, emit); err != nil {
		t.Fatal(err)
	}
	if got := tools.executed(); len(got) != 0 {
		t.Fatalf("half-written arguments must never run: %v", got)
	}
}

func TestTurnIsIdempotentAndDoesNotSpendTwice(t *testing.T) {
	service, _, runner, _, session := chatFixture(t, Config{MaxSteps: 4})
	runner.steps = []*StepResult{textStep("答案")}
	_, emit := collectEvents()
	req := TurnRequest{OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_same", Content: "问题"}

	first, err := service.Turn(context.Background(), req, emit)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Turn(context.Background(), req, emit)
	if err != nil {
		t.Fatalf("a resent turn must be answered from the record, not by paying again: %v", err)
	}
	if !second.AlreadyDone {
		t.Fatal("second submission was not recognised as already done")
	}
	if second.Assistant == nil || second.Assistant.ID != first.Assistant.ID {
		t.Fatalf("second submission returned a different answer: %+v", second.Assistant)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1 (the question must be billed once)", len(runner.calls))
	}
}

func TestViewerCannotAskOrBind(t *testing.T) {
	service, store, runner, _, session := chatFixture(t, Config{MaxSteps: 4})
	_, emit := collectEvents()
	_, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "reader", Role: RoleViewer,
		SessionID: session.ID, TurnID: "turn_1", Content: "问题",
	}, emit)
	if !isForbidden(err) {
		t.Fatalf("viewer turn error = %v, want 403", err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("a refused turn must not reach the model")
	}
	if _, err := service.CreateSession(context.Background(), 1, "reader", RoleViewer, SessionInput{
		Model: "m", AccountID: 1, APIKeyID: 1,
	}); err == nil {
		t.Fatal("a viewer must not be able to bind a billing key")
	}
	// Rebinding is an administrator act too: a viewer must not be able to point the
	// conversation at another token, not even a harmless one.
	store.addToken(5, "admin_read")
	if _, err := service.UpdateSession(context.Background(), 1, RoleViewer, session.ID, SessionInput{
		MCPTokenID: 5,
	}); err == nil {
		t.Fatal("a viewer must not be able to rebind the conversation's MCP token")
	}
}

// Binding is what decides a conversation's authority, so an unusable token is refused at
// binding time rather than accepted and discovered on the first question.
func TestSessionBindingRefusesUnusableTokens(t *testing.T) {
	service, store, _, _, _ := chatFixture(t, Config{MaxSteps: 4})
	ctx := context.Background()

	// No token at all.
	if _, err := service.CreateSession(ctx, 1, "admin", RoleAdmin, SessionInput{
		Model: "m", AccountID: 1, APIKeyID: 1,
	}); err == nil {
		t.Fatal("a conversation without an MCP token was created")
	}
	// A token that does not exist.
	if _, err := service.CreateSession(ctx, 1, "admin", RoleAdmin, SessionInput{
		Model: "m", AccountID: 1, APIKeyID: 1, MCPTokenID: 404,
	}); err == nil {
		t.Fatal("a conversation bound to a missing token was created")
	}
	// A revoked one.
	revoked := store.addToken(6, "admin")
	revoked.Status = "revoked"
	if _, err := service.CreateSession(ctx, 1, "admin", RoleAdmin, SessionInput{
		Model: "m", AccountID: 1, APIKeyID: 1, MCPTokenID: 6,
	}); err == nil {
		t.Fatal("a conversation bound to a revoked token was created")
	}

	// A usable admin-scope token is accepted, and the write mode is derived from its scope
	// rather than supplied by the caller.
	store.addToken(7, "admin")
	created, err := service.CreateSession(ctx, 1, "admin", RoleAdmin, SessionInput{
		Model: "m", AccountID: 1, APIKeyID: 1, MCPTokenID: 7,
	})
	if err != nil {
		t.Fatalf("binding an active admin token failed: %v", err)
	}
	if created.MCPTokenID == nil || *created.MCPTokenID != 7 {
		t.Fatalf("session token = %v, want 7", created.MCPTokenID)
	}
	if created.WriteMode != domain.ChatWriteModeAllow {
		t.Fatalf("write mode = %q, want %q", created.WriteMode, domain.ChatWriteModeAllow)
	}

	// A read-scope token yields a read-only conversation.
	store.addToken(8, "query")
	readOnly, err := service.CreateSession(ctx, 1, "admin", RoleAdmin, SessionInput{
		Model: "m", AccountID: 1, APIKeyID: 1, MCPTokenID: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.WriteMode != domain.ChatWriteModeReadOnly {
		t.Fatalf("write mode = %q, want %q", readOnly.WriteMode, domain.ChatWriteModeReadOnly)
	}
}

func TestRoleIsRecheckedBeforeEveryToolCall(t *testing.T) {
	service, store, runner, tools, session := chatFixture(t, Config{MaxSteps: 4})
	runner.steps = []*StepResult{
		textStep("改一下", callItem("call_1", "admin_request", `{"name":"admin_update_model"}`)),
		textStep("好了"),
	}
	// The administrator is demoted while the answer is being produced. The turn itself was
	// authorized, but the tool call must carry the *current* role: the tools adapter turns
	// that into the read-only scope, which is what refuses the write.
	store.role = RoleViewer
	_, emit := collectEvents()

	if _, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "把模型停掉",
	}, emit); err != nil {
		t.Fatal(err)
	}
	if len(tools.accesses) != 1 {
		t.Fatalf("tool calls = %d", len(tools.accesses))
	}
	if tools.accesses[0].Role != RoleViewer {
		t.Fatalf("tool call carried role %q, want the freshly read %q", tools.accesses[0].Role, RoleViewer)
	}
}

func TestToolFailureIsReportedToTheModelNotFatal(t *testing.T) {
	service, _, runner, tools, session := chatFixture(t, Config{MaxSteps: 4})
	tools.err = errors.New("admin_create_key cannot be called from the console chat")
	runner.steps = []*StepResult{
		textStep("尝试创建", callItem("call_1", "admin_request", `{"name":"admin_create_key"}`)),
		textStep("需要你到控制台操作"),
	}
	_, emit := collectEvents()

	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "给我建个 Key",
	}, emit)
	if err != nil {
		t.Fatalf("a refused tool call must not fail the turn: %v", err)
	}
	if result.Status != domain.ChatTurnCompleted {
		t.Fatalf("status = %q", result.Status)
	}
	assistant := result.Assistant
	var failed bool
	for _, part := range assistant.Parts {
		if part.Type == "tool_call" && part.IsError && strings.Contains(part.Result, "console chat") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("the refusal was not recorded as a tool error: %+v", assistant.Parts)
	}
	// And the next step is told what happened, so the model can explain it to the operator.
	var told bool
	for _, item := range runner.calls[1].Items {
		if item.Type == "function_call_output" && strings.Contains(item.Output, "console chat") {
			told = true
		}
	}
	if !told {
		t.Fatalf("the refusal was not fed back to the model: %+v", runner.calls[1].Items)
	}
}

func TestToolResultTruncationSaysSo(t *testing.T) {
	cfg := Config{MaxSteps: 4, MaxToolResultBytes: 64}
	service, _, runner, tools, session := chatFixture(t, cfg)
	tools.result = ToolResult{Value: map[string]any{"blob": strings.Repeat("x", 400)}}
	runner.steps = []*StepResult{
		textStep("查一下", callItem("call_1", "admin_request", `{"name":"admin_list_requests"}`)),
		textStep("好了"),
	}
	_, emit := collectEvents()

	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "最近的请求",
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	var truncated bool
	for _, part := range result.Assistant.Parts {
		if part.Type == "tool_call" {
			var payload map[string]any
			if err := json.Unmarshal([]byte(part.Result), &payload); err != nil {
				t.Fatalf("a truncated result must stay valid JSON: %v (%q)", err, part.Result)
			}
			if payload["truncated"] == true {
				truncated = true
			}
		}
	}
	if !truncated {
		t.Fatal("a truncated tool result must say so inside a valid envelope")
	}
}

func TestNoStepLimitKeepsGoing(t *testing.T) {
	// The default configuration sets no ceiling, so a model that keeps asking for tools
	// must be allowed to finish: the loop only ends when it stops calling them.
	service, _, runner, tools, session := chatFixture(t, Config{})
	steps := make([]*StepResult, 0, 12)
	for i := 0; i < 11; i++ {
		steps = append(steps, textStep("继续", callItem(
			fmt.Sprintf("call_%d", i), "admin_request", `{"name":"admin_list_accounts"}`)))
	}
	steps = append(steps, textStep("查完了"))
	runner.script(steps...)
	events, emit := collectEvents()

	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "一直查",
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 12 {
		t.Fatalf("steps = %d, want all 12", len(runner.calls))
	}
	if len(tools.executed()) != 11 {
		t.Fatalf("tool calls = %d, want all 11", len(tools.executed()))
	}
	if result.Status != domain.ChatTurnCompleted || !strings.Contains(result.Assistant.Content, "查完了") {
		t.Fatalf("turn did not finish: %+v", result.Assistant)
	}
	for _, ev := range *events {
		if ev.Type == EventNotice && strings.Contains(ev.Notice, "上限") {
			t.Fatalf("a limit was reported although none is configured: %q", ev.Notice)
		}
	}
}

func TestStepLimitStopsTheLoop(t *testing.T) {
	service, _, runner, _, session := chatFixture(t, Config{MaxSteps: 2, MaxToolCalls: 8})
	runner.steps = []*StepResult{
		textStep("继续", callItem("call_1", "admin_request", `{"name":"admin_list_accounts"}`)),
		textStep("继续", callItem("call_2", "admin_request", `{"name":"admin_list_keys"}`)),
	}
	events, emit := collectEvents()
	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "一直查",
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("steps = %d, want the configured limit of 2", len(runner.calls))
	}
	if result.Status != domain.ChatTurnCompleted {
		t.Fatalf("status = %q", result.Status)
	}
	var limited bool
	for _, ev := range *events {
		if ev.Type == EventNotice && strings.Contains(ev.Notice, "步数上限") {
			limited = true
		}
	}
	if !limited {
		t.Fatalf("no notice about the step limit: %+v", *events)
	}
}

func TestToolCallBudgetStopsExecution(t *testing.T) {
	service, _, runner, tools, session := chatFixture(t, Config{MaxSteps: 4, MaxToolCalls: 1})
	runner.steps = []*StepResult{
		textStep("查两个",
			callItem("call_1", "admin_request", `{"name":"admin_list_accounts"}`),
			callItem("call_2", "admin_request", `{"name":"admin_list_keys"}`)),
		textStep("结束"),
	}
	_, emit := collectEvents()
	if _, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "查两个",
	}, emit); err != nil {
		t.Fatal(err)
	}
	if got := tools.executed(); len(got) != 0 {
		t.Fatalf("a batch larger than the remaining budget must not run partially: %v", got)
	}
}

func TestCancelledTurnStoresWhatItGot(t *testing.T) {
	service, store, runner, _, session := chatFixture(t, Config{MaxSteps: 4})
	runner.onStep = func(step Step) {}
	ctx, cancel := context.WithCancel(context.Background())
	runner.steps = []*StepResult{textStep("半句话")}
	// The runner cancels the turn as if the client had gone away mid-answer.
	runner.onStep = func(step Step) { cancel() }
	_, emit := collectEvents()

	result, err := service.Turn(ctx, TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "问题",
	}, emit)
	if err != nil {
		t.Fatalf("a cancelled turn must still be recorded: %v", err)
	}
	messages, _ := store.ListChatMessages(context.Background(), session.ID, 0)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the question and the partial answer", len(messages))
	}
	if messages[1].Status != domain.ChatMessageAborted {
		t.Fatalf("partial answer status = %q", messages[1].Status)
	}
	if result == nil || result.Turn == nil || result.Turn.Status != domain.ChatTurnAborted {
		t.Fatalf("turn = %+v", result)
	}
}

func TestHistoryIsCutAtWholeTurnsAndKeepsTheNewest(t *testing.T) {
	service, store, runner, _, session := chatFixture(t, Config{MaxSteps: 2, MaxHistoryMessages: 2, MaxHistoryBytes: 1 << 20})
	// Two older turns of two messages each, then the new question.
	for i := 1; i <= 4; i++ {
		if err := store.AppendChatMessage(context.Background(), &domain.ChatMessage{
			ID: fmt.Sprintf("cmsg_old_%d", i), SessionID: session.ID, TurnID: fmt.Sprintf("turn_old_%d", (i+1)/2),
			Role:    map[bool]string{true: domain.ChatRoleUser, false: domain.ChatRoleAssistant}[i%2 == 1],
			Content: fmt.Sprintf("旧消息 %d", i), Status: domain.ChatMessageOK,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runner.steps = []*StepResult{textStep("新回答")}
	_, emit := collectEvents()
	if _, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_new", Content: "新问题",
	}, emit); err != nil {
		t.Fatal(err)
	}
	items := runner.calls[0].Items
	// MaxHistoryMessages=2 keeps exactly the newest turn (the question asked just now).
	if len(items) != 1 {
		t.Fatalf("replayed items = %d, want only the newest question: %+v", len(items), items)
	}
	if !strings.Contains(string(items[0].Content), "新问题") {
		t.Fatalf("newest question missing from the replay: %s", items[0].Content)
	}
}

func TestHistoryTooLargeRefusesInsteadOfTruncatingTheQuestion(t *testing.T) {
	service, store, runner, _, session := chatFixture(t, Config{MaxSteps: 2, MaxHistoryMessages: 2, MaxHistoryBytes: 16})
	if err := store.AppendChatMessage(context.Background(), &domain.ChatMessage{
		ID: "cmsg_big", SessionID: session.ID, TurnID: "turn_old", Role: domain.ChatRoleUser,
		Content: strings.Repeat("很长的历史", 20), Status: domain.ChatMessageOK,
	}); err != nil {
		t.Fatal(err)
	}
	_, emit := collectEvents()
	result, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: strings.Repeat("长问题", 10),
	}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("a conversation that does not fit must not be sent half-way to a model")
	}
	if result.Assistant.Status != domain.ChatMessageFailed || !strings.Contains(result.Assistant.Error, "上下文上限") {
		t.Fatalf("failure not explained: %+v", result.Assistant)
	}
}

func TestSkillsAreInjectedIntoTheInstructions(t *testing.T) {
	service, store, runner, _, session := chatFixture(t, Config{MaxSteps: 2})
	skill := &domain.ChatSkill{OwnerUserID: 1, Name: "成本排查", Description: "按账户核对", Instructions: "先查账户再查用量。"}
	if _, err := store.CreateChatSkill(context.Background(), skill); err != nil {
		t.Fatal(err)
	}
	session.SkillIDs = []int64{skill.ID}
	runner.steps = []*StepResult{textStep("好的")}
	_, emit := collectEvents()
	if _, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "帮我看看成本",
	}, emit); err != nil {
		t.Fatal(err)
	}
	instructions := runner.calls[0].Instructions
	if !strings.Contains(instructions, "成本排查") || !strings.Contains(instructions, "先查账户再查用量") {
		t.Fatalf("skill was not injected: %q", instructions)
	}
	if !strings.Contains(instructions, "admin_endpoints") {
		t.Fatal("the built-in instructions about the management tools are missing")
	}
	if !strings.Contains(instructions, fmt.Sprintf("最多 %d 条序列", MaxChartSeries)) {
		t.Fatalf("the chart bounds are not stated in the prompt: %q", instructions)
	}

	// A skill deleted after it was attached simply stops being loaded.
	store.skills = map[int64]*domain.ChatSkill{}
	runner.steps = []*StepResult{textStep("好的")}
	if _, err := service.Turn(context.Background(), TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_2", Content: "再问一次",
	}, emit); err != nil {
		t.Fatalf("a dangling skill id must not break the turn: %v", err)
	}
	if strings.Contains(runner.calls[1].Instructions, "成本排查") {
		t.Fatal("a deleted skill was still injected")
	}
}

// TestEmptyQuestionRunsLoadedSkills covers the send an empty input box produces once skills are
// loaded: the skills carry the instructions, so the turn is real and must be stored as something
// a person can read back. It is refused exactly when there is nothing to run — no skills at all,
// or an id whose skill was deleted from the library (the id stays in the session).
func TestEmptyQuestionRunsLoadedSkills(t *testing.T) {
	service, store, runner, _, session := chatFixture(t, Config{MaxSteps: 2})
	ctx := context.Background()
	_, emit := collectEvents()

	empty := TurnRequest{OwnerID: 1, Username: "admin", Role: RoleAdmin, SessionID: session.ID}
	empty.TurnID, empty.Content = "turn_empty", "   "
	if _, err := service.Turn(ctx, empty, emit); err == nil {
		t.Fatal("an empty question with no skills loaded has nothing to ask: it must be refused")
	}

	skill := &domain.ChatSkill{OwnerUserID: 1, Name: "成本排查", Instructions: "先查账户，再查用量。"}
	if _, err := store.CreateChatSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}
	session.SkillIDs = []int64{skill.ID}
	runner.steps = []*StepResult{textStep("开始核对")}
	result, err := service.Turn(ctx, TurnRequest{
		OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_1", Content: "  ",
	}, emit)
	if err != nil {
		t.Fatalf("an empty question with a loaded skill must run: %v", err)
	}
	if result.User.Content != DefaultSkillRunText {
		t.Fatalf("stored question = %q, want the readable default %q", result.User.Content, DefaultSkillRunText)
	}
	if len(result.User.Parts) != 1 || result.User.Parts[0].Text != DefaultSkillRunText {
		t.Fatalf("the stored user message must carry the same text as its parts: %+v", result.User.Parts)
	}
	// The skill body belongs in the instructions, not in the question: repeating it as if the
	// operator had typed it would deliver the same instructions twice.
	if strings.Contains(result.User.Content, "先查账户，再查用量") {
		t.Fatalf("the question must not repeat the skill body: %q", result.User.Content)
	}
	if !strings.Contains(runner.calls[0].Instructions, "先查账户，再查用量") {
		t.Fatal("the skill still has to reach the model, through the system prompt")
	}

	// The dangling id: the session still lists it, the library no longer holds it.
	store.skills = map[int64]*domain.ChatSkill{}
	runner.steps = []*StepResult{textStep("好的")}
	gone := TurnRequest{OwnerID: 1, Username: "admin", Role: RoleAdmin,
		SessionID: session.ID, TurnID: "turn_2", Content: ""}
	if _, err := service.Turn(ctx, gone, emit); err == nil {
		t.Fatal("an empty question whose only skill was deleted must be refused, not sent as an empty turn")
	}
}

func TestSkillLibraryIsPrivateAndValidated(t *testing.T) {
	service, _, _, _, _ := chatFixture(t, Config{})
	ctx := context.Background()
	skill, err := service.CreateSkill(ctx, 1, SkillInput{Name: "我的技能", Instructions: "步骤一"}, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateSkill(ctx, 1, SkillInput{Name: "我的技能", Instructions: "步骤二"}, "", "m"); err == nil {
		t.Fatal("duplicate names must be refused")
	}
	if _, err := service.store.GetChatSkill(ctx, skill.ID, 2); err == nil {
		t.Fatal("another owner must not be able to read the skill")
	}
	if _, err := service.CreateSkill(ctx, 1, SkillInput{Name: "空的", Instructions: "  "}, "", "m"); err == nil {
		t.Fatal("a skill without instructions must be refused")
	}
	long := strings.Repeat("x", service.Config().MaxSkillBytes+1)
	if _, err := service.CreateSkill(ctx, 1, SkillInput{Name: "太长", Instructions: long}, "", "m"); err == nil {
		t.Fatal("instructions over the limit must be refused")
	}
	if err := service.DeleteSkill(ctx, 2, skill.ID); err == nil {
		t.Fatal("another owner must not be able to delete the skill")
	}
}

func TestSkillDraftFallsBackHonestly(t *testing.T) {
	service, _, runner, _, session := chatFixture(t, Config{MaxSteps: 2})
	// The model answers with prose, not JSON: the draft must say so instead of pretending
	// the model wrote a skill.
	runner.script(textStep("这段对话讲了如何查成本。"))
	store := service.store.(*fakeStore)
	if err := store.AppendChatMessage(context.Background(), &domain.ChatMessage{
		ID: "cmsg_u", SessionID: session.ID, TurnID: "turn_1", Role: domain.ChatRoleUser,
		Content: "帮我算这个月的成本", Status: domain.ChatMessageOK,
	}); err != nil {
		t.Fatal(err)
	}
	draft, err := service.SkillDraft(context.Background(), 1, "admin", RoleAdmin, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Note == "" || !strings.Contains(draft.Note, "骨架") {
		t.Fatalf("a fallback draft must explain itself: %+v", draft)
	}
	if !strings.Contains(draft.Instructions, "适用场景") {
		t.Fatalf("fallback instructions = %q", draft.Instructions)
	}
	if draft.Name == "" {
		t.Fatal("the fallback draft needs a name")
	}

	// A JSON answer is parsed, fences and prose included.
	runner.script(textStep("好的：\n```json\n{\"name\":\"成本核对\",\"description\":\"每月核对\",\"instructions\":\"先查账户\"}\n```\n"))
	draft, err = service.SkillDraft(context.Background(), 1, "admin", RoleAdmin, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Name != "成本核对" || draft.Instructions != "先查账户" || draft.Note != "" {
		t.Fatalf("parsed draft = %+v", draft)
	}
	// Distilling is a billed step like any other, and it offers no tools.
	last := runner.calls[len(runner.calls)-1]
	if len(last.Tools) != 0 {
		t.Fatalf("distilling must not offer tools: %+v", last.Tools)
	}
	if last.APIKeyID != session.APIKeyID || last.AccountID != session.AccountID {
		t.Fatalf("distilling must bill the same key: %+v", last)
	}
}

func TestExtractJSONObjectHandlesNestingAndStrings(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"a\":1}\n```":     `{"a":1}`,
		"前言 {\"a\":{\"b\":\"}\"}} 后记": `{"a":{"b":"}"}}`,
		"没有对象":                        "",
		"{\"unclosed\": ":             "",
	}
	for input, want := range cases {
		if got := extractJSONObject(input); got != want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRecoverInterruptedMarksTurnsAndToolCalls(t *testing.T) {
	service, store, _, _, session := chatFixture(t, Config{})
	ctx := context.Background()
	if err := store.CreateChatTurn(ctx, &domain.ChatTurn{ID: "turn_row", SessionID: session.ID, TurnID: "turn_1", Status: domain.ChatTurnRunning}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateChatToolCall(ctx, &domain.ChatToolCall{
		ID: "tcall_1", SessionID: session.ID, TurnID: "turn_1", Step: 1, CallID: "call_1",
		Name: "admin_request", Status: domain.ChatToolPending,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	turn, _ := store.GetChatTurn(ctx, session.ID, "turn_1")
	if turn.Status != domain.ChatTurnInterrupted {
		t.Fatalf("turn status = %q", turn.Status)
	}
	calls, _ := store.ListChatToolCalls(ctx, session.ID, 10)
	if len(calls) != 1 || calls[0].Status != domain.ChatToolUnknown {
		t.Fatalf("a pending call must be reported as unknown, not silently pending: %+v", calls[0])
	}
}

func TestSessionEditRequiresAdministratorForBinding(t *testing.T) {
	service, _, _, _, session := chatFixture(t, Config{})
	ctx := context.Background()
	// Renaming is the owner's business, even for a viewer.
	title := "新标题"
	updated, err := service.UpdateSession(ctx, 1, RoleViewer, session.ID, SessionInput{Title: &title})
	if err != nil {
		t.Fatalf("a viewer must be able to rename their own conversation: %v", err)
	}
	if updated.Title != "新标题" {
		t.Fatalf("title = %q", updated.Title)
	}
	// Changing what the conversation may spend is not.
	if _, err := service.UpdateSession(ctx, 1, RoleViewer, session.ID, SessionInput{APIKeyID: 42}); err == nil {
		t.Fatal("a viewer must not be able to rebind the billing key")
	}
	if _, err := service.UpdateSession(ctx, 1, RoleAdmin, session.ID, SessionInput{APIKeyID: 42}); err != nil {
		t.Fatalf("an administrator must be able to rebind: %v", err)
	}
}

func TestDeleteSessionRefusesWhileATurnRuns(t *testing.T) {
	service, _, _, _, session := chatFixture(t, Config{})
	service.setBusy(session.ID, true)
	defer service.setBusy(session.ID, false)
	if err := service.DeleteSession(context.Background(), 1, session.ID); err == nil {
		t.Fatal("a conversation must not be deleted out from under a running turn")
	}
}

func isForbidden(err error) bool {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 403
	}
	return false
}

// TestSessionWebAccessIsOffByDefaultAndGatedByTheDeployment pins M73's two-switch rule at the
// service boundary, which is where a flag that silently does nothing would be introduced.
func TestSessionWebAccessIsOffByDefaultAndGatedByTheDeployment(t *testing.T) {
	ctx := context.Background()
	yes := true

	// Deployment off: the flag is refused rather than stored, on create and on update, because a
	// conversation that claims a capability this gateway does not have would mislead both the
	// console and the model.
	service, store, _, _, session := chatFixture(t, Config{})
	if _, err := service.UpdateSession(ctx, 1, RoleAdmin, session.ID, SessionInput{WebAccess: &yes}); err == nil {
		t.Fatal("switching web access on must be refused while the deployment has it off")
	}
	if _, err := store.GetChatSession(ctx, session.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Nil means unchanged: an unrelated update keeps the stored value.
	stored, err := service.UpdateSession(ctx, 1, RoleAdmin, session.ID, SessionInput{Title: strPtr("新标题")})
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebAccess {
		t.Error("web access must default to off")
	}

	// Deployment on: the session decides, and the value round-trips through the store.
	on, store, _, _, session := chatFixture(t, Config{WebAccess: true})
	enabled, err := on.UpdateSession(ctx, 1, RoleAdmin, session.ID, SessionInput{WebAccess: &yes})
	if err != nil {
		t.Fatalf("enabling web access: %v", err)
	}
	if !enabled.WebAccess {
		t.Fatal("the switch must be stored")
	}
	reloaded, err := store.GetChatSession(ctx, session.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.WebAccess {
		t.Fatal("the switch must survive a reload")
	}
	no := false
	disabled, err := on.UpdateSession(ctx, 1, RoleAdmin, session.ID, SessionInput{WebAccess: &no})
	if err != nil {
		t.Fatalf("disabling web access: %v", err)
	}
	if disabled.WebAccess {
		t.Fatal("the switch must be able to go back off")
	}
}

// TestAccessFoldsBothWebSwitches pins what the tool surface and the prompt are handed: web
// access is on only when the deployment and the session both say so.
func TestAccessFoldsBothWebSwitches(t *testing.T) {
	service, _, _, _, session := chatFixture(t, Config{WebAccess: true})
	for _, tc := range []struct {
		name          string
		deploymentOn  bool
		sessionAccess bool
		want          bool
	}{
		{"both on", true, true, true},
		{"deployment off", false, true, false},
		{"session off", true, false, false},
		{"both off", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service.cfg.WebAccess = tc.deploymentOn
			session.WebAccess = tc.sessionAccess
			access := service.accessFor(session, "admin", RoleAdmin, "turn_1")
			if access.WebAccess != tc.want {
				t.Errorf("WebAccess = %v, want %v", access.WebAccess, tc.want)
			}
			if access.TurnID != "turn_1" {
				t.Errorf("TurnID = %q: the per-turn budget needs it", access.TurnID)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

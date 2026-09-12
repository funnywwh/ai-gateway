package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
)

// This file is the console chat's HTTP surface: conversations, questions, answers, skills
// and the model picker. Every route here is NoTool in the route table — a conversation
// belongs to the administrator who is logged in, and an MCP token has no administrator
// account to own one. They also refuse the synthetic MCP actor for the same reason.

// ChatService is the chat surface the handlers call. *chat.Service implements it; tests
// substitute a fake so the HTTP contracts can be exercised without a model.
type ChatService interface {
	Config() chat.Config
	ListSessions(ctx context.Context, ownerID int64, limit, offset int) ([]*domain.ChatSession, int, error)
	CreateSession(ctx context.Context, ownerID int64, username, role string, in chat.SessionInput) (*domain.ChatSession, error)
	UpdateSession(ctx context.Context, ownerID int64, role, id string, in chat.SessionInput) (*domain.ChatSession, error)
	Session(ctx context.Context, ownerID int64, id string, usage chat.UsageRecordReader) (*chat.SessionDetail, error)
	DeleteSession(ctx context.Context, ownerID int64, id string) error
	Turn(ctx context.Context, req chat.TurnRequest, emit func(chat.Event) error) (*chat.TurnResult, error)
	ListSkills(ctx context.Context, ownerID int64, limit, offset int) ([]*domain.ChatSkill, int, error)
	CreateSkill(ctx context.Context, ownerID int64, in chat.SkillInput, sourceSessionID, sourceModel string) (*domain.ChatSkill, error)
	UpdateSkill(ctx context.Context, ownerID, id int64, in chat.SkillInput) (*domain.ChatSkill, error)
	DeleteSkill(ctx context.Context, ownerID, id int64) error
	SkillDraft(ctx context.Context, ownerID int64, username, role, sessionID string) (*chat.SkillDraft, error)
}

// newChatService builds the chat service from the injected store unless a caller supplied
// one, so tests can drive the handlers without a gateway behind them.
func (s *Server) newChatService() ChatService {
	if s.deps.Chat != nil {
		return s.deps.Chat
	}
	if s.deps.ChatStore == nil {
		return nil
	}
	cfg := chatConfig(s.deps.Config)
	if !cfg.Enabled {
		return nil
	}
	return chat.New(cfg, s.deps.ChatStore, &chatRunner{s: s},
		&chatTools{s: s, token: s.deps.MCPTokens, log: s.deps.Log}, s.deps.Log)
}

// chatConfig maps the gateway configuration onto the chat service's.
func chatConfig(cfg *config.Config) chat.Config {
	if cfg == nil {
		return chat.Config{}
	}
	return chat.Config{
		Enabled:            cfg.Chat.Enabled,
		MaxSteps:           cfg.Chat.MaxSteps,
		MaxToolCalls:       cfg.Chat.MaxToolCalls,
		MaxToolResultBytes: cfg.Chat.MaxToolResultBytes,
		MaxHistoryMessages: cfg.Chat.MaxHistoryMessages,
		MaxHistoryBytes:    cfg.Chat.MaxHistoryBytes,
		MaxLoadedSkills:    cfg.Chat.MaxLoadedSkills,
		MaxSkillBytes:      cfg.Chat.MaxSkillBytes,
		RecordReasoning:    cfg.Chat.RecordReasoning,
		MaxOutputTokens:    cfg.Chat.MaxOutputTokens,
		SystemPrompt:       cfg.Chat.SystemPrompt,
		// The instructions about interactive previews describe a capability the deployment can
		// switch off; when it is off, the page a form would submit from has no way to talk
		// back, so the model is not told to build one.
		UIBridge: cfg.Chat.UIBridgeEnabled,
	}
}

// RecoverChatTurns marks turns left running by a previous process. It is called once at
// startup, before the listener accepts traffic.
func (s *Server) RecoverChatTurns(ctx context.Context) (int64, error) {
	service, ok := s.chat.(*chat.Service)
	if !ok || service == nil {
		return 0, nil
	}
	return service.RecoverInterrupted(ctx)
}

// chatActor resolves the logged-in administrator for a chat route. Unlike adminActor it
// rejects the synthetic MCP actor: a conversation and a skill library are owned by an
// admin_users row, and a token has none.
func (s *Server) chatActor(w http.ResponseWriter, r *http.Request, requireAdmin bool) (*domain.AdminUser, bool) {
	if _, isMCP := mcpActorFrom(r.Context()); isMCP {
		writeAPIError(w, domain.ErrForbidden(
			"console chat is not available over MCP: conversations and skills belong to a logged-in administrator account"))
		return nil, false
	}
	user, ok := s.adminActor(w, r, requireAdmin)
	if !ok {
		return nil, false
	}
	if user.ID <= 0 {
		writeAPIError(w, domain.ErrUnauthorized("missing admin session"))
		return nil, false
	}
	return user, true
}

// chatReady answers the request when the feature is switched off.
func (s *Server) chatReady(w http.ResponseWriter) bool {
	if s.chat == nil {
		writeAPIError(w, domain.ErrUnsupported("the console chat is disabled on this deployment (chat.enabled)"))
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

type chatSessionRequest struct {
	Title     *string `json:"title"`
	Model     string  `json:"model"`
	AccountID int64   `json:"account_id"`
	APIKeyID  int64   `json:"api_key_id"`
	// MCPTokenID is the token this conversation acts as; it decides the tool surface and
	// which endpoints may be written. write_mode is no longer accepted — it is derived from
	// this token's scope, so a client cannot claim an authority the token does not carry.
	MCPTokenID int64   `json:"mcp_token_id"`
	SkillIDs   []int64 `json:"skill_ids"`
}

func (s *Server) handleAdminChatListSessions(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	page, ok := chatPage(r)
	if !ok {
		writeAPIError(w, domain.ErrInvalidRequest("limit and offset must be non-negative integers"))
		return
	}
	sessions, total, err := s.chat.ListSessions(r.Context(), user.ID, page.Limit, page.Offset)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	rows := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		rows = append(rows, chatSessionJSON(session, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": rows, "total": total, "limit": page.Limit, "offset": page.Offset,
	})
}

func (s *Server) handleAdminChatCreateSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, true)
	if !ok || !s.chatReady(w) {
		return
	}
	var body chatSessionRequest
	if !decodeChatBody(w, r, &body) {
		return
	}
	session, err := s.chat.CreateSession(r.Context(), user.ID, user.Username, user.Role, chat.SessionInput{
		Title: body.Title, Model: body.Model, AccountID: body.AccountID,
		APIKeyID: body.APIKeyID, MCPTokenID: body.MCPTokenID, SkillIDs: body.SkillIDs,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), user.Username, "chat.session_create", "chat_session", session.ID,
		map[string]any{"model": session.Model, "account_id": session.AccountID,
			"mcp_token_id": session.MCPTokenID, "write_mode": session.WriteMode}, "ok")
	writeJSON(w, http.StatusCreated, chatSessionJSON(session, true))
}

func (s *Server) handleAdminChatGetSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	detail, err := s.chat.Session(r.Context(), user.ID, r.PathValue("id"), s.chatUsageReader())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	payload := chatSessionJSON(detail.Session, true)
	payload["messages"] = chatMessagesJSON(detail.Messages, detail.Usage)
	payload["tool_calls"] = chatToolCallsJSON(detail.ToolCalls)
	payload["skills"] = chatSkillsJSON(detail.Skills)
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAdminChatUpdateSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, true)
	if !ok || !s.chatReady(w) {
		return
	}
	var body chatSessionRequest
	if !decodeChatBody(w, r, &body) {
		return
	}
	session, err := s.chat.UpdateSession(r.Context(), user.ID, user.Role, r.PathValue("id"), chat.SessionInput{
		Title: body.Title, Model: body.Model, AccountID: body.AccountID,
		APIKeyID: body.APIKeyID, MCPTokenID: body.MCPTokenID, SkillIDs: body.SkillIDs,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), user.Username, "chat.session_update", "chat_session", session.ID,
		map[string]any{"model": session.Model, "account_id": session.AccountID,
			"mcp_token_id": session.MCPTokenID, "write_mode": session.WriteMode}, "ok")
	writeJSON(w, http.StatusOK, chatSessionJSON(session, true))
}

func (s *Server) handleAdminChatDeleteSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.chat.DeleteSession(r.Context(), user.ID, id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), user.Username, "chat.session_delete", "chat_session", id, nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// ---------------------------------------------------------------------------
// models available to one billing key
// ---------------------------------------------------------------------------

func (s *Server) handleAdminChatModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.chatActor(w, r, true); !ok || !s.chatReady(w) {
		return
	}
	if s.deps.Verifier == nil || s.deps.Registry == nil || s.deps.Router == nil {
		writeAPIError(w, domain.ErrUnsupported("the data plane is disabled on this deployment"))
		return
	}
	keyID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("api_key_id")), 10, 64)
	if err != nil || keyID <= 0 {
		writeAPIError(w, domain.ErrInvalidRequest("api_key_id must be a positive integer"))
		return
	}
	accountID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("account_id")), 10, 64)
	if err != nil || accountID <= 0 {
		writeAPIError(w, domain.ErrInvalidRequest("account_id must be a positive integer"))
		return
	}
	key, account, err := s.deps.Verifier.VerifyID(r.Context(), keyID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if account.ID != accountID {
		writeAPIError(w, domain.ErrInvalidRequest("that API key does not belong to the selected account"))
		return
	}
	snap := s.deps.Registry.Snapshot()
	grant := s.deps.Router.Authorize(key, s.deps.Router.ResolveTags(snap, key))
	models := make([]map[string]any, 0, len(snap.Models))
	for _, model := range snap.Models {
		if !model.Enabled || !granted(grant.Models, model.PublicName) {
			continue
		}
		cands, err := s.deps.Router.Candidates(domain.RouteRequest{Model: model.PublicName, Key: key, Grant: grant})
		if err != nil || len(cands) == 0 {
			continue
		}
		models = append(models, map[string]any{
			"id": model.PublicName, "has_tools": modelSupportsTools(cands),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models, "account": account.Name})
}

// modelSupportsTools reports whether any candidate can honour a tool-calling request. The
// chat tells the operator when a model cannot, instead of silently answering without tools.
func modelSupportsTools(cands []domain.Candidate) bool {
	for _, cand := range cands {
		degradedTools := false
		for _, feature := range cand.Degraded {
			if feature == "tools" {
				degradedTools = true
			}
		}
		if !degradedTools {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// one question
// ---------------------------------------------------------------------------

type chatTurnRequest struct {
	TurnID  string `json:"turn_id"`
	Content string `json:"content"`
}

// handleAdminChatTurn streams one answer as SSE. The stream carries chat events rather than
// raw Responses events, because the console needs turn/step/tool framing that the Responses
// wire format does not have.
func (s *Server) handleAdminChatTurn(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, true)
	if !ok || !s.chatReady(w) {
		return
	}
	var body chatTurnRequest
	if !decodeChatBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.TurnID) == "" {
		body.TurnID = ids.ChatTurn()
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, domain.ErrInternal("streaming is not supported by this connection"))
		return
	}

	stream := &chatSSE{w: w, flusher: flusher}
	// The turn's own work is bounded by the request context: closing the tab stops the
	// model. Nothing is lost by that — the service stores what it already got.
	result, err := s.chat.Turn(r.Context(), chat.TurnRequest{
		OwnerID: user.ID, Username: user.Username, Role: user.Role,
		SessionID: r.PathValue("id"), TurnID: body.TurnID, Content: body.Content,
	}, stream.send)
	if err != nil {
		stream.sendError(err)
		stream.finish("failed")
		return
	}
	stream.finish(result.Status)
}

// chatSSE writes the console's event stream.
type chatSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func (c *chatSSE) send(ev chat.Event) error {
	if !c.started {
		header := c.w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-store")
		header.Set("Connection", "keep-alive")
		// A proxy that buffers would break the whole point of streaming.
		header.Set("X-Accel-Buffering", "no")
		c.w.WriteHeader(http.StatusOK)
		c.started = true
	}
	payload, err := json.Marshal(chatEventJSON(ev))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
		return err
	}
	c.flusher.Flush()
	return nil
}

func (c *chatSSE) sendError(err error) {
	apiErr := toAPIError(err)
	_ = c.send(chat.Event{Type: chat.EventError, Code: apiErr.Code, Text: apiErr.Message,
		Level: chat.LevelError, Status: "failed"})
}

func (c *chatSSE) finish(status string) {
	if status == "" {
		status = "completed"
	}
	_ = c.send(chat.Event{Type: chat.EventDone, Status: status})
}

// chatEventJSON renders one event for the wire. Field names are short because the same
// payload is emitted for every text delta.
func chatEventJSON(ev chat.Event) map[string]any {
	out := map[string]any{"type": ev.Type}
	if ev.TurnID != "" {
		out["turn_id"] = ev.TurnID
	}
	if ev.Step > 0 {
		out["step"] = ev.Step
	}
	if ev.Delta != "" {
		out["delta"] = ev.Delta
	}
	if ev.CallID != "" {
		out["call_id"] = ev.CallID
	}
	if ev.RequestID != "" {
		out["request_id"] = ev.RequestID
	}
	if ev.Model != "" {
		out["model"] = ev.Model
	}
	if ev.Level != "" {
		out["level"] = ev.Level
	}
	if ev.Notice != "" {
		out["notice"] = ev.Notice
	}
	if ev.Code != "" {
		out["code"] = ev.Code
	}
	if ev.Text != "" {
		out["message"] = ev.Text
	}
	if ev.Status != "" {
		out["status"] = ev.Status
	}
	if ev.Outcome != "" {
		out["outcome"] = ev.Outcome
	}
	if ev.Usage != nil {
		out["usage"] = map[string]any{
			"input_tokens": ev.Usage.InputTokens, "output_tokens": ev.Usage.OutputTokens,
			"reasoning_tokens": ev.Usage.ReasoningTokens, "metered": ev.Usage.Metered,
		}
	}
	if ev.Tool != nil {
		out["tool"] = map[string]any{
			"call_id": ev.Tool.CallID, "name": ev.Tool.Name, "arguments": ev.Tool.Arguments,
			"result": ev.Tool.Result, "is_error": ev.Tool.IsError, "status": ev.Tool.Status,
			"duration_ms": ev.Tool.DurationMS,
		}
	}
	if ev.Message != nil {
		out["message"] = chatMessageJSON(ev.Message, nil)
	}
	return out
}

// ---------------------------------------------------------------------------
// skills
// ---------------------------------------------------------------------------

type chatSkillRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	SessionID    string `json:"session_id"`
}

func (s *Server) handleAdminChatListSkills(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	page, ok := chatPage(r)
	if !ok {
		writeAPIError(w, domain.ErrInvalidRequest("limit and offset must be non-negative integers"))
		return
	}
	skills, total, err := s.chat.ListSkills(r.Context(), user.ID, page.Limit, page.Offset)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "data": chatSkillsJSON(skills), "total": total,
		"limit": page.Limit, "offset": page.Offset,
	})
}

func (s *Server) handleAdminChatCreateSkill(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	var body chatSkillRequest
	if !decodeChatBody(w, r, &body) {
		return
	}
	skill, err := s.chat.CreateSkill(r.Context(), user.ID, chat.SkillInput{
		Name: body.Name, Description: body.Description, Instructions: body.Instructions,
	}, body.SessionID, "")
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The audit row names the operation and the opaque id, never the skill's name or text:
	// the audit log is global and a skill is private to its author.
	s.audit(r.Context(), user.Username, "chat.skill_create", "chat_skill",
		strconv.FormatInt(skill.ID, 10), map[string]any{"chars": len(skill.Instructions)}, "ok")
	writeJSON(w, http.StatusCreated, chatSkillJSON(skill))
}

func (s *Server) handleAdminChatUpdateSkill(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, domain.ErrInvalidRequest("the skill id must be a positive integer"))
		return
	}
	var body chatSkillRequest
	if !decodeChatBody(w, r, &body) {
		return
	}
	skill, err := s.chat.UpdateSkill(r.Context(), user.ID, id, chat.SkillInput{
		Name: body.Name, Description: body.Description, Instructions: body.Instructions,
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), user.Username, "chat.skill_update", "chat_skill",
		strconv.FormatInt(skill.ID, 10), map[string]any{"chars": len(skill.Instructions)}, "ok")
	writeJSON(w, http.StatusOK, chatSkillJSON(skill))
}

func (s *Server) handleAdminChatDeleteSkill(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, false)
	if !ok || !s.chatReady(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, domain.ErrInvalidRequest("the skill id must be a positive integer"))
		return
	}
	if err := s.chat.DeleteSkill(r.Context(), user.ID, id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), user.Username, "chat.skill_delete", "chat_skill",
		strconv.FormatInt(id, 10), nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// handleAdminChatSkillDraft distills a conversation into an unsaved skill proposal.
func (s *Server) handleAdminChatSkillDraft(w http.ResponseWriter, r *http.Request) {
	user, ok := s.chatActor(w, r, true)
	if !ok || !s.chatReady(w) {
		return
	}
	draft, err := s.chat.SkillDraft(r.Context(), user.ID, user.Username, user.Role, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": draft.Name, "description": draft.Description, "instructions": draft.Instructions,
		"note": draft.Note, "request_id": draft.RequestID, "model": draft.Model,
		"usage": map[string]any{
			"input_tokens": draft.Usage.InputTokens, "output_tokens": draft.Usage.OutputTokens,
			"reasoning_tokens": draft.Usage.ReasoningTokens,
		},
	})
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

func chatSessionJSON(session *domain.ChatSession, detail bool) map[string]any {
	if session == nil {
		return map[string]any{}
	}
	out := map[string]any{
		"id": session.ID, "title": session.Title, "model": session.Model,
		"account_id": session.AccountID, "api_key_id": session.APIKeyID,
		"write_mode": session.WriteMode, "skill_ids": session.SkillIDs,
		"message_count": session.MessageCount, "status": session.Status,
		"tokens_in": session.TokensIn, "tokens_out": session.TokensOut,
		"tokens_reasoning": session.Reasoning, "owner": session.OwnerName,
		"created_at": session.CreatedAt, "updated_at": session.UpdatedAt,
	}
	if session.MCPTokenID != nil {
		out["mcp_token_id"] = *session.MCPTokenID
	}
	if session.LastMessage != nil {
		out["last_message_at"] = session.LastMessage
	}
	if detail {
		out["object"] = "chat.session"
	}
	return out
}

func chatMessagesJSON(messages []*domain.ChatMessage, usage map[string]*domain.RequestUsage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		out = append(out, chatMessageJSON(message, usage))
	}
	return out
}

func chatMessageJSON(message *domain.ChatMessage, usage map[string]*domain.RequestUsage) map[string]any {
	if message == nil {
		return map[string]any{}
	}
	out := map[string]any{
		"id": message.ID, "session_id": message.SessionID, "turn_id": message.TurnID,
		"seq": message.Seq, "role": message.Role, "content": message.Content,
		"parts": message.Parts, "status": message.Status, "truncated": message.Truncated,
		"error": message.Error, "model": message.Model, "resolved_model": message.ResolvedModel,
		"provider": message.Provider, "request_ids": message.RequestIDs,
		"tokens_in": message.TokensIn, "tokens_out": message.TokensOut,
		"tokens_reasoning": message.ReasoningTok, "created_at": message.CreatedAt,
	}
	if message.Reasoning != "" {
		out["reasoning"] = message.Reasoning
	}
	// Cost is joined live from the usage records, never copied: the usage table is the
	// only owner of the money facts.
	if usage != nil {
		var cost, charge int64
		for _, id := range message.RequestIDs {
			if row, ok := usage[id]; ok && row != nil {
				cost += row.CostMicros
				charge += row.ChargeMicros
			}
		}
		out["cost_micros"] = cost
		out["charge_micros"] = charge
	}
	return out
}

func chatToolCallsJSON(calls []*domain.ChatToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		if call == nil {
			continue
		}
		out = append(out, map[string]any{
			"id": call.ID, "call_id": call.CallID, "step": call.Step, "name": call.Name,
			"arguments": call.Arguments, "result": call.Result, "is_error": call.IsError,
			"status": call.Status, "duration_ms": call.DurationMS, "created_at": call.CreatedAt,
		})
	}
	return out
}

func chatSkillsJSON(skills []*domain.ChatSkill) []map[string]any {
	out := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		if skill != nil {
			out = append(out, chatSkillJSON(skill))
		}
	}
	return out
}

func chatSkillJSON(skill *domain.ChatSkill) map[string]any {
	if skill == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id": skill.ID, "name": skill.Name, "description": skill.Description,
		"instructions": skill.Instructions, "source_session_id": skill.SourceSessionID,
		"source_model": skill.SourceModel, "chars": len(skill.Instructions),
		"created_at": skill.CreatedAt, "updated_at": skill.UpdatedAt,
	}
}

// chatUsageReader is the cost join used when a conversation is read.
func (s *Server) chatUsageReader() chat.UsageRecordReader {
	if s.deps.AdminStore == nil {
		return nil
	}
	return s.deps.AdminStore
}

// chatPageSpec is the window every chat list shares. Conversations and skills are the
// administrator's own rows, so the ceiling is generous but still enforced by the handler
// that parses it (the route table publishes the same numbers to MCP clients).
var chatPageSpec = pageSpec{Def: 30, Max: 200, Noun: "条数"}

// chatPage parses limit/offset for a chat list.
func chatPage(r *http.Request) (pageParams, bool) {
	page, err := adminPage(r, chatPageSpec.Def, chatPageSpec.Max)
	if err != nil {
		return pageParams{}, false
	}
	return page, true
}

// decodeChatBody reads a small JSON object body, answering 400 on malformed input.
func decodeChatBody(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := readBodyLimited(r, 1<<20)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("the request body could not be read"))
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, target); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("the request body is not valid JSON"))
		return false
	}
	return true
}

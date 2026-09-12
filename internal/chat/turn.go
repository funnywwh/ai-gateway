package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// maxQuestionBytes bounds one question. A question is a person typing into a box, so a
// megabyte of it is a paste accident rather than an intent, and it would be billed.
const maxQuestionBytes = 32 * 1024

// TurnRequest is one question asked inside a conversation.
type TurnRequest struct {
	OwnerID   int64
	Username  string
	Role      string
	SessionID string
	// TurnID is the browser's idempotency key. The same id twice must never pay twice.
	TurnID  string
	Content string
}

// TurnResult is what one turn produced.
type TurnResult struct {
	Turn      *domain.ChatTurn
	User      *domain.ChatMessage
	Assistant *domain.ChatMessage
	Status    string
	// AlreadyDone reports that this turn id had already finished, so nothing was sent to a
	// model and nothing was billed.
	AlreadyDone bool
}

// Event is one incremental update for the console. It is a single flat struct rather than
// an interface so the HTTP layer can frame it as SSE without type switches.
type Event struct {
	Type    string
	TurnID  string
	Step    int
	Delta   string
	Message *domain.ChatMessage
	Tool    *domain.ChatToolCall
	Usage   *Usage
	Level   string
	Notice  string
	Code    string
	Text    string
	Status  string
	Model   string
	Outcome string
	// CallID identifies the tool call a tool_call/tool_result event belongs to.
	CallID string
	// RequestID lets the console link a step to the request log row it produced.
	RequestID string
}

// Turn answers one question: it stores the question, runs the model/tool loop, and stores
// the answer. Everything it spends is spent through the Runner, which bills each step.
func (s *Service) Turn(ctx context.Context, req TurnRequest, emit func(Event) error) (*TurnResult, error) {
	if req.OwnerID <= 0 {
		return nil, domain.ErrUnauthorized("chat requires an authenticated administrator session")
	}
	if err := requireAdmin(req.Role, "ask a billed question in the console"); err != nil {
		return nil, err
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return nil, domain.ErrInvalidRequest("type a question first")
	}
	if len(content) > maxQuestionBytes {
		return nil, domain.ErrInvalidRequest(fmt.Sprintf("the question must be at most %d bytes", maxQuestionBytes))
	}
	if strings.TrimSpace(req.TurnID) == "" {
		return nil, domain.ErrInvalidRequest("this turn is missing its id")
	}
	session, err := s.store.GetChatSession(ctx, req.SessionID, req.OwnerID)
	if err != nil {
		return nil, err
	}
	if session.Status != "" && session.Status != "active" {
		return nil, domain.ErrConflict("this conversation is archived")
	}
	if session.AccountID <= 0 || session.APIKeyID <= 0 {
		return nil, domain.ErrInvalidRequest("bind an account and API key to this conversation before asking")
	}

	// Idempotency first: a resend after a dropped connection must return what already
	// happened instead of buying the same answer twice.
	if existing, err := s.store.GetChatTurn(ctx, session.ID, req.TurnID); err == nil {
		if existing.Status == domain.ChatTurnRunning {
			return nil, domain.ErrConflict("this turn is still running")
		}
		messages, err := s.store.ListChatTurnMessages(ctx, session.ID, req.TurnID)
		if err != nil {
			return nil, err
		}
		result := &TurnResult{Turn: existing, Status: existing.Status, AlreadyDone: true}
		for _, m := range messages {
			switch m.Role {
			case domain.ChatRoleUser:
				result.User = m
			case domain.ChatRoleAssistant:
				result.Assistant = m
			}
		}
		return result, nil
	} else if !isNotFound(err) {
		return nil, err
	}

	if s.isBusy(session.ID) {
		return nil, domain.ErrConflict("this conversation is already answering a question")
	}

	turn := &domain.ChatTurn{
		ID:        ids.ChatTurn(),
		SessionID: session.ID,
		TurnID:    req.TurnID,
		Status:    domain.ChatTurnRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateChatTurn(ctx, turn); err != nil {
		return nil, err
	}
	s.setBusy(session.ID, true)
	defer s.setBusy(session.ID, false)

	user := &domain.ChatMessage{
		ID:        ids.ChatMessage(),
		SessionID: session.ID,
		TurnID:    turn.TurnID,
		Role:      domain.ChatRoleUser,
		Content:   content,
		Parts:     []domain.ChatPart{{Type: "text", Text: content}},
		Status:    domain.ChatMessageOK,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.AppendChatMessage(ctx, user); err != nil {
		_ = s.store.SetChatTurnStatus(ctx, turn.ID, domain.ChatTurnFailed, err.Error(), nil)
		return nil, err
	}
	s.emit(emit, Event{Type: EventTurn, TurnID: turn.TurnID, Message: user})

	run := s.runTurn(ctx, session, turn, req, emit)
	return s.finishTurn(ctx, session, turn, user, run, emit)
}

// turnRun is the in-memory state of one turn's model/tool loop.
type turnRun struct {
	parts         []domain.ChatPart
	providerItems []pluginapi.Item
	requestIDs    []string
	text          strings.Builder
	reasoning     strings.Builder
	usage         Usage
	steps         int
	toolCalls     int
	status        string
	outcome       string
	truncated     bool
	errMsg        string
	resolvedModel string
	provider      string
}

// runTurn drives the model/tool loop. It never returns an error: a failed turn is data to
// store (the user paid for it and must see what happened), not a reason to lose the
// question that was already persisted.
func (s *Service) runTurn(ctx context.Context, session *domain.ChatSession, turn *domain.ChatTurn, req TurnRequest, emit func(Event) error) *turnRun {
	run := &turnRun{status: domain.ChatMessageOK, outcome: OutcomeCompleted}
	skills, err := s.loadedSkills(ctx, session.OwnerUserID, session.SkillIDs)
	if err != nil {
		run.status, run.outcome, run.errMsg = domain.ChatMessageFailed, OutcomeFailed, err.Error()
		return run
	}
	messages, err := s.store.ListChatMessages(ctx, session.ID, 0)
	if err != nil {
		run.status, run.outcome, run.errMsg = domain.ChatMessageFailed, OutcomeFailed, err.Error()
		return run
	}
	history := buildHistory(messages, s.cfg.MaxHistoryMessages, s.cfg.MaxHistoryBytes)
	if history.tooLarge {
		// Sending half a question would produce an answer about a conversation nobody had.
		run.status, run.outcome = domain.ChatMessageFailed, OutcomeFailed
		run.errMsg = "这段对话已经超出单次请求的上下文上限，请新建一个会话继续提问"
		s.emit(emit, Event{Type: EventNotice, TurnID: turn.TurnID, Level: LevelError, Notice: run.errMsg})
		return run
	}
	if history.dropped > 0 {
		s.emit(emit, Event{
			Type: EventNotice, TurnID: turn.TurnID, Level: LevelInfo,
			Notice: fmt.Sprintf("为控制请求大小，更早的 %d 轮对话没有随本次请求发送", history.dropped),
		})
	}

	for step := 1; step <= s.cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			run.status, run.outcome = domain.ChatMessageAborted, OutcomeCancelled
			run.errMsg = "已停止生成"
			break
		}
		// The session is re-read for every step so a write-permission change, or a
		// conversation deleted from another tab, takes effect inside a long answer.
		current, err := s.store.GetChatSession(ctx, session.ID, session.OwnerUserID)
		if err != nil {
			run.status, run.outcome, run.errMsg = domain.ChatMessageFailed, OutcomeFailed, "会话已不存在或已无权访问"
			break
		}
		session = current
		role, err := s.currentRole(ctx, req)
		if err != nil {
			run.status, run.outcome, run.errMsg = domain.ChatMessageFailed, OutcomeFailed, err.Error()
			break
		}
		access := s.accessFor(session, req.Username, role)

		stepTools := s.toolSurfaceFor(access)
		stepReq := Step{
			Model:           session.Model,
			Instructions:    systemPrompt(promptContext{skills: skills, cfg: s.cfg}),
			Items:           append(append([]pluginapi.Item{}, history.items...), run.providerItems...),
			Tools:           stepTools,
			PromptCacheKey:  session.ID,
			AccountID:       session.AccountID,
			APIKeyID:        session.APIKeyID,
			Actor:           req.Username,
			MaxOutputTokens: s.cfg.MaxOutputTokens,
		}
		run.steps = step
		s.emit(emit, Event{Type: EventStep, TurnID: turn.TurnID, Step: step, Model: session.Model})

		result, err := s.runner.RunStep(ctx, stepReq, func(ev StepEvent) error {
			return s.forwardStepEvent(emit, turn, step, ev)
		})
		if err != nil {
			run.status, run.outcome = domain.ChatMessageFailed, OutcomeFailed
			run.errMsg = stepErrorText(err)
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				run.status, run.outcome, run.errMsg = domain.ChatMessageAborted, OutcomeCancelled, "已停止生成"
			}
			s.emit(emit, Event{Type: EventError, TurnID: turn.TurnID, Step: step, Code: stepErrorCode(err), Text: run.errMsg})
			break
		}
		s.absorbStep(run, step, result, emit, turn)
		if result.Outcome != OutcomeCompleted {
			run.outcome = result.Outcome
			switch result.Outcome {
			case OutcomeIncomplete:
				run.truncated = true
				run.status = domain.ChatMessageOK
				run.errMsg = "模型输出被长度限制截断"
				s.emit(emit, Event{Type: EventNotice, TurnID: turn.TurnID, Step: step, Level: LevelWarn,
					Notice: "这一轮的输出被截断，因此没有执行它提出的工具调用；可以让我继续，或把问题拆小一点"})
			case OutcomeCancelled:
				run.status, run.errMsg = domain.ChatMessageAborted, "已停止生成"
			default:
				run.status = domain.ChatMessageFailed
				if run.errMsg == "" {
					run.errMsg = "模型这一轮没有正常结束"
				}
			}
			break
		}

		calls := callsOf(result)
		if len(calls) == 0 {
			break
		}
		if !result.ToolCallsComplete {
			run.truncated = true
			s.emit(emit, Event{Type: EventNotice, TurnID: turn.TurnID, Step: step, Level: LevelWarn,
				Notice: "工具调用的参数没有完整生成，已跳过执行以免误操作"})
			break
		}
		if len(calls) > remainingToolCalls(s.cfg.MaxToolCalls, run.toolCalls) {
			s.emit(emit, Event{Type: EventNotice, TurnID: turn.TurnID, Step: step, Level: LevelWarn,
				Notice: fmt.Sprintf("已达到本次提问的工具调用上限（%d 次），剩余调用未执行", s.cfg.MaxToolCalls)})
			break
		}

		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				run.status, run.outcome, run.errMsg = domain.ChatMessageAborted, OutcomeCancelled, "已停止生成"
				return run
			}
			// The role and the write permission are re-resolved for every single tool
			// call: a demoted administrator, or a conversation switched back to
			// read-only, must not keep writing because a turn started earlier.
			liveRole, err := s.currentRole(ctx, req)
			if err != nil {
				run.status, run.outcome, run.errMsg = domain.ChatMessageFailed, OutcomeFailed, err.Error()
				return run
			}
			liveSession, err := s.store.GetChatSession(ctx, session.ID, session.OwnerUserID)
			if err != nil {
				run.status, run.outcome, run.errMsg = domain.ChatMessageFailed, OutcomeFailed, "会话已不存在或已无权访问"
				return run
			}
			session = liveSession
			callAccess := s.accessFor(session, req.Username, liveRole)
			outcome := s.executeToolCall(ctx, callAccess, session, turn, step, call, result)
			run.toolCalls++
			run.parts = append(run.parts, domain.ChatPart{
				Type:      "tool_call",
				ID:        call.CallID,
				Name:      call.Name,
				Arguments: call.Arguments,
				Result:    outcome.result,
				IsError:   outcome.isError,
				Status:    outcome.status,
			})
			run.providerItems = append(run.providerItems, outcome.items...)
			s.emit(emit, Event{Type: EventToolResult, TurnID: turn.TurnID, Step: step, Tool: outcome.record})
		}
		if step == s.cfg.MaxSteps {
			s.emit(emit, Event{Type: EventNotice, TurnID: turn.TurnID, Level: LevelWarn,
				Notice: fmt.Sprintf("已达到本次提问的步数上限（%d 步），回答可能还没完成", s.cfg.MaxSteps)})
		}
	}
	return run
}

// absorbStep folds one model step into the turn state.
func (s *Service) absorbStep(run *turnRun, step int, result *StepResult, emit func(Event) error, turn *domain.ChatTurn) {
	if result == nil {
		return
	}
	if result.Text != "" {
		run.text.WriteString(result.Text)
		if n := len(run.parts); n > 0 && run.parts[n-1].Type == "text" {
			run.parts[n-1].Text += result.Text
		} else {
			run.parts = append(run.parts, domain.ChatPart{Type: "text", Text: result.Text})
		}
	}
	if s.cfg.RecordReasoning && result.Reasoning != "" {
		run.reasoning.WriteString(result.Reasoning)
	}
	run.providerItems = append(run.providerItems, result.Items...)
	if result.RequestID != "" {
		run.requestIDs = append(run.requestIDs, result.RequestID)
	}
	run.usage.InputTokens += result.Usage.InputTokens
	run.usage.OutputTokens += result.Usage.OutputTokens
	run.usage.ReasoningTokens += result.Usage.ReasoningTokens
	run.usage.Metered = run.usage.Metered || result.Usage.Metered
	if result.ResolvedModel != "" {
		run.resolvedModel = result.ResolvedModel
	}
	if result.Provider != "" {
		run.provider = result.Provider
	}
	s.emit(emit, Event{Type: EventUsage, TurnID: turn.TurnID, Step: step, RequestID: result.RequestID, Usage: &Usage{
		InputTokens:     result.Usage.InputTokens,
		OutputTokens:    result.Usage.OutputTokens,
		ReasoningTokens: result.Usage.ReasoningTokens,
		CostMicros:      result.Usage.CostMicros,
		ChargeMicros:    result.Usage.ChargeMicros,
		Metered:         result.Usage.Metered,
	}})
	if len(result.Degraded) > 0 {
		s.emit(emit, Event{Type: EventNotice, TurnID: turn.TurnID, Step: step, Level: LevelWarn,
			Notice: "本次请求被路由降级：" + strings.Join(result.Degraded, ", ")})
	}
}

// toolOutcome is one executed (or refused) tool call, plus the provider items that carry
// its result back to the model on the next step.
type toolOutcome struct {
	record  *domain.ChatToolCall
	result  string
	isError bool
	status  string
	items   []pluginapi.Item
}

// executeToolCall runs one management tool call. The pending row is written first: if the
// process dies mid-call, the record says "this may already have happened" instead of
// leaving a silent gap that a retry would happily fill twice.
func (s *Service) executeToolCall(ctx context.Context, access Access, session *domain.ChatSession, turn *domain.ChatTurn, step int, call pluginapi.Item, stepResult *StepResult) toolOutcome {
	record := &domain.ChatToolCall{
		ID:        ids.ChatToolCall(),
		SessionID: session.ID,
		TurnID:    turn.TurnID,
		Step:      step,
		CallID:    call.CallID,
		Name:      call.Name,
		Arguments: call.Arguments,
		Status:    domain.ChatToolPending,
		CreatedAt: time.Now().UTC(),
	}
	if s.tools == nil {
		msg := `{"error":"the management tool surface is not enabled on this gateway"}`
		return refuseToolCall(record, msg)
	}
	if err := s.store.CreateChatToolCall(ctx, record); err != nil {
		if isConflict(err) {
			// The unique key says this call id already ran for this turn and step. That can
			// only happen after a restart or a duplicated stream, and re-running a write
			// tool is exactly what must not happen.
			msg := `{"error":"this tool call was already executed in this turn and was not repeated"}`
			return refuseToolCall(record, msg)
		}
		return refuseToolCall(record, `{"error":"could not record the tool call, so it was not executed"}`)
	}

	args := map[string]any{}
	if strings.TrimSpace(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			msg := `{"error":"the tool arguments were not valid JSON, so nothing was executed"}`
			_ = s.store.FinishChatToolCall(ctx, record.ID, msg, true, domain.ChatToolFailed, 0)
			record.Result, record.IsError, record.Status = msg, true, domain.ChatToolFailed
			return refuseToolCall(record, msg)
		}
	}

	started := time.Now()
	result, err := s.tools.Call(ctx, access, call.Name, args)
	duration := time.Since(started).Milliseconds()

	value := any(nil)
	isError := false
	if err != nil {
		value = map[string]any{"error": err.Error()}
		isError = true
	} else {
		value = result.Value
		isError = result.IsError
	}
	encoded := encodeToolValue(value)
	encoded, _ = s.truncateToolResult(encoded)
	status := domain.ChatToolDone
	if isError {
		status = domain.ChatToolFailed
	}
	if err := s.store.FinishChatToolCall(ctx, record.ID, encoded, isError, status, int(duration)); err != nil {
		// The record could not be closed, so the outcome is genuinely unknown even though
		// the call returned. Say so rather than reporting a clean success.
		s.log.Warn("recording chat tool result failed", "err", err, "call", record.CallID, "tool", record.Name)
		status = domain.ChatToolUnknown
	}
	record.Result, record.IsError, record.Status, record.DurationMS = encoded, isError, status, int(duration)
	return toolOutcome{
		record: record, result: encoded, isError: isError, status: status,
		items: toolResultItems(stepResult, record, encoded),
	}
}

// toolResultItems builds the provider items that carry one tool result back to the model.
// The function_call item itself normally came from the step's own output items, so it is
// only added when the provider did not emit one (a streaming quirk, or a plugin that
// reports calls out of band) — otherwise the conversation would contain the same call twice.
func toolResultItems(result *StepResult, record *domain.ChatToolCall, encoded string) []pluginapi.Item {
	items := []pluginapi.Item{}
	if !hasFunctionCall(result, record.CallID) {
		items = append(items, functionCallItem(record.CallID, record.Name, record.Arguments))
	}
	return append(items, functionOutputItem(record.CallID, encoded))
}

// hasFunctionCall reports whether a step already emitted the function_call item for one
// call id.
func hasFunctionCall(result *StepResult, callID string) bool {
	if result == nil || callID == "" {
		return false
	}
	for _, item := range result.Items {
		if item.Type == "function_call" && item.CallID == callID {
			return true
		}
	}
	return false
}

// refuseToolCall records a tool call that was deliberately not executed, and answers the
// model with the reason. The function_call item is echoed back so the provider sees a call
// that was answered rather than one that vanished.
func refuseToolCall(record *domain.ChatToolCall, message string) toolOutcome {
	return toolOutcome{
		record: record, result: message, isError: true, status: domain.ChatToolFailed,
		items: []pluginapi.Item{
			functionCallItem(record.CallID, record.Name, record.Arguments),
			functionOutputItem(record.CallID, message),
		},
	}
}

// finishTurn stores the answer and closes the turn, then tells the console.
func (s *Service) finishTurn(ctx context.Context, session *domain.ChatSession, turn *domain.ChatTurn, user *domain.ChatMessage, run *turnRun, emit func(Event) error) (*TurnResult, error) {
	// The client may be gone by now (a closed tab, a cancelled fetch). The question was
	// billed, so the answer is written with a context that survives the cancellation.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()

	status := run.status
	if ctx.Err() != nil && status == domain.ChatMessageOK {
		status = domain.ChatMessageAborted
	}
	if err := ctx.Err(); err != nil && run.errMsg == "" {
		run.errMsg = "已停止生成"
	}
	assistant := &domain.ChatMessage{
		ID:            ids.ChatMessage(),
		SessionID:     session.ID,
		TurnID:        turn.TurnID,
		Role:          domain.ChatRoleAssistant,
		Content:       contentOf(run.parts),
		Parts:         run.parts,
		ProviderItems: encodeItems(run.providerItems),
		Status:        status,
		Truncated:     run.truncated,
		Error:         run.errMsg,
		Model:         session.Model,
		ResolvedModel: run.resolvedModel,
		Provider:      run.provider,
		RequestIDs:    run.requestIDs,
		TokensIn:      run.usage.InputTokens,
		TokensOut:     run.usage.OutputTokens,
		ReasoningTok:  run.usage.ReasoningTokens,
		CreatedAt:     time.Now().UTC(),
	}
	if s.cfg.RecordReasoning {
		assistant.Reasoning = truncateBytes(run.reasoning.String(), maxReasoningStored)
	}
	if assistant.Status == domain.ChatMessageOK && len(run.parts) == 0 {
		assistant.Status = domain.ChatMessageFailed
		if assistant.Error == "" {
			assistant.Error = "模型没有返回任何内容"
		}
	}
	if err := s.store.AppendChatMessage(saveCtx, assistant); err != nil {
		s.log.Error("storing the chat answer failed", "err", err, "session", session.ID, "turn", turn.TurnID)
		_ = s.store.SetChatTurnStatus(saveCtx, turn.ID, domain.ChatTurnFailed, err.Error(), run.requestIDs)
		return nil, err
	}
	title := ""
	if session.MessageCount == 0 && strings.TrimSpace(session.Title) == "" {
		title = titleFromQuestion(user.Content)
	}
	if err := s.store.UpdateChatSessionAggregates(saveCtx, session.ID, session.OwnerUserID, 2,
		run.usage.InputTokens, run.usage.OutputTokens, run.usage.ReasoningTokens, assistant.CreatedAt, title); err != nil {
		s.log.Warn("updating chat session totals failed", "err", err, "session", session.ID)
	}

	turnStatus := domain.ChatTurnCompleted
	switch assistant.Status {
	case domain.ChatMessageFailed:
		turnStatus = domain.ChatTurnFailed
	case domain.ChatMessageAborted:
		turnStatus = domain.ChatTurnAborted
	case domain.ChatMessageInterrupted:
		turnStatus = domain.ChatTurnInterrupted
	}
	if err := s.store.SetChatTurnStatus(saveCtx, turn.ID, turnStatus, assistant.Error, run.requestIDs); err != nil {
		s.log.Warn("closing the chat turn failed", "err", err, "turn", turn.TurnID)
	}
	turn.Status = turnStatus
	turn.RequestIDs = run.requestIDs

	s.emit(emit, Event{Type: EventMessage, TurnID: turn.TurnID, Message: assistant, Status: turnStatus})
	s.emit(emit, Event{Type: EventDone, TurnID: turn.TurnID, Status: turnStatus, Text: assistant.Error})
	return &TurnResult{Turn: turn, User: user, Assistant: assistant, Status: turnStatus}, nil
}

// currentRole re-reads the administrator's role. The turn loop calls it before every
// tool call so a role change takes effect inside a long answer rather than at the next
// login.
func (s *Service) currentRole(ctx context.Context, req TurnRequest) (string, error) {
	role, err := s.store.AdminRole(ctx, req.OwnerID)
	if err != nil {
		if isNotFound(err) {
			return "", domain.ErrUnauthorized("your administrator session is no longer valid")
		}
		return "", err
	}
	return role, nil
}

func (s *Service) accessFor(session *domain.ChatSession, username, role string) Access {
	if session == nil {
		return Access{Username: username, Role: role}
	}
	return Access{
		OwnerID:      session.OwnerUserID,
		Username:     username,
		Role:         role,
		WriteMode:    session.WriteMode,
		SessionID:    session.ID,
		SessionTitle: session.Title,
	}
}

// forwardStepEvent re-emits one model-step event to the console.
func (s *Service) forwardStepEvent(emit func(Event) error, turn *domain.ChatTurn, step int, ev StepEvent) error {
	switch ev.Kind {
	case "text":
		return emit(Event{Type: EventText, TurnID: turn.TurnID, Step: step, Delta: ev.Text})
	case "reasoning":
		return emit(Event{Type: EventReasoning, TurnID: turn.TurnID, Step: step, Delta: ev.Text})
	case "tool_call":
		return emit(Event{Type: EventToolCall, TurnID: turn.TurnID, Step: step, CallID: ev.CallID,
			Tool: &domain.ChatToolCall{CallID: ev.CallID, Name: ev.Name, Arguments: ev.Arguments, Status: domain.ChatToolPending}})
	default:
		return nil
	}
}

// emit sends one event, ignoring a closed stream: the caller's context is what decides
// whether the turn continues.
func (s *Service) emit(emit func(Event) error, ev Event) {
	if emit == nil || ev.Type == "" {
		return
	}
	_ = emit(ev)
}

func callsOf(result *StepResult) []pluginapi.Item {
	if result == nil {
		return nil
	}
	out := make([]pluginapi.Item, 0, 2)
	for _, item := range result.Items {
		if item.Type == "function_call" {
			out = append(out, item)
		}
	}
	return out
}

func remainingToolCalls(max, used int) int {
	if max <= 0 {
		return 1 << 30
	}
	return max - used
}

// contentOf projects the ordered parts onto plain text: it is what the console shows in a
// collapsed view, what a session title is derived from, and what is sent as the assistant
// message when no provider items were stored.
func contentOf(parts []domain.ChatPart) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func encodeToolValue(value any) string {
	if value == nil {
		return "{}"
	}
	if text, ok := value.(string); ok {
		encoded, err := json.Marshal(map[string]string{"result": text})
		if err != nil {
			return `{"error":"the tool result could not be encoded"}`
		}
		return string(encoded)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"error":"the tool result could not be encoded"}`
	}
	return string(encoded)
}

func (s *Service) truncateToolResult(encoded string) (string, bool) {
	if len(encoded) <= s.cfg.MaxToolResultBytes {
		return encoded, false
	}
	// Truncated JSON is not JSON, so the cut text is carried inside a valid envelope: the
	// model gets a parseable result that says out loud that it is incomplete.
	cut := encoded[:s.cfg.MaxToolResultBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	wrapped, err := json.Marshal(map[string]any{
		"truncated": true,
		"note":      fmt.Sprintf("the management tool returned more than %d bytes; only the beginning is shown", s.cfg.MaxToolResultBytes),
		"result":    cut,
	})
	if err != nil {
		return `{"truncated":true,"note":"the tool result could not be encoded"}`, true
	}
	return string(wrapped), true
}

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// stepErrorText renders a failed step for the user. A gateway/upstream error keeps the
// message the API returned, because that is the same text any other client would see.
func stepErrorText(err error) string {
	var failure *StepFailure
	if errors.As(err, &failure) {
		if strings.TrimSpace(failure.Message) != "" {
			return failure.Message
		}
	}
	if err == nil {
		return "模型调用失败"
	}
	return err.Error()
}

func stepErrorCode(err error) string {
	var failure *StepFailure
	if errors.As(err, &failure) && failure.Code != "" {
		return failure.Code
	}
	return "step_failed"
}

// Package chat implements the console's smart-chat service: one conversation loop that
// asks a model, lets it call the gateway's own management tools, and stores the result.
//
// It is deliberately free of HTTP, SQL and routing knowledge. Three narrow ports carry
// everything it needs: Store (owner-scoped persistence), Runner (one billed model step
// through the gateway data plane) and Tools (the management tool surface). That split is
// what lets the loop be tested without a gateway, and lets the HTTP layer own the risky
// parts — authenticating the billed key and dispatching into the real Responses handler.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Roles, mirroring the administrator roles the console issues.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Chart bounds. The console renders `chart` blocks itself (no CDN, no model-authored
// code), and these two numbers are the contract between the prompt that asks the model to
// stay inside them, the Go side that documents them, and the JavaScript renderer that
// enforces them. internal/webui/embed_test.go asserts the JS constants still match.
const (
	MaxChartSeries = 8
	MaxChartPoints = 500
)

// Limits on stored text. They are not configurable because they exist to keep one row
// readable, not to express a policy.
const (
	maxTitleRunes        = 60
	maxSkillNameRunes    = 80
	maxSkillDescRunes    = 300
	maxDraftBytes        = 60 * 1024
	maxReasoningStored   = 256 * 1024
	saveTimeout          = 5 * time.Second
	defaultDraftMaxTurns = 20
)

// Step outcomes. They decide whether the loop may execute the tools a step proposed: only
// a step that finished normally can be trusted to have produced complete arguments.
const (
	OutcomeCompleted  = "completed"
	OutcomeIncomplete = "incomplete"
	OutcomeFailed     = "failed"
	OutcomeCancelled  = "cancelled"
)

// Event types emitted by the service. The HTTP layer re-frames them as SSE.
const (
	EventTurn       = "turn"
	EventStep       = "step"
	EventText       = "text"
	EventReasoning  = "reasoning"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventUsage      = "usage"
	EventNotice     = "notice"
	EventMessage    = "message"
	EventError      = "error"
	EventDone       = "done"
)

// Notice levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Config is the service configuration, mapped from config.Chat by the composition root.
type Config struct {
	Enabled            bool
	MaxSteps           int
	MaxToolCalls       int
	MaxToolResultBytes int
	MaxHistoryMessages int
	MaxHistoryBytes    int
	MaxLoadedSkills    int
	MaxSkillBytes      int
	RecordReasoning    bool
	MaxOutputTokens    int
	SystemPrompt       string
	// UIBridge tells the model that a previewed HTML page can send an event back into this
	// conversation. It mirrors chat.ui_bridge_enabled: when the deployment serves read-only
	// previews, instructing the model to build submitting forms would only produce buttons
	// that do nothing.
	UIBridge bool
}

// Usage is what one model step consumed. Tokens come from the provider's own report; cost
// and charge are read back from the usage records (the single owner of the money facts)
// and stay zero here — the console joins them per request id when it reads a conversation.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	CostMicros      int64
	ChargeMicros    int64
	Metered         bool
}

// Step is one model call.
type Step struct {
	Model           string
	Instructions    string
	Items           []pluginapi.Item
	Tools           []Tool
	PromptCacheKey  string
	AccountID       int64
	APIKeyID        int64
	Actor           string
	MaxOutputTokens int
}

// StepEvent is one incremental piece of a step's output.
type StepEvent struct {
	Kind      string // text | reasoning | tool_call
	Text      string
	CallID    string
	Name      string
	Arguments string
}

// StepResult is everything one model step produced. Outcome is authoritative: a step that
// was cut short must not have its tools executed, even if it emitted a call before dying.
type StepResult struct {
	Items             []pluginapi.Item
	Text              string
	Reasoning         string
	Usage             Usage
	Outcome           string
	ToolCallsComplete bool
	RequestID         string
	ResolvedModel     string
	Provider          string
	Degraded          []string
	FinishReason      string
}

// Tool is one management tool offered to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolResult is the outcome of one management tool call.
type ToolResult struct {
	Value   any
	IsError bool
}

// Runner performs one billed model step. The implementation lives in the HTTP layer
// because that is where the real Responses handler and the billed key live.
type Runner interface {
	RunStep(ctx context.Context, step Step, emit func(StepEvent) error) (*StepResult, error)
}

// Tools is the management tool surface offered to the model.
type Tools interface {
	List(access Access) []Tool
	Call(ctx context.Context, access Access, name string, args map[string]any) (ToolResult, error)
}

// Access is the authorization context of one turn. It is rebuilt from the database for
// every tool call rather than trusted for the length of a turn: a viewer demoted, a session
// rebound to another token or a token revoked must take effect immediately.
type Access struct {
	OwnerID      int64
	Username     string
	Role         string
	WriteMode    string
	SessionID    string
	SessionTitle string
	// MCPTokenID is the MCP token this conversation acts as, and therefore the only source
	// of its authority. The tool surface resolves it again on every call instead of trusting
	// a scope captured here, so a revoked or expired token stops the conversation at the
	// next call. Zero means unbound.
	MCPTokenID int64
}

// Store is the owner-scoped persistence the service needs.
type Store interface {
	CreateChatSession(ctx context.Context, s *domain.ChatSession) error
	GetChatSession(ctx context.Context, id string, ownerUserID int64) (*domain.ChatSession, error)
	ListChatSessions(ctx context.Context, ownerUserID int64, limit, offset int) ([]*domain.ChatSession, int, error)
	UpdateChatSession(ctx context.Context, s *domain.ChatSession) error
	UpdateChatSessionAggregates(ctx context.Context, id string, ownerUserID int64, messages, tokensIn, tokensOut, tokensReasoning int, lastMessage time.Time, title string) error
	DeleteChatSession(ctx context.Context, id string, ownerUserID int64) error

	CreateChatTurn(ctx context.Context, t *domain.ChatTurn) error
	GetChatTurn(ctx context.Context, sessionID, turnID string) (*domain.ChatTurn, error)
	SetChatTurnStatus(ctx context.Context, id, status, errMsg string, requestIDs []string) error
	InterruptRunningChatTurns(ctx context.Context) (int64, error)

	AppendChatMessage(ctx context.Context, m *domain.ChatMessage) error
	ListChatMessages(ctx context.Context, sessionID string, newest int) ([]*domain.ChatMessage, error)
	ListChatTurnMessages(ctx context.Context, sessionID, turnID string) ([]*domain.ChatMessage, error)

	CreateChatToolCall(ctx context.Context, c *domain.ChatToolCall) error
	FinishChatToolCall(ctx context.Context, id, result string, isError bool, status string, durationMS int) error
	ListChatToolCalls(ctx context.Context, sessionID string, limit int) ([]*domain.ChatToolCall, error)

	// AdminRole re-reads the caller's administrator role. The turn loop calls it before
	// every tool call, so a demoted account stops being able to write inside a long answer.
	AdminRole(ctx context.Context, userID int64) (string, error)
	// AdminSessionUser resolves a still-valid console session id to its administrator.
	// Preview tickets are bound to a session, so this is what makes logging out revoke
	// every outstanding preview link.
	AdminSessionUser(ctx context.Context, sessionID string) (int64, error)

	CreateChatSkill(ctx context.Context, s *domain.ChatSkill) (int64, error)
	GetChatSkill(ctx context.Context, id, ownerUserID int64) (*domain.ChatSkill, error)
	ListChatSkills(ctx context.Context, ownerUserID int64, limit, offset int) ([]*domain.ChatSkill, int, error)
	UpdateChatSkill(ctx context.Context, s *domain.ChatSkill) error
	DeleteChatSkill(ctx context.Context, id, ownerUserID int64) error
}

// StepFailure is a model step that failed at the gateway or upstream. The HTTP layer fills
// it in from the real API error so the console can show the same message any other client
// would receive.
type StepFailure struct {
	Status  int
	Code    string
	Message string
}

func (e *StepFailure) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "the model step failed"
}

// UsageRecordReader is the narrow read the console uses to attach cost and charge to a
// stored conversation. It is implemented by the same store the request log page reads.
type UsageRecordReader interface {
	RequestUsages(ctx context.Context, requestIDs []string) (map[string]*domain.RequestUsage, error)
}

// Service is the console chat service.
type Service struct {
	cfg    Config
	store  Store
	runner Runner
	tools  Tools
	log    *slog.Logger

	// busy guards one turn per conversation inside this process. The durable half of the
	// same rule is the running-turn row: it survives a restart, this one stops two
	// simultaneous clicks from both paying for a first step.
	mu   sync.Mutex
	busy map[string]bool
}

// New builds the service.
func New(cfg Config, store Store, runner Runner, tools Tools, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	cfg = withDefaults(cfg)
	return &Service{cfg: cfg, store: store, runner: runner, tools: tools, log: log, busy: map[string]bool{}}
}

// withDefaults fills in the values a zero config would otherwise turn into a broken
// service. MaxSteps and MaxToolCalls are deliberately *not* defaulted: zero means "no
// limit", which is what an operator who did not configure a ceiling asked for.
func withDefaults(cfg Config) Config {
	def := Config{
		Enabled:            true,
		MaxToolResultBytes: 64 * 1024,
		MaxHistoryMessages: 40,
		MaxHistoryBytes:    256 * 1024,
		MaxLoadedSkills:    12,
		MaxSkillBytes:      16 * 1024,
		RecordReasoning:    true,
	}
	if cfg.MaxSteps < 0 {
		cfg.MaxSteps = 0
	}
	if cfg.MaxToolCalls < 0 {
		cfg.MaxToolCalls = 0
	}
	if cfg.MaxToolResultBytes <= 0 {
		cfg.MaxToolResultBytes = def.MaxToolResultBytes
	}
	if cfg.MaxHistoryMessages <= 0 {
		cfg.MaxHistoryMessages = def.MaxHistoryMessages
	}
	if cfg.MaxHistoryBytes <= 0 {
		cfg.MaxHistoryBytes = def.MaxHistoryBytes
	}
	if cfg.MaxLoadedSkills < 0 {
		cfg.MaxLoadedSkills = def.MaxLoadedSkills
	}
	if cfg.MaxSkillBytes <= 0 {
		cfg.MaxSkillBytes = def.MaxSkillBytes
	}
	return cfg
}

// Config exposes the effective configuration (the HTTP layer reads the skill/artifact
// limits from here rather than keeping a second copy).
func (s *Service) Config() Config { return s.cfg }

// RecoverInterrupted marks turns that were running when the process last stopped. Their
// tool calls are reported as unknown instead of being retried, because a pending row means
// "this may already have happened".
func (s *Service) RecoverInterrupted(ctx context.Context) (int64, error) {
	n, err := s.store.InterruptRunningChatTurns(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Warn("chat turns were interrupted by a restart", "turns", n)
	}
	return n, nil
}

// SessionInput is the editable state of a conversation.
type SessionInput struct {
	Title     *string
	Model     string
	AccountID int64
	APIKeyID  int64
	// MCPTokenID binds the MCP token this conversation acts as. It is the only input that
	// decides authority; write_mode is derived from it and is not accepted from a client.
	MCPTokenID int64
	SkillIDs   []int64
}

// mcpTokenLookup is the optional persistence half of token binding. It is separate from
// Store because the token table belongs to the MCP token store, not the chat store; a Store
// that does not implement it simply cannot validate a binding at creation time (authority
// still lives in the per-call resolution the tool surface does).
type mcpTokenLookup interface {
	GetMCPTokenByID(ctx context.Context, id int64) (*domain.MCPToken, error)
}

// resolveMCPToken loads the bound token and refuses one that is unusable right now. Checking
// at binding time turns "your token was revoked an hour ago" into an error on the form
// instead of a mystery failure on the first question.
func (s *Service) resolveMCPToken(ctx context.Context, tokenID int64) (*domain.MCPToken, error) {
	if tokenID <= 0 {
		return nil, domain.ErrInvalidRequest("choose an MCP token for this conversation")
	}
	lookup, ok := s.store.(mcpTokenLookup)
	if !ok {
		return nil, domain.ErrInternal("this deployment cannot resolve MCP tokens")
	}
	token, err := lookup.GetMCPTokenByID(ctx, tokenID)
	if err != nil {
		if isNotFound(err) {
			return nil, domain.ErrInvalidRequest("that MCP token no longer exists")
		}
		return nil, err
	}
	if token.Status != "active" {
		return nil, domain.ErrForbidden("that MCP token is revoked; issue a new one before binding it")
	}
	if token.ExpiresAt != nil && time.Now().UTC().After(*token.ExpiresAt) {
		return nil, domain.ErrForbidden("that MCP token has expired; issue a new one before binding it")
	}
	return token, nil
}

// MCP token scopes, as the token row stores them. They are spelled out here rather than
// imported from internal/mcpsrv because the chat service must not depend on the MCP server
// package (see the layering table in docs/architecture.md); the two spellings are held
// together by TestChatScopeVocabularyMatchesMCP in the transport's test suite, which is the
// one place both packages are legitimately visible.
const scopeAdmin = "admin"

// WritableScope names the one scope that makes a conversation able to write. It is exported
// for that anti-drift assertion: without it, renaming the scope in internal/mcpsrv would
// silently turn every console conversation read-only.
func WritableScope() string { return scopeAdmin }

// WriteModeForScope exposes writeModeFor to the same assertion.
func WriteModeForScope(scope string) string { return writeModeFor(scope) }

// writeModeFor derives the session's displayed write mode from the bound token's scope. The
// token stays the authority; this field exists so the console can label a conversation and so
// code holding only the session row can still tell read-only from writable. Anything that is
// not an admin scope reads as read-only, so an unknown or empty scope can never widen access.
func writeModeFor(scope string) string {
	if strings.TrimSpace(scope) == scopeAdmin {
		return domain.ChatWriteModeAllow
	}
	return domain.ChatWriteModeReadOnly
}

// CreateSession opens a conversation owned by the calling administrator. Only an admin may
// bind a billing key, because binding is what makes the conversation able to spend money.
func (s *Service) CreateSession(ctx context.Context, ownerID int64, username, role string, in SessionInput) (*domain.ChatSession, error) {
	if ownerID <= 0 {
		return nil, domain.ErrUnauthorized("chat requires an authenticated administrator session")
	}
	if err := requireAdmin(role, "start a billed conversation"); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return nil, domain.ErrInvalidRequest("choose a model for this conversation")
	}
	if in.AccountID <= 0 || in.APIKeyID <= 0 {
		return nil, domain.ErrInvalidRequest("choose the account and API key that will be billed for this conversation")
	}
	// An admin-scope token is an administrator credential, so binding one is an
	// administrator act. Every caller today is already an admin; this is the anchor that
	// keeps that true if the console ever admits another role.
	token, err := s.resolveMCPToken(ctx, in.MCPTokenID)
	if err != nil {
		return nil, err
	}
	if writeModeFor(token.Scope) == domain.ChatWriteModeAllow && role != RoleAdmin {
		return nil, domain.ErrForbidden("only an administrator may bind an admin-scope MCP token")
	}
	skills, err := s.normalizeSkillIDs(ctx, ownerID, in.SkillIDs)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tokenID := token.ID
	session := &domain.ChatSession{
		ID:          ids.ChatSession(),
		OwnerUserID: ownerID,
		OwnerName:   username,
		Model:       model,
		AccountID:   in.AccountID,
		APIKeyID:    in.APIKeyID,
		WriteMode:   writeModeFor(token.Scope),
		MCPTokenID:  &tokenID,
		SkillIDs:    skills,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if in.Title != nil {
		session.Title = truncateRunes(strings.TrimSpace(*in.Title), maxTitleRunes)
	}
	if err := s.store.CreateChatSession(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

// UpdateSession edits one conversation. Renaming is the owner's business; changing what
// the conversation is allowed to spend or do is an administrator's.
func (s *Service) UpdateSession(ctx context.Context, ownerID int64, role, id string, in SessionInput) (*domain.ChatSession, error) {
	session, err := s.store.GetChatSession(ctx, id, ownerID)
	if err != nil {
		return nil, err
	}
	bindingChanged := false
	if strings.TrimSpace(in.Model) != "" && in.Model != session.Model {
		bindingChanged = true
		session.Model = strings.TrimSpace(in.Model)
	}
	if in.AccountID > 0 && in.AccountID != session.AccountID {
		bindingChanged = true
		session.AccountID = in.AccountID
	}
	if in.APIKeyID > 0 && in.APIKeyID != session.APIKeyID {
		bindingChanged = true
		session.APIKeyID = in.APIKeyID
	}
	if in.MCPTokenID > 0 {
		token, err := s.resolveMCPToken(ctx, in.MCPTokenID)
		if err != nil {
			return nil, err
		}
		if writeModeFor(token.Scope) == domain.ChatWriteModeAllow && role != RoleAdmin {
			return nil, domain.ErrForbidden("only an administrator may bind an admin-scope MCP token")
		}
		if session.MCPTokenID == nil || *session.MCPTokenID != token.ID {
			bindingChanged = true
			tokenID := token.ID
			session.MCPTokenID = &tokenID
			session.WriteMode = writeModeFor(token.Scope)
		}
	}
	if in.SkillIDs != nil {
		skills, err := s.normalizeSkillIDs(ctx, ownerID, in.SkillIDs)
		if err != nil {
			return nil, err
		}
		session.SkillIDs = skills
	}
	if bindingChanged {
		if err := requireAdmin(role, "change the model, billing key or MCP token of a conversation"); err != nil {
			return nil, err
		}
	}
	if in.Title != nil {
		session.Title = truncateRunes(strings.TrimSpace(*in.Title), maxTitleRunes)
	}
	if session.AccountID <= 0 || session.APIKeyID <= 0 {
		return nil, domain.ErrInvalidRequest("the conversation needs an account and API key before it can be billed")
	}
	if err := s.store.UpdateChatSession(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

// ListSessions pages through one administrator's conversations.
func (s *Service) ListSessions(ctx context.Context, ownerID int64, limit, offset int) ([]*domain.ChatSession, int, error) {
	if ownerID <= 0 {
		return []*domain.ChatSession{}, 0, nil
	}
	return s.store.ListChatSessions(ctx, ownerID, limit, offset)
}

// SessionDetail is one conversation with everything the console renders.
type SessionDetail struct {
	Session   *domain.ChatSession
	Messages  []*domain.ChatMessage
	ToolCalls []*domain.ChatToolCall
	Skills    []*domain.ChatSkill
	Usage     map[string]*domain.RequestUsage
}

// Session loads one conversation for its owner, together with the live cost of every model
// step it ran. Cost is joined from the usage records rather than copied into chat tables:
// money facts have exactly one owner in this gateway.
func (s *Service) Session(ctx context.Context, ownerID int64, id string, usage UsageRecordReader) (*SessionDetail, error) {
	session, err := s.store.GetChatSession(ctx, id, ownerID)
	if err != nil {
		return nil, err
	}
	messages, err := s.store.ListChatMessages(ctx, session.ID, 0)
	if err != nil {
		return nil, err
	}
	toolCalls, err := s.store.ListChatToolCalls(ctx, session.ID, 100)
	if err != nil {
		return nil, err
	}
	skills, err := s.loadedSkills(ctx, ownerID, session.SkillIDs)
	if err != nil {
		return nil, err
	}
	detail := &SessionDetail{Session: session, Messages: messages, ToolCalls: toolCalls, Skills: skills}
	if usage != nil {
		ids := make([]string, 0, len(messages))
		for _, m := range messages {
			ids = append(ids, m.RequestIDs...)
		}
		if len(ids) > 0 {
			rows, err := usage.RequestUsages(ctx, ids)
			if err != nil {
				// Cost is decoration on top of a conversation that loaded fine; a failed
				// join must not turn the page into an error.
				s.log.Warn("chat usage join failed", "err", err, "session", session.ID)
			} else {
				detail.Usage = rows
			}
		}
	}
	return detail, nil
}

// DeleteSession removes one conversation and everything that belonged to it.
func (s *Service) DeleteSession(ctx context.Context, ownerID int64, id string) error {
	if s.isBusy(id) {
		return domain.ErrConflict("this conversation is answering a question right now")
	}
	return s.store.DeleteChatSession(ctx, id, ownerID)
}

// SkillInput is the editable state of one skill.
type SkillInput struct {
	Name         string
	Description  string
	Instructions string
}

// SkillDraft is a generated-but-unsaved skill proposal.
type SkillDraft struct {
	Name         string
	Description  string
	Instructions string
	// Note explains a degraded outcome (the model's answer could not be parsed, so a
	// skeleton was returned) instead of pretending the draft is model-authored.
	Note      string
	RequestID string
	Model     string
	Usage     Usage
}

// ListSkills pages through one administrator's private skills.
func (s *Service) ListSkills(ctx context.Context, ownerID int64, limit, offset int) ([]*domain.ChatSkill, int, error) {
	if ownerID <= 0 {
		return []*domain.ChatSkill{}, 0, nil
	}
	return s.store.ListChatSkills(ctx, ownerID, limit, offset)
}

// CreateSkill stores a private skill, either handwritten or saved from a draft.
func (s *Service) CreateSkill(ctx context.Context, ownerID int64, in SkillInput, sourceSessionID, sourceModel string) (*domain.ChatSkill, error) {
	if ownerID <= 0 {
		return nil, domain.ErrUnauthorized("chat skills require an authenticated administrator session")
	}
	skill, err := s.validateSkill(ownerID, in)
	if err != nil {
		return nil, err
	}
	skill.SourceSessionID = sourceSessionID
	skill.SourceModel = sourceModel
	if _, err := s.store.CreateChatSkill(ctx, skill); err != nil {
		return nil, err
	}
	return skill, nil
}

// UpdateSkill rewrites one owned skill.
func (s *Service) UpdateSkill(ctx context.Context, ownerID, id int64, in SkillInput) (*domain.ChatSkill, error) {
	existing, err := s.store.GetChatSkill(ctx, id, ownerID)
	if err != nil {
		return nil, err
	}
	updated, err := s.validateSkill(ownerID, in)
	if err != nil {
		return nil, err
	}
	existing.Name = updated.Name
	existing.Description = updated.Description
	existing.Instructions = updated.Instructions
	if err := s.store.UpdateChatSkill(ctx, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// DeleteSkill removes one owned skill.
func (s *Service) DeleteSkill(ctx context.Context, ownerID, id int64) error {
	return s.store.DeleteChatSkill(ctx, id, ownerID)
}

func (s *Service) validateSkill(ownerID int64, in SkillInput) (*domain.ChatSkill, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, domain.ErrInvalidRequest("give the skill a name")
	}
	if len([]rune(name)) > maxSkillNameRunes {
		return nil, domain.ErrInvalidRequest(fmt.Sprintf("the skill name must be at most %d characters", maxSkillNameRunes))
	}
	description := strings.TrimSpace(in.Description)
	if len([]rune(description)) > maxSkillDescRunes {
		return nil, domain.ErrInvalidRequest(fmt.Sprintf("the skill description must be at most %d characters", maxSkillDescRunes))
	}
	instructions := strings.TrimSpace(in.Instructions)
	if instructions == "" {
		return nil, domain.ErrInvalidRequest("a skill needs instructions the model can follow")
	}
	if len(instructions) > s.cfg.MaxSkillBytes {
		return nil, domain.ErrInvalidRequest(fmt.Sprintf("the skill instructions must be at most %d bytes", s.cfg.MaxSkillBytes))
	}
	return &domain.ChatSkill{
		OwnerUserID:  ownerID,
		Name:         name,
		Description:  description,
		Instructions: instructions,
	}, nil
}

// normalizeSkillIDs keeps only skills the owner actually has, preserving order and
// dropping duplicates. A skill deleted after it was attached to a conversation simply
// stops being loaded — it is not an error the user has to fix.
func (s *Service) normalizeSkillIDs(ctx context.Context, ownerID int64, raw []int64) ([]int64, error) {
	if len(raw) == 0 {
		return []int64{}, nil
	}
	if s.cfg.MaxLoadedSkills > 0 && len(raw) > s.cfg.MaxLoadedSkills {
		return nil, domain.ErrInvalidRequest(fmt.Sprintf("a conversation can load at most %d skills", s.cfg.MaxLoadedSkills))
	}
	out := make([]int64, 0, len(raw))
	seen := map[int64]bool{}
	for _, id := range raw {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := s.store.GetChatSkill(ctx, id, ownerID); err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// loadedSkills resolves the skills attached to a conversation.
func (s *Service) loadedSkills(ctx context.Context, ownerID int64, skillIDs []int64) ([]*domain.ChatSkill, error) {
	out := make([]*domain.ChatSkill, 0, len(skillIDs))
	for _, id := range skillIDs {
		skill, err := s.store.GetChatSkill(ctx, id, ownerID)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, skill)
	}
	return out, nil
}

func requireAdmin(role, action string) error {
	if role == RoleAdmin {
		return nil
	}
	return domain.ErrForbidden("only an administrator may " + action + "; the gateway bills the account behind the selected API key, and a viewer role cannot spend it")
}

func (s *Service) isBusy(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[sessionID]
}

func (s *Service) setBusy(sessionID string, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if busy {
		s.busy[sessionID] = true
		return
	}
	delete(s.busy, sessionID)
}

func isNotFound(err error) bool {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 404
	}
	return false
}

func isConflict(err error) bool {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 409
	}
	return false
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// titleFromQuestion derives a conversation title from its first question.
func titleFromQuestion(question string) string {
	line := strings.TrimSpace(question)
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	return truncateRunes(line, maxTitleRunes)
}

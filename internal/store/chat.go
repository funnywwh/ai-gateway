package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// The console chat is the only place in the gateway where the *administrator account*
// (admin_users) owns rows. Every read below therefore filters on owner_user_id in SQL and
// there is deliberately no "owner <= 0 means all" escape hatch: a zero owner matches
// nothing, so a caller that forgets to pass the logged-in id gets an empty result rather
// than another administrator's data.

const chatSessionCols = `id, owner_user_id, owner_username, title, model, account_id, api_key_id,
	write_mode, mcp_token_id, skill_ids_json, web_access, status, message_count, tokens_in, tokens_out,
	tokens_reasoning, last_message_at, created_at, updated_at`

func scanChatSession(row rowScanner) (*domain.ChatSession, error) {
	var (
		s          domain.ChatSession
		skillsJSON string
		mcpTokenID sql.NullInt64
		lastMsg    sql.NullInt64
		createdAt  int64
		updatedAt  int64
	)
	if err := row.Scan(&s.ID, &s.OwnerUserID, &s.OwnerName, &s.Title, &s.Model, &s.AccountID,
		&s.APIKeyID, &s.WriteMode, &mcpTokenID, &skillsJSON, &s.WebAccess, &s.Status, &s.MessageCount, &s.TokensIn,
		&s.TokensOut, &s.Reasoning, &lastMsg, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	s.MCPTokenID = nullInt64Ptr(mcpTokenID)
	s.SkillIDs = decodeInt64List(skillsJSON)
	s.LastMessage = timePtrFromNull(lastMsg)
	s.CreatedAt = timeFromUnix(createdAt)
	s.UpdatedAt = timeFromUnix(updatedAt)
	return &s, nil
}

// CreateChatSession inserts one conversation owned by owner_user_id.
func (db *DB) CreateChatSession(ctx context.Context, s *domain.ChatSession) error {
	if s == nil || s.ID == "" {
		return domain.ErrInvalidRequest("chat session requires an id")
	}
	if s.OwnerUserID <= 0 {
		return domain.ErrInvalidRequest("chat session requires an owner")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.UpdatedAt = s.CreatedAt
	if s.Status == "" {
		s.Status = "active"
	}
	if s.WriteMode == "" {
		s.WriteMode = domain.ChatWriteModeReadOnly
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO chat_sessions(id, owner_user_id, owner_username, title, model, account_id, api_key_id,
  write_mode, mcp_token_id, skill_ids_json, web_access, status, message_count, tokens_in, tokens_out,
  tokens_reasoning, last_message_at, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.OwnerUserID, s.OwnerName, s.Title, s.Model, s.AccountID, s.APIKeyID,
		s.WriteMode, int64PtrNull(s.MCPTokenID), encodeInt64List(s.SkillIDs), s.WebAccess, s.Status, s.MessageCount,
		s.TokensIn, s.TokensOut, s.Reasoning, unixPtr(s.LastMessage), unix(s.CreatedAt), unix(s.UpdatedAt)); err != nil {
		return fmt.Errorf("store: create chat session: %w", err)
	}
	return nil
}

// GetChatSession loads one conversation, but only for its owner.
func (db *DB) GetChatSession(ctx context.Context, id string, ownerUserID int64) (*domain.ChatSession, error) {
	if id == "" || ownerUserID <= 0 {
		return nil, domain.ErrNotFound("chat session")
	}
	row := db.read.QueryRowContext(ctx,
		"SELECT "+chatSessionCols+" FROM chat_sessions WHERE id = ? AND owner_user_id = ?", id, ownerUserID)
	s, err := scanChatSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("chat session " + id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat session: %w", err)
	}
	return s, nil
}

// ListChatSessions returns one owner's conversations, newest activity first.
func (db *DB) ListChatSessions(ctx context.Context, ownerUserID int64, limit, offset int) ([]*domain.ChatSession, int, error) {
	if ownerUserID <= 0 {
		return []*domain.ChatSession{}, 0, nil
	}
	limit = normalizeLimit(limit, 20, 100)
	where := " WHERE owner_user_id = ?"
	args := []any{ownerUserID}
	total, err := db.countRows(ctx, "chat_sessions", where, args, "chat sessions")
	if err != nil {
		return nil, 0, err
	}
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+chatSessionCols+" FROM chat_sessions"+where+" ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?",
		ownerUserID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list chat sessions: %w", err)
	}
	defer rows.Close()
	out := []*domain.ChatSession{}
	for rows.Next() {
		s, err := scanChatSession(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan chat session: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate chat sessions: %w", err)
	}
	return out, total, nil
}

// UpdateChatSession rewrites the editable fields of one conversation. The owner is part
// of the WHERE clause so a stale in-memory copy cannot write across accounts.
func (db *DB) UpdateChatSession(ctx context.Context, s *domain.ChatSession) error {
	if s == nil || s.ID == "" || s.OwnerUserID <= 0 {
		return domain.ErrInvalidRequest("chat session update requires an id and owner")
	}
	res, err := db.write.ExecContext(ctx, `
UPDATE chat_sessions SET title = ?, model = ?, account_id = ?, api_key_id = ?, write_mode = ?,
  mcp_token_id = ?, skill_ids_json = ?, web_access = ?, status = ?, updated_at = ?
WHERE id = ? AND owner_user_id = ?`,
		s.Title, s.Model, s.AccountID, s.APIKeyID, s.WriteMode, int64PtrNull(s.MCPTokenID),
		encodeInt64List(s.SkillIDs), s.WebAccess, s.Status, unix(time.Now()), s.ID, s.OwnerUserID)
	if err != nil {
		return fmt.Errorf("store: update chat session: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return domain.ErrNotFound("chat session " + s.ID)
	}
	return nil
}

// UpdateChatSessionAggregates folds one finished turn into the session's running totals.
// It is an increment, not a rewrite, so two turns finishing close together cannot lose
// each other's tokens.
func (db *DB) UpdateChatSessionAggregates(ctx context.Context, id string, ownerUserID int64, messages, tokensIn, tokensOut, tokensReasoning int, lastMessage time.Time, title string) error {
	if id == "" || ownerUserID <= 0 {
		return domain.ErrInvalidRequest("chat session aggregate update requires an id and owner")
	}
	now := unix(time.Now())
	// An empty title is only filled in once (the first question names the conversation);
	// a later rename by the user must never be overwritten by a subsequent turn.
	if strings.TrimSpace(title) != "" {
		if _, err := db.write.ExecContext(ctx,
			"UPDATE chat_sessions SET title = ? WHERE id = ? AND owner_user_id = ? AND title = ''",
			title, id, ownerUserID); err != nil {
			return fmt.Errorf("store: title chat session: %w", err)
		}
	}
	if _, err := db.write.ExecContext(ctx, `
UPDATE chat_sessions SET message_count = message_count + ?, tokens_in = tokens_in + ?,
  tokens_out = tokens_out + ?, tokens_reasoning = tokens_reasoning + ?,
  last_message_at = ?, updated_at = ?
WHERE id = ? AND owner_user_id = ?`,
		messages, tokensIn, tokensOut, tokensReasoning, unix(lastMessage), now, id, ownerUserID); err != nil {
		return fmt.Errorf("store: update chat session aggregates: %w", err)
	}
	return nil
}

// DeleteChatSession removes a conversation. Messages, turns, tool calls and artifacts go
// with it through ON DELETE CASCADE (foreign_keys is enabled on every pooled connection).
func (db *DB) DeleteChatSession(ctx context.Context, id string, ownerUserID int64) error {
	if id == "" || ownerUserID <= 0 {
		return domain.ErrNotFound("chat session")
	}
	res, err := db.write.ExecContext(ctx, "DELETE FROM chat_sessions WHERE id = ? AND owner_user_id = ?", id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: delete chat session: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return domain.ErrNotFound("chat session " + id)
	}
	return nil
}

const chatTurnCols = `id, session_id, turn_id, status, error, request_ids_json, created_at, updated_at`

func scanChatTurn(row rowScanner) (*domain.ChatTurn, error) {
	var (
		t         domain.ChatTurn
		idsJSON   string
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&t.ID, &t.SessionID, &t.TurnID, &t.Status, &t.Error, &idsJSON, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t.RequestIDs = decodeStringList(idsJSON)
	t.CreatedAt = timeFromUnix(createdAt)
	t.UpdatedAt = timeFromUnix(updatedAt)
	return &t, nil
}

// CreateChatTurn opens one turn. The unique (session_id, turn_id) index makes the client's
// idempotency key authoritative: a duplicate insert loses the race and is reported as a
// conflict for the caller to resolve by reading the existing row.
func (db *DB) CreateChatTurn(ctx context.Context, t *domain.ChatTurn) error {
	if t == nil || t.ID == "" || t.SessionID == "" || t.TurnID == "" {
		return domain.ErrInvalidRequest("chat turn requires an id, session and turn id")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	t.UpdatedAt = t.CreatedAt
	if t.Status == "" {
		t.Status = domain.ChatTurnRunning
	}
	_, err := db.write.ExecContext(ctx, `
INSERT INTO chat_turns(id, session_id, turn_id, status, error, request_ids_json, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.SessionID, t.TurnID, t.Status, t.Error, encodeStringList(t.RequestIDs),
		unix(t.CreatedAt), unix(t.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("this turn was already submitted")
		}
		return fmt.Errorf("store: create chat turn: %w", err)
	}
	return nil
}

// GetChatTurn loads a turn by its client idempotency key.
func (db *DB) GetChatTurn(ctx context.Context, sessionID, turnID string) (*domain.ChatTurn, error) {
	if sessionID == "" || turnID == "" {
		return nil, domain.ErrNotFound("chat turn")
	}
	row := db.read.QueryRowContext(ctx,
		"SELECT "+chatTurnCols+" FROM chat_turns WHERE session_id = ? AND turn_id = ?", sessionID, turnID)
	t, err := scanChatTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("chat turn")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat turn: %w", err)
	}
	return t, nil
}

// SetChatTurnStatus closes (or updates) a turn.
func (db *DB) SetChatTurnStatus(ctx context.Context, id, status, errMsg string, requestIDs []string) error {
	if id == "" {
		return domain.ErrInvalidRequest("chat turn update requires an id")
	}
	if _, err := db.write.ExecContext(ctx, `
UPDATE chat_turns SET status = ?, error = ?, request_ids_json = ?, updated_at = ?
WHERE id = ?`, status, errMsg, encodeStringList(requestIDs), unix(time.Now()), id); err != nil {
		return fmt.Errorf("store: set chat turn status: %w", err)
	}
	return nil
}

// InterruptRunningChatTurns marks turns that were still running when the process started.
// Their tool calls may or may not have reached the gateway, which is exactly why they are
// reported as interrupted instead of being retried automatically.
func (db *DB) InterruptRunningChatTurns(ctx context.Context) (int64, error) {
	now := unix(time.Now())
	res, err := db.write.ExecContext(ctx,
		"UPDATE chat_turns SET status = ?, updated_at = ? WHERE status = ?",
		domain.ChatTurnInterrupted, now, domain.ChatTurnRunning)
	if err != nil {
		return 0, fmt.Errorf("store: interrupt chat turns: %w", err)
	}
	// A pending tool call belongs to an interrupted turn: record that its outcome is
	// unknown rather than leaving it looking merely unfinished.
	if _, err := db.write.ExecContext(ctx,
		"UPDATE chat_tool_calls SET status = ?, updated_at = ? WHERE status = ?",
		domain.ChatToolUnknown, now, domain.ChatToolPending); err != nil {
		return 0, fmt.Errorf("store: mark chat tool calls unknown: %w", err)
	}
	affected, _ := res.RowsAffected()
	return affected, nil
}

const chatMessageCols = `id, session_id, turn_id, seq, role, content, parts_json, provider_items_json,
	reasoning, status, truncated, error, model, resolved_model, provider, request_ids_json,
	tokens_in, tokens_out, tokens_reasoning, created_at`

func scanChatMessage(row rowScanner) (*domain.ChatMessage, error) {
	var (
		m          domain.ChatMessage
		partsJSON  string
		idsJSON    string
		truncated  int
		createdAt  int64
		providerIx string
	)
	if err := row.Scan(&m.ID, &m.SessionID, &m.TurnID, &m.Seq, &m.Role, &m.Content, &partsJSON,
		&providerIx, &m.Reasoning, &m.Status, &truncated, &m.Error, &m.Model, &m.ResolvedModel,
		&m.Provider, &idsJSON, &m.TokensIn, &m.TokensOut, &m.ReasoningTok, &createdAt); err != nil {
		return nil, err
	}
	m.Parts = decodeParts(partsJSON)
	m.ProviderItems = providerIx
	m.Truncated = truncated != 0
	m.RequestIDs = decodeStringList(idsJSON)
	m.CreatedAt = timeFromUnix(createdAt)
	return &m, nil
}

// AppendChatMessage stores one message, assigning its sequence number inside the write
// transaction so two turns of the same session can never claim the same seq.
func (db *DB) AppendChatMessage(ctx context.Context, m *domain.ChatMessage) error {
	if m == nil || m.ID == "" || m.SessionID == "" {
		return domain.ErrInvalidRequest("chat message requires an id and session")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.Status == "" {
		m.Status = domain.ChatMessageOK
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin chat message: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var next int
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0) + 1 FROM chat_messages WHERE session_id = ?", m.SessionID).Scan(&next); err != nil {
		return fmt.Errorf("store: next chat message seq: %w", err)
	}
	m.Seq = next
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_messages(id, session_id, turn_id, seq, role, content, parts_json, provider_items_json,
  reasoning, status, truncated, error, model, resolved_model, provider, request_ids_json,
  tokens_in, tokens_out, tokens_reasoning, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.SessionID, m.TurnID, m.Seq, m.Role, m.Content, encodeParts(m.Parts),
		m.ProviderItems, m.Reasoning, m.Status, boolInt(m.Truncated), m.Error, m.Model,
		m.ResolvedModel, m.Provider, encodeStringList(m.RequestIDs), m.TokensIn, m.TokensOut,
		m.ReasoningTok, unix(m.CreatedAt)); err != nil {
		return fmt.Errorf("store: append chat message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit chat message: %w", err)
	}
	return nil
}

// ListChatMessages returns a conversation's messages in reading order. newest limits the
// read to the tail of a long conversation (0 = all); the tail is then reversed so callers
// always receive ascending seq.
func (db *DB) ListChatMessages(ctx context.Context, sessionID string, newest int) ([]*domain.ChatMessage, error) {
	if sessionID == "" {
		return []*domain.ChatMessage{}, nil
	}
	query := "SELECT " + chatMessageCols + " FROM chat_messages WHERE session_id = ? ORDER BY seq DESC"
	args := []any{sessionID}
	if newest > 0 {
		query += " LIMIT ?"
		args = append(args, newest)
	}
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list chat messages: %w", err)
	}
	defer rows.Close()
	out := []*domain.ChatMessage{}
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan chat message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate chat messages: %w", err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListChatTurnMessages returns the messages of one turn in reading order. It exists for
// the idempotent replay path: a resent turn must hand back exactly what it produced.
func (db *DB) ListChatTurnMessages(ctx context.Context, sessionID, turnID string) ([]*domain.ChatMessage, error) {
	if sessionID == "" || turnID == "" {
		return []*domain.ChatMessage{}, nil
	}
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+chatMessageCols+" FROM chat_messages WHERE session_id = ? AND turn_id = ? ORDER BY seq ASC",
		sessionID, turnID)
	if err != nil {
		return nil, fmt.Errorf("store: list chat turn messages: %w", err)
	}
	defer rows.Close()
	out := []*domain.ChatMessage{}
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan chat turn message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate chat turn messages: %w", err)
	}
	return out, nil
}

// AdminSessionUser returns the administrator behind a still-valid console session.
// Preview tickets are bound to a session so logging out revokes them, and this is the check
// that makes that promise true.
func (db *DB) AdminSessionUser(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, domain.ErrUnauthorized("missing admin session")
	}
	user, _, err := db.GetAdminSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	return user.ID, nil
}

// AdminRole reads one administrator's role by id. The console chat re-checks it before
// every tool call rather than trusting the role it saw when the turn started.
func (db *DB) AdminRole(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", domain.ErrNotFound("admin user")
	}
	var role string
	if err := db.read.QueryRowContext(ctx, "SELECT role FROM admin_users WHERE id = ?", userID).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", domain.ErrNotFound(fmt.Sprintf("admin user %d", userID))
		}
		return "", fmt.Errorf("store: read admin role: %w", err)
	}
	return role, nil
}

// GetChatMessage loads one message, still filtered by session so a message id alone can
// never cross conversations.
func (db *DB) GetChatMessage(ctx context.Context, sessionID, id string) (*domain.ChatMessage, error) {
	if sessionID == "" || id == "" {
		return nil, domain.ErrNotFound("chat message")
	}
	row := db.read.QueryRowContext(ctx,
		"SELECT "+chatMessageCols+" FROM chat_messages WHERE session_id = ? AND id = ?", sessionID, id)
	m, err := scanChatMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("chat message " + id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat message: %w", err)
	}
	return m, nil
}

// CreateChatToolCall records a tool call as pending *before* it runs.
func (db *DB) CreateChatToolCall(ctx context.Context, c *domain.ChatToolCall) error {
	if c == nil || c.ID == "" || c.SessionID == "" {
		return domain.ErrInvalidRequest("chat tool call requires an id and session")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.UpdatedAt = c.CreatedAt
	if c.Status == "" {
		c.Status = domain.ChatToolPending
	}
	_, err := db.write.ExecContext(ctx, `
INSERT INTO chat_tool_calls(id, session_id, turn_id, step, call_id, name, arguments, result,
  is_error, status, duration_ms, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.SessionID, c.TurnID, c.Step, c.CallID, c.Name, c.Arguments, c.Result,
		boolInt(c.IsError), c.Status, c.DurationMS, unix(c.CreatedAt), unix(c.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("this tool call was already executed")
		}
		return fmt.Errorf("store: create chat tool call: %w", err)
	}
	return nil
}

// FinishChatToolCall stores the outcome of one tool call.
func (db *DB) FinishChatToolCall(ctx context.Context, id, result string, isError bool, status string, durationMS int) error {
	if id == "" {
		return domain.ErrInvalidRequest("chat tool call update requires an id")
	}
	if status == "" {
		if isError {
			status = domain.ChatToolFailed
		} else {
			status = domain.ChatToolDone
		}
	}
	if _, err := db.write.ExecContext(ctx, `
UPDATE chat_tool_calls SET result = ?, is_error = ?, status = ?, duration_ms = ?, updated_at = ?
WHERE id = ?`, result, boolInt(isError), status, durationMS, unix(time.Now()), id); err != nil {
		return fmt.Errorf("store: finish chat tool call: %w", err)
	}
	return nil
}

// ListChatToolCalls lists a conversation's tool calls, newest last.
func (db *DB) ListChatToolCalls(ctx context.Context, sessionID string, limit int) ([]*domain.ChatToolCall, error) {
	if sessionID == "" {
		return []*domain.ChatToolCall{}, nil
	}
	rows, err := db.read.QueryContext(ctx, `
SELECT id, session_id, turn_id, step, call_id, name, arguments, result, is_error, status,
  duration_ms, created_at, updated_at
FROM chat_tool_calls WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		sessionID, normalizeLimit(limit, 50, 200))
	if err != nil {
		return nil, fmt.Errorf("store: list chat tool calls: %w", err)
	}
	defer rows.Close()
	out := []*domain.ChatToolCall{}
	for rows.Next() {
		var (
			c         domain.ChatToolCall
			isErr     int
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(&c.ID, &c.SessionID, &c.TurnID, &c.Step, &c.CallID, &c.Name,
			&c.Arguments, &c.Result, &isErr, &c.Status, &c.DurationMS, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan chat tool call: %w", err)
		}
		c.IsError = isErr != 0
		c.CreatedAt = timeFromUnix(createdAt)
		c.UpdatedAt = timeFromUnix(updatedAt)
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate chat tool calls: %w", err)
	}
	return out, nil
}

const chatSkillCols = `id, owner_user_id, name, description, instructions, source_session_id,
	source_model, created_at, updated_at`

func scanChatSkill(row rowScanner) (*domain.ChatSkill, error) {
	var (
		s         domain.ChatSkill
		createdAt int64
		updatedAt int64
	)
	if err := row.Scan(&s.ID, &s.OwnerUserID, &s.Name, &s.Description, &s.Instructions,
		&s.SourceSessionID, &s.SourceModel, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	s.CreatedAt = timeFromUnix(createdAt)
	s.UpdatedAt = timeFromUnix(updatedAt)
	return &s, nil
}

// CreateChatSkill stores one private skill.
func (db *DB) CreateChatSkill(ctx context.Context, s *domain.ChatSkill) (int64, error) {
	if s == nil || s.OwnerUserID <= 0 {
		return 0, domain.ErrInvalidRequest("chat skill requires an owner")
	}
	if strings.TrimSpace(s.Name) == "" {
		return 0, domain.ErrInvalidRequest("chat skill requires a name")
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	res, err := db.write.ExecContext(ctx, `
INSERT INTO chat_skills(owner_user_id, name, description, instructions, source_session_id,
  source_model, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
		s.OwnerUserID, s.Name, s.Description, s.Instructions, s.SourceSessionID,
		s.SourceModel, unix(s.CreatedAt), unix(s.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return 0, domain.ErrConflict("a skill named " + s.Name + " already exists")
		}
		return 0, fmt.Errorf("store: create chat skill: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: chat skill id: %w", err)
	}
	s.ID = id
	return id, nil
}

// GetChatSkill loads one skill for its owner only.
func (db *DB) GetChatSkill(ctx context.Context, id, ownerUserID int64) (*domain.ChatSkill, error) {
	if id <= 0 || ownerUserID <= 0 {
		return nil, domain.ErrNotFound("chat skill")
	}
	row := db.read.QueryRowContext(ctx,
		"SELECT "+chatSkillCols+" FROM chat_skills WHERE id = ? AND owner_user_id = ?", id, ownerUserID)
	s, err := scanChatSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("chat skill %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat skill: %w", err)
	}
	return s, nil
}

// ListChatSkills pages through one owner's skills, most recently updated first.
func (db *DB) ListChatSkills(ctx context.Context, ownerUserID int64, limit, offset int) ([]*domain.ChatSkill, int, error) {
	if ownerUserID <= 0 {
		return []*domain.ChatSkill{}, 0, nil
	}
	limit = normalizeLimit(limit, 20, 100)
	where := " WHERE owner_user_id = ?"
	args := []any{ownerUserID}
	total, err := db.countRows(ctx, "chat_skills", where, args, "chat skills")
	if err != nil {
		return nil, 0, err
	}
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+chatSkillCols+" FROM chat_skills"+where+" ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?",
		ownerUserID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list chat skills: %w", err)
	}
	defer rows.Close()
	out := []*domain.ChatSkill{}
	for rows.Next() {
		s, err := scanChatSkill(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan chat skill: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate chat skills: %w", err)
	}
	return out, total, nil
}

// UpdateChatSkill rewrites one owned skill.
func (db *DB) UpdateChatSkill(ctx context.Context, s *domain.ChatSkill) error {
	if s == nil || s.ID <= 0 || s.OwnerUserID <= 0 {
		return domain.ErrInvalidRequest("chat skill update requires an id and owner")
	}
	if strings.TrimSpace(s.Name) == "" {
		return domain.ErrInvalidRequest("chat skill requires a name")
	}
	res, err := db.write.ExecContext(ctx, `
UPDATE chat_skills SET name = ?, description = ?, instructions = ?, updated_at = ?
WHERE id = ? AND owner_user_id = ?`,
		s.Name, s.Description, s.Instructions, unix(time.Now()), s.ID, s.OwnerUserID)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("a skill named " + s.Name + " already exists")
		}
		return fmt.Errorf("store: update chat skill: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return domain.ErrNotFound(fmt.Sprintf("chat skill %d", s.ID))
	}
	return nil
}

// DeleteChatSkill removes one owned skill.
func (db *DB) DeleteChatSkill(ctx context.Context, id, ownerUserID int64) error {
	if id <= 0 || ownerUserID <= 0 {
		return domain.ErrNotFound("chat skill")
	}
	res, err := db.write.ExecContext(ctx,
		"DELETE FROM chat_skills WHERE id = ? AND owner_user_id = ?", id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: delete chat skill: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return domain.ErrNotFound(fmt.Sprintf("chat skill %d", id))
	}
	return nil
}

// 注意：表上有一列 bridge_token（迁移 0012 留下的占位），代码不读写它——可交互预览
// 现在不需要凭证。见该迁移文件的说明。
const chatArtifactCols = `id, owner_user_id, session_id, key, title, format, body, size_bytes, created_at`

func scanChatArtifact(row rowScanner) (*domain.ChatArtifact, error) {
	var (
		a         domain.ChatArtifact
		createdAt int64
	)
	if err := row.Scan(&a.ID, &a.OwnerUserID, &a.SessionID, &a.Key, &a.Title, &a.Format,
		&a.Body, &a.SizeBytes, &createdAt); err != nil {
		return nil, err
	}
	a.CreatedAt = timeFromUnix(createdAt)
	return &a, nil
}

// UpsertChatArtifact stores a preview payload. (session_id, key) is the identity: the
// console re-previews the same code block with the same key, so this replaces instead of
// accumulating copies.
//
// The conflicting branch deliberately does not overwrite `id` — the row keeps the id it was
// first stored under, so a URL handed out for that block stays the URL for that block. That
// makes the caller's `a.ID` stale whenever it generated a fresh one, so it is replaced here
// with the id that actually identifies the row. Returning silently and letting the caller
// keep its own id is what produced a preview URL that 404'd: the console built `url` from the
// id it sent, while the row (and the ticket the handler then signed) carried the old one.
func (db *DB) UpsertChatArtifact(ctx context.Context, a *domain.ChatArtifact) error {
	if a == nil || a.ID == "" || a.SessionID == "" || a.OwnerUserID <= 0 {
		return domain.ErrInvalidRequest("chat artifact requires an id, session and owner")
	}
	if a.Format == "" {
		a.Format = domain.ChatArtifactHTML
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	// RETURNING makes the identity decision the database's, in one statement: on insert it is
	// the row we just wrote, on conflict it is the row that was already there.
	var storedID string
	err := db.write.QueryRowContext(ctx, `
INSERT INTO chat_artifacts(id, owner_user_id, session_id, key, title, format, body, size_bytes, created_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_id, key) DO UPDATE SET
  owner_user_id = excluded.owner_user_id,
  title = excluded.title,
  format = excluded.format,
  body = excluded.body,
  size_bytes = excluded.size_bytes,
  created_at = excluded.created_at
RETURNING id`,
		a.ID, a.OwnerUserID, a.SessionID, a.Key, a.Title, a.Format, a.Body, a.SizeBytes, unix(a.CreatedAt)).Scan(&storedID)
	if err != nil {
		return fmt.Errorf("store: upsert chat artifact: %w", err)
	}
	if storedID != "" {
		a.ID = storedID
	}
	return nil
}

// GetChatArtifact loads one preview payload for its owner.
func (db *DB) GetChatArtifact(ctx context.Context, id string, ownerUserID int64) (*domain.ChatArtifact, error) {
	if id == "" || ownerUserID <= 0 {
		return nil, domain.ErrNotFound("chat artifact")
	}
	row := db.read.QueryRowContext(ctx,
		"SELECT "+chatArtifactCols+" FROM chat_artifacts WHERE id = ? AND owner_user_id = ?", id, ownerUserID)
	a, err := scanChatArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("chat artifact " + id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat artifact: %w", err)
	}
	return a, nil
}

// PruneChatArtifacts keeps only the newest `keep` previews of a session.
func (db *DB) PruneChatArtifacts(ctx context.Context, sessionID string, keep int) error {
	if sessionID == "" || keep <= 0 {
		return nil
	}
	if _, err := db.write.ExecContext(ctx, `
DELETE FROM chat_artifacts WHERE session_id = ? AND id NOT IN (
  SELECT id FROM chat_artifacts WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT ?
)`, sessionID, sessionID, keep); err != nil {
		return fmt.Errorf("store: prune chat artifacts: %w", err)
	}
	return nil
}

// GetAPIKey loads a key by its database id. The console chat names a key by id rather than
// by its plaintext (which nobody has), so this is the lookup VerifyID builds on; the
// validity checks themselves stay in internal/apikey, shared with normal bearer auth.
func (db *DB) GetAPIKey(ctx context.Context, id int64) (*domain.APIKey, error) {
	if id <= 0 {
		return nil, domain.ErrUnauthorized("invalid API key")
	}
	row := db.read.QueryRowContext(ctx, "SELECT "+apiKeyCols+" FROM api_keys WHERE id = ?", id)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrUnauthorized("invalid API key")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get api key by id: %w", err)
	}
	return k, nil
}

// isUniqueViolation reports whether an error is SQLite's UNIQUE constraint failure. The
// driver is modernc.org/sqlite, which does not export a typed error for it, so the message
// is what is available; the alternative — a pre-flight SELECT — would still race.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}

func decodeInt64List(raw string) []int64 {
	if strings.TrimSpace(raw) == "" {
		return []int64{}
	}
	var out []int64
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []int64{}
	}
	if out == nil {
		return []int64{}
	}
	return out
}

func encodeInt64List(values []int64) string {
	if len(values) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func decodeStringList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func encodeStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func decodeParts(raw string) []domain.ChatPart {
	if strings.TrimSpace(raw) == "" {
		return []domain.ChatPart{}
	}
	var out []domain.ChatPart
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []domain.ChatPart{}
	}
	if out == nil {
		return []domain.ChatPart{}
	}
	return out
}

func encodeParts(parts []domain.ChatPart) string {
	if len(parts) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

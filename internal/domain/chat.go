package domain

import "time"

// ChatSession is one console conversation. It is owned by an administrator account
// (AdminUser), not by a billing account: the model steps are billed through the API key
// the session binds, but "whose conversation is this" is the person who is logged in.
type ChatSession struct {
	ID           string
	OwnerUserID  int64
	OwnerName    string
	Title        string
	Model        string
	AccountID    int64
	APIKeyID     int64
	WriteMode    string // read_only | allow_writes
	SkillIDs     []int64
	Status       string // active
	MessageCount int
	TokensIn     int
	TokensOut    int
	Reasoning    int
	LastMessage  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AllowsWrites reports whether this session's management tool calls may modify the
// gateway. It is one of two gates that must agree — the other is the administrator's own
// role, re-checked on every tool call rather than trusted from the session row.
func (s *ChatSession) AllowsWrites() bool { return s != nil && s.WriteMode == ChatWriteModeAllow }

// Chat turn and session vocabulary.
const (
	ChatWriteModeReadOnly = "read_only"
	ChatWriteModeAllow    = "allow_writes"

	ChatTurnRunning     = "running"
	ChatTurnCompleted   = "completed"
	ChatTurnFailed      = "failed"
	ChatTurnAborted     = "aborted"
	ChatTurnInterrupted = "interrupted"

	ChatMessageOK          = "ok"
	ChatMessageFailed      = "failed"
	ChatMessageAborted     = "aborted"
	ChatMessageInterrupted = "interrupted"

	ChatToolPending = "pending"
	ChatToolDone    = "done"
	ChatToolFailed  = "failed"
	// ChatToolUnknown is what a pending row becomes when the process that owned it died:
	// the call may or may not have reached the gateway, so it is never replayed
	// automatically and is reported to the user as "possibly executed".
	ChatToolUnknown = "unknown"

	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"

	ChatArtifactHTML = "html"
	ChatArtifactSVG  = "svg"
)

// ChatTurn is one question and everything the server did to answer it. turn_id is the
// client's idempotency key: a retry with the same id returns this row instead of paying
// for the question twice.
type ChatTurn struct {
	ID         string
	SessionID  string
	TurnID     string
	Status     string
	Error      string
	RequestIDs []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ChatPart is one ordered piece of an assistant message: either model text or a tool
// call the gateway executed. Parts are what the console renders; text parts are also
// concatenated into the message's plain content for previews and search.
type ChatPart struct {
	Type      string `json:"type"` // text | tool_call
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Status    string `json:"status,omitempty"`
}

// ChatMessage is one stored message. ProviderItems is the canonical conversation state
// replayed to the model; Content is the human-readable projection of the parts.
type ChatMessage struct {
	ID            string
	SessionID     string
	TurnID        string
	Seq           int
	Role          string
	Content       string
	Parts         []ChatPart
	ProviderItems string
	Reasoning     string
	Status        string
	Truncated     bool
	Error         string
	Model         string
	ResolvedModel string
	Provider      string
	RequestIDs    []string
	TokensIn      int
	TokensOut     int
	ReasoningTok  int
	CreatedAt     time.Time
}

// ChatToolCall is the durable record of one tool execution. A row is written as pending
// before the tool runs so an interrupted turn can be reported honestly.
type ChatToolCall struct {
	ID         string
	SessionID  string
	TurnID     string
	Step       int
	CallID     string
	Name       string
	Arguments  string
	Result     string
	IsError    bool
	Status     string
	DurationMS int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ChatSkill is a reusable instruction set distilled from a conversation. Skills are
// private to the administrator who created them.
type ChatSkill struct {
	ID              int64
	OwnerUserID     int64
	Name            string
	Description     string
	Instructions    string
	SourceSessionID string
	SourceModel     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ChatArtifact is a previewable payload (HTML5 page or SVG) extracted from an assistant
// message. Artifacts are served only to their owner, behind a short-lived ticket.
type ChatArtifact struct {
	ID          string
	OwnerUserID int64
	SessionID   string
	Key         string
	Title       string
	Format      string
	Body        string
	SizeBytes   int
	CreatedAt   time.Time
}

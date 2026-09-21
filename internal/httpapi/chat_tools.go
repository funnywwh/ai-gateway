package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
)

// The console chat is a plain MCP client. It holds no privileges of its own: every tool call
// is a POST /mcp whose authority comes from the MCP token the conversation is bound to. That
// means there is exactly one implementation of "what may be done" — the one behind the /mcp
// endpoint — instead of a second, drifting copy shaped for the console.
//
// The call is made in-process, through the same handler an external client reaches, because
// the gateway already calls its own endpoints that way (see chatRunner, which POSTs
// /v1/responses with an injected identity). The difference from a socket client is only the
// transport: the request object, the handler, the scope checks, the audit rows and the JSON
// error shapes are identical. Doing it over TCP instead would require the gateway to hold the
// *plaintext* token, which exists only in the response that issued it — a credential copy the
// database deliberately does not keep.

// mcpCallRecorder captures one handler response. It is the chat equivalent of stepRecorder:
// the handler writes bytes, we read them back instead of sending them over a socket.
type mcpCallRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newMCPCallRecorder() *mcpCallRecorder {
	return &mcpCallRecorder{header: http.Header{}, status: http.StatusOK}
}

func (r *mcpCallRecorder) Header() http.Header { return r.header }

func (r *mcpCallRecorder) WriteHeader(status int) { r.status = status }

func (r *mcpCallRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }

// Flush satisfies http.Flusher. Nothing here streams: one MCP response arrives whole.
func (r *mcpCallRecorder) Flush() {}

// chatTools implements chat.Tools over the MCP endpoint.
const (
	toolCreateSkill        = "create_skill"
	toolUpdateSessionTitle = "update_session_title"
)

// skillDraftValidator is intentionally optional so HTTP tests can keep using small fake chat
// services; production chat.Service implements it and shares the CRUD validation rules.
type skillDraftValidator interface {
	ValidateSkillDraft(ownerID int64, in chat.SkillInput) (chat.SkillInput, error)
}

type chatTools struct {
	s     *Server
	token mcpsrv.PrincipalLookup
	log   *slog.Logger
	// web is the console's internet access (M73): nil when the deployment has no search
	// backend configured, in which case the web tools do not exist at all.
	web *webTools
}

// principal resolves the token a conversation is bound to, and refuses one that has stopped
// working. This runs on every call rather than once per conversation: revoking a token is
// how an operator takes authority away, so it has to bite at the next call.
func (t *chatTools) principal(ctx context.Context, access chat.Access) (mcpsrv.Principal, error) {
	lookup, err := t.lookup()
	if err != nil {
		return mcpsrv.Principal{}, err
	}
	if access.MCPTokenID <= 0 {
		return mcpsrv.Principal{}, fmt.Errorf(
			"this conversation is not bound to an MCP token, so it cannot call any tool; " +
				"bind one on the conversation, or start a new conversation and choose a token")
	}
	row, err := lookup.GetMCPTokenByID(ctx, access.MCPTokenID)
	if err != nil {
		return mcpsrv.Principal{}, fmt.Errorf(
			"the MCP token bound to this conversation no longer exists; bind another one to continue")
	}
	if row.Status != "active" {
		return mcpsrv.Principal{}, fmt.Errorf("MCP token is not active")
	}
	if row.ExpiresAt != nil && time.Now().UTC().After(*row.ExpiresAt) {
		return mcpsrv.Principal{}, fmt.Errorf("MCP token has expired")
	}
	return mcpsrv.Principal{
		AccountID: row.AccountID,
		TokenID:   row.ID,
		Name:      row.Name,
		Scope:     mcpsrv.NormalizeScope(row.Scope),
	}, nil
}

func (t *chatTools) lookup() (mcpsrv.PrincipalLookup, error) {
	if t.token == nil {
		return nil, fmt.Errorf("this deployment cannot resolve MCP tokens")
	}
	return t.token, nil
}

// warn logs a tool-surface problem. A missing tool list is not fatal to a turn — the model
// simply has nothing to call — so it must not fail the whole answer, but it must be visible
// to the operator rather than silently producing an unhelpful reply.
func (t *chatTools) warn(msg string, args ...any) {
	if t.log != nil {
		t.log.Warn(msg, args...)
	}
}

// List offers the tools the bound token may call. The list comes from the MCP service itself
// so a scope change (or a new endpoint) cannot make the console's menu disagree with what the
// endpoint would actually accept.
func (t *chatTools) List(access chat.Access) []chat.Tool {
	ctx := context.Background()
	// The web tools are not management tools and need no MCP token, so they are listed
	// independently of the block below: a conversation that has internet access switched on
	// keeps searching even when its token was revoked. Everything else still requires the
	// token, because everything else acts on this gateway.
	web := t.webToolDefs(access)
	principal, err := t.principal(ctx, access)
	if err != nil {
		// No usable token means no management tools, which is the honest answer; the failure
		// itself is reported when the model tries to call something.
		return web
	}
	resp, err := t.exchange(ctx, principal, "tools/list", nil)
	if err != nil {
		t.warn("chat: listing MCP tools failed", "err", err, "session", access.SessionID)
		return nil
	}
	raw, ok := resp.Result.(map[string]any)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw["tools"])
	if err != nil {
		t.warn("chat: encoding MCP tool list failed", "err", err)
		return nil
	}
	var tools []mcpsrv.Tool
	if err := json.Unmarshal(encoded, &tools); err != nil {
		t.warn("chat: decoding MCP tool list failed", "err", err)
		return nil
	}
	out := make([]chat.Tool, 0, len(tools))
	// Skill creation is a console-only capability: it produces a reviewable draft and
	// never writes the private library from inside a model call. It requires the full admin
	// scope because the resulting draft can be confirmed into a private library.
	if principal.Scope == mcpsrv.ScopeAdmin {
		out = append(out, chat.Tool{
			Name:        toolCreateSkill,
			Description: "创建技能草稿：根据当前会话整理名称、描述和可执行指令；只生成待用户确认的草稿，不会自动保存",
			Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"技能名称"},"description":{"type":"string","description":"何时使用该技能"},"instructions":{"type":"string","description":"给模型执行的详细步骤，Markdown"}},"required":["name","instructions"],"additionalProperties":false}`),
		})
	}
	// Updating the current conversation title is a console-only operation. It is deliberately
	// added after MCP tools are loaded so it is never exposed through the external MCP surface.
	if access.OwnerID > 0 && access.SessionID != "" {
		out = append(out, chat.Tool{
			Name:        toolUpdateSessionTitle,
			Description: "更新当前智能问答会话标题；标题应简洁、单行，不要包含 Markdown 或解释文字",
			Schema:      json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"新的简洁单行会话标题"}},"required":["title"],"additionalProperties":false}`),
		})
	}
	for _, tool := range tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			// A tool without a declared schema must still be callable; an empty object
			// schema is what the model can actually use.
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, chat.Tool{Name: tool.Name, Description: tool.Description, Schema: schema})
	}
	return append(out, web...)
}

// webToolDefs renders the web tools for one access context, or nothing when either switch is
// off. Both are required: the deployment's switch says the gateway has a backend, the session's
// says this conversation may use it.
func (t *chatTools) webToolDefs(access chat.Access) []chat.Tool {
	if !access.WebAccess {
		return nil
	}
	return t.web.webToolDefs()
}

// Call runs one tool through POST /mcp.
func (t *chatTools) Call(ctx context.Context, access chat.Access, name string, args map[string]any) (chat.ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	// Web tools are dispatched before anything else, and before the name rewriting below: they
	// are not endpoints, so routing them through admin_request would answer "unknown endpoint".
	// They also need no MCP token, which is the whole point of keeping them separate.
	if name == toolWebSearch || name == toolWebFetch {
		return t.callWebTool(ctx, access, name, args), nil
	}
	principal, err := t.principal(ctx, access)
	if err != nil {
		// A binding problem is an answer the operator needs to see, not a silent failure of
		// the whole turn.
		return chat.ToolResult{Value: err.Error(), IsError: true}, nil
	}
	if name == toolCreateSkill {
		return t.createSkillDraft(access, args)
	}
	if name == toolUpdateSessionTitle {
		return t.updateSessionTitle(ctx, access, args)
	}
	// Models routinely collapse the two-level convention and call a management endpoint by
	// its own name instead of routing through admin_request. The intent is unambiguous — the
	// name is the endpoint they wanted and the arguments are already shaped for it — so the
	// call is routed rather than bounced. This is not a widened surface: it lands in exactly
	// the place the equivalent admin_request call would.
	//
	// The read-only query tools (get_balance, get_usage_summary, …) are *not* endpoints:
	// they are first-class MCP tools and must be passed through unchanged. Rewriting them
	// into admin_request{name: "get_balance"} would answer "unknown endpoint", which is what
	// made the console chat claim the query tools were not registered at all.
	if name != toolAdminEndpoints && name != toolAdminDescribe && name != toolAdminRequest && !mcpsrv.IsQueryTool(name) {
		if _, exists := args["name"]; !exists {
			args["name"] = name
		}
		name = toolAdminRequest
	}
	resp, err := t.exchange(ctx, principal, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return chat.ToolResult{Value: err.Error(), IsError: true}, nil
	}
	if resp.Error != nil {
		return chat.ToolResult{Value: resp.Error.Message, IsError: true}, nil
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		return chat.ToolResult{Value: "the gateway returned an unreadable tool response", IsError: true}, nil
	}
	text := mcpContentText(result)
	// A plaintext credential is shown exactly once, and from here it also lands in the
	// conversation record. Saying so is the only mitigation that does not hide the fact.
	if issued := issuedCredentialNotice(name, args, text); issued != "" {
		text += issued
	}
	isError, _ := result["isError"].(bool)
	return chat.ToolResult{Value: text, IsError: isError}, nil
}

func (t *chatTools) updateSessionTitle(ctx context.Context, access chat.Access, args map[string]any) (chat.ToolResult, error) {
	if access.OwnerID <= 0 || strings.TrimSpace(access.SessionID) == "" {
		return chat.ToolResult{Value: map[string]any{"error": "当前会话身份不可用"}, IsError: true}, nil
	}
	title, _ := args["title"].(string)
	title = chat.NormalizeSessionTitle(title)
	if title == "" {
		return chat.ToolResult{Value: map[string]any{"error": "标题不能为空"}, IsError: true}, nil
	}
	if t.s == nil || t.s.chat == nil {
		return chat.ToolResult{Value: map[string]any{"error": "聊天服务不可用"}, IsError: true}, nil
	}
	service, ok := t.s.chat.(interface {
		UpdateSessionTitle(context.Context, int64, string, string) (*domain.ChatSession, error)
	})
	if !ok {
		return chat.ToolResult{Value: map[string]any{"error": "当前部署不支持更新会话标题"}, IsError: true}, nil
	}
	session, err := service.UpdateSessionTitle(ctx, access.OwnerID, access.SessionID, title)
	if err != nil {
		return chat.ToolResult{Value: map[string]any{"error": err.Error()}, IsError: true}, nil
	}
	if t.s != nil {
		t.s.audit(ctx, access.Username, "chat.session_title_update", "chat_session", session.ID,
			map[string]any{"title": session.Title}, "ok")
	}
	return chat.ToolResult{Value: map[string]any{"updated": true, "title": session.Title, "message": "会话标题已更新"}}, nil
}

func (t *chatTools) createSkillDraft(access chat.Access, args map[string]any) (chat.ToolResult, error) {
	if access.Role != chat.RoleAdmin || access.OwnerID <= 0 {
		return chat.ToolResult{Value: map[string]any{"error": "创建技能需要管理员身份"}, IsError: true}, nil
	}
	name, _ := args["name"].(string)
	description, _ := args["description"].(string)
	instructions, _ := args["instructions"].(string)
	input := chat.SkillInput{Name: name, Description: description, Instructions: instructions}
	if t.s == nil || t.s.chat == nil {
		return chat.ToolResult{Value: map[string]any{"error": "聊天技能服务不可用"}, IsError: true}, nil
	}
	if validator, ok := t.s.chat.(skillDraftValidator); ok {
		validated, err := validator.ValidateSkillDraft(access.OwnerID, input)
		if err != nil {
			return chat.ToolResult{Value: map[string]any{"error": err.Error(), "draft": input}, IsError: true}, nil
		}
		input = validated
	}
	return chat.ToolResult{Value: map[string]any{
		"draft": map[string]string{
			"name": input.Name, "description": input.Description, "instructions": input.Instructions,
		},
		"requires_confirmation": true,
		"source_session_id":     access.SessionID,
		"message":               "技能草稿已生成，等待用户在对话中确认保存；不会自动写入技能库。",
	}, IsError: false}, nil
}

// exchange performs one in-process POST /mcp and decodes the JSON-RPC response.
func (t *chatTools) exchange(ctx context.Context, principal mcpsrv.Principal, method string, params map[string]any) (*mcpsrv.Response, error) {
	if t.s == nil || t.s.deps.MCP == nil {
		return nil, fmt.Errorf("the MCP service is unavailable on this deployment")
	}
	envelope := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		envelope["params"] = params
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encoding the MCP request failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building the MCP request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request = withMCPPrincipal(request, principal)
	recorder := newMCPCallRecorder()
	// The real handler. It enforces the same scope checks, confirm requirements and audit
	// rows an external MCP client meets.
	t.s.handleMCP(recorder, request)
	if recorder.status != http.StatusOK {
		return nil, fmt.Errorf("the MCP endpoint answered %d: %s", recorder.status, truncateForError(recorder.body.String()))
	}
	var response mcpsrv.Response
	if err := json.Unmarshal(recorder.body.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("decoding the MCP response failed")
	}
	return &response, nil
}

// mcpContentText flattens the MCP content array into the text the model reads.
func mcpContentText(result map[string]any) string {
	content, ok := result["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range content {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := entry["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// issuedCredentialNotice appends a one-time-secret warning to the two endpoints that hand out
// a credential. The value is not redacted: hiding it would leave the operator without a
// credential while still having stored it, which is strictly worse than saying so.
func issuedCredentialNotice(name string, args map[string]any, text string) string {
	if text == "" {
		return ""
	}
	endpoint, _ := args["name"].(string)
	if name != toolAdminRequest || (endpoint != "admin_create_key" && endpoint != "admin_create_mcp_token") {
		return ""
	}
	return "\n\n[注意] 上面这个明文凭据只显示这一次。它现在也保存在本会话记录里，请立即复制到密码管理器；" +
		"如果结果看起来被截断了，请到控制台对应页面重新签发，不要使用不完整的凭据。"
}

// truncateForError keeps a non-200 response readable in a tool result.
func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

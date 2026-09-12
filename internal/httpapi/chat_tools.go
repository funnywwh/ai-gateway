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
type chatTools struct {
	s     *Server
	token mcpsrv.PrincipalLookup
	log   *slog.Logger
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
	principal, err := t.principal(ctx, access)
	if err != nil {
		// No usable token means no tools, which is the honest answer; the failure itself is
		// reported when the model tries to call something.
		return nil
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
	for _, tool := range tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			// A tool without a declared schema must still be callable; an empty object
			// schema is what the model can actually use.
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, chat.Tool{Name: tool.Name, Description: tool.Description, Schema: schema})
	}
	return out
}

// Call runs one tool through POST /mcp.
func (t *chatTools) Call(ctx context.Context, access chat.Access, name string, args map[string]any) (chat.ToolResult, error) {
	principal, err := t.principal(ctx, access)
	if err != nil {
		// A binding problem is an answer the operator needs to see, not a silent failure of
		// the whole turn.
		return chat.ToolResult{Value: err.Error(), IsError: true}, nil
	}
	if args == nil {
		args = map[string]any{}
	}
	// Models routinely collapse the two-level convention and call a management endpoint by
	// its own name instead of routing through admin_request. The intent is unambiguous — the
	// name is the endpoint they wanted and the arguments are already shaped for it — so the
	// call is routed rather than bounced. This is not a widened surface: it lands in exactly
	// the place the equivalent admin_request call would.
	if name != toolAdminEndpoints && name != toolAdminDescribe && name != toolAdminRequest {
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

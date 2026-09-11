package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP protocol revision this server speaks.
const ProtocolVersion = "2025-06-18"

// Request is one JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC / MCP error codes used by this server.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeUnauthorized   = -32001
)

// Handle processes one JSON-RPC request on behalf of an authenticated principal.
// It never panics: internal failures are reported as JSON-RPC errors.
func (s *Service) Handle(ctx context.Context, principal Principal, raw []byte) *Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return &Response{JSONRPC: "2.0", Error: &RPCError{Code: CodeParse, Message: "invalid JSON"}}
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: CodeInvalidRequest, Message: "unsupported jsonrpc version"}}
	}

	switch req.Method {
	case "initialize":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "aigw-mcp", "version": "0.2.0"},
			"instructions":    s.instructions(principal),
		}}
	case "ping":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.ToolsFor(principal)}}
	case "tools/call":
		return s.handleToolCall(ctx, principal, req)
	default:
		return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{
			Code: CodeMethodNotFound, Message: "unknown method: " + req.Method,
		}}
	}
}

// instructions tells the client what this credential can do. Scope is the whole
// difference between "read your own usage" and "operate the gateway", so it is
// stated up front instead of being discovered by a 403.
func (s *Service) instructions(principal Principal) string {
	base := "Read-only access to this account's usage, balance, ledger and recorded requests. " +
		"Content is available only for channels the account opted into recording."
	switch NormalizeScope(principal.Scope) {
	case ScopeAdmin:
		return base + " This token also carries scope=admin: every management endpoint is available through " +
			"admin_endpoints (overview), admin_describe (parameters and body schema) and admin_request (execute). " +
			"Prefer the query tools for reporting questions; call admin_endpoints before the first administrative call."
	case ScopeAdminRead:
		return base + " This token also carries scope=admin_read: the management endpoints are visible through " +
			"admin_endpoints / admin_describe, and admin_request may only call the read-only ones (writes answer 403)."
	default:
		return base + " This token has scope=query: administrative endpoints are not available."
	}
}

func (s *Service) handleToolCall(ctx context.Context, principal Principal, req Request) *Response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: CodeInvalidParams, Message: "invalid params"}}
		}
	}
	if params.Name == "" {
		return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: CodeInvalidParams, Message: "tool name is required"}}
	}

	result, err := s.CallAs(ctx, principal, params.Name, params.Arguments)
	if err != nil {
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}}
	}

	payload, err := json.MarshalIndent(result.Value, "", "  ")
	if err != nil {
		return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: CodeInternal, Message: "encoding result failed"}}
	}
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(payload)}},
		"isError": result.IsError,
	}}
}

// DescribeTools returns a human-readable tool catalogue (admin diagnostics).
func (s *Service) DescribeTools() []string {
	tools := s.Tools()
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, fmt.Sprintf("%s - %s", tool.Name, tool.Description))
	}
	return out
}

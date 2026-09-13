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
//
// It also states the three-step workflow (see what exists → read how to call it → call it) and
// the rule that follows from the field shapes this milestone documented: a configuration
// document is written from the schema admin_describe returns, never from memory. An agent that
// guesses a field name gets a 400, and an agent that refuses to write because it cannot see the
// field names is not being cautious — it is being handed an incomplete interface.
func (s *Service) instructions(principal Principal) string {
	base := "这个令牌可以读取本账户（token 所属账户）的用量、余额、账本与已录制的请求内容；" +
		"内容只对开启了录制的渠道可见。查询工具（get_dashboard、get_usage_breakdown、list_requests、get_request、" +
		"get_balance、get_ledger、get_usage_summary、get_rate_limits、list_invoices、get_invoice、get_models）各自说明了" +
		"它返回什么、什么时候该用它而不是相邻工具；报告类问题优先用它们，不要为了查询去调后台接口。"
	switch NormalizeScope(principal.Scope) {
	case ScopeAdmin:
		return base + " 该令牌 scope=admin：全部管理面接口都可用，入口是 admin_endpoints（先看有什么）、" +
			"admin_describe（查参数与请求体 schema）与 admin_request（执行）。" +
			"工作流固定为三步：先 admin_endpoints 找到接口名 → 再 admin_describe 拿到参数与 body_schema/example → 最后 admin_request 执行。" +
			"配置类文档（价格规则集、策略、能力声明等）的字段名、单位与约束都在 body_schema 里，" +
			"必须按它写；凭记忆猜字段名会被严格解析拒绝（400），而形状确实没给时应当明说拿不到，不要猜。"
	case ScopeAdminRead:
		return base + " 该令牌 scope=admin_read：管理面接口可以通过 admin_endpoints / admin_describe 查看，" +
			"admin_request 只能调用只读接口（写接口返回 403，需要 scope=admin 的令牌）。" +
			"注意 admin_validate_pricing 是只读接口（只校验不写库），可用它确认一份价格规则集是否合法。"
	default:
		return base + " 该令牌 scope=query：没有后台管理接口，看不到也调不到 admin_endpoints / admin_describe / admin_request。"
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

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
)

// This file is the bridge that lets an MCP client execute the management API.
//
// An agent gets three tools, not eighty: admin_endpoints lists what exists,
// admin_describe explains one endpoint (parameters, body schema, whether it is
// destructive), and admin_request executes it. The third one dispatches straight
// into the very handler the console calls, with a synthetic administrator
// principal whose role comes from the MCP token scope — so MCP and console can
// never diverge in behaviour, validation or auditing.

// mcpActorKey is the context key carrying the synthetic principal. It is an
// unexported type, so only this package can put a value under it: an HTTP request
// that merely claims to be an MCP caller cannot forge one.
type mcpActorKey struct{}

// mcpActor is the identity the management handlers see for an MCP call.
type mcpActor struct {
	Username string
	Role     string
	TokenID  int64
	Scope    string
	Endpoint string
}

// withMCPActor returns a request bound to the synthetic principal.
func withMCPActor(r *http.Request, actor mcpActor) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), mcpActorKey{}, actor))
}

// mcpActorFrom reports the synthetic principal of a request, if any.
func mcpActorFrom(ctx context.Context) (mcpActor, bool) {
	actor, ok := ctx.Value(mcpActorKey{}).(mcpActor)
	return actor, ok
}

// adminBackend implements mcpsrv.Backend.
type adminBackend struct{ s *Server }

// Tool names of the administrative entry points.
const (
	toolAdminEndpoints = "admin_endpoints"
	toolAdminDescribe  = "admin_describe"
	toolAdminRequest   = "admin_request"
)

// AdminTools returns the three entry points, and nothing when the deployment has
// turned the administrative surface off.
func (b *adminBackend) AdminTools(p mcpsrv.Principal) []mcpsrv.Tool {
	if b.s.adminToolsDisabled() {
		return nil
	}
	if b.s.adminIndex == nil || len(b.s.adminIndex.routes) == 0 {
		return nil
	}
	role := mcpsrv.AdminRole(p.Scope)
	if role == "" {
		return nil
	}
	readOnly := role != "admin"
	return []mcpsrv.Tool{
		{
			Name: toolAdminEndpoints,
			Description: "List the management endpoints this token may call (name, method, path, one-line summary, " +
				"required role, parameters, whether a body is needed, whether the call is destructive). " +
				"Start here: filter by keyword or group instead of guessing names.",
			InputSchema: objectSchema(map[string]any{
				"filter": prop("string", "Case-insensitive substring matched against name, path and summary (for example provider or credit)"),
				"group":  prop("string", "Only endpoints of one family: system, keys, requests, audit, accounts, models, providers, billing, backups, portal, pricing, mcp, hooks, settings, chat"),
				"limit": map[string]any{
					"type": "integer", "minimum": 1, "maximum": 200,
					"description": "Maximum rows to return (default 200)",
				},
			}),
		},
		{
			Name: toolAdminDescribe,
			Description: "Explain one management endpoint in full: path parameters, query parameters, request body JSON Schema, " +
				"an example admin_request payload, and why a destructive endpoint needs confirmation. " +
				"Call this before the first admin_request for an endpoint.",
			InputSchema: objectSchema(map[string]any{
				"name": prop("string", "Endpoint name from admin_endpoints, for example admin_create_provider"),
				"names": map[string]any{
					"type": "array", "items": prop("string", "Endpoint name"),
					"description": "Describe several endpoints in one call",
				},
			}),
		},
		{
			Name: toolAdminRequest,
			Description: "Execute one management endpoint by name. Pass path parameters in params, query parameters in query " +
				"and the JSON body in body. Returns the endpoint's HTTP status and response. Destructive endpoints require confirm=true." +
				readOnlyNote(readOnly),
			InputSchema: objectSchema(map[string]any{
				"name":    prop("string", "Endpoint name from admin_endpoints"),
				"params":  prop("object", "Path parameters, for example {\"id\": 12}"),
				"query":   prop("object", "Query parameters; arrays become repeated keys"),
				"body":    prop("object", "JSON request body (omit for endpoints that take none)"),
				"confirm": prop("boolean", "Required for destructive endpoints: set true only after telling the user what will happen"),
			}, "name"),
		},
	}
}

func readOnlyNote(readOnly bool) string {
	if readOnly {
		return " This token has scope=admin_read, so only endpoints with role=viewer can run."
	}
	return ""
}

// adminToolsDisabled reports whether the operator switched the surface off.
func (s *Server) adminToolsDisabled() bool {
	return s.deps.Config != nil && !s.deps.Config.MCP.AdminTools
}

// CallAdmin executes one administrative tool.
func (b *adminBackend) CallAdmin(ctx context.Context, p mcpsrv.Principal, name string, args map[string]any) (mcpsrv.ToolResult, error) {
	if b.s.adminToolsDisabled() {
		return mcpsrv.ToolResult{}, fmt.Errorf("the administrative tool surface is disabled on this deployment")
	}
	role := mcpsrv.AdminRole(p.Scope)
	if role == "" {
		// Same wording as an unknown tool: a query token must not be able to probe
		// which administrative endpoints exist.
		return mcpsrv.ToolResult{}, fmt.Errorf("unknown tool %q", name)
	}
	switch name {
	case toolAdminEndpoints:
		value, err := b.listEndpoints(ctx, args)
		return mcpsrv.ToolResult{Value: value}, err
	case toolAdminDescribe:
		value, err := b.describeEndpoints(ctx, args)
		return mcpsrv.ToolResult{Value: value}, err
	case toolAdminRequest:
		return b.execute(ctx, p, role, args)
	default:
		return mcpsrv.ToolResult{}, fmt.Errorf("unknown tool %q", name)
	}
}

// listEndpoints answers the overview query.
func (b *adminBackend) listEndpoints(ctx context.Context, args map[string]any) (any, error) {
	filter := strings.ToLower(strings.TrimSpace(stringArg(args, "filter")))
	group := strings.TrimSpace(stringArg(args, "group"))
	limit := intArg(args, "limit")
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows := make([]map[string]any, 0, len(b.s.adminIndex.routes))
	groups := map[string]int{}
	visible, hidden := 0, 0
	for _, route := range b.s.adminIndex.routes {
		// Every route is listed, including the ones with NoTool: they appear with tool=null
		// and a reason, so a model learns the boundary instead of assuming the endpoint does
		// not exist and inventing a name. NoTool is a property of the surface itself, not of
		// the caller — the caller's scope decides what it may CALL, which execute() enforces.
		visible++
		if !route.exposed() {
			hidden++
		}
		groups[route.Group]++
		if group != "" && route.Group != group {
			continue
		}
		if filter != "" && !endpointMatches(route, filter) {
			continue
		}
		rows = append(rows, route.summaryRow())
		if len(rows) >= limit {
			break
		}
	}
	note := "call admin_describe with a name before the first admin_request; " +
		"endpoints with tool=null are registered but not offered to MCP (see reason)"
	payload := map[string]any{
		"count":     len(rows),
		"total":     visible,
		"groups":    groups,
		"endpoints": rows,
		"note":      note,
	}
	if hidden > 0 {
		payload["unavailable_over_mcp"] = hidden
	}
	return payload, nil
}

func endpointMatches(route adminRoute, filter string) bool {
	haystack := strings.ToLower(route.Name + " " + route.Path + " " + route.Summary + " " + route.Group)
	return strings.Contains(haystack, filter)
}

// describeEndpoints answers the "how do I call this" query.
func (b *adminBackend) describeEndpoints(ctx context.Context, args map[string]any) (any, error) {
	names := make([]string, 0, 4)
	if name := strings.TrimSpace(stringArg(args, "name")); name != "" {
		names = append(names, name)
	}
	if raw, ok := args["names"].([]any); ok {
		for _, item := range raw {
			if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
				names = append(names, strings.TrimSpace(name))
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("name or names is required; call %s to see the available endpoints", toolAdminEndpoints)
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		route, ok := b.s.adminIndex.lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown endpoint %q; call %s to list the available names", name, toolAdminEndpoints)
		}
		out = append(out, route.detail())
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return map[string]any{"endpoints": out, "count": len(out)}, nil
}

// execute runs one management endpoint in process.
func (b *adminBackend) execute(ctx context.Context, p mcpsrv.Principal, role string, args map[string]any) (mcpsrv.ToolResult, error) {
	name := strings.TrimSpace(stringArg(args, "name"))
	if name == "" {
		return mcpsrv.ToolResult{}, fmt.Errorf("name is required; call %s to list the available endpoints", toolAdminEndpoints)
	}
	route, ok := b.s.adminIndex.lookup(name)
	if !ok {
		return mcpsrv.ToolResult{}, fmt.Errorf("unknown endpoint %q; call %s to list the available names", name, toolAdminEndpoints)
	}
	if !route.exposed() {
		return mcpsrv.ToolResult{}, fmt.Errorf("%s is not available over MCP: %s", name, route.NoTool)
	}
	if route.Role == roleAdmin && role != roleAdmin {
		return mcpsrv.ToolResult{}, fmt.Errorf(
			"%s requires scope=admin (this token has scope=%s, which may only call read-only endpoints)",
			name, mcpsrv.NormalizeScope(p.Scope))
	}
	if route.Dangerous && !boolArg(args, "confirm") {
		return mcpsrv.ToolResult{}, fmt.Errorf(
			"%s is destructive and needs confirm=true: %s. Tell the user what will happen first, then repeat the call with confirm=true",
			name, route.ConfirmReason)
	}

	pathParams, err := pathParamsFor(route, args)
	if err != nil {
		return mcpsrv.ToolResult{}, err
	}
	query, err := queryValues(route, args)
	if err != nil {
		return mcpsrv.ToolResult{}, err
	}
	body, err := bodyBytes(route, args)
	if err != nil {
		return mcpsrv.ToolResult{}, err
	}

	target := route.Path
	for key, value := range pathParams {
		target = strings.ReplaceAll(target, "{"+key+"}", url.PathEscape(value))
	}
	request, err := http.NewRequestWithContext(ctx, route.Method, target, bytes.NewReader(body))
	if err != nil {
		return mcpsrv.ToolResult{}, fmt.Errorf("building the request failed: %w", err)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	request.URL.RawQuery = query.Encode()
	for key, value := range pathParams {
		request.SetPathValue(key, value)
	}
	actor := mcpActor{
		Username: p.Actor(), Role: role, TokenID: p.TokenID,
		Scope: mcpsrv.NormalizeScope(p.Scope), Endpoint: name,
	}
	request = withMCPActor(request, actor)

	recorder := newAdminRecorder(b.s.adminMaxResponseBytes())
	started := time.Now()
	// The parameters are recorded but never their values: a body can carry provider
	// credentials, and audit rows are readable from the console.
	b.s.audit(ctx, actor.Username, "mcp.admin_call", "admin_endpoint", name, map[string]any{
		"method": route.Method, "path": route.Path,
		"params": sortedKeys(pathParams), "query": sortedKeys(query),
		"body_keys": bodyKeys(args),
		"scope":     actor.Scope, "token_id": actor.TokenID,
	}, "started")

	route.Handler(recorder, request)
	status := recorder.Status()
	result := recorder.Result(route, name)
	duration := time.Since(started).Milliseconds()

	outcome := "ok"
	if status >= 400 {
		outcome = "failed"
	}
	b.s.audit(ctx, actor.Username, "mcp.admin_call", "admin_endpoint", name, map[string]any{
		"method": route.Method, "path": route.Path, "status": status, "duration_ms": duration,
	}, outcome)
	b.s.emitMCPCall(ctx, actor, route, status, duration)

	return mcpsrv.ToolResult{Value: result, IsError: status >= 400}, nil
}

// pathParamsFor validates that every path parameter is present.
func pathParamsFor(route adminRoute, args map[string]any) (map[string]string, error) {
	raw := mapArg(args, "params")
	out := make(map[string]string, len(route.Params))
	missing := make([]string, 0, 2)
	for _, param := range route.Params {
		value, ok := raw[param.Name]
		if !ok || value == nil || strings.TrimSpace(scalarString(value)) == "" {
			missing = append(missing, param.Name)
			continue
		}
		out[param.Name] = scalarString(value)
	}
	if len(missing) > 0 {
		declared := make([]string, 0, len(route.Params))
		for _, param := range route.Params {
			declared = append(declared, param.Name)
		}
		return nil, fmt.Errorf("%s needs the path parameter(s) %s in params (missing: %s)",
			route.Name, strings.Join(declared, ", "), strings.Join(missing, ", "))
	}
	return out, nil
}

// queryValues renders the query object, expanding arrays into repeated keys.
func queryValues(route adminRoute, args map[string]any) (url.Values, error) {
	raw := mapArg(args, "query")
	values := url.Values{}
	for key, value := range raw {
		if err := appendQueryValue(values, key, value); err != nil {
			return nil, err
		}
	}
	if len(raw) == 0 {
		// A required query parameter that is absent is a mistake worth reporting
		// before the handler answers with a generic 400.
		for _, param := range route.Query {
			if param.Required {
				return nil, fmt.Errorf("%s needs the query parameter %q", route.Name, param.Name)
			}
		}
	}
	return values, nil
}

func appendQueryValue(values url.Values, key string, value any) error {
	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		for _, item := range typed {
			values.Add(key, scalarString(item))
		}
		return nil
	case map[string]any:
		return fmt.Errorf("query parameter %q must be a scalar or an array, not an object", key)
	default:
		values.Add(key, scalarString(typed))
		return nil
	}
}

// bodyBytes renders the body object as JSON.
func bodyBytes(route adminRoute, args map[string]any) ([]byte, error) {
	raw, ok := args["body"]
	if !ok || raw == nil {
		return nil, nil
	}
	if _, isObject := raw.(map[string]any); !isObject {
		return nil, fmt.Errorf("body must be a JSON object for %s", route.Name)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encoding the body failed: %w", err)
	}
	return encoded, nil
}

func sortedKeys[V any](values map[string]V) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func bodyKeys(args map[string]any) []string {
	body := mapArg(args, "body")
	out := make([]string, 0, len(body))
	for key := range body {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// emitMCPCall publishes the mcp.call hook event (best effort, no body).
func (s *Server) emitMCPCall(ctx context.Context, actor mcpActor, route adminRoute, status int, durationMS int64) {
	if s.deps.Hooks == nil {
		return
	}
	s.deps.Hooks.Emit(ctx, &domain.Event{
		Name: "mcp.call", Timestamp: time.Now().UTC(),
		Payload: map[string]any{
			"actor": actor.Username, "endpoint": route.Name,
			"method": route.Method, "path": route.Path,
			"status": status, "ok": status < 400, "duration_ms": durationMS,
			"scope": actor.Scope, "token_id": actor.TokenID,
		},
	})
}

// adminMaxResponseBytes is the response body cap for one MCP call.
func (s *Server) adminMaxResponseBytes() int {
	if s.deps.Config != nil && s.deps.Config.MCP.AdminMaxResponseBytes > 0 {
		return s.deps.Config.MCP.AdminMaxResponseBytes
	}
	return defaultAdminMaxResponseBytes
}

const defaultAdminMaxResponseBytes = 256 * 1024

// adminRecorder captures what a management handler wrote, without touching the
// network: the agent gets the endpoint's real status, headers and body.
type adminRecorder struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	cap       int
	truncated bool
	wrotehead bool
}

func newAdminRecorder(capacity int) *adminRecorder {
	if capacity <= 0 {
		capacity = defaultAdminMaxResponseBytes
	}
	return &adminRecorder{header: http.Header{}, status: http.StatusOK, cap: capacity}
}

func (r *adminRecorder) Header() http.Header { return r.header }

func (r *adminRecorder) WriteHeader(status int) {
	if r.wrotehead {
		return
	}
	r.wrotehead = true
	r.status = status
}

func (r *adminRecorder) Write(payload []byte) (int, error) {
	r.wrotehead = true
	remaining := r.cap - r.body.Len()
	if remaining <= 0 {
		r.truncated = true
		return len(payload), nil
	}
	if len(payload) > remaining {
		r.body.Write(payload[:remaining])
		r.truncated = true
		return len(payload), nil
	}
	r.body.Write(payload)
	return len(payload), nil
}

// Flush exists so handlers that stream stay functional; nothing is buffered
// outside this recorder.
func (r *adminRecorder) Flush() {}

func (r *adminRecorder) Status() int { return r.status }

// Result renders the captured response for the agent: JSON as JSON, text as text,
// anything else as metadata (never raw binary in a chat transcript).
func (r *adminRecorder) Result(route adminRoute, name string) map[string]any {
	contentType := r.header.Get("Content-Type")
	payload := map[string]any{
		"endpoint": name, "method": route.Method, "path": route.Path,
		"status": r.status, "ok": r.status < 400,
	}
	if r.truncated {
		payload["truncated"] = true
		payload["truncated_note"] = fmt.Sprintf("the response exceeded %d bytes and was cut off", r.cap)
	}
	body := r.body.Bytes()
	switch {
	case strings.Contains(contentType, "application/json") && len(body) > 0:
		var decoded any
		if err := json.Unmarshal(body, &decoded); err == nil {
			payload["body"] = decoded
			return payload
		}
		payload["body"] = string(body)
	case strings.HasPrefix(contentType, "text/") && len(body) > 0:
		payload["content_type"] = contentType
		payload["text"] = string(body)
	case len(body) == 0:
		payload["body"] = nil
	default:
		payload["content_type"] = contentType
		payload["size_bytes"] = len(body)
		payload["note"] = "the response is not text or JSON; it is not shown in full here"
	}
	return payload
}

// ---------------------------------------------------------------------------
// argument helpers (JSON numbers arrive as float64 through encoding/json)
// ---------------------------------------------------------------------------

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return value
}

func boolArg(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	value, _ := args[key].(bool)
	return value
}

func intArg(args map[string]any, key string) int {
	if args == nil {
		return 0
	}
	switch typed := args[key].(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return 0
}

func mapArg(args map[string]any, key string) map[string]any {
	if args == nil {
		return nil
	}
	value, _ := args[key].(map[string]any)
	return value
}

// scalarString renders a JSON scalar as the string a query parameter or path
// segment expects.
func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

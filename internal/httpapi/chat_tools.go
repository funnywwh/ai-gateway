package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
)

// The console chat gets the same three administrative entry points an MCP token with
// admin_read scope gets — admin_endpoints, admin_describe, admin_request — because they are
// the gateway's own progressive-disclosure surface and reimplementing them would guarantee
// drift. What differs is the restriction layered on top:
//
//   - reads are allowed exactly as a viewer token sees them;
//   - writes are allowed only for endpoints on an explicit allowlist, so an endpoint added
//     in a future milestone is unreachable from a chat transcript until someone decides
//     otherwise;
//   - dangerous side effects (credentials, permissions, money, backups, hooks, deletions)
//     stay in the console pages where a person is looking at what they are doing.
//
// The filter is applied in the bridge itself (list, describe and execute alike), not just
// in what the model is shown: a model that guesses a forbidden endpoint name is refused by
// the same rule that hid it.

// chatWritableEndpoints is the write allowlist for console conversations. Everything not
// listed here is refused from chat even when the conversation has writes enabled, which is
// what makes the refusal list below a *default* rather than an enumeration that rots.
var chatWritableEndpoints = map[string]bool{
	// Routing and model configuration: reversible, no secrets, and exactly the kind of
	// "why is this model not being served" work a console conversation is for.
	"admin_update_model":          true,
	"admin_upsert_model":          true,
	"admin_update_route":          true,
	"admin_upsert_route":          true,
	"admin_upsert_model_mapping":  true,
	"admin_upsert_provider_model": true,
	// Provider operations that do not hand out or rewrite credentials.
	"admin_restart_provider":        true,
	"admin_test_provider":           true,
	"admin_refresh_provider_models": true,
}

// chatToolRefusalReason is shown to the model (and echoed to the operator) when it reaches
// for something outside the allowlist. It names the alternative, because "denied" without a
// next step just makes a model retry.
const chatToolRefusalReason = "this operation is not available from the console chat: it issues or changes credentials, " +
	"moves money, changes permissions, touches backups or hooks, or deletes data. Ask the operator to do it on the " +
	"matching console page, where they can see what they are changing"

// chatTools implements chat.Tools.
type chatTools struct{ s *Server }

// List returns the tool surface for one conversation, with a description note that states
// the restriction up front.
func (t *chatTools) List(access chat.Access) []chat.Tool {
	backend := &adminBackend{s: t.s}
	tools := backend.AdminTools(chatPrincipal(access))
	out := make([]chat.Tool, 0, len(tools))
	for _, tool := range tools {
		item := chat.Tool{Name: tool.Name, Description: tool.Description, Schema: tool.InputSchema}
		if tool.Name == toolAdminRequest {
			item.Description += ". From the console chat only read-only endpoints and a small allowlist of " +
				"configuration changes may be called; admin_endpoints marks what is unavailable"
		}
		out = append(out, item)
	}
	return out
}

// Call executes one management tool call under the conversation's policy.
//
// Models routinely collapse the two-level convention and call a *management endpoint* by
// its own name ("admin_request_dimensions") instead of calling admin_request with that
// name. The intent is unambiguous — the name is the endpoint they wanted, and the arguments
// are already shaped the way admin_request takes them — so the call is routed rather than
// bounced. This is not a widened surface: the same allowlist decides, because the call ends
// up in exactly the same place it would have.
func (t *chatTools) Call(ctx context.Context, access chat.Access, name string, args map[string]any) (chat.ToolResult, error) {
	backend := &adminBackend{s: t.s}
	toolName := name
	if name != toolAdminEndpoints && name != toolAdminDescribe && name != toolAdminRequest {
		// A name that is not one of the three tools is either an endpoint the model meant to
		// invoke (handled here) or a hallucination (answered with what does exist).
		if _, ok := t.s.adminIndex.lookup(name); !ok {
			return chat.ToolResult{}, fmt.Errorf("unknown tool %q: this gateway offers %s, %s and %s (use %s with the endpoint name in its name argument)",
				name, toolAdminEndpoints, toolAdminDescribe, toolAdminRequest, toolAdminRequest)
		}
		toolName = toolAdminRequest
		if args == nil {
			args = map[string]any{}
		}
		if _, exists := args["name"]; !exists {
			args["name"] = name
		}
	}
	ctx = withChatToolPolicy(ctx, t.s.chatToolPolicy())
	result, err := backend.CallAdmin(ctx, chatPrincipal(access), toolName, args)
	if err != nil {
		return chat.ToolResult{}, err
	}
	return chat.ToolResult{Value: result.Value, IsError: result.IsError}, nil
}

// chatPrincipal maps a conversation's access onto the MCP principal the management handlers
// already understand. Writes require both the conversation switch *and* the administrator
// role, re-checked by the chat service before every call.
func chatPrincipal(access chat.Access) mcpsrv.Principal {
	scope := mcpsrv.ScopeAdminRead
	if access.Role == chat.RoleAdmin && access.WriteMode == domain.ChatWriteModeAllow {
		scope = mcpsrv.ScopeAdmin
	}
	name := strings.TrimSpace(access.Username)
	if name == "" {
		name = "admin"
	}
	return mcpsrv.Principal{Name: name, Scope: scope, Source: mcpsrv.SourceConsole}
}

// chatToolPolicy computes the endpoint allowlist for console conversations. The index is
// immutable after startup, so this is computed once.
func (s *Server) chatToolPolicy() chatToolPolicy {
	if s.chatAllowed != nil {
		return chatToolPolicy{allow: s.chatAllowed}
	}
	return chatToolPolicy{}
}

// buildChatAllowlist derives the allowlist from the route table, so it cannot disagree with
// what actually exists.
func (s *Server) buildChatAllowlist() map[string]bool {
	extra := map[string]bool{}
	if s.deps.Config != nil {
		for _, name := range s.deps.Config.Chat.HighRiskTools {
			extra[strings.TrimSpace(name)] = true
		}
	}
	allow := map[string]bool{}
	for _, route := range s.admin {
		if !route.exposed() || extra[route.Name] {
			continue
		}
		if route.Method == "GET" && route.Role == roleViewer {
			allow[route.Name] = true
			continue
		}
		if chatWritableEndpoints[route.Name] {
			allow[route.Name] = true
		}
	}
	return allow
}

// chatAllEndpoints is the sorted allowlist, used by tests and diagnostics.
func (s *Server) chatAllowedEndpoints() []string {
	out := make([]string, 0, len(s.chatAllowed))
	for name := range s.chatAllowed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

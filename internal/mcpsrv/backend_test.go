package mcpsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeBackend stands in for the transport-layer admin bridge.
type fakeBackend struct {
	calls  []string
	result ToolResult
	err    error
}

func (f *fakeBackend) AdminTools(p Principal) []Tool {
	if !p.AllowsAdmin() {
		return nil
	}
	return []Tool{{
		Name:        "admin_probe",
		Description: "fake administrative tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}}
}

func (f *fakeBackend) CallAdmin(_ context.Context, _ Principal, name string, _ map[string]any) (ToolResult, error) {
	f.calls = append(f.calls, name)
	return f.result, f.err
}

func TestToolsForAddsTheAdminSurfaceOnlyForWideScopes(t *testing.T) {
	service, _, _ := newMCPFixture(t)
	backend := &fakeBackend{}
	service.SetBackend(backend)

	for _, tc := range []struct {
		scope string
		want  int
	}{
		{ScopeQuery, 11},
		{ScopeAdminRead, 12},
		{ScopeAdmin, 12},
		{"", 11}, // unknown scopes behave like the read-only default
		{"nonsense", 11},
	} {
		if got := len(service.ToolsFor(Principal{Scope: tc.scope})); got != tc.want {
			t.Errorf("scope %q: got %d tools, want %d", tc.scope, got, tc.want)
		}
	}
}

func TestCallAsDelegatesAdminToolsAndHidesThemFromQueryTokens(t *testing.T) {
	service, _, accountID := newMCPFixture(t)
	backend := &fakeBackend{result: ToolResult{Value: map[string]any{"ok": true}}}
	service.SetBackend(backend)

	result, err := service.CallAs(context.Background(), Principal{AccountID: accountID, Scope: ScopeAdmin}, "admin_probe", nil)
	if err != nil {
		t.Fatalf("admin scope must reach the backend: %v", err)
	}
	if len(backend.calls) != 1 || backend.calls[0] != "admin_probe" {
		t.Fatalf("backend calls = %v", backend.calls)
	}
	if result.Value == nil {
		t.Fatal("the backend result must be passed through")
	}

	// A query token gets the same answer as for a tool that does not exist.
	_, err = service.CallAs(context.Background(), Principal{AccountID: accountID, Scope: ScopeQuery}, "admin_probe", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("query scope must not reach the backend, got %v", err)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("the backend must not be called for a query token: %v", backend.calls)
	}
}

func TestHandleReportsToolErrorsFromTheBackend(t *testing.T) {
	service, _, accountID := newMCPFixture(t)
	service.SetBackend(&fakeBackend{result: ToolResult{
		Value:   map[string]any{"status": 403, "ok": false},
		IsError: true,
	}})

	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "admin_probe", "arguments": map[string]any{}},
	})
	response := service.Handle(context.Background(), Principal{AccountID: accountID, Scope: ScopeAdmin}, raw)
	result, _ := response.Result.(map[string]any)
	if result["isError"] != true {
		t.Fatalf("a failed endpoint must set isError: %+v", result)
	}

	// A tool that never reached an endpoint is an error result too, not a protocol
	// error: the client should show it to the model.
	service.SetBackend(&fakeBackend{err: context.DeadlineExceeded})
	response = service.Handle(context.Background(), Principal{AccountID: accountID, Scope: ScopeAdmin}, raw)
	result, _ = response.Result.(map[string]any)
	if result["isError"] != true {
		t.Fatalf("a bridge failure must set isError: %+v", result)
	}
}

func TestPrincipalActorNamesTheToken(t *testing.T) {
	for _, tc := range []struct {
		principal Principal
		want      string
	}{
		{Principal{Name: "agent", TokenID: 7}, "mcp:agent#7"},
		{Principal{Name: "agent"}, "mcp:agent"},
		{Principal{TokenID: 7}, "mcp:token#7"},
		{Principal{}, "mcp:token"},
	} {
		if got := tc.principal.Actor(); got != tc.want {
			t.Errorf("Actor() = %q, want %q", got, tc.want)
		}
	}
}

// The query tool names are written in three places — the closed set behind
// IsQueryTool, the declarations in Tools, and the dispatch switch in callRead —
// and they must never drift apart, because the console chat routes on IsQueryTool.
func TestQueryToolSetIsConsistent(t *testing.T) {
	service, _, _ := newMCPFixture(t)

	declared := map[string]bool{}
	for _, tool := range service.Tools() {
		declared[tool.Name] = true
	}
	if len(declared) != len(queryToolNames) {
		t.Fatalf("Tools() declares %d tools but queryToolNames has %d", len(declared), len(queryToolNames))
	}
	for name := range queryToolNames {
		if !declared[name] {
			t.Errorf("query tool %q is in queryToolNames but not declared in Tools()", name)
		}
		if _, _, known := service.callRead(context.Background(), 0, name, nil); !known {
			t.Errorf("query tool %q is in queryToolNames but callRead does not dispatch it", name)
		}
	}
	for name := range declared {
		if !IsQueryTool(name) {
			t.Errorf("tool %q is declared in Tools() but IsQueryTool reports false", name)
		}
	}
}

func TestScopeHelpers(t *testing.T) {
	if NormalizeScope("admin") != ScopeAdmin || NormalizeScope("") != ScopeQuery || NormalizeScope("root") != ScopeQuery {
		t.Fatal("NormalizeScope must map unknown scopes to the read-only default")
	}
	if AdminRole(ScopeAdmin) != "admin" || AdminRole(ScopeAdminRead) != "viewer" || AdminRole(ScopeQuery) != "" {
		t.Fatal("AdminRole mapping changed")
	}
	if !AllowsAdminTools(ScopeAdminRead) || AllowsAdminTools(ScopeQuery) {
		t.Fatal("AllowsAdminTools mapping changed")
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/secret"
	"github.com/winger/ai-gateway/internal/store"
)

// Three tokens, one per scope, so every branch of the permission model is
// exercised against the real HTTP endpoint.
// The prefixes must differ from each other (token_prefix is unique), so the scope
// marker sits right after the "aigw_mcp_" prefix.
const (
	testQueryMCPToken = "aigw_mcp_query-0001"
	testReadMCPToken  = "aigw_mcp_readx-0002"
	testAdminMCPToken = "aigw_mcp_admin-0003"
)

func (f *adminFixture) seedScopedMCPToken(t *testing.T, token, scope string) *domain.MCPToken {
	t.Helper()
	ctx := context.Background()
	if _, err := f.db.UpsertMCPToken(ctx, &domain.MCPToken{
		AccountID: 1, Name: scope, TokenHash: secret.Hash(token),
		TokenPrefix: secret.Prefix(token), Scope: scope, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.db.GetMCPTokenByPrefix(ctx, secret.Prefix(token))
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (f *adminFixture) mcpCall(t *testing.T, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding MCP response: %v", err)
	}
	return resp, payload
}

// mcpToolResult decodes the text content of a tools/call response. Tool errors are
// plain text, so they come back as {"error_text": ...}.
func mcpToolResult(t *testing.T, payload map[string]any) (map[string]any, bool) {
	t.Helper()
	result, _ := payload["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result in %+v", payload)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %+v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return map[string]any{"error_text": text}, result["isError"] == true
	}
	return decoded, result["isError"] == true
}

func (f *adminFixture) callTool(t *testing.T, token string, id int, name, arguments string) (map[string]any, bool) {
	t.Helper()
	body := "{\"jsonrpc\":\"2.0\",\"id\":" + strconv.Itoa(id) +
		",\"method\":\"tools/call\",\"params\":{\"name\":\"" + name + "\",\"arguments\":" + arguments + "}}"
	_, payload := f.mcpCall(t, token, body)
	return mcpToolResult(t, payload)
}

func mcpToolNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	result, _ := payload["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestMCPToolSchemasAreValidJSON guards the wire contract: an invalid input schema
// makes encoding the whole response fail, and the client sees an empty body instead
// of an error.
func TestMCPToolSchemasAreValidJSON(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	f.seedScopedMCPToken(t, testQueryMCPToken, mcpsrv.ScopeQuery)

	for _, token := range []string{testQueryMCPToken, testAdminMCPToken} {
		resp, payload := f.mcpCall(t, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tools/list status = %d", resp.StatusCode)
		}
		result, _ := payload["result"].(map[string]any)
		if result == nil {
			t.Fatalf("tools/list returned no result (an encoding failure looks like this): %+v", payload)
		}
		tools, _ := result["tools"].([]any)
		if len(tools) == 0 {
			t.Fatal("tools/list returned no tools")
		}
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			name, _ := tool["name"].(string)
			schema, ok := tool["inputSchema"].(map[string]any)
			if !ok || len(schema) == 0 {
				t.Errorf("tool %s has no usable inputSchema: %+v", name, tool["inputSchema"])
			}
			if _, ok := schema["type"]; !ok {
				t.Errorf("tool %s inputSchema has no type: %+v", name, schema)
			}
			if description, _ := tool["description"].(string); strings.TrimSpace(description) == "" {
				t.Errorf("tool %s has no description", name)
			}
		}
	}
}

func TestMCPQueryScopeSeesNoAdminTools(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testQueryMCPToken, mcpsrv.ScopeQuery)

	_, listed := f.mcpCall(t, testQueryMCPToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	names := mcpToolNames(t, listed)
	if len(names) != 11 {
		t.Fatalf("query scope must see exactly the 11 read-only tools, got %d: %v", len(names), names)
	}
	for _, hidden := range []string{toolAdminEndpoints, toolAdminDescribe, toolAdminRequest} {
		if containsName(names, hidden) {
			t.Errorf("query scope must not see %s", hidden)
		}
	}

	// Calling the admin surface with a query token is reported exactly like an
	// unknown tool, so the administrative surface cannot be enumerated.
	decoded, isError := f.callTool(t, testQueryMCPToken, 2, "admin_request", `{"name":"admin_list_providers"}`)
	if !isError {
		t.Fatalf("query scope must not execute admin tools: %+v", decoded)
	}
	if !strings.Contains(decoded["error_text"].(string), "unknown tool") {
		t.Fatalf("expected an unknown-tool error, got %+v", decoded)
	}
}

func TestMCPAdminTokenListsAndDescribesEndpoints(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	_, listed := f.mcpCall(t, testAdminMCPToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	names := mcpToolNames(t, listed)
	for _, want := range []string{toolAdminEndpoints, toolAdminDescribe, toolAdminRequest} {
		if !containsName(names, want) {
			t.Errorf("admin scope must see %s, got %v", want, names)
		}
	}
	if len(names) != 14 {
		t.Errorf("expected 11 read tools + 3 admin tools, got %d: %v", len(names), names)
	}

	// initialize states the scope and the discovery entry point.
	_, init := f.mcpCall(t, testAdminMCPToken, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`)
	initResult, _ := init["result"].(map[string]any)
	instructions, _ := initResult["instructions"].(string)
	if !strings.Contains(instructions, "scope=admin") || !strings.Contains(instructions, toolAdminEndpoints) {
		t.Fatalf("initialize must state the scope and the discovery tool: %s", instructions)
	}

	overviewPayload, isError := f.callTool(t, testAdminMCPToken, 3, toolAdminEndpoints, `{"filter":"provider"}`)
	if isError {
		t.Fatalf("admin_endpoints failed: %+v", overviewPayload)
	}
	endpoints, _ := overviewPayload["endpoints"].([]any)
	if len(endpoints) == 0 {
		t.Fatalf("filter=provider returned nothing: %+v", overviewPayload)
	}
	if total, _ := overviewPayload["total"].(float64); total < 80 {
		t.Errorf("the catalogue must cover the whole surface, got total=%v", overviewPayload["total"])
	}
	first, _ := endpoints[0].(map[string]any)
	for _, key := range []string{"name", "method", "path", "summary", "role", "dangerous"} {
		if _, ok := first[key]; !ok {
			t.Errorf("catalogue row is missing %q: %+v", key, first)
		}
	}

	detail, isError := f.callTool(t, testAdminMCPToken, 4, toolAdminDescribe, `{"name":"admin_create_provider"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", detail)
	}
	if detail["method"] != "POST" || detail["path"] != "/admin/api/v1/providers" {
		t.Fatalf("describe returned the wrong endpoint: %+v", detail)
	}
	if detail["body_schema"] == nil {
		t.Errorf("describe must include the body schema: %+v", detail)
	}
	if detail["dangerous"] != true || detail["confirm_reason"] == nil {
		t.Errorf("describe must flag a dangerous endpoint: %+v", detail)
	}
	if detail["example"] == nil {
		t.Errorf("describe must include a call example")
	}

	if _, isError := f.callTool(t, testAdminMCPToken, 5, toolAdminDescribe, `{"name":"admin_nope"}`); !isError {
		t.Fatal("describing an unknown endpoint must fail")
	}
}

// The dimension breakdown's window and order are declared in the route table, which is what
// an MCP client reads: a parameter the handler accepts but admin_describe does not list is
// invisible to an agent, and a limit described as "条数" on a table of buckets is a small
// lie an agent plans around (docs/design/m31-request-log-stats-pagination.md).
func TestMCPDescribesTheDimensionBreakdownWindow(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	detail, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminDescribe, `{"name":"admin_request_dimensions"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", detail)
	}
	docs, _ := detail["query"].([]any)
	fields := map[string]map[string]any{}
	for _, raw := range docs {
		field, _ := raw.(map[string]any)
		name, _ := field["name"].(string)
		fields[name] = field
	}
	for _, name := range []string{"limit", "offset", "sort", "group_by", "days"} {
		if fields[name] == nil {
			t.Fatalf("admin_describe must list the %q query parameter: %v", name, fields)
		}
	}
	// The window and the order are the two things this milestone added; naming them here
	// keeps the test honest if the route entry is ever trimmed back.
	if text, _ := fields["offset"]["description"].(string); text == "" {
		t.Errorf("offset must be described, not just listed: %v", fields["offset"])
	}
	if !strings.Contains(fields["limit"]["description"].(string), "分组数") {
		t.Errorf("limit must say what it counts on this endpoint: %v", fields["limit"])
	}
	enum, _ := fields["sort"]["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, value := range enum {
		text, _ := value.(string)
		got = append(got, text)
	}
	if strings.Join(got, ",") != strings.Join(store.RequestLogDimensionSorts, ",") {
		t.Fatalf("the advertised sort values %v must be the store's %v", got, store.RequestLogDimensionSorts)
	}
}

func TestMCPAdminReadScopeCannotWrite(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testReadMCPToken, mcpsrv.ScopeAdminRead)

	payload, isError := f.callTool(t, testReadMCPToken, 1, toolAdminRequest, `{"name":"admin_list_providers"}`)
	if isError {
		t.Fatalf("admin_read must be able to list providers: %+v", payload)
	}
	if payload["ok"] != true || payload["status"].(float64) != 200 {
		t.Fatalf("unexpected list result: %+v", payload)
	}

	decoded, isError := f.callTool(t, testReadMCPToken, 2, toolAdminRequest,
		`{"name":"admin_create_provider","body":{"name":"p1","kind":"testecho"}}`)
	if !isError {
		t.Fatalf("admin_read must not create providers: %+v", decoded)
	}
	if !strings.Contains(decoded["error_text"].(string), "scope=admin") {
		t.Fatalf("the refusal must explain the required scope: %+v", decoded)
	}
}

func TestMCPAdminRequestRunsTheRealHandler(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	created, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_create_provider","confirm":true,"body":{"name":"mcp-made","kind":"testecho","enabled":true,"priority":5,"weight":50}}`)
	if isError {
		t.Fatalf("creating a provider through MCP failed: %+v", created)
	}
	if created["status"].(float64) != 201 {
		t.Fatalf("expected 201 from the handler, got %+v", created)
	}

	// The write is visible through the ordinary console endpoint.
	cookie := f.login(t, adminUser, adminPassword)
	list := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/providers", "", cookie))
	data, _ := list["data"].([]any)
	found := false
	for _, raw := range data {
		row, _ := raw.(map[string]any)
		if row["name"] == "mcp-made" {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider created over MCP is missing from the console listing: %+v", list)
	}

	// A destructive endpoint without confirm is refused and explains itself.
	refused, refusedErr := f.callTool(t, testAdminMCPToken, 2, toolAdminRequest, `{"name":"admin_delete_provider","params":{"id":1}}`)
	if !refusedErr {
		t.Fatalf("an unconfirmed destructive call must be refused: %+v", refused)
	}
	if !strings.Contains(refused["error_text"].(string), "confirm=true") {
		t.Fatalf("delete without confirm must explain the requirement: %+v", refused)
	}

	// Missing path parameters are named.
	missing, missingErr := f.callTool(t, testAdminMCPToken, 3, toolAdminRequest, `{"name":"admin_get_provider","params":{}}`)
	if !missingErr {
		t.Fatalf("a missing path parameter must fail: %+v", missing)
	}
	if !strings.Contains(missing["error_text"].(string), "id") {
		t.Fatalf("a missing path parameter must be named: %+v", missing)
	}

	// Query parameters reach the handler: only the named provider is returned.
	only, isError := f.callTool(t, testAdminMCPToken, 4, toolAdminRequest,
		`{"name":"admin_get_provider","params":{"id":1}}`)
	if isError {
		t.Fatalf("get provider failed: %+v", only)
	}
	body, _ := only["body"].(map[string]any)
	if body == nil || body["name"] != "mcp-made" {
		t.Fatalf("path parameters were not applied: %+v", only)
	}

	// With confirm the call goes through.
	deleted, isError := f.callTool(t, testAdminMCPToken, 5, toolAdminRequest,
		`{"name":"admin_delete_provider","params":{"id":1},"confirm":true}`)
	if isError {
		t.Fatalf("confirmed delete failed: %+v", deleted)
	}
	if deleted["status"].(float64) != 200 {
		t.Fatalf("expected 200 from delete, got %+v", deleted)
	}
}

func TestMCPAdminRequestAuditsTheAgent(t *testing.T) {
	f := newAdminFixture(t)
	stored := f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	payload, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_create_account","body":{"name":"acme2"}}`)
	if isError {
		t.Fatalf("create account failed: %+v", payload)
	}

	rows, err := f.db.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	wantActor := "mcp:admin#" + strconv.FormatInt(stored.ID, 10)
	var callRows, accountRows, keyRows int
	for _, row := range rows {
		if !strings.HasPrefix(row.Actor, "mcp:") {
			continue
		}
		if row.Actor != wantActor {
			t.Errorf("audit actor = %q, want %q", row.Actor, wantActor)
		}
		if row.TargetType == "admin_endpoint" {
			callRows++
			// A body can carry provider credentials, so the bridge records which keys
			// were sent, never their values.
			if strings.Contains(row.ChangesJSON, "acme2") {
				t.Errorf("the bridge must not record body values, got %s", row.ChangesJSON)
			}
			if strings.Contains(row.ChangesJSON, "body_keys") {
				keyRows++
			}
		}
		if row.TargetType == "account" {
			accountRows++
		}
	}
	if callRows < 2 {
		t.Fatalf("expected the bridge to audit both the attempt and the outcome, got %d rows", callRows)
	}
	if keyRows == 0 {
		t.Fatal("the bridge should record which body keys were sent (body_keys)")
	}
	if accountRows == 0 {
		t.Fatal("the endpoint's own audit row must be attributed to the MCP actor")
	}
}

func TestMCPAdminRequestPassesThroughUnwiredPorts(t *testing.T) {
	f := newAdminFixtureWithout(t, "backups")
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	// This fixture wires no backup manager. The endpoint's own refusal (400
	// unsupported_error, written by portReady) must reach the agent unchanged: the
	// bridge reports the real status instead of inventing one.
	decoded, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest, `{"name":"admin_list_backups"}`)
	if !isError {
		t.Fatalf("a port that is not wired must surface as a failed call: %+v", decoded)
	}
	if decoded["status"].(float64) != http.StatusBadRequest {
		t.Fatalf("expected the handler's own status, got %+v", decoded)
	}
	body, _ := decoded["body"].(map[string]any)
	errObject, _ := body["error"].(map[string]any)
	if errObject["code"] != "unsupported_parameter" {
		t.Fatalf("the handler's error must be passed through: %+v", decoded)
	}
}

func TestMCPAdminHiddenEndpointsAreExplained(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	decoded, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_download_backup","params":{"id":1},"confirm":true}`)
	if !isError {
		t.Fatalf("a hidden endpoint must not be callable: %+v", decoded)
	}
	if !strings.Contains(decoded["error_text"].(string), "not available over MCP") {
		t.Fatalf("the refusal must explain why: %+v", decoded)
	}

	// It still shows up in the catalogue, with the reason.
	overview, _ := f.callTool(t, testAdminMCPToken, 2, toolAdminEndpoints, `{"filter":"download"}`)
	endpoints, _ := overview["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("expected the hidden endpoint to be listed, got %+v", overview)
	}
	row, _ := endpoints[0].(map[string]any)
	if row["tool"] != nil || row["reason"] == nil {
		t.Fatalf("a hidden endpoint must be listed with tool=null and a reason: %+v", row)
	}
}

func TestMCPAdminResponseIsTruncated(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	// Create a provider first, then shrink the cap so the listing cannot fit.
	if _, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_create_provider","confirm":true,"body":{"name":"truncation-probe","kind":"testecho","priority":9,"weight":80}}`); isError {
		t.Fatal("seeding a provider failed")
	}
	f.cfg.MCP.AdminMaxResponseBytes = 64

	decoded, isError := f.callTool(t, testAdminMCPToken, 2, toolAdminRequest, `{"name":"admin_list_providers"}`)
	if isError {
		t.Fatalf("list providers failed: %+v", decoded)
	}
	if decoded["truncated"] != true {
		t.Fatalf("an oversized response must be flagged as truncated: %+v", decoded)
	}
}

func TestMCPAdminToolsCanBeDisabledByConfig(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	f.cfg.MCP.AdminTools = false

	_, listed := f.mcpCall(t, testAdminMCPToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if names := mcpToolNames(t, listed); containsName(names, toolAdminRequest) {
		t.Fatalf("mcp.admin_tools=false must hide the administrative tools: %v", names)
	}
	out, isError := f.callTool(t, testAdminMCPToken, 2, toolAdminRequest, `{"name":"admin_list_providers"}`)
	if !isError || !strings.Contains(out["error_text"].(string), "disabled") {
		t.Fatalf("a disabled surface must say so: %+v", out)
	}
}

func TestMCPPrincipalCannotBeForgedOverHTTP(t *testing.T) {
	f := newAdminFixture(t)
	// The synthetic principal only exists inside this process, for a call the
	// bridge itself makes. An HTTP request without a session stays unauthorized.
	resp := f.call(t, http.MethodGet, "/admin/api/v1/providers", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("management API must require a real identity, got %d", resp.StatusCode)
	}
}

func TestAdminRecorderRendersNonJSONResponses(t *testing.T) {
	route := adminRoute{Method: "GET", Path: "/admin/api/v1/x", Name: "admin_x"}

	csv := newAdminRecorder(1024)
	csv.Header().Set("Content-Type", "text/csv; charset=utf-8")
	_, _ = csv.Write([]byte("id,total\n1,42\n"))
	textPayload := csv.Result(route, "admin_x")
	if textPayload["text"] != "id,total\n1,42\n" {
		t.Fatalf("text responses must be passed through as text: %+v", textPayload)
	}

	binary := newAdminRecorder(4)
	binary.Header().Set("Content-Type", "application/octet-stream")
	_, _ = binary.Write([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05})
	binaryPayload := binary.Result(route, "admin_x")
	if binaryPayload["truncated"] != true {
		t.Fatalf("binary payload must be capped and flagged: %+v", binaryPayload)
	}
	if _, shown := binaryPayload["text"]; shown {
		t.Fatalf("binary payloads must not be pasted into the transcript: %+v", binaryPayload)
	}

	jsonBody := newAdminRecorder(1024)
	jsonBody.Header().Set("Content-Type", "application/json")
	_, _ = jsonBody.Write([]byte(`{"data":[1,2]}`))
	jsonPayload := jsonBody.Result(route, "admin_x")
	body, _ := jsonPayload["body"].(map[string]any)
	if body == nil {
		t.Fatalf("JSON responses must be decoded: %+v", jsonPayload)
	}
}

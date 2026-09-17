package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Model reasoning is readable through the list endpoint but remains a write
	// operation even when a caller only attempts to clear or change that field.
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	created, isError := f.callTool(t, testAdminMCPToken, 3, toolAdminRequest,
		`{"name":"admin_upsert_model","body":{"public_name":"readback-reasoning","reasoning":{"mode":"force","effort":"high"}}}`)
	if isError {
		t.Fatalf("admin must create a model reasoning setting: %+v", created)
	}
	listed, isError := f.callTool(t, testReadMCPToken, 4, toolAdminRequest, `{"name":"admin_list_models"}`)
	if isError {
		t.Fatalf("admin_read must list model reasoning: %+v", listed)
	}
	listBody, _ := listed["body"].(map[string]any)
	rows, _ := listBody["data"].([]any)
	var reasoning any
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["public_name"] == "readback-reasoning" {
			reasoning = row["reasoning"]
			break
		}
	}
	got, _ := reasoning.(map[string]any)
	if got == nil || got["mode"] != "force" || got["effort"] != "high" {
		t.Fatalf("admin_read list must expose reasoning object: %#v", reasoning)
	}
	denied, isError := f.callTool(t, testReadMCPToken, 5, toolAdminRequest,
		`{"name":"admin_update_model","params":{"name":"readback-reasoning"},"body":{"reasoning":null}}`)
	if !isError || !strings.Contains(denied["error_text"].(string), "scope=admin") {
		t.Fatalf("admin_read must not change or clear model reasoning: %+v", denied)
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

// TestMCPDescribeCarriesThePricingRuleSchema pins the fix for the one failure this milestone
// exists for: an operator asked for a cost price, the model called admin_describe, saw
// pricing_rules as {"type":"object"} with an example of {}, and — correctly — refused to write
// a document whose field names it had no way to know.
//
// The assertions are about what an agent must be able to read: the field names of a rule set,
// the unit its rates are in, and the one structural rule that makes a written document valid.
func TestMCPDescribeCarriesThePricingRuleSchema(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	detail, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminDescribe,
		`{"name":"admin_upsert_provider_model"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", detail)
	}
	schema, _ := detail["body_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	pricing, _ := properties["pricing_rules"].(map[string]any)
	if pricing == nil {
		t.Fatalf("the body schema has no pricing_rules property: %+v", properties)
	}
	if pricing["additionalProperties"] != false {
		t.Errorf("pricing_rules must be strict: the server parses it with DisallowUnknownFields, "+
			"so a permissive schema would describe a document that cannot be saved: %+v", pricing)
	}
	desc, _ := pricing["description"].(string)
	for _, want := range []string{"微单位/百万 token", "200000"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the schema must state the rate unit (%q): %s", want, desc)
		}
	}
	inner, _ := pricing["properties"].(map[string]any)
	for _, want := range []string{"currency", "basis", "markup_bp", "rules"} {
		if inner[want] == nil {
			t.Errorf("pricing_rules schema is missing %q: %v", want, inner)
		}
	}
	rules, _ := inner["rules"].(map[string]any)
	items, _ := rules["items"].(map[string]any)
	ruleProps, _ := items["properties"].(map[string]any)
	for _, want := range []string{"id", "order", "when", "rates", "per_request_fee_micros"} {
		if ruleProps[want] == nil {
			t.Errorf("a rule is missing %q: %v", want, ruleProps)
		}
	}
	rates, _ := ruleProps["rates"].(map[string]any)
	rateProps, _ := rates["properties"].(map[string]any)
	for _, dimension := range pricingDimensions {
		if rateProps[dimension] == nil {
			t.Errorf("rates must document the %q dimension: %v", dimension, rateProps)
		}
	}

	// The example has to be copyable, which for a rule set means a catch-all rule (an empty
	// when object): without one the server answers 400.
	example, _ := detail["example"].(map[string]any)
	arguments, _ := example["arguments"].(map[string]any)
	body, _ := arguments["body"].(map[string]any)
	exampleRules, _ := body["pricing_rules"].(map[string]any)
	if exampleRules == nil {
		t.Fatalf("the example has no pricing_rules document: %+v", body)
	}
	list, _ := exampleRules["rules"].([]any)
	if len(list) == 0 {
		t.Fatalf("the example rule set has no rules: %+v", exampleRules)
	}
	first, _ := list[0].(map[string]any)
	when, ok := first["when"].(map[string]any)
	if !ok || len(when) != 0 {
		t.Errorf("the example's first rule must be a catch-all (when: {}): %+v", first)
	}
}

// TestMCPPricingExampleIsWritable is the regression test for the reported failure: the document
// admin_describe hands the model must be a document the gateway accepts.
//
// It walks the path an agent would walk — describe, validate, write — against the real
// handlers, so a description that drifts from pricing.ParseRuleSet fails here instead of
// leaving a model to guess. The numbers are the ones from the report: $0.20 per 1M input,
// $1.20 per 1M output, $0.02 per 1M cache-hit, expressed in micros.
func TestMCPPricingExampleIsWritable(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	// The validator is role=viewer, so the read-only token can confirm a rule set before an
	// operator commits it — that is the workflow the tool description recommends.
	f.seedScopedMCPToken(t, testReadMCPToken, mcpsrv.ScopeAdminRead)

	// A provider to hang the upstream model on.
	if _, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_create_provider","confirm":true,"body":{"name":"codex-sub","kind":"testecho"}}`); isError {
		t.Fatal("seeding the provider failed")
	}

	detail, isError := f.callTool(t, testAdminMCPToken, 2, toolAdminDescribe,
		`{"name":"admin_upsert_provider_model"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", detail)
	}
	example, _ := detail["example"].(map[string]any)
	arguments, _ := example["arguments"].(map[string]any)
	body, _ := arguments["body"].(map[string]any)
	rules, _ := body["pricing_rules"].(map[string]any)
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: the validator accepts the documented shape (viewer scope is enough, which is why
	// this is the cheap way for a model to check its own work before writing).
	validated, isError := f.callTool(t, testReadMCPToken, 3, toolAdminRequest,
		`{"name":"admin_validate_pricing","body":`+string(raw)+`}`)
	if isError {
		t.Fatalf("the documented rule set was rejected by the validator: %+v", validated)
	}
	inner, _ := validated["body"].(map[string]any)
	if inner["valid"] != true {
		t.Fatalf("the documented rule set must validate: %+v", validated)
	}

	// Step 2: it is accepted at write time too, and reads back unchanged.
	writeBody, _ := json.Marshal(map[string]any{
		"public_model": "gpt-5.6-luna", "upstream_model": "gpt-5.6-luna", "pricing_rules": rules,
	})
	written, isError := f.callTool(t, testAdminMCPToken, 4, toolAdminRequest,
		`{"name":"admin_upsert_provider_model","params":{"id":1},"body":`+string(writeBody)+`}`)
	if isError {
		t.Fatalf("the documented rule set could not be written: %+v", written)
	}
	stored, _ := written["body"].(map[string]any)
	if stored["pricing_rules"] == nil {
		t.Fatalf("the write did not keep the rules: %+v", written)
	}
	writtenRules, _ := stored["pricing_rules"].(map[string]any)
	if len(writtenRules) == 0 {
		t.Fatalf("the stored rule set came back empty: %+v", stored)
	}

	// Step 3: the sale side's documented example works the same way, and "售价 = 成本" is
	// markup_bp 0 on the sale side rather than anything on the cost side.
	saleDetail, isError := f.callTool(t, testAdminMCPToken, 5, toolAdminDescribe,
		`{"name":"admin_update_model"}`)
	if isError {
		t.Fatalf("admin_describe(admin_update_model) failed: %+v", saleDetail)
	}
	saleExample, _ := saleDetail["example"].(map[string]any)
	saleArguments, _ := saleExample["arguments"].(map[string]any)
	saleBody, _ := saleArguments["body"].(map[string]any)
	salePricing, _ := saleBody["sale_pricing"].(map[string]any)
	if salePricing == nil {
		t.Fatalf("the model example has no sale_pricing document: %+v", saleBody)
	}
	pricingJSON, _ := json.Marshal(map[string]any{"public_name": "gpt-5.6-luna", "sale_pricing": salePricing})
	modelWritten, isError := f.callTool(t, testAdminMCPToken, 6, toolAdminRequest,
		`{"name":"admin_upsert_model","body":`+string(pricingJSON)+`}`)
	if isError {
		t.Fatalf("the documented sale pricing could not be written: %+v", modelWritten)
	}
	modelRow, _ := modelWritten["body"].(map[string]any)
	if modelRow["sale_pricing"] == nil {
		t.Fatalf("the sale pricing was not stored: %+v", modelWritten)
	}

	// The markup the example states is the markup that has to take effect. This is the assertion
	// that keeps the example off a zero: a model-level markup_bp of 0 is indistinguishable from
	// "unset" and silently resolves to billing.default_markup_bp, so an example built on 0 would
	// document a document that does not do what it says (see salePricingExample).
	documentedMarkup := salePricing["markup_bp"]
	if documentedMarkup == nil {
		t.Fatalf("the documented sale pricing has no markup_bp: %+v", salePricing)
	}
	bp, _ := numeric(documentedMarkup)
	if bp == 0 {
		t.Fatalf("the sale-pricing example must not use markup_bp 0: on the model side 0 means "+
			"\"unset\" and resolves to the default markup, so the example would silently not "+
			"match what it appears to say: %+v", salePricing)
	}
	simulated, isError := f.callTool(t, testAdminMCPToken, 7, toolAdminRequest,
		`{"name":"admin_simulate_pricing","body":{"model":"gpt-5.6-luna","dimensions":{"input":1000000,"input_cache_miss":1000000,"output":1000000}}}`)
	if isError {
		t.Fatalf("simulating the price of what was just written failed: %+v", simulated)
	}
	simBody, _ := simulated["body"].(map[string]any)
	if effective, _ := numeric(simBody["markup_bp"]); effective != bp {
		t.Errorf("the written sale pricing says markup_bp %v but the simulator priced it at %v "+
			"(cost_micros %v, charge_micros %v): the two must agree for the example to be "+
			"copyable", bp, simBody["markup_bp"], simBody["cost_micros"], simBody["charge_micros"])
	}
	if cost, _ := numeric(simBody["cost_micros"]); cost != 1600000 {
		t.Errorf("$0.20/1M input, $0.02/1M cache-hit and $1.20/1M output over 1M+1M+1M tokens "+
			"is 1.60 USD = 1600000 micros, got %v: the unit conversion the operator's request "+
			"depended on is wrong", simBody["cost_micros"])
	}
}

func TestMCPModelReasoningSchemaAndClear(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)

	detail, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminDescribe,
		`{"name":"admin_update_model"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", detail)
	}
	schema, _ := detail["body_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	reasoningSchema, _ := properties["reasoning"].(map[string]any)
	oneOf, _ := reasoningSchema["oneOf"].([]any)
	if _, hasType := reasoningSchema["type"]; hasType {
		t.Fatalf("reasoning schema must not constrain its oneOf alternatives with a top-level type: %+v", reasoningSchema)
	}
	if len(oneOf) != 2 {
		t.Fatalf("reasoning schema must allow object or null: %+v", reasoningSchema)
	}
	object, _ := oneOf[0].(map[string]any)
	if object["type"] != "object" || object["additionalProperties"] != false {
		t.Fatalf("reasoning object must be strict: %+v", object)
	}
	required, _ := object["required"].([]any)
	if len(required) != 2 || required[0] != "mode" || required[1] != "effort" {
		t.Fatalf("reasoning object must require mode and effort: %+v", object)
	}
	null, _ := oneOf[1].(map[string]any)
	if null["type"] != "null" {
		t.Fatalf("reasoning schema second alternative must allow clear with null: %+v", null)
	}
	reasoningProperties, _ := object["properties"].(map[string]any)
	mode, _ := reasoningProperties["mode"].(map[string]any)
	if !strings.Contains(fmt.Sprint(mode["description"]), "客户端未提供") {
		t.Fatalf("default mode description must state it fills only a missing request effort: %+v", mode)
	}
	// admin_request places path values in params and JSON fields under body; keep
	// the generated MCP example aligned with the bridge's actual convention.
	example, _ := detail["example"].(map[string]any)
	arguments, _ := example["arguments"].(map[string]any)
	params, _ := arguments["params"].(map[string]any)
	if params["name"] == nil {
		t.Fatalf("model update example must place path name in params: %+v", arguments)
	}
	exampleBody, _ := arguments["body"].(map[string]any)
	if _, present := exampleBody["reasoning"]; !present {
		t.Fatalf("model update example must place reasoning in body: %+v", arguments)
	}

	created, isError := f.callTool(t, testAdminMCPToken, 2, toolAdminRequest,
		`{"name":"admin_upsert_model","body":{"public_name":"mcp-reasoning","reasoning":{"mode":"force","effort":"high"}}}`)
	if isError {
		t.Fatalf("writing model reasoning failed: %+v", created)
	}
	body, _ := created["body"].(map[string]any)
	if got, _ := body["reasoning"].(map[string]any); got == nil || got["effort"] != "high" {
		t.Fatalf("written reasoning = %#v", body["reasoning"])
	}

	cleared, isError := f.callTool(t, testAdminMCPToken, 3, toolAdminRequest,
		`{"name":"admin_update_model","params":{"name":"mcp-reasoning"},"body":{"reasoning":null}}`)
	if isError {
		t.Fatalf("clearing model reasoning failed: %+v", cleared)
	}
	body, _ = cleared["body"].(map[string]any)
	if body["reasoning"] != nil {
		t.Fatalf("cleared reasoning = %#v, want null", body["reasoning"])
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

// TestMCPAdminSetsProviderConcurrency is the M44 acceptance test on the MCP surface: an
// agent must be able to set a provider's concurrency ceiling and read back both the setting
// and the live gate. The write goes through the same handler the console uses
// (admin_update_provider), so there is no second write path to keep in sync — what this
// test pins is that the field is documented well enough to be written and that the effect
// is visible to an agent that only has MCP.
func TestMCPAdminSetsProviderConcurrency(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	f.seedScopedMCPToken(t, testReadMCPToken, mcpsrv.ScopeAdminRead)

	ctx := context.Background()
	providerID, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: "limited", Kind: "testecho", Enabled: true, Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.api.deps.Reload(ctx); err != nil {
		t.Fatalf("reload after seeding the provider: %v", err)
	}
	id := strconv.FormatInt(providerID, 10)

	// 1) An admin-scope agent sets the ceiling, and the response already carries the live gate.
	set, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_update_provider","params":{"id":`+id+`},"confirm":true,"body":{"max_inflight":1}}`)
	if isError {
		t.Fatalf("setting max_inflight over MCP failed: %+v", set)
	}
	setBody, _ := set["body"].(map[string]any)
	if setBody == nil || setBody["max_inflight"] != float64(1) {
		t.Fatalf("the write did not take effect: %+v", set)
	}
	if capacity, _ := setBody["capacity"].(map[string]any); capacity == nil || capacity["limit"] != float64(1) {
		t.Fatalf("the write must report the live gate it created: %+v", setBody)
	}

	// 2) The read-back an admin_read token has.
	read, isError := f.callTool(t, testReadMCPToken, 2, toolAdminRequest,
		`{"name":"admin_get_provider","params":{"id":`+id+`}}`)
	if isError {
		t.Fatalf("admin_read must read a provider: %+v", read)
	}
	readBody, _ := read["body"].(map[string]any)
	if readBody == nil || readBody["max_inflight"] != float64(1) {
		t.Fatalf("read-back mismatch: %+v", readBody)
	}
	capacity, _ := readBody["capacity"].(map[string]any)
	if capacity == nil || capacity["limit"] != float64(1) || capacity["inflight"] != float64(0) {
		t.Fatalf("capacity read-back mismatch: %+v", readBody)
	}

	// 3) "Who is queueing" is answerable from the provider list.
	list, isError := f.callTool(t, testReadMCPToken, 3, toolAdminRequest, `{"name":"admin_list_providers"}`)
	if isError {
		t.Fatalf("listing providers failed: %+v", list)
	}
	listBody, _ := list["body"].(map[string]any)
	rows, _ := listBody["data"].([]any)
	listed := false
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["name"] != "limited" {
			continue
		}
		if c, ok := row["capacity"].(map[string]any); ok && c["limit"] == float64(1) && c["waiting"] == float64(0) {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("admin_list_providers must carry the live capacity: %+v", listBody)
	}

	// 4) admin_stats reports the queue policy alongside the gates, so an agent can answer
	// "how long may a request wait here" without reading the configuration file.
	stats, isError := f.callTool(t, testReadMCPToken, 4, toolAdminRequest, `{"name":"admin_stats"}`)
	if isError {
		t.Fatalf("admin_stats failed: %+v", stats)
	}
	statsBody, _ := stats["body"].(map[string]any)
	block, _ := statsBody["provider_capacity"].(map[string]any)
	if block == nil {
		t.Fatalf("admin_stats must report provider_capacity: %+v", statsBody)
	}
	if block["queue_wait_s"] != float64(30) || block["queue_max_waiters"] != float64(100) {
		t.Fatalf("the reported queue policy must be the live one: %+v", block)
	}
	providers, _ := block["providers"].(map[string]any)
	entry, _ := providers[id].(map[string]any)
	if entry == nil || entry["limit"] != float64(1) || entry["admitted"] != float64(0) {
		t.Fatalf("admin_stats capacity entry mismatch: %+v", block)
	}

	// 5) The description an agent reads *before* writing must state the default and the
	// queueing behaviour: "0 = unlimited" alone leaves the consequence unguessable
	// (docs/mcp.md §4.5).
	described, isError := f.callTool(t, testAdminMCPToken, 5, toolAdminDescribe, `{"name":"admin_update_provider"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", described)
	}
	schema, _ := described["body_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	field, _ := properties["max_inflight"].(map[string]any)
	desc, _ := field["description"].(string)
	for _, want := range []string{"0", "排队", "provider_busy"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("the max_inflight description must mention %q, got %q", want, desc)
		}
	}

	// 6) admin_read may read the ceiling but not write it.
	denied, deniedErr := f.callTool(t, testReadMCPToken, 6, toolAdminRequest,
		`{"name":"admin_update_provider","params":{"id":`+id+`},"confirm":true,"body":{"max_inflight":4}}`)
	if !deniedErr || !strings.Contains(denied["error_text"].(string), "scope=admin") {
		t.Fatalf("admin_read must not set the ceiling: %+v", denied)
	}

	// 7) Writing 0 lifts the ceiling.
	lifted, isError := f.callTool(t, testAdminMCPToken, 7, toolAdminRequest,
		`{"name":"admin_update_provider","params":{"id":`+id+`},"confirm":true,"body":{"max_inflight":0}}`)
	if isError {
		t.Fatalf("lifting the ceiling failed: %+v", lifted)
	}
	liftedBody, _ := lifted["body"].(map[string]any)
	if liftedBody["max_inflight"] != float64(0) {
		t.Fatalf("lifting the ceiling did not take effect: %+v", liftedBody)
	}
	if stat := f.api.deps.Dispatcher.CapacityStats()[providerID]; stat.Limit != 0 {
		t.Fatalf("the gate must follow the write back to unlimited: %+v", stat)
	}
}

// An agent asked "which upstream did this model's spend go to" has to be able to answer it
// through MCP without a new tool: the grouping value and the filter are part of the same
// auto-derived admin surface, so this pins both the schema an agent plans around and the
// numbers it reads back (M53).
func TestMCPAdminStatisticsByProvider(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	cheap := seedAdminProvider(t, f, "mcp-cheap")
	dear := seedAdminProvider(t, f, "mcp-dear")

	seedIdentityRow(t, f, &domain.RequestLogRecord{RequestID: "req_mcp0001", AccountID: 1, APIKeyID: 1, Client: "dsh", Model: "luna", Status: "completed"})
	seedProviderAttempt(t, f, "req_mcp0001", 1, cheap, 10, 20)
	seedProviderAttempt(t, f, "req_mcp0001", 2, dear, 90, 180)

	// The description has to carry the counting rule: an agent that sums the buckets' request
	// counts against the window's total would otherwise read a correct answer as a bug.
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
	groupBy := fields["group_by"]
	if groupBy == nil {
		t.Fatalf("admin_request_dimensions must document group_by: %v", fields)
	}
	enum, _ := groupBy["enum"].([]any)
	advertised := make([]string, 0, len(enum))
	for _, value := range enum {
		text, _ := value.(string)
		advertised = append(advertised, text)
	}
	if !containsString(enum, "provider") {
		t.Fatalf("group_by must offer provider: %v", advertised)
	}
	if text, _ := groupBy["description"].(string); !strings.Contains(text, "供应商") {
		t.Errorf("group_by must say provider means 供应商: %v", groupBy)
	}
	summary, _ := detail["summary"].(string)
	if !strings.Contains(summary, "各分组「请求数」之和可能大于窗口总请求数") {
		t.Errorf("the tool description must state the per-provider counting rule: %v", summary)
	}
	filter := fields["provider_id"]
	if filter == nil {
		t.Fatalf("admin_request_dimensions must accept provider_id: %v", fields)
	}
	if text, _ := filter["description"].(string); !strings.Contains(text, "供应商") {
		t.Errorf("provider_id must be described, not just listed: %v", filter)
	}

	// And the read itself: one bucket per provider, each carrying its own cost and name.
	grouped, isError := f.callTool(t, testAdminMCPToken, 2, toolAdminRequest,
		`{"name":"admin_request_dimensions","query":{"group_by":"provider","days":1,"sort":"charge"}}`)
	if isError {
		t.Fatalf("grouped read failed: %+v", grouped)
	}
	body, _ := grouped["body"].(map[string]any)
	rows, _ := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("provider buckets = %v, want one per provider", rows)
	}
	first, _ := rows[0].(map[string]any)
	if first["key"] != strconv.FormatInt(dear, 10) || first["provider_name"] != "mcp-dear" || first["cost_micros"] != float64(90) {
		t.Fatalf("first bucket = %v, want the dearest provider with its own cost", first)
	}

	// The filter is the other half: it selects the requests a provider served.
	filtered, isError := f.callTool(t, testAdminMCPToken, 3, toolAdminRequest,
		`{"name":"admin_list_requests","query":{"days":1,"provider_id":`+strconv.FormatInt(cheap, 10)+`}}`)
	if isError {
		t.Fatalf("filtered list failed: %+v", filtered)
	}
	listBody, _ := filtered["body"].(map[string]any)
	if listBody["total"] != float64(1) {
		t.Fatalf("filtered list total = %v, want the one request", listBody["total"])
	}
	data, _ := listBody["data"].([]any)
	row, _ := data[0].(map[string]any)
	providers, _ := row["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("the row must list both providers that served it: %v", row["providers"])
	}
}

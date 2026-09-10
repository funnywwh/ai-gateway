package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

const testMCPToken = "aigw_mcp_httpapi-test-token-0001"

func (f *fixture) seedMCPToken(t *testing.T) {
	t.Helper()
	_, err := f.db.UpsertMCPToken(context.Background(), &domain.MCPToken{
		AccountID:   f.key.AccountID,
		Name:        "agent",
		TokenHash:   secret.Hash(testMCPToken),
		TokenPrefix: secret.Prefix(testMCPToken),
		Status:      "active",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) mcpCall(t *testing.T, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", f.server.URL+"/mcp", strings.NewReader(body))
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

func TestMCPRequiresToken(t *testing.T) {
	f := newFixture(t)
	f.seedMCPToken(t)

	req, _ := http.NewRequest("POST", f.server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without a token, got %d", resp.StatusCode)
	}

	// An API key must not work here: MCP access needs an account-scoped MCP token.
	req2, _ := http.NewRequest("POST", f.server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("API keys must not authenticate MCP calls, got %d", resp2.StatusCode)
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	f := newFixture(t)
	f.seedMCPToken(t)

	resp, payload := f.mcpCall(t, testMCPToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	result, _ := payload["result"].(map[string]any)
	if result == nil || result["protocolVersion"] == nil {
		t.Fatalf("initialize result mismatch: %+v", payload)
	}

	_, listed := f.mcpCall(t, testMCPToken, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listResult, _ := listed["result"].(map[string]any)
	tools, _ := listResult["tools"].([]any)
	if len(tools) < 6 {
		t.Fatalf("expected at least 6 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		if tool["inputSchema"] == nil {
			t.Errorf("tool %s has no inputSchema", name)
		}
	}
	for _, want := range []string{"get_balance", "get_ledger", "get_usage_summary", "list_requests", "get_request", "get_models"} {
		if !names[want] {
			t.Errorf("tool %s missing", want)
		}
	}
}

func TestMCPToolCallReturnsAccountData(t *testing.T) {
	f := newFixture(t)
	f.seedMCPToken(t)

	// Generate usage and a recorded request first.
	apiResp := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	apiResp.Body.Close()
	requestID := apiResp.Header.Get("x-request-id")

	_, payload := f.mcpCall(t, testMCPToken,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_balance","arguments":{}}}`)
	result, _ := payload["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		t.Fatalf("get_balance failed: %+v", payload)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content returned: %+v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "billing_mode") || !strings.Contains(text, "balance_usd") {
		t.Fatalf("balance payload mismatch: %s", text)
	}
	if strings.Contains(text, "cost") {
		t.Fatalf("MCP responses must not expose cost figures: %s", text)
	}

	// get_request returns the input text, and explains that thinking/output are off.
	_, reqPayload := f.mcpCall(t, testMCPToken,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_request","arguments":{"request_id":"`+requestID+`"}}}`)
	reqResult, _ := reqPayload["result"].(map[string]any)
	reqContent, _ := reqResult["content"].([]any)
	reqText, _ := reqContent[0].(map[string]any)["text"].(string)
	if !strings.Contains(reqText, "input_recorded") || !strings.Contains(reqText, "ping") {
		t.Fatalf("input text must be returned: %s", reqText)
	}
	if !strings.Contains(reqText, "output_text_recorded") {
		t.Fatalf("recording flags must be reported: %s", reqText)
	}
}

func TestMCPUnknownToolAndMethod(t *testing.T) {
	f := newFixture(t)
	f.seedMCPToken(t)

	_, payload := f.mcpCall(t, testMCPToken,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"drop_tables","arguments":{}}}`)
	result, _ := payload["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("unknown tool must be an error result: %+v", payload)
	}

	_, methodPayload := f.mcpCall(t, testMCPToken, `{"jsonrpc":"2.0","id":6,"method":"tools/execute"}`)
	rpcErr, _ := methodPayload["error"].(map[string]any)
	if rpcErr == nil || rpcErr["code"].(float64) != -32601 {
		t.Fatalf("unknown method must be -32601: %+v", methodPayload)
	}
}

func TestMCPCrossAccountIsolation(t *testing.T) {
	f := newFixture(t)
	f.seedMCPToken(t)
	ctx := context.Background()

	// A second account with its own recorded request.
	otherAccID, err := f.db.UpsertAccount(ctx, &domain.Account{Name: "other", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.PutRequestLog(ctx, &domain.RequestLogRecord{
		RequestID: "req_other_account", AccountID: otherAccID, APIKeyID: 999,
		Endpoint: "/v1/responses", RequestJSON: `{"input":"secret of another tenant"}`,
		Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}

	_, payload := f.mcpCall(t, testMCPToken,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_request","arguments":{"request_id":"req_other_account"}}}`)
	result, _ := payload["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("cross-account access must fail: %+v", payload)
	}
	content, _ := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "another tenant") {
		t.Fatalf("cross-account content leaked: %s", text)
	}
}

func TestMCPRevokedTokenIsRejected(t *testing.T) {
	f := newFixture(t)
	f.seedMCPToken(t)
	ctx := context.Background()

	row, err := f.db.GetMCPTokenByPrefix(ctx, secret.Prefix(testMCPToken))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RevokeMCPToken(ctx, row.ID); err != nil {
		t.Fatal(err)
	}

	resp, _ := f.mcpCall(t, testMCPToken, `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	if resp.StatusCode != 401 {
		t.Fatalf("revoked token must be rejected, got %d", resp.StatusCode)
	}
}

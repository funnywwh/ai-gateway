package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/webaccess"
)

// bingResultPage is a minimal result page in the shape Bing served when M73 was written (the
// real capture lives in internal/webaccess/testdata). The console tests use a local page so no
// test ever depends on the public internet.
const bingResultPage = `<html><body><ol id="b_results">
<li class="b_algo"><div class="b_tpcn"><a href="https://example.com/a"><cite>example.com › docs</cite></a></div>
<h2><a href="https://example.com/a">示例结果 A</a></h2>
<div class="b_caption"><p>这是示例摘要 A。</p></div></li>
<li class="b_algo"><h2><a href="https://example.com/b">示例结果 B</a></h2>
<div class="b_caption"><p>这是示例摘要 B。</p></div></li>
</ol></body></html>`

// newWebClient builds a web-access client that talks to a local test server. allowPrivate is on
// because the server is on 127.0.0.1; the guard's own refusals are asserted separately.
func newWebClient(t *testing.T, baseURL string, allowPrivate bool, maxResults int) *webaccess.Client {
	t.Helper()
	client, err := webaccess.New(webaccess.Config{
		Provider:          webaccess.ProviderBing,
		BaseURL:           baseURL,
		Timeout:           5 * time.Second,
		MaxResults:        maxResults,
		AllowPrivateHosts: allowPrivate,
	}, nil)
	if err != nil {
		t.Fatalf("build web access client: %v", err)
	}
	return client
}

// webSearchServer serves a Bing-shaped result page, so a web_search tool call can be exercised
// end to end without a network.
func webSearchServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(bingResultPage))
	}))
	t.Cleanup(server.Close)
	return server
}

func webAccessWith(access chat.Access, web *webTools) chat.Access {
	access.WebAccess = true
	access.TurnID = "t1"
	return access
}

// TestChatWebToolsNeedNoMCPToken is the whole point of keeping the web tools outside the MCP
// token model: a conversation with no token can still search and read, while its management
// surface stays empty.
func TestChatWebToolsNeedNoMCPToken(t *testing.T) {
	server := webSearchServer(t)
	tools := &chatTools{web: newWebTools(newWebClient(t, server.URL, true, 6), 8)}

	access := webAccessWith(chat.Access{OwnerID: 1, Username: "admin", Role: chat.RoleAdmin, SessionID: "s1"}, nil)
	listed := tools.List(access)
	names := toolNames(listed)
	if len(listed) != 2 || !names[toolWebSearch] || !names[toolWebFetch] {
		t.Fatalf("an unbound session with web access must still get the web tools, got %v", names)
	}
	if names[toolAdminRequest] || names[toolCreateSkill] {
		t.Fatalf("an unbound session must not get management tools: %v", names)
	}

	// And the web tools actually work in that state: no token is read on this path.
	result, err := tools.Call(context.Background(), access, toolWebSearch, map[string]any{"query": "示例"})
	if err != nil {
		t.Fatalf("web_search returned a transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("web_search failed: %+v", result.Value)
	}
}

// TestChatWebToolsAbsentWhenASwitchIsOff pins both switches: the deployment's and the session's.
func TestChatWebToolsAbsentWhenASwitchIsOff(t *testing.T) {
	server := webSearchServer(t)
	client := newWebClient(t, server.URL, true, 6)

	// Session switch off (deployment on).
	tools := &chatTools{web: newWebTools(client, 8)}
	off := chat.Access{OwnerID: 1, SessionID: "s1", TurnID: "t1"}
	if listed := tools.List(off); len(listed) != 0 {
		t.Fatalf("a session without web access must get no tools, got %v", toolNames(listed))
	}
	// Deployment off (session flag on): the state no tool list may contradict.
	deploymentOff := &chatTools{}
	if listed := deploymentOff.List(webAccessWith(off, nil)); len(listed) != 0 {
		t.Fatalf("a deployment without a search backend must offer no tools, got %v", toolNames(listed))
	}
}

// TestChatWebToolsListedAlongsideMCPTools: a bound admin session gets both, and the web tools
// are appended after the management surface.
func TestChatWebToolsListedAlongsideMCPTools(t *testing.T) {
	f := newChatFixture(t)
	server := webSearchServer(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens, web: newWebTools(newWebClient(t, server.URL, true, 6), 8)}
	access := webAccessWith(chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s1",
	}, nil)

	names := toolNames(tools.List(access))
	for _, want := range []string{toolAdminRequest, toolWebSearch, toolWebFetch} {
		if !names[want] {
			t.Fatalf("tool list is missing %s: %v", want, names)
		}
	}
}

// TestChatWebSearchRendersTheDocumentedResultShape: the model reads this JSON, so its keys are
// a contract, not an implementation detail.
func TestChatWebSearchRendersTheDocumentedResultShape(t *testing.T) {
	server := webSearchServer(t)
	tools := &chatTools{web: newWebTools(newWebClient(t, server.URL, true, 6), 8)}
	access := webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil)

	result, err := tools.Call(context.Background(), access, toolWebSearch, map[string]any{"query": "示例", "count": float64(2)})
	if err != nil || result.IsError {
		t.Fatalf("web_search = %+v err=%v", result, err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("result value = %T", result.Value)
	}
	if value["provider"] != webaccess.ProviderBing || value["query"] != "示例" {
		t.Errorf("result metadata = %v", value)
	}
	items, _ := value["results"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("results = %v", value["results"])
	}
	first := items[0]
	if first["url"] != "https://example.com/a" || first["title"] != "示例结果 A" || first["snippet"] == "" {
		t.Errorf("first result = %v", first)
	}
}

// TestChatWebFetchReturnsTextAndRefusesPrivateTargets covers both halves of web_fetch: the page
// it reads, and the address it will not read.
func TestChatWebFetchReturnsTextAndRefusesPrivateTargets(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>文档</title></head><body><p>正文内容</p></body></html>`))
	}))
	t.Cleanup(page.Close)

	tools := &chatTools{web: newWebTools(newWebClient(t, page.URL, true, 6), 8)}
	access := webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil)
	result, err := tools.Call(context.Background(), access, toolWebFetch, map[string]any{"url": page.URL + "/doc"})
	if err != nil || result.IsError {
		t.Fatalf("web_fetch = %+v err=%v", result, err)
	}
	value, _ := result.Value.(map[string]any)
	if value["title"] != "文档" || !strings.Contains(fmt.Sprint(value["content"]), "正文内容") {
		t.Errorf("fetched page = %v", value)
	}
	if value["url"] != page.URL+"/doc" || value["truncated"] != false {
		t.Errorf("page metadata = %v", value)
	}

	// The guard is the deployment's default (private hosts refused), and the refusal has to be
	// readable — it is shown to a model that will otherwise keep trying.
	guarded := &chatTools{web: newWebTools(newWebClient(t, page.URL, false, 6), 8)}
	refused, err := guarded.Call(context.Background(), access, toolWebFetch, map[string]any{"url": "http://127.0.0.1/admin/ui/"})
	if err != nil {
		t.Fatalf("a refused fetch must be a tool result, not a transport error: %v", err)
	}
	if !refused.IsError || !strings.Contains(fmt.Sprint(refused.Value), "SSRF") {
		t.Fatalf("refusal = %+v", refused.Value)
	}
}

// TestChatWebToolsRespectThePerTurnBudget: one question may spend a bounded number of external
// calls, and going past it explains itself instead of failing the turn or hammering a paid API.
func TestChatWebToolsRespectThePerTurnBudget(t *testing.T) {
	server := webSearchServer(t)
	tools := &chatTools{web: newWebTools(newWebClient(t, server.URL, true, 6), 2)}
	access := webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil)
	ctx := context.Background()
	args := map[string]any{"query": "示例"}

	for i := 0; i < 2; i++ {
		result, err := tools.Call(ctx, access, toolWebSearch, args)
		if err != nil || result.IsError {
			t.Fatalf("call %d should have been allowed: %+v err=%v", i+1, result, err)
		}
	}
	over, err := tools.Call(ctx, access, toolWebSearch, args)
	if err != nil {
		t.Fatalf("the over-budget call must be a tool result: %v", err)
	}
	if !over.IsError || !strings.Contains(fmt.Sprint(over.Value), "上限") {
		t.Fatalf("over-budget result = %+v", over.Value)
	}

	// A new turn starts with a fresh budget; the counter is per turn, not per conversation.
	next := webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil)
	next.TurnID = "t2"
	if result, err := tools.Call(ctx, next, toolWebSearch, args); err != nil || result.IsError {
		t.Fatalf("a new turn must get its own budget: %+v err=%v", result, err)
	}
	// A different session is counted separately too.
	other := webAccessWith(chat.Access{OwnerID: 1, SessionID: "s2"}, nil)
	if result, err := tools.Call(ctx, other, toolWebSearch, args); err != nil || result.IsError {
		t.Fatalf("another session must have its own budget: %+v err=%v", result, err)
	}
}

// TestChatWebToolRefusalsExplainThemselves: every refusal a model can meet names the switch or
// the reason, because "the tool failed" teaches the operator nothing.
func TestChatWebToolRefusalsExplainThemselves(t *testing.T) {
	server := webSearchServer(t)
	access := webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil)
	access.WebAccess = false
	ctx := context.Background()

	sessionOff := &chatTools{web: newWebTools(newWebClient(t, server.URL, true, 6), 8)}
	result, _ := sessionOff.Call(ctx, access, toolWebSearch, map[string]any{"query": "示例"})
	if !result.IsError || !strings.Contains(fmt.Sprint(result.Value), "会话") {
		t.Errorf("session switch refusal = %+v", result.Value)
	}

	deploymentOff := &chatTools{}
	result, _ = deploymentOff.Call(ctx, webAccessWith(access, nil), toolWebSearch, map[string]any{"query": "示例"})
	if !result.IsError || !strings.Contains(fmt.Sprint(result.Value), "chat.web_access.enabled") {
		t.Errorf("deployment switch refusal = %+v", result.Value)
	}

	// A model that invents a tool name inside the web namespace is told so rather than being
	// routed somewhere unexpected.
	on := &chatTools{web: newWebTools(newWebClient(t, server.URL, true, 6), 8)}
	result, _ = on.Call(ctx, webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil), "web_crawl", map[string]any{})
	if !result.IsError {
		t.Errorf("an unknown web tool must be refused: %+v", result.Value)
	}

	// An empty query is refused by the client, and the reason reaches the model.
	result, _ = on.Call(ctx, webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil), toolWebSearch, map[string]any{"query": "  "})
	if !result.IsError || !strings.Contains(fmt.Sprint(result.Value), "检索词") {
		t.Errorf("empty query refusal = %+v", result.Value)
	}

	// A backend that is down is reported as such.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid API KEY"}`))
	}))
	defer dead.Close()
	broken := &chatTools{web: newWebTools(newWebClient(t, dead.URL, true, 6), 8)}
	result, _ = broken.Call(ctx, webAccessWith(chat.Access{OwnerID: 1, SessionID: "s1"}, nil), toolWebSearch, map[string]any{"query": "示例"})
	if !result.IsError || !strings.Contains(fmt.Sprint(result.Value), "api_key") {
		t.Errorf("backend failure refusal = %+v", result.Value)
	}
}

// TestChatSessionWebAccessRoundTrip drives the console's own surface: the switch persists, the
// payload carries it back, and the deployment facts come along so the UI can hide the control
// where the feature does not exist.
func TestChatSessionWebAccessRoundTrip(t *testing.T) {
	f := newChatFixture(t)
	server := webSearchServer(t)
	f.cfg.Chat.WebAccess.Enabled = true
	f.cfg.Chat.WebAccess.MaxCallsPerTurn = 8
	f.api.deps.WebAccess = newWebClient(t, server.URL, true, 6)
	f.api.chat = f.api.newChatService()

	cookie := f.login(t, "admin")
	created := decodeChatJSON(t, f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions", fmt.Sprintf(
		`{"model":%q,"account_id":%d,"api_key_id":%d,"mcp_token_id":%d,"web_access":true}`,
		f.model, f.accountID, f.keyID, f.adminTokenID), cookie))
	if created["web_access"] != true {
		t.Fatalf("created session = %v", created)
	}
	if created["web_access_available"] != true || created["web_access_provider"] != webaccess.ProviderBing {
		t.Fatalf("deployment facts missing from the payload: %v", created)
	}
	id, _ := created["id"].(string)

	// Reading it back proves the flag was persisted, not just echoed.
	read := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions/"+id, "", cookie))
	if read["web_access"] != true {
		t.Fatalf("stored session = %v", read)
	}
	// And switching it off is a normal update.
	updated := decodeChatJSON(t, f.call(t, http.MethodPatch, "/admin/api/v1/chat/sessions/"+id, `{"web_access":false}`, cookie))
	if updated["web_access"] != false {
		t.Fatalf("updated session = %v", updated)
	}
	// An update that says nothing about web access leaves it alone.
	updated = decodeChatJSON(t, f.call(t, http.MethodPatch, "/admin/api/v1/chat/sessions/"+id, `{"web_access":true}`, cookie))
	if updated["web_access"] != true {
		t.Fatalf("re-enabled session = %v", updated)
	}
	titled := decodeChatJSON(t, f.call(t, http.MethodPatch, "/admin/api/v1/chat/sessions/"+id, `{"title":"改名"}`, cookie))
	if titled["web_access"] != true || titled["title"] != "改名" {
		t.Fatalf("a title-only update must not touch the switch: %v", titled)
	}
}

// TestChatSessionWebAccessRefusedWhenDeploymentIsOff: a session flag that cannot do anything is
// worse than an error on the form, so the API refuses it and says which switch is missing.
func TestChatSessionWebAccessRefusedWhenDeploymentIsOff(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	id := f.createSession(t, cookie)

	resp := f.call(t, http.MethodPatch, "/admin/api/v1/chat/sessions/"+id, `{"web_access":true}`, cookie)
	payload := decodeChatJSON(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d payload = %v", resp.StatusCode, payload)
	}
	if !strings.Contains(fmt.Sprint(payload["error"]), "web_access") {
		t.Errorf("the refusal must name the switch: %v", payload)
	}
	// The console is also told the capability is absent here, so it can hide the control.
	read := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions/"+id, "", cookie))
	if read["web_access_available"] != false {
		t.Fatalf("deployment facts = %v", read)
	}
	if _, present := read["web_access_provider"]; present {
		t.Errorf("no provider may be advertised when the feature is off: %v", read)
	}
}

func toolNames(tools []chat.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, tool := range tools {
		out[tool.Name] = true
	}
	return out
}

// TestChatWebToolsLeakNothingIntoAuditOrTheRequestLog pins decision D6: the search term and the
// page text belong to the conversation's own tool-call record, and to nothing else. The audit
// trail is read by operators and the request log by billing; neither has any business
// accumulating what somebody asked a model to look up, which is exactly the kind of thing that
// gets added later "for debugging" unless a test says no.
func TestChatWebToolsLeakNothingIntoAuditOrTheRequestLog(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	server := webSearchServer(t)
	// A real conversation is created first so the audit trail is *not empty*: the assertions
	// below are substring searches over the whole trail, and an empty table would make them pass
	// for the wrong reason.
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	tools := &chatTools{web: newWebTools(newWebClient(t, server.URL, true, 6), 8)}
	access := webAccessWith(chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin, SessionID: sessionID, TurnID: "t1",
	}, nil)

	// A term distinctive enough that a substring search is conclusive, plus a page fetch whose
	// URL is equally distinctive.
	const query = "Zx9-只有这条会话该记得"
	if result, err := tools.Call(ctx, access, toolWebSearch, map[string]any{"query": query}); err != nil || result.IsError {
		t.Fatalf("web_search: err=%v result=%+v", err, result.Value)
	}
	target := server.URL + "/leak-probe"
	if result, err := tools.Call(ctx, access, toolWebFetch, map[string]any{"url": target}); err != nil || result.IsError {
		t.Fatalf("web_fetch: err=%v result=%+v", err, result.Value)
	}

	entries, err := f.db.ListAudit(ctx, 200)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit rows at all — this test would pass vacuously")
	}
	for _, entry := range entries {
		blob := entry.Action + entry.TargetType + entry.TargetID + entry.ChangesJSON
		if strings.Contains(blob, query) || strings.Contains(blob, "leak-probe") {
			t.Errorf("audit entry %d (%s) carries the tool's input: %s", entry.ID, entry.Action, blob)
		}
	}
	// No data-plane traffic at all: the search backend is not a gateway provider, so a web tool
	// call must never appear as a billed request.
	logs, err := f.db.ListRequestLogs(ctx, domain.RequestLogFilter{}, 100)
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("a web tool call produced %d request-log rows: %+v", len(logs), logs)
	}
}

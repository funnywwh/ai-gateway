package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/apikey"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/secret"
	"github.com/winger/ai-gateway/internal/store"
	"github.com/winger/ai-gateway/internal/usage"
)

// The console chat is the only feature that spends money on behalf of a *browser session*
// instead of a bearer token, so its HTTP tests are grouped around three questions:
// who may spend, what is recorded about it, and what a preview can reach. The data-plane
// end-to-end cases run against the built-in testecho provider (no network, no fixtures).

type chatFixture struct {
	server    *httptest.Server
	api       *Server
	db        *store.DB
	cfg       *config.Config
	service   *billing.Service
	accountID int64
	keyID     int64
	model     string
	// The console chat is an MCP client, so every conversation must be bound to a token.
	// The fixture mints one per scope so a test can pick the authority it needs.
	adminTokenID int64
	readTokenID  int64
	queryTokenID int64
}

const chatPassword = "chat-secret-1"

// newMCPToken issues one token row directly, the way the admin API would. The plaintext is
// random because the store upserts on token_prefix (its first 12 characters) and a readable
// name would collide with every other token that shares its opening words.
func newMCPToken(t *testing.T, db *store.DB, accountID int64, name, scope string) int64 {
	t.Helper()
	plaintext := ids.MCPToken()
	id, err := db.UpsertMCPToken(context.Background(), &domain.MCPToken{
		AccountID:   accountID,
		Name:        name,
		TokenHash:   secret.Hash(plaintext),
		TokenPrefix: secret.Prefix(plaintext),
		Scope:       scope,
		Status:      "active",
		CreatedBy:   "admin",
	})
	if err != nil {
		t.Fatalf("issue MCP token %s: %v", name, err)
	}
	return id
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Billing.DefaultMarkupBP = 10000
	cfg.Database.Path = filepath.Join(t.TempDir(), "chat.db")
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hash, err := admin.HashPassword(chatPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAdminUser(ctx, &domain.AdminUser{Username: "admin", PasswordHash: hash, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertAdminUser(ctx, &domain.AdminUser{Username: "reader", PasswordHash: hash, Role: "viewer"}); err != nil {
		t.Fatal(err)
	}

	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "payer", BillingMode: domain.BillingPrepaid, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendLedger(ctx, []*domain.LedgerEntry{{
		AccountID: accountID, Kind: "topup", AmountMicros: 50_000_000, IdemKey: "topup:chat",
	}}); err != nil {
		t.Fatal(err)
	}
	key := &domain.APIKey{
		AccountID: accountID, Name: "console-key",
		KeyPrefix: secret.Prefix("sk-gw-console-test"), KeyHash: secret.Hash("sk-gw-console-test"),
		Status: "active", RecordInputMode: "inherit",
	}
	keyID, err := db.UpsertAPIKey(ctx, key)
	if err != nil {
		t.Fatal(err)
	}

	providerID, err := db.UpsertProvider(ctx, &domain.Provider{Name: "echo", Kind: "testecho", Enabled: true, Priority: 10, Weight: 100})
	if err != nil {
		t.Fatal(err)
	}
	const model = "chat-echo"
	if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: providerID, PublicModel: model, UpstreamModel: model, Enabled: true,
		MaxOutputTokens: 512, CapabilitiesJSON: `{"stream":true,"tools":true}`,
		PricingRulesJSON: `{"rules":[{"id":"cost","order":10,"when":{},"rates":{"input":100000,"output":2000000}}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel(ctx, &domain.Model{
		PublicName: model, Enabled: true, SalePricingJSON: `{"basis":"cost_follow","markup_bp":20000}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRoute(ctx, &domain.Route{
		ModelID: modelID, ProviderID: providerID, UpstreamModel: model, Priority: 10, Weight: 100, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(db)
	if _, err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	bal := balancer.New(balancer.DefaultConfig())
	router := routing.New(routing.Config{DefaultGrant: "all", Degradation: "strip"}, reg, bal)
	dispatcher := runtime.New(runtime.Config{}, db, reg, nil, bal, nil)
	service := billing.NewService(ctx, db, billing.ServiceConfig{
		Writer:         billing.Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond},
		ReservationTTL: time.Minute,
	}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	srv := New(Deps{
		Config:     &cfg,
		FX:         nil,
		Registry:   reg,
		Router:     router,
		Dispatcher: dispatcher,
		Verifier:   apikey.New(db, apikey.DefaultConfig()),
		Limiter:    quotaLimiter(),
		Meter:      usage.New(db),
		Records:    db,
		Admin:      admin.NewAuth(db, admin.Config{SessionTTL: time.Hour, LoginAttempts: 20, LoginWindow: time.Minute}),
		AdminStore: db,
		ChatStore:  db,
		Accounts:   db,
		Models:     db,
		Providers:  db,
		MCP:        mcpsrv.New(db, reg, mcpsrv.Config{MaxRows: 100, WindowDays: 30, Currency: "USD"}),
		MCPTokens:  db,
		Billing:    service,
		Ledger:     service,
		Log:        nil,
		Version:    "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &chatFixture{
		server: ts, api: srv, db: db, cfg: &cfg, service: service,
		accountID: accountID, keyID: keyID, model: model,
		adminTokenID: newMCPToken(t, db, accountID, "chat-admin", mcpsrv.ScopeAdmin),
		readTokenID:  newMCPToken(t, db, accountID, "chat-read", mcpsrv.ScopeAdminRead),
		queryTokenID: newMCPToken(t, db, accountID, "chat-query", mcpsrv.ScopeQuery),
	}
}

func (f *chatFixture) call(t *testing.T, method, path, body, cookie string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *chatFixture) login(t *testing.T, username string) string {
	t.Helper()
	resp := f.call(t, http.MethodPost, "/admin/api/v1/auth/login",
		`{"username":"`+username+`","password":"`+chatPassword+`"}`, "")
	defer resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == adminCookieName {
			return cookie.Value
		}
	}
	t.Fatalf("login for %s issued no session cookie (status %d)", username, resp.StatusCode)
	return ""
}

func decodeChatJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %q: %v", string(raw), err)
	}
	return payload
}

// createSession makes a conversation bound to the fixture's billed key and to the
// admin-scope MCP token, which is what makes it able to drive every console operation.
func (f *chatFixture) createSession(t *testing.T, cookie string) string {
	t.Helper()
	return f.createSessionWithToken(t, cookie, f.adminTokenID)
}

// createSessionWithToken binds a specific token, so a test can pin the authority under test.
func (f *chatFixture) createSessionWithToken(t *testing.T, cookie string, tokenID int64) string {
	t.Helper()
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions", fmt.Sprintf(
		`{"model":%q,"account_id":%d,"api_key_id":%d,"mcp_token_id":%d}`,
		f.model, f.accountID, f.keyID, tokenID), cookie)
	payload := decodeChatJSON(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session status = %d payload=%v", resp.StatusCode, payload)
	}
	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("created session has no id: %v", payload)
	}
	return id
}

// callTool invokes one management tool through the console's MCP client.
func (f *chatFixture) callTool(t *testing.T, ctx context.Context, tools *chatTools, access chat.Access, args map[string]any) chat.ToolResult {
	t.Helper()
	result, err := tools.Call(ctx, access, toolAdminRequest, args)
	if err != nil {
		t.Fatalf("tool call %v failed: %v", args["name"], err)
	}
	return result
}

// runTurn posts one question and returns the raw SSE body.
func (f *chatFixture) runTurn(t *testing.T, cookie, sessionID, turnID, content string) (int, string) {
	t.Helper()
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/turns",
		fmt.Sprintf(`{"turn_id":%q,"content":%q}`, turnID, content), cookie)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func TestChatRequiresAnAdministratorSession(t *testing.T) {
	f := newChatFixture(t)

	// No cookie at all.
	resp := f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}

	// A viewer may read their own (empty) library and list, but must not be able to bind a
	// billing key or ask a question: the key list is visible to them, so binding it would
	// turn "can look" into "can spend".
	readerCookie := f.login(t, "reader")
	list := f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions", "", readerCookie)
	if payload := decodeChatJSON(t, list); list.StatusCode != http.StatusOK {
		t.Fatalf("viewer list status = %d payload=%v", list.StatusCode, payload)
	}
	create := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions", fmt.Sprintf(
		`{"model":%q,"account_id":%d,"api_key_id":%d}`, f.model, f.accountID, f.keyID), readerCookie)
	payload := decodeChatJSON(t, create)
	if create.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create status = %d payload=%v", create.StatusCode, payload)
	}

	// The synthetic MCP actor has no administrator account, so it cannot reach the chat at
	// all — even for the read paths.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/chat/sessions", nil)
	req = withMCPActor(req, mcpActor{Username: "agent", Role: "admin", Scope: mcspAdminScope()})
	rec := httptest.NewRecorder()
	f.api.handleAdminChatListSessions(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MCP actor status = %d, want 403", rec.Code)
	}
}

func TestChatViewerRequestSpendsNothing(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	readerCookie := f.login(t, "reader")

	before := f.ledgerCount(t)
	for _, call := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/admin/api/v1/chat/sessions/" + sessionID + "/turns", `{"content":"这个月花了多少"}`},
		{http.MethodPost, "/admin/api/v1/chat/sessions/" + sessionID + "/skill-draft", `{}`},
		{http.MethodGet, "/admin/api/v1/chat/models?account_id=" + itoa64(f.accountID) + "&api_key_id=" + itoa64(f.keyID), ""},
	} {
		resp := f.call(t, call.method, call.path, call.body, readerCookie)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s as viewer = %d, want 403", call.method, call.path, resp.StatusCode)
		}
	}
	if after := f.ledgerCount(t); after != before {
		t.Fatalf("a refused request moved money: ledger entries %d -> %d", before, after)
	}
	// And nothing reached the model: no usage row was written for this account beyond what
	// existed before.
	rows, err := f.db.RequestUsages(ctx, []string{"req_anything"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("unexpected usage rows: %v", rows)
	}
}

func TestChatInterruptedTurnIsReportedNotReplayed(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)

	// Simulate a process that died in the middle of a turn that had already recorded a
	// pending tool call: the recovery pass must mark both, and a resend of the same turn id
	// must be answered from the interrupted record rather than executed again.
	if err := f.db.CreateChatTurn(ctx, &domain.ChatTurn{
		ID: "turn_row", SessionID: sessionID, TurnID: "turn_dead", Status: domain.ChatTurnRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateChatToolCall(ctx, &domain.ChatToolCall{
		ID: "tcall_1", SessionID: sessionID, TurnID: "turn_dead", Step: 1, CallID: "call_1",
		Name: "admin_update_model", Arguments: `{"name":"chat-echo"}`, Status: domain.ChatToolPending,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.api.RecoverChatTurns(ctx); err != nil || n != 1 {
		t.Fatalf("recovered = %d err=%v", n, err)
	}

	status, body := f.runTurn(t, cookie, sessionID, "turn_dead", "把模型停掉")
	if status != http.StatusOK {
		t.Fatalf("resend status = %d body=%s", status, body)
	}
	detail := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions/"+sessionID, "", cookie))
	calls, _ := detail["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %v", detail["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["status"] != domain.ChatToolUnknown {
		t.Fatalf("a pending call must be reported as unknown after a restart: %v", call)
	}
	// The write was never executed by the recovery, so the model is still enabled.
	model, err := f.db.GetModelByName(ctx, f.model)
	if err != nil {
		t.Fatal(err)
	}
	if !model.Enabled {
		t.Fatal("recovery must not replay a pending write")
	}
}

func TestChatTurnIsBilledAndRecordedAsConsoleMetadataOnly(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	// A skill whose instructions must not leak into any global record.
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/skills",
		`{"name":"成本排查","description":"按账户核对","instructions":"SECRET-INSTRUCTION-先查账户再查用量"}`, cookie)
	skill := decodeChatJSON(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create skill status = %d payload=%v", resp.StatusCode, skill)
	}
	skillID := int64(skill["id"].(float64))
	if resp := f.call(t, http.MethodPatch, "/admin/api/v1/chat/sessions/"+sessionID,
		fmt.Sprintf(`{"skill_ids":[%d]}`, skillID), cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("attach skill status = %d", resp.StatusCode)
	}

	status, body := f.runTurn(t, cookie, sessionID, "turn_bill", "SECRET-QUESTION 这个月花了多少")
	if status != http.StatusOK {
		t.Fatalf("turn status = %d body=%s", status, body)
	}
	if !strings.Contains(body, "event: text") || !strings.Contains(body, "event: done") {
		t.Fatalf("stream is missing its frames: %s", body)
	}

	// Settlement is batched; wait for it before reading money facts.
	deadline := time.Now().Add(5 * time.Second)
	var logged []*domain.RequestLogRecord
	for time.Now().Before(deadline) {
		logged = f.requestLogs(t)
		if len(logged) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(logged) == 0 {
		t.Fatal("the console turn wrote no request log row: it must be billed like any other request")
	}
	row := logged[0]
	if row.Client != "console" {
		t.Fatalf("client = %q, want console", row.Client)
	}
	if row.SessionID != sessionID {
		t.Fatalf("session id = %q, want the chat session", row.SessionID)
	}
	// The private material must not be in the global record: no request body, no output
	// text, no reasoning — while the identity, tokens and cost are still there.
	if row.RecordInputMode != "off" {
		t.Fatalf("record_input_mode = %q, want off", row.RecordInputMode)
	}
	if row.RequestJSON != "" {
		t.Fatalf("the request body was recorded: %q", row.RequestJSON)
	}
	if row.ResponseText != "" || row.ResponseReasoning != "" {
		t.Fatalf("content was recorded: text=%q reasoning=%q", row.ResponseText, row.ResponseReasoning)
	}
	if row.RecordOutputText || row.RecordReasoning {
		t.Fatalf("content switches were left on: %+v", row)
	}
	if row.Model != f.model || row.Status == "" {
		t.Fatalf("metadata is incomplete: %+v", row)
	}

	// Tokens and money are recorded normally.
	usages, err := f.db.RequestUsages(ctx, []string{row.RequestID})
	if err != nil {
		t.Fatal(err)
	}
	usage := usages[row.RequestID]
	if usage == nil || usage.InputTokens == 0 {
		t.Fatalf("no usage row for the console step: %v", usages)
	}

	// The conversation itself is stored, with the tool/answer parts and request ids.
	detail := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions/"+sessionID, "", cookie))
	messages, _ := detail["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("conversation messages = %d", len(messages))
	}
	assistant := messages[1].(map[string]any)
	if ids, ok := assistant["request_ids"].([]any); !ok || len(ids) != 1 || ids[0] != row.RequestID {
		t.Fatalf("message request ids = %v, want the step's request id", assistant["request_ids"])
	}
	if _, ok := assistant["charge_micros"]; !ok {
		t.Fatal("cost must be joined live from the usage records")
	}

	// The audit trail names the operations and the opaque ids, never the skill's text.
	entries := f.auditEntries(t)
	for _, entry := range entries {
		if strings.Contains(entry.ChangesJSON, "SECRET-INSTRUCTION") || strings.Contains(entry.TargetID, "SECRET") {
			t.Fatalf("a skill's text reached the global audit log: %+v", entry)
		}
		if entry.Action == "chat.skill_create" && entry.TargetID == "" {
			t.Fatal("skill audit rows must carry the opaque id")
		}
	}

	// Cross-owner visibility: the reader sees neither the conversation nor the skill.
	readerCookie := f.login(t, "reader")
	if resp := f.call(t, http.MethodGet, "/admin/api/v1/chat/sessions/"+sessionID, "", readerCookie); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another administrator read the conversation: %d", resp.StatusCode)
	}
	skills := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/skills", "", readerCookie))
	if data, _ := skills["data"].([]any); len(data) != 0 {
		t.Fatalf("another administrator's skill library = %v", data)
	}
	if resp := f.call(t, http.MethodPatch, "/admin/api/v1/chat/skills/"+itoa64(skillID),
		`{"name":"偷来的","instructions":"x"}`, readerCookie); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another administrator edited the skill: %d", resp.StatusCode)
	}
}

func TestChatSkillDraftIsBilledAndNeverStored(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)
	if status, body := f.runTurn(t, cookie, sessionID, "turn_1", "帮我看看这个月的成本"); status != http.StatusOK {
		t.Fatalf("turn status = %d body=%s", status, body)
	}
	before := len(f.requestLogs(t))
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/skill-draft", `{}`, cookie)
	draft := decodeChatJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("draft status = %d payload=%v", resp.StatusCode, draft)
	}
	if draft["instructions"] == "" {
		t.Fatalf("draft is empty: %v", draft)
	}
	// testecho answers with prose, so the fallback path is what runs here: it must say so.
	if note, _ := draft["note"].(string); !strings.Contains(note, "骨架") {
		t.Fatalf("an unparseable draft must explain itself: %v", draft)
	}
	if draft["request_id"] == "" {
		t.Fatal("the draft must report the billed request id")
	}
	// The draft itself is not stored: the library is still empty until the operator saves.
	skills := decodeChatJSON(t, f.call(t, http.MethodGet, "/admin/api/v1/chat/skills", "", cookie))
	if data, _ := skills["data"].([]any); len(data) != 0 {
		t.Fatalf("a draft was stored without the operator asking: %v", data)
	}
	// And it was billed: the draft call is a step like any other.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(f.requestLogs(t)) <= before {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.requestLogs(t)) <= before {
		t.Fatal("distilling a skill wrote no request log row")
	}
}

func TestChatPreviewTicketsGateEveryRead(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)

	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/artifacts",
		`{"key":"msg_1:0","format":"html","title":"示例","body":"<h1>hello</h1><script>document.title='x'</script>"}`, cookie)
	artifact := decodeChatJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("artifact status = %d payload=%v", resp.StatusCode, artifact)
	}
	url, _ := artifact["url"].(string)
	ticket, _ := artifact["ticket"].(string)
	if url == "" || ticket == "" {
		t.Fatalf("artifact response incomplete: %v", artifact)
	}

	// With the ticket: served, sandboxed, never cached.
	ok := f.call(t, http.MethodGet, url+"?ticket="+ticket, "", "")
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("preview with a ticket = %d", ok.StatusCode)
	}
	csp := ok.Header.Get("Content-Security-Policy")
	for _, want := range []string{"sandbox allow-scripts", "default-src 'none'", "connect-src 'none'", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("preview CSP is missing %q: %s", want, csp)
		}
	}
	if got := ok.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if ct := ok.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(ok.Body)
	ok.Body.Close()
	if !strings.Contains(string(body), "hello") {
		t.Fatal("the preview did not serve the uploaded payload")
	}

	// Without a ticket, or with a tampered one: nothing is served, and the answer does not
	// reveal whether the artifact exists.
	for _, target := range []string{url, url + "?ticket=forged", url + "?ticket=" + ticket + "x"} {
		denied := f.call(t, http.MethodGet, target, "", "")
		denied.Body.Close()
		if denied.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, denied.StatusCode)
		}
	}

	// Logging out revokes outstanding previews: the ticket names a session, and that
	// session is what gets checked.
	if resp := f.call(t, http.MethodPost, "/admin/api/v1/auth/logout", `{}`, cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}
	revoked := f.call(t, http.MethodGet, url+"?ticket="+ticket, "", "")
	revoked.Body.Close()
	if revoked.StatusCode != http.StatusNotFound {
		t.Fatalf("a preview survived the logout that created it: %d", revoked.StatusCode)
	}
}

func TestChatPreviewTicketIsReissuedAndBounded(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)

	put := decodeChatJSON(t, f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/artifacts",
		`{"key":"block","format":"svg","body":"<svg xmlns='http://www.w3.org/2000/svg'/>"}`, cookie))
	id := put["id"].(string)
	fresh := decodeChatJSON(t, f.call(t, http.MethodPost,
		"/admin/api/v1/chat/sessions/"+sessionID+"/artifacts/"+id+"/ticket", `{}`, cookie))
	if fresh["ticket"] == "" {
		t.Fatalf("no fresh ticket: %v", fresh)
	}
	svg := f.call(t, http.MethodGet, "/admin/chat-artifact/"+id+"?ticket="+fresh["ticket"].(string), "", "")
	svg.Body.Close()
	if svg.StatusCode != http.StatusOK {
		t.Fatalf("svg preview = %d", svg.StatusCode)
	}
	if ct := svg.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("svg Content-Type = %q", ct)
	}
	// An SVG needs no scripting at all, so it does not get the allowance HTML requires.
	if csp := svg.Header.Get("Content-Security-Policy"); strings.Contains(csp, "allow-scripts") {
		t.Fatalf("an SVG preview must not be allowed to run scripts: %s", csp)
	}

	// The payload limit is enforced before anything is stored.
	big := strings.Repeat("x", f.cfg.Chat.ArtifactMaxBytes+1)
	tooBig := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions/"+sessionID+"/artifacts",
		fmt.Sprintf(`{"key":"big","format":"html","body":%q}`, big), cookie)
	tooBig.Body.Close()
	if tooBig.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized preview = %d, want 400", tooBig.StatusCode)
	}
	// An expired ticket stops working. The TTL is read when the ticket is signed, so the
	// configuration is shortened before asking for a new one.
	short := *f.cfg
	short.Chat.ArtifactTicketTTL = time.Millisecond
	f.api.deps.Config = &short
	quick := decodeChatJSON(t, f.call(t, http.MethodPost,
		"/admin/api/v1/chat/sessions/"+sessionID+"/artifacts/"+id+"/ticket", `{}`, cookie))
	time.Sleep(10 * time.Millisecond)
	expired := f.call(t, http.MethodGet, "/admin/chat-artifact/"+id+"?ticket="+quick["ticket"].(string), "", "")
	expired.Body.Close()
	if expired.StatusCode != http.StatusNotFound {
		t.Fatalf("an expired ticket still worked: %d", expired.StatusCode)
	}
}

// The console chat is an MCP client: what it may do is exactly what the MCP token it is
// bound to may do. These tests pin that down from both sides — an admin-scope token drives
// every console operation, and a narrower token (or a revoked one) is refused by the same
// rules an external MCP client meets, because both go through POST /mcp.
func TestChatAdminTokenExecutesEveryEndpoint(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	ctx := context.Background()
	access := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s1",
	}

	// A read endpoint works.
	read := f.callTool(t, ctx, tools, access, map[string]any{"name": "admin_list_keys"})
	if read.IsError {
		t.Fatalf("admin_list_keys reported an error: %+v", read.Value)
	}

	// The write the old console allowlist used to refuse. Creating an account is not marked
	// dangerous, so it needs no confirm.
	created := f.callTool(t, ctx, tools, access, map[string]any{
		"name": "admin_create_account", "body": map[string]any{"name": "ranqiliang"},
	})
	if created.IsError {
		t.Fatalf("admin_create_account must be callable from the console chat: %+v", created.Value)
	}
	if _, err := f.db.GetAccountByName(ctx, "ranqiliang"); err != nil {
		t.Fatalf("the account was not created: %v", err)
	}

	// Credential issuance is the case the old design blocked outright. It is reachable now,
	// but only with confirm=true, and the plaintext comes back with a one-time warning.
	unconfirmed := f.callTool(t, ctx, tools, access, map[string]any{
		"name": "admin_create_key", "body": map[string]any{"name": "k1", "account_id": f.accountID},
	})
	if !unconfirmed.IsError {
		t.Fatalf("a dangerous endpoint must require confirm: %+v", unconfirmed.Value)
	}
	if text, _ := unconfirmed.Value.(string); !strings.Contains(text, "confirm=true") {
		t.Fatalf("the refusal does not ask for confirmation: %v", unconfirmed.Value)
	}

	issued := f.callTool(t, ctx, tools, access, map[string]any{
		"name": "admin_create_key", "confirm": true,
		"body": map[string]any{"name": "k1", "account_id": f.accountID},
	})
	if issued.IsError {
		t.Fatalf("admin_create_key with confirm failed: %+v", issued.Value)
	}
	text, _ := issued.Value.(string)
	if !strings.Contains(text, "sk-gw_") {
		t.Fatalf("the issued key is missing from the result: %s", text)
	}
	if !strings.Contains(text, "只显示这一次") {
		t.Fatalf("the one-time-credential warning is missing: %s", text)
	}
}

// A narrower token cannot reach what its scope forbids, and the refusal comes from the MCP
// scope rule rather than from a console-specific second implementation.
func TestChatTokenScopeBoundsWhatItCanDo(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	ctx := context.Background()
	write := map[string]any{
		"name": "admin_update_model", "params": map[string]any{"name": "chat-echo"},
		"body": map[string]any{"enabled": true},
	}

	readAccess := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeReadOnly, MCPTokenID: f.readTokenID, SessionID: "s2",
	}
	ok := f.callTool(t, ctx, tools, readAccess, map[string]any{"name": "admin_list_keys"})
	if ok.IsError {
		t.Fatalf("an admin_read token must still read: %+v", ok.Value)
	}
	refused := f.callTool(t, ctx, tools, readAccess, write)
	if !refused.IsError {
		t.Fatalf("an admin_read token executed a write: %+v", refused.Value)
	}
	if text, _ := refused.Value.(string); !strings.Contains(text, "scope=admin") {
		t.Fatalf("the refusal does not name the missing scope: %v", refused.Value)
	}

	// A query token gets the account-scoped query tools and no administrative surface at all.
	queryAccess := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeReadOnly, MCPTokenID: f.queryTokenID, SessionID: "s3",
	}
	for _, tool := range tools.List(queryAccess) {
		if strings.HasPrefix(tool.Name, "admin_") {
			t.Fatalf("a query token was offered %q", tool.Name)
		}
	}
	hidden := f.callTool(t, ctx, tools, queryAccess, map[string]any{"name": "admin_list_keys"})
	if !hidden.IsError {
		t.Fatalf("a query token reached the administrative surface: %+v", hidden.Value)
	}

	// An admin token sees the endpoints that exist, including credential issuance, and the
	// console-only restriction is gone.
	adminAccess := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s4",
	}
	overview, err := tools.Call(ctx, adminAccess, toolAdminEndpoints, map[string]any{})
	if err != nil {
		t.Fatalf("admin_endpoints failed: %v", err)
	}
	encoded, _ := json.Marshal(overview.Value)
	if !strings.Contains(string(encoded), "admin_create_key") {
		t.Fatalf("an admin token cannot see credential issuance in the catalogue: %s", encoded)
	}
	if strings.Contains(string(encoded), "unavailable_from_console_chat") {
		t.Fatalf("the console-only restriction is still advertised: %s", encoded)
	}
	detail, err := tools.Call(ctx, adminAccess, toolAdminDescribe, map[string]any{"name": "admin_create_key"})
	if err != nil {
		t.Fatalf("admin_describe failed: %v", err)
	}
	if detail.IsError {
		t.Fatalf("admin_describe must explain credential issuance now: %+v", detail.Value)
	}
}

// The tool surface is exactly the bound token's: the administrative entry points on top of
// the query tools, and nothing at all without a token.
func TestChatToolSurfaceFollowsTheBoundToken(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	base := chat.Access{OwnerID: 1, Username: "admin", Role: chat.RoleAdmin, WriteMode: domain.ChatWriteModeAllow}

	withAdmin := base
	withAdmin.MCPTokenID = f.adminTokenID
	listed := tools.List(withAdmin)
	if len(listed) != 14 {
		t.Fatalf("admin-scope tool surface = %d tools, want 11 query + 3 administrative", len(listed))
	}
	names := map[string]bool{}
	for _, tool := range listed {
		names[tool.Name] = true
		if len(tool.Schema) == 0 {
			t.Fatalf("tool %q reached the model without a schema", tool.Name)
		}
	}
	for _, want := range []string{"admin_endpoints", "admin_describe", "admin_request", "get_balance"} {
		if !names[want] {
			t.Fatalf("tool %q is missing from the surface", want)
		}
	}

	withQuery := base
	withQuery.MCPTokenID = f.queryTokenID
	if got := len(tools.List(withQuery)); got != 11 {
		t.Fatalf("query-scope tool surface = %d tools, want 11", got)
	}

	// An unbound conversation has no tools, and calling anything explains what to do.
	unbound := base
	result, err := tools.Call(context.Background(), unbound, toolAdminRequest, map[string]any{"name": "admin_list_keys"})
	if err != nil {
		t.Fatalf("an unbound conversation must answer, not fail the turn: %v", err)
	}
	if !result.IsError {
		t.Fatal("an unbound conversation executed a call")
	}
	if text, _ := result.Value.(string); !strings.Contains(text, "not bound to an MCP token") {
		t.Fatalf("the unbound refusal is not actionable: %v", result.Value)
	}
}

// A model may call a query tool by its own name (they are first-class tools, listed
// alongside the administrative entry points). The call must reach the query handler, not be
// misrouted into admin_request — which would answer "unknown endpoint" and make the model
// believe the read-only tools do not exist.
func TestChatToolCallsQueryToolsDirectly(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	ctx := context.Background()
	access := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s6",
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string // a substring only the query handler produces
	}{
		{"get_balance", map[string]any{}, "balance"},
		{"get_usage_summary", map[string]any{"period": "last_7_days"}, "attempts"},
		{"get_usage_breakdown", map[string]any{"period": "last_7_days", "group_by": "key"}, "group_by"},
		{"get_dashboard", map[string]any{"period": "last_7_days"}, "requests"},
		{"get_models", map[string]any{}, "models"},
		{"list_requests", map[string]any{"period": "last_7_days"}, "requests"},
	} {
		result, err := tools.Call(ctx, access, tc.name, tc.args)
		if err != nil {
			t.Fatalf("%s failed: %v", tc.name, err)
		}
		if result.IsError {
			t.Fatalf("%s was reported as an error: %v", tc.name, result.Value)
		}
		text, _ := result.Value.(string)
		if strings.Contains(text, "unknown endpoint") || strings.Contains(text, "unknown tool") {
			t.Fatalf("%s was misrouted through admin_request: %s", tc.name, text)
		}
		if !strings.Contains(text, tc.want) {
			t.Fatalf("%s response lacks %q: %s", tc.name, tc.want, text)
		}
	}
}

// Revoking the token ends the conversation's authority at the next call. This is the whole
// reason scope is re-read per call instead of snapshotted onto the session.
func TestChatRevokedOrExpiredTokenStopsTheConversation(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	ctx := context.Background()
	access := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s5",
	}

	if got := f.callTool(t, ctx, tools, access, map[string]any{"name": "admin_list_keys"}); got.IsError {
		t.Fatalf("the bound token must work before revocation: %+v", got.Value)
	}
	if err := f.db.RevokeMCPToken(ctx, f.adminTokenID); err != nil {
		t.Fatal(err)
	}
	revoked := f.callTool(t, ctx, tools, access, map[string]any{"name": "admin_list_keys"})
	if !revoked.IsError {
		t.Fatalf("a revoked token still executed a call: %+v", revoked.Value)
	}
	if text, _ := revoked.Value.(string); !strings.Contains(text, "not active") {
		t.Fatalf("the revoked refusal does not match the MCP wording: %v", revoked.Value)
	}

	// The same holds for expiry.
	expiredID := newMCPToken(t, f.db, f.accountID, "chat-expiring", mcpsrv.ScopeAdmin)
	if _, err := f.db.Writer().ExecContext(ctx,
		"UPDATE mcp_tokens SET expires_at = unixepoch('now') - 60 WHERE id = ?", expiredID); err != nil {
		t.Fatal(err)
	}
	expiring := access
	expiring.MCPTokenID = expiredID
	expired := f.callTool(t, ctx, tools, expiring, map[string]any{"name": "admin_list_keys"})
	if !expired.IsError {
		t.Fatalf("an expired token still executed a call: %+v", expired.Value)
	}
	if text, _ := expired.Value.(string); !strings.Contains(text, "expired") {
		t.Fatalf("the expiry refusal does not explain itself: %v", expired.Value)
	}
}

// A conversation stores the token's id and nothing else: no plaintext, no hash. The
// credential stays in the row that issued it.
func TestChatSessionStoresOnlyTheTokenID(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	sessionID := f.createSession(t, cookie)

	session, err := f.db.GetChatSession(context.Background(), sessionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if session.MCPTokenID == nil || *session.MCPTokenID != f.adminTokenID {
		t.Fatalf("session token binding = %v, want %d", session.MCPTokenID, f.adminTokenID)
	}
	if session.WriteMode != domain.ChatWriteModeAllow {
		t.Fatalf("write mode = %q, want it derived as %q from the admin-scope token",
			session.WriteMode, domain.ChatWriteModeAllow)
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"aigw_mcp_", "token_hash", "TokenHash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the session row carries %q: %s", forbidden, encoded)
		}
	}

	// Binding is rejected outright when the token is not usable, rather than accepted and
	// failing later on the first question.
	if err := f.db.RevokeMCPToken(context.Background(), f.readTokenID); err != nil {
		t.Fatal(err)
	}
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions", fmt.Sprintf(
		`{"model":%q,"account_id":%d,"api_key_id":%d,"mcp_token_id":%d}`,
		f.model, f.accountID, f.keyID, f.readTokenID), cookie)
	payload := decodeChatJSON(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("binding a revoked token = %d %v, want 403", resp.StatusCode, payload)
	}
}

// A conversation with no token cannot be created: the token is what the tool surface is
// derived from, so "no token" would mean "a chat that can do nothing".
func TestChatSessionRequiresAToken(t *testing.T) {
	f := newChatFixture(t)
	cookie := f.login(t, "admin")
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions", fmt.Sprintf(
		`{"model":%q,"account_id":%d,"api_key_id":%d}`, f.model, f.accountID, f.keyID), cookie)
	payload := decodeChatJSON(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d %v, want 400", resp.StatusCode, payload)
	}
	if message := fmt.Sprint(payload["error"]); !strings.Contains(message, "MCP token") {
		t.Fatalf("the error does not say what is missing: %v", payload)
	}
}

// A write made from the console is attributed to the MCP token, because the console is
// acting as that token — the same identity an external client would produce.
func TestChatWriteIsAuditedAsTheToken(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	access := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s6",
	}
	f.callTool(t, context.Background(), tools, access, map[string]any{
		"name": "admin_update_model", "params": map[string]any{"name": "chat-echo"},
		"body": map[string]any{"enabled": false},
	})
	var actor string
	for _, entry := range f.auditEntries(t) {
		if entry.Action == "mcp.admin_call" {
			actor = entry.Actor
		}
	}
	want := fmt.Sprintf("mcp:chat-admin#%d", f.adminTokenID)
	if actor != want {
		t.Fatalf("audit actor = %q, want %q", actor, want)
	}
}

// A model that collapses the two-level convention and calls a management endpoint by its own
// name must get the call it meant — this is what the console actually saw in production
// ("unknown tool \"admin_request_dimensions\"" while the model was holding a perfectly good
// endpoint name and a matching argument shape).
func TestChatToolAcceptsEndpointNamesAsToolNames(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api, token: f.api.deps.MCPTokens}
	access := chat.Access{
		OwnerID: 1, Username: "admin", Role: chat.RoleAdmin,
		WriteMode: domain.ChatWriteModeAllow, MCPTokenID: f.adminTokenID, SessionID: "s7",
	}
	ctx := context.Background()

	result, err := tools.Call(ctx, access, "admin_request_dimensions", map[string]any{
		"query": map[string]any{"days": 30, "group_by": "api_key", "limit": 50, "sort": "requests"},
	})
	if err != nil {
		t.Fatalf("an endpoint name must be routed to admin_request: %v", err)
	}
	if result.IsError {
		t.Fatalf("the routed call failed: %+v", result.Value)
	}

	// The same routing works for a write, which is what an operator now expects to be able to
	// ask for in plain language.
	routed := f.callTool(t, ctx, tools, access, map[string]any{
		"name": "admin_create_account", "body": map[string]any{"name": "routed-account"},
	})
	if routed.IsError {
		t.Fatalf("a named write endpoint was not routed: %+v", routed.Value)
	}
	if _, err := f.db.GetAccountByName(ctx, "routed-account"); err != nil {
		t.Fatalf("the routed write did not reach the endpoint: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (f *chatFixture) requestLogs(t *testing.T) []*domain.RequestLogRecord {
	t.Helper()
	rows, err := f.db.ListRequestLogs(context.Background(), domain.RequestLogFilter{}, 20)
	if err != nil {
		t.Fatalf("list request logs: %v", err)
	}
	return rows
}

func (f *chatFixture) ledgerCount(t *testing.T) int {
	t.Helper()
	entries, err := f.db.ListLedger(context.Background(), f.accountID,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 200)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	return len(entries)
}

// chatAuditEntry is the slice of an audit row these tests look at.
type chatAuditEntry struct {
	Actor       string
	Action      string
	TargetType  string
	TargetID    string
	ChangesJSON string
	Result      string
}

func (f *chatFixture) auditEntries(t *testing.T) []chatAuditEntry {
	t.Helper()
	entries, err := f.db.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	out := make([]chatAuditEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, chatAuditEntry{
			Actor: entry.Actor, Action: entry.Action, TargetType: entry.TargetType,
			TargetID: entry.TargetID, ChangesJSON: entry.ChangesJSON, Result: entry.Result,
		})
	}
	return out
}

func itoa64(v int64) string { return fmt.Sprintf("%d", v) }

func mcspAdminScope() string { return mcpsrv.ScopeAdmin }

func quotaLimiter() *quota.Limiter { return quota.New(4) }

// The chat service must not import internal/mcpsrv (see the layering table in
// docs/architecture.md), so it spells the three scope names itself and derives write mode
// from them. This is the one package where both vocabularies are legitimately visible, so the
// equivalence is asserted here: if either side is renamed, the chat would silently stop
// honouring admin tokens and every conversation would become read-only.
func TestChatScopeVocabularyMatchesMCP(t *testing.T) {
	cases := []struct {
		scope string
		want  string
	}{
		{mcpsrv.ScopeAdmin, domain.ChatWriteModeAllow},
		{mcpsrv.ScopeAdminRead, domain.ChatWriteModeReadOnly},
		{mcpsrv.ScopeQuery, domain.ChatWriteModeReadOnly},
		{"", domain.ChatWriteModeReadOnly},
		{"ADMIN", domain.ChatWriteModeReadOnly}, // only the exact spelling counts
	}
	for _, tc := range cases {
		got := chat.WriteModeForScope(tc.scope)
		if got != tc.want {
			t.Errorf("chat write mode for scope %q = %q, want %q", tc.scope, got, tc.want)
		}
	}
	// The scope the chat treats as writable must be the one the MCP service calls admin.
	if chat.WritableScope() != mcpsrv.ScopeAdmin {
		t.Fatalf("chat writable scope = %q, want %q", chat.WritableScope(), mcpsrv.ScopeAdmin)
	}
}

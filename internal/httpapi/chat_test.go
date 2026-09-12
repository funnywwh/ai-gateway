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
}

const chatPassword = "chat-secret-1"

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
	return &chatFixture{server: ts, api: srv, db: db, cfg: &cfg, service: service, accountID: accountID, keyID: keyID, model: model}
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

// createSession makes a conversation bound to the fixture's billed key.
func (f *chatFixture) createSession(t *testing.T, cookie string) string {
	t.Helper()
	resp := f.call(t, http.MethodPost, "/admin/api/v1/chat/sessions", fmt.Sprintf(
		`{"model":%q,"account_id":%d,"api_key_id":%d}`, f.model, f.accountID, f.keyID), cookie)
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

func TestChatToolsHideAndRefuseHighRiskEndpoints(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api}
	access := chat.Access{OwnerID: 1, Username: "admin", Role: chat.RoleAdmin, WriteMode: domain.ChatWriteModeAllow}

	listed := tools.List(access)
	if len(listed) != 3 {
		t.Fatalf("tool surface = %d tools, want admin_endpoints/admin_describe/admin_request", len(listed))
	}

	ctx := context.Background()
	// A read endpoint is available.
	read, err := tools.Call(ctx, access, toolAdminRequest, map[string]any{"name": "admin_list_keys"})
	if err != nil {
		t.Fatalf("a read endpoint must be callable from the chat: %v", err)
	}
	if read.IsError {
		t.Fatalf("admin_list_keys reported an error: %+v", read)
	}

	// Credential issuance is refused even though the conversation allows writes and the
	// caller is an administrator.
	for _, name := range []string{"admin_create_key", "admin_create_mcp_token", "admin_run_backup", "admin_restore_backup", "admin_upsert_hook", "admin_grant_credits", "admin_prune_requests", "admin_put_setting"} {
		result, err := tools.Call(ctx, access, toolAdminRequest, map[string]any{"name": name, "confirm": true})
		if err == nil && !result.IsError {
			t.Fatalf("%s was callable from the console chat", name)
		}
		message := ""
		if err != nil {
			message = err.Error()
		} else if text, ok := result.Value.(string); ok {
			message = text
		}
		if err == nil && !strings.Contains(fmt.Sprint(result.Value), "console chat") {
			t.Fatalf("%s refusal does not explain itself: %v", name, result.Value)
		}
		if err != nil && !strings.Contains(message, "console chat") {
			t.Fatalf("%s refusal does not explain itself: %v", name, err)
		}
	}

	// The allowlisted configuration writes stay available.
	updateModel := map[string]any{
		"name": "admin_update_model", "params": map[string]any{"name": "chat-echo"},
		"body": map[string]any{"enabled": true},
	}
	allowed, err := tools.Call(ctx, access, toolAdminRequest, updateModel)
	if err != nil {
		t.Fatalf("an allowlisted write must remain available: %v", err)
	}
	if allowed.IsError {
		t.Fatalf("admin_update_model reported an error: %+v", allowed)
	}

	// A read-only conversation gets the read-only scope, so writes are refused by the
	// existing scope rule rather than by a second implementation.
	readOnly := access
	readOnly.WriteMode = domain.ChatWriteModeReadOnly
	if _, err := tools.Call(ctx, readOnly, toolAdminRequest, updateModel); err == nil {
		t.Fatal("a read-only conversation executed a write")
	}
	// A viewer's conversation is read-only even if the switch says otherwise.
	viewer := access
	viewer.Role = chat.RoleViewer
	if _, err := tools.Call(ctx, viewer, toolAdminRequest, updateModel); err == nil {
		t.Fatal("a viewer executed a write inside a write-enabled conversation")
	}

	// The catalogue the model sees does not advertise what it may not call, and says how
	// many endpoints are hidden.
	overview, err := tools.Call(ctx, access, toolAdminEndpoints, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(overview.Value)
	if strings.Contains(string(encoded), "admin_create_key") {
		t.Fatalf("a hidden endpoint is still advertised: %s", encoded)
	}
	if !strings.Contains(string(encoded), "unavailable_from_console_chat") {
		t.Fatalf("the catalogue does not mention the restriction: %s", encoded)
	}
	if describe, err := tools.Call(ctx, access, toolAdminDescribe, map[string]any{"name": "admin_create_key"}); err == nil {
		t.Fatalf("admin_describe explained a hidden endpoint: %v", describe.Value)
	}
}

// A model that collapses the two-level convention and calls a management endpoint by its own
// name must get the call it meant — this is what the console actually saw in production
// ("unknown tool \"admin_request_dimensions\"" while the model was holding a perfectly good
// endpoint name and a matching argument shape).
func TestChatToolAcceptsEndpointNamesAsToolNames(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api}
	access := chat.Access{OwnerID: 1, Username: "admin", Role: chat.RoleAdmin, WriteMode: domain.ChatWriteModeAllow}
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
	payload, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result shape: %T", result.Value)
	}
	if payload["endpoint"] != "admin_request_dimensions" {
		t.Fatalf("the call did not reach the endpoint: %v", payload)
	}

	// The routing goes through the same allowlist: an endpoint the chat may not call is
	// still refused when it is named directly.
	if _, err := tools.Call(ctx, access, "admin_create_key", map[string]any{"body": map[string]any{"name": "x"}}); err == nil {
		t.Fatal("naming a forbidden endpoint directly bypassed the allowlist")
	}
	// And a name that is neither a tool nor an endpoint is answered with what does exist.
	_, err = tools.Call(ctx, access, "admin_make_me_a_sandwich", nil)
	if err == nil || !strings.Contains(err.Error(), "admin_request") {
		t.Fatalf("unknown tool error = %v, want it to name the real tools", err)
	}
}

func TestChatActorIdentityIsTheConsoleNotAToken(t *testing.T) {
	f := newChatFixture(t)
	tools := &chatTools{s: f.api}
	access := chat.Access{OwnerID: 1, Username: "admin", Role: chat.RoleAdmin, WriteMode: domain.ChatWriteModeAllow}
	if _, err := tools.Call(context.Background(), access, toolAdminRequest, map[string]any{
		"name": "admin_update_model", "params": map[string]any{"name": "chat-echo"},
		"body": map[string]any{"enabled": true},
	}); err != nil {
		t.Fatal(err)
	}
	entries := f.auditEntries(t)
	var actor string
	for _, entry := range entries {
		if entry.Action == "mcp.admin_call" {
			actor = entry.Actor
		}
	}
	if actor != "console:admin" {
		t.Fatalf("audit actor = %q, want console:admin", actor)
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

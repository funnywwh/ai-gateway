package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/apikey"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/secret"
	"github.com/winger/ai-gateway/internal/store"
	"github.com/winger/ai-gateway/internal/usage"
	"github.com/winger/ai-gateway/pkg/pluginapi"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

const testToken = "sk-gw-httpapi-test-token-0001"

type fixture struct {
	server   *httptest.Server
	db       *store.DB
	cfg      *config.Config
	key      *domain.APIKey
	verifier *apikey.Verifier
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "httpapi.db")
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accID, err := db.UpsertAccount(ctx, &domain.Account{
		Name: "acme", BillingMode: domain.BillingPostpaid, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	key := &domain.APIKey{
		AccountID: accID, Name: "dev",
		KeyPrefix: secret.Prefix(testToken), KeyHash: secret.Hash(testToken),
		Status: "active", RecordInputMode: "inherit",
	}
	if _, err := db.UpsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetAPIKeyByPrefix(ctx, secret.Prefix(testToken))
	if err != nil {
		t.Fatal(err)
	}
	key = stored

	provID, err := db.UpsertProvider(ctx, &domain.Provider{
		Name: "echo", Kind: "testecho", Enabled: true, Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: provID, PublicModel: "echo-model", UpstreamModel: "echo-model", Enabled: true,
		MaxOutputTokens: 1024, CapabilitiesJSON: capabilitiesJSON,
	}); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel(ctx, &domain.Model{PublicName: "echo-model", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRoute(ctx, &domain.Route{
		ModelID: modelID, ProviderID: provID, Priority: 10, Weight: 100, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertTag(ctx, &domain.Tag{
		Name: "free", GrantsJSON: grantsAll, Priority: 10,
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

	verifier := apikey.New(db, apikey.DefaultConfig())
	mcpService := mcpsrv.New(db, reg, mcpsrv.Config{MaxRows: 100, WindowDays: 30, Currency: "USD"})
	srv := New(Deps{
		Config:     &cfg,
		Registry:   reg,
		Router:     router,
		Dispatcher: dispatcher,
		Verifier:   verifier,
		Limiter:    quota.New(8),
		Meter:      usage.New(db),
		Records:    db,
		MCP:        mcpService,
		MCPTokens:  db,
		Version:    "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{server: ts, db: db, cfg: &cfg, key: key, verifier: verifier}
}

const (
	capabilitiesJSON = `{"stream":true,"tools":true}`
	grantsAll        = `{"models":["*"],"providers":["*"]}`
	nonStreamBody    = `{"model":"echo-model","input":"ping"}`
)

func (f *fixture) do(t testing.TB, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeError(t *testing.T, resp *http.Response) (status int, code string) {
	t.Helper()
	defer resp.Body.Close()
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding error payload: %v", err)
	}
	return resp.StatusCode, payload.Error.Code
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	f := newFixture(t)

	req, _ := http.NewRequest("POST", f.server.URL+"/v1/responses", strings.NewReader(nonStreamBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if status, code := decodeError(t, resp); status != 401 || code != "invalid_api_key" {
		t.Fatalf("expected 401 invalid_api_key, got %d %s", status, code)
	}

	req, _ = http.NewRequest("POST", f.server.URL+"/v1/responses", strings.NewReader(nonStreamBody))
	req.Header.Set("Authorization", "Bearer sk-gw-wrong-token-0000000000")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := decodeError(t, resp); status != 401 {
		t.Fatalf("expected 401 for a wrong token, got %d", status)
	}
}

func TestValidationRejectionsDoNotReachUpstream(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ name, body string }{
		{"background", `{"model":"echo-model","input":"hi","background":true}`},
		{"max_output_tokens", `{"model":"echo-model","input":"hi","max_output_tokens":8}`},
		{"empty input", `{"model":"echo-model","input":""}`},
		{"missing model", `{"input":"hi"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.do(t, "POST", "/v1/responses", tc.body, nil)
			status, code := decodeError(t, resp)
			if status != 400 {
				t.Fatalf("expected 400, got %d (%s)", status, code)
			}
		})
	}

	// Local rejections must not create usage records.
	rows, err := f.db.ListUsage(context.Background(), f.key.AccountID, time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("local rejections must not be metered, got %d rows", len(rows))
	}
}

func TestNonStreamingResponseRoundTrip(t *testing.T) {
	f := newFixture(t)
	resp := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("x-gateway-model"); got != "echo-model" {
		t.Fatalf("x-gateway-model = %q", got)
	}
	if got := resp.Header.Get("x-gateway-provider"); got != "echo" {
		t.Fatalf("x-gateway-provider = %q", got)
	}

	var payload struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "response" || payload.Status != "completed" {
		t.Fatalf("response envelope mismatch: %+v", payload)
	}
	if !strings.HasPrefix(payload.ID, "resp_") {
		t.Fatalf("unexpected response id: %q", payload.ID)
	}
	if len(payload.Output) != 1 || len(payload.Output[0].Content) == 0 {
		t.Fatalf("output mismatch: %+v", payload.Output)
	}
	if !strings.Contains(payload.Output[0].Content[0].Text, "ping") {
		t.Fatalf("expected the echo to contain the input, got %q", payload.Output[0].Content[0].Text)
	}
	if payload.Usage.TotalTokens == 0 || payload.Usage.TotalTokens != payload.Usage.InputTokens+payload.Usage.OutputTokens {
		t.Fatalf("usage mismatch: %+v", payload.Usage)
	}

	// The response is retrievable and deletable by the owning key.
	getResp := f.do(t, "GET", "/v1/responses/"+payload.ID, "", nil)
	if getResp.StatusCode != 200 {
		t.Fatalf("GET stored response = %d", getResp.StatusCode)
	}
	var stored map[string]any
	if err := json.NewDecoder(getResp.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if stored["id"] != payload.ID || stored["status"] != "completed" {
		t.Fatalf("stored response mismatch: %+v", stored)
	}

	delResp := f.do(t, "DELETE", "/v1/responses/"+payload.ID, "", nil)
	if delResp.StatusCode != 200 {
		t.Fatalf("DELETE = %d", delResp.StatusCode)
	}
	delResp.Body.Close()
	after := f.do(t, "GET", "/v1/responses/"+payload.ID, "", nil)
	if after.StatusCode != 404 {
		t.Fatalf("deleted response must be gone, got %d", after.StatusCode)
	}
	after.Body.Close()
}

func TestStreamingEventSequence(t *testing.T) {
	f := newFixture(t)
	resp := f.do(t, "POST", "/v1/responses",
		`{"model":"echo-model","input":"stream me","stream":true}`, nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	type frame struct {
		Name string
		Data []byte
	}
	var frames []frame
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var current frame
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			current.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = []byte(strings.TrimPrefix(line, "data: "))
		case line == "":
			if current.Name != "" {
				frames = append(frames, current)
			}
			current = frame{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	order := []string{}
	lastSeq := 0
	var text strings.Builder
	for _, fr := range frames {
		var ev struct {
			Type           string `json:"type"`
			SequenceNumber int    `json:"sequence_number"`
			Delta          string `json:"delta"`
		}
		if err := json.Unmarshal(fr.Data, &ev); err != nil {
			t.Fatalf("bad event payload %q: %v", fr.Data, err)
		}
		if ev.Type != fr.Name {
			t.Fatalf("event name %q != payload type %q", fr.Name, ev.Type)
		}
		if ev.SequenceNumber <= lastSeq {
			t.Fatalf("sequence numbers must increase: %d after %d", ev.SequenceNumber, lastSeq)
		}
		lastSeq = ev.SequenceNumber
		order = append(order, ev.Type)
		if ev.Type == "response.output_text.delta" {
			text.WriteString(ev.Delta)
		}
	}

	if len(order) < 6 {
		t.Fatalf("too few events: %v", order)
	}
	for _, want := range []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "response.completed",
	} {
		found := false
		for _, got := range order {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing event %q in %v", want, order)
		}
	}
	if order[0] != "response.created" || order[len(order)-1] != "response.completed" {
		t.Fatalf("first/last events wrong: %v", order)
	}
	if !strings.Contains(text.String(), "stream me") {
		t.Fatalf("streamed text mismatch: %q", text.String())
	}
}

func TestRateLimitReturns429WithHeaders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := f.key
	key.PolicyJSON = `{"rpm":1}`
	if _, err := f.db.UpsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}

	first := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first request must succeed, got %d", first.StatusCode)
	}

	second := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	defer second.Body.Close()
	if second.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", second.StatusCode)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Error("Retry-After header missing")
	}
	if second.Header.Get("x-ratelimit-limit-requests") != "1" {
		t.Errorf("x-ratelimit-limit-requests = %q", second.Header.Get("x-ratelimit-limit-requests"))
	}
}

func TestModelsEndpointExposesSalePriceOnly(t *testing.T) {
	f := newFixture(t)
	resp := f.do(t, "GET", "/v1/models", "", nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	// Sale prices only: no cost figures and no upstream/provider identifiers.
	for _, forbidden := range []string{"cost_micros", "price_in", "price_out", "upstream", "provider"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("GET /v1/models must never expose %q: %s", forbidden, body)
		}
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Pricing *struct {
				Currency string `json:"currency"`
				Basis    string `json:"basis"`
			} `json:"x-gateway-pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "echo-model" {
		t.Fatalf("model list mismatch: %+v", list)
	}
	if list.Data[0].Pricing == nil || list.Data[0].Pricing.Currency != "USD" {
		t.Fatalf("sale pricing missing: %+v", list.Data[0].Pricing)
	}
}

func TestRecordingSwitchesAreIndependent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Default: input recorded, thinking and final output not recorded.
	resp := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request failed: %d", resp.StatusCode)
	}
	requestID := resp.Header.Get("x-request-id")
	log, err := f.db.GetRequestLog(ctx, requestID)
	if err != nil {
		t.Fatalf("request log missing: %v", err)
	}
	if log.RequestJSON == "" || !strings.Contains(log.RequestJSON, "ping") {
		t.Fatalf("input text must be recorded by default: %q", log.RequestJSON)
	}
	if log.OutputTextRecorded || log.ResponseText != "" {
		t.Fatalf("final output text must NOT be recorded by default: %+v", log)
	}
	if log.ReasoningRecorded || log.ResponseReasoning != "" {
		t.Fatalf("thinking text must NOT be recorded by default: %+v", log)
	}

	// Enable the output-text switch for this key. The admin API invalidates the
	// verification cache after such a write; do the same here.
	if err := f.db.SetAPIKeyRecording(ctx, f.key.ID, true, false, "full"); err != nil {
		t.Fatal(err)
	}
	f.verifier.Invalidate(secret.Prefix(testToken))
	resp2 := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	resp2.Body.Close()
	log2, err := f.db.GetRequestLog(ctx, resp2.Header.Get("x-request-id"))
	if err != nil {
		t.Fatal(err)
	}
	if !log2.OutputTextRecorded || !strings.Contains(log2.ResponseText, "ping") {
		t.Fatalf("output text must be recorded once the key opts in: %+v", log2)
	}
	if log2.ReasoningRecorded {
		t.Fatalf("thinking text must stay off: %+v", log2)
	}
}

func TestUnknownModelReturns404(t *testing.T) {
	f := newFixture(t)
	resp := f.do(t, "POST", "/v1/responses", `{"model":"nope","input":"hi"}`, nil)
	if status, code := decodeError(t, resp); status != 404 || code != "model_not_found" {
		t.Fatalf("expected 404 model_not_found, got %d %s", status, code)
	}
}

func TestUsageIsMeteredPerAttempt(t *testing.T) {
	f := newFixture(t)
	resp := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	resp.Body.Close()

	rows, err := f.db.ListUsage(context.Background(), f.key.AccountID, time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one metered attempt, got %d", len(rows))
	}
	row := rows[0]
	if row.Model != "echo-model" || row.Status != "completed" || row.UsageSource == "" {
		t.Fatalf("usage record mismatch: %+v", row)
	}
	if row.DimensionsJSON == "" || row.DimensionsJSON == "{}" {
		t.Fatalf("dimensions must be recorded: %q", row.DimensionsJSON)
	}
}

// ---------------------------------------------------------------------------
// chain-of-thought continuation (M17)
// ---------------------------------------------------------------------------

// TestStoredReasoningSurvivesContinuation walks the path a previous_response_id
// continuation takes: the reasoning streamed by a provider is assembled, stored as
// output JSON, decoded back into canonical items, and finally translated into the
// upstream request. Upstreams that require their own reasoning back (DeepSeek, when
// tools are involved) reject the request with 400 when any step loses the text.
func TestStoredReasoningSurvivesContinuation(t *testing.T) {
	assembler := responses.NewAssembler("deepseek-flash", nil)
	if err := assembler.Start(); err != nil {
		t.Fatal(err)
	}
	for _, event := range []pluginapi.Event{
		{Type: pluginapi.EventReasoningDelta, Text: "look up the "},
		{Type: pluginapi.EventReasoningDelta, Text: "weather first"},
		{Type: pluginapi.EventTextDelta, Text: "checking"},
		{Type: pluginapi.EventToolCallStart, CallID: "call_1", Name: "weather"},
		{Type: pluginapi.EventToolArgsDelta, CallID: "call_1", Name: "weather", Text: `{"city":"hz"}`},
		{Type: pluginapi.EventUsage, Usage: &pluginapi.Usage{Dimensions: map[string]int64{"input": 10, "output": 4}}},
	} {
		if err := assembler.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := json.Marshal(assembler.Response().Output)
	if err != nil {
		t.Fatal(err)
	}

	items := decodeStoredItems(string(stored))
	if len(items) != 3 {
		t.Fatalf("expected reasoning + message + function_call, got %+v", items)
	}
	if items[0].Type != "reasoning" {
		t.Fatalf("reasoning item must lead the stored output: %+v", items[0])
	}
	if got := items[0].Summary; len(got) != 1 || got[0].Text != "look up the weather first" {
		t.Fatalf("the stored chain of thought must survive decoding: %+v", got)
	}

	chat, err := providerkit.ResponsesToChatWithOptions(&pluginapi.Request{
		Model: "deepseek-flash",
		// The stored items are what the gateway prepends to a continuation request; the
		// request itself carries the tool output that answers the stored call, so the pair
		// travels together (a chat request may not contain an unanswered tool_call).
		Input: append(append([]pluginapi.Item{}, items...),
			pluginapi.Item{Type: "function_call_output", CallID: "call_1", Output: "sunny"}),
	}, providerkit.ChatConvertOptions{ReplayReasoningContent: true})
	if err != nil {
		t.Fatal(err)
	}
	var assistant *providerkit.ChatMessage
	for i := range chat.Messages {
		if len(chat.Messages[i].ToolCalls) > 0 {
			assistant = &chat.Messages[i]
		}
	}
	if assistant == nil {
		t.Fatalf("the tool call must survive the round trip: %+v", chat.Messages)
	}
	if assistant.ReasoningContent != "look up the weather first" {
		t.Fatalf("reasoning_content = %q, want the stored chain of thought", assistant.ReasoningContent)
	}
}

func TestDecodeStoredItemsWithoutReasoning(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		items  int
	}{
		{name: "empty output", stored: "", items: 0},
		{name: "malformed output", stored: "{not json", items: 0},
		{name: "reasoning without text", stored: `[{"type":"reasoning","id":"rs_1"}]`, items: 1},
		{name: "blank reasoning text", stored: `[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"  "}]}]`, items: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := decodeStoredItems(tc.stored)
			if len(items) != tc.items {
				t.Fatalf("items = %+v, want %d", items, tc.items)
			}
			if tc.items == 1 && len(items[0].Summary) != 0 {
				t.Fatalf("a chain of thought with no text must not invent a summary part: %+v", items[0])
			}
		})
	}
}

// TestFeedItemsReadsUpstreamReasoningShape covers an upstream that returns reasoning
// in the Responses shape (reasoning_text content parts) without a summary.
func TestFeedItemsReadsUpstreamReasoningShape(t *testing.T) {
	assembler := responses.NewAssembler("deepseek-flash", nil)
	if err := assembler.Start(); err != nil {
		t.Fatal(err)
	}
	items := []pluginapi.Item{{
		Type:    "reasoning",
		ID:      "rs_1",
		Content: json.RawMessage(`[{"type":"reasoning_text","text":"upstream thinking"}]`),
	}}
	if err := responses.FeedItems(assembler, items); err != nil {
		t.Fatal(err)
	}
	if got := assembler.Reasoning(); got != "upstream thinking" {
		t.Fatalf("reasoning = %q, want the upstream text", got)
	}
}

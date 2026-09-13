package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	registry *registry.Registry
	// handler is the server itself, so a test can drive it with a context of its own
	// (a canceled one stands in for a client that has already hung up).
	handler http.Handler
	// srv is that same instance, for paths a data-plane request cannot reach here (a
	// billing rejection needs Deps.Billing, which this fixture leaves nil).
	srv *Server
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
	// Session stickiness is on, as it is in a default deployment (config.Default()).
	router := routing.New(routing.Config{
		DefaultGrant: "all", Degradation: "strip", SessionAffinity: true,
	}, reg, bal)
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

	return &fixture{server: ts, db: db, cfg: &cfg, key: key, verifier: verifier, registry: reg, handler: srv.Handler(), srv: srv}
}

const (
	capabilitiesJSON = `{"stream":true,"tools":true}`
	grantsAll        = `{"models":["*"],"providers":["*"]}`
	nonStreamBody    = `{"model":"echo-model","input":"ping"}`
	// agentBody is shaped like a real agent turn: a developer instruction, a tool
	// definition, the user's own message, a tool call and its output (a whole file in
	// practice) and an earlier assistant turn. Only "ping" is user input.
	agentBody = `{"model":"echo-model","input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"SYSTEM INSTRUCTION"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"file\":\"/etc/shadow\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ZZ_TOOL_OUTPUT_SECRET"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"an earlier answer"}]}` +
		`],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`
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

	// Default: only the user's own input is recorded; everything else the client sent
	// (developer instruction, tool definition, tool output, earlier assistant turn) is
	// tallied but not stored, and thinking/final output are not recorded at all.
	resp := f.do(t, "POST", "/v1/responses", agentBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request failed: %d", resp.StatusCode)
	}
	requestID := resp.Header.Get("x-request-id")
	log, err := f.db.GetRequestLog(ctx, requestID)
	if err != nil {
		t.Fatalf("request log missing: %v", err)
	}
	if log.RecordInputMode != "user" {
		t.Fatalf("record_input_mode = %q, want the resolved default user", log.RecordInputMode)
	}
	if !strings.Contains(log.RequestJSON, "ping") {
		t.Fatalf("the user's own input must be recorded by default: %q", log.RequestJSON)
	}
	for _, forbidden := range []string{"ZZ_TOOL_OUTPUT_SECRET", "SYSTEM INSTRUCTION", "an earlier answer", "/etc/shadow"} {
		if strings.Contains(log.RequestJSON, forbidden) {
			t.Fatalf("only user input may be recorded by default, found %q in %q", forbidden, log.RequestJSON)
		}
	}
	if !strings.Contains(log.RequestJSON, `"function_call_output":1`) || !strings.Contains(log.RequestJSON, `"tools":1`) {
		t.Fatalf("the record must tally what was left out: %q", log.RequestJSON)
	}
	if log.RequestBytes <= 0 {
		t.Fatalf("request_bytes must still report how big the request was: %+v", log)
	}
	if log.OutputTextRecorded || log.ResponseText != "" {
		t.Fatalf("final output text must NOT be recorded by default: %+v", log)
	}
	if log.ReasoningRecorded || log.ResponseReasoning != "" {
		t.Fatalf("thinking text must NOT be recorded by default: %+v", log)
	}

	// The output-text switch is per key; switching to full keeps the whole body. The two
	// channels stay independent of each other. The admin API invalidates the verification
	// cache after such a write; do the same here.
	if err := f.db.SetAPIKeyRecording(ctx, f.key.ID, true, false, "full"); err != nil {
		t.Fatal(err)
	}
	f.verifier.Invalidate(secret.Prefix(testToken))
	resp2 := f.do(t, "POST", "/v1/responses", agentBody, nil)
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
	if log2.RecordInputMode != "full" || !strings.Contains(log2.RequestJSON, "ZZ_TOOL_OUTPUT_SECRET") {
		t.Fatalf("full mode must keep the whole body for diagnosis: %+v", log2)
	}
}

// TestMetadataAndOffModesStoreNoBody pins the two modes that store no content: metadata
// keeps the size of the request, off keeps nothing at all.
func TestMetadataAndOffModesStoreNoBody(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// metadata used to behave exactly like full, which made the mode name a lie.
	if err := f.db.SetAPIKeyRecording(ctx, f.key.ID, false, false, "metadata"); err != nil {
		t.Fatal(err)
	}
	f.verifier.Invalidate(secret.Prefix(testToken))
	resp := f.do(t, "POST", "/v1/responses", agentBody, nil)
	resp.Body.Close()
	row, err := f.db.GetRequestLog(ctx, resp.Header.Get("x-request-id"))
	if err != nil {
		t.Fatal(err)
	}
	if row.RequestJSON != "" {
		t.Fatalf("metadata mode must not store content: %q", row.RequestJSON)
	}
	if row.RequestBytes <= 0 {
		t.Fatalf("metadata mode must still report the size: %+v", row)
	}
	if row.RecordInputMode != "metadata" {
		t.Fatalf("record_input_mode = %q", row.RecordInputMode)
	}

	if err := f.db.SetAPIKeyRecording(ctx, f.key.ID, false, false, "off"); err != nil {
		t.Fatal(err)
	}
	f.verifier.Invalidate(secret.Prefix(testToken))
	resp2 := f.do(t, "POST", "/v1/responses", agentBody, nil)
	resp2.Body.Close()
	row2, err := f.db.GetRequestLog(ctx, resp2.Header.Get("x-request-id"))
	if err != nil {
		t.Fatal(err)
	}
	if row2.RequestJSON != "" || row2.RequestBytes != 0 {
		t.Fatalf("off mode must store nothing at all: %+v", row2)
	}
	// The row itself must survive: a request with recording off is still a request, and
	// its status is what an operator reads.
	if row2.Status != "completed" {
		t.Fatalf("status = %q, want completed", row2.Status)
	}
}

// TestEffectiveReasoningEffortIsLoggedWithoutRequestContent keeps execution metadata
// independent of the request-content recording policy. The logged value comes from the
// provider request after the canonical model's policy has been applied.
func TestEffectiveReasoningEffortIsLoggedWithoutRequestContent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.db.SetAPIKeyRecording(ctx, f.key.ID, false, false, "off"); err != nil {
		t.Fatal(err)
	}
	f.verifier.Invalidate(secret.Prefix(testToken))

	model, err := f.db.GetModelByName(ctx, "echo-model")
	if err != nil {
		t.Fatal(err)
	}
	model.ReasoningJSON = `{"mode":"force","effort":"high"}`
	if _, err := f.db.UpsertModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	resp := f.do(t, "POST", "/v1/responses", `{"model":"echo-model","input":"ping","reasoning":{"effort":"low"}}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request failed: %d", resp.StatusCode)
	}
	row, err := f.db.GetRequestLog(ctx, resp.Header.Get("x-request-id"))
	if err != nil {
		t.Fatal(err)
	}
	if row.RequestJSON != "" || row.RecordInputMode != "off" {
		t.Fatalf("off recording must not retain a body: %+v", row)
	}
	if row.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want policy-forced high", row.ReasoningEffort)
	}
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "reasoning"} {
		t.Run("redact_"+path, func(t *testing.T) {
			f.srv.deps.Config.Recording.RedactPaths = []string{path}
			resp := f.do(t, "POST", "/v1/responses", `{"model":"echo-model","input":"ping","reasoning":{"effort":"low"}}`, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("request failed: %d", resp.StatusCode)
			}
			row, err := f.db.GetRequestLog(ctx, resp.Header.Get("x-request-id"))
			if err != nil {
				t.Fatal(err)
			}
			if row.ReasoningEffort != "" {
				t.Fatalf("redacted reasoning_effort = %q", row.ReasoningEffort)
			}
		})
	}
}

// TestFlatKeyPolicyIsEnforced pins the shape admission actually reads: the quota fields
// live at the top level of the policy. The console used to advertise a nested
// {"rate_limit":{...}} document that nothing enforced.
func TestFlatKeyPolicyIsEnforced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	key, err := f.db.GetAPIKeyByPrefix(ctx, secret.Prefix(testToken))
	if err != nil {
		t.Fatal(err)
	}
	key.PolicyJSON = `{"rpm":1}`
	if _, err := f.db.UpsertAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	f.verifier.Invalidate(secret.Prefix(testToken))

	first := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("the first request inside the window must pass: %d", first.StatusCode)
	}
	second := f.do(t, "POST", "/v1/responses", nonStreamBody, nil)
	second.Body.Close()
	if second.StatusCode != 429 {
		t.Fatalf("rpm=1 in the key policy must reject the second request, got %d", second.StatusCode)
	}
	if got := second.Header.Get("x-ratelimit-limit-requests"); got != "1" {
		t.Fatalf("x-ratelimit-limit-requests = %q, want 1", got)
	}
}

// TestDeniedRequestGoesThroughTheRecordingPolicy covers the local-rejection path, which
// used to store the whole request body with no redaction at all.
func TestDeniedRequestGoesThroughTheRecordingPolicy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The fixture has no billing wired, so the rejection is invoked the way the data
	// plane invokes it (rejectForQuota) instead of by driving a payment failure.
	req, apiErr := responses.Parse([]byte(agentBody))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	account := &domain.Account{ID: f.key.AccountID, Name: "acme", Status: "active"}
	// The request id normally arrives from the middleware; without it there is nothing to
	// key the row on and the write is refused.
	ctx = context.WithValue(ctx, ctxRequestID, "req_denied0001")
	f.srv.recordDenied(ctx, f.key, account, req, domain.ErrInsufficientQuota("no funds"), "")

	logs, err := f.db.ListRequestLogs(ctx, domain.RequestLogFilter{AccountID: f.key.AccountID, From: time.Time{}, To: time.Time{}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("the rejected request must still be recorded, got %d rows", len(logs))
	}
	denied := logs[0]
	if denied.Status != "402" {
		t.Fatalf("status = %q, want the rejection's HTTP status", denied.Status)
	}
	if denied.RecordInputMode != "user" {
		t.Fatalf("the denial must resolve the key policy, got %q", denied.RecordInputMode)
	}
	if !strings.Contains(denied.RequestJSON, "ping") {
		t.Fatalf("the user input must survive for diagnosis: %q", denied.RequestJSON)
	}
	for _, forbidden := range []string{"ZZ_TOOL_OUTPUT_SECRET", "SYSTEM INSTRUCTION"} {
		if strings.Contains(denied.RequestJSON, forbidden) {
			t.Fatalf("the denial path must apply the input policy, found %q", forbidden)
		}
	}
}

// TestDeniedRequestIsRedacted pins that the rejection path runs the same redaction as a
// served request: it used to store the body verbatim, credentials included. Redaction is
// observable in full mode; under the default policy a credential that is not part of the
// user's own input is dropped outright, which is stricter still.
func TestDeniedRequestIsRedacted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.db.SetAPIKeyRecording(ctx, f.key.ID, false, false, "full"); err != nil {
		t.Fatal(err)
	}
	key, err := f.db.GetAPIKeyByPrefix(ctx, secret.Prefix(testToken))
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"echo-model","input":"ping","metadata":{"api_key":"sk-leak-me"}}`
	req, apiErr := responses.Parse([]byte(body))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	account := &domain.Account{ID: key.AccountID, Name: "acme", Status: "active"}
	// The request id normally arrives from the middleware; without it there is nothing to
	// key the row on and the write is refused.
	ctx = context.WithValue(ctx, ctxRequestID, "req_denied0002")
	f.srv.recordDenied(ctx, key, account, req, domain.ErrInsufficientQuota("no funds"), "")

	logs, err := f.db.ListRequestLogs(ctx, domain.RequestLogFilter{AccountID: f.key.AccountID, From: time.Time{}, To: time.Time{}}, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("request log = %v (err %v)", logs, err)
	}
	if logs[0].RecordInputMode != "full" {
		t.Fatalf("record_input_mode = %q, want the key's full", logs[0].RecordInputMode)
	}
	if strings.Contains(logs[0].RequestJSON, "sk-leak-me") || !strings.Contains(logs[0].RequestJSON, "[redacted]") {
		t.Fatalf("the denial path must redact credentials: %q", logs[0].RequestJSON)
	}
}

func TestTruncateKeepsRunesIntact(t *testing.T) {
	// Eight Chinese characters, 24 bytes: cutting at 5 must not split the second rune.
	text := "配置请求日志"
	got := truncate(text, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got != "配" {
		t.Fatalf("truncate(%q, 5) = %q, want the first whole rune", text, got)
	}
	if truncate(text, 0) != text || truncate(text, 99) != text {
		t.Fatal("limit<=0 and limits past the end must return the string unchanged")
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

// flakyRecords fails the first content-bearing request-log write and records what the
// server did next. The skeleton fallback is the whole point: the row must survive even
// when its content does not.
type flakyRecords struct {
	Records
	failNext  bool
	skeletons int
}

func (f *flakyRecords) PutRequestLog(ctx context.Context, rec *domain.RequestLogRecord) error {
	if rec.RequestJSON != "" && f.failNext {
		f.failNext = false
		return errors.New("store: put request log: context deadline exceeded")
	}
	if rec.RequestJSON == "" {
		f.skeletons++
	}
	return f.Records.PutRequestLog(ctx, rec)
}

func TestFailedContentWriteStillLeavesASkeletonRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	flaky := &flakyRecords{Records: f.srv.deps.Records, failNext: true}
	f.srv.deps.Records = flaky

	resp := f.do(t, "POST", "/v1/responses", agentBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the request itself must be served: %d", resp.StatusCode)
	}
	requestID := resp.Header.Get("x-request-id")

	log, err := f.db.GetRequestLog(ctx, requestID)
	if err != nil {
		t.Fatalf("the request must still be recorded: %v", err)
	}
	if log.RequestJSON != "" {
		t.Fatalf("the fallback row must carry no content: %q", log.RequestJSON)
	}
	if log.RequestBytes <= 0 {
		t.Fatalf("the fallback row must keep the size of what was lost: %+v", log)
	}
	if log.Status != "completed" || log.RecordInputMode != "user" {
		t.Fatalf("the fallback row must keep the facts: %+v", log)
	}
	if flaky.skeletons != 1 {
		t.Fatalf("exactly one skeleton write, got %d", flaky.skeletons)
	}
	if got := f.srv.requestLogWriteFailures.Load(); got != 1 {
		t.Fatalf("write failures = %d, want 1", got)
	}
	if got := f.srv.requestLogDropped.Load(); got != 0 {
		t.Fatalf("nothing was dropped: %d", got)
	}
}

// TestDroppedRequestLogIsCounted covers the second failure: the skeleton write fails too,
// and the loss must be visible as a number rather than only as a log line.
func TestDroppedRequestLogIsCounted(t *testing.T) {
	f := newFixture(t)
	f.srv.deps.Records = &alwaysFailRecords{Records: f.srv.deps.Records}

	resp := f.do(t, "POST", "/v1/responses", agentBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a broken audit write must not fail the request: %d", resp.StatusCode)
	}
	if got := f.srv.requestLogWriteFailures.Load(); got != 1 {
		t.Fatalf("write failures = %d, want 1", got)
	}
	if got := f.srv.requestLogDropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

type alwaysFailRecords struct {
	Records
}

func (f *alwaysFailRecords) PutRequestLog(context.Context, *domain.RequestLogRecord) error {
	return errors.New("store: put request log: database is locked")
}

// TestDeniedRequestSurvivesACancelledClient pins the other half of the same lesson: the
// local-rejection row used to be written with the request's own context, so a client that
// hung up took the evidence with it.
func TestDeniedRequestSurvivesACancelledClient(t *testing.T) {
	f := newFixture(t)
	req, apiErr := responses.Parse([]byte(agentBody))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, ctxRequestID, "req_cancelled01")
	cancel()

	f.srv.recordDenied(ctx, f.key, &domain.Account{ID: f.key.AccountID, Name: "acme"}, req,
		domain.ErrInsufficientQuota("no funds"), "")

	logs, err := f.db.ListRequestLogs(context.Background(), domain.RequestLogFilter{AccountID: f.key.AccountID, From: time.Time{}, To: time.Time{}}, 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("the rejected request must be recorded despite the cancelled client: %v (%v)", logs, err)
	}
	if logs[0].RequestID != "req_cancelled01" {
		t.Fatalf("request id = %q", logs[0].RequestID)
	}
}

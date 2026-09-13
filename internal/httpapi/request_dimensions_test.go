package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/store"
)

// The bodies below are the shapes M27's extractor keys on, trimmed to what it reads: a
// DSH session (developer system prompt + runtime-context snapshot + prompt_cache_key), a
// Codex session (instructions + environment context) and a DSH title call. They are
// trimmed copies of requests captured from this deployment.

const dshAgentBody = `{"model":"echo-model","stream":false,` +
	`"prompt_cache_key":"session-ebf36761-3295-4881-bbec-73f80c9a4589",` +
	`"input":[` +
	`{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness.\n\nThe DeepSeek Harness implementation checkout is at /home/winger/.local/dsh-0.1.2-rc.1/."},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"重构请求日志"}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nCurrent DSH file policy: workspace-write. Any available operation enforced by the DSH file sandbox may modify files under the session workspace: \"/home/winger/work/ai_gateway\". Some platform temporary areas may also be writable."}]}` +
	`]}`

const codexAgentBody = `{"model":"echo-model","stream":false,` +
	`"prompt_cache_key":"01a08f27-cc56-7491-abf3-c5db92e442d9",` +
	`"instructions":"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",` +
	`"input":[` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/home/winger/work/ai_gateway</cwd>\n  <shell>bash</shell>\n</environment_context>"}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"用一句话回答：1+1 等于几？"}]}` +
	`]}`

const dshTitleBody = `{"model":"echo-model","stream":false,"max_output_tokens":64,` +
	`"prompt_cache_key":"session-d680bd79-6be1-426b-a91b-fc232e0f80c3",` +
	`"input":[` +
	`{"role":"developer","content":"Create a concise title for an AI coding-assistant session from the supplied human messages.\nReturn only the title on one line."},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"Generate the session title from this JSON array of human messages:\n[{\"seq\":7,\"text\":\"看看能不能从dsh,codex的请求里解析出workspace\"}]"}]}` +
	`]}`

// postResponses serves one request through the data plane.
func (f *fixture) postResponses(t testing.TB, body string) *http.Response {
	t.Helper()
	return f.do(t, http.MethodPost, "/v1/responses", body, map[string]string{
		"Authorization": "Bearer " + testToken,
		"Content-Type":  "application/json",
	})
}

// latestLog returns the newest recorded request of the fixture's account.
func (f *fixture) latestLog(t testing.TB) *domain.RequestLogRecord {
	t.Helper()
	logs, err := f.db.ListRequestLogs(context.Background(),
		domain.RequestLogFilter{AccountID: f.key.AccountID}, 1)
	if err != nil {
		t.Fatalf("list request logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("no request log was recorded")
	}
	return logs[0]
}

func TestRequestLogRecordsDSHIdentity(t *testing.T) {
	f := newFixture(t)
	resp := f.postResponses(t, dshAgentBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want 200", resp.StatusCode)
	}

	row := f.latestLog(t)
	if row.Client != "dsh" {
		t.Fatalf("client = %q, want dsh", row.Client)
	}
	if row.Workspace != "/home/winger/work/ai_gateway" {
		t.Fatalf("workspace = %q", row.Workspace)
	}
	if row.SessionID != "session-ebf36761-3295-4881-bbec-73f80c9a4589" {
		t.Fatalf("session_id = %q", row.SessionID)
	}
	if row.CallKind != "agent" {
		t.Fatalf("call_kind = %q, want agent", row.CallKind)
	}
	// The identity is recorded under the default "user" policy, where the developer
	// message and everything else but the user's own words are dropped from the body.
	if row.RequestJSON == "" {
		t.Fatal("the user input is still recorded under the default policy")
	}
	if strings.Contains(row.RequestJSON, "You are an AI agent powered by DeepSeek Harness") {
		t.Fatalf("the default policy must not store the system prompt: %q", row.RequestJSON)
	}
}

func TestRequestLogRecordsCodexIdentity(t *testing.T) {
	f := newFixture(t)
	resp := f.postResponses(t, codexAgentBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want 200", resp.StatusCode)
	}

	row := f.latestLog(t)
	if row.Client != "codex" {
		t.Fatalf("client = %q, want codex", row.Client)
	}
	if row.Workspace != "/home/winger/work/ai_gateway" {
		t.Fatalf("workspace = %q", row.Workspace)
	}
	if row.SessionID != "01a08f27-cc56-7491-abf3-c5db92e442d9" {
		t.Fatalf("session_id = %q", row.SessionID)
	}
}

// The requested model and the routed model are two different facts, and an alias is the
// case where they differ.
func TestRequestLogRecordsRequestedAndRoutedModel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.db.UpsertModelMapping(ctx, &domain.ModelMapping{
		Kind: "exact", Pattern: "luna", TargetModel: "echo-model", Priority: 10, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	resp := f.postResponses(t, `{"model":"luna","input":"ping"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want 200", resp.StatusCode)
	}

	row := f.latestLog(t)
	if row.Model != "luna" {
		t.Fatalf("model = %q, want the model the client asked for", row.Model)
	}
	if row.ResolvedModel != "echo-model" {
		t.Fatalf("resolved_model = %q, want the routed canonical model", row.ResolvedModel)
	}
}

// record_input=off keeps a row with no body; the identity is not part of that policy.
func TestRequestLogKeepsIdentityWhenInputRecordingIsOff(t *testing.T) {
	f := newFixture(t)
	if err := f.db.SetAPIKeyRecording(context.Background(), f.key.ID, false, false, "off"); err != nil {
		t.Fatal(err)
	}

	resp := f.postResponses(t, dshAgentBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want 200", resp.StatusCode)
	}

	row := f.latestLog(t)
	if row.RecordInputMode != "off" || row.RequestJSON != "" {
		t.Fatalf("off must keep the row without content: mode=%q body=%q", row.RecordInputMode, row.RequestJSON)
	}
	if row.Client != "dsh" || row.Workspace != "/home/winger/work/ai_gateway" ||
		row.SessionID == "" || row.Model != "echo-model" {
		t.Fatalf("identity must survive record_input=off: %+v", row)
	}
}

// A rejected request never reached routing: it has a requested model and no routed one.
func TestDeniedRequestRecordsModelWithoutResolvedModel(t *testing.T) {
	f := newFixture(t)
	ctx := context.WithValue(context.Background(), ctxRequestID, "req_denied_dim1")

	req, apiErr := responses.Parse([]byte(`{"model":"luna","input":"ping"}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	account := &domain.Account{ID: f.key.AccountID, Name: "acme", Status: "active"}
	f.srv.recordDenied(ctx, f.key, account, req, domain.ErrInsufficientQuota("no funds"), "")

	row := f.latestLog(t)
	if row.Status != "402" {
		t.Fatalf("status = %q, want the rejection status", row.Status)
	}
	if row.Model != "luna" {
		t.Fatalf("model = %q, want the requested model", row.Model)
	}
	if row.ResolvedModel != "" {
		t.Fatalf("resolved_model = %q, want empty for a request that never routed", row.ResolvedModel)
	}
}

// The title is metadata about the session, so it is recorded even though final-output
// recording is off — but only on the row that actually produced it.
func TestRequestLogRecordsTitleOnlyOnTheTitleCall(t *testing.T) {
	f := newFixture(t)

	resp := f.postResponses(t, dshTitleBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want 200", resp.StatusCode)
	}
	title := f.latestLog(t)
	if title.CallKind != "title" {
		t.Fatalf("call_kind = %q, want title", title.CallKind)
	}
	if title.Title == "" {
		t.Fatal("the title call must record the title")
	}
	if title.OutputTextRecorded {
		t.Fatal("the title is metadata: it must not depend on output-text recording")
	}

	// A normal agent turn belongs to no title and must not invent one.
	resp2 := f.postResponses(t, dshAgentBody)
	defer resp2.Body.Close()
	agent := f.latestLog(t)
	if agent.CallKind != "agent" || agent.Title != "" {
		t.Fatalf("agent row = call_kind %q title %q, want agent/empty", agent.CallKind, agent.Title)
	}
}

func TestRequestLogTitleSwitchOff(t *testing.T) {
	f := newFixture(t)
	f.cfg.Recording.RecordTitle = false

	resp := f.postResponses(t, dshTitleBody)
	defer resp.Body.Close()

	row := f.latestLog(t)
	if row.CallKind != "title" {
		t.Fatalf("call_kind = %q, want title", row.CallKind)
	}
	if row.Title != "" {
		t.Fatalf("recording.record_title=false must not store a title, got %q", row.Title)
	}
}

// redact_paths governs the recorded body; a workspace column that ignored it would
// silently undo the setting.
func TestRequestLogDimensionsHonourRedactPaths(t *testing.T) {
	f := newFixture(t)
	f.cfg.Recording.RedactPaths = []string{"workspace", "session_id"}

	resp := f.postResponses(t, dshAgentBody)
	defer resp.Body.Close()

	row := f.latestLog(t)
	if row.Workspace != "" || row.SessionID != "" {
		t.Fatalf("redacted dimensions must be blank: workspace=%q session_id=%q", row.Workspace, row.SessionID)
	}
	if row.Client != "dsh" {
		t.Fatalf("client = %q: only the listed columns may be blanked", row.Client)
	}
}

// The content-free fallback row exists so a request stays visible; the identity is the
// part that must survive it.
func TestSkeletonRowKeepsIdentity(t *testing.T) {
	rec := &domain.RequestLogRecord{
		RequestID: "req_skeleton0001", Status: "completed", RequestJSON: `{"input":"ping"}`,
		ResponseText: "answer", Client: "dsh", Model: "deepseek-flash", ResolvedModel: "deepseek-flash",
		Workspace: "/home/winger/work/ai_gateway", SessionID: "session-abc", CallKind: "agent",
	}
	bare := skeletonLog(rec)

	if bare.RequestJSON != "" || bare.ResponseText != "" {
		t.Fatalf("the skeleton must drop content: %+v", bare)
	}
	if bare.Client != "dsh" || bare.Model != "deepseek-flash" || bare.ResolvedModel != "deepseek-flash" ||
		bare.Workspace != "/home/winger/work/ai_gateway" || bare.SessionID != "session-abc" || bare.CallKind != "agent" {
		t.Fatalf("the skeleton must keep the identity: %+v", bare)
	}
}

// The management surface is exercised through the admin fixture, which owns the admin
// session and the full management wiring. Its rows are seeded directly: the point here is
// what the console reads back, not how the row was produced.

func seedIdentityRow(t *testing.T, f *adminFixture, rec *domain.RequestLogRecord) {
	t.Helper()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if err := f.db.PutRequestLog(context.Background(), rec); err != nil {
		t.Fatalf("put request log: %v", err)
	}
}

func TestAdminRequestsCarryIdentityAndUsage(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0001", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "completed", RecordInputMode: "user", Client: "dsh", Model: "luna",
		ResolvedModel: "deepseek-flash", Workspace: "/home/winger/work/ai_gateway",
		SessionID: "session-ebf36761", CallKind: "agent", Title: "",
	})
	usage := &domain.UsageRecord{
		RequestID: "req_dim0001", AttemptNo: 1, AccountID: 1, APIKeyID: 1,
		Model: "luna", ResolvedModel: "deepseek-flash",
		DimensionsJSON: `{"input_cache_hit":100,"input_cache_miss":20,"output":7,"reasoning":3}`,
		CostMicros:     1234, ChargeMicros: 2468, LatencyMS: 900, TTFTMS: 120,
		Status: "completed", CreatedAt: time.Now().UTC(),
	}
	if _, err := f.db.InsertUsage(context.Background(), usage); err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	body := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?days=1&limit=10", "", cookie))
	rows, _ := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list returned %d rows, want 1", len(rows))
	}
	item, _ := rows[0].(map[string]any)
	if item["client"] != "dsh" || item["model"] != "luna" || item["resolved_model"] != "deepseek-flash" {
		t.Fatalf("list row lacks the identity: %v", item)
	}
	if item["workspace"] != "/home/winger/work/ai_gateway" || item["session_id"] != "session-ebf36761" {
		t.Fatalf("list row lacks the workspace/session: %v", item)
	}
	got, _ := item["usage"].(map[string]any)
	if got == nil || got["metered"] != true {
		t.Fatalf("list row lacks metered usage: %v", item["usage"])
	}
	// 100 cached + 20 uncached = the input side, exactly as the billing breakdown counts it.
	if got["input_tokens"].(float64) != 120 || got["output_tokens"].(float64) != 7 ||
		got["reasoning_tokens"].(float64) != 3 {
		t.Fatalf("usage tokens = %v", got)
	}
	if got["charge_micros"].(float64) != 2468 || got["latency_ms"].(float64) != 900 {
		t.Fatalf("usage money/latency = %v", got)
	}

	detail := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests/req_dim0001", "", cookie))
	if detail["model"] != "luna" || detail["resolved_model"] != "deepseek-flash" {
		t.Fatalf("detail lacks the model identity: %v", detail)
	}
	if dusage, _ := detail["usage"].(map[string]any); dusage == nil || dusage["metered"] != true {
		t.Fatalf("detail lacks usage: %v", detail["usage"])
	}
}

func TestAdminRequestCachedTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dims   string
		input  float64
		cached float64
	}{
		{"cache hit", `{"input_cache_hit":80,"input_cache_miss":20}`, 100, 80},
		{"missing cache", `{"input":100}`, 100, 0},
		{"empty dimensions", `{}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdminFixture(t)
			cookie := f.login(t, adminUser, adminPassword)
			seedIdentityRow(t, f, &domain.RequestLogRecord{
				RequestID: "req_cached0001", AccountID: 1, APIKeyID: 1, Client: "dsh",
			})
			if _, err := f.db.InsertUsage(context.Background(), &domain.UsageRecord{
				RequestID: "req_cached0001", AttemptNo: 1, AccountID: 1, APIKeyID: 1,
				Model: "m", ResolvedModel: "m", DimensionsJSON: tc.dims,
				Status: "completed", CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			list := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?days=1", "", cookie))
			item := list["data"].([]any)[0].(map[string]any)
			detail := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests/req_cached0001", "", cookie))
			dimensions := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests/dimensions?group_by=client&days=1", "", cookie))
			bucket := dimensions["rows"].([]any)[0].(map[string]any)
			for name, got := range map[string]map[string]any{
				"list": item["usage"].(map[string]any), "detail": detail["usage"].(map[string]any), "dimensions": bucket,
			} {
				if got["cached_tokens"] != tc.cached || got["input_tokens"] != tc.input {
					t.Errorf("%s tokens = %v, want input %v including cached %v", name, got, tc.input, tc.cached)
				}
			}
		})
	}
}

// A request with no usage row (a locally rejected one) reports metered=false rather than
// a zero that reads as "it consumed nothing".
func TestAdminRequestsReportUnmeteredRequests(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0002", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "402", RecordInputMode: "user", Client: "dsh", Model: "deepseek-flash",
	})

	body := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?days=1", "", cookie))
	rows, _ := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list returned %d rows, want 1", len(rows))
	}
	item, _ := rows[0].(map[string]any)
	got, _ := item["usage"].(map[string]any)
	if got == nil || got["metered"] != false {
		t.Fatalf("an unmetered request must say so: %v", item["usage"])
	}
	if item["resolved_model"] != "" {
		t.Fatalf("a rejected request has no routed model: %v", item["resolved_model"])
	}
}

func TestAdminRequestDimensionsGroupsAndFilters(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0010", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "completed", Client: "dsh", Model: "deepseek-flash", ResolvedModel: "deepseek-flash",
		Workspace: "/home/winger/work/ai_gateway", SessionID: "session-aaa", CallKind: "agent",
	})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0011", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "completed", Client: "dsh", Model: "deepseek-flash", ResolvedModel: "deepseek-flash",
		Workspace: "/home/winger/work/ai_gateway", SessionID: "session-aaa", CallKind: "title",
		Title: "从 dsh 和 codex 请求解析 workspace",
	})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0012", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "completed", Client: "codex", Model: "gpt-5.6-luna", ResolvedModel: "gpt-5.6-luna",
		Workspace: "/home/winger/work/ai_gateway", SessionID: "01a08f27", CallKind: "agent",
	})

	body := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=client&limit=10", "", cookie))
	if body["group_by"] != "client" {
		t.Fatalf("group_by = %v", body["group_by"])
	}
	// The paging envelope (M31): rows stays the list it has been since M27, and the
	// identifiers around it are the ones every other list endpoint answers with.
	if body["sort"] != "last_seen" {
		t.Fatalf("sort = %v, want the default last_seen", body["sort"])
	}
	if body["limit"].(float64) != 10 || body["offset"].(float64) != 0 || body["count"].(float64) != 2 {
		t.Fatalf("window = limit %v offset %v count %v, want 10/0/2", body["limit"], body["offset"], body["count"])
	}
	if body["total"].(float64) != 2 || body["has_more"] != false {
		t.Fatalf("total = %v has_more = %v, want 2 buckets and no next page", body["total"], body["has_more"])
	}
	rows, _ := body["rows"].([]any)
	counts := map[string]float64{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		key, _ := row["key"].(string)
		counts[key], _ = row["requests"].(float64)
		// The window a bucket spans travels with it (the console renders last_seen).
		if row["first_seen"] == "" || row["last_seen"] == "" {
			t.Fatalf("bucket %q lost its window: %v", key, row)
		}
		// No usage rows were seeded here, so every bucket is unmetered — and the two
		// counts must say exactly that.
		if row["metered"].(float64) != 0 {
			t.Fatalf("bucket %q: metered = %v, want 0", key, row["metered"])
		}
	}
	if counts["dsh"] != 2 || counts["codex"] != 1 {
		t.Fatalf("client buckets = %v, want dsh=2 codex=1", counts)
	}

	// The session grouping is where the title and the workspace mean something.
	sessions := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=session&limit=10", "", cookie))
	srows, _ := sessions["rows"].([]any)
	found := false
	for _, raw := range srows {
		row, _ := raw.(map[string]any)
		if row["key"] != "session-aaa" {
			continue
		}
		found = true
		if row["workspace"] != "/home/winger/work/ai_gateway" {
			t.Fatalf("session bucket lost its workspace: %v", row)
		}
		if row["title"] != "从 dsh 和 codex 请求解析 workspace" {
			t.Fatalf("session bucket lost its title: %v", row)
		}
		if row["requests"].(float64) != 2 {
			t.Fatalf("session bucket = %v requests, want 2", row["requests"])
		}
	}
	if !found {
		t.Fatalf("the recorded session is missing from the session breakdown: %v", srows)
	}

	// A dimension filter narrows the totals too, so a page and its count agree.
	filtered := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=client&client=codex", "", cookie))
	frows, _ := filtered["rows"].([]any)
	if len(frows) != 1 {
		t.Fatalf("filtered grouping returned %d buckets, want 1", len(frows))
	}
	if item, _ := frows[0].(map[string]any); item["key"] != "codex" || item["requests"].(float64) != 1 {
		t.Fatalf("filtered bucket = %v", frows[0])
	}
}

func TestAdminRequestDimensionsRejectsUnknownGrouping(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	resp := f.call(t, http.MethodGet, "/admin/api/v1/requests/dimensions?group_by=password", "", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown grouping", resp.StatusCode)
	}
}

// A page of buckets and the total that describes them: the console's pager reads both, so
// they have to come from the same filters and the pages must neither repeat nor skip a
// bucket (docs/design/m31-request-log-stats-pagination.md).
func TestAdminRequestDimensionsPaging(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	// Bucket cN holds N+1 requests, so sort=requests ranks c4 first and c0 last.
	for i := 0; i < 5; i++ {
		for n := 0; n <= i; n++ {
			seedIdentityRow(t, f, &domain.RequestLogRecord{
				RequestID: fmt.Sprintf("req_pg_%d_%d", i, n),
				AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses", Status: "completed",
				Client: fmt.Sprintf("c%d", i), Model: "deepseek-flash",
			})
		}
	}
	const base = "/admin/api/v1/requests/dimensions?days=1&group_by=client&sort=requests&limit=2"

	pages := []struct {
		offset int
		want   string
		more   bool
		count  int
	}{
		{0, "c4,c3", true, 2},
		{2, "c2,c1", true, 2},
		{4, "c0", false, 1},
	}
	seen := map[string]int{}
	for _, page := range pages {
		body := decodeJSONBody(t, f.call(t, http.MethodGet,
			fmt.Sprintf("%s&offset=%d", base, page.offset), "", cookie))
		if got := joinedKeys(body); got != page.want {
			t.Fatalf("offset %d: buckets %s, want %s", page.offset, got, page.want)
		}
		if body["total"].(float64) != 5 {
			t.Fatalf("offset %d: total = %v, want 5 buckets", page.offset, body["total"])
		}
		if body["offset"].(float64) != float64(page.offset) {
			t.Fatalf("offset %d echoed as %v", page.offset, body["offset"])
		}
		if body["count"].(float64) != float64(page.count) || body["has_more"] != page.more {
			t.Fatalf("offset %d: count %v has_more %v, want %d/%v",
				page.offset, body["count"], body["has_more"], page.count, page.more)
		}
		rows, _ := body["rows"].([]any)
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			key, _ := row["key"].(string)
			seen[key]++
		}
	}
	if len(seen) != 5 {
		t.Fatalf("the three pages covered %d buckets, want 5", len(seen))
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("bucket %q appeared %d times across pages", key, count)
		}
	}

	// An offset past the end is an empty page with the total unmoved, not an error: that is
	// what the console's "step back a page" rule reacts to.
	beyond := decodeJSONBody(t, f.call(t, http.MethodGet, base+"&offset=40", "", cookie))
	if rows, _ := beyond["rows"].([]any); len(rows) != 0 {
		t.Fatalf("an offset past the end returned %d buckets", len(rows))
	}
	if beyond["total"].(float64) != 5 || beyond["has_more"] != false {
		t.Fatalf("an empty page changed the total: %v / %v", beyond["total"], beyond["has_more"])
	}

	// A filter narrows the pages and the total together.
	filtered := decodeJSONBody(t, f.call(t, http.MethodGet, base+"&client=c0", "", cookie))
	if got := joinedKeys(filtered); got != "c0" || filtered["total"].(float64) != 1 {
		t.Fatalf("filtered breakdown = %s with total %v, want the single c0 bucket", got, filtered["total"])
	}

	// A malformed offset is a 400 rather than "page 1", which is the worst possible answer
	// to a request for page 5 (the rule the list endpoints already follow).
	for _, path := range []string{
		"/admin/api/v1/requests/dimensions?group_by=client&offset=abc",
		"/admin/api/v1/requests/dimensions?group_by=client&offset=-1",
	} {
		resp := f.call(t, http.MethodGet, path, "", cookie)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("%s: status = %d, want 400", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// The sort key decides which bucket is first, and the three keys exist to answer three
// different questions about the same buckets. An unknown key is refused by name.
func TestAdminRequestDimensionsSort(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	// a: 3 requests, most recent, cheapest · b: 2 requests, oldest, dearest ·
	// c: 1 request, in between.
	buckets := []struct {
		key     string
		count   int
		minutes int
		charge  int64
	}{
		{"a", 3, 1, 100},
		{"b", 2, 3, 450},
		{"c", 1, 2, 500},
	}
	for _, bucket := range buckets {
		for i := 0; i < bucket.count; i++ {
			id := fmt.Sprintf("req_sort_%s_%d", bucket.key, i)
			seedIdentityRow(t, f, &domain.RequestLogRecord{
				RequestID: id, AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
				Status: "completed", Client: bucket.key,
				CreatedAt: base.Add(-time.Duration(bucket.minutes)*time.Minute + time.Duration(i)*time.Second),
			})
			if _, err := f.db.InsertUsage(context.Background(), &domain.UsageRecord{
				RequestID: id, AttemptNo: 1, AccountID: 1, APIKeyID: 1, Model: "m", ResolvedModel: "m",
				DimensionsJSON: `{"input":10,"output":2}`, CostMicros: 1, ChargeMicros: bucket.charge,
				Status: "completed", CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	cases := []struct {
		query string
		key   string
		want  string
	}{
		{"", "last_seen", "a,c,b"}, // the default is "active most recently"
		{"&sort=last_seen", "last_seen", "a,c,b"},
		{"&sort=requests", "requests", "a,b,c"},
		{"&sort=charge", "charge", "b,c,a"},
	}
	for _, tc := range cases {
		body := decodeJSONBody(t, f.call(t, http.MethodGet,
			"/admin/api/v1/requests/dimensions?days=1&group_by=client&limit=10"+tc.query, "", cookie))
		if got := joinedKeys(body); got != tc.want {
			t.Fatalf("sort %q ordered %s, want %s", tc.query, got, tc.want)
		}
		if body["sort"] != tc.key {
			t.Fatalf("the response must echo the sort it used: %v, want %q", body["sort"], tc.key)
		}
		// The number of buckets is a property of the set, not of the order.
		if body["total"].(float64) != 3 {
			t.Fatalf("sort %q changed the total: %v", tc.query, body["total"])
		}
	}

	// A sort the store cannot honour is refused by name: silently falling back to the
	// default would answer a question nobody asked, which is the failure mode the
	// account_id filter had (M30).
	resp := f.call(t, http.MethodGet, "/admin/api/v1/requests/dimensions?group_by=client&sort=latency", "", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown sort", resp.StatusCode)
	}
	payload := decodeJSONBody(t, resp)
	message, _ := payload["error"].(map[string]any)
	text, _ := message["message"].(string)
	for _, name := range store.RequestLogDimensionSorts {
		if !strings.Contains(text, name) {
			t.Fatalf("the rejection must name %q: %q", name, text)
		}
	}
}

// The list filters are exact matches on the indexed identity columns, and the total must
// describe the same rows the page does.
func TestAdminRequestsFilterByIdentity(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0020", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "completed", Client: "dsh", Model: "deepseek-flash",
		SessionID: "session-ebf36761", Workspace: "/home/winger/work/ai_gateway",
	})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_dim0021", AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
		Status: "completed", Client: "codex", Model: "gpt-5.6-luna",
		SessionID: "01a08f27", Workspace: "/home/winger/work/other",
	})

	body := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?days=1&client=codex&limit=10", "", cookie))
	rows, _ := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("client filter returned %d rows, want 1", len(rows))
	}
	if item, _ := rows[0].(map[string]any); item["client"] != "codex" {
		t.Fatalf("filtered row = %v", rows[0])
	}
	if total, _ := body["total"].(float64); total != 1 {
		t.Fatalf("total = %v, want the filtered count", body["total"])
	}

	sessions := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?days=1&session_id=session-ebf36761", "", cookie))
	srows, _ := sessions["data"].([]any)
	if len(srows) != 1 {
		t.Fatalf("session filter returned %d rows, want 1", len(srows))
	}
	if item, _ := srows[0].(map[string]any); item["request_id"] != "req_dim0020" {
		t.Fatalf("session filter returned %v", srows[0])
	}

	workspaces := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?days=1&workspace=/home/winger/work/other", "", cookie))
	wrows, _ := workspaces["data"].([]any)
	if len(wrows) != 1 {
		t.Fatalf("workspace filter returned %d rows, want 1", len(wrows))
	}
}

// seedAdminKey inserts one API key into the admin fixture and returns its id. The fixture
// seeds an account but no key, and the request log's Key dimension reads its names from
// api_keys — so a test that asserts a name has to create the row that owns it.
func seedAdminKey(t *testing.T, f *adminFixture, name, prefix string) int64 {
	t.Helper()
	id, err := f.db.UpsertAPIKey(context.Background(), &domain.APIKey{
		AccountID: 1, Name: name, KeyPrefix: prefix, KeyHash: "hash-" + prefix, Status: "active",
	})
	if err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	return id
}

// The credential dimensions (M30) are read-time labels: the row keeps the account/key ids
// the request authenticated with, and the names come from the tables that own them.
func TestAdminRequestsCarryOwnerLabels(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	keyID := seedAdminKey(t, f, "dev-key", "sk-gw-label001")

	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_owner0001", AccountID: 1, APIKeyID: keyID, Endpoint: "/v1/responses",
		Status: "completed", Client: "dsh", Model: "deepseek-flash",
	})
	// A row with no credential at all: the ids stay 0 and the labels stay empty. It must
	// still be listed — an unowned request is exactly the kind an operator needs to see.
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_owner0002", AccountID: 0, APIKeyID: 0, Endpoint: "/v1/responses",
		Status: "completed", Client: "unknown", Model: "deepseek-flash",
	})

	body := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?days=1&limit=10", "", cookie))
	rows, _ := body["data"].([]any)
	byID := map[string]map[string]any{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		byID[row["request_id"].(string)] = row
	}
	named := byID["req_owner0001"]
	if named == nil {
		t.Fatalf("the recorded request is missing from the list: %v", rows)
	}
	if named["account_name"] != "acme" || named["api_key_name"] != "dev-key" || named["api_key_prefix"] != "sk-gw-label001" {
		t.Fatalf("owner labels = %v, want the account and key names", named)
	}
	unowned := byID["req_owner0002"]
	if unowned == nil {
		t.Fatalf("a request without a credential must still be listed: %v", rows)
	}
	if unowned["account_id"].(float64) != 0 || unowned["api_key_id"].(float64) != 0 {
		t.Fatalf("unowned ids = %v", unowned)
	}
	if unowned["account_name"] != "" || unowned["api_key_name"] != "" || unowned["api_key_prefix"] != "" {
		t.Fatalf("an id with no row must resolve to empty labels, not an error: %v", unowned)
	}

	detail := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests/req_owner0001", "", cookie))
	if detail["account_name"] != "acme" || detail["api_key_name"] != "dev-key" || detail["api_key_prefix"] != "sk-gw-label001" {
		t.Fatalf("detail owner labels = %v", detail)
	}
}

// The API key filter is an exact match on the credential, and the total describes the same
// rows the page does (the credential dimensions are what make "whose traffic is this"
// answerable, so a page and its count drifting apart would be a wrong answer, not a bug in
// a filter).
func TestAdminRequestsFilterByAPIKey(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	keyA := seedAdminKey(t, f, "key-a", "sk-gw-keya00001")
	keyB := seedAdminKey(t, f, "key-b", "sk-gw-keyb00001")

	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_keyf0001", AccountID: 1, APIKeyID: keyA, Status: "completed", Client: "dsh",
	})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_keyf0002", AccountID: 1, APIKeyID: keyB, Status: "completed", Client: "codex",
	})

	body := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?days=1&api_key_id="+strconv.FormatInt(keyB, 10)+"&limit=10", "", cookie))
	rows, _ := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("api_key_id filter returned %d rows, want 1", len(rows))
	}
	if item, _ := rows[0].(map[string]any); item["request_id"] != "req_keyf0002" || item["api_key_name"] != "key-b" {
		t.Fatalf("filtered row = %v", rows[0])
	}
	if total, _ := body["total"].(float64); total != 1 {
		t.Fatalf("total = %v, want the filtered count", body["total"])
	}

	// The credential filter composes with the account filter and with the identity
	// dimensions instead of replacing them.
	combined := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?days=1&account_id=1&api_key_id="+strconv.FormatInt(keyA, 10)+"&client=dsh", "", cookie))
	if crows, _ := combined["data"].([]any); len(crows) != 1 {
		t.Fatalf("combined filter returned %d rows, want 1", len(crows))
	}
	mismatch := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?days=1&account_id=1&api_key_id="+strconv.FormatInt(keyA, 10)+"&client=codex", "", cookie))
	if mrows, _ := mismatch["data"].([]any); len(mrows) != 0 {
		t.Fatalf("combined filter returned %d rows, want 0", len(mrows))
	}
}

// A malformed id is rejected instead of ignored: answering "every account" to a question
// that named one account is the failure mode the invoices endpoint was fixed for.
func TestAdminRequestsRejectMalformedOwnerFilters(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	for _, path := range []string{
		"/admin/api/v1/requests?days=1&account_id=abc",
		"/admin/api/v1/requests?days=1&api_key_id=1.5",
		"/admin/api/v1/requests/dimensions?days=1&group_by=api_key&account_id=abc",
		"/admin/api/v1/requests/dimensions?days=1&group_by=account&api_key_id=two",
	} {
		resp := f.call(t, http.MethodGet, path, "", cookie)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("%s: status = %d, want 400", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// The credential groupings bucket on ids and carry the names the console renders; the id
// 0 bucket (a row with no credential) is reported with an empty key, the same unknown
// bucket the other dimensions use, and its counts are kept.
func TestAdminRequestDimensionsGroupByOwner(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	keyA := seedAdminKey(t, f, "key-a", "sk-gw-keya00001")
	keyB := seedAdminKey(t, f, "key-b", "sk-gw-keyb00001")

	seedIdentityRow(t, f, &domain.RequestLogRecord{RequestID: "req_grp0001", AccountID: 1, APIKeyID: keyA, Status: "completed", Client: "dsh"})
	seedIdentityRow(t, f, &domain.RequestLogRecord{RequestID: "req_grp0002", AccountID: 1, APIKeyID: keyA, Status: "completed", Client: "dsh"})
	seedIdentityRow(t, f, &domain.RequestLogRecord{RequestID: "req_grp0003", AccountID: 1, APIKeyID: keyB, Status: "completed", Client: "codex"})
	seedIdentityRow(t, f, &domain.RequestLogRecord{RequestID: "req_grp0004", AccountID: 0, APIKeyID: 0, Status: "completed", Client: "unknown"})

	// The credential buckets are read with an explicit sort=requests so this case keeps
	// asserting identity and counts: the ordering has its own test
	// (TestAdminRequestDimensionsSort), and the seeded rows all share one timestamp.
	accounts := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=account&limit=10&sort=requests", "", cookie))
	if accounts["group_by"] != "account" {
		t.Fatalf("group_by = %v", accounts["group_by"])
	}
	if names, _ := accounts["dimensions"].([]any); !containsString(names, "account") || !containsString(names, "api_key") {
		t.Fatalf("the endpoint must advertise the credential groupings: %v", accounts["dimensions"])
	}
	arows, _ := accounts["rows"].([]any)
	if len(arows) != 2 {
		t.Fatalf("account buckets = %v, want acme and the unknown bucket", arows)
	}
	acme, _ := arows[0].(map[string]any)
	if acme["key"] != "1" || acme["account_name"] != "acme" || acme["requests"].(float64) != 3 {
		t.Fatalf("account bucket = %v", acme)
	}
	unknown, _ := arows[1].(map[string]any)
	if unknown["key"] != "" || unknown["requests"].(float64) != 1 || unknown["account_name"] != "" {
		t.Fatalf("the unknown credential bucket = %v, want key \"\" with its count kept", unknown)
	}

	keys := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=api_key&limit=10&sort=requests", "", cookie))
	krows, _ := keys["rows"].([]any)
	if len(krows) != 3 {
		t.Fatalf("api key buckets = %v, want key-a, key-b and the unknown bucket", krows)
	}
	first, _ := krows[0].(map[string]any)
	if first["key"] != strconv.FormatInt(keyA, 10) || first["api_key_name"] != "key-a" ||
		first["api_key_prefix"] != "sk-gw-keya00001" || first["requests"].(float64) != 2 {
		t.Fatalf("api key bucket = %v", first)
	}

	// The API key filter narrows the breakdown, so a drill-down from the list keeps
	// describing one key.
	filtered := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=api_key&api_key_id="+strconv.FormatInt(keyB, 10), "", cookie))
	frows, _ := filtered["rows"].([]any)
	if len(frows) != 1 {
		t.Fatalf("filtered breakdown = %v, want one bucket", frows)
	}
	if bucket, _ := frows[0].(map[string]any); bucket["api_key_name"] != "key-b" || bucket["requests"].(float64) != 1 {
		t.Fatalf("filtered bucket = %v", frows[0])
	}
}

// joinedKeys renders the bucket keys of a breakdown response in the order the server sent
// them: the ordering is what these cases are about, so it must not be read through a map.
func joinedKeys(body map[string]any) string {
	rows, _ := body["rows"].([]any)
	keys := make([]string, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		key, _ := row["key"].(string)
		keys = append(keys, key)
	}
	return strings.Join(keys, ",")
}

// containsString reports whether a decoded JSON array carries a string.
func containsString(values []any, want string) bool {
	for _, raw := range values {
		if text, _ := raw.(string); text == want {
			return true
		}
	}
	return false
}

func TestRequestLogCodexStructuredTitle(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/title=%v", stream, enabled), func(t *testing.T) {
				f := newFixture(t)
				f.cfg.Recording.RecordTitle = enabled
				f.setProviderConfig(t, `{"prefix":" ","chunks":3}`)
				// Echo the final user message to exercise JSON assembly and persistence.
				body := fmt.Sprintf(`{"model":"echo-model","stream":%v,"prompt_cache_key":"title-helper","input":[{"role":"user","content":"You are a helpful assistant. You will be presented with a user prompt, and your job is to provide a short title for a task that will be created from that prompt."},{"role":"user","content":"{\"title\":\"修复 Codex 标题\",\"description\":\"不应写入标题\"}"}]}`, stream)
				resp := f.postResponses(t, body)
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					t.Fatal(err)
				}
				row := f.latestLog(t)
				want := ""
				if enabled {
					want = "修复 Codex 标题"
				}
				if row.Client != "codex" || row.CallKind != "title" || row.Title != want || row.SessionID != "title-helper" {
					t.Fatalf("unexpected dimensions: client=%q kind=%q title=%q session=%q", row.Client, row.CallKind, row.Title, row.SessionID)
				}
				if row.OutputTextRecorded {
					t.Fatal("title recording must be independent of output recording")
				}
			})
		}
	}
}

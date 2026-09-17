package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/runtime"
)

// seedProviderSpend writes one metered attempt against a provider, which is the fact the cost
// cap is computed from. Going through the store rather than the data plane keeps these tests
// about the cap: what matters is that the cap reads the metering table, not how a row got there.
func seedProviderSpend(t *testing.T, db usageInserter, accountID, providerID, costMicros int64, at time.Time, requestID string) {
	t.Helper()
	if _, err := db.InsertUsage(context.Background(), &domain.UsageRecord{
		RequestID: requestID, AttemptNo: 1, AccountID: accountID,
		ProviderID: providerID, Model: "echo-model", ResolvedModel: "echo-model",
		DimensionsJSON: `{"output":1}`, CostMicros: costMicros, ChargeMicros: costMicros,
		Status: "completed", CreatedAt: at.UTC(),
	}); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

// usageInserter is the one store method these tests need, so they work against either fixture.
type usageInserter interface {
	InsertUsage(ctx context.Context, rec *domain.UsageRecord) (int64, error)
}

// ---------------------------------------------------------------------------
// data plane
// ---------------------------------------------------------------------------

// A provider over its cap stops being chosen, and when it is the only candidate the client gets
// the dedicated 503 instead of a misleading 502.
func TestProviderCostCapBlocksTheLastCandidateWith503(t *testing.T) {
	f := newFixture(t, withProviderCostCap(1_000, "none"))
	ctx := context.Background()
	providerID := f.providerID(t)

	seedProviderSpend(t, f.db, f.key.AccountID, providerID, 2_500, time.Now().Add(-time.Minute), "req-cap-1")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	resp := f.do(t, http.MethodPost, "/v1/responses", nonStreamBody, nil)
	if status, code := decodeError(t, resp); status != http.StatusServiceUnavailable || code != "provider_cost_capped" {
		t.Fatalf("capped provider answer = %d/%s, want 503/provider_cost_capped", status, code)
	}

	// A local rejection never reached an upstream, so it must not have metered anything: the cap
	// is computed from metering rows, and inventing one would move the very number the guard
	// reads.
	rows, err := f.db.ListUsage(ctx, f.key.AccountID, time.Time{}, time.Now().Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want only the seeded one", len(rows))
	}
}

// Raising the limit is effective immediately (the comparison uses the configuration, not the
// reading), and a reset makes the accumulated spend stop counting without touching a row.
func TestProviderCostCapReleasesOnRaiseAndOnReset(t *testing.T) {
	f := newFixture(t, withProviderCostCap(1_000, "none"))
	ctx := context.Background()
	providerID := f.providerID(t)
	seedProviderSpend(t, f.db, f.key.AccountID, providerID, 2_500, time.Now().Add(-time.Minute), "req-cap-2")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if resp := f.do(t, http.MethodPost, "/v1/responses", nonStreamBody, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected the provider to be blocked first, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// 1) Raise the cap above the spend: no refresh and no reset, immediately usable.
	provider, err := f.db.GetProvider(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	provider.CostLimitMicros = 10_000
	if _, err := f.db.UpsertProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	resp := f.do(t, http.MethodPost, "/v1/responses", nonStreamBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raising the cap must release the provider at once, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2) The same provider with the old limit, reset through the write path's own primitive: the
	// window start moves to now, so the earlier spend stops counting.
	provider, err = f.db.GetProvider(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	provider.CostLimitMicros = 1_000
	now := time.Now().UTC()
	provider.CostWindowStart = &now
	if _, err := f.db.UpsertProvider(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	f.providerCost.MarkReset(providerID, now)
	if resp := f.do(t, http.MethodPost, "/v1/responses", nonStreamBody, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a reset must release the provider at once, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// With a second provider available the request simply moves on: a spent budget is an availability
// fact, not a failure of the request.
func TestProviderCostCapFailsOverToAnotherProvider(t *testing.T) {
	f := newFixture(t, withProviderCostCap(1_000, "none"))
	ctx := context.Background()

	backup, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: "backup", Kind: "testecho", Enabled: true, Priority: 20, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := f.db.GetModelByName(ctx, "echo-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: backup, PublicModel: "echo-model", UpstreamModel: "echo-model",
		Enabled: true, MaxOutputTokens: 1024, CapabilitiesJSON: capabilitiesJSON,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertRoute(ctx, &domain.Route{
		ModelID: model.ID, ProviderID: backup, Priority: 20, Weight: 100, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	primaryID := f.providerID(t)
	seedProviderSpend(t, f.db, f.key.AccountID, primaryID, 2_500, time.Now().Add(-time.Minute), "req-cap-3")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	resp := f.do(t, http.MethodPost, "/v1/responses", nonStreamBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the request must fail over to the backup provider, got %d", resp.StatusCode)
	}
	requestID := resp.Header.Get("x-request-id")
	resp.Body.Close()

	// Whose money was it? The metering row answers, and it must name the backup provider only.
	providers, err := f.db.RequestProviders(ctx, []string{requestID})
	if err != nil {
		t.Fatal(err)
	}
	got := providers[requestID]
	if len(got) != 1 || got[0] != backup {
		t.Fatalf("served by %v, want only provider %d (the capped one is %d)", got, backup, primaryID)
	}
}

// The metric series are how an operator notices a spent budget without opening a provider page.
func TestProviderCostCapIsExportedAsMetrics(t *testing.T) {
	f := newFixture(t, withProviderCostCap(1_000, "none"))
	ctx := context.Background()
	seedProviderSpend(t, f.db, f.key.AccountID, f.providerID(t), 2_500, time.Now().Add(-time.Minute), "req-cap-4")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`aigw_provider_cost_used_micros{target="provider:`,
		`aigw_provider_cost_limit_micros{target="provider:`,
		`aigw_provider_cost_exceeded{target="provider:`,
		"} 2500\n", // the reading
		"} 1000\n", // the configured cap
		"} 1\n",    // and the flag that says the provider is out
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics are missing %q:\n%s", want, metricLines(text, "aigw_provider_cost"))
		}
	}
}

// metricLines keeps a failure readable when the metric block is the thing under test.
func metricLines(text, prefix string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// The data-plane fixture wires the same port the composition root does, so the admin read path
// has a reader to consult. The tracker's own behaviour is covered in internal/runtime.
func TestProviderCostTrackerIsWiredLikeTheCompositionRoot(t *testing.T) {
	f := newFixture(t)
	if f.srv.deps.ProviderCost == nil {
		t.Fatal("the fixture must wire the cost port, as the composition root does")
	}
	if got := f.srv.deps.ProviderCost.Status().IntervalS; got != int(runtime.CostRefreshInterval/time.Second) {
		t.Fatalf("refresh interval = %ds, want %s", got, runtime.CostRefreshInterval)
	}
	if block := f.srv.costBlock(); block == nil {
		t.Fatal("the stats block must be present when the port is wired")
	}
	if block := (&Server{}).costBlock(); block != nil {
		t.Fatalf("an unwired server must not publish a cost block: %v", block)
	}
}

// ---------------------------------------------------------------------------
// management surface
// ---------------------------------------------------------------------------

// The MCP half of the same contract: an agent sets the cap, reads it back, resets it, and the
// description it plans from states the default and the routing consequence (docs/mcp.md §4.5).
func TestMCPAdminManagesTheProviderCostCap(t *testing.T) {
	f := newAdminFixture(t)
	f.seedScopedMCPToken(t, testAdminMCPToken, mcpsrv.ScopeAdmin)
	f.seedScopedMCPToken(t, testReadMCPToken, mcpsrv.ScopeAdminRead)
	ctx := context.Background()
	providerID := seedAdminProvider(t, f, "mcp-capped")
	if _, err := f.api.deps.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	id := itoa(providerID)

	// 1) An admin-scope agent arms the cap, and the answer already carries the reading.
	set, isError := f.callTool(t, testAdminMCPToken, 1, toolAdminRequest,
		`{"name":"admin_update_provider","params":{"id":`+id+`},"confirm":true,`+
			`"body":{"cost_limit_micros":1000000,"cost_period":"daily"}}`)
	if isError {
		t.Fatalf("setting the cost cap over MCP failed: %+v", set)
	}
	setBody, _ := set["body"].(map[string]any)
	if setBody == nil || setBody["cost_limit_micros"] != float64(1_000_000) || setBody["cost_period"] != "daily" {
		t.Fatalf("the write did not take effect: %+v", set)
	}
	cost, _ := setBody["cost"].(map[string]any)
	if cost == nil || cost["used_micros"] != float64(0) || cost["exceeded"] != false {
		t.Fatalf("the write must report the reading it created: %+v", setBody)
	}

	// 2) Spending past the cap, then reading it back with an admin_read token.
	seedProviderSpend(t, f.db, 1, providerID, 2_000_000, time.Now(), "req-mcp-cap-1")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	read, isError := f.callTool(t, testReadMCPToken, 2, toolAdminRequest, `{"name":"admin_get_provider","params":{"id":`+id+`}}`)
	if isError {
		t.Fatalf("admin_read must read a provider: %+v", read)
	}
	readBody, _ := read["body"].(map[string]any)
	cost, _ = readBody["cost"].(map[string]any)
	if cost == nil || cost["used_micros"] != float64(2_000_000) || cost["exceeded"] != true {
		t.Fatalf("read-back mismatch: %+v", readBody)
	}

	// 3) admin_stats answers "is anything over its budget here" without listing providers.
	stats, isError := f.callTool(t, testReadMCPToken, 3, toolAdminRequest, `{"name":"admin_stats"}`)
	if isError {
		t.Fatalf("admin_stats failed: %+v", stats)
	}
	statsBody, _ := stats["body"].(map[string]any)
	block, _ := statsBody["provider_cost"].(map[string]any)
	if block == nil || block["currency"] != "USD" || block["refresh_s"] != float64(5) {
		t.Fatalf("admin_stats must report provider_cost with its freshness: %+v", statsBody)
	}
	providers, _ := block["providers"].(map[string]any)
	if entry, _ := providers[id].(map[string]any); entry == nil || entry["exceeded"] != true {
		t.Fatalf("admin_stats cost entry mismatch: %+v", block)
	}

	// 4) The description an agent reads *before* writing must state the unit, the default and the
	// consequence: "cost_limit_micros is an integer" leaves all three unguessable.
	described, isError := f.callTool(t, testAdminMCPToken, 4, toolAdminDescribe, `{"name":"admin_update_provider"}`)
	if isError {
		t.Fatalf("admin_describe failed: %+v", described)
	}
	schema, _ := described["body_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	limit, _ := properties["cost_limit_micros"].(map[string]any)
	limitDesc, _ := limit["description"].(string)
	for _, want := range []string{"微单位", "0 = 不限", "provider_cost_capped", "cost_cap_reached"} {
		if !strings.Contains(limitDesc, want) {
			t.Fatalf("the cost_limit_micros description must mention %q, got %q", want, limitDesc)
		}
	}
	period, _ := properties["cost_period"].(map[string]any)
	if period == nil {
		t.Fatalf("cost_period must be described: %v", properties)
	}
	if enum, _ := period["enum"].([]any); len(enum) != 3 {
		t.Fatalf("cost_period must enumerate its values, got %v", period["enum"])
	}
	reset, _ := properties["reset_cost"].(map[string]any)
	resetDesc, _ := reset["description"].(string)
	if !strings.Contains(resetDesc, "起算点") || !strings.Contains(resetDesc, "不修改") {
		t.Fatalf("the reset_cost description must say what a reset moves and what it keeps, got %q", resetDesc)
	}

	// 5) admin_read may read the cap but not write (including the reset).
	denied, deniedErr := f.callTool(t, testReadMCPToken, 5, toolAdminRequest,
		`{"name":"admin_update_provider","params":{"id":`+id+`},"confirm":true,"body":{"reset_cost":true}}`)
	if !deniedErr || !strings.Contains(denied["error_text"].(string), "scope=admin") {
		t.Fatalf("admin_read must not reset the accumulated cost: %+v", denied)
	}

	// 6) The reset releases the provider immediately, without waiting for the next reading.
	before := f.api.deps.ProviderCost.Stats()[providerID]
	if !before.Exceeded {
		t.Fatalf("the provider should be over its cap before the reset: %+v", before)
	}
	resetCall, resetErr := f.callTool(t, testAdminMCPToken, 6, toolAdminRequest,
		`{"name":"admin_update_provider","params":{"id":`+id+`},"confirm":true,"body":{"reset_cost":true}}`)
	if resetErr {
		t.Fatalf("the reset failed: %+v", resetCall)
	}
	resetBody, _ := resetCall["body"].(map[string]any)
	cost, _ = resetBody["cost"].(map[string]any)
	if cost == nil || cost["used_micros"] != float64(0) || cost["exceeded"] != false {
		t.Fatalf("the reset must clear the reading: %+v", resetBody)
	}
	if after := f.api.deps.ProviderCost.Stats()[providerID]; after.Exceeded || after.UsedMicros != 0 {
		t.Fatalf("the tracker must know about the reset at once: %+v", after)
	}
}

// The whole lifecycle over the real admin API: the fields are writable, validated, reported back
// with the configuration and the reading, and 复位 is a visible effect rather than a field the
// operator has to interpret.
func TestAdminProviderCostCapLifecycle(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	created := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/providers",
		`{"name":"capped-echo","kind":"testecho"}`, cookie))
	id := int64(created["id"].(float64))
	if created["cost_limit_micros"] != float64(0) || created["cost_period"] != "none" {
		t.Fatalf("a new provider must default to no cap: %v", created)
	}
	if _, present := created["cost"]; present {
		t.Fatalf("an uncapped provider must not carry a reading: %v", created["cost"])
	}
	path := "/admin/api/v1/providers/" + itoa(id)

	// A typo in the period must be rejected rather than read as "unlimited": that would turn a
	// monthly budget into a permanently accumulating one.
	bad := f.call(t, http.MethodPatch, path, `{"cost_period":"montly"}`, cookie)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid cost_period status = %d, want 400", bad.StatusCode)
	}
	bad.Body.Close()
	negative := f.call(t, http.MethodPatch, path, `{"cost_limit_micros":-1}`, cookie)
	if negative.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative limit status = %d, want 400", negative.StatusCode)
	}
	negative.Body.Close()

	// Spend recorded before the cap exists, which is exactly the case the first-enable anchor is
	// there for.
	seedProviderSpend(t, f.db, 1, id, 900_000, time.Now().Add(-time.Hour), "req-admin-cap-1")

	// A model, mapping and route, so the same exclusion is visible where an operator looks for
	// "why is this upstream not being used": the routing explain.
	f.call(t, http.MethodPost, "/admin/api/v1/models", `{"public_name":"capped-model"}`, cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/providers/"+itoa(id)+"/models",
		`{"public_model":"capped-model","upstream_model":"up-1"}`, cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/routes",
		`{"model":"capped-model","provider_id":`+itoa(id)+`,"upstream_model":"up-1"}`, cookie).Body.Close()

	enabled := decodeJSONBody(t, f.call(t, http.MethodPatch, path,
		`{"cost_limit_micros":1000000,"cost_period":"monthly"}`, cookie))
	if enabled["cost_limit_micros"] != float64(1_000_000) || enabled["cost_period"] != "monthly" {
		t.Fatalf("configuration not stored: %v", enabled)
	}
	if enabled["cost_window_start"] == nil {
		t.Fatalf("the first enable must anchor the window: %v", enabled)
	}
	cost, _ := enabled["cost"].(map[string]any)
	if cost == nil {
		t.Fatalf("a capped provider must carry its reading: %v", enabled)
	}
	if cost["used_micros"] != float64(0) {
		t.Fatalf("the anchor must exclude the earlier spend, got %v", cost)
	}
	if cost["currency"] != "USD" || cost["limit_micros"] != float64(1_000_000) || cost["exceeded"] != false {
		t.Fatalf("cost block = %v", cost)
	}

	// Money spent after the anchor counts, and crossing the cap flips the flag without any
	// further write.
	seedProviderSpend(t, f.db, 1, id, 1_100_000, time.Now(), "req-admin-cap-2")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	row := decodeJSONBody(t, f.call(t, http.MethodGet, path, "", cookie))
	cost, _ = row["cost"].(map[string]any)
	if cost["used_micros"] != float64(1_100_000) || cost["exceeded"] != true {
		t.Fatalf("reading after the crossing = %v", cost)
	}

	// The routing diagnostic must name the reason, otherwise an operator seeing "requests are
	// going to the other provider" has to guess between the budget and the upstream's health.
	explain := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/router/explain?model=capped-model", "", cookie))
	excluded, _ := explain["excluded"].([]any)
	reason := ""
	for _, item := range excluded {
		entry, _ := item.(map[string]any)
		if entry["provider"] == "capped-echo" {
			reason, _ = entry["reason"].(string)
		}
	}
	if reason != "cost_cap_reached" {
		t.Fatalf("explain exclusion = %q, want cost_cap_reached (excluded: %v)", reason, excluded)
	}

	// The list carries the same block, so the operator sees it without opening the detail page.
	list := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/providers?limit=50", "", cookie))
	rows, _ := list["data"].([]any)
	found := false
	for _, item := range rows {
		entry, _ := item.(map[string]any)
		if entry == nil || entry["name"] != "capped-echo" {
			continue
		}
		found = true
		if block, _ := entry["cost"].(map[string]any); block == nil || block["exceeded"] != true {
			t.Fatalf("list row cost block = %v", entry["cost"])
		}
	}
	if !found {
		t.Fatal("the capped provider is missing from the list")
	}

	// 复位: the accumulated cost stops counting immediately, and the response says so — the
	// operator must not have to wait for a refresh to see their click took effect.
	reset := decodeJSONBody(t, f.call(t, http.MethodPatch, path, `{"reset_cost":true}`, cookie))
	cost, _ = reset["cost"].(map[string]any)
	if cost == nil || cost["used_micros"] != float64(0) || cost["exceeded"] != false {
		t.Fatalf("reading after the reset = %v", cost)
	}
	if reset["cost_window_start"] == nil {
		t.Fatalf("the reset must move the window start: %v", reset)
	}
	// The metering rows are untouched: a reset moves the accounting, it does not rewrite history.
	after, err := f.db.ProviderCostsSince(ctx, map[int64]time.Time{id: {}})
	if err != nil {
		t.Fatal(err)
	}
	if after[id] != 2_000_000 {
		t.Fatalf("the metering rows must still carry both attempts, got %d", after[id])
	}

	// The audit trail names what changed, which is what makes a reset reviewable.
	audit := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/audit-logs?limit=20", "", cookie))
	logs, _ := audit["data"].([]any)
	seenReset := false
	for _, item := range logs {
		entry, _ := item.(map[string]any)
		changes, _ := entry["changes"].(map[string]any)
		if changes == nil || changes["cost_reset"] != true {
			continue
		}
		seenReset = true
		if changes["cost_limit_micros"] != float64(1_000_000) || changes["cost_period"] != "monthly" {
			t.Fatalf("the audit row must record the cap too: %v", changes)
		}
	}
	if !seenReset {
		t.Fatalf("no audit row recorded the reset: %v", logs)
	}
}

// /stats (and therefore MCP admin_stats) publishes the reader's freshness next to the readings:
// the number is a guard rail with a few seconds of lag, and saying so is part of the interface.
func TestAdminStatsPublishesTheCostBlock(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	created := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/providers",
		`{"name":"stats-echo","kind":"testecho","cost_limit_micros":500000}`, cookie))
	id := int64(created["id"].(float64))
	seedProviderSpend(t, f.db, 1, id, 250_000, time.Now(), "req-admin-cap-3")
	if err := f.providerCost.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	stats := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/stats", "", cookie))
	block, _ := stats["provider_cost"].(map[string]any)
	if block == nil {
		t.Fatalf("the stats payload must carry provider_cost: %v", stats)
	}
	if block["currency"] != "USD" || block["refresh_s"] != float64(5) || block["tracked"] != float64(1) {
		t.Fatalf("cost block header = %v", block)
	}
	if block["as_of"] == nil {
		t.Fatalf("the block must say when the reading was taken: %v", block)
	}
	providers, _ := block["providers"].(map[string]any)
	entry, _ := providers[itoa(id)].(map[string]any)
	if entry == nil || entry["used_micros"] != float64(250_000) || entry["exceeded"] != false {
		t.Fatalf("per-provider reading = %v", providers)
	}
}

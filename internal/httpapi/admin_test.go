package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/apikey"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/store"
	"github.com/winger/ai-gateway/internal/usage"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

const (
	adminUser     = "root"
	adminPassword = "correct-horse-battery-staple"
)

// fakeSealer stands in for internal/creds: it is reversible on purpose so the test
// can assert what was stored without importing the real key derivation.
type fakeSealer struct{ ready bool }

const sealedPrefix = "sealed:"

func (f *fakeSealer) Ready() bool { return f.ready }

func (f *fakeSealer) Seal(providerID int64, plaintext []byte) ([]byte, error) {
	if !f.ready {
		return nil, errSealerNotReady
	}
	return append([]byte(sealedPrefix), plaintext...), nil
}

func (f *fakeSealer) KeyNames(providerID int64, ciphertext []byte) []string {
	raw := strings.TrimPrefix(string(ciphertext), sealedPrefix)
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	return names
}

var errSealerNotReady = &domain.APIError{Message: "sealer not ready"}

// fakeProber records probe traffic and returns canned results.
type fakeProber struct {
	result    *runtime.ProbeResult
	actions   []pluginapi.Action
	restarted []int64
}

func (f *fakeProber) Probe(ctx context.Context, providerID int64, mode string) *runtime.ProbeResult {
	if f.result != nil {
		return f.result
	}
	return &runtime.ProbeResult{OK: true, Mode: mode, ProviderID: providerID, LatencyMS: 3}
}

func (f *fakeProber) Actions(ctx context.Context, providerID int64) ([]pluginapi.Action, error) {
	return f.actions, nil
}

func (f *fakeProber) RunAction(ctx context.Context, providerID int64, name string, in json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"ran":"` + name + `"}`), nil
}

func (f *fakeProber) Logs(ctx context.Context, providerID int64, tail int) ([]string, bool, error) {
	return []string{"stderr line"}, true, nil
}

func (f *fakeProber) Restart(ctx context.Context, providerID int64) error {
	f.restarted = append(f.restarted, providerID)
	return nil
}

type adminFixture struct {
	server      *httptest.Server
	db          *store.DB
	reg         *registry.Registry
	sealer      *fakeSealer
	prober      *fakeProber
	hookReloads int
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "admin.db")
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hash, err := admin.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := db.UpsertAdminUser(ctx, &domain.AdminUser{Username: adminUser, PasswordHash: hash, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	viewerHash, err := admin.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := db.UpsertAdminUser(ctx, &domain.AdminUser{Username: "reader", PasswordHash: viewerHash, Role: "viewer"}); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	if _, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", BillingMode: domain.BillingPostpaid, Status: "active"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	reg := registry.New(db)
	if _, err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	bal := balancer.New(balancer.DefaultConfig())
	router := routing.New(routing.Config{DefaultGrant: "all", Degradation: "strip"}, reg, bal)
	dispatcher := runtime.New(runtime.Config{}, db, reg, nil, bal, nil)
	sealer := &fakeSealer{ready: true}
	prober := &fakeProber{}
	fixture := &adminFixture{db: db, reg: reg, sealer: sealer, prober: prober}

	auth := admin.NewAuth(db, admin.Config{SessionTTL: time.Hour, LoginAttempts: 20, LoginWindow: time.Minute})
	srv := New(Deps{
		Config:        &cfg,
		Registry:      reg,
		Router:        router,
		Dispatcher:    dispatcher,
		Verifier:      apikey.New(db, apikey.DefaultConfig()),
		Limiter:       quota.New(4),
		Meter:         usage.New(db),
		Records:       db,
		Admin:         auth,
		AdminStore:    db,
		Accounts:      db,
		Providers:     db,
		Models:        db,
		Tags:          db,
		HookStore:     db,
		MCPTokenStore: db,
		Settings:      db,
		Secrets:       sealer,
		Prober:        prober,
		Reload: func(ctx context.Context) (any, error) {
			snap, err := reg.Reload(ctx)
			if err != nil {
				return nil, err
			}
			return snap.String(), nil
		},
		ReloadHooks: func(ctx context.Context) error {
			fixture.hookReloads++
			return nil
		},
		Version: "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	fixture.server = ts
	return fixture
}

func (f *adminFixture) call(t *testing.T, method, path, body, cookie string) *http.Response {
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

func (f *adminFixture) login(t *testing.T, username, password string) string {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	resp := f.call(t, http.MethodPost, "/admin/api/v1/auth/login", body, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == adminCookieName {
			return cookie.Value
		}
	}
	t.Fatal("login did not set a session cookie")
	return ""
}

func decodeJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	return payload
}

func TestAdminSurfaceRequiresSession(t *testing.T) {
	f := newAdminFixture(t)
	resp := f.call(t, http.MethodGet, "/admin/api/v1/providers", "", "")
	status, code := decodeError(t, resp)
	if status != http.StatusUnauthorized || code != "invalid_api_key" {
		t.Fatalf("unauthenticated status=%d code=%s", status, code)
	}
	bad := f.call(t, http.MethodPost, "/admin/api/v1/auth/login", `{"username":"root","password":"wrong"}`, "")
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", bad.StatusCode)
	}
	bad.Body.Close()
}

func TestAdminViewerCannotWrite(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, "reader", adminPassword)
	resp := f.call(t, http.MethodPost, "/admin/api/v1/providers", `{"name":"p1","kind":"testecho"}`, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer write status = %d, want 403", resp.StatusCode)
	}
	read := f.call(t, http.MethodGet, "/admin/api/v1/providers", "", cookie)
	defer read.Body.Close()
	if read.StatusCode != http.StatusOK {
		t.Fatalf("viewer read status = %d, want 200", read.StatusCode)
	}
}

func TestAdminProviderCredentialLifecycle(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	create := f.call(t, http.MethodPost, "/admin/api/v1/providers",
		`{"name":"openai-main","kind":"openai-chat","config":{"base_url":"https://api.example.com/v1"},`+
			`"credentials":{"api_key":"sk-secret","org":"acme"},"priority":20,"weight":50}`, cookie)
	payload := decodeJSONBody(t, create)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", create.StatusCode, payload)
	}
	if payload["has_credentials"] != true {
		t.Fatalf("has_credentials = %v, want true", payload["has_credentials"])
	}
	keys, _ := payload["credential_keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("credential_keys = %v, want 2 entries", payload["credential_keys"])
	}
	id := int64(payload["id"].(float64))
	plaintext, err := f.db.GetProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(plaintext.CredentialsEnc), sealedPrefix) {
		t.Fatalf("credentials were not sealed: %q", plaintext.CredentialsEnc)
	}
	if strings.HasPrefix(string(plaintext.CredentialsEnc), "{") {
		t.Fatal("credentials were stored as raw JSON instead of a sealed blob")
	}

	// PATCH without credentials keeps them; PATCH with {} clears them.
	patch := f.call(t, http.MethodPatch, "/admin/api/v1/providers/"+itoa(id), `{"weight":70}`, cookie)
	patched := decodeJSONBody(t, patch)
	if patched["has_credentials"] != true {
		t.Fatalf("credentials lost on unrelated patch: %v", patched["has_credentials"])
	}
	if patched["weight"] != float64(70) {
		t.Fatalf("weight = %v, want 70", patched["weight"])
	}

	clear := f.call(t, http.MethodPatch, "/admin/api/v1/providers/"+itoa(id), `{"credentials":{}}`, cookie)
	cleared := decodeJSONBody(t, clear)
	if cleared["has_credentials"] != false {
		t.Fatalf("credentials not cleared: %v", cleared["has_credentials"])
	}
}

func TestAdminProviderKindAndJSONValidation(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad kind", `{"name":"x","kind":"nope"}`, http.StatusBadRequest},
		{"bad name", `{"name":"has space","kind":"testecho"}`, http.StatusBadRequest},
		{"bad config json", `{"name":"x","kind":"testecho","config":"not-an-object"}`, http.StatusBadRequest},
		{"negative weight", `{"name":"x","kind":"testecho","weight":-1}`, http.StatusBadRequest},
		{"bad degradation", `{"name":"x","kind":"testecho","degradation":"maybe"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp := f.call(t, http.MethodPost, "/admin/api/v1/providers", tc.body, cookie)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

func TestAdminRouteReferencesAndDeleteGuard(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	provResp := f.call(t, http.MethodPost, "/admin/api/v1/providers", `{"name":"echo-a","kind":"testecho"}`, cookie)
	prov := decodeJSONBody(t, provResp)
	providerID := int64(prov["id"].(float64))

	modelResp := f.call(t, http.MethodPost, "/admin/api/v1/models", `{"public_name":"gpt-test","display_name":"GPT Test"}`, cookie)
	model := decodeJSONBody(t, modelResp)
	if modelResp.StatusCode != http.StatusCreated {
		t.Fatalf("model create status = %d body=%v", modelResp.StatusCode, model)
	}
	if model["enabled"] != true {
		t.Fatalf("new model should be enabled: %v", model)
	}

	missing := f.call(t, http.MethodPost, "/admin/api/v1/routes", `{"model":"ghost","provider":"echo-a","upstream_model":"m"}`, cookie)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("route with unknown model status = %d, want 404", missing.StatusCode)
	}

	routeResp := f.call(t, http.MethodPost, "/admin/api/v1/routes",
		`{"model":"gpt-test","provider":"echo-a","upstream_model":"echo-1","weight":40}`, cookie)
	route := decodeJSONBody(t, routeResp)
	if routeResp.StatusCode != http.StatusOK {
		t.Fatalf("route create status = %d body=%v", routeResp.StatusCode, route)
	}
	if route["model"] != "gpt-test" || route["provider"] != "echo-a" {
		t.Fatalf("route names not resolved: %v", route)
	}
	routeID := int64(route["id"].(float64))

	// The registry must be routable after the admin write.
	snap := f.snapshot(t)
	if len(snap.Routes) != 1 || len(snap.Models) != 1 {
		t.Fatalf("registry snapshot not reloaded: routes=%d models=%d", len(snap.Routes), len(snap.Models))
	}

	listed := f.call(t, http.MethodGet, "/admin/api/v1/routes", "", cookie)
	listPayload := decodeJSONBody(t, listed)
	if listPayload["count"] != float64(1) {
		t.Fatalf("route list count = %v, want 1", listPayload["count"])
	}

	guard := f.call(t, http.MethodDelete, "/admin/api/v1/providers/"+itoa(providerID), "", cookie)
	status, _ := decodeError(t, guard)
	if status != http.StatusConflict {
		t.Fatalf("delete with references status = %d, want 409", status)
	}

	forced := f.call(t, http.MethodDelete, "/admin/api/v1/providers/"+itoa(providerID)+"?force=true", "", cookie)
	forced.Body.Close()
	if forced.StatusCode != http.StatusOK {
		t.Fatalf("forced delete status = %d, want 200", forced.StatusCode)
	}
	if routes, err := f.db.ListRoutes(ctx); err != nil || len(routes) != 0 {
		t.Fatalf("cascade delete left routes: %v %v", routes, err)
	}
	if !containsID(f.prober.restarted, providerID) {
		t.Fatal("provider deletion did not stop the plugin process")
	}
	_ = routeID
}

func TestAdminProviderModelRefreshOnlyFillsBlanks(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	providerID, err := f.db.UpsertProvider(ctx, &domain.Provider{Name: "disc", Kind: "testecho", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// A hand-tuned mapping must survive a discovery refresh.
	if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: providerID, PublicModel: "manual-model", UpstreamModel: "custom-upstream",
		Enabled: true, Weight: 250, ContextWindow: 8192, Source: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	f.prober.result = &runtime.ProbeResult{
		OK: true, Mode: "models", ProviderID: providerID,
		Models: []pluginapi.ModelInfo{
			{ID: "manual-model", UpstreamModel: "upstream-from-discovery", ContextWindow: 32000},
			{ID: "brand-new", ContextWindow: 128000},
		},
	}
	resp := f.call(t, http.MethodPost, "/admin/api/v1/providers/"+itoa(providerID)+"/models/refresh", "", cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK || payload["ok"] != true {
		t.Fatalf("refresh payload = %v (status %d)", payload, resp.StatusCode)
	}
	if payload["added"] != float64(1) || payload["kept"] != float64(1) {
		t.Fatalf("refresh counts = %v", payload)
	}
	rows, err := f.db.ListProviderModels(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("provider models = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.PublicModel == "manual-model" {
			if row.UpstreamModel != "custom-upstream" || row.Weight != 250 {
				t.Fatalf("manual mapping was overwritten: %+v", row)
			}
		}
	}
}

func TestAdminProbeReportsBusinessFailure(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()
	providerID, err := f.db.UpsertProvider(ctx, &domain.Provider{Name: "flaky", Kind: "testecho", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	f.prober.result = &runtime.ProbeResult{OK: false, Mode: "health", ProviderID: providerID, Error: "upstream unreachable", LatencyMS: 12}

	resp := f.call(t, http.MethodPost, "/admin/api/v1/providers/"+itoa(providerID)+"/test", "", cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, want 200", resp.StatusCode)
	}
	if payload["ok"] != false || payload["error"] != "upstream unreachable" {
		t.Fatalf("probe payload = %v", payload)
	}
	stored, err := f.db.GetProvider(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.HealthJSON, "upstream unreachable") {
		t.Fatalf("probe outcome not persisted: %q", stored.HealthJSON)
	}
	logs := f.call(t, http.MethodGet, "/admin/api/v1/providers/"+itoa(providerID)+"/logs", "", cookie)
	logsPayload := decodeJSONBody(t, logs)
	if logsPayload["running"] != true {
		t.Fatalf("logs running = %v, want true", logsPayload["running"])
	}
}

func TestAdminHookWriteReloadsDispatcher(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	resp := f.call(t, http.MethodPost, "/admin/api/v1/hooks",
		`{"name":"audit-sink","type":"jsonl","url":"/tmp/hooks.jsonl","events":["response.completed"],"sample_rate":0.5}`, cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hook create status = %d body=%v", resp.StatusCode, payload)
	}
	if f.hookReloads != 1 {
		t.Fatalf("hook reloads = %d, want 1", f.hookReloads)
	}
	insecure := f.call(t, http.MethodPost, "/admin/api/v1/hooks",
		`{"name":"plain","type":"webhook","url":"http://example.com/hook"}`, cookie)
	insecure.Body.Close()
	if insecure.StatusCode != http.StatusBadRequest {
		t.Fatalf("http webhook status = %d, want 400", insecure.StatusCode)
	}
	badRate := f.call(t, http.MethodPost, "/admin/api/v1/hooks",
		`{"name":"rate","type":"jsonl","url":"/tmp/h.jsonl","sample_rate":2}`, cookie)
	badRate.Body.Close()
	if badRate.StatusCode != http.StatusBadRequest {
		t.Fatalf("sample_rate status = %d, want 400", badRate.StatusCode)
	}
}

func TestAdminSettingsAndMCPTokenRoundTrip(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	put := f.call(t, http.MethodPut, "/admin/api/v1/settings/recording.default", `{"value":{"input":"full"}}`, cookie)
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("setting put status = %d", put.StatusCode)
	}
	get := f.call(t, http.MethodGet, "/admin/api/v1/settings?key=recording.default", "", cookie)
	payload := decodeJSONBody(t, get)
	data, _ := payload["data"].(map[string]any)
	value, _ := data["recording.default"].(map[string]any)
	if value["input"] != "full" {
		t.Fatalf("setting round trip = %v", payload)
	}

	tokenResp := f.call(t, http.MethodPost, "/admin/api/v1/mcp-tokens", `{"name":"agent","account":"acme"}`, cookie)
	tokenPayload := decodeJSONBody(t, tokenResp)
	if tokenResp.StatusCode != http.StatusCreated {
		t.Fatalf("mcp token status = %d body=%v", tokenResp.StatusCode, tokenPayload)
	}
	token, _ := tokenPayload["token"].(string)
	if !strings.HasPrefix(token, "aigw_mcp") {
		t.Fatalf("token = %q, want aigw_mcp prefix", token)
	}
	id := int64(tokenPayload["id"].(float64))
	revoke := f.call(t, http.MethodDelete, "/admin/api/v1/mcp-tokens/"+itoa(id), "", cookie)
	revokePayload := decodeJSONBody(t, revoke)
	if revokePayload["status"] != "revoked" {
		t.Fatalf("revoke payload = %v", revokePayload)
	}

	unknown := f.call(t, http.MethodPost, "/admin/api/v1/mcp-tokens", `{"name":"x","account":"ghost"}`, cookie)
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", unknown.StatusCode)
	}
}

func TestAdminMappingValidation(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	bad := f.call(t, http.MethodPost, "/admin/api/v1/model-mappings", `{"kind":"weird","pattern":"a","target_model":"b"}`, cookie)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kind status = %d, want 400", bad.StatusCode)
	}
	noTarget := f.call(t, http.MethodPost, "/admin/api/v1/model-mappings", `{"kind":"prefix","pattern":"gpt-"}`, cookie)
	noTarget.Body.Close()
	if noTarget.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing target status = %d, want 400", noTarget.StatusCode)
	}
	ok := f.call(t, http.MethodPost, "/admin/api/v1/model-mappings",
		`{"kind":"prefix","pattern":"gpt-","target_model":"gpt-test","priority":10}`, cookie)
	mapping := decodeJSONBody(t, ok)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("mapping create status = %d body=%v", ok.StatusCode, mapping)
	}
	id := int64(mapping["id"].(float64))
	del := f.call(t, http.MethodDelete, "/admin/api/v1/model-mappings/"+itoa(id), "", cookie)
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("mapping delete status = %d", del.StatusCode)
	}
}

func (f *adminFixture) snapshot(t *testing.T) *registry.Snapshot {
	t.Helper()
	return f.reg.Snapshot()
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestAdminWritesRequireJSONContentType(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/admin/api/v1/providers",
		strings.NewReader("name=x&kind=testecho"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := decodeError(t, resp)
	if status != http.StatusBadRequest {
		t.Fatalf("form-encoded write status = %d, want 400", status)
	}
	// Deleting needs no body, so it must keep working without a content type.
	created := f.call(t, http.MethodPost, "/admin/api/v1/providers", `{"name":"csrf-ok","kind":"testecho"}`, cookie)
	payload := decodeJSONBody(t, created)
	id := int64(payload["id"].(float64))
	del := f.call(t, http.MethodDelete, "/admin/api/v1/providers/"+itoa(id), "", cookie)
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", del.StatusCode)
	}
}

func TestAdminRouterExplain(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	provider := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/providers",
		`{"name":"echo-explain","kind":"testecho"}`, cookie))
	providerID := int64(provider["id"].(float64))
	f.call(t, http.MethodPost, "/admin/api/v1/models", `{"public_name":"explain-model"}`, cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/providers/"+itoa(providerID)+"/models",
		`{"public_model":"explain-model","upstream_model":"up-1"}`, cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/routes",
		`{"model":"explain-model","provider_id":`+itoa(providerID)+`,"upstream_model":"up-1"}`, cookie).Body.Close()

	resp := f.call(t, http.MethodGet, "/admin/api/v1/router/explain?model=explain-model", "", cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explain status = %d body=%v", resp.StatusCode, payload)
	}
	if payload["canonical"] != "explain-model" {
		t.Fatalf("canonical = %v", payload["canonical"])
	}
	order, _ := payload["order"].([]any)
	if len(order) != 1 {
		t.Fatalf("explain order = %v, want one candidate", payload["order"])
	}
	first, _ := order[0].(map[string]any)
	if first["upstream_model"] != "up-1" || first["provider"] != "echo-explain" {
		t.Fatalf("candidate = %v", first)
	}

	// An unresolvable model is still a successful diagnosis: the exclusion says why.
	missing := f.call(t, http.MethodGet, "/admin/api/v1/router/explain?model=ghost", "", cookie)
	missingPayload := decodeJSONBody(t, missing)
	if missing.StatusCode != http.StatusOK {
		t.Fatalf("unknown model explain status = %d, want 200", missing.StatusCode)
	}
	if excluded, _ := missingPayload["excluded"].([]any); len(excluded) == 0 {
		t.Fatalf("unknown model explain should report an exclusion: %v", missingPayload)
	}
	noModel := f.call(t, http.MethodGet, "/admin/api/v1/router/explain", "", cookie)
	if noModel.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model status = %d, want 400", noModel.StatusCode)
	}
	noModel.Body.Close()
}

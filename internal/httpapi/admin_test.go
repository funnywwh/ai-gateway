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
	"github.com/winger/ai-gateway/internal/backup"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/billing"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/retention"
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
	probes    int
}

func (f *fakeProber) Probe(ctx context.Context, providerID int64, mode string) *runtime.ProbeResult {
	f.probes++
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
	api         *Server
	db          *store.DB
	reg         *registry.Registry
	cfg         *config.Config
	sealer      *fakeSealer
	prober      *fakeProber
	hookReloads int
	fxReloads   int
	fx          *pricing.FXStore
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	return newAdminFixtureWithout(t, "")
}

// newAdminFixtureWithout builds the management fixture with one resource port left
// unwired, by name ("backups", "invoices", "ledger", "codes", "reconciliation"). The
// unwired path is part of the contract: portReady answers 501/400 and the MCP bridge
// must pass that refusal through unchanged, so a test needs a way to reach it.
func newAdminFixtureWithout(t *testing.T, unwired string) *adminFixture {
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
	mcpService := mcpsrv.New(db, reg, mcpsrv.Config{MaxRows: 100, WindowDays: 30, Currency: "USD"})
	fixture := &adminFixture{db: db, reg: reg, cfg: &cfg, sealer: sealer, prober: prober}

	// The currency table mirrors the wiring in cmd/aigw: configuration first, then
	// the console override, and a reload after every settings write.
	fxStore := pricing.NewFXStore(cfg.Billing.Currency, cfg.Billing.FXRates)
	fixture.fx = fxStore
	reloadFX := func(ctx context.Context) error {
		fixture.fxReloads++
		rates := cfg.Billing.FXRates
		raw, found, err := db.GetSetting(ctx, pricing.SettingFXRates)
		if err != nil {
			return err
		}
		if found {
			override, err := pricing.ParseFXRates(raw)
			if err != nil {
				return err
			}
			rates = pricing.MergeFXRates(rates, override)
		}
		table, err := pricing.NewFXTable(cfg.Billing.Currency, rates)
		if err != nil {
			return err
		}
		fxStore.Replace(table.Ledger, table.Rates)
		return nil
	}

	auth := admin.NewAuth(db, admin.Config{SessionTTL: time.Hour, LoginAttempts: 20, LoginWindow: time.Minute})
	// The billing reads and the backup manager are wired here as well: they are the
	// ports the paged list endpoints need (M24), and a fixture without them would make
	// those endpoints answer 501 instead of a page.
	billingService := billing.NewService(ctx, db, billing.ServiceConfig{
		Writer:         billing.Config{BatchSize: 2, FlushInterval: 5 * time.Millisecond},
		ReservationTTL: time.Minute,
	}, nil)
	t.Cleanup(func() { billingService.Close(time.Second) })
	backupManager := backup.New(backup.Config{
		DatabasePath: cfg.Database.Path, Dir: filepath.Join(t.TempDir(), "backups"),
	}, db, nil)

	deps := Deps{
		Config:         &cfg,
		FX:             fxStore,
		ReloadFX:       reloadFX,
		Registry:       reg,
		Router:         router,
		Dispatcher:     dispatcher,
		Verifier:       apikey.New(db, apikey.DefaultConfig()),
		Limiter:        quota.New(4),
		Meter:          usage.New(db),
		Records:        db,
		Admin:          auth,
		AdminStore:     db,
		MCP:            mcpService,
		MCPTokens:      db,
		Accounts:       db,
		Providers:      db,
		Models:         db,
		Tags:           db,
		HookStore:      db,
		MCPTokenStore:  db,
		Settings:       db,
		Secrets:        sealer,
		Prober:         prober,
		PortalUsers:    db,
		Billing:        billingService,
		Ledger:         billingService,
		Invoices:       billingService,
		Codes:          billingService,
		Reconciliation: billingService,
		Backups:        backupManager,
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
	}
	switch unwired {
	case "backups":
		deps.Backups = nil
	case "invoices":
		deps.Invoices = nil
	case "ledger":
		deps.Ledger = nil
	case "codes":
		deps.Codes = nil
	case "reconciliation":
		deps.Reconciliation = nil
	case "":
	default:
		t.Fatalf("unknown port to leave unwired: %q", unwired)
	}
	// The retention janitor is wired here too: the manual prune endpoint is part of the
	// management surface, and a fixture without it would answer 501 (M25).
	deps.LogJanitor = retention.New(db, retention.Config{RetentionDays: cfg.Recording.RetentionDays}, nil)
	srv := New(deps)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	fixture.server = ts
	fixture.api = srv
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

func TestAdminProviderKindsDocumentEveryBuiltin(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	resp := f.call(t, http.MethodGet, "/admin/api/v1/provider-kinds", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider-kinds status = %d, want 200", resp.StatusCode)
	}
	payload := decodeJSONBody(t, resp)
	rows := payload["data"].([]any)
	if len(rows) != 3 {
		t.Fatalf("provider-kinds returned %d kinds, want the three builtin ones", len(rows))
	}
	found := map[string]map[string]any{}
	for _, row := range rows {
		entry := row.(map[string]any)
		found[entry["kind"].(string)] = entry
		if entry["schema_source"] != "builtin" {
			t.Fatalf("%v: schema_source = %v", entry["kind"], entry["schema_source"])
		}
		if strings.TrimSpace(entry["kind_note"].(string)) == "" {
			t.Fatalf("%v: kind_note is empty; operators need to know what the kind talks to", entry["kind"])
		}
		if entry["config_schema"] == nil {
			t.Fatalf("%v: config_schema is missing", entry["kind"])
		}
	}
	chat, ok := found["openai-chat"]
	if !ok {
		t.Fatal("openai-chat is missing from provider-kinds")
	}
	props := chat["config_schema"].(map[string]any)["properties"].(map[string]any)
	for _, field := range []string{"base_url", "api_key", "models", "thinking", "response_format"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("openai-chat config_schema does not document %q", field)
		}
	}
	// The whole point of the milestone: an operator must be told where the key goes.
	apiKey := props["api_key"].(map[string]any)
	if apiKey["x-prefer-credential"] != "api_key" {
		t.Fatalf("config.api_key = %v, want a pointer to the credential field", apiKey)
	}
	creds := chat["credentials_schema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := creds["api_key"]; !ok {
		t.Fatal("openai-chat credentials_schema does not document api_key")
	}
}

func TestAdminProviderDetailCarriesSchemaWithoutStartingPlugins(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// A builtin kind is documented straight from the binary.
	builtinID, err := f.db.UpsertProvider(ctx, &domain.Provider{Name: "echo", Kind: "testecho", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	detail := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/providers/"+itoa(builtinID), "", cookie))
	if detail["schema_source"] != "builtin" {
		t.Fatalf("builtin schema_source = %v", detail["schema_source"])
	}
	if detail["config_schema"] == nil {
		t.Fatal("a builtin kind must carry its config schema on the detail endpoint")
	}

	// A plugin kind is only documented by its handshake: the detail endpoint must
	// report that instead of starting the plugin process to find out.
	pluginID, err := f.db.UpsertProvider(ctx, &domain.Provider{Name: "ghost", Kind: "plugin:does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	before := f.prober.probes
	plugin := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/providers/"+itoa(pluginID), "", cookie))
	if plugin["schema_source"] != "plugin" {
		t.Fatalf("plugin schema_source = %v", plugin["schema_source"])
	}
	if plugin["config_schema"] != nil {
		t.Fatalf("a plugin kind has no builtin schema, got %v", plugin["config_schema"])
	}
	if !strings.Contains(plugin["kind_note"].(string), "握手") {
		t.Fatalf("plugin kind_note must say the schema comes from the handshake: %v", plugin["kind_note"])
	}
	if f.prober.probes != before {
		t.Fatal("reading provider documentation must not probe (and therefore must not start) the plugin")
	}

	// Once a handshake happened, its schema is reused — still without probing.
	discovered := `{"config_schema":{"type":"object","properties":{"base_url":{"type":"string","description":"上游根地址"}}},` +
		`"credentials_schema":{"type":"object","properties":{"access_token":{"type":"string","x-secret":true}}}}`
	if err := f.db.SetProviderDiscovered(ctx, pluginID, discovered, `{"ok":true}`, ""); err != nil {
		t.Fatal(err)
	}
	after := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/providers/"+itoa(pluginID), "", cookie))
	schema, ok := after["config_schema"].(map[string]any)
	if !ok {
		t.Fatalf("the last handshake schema was not surfaced: %v", after["config_schema"])
	}
	if _, ok := schema["properties"].(map[string]any)["base_url"]; !ok {
		t.Fatalf("surfaced plugin schema = %v", schema)
	}
	if f.prober.probes != before {
		t.Fatal("surfacing a recorded schema must not probe")
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

func TestAdminPricingSimulate(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	provider := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/providers",
		`{"name":"price-echo","kind":"testecho"}`, cookie))
	providerID := int64(provider["id"].(float64))
	f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"priced-model","sale_pricing":{"basis":"absolute","rules":[{"id":"sale","order":10,"when":{},"rates":{"output":2000000}}]}}`,
		cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/providers/"+itoa(providerID)+"/models",
		`{"public_model":"priced-model","upstream_model":"up","pricing_rules":{"rules":[{"id":"cost","order":10,"when":{},"rates":{"output":1000000}}]}}`,
		cookie).Body.Close()

	resp := f.call(t, http.MethodPost, "/admin/api/v1/pricing/simulate",
		`{"model":"priced-model","at":"2026-03-02T12:00:00Z","dimensions":{"output":1000000}}`, cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("simulate status = %d body=%v", resp.StatusCode, payload)
	}
	if payload["cost_micros"] != float64(1000000) {
		t.Fatalf("cost = %v, want 1000000", payload["cost_micros"])
	}
	if payload["charge_micros"] != float64(2000000) {
		t.Fatalf("charge = %v, want 2000000 (absolute sale table)", payload["charge_micros"])
	}
	sources, _ := payload["sources"].(map[string]any)
	if sources["cost"] == "none" || sources["sale"] == "none (cost_follow with the configured default markup)" {
		t.Fatalf("rule set sources were not resolved: %v", sources)
	}
	if _, ok := payload["snapshot"].(map[string]any); !ok {
		t.Fatalf("simulate must return a replayable snapshot: %v", payload["snapshot"])
	}

	// Inline rules let an operator preview an unsaved change.
	offpeak := f.call(t, http.MethodPost, "/admin/api/v1/pricing/simulate",
		`{"model":"priced-model","at":"2026-03-02T17:00:00Z","dimensions":{"output":1000000},`+
			`"cost_rules":{"rules":[{"id":"offpeak","order":10,"when":{"time_windows":[{"start":"16:30","end":"00:30"}]},"rates":{"output":100000}},`+
			`{"id":"standard","order":100,"when":{},"rates":{"output":1000000}}]}}`, cookie)
	offPeakPayload := decodeJSONBody(t, offpeak)
	if offPeakPayload["cost_rule_id"] != "offpeak" || offPeakPayload["cost_micros"] != float64(100000) {
		t.Fatalf("inline off-peak rules not applied: %v", offPeakPayload)
	}

	bad := f.call(t, http.MethodPost, "/admin/api/v1/pricing/simulate",
		`{"model":"priced-model","dimensions":{"output":1},"cost_rules":{"rules":[{"id":"x","order":10,"when":{"model_variant":"v"},"rates":{"output":1}}]}}`,
		cookie)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("rule set without a catch-all must be rejected: status %d", bad.StatusCode)
	}
	bad.Body.Close()
}

func TestAdminPricingValidate(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	good := f.call(t, http.MethodPost, "/admin/api/v1/pricing/validate",
		`{"rules":[{"id":"catchall","order":10,"when":{},"rates":{"input":100}},`+
			`{"id":"never","order":20,"when":{"model_variant":"x"},"rates":{"input":50}}]}`, cookie)
	payload := decodeJSONBody(t, good)
	if payload["valid"] != true {
		t.Fatalf("validate payload = %v", payload)
	}
	shadowed, _ := payload["shadowed"].([]any)
	if len(shadowed) != 1 {
		t.Fatalf("expected one shadowed rule, got %v", payload["shadowed"])
	}

	broken := f.call(t, http.MethodPost, "/admin/api/v1/pricing/validate",
		`{"rules":[{"id":"a","order":10,"when":{"model_variant":"x"},"rates":{"input":1}}]}`, cookie)
	brokenPayload := decodeJSONBody(t, broken)
	if broken.StatusCode != http.StatusOK || brokenPayload["valid"] != false {
		t.Fatalf("invalid rule set should report valid=false with 200: %v", brokenPayload)
	}
	message, _ := brokenPayload["error"].(string)
	if !strings.Contains(message, "catch-all") {
		t.Fatalf("error should name the missing catch-all: %q", message)
	}
}

func TestAdminPricingTargetsAndMarkup(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	provider := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/providers",
		`{"name":"price-provider","kind":"testecho"}`, cookie))
	providerID := int64(provider["id"].(float64))
	f.call(t, http.MethodPost, "/admin/api/v1/providers/"+itoa(providerID)+"/models",
		`{"public_model":"priced","upstream_model":"up","pricing_rules":{"rules":[{"id":"cost","order":10,"when":{},"rates":{"input":1000},"per_request_fee_micros":0},{"id":"alt","order":20,"when":{"model_variant":"x"},"rates":{"input":2000}}]}}`,
		cookie).Body.Close()
	f.call(t, http.MethodPost, "/admin/api/v1/models",
		`{"public_name":"priced","sale_pricing":{"basis":"cost_follow","markup_bp":10000,"dimension_markup_bp":{"output":20000}}}`,
		cookie).Body.Close()

	resp := f.call(t, http.MethodGet, "/admin/api/v1/pricing/targets", "", cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("targets status = %d body=%v", resp.StatusCode, payload)
	}
	targets, _ := payload["targets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want one sale and one cost entry", targets)
	}
	var sale, cost map[string]any
	for _, entry := range targets {
		item, _ := entry.(map[string]any)
		switch item["kind"] {
		case "sale":
			sale = item
		case "cost":
			cost = item
		}
	}
	if sale == nil || cost == nil {
		t.Fatalf("missing a target kind: %v", targets)
	}
	if sale["markup_bp"] != float64(10000) || sale["cost_rules_configured"] != true {
		t.Fatalf("sale target = %v", sale)
	}
	// A cost_follow sale side carries no rules of its own: the multiplier does the work,
	// so it must not be reported as having a catch-all rule.
	if sale["catch_all"] != false {
		t.Fatalf("sale target should have no rules: %v", sale)
	}
	if cost["catch_all"] != true {
		t.Fatalf("the cost table has a catch-all rule: %v", cost)
	}
	if cost["shadowed"] != float64(1) {
		t.Fatalf("cost shadowed = %v, want 1 (the variant rule is unreachable)", cost["shadowed"])
	}
	effective, _ := sale["effective_markup"].(map[string]any)
	if effective["bp"] != float64(10000) || effective["source"] != "model" {
		t.Fatalf("effective markup = %v", effective)
	}

	// Changing the multiplier must keep the rule array untouched.
	patched := f.call(t, http.MethodPatch, "/admin/api/v1/pricing/markup",
		`{"model":"priced","markup_bp":17500,"dimension_markup_bp":{"output":25000}}`, cookie)
	patchedPayload := decodeJSONBody(t, patched)
	if patched.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d body=%v", patched.StatusCode, patchedPayload)
	}
	if patchedPayload["markup_bp"] != float64(17500) {
		t.Fatalf("markup not applied: %v", patchedPayload)
	}
	stored, err := f.db.GetModelByName(context.Background(), "priced")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.SalePricingJSON, "cost_follow") {
		t.Fatalf("basis lost: %s", stored.SalePricingJSON)
	}

	bad := f.call(t, http.MethodPatch, "/admin/api/v1/pricing/markup", `{"model":"priced","markup_bp":-1}`, cookie)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative markup status = %d, want 400", bad.StatusCode)
	}
	missing := f.call(t, http.MethodPatch, "/admin/api/v1/pricing/markup", `{"model":"ghost","markup_bp":15000}`, cookie)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404", missing.StatusCode)
	}
}

func TestAdminPortalUserLifecycle(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	account := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/accounts",
		`{"name":"portal-tenant","billing_mode":"prepaid"}`, cookie))
	accountID := int64(account["id"].(float64))

	created := f.call(t, http.MethodPost, "/admin/api/v1/accounts/"+itoa(accountID)+"/portal-users",
		`{"username":"customer"}`, cookie)
	payload := decodeJSONBody(t, created)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", created.StatusCode, payload)
	}
	password, _ := payload["password"].(string)
	if len(password) < 12 {
		t.Fatalf("the one-time password is too short: %q", password)
	}
	if payload["must_change_password"] != true {
		t.Fatalf("a new portal user must be asked to change the password: %v", payload)
	}
	userID := int64(payload["id"].(float64))

	// The plaintext password must never be readable again.
	list := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/accounts/"+itoa(accountID)+"/portal-users", "", cookie))
	if list["count"] != float64(1) {
		t.Fatalf("list = %v", list)
	}
	entry, _ := list["data"].([]any)[0].(map[string]any)
	if _, leaked := entry["password"]; leaked {
		t.Fatalf("the list must not expose the password: %v", entry)
	}
	if entry["username"] != "customer" || entry["status"] != "active" {
		t.Fatalf("entry = %v", entry)
	}

	duplicate := f.call(t, http.MethodPost, "/admin/api/v1/accounts/"+itoa(accountID)+"/portal-users",
		`{"username":"customer"}`, cookie)
	duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate username status = %d, want 409", duplicate.StatusCode)
	}
	badName := f.call(t, http.MethodPost, "/admin/api/v1/accounts/"+itoa(accountID)+"/portal-users",
		`{"username":"has space"}`, cookie)
	badName.Body.Close()
	if badName.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad username status = %d, want 400", badName.StatusCode)
	}

	reset := f.call(t, http.MethodPost, "/admin/api/v1/portal-users/"+itoa(userID)+"/password", `{}`, cookie)
	resetPayload := decodeJSONBody(t, reset)
	if resetPayload["password"] == password {
		t.Fatal("the reset must issue a new password")
	}
	if resetPayload["sessions_revoked"] != true {
		t.Fatalf("reset payload = %v", resetPayload)
	}

	disabled := f.call(t, http.MethodDelete, "/admin/api/v1/portal-users/"+itoa(userID), "", cookie)
	disabledPayload := decodeJSONBody(t, disabled)
	if disabledPayload["status"] != "disabled" {
		t.Fatalf("disable payload = %v", disabledPayload)
	}
}

// TestKeyQuotaAndRecordingWritesAreValidated covers the two write paths that used to
// accept anything: a recording mode no code resolves, and a quota policy in a shape no
// code reads (the console used to advertise {"rate_limit":{"rpm":60}}, which was stored
// and then ignored, so a limit could look configured while traffic kept flowing).
func TestKeyQuotaAndRecordingWritesAreValidated(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	created := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/keys",
		`{"name":"dev","account":"acme","policy":{"rpm":5,"monthly_tokens":1000}}`, cookie))
	id, ok := created["id"].(float64)
	if !ok {
		t.Fatalf("key creation failed: %v", created)
	}
	path := "/admin/api/v1/keys/" + itoa(int64(id))

	// A policy field nothing reads is refused, with the accepted list in the message.
	bad := f.call(t, http.MethodPatch, path, `{"policy":{"rate_limit":{"rpm":60}}}`, cookie)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("a nested policy must be rejected, got %d", bad.StatusCode)
	}
	if message := apiErrorMessage(t, bad); !strings.Contains(message, "rate_limit") || !strings.Contains(message, "rpm") {
		t.Fatalf("the rejection must name the offending field and the accepted ones: %q", message)
	}

	// The mode a stale console sends is refused instead of stored.
	badMode := f.call(t, http.MethodPatch, path, `{"record_input_mode":"meta"}`, cookie)
	if badMode.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown recording mode must be rejected, got %d", badMode.StatusCode)
	}
	if message := apiErrorMessage(t, badMode); !strings.Contains(message, "record_input_mode") {
		t.Fatalf("the rejection must name the parameter: %q", message)
	}

	// A valid mode and a flat quota policy are stored and readable back.
	patched := decodeJSONBody(t, f.call(t, http.MethodPatch, path,
		`{"record_input_mode":"user","policy":{"rpm":5,"concurrency":2}}`, cookie))
	if patched["record_input_mode"] != "user" {
		t.Fatalf("patch response = %v", patched)
	}

	list := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/keys", "", cookie))
	rows, _ := list["data"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["id"] != id {
			continue
		}
		policy, _ := row["policy"].(map[string]any)
		if policy["rpm"] != float64(5) || policy["concurrency"] != float64(2) {
			t.Fatalf("the console must be able to read the key policy back: %v", row["policy"])
		}
		if row["record_input_mode"] != "user" {
			t.Fatalf("record_input_mode = %v", row["record_input_mode"])
		}
		return
	}
	t.Fatalf("the created key is missing from the list: %v", list)
}

// apiErrorMessage returns error.message from an API error response.
func apiErrorMessage(t *testing.T, resp *http.Response) string {
	t.Helper()
	payload := decodeJSONBody(t, resp)
	envelope, _ := payload["error"].(map[string]any)
	message, _ := envelope["message"].(string)
	return message
}

// TestPruneRequestsEndpointAndStats covers the retention surface end to end: the manual
// endpoint deletes exactly what the window says, the daily job is not needed to make it
// work, and /stats reports the policy and the write health the console renders.
func TestPruneRequestsEndpointAndStats(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()
	cookie := f.login(t, adminUser, adminPassword)

	seed := func(id string, ageDays int) {
		t.Helper()
		if err := f.db.PutRequestLog(ctx, &domain.RequestLogRecord{
			RequestID: id, AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
			RequestJSON: `{"input":"ping"}`, Status: "completed",
			CreatedAt: time.Now().UTC().AddDate(0, 0, -ageDays),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("req_old_a", 40)
	seed("req_old_b", 31)
	seed("req_new", 1)

	now := time.Now().UTC()
	expired, live := now.Add(-time.Hour), now.Add(time.Hour)
	for _, rec := range []*domain.ResponseRecord{
		{ID: "resp_old", Model: "m", Status: "completed", ExpiresAt: &expired},
		{ID: "resp_live", Model: "m", Status: "completed", ExpiresAt: &live},
	} {
		if err := f.db.PutResponse(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	// A viewer may read the log but not decide what to delete.
	viewer := f.login(t, "reader", adminPassword)
	denied := f.call(t, http.MethodPost, "/admin/api/v1/requests/prune", "", viewer)
	denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer prune status = %d, want 403", denied.StatusCode)
	}

	resp := f.call(t, http.MethodPost, "/admin/api/v1/requests/prune", "", cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prune status = %d: %v", resp.StatusCode, payload)
	}
	if payload["request_logs"] != float64(2) || payload["responses"] != float64(1) {
		t.Fatalf("prune result = %v, want the two old logs and the expired response", payload)
	}
	if payload["disabled"] == true {
		t.Fatalf("retention is on by default: %v", payload)
	}

	logs, err := f.db.ListRequestLogs(ctx, 0, time.Time{}, time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range logs {
		if row.RequestID != "req_new" {
			t.Fatalf("only the row inside the window may survive, found %q", row.RequestID)
		}
	}
	if _, err := f.db.GetResponse(ctx, "resp_old"); err == nil {
		t.Fatal("the expired stored response must be gone")
	}
	if _, err := f.db.GetResponse(ctx, "resp_live"); err != nil {
		t.Fatalf("a response inside its window must survive: %v", err)
	}

	audit, err := f.db.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range audit {
		if entry.Action == "prune" && entry.TargetType == "request_log" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the manual prune must be audited: %+v", audit)
	}

	stats := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/stats", "", cookie))
	block, ok := stats["request_log"].(map[string]any)
	if !ok {
		t.Fatalf("stats must report the request-log block: %v", stats)
	}
	if block["retention_days"] != float64(30) || block["enabled"] != true {
		t.Fatalf("retention block = %v", block)
	}
	if block["pruned"] != float64(3) {
		t.Fatalf("pruned = %v, want the 3 rows this process deleted", block["pruned"])
	}
	if _, ok := block["dropped"]; !ok {
		t.Fatalf("the console needs the drop counter too: %v", block)
	}
}

// TestPruneRequestsReportsDisabledRetention pins the 0 case: the endpoint must say that
// nothing is pruned rather than silently reporting a successful empty pass.
func TestPruneRequestsReportsDisabledRetention(t *testing.T) {
	f := newAdminFixture(t)
	// The janitor is built from the configuration at start-up, so a test that changes the
	// policy has to wire the janitor it wants (exactly as a restart would).
	f.api.deps.LogJanitor = retention.New(f.db, retention.Config{RetentionDays: 0}, nil)
	cookie := f.login(t, adminUser, adminPassword)

	payload := decodeJSONBody(t, f.call(t, http.MethodPost, "/admin/api/v1/requests/prune", "", cookie))
	if payload["disabled"] != true {
		t.Fatalf("retention_days=0 must report disabled: %v", payload)
	}
}

// Package httpapi exposes the public (v1) and administrative HTTP surfaces.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/apikey"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/usage"
)

// AdminService is the management authentication port.
type AdminService interface {
	Login(ctx context.Context, username, password, clientKey string) (*admin.Session, error)
	Authenticate(ctx context.Context, sessionID, token string) (*domain.AdminUser, error)
	Logout(ctx context.Context, sessionID string) error
}

// HookEmitter publishes lifecycle events (best effort, never blocking).
type HookEmitter interface {
	Emit(ctx context.Context, ev *domain.Event)
}

// Records is the persistence subset the public API needs.
type Records interface {
	PutResponse(ctx context.Context, rec *domain.ResponseRecord) error
	GetResponse(ctx context.Context, id string) (*domain.ResponseRecord, error)
	DeleteResponse(ctx context.Context, id string) error
	PutRequestLog(ctx context.Context, rec *domain.RequestLogRecord) error
}

// Deps are the collaborators of the HTTP server.
type Deps struct {
	Config     *config.Config
	Registry   *registry.Registry
	Router     *routing.Router
	Dispatcher *runtime.Dispatcher
	Verifier   *apikey.Verifier
	Limiter    *quota.Limiter
	Meter      *usage.Meter
	Records    Records
	// MCP/ MCPTokens enable the read-only MCP query endpoint (POST /mcp).
	MCP       MCPQuery
	MCPTokens MCPTokens
	// Hooks receives lifecycle events; nil disables hook delivery.
	Hooks HookEmitter
	// Admin enables the management API (session auth + CRUD).
	Admin      AdminService
	AdminStore AdminStore
	// The resource ports below are one narrow interface per resource family; a nil
	// port disables just that family with a 501 instead of breaking the server.
	Accounts      AccountAdmin
	Providers     ProviderAdmin
	Models        ModelAdmin
	Tags          TagAdmin
	HookStore     HookAdmin
	MCPTokenStore MCPTokenAdmin
	Settings      SettingsAdmin
	// Secrets seals provider credentials; Prober exercises providers out of band.
	Secrets Sealer
	Prober  Prober
	// ReloadHooks swaps the in-memory hook set after a hook write.
	ReloadHooks func(ctx context.Context) error
	// UI serves the embedded management console at /admin/ui/; nil disables it.
	UI http.Handler
	// Billing exposes ledger maintenance; Ledger reads balances and history.
	Billing BillingPort
	Ledger  LedgerAdmin
	// Invoicing, credits, redemption codes and reconciliation (M12).
	Invoices       InvoiceAdmin
	Codes          RedemptionAdmin
	Reconciliation ReconciliationAdmin
	// Reload rebuilds the routing snapshot after a write; InvalidateKey/All drop
	// cached credentials; KeyCacheSize reports cache occupancy for /stats.
	Reload        func(ctx context.Context) (any, error)
	InvalidateKey func(prefix string)
	InvalidateAll func()
	KeyCacheSize  func() int
	Log           *slog.Logger
	Version       string
}

// Server wires the HTTP surfaces.
type Server struct {
	deps  Deps
	mux   *http.ServeMux
	ready atomic.Bool
}

// New builds the HTTP server.
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.ready.Store(true)
	s.routes()
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	return s.withRecovery(s.withRequestID(s.withLogging(s.withAdminCSRF(s.mux))))
}

// withAdminCSRF rejects management writes that do not declare a JSON body. A
// cross-site HTML form cannot set that content type without a CORS preflight, so
// this closes the classic form-post CSRF hole on top of the SameSite=Lax cookie.
// DELETE carries no body and is exempt; the session cookie is still required.
func (s *Server) withAdminCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/api/v1/") {
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodPut:
				contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
				if !strings.HasPrefix(contentType, "application/json") {
					writeAPIError(w, domain.ErrInvalidRequest("management writes require Content-Type: application/json"))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// SetReady flips the readiness probe (used during shutdown/drain).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

func (s *Server) routes() {
	if s.deps.UI != nil {
		ui := s.deps.UI
		s.mux.Handle("GET /admin/ui/", http.StripPrefix("/admin/ui/", ui))
		s.mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
		})
	}

	s.mux.HandleFunc("POST /v1/responses", s.handleCreateResponse)
	s.mux.HandleFunc("GET /v1/responses/{id}", s.handleGetResponse)
	s.mux.HandleFunc("DELETE /v1/responses/{id}", s.handleDeleteResponse)
	s.mux.HandleFunc("GET /v1/models", s.handleListModels)
	s.mux.HandleFunc("POST /mcp", s.handleMCP)

	s.mux.HandleFunc("POST /admin/api/v1/auth/login", s.handleAdminLogin)
	s.mux.HandleFunc("POST /admin/api/v1/auth/logout", s.handleAdminLogout)
	s.mux.HandleFunc("GET /admin/api/v1/auth/me", s.handleAdminMe)
	s.mux.HandleFunc("GET /admin/api/v1/stats", s.handleAdminStats)
	s.mux.HandleFunc("GET /admin/api/v1/keys", s.handleAdminListKeys)
	s.mux.HandleFunc("POST /admin/api/v1/keys", s.handleAdminCreateKey)
	s.mux.HandleFunc("PATCH /admin/api/v1/keys/{id}", s.handleAdminPatchKey)
	s.mux.HandleFunc("GET /admin/api/v1/requests", s.handleAdminRequests)
	s.mux.HandleFunc("GET /admin/api/v1/requests/{id}", s.handleAdminRequestDetail)
	s.mux.HandleFunc("GET /admin/api/v1/audit-logs", s.handleAdminAuditLogs)

	s.mux.HandleFunc("GET /admin/api/v1/accounts", s.handleAdminListAccounts)
	s.mux.HandleFunc("POST /admin/api/v1/accounts", s.handleAdminCreateAccount)
	s.mux.HandleFunc("PATCH /admin/api/v1/accounts/{id}", s.handleAdminPatchAccount)

	s.mux.HandleFunc("GET /admin/api/v1/providers", s.handleAdminListProviders)
	s.mux.HandleFunc("POST /admin/api/v1/providers", s.handleAdminCreateProvider)
	s.mux.HandleFunc("GET /admin/api/v1/providers/{id}", s.handleAdminGetProvider)
	s.mux.HandleFunc("PATCH /admin/api/v1/providers/{id}", s.handleAdminPatchProvider)
	s.mux.HandleFunc("DELETE /admin/api/v1/providers/{id}", s.handleAdminDeleteProvider)
	s.mux.HandleFunc("POST /admin/api/v1/providers/{id}/test", s.handleAdminProbeProvider)
	s.mux.HandleFunc("POST /admin/api/v1/providers/{id}/restart", s.handleAdminRestartProvider)
	s.mux.HandleFunc("GET /admin/api/v1/providers/{id}/logs", s.handleAdminProviderLogs)
	s.mux.HandleFunc("GET /admin/api/v1/providers/{id}/actions", s.handleAdminProviderActions)
	s.mux.HandleFunc("POST /admin/api/v1/providers/{id}/actions/{name}", s.handleAdminRunProviderAction)
	s.mux.HandleFunc("GET /admin/api/v1/providers/{id}/models", s.handleAdminListProviderModels)
	s.mux.HandleFunc("POST /admin/api/v1/providers/{id}/models", s.handleAdminUpsertProviderModel)
	s.mux.HandleFunc("POST /admin/api/v1/providers/{id}/models/refresh", s.handleAdminRefreshProviderModels)
	s.mux.HandleFunc("GET /admin/api/v1/provider-models", s.handleAdminListProviderModels)
	s.mux.HandleFunc("DELETE /admin/api/v1/provider-models/{id}", s.handleAdminDeleteProviderModel)

	s.mux.HandleFunc("GET /admin/api/v1/models", s.handleAdminListModels)
	s.mux.HandleFunc("POST /admin/api/v1/models", s.handleAdminUpsertModel)
	s.mux.HandleFunc("PATCH /admin/api/v1/models/{name}", s.handleAdminUpsertModel)

	s.mux.HandleFunc("GET /admin/api/v1/model-mappings", s.handleAdminListMappings)
	s.mux.HandleFunc("POST /admin/api/v1/model-mappings", s.handleAdminUpsertMapping)
	s.mux.HandleFunc("DELETE /admin/api/v1/model-mappings/{id}", s.handleAdminDeleteMapping)

	s.mux.HandleFunc("GET /admin/api/v1/routes", s.handleAdminListRoutes)
	s.mux.HandleFunc("POST /admin/api/v1/routes", s.handleAdminUpsertRoute)
	s.mux.HandleFunc("PATCH /admin/api/v1/routes/{id}", s.handleAdminPatchRoute)
	s.mux.HandleFunc("DELETE /admin/api/v1/routes/{id}", s.handleAdminDeleteRoute)

	s.mux.HandleFunc("GET /admin/api/v1/tags", s.handleAdminListTags)
	s.mux.HandleFunc("POST /admin/api/v1/tags", s.handleAdminUpsertTag)
	s.mux.HandleFunc("DELETE /admin/api/v1/tags/{id}", s.handleAdminDeleteTag)

	s.mux.HandleFunc("GET /admin/api/v1/mcp-tokens", s.handleAdminListMCPTokens)
	s.mux.HandleFunc("POST /admin/api/v1/mcp-tokens", s.handleAdminCreateMCPToken)
	s.mux.HandleFunc("DELETE /admin/api/v1/mcp-tokens/{id}", s.handleAdminRevokeMCPToken)

	s.mux.HandleFunc("GET /admin/api/v1/hooks", s.handleAdminListHooks)
	s.mux.HandleFunc("POST /admin/api/v1/hooks", s.handleAdminUpsertHook)
	s.mux.HandleFunc("DELETE /admin/api/v1/hooks/{id}", s.handleAdminDeleteHook)

	s.mux.HandleFunc("GET /admin/api/v1/router/explain", s.handleAdminExplainRouter)
	s.mux.HandleFunc("POST /admin/api/v1/pricing/simulate", s.handleAdminSimulatePricing)
	s.mux.HandleFunc("GET /admin/api/v1/billing/invariants", s.handleAdminInvariants)
	s.mux.HandleFunc("GET /admin/api/v1/billing/status", s.handleAdminBillingStatus)
	s.mux.HandleFunc("POST /admin/api/v1/billing/rebuild-ledger", s.handleAdminRebuildLedger)
	s.mux.HandleFunc("GET /admin/api/v1/accounts/{id}/balance", s.handleAdminAccountBalance)
	s.mux.HandleFunc("GET /admin/api/v1/accounts/{id}/ledger", s.handleAdminAccountLedger)
	s.mux.HandleFunc("GET /admin/api/v1/invoices", s.handleAdminListInvoices)
	s.mux.HandleFunc("GET /admin/api/v1/accounts/{id}/invoices", s.handleAdminListInvoices)
	s.mux.HandleFunc("POST /admin/api/v1/accounts/{id}/invoices", s.handleAdminBuildInvoice)
	s.mux.HandleFunc("GET /admin/api/v1/invoices/{id}", s.handleAdminGetInvoice)
	s.mux.HandleFunc("POST /admin/api/v1/invoices/{id}/{action}", s.handleAdminInvoiceAction)
	s.mux.HandleFunc("POST /admin/api/v1/accounts/{id}/credits", s.handleAdminAccountCredits)
	s.mux.HandleFunc("GET /admin/api/v1/accounts/{id}/credits", s.handleAdminAccountCreditList)
	s.mux.HandleFunc("POST /admin/api/v1/redemption-codes", s.handleAdminGenerateCodes)
	s.mux.HandleFunc("GET /admin/api/v1/redemption-codes", s.handleAdminListCodes)
	s.mux.HandleFunc("POST /admin/api/v1/redemption-codes/redeem", s.handleAdminRedeemCode)
	s.mux.HandleFunc("POST /admin/api/v1/billing/reconcile", s.handleAdminReconcile)
	s.mux.HandleFunc("GET /admin/api/v1/billing/reconciliations", s.handleAdminReconciliations)
	s.mux.HandleFunc("POST /admin/api/v1/billing/failures/replay", s.handleAdminReplayFailures)
	s.mux.HandleFunc("POST /admin/api/v1/pricing/validate", s.handleAdminValidatePricing)
	s.mux.HandleFunc("GET /admin/api/v1/settings", s.handleAdminGetSettings)
	s.mux.HandleFunc("PUT /admin/api/v1/settings/{key}", s.handleAdminPutSetting)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

type ctxKey string

const (
	ctxRequestID ctxKey = "request_id"
	ctxKeyRecord ctxKey = "api_key"
	ctxAccount   ctxKey = "account"
)

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func requestID(w http.ResponseWriter, r *http.Request) string {
	if id := r.Header.Get("x-request-id"); id != "" {
		return id
	}
	return ids.Request()
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		w.Header().Set("x-request-id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.deps.Log.Debug("http request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(started).Milliseconds(),
			"request_id", requestIDFrom(r.Context()),
		)
	})
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.deps.Log.Error("panic in handler", "err", fmt.Sprint(rec), "path", r.URL.Path)
				writeAPIError(w, domain.ErrInternal("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush keeps SSE working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*domain.APIKey, *domain.Account, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		header = r.Header.Get("x-api-key")
	}
	key, account, err := s.deps.Verifier.Verify(r.Context(), header)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return nil, nil, false
	}
	return key, account, true
}

// ---------------------------------------------------------------------------
// responses
// ---------------------------------------------------------------------------

func writeAPIError(w http.ResponseWriter, err *domain.APIError) {
	if err == nil {
		err = domain.ErrInternal("unknown error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.Status)
	payload := map[string]any{
		"error": map[string]any{
			"message": err.Message,
			"type":    string(err.Type),
			"code":    err.Code,
		},
	}
	if err.Param != "" {
		payload["error"].(map[string]any)["param"] = err.Param
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func toAPIError(err error) *domain.APIError {
	if err == nil {
		return nil
	}
	if apiErr, ok := domain.AsAPIError(err); ok {
		return apiErr
	}
	if errors.Is(err, context.Canceled) {
		return domain.ErrUpstream(499, "request cancelled by the client")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.ErrGatewayTimeout("request deadline exceeded")
	}
	return domain.ErrInternal(err.Error())
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	broken  bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w: w, flusher: flusher}
}

// Send writes one SSE frame (escape-free framing: LF separators are written as bytes).
func (s *sseWriter) Send(ev *responses.Event) error {
	if s.broken {
		return errClientGone
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s%cdata: %s%c%c", ev.Type, byte(0x0A), payload, byte(0x0A), byte(0x0A)); err != nil {
		s.broken = true
		return errClientGone
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

var errClientGone = errors.New("httpapi: client disconnected")

// ---------------------------------------------------------------------------
// health
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.deps.Version})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Registry.Snapshot()
	ready := s.ready.Load() && snap.Ready()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":    map[bool]string{true: "ready", false: "not_ready"}[ready],
		"registry":  snap.String(),
		"providers": len(snap.Providers),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	balances := map[string]any{}
	metrics := s.deps.Router.Balancer().SnapshotMetrics()
	cooldowns := s.deps.Router.Balancer().Cooldowns(time.Now())
	for key, m := range metrics {
		balances[key] = map[string]any{
			"inflight": m.Inflight, "samples": m.Samples,
			"latency_ewma_ms": m.LatencyEWMA, "total": m.Total, "failures": m.Failures,
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	lf := byte(0x0A)
	_, _ = fmt.Fprintf(w, "# HELP aigw_provider_inflight Requests currently in flight per provider%c", lf)
	_, _ = fmt.Fprintf(w, "# TYPE aigw_provider_inflight gauge%c", lf)
	for key, m := range metrics {
		_, _ = fmt.Fprintf(w, "aigw_provider_inflight{target=%q} %d%c", key, m.Inflight, lf)
	}
	_, _ = fmt.Fprintf(w, "# TYPE aigw_provider_failures counter%c", lf)
	for key, m := range metrics {
		_, _ = fmt.Fprintf(w, "aigw_provider_failures{target=%q} %d%c", key, m.Failures, lf)
	}
	_, _ = fmt.Fprintf(w, "# TYPE aigw_cooldowns gauge%c", lf)
	for key, until := range cooldowns {
		_, _ = fmt.Fprintf(w, "aigw_cooldown_until_seconds{target=%q} %d%c", key, until.Unix(), lf)
	}
	_ = balances
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// normalizedModel strips an optional @provider suffix for presentation purposes.
func normalizedModel(model string) string {
	if idx := strings.LastIndex(model, "@"); idx > 0 {
		return model[:idx]
	}
	return model
}

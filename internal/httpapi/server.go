// Package httpapi exposes the public (v1) and administrative HTTP surfaces.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
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
	// MCP/ MCPTokens enable the MCP endpoint (POST /mcp): account query tools plus,
	// for admin-scoped tokens, the administrative tool surface.
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
	// Backups snapshots the database on a schedule (M16).
	Backups BackupAdmin
	// PortalUsers manages customer self-service logins (M14).
	PortalUsers PortalUserAdmin
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
	// admin is the declarative management route table; registration and the MCP
	// admin bridge both read it, so the two surfaces cannot drift apart.
	admin      []adminRoute
	adminIndex *adminEndpointIndex
	// registered records every pattern handed to the mux (tests compare it against
	// the table; it is never read on the request path).
	registered []string
}

// New builds the HTTP server.
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.admin = s.adminRoutes()
	s.adminIndex = newAdminEndpointIndex(s.admin)
	if binder, ok := deps.MCP.(MCPBackendBinder); ok {
		// The MCP service gains the administrative surface from here: the transport
		// layer owns both halves, so no other package needs to know they are linked.
		binder.SetBackend(&adminBackend{s: s})
	}
	s.ready.Store(true)
	s.routes()
	return s
}

// handle registers one pattern and records it for the coverage tests.
func (s *Server) handle(pattern string, handler http.HandlerFunc) {
	s.registered = append(s.registered, pattern)
	s.mux.HandleFunc(pattern, handler)
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
	if s.deps.Config != nil && s.deps.Config.Server.Pprof {
		// Profiling is opt-in: it exposes goroutine dumps and heap profiles.
		s.mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		s.mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		s.mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		s.mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		s.mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}

	if s.deps.UI != nil {
		ui := s.deps.UI
		s.registered = append(s.registered, "GET /admin/ui/")
		s.mux.Handle("GET /admin/ui/", http.StripPrefix("/admin/ui/", ui))
		s.handle("GET /admin/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
		})
	}

	s.handle("POST /v1/responses", s.handleCreateResponse)
	s.handle("GET /v1/responses/{id}", s.handleGetResponse)
	s.handle("DELETE /v1/responses/{id}", s.handleDeleteResponse)
	s.handle("GET /v1/models", s.handleListModels)
	s.handle("POST /mcp", s.handleMCP)

	// The management surface comes from the declarative table (admin_routes.go):
	// one entry per endpoint, carrying both the handler and the MCP metadata, so a
	// new endpoint cannot exist on the wire without its agent-facing description.
	for _, route := range s.admin {
		s.handle(route.pattern(), route.Handler)
	}

	s.handle("GET /healthz", s.handleHealthz)
	s.handle("GET /readyz", s.handleReadyz)
	s.handle("GET /metrics", s.handleMetrics)
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

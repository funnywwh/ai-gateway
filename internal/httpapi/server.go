// Package httpapi exposes the public (v1) and administrative HTTP surfaces.
package httpapi

import (
	"bytes"
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
	"github.com/winger/ai-gateway/internal/chat"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/localdshgw"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/store"
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

// LogRecorder batches the per-request audit writes instead of performing them inside the
// handler. It is optional: when Deps.LogRecorder is nil the server writes the same rows
// synchronously through Records, which is what tests and small deployments want.
//
// Contract: Enqueue must not block on the database, and WriteNow must write immediately
// (used for the content-free fallback row, which must not be batched again behind the
// rows whose failure caused it).
type LogRecorder interface {
	EnqueueRecording(resp *domain.ResponseRecord, log *domain.RequestLogRecord)
	WriteNow(ctx context.Context, resp *domain.ResponseRecord, log *domain.RequestLogRecord) error
	Stats() map[string]any
}

// responseWaiter is the optional read-after-write half of a batching LogRecorder: a reader
// that names a response id can wait for that id's row specifically. A recorder that does
// not implement it (or no recorder at all) means reads never need to wait.
type responseWaiter interface {
	AwaitResponse(ctx context.Context, id string) error
}

// KeyStore is the key-writing subset the dshgw provisioning flow needs.
type KeyStore interface {
	ListAPIKeys(ctx context.Context, accountID int64) ([]*domain.APIKey, error)
	UpsertAPIKey(ctx context.Context, k *domain.APIKey) (int64, error)
}

// DshgwAdminOps is the aigw-side view of the dshgw local provisioning channel. The key
// arguments travel only over the root-owned local socket and are never logged.
type DshgwAdminOps interface {
	CreateTenant(ctx context.Context, name, key string) error
	StartTenant(ctx context.Context, name string) error
	StopTenant(ctx context.Context, name string) error
	SetTenantKey(ctx context.Context, name, key string) error
	ListTenants(ctx context.Context) ([]localdshgw.TenantInfo, error)
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
	// LogRecorder batches the request-log and stored-response writes in the background.
	// nil keeps the synchronous per-request write path.
	LogRecorder LogRecorder
	// MCP/ MCPTokens enable the MCP endpoint (POST /mcp): account query tools plus,
	// for admin-scoped tokens, the administrative tool surface.
	MCP       MCPQuery
	MCPTokens MCPTokens
	// Hooks receives lifecycle events; nil disables hook delivery.
	Hooks HookEmitter
	// LogJanitor prunes recorded observability data past the retention window; nil
	// disables the manual prune endpoint (the daily job lives in cmd/aigw).
	LogJanitor       LogJanitor
	DimensionRollups interface {
		DimensionRollupStats(context.Context) (store.DimensionRollupStatus, error)
	}
	// Admin enables the management API (session auth + CRUD).
	Admin      AdminService
	AdminStore AdminStore
	// ChatStore is the owner-scoped persistence of the console chat. When it is set, the
	// chat service (and its preview-ticket signer) is built here, so the composition root
	// only has to hand over the store.
	ChatStore chat.Store
	// Chat overrides the built-in chat service (tests drive the handlers with a fake).
	Chat ChatService
	// The resource ports below are one narrow interface per resource family; a nil
	// port disables just that family with a 501 instead of breaking the server.
	Accounts      AccountAdmin
	Providers     ProviderAdmin
	Models        ModelAdmin
	Tags          TagAdmin
	Org           OrgAdmin
	HookStore     HookAdmin
	MCPTokenStore MCPTokenAdmin
	Settings      SettingsAdmin
	// Secrets seals provider credentials; Prober exercises providers out of band.
	Secrets Sealer
	Prober  Prober
	// Capacity reports the provider concurrency gates (in-flight slots, waiting queues and
	// the queue policy). A nil port omits the /stats block and the per-provider field.
	Capacity Capacity
	// ReloadHooks swaps the in-memory hook set after a hook write.
	ReloadHooks func(ctx context.Context) error
	// FX is the live currency table (ledger currency + rates). A nil store means
	// "no conversion": every currency is read as the ledger currency, which is what
	// a deployment with a single currency wants. ReloadFX swaps the table after an
	// operator edits billing.fx_rates in the console.
	FX       *pricing.FXStore
	ReloadFX func(ctx context.Context) error
	// UI serves the embedded management console at /admin/ui/; nil disables it.
	UI http.Handler
	// DshgwAdmin drives the dshgw local provisioning channel (M52). nil makes the
	// console dsh toggle answer 501 instead of pretending to provision.
	DshgwAdmin DshgwAdminOps
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
	// Version is the release version (a.b.c) and Revision the short commit the binary
	// was built from. Both are build-time facts surfaced by /version, /healthz and the
	// console badge, so an operator can tell what is running without reading logs.
	Version  string
	Revision string
	// UIAssets is the shape of the console assets this binary carries: "minified" for a
	// release build (compiled with -overlay) or "source" for a hand build. It answers,
	// from the running instance, the question that used to require
	// `strings bin/aigw | grep -c renderShell`. Empty is reported as "unknown" rather
	// than omitted, so a reader can always tell "this deployment is not minified" from
	// "this binary predates the field". Named UIAssets rather than UI because UI above
	// is the console handler itself: two fields of one struct cannot both be UI.
	// See docs/design/m54-console-asset-shape.md.
	UIAssets string
	// UIEncoding is the transfer encoding the console assets can be served with ("gzip"
	// when a release build embedded the pre-compressed copies, "identity" otherwise). It
	// answers, from the running instance, what a plain `curl -D-` used to be the only way to
	// find out. Empty is reported as "unknown", like UIAssets. See
	// docs/design/m55-console-transfer-compression.md.
	UIEncoding string
}

// uiShape is the ui field as it goes on the wire. An empty value means the caller
// supplied none (a fixture, an embedding application, a binary older than M54), and
// saying so out loud is the point: a missing key would be ambiguous.
func (s *Server) uiShape() string {
	if s.deps.UIAssets == "" {
		return "unknown"
	}
	return s.deps.UIAssets
}

// uiEncodingShape is ui_encoding as it goes on the wire, with the same rule as uiShape: an
// empty value is reported rather than omitted, so a reader never has to guess whether a
// missing key means "not compressed" or "older binary".
func (s *Server) uiEncodingShape() string {
	if s.deps.UIEncoding == "" {
		return "unknown"
	}
	return s.deps.UIEncoding
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
	// chat is the console chat service (nil when the deployment disabled it).
	chat ChatService
	// chatSigner signs the short-lived preview tickets.
	chatSigner *chatTicketSigner
	// Request-log write health: a failed content write is retried as a skeleton row, and
	// a failure of that retry is counted here rather than only logged.
	requestLogWriteFailures atomic.Int64
	requestLogDropped       atomic.Int64
}

// New builds the HTTP server.
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.admin = s.adminRoutes()
	s.adminIndex = newAdminEndpointIndex(s.admin)
	s.chatSigner = newChatTicketSigner()
	s.chat = s.newChatService()
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
	return s.withBasePath(s.withRecovery(s.withRequestID(s.withLogging(s.withAdminCSRF(s.mux)))))
}

// basePath is the deployment's mount prefix ("" when the surface is served from the
// root). It is read from the configuration rather than from a forwarded header: a
// header a proxy sets would have to be trusted, while this one is a deployment fact.
func (s *Server) basePath() string {
	if s.deps.Config == nil {
		return ""
	}
	return s.deps.Config.Server.NormalizedBasePath()
}

// url joins the mount prefix with an internal path. It is the one place a generated
// URL learns about the prefix.
func (s *Server) url(path string) string {
	return s.basePath() + path
}

// withBasePath mounts the whole surface under the configured prefix. The prefix is
// stripped before routing, so every pattern stays written as if the server owned the
// root ("GET /admin/ui/", "POST /v1/responses") and a prefixed deployment cannot drift
// from an unprefixed one. A request outside the prefix is answered 404 here, because a
// proxy that forwards the prefix is the only way such a request can arrive.
func (s *Server) withBasePath(next http.Handler) http.Handler {
	base := s.basePath()
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := trimBasePath(r.URL.Path, base)
		if !ok {
			writeAPIError(w, domain.ErrNotFound("no route matches this path"))
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		if r.URL.RawPath != "" {
			r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, base)
		}
		next.ServeHTTP(w, r2)
	})
}

// trimBasePath removes prefix from path. The prefix only matches on a segment
// boundary, so a mount at /aigw never swallows /aigw-other.
func trimBasePath(path, prefix string) (string, bool) {
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return "", false
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
			// Built from the mount prefix rather than written literally: a prefixed
			// deployment has to redirect inside its own mount, and http.Redirect would
			// not add a prefix the handler does not know about.
			http.Redirect(w, r, s.url("/admin/ui/"), http.StatusMovedPermanently)
		})
	}

	s.handle("POST /v1/responses", s.handleCreateResponse)
	s.handle("GET /v1/responses/{id}", s.handleGetResponse)
	s.handle("DELETE /v1/responses/{id}", s.handleDeleteResponse)
	s.handle("GET /v1/models", s.handleListModels)
	s.handle("POST /v1/dshgw/authorize", s.handleDSHGWAuthorize)
	s.handle("POST /mcp", s.handleMCP)

	// The management surface comes from the declarative table (admin_routes.go):
	// one entry per endpoint, carrying both the handler and the MCP metadata, so a
	// new endpoint cannot exist on the wire without its agent-facing description.
	for _, route := range s.admin {
		s.handle(route.pattern(), route.Handler)
	}

	// The preview document is fetched by a sandboxed frame that carries no cookie, so it
	// authorizes with the short-lived ticket the console obtained instead.
	s.handle("GET /admin/chat-artifact/{id}", s.handleAdminChatArtifact)

	s.handle("GET /healthz", s.handleHealthz)
	s.handle("GET /readyz", s.handleReadyz)
	s.handle("GET /metrics", s.handleMetrics)
	s.handle("GET /version", s.handleVersion)
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
	// An in-process caller (the console chat) has no bearer token to send: it names a key
	// in its own session and the server resolves it with the same checks bearer traffic
	// gets. The context key is an unexported type, so an external request cannot install
	// one no matter what it sends.
	if identity, ok := verifiedIdentityFrom(r.Context()); ok && identity.key != nil && identity.account != nil {
		return identity.key, identity.account, true
	}
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
	// Every candidate was at its provider concurrency limit: a capacity condition, not an
	// upstream failure, so it gets the rate-limit shape (and its own code) instead of the
	// generic 500 an unrecognised error would produce.
	if errors.Is(err, runtime.ErrProviderBusy) {
		return domain.ErrProviderBusy(err.Error())
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"version":     s.deps.Version,
		"revision":    s.deps.Revision,
		"ui":          s.uiShape(),
		"ui_encoding": s.uiEncodingShape(),
	})
}

// handleVersion answers with the build identity and nothing else. It is deliberately
// unauthenticated and touches nothing but its own fields: it must answer while the
// database is still opening, because that is exactly when an operator asks it.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     s.deps.Version,
		"revision":    s.deps.Revision,
		"ui":          s.uiShape(),
		"ui_encoding": s.uiEncodingShape(),
	})
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
	_, _ = fmt.Fprintf(w, "# HELP aigw_request_log_write_failures_total Request-log writes that failed and had to be retried without content%c", lf)
	_, _ = fmt.Fprintf(w, "# TYPE aigw_request_log_write_failures_total counter%c", lf)
	_, _ = fmt.Fprintf(w, "aigw_request_log_write_failures_total %d%c", s.requestLogWriteFailures.Load(), lf)
	_, _ = fmt.Fprintf(w, "# HELP aigw_request_log_dropped_total Request logs lost entirely, even as a content-free skeleton%c", lf)
	_, _ = fmt.Fprintf(w, "# TYPE aigw_request_log_dropped_total counter%c", lf)
	_, _ = fmt.Fprintf(w, "aigw_request_log_dropped_total %d%c", s.requestLogDropped.Load(), lf)
	if rec := s.deps.LogRecorder; rec != nil {
		st := rec.Stats()
		_, _ = fmt.Fprintf(w, "# HELP aigw_audit_batched_requests_total Requests whose audit rows were written by the background batcher%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_audit_batched_requests_total counter%c", lf)
		_, _ = fmt.Fprintf(w, "aigw_audit_batched_requests_total %v%c", st["batched_requests"], lf)
		_, _ = fmt.Fprintf(w, "# HELP aigw_audit_batches_total Batched transactions committed%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_audit_batches_total counter%c", lf)
		_, _ = fmt.Fprintf(w, "aigw_audit_batches_total %v%c", st["batches"], lf)
		_, _ = fmt.Fprintf(w, "# HELP aigw_audit_queued_requests Requests waiting for the next flush%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_audit_queued_requests gauge%c", lf)
		_, _ = fmt.Fprintf(w, "aigw_audit_queued_requests %v%c", st["queued_requests"], lf)
	}
	if janitor := s.deps.LogJanitor; janitor != nil {
		_, _ = fmt.Fprintf(w, "# TYPE aigw_request_log_pruned_total counter%c", lf)
		_, _ = fmt.Fprintf(w, "aigw_request_log_pruned_total %d%c", janitor.PrunedTotal(), lf)
	}
	if rollups := s.deps.DimensionRollups; rollups != nil {
		if st, err := rollups.DimensionRollupStats(r.Context()); err == nil {
			for name, value := range map[string]int64{
				"backfill_cursor": st.BackfillCursor,
				"pending_hours":   st.PendingHours,
			} {
				_, _ = fmt.Fprintf(w, "# TYPE aigw_dimension_rollup_%s gauge\naigw_dimension_rollup_%s %d\n", name, name, value)
			}
		}
	}
	// Provider capacity (M44): one series per limited provider. The counters name the three
	// ways an attempt can be refused, so an alert can tell "queue is too small" (queue_full)
	// from "the upstream is too slow" (timeouts).
	if stats := s.capacityStats(); len(stats) > 0 {
		policy := s.deps.Capacity.CapacityPolicy()
		_, _ = fmt.Fprintf(w, "# HELP aigw_provider_capacity_limit Configured concurrent attempts per provider (0 means unlimited)%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_provider_capacity_limit gauge%c", lf)
		for id, stat := range stats {
			_, _ = fmt.Fprintf(w, "aigw_provider_capacity_limit{target=%q} %d%c", runtime.ProviderKey(id), stat.Limit, lf)
		}
		_, _ = fmt.Fprintf(w, "# HELP aigw_provider_capacity_inflight Attempts holding a provider concurrency slot%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_provider_capacity_inflight gauge%c", lf)
		for id, stat := range stats {
			_, _ = fmt.Fprintf(w, "aigw_provider_capacity_inflight{target=%q} %d%c", runtime.ProviderKey(id), stat.Inflight, lf)
		}
		_, _ = fmt.Fprintf(w, "# HELP aigw_provider_capacity_waiting Attempts queued for a provider concurrency slot%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_provider_capacity_waiting gauge%c", lf)
		for id, stat := range stats {
			_, _ = fmt.Fprintf(w, "aigw_provider_capacity_waiting{target=%q} %d%c", runtime.ProviderKey(id), stat.Waiting, lf)
		}
		for _, counter := range []struct {
			name  string
			help  string
			value func(runtime.CapacityStat) int64
		}{
			{"aigw_provider_capacity_admitted_total", "Attempts admitted by a provider concurrency gate", func(st runtime.CapacityStat) int64 { return st.Admitted }},
			{"aigw_provider_capacity_queue_full_total", "Attempts refused because a provider's concurrency queue was full", func(st runtime.CapacityStat) int64 { return st.QueueFull }},
			{"aigw_provider_capacity_timeouts_total", "Attempts that gave up waiting for a provider concurrency slot", func(st runtime.CapacityStat) int64 { return st.TimedOut }},
			{"aigw_provider_capacity_cancelled_total", "Attempts whose request ended while they queued for a provider slot", func(st runtime.CapacityStat) int64 { return st.Cancelled }},
			{"aigw_provider_capacity_wait_ms_total", "Total milliseconds attempts spent queued for a provider concurrency slot", func(st runtime.CapacityStat) int64 { return st.WaitTotalMS }},
		} {
			_, _ = fmt.Fprintf(w, "# HELP %s %s%c", counter.name, counter.help, lf)
			_, _ = fmt.Fprintf(w, "# TYPE %s counter%c", counter.name, lf)
			for id, stat := range stats {
				_, _ = fmt.Fprintf(w, "%s{target=%q} %d%c", counter.name, runtime.ProviderKey(id), counter.value(stat), lf)
			}
		}
		_, _ = fmt.Fprintf(w, "# HELP aigw_provider_capacity_queue_wait_seconds How long one attempt may wait for a provider concurrency slot (0 disables queueing)%c", lf)
		_, _ = fmt.Fprintf(w, "# TYPE aigw_provider_capacity_queue_wait_seconds gauge%c", lf)
		_, _ = fmt.Fprintf(w, "aigw_provider_capacity_queue_wait_seconds %d%c", policy.QueueWaitS, lf)
	}
	_ = balances
}

// writeJSON writes one JSON answer. The payload is encoded before the status line goes
// out: encoding/json reports a value it cannot encode (a json.RawMessage holding
// something that is not JSON, a NaN, a channel) as an error *after* the status has been
// committed, which used to hand the caller a 200 with no body at all — a shape no client
// can tell from an empty payload, and one the console parsed to null before
// dereferencing it. A body that cannot be produced is now a 500 that says so.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		slog.Default().Error("encoding an API response failed", "err", err, "status", status)
		writeAPIError(w, domain.ErrInternal("failed to encode the response"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// normalizedModel strips an optional @provider suffix for presentation purposes.
func normalizedModel(model string) string {
	if idx := strings.LastIndex(model, "@"); idx > 0 {
		return model[:idx]
	}
	return model
}

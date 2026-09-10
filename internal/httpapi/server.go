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
	Log        *slog.Logger
	Version    string
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
	return s.withRecovery(s.withRequestID(s.withLogging(s.mux)))
}

// SetReady flips the readiness probe (used during shutdown/drain).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/responses", s.handleCreateResponse)
	s.mux.HandleFunc("GET /v1/responses/{id}", s.handleGetResponse)
	s.mux.HandleFunc("DELETE /v1/responses/{id}", s.handleDeleteResponse)
	s.mux.HandleFunc("GET /v1/models", s.handleListModels)
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

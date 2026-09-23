// Package nodeserve is the worker node's side of the control protocol (M77): the one listener
// a node exposes to the LAN.
//
// Two things share that listener, and both are gated by the same token gate:
//
//   - the control surface (/node/v1/control/<op>), whose operations are supplied by the caller
//     (the node's own lifecycle layer owns the tenants, so it owns the handlers);
//   - the tenant data plane (/node/v1/tenant/...), which forwards one tenant's HTTP traffic to
//     that tenant's worker on loopback, presenting the worker the authority it was handshaken
//     with (Host: 127.0.0.1:<worker_port>). That single detail is what makes dsh's
//     authority-bound cookie keep working unchanged on a remote machine.
//
// Nothing here decides policy: the token comes from configuration, the handlers come from the
// lifecycle layer, and the tenant forwarder comes from the tenant plane. Keeping this package
// that thin is what lets it be tested without a single tenant.
package nodeserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
)

// Handler is one control operation. It receives the raw request body and returns the value to
// put in the response envelope, or a *nodeproto.Error to be reported to the control plane.
//
// The body is passed as bytes rather than a decoded struct so the operation owns its own
// request shape: this package must not grow a copy of every tenant record the lifecycle layer
// already knows how to decode.
type Handler func(ctx context.Context, body []byte) (any, error)

// TenantPlane forwards one tenant's requests to its worker on this machine. Implemented by the
// node runtime; nil means tenant traffic is refused (the P1 skeleton, and any test that only
// exercises the control surface).
type TenantPlane interface {
	ServeTenant(w http.ResponseWriter, r *http.Request, tenant string, browserSession string)
}

// Options is everything the node server needs.
type Options struct {
	// Name is this node's name. It is reported in health, and every request's tenant header is
	// checked against the node's own allocation table (not against this name).
	Name string
	// Version/Revision are the build identity, reported in health so the control plane can say
	// "this node runs an older build".
	Version  string
	Revision string
	// Token is the shared secret. An empty token is refused at construction: a node listener
	// without a token is full access to every tenant on the machine for anyone on the LAN.
	Token string
	// Ops are the control operations. A missing op answers not_implemented, which is how a
	// partially built node reports its own gaps instead of 404ing like a broken deployment.
	Ops map[string]Handler
	// Tenants is the data plane. Optional.
	Tenants TenantPlane
	// Logger receives refusals and failures. Nil means no logging.
	Logger *slog.Logger
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time
	// StartedAt is when this node agent started (health reports it).
	StartedAt time.Time
	// MaxControlBytes overrides the control body bound.
	MaxControlBytes int64
}

// Server is the node's HTTP surface.
type Server struct {
	opts    Options
	handler http.Handler
}

// New validates the options and builds the server.
func New(opts Options) (*Server, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("nodeserve: a node name is required")
	}
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("nodeserve: a node token is required (a listener without one is full access to every tenant on this machine)")
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now().UTC()
	}
	s := &Server{opts: opts}
	// The status operation is the protocol's one self-description, so the build identity is
	// filled in here rather than by the handler: the handler owns the tenant table, this
	// process owns the version, and /health and the status operation must describe one node.
	ops := make(map[string]Handler, len(opts.Ops))
	for op, handler := range opts.Ops {
		ops[op] = handler
	}
	if handler, ok := ops["status"]; ok && handler != nil {
		ops["status"] = s.withIdentity(handler)
	}
	s.opts.Ops = ops
	mux := http.NewServeMux()
	mux.HandleFunc(nodeproto.HealthPath, s.handleHealth)
	mux.HandleFunc(nodeproto.ControlPath, s.handleControl)
	mux.HandleFunc(nodeproto.TenantPath, s.handleTenant)
	mux.HandleFunc("/", s.handleUnknownPath)
	s.handler = s.gate(mux)
	return s, nil
}

// Handler is the authenticated HTTP surface.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) log() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// gate is the single place a request is authenticated. It runs before any routing decision and
// before any lookup, so an unauthenticated caller learns nothing — not even whether the path
// exists, and certainly not whether a tenant is hosted here.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := nodeproto.BearerToken(r.Header.Get("Authorization"))
		if !ok || !nodeproto.TokenMatches(s.opts.Token, presented) {
			s.log().Warn("node control request refused", "node", s.opts.Name, "path", r.URL.Path, "source", remoteHost(r))
			w.Header().Set("WWW-Authenticate", `Bearer realm="dshgw-node"`)
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "this node requires its control plane's token")
			return
		}
		if sent := strings.TrimSpace(r.Header.Get(nodeproto.HeaderProtocol)); sent != "" && sent != nodeproto.ProtocolHeaderValue {
			nodeproto.WriteError(w, http.StatusUpgradeRequired, nodeproto.CodeProtocolMismatch,
				"this node speaks protocol "+nodeproto.ProtocolHeaderValue+", the request announced "+sent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		nodeproto.WriteError(w, http.StatusMethodNotAllowed, nodeproto.CodeBadRequest, "health is a GET")
		return
	}
	nodeproto.WriteValue(w, http.StatusOK, s.health(r.Context()))
}

// health assembles the self-description. The tenant counts come from the control plane's own
// handler set: the node's allocation table lives in its registry, which this package does not
// own.
func (s *Server) health(ctx context.Context) nodeproto.Health {
	status, err := s.status(ctx)
	if err != nil {
		return s.identity()
	}
	health := status.Health
	s.overlayIdentity(&health)
	return health
}

// withIdentity wraps the status operation so every answer carries this process's build
// identity. A handler that returns a value of another shape is passed through untouched: the
// registration is the node runtime's, and second-guessing it here would turn a wiring mistake
// into a silently emptied response.
func (s *Server) withIdentity(handler Handler) Handler {
	return func(ctx context.Context, body []byte) (any, error) {
		value, err := handler(ctx, body)
		if err != nil {
			return nil, err
		}
		if status, ok := value.(nodeproto.Status); ok {
			s.overlayIdentity(&status.Health)
			return status, nil
		}
		return value, nil
	}
}

// status runs the (identity-wrapped) "status" operation when the node runtime registered one.
func (s *Server) status(ctx context.Context) (nodeproto.Status, error) {
	handler, ok := s.opts.Ops["status"]
	if !ok || handler == nil {
		return nodeproto.Status{}, errors.New("no status operation registered")
	}
	value, err := handler(ctx, nil)
	if err != nil {
		return nodeproto.Status{}, err
	}
	status, ok := value.(nodeproto.Status)
	if !ok {
		return nodeproto.Status{}, errors.New("status operation returned an unexpected value")
	}
	return status, nil
}

// identity is the build's half of the self-description.
func (s *Server) identity() nodeproto.Health {
	return nodeproto.Health{
		Name:      s.opts.Name,
		Version:   s.opts.Version,
		Revision:  s.opts.Revision,
		Protocol:  nodeproto.Version,
		StartedAt: s.opts.StartedAt,
	}
}

// overlayIdentity writes the build's half of the self-description over a health value that
// came from the node's own status operation (which reports the tenant counts).
func (s *Server) overlayIdentity(health *nodeproto.Health) {
	health.Name = s.opts.Name
	health.Version = s.opts.Version
	health.Revision = s.opts.Revision
	health.Protocol = nodeproto.Version
	health.StartedAt = s.opts.StartedAt
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		nodeproto.WriteError(w, http.StatusMethodNotAllowed, nodeproto.CodeBadRequest, "control operations are POST")
		return
	}
	op := strings.Trim(strings.TrimPrefix(r.URL.Path, nodeproto.ControlPath), "/")
	if op == "" || strings.Contains(op, "/") {
		nodeproto.WriteError(w, http.StatusNotFound, nodeproto.CodeBadRequest, "unknown control operation")
		return
	}
	handler, ok := s.opts.Ops[op]
	if !ok || handler == nil {
		nodeproto.WriteError(w, http.StatusNotImplemented, nodeproto.CodeNotImplemented, "operation "+op+" is not implemented by this node build")
		return
	}
	limit := s.opts.MaxControlBytes
	if limit <= 0 {
		limit = nodeproto.MaxControlBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		nodeproto.WriteError(w, http.StatusBadRequest, nodeproto.CodeBadRequest, "cannot read the request body")
		return
	}
	if int64(len(body)) > limit {
		nodeproto.WriteError(w, http.StatusRequestEntityTooLarge, nodeproto.CodeBadRequest, "control request body is too large")
		return
	}
	if len(body) > 0 {
		// Validate the body is an object before handing it on: a handler that starts by
		// unmarshalling would otherwise report a JSON syntax error as an internal failure.
		var probe map[string]any
		if err := json.Unmarshal(body, &probe); err != nil {
			nodeproto.WriteError(w, http.StatusBadRequest, nodeproto.CodeBadRequest, "control request body must be a JSON object")
			return
		}
	}
	value, err := handler(r.Context(), body)
	if err != nil {
		var proto *nodeproto.Error
		if errors.As(err, &proto) {
			nodeproto.WriteError(w, statusForCode(proto.Code), proto.Code, proto.Message)
			return
		}
		s.log().Error("node control operation failed", "node", s.opts.Name, "op", op, "err", err)
		nodeproto.WriteError(w, http.StatusInternalServerError, nodeproto.CodeInternal, err.Error())
		return
	}
	if value == nil {
		value = map[string]any{"ok": true}
	}
	nodeproto.WriteValue(w, http.StatusOK, value)
}

func (s *Server) handleTenant(w http.ResponseWriter, r *http.Request) {
	if s.opts.Tenants == nil {
		nodeproto.WriteError(w, http.StatusNotImplemented, nodeproto.CodeNotImplemented, "this node build does not forward tenant traffic yet")
		return
	}
	tenant := strings.TrimSpace(r.Header.Get(nodeproto.HeaderTenant))
	if tenant == "" {
		nodeproto.WriteError(w, http.StatusBadRequest, nodeproto.CodeBadRequest, "a forwarded request must name its tenant")
		return
	}
	s.opts.Tenants.ServeTenant(w, r, tenant, strings.TrimSpace(r.Header.Get(nodeproto.HeaderBrowserSession)))
}

// handleUnknownPath answers anything outside the protocol: a 404 in the protocol's own shape,
// so a control plane that guessed a path still reads a structured error.
func (s *Server) handleUnknownPath(w http.ResponseWriter, r *http.Request) {
	nodeproto.WriteError(w, http.StatusNotFound, nodeproto.CodeProtocolMismatch,
		"no node endpoint at "+r.URL.Path+" (this node speaks protocol version "+nodeproto.ProtocolHeaderValue+")")
}

// statusForCode maps a protocol code onto the HTTP status that carries it. Only the codes a
// caller branches on are distinguished.
func statusForCode(code string) int {
	switch code {
	case nodeproto.CodeBadRequest:
		return http.StatusBadRequest
	case nodeproto.CodeTenantUnknown:
		return http.StatusNotFound
	case nodeproto.CodeWorkerNotRunning:
		return http.StatusServiceUnavailable
	case nodeproto.CodeProtocolMismatch:
		return http.StatusUpgradeRequired
	case nodeproto.CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Serve runs the server on ln until ctx is done, then shuts it down.
func (s *Server) Serve(ctx context.Context, ln net.Listener, readHeaderTimeout time.Duration) error {
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = 10 * time.Second
	}
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    128 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err == nil {
			err = ctx.Err()
		}
		return err
	}
}

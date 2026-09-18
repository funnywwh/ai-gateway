// Package edge binds the public surface of a supervised dshgw.
//
// The systemd shape put nginx in front: one listener per tenant public port plus
// the portal, each rewriting the edge port header before forwarding to dshgw's
// single internal address. That edge is gone, so dshgw binds those ports itself:
//
//   - the portal port serves the login page and the post-login redirects;
//   - each tenant's public port serves that tenant only.
//
// Both wrap the gateway handler and set the edge port header on the way in, which
// is exactly what nginx used to do — the gateway keeps one routing rule instead of
// learning a second one. TLS is served directly when a certificate is configured;
// otherwise the listeners are plain HTTP, which is only appropriate on a trusted
// network.
package edge

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// Edge owns one listener per public surface. Reconcile is idempotent: it starts
// what is missing, closes what no longer belongs, and keeps untouched listeners
// running (a tenant being added must not interrupt the others).
type Edge struct {
	cfg     *config.Config
	gateway http.Handler
	log     *slog.Logger

	mu      sync.Mutex
	servers map[int]*http.Server
}

func New(cfg *config.Config, gateway http.Handler, log *slog.Logger) *Edge {
	if log == nil {
		log = slog.Default()
	}
	return &Edge{cfg: cfg, gateway: gateway, log: log, servers: map[int]*http.Server{}}
}

// Reconcile makes the bound ports match the registry plus the portal.
func (e *Edge) Reconcile(tenants []registry.Tenant) error {
	wanted := map[int]string{e.cfg.PortalPort: "portal"}
	for _, tenant := range tenants {
		wanted[tenant.PublicPort] = tenant.Name
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var failures []error
	for port := range e.servers {
		if _, keep := wanted[port]; !keep {
			server := e.servers[port]
			delete(e.servers, port)
			if err := server.Close(); err != nil {
				failures = append(failures, fmt.Errorf("closing port %d: %w", port, err))
			} else {
				e.log.Info("edge listener closed", "port", port)
			}
		}
	}
	// Sorted so a failure report is deterministic and the log reads in port order.
	ports := make([]int, 0, len(wanted))
	for port := range wanted {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	for _, port := range ports {
		if _, exists := e.servers[port]; exists {
			continue
		}
		server, err := e.listen(port, wanted[port])
		if err != nil {
			// One unreachable tenant port must not stop the others or the portal:
			// the caller reports the error and keeps serving.
			failures = append(failures, fmt.Errorf("port %d (%s): %w", port, wanted[port], err))
			continue
		}
		e.servers[port] = server
		e.log.Info("edge listener started", "port", port, "surface", wanted[port], "tls", e.tlsEnabled())
	}
	return errors.Join(failures...)
}

// Close stops every listener.
func (e *Edge) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var failures []error
	for port, server := range e.servers {
		if err := server.Close(); err != nil {
			failures = append(failures, fmt.Errorf("closing port %d: %w", port, err))
		}
		delete(e.servers, port)
	}
	return errors.Join(failures...)
}

// Ports reports the currently bound public ports.
func (e *Edge) Ports() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	ports := make([]int, 0, len(e.servers))
	for port := range e.servers {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func (e *Edge) tlsEnabled() bool {
	return e.cfg.TLS.Certificate != "" && e.cfg.TLS.CertificateKey != ""
}

func (e *Edge) listen(port int, surface string) (*http.Server, error) {
	address := net.JoinHostPort(e.cfg.Deploy.PublicListen, strconv.Itoa(port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gateway routes on this header; the edge supplies it for real clients
		// exactly as nginx did. A client-supplied value is overwritten, so a
		// tenant port can never be talked into serving another tenant.
		r.Header.Set(e.cfg.EdgePortHeader, strconv.Itoa(port))
		e.gateway.ServeHTTP(w, r)
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    e.cfg.MaxHeaderBytes,
	}
	if e.tlsEnabled() {
		certificate, err := tls.LoadX509KeyPair(e.cfg.TLS.Certificate, e.cfg.TLS.CertificateKey)
		if err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("loading the TLS certificate: %w", err)
		}
		server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.log.Error("edge listener failed", "port", port, "surface", surface, "err", err)
		}
	}()
	return server, nil
}

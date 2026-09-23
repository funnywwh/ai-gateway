// Package nodeplane is the worker node's tenant data plane (M77): the half that makes a tenant
// that runs on another machine reachable.
//
// It receives requests the control plane addressed to this node (`/node/v1/tenant/...` with the
// tenant in a header), restores the tenant's own path, and forwards to that tenant's worker on
// loopback — presenting exactly the authority the control plane handshook with
// (`Host: 127.0.0.1:<worker_port>`). That single detail is what keeps dsh's authority-bound cookie
// working across machines: the worker sees the same Host it saw during the handshake, so neither
// side has to share a key or change a verification rule.
//
// Paths a local deployment answers itself (browser-workspace long polls) are answered here when
// this machine owns the mount, because the FUSE mount lives where the worker lives.
package nodeplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// BrowserPlane is the optional browser-workspace half: the FUSE mount service this node runs.
// Nil means the path is forwarded to the worker (which will 404 it), which is the honest answer
// for a node that does not run browser mounts.
type BrowserPlane interface {
	ServeTenant(http.ResponseWriter, *http.Request, registry.Tenant, string)
}

// Workers reports whether a tenant's worker is running on this machine. *tenancy.Manager
// implements it; tests inject a stub so the plane can be exercised without starting a process.
type Workers interface {
	Status(ctx context.Context, t registry.Tenant) (tenancy.WorkerState, error)
}

// Plane forwards one tenant's traffic to its worker on this machine.
type Plane struct {
	Config   *config.Config
	Registry *registry.Registry
	Workers  Workers
	// Browser is set when this node runs browser-directory mounts (M65).
	Browser BrowserPlane
	// Transport is the loopback transport to the worker. Nil uses a plain one with proxy
	// discovery disabled: an environment-set proxy must never receive tenant traffic.
	Transport http.RoundTripper
	Logger    *slog.Logger
	Now       func() time.Time
}

func (p *Plane) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (p *Plane) transport() http.RoundTripper {
	if p.Transport != nil {
		return p.Transport
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 16
	transport.ResponseHeaderTimeout = 0 // model streams and long polls have no header deadline
	return transport
}

// ServeTenant is the nodeserve.TenantPlane entry point.
func (p *Plane) ServeTenant(w http.ResponseWriter, r *http.Request, tenant, browserSession string) {
	restorePath(r)
	t, ok := p.Registry.Get(tenant)
	if !ok {
		nodeproto.WriteTenantError(w, http.StatusNotFound, nodeproto.CodeTenantUnknown,
			"this node does not host tenant "+tenant)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/browser-workspace/") && p.Browser != nil {
		p.Browser.ServeTenant(w, r, t, browserSession)
		return
	}
	if status, err := p.Workers.Status(context.Background(), t); err != nil {
		p.log().Error("cannot read a worker's state", "tenant", tenant, "err", err)
		nodeproto.WriteTenantError(w, http.StatusInternalServerError, nodeproto.CodeInternal,
			"cannot read the worker's state")
		return
	} else if !status.Running {
		// The control plane turns this into its own 503; a worker that was restarted since the
		// handshake also lands here, which is why the control plane re-handshakes and retries once.
		nodeproto.WriteTenantError(w, http.StatusServiceUnavailable, nodeproto.CodeWorkerNotRunning,
			"tenant "+tenant+" has no running worker on this node")
		return
	}

	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.WorkerPort))
	target := &url.URL{Scheme: "http", Host: authority}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // streams and WebSocket frames must not be buffered
		Transport:     p.transport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = authority
			// Everything the control channel carried is ours, not the worker's. The worker cookie
			// stays: that is the credential this whole hop exists to present.
			for _, header := range []string{
				nodeproto.HeaderTenant, nodeproto.HeaderBrowserSession, nodeproto.HeaderProtocol,
				nodeproto.HeaderError, "Authorization", "X-Forwarded-For", "X-Forwarded-Host",
				"X-Forwarded-Proto", "Forwarded", "X-Real-IP",
			} {
				pr.Out.Header.Del(header)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// A worker must not be able to speak the node's error channel: X-Dshgw-Error is how
			// this node tells the control plane it refused a request, and a tenant plugin that
			// emitted it would have its own response replaced by a gateway error.
			resp.Header.Del(nodeproto.HeaderError)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log().Error("forwarding to the tenant's worker failed", "tenant", tenant, "err", err)
			nodeproto.WriteTenantError(w, http.StatusServiceUnavailable, nodeproto.CodeWorkerNotRunning,
				"the tenant's worker did not answer on this node")
		},
	}
	rp.ServeHTTP(w, r)
}

// restorePath turns the node's addressed path back into the tenant's own path, preserving its
// escaping: the control plane prefixes without re-encoding, so the RawPath has to be trimmed the
// same way or a path like /plugins/foo%2Fbar.js would be sent to the worker double-encoded.
func restorePath(r *http.Request) {
	if strings.HasPrefix(r.URL.Path, nodeproto.TenantPath) {
		trimmed := strings.TrimPrefix(r.URL.Path, nodeproto.TenantPath)
		if trimmed == "" {
			trimmed = "/"
		} else if !strings.HasPrefix(trimmed, "/") {
			trimmed = "/" + trimmed
		}
		r.URL.Path = trimmed
	}
	if r.URL.RawPath != "" {
		if strings.HasPrefix(r.URL.RawPath, nodeproto.TenantPath) {
			trimmed := strings.TrimPrefix(r.URL.RawPath, nodeproto.TenantPath)
			if trimmed == "" {
				trimmed = "/"
			} else if !strings.HasPrefix(trimmed, "/") {
				trimmed = "/" + trimmed
			}
			r.URL.RawPath = trimmed
		}
	}
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
}

// ErrNoBrowserMounts reports that this node does not run browser mounts, used by callers that
// need to distinguish "no mount" from "mount failed".
var ErrNoBrowserMounts = errors.New("this node does not run browser workspaces")

// Healthy is a small readiness helper for tests and the node's own operator surface.
func (p *Plane) Healthy(ctx context.Context, tenant string) error {
	_, ok := p.Registry.Get(tenant)
	if !ok {
		return nodeproto.Errorf(nodeproto.CodeTenantUnknown, "this node does not host tenant %s", tenant)
	}
	return nil
}

// now is unused today but keeps the clock injectable for the mount lease work in P4.
func (p *Plane) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

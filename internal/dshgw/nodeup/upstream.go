// Package nodeup is the control plane's implementation of the proxy's node seam (M77): it maps a
// tenant's placement onto a node client and runs the worker handshake there.
//
// It exists as its own package so the proxy can depend on a three-method interface instead of the
// node client, and so this mapping — "which node, which token, is this tenant remote at all" —
// has exactly one implementation to test.
package nodeup

import (
	"context"
	"log/slog"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

// Service resolves tenants to their nodes.
type Service struct {
	Config  *config.Config
	Clients *nodeclient.Set
	Logger  *slog.Logger
}

// New builds the seam. A nil client set is a single-machine deployment: every tenant is local and
// Service reports none of them as remote.
func New(cfg *config.Config, clients *nodeclient.Set, logger *slog.Logger) *Service {
	return &Service{Config: cfg, Clients: clients, Logger: logger}
}

// NodeFor reports where a tenant runs, or ok=false when it runs in this process.
func (s *Service) NodeFor(t registry.Tenant) (proxy.NodeRef, bool) {
	if s.Config.IsLocalNode(t.Node) {
		return proxy.NodeRef{}, false
	}
	client, ok := s.Clients.Get(t.Node)
	if !ok {
		return proxy.NodeRef{}, false
	}
	return proxy.NodeRef{Name: client.Name, BaseURL: client.BaseAddress(), Token: client.Secret()}, true
}

// Handshake asks the node to exchange the worker's startup token for the upstream cookie.
//
// The authority is checked against what the node returns, not assumed: dsh binds the cookie to
// the authority, so a node that handshook under another one would hand back a credential this
// control plane could never use — and the failure would look like "the tenant's UI is broken"
// rather than like a mismatched record.
func (s *Service) Handshake(ctx context.Context, t registry.Tenant, authority string) (*session.Upstream, error) {
	client, ok := s.Clients.Get(t.Node)
	if !ok {
		return nil, nodeproto.Errorf(nodeproto.CodeBadRequest,
			"tenant %s runs on node %s, which this deployment does not define", t.Name, t.Node)
	}
	result, err := client.TenantHandshake(ctx, t.Name, authority)
	if err != nil {
		return nil, err
	}
	if result.Authority != authority {
		return nil, nodeproto.Errorf(nodeproto.CodeProtocolMismatch,
			"node %s handshook against authority %q, not %q", client.Name, result.Authority, authority)
	}
	if result.Name == "" || result.Value == "" {
		return nil, nodeproto.Errorf(nodeproto.CodeInternal, "node %s returned an empty worker credential", client.Name)
	}
	return &session.Upstream{
		Name: result.Name, Value: result.Value, Authority: result.Authority, ExpiresAt: result.ExpiresAt,
	}, nil
}

// ServesBrowserWorkspaces reports whether this process answers /browser-workspace/ for a tenant.
// A remote tenant's mounts live on its node, so the control plane must forward that path instead
// of answering it here.
func (s *Service) ServesBrowserWorkspaces(t registry.Tenant) bool {
	return s.Config.IsLocalNode(t.Node)
}

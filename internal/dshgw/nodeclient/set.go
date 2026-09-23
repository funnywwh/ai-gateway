package nodeclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
)

// Spec is one node as the control plane's configuration and node store describe it. The token
// is resolved by the caller (from the record or its token file) so this package never has to
// know where secrets live.
type Spec struct {
	Name    string
	BaseURL string
	Token   string
}

// Set is the control plane's collection of node clients, looked up by node name.
//
// It is immutable once built: a deployment changes its node list by editing configuration or by
// deploying a node, and both paths rebuild the process's view rather than mutating a live one.
type Set struct {
	mu      sync.RWMutex
	clients map[string]*Client
	names   []string
}

// NewSet validates and builds the collection. A duplicate name or an unusable address is an
// error rather than a silent winner: both mean two records describe one machine, which is the
// mistake that later looks like "requests go to the wrong node".
func NewSet(specs []Spec) (*Set, error) {
	set := &Set{clients: make(map[string]*Client, len(specs))}
	for i, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			return nil, fmt.Errorf("node spec %d has no name", i)
		}
		if !strings.HasPrefix(spec.BaseURL, "http://") && !strings.HasPrefix(spec.BaseURL, "https://") {
			return nil, fmt.Errorf("node %s has an unusable address %q", name, spec.BaseURL)
		}
		if _, exists := set.clients[name]; exists {
			return nil, fmt.Errorf("node %s appears twice", name)
		}
		client := New(name, spec.BaseURL, spec.Token)
		set.clients[name] = client
		set.names = append(set.names, name)
	}
	sort.Strings(set.names)
	return set, nil
}

// Refresh replaces the addresses and secrets of the known nodes and drops the ones that are gone.
//
// A deploy (or a token rotation) rewrites the node store while the gateway is running; a long-lived
// process that kept its startup snapshot would keep presenting a secret the node no longer accepts,
// and every tenant on that node would fail until somebody restarted the gateway.
func (s *Set) Refresh(specs []Spec) {
	next := make(map[string]*Client, len(specs))
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		if existing, ok := s.clients[name]; ok {
			existing.mu.Lock()
			existing.BaseURL = strings.TrimRight(spec.BaseURL, "/")
			existing.Token = spec.Token
			existing.mu.Unlock()
			next[name] = existing
		} else {
			next[name] = New(name, spec.BaseURL, spec.Token)
		}
		names = append(names, name)
	}
	for name, client := range s.clients {
		if _, keep := next[name]; !keep {
			client.CloseIdle()
		}
	}
	sort.Strings(names)
	s.mu.Lock()
	s.clients, s.names = next, names
	s.mu.Unlock()
}

// Len is how many nodes this set knows.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// Names lists the node names in order.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.names...)
}

// Get looks one node up.
func (s *Set) Get(name string) (*Client, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, ok := s.clients[name]
	return client, ok
}

// CloseIdle releases every node's pooled connections (M77).
func (s *Set) CloseIdle() {
	if s == nil {
		return
	}
	s.Each(func(client *Client) { client.CloseIdle() })
}

// Each visits every node in name order. Reconciliation walks the whole set; a nil set is a
// single-machine deployment and visits nothing.
func (s *Set) Each(visit func(*Client)) {
	if s == nil {
		return
	}
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for _, name := range s.names {
		clients = append(clients, s.clients[name])
	}
	s.mu.RUnlock()
	for _, client := range clients {
		visit(client)
	}
}

// TenantStatus is one tenant's runtime state as a node reports it, with the node's name added:
// the callers (console page, CLI, reconciliation) always need to say which machine answered.
type TenantStatus struct {
	Node  string
	State nodeproto.TenantState
}

// Reconcile pushes the control plane's authoritative tenant list for this node and returns the
// node's state afterwards.
func (c *Client) Reconcile(ctx context.Context, request nodeproto.ReconcileRequest, result *nodeproto.ReconcileResult) error {
	return c.Call(ctx, "reconcile", request, result)
}

// TenantCreate provisions one tenant on the node and returns the node's own record for it
// (which carries the worker port and paths the node chose).
func (c *Client) TenantCreate(ctx context.Context, request nodeproto.TenantCreateRequest) (nodeproto.TenantState, error) {
	var state nodeproto.TenantState
	err := c.Call(ctx, "tenant-create", request, &state)
	return state, err
}

// AuditTail asks a node for its security events since cursor (empty = from now), or for its newest
// Last lines when last > 0.
func (c *Client) AuditTail(ctx context.Context, cursor string, last int) (nodeproto.AuditTailResult, error) {
	var result nodeproto.AuditTailResult
	request := nodeproto.AuditTailRequest{Cursor: cursor, Last: last}
	err := c.Call(ctx, "audit-tail", request, &result)
	return result, err
}

// TenantAdopt registers a tenant the operator has already copied onto this node, without
// creating or starting anything. The node verifies the data is there and answers with the record
// it registered (its own paths and worker port).
func (c *Client) TenantAdopt(ctx context.Context, spec nodeproto.TenantSpec) (nodeproto.TenantState, error) {
	var state nodeproto.TenantState
	err := c.Call(ctx, "tenant-adopt", spec, &state)
	return state, err
}

// TenantHandshake asks the node to exchange the worker's startup token for the upstream cookie,
// against the authority the control plane will present on every forwarded request.
func (c *Client) TenantHandshake(ctx context.Context, name, authority string) (nodeproto.TenantHandshakeResult, error) {
	var result nodeproto.TenantHandshakeResult
	request := nodeproto.TenantHandshakeRequest{Name: name, Authority: authority}
	err := c.Call(ctx, "handshake", request, &result)
	return result, err
}

// TenantStart starts a tenant's worker on the node.
func (c *Client) TenantStart(ctx context.Context, name string) error {
	return c.Call(ctx, "tenant-start", nodeproto.TenantRef{Name: name}, nil)
}

// TenantStop stops a tenant's worker on the node, recording the operator's intent.
func (c *Client) TenantStop(ctx context.Context, name string) error {
	return c.Call(ctx, "tenant-stop", nodeproto.TenantRef{Name: name}, nil)
}

// TenantRestart replaces a tenant's worker in place.
func (c *Client) TenantRestart(ctx context.Context, name string) error {
	return c.Call(ctx, "tenant-restart", nodeproto.TenantRef{Name: name}, nil)
}

// TenantEnsureRunning brings a worker up when it is not running.
func (c *Client) TenantEnsureRunning(ctx context.Context, name string) (bool, error) {
	var result nodeproto.TenantEnsureRunningResult
	err := c.Call(ctx, "tenant-ensure-running", nodeproto.TenantRef{Name: name}, &result)
	return result.Started, err
}

// TenantRemove removes a tenant from the node, returning the snapshot it wrote (or an empty
// string when nothing was snapshotted).
func (c *Client) TenantRemove(ctx context.Context, request nodeproto.TenantRemoveRequest) (string, error) {
	var result nodeproto.TenantRemoveResult
	err := c.Call(ctx, "tenant-remove", request, &result)
	return result.Snapshot, err
}

// TenantSetKey rotates a tenant's worker credential on the node.
func (c *Client) TenantSetKey(ctx context.Context, request nodeproto.TenantSetKeyRequest) error {
	return c.Call(ctx, "tenant-set-key", request, nil)
}

// TenantSyncModels replaces a tenant's model list on the node.
func (c *Client) TenantSyncModels(ctx context.Context, request nodeproto.TenantSyncModelsRequest) error {
	return c.Call(ctx, "tenant-sync-models", request, nil)
}

// TenantCaptureURL reads the worker's startup URL from the node.
func (c *Client) TenantCaptureURL(ctx context.Context, name string) (string, error) {
	var result nodeproto.TenantCaptureURLResult
	err := c.Call(ctx, "tenant-capture-url", nodeproto.TenantRef{Name: name}, &result)
	return result.URL, err
}

// TenantLogoutStop runs a sign-out teardown on the node: mounts first, worker last.
func (c *Client) TenantLogoutStop(ctx context.Context, name string) (nodeproto.TenantLogoutResult, error) {
	var result nodeproto.TenantLogoutResult
	err := c.Call(ctx, "tenant-logout-stop", nodeproto.TenantRef{Name: name}, &result)
	return result, err
}

// TenantState returns one tenant's runtime state from the node's full status answer.
func (c *Client) TenantState(ctx context.Context, name string) (nodeproto.TenantState, error) {
	status, err := c.Status(ctx)
	if err != nil {
		return nodeproto.TenantState{}, err
	}
	for _, tenant := range status.Tenants {
		if tenant.Name == name {
			return tenant, nil
		}
	}
	return nodeproto.TenantState{}, nodeproto.Errorf(nodeproto.CodeTenantUnknown,
		"node %s does not host tenant %s", c.Name, name)
}

// ErrNoNode reports that a node name has no client in this deployment.
func ErrNoNode(name string) error {
	return nodeproto.Errorf(nodeproto.CodeBadRequest, "node %q is not defined by this deployment", name)
}

// Package nodeclient is the control plane's client for the worker-node protocol (M77).
//
// It does one thing beyond moving JSON: it converts every transport-level failure into a
// *nodeproto.Error with a stable code. That matters because the callers are operator-facing —
// the console page, `dshgw node status`, the tenant request path — and "connection refused",
// "401", "404 on the protocol path" and "the node answered with an error" are four different
// situations with four different remedies.
package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
)

// DefaultHealthTimeout bounds one health probe; DefaultCallTimeout bounds one control
// operation, which may start a worker (a bwrap exec plus a readiness probe).
const (
	DefaultHealthTimeout = 5 * time.Second
	DefaultCallTimeout   = 5 * time.Minute
)

// Client talks to one node.
type Client struct {
	// Name is the node name this client was built for. It is what error messages name, and
	// what a health answer is checked against.
	Name string
	// BaseURL is the node's address, e.g. http://192.168.190.87:18400.
	BaseURL string
	// Token is the shared secret (from the node record or its token file).
	Token string

	HTTP          *http.Client
	HealthTimeout time.Duration
	CallTimeout   time.Duration

	// shared is the client built on first use. It is created once per Client so its transport
	// pools connections: the tenant data plane will send every one of a tenant's requests through
	// this client, and a fresh transport per call would mean a fresh TCP connection per request
	// (and a leaked one at that, since a transport nobody closes keeps its idle connection).
	sharedMu sync.Mutex
	shared   *http.Client

	// mu guards the identity fields, which a running gateway updates in place when a node is
	// redeployed or its token rotated (Set.Refresh).
	mu sync.RWMutex
}

// CloseIdle releases this client's pooled connections. A control plane that drops a node (removed
// from configuration, or rotated to another address) calls it so the old connections go away.
func (c *Client) CloseIdle() {
	c.sharedMu.Lock()
	client := c.shared
	c.sharedMu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

// New builds a client with the standard timeouts.
func New(name, baseURL, token string) *Client {
	return &Client{Name: name, BaseURL: strings.TrimRight(baseURL, "/"), Token: token}
}

// identity reads the address and secret under the lock.
func (c *Client) identity() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BaseURL, c.Token
}

func (c *Client) healthTimeout() time.Duration {
	if c.HealthTimeout > 0 {
		return c.HealthTimeout
	}
	return DefaultHealthTimeout
}

func (c *Client) callTimeout() time.Duration {
	if c.CallTimeout > 0 {
		return c.CallTimeout
	}
	return DefaultCallTimeout
}

// httpClient returns the client to use, with a transport that ignores the environment's proxy
// settings and does not attempt HTTP/2: a node agent speaks plain HTTP/1.1 on a LAN, and
// inheriting a corporate proxy from the environment would send tenant traffic somewhere else.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.sharedMu.Lock()
	defer c.sharedMu.Unlock()
	if c.shared == nil {
		c.shared = &http.Client{Transport: &http.Transport{
			Proxy:                 nil,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}}
	}
	return c.shared
}

// Health asks the node to describe itself.
func (c *Client) Health(ctx context.Context) (nodeproto.Health, error) {
	var health nodeproto.Health
	ctx, cancel := context.WithTimeout(ctx, c.healthTimeout())
	defer cancel()
	if err := c.call(ctx, http.MethodGet, nodeproto.HealthPath, nil, &health); err != nil {
		return health, err
	}
	return health, nil
}

// Probe asks for the node's health and checks the two facts a control plane must not assume:
// that the answer comes from the node we think it is, and that both sides speak one protocol
// version. Any mismatch is reported as a protocol error with its own code, so the console can
// say "deploy again" instead of "unreachable".
func (c *Client) Probe(ctx context.Context) (nodeproto.Health, error) {
	health, err := c.Health(ctx)
	if err != nil {
		return health, err
	}
	if health.Protocol != nodeproto.Version {
		return health, nodeproto.Errorf(nodeproto.CodeProtocolMismatch,
			"node %s speaks protocol %d, this control plane speaks %d: deploy the node again with matching binaries",
			c.Name, health.Protocol, nodeproto.Version)
	}
	if health.Name != "" && health.Name != c.Name {
		return health, nodeproto.Errorf(nodeproto.CodeBadRequest,
			"the address configured for node %s answers as node %s: two records point at one machine, or the address is wrong",
			c.Name, health.Name)
	}
	return health, nil
}

// Call runs one control operation and decodes its value into out (which may be nil).
func (c *Client) Call(ctx context.Context, op string, request, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout())
	defer cancel()
	var body []byte
	if request != nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			return nodeproto.Errorf(nodeproto.CodeInternal, "encode %s request: %v", op, err)
		}
		body = encoded
	}
	return c.call(ctx, http.MethodPost, nodeproto.ControlPath+op, body, out)
}

// Ping is the cheapest control operation; it exists so an operator can tell "the node agent is
// listening and the token is right" from "the node agent is running" without starting anything.
func (c *Client) Ping(ctx context.Context) error {
	return c.Call(ctx, "ping", nil, nil)
}

// Status asks for the node's self-description plus its per-tenant runtime state.
func (c *Client) Status(ctx context.Context) (nodeproto.Status, error) {
	var status nodeproto.Status
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout())
	defer cancel()
	if err := c.call(ctx, http.MethodPost, nodeproto.ControlPath+"status", nil, &status); err != nil {
		return status, err
	}
	return status, nil
}

// call performs one request/response round trip and maps failures onto protocol errors.
func (c *Client) call(ctx context.Context, method, path string, body []byte, out any) error {
	baseURL, token := c.identity()
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nodeproto.Errorf(nodeproto.CodeUnreachable, "node %s has an unusable address %q", c.Name, baseURL)
	}
	target := *base
	target.Path = path

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nodeproto.Errorf(nodeproto.CodeInternal, "build request for node %s: %v", c.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(nodeproto.HeaderProtocol, nodeproto.ProtocolHeaderValue)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return c.transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nodeproto.Errorf(nodeproto.CodeAuthFailed, "node %s refused this control plane's token (HTTP %d)", c.Name, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUpgradeRequired || resp.StatusCode == http.StatusHTTPVersionNotSupported {
		return nodeproto.Errorf(nodeproto.CodeProtocolMismatch, "node %s wants another protocol version (HTTP %d)", c.Name, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		// The version lives in the path, so a 404 on it means either another protocol major
		// version or something that is not a node agent at all. Both are reported the same
		// way: the operator's next action ("deploy again", "check the address") is the same.
		return nodeproto.Errorf(nodeproto.CodeProtocolMismatch,
			"node %s answered 404 for %s: it speaks another protocol version, or that address is not a dshgw node agent", c.Name, path)
	}

	env, decodeErr := nodeproto.DecodeEnvelope(resp.Body)
	if decodeErr != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nodeproto.Errorf(nodeproto.CodeInternal, "node %s: %v", c.Name, decodeErr)
		}
		return nodeproto.Errorf(nodeproto.CodeInternal, "node %s answered HTTP %d with an unreadable body", c.Name, resp.StatusCode)
	}
	if !env.OK {
		if env.Error != nil {
			return nodeproto.Errorf(env.Error.Code, "node %s: %s", c.Name, env.Error.Message)
		}
		return nodeproto.Errorf(nodeproto.CodeInternal, "node %s answered an empty error envelope (HTTP %d)", c.Name, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nodeproto.Errorf(nodeproto.CodeInternal, "node %s answered HTTP %d", c.Name, resp.StatusCode)
	}
	if out == nil || len(env.Value) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Value, out); err != nil {
		return nodeproto.Errorf(nodeproto.CodeInternal, "node %s answered an unreadable value: %v", c.Name, err)
	}
	return nil
}

// transportError classifies a dial/DNS/timeout failure. Everything that is not a deliberate
// context cancellation is "unreachable": from the caller's point of view there is no
// difference between a dead machine, a wrong address and a firewall.
func (c *Client) transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return nodeproto.Errorf(nodeproto.CodeUnreachable, "node %s request was canceled", c.Name)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nodeproto.Errorf(nodeproto.CodeUnreachable, "node %s did not answer in time: %v", c.Name, err)
	}
	return nodeproto.Errorf(nodeproto.CodeUnreachable, "node %s is unreachable: %v", c.Name, err)
}

// BaseAddress is the node's address as configured: safe to log and show (configuration
// validation refuses credentials, a query and a fragment, so there is nothing to redact).
func (c *Client) BaseAddress() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BaseURL
}

// Secret returns the node's shared secret as the client will present it. The proxy needs it for the
// tenant data plane's Authorization header; it is never logged.
func (c *Client) Secret() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Token
}

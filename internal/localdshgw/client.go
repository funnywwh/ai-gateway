// Package localdshgw is aigw's only client for the dshgw local provisioning channel
// (M52-rev2). It speaks newline-delimited JSON over a root-owned UNIX socket and never
// touches the network. Keeping this in its own package preserves the aigw ↔ dshgw
// decoupling: no aigw code imports dshgw internals, and vice versa.
package localdshgw

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Ops is what the console needs to enable/disable an account's dsh tenant. The key is
// handed over only on create/set-key, only over the local socket, and never logged.
// Account is the console's own account name (M67): dshgw records it on the tenant so the
// tenant's sidebar can name the signed-in person. It is display data, so an empty string is
// accepted by every operation.
type Ops interface {
	CreateTenant(ctx context.Context, name, account, key string) error
	// CreateTenantIn provisions a tenant on a named worker node (M77); an empty node means this
	// machine.
	CreateTenantIn(ctx context.Context, name, account, key, node string) error
	StartTenant(ctx context.Context, name string) error
	StopTenant(ctx context.Context, name string) error
	RestartTenant(ctx context.Context, name string) error
	SetTenantKey(ctx context.Context, name, account, key string) error
	ListTenants(ctx context.Context) ([]TenantInfo, error)
}

type TenantInfo struct {
	Name          string `json:"name"`
	Account       string `json:"account,omitempty"`
	PublicPort    int    `json:"public_port"`
	WorkerPort    int    `json:"worker_port"`
	UID           int    `json:"uid"`
	ModelsPending bool   `json:"models_pending"`
	// M77: where this tenant's worker runs and what it is doing.
	Node      string `json:"node,omitempty"`
	Running   bool   `json:"running"`
	Suspended bool   `json:"suspended"`
	Handshake string `json:"handshake,omitempty"`
	LastLogin string `json:"last_login,omitempty"`
	PortalURL string `json:"portal_url,omitempty"`
	TenantURL string `json:"tenant_url,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
}

type Client struct {
	SocketPath string
	// Timeout bounds one round trip. Tenant creation includes a worker start and a
	// worker probe, so the default is generous.
	Timeout time.Duration
}

type request struct {
	ID               int64  `json:"id"`
	Op               string `json:"op"`
	Name             string `json:"name,omitempty"`
	Account          string `json:"account,omitempty"`
	Key              string `json:"key,omitempty"`
	AllowEmptyModels bool   `json:"allow_empty_models"`

	// M77: the node surface.
	Node   string         `json:"node,omitempty"`
	Spec   *NodeSpec      `json:"spec,omitempty"`
	Deploy *DeployOptions `json:"deploy,omitempty"`
	Purge  bool           `json:"purge,omitempty"`
	Probe  bool           `json:"probe,omitempty"`
	Lines  int            `json:"lines,omitempty"`
}

type response struct {
	ID      int64          `json:"id"`
	OK      bool           `json:"ok"`
	Result  map[string]any `json:"result"`
	Error   string         `json:"error"`
	ErrType string         `json:"error_type"`
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 5 * time.Minute
}

func (c *Client) call(ctx context.Context, op, name, account, key string, allowEmpty bool, node string) (map[string]any, error) {
	return c.send(ctx, request{ID: 1, Op: op, Name: name, Account: account, Key: key, AllowEmptyModels: allowEmpty, Node: node})
}

// send is the one place a request is written and an answer read.
func (c *Client) send(ctx context.Context, req request) (map[string]any, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("dshgw admin channel unavailable at %s: %w", c.SocketPath, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(c.timeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(conn, 1<<20).ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("dshgw admin channel: %w", err)
	}
	var resp response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		if resp.ErrType == "bad_request" {
			return nil, errors.New("dshgw refused the request: " + resp.Error)
		}
		return nil, errors.New("dshgw admin operation failed: " + resp.Error)
	}
	return resp.Result, nil
}

func (c *Client) CreateTenant(ctx context.Context, name, account, key string) error {
	// An empty model list is refused: a tenant without a single granted model boots into
	// a dsh UI that cannot load its provider settings. The console surfaces the daemon's
	// error and the admin fixes the account's model grants first (default_grant: none
	// makes this the common case, not the exception).
	_, err := c.call(ctx, "tenant-create", name, account, key, false, "")
	return err
}
func (c *Client) StartTenant(ctx context.Context, name string) error {
	_, err := c.call(ctx, "tenant-start", name, "", "", false, "")
	return err
}
func (c *Client) StopTenant(ctx context.Context, name string) error {
	_, err := c.call(ctx, "tenant-stop", name, "", "", false, "")
	return err
}

// SetTenantKey rotates a tenant's worker credential and, when the tenant has no account
// label yet, records one (that is how a tenant provisioned before M67 picks its account up).
func (c *Client) SetTenantKey(ctx context.Context, name, account, key string) error {
	_, err := c.call(ctx, "tenant-set-key", name, account, key, false, "")
	return err
}
func (c *Client) ListTenants(ctx context.Context) ([]TenantInfo, error) {
	result, err := c.call(ctx, "tenant-list", "", "", "", false, "")
	if err != nil {
		return nil, err
	}
	rows, _ := result["tenants"].([]any)
	out := make([]TenantInfo, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		info := TenantInfo{}
		if v, ok := m["name"].(string); ok {
			info.Name = v
		}
		if v, ok := m["account"].(string); ok {
			info.Account = v
		}
		if v, ok := m["public_port"].(float64); ok {
			info.PublicPort = int(v)
		}
		if v, ok := m["worker_port"].(float64); ok {
			info.WorkerPort = int(v)
		}
		if v, ok := m["uid"].(float64); ok {
			info.UID = int(v)
		}
		if v, ok := m["models_pending"].(bool); ok {
			info.ModelsPending = v
		}
		if v, ok := m["node"].(string); ok {
			info.Node = v
		}
		if v, ok := m["key_prefix"].(string); ok {
			info.KeyPrefix = v
		}
		if v, ok := m["handshake"].(string); ok {
			info.Handshake = v
		}
		if v, ok := m["last_login"].(string); ok {
			info.LastLogin = v
		}
		if v, ok := m["portal_url"].(string); ok {
			info.PortalURL = v
		}
		if v, ok := m["tenant_url"].(string); ok {
			info.TenantURL = v
		}
		if v, ok := m["running"].(bool); ok {
			info.Running = v
		}
		if v, ok := m["suspended"].(bool); ok {
			info.Suspended = v
		}
		out = append(out, info)
	}
	return out, nil
}

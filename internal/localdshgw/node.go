package localdshgw

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The multi-machine node surface (M77). The shapes here mirror what dshgw's admin channel answers;
// they are declared again rather than imported because aigw must not import dshgw internals — the
// two halves of the product talk over this documented protocol and nothing else.
//
// Every operation is one round trip over the local socket. A deploy is not: it returns as soon as a
// job has started and the console watches it with NodeDeployStatus, because an ssh install takes
// tens of seconds and holding an HTTP request open for it would be a timeout waiting to happen.

// NodeSpec registers or edits a node. Empty fields keep what the record already has (an update) or
// take the deployment's defaults (a new node).
type NodeSpec struct {
	Name         string `json:"name"`
	Listen       string `json:"listen,omitempty"`
	URL          string `json:"url,omitempty"`
	SSHHost      string `json:"ssh_host,omitempty"`
	SSHPort      int    `json:"ssh_port,omitempty"`
	SSHUser      string `json:"ssh_user,omitempty"`
	SSHKeyFile   string `json:"ssh_key_file,omitempty"`
	DeployDir    string `json:"deploy_dir,omitempty"`
	NodeStateDir string `json:"node_state_dir,omitempty"`
	PluginDir    string `json:"plugin_dir,omitempty"`
	TemplateHome string `json:"template_home,omitempty"`
	TokenPath    string `json:"token_path,omitempty"`
	Token        string `json:"token,omitempty"`
	BwrapBin     string `json:"bwrap_bin,omitempty"`
	NodeBin      string `json:"node_bin,omitempty"`
	BinJS        string `json:"bin_js,omitempty"`
	CurrentLink  string `json:"current_link,omitempty"`
	WorkerPortLo int    `json:"worker_port_lo,omitempty"`
	WorkerPortHi int    `json:"worker_port_hi,omitempty"`
	Default      bool   `json:"default,omitempty"`
}

// DeployOptions are one deploy's switches.
type DeployOptions struct {
	AcceptHostKey         string `json:"accept_host_key,omitempty"`
	PrepareTemplateOnNode bool   `json:"prepare_template_on_node,omitempty"`
	WithPackages          bool   `json:"with_packages,omitempty"`
	RotateToken           bool   `json:"rotate_token,omitempty"`
}

// NodeFeatures is what a node reported it can serve.
type NodeFeatures struct {
	SSHWorkspaces     bool     `json:"ssh_workspaces,omitempty"`
	BrowserWorkspaces bool     `json:"browser_workspaces,omitempty"`
	SSHMounts         int      `json:"ssh_mounts,omitempty"`
	HostShares        int      `json:"host_shares,omitempty"`
	TenantPlugins     []string `json:"tenant_plugins,omitempty"`
	DirectoryPicker   string   `json:"directory_picker,omitempty"`
	PluginBrowserFS   string   `json:"plugin_browser_fs,omitempty"`
}

// NodeView is one node as the console shows it.
type NodeView struct {
	Name       string       `json:"name"`
	Source     string       `json:"source"`
	Listen     string       `json:"listen,omitempty"`
	URL        string       `json:"url,omitempty"`
	SSHHost    string       `json:"ssh_host,omitempty"`
	SSHUser    string       `json:"ssh_user,omitempty"`
	DeployDir  string       `json:"deploy_dir,omitempty"`
	Default    bool         `json:"default"`
	State      string       `json:"state"`
	Phase      string       `json:"phase,omitempty"`
	Error      string       `json:"error,omitempty"`
	Version    string       `json:"version,omitempty"`
	Revision   string       `json:"revision,omitempty"`
	Protocol   int          `json:"protocol,omitempty"`
	Features   NodeFeatures `json:"features,omitempty"`
	Tenants    int          `json:"tenants"`
	Running    int          `json:"running"`
	Suspended  int          `json:"suspended"`
	Reachable  bool         `json:"reachable"`
	ProbedAt   *time.Time   `json:"probed_at,omitempty"`
	ReadyAt    *time.Time   `json:"ready_at,omitempty"`
	LogPath    string       `json:"log_path,omitempty"`
	TokenState string       `json:"token_state,omitempty"`
	FailureLog string       `json:"failure_log,omitempty"`
}

// DeployPhase is one finished phase of a deploy.
type DeployPhase struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Millis int64  `json:"millis,omitempty"`
}

// DeployStatus is a deploy's progress.
type DeployStatus struct {
	Node        string        `json:"node"`
	Running     bool          `json:"running"`
	State       string        `json:"state"`
	Phase       string        `json:"phase,omitempty"`
	Error       string        `json:"error,omitempty"`
	StartedAt   *time.Time    `json:"started_at,omitempty"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Phases      []DeployPhase `json:"phases,omitempty"`
	LogTail     string        `json:"log_tail,omitempty"`
	LogPath     string        `json:"log_path,omitempty"`
	Version     string        `json:"version,omitempty"`
	Revision    string        `json:"revision,omitempty"`
	Systemd     bool          `json:"systemd"`
	Fingerprint string        `json:"fingerprint,omitempty"`
	Rotated     bool          `json:"rotated_token,omitempty"`
}

// ReconcileResult summarises one node's reconciliation.
type ReconcileResult struct {
	Started []string `json:"started,omitempty"`
	Stopped []string `json:"stopped,omitempty"`
	Pruned  []string `json:"pruned,omitempty"`
}

// NodeCall is a low-level admin request. It exists so a caller can drive an operation the typed
// wrappers do not cover yet without reaching into the protocol.
type NodeCall struct {
	Op      string         `json:"op"`
	Name    string         `json:"name,omitempty"`
	Node    string         `json:"node,omitempty"`
	Spec    *NodeSpec      `json:"spec,omitempty"`
	Deploy  *DeployOptions `json:"deploy,omitempty"`
	Purge   bool           `json:"purge,omitempty"`
	Probe   bool           `json:"probe,omitempty"`
	Lines   int            `json:"lines,omitempty"`
	Account string         `json:"account,omitempty"`
	Key     string         `json:"key,omitempty"`
}

// Call sends one raw operation and returns its result map.
func (c *Client) Call(ctx context.Context, call NodeCall) (map[string]any, error) {
	return c.send(ctx, request{
		ID: 1, Op: call.Op, Name: call.Name, Node: call.Node, Spec: call.Spec, Deploy: call.Deploy,
		Purge: call.Purge, Probe: call.Probe, Lines: call.Lines, Account: call.Account, Key: call.Key,
	})
}

// CreateTenantIn provisions a tenant, optionally on a named worker node.
func (c *Client) CreateTenantIn(ctx context.Context, name, account, key, node string) error {
	_, err := c.call(ctx, "tenant-create", name, account, key, false, node)
	return err
}

// RestartTenant restarts a tenant's worker without changing its stored credential.
func (c *Client) RestartTenant(ctx context.Context, name string) error {
	_, err := c.send(ctx, request{ID: 1, Op: "tenant-restart", Name: name})
	return err
}

// SetTenantNode records a tenant's placement, refusing a running tenant (the caller stops it first).
func (c *Client) SetTenantNode(ctx context.Context, tenant, node string) error {
	_, err := c.send(ctx, request{ID: 1, Op: "tenant-set-node", Name: tenant, Node: node})
	return err
}

// ListNodes returns every node, optionally probing each one (the slow path: it talks to the nodes).
func (c *Client) ListNodes(ctx context.Context, probe bool) ([]NodeView, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-list", Probe: probe})
	if err != nil {
		return nil, err
	}
	return decodeList[NodeView](result, "nodes")
}

// AddNode registers a node.
func (c *Client) AddNode(ctx context.Context, spec NodeSpec) (NodeView, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-add", Name: spec.Name, Spec: &spec})
	if err != nil {
		return NodeView{}, err
	}
	return decodeOne[NodeView](result, "node")
}

// UpdateNode edits a node's record.
func (c *Client) UpdateNode(ctx context.Context, spec NodeSpec) (NodeView, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-update", Name: spec.Name, Spec: &spec})
	if err != nil {
		return NodeView{}, err
	}
	return decodeOne[NodeView](result, "node")
}

// RemoveNode forgets a node, optionally purging the machine.
func (c *Client) RemoveNode(ctx context.Context, name string, purge bool) error {
	_, err := c.send(ctx, request{ID: 1, Op: "node-remove", Name: name, Purge: purge})
	return err
}

// DeployNode starts a deploy or upgrade and returns as soon as the job has begun.
func (c *Client) DeployNode(ctx context.Context, name string, opts DeployOptions) (DeployStatus, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-deploy", Name: name, Deploy: &opts})
	if err != nil {
		return DeployStatus{}, err
	}
	return decodeOne[DeployStatus](result, "deploy")
}

// RotateNodeToken deploys with a fresh shared secret.
func (c *Client) RotateNodeToken(ctx context.Context, name string, opts DeployOptions) (DeployStatus, error) {
	opts.RotateToken = true
	result, err := c.send(ctx, request{ID: 1, Op: "node-rotate-token", Name: name, Deploy: &opts})
	if err != nil {
		return DeployStatus{}, err
	}
	return decodeOne[DeployStatus](result, "deploy")
}

// NodeDeployStatus reports a node's deploy progress (running or last finished).
func (c *Client) NodeDeployStatus(ctx context.Context, name string) (DeployStatus, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-deploy-status", Name: name})
	if err != nil {
		return DeployStatus{}, err
	}
	return decodeOne[DeployStatus](result, "deploy")
}

// ProbeNode asks one node to describe itself.
func (c *Client) ProbeNode(ctx context.Context, name string) (NodeView, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-probe", Name: name})
	if err != nil {
		return NodeView{}, err
	}
	return decodeOne[NodeView](result, "node")
}

// ReconcileNode pushes the control plane's tenant list to one node.
func (c *Client) ReconcileNode(ctx context.Context, name string) (ReconcileResult, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-reconcile", Name: name})
	if err != nil {
		return ReconcileResult{}, err
	}
	return decodeOne[ReconcileResult](result, "result")
}

// NodeAudit reads a node's recent security events.
func (c *Client) NodeAudit(ctx context.Context, name string, lines int) ([]string, error) {
	result, err := c.send(ctx, request{ID: 1, Op: "node-audit", Name: name, Lines: lines})
	if err != nil {
		return nil, err
	}
	return decodeList[string](result, "lines")
}

// decodeOne re-decodes one result field into a typed value. The daemon answers with maps (the
// protocol is deliberately schemaless on the wire); re-marshalling is what keeps this side honest
// about the fields it actually reads.
func decodeOne[T any](result map[string]any, key string) (T, error) {
	var out T
	raw, ok := result[key]
	if !ok {
		return out, fmt.Errorf("dshgw admin channel: answer has no %q", key)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("dshgw admin channel: %q: %w", key, err)
	}
	return out, nil
}

func decodeList[T any](result map[string]any, key string) ([]T, error) {
	raw, ok := result[key]
	if !ok {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("dshgw admin channel: %q: %w", key, err)
	}
	return out, nil
}

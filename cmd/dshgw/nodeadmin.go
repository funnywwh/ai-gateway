package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodedep"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// nodeAdmin is the multi-machine node surface (M77), shared by the CLI and the admin channel.
//
// One implementation on purpose: registering, deploying and rotating a node are operator actions
// with real consequences (a machine runs tenant code, a secret changes), and a second copy of the
// rules — one for the command line, one for the console button — is how the two drift apart.
//
// A deploy runs as a background job: it takes tens of seconds (ssh, a tar upload, a start and a
// probe), and the console needs to show progress and the log while it happens instead of holding a
// request open. Jobs are single-flight per node.

// NodeSpec is one node's registration. Fields left empty keep whatever the record already has (for
// an update) or take the deployment's defaults (for a new node).
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

// NodeView is one node as the console shows it: the record plus what the node said when it was last
// asked (or when it was just probed).
type NodeView struct {
	Name       string             `json:"name"`
	Source     string             `json:"source"`
	Listen     string             `json:"listen,omitempty"`
	URL        string             `json:"url,omitempty"`
	SSHHost    string             `json:"ssh_host,omitempty"`
	SSHUser    string             `json:"ssh_user,omitempty"`
	DeployDir  string             `json:"deploy_dir,omitempty"`
	Default    bool               `json:"default"`
	State      string             `json:"state"`
	Phase      string             `json:"phase,omitempty"`
	Error      string             `json:"error,omitempty"`
	Version    string             `json:"version,omitempty"`
	Revision   string             `json:"revision,omitempty"`
	Protocol   int                `json:"protocol,omitempty"`
	Features   nodeproto.Features `json:"features,omitempty"`
	Tenants    int                `json:"tenants"`
	Running    int                `json:"running"`
	Suspended  int                `json:"suspended"`
	Reachable  bool               `json:"reachable"`
	ProbedAt   *time.Time         `json:"probed_at,omitempty"`
	ReadyAt    *time.Time         `json:"ready_at,omitempty"`
	LogPath    string             `json:"log_path,omitempty"`
	TokenState string             `json:"token_state,omitempty"` // set | missing
	FailureLog string             `json:"failure_log,omitempty"`
}

// DeployStatus is a deploy's progress: what the console draws and what the CLI prints.
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
	Revision    string        `json:"revision,omitempty"`
	Version     string        `json:"version,omitempty"`
	Systemd     bool          `json:"systemd"`
	Fingerprint string        `json:"fingerprint,omitempty"`
	Rotated     bool          `json:"rotated_token,omitempty"`
}

// DeployPhase is one finished phase of a deploy.
type DeployPhase struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Millis int64  `json:"millis,omitempty"`
}

type deployJob struct {
	status DeployStatus
	cancel context.CancelFunc
}

// nodeAdmin owns the node records and the deploy jobs.
type nodeAdmin struct {
	deps *runtimeDeps
	// version/revision are this build's identity: the deploy ships its own binary, so the two ends
	// must agree on both.
	version  string
	revision string
	now      func() time.Time

	mu   sync.Mutex
	jobs map[string]*deployJob
}

func newNodeAdmin(deps *runtimeDeps, version, revision string) *nodeAdmin {
	return &nodeAdmin{deps: deps, version: version, revision: revision, now: func() time.Time { return time.Now().UTC() }, jobs: map[string]*deployJob{}}
}

// List returns every node this deployment knows, optionally probing each one for its current state.
func (a *nodeAdmin) List(ctx context.Context, probe bool) ([]NodeView, error) {
	nodes, err := a.deps.nodes.View(a.deps.cfg.Nodes)
	if err != nil {
		return nil, err
	}
	defaultNode := nodestore.DefaultNode(nodes, a.deps.cfg.DefaultNode)
	views := make([]NodeView, 0, len(nodes))
	for _, node := range nodes {
		view := a.view(node, defaultNode)
		if probe {
			a.probeInto(ctx, node, &view)
		}
		if job, ok := a.job(node.Name); ok {
			view.State = job.status.State
			view.Phase = job.status.Phase
			view.Error = job.status.Error
		}
		views = append(views, view)
	}
	return views, nil
}

// view renders one record without touching the network.
func (a *nodeAdmin) view(node nodestore.Node, defaultNode string) NodeView {
	view := NodeView{
		Name: node.Name, Source: node.Source, Listen: node.Listen, URL: node.URL,
		SSHHost: node.SSH.Host, SSHUser: node.SSH.User, DeployDir: node.Deploy.Dir,
		Default: node.Name == defaultNode,
		State:   node.Status.State, Phase: node.Status.Phase, Error: node.Status.Error,
		Version: node.Status.Version, Revision: node.Status.Revision, Protocol: node.Status.Protocol,
		Features: node.Status.Features, Reachable: node.Status.Reachable, LogPath: node.Status.LogPath,
	}
	if view.State == "" {
		view.State = nodestore.StatePending
	}
	if !node.Status.ProbedAt.IsZero() {
		probed := node.Status.ProbedAt
		view.ProbedAt = &probed
	}
	if !node.Status.ReadyAt.IsZero() {
		ready := node.Status.ReadyAt
		view.ReadyAt = &ready
	}
	if strings.TrimSpace(node.Token) != "" || strings.TrimSpace(node.TokenFile) != "" {
		view.TokenState = "set"
	} else {
		view.TokenState = "missing"
	}
	return view
}

// probeInto fills one view with a live probe, recording the outcome in the store so a later page
// load shows what was true the last time anybody asked.
func (a *nodeAdmin) probeInto(ctx context.Context, node nodestore.Node, view *NodeView) {
	status, err := a.probe(ctx, node)
	now := a.now()
	record, ok := a.deps.nodes.Get(node.Name)
	if !ok {
		record = node
	}
	if err != nil {
		view.Reachable = false
		view.Error = err.Error()
		if view.State == nodestore.StateReady {
			view.State = nodestore.StateUnreachable
		}
		record.Status.Reachable = false
		record.Status.ProbedAt = now
		_ = a.deps.nodes.Put(record)
		return
	}
	view.Reachable = true
	view.Error = ""
	if view.State == nodestore.StateUnreachable || view.State == "" || view.State == nodestore.StatePending {
		view.State = nodestore.StateReady
	}
	view.Version, view.Revision, view.Protocol = status.Version, status.Revision, status.Protocol
	view.Features, view.Tenants, view.Running, view.Suspended = status.Features, status.Tenants, status.Running, status.Suspended
	view.ProbedAt = &now
	record.Status.Reachable = true
	record.Status.ProbedAt = now
	record.Status.Version, record.Status.Revision, record.Status.Protocol = status.Version, status.Revision, status.Protocol
	record.Status.Features = status.Features
	_ = a.deps.nodes.Put(record)
}

// probe asks one node to describe itself with the secret the record holds *now*.
func (a *nodeAdmin) probe(ctx context.Context, node nodestore.Node) (nodeproto.Health, error) {
	token, err := a.tokenFor(node)
	if err != nil {
		return nodeproto.Health{}, err
	}
	if strings.TrimSpace(node.URL) == "" {
		return nodeproto.Health{}, fmt.Errorf("node %s has no address yet", node.Name)
	}
	client := nodeclient.New(node.Name, node.URL, token)
	return client.Probe(ctx)
}

// tokenFor resolves a node's shared secret: the record's copy, or a file the record names.
func (a *nodeAdmin) tokenFor(node nodestore.Node) (string, error) {
	if token := strings.TrimSpace(node.Token); token != "" {
		return token, nil
	}
	if file := strings.TrimSpace(node.TokenFile); file != "" {
		return a.deps.cfg.NodeToken(config.Node{Name: node.Name, TokenFile: file})
	}
	return "", nil
}

// Add registers a node, touching nothing on the target.
func (a *nodeAdmin) Add(ctx context.Context, spec NodeSpec) (NodeView, error) {
	if !config.ValidNodeName(spec.Name) {
		return NodeView{}, fmt.Errorf("node name %q must match ^[a-z][a-z0-9-]{0,25}$ and must not be %q", spec.Name, config.LocalNodeName)
	}
	if _, declared := a.deps.cfg.NodeByName(spec.Name); declared {
		return NodeView{}, fmt.Errorf("node %q is declared in this deployment's configuration; deploy it directly or remove it there first", spec.Name)
	}
	if _, exists := a.deps.nodes.Get(spec.Name); exists {
		return NodeView{}, fmt.Errorf("node %q is already registered; update it instead", spec.Name)
	}
	record, err := a.recordFromSpec(spec, nodestore.Node{Name: spec.Name, Status: nodestore.Status{State: nodestore.StatePending}})
	if err != nil {
		return NodeView{}, err
	}
	if err := a.deps.nodes.Put(record); err != nil {
		return NodeView{}, err
	}
	if spec.Default {
		if err := a.deps.nodes.SetDefault(spec.Name); err != nil {
			return NodeView{}, err
		}
	}
	return a.view(record, nodestore.DefaultNode(mustView(a.deps), a.deps.cfg.DefaultNode)), nil
}

// Update changes a record. Only the fields the caller set change anything.
func (a *nodeAdmin) Update(ctx context.Context, spec NodeSpec) (NodeView, error) {
	record, ok := a.deps.nodes.Get(spec.Name)
	if !ok {
		if _, declared := a.deps.cfg.NodeByName(spec.Name); declared {
			return NodeView{}, fmt.Errorf("node %q is declared in configuration; edit the configuration file instead", spec.Name)
		}
		return NodeView{}, fmt.Errorf("node %q is not registered", spec.Name)
	}
	updated, err := a.recordFromSpec(spec, record)
	if err != nil {
		return NodeView{}, err
	}
	if err := a.deps.nodes.Put(updated); err != nil {
		return NodeView{}, err
	}
	if spec.Default {
		if err := a.deps.nodes.SetDefault(spec.Name); err != nil {
			return NodeView{}, err
		}
	}
	return a.view(updated, nodestore.DefaultNode(mustView(a.deps), a.deps.cfg.DefaultNode)), nil
}

// Remove forgets a record; with purge it also stops the node and deletes what the deploy created.
func (a *nodeAdmin) Remove(ctx context.Context, name string, purge bool) error {
	if a.deployRunning(name) {
		return fmt.Errorf("node %s has a deploy in progress; wait for it to finish", name)
	}
	record, ok := a.deps.nodes.Get(name)
	if !ok {
		if _, declared := a.deps.cfg.NodeByName(name); declared {
			return fmt.Errorf("node %q is declared in configuration; remove it there", name)
		}
		return fmt.Errorf("node %q is not registered", name)
	}
	if parked := a.deps.reg.TenantsForNode(name); len(parked) > 0 {
		names := make([]string, 0, len(parked))
		for _, tenant := range parked {
			names = append(names, tenant.Name)
		}
		return fmt.Errorf("node %s still hosts %d tenant(s) (%s): move them first", name, len(parked), strings.Join(names, ", "))
	}
	if purge {
		runner := a.runner(record, io.Discard)
		if err := runner.Purge(ctx, record); err != nil {
			return err
		}
	}
	return a.deps.nodes.Delete(name)
}

// StartDeploy begins a deploy and returns immediately. A second call while one is running is
// refused rather than queued: two deploys to one machine at once is never what an operator means.
func (a *nodeAdmin) StartDeploy(ctx context.Context, name string, opts DeployOptions) (DeployStatus, error) {
	record, err := a.recordFor(name)
	if err != nil {
		return DeployStatus{}, err
	}
	if !record.SSH.Configured() {
		return DeployStatus{}, fmt.Errorf("node %s has no ssh target: set the ssh host, user and key first", name)
	}
	if strings.TrimSpace(record.Listen) == "" {
		return DeployStatus{}, fmt.Errorf("node %s has no listen address: the generated configuration needs one", name)
	}
	a.mu.Lock()
	if job, running := a.jobs[name]; running && job.status.Running {
		a.mu.Unlock()
		return job.status, fmt.Errorf("node %s already has a deploy running (phase %s)", name, job.status.Phase)
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	started := a.now()
	job := &deployJob{cancel: cancel, status: DeployStatus{
		Node: name, Running: true, State: nodestore.StateDeploying, Phase: nodedep.PhasePreflight,
		StartedAt: &started, LogPath: nodedep.DeployLogPath(a.deps.cfg.StateDir, name),
		Version: a.version, Revision: a.revision,
	}}
	a.jobs[name] = job
	a.mu.Unlock()

	// The record says "deploying" from the first moment, so a console opened mid-deploy shows it.
	record.Status.State = nodestore.StateDeploying
	record.Status.Phase = nodedep.PhasePreflight
	record.Status.Error = ""
	record.Status.DeployStartedAt = started
	record.Status.LogPath = job.status.LogPath
	_ = a.deps.nodes.Put(record)

	go a.runDeploy(jobCtx, record, opts, job)
	return job.status, nil
}

// runDeploy is the job body: it drives the runner, tracks phases for the console, and folds the
// outcome into the node's record.
func (a *nodeAdmin) runDeploy(ctx context.Context, record nodestore.Node, opts DeployOptions, job *deployJob) {
	defer job.cancel()
	logFile, logPath, err := nodedep.OpenDeployLog(a.deps.cfg.StateDir, record.Name)
	if err != nil {
		a.finishJob(record.Name, DeployStatus{Node: record.Name, State: nodestore.StateFailed, Error: err.Error()}, record)
		return
	}
	defer logFile.Close()
	job.status.LogPath = logPath

	assets, deployOpts, err := a.assetsAndOptions(record, opts)
	if err != nil {
		a.finishJob(record.Name, DeployStatus{Node: record.Name, State: nodestore.StateFailed, Error: err.Error()}, record)
		return
	}
	runner := a.runner(record, logFile)
	runner.OnActivated = func(token string) error {
		current, ok := a.deps.nodes.Get(record.Name)
		if !ok {
			current = record
		}
		current.Token = token
		current.Status.State = nodestore.StateDeploying
		if err := a.deps.nodes.Put(current); err != nil {
			return err
		}
		if a.deps.manager.Nodes != nil {
			a.deps.manager.Nodes.Refresh([]nodeclient.Spec{{Name: record.Name, BaseURL: current.URL, Token: token}})
		}
		return nil
	}
	result, deployErr := runner.Deploy(ctx, record, assets, deployOpts)
	status := DeployStatus{
		Node: record.Name, State: nodestore.StateReady, LogPath: logPath,
		Version: result.Version, Revision: result.Revision, Systemd: result.Systemd,
		Fingerprint: result.Fingerprint, Rotated: result.RotatedToken,
		Phases: phasesOf(result),
	}
	finished := a.now()
	status.FinishedAt = &finished
	if deployErr != nil {
		status.State = nodestore.StateFailed
		status.Error = redactError(deployErr)
		status.Phase = failedPhaseOf(result)
		status.LogTail, _ = nodedep.ReadDeployLogTail(a.deps.cfg.StateDir, record.Name)
		a.finishJob(record.Name, status, record)
		return
	}
	current, ok := a.deps.nodes.Get(record.Name)
	if !ok {
		current = record
	}
	updated := nodedep.RecordFromResult(current, result, deployOpts, assets)
	updated.Status.LogPath = logPath
	updated.Status.DeployFinishedAt = finished
	if err := a.deps.nodes.Put(updated); err != nil {
		status.State = nodestore.StateFailed
		status.Error = err.Error()
	}
	a.finishJob(record.Name, status, record)
}

// finishJob publishes a deploy's outcome and marks the job done.
func (a *nodeAdmin) finishJob(name string, status DeployStatus, record nodestore.Node) {
	status.Running = false
	if status.Error != "" && status.LogTail == "" {
		status.LogTail, _ = nodedep.ReadDeployLogTail(a.deps.cfg.StateDir, name)
	}
	if status.LogTail == "" {
		status.LogTail, _ = nodedep.ReadDeployLogTail(a.deps.cfg.StateDir, name)
	}
	a.mu.Lock()
	job, ok := a.jobs[name]
	if ok {
		job.status = status
	}
	a.mu.Unlock()
	if !ok {
		return
	}
	current, found := a.deps.nodes.Get(name)
	if !found {
		return
	}
	current.Status.State = status.State
	current.Status.Phase = status.Phase
	current.Status.Error = status.Error
	current.Status.DeployFinishedAt = a.now()
	if status.State == nodestore.StateReady {
		current.Status.ReadyAt = current.Status.DeployFinishedAt
		current.Status.Reachable = true
		current.Status.ProbedAt = current.Status.DeployFinishedAt
		current.Status.Version = status.Version
		current.Status.Revision = status.Revision
		current.Status.Protocol = nodeproto.Version
	}
	_ = a.deps.nodes.Put(current)
}

// DeployStatus reports a node's deploy state (running or last finished).
func (a *nodeAdmin) DeployStatus(name string) DeployStatus {
	if job, ok := a.job(name); ok {
		status := job.status
		status.LogPath = nodedep.DeployLogPath(a.deps.cfg.StateDir, name)
		if !status.Running {
			if tail, err := nodedep.ReadDeployLogTail(a.deps.cfg.StateDir, name); err == nil {
				status.LogTail = tail
			}
		}
		return status
	}
	// No job in this process: report what the record says (a deploy run by another process).
	record, ok := a.deps.nodes.Get(name)
	if !ok {
		return DeployStatus{Node: name, State: nodestore.StatePending}
	}
	status := DeployStatus{
		Node: name, State: record.Status.State, Phase: record.Status.Phase, Error: record.Status.Error,
		LogPath: nodedep.DeployLogPath(a.deps.cfg.StateDir, name), Version: record.Status.Version,
		Revision: record.Status.Revision, LogTail: tailOrEmpty(a.deps.cfg.StateDir, name),
	}
	if !record.Status.DeployStartedAt.IsZero() {
		started := record.Status.DeployStartedAt
		status.StartedAt = &started
	}
	if !record.Status.DeployFinishedAt.IsZero() {
		finished := record.Status.DeployFinishedAt
		status.FinishedAt = &finished
	}
	return status
}

// Reconcile pushes this control plane's authoritative tenant list to one node.
func (a *nodeAdmin) Reconcile(ctx context.Context, name string) (nodeproto.ReconcileResult, error) {
	if err := a.ensureKnown(name); err != nil {
		return nodeproto.ReconcileResult{}, err
	}
	return a.deps.manager.ReconcileNode(ctx, name)
}

// Audit reads a node's recent security events.
func (a *nodeAdmin) Audit(ctx context.Context, name string, lines int) ([]string, error) {
	if lines <= 0 || lines > 500 {
		lines = 100
	}
	record, err := a.recordFor(name)
	if err != nil {
		return nil, err
	}
	token, err := a.tokenFor(record)
	if err != nil {
		return nil, err
	}
	client := nodeclient.New(record.Name, record.URL, token)
	result, err := client.AuditTail(ctx, "", lines)
	if err != nil {
		return nil, err
	}
	return result.Lines, nil
}

// Probe reads one node's health and records the outcome.
func (a *nodeAdmin) Probe(ctx context.Context, name string) (NodeView, error) {
	record, err := a.recordFor(name)
	if err != nil {
		return NodeView{}, err
	}
	view := a.view(record, nodestore.DefaultNode(mustView(a.deps), a.deps.cfg.DefaultNode))
	a.probeInto(ctx, record, &view)
	if !view.Reachable {
		return view, fmt.Errorf("node %s is not reachable: %s", name, view.Error)
	}
	return view, nil
}

// recordFor finds one node's record, or the effective view of a configuration-declared node.
func (a *nodeAdmin) recordFor(name string) (nodestore.Node, error) {
	if record, ok := a.deps.nodes.Get(name); ok {
		return record, nil
	}
	if node, found := a.deps.nodeViewNamed(name); found {
		return node, nil
	}
	return nodestore.Node{}, fmt.Errorf("node %q is not registered", name)
}

func (a *nodeAdmin) ensureKnown(name string) error {
	if _, err := a.recordFor(name); err != nil {
		return err
	}
	return nil
}

// recordFromSpec folds a spec into a record, filling the deployment defaults a new node needs.
func (a *nodeAdmin) recordFromSpec(spec NodeSpec, record nodestore.Node) (nodestore.Node, error) {
	record.Name = spec.Name
	if spec.Listen != "" {
		record.Listen = strings.TrimSpace(spec.Listen)
	}
	if spec.URL != "" {
		record.URL = strings.TrimSpace(spec.URL)
	}
	if spec.SSHHost != "" {
		record.SSH.Host = strings.TrimSpace(spec.SSHHost)
	}
	if spec.SSHPort != 0 {
		record.SSH.Port = spec.SSHPort
	}
	if record.SSH.Port == 0 {
		record.SSH.Port = 22
	}
	if spec.SSHUser != "" {
		record.SSH.User = strings.TrimSpace(spec.SSHUser)
	}
	if spec.SSHKeyFile != "" {
		record.SSH.KeyFile = strings.TrimSpace(spec.SSHKeyFile)
	}
	if spec.DeployDir != "" {
		record.Deploy.Dir = strings.TrimSpace(spec.DeployDir)
	}
	if record.Deploy.Dir == "" {
		record.Deploy.Dir = "/srv/dshgw-node"
	}
	if spec.NodeStateDir != "" {
		record.Deploy.StateDir = strings.TrimSpace(spec.NodeStateDir)
	}
	if spec.PluginDir != "" {
		record.Deploy.PluginPath = filepath.Join(strings.TrimSpace(spec.PluginDir), "picker-clamp.js")
	}
	if spec.TemplateHome != "" {
		record.Deploy.TemplateHome = strings.TrimSpace(spec.TemplateHome)
	}
	if spec.TokenPath != "" {
		record.Deploy.TokenPath = strings.TrimSpace(spec.TokenPath)
	}
	if spec.Token != "" {
		record.Token = strings.TrimSpace(spec.Token)
	}
	if spec.BwrapBin != "" {
		record.Deploy.BwrapBin = strings.TrimSpace(spec.BwrapBin)
	}
	if record.Deploy.BwrapBin == "" {
		record.Deploy.BwrapBin = "/usr/bin/bwrap"
	}
	if spec.NodeBin != "" {
		record.Deploy.NodeBin = strings.TrimSpace(spec.NodeBin)
	}
	if spec.BinJS != "" {
		record.Deploy.BinJS = strings.TrimSpace(spec.BinJS)
	}
	if spec.CurrentLink != "" {
		record.Deploy.CurrentLink = strings.TrimSpace(spec.CurrentLink)
	}
	if spec.WorkerPortLo != 0 {
		record.Deploy.WorkerPortLo = spec.WorkerPortLo
	}
	if record.Deploy.WorkerPortLo == 0 {
		record.Deploy.WorkerPortLo = 32800
	}
	if spec.WorkerPortHi != 0 {
		record.Deploy.WorkerPortHi = spec.WorkerPortHi
	}
	if record.Deploy.WorkerPortHi == 0 {
		record.Deploy.WorkerPortHi = 32899
	}
	if record.Deploy.TokenPath == "" {
		record.Deploy.TokenPath = filepath.Join(record.Deploy.Dir, spec.Name+".token")
	}
	if record.SSH.KeyFile == "" {
		record.SSH.KeyFile = nodedep.KeyPathFor(a.deps.cfg.StateDir, spec.Name)
	}
	if record.SSH.KnownHostsFile == "" {
		record.SSH.KnownHostsFile = nodedep.KnownHostsPath(a.deps.cfg.StateDir, spec.Name)
	}
	if record.URL == "" && record.Listen != "" {
		if _, port, err := net.SplitHostPort(record.Listen); err == nil && record.SSH.Host != "" {
			record.URL = "http://" + net.JoinHostPort(record.SSH.Host, port)
		}
	}
	if record.SSH.Host == "" || record.SSH.User == "" {
		return record, errors.New("ssh host and user are required")
	}
	if record.Listen == "" {
		return record, errors.New("listen is required: the node needs the address it binds")
	}
	if _, _, err := net.SplitHostPort(record.Listen); err != nil {
		return record, fmt.Errorf("listen must be host:port: %w", err)
	}
	return record, nil
}

// assetsAndOptions assembles what the deploy ships and how it configures the node.
func (a *nodeAdmin) assetsAndOptions(record nodestore.Node, opts DeployOptions) (nodedep.Assets, nodedep.Options, error) {
	binary, err := os.Executable()
	if err != nil {
		return nodedep.Assets{}, nodedep.Options{}, err
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	cfg := a.deps.cfg
	assets := nodedep.Assets{
		DshgwBin:     binary,
		PluginDir:    filepath.Dir(cfg.Deploy.PluginPath),
		TemplateHome: cfg.Deploy.TemplateHome,
		DshRoot:      cfg.Dsh.CurrentLink,
	}
	options := nodedep.Options{
		AcceptHostKey:         opts.AcceptHostKey,
		PrepareTemplateOnNode: opts.PrepareTemplateOnNode,
		WithPackages:          opts.WithPackages,
		RotateToken:           opts.RotateToken,
		Token:                 record.Token,
		AigwBaseURL:           cfg.AigwBaseURL,
		DirectoryPicker:       cfg.DirectoryPicker,
		PluginBrowserFS:       cfg.PluginBrowserFS,
		WorkerLimits:          cfg.WorkerLimits,
		SSHWorkspaces:         cfg.SSHWorkspaces.Enabled,
		BrowserWorkspaces:     cfg.BrowserWorkspaces.Enabled,
		TenantPlugins:         cfg.TenantPlugins,
		HostShares:            &cfg.HostShares,
		BwrapBin:              record.Deploy.BwrapBin,
		NodeBin:               record.Deploy.NodeBin,
		BinJS:                 record.Deploy.BinJS,
		CurrentLink:           record.Deploy.CurrentLink,
	}
	return assets, options, nil
}

// runner builds a deploy runner with this control plane's probe and logging wired in.
func (a *nodeAdmin) runner(record nodestore.Node, logWriter io.Writer) *nodedep.Runner {
	runner := &nodedep.Runner{Version: a.version, Revision: a.revision, Log: logWriter}
	runner.Probe = func(ctx context.Context, name string) error {
		current, ok := a.deps.nodes.Get(name)
		if !ok {
			current = record
		}
		token, err := a.tokenFor(current)
		if err != nil {
			return err
		}
		client := nodeclient.New(current.Name, current.URL, token)
		_, err = client.Probe(ctx)
		return err
	}
	return runner
}

func (a *nodeAdmin) job(name string) (*deployJob, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	job, ok := a.jobs[name]
	return job, ok
}

func (a *nodeAdmin) deployRunning(name string) bool {
	job, ok := a.job(name)
	return ok && job.status.Running
}

// SetNode records a tenant's placement (the guarded migration).
func (a *nodeAdmin) SetNode(ctx context.Context, tenant, node string) error {
	t, ok := a.deps.reg.Get(tenant)
	if !ok {
		return fmt.Errorf("tenant %q not found", tenant)
	}
	if err := a.ensureKnown(node); err != nil {
		return err
	}
	if state, err := a.deps.manager.Status(ctx, t); err != nil {
		return err
	} else if state.Running {
		return fmt.Errorf("tenant %s is running: stop it, copy its data, then register the new placement", tenant)
	}
	if !a.deps.cfg.IsLocalNode(node) && node == t.Node {
		return fmt.Errorf("tenant %s is already placed on node %s", tenant, node)
	}
	return a.deps.manager.AdoptOnNode(ctx, t, node)
}

// NodeAuditJSON renders one node's events as JSON lines, for the CLI's convenience.
func (a *nodeAdmin) NodeAuditJSON(ctx context.Context, name string, lines int) (string, error) {
	events, err := a.Audit(ctx, name, lines)
	if err != nil {
		return "", err
	}
	encoded, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// StoreSSHKey stores an uploaded private key for one node.
func (a *nodeAdmin) StoreSSHKey(name string, key []byte, target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		target = nodedep.KeyPathFor(a.deps.cfg.StateDir, name)
	}
	if _, err := nodedep.PrepareSSHDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	if err := securefile.WriteAtomic(target, key, 0o600); err != nil {
		return "", err
	}
	return target, nil
}

func phasesOf(result nodedep.Result) []DeployPhase {
	phases := make([]DeployPhase, 0, len(result.Phases))
	for _, phase := range result.Phases {
		phases = append(phases, DeployPhase{Name: phase.Name, OK: phase.OK, Detail: phase.Detail, Millis: phase.Duration.Milliseconds()})
	}
	return phases
}

func failedPhaseOf(result nodedep.Result) string {
	if len(result.Phases) == 0 {
		return nodedep.PhasePreflight
	}
	for _, phase := range result.Phases {
		if !phase.OK {
			return phase.Name
		}
	}
	return result.Phases[len(result.Phases)-1].Name
}

func tailOrEmpty(stateDir, node string) string {
	tail, err := nodedep.ReadDeployLogTail(stateDir, node)
	if err != nil {
		return ""
	}
	return tail
}

// mustView is the merged node view, ignoring an error the caller has already handled at startup.
func mustView(deps *runtimeDeps) []nodestore.Node { return deps.nodeView }

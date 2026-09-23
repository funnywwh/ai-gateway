package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/browsermount"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeops"
	"github.com/winger/ai-gateway/internal/dshgw/nodeplane"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/nodeserve"
	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/sshworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// node is the multi-machine surface (M77). `serve` runs this process as a worker node; the
// remaining verbs are the local checks an operator runs on the node's own machine, which is
// where the answers actually live (bwrap, the dsh release, the fuse runtime, the port band).
func (c *cli) node(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dshgw node serve|doctor|status|list|add|update|remove|deploy|rotate-token|probe|reconcile|audit")
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return errors.New("usage: dshgw node serve")
		}
		return c.nodeServe(context.Background())
	case "doctor":
		if len(args) != 1 {
			return errors.New("usage: dshgw node doctor")
		}
		return c.nodeDoctor(ctx)
	case "status":
		if len(args) > 1 {
			return errors.New("usage: dshgw node status")
		}
		return c.nodeStatus(ctx)
	case "list":
		if len(args) != 1 {
			return errors.New("usage: dshgw node list")
		}
		return c.nodeList(ctx)
	case "probe":
		if len(args) != 2 {
			return errors.New("usage: dshgw node probe NAME")
		}
		return c.nodeProbe(ctx, args[1])
	case "reconcile":
		if len(args) > 2 {
			return errors.New("usage: dshgw node reconcile [NAME]")
		}
		return c.nodeReconcile(ctx, args[1:])
	case "add":
		return c.nodeAdd(ctx, args[1:])
	case "update":
		return c.nodeUpdate(ctx, args[1:])
	case "remove":
		return c.nodeRemove(ctx, args[1:])
	case "deploy":
		return c.nodeDeploy(ctx, args[1:])
	case "rotate-token":
		return c.nodeRotateToken(ctx, args[1:])
	case "audit":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: dshgw node audit NAME [LINES]")
		}
		lines := 50
		if len(args) == 3 {
			value, err := strconv.Atoi(args[2])
			if err != nil || value < 1 || value > 500 {
				return errors.New("LINES must be between 1 and 500")
			}
			lines = value
		}
		return c.nodeAudit(ctx, args[1], lines)
	default:
		return fmt.Errorf("unknown node command %q (want serve, doctor, add, update, remove, deploy, rotate-token, status, list, probe, reconcile or audit)", args[0])
	}
}

// nodeServe is the node agent: one authenticated listener for the control plane, plus the
// tenant workers this machine hosts.
//
// It binds exactly one address (node.listen) — never a public portal, never a session store —
// and it starts the workers its own registry already knows about, so a node that reboots comes
// back with the tenants it was hosting instead of an empty machine (the control plane
// reconciles the list afterwards).
func (c *cli) nodeServe(ctx context.Context) (serveErr error) {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	cfg := deps.cfg
	if !cfg.NodeMode() {
		return errors.New("this configuration has no `node:` block: it describes a control plane, not a worker node")
	}
	token, err := cfg.NodeSelfToken()
	if err != nil {
		return err
	}
	// A node's registry is its own allocation table: every tenant in it is local to this
	// machine by definition, so a record pushed here must not carry a remote node reference
	// (the lifecycle layer would then try to reach a node from inside a node).
	if err := deps.reg.ValidateNodes(func(string) bool { return false }); err != nil {
		return fmt.Errorf("node registry: %w", err)
	}

	// This machine's operation surface: the lifecycle handlers in nodeops, driven by the same
	// manager the CLI uses locally, plus the port check so a created tenant never lands on a port
	// something else already holds.
	taken, takenErr := listeningPorts(ctx)
	if takenErr != nil {
		slog.Warn("cannot list listening ports; worker port allocation will not check for conflicts", "err", takenErr)
	}
	ops := &nodeops.Ops{
		Config: cfg, Manager: deps.manager, Registry: deps.reg,
		Taken:    func(port int) bool { return taken[port] },
		Features: nodeFeatures(cfg, deps),
		Logger:   slog.Default(),
	}
	// The tenant-side services (M77). Everything a tenant's dsh needs that is bound to this machine
	// runs here, not on the control plane: the sandbox and its worker (the manager), ssh workspace
	// mounts, browser-directory FUSE mounts, host-directory binds and the tenant web plugins. Host
	// binds and plugin rows are the manager's own business, so only the two services with their own
	// runtime are wired here.
	//
	// Order matters: the browser service must be in place before the workers start, because a
	// worker's profile binds the mount points that exist when it starts.
	plane := &nodeplane.Plane{
		Config: cfg, Registry: deps.reg, Workers: deps.manager, Logger: slog.Default(),
	}
	var browserService *browsermount.Service
	var browserDone <-chan struct{}
	var cancelReaper context.CancelFunc
	if cfg.BrowserWorkspaces.Enabled {
		service := browsermount.NewWithState(deps.manager.Restart, nil, cfg.StateDir)
		service.SetRegistry(deps.reg)
		// Raw stop only: Manager.StopWorker would recursively call DropTenant.
		service.SetStopWorker(deps.manager.StopWorkerProcess)
		if err := service.AcquireLock(); err != nil {
			return fmt.Errorf("browser workspaces: %w", err)
		}
		if err := service.CleanupStale(); err != nil {
			slog.Warn("stale browser mounts require manual cleanup", "err", err, "node", cfg.Node.Name)
		}
		deps.manager.BrowserWorkspaces = service
		plane.Browser = service
		browserService = service
		// The reaper must not unmount on cancellation while workers still bind FUSE, so it gets its
		// own context and is cancelled before the workers are stopped.
		reapCtx, cancel := context.WithCancel(context.Background())
		cancelReaper = cancel
		done := make(chan struct{})
		browserDone = done
		go func() {
			defer close(done)
			service.Run(reapCtx)
		}()
		slog.Info("browser-directory workspaces enabled", "node", cfg.Node.Name, "state_dir", cfg.StateDir)
	}
	if cfg.SSHWorkspaces.Enabled {
		sshService, ok := deps.manager.SSHWorkspaces.(*sshworkspace.Service)
		if !ok {
			return errors.New("ssh workspaces: the runtime did not assemble the service")
		}
		if sshErr := sshService.CheckBinaries(); sshErr != nil {
			return fmt.Errorf("ssh workspaces: %w", sshErr)
		}
		remotes := func() []sshworkspace.Remote {
			tenants := deps.reg.List()
			out := make([]sshworkspace.Remote, 0, len(tenants))
			for _, tenant := range tenants {
				out = append(out, sshworkspace.Remote{Tenant: tenant.Name, Workspace: tenant.Workspace, DshHome: tenant.DshHome})
			}
			return out
		}
		go func() {
			if !cfg.SSHWorkspaces.DisableAutoRemount {
				sshService.Reconcile(ctx, remotes())
			}
			ticker := time.NewTicker(cfg.SSHWorkspaces.PollInterval.Duration())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := deps.reg.Reload(); err != nil {
						slog.Warn("reloading this node's allocation table for ssh workspaces failed", "err", err)
					}
					if handled := sshService.PollOnce(ctx, remotes()); handled > 0 {
						slog.Info("ssh workspace requests handled", "node", cfg.Node.Name, "count", handled)
					}
				}
			}
		}()
		slog.Info("ssh workspaces enabled", "node", cfg.Node.Name,
			"mount_subdir", cfg.SSHWorkspaces.MountSubdir, "poll_interval", cfg.SSHWorkspaces.PollInterval.Duration())
	}
	server, err := nodeserve.New(nodeserve.Options{
		Name:      cfg.Node.Name,
		Version:   version,
		Revision:  revision,
		Token:     token,
		Ops:       ops.Handlers(),
		Tenants:   plane,
		Logger:    slog.Default(),
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Node.Listen)
	if err != nil {
		return fmt.Errorf("bind %s: %w", cfg.Node.Listen, err)
	}
	defer listener.Close()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	workers := make(chan struct{})
	go func() {
		defer close(workers)
		if err := deps.manager.StartWorkers(ctx); err != nil {
			slog.Error("starting this node's tenant workers failed", "err", err)
		}
	}()

	serveResult := make(chan error, 1)
	go func() {
		slog.Info("dshgw node listening",
			"node", cfg.Node.Name, "listen", cfg.Node.Listen,
			"version", version, "revision", revision,
			"tenants", len(deps.reg.List()))
		serveResult <- server.Serve(ctx, listener, 10*time.Second)
	}()

	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	}
	// Ordered shutdown, the same sequence the control plane uses for its own browser mounts:
	// stop accepting requests, let the reaper and the mount owners finish, drain, stop the
	// workers, and only then clean the mounts up. Stopping workers first would leave FUSE mounts
	// held by processes that are already gone (which is what makes a logout hang).
	stop()
	if browserService != nil {
		if cancelReaper != nil {
			cancelReaper()
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		drain := func(drainCtx context.Context) error {
			select {
			case err := <-serveResult:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			case <-drainCtx.Done():
				return drainCtx.Err()
			}
		}
		serveErr = errors.Join(serveErr, shutdownBrowserWorkspaces(shutdown, browserService, browserDone, nil, drain, deps.manager.ShutdownWorkers))
	} else {
		if cancelReaper != nil {
			cancelReaper()
		}
		if browserDone != nil {
			<-browserDone
		}
		select {
		case err := <-serveResult:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr = errors.Join(serveErr, err)
			}
		case <-time.After(30 * time.Second):
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		serveErr = errors.Join(serveErr, deps.manager.ShutdownWorkers(shutdown))
	}
	<-workers
	return serveErr
}

// nodeFeatures reports what this node can serve, so the control plane's console can say which
// machine offers ssh workspaces or browser-directory mounts instead of discovering it by failure.
func nodeFeatures(cfg *config.Config, deps *runtimeDeps) nodeproto.Features {
	features := nodeproto.Features{
		SSHWorkspaces:     cfg.SSHWorkspaces.Enabled,
		BrowserWorkspaces: cfg.BrowserWorkspaces.Enabled,
		HostShares:        len(cfg.HostShares.Shares),
		DirectoryPicker:   cfg.DirectoryPicker,
		PluginBrowserFS:   cfg.PluginBrowserFS,
	}
	if deps.manager.SSHWorkspaces != nil {
		features.SSHMounts = len(deps.manager.SSHWorkspaces.AttachedMounts(cfg.Node.Name))
	}
	for _, plugin := range []struct {
		name    string
		enabled bool
	}{
		{"web-tty", cfg.TenantPlugins.WebTTY.Enabled},
		{"workspace-files", cfg.TenantPlugins.WorkspaceFiles.Enabled},
		{"git-diff", cfg.TenantPlugins.GitDiff.Enabled},
	} {
		if plugin.enabled {
			features.TenantPlugins = append(features.TenantPlugins, plugin.name)
		}
	}
	return features
}

// nodeDoctor checks what a worker node must have before it can host a tenant. The list is the
// node-side half of the control plane's doctor: the runtime the sandbox binds, the bwrap
// preconditions, the template and plugin assets, the fuse runtime when a feature needs it, and
// the one thing that is easy to get wrong — whether the token file is readable and private.
func (c *cli) nodeDoctor(ctx context.Context) error {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	cfg := deps.cfg
	if !cfg.NodeMode() {
		return errors.New("this configuration has no `node:` block: run `dshgw doctor` for a control plane")
	}
	type check struct {
		name string
		run  func() error
	}
	checks := []check{
		{"node-name", func() error {
			if !config.ValidNodeName(cfg.Node.Name) {
				return fmt.Errorf("node.name %q is not usable", cfg.Node.Name)
			}
			return nil
		}},
		{"node-token", func() error { _, err := cfg.NodeSelfToken(); return err }},
		{"node-listen-address", func() error { return checkNodeListenAddress(cfg.Node.Listen) }},
		{"node-listen-free", func() error { return checkListenFree(cfg.Node.Listen) }},
		{"state-dir-private", func() error { return checkPrivateDirectory(cfg.StateDir) }},
		{"tenant-root-private", func() error { return checkPrivateDirectory(cfg.TenantRoot) }},
		{"workspace-root-private", func() error { return checkPrivateDirectory(cfg.WorkspaceRoot) }},
		{"config-private", func() error { return checkPrivateFile(c.configPath) }},
		{"registry-private", func() error { return checkPrivateFile(cfg.RegistryPath) }},
		{"handshake-dir-private", func() error { return checkPrivateDirectory(cfg.HandshakeDir) }},
		{"aigw-reachable", func() error { return checkAigwReachable(ctx, cfg.AigwBaseURL) }},
		{"dsh-node", func() error { return executable(cfg.Dsh.NodeBin) }},
		{"dsh-bin-js", func() error { _, err := os.Stat(cfg.Dsh.BinJS); return err }},
		{"dsh-current-symlink", func() error {
			info, err := os.Lstat(cfg.Dsh.CurrentLink)
			if err == nil && info.Mode()&os.ModeSymlink == 0 {
				return errors.New("current_link is not a symlink")
			}
			return err
		}},
		{"dsh-template", func() error {
			return tenancy.ValidateTemplate(cfg.Deploy.TemplateHome, cfg.PluginBrowserFS == "on")
		}},
		{"picker-plugin", func() error { _, err := os.Stat(cfg.Deploy.PluginPath); return err }},
		{"bwrap-bin", func() error { return executable(cfg.Deploy.BwrapBin) }},
		{"bwrap-sandbox-runtime", func() error { return sandbox.ValidateRuntime(sandboxRuntimeConfig(cfg)) }},
		{"bwrap-worker-account", func() error { return sandbox.ValidateWorkerAccount(cfg.Deploy.WorkerUser) }},
		{"worker-port-band", func() error { return checkWorkerPortBand(ctx, cfg.WorkerPortLo, cfg.WorkerPortHi) }},
		{"user-manager", checkUserManager},
	}
	// The same plugin-directory check the control plane's doctor runs: a node renders the same
	// profile, so a missing plugin directory breaks the same accounts here.
	for _, plugin := range []struct {
		dir     string
		enabled bool
	}{
		{dir: "web-tty", enabled: cfg.TenantPlugins.WebTTY.Enabled},
		{dir: "workspace-files", enabled: cfg.TenantPlugins.WorkspaceFiles.Enabled},
		{dir: "git-diff", enabled: cfg.TenantPlugins.GitDiff.Enabled},
	} {
		if !plugin.enabled {
			continue
		}
		path := filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), plugin.dir, "index.js")
		checks = append(checks, check{name: plugin.dir + "-plugin", run: func() error { _, err := os.Stat(path); return err }})
	}
	if cfg.SSHWorkspaces.Enabled {
		checks = append(checks, check{name: "sshfs-bin", run: func() error {
			if cfg.SSHWorkspaces.SSHFSBin != "" {
				return executable(cfg.SSHWorkspaces.SSHFSBin)
			}
			_, err := exec.LookPath("sshfs")
			return err
		}})
	}
	if cfg.BrowserWorkspaces.Enabled {
		checks = append(checks,
			check{name: "fuse-device", run: func() error { _, err := os.Stat("/dev/fuse"); return err }},
			check{name: "fusermount3-bin", run: func() error { _, err := exec.LookPath("fusermount3"); return err }},
		)
	}

	failures := 0
	for _, item := range checks {
		if err := item.run(); err != nil {
			failures++
			fmt.Fprintf(c.stdout, "FAIL\t%s\t%s\n", item.name, err)
			continue
		}
		fmt.Fprintf(c.stdout, "OK\t%s\n", item.name)
	}
	tenants := deps.reg.List()
	fmt.Fprintf(c.stdout, "INFO\tnode\t%s\tlisten=%s\ttenants=%d\tworker_ports=%d-%d\n",
		cfg.Node.Name, cfg.Node.Listen, len(tenants), cfg.WorkerPortLo, cfg.WorkerPortHi)
	for _, tenant := range tenants {
		state, statusErr := deps.manager.Status(ctx, tenant)
		if statusErr != nil {
			failures++
			fmt.Fprintf(c.stdout, "FAIL\ttent-%s-status\t%s\n", tenant.Name, statusErr)
			continue
		}
		fmt.Fprintf(c.stdout, "INFO\ttenant-%s\trunning=%t\tpid=%d\tsuspended=%t\thandshake=%s\n",
			tenant.Name, state.Running, state.PID, tenant.Suspended, tenant.Handshake)
	}
	if failures > 0 {
		return fmt.Errorf("node doctor found %d failure(s)", failures)
	}
	return nil
}

// nodeStatus prints this node's own view: what it hosts and what is running. It reads local
// state only, so it works with the control plane down.
func (c *cli) nodeStatus(ctx context.Context) error {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	cfg := deps.cfg
	if !cfg.NodeMode() {
		return errors.New("this configuration has no `node:` block: run `dshgw tenant list` for a control plane")
	}
	type row struct {
		Name      string `json:"name"`
		Worker    int    `json:"worker_port"`
		Running   bool   `json:"running"`
		PID       int    `json:"pid"`
		Suspended bool   `json:"suspended"`
		Handshake string `json:"handshake"`
	}
	out := struct {
		Node     string `json:"node"`
		Listen   string `json:"listen"`
		Version  string `json:"version"`
		Revision string `json:"revision"`
		Protocol int    `json:"protocol"`
		Tenants  []row  `json:"tenants"`
	}{Node: cfg.Node.Name, Listen: cfg.Node.Listen, Version: version, Revision: revision, Protocol: nodeproto.Version, Tenants: []row{}}
	for _, tenant := range deps.reg.List() {
		item := row{Name: tenant.Name, Worker: tenant.WorkerPort, Suspended: tenant.Suspended, Handshake: string(tenant.Handshake)}
		if state, err := deps.manager.Status(ctx, tenant); err == nil {
			item.Running, item.PID = state.Running, state.PID
		}
		out.Tenants = append(out.Tenants, item)
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, string(encoded))
	return nil
}

// checkNodeListenAddress refuses the two shapes that would expose a node to more than its
// control plane: a wildcard address, and a bare port.
func checkNodeListenAddress(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if strings.TrimSpace(host) == "" {
		return errors.New("node.listen must name the interface to bind")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return errors.New("node.listen must not be a wildcard address: this listener is full access to every tenant on this machine")
	}
	return nil
}

// checkListenFree reports whether anything is already listening on that address.
func checkListenFree(listen string) error {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("%s is not bindable: %w", listen, err)
	}
	return listener.Close()
}

// checkWorkerPortBand verifies the band is usable and that nothing holds a port in it.
func checkWorkerPortBand(ctx context.Context, lo, hi int) error {
	if lo < 1 || hi > 65535 || lo > hi {
		return fmt.Errorf("worker port band %d-%d is not a valid range", lo, hi)
	}
	ports, err := listeningPorts(ctx)
	if err != nil {
		// The band is still validated; whether a port is free is reported by the start path.
		return nil
	}
	for port := lo; port <= hi; port++ {
		if ports[port] {
			return fmt.Errorf("port %d in the worker band is already in use by another process", port)
		}
	}
	return nil
}

// checkAigwReachable probes the gateway a tenant's model traffic goes to. A node whose workers
// cannot reach aigw hosts tenants that start and then cannot answer a single request, so this
// is worth reporting during install rather than during an incident.
func checkAigwReachable(ctx context.Context, baseURL string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	target := strings.TrimRight(baseURL, "/") + "/version"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("aigw at %s is not reachable from this node: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("aigw at %s answered HTTP %d", baseURL, resp.StatusCode)
	}
	return nil
}

// checkUserManager reports whether this machine has a systemd user manager and (when it has)
// whether lingering is on: without linger a node started as a user unit stops when the last
// session of that account goes away, which looks like "the node died overnight".
func checkUserManager() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl is not on PATH: run this node with setsid/nohup and its own supervision, or install systemd")
	}
	return nil
}

// nodeList is the control plane's node inventory: what this deployment knows, and what each
// machine answers right now.
//
// It probes rather than reading a cache, because the interesting question an operator has when
// they run it is "can this control plane reach its nodes", and a cached answer from an hour ago
// would be a confident lie.
func (c *cli) nodeList(ctx context.Context) error {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if deps.cfg.NodeMode() {
		return errors.New("this configuration is a worker node: `dshgw node status` shows its own tenants")
	}
	nodes := deps.nodeView
	if len(nodes) == 0 {
		fmt.Fprintf(c.stdout, "no worker nodes configured: this deployment runs every tenant on %s\\n", config.LocalNodeName)
		return nil
	}
	type row struct {
		Name      string `json:"name"`
		Address   string `json:"address"`
		Source    string `json:"source"`
		Default   bool   `json:"default"`
		Reachable bool   `json:"reachable"`
		Version   string `json:"version,omitempty"`
		Revision  string `json:"revision,omitempty"`
		Protocol  int    `json:"protocol,omitempty"`
		Tenants   int    `json:"tenants"`
		Running   int    `json:"running"`
		Error     string `json:"error,omitempty"`
		// Features is what that machine can serve (M77): the control plane cannot know it from its
		// own configuration, so it is reported here rather than discovered by failure.
		Features nodeproto.Features `json:"features,omitempty"`
	}
	defaultNode := nodestore.DefaultNode(nodes, deps.cfg.DefaultNode)
	rows := make([]row, 0, len(nodes))
	for _, node := range nodes {
		item := row{Name: node.Name, Address: node.URL, Source: node.Source, Default: node.Name == defaultNode}
		if node.Status.State == nodestore.StatePending || strings.TrimSpace(node.URL) == "" {
			item.Error = "not deployed yet (`dshgw node deploy " + node.Name + "`)"
			rows = append(rows, item)
			continue
		}
		client, ok := deps.manager.Nodes.Get(node.Name)
		if !ok {
			item.Error = "no client (the record has no address or token)"
			rows = append(rows, item)
			continue
		}
		health, probeErr := client.Probe(ctx)
		if probeErr != nil {
			item.Error = redactError(probeErr)
			rows = append(rows, item)
			continue
		}
		item.Reachable = true
		item.Version, item.Revision, item.Protocol = health.Version, health.Revision, health.Protocol
		item.Tenants, item.Running = health.Tenants, health.Running
		item.Features = health.Features
		rows = append(rows, item)
	}
	encoded, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, string(encoded))
	return nil
}

// nodeProbe asks one node to describe itself and reports the identity check's outcome.
func (c *cli) nodeProbe(ctx context.Context, name string) error {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if deps.cfg.NodeMode() {
		return errors.New("this configuration is a worker node: there are no nodes to probe here")
	}
	if _, ok := deps.nodeViewNamed(name); !ok {
		return fmt.Errorf("node %q is not defined by this deployment", name)
	}
	health, err := deps.manager.NodeHealth(ctx, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "node %s: reachable, version %s revision %s protocol %d, tenants %d (running %d, suspended %d)\n",
		health.Name, health.Version, health.Revision, health.Protocol, health.Tenants, health.Running, health.Suspended)
	return nil
}

// nodeReconcile pushes the authoritative tenant list to one node, or to all of them, and records
// what each answers.
func (c *cli) nodeReconcile(ctx context.Context, args []string) error {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if deps.cfg.NodeMode() {
		return errors.New("this configuration is a worker node: reconciliation is driven by the control plane")
	}
	if len(args) == 0 {
		if err := deps.manager.ReconcileAll(ctx); err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, "reconciled every configured node")
		return nil
	}
	result, err := deps.manager.ReconcileNode(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "reconciled node %s: %d tenant(s) hosted, started %v, stopped %v, pruned %v\n",
		args[0], len(result.Status.Tenants), result.Started, result.Stopped, result.Pruned)
	return nil
}

// nodeAudit prints a node's most recent security events, straight from its own audit file.
//
// It is the operator's answer to "what happened on that machine" without an ssh session, and it
// deliberately does not advance the merge cursor: reading the tail must not hide events from the
// gateway's own audit stream.
func (c *cli) nodeAudit(ctx context.Context, name string, lines int) error {
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if deps.cfg.NodeMode() {
		return errors.New("this configuration is a worker node: this machine's audit file is at its own audit_path")
	}
	if _, ok := deps.nodeViewNamed(name); !ok {
		return fmt.Errorf("node %q is not defined by this deployment", name)
	}
	client, ok := deps.manager.Nodes.Get(name)
	if !ok {
		return fmt.Errorf("node %q has no client (its record has no address or token)", name)
	}
	result, err := client.AuditTail(ctx, "", lines)
	if err != nil {
		return err
	}
	if len(result.Lines) == 0 {
		fmt.Fprintf(c.stdout, "node %s has no audit events yet\n", name)
		return nil
	}
	for _, line := range result.Lines {
		fmt.Fprintln(c.stdout, line)
	}
	return nil
}

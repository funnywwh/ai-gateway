package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/browsermount"
	"github.com/winger/ai-gateway/internal/dshgw/edge"
	"github.com/winger/ai-gateway/internal/dshgw/feishu"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/nodeaudit"
	"github.com/winger/ai-gateway/internal/dshgw/nodeup"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/sshworkspace"
)

// pickTicketTTL bounds the key-pick ticket this process mints for its own key login (M72). The
// page it leads to is rendered immediately and submitted once, so a couple of minutes is generous
// while keeping a leaked URL worthless.
const pickTicketTTL = 2 * time.Minute

func (c *cli) serve() (serveErr error) {
	deps, err := c.loadRuntime(true)
	if err != nil {
		return err
	}
	// M77: a worker node is not a control plane. Running this command on a node configuration
	// would bind the portal and the tenant port band on a machine that serves no tenant of its
	// own, so it fails with the command the file actually wants.
	if deps.cfg.NodeMode() {
		return errors.New("this configuration has a node: block: run `dshgw node serve` on this machine, or remove the block to run it as a control plane")
	}
	store := deps.manager.Sessions
	ops := managerOps{m: deps.manager, deps: deps, validator: deps.validator, cfg: deps.cfg, logger: slog.Default(), auditor: &audit.JSONL{Path: deps.cfg.AuditPath}}
	gateway := proxy.New(deps.cfg, deps.reg, store, handshake.FileSource{Dir: deps.cfg.HandshakeDir}, &handshake.HTTPExchanger{}, deps.validator)
	// M77: tenants placed on worker nodes get their traffic forwarded there. A single-machine
	// deployment passes an empty client set, and every tenant stays local.
	gateway.Nodes = nodeup.New(deps.cfg, deps.manager.Nodes, slog.Default())
	gateway.Authorizer = deps.validator
	gateway.KeySource = proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}
	// Login is also the tenant's lifecycle moment (M69): every sign-in re-applies the platform
	// slice of that tenant's dsh configuration and brings its worker up; signing out of the last
	// session in a tenant stops it.
	gateway.LoginPrepare = ops
	gateway.LogoutStop = ops
	if deps.cfg.Feishu.Enabled {
		verifier, err := feishu.New([]byte(deps.cfg.Feishu.TicketSecret))
		if err != nil {
			return err
		}
		// The picker's own ticket (M72) is minted here and redeemed a moment later on the same
		// origin, so it is deliberately short-lived: it covers one form submission, not a session.
		verifier.SetIssueTTL(pickTicketTTL)
		gateway.Feishu = &proxy.FeishuPortal{Enabled: true, AigwLoginURL: deps.cfg.Feishu.AigwLoginURL, Verifier: verifier}
		slog.Info("feishu login enabled", "aigw_login_url", deps.cfg.Feishu.AigwLoginURL)
	}
	gateway.Auditor = &audit.JSONL{Path: deps.cfg.AuditPath}
	gateway.Activity = &activity.Store{Path: deps.cfg.ActivityPath}
	server := &http.Server{Addr: deps.cfg.Listen, Handler: gateway.Dispatch(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: deps.cfg.MaxHeaderBytes}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var startupDone <-chan struct{}
	if deps.cfg.BrowserWorkspaces.Enabled {
		service := browsermount.NewWithState(deps.manager.Restart, nil, deps.cfg.StateDir)
		service.SetRegistry(deps.reg)
		// Raw stop only: Manager.StopWorker would recursively call DropTenant.
		service.SetStopWorker(deps.manager.StopWorkerProcess)
		if err := service.AcquireLock(); err != nil {
			return fmt.Errorf("browser workspaces: %w", err)
		}
		if err := service.CleanupStale(); err != nil {
			slog.Warn("stale browser mounts require manual cleanup", "err", err)
		}
		deps.manager.BrowserWorkspaces = service
		gateway.BrowserWorkspaces = service
		// The reaper must not unmount on cancellation while workers still bind FUSE.
		reapCtx, cancelReaper := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); service.Run(reapCtx) }()
		defer func() {
			stop()
			cancelReaper()
			shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			// Terminal worker shutdown fences in-flight admin Start/Restart without
			// persisting suspension or reentering DropTenant under service locks.
			serveErr = errors.Join(serveErr, shutdownBrowserWorkspaces(shutdown, service, done, startupDone, server.Shutdown, deps.manager.ShutdownWorkers))
		}()
	}

	if deps.cfg.PathMode() {
		slog.Warn("public_base_url path mode: the dsh UI requires its own origin; tenants render blank unless reached on their own port", "tenant_path_prefix", deps.cfg.TenantPathPrefix)
	}
	publicEdge := edge.New(deps.cfg, gateway.Dispatch(), slog.Default())
	if err := publicEdge.Reconcile(deps.reg.List()); err != nil {
		slog.Error("binding the public surface failed", "err", err)
	}
	defer publicEdge.Close()

	if deps.cfg.AdminSocket != "" {
		adminListener, err := ListenAdmin(deps.cfg)
		if err != nil {
			return fmt.Errorf("admin channel: %w", err)
		}
		defer adminListener.Close()
		admin := &AdminServer{Ops: ops, OwnerUID: os.Geteuid()}
		admin.OnTenantsChanged = func() {
			if err := publicEdge.Reconcile(deps.reg.List()); err != nil {
				slog.Error("rebinding the public surface failed", "err", err)
			}
		}
		go func() { <-ctx.Done(); adminListener.Close() }()
		go admin.Serve(ctx, adminListener)
		slog.Info("dshgw admin channel listening", "socket", deps.cfg.AdminSocket)
	}

	if deps.cfg.SSHWorkspaces.Enabled {
		sshService, ok := deps.manager.SSHWorkspaces.(*sshworkspace.Service)
		if !ok {
			return fmt.Errorf("ssh workspaces: the runtime did not assemble the service")
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
			if !deps.cfg.SSHWorkspaces.DisableAutoRemount {
				sshService.Reconcile(ctx, remotes())
			}
			ticker := time.NewTicker(deps.cfg.SSHWorkspaces.PollInterval.Duration())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := deps.reg.Reload(); err != nil {
						slog.Warn("reloading the registry for ssh workspaces failed", "err", err)
					}
					if handled := sshService.PollOnce(ctx, remotes()); handled > 0 {
						slog.Info("ssh workspace requests handled", "count", handled)
					}
				}
			}
		}()
		slog.Info("ssh workspaces enabled", "mount_subdir", deps.cfg.SSHWorkspaces.MountSubdir, "poll_interval", deps.cfg.SSHWorkspaces.PollInterval.Duration(), "hosts", deps.cfg.SSHWorkspaces.Hosts)
	}

	// M77: the nodes' own security events (a refused ssh mount, a logout that had to break a mount)
	// are pulled into this gateway's audit file, tagged with the node they happened on, so an
	// operator reads one stream. The cursor is persisted per node, so a restart resumes.
	if deps.manager.Nodes.Len() > 0 {
		merger := nodeaudit.New(deps.manager.Nodes, deps.nodes, &audit.JSONL{Path: deps.cfg.AuditPath}, slog.Default())
		go merger.Run(ctx)
	}

	// M77: a tenant that appears while this process is running must become reachable. The console
	// creates tenants through the admin socket (which reconciles the public surface directly), but
	// the CLI is a separate process: without this, a CLI-created tenant would be recorded, running
	// on its node, and still unreachable on its public port until the gateway restarted. Watching
	// the registry file is enough — the file is replaced atomically, and a handful of tenants make
	// re-reading it cheap compared with a poll that ignores whether anything changed.
	go watchRegistry(ctx, deps, publicEdge)

	started := make(chan struct{})
	startupDone = started
	go func() {
		defer close(started)
		if err := deps.manager.StartWorkers(ctx); err != nil {
			slog.Error("starting tenant workers failed", "err", err)
		}
		// M77: the local workers are up; now make the nodes agree with this registry. Two things
		// are repaired here — a node that restarted and re-allocated worker ports while this
		// process was down (the control plane's recorded port is what the handshake's Host
		// authority is built from), and tenants whose worker state drifted from their intent.
		//
		// This runs after the workers so the local tenants are already serving while a slow or
		// unreachable node is being retried: one broken machine must not delay the gateway's own
		// tenants by a single request.
		if deps.manager.Nodes.Len() > 0 {
			if err := deps.manager.ReconcileAll(ctx); err != nil {
				slog.Error("reconciling worker nodes failed", "err", err)
			} else {
				slog.Info("worker nodes reconciled", "nodes", deps.manager.Nodes.Names())
			}
		}
	}()
	result := make(chan error, 1)
	go func() {
		slog.Info("dshgw listening", "version", version, "revision", revision, "listen", deps.cfg.Listen)
		result <- server.ListenAndServe()
	}()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

// watchRegistry reconciles the public surface when the registry file changes underneath a running
// gateway (M77).
//
// The alternative — telling operators "restart the gateway after creating a tenant" — is exactly
// the kind of undocumented step that turns into a support question, and the console's own path
// only covers tenants created through the admin socket.
func watchRegistry(ctx context.Context, deps *runtimeDeps, publicEdge *edge.Edge) {
	const interval = 2 * time.Second
	var lastMod time.Time
	var lastSize int64
	if info, err := os.Stat(deps.cfg.RegistryPath); err == nil {
		lastMod, lastSize = info.ModTime(), info.Size()
	}
	// The node store changes when a node is deployed, rotated or registered. The clients already in
	// memory must follow, or a deploy made through this gateway would leave its own tenant traffic
	// presenting a secret the node no longer accepts.
	lastNodeMod, lastNodeSize := nodeStoreFingerprint(deps.cfg)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		nodeMod, nodeSize := nodeStoreFingerprint(deps.cfg)
		if !nodeMod.Equal(lastNodeMod) || nodeSize != lastNodeSize {
			lastNodeMod, lastNodeSize = nodeMod, nodeSize
			if err := refreshNodeClients(deps.cfg, deps.nodes, deps.manager.Nodes); err != nil {
				slog.Error("refreshing the node clients failed", "err", err)
			} else {
				slog.Info("node store changed; node clients refreshed")
			}
		}
		info, err := os.Stat(deps.cfg.RegistryPath)
		if err != nil {
			continue
		}
		if info.ModTime().Equal(lastMod) && info.Size() == lastSize {
			continue
		}
		lastMod, lastSize = info.ModTime(), info.Size()
		if err := deps.reg.Reload(); err != nil {
			slog.Warn("reloading a changed registry failed", "err", err)
			continue
		}
		if err := publicEdge.Reconcile(deps.reg.List()); err != nil {
			slog.Error("reconciling the public surface after a registry change failed", "err", err)
			continue
		}
		slog.Info("registry changed; public surface reconciled", "tenants", len(deps.reg.List()))
	}
}

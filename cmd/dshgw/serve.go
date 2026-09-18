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
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/sshworkspace"
)

func (c *cli) serve() (serveErr error) {
	deps, err := c.loadRuntime(true)
	if err != nil {
		return err
	}
	store := deps.manager.Sessions
	ops := managerOps{m: deps.manager, validator: deps.validator, cfg: deps.cfg, logger: slog.Default()}
	gateway := proxy.New(deps.cfg, deps.reg, store, handshake.FileSource{Dir: deps.cfg.HandshakeDir}, &handshake.HTTPExchanger{}, deps.validator)
	gateway.Authorizer = deps.validator
	gateway.KeySource = proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}
	// Login adopts a credential already proven valid for this tenant.
	gateway.KeyAdopter = ops
	if deps.cfg.Feishu.Enabled {
		verifier, err := feishu.New([]byte(deps.cfg.Feishu.TicketSecret))
		if err != nil {
			return err
		}
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

	started := make(chan struct{})
	startupDone = started
	go func() {
		defer close(started)
		if err := deps.manager.StartWorkers(ctx); err != nil {
			slog.Error("starting tenant workers failed", "err", err)
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

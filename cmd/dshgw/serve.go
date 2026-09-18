package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/edge"
	"github.com/winger/ai-gateway/internal/dshgw/feishu"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/sshworkspace"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func (c *cli) serve() error {
	deps, err := c.loadRuntime(true)
	if err != nil {
		return err
	}
	store := deps.manager.Sessions
	ops := managerOps{m: deps.manager, validator: deps.validator, cfg: deps.cfg, logger: slog.Default()}
	gateway := proxy.New(deps.cfg, deps.reg, store, handshake.FileSource{Dir: deps.cfg.HandshakeDir}, &handshake.HTTPExchanger{}, deps.validator)
	gateway.Authorizer = deps.validator
	gateway.KeySource = proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}
	// Login is the one moment the deployment holds a key it has already proven valid
	// for this tenant: a tenant that never stored one (hand-built, restored, migrated)
	// is configured here instead of opening dsh with no providers at all.
	gateway.KeyAdopter = ops
	// M61: the Feishu login handoff. dshgw holds no Feishu credential — aigw identified the
	// person and signed a short-lived ticket, and this side verifies it and then asks the same
	// entitlement question the key login asks.
	if deps.cfg.Feishu.Enabled {
		verifier, err := feishu.New([]byte(deps.cfg.Feishu.TicketSecret))
		if err != nil {
			return err
		}
		gateway.Feishu = &proxy.FeishuPortal{
			Enabled:      true,
			AigwLoginURL: deps.cfg.Feishu.AigwLoginURL,
			Verifier:     verifier,
		}
		slog.Info("feishu login enabled", "aigw_login_url", deps.cfg.Feishu.AigwLoginURL)
	}
	gateway.Auditor = &audit.JSONL{Path: deps.cfg.AuditPath}
	gateway.Activity = &activity.Store{Path: deps.cfg.ActivityPath}
	server := &http.Server{Addr: deps.cfg.Listen, Handler: gateway.Dispatch(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: deps.cfg.MaxHeaderBytes}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The public surface: with no nginx in this shape, dshgw binds the portal port
	// and every tenant's public port itself, setting the edge port header the
	// gateway routes on (exactly what the edge proxy used to do). Reconcile is
	// idempotent, so it runs at startup and after any lifecycle change.
	if deps.cfg.PathMode() {
		// Measured: dsh's web client builds its API and stream URLs from
		// location.origin, and an origin never carries a path. Under a path prefix
		// those requests land on the domain root — served by aigw, not the tenant —
		// and the UI renders blank. Path mode is therefore for the *portal* (our own
		// HTML); a tenant needs its own origin, which in practice means its own port.
		slog.Warn("public_base_url path mode: the dsh UI requires its own origin; tenants render blank unless reached on their own port",
			"tenant_path_prefix", deps.cfg.TenantPathPrefix)
	}
	publicEdge := edge.New(deps.cfg, gateway.Dispatch(), slog.Default())
	if err := publicEdge.Reconcile(deps.reg.List()); err != nil {
		// A port that cannot be bound is reported, not fatal: the gateway keeps its
		// loopback listener and the surfaces that did bind keep serving.
		slog.Error("binding the public surface failed", "err", err)
	}
	defer publicEdge.Close()

	// The provisioning channel runs in this process when a socket is configured:
	// the console's "启用/停用 DSH" buttons need a live lifecycle owner, and in the
	// supervised shape the running gateway *is* that owner (a standalone CLI
	// process could not keep a tenant's worker alive after it exits).
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

	// M64: ssh workspaces. The service owns the mount record and the per-account mailbox;
	// the manager hook is what makes a worker's profile bind the mounts and what detaches
	// them when an account goes away. It is assembled only when configured — a gateway that
	// does not enable the feature never runs ssh.
	if deps.cfg.SSHWorkspaces.Enabled {
		sshService, sshErr := sshworkspace.New(sshworkspace.Options{
			MountSubdir:     deps.cfg.SSHWorkspaces.MountSubdir,
			SSHBin:          deps.cfg.SSHWorkspaces.SSHBin,
			SSHFSBin:        deps.cfg.SSHWorkspaces.SSHFSBin,
			IdentitySource:  deps.cfg.SSHWorkspaces.IdentitySource,
			IdentityDir:     deps.cfg.SSHWorkspaces.IdentityDir,
			SSHConfigSource: deps.cfg.SSHWorkspaces.SSHConfigSource,
			Hosts:           deps.cfg.SSHWorkspaces.Hosts,
			ConnectTimeout:  deps.cfg.SSHWorkspaces.ConnectTimeout.Duration(),
			MaxEntries:      deps.cfg.SSHWorkspaces.MaxEntries,
			SSHFSOptions:    deps.cfg.SSHWorkspaces.SSHFSOptions,
		}, sshworkspace.NewStore(filepath.Join(deps.cfg.StateDir, "ssh-mounts.json")), func(restartCtx context.Context, tenant string) error {
			current, ok := deps.reg.Get(tenant)
			if !ok {
				return fmt.Errorf("unknown tenant %s", tenant)
			}
			return deps.manager.Restart(restartCtx, current)
		}, &audit.JSONL{Path: deps.cfg.AuditPath}, slog.Default())
		if sshErr != nil {
			return fmt.Errorf("ssh workspaces: %w", sshErr)
		}
		if sshErr := sshService.CheckBinaries(); sshErr != nil {
			return fmt.Errorf("ssh workspaces: %w", sshErr)
		}
		deps.manager.SSHWorkspaces = sshService
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
				// Mounts are kernel state: they do not survive this process, so the record
				// file is re-applied at every start.
				sshService.Reconcile(ctx, remotes())
			}
			ticker := time.NewTicker(deps.cfg.SSHWorkspaces.PollInterval.Duration())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if handled := sshService.PollOnce(ctx, remotes()); handled > 0 {
						slog.Info("ssh workspace requests handled", "count", handled)
					}
				}
			}
		}()
		slog.Info("ssh workspaces enabled", "mount_subdir", deps.cfg.SSHWorkspaces.MountSubdir,
			"poll_interval", deps.cfg.SSHWorkspaces.PollInterval.Duration(), "hosts", deps.cfg.SSHWorkspaces.Hosts)
	}

	// Tenants that are not suspended come back with their gateway: this is what
	// replaced systemd's "enabled units start on boot". A worker that fails to
	// start is reported and skipped — one broken tenant must not keep the others
	// (or the gateway itself) down.
	go func() {
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

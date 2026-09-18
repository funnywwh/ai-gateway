package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/handshake"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func (c *cli) serve() error {
	deps, err := c.loadRuntime(true)
	if err != nil {
		return err
	}
	store := deps.manager.Sessions
	gateway := proxy.New(deps.cfg, deps.reg, store, handshake.FileSource{Dir: deps.cfg.HandshakeDir}, &handshake.HTTPExchanger{}, deps.validator)
	gateway.Authorizer = deps.validator
	gateway.KeySource = proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}
	gateway.Auditor = &audit.JSONL{Path: deps.cfg.AuditPath}
	gateway.Activity = &activity.Store{Path: deps.cfg.ActivityPath}
	server := &http.Server{Addr: deps.cfg.Listen, Handler: gateway.Dispatch(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: deps.cfg.MaxHeaderBytes}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		admin := &AdminServer{Ops: managerOps{m: deps.manager, validator: deps.validator, cfg: deps.cfg}, OwnerUID: os.Geteuid()}
		go func() { <-ctx.Done(); adminListener.Close() }()
		go admin.Serve(ctx, adminListener)
		slog.Info("dshgw admin channel listening", "socket", deps.cfg.AdminSocket)
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

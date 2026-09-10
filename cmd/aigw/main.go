// Command aigw is the AI gateway server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/apikey"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/creds"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/hook"
	"github.com/winger/ai-gateway/internal/httpapi"
	"github.com/winger/ai-gateway/internal/logx"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/pluginhost"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/store"
	"github.com/winger/ai-gateway/internal/usage"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() { os.Exit(run()) }

func run() int {
	showVersion := flag.Bool("version", false, "print version information and exit")
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	if *showVersion {
		fmt.Println("aigw", version, "(commit "+commit+",", "built "+date+")")
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aigw: configuration error:", err)
		return 2
	}

	log := logx.New(cfg.Log)
	log.Info("aigw starting",
		"version", version,
		"commit", commit,
		"config", *configPath,
		"listen", cfg.Server.Listen,
		"database", cfg.Database.Path,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		log.Error("open database failed", "err", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	log.Info("database ready", "path", db.Path())

	if res, err := db.Bootstrap(ctx, cfg.Bootstrap, cfg.Plugins.StateDir); err != nil {
		log.Error("bootstrap failed", "err", err)
		return 1
	} else if res.Mode != "off" {
		log.Info("bootstrap done",
			"mode", res.Mode,
			"accounts", res.AccountsCreated,
			"api_keys", res.APIKeysCreated,
			"providers", res.ProvidersCreated,
			"provider_models", res.ProviderModelsAdded,
			"models", res.ModelsCreated,
			"routes", res.RoutesAdded,
			"tags", res.TagsAdded,
		)
	}

	reg := registry.New(db)
	snap, err := reg.Reload(ctx)
	if err != nil {
		log.Error("registry reload failed", "err", err)
		return 1
	}
	log.Info("registry loaded", "summary", snap.String(), "ready", snap.Ready())

	balancerState := balancer.New(balancer.Config{
		BreakerFailures: cfg.Routing.Breaker.Failures,
		BreakerWindow:   time.Duration(cfg.Routing.Breaker.WindowS) * time.Second,
		BreakerCooldown: time.Duration(cfg.Routing.Breaker.CooldownS) * time.Second,
	})
	router := routing.New(routing.Config{
		DefaultStrategy: cfg.Routing.DefaultStrategy,
		Degradation:     cfg.Routing.Degradation,
		DefaultGrant:    cfg.Auth.DefaultGrant,
		ModelFallback:   cfg.Routing.ModelFallback,
	}, reg, balancerState)
	log.Info("routing ready",
		"strategy", cfg.Routing.DefaultStrategy,
		"degradation", cfg.Routing.Degradation,
		"models", len(snap.Models),
		"routes", len(snap.Routes),
		"mappings", len(snap.Mappings),
	)

	verifier := apikey.New(db, apikey.Config{
		TTL:           cfg.Auth.KeyCacheTTL(),
		NegativeTTL:   5 * time.Second,
		MaxEntries:    10000,
		TouchInterval: time.Minute,
	})
	limiter := quota.New(cfg.RateLimit.Shards)
	meter := usage.New(db)
	log.Info("auth, rate limit and metering ready",
		"key_cache_ttl", cfg.Auth.KeyCacheTTL().String(),
		"limiter_shards", cfg.RateLimit.Shards,
		"default_grant", cfg.Auth.DefaultGrant,
	)

	host := pluginhost.New(pluginhost.Config{
		Dir:                  cfg.Plugins.Dir,
		Extra:                cfg.Plugins.Extra,
		StateDir:             cfg.Plugins.StateDir,
		StartTimeout:         time.Duration(cfg.Plugins.StartTimeoutS) * time.Second,
		PingInterval:         time.Duration(cfg.Plugins.PingIntervalS) * time.Second,
		MaxRestartsPerMinute: cfg.Plugins.MaxRestartsPerMin,
		LogTailLines:         cfg.Plugins.LogTailLines,
		CancelGrace:          time.Duration(cfg.Routing.TTFTTimeoutS) * time.Second,
	}, log)
	dispatcher := runtime.New(runtime.Config{
		CredentialsKey: creds.DeriveKey(cfg.CredentialsKey),
	}, db, reg, host, balancerState, log)

	hooks, err := db.ListHooks(ctx)
	if err != nil {
		log.Error("loading hooks failed", "err", err)
		return 1
	}
	hookCfg := hook.DefaultConfig()
	hookCfg.QueueSize = cfg.Hooks.QueueSize
	hookCfg.Workers = cfg.Hooks.Workers
	hookCfg.Timeout = time.Duration(cfg.Hooks.TimeoutS) * time.Second
	hookCfg.Retries = cfg.Hooks.Retries
	hookCfg.DeadLetter = cfg.Hooks.DeadLetter
	hookDispatcher := hook.New(hookCfg, hooks, log)
	log.Info("hooks ready", "configured", len(hookDispatcher.Hooks()), "queue_size", hookCfg.QueueSize)

	mcpService := mcpsrv.New(db, reg, mcpsrv.Config{
		MaxRows:    cfg.MCP.MaxQueryRows,
		WindowDays: cfg.MCP.RequestWindowDays,
		Currency:   cfg.Billing.Currency,
	})

	adminAuth := admin.NewAuth(db, admin.Config{
		SessionTTL:    12 * time.Hour,
		LoginAttempts: 10,
		LoginWindow:   5 * time.Minute,
	})
	adminHash, err := admin.HashPassword(cfg.Bootstrap.Admin.Password)
	if err != nil {
		log.Error("hashing the bootstrap admin password failed", "err", err)
		return 1
	}
	if cfg.Bootstrap.Admin.Username != "" && cfg.Bootstrap.Admin.Password != "" {
		if _, err := db.UpsertAdminUser(ctx, &domain.AdminUser{
			Username: cfg.Bootstrap.Admin.Username, PasswordHash: adminHash, Role: "admin",
		}); err != nil {
			log.Error("seeding the bootstrap admin user failed", "err", err)
			return 1
		}
		log.Info("admin user ready", "username", cfg.Bootstrap.Admin.Username)
	}

	api := httpapi.New(httpapi.Deps{
		Config:     cfg,
		Registry:   reg,
		Router:     router,
		Dispatcher: dispatcher,
		Verifier:   verifier,
		Limiter:    limiter,
		Meter:      meter,
		Records:    db,
		MCP:        mcpService,
		MCPTokens:  db,
		Hooks:      hookDispatcher,
		Admin:      adminAuth,
		AdminStore: db,
		Reload: func(ctx context.Context) (any, error) {
			snap, err := reg.Reload(ctx)
			if err != nil {
				return nil, err
			}
			return snap.String(), nil
		},
		InvalidateKey: func(prefix string) {
			if prefix != "" {
				verifier.Invalidate(prefix)
			}
		},
		InvalidateAll: verifier.InvalidateAll,
		KeyCacheSize:  verifier.Size,
		Log:        log,
		Version:    version,
	})

	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout(),
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.Server.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Error("http server failed", "err", err)
		_ = host.StopAll(ctx)
		return 1
	case <-ctx.Done():
	}

	log.Info("shutting down")
	api.SetReady(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown incomplete", "err", err)
	}
	if err := host.StopAll(shutdownCtx); err != nil {
		log.Warn("stopping plugin processes failed", "err", err)
	}
	hookDispatcher.Close(3 * time.Second)
	return 0
}

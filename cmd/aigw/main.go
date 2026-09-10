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
	"github.com/winger/ai-gateway/internal/backup"
	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/billing"
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
	"github.com/winger/ai-gateway/internal/webui"
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

	// A staged restore is applied before anything opens the database: the swap has to

	// happen while no connection pool holds the old file.

	if preserved, err := backup.ApplyPendingRestore(cfg.Database.Path, time.Now().UTC(), log); err != nil {

		log.Error("applying a staged database restore failed", "err", err)

		return 1

	} else if preserved != "" {

		log.Warn("database restored from a staged snapshot", "preserved", preserved)

	}

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
	credKey := creds.DeriveKey(cfg.CredentialsKey)
	var credentialSealer *creds.Sealer
	if cfg.CredentialsKey == "" {
		log.Warn("credentials_key is not configured: provider credentials cannot be stored through the admin API")
		credentialSealer = creds.NewSealer(nil)
	} else {
		credentialSealer = creds.NewSealer(credKey)
	}
	dispatcher := runtime.New(runtime.Config{
		CredentialsKey: credKey,
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

	backupManager := backup.New(backup.Config{
		OnEvent: func(name string, payload map[string]any) {
			hookDispatcher.Emit(ctx, &domain.Event{
				Name: name, Timestamp: time.Now().UTC(), Payload: payload,
			})
		},

		DatabasePath: cfg.Database.Path,

		Dir: cfg.Backup.Dir,

		Enabled: cfg.Backup.Enabled,

		Verify: cfg.Backup.Verify,

		Retention: backup.Retention{

			Daily: cfg.Backup.RetentionDaily, Weekly: cfg.Backup.RetentionWeekly,

			Monthly: cfg.Backup.RetentionMonthly,
		},
	}, db, log)

	if interrupted, err := db.FailRunningBackupJobs(ctx, "interrupted by a restart"); err != nil {

		log.Warn("closing out interrupted backup jobs failed", "err", err)

	} else if interrupted > 0 {

		log.Warn("marked interrupted backup jobs as failed", "count", interrupted)

	}

	backupScheduler, err := backup.NewScheduler(backupManager, cfg.Backup.Cron)

	if err != nil {

		log.Error("backup schedule is invalid", "err", err, "cron", cfg.Backup.Cron)

		return 2

	}

	if cfg.Backup.Enabled {

		backupScheduler.Start(ctx, log)

	} else {

		log.Info("automatic backups are disabled")

	}

	billingService := billing.NewService(ctx, db, billing.ServiceConfig{
		Writer: billing.Config{
			BatchSize:     cfg.Billing.WriterBatchSize,
			FlushInterval: time.Duration(cfg.Billing.WriterFlushMS) * time.Millisecond,
			FallbackFile:  cfg.Billing.FallbackFile,
			OnFallback: func(settlement *billing.Settlement, cause error) {
				// The fallback file is the durable record; the database row is for querying.
				if err := db.RecordBillingFailure(ctx, settlement, cause); err != nil {
					log.Error("recording a billing failure failed", "err", err)
				}
			},
		},
		ReservationTTL: time.Duration(cfg.Billing.ReservationTTLS) * time.Second,
		ReplayInterval: time.Minute,
	}, log)
	billingService.StartReservationGC(ctx, time.Minute)
	billingService.SetMismatchHandler(func(record *domain.Reconciliation) {
		hookDispatcher.Emit(ctx, &domain.Event{
			Name:      "billing.reconcile_mismatch",
			Timestamp: time.Now().UTC(),
			Payload: map[string]any{
				"kind": record.Kind, "diff_micros": record.DiffMicros,
				"usage_charge_micros":  record.UsageChargeMicros,
				"ledger_charge_micros": record.LedgerChargeMicros,
			},
		})
	})
	log.Info("billing ready", "batch_size", cfg.Billing.WriterBatchSize, "fallback_file", cfg.Billing.FallbackFile)

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
		// One narrow port per resource family; the composition root is the only place
		// that knows a single *store.DB backs all of them.
		Accounts:      db,
		Providers:     db,
		Models:        db,
		Tags:          db,
		HookStore:     db,
		MCPTokenStore: db,
		Settings:      db,
		Secrets:       credentialSealer,
		Prober:        dispatcher,
		ReloadHooks: func(ctx context.Context) error {
			list, err := db.ListHooks(ctx)
			if err != nil {
				return err
			}
			hookDispatcher.SetHooks(list)
			log.Info("hooks reloaded", "configured", len(list))
			return nil
		},
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
		InvalidateAll:  verifier.InvalidateAll,
		KeyCacheSize:   verifier.Size,
		UI:             webui.Handler(),
		Billing:        billingService,
		Ledger:         billingService,
		Invoices:       billingService,
		Codes:          billingService,
		Reconciliation: billingService,
		Backups:        backupManager,
		Log:            log,
		Version:        version,
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
	if remaining := billingService.Close(5 * time.Second); remaining > 0 {
		log.Error("billing settlements could not be persisted before shutdown", "remaining", remaining)
	}
	hookDispatcher.Close(3 * time.Second)
	return 0
}

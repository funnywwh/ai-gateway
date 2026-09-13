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
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/quota"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/retention"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/internal/store"
	"github.com/winger/ai-gateway/internal/usage"
	"github.com/winger/ai-gateway/internal/webui"
)

var (
	// Set by the build (`make build`): the release version comes from the VERSION file at
	// the repository root, the revision from git. The defaults describe a binary built by
	// hand — a bare `go build` reports "dev", which is honest, rather than inventing a
	// version number nobody can trace back to a release.
	version  = "dev"
	revision = "none"
	date     = "unknown"
)

func main() { os.Exit(run()) }

func run() int {
	// A subcommand form keeps the local MCP server out of the HTTP binary''s flag space.
	if len(os.Args) > 1 && os.Args[1] == "mcp-serve" {
		return runMCPServe(os.Args[2:])
	}

	showVersion := flag.Bool("version", false, "print version information and exit")
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	if *showVersion {
		fmt.Println("aigw", version, "(revision "+revision+",", "built "+date+")")
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
		"revision", revision,
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
	db.SetDimensionRollupsEnabled(cfg.Recording.DimensionRollupEnabled)
	stopDimensionRollups := db.StartDimensionRollups(ctx, log)
	defer stopDimensionRollups()
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
	// Session stickiness is configured in seconds; the router works in durations.
	affinityTTL := time.Duration(cfg.Routing.SessionAffinityTTLS) * time.Second
	router := routing.New(routing.Config{
		DefaultStrategy: cfg.Routing.DefaultStrategy,
		Degradation:     cfg.Routing.Degradation,
		DefaultGrant:    cfg.Auth.DefaultGrant,
		ModelFallback:   cfg.Routing.ModelFallback,

		SessionAffinity:    cfg.Routing.SessionAffinity,
		AffinityTTL:        affinityTTL,
		AffinityMaxEntries: cfg.Routing.SessionAffinityMaxEntries,
	}, reg, balancerState)
	log.Info("routing ready",
		"strategy", cfg.Routing.DefaultStrategy,
		"degradation", cfg.Routing.Degradation,
		"models", len(snap.Models),
		"routes", len(snap.Routes),
		"mappings", len(snap.Mappings),
		"session_affinity", cfg.Routing.SessionAffinity,
		"affinity_ttl", affinityTTL.String(),
		"affinity_max_entries", cfg.Routing.SessionAffinityMaxEntries,
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
	// Gift credit matures on a daily policy, not in real time.
	billingService.StartExpiryJob(ctx, 24*time.Hour)
	mcpService.SetReservationReporter(func(accountID int64) int64 {
		var total int64
		for _, reservation := range billingService.Reservations() {
			if reservation.AccountID == accountID {
				total += reservation.AmountMicros
			}
		}
		return total
	})
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

	// Recorded observability data (request logs, stored responses) is pruned on a daily
	// policy, in batches so the single writer connection stays available to requests.
	logJanitor := retention.New(db, retention.Config{RetentionDays: cfg.Recording.RetentionDays}, log)
	logJanitor.Start(ctx, 24*time.Hour)

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

	// The currency table: configuration first, then the console override stored in
	// settings. It is rebuilt on every settings write, so an operator can fix a rate
	// without restarting the gateway.
	fxStore := pricing.NewFXStore(cfg.Billing.Currency, cfg.Billing.FXRates)
	reloadFX := func(ctx context.Context) error {
		rates := cfg.Billing.FXRates
		raw, found, err := db.GetSetting(ctx, pricing.SettingFXRates)
		if err != nil {
			return err
		}
		if found {
			override, err := pricing.ParseFXRates(raw)
			if err != nil {
				return err
			}
			rates = pricing.MergeFXRates(rates, override)
		}
		table, err := pricing.NewFXTable(cfg.Billing.Currency, rates)
		if err != nil {
			return err
		}
		fxStore.Replace(table.Ledger, table.Rates)
		return nil
	}
	if err := reloadFX(ctx); err != nil {
		log.Error("loading the fx table failed", "err", err)
		return 1
	}
	if codes := fxStore.Codes(); len(codes) > 1 {
		log.Info("fx table ready", "ledger", fxStore.Ledger(), "currencies", codes)
	}

	// Rule sets declared in the configuration file never pass a write API, so a
	// currency without a rate is named here: such a model is recorded with zero
	// cost and charge rather than converted with a guessed rate.
	fxTable := fxStore.Snapshot()
	warnMissingRate := func(where, raw string) {
		code := pricing.DeclaredCurrency(raw)
		if code == "" || code == fxTable.Ledger {
			return
		}
		if _, ok := fxTable.RateOf(code); !ok {
			log.Warn("rule set declares a currency with no exchange rate; add it to billing.fx_rates",
				"where", where, "currency", code)
		}
	}
	for _, provider := range cfg.Bootstrap.Providers {
		for _, model := range provider.Models {
			warnMissingRate("bootstrap.providers."+provider.Name+"."+model.Public, model.PricingRules)
		}
	}
	for _, model := range cfg.Bootstrap.Models {
		warnMissingRate("bootstrap.models."+model.PublicName, model.SalePricing)
	}

	// Audit batching: the request path hands its two audit rows (stored response +
	// request log) to a background flusher instead of taking the single writer
	// connection twice per request. The transport keeps the retry policy, so the store
	// reports a row it could not write back to the server that counts and retries it.
	var auditWriter *store.LogWriter
	if cfg.Recording.BatchingEnabled() {
		auditWriter = store.NewLogWriter(db, store.LogWriterConfig{
			FlushInterval: time.Duration(cfg.Recording.BatchFlushMS) * time.Millisecond,
			MaxBatch:      cfg.Recording.BatchMaxRows,
			MaxBytes:      cfg.Recording.BatchMaxBytes,
			QueueRows:     cfg.Recording.BatchQueueRows,
			QueueBytes:    cfg.Recording.BatchQueueBytes,
		}, log, nil)
		log.Info("audit writes batched in the background",
			"flush_ms", cfg.Recording.BatchFlushMS,
			"max_rows", cfg.Recording.BatchMaxRows,
			"max_bytes", cfg.Recording.BatchMaxBytes,
			"queue_rows", cfg.Recording.BatchQueueRows,
			"queue_bytes", cfg.Recording.BatchQueueBytes)
	}

	api := httpapi.New(httpapi.Deps{
		Config:           cfg,
		FX:               fxStore,
		ReloadFX:         reloadFX,
		Registry:         reg,
		Router:           router,
		Dispatcher:       dispatcher,
		Verifier:         verifier,
		Limiter:          limiter,
		Meter:            meter,
		Records:          db,
		LogRecorder:      auditWriter,
		LogJanitor:       logJanitor,
		DimensionRollups: db,
		MCP:              mcpService,
		MCPTokens:        db,
		Hooks:            hookDispatcher,
		Admin:            adminAuth,
		AdminStore:       db,
		// The console chat persists conversations, skills and preview payloads in the same
		// database; the transport builds its service and preview-ticket signer from here.
		ChatStore: db,
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
		PortalUsers:    db,
		Log:            log,
		Version:        version,
		Revision:       revision,
	})

	if auditWriter != nil {
		// The transport owns the skeleton fallback and its counters; the store owns the
		// queue. Hand the store a way to report a row it gave up on.
		api.SetRecordingFailureHandler(auditWriter.SetFailureHandler)
	}

	if cfg.Chat.Enabled {
		// A turn that was running when the process died may have executed write tools
		// already. Marking it interrupted (and its pending tool calls unknown) is what
		// keeps a restart from silently replaying them.
		if n, err := api.RecoverChatTurns(ctx); err != nil {
			log.Warn("recovering interrupted chat turns failed", "err", err)
		} else if n > 0 {
			log.Info("marked interrupted chat turns", "turns", n)
		}
	}

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
	// Drain the audit queue after the server has stopped accepting requests and before
	// the store closes: everything accepted is written, nothing is left in memory.
	if auditWriter != nil {
		if err := auditWriter.Close(shutdownCtx); err != nil {
			log.Error("audit write queue could not be drained", "err", err)
		}
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

// Command aigw is the AI gateway server.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/logx"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/routing"
	"github.com/winger/ai-gateway/internal/store"
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
	_ = router // wired into the HTTP layer in M5

	if cfg.CredentialsKey == "" {
		log.Warn("credentials_key is empty: provider credentials cannot be encrypted at rest")
	}

	// Remaining wiring: M4 key auth + rate limits, M5 HTTP API, M6 MCP server,
	// M7 hooks/recording, M8 admin API, M11+ billing, M16 backups.
	log.Info("aigw ready",
		"store", "sqlite",
		"registry", snap.String(),
		"next", "M4 api keys + rate limiting (see docs/TODO.md)",
	)
	return 0
}

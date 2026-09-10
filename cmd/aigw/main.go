// Command aigw is the AI gateway server.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/logx"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "print version information and exit")
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	if *showVersion {
		fmt.Println("aigw", version, "(commit "+commit+",", "built "+date+")")
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aigw: configuration error:", err)
		os.Exit(2)
	}

	log := logx.New(cfg.Log)
	log.Info("aigw starting",
		"version", version,
		"commit", commit,
		"config", *configPath,
		"listen", cfg.Server.Listen,
		"database", cfg.Database.Path,
		"plugins_dir", cfg.Plugins.Dir,
		"backup_enabled", cfg.Backup.Enabled,
	)

	// M1+ wires the store, registry, routing, billing, HTTP server and MCP server here.
	log.Info("scaffold ready: datapath modules are pending (see docs/TODO.md)")
}

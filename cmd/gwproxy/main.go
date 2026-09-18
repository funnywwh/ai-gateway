// Command gwproxy is the single-domain path-prefix entry point for this project:
// one listener that fronts aigw, the dshgw portal and every tenant's dsh UI.
//
// It exists because the deployment outgrew "one port per tenant": a public site
// wants one domain, one TLS certificate and one firewall rule, with the services
// separated by URL prefix. Everything it does is routing, header hygiene and TLS
// termination — tenant identity, sessions and the worker handshake stay in dshgw,
// and aigw keeps serving its own base path.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/frontproxy"
)

var (
	version  = "dev"
	revision = "none"
	date     = "unknown"
)

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "/etc/dshgw/frontproxy.yaml", "path to the proxy configuration")
	showVersion := flag.Bool("version", false, "print version information")
	flag.Parse()
	if *showVersion {
		fmt.Printf("gwproxy %s (revision %s, built %s)\n", version, revision, date)
		return 0
	}
	cfg, err := frontproxy.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gwproxy:", err)
		return 2
	}
	level := slog.LevelInfo
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	proxy, err := frontproxy.New(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gwproxy:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go proxy.Run(ctx.Done())

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gwproxy:", err)
		return 1
	}
	scheme := "http"
	if cfg.TLS.Certificate != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLS.Certificate, cfg.TLS.CertificateKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gwproxy:", err)
			return 1
		}
		listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		})
		scheme = "https"
	}
	server := &http.Server{
		Handler:           proxy.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	aigw, portal, tenants := cfg.NormalizedPrefixes()
	log.Info("front proxy listening",
		"addr", cfg.Listen, "scheme", scheme, "host", cfg.PublicHost,
		"aigw", scheme+"://"+cfg.PublicHost+aigw, "portal", scheme+"://"+cfg.PublicHost+portal,
		"tenants", scheme+"://"+cfg.PublicHost+tenants+"/<tenant>/")

	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("front proxy failed", "err", err)
			return 1
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		log.Warn("graceful shutdown incomplete", "err", err)
	}
	return 0
}

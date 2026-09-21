package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/webaccess"
)

// buildWebAccess turns the chat.web_access block into the console's internet access client, or
// nil when the feature is off.
//
// It is built here, in the composition root, for the same reason the Feishu identity client is:
// everything that can be proven wrong is proven at startup, so a typo names the setting instead
// of surfacing later as "the model could not find anything". The client also carries the SSRF
// guard's configuration, and a guard that silently fell back to permissive behaviour would be
// the one failure this feature must not have.
func buildWebAccess(cfg *config.Config, log *slog.Logger) (*webaccess.Client, error) {
	web := cfg.Chat.WebAccess
	if !web.Enabled {
		return nil, nil
	}
	client, err := webaccess.New(webaccess.Config{
		Provider:          web.Provider,
		BaseURL:           web.BaseURL,
		APIKey:            web.APIKey,
		Proxy:             web.Proxy,
		Timeout:           time.Duration(web.TimeoutS) * time.Second,
		MaxResults:        web.MaxResults,
		FetchMaxBytes:     web.FetchMaxBytes,
		FetchMaxTextBytes: web.FetchMaxTextBytes,
		AllowPrivateHosts: web.AllowPrivateHosts,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("chat.web_access is unusable: %w", err)
	}
	// The operator needs to see which backend is live and whether the SSRF guard is off; the
	// API key is deliberately not logged, not even partially.
	log.Info("console web access enabled",
		"provider", client.Provider(),
		"base_url", web.BaseURL,
		"max_results", web.MaxResults,
		"max_calls_per_turn", web.MaxCallsPerTurn,
		"allow_private_hosts", web.AllowPrivateHosts)
	if web.AllowPrivateHosts {
		log.Warn("chat.web_access.allow_private_hosts is on: the console may fetch private, loopback and cloud-metadata addresses")
	}
	return client, nil
}

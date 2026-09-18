package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/feishu"
	"github.com/winger/ai-gateway/internal/httpapi"
)

// buildFeishuDeps turns the configuration block into the identity integration, or nil when
// the feature is off. Everything that can be proven wrong is proven here, at startup, so a
// typo is an error naming the setting instead of a broken consent screen later.
func buildFeishuDeps(cfg *config.Config, log *slog.Logger) (*httpapi.FeishuDeps, error) {
	if !cfg.Feishu.Enabled {
		return nil, nil
	}
	basePath := strings.TrimRight(cfg.Server.NormalizedBasePath(), "/")
	loginPath := basePath + "/feishu/login"
	callbackPath := basePath + "/feishu/callback"

	// The configured callback must be the path this server actually serves, including the
	// mount prefix: a mismatch would send users to a Feishu error page after they consented,
	// and the only way to notice would be to try it.
	parsed, err := url.Parse(cfg.Feishu.CallbackURL)
	if err != nil {
		return nil, fmt.Errorf("feishu.callback_url is not a URL: %w", err)
	}
	if parsed.Path != callbackPath {
		return nil, fmt.Errorf("feishu.callback_url path %q does not match the path this server serves (%q); fix the URL or server.base_path", parsed.Path, callbackPath)
	}

	stateKey, err := feishuSecret(cfg, cfg.Feishu.StateSecret, "feishu-state")
	if err != nil {
		return nil, err
	}
	states, err := feishu.NewStateCodec(stateKey, time.Duration(cfg.Feishu.StateTTLS)*time.Second)
	if err != nil {
		return nil, err
	}
	deps := &httpapi.FeishuDeps{
		Client:       feishu.New(cfg.Feishu),
		States:       states,
		RedirectURI:  cfg.Feishu.CallbackURL,
		LoginPath:    loginPath,
		CallbackPath: callbackPath,
		DSHLogin:     cfg.Feishu.DSHLogin,
		PortalURL:    cfg.DSHGWPortalURL(),
		LoginURL:     cfg.FeishuLoginURL(),
	}
	if cfg.Feishu.DSHLogin {
		ticketKey, err := feishuSecret(cfg, cfg.Feishu.TicketSecret, "feishu-ticket")
		if err != nil {
			return nil, err
		}
		tickets, err := feishu.NewTicketCodec(ticketKey, time.Duration(cfg.Feishu.TicketTTLS)*time.Second)
		if err != nil {
			return nil, err
		}
		deps.Tickets = tickets
	}
	if log != nil {
		log.Info("feishu identity enabled",
			"app_id", cfg.Feishu.AppID,
			"callback", cfg.Feishu.CallbackURL,
			"dsh_login", cfg.Feishu.DSHLogin,
			"portal", deps.PortalURL)
	}
	return deps, nil
}

// feishuSecret resolves one signing secret: an explicit value wins, otherwise the
// deployment's credentials key is stretched into a purpose-bound key. Two purposes never
// share a key, so one leaked value cannot sign the other's payloads.
func feishuSecret(cfg *config.Config, explicit, purpose string) ([]byte, error) {
	if value := strings.TrimSpace(explicit); value != "" {
		return []byte(value), nil
	}
	if value := strings.TrimSpace(cfg.CredentialsKey); value != "" {
		return feishu.DeriveSecret(value, purpose), nil
	}
	return nil, errors.New("feishu is enabled but no signing key is available: set credentials_key")
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// keyValidator is the part of the aigw client the refresh needs. It is an
// interface so the policy below is testable without an HTTP server.
type keyValidator interface {
	ValidateKey(ctx context.Context, key string) ([]string, error)
}

// modelRefreshHook builds the function dshgw calls before starting a tenant's
// worker: dsh is configured from settings.yaml, and a tenant whose key gained or
// lost models in aigw must see that when dsh starts — not whenever somebody
// remembers to run `dshgw sync-models`.
//
// Failure policy, decided here rather than in the lifecycle layer:
//
//   - aigw says the key is invalid (401) or forbidden (403): refuse to start. The
//     tenant could not call a single model, so starting it would only present a
//     broken UI and hide the real problem.
//   - aigw is unreachable, times out, or answers 5xx: warn and start with the
//     model list already in settings.yaml. Availability first: a transient blip
//     in aigw must not stop every tenant from coming up.
func modelRefreshHook(cfg *config.Config, validator keyValidator, manager *tenancy.Manager, log *slog.Logger) func(context.Context, registry.Tenant) error {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, t registry.Tenant) error {
		key, err := (proxy.FileKeySource{Root: cfg.Deploy.TenantConfigRoot}).Key(t.Name)
		if err != nil {
			// Without a stored key there is nothing to ask aigw about. The worker
			// keeps whatever settings it has; a deployment that keeps keys
			// elsewhere is not blocked by this.
			log.Warn("model refresh skipped: no stored tenant key", "tenant", t.Name, "err", err)
			return nil
		}
		models, err := validator.ValidateKey(ctx, key)
		if err != nil {
			if errors.Is(err, aigw.ErrInvalidKey) {
				return fmt.Errorf("aigw rejected the tenant key (401); not starting %s with a key that cannot call models: %w", t.Name, err)
			}
			var status *aigw.StatusError
			if errors.As(err, &status) && status.Status == http.StatusForbidden {
				return fmt.Errorf("aigw forbids this account's models (403); not starting %s: %w", t.Name, err)
			}
			log.Warn("model refresh skipped: aigw unavailable; starting with the current model list", "tenant", t.Name, "err", err)
			return nil
		}
		if err := manager.SyncModels(t, models); err != nil {
			return fmt.Errorf("applying the refreshed model list for %s: %w", t.Name, err)
		}
		log.Info("tenant models refreshed before worker start", "tenant", t.Name, "models", len(models))
		return nil
	}
}

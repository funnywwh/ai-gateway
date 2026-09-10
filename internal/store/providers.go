package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const providerCols = `id, name, kind, display_name, config_json, config_version, credentials_enc,
	state_dir, meta_json, discovered_json, health_json, last_error, enabled, priority, weight,
	max_inflight, timeout_overrides, degradation, cooldown_until, draining, created_at, updated_at`

func scanProvider(row rowScanner) (*domain.Provider, error) {
	var (
		p                       domain.Provider
		enabled, draining       int
		cooldownUntil           sql.NullInt64
		createdAt, updatedAt    int64
		creds                   []byte
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.DisplayName, &p.ConfigJSON, &p.ConfigVersion,
		&creds, &p.StateDir, &p.MetaJSON, &p.DiscoveredJSON, &p.HealthJSON, &p.LastError,
		&enabled, &p.Priority, &p.Weight, &p.MaxInflight, &p.TimeoutOverrides, &p.Degradation,
		&cooldownUntil, &draining, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.CredentialsEnc = creds
	p.Enabled = enabled != 0
	p.Draining = draining != 0
	p.CooldownUntil = timePtrFromNull(cooldownUntil)
	p.CreatedAt = timeFromUnix(createdAt)
	p.UpdatedAt = timeFromUnix(updatedAt)
	return &p, nil
}

// ListProviders returns all provider instances ordered by priority then name.
func (db *DB) ListProviders(ctx context.Context) ([]*domain.Provider, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+providerCols+" FROM providers ORDER BY priority, name")
	if err != nil {
		return nil, fmt.Errorf("store: list providers: %w", err)
	}
	defer rows.Close()

	out := []*domain.Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan provider: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate providers: %w", err)
	}
	return out, nil
}

// GetProvider loads one provider by id.
func (db *DB) GetProvider(ctx context.Context, id int64) (*domain.Provider, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+providerCols+" FROM providers WHERE id = ?", id)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("provider %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get provider %d: %w", id, err)
	}
	return p, nil
}

// GetProviderByName loads one provider by unique name.
func (db *DB) GetProviderByName(ctx context.Context, name string) (*domain.Provider, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+providerCols+" FROM providers WHERE name = ?", name)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("provider " + name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get provider %q: %w", name, err)
	}
	return p, nil
}

// UpsertProvider inserts or updates a provider (matched by unique name).
// Credentials are passed already encrypted; plaintext never reaches the store.
func (db *DB) UpsertProvider(ctx context.Context, p *domain.Provider) (int64, error) {
	if p == nil || p.Name == "" || p.Kind == "" {
		return 0, domain.ErrInvalidRequest("provider name and kind are required")
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.ConfigVersion == 0 {
		p.ConfigVersion = 1
	}
	if p.Priority == 0 {
		p.Priority = 100
	}
	if p.Weight == 0 {
		p.Weight = 100
	}

	if _, err := db.write.ExecContext(ctx, `
INSERT INTO providers(name, kind, display_name, config_json, config_version, credentials_enc,
  state_dir, meta_json, discovered_json, health_json, last_error, enabled, priority, weight,
  max_inflight, timeout_overrides, degradation, cooldown_until, draining, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  kind = excluded.kind,
  display_name = excluded.display_name,
  config_json = excluded.config_json,
  config_version = excluded.config_version,
  credentials_enc = excluded.credentials_enc,
  state_dir = excluded.state_dir,
  meta_json = excluded.meta_json,
  discovered_json = excluded.discovered_json,
  health_json = excluded.health_json,
  last_error = excluded.last_error,
  enabled = excluded.enabled,
  priority = excluded.priority,
  weight = excluded.weight,
  max_inflight = excluded.max_inflight,
  timeout_overrides = excluded.timeout_overrides,
  degradation = excluded.degradation,
  cooldown_until = excluded.cooldown_until,
  draining = excluded.draining,
  updated_at = excluded.updated_at`,
		p.Name, p.Kind, p.DisplayName, p.ConfigJSON, p.ConfigVersion, p.CredentialsEnc,
		p.StateDir, p.MetaJSON, p.DiscoveredJSON, p.HealthJSON, p.LastError, boolInt(p.Enabled),
		p.Priority, p.Weight, p.MaxInflight, p.TimeoutOverrides, p.Degradation,
		unixPtr(p.CooldownUntil), boolInt(p.Draining), unix(p.CreatedAt), unix(p.UpdatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert provider %q: %w", p.Name, err)
	}

	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM providers WHERE name = ?", p.Name).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve provider id %q: %w", p.Name, err)
	}
	p.ID = id
	return id, nil
}

// SetProviderFlags updates the gateway-owned runtime flags (no restart semantics here).
func (db *DB) SetProviderFlags(ctx context.Context, id int64, enabled, draining bool, priority, weight int) error {
	_, err := db.write.ExecContext(ctx, `
UPDATE providers SET enabled = ?, draining = ?, priority = ?, weight = ?, updated_at = ?
WHERE id = ?`, boolInt(enabled), boolInt(draining), priority, weight, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: set provider %d flags: %w", id, err)
	}
	return nil
}

// SetProviderDiscovered stores handshake results (protocol/version/capabilities/actions/schema digest).
func (db *DB) SetProviderDiscovered(ctx context.Context, id int64, discoveredJSON, healthJSON, lastError string) error {
	_, err := db.write.ExecContext(ctx, `
UPDATE providers SET discovered_json = ?, health_json = ?, last_error = ?, updated_at = ?
WHERE id = ?`, discoveredJSON, healthJSON, lastError, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: set provider %d discovered: %w", id, err)
	}
	return nil
}

// DeleteProvider removes a provider (provider_models and routes cascade).
func (db *DB) DeleteProvider(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM providers WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete provider %d: %w", id, err)
	}
	return nil
}

const providerModelCols = `id, provider_id, public_model, upstream_model, enabled, priority, weight,
	context_window, max_output_tokens, pricing_rules_json, capabilities_json, capabilities_override,
	source, updated_at`

func scanProviderModel(row rowScanner) (*domain.ProviderModel, error) {
	var (
		pm           domain.ProviderModel
		enabled      int
		updatedAt    int64
	)
	if err := row.Scan(&pm.ID, &pm.ProviderID, &pm.PublicModel, &pm.UpstreamModel, &enabled,
		&pm.Priority, &pm.Weight, &pm.ContextWindow, &pm.MaxOutputTokens, &pm.PricingRulesJSON,
		&pm.CapabilitiesJSON, &pm.CapabilitiesOverride, &pm.Source, &updatedAt); err != nil {
		return nil, err
	}
	pm.Enabled = enabled != 0
	pm.UpdatedAt = timeFromUnix(updatedAt)
	return &pm, nil
}

// ListProviderModels lists every provider-model mapping (providerID <= 0 means all).
func (db *DB) ListProviderModels(ctx context.Context, providerID int64) ([]*domain.ProviderModel, error) {
	query := "SELECT " + providerModelCols + " FROM provider_models"
	args := []any{}
	if providerID > 0 {
		query += " WHERE provider_id = ?"
		args = append(args, providerID)
	}
	query += " ORDER BY provider_id, public_model"

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list provider models: %w", err)
	}
	defer rows.Close()

	out := []*domain.ProviderModel{}
	for rows.Next() {
		pm, err := scanProviderModel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan provider model: %w", err)
		}
		out = append(out, pm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate provider models: %w", err)
	}
	return out, nil
}

// UpsertProviderModel inserts or updates a provider-model mapping.
func (db *DB) UpsertProviderModel(ctx context.Context, pm *domain.ProviderModel) (int64, error) {
	if pm == nil || pm.ProviderID == 0 || pm.PublicModel == "" {
		return 0, domain.ErrInvalidRequest("provider model requires provider_id and public_model")
	}
	pm.UpdatedAt = time.Now().UTC()
	if pm.Source == "" {
		pm.Source = "manual"
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO provider_models(provider_id, public_model, upstream_model, enabled, priority, weight,
  context_window, max_output_tokens, pricing_rules_json, capabilities_json, capabilities_override,
  source, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(provider_id, public_model) DO UPDATE SET
  upstream_model = excluded.upstream_model,
  enabled = excluded.enabled,
  priority = excluded.priority,
  weight = excluded.weight,
  context_window = excluded.context_window,
  max_output_tokens = excluded.max_output_tokens,
  pricing_rules_json = excluded.pricing_rules_json,
  capabilities_json = excluded.capabilities_json,
  capabilities_override = excluded.capabilities_override,
  source = excluded.source,
  updated_at = excluded.updated_at`,
		pm.ProviderID, pm.PublicModel, pm.UpstreamModel, boolInt(pm.Enabled), pm.Priority, pm.Weight,
		pm.ContextWindow, pm.MaxOutputTokens, pm.PricingRulesJSON, pm.CapabilitiesJSON,
		pm.CapabilitiesOverride, pm.Source, unix(pm.UpdatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert provider model %d/%s: %w", pm.ProviderID, pm.PublicModel, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx,
		"SELECT id FROM provider_models WHERE provider_id = ? AND public_model = ?",
		pm.ProviderID, pm.PublicModel).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve provider model id: %w", err)
	}
	pm.ID = id
	return id, nil
}

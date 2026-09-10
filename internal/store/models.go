package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const modelCols = `id, public_name, display_name, aliases_json, enabled, sale_pricing_json,
	policy_json, created_at, updated_at`

func scanModel(row rowScanner) (*domain.Model, error) {
	var (
		m                    domain.Model
		enabled              int
		createdAt, updatedAt int64
	)
	if err := row.Scan(&m.ID, &m.PublicName, &m.DisplayName, &m.AliasesJSON, &enabled,
		&m.SalePricingJSON, &m.PolicyJSON, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	m.Enabled = enabled != 0
	m.CreatedAt = timeFromUnix(createdAt)
	m.UpdatedAt = timeFromUnix(updatedAt)
	return &m, nil
}

// ListModels returns every canonical model.
func (db *DB) ListModels(ctx context.Context) ([]*domain.Model, error) {
	rows, err := db.read.QueryContext(ctx, "SELECT "+modelCols+" FROM models ORDER BY public_name")
	if err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}
	defer rows.Close()

	out := []*domain.Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan model: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate models: %w", err)
	}
	return out, nil
}

// GetModelByName loads one canonical model by public name.
func (db *DB) GetModelByName(ctx context.Context, name string) (*domain.Model, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+modelCols+" FROM models WHERE public_name = ?", name)
	m, err := scanModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrModelNotFound(name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get model %q: %w", name, err)
	}
	return m, nil
}

// UpsertModel inserts or updates a canonical model (matched by public_name).
func (db *DB) UpsertModel(ctx context.Context, m *domain.Model) (int64, error) {
	if m == nil || m.PublicName == "" {
		return 0, domain.ErrInvalidRequest("model public_name is required")
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now

	if _, err := db.write.ExecContext(ctx, `
INSERT INTO models(public_name, display_name, aliases_json, enabled, sale_pricing_json, policy_json, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(public_name) DO UPDATE SET
  display_name = excluded.display_name,
  aliases_json = excluded.aliases_json,
  enabled = excluded.enabled,
  sale_pricing_json = excluded.sale_pricing_json,
  policy_json = excluded.policy_json,
  updated_at = excluded.updated_at`,
		m.PublicName, m.DisplayName, m.AliasesJSON, boolInt(m.Enabled), m.SalePricingJSON,
		m.PolicyJSON, unix(m.CreatedAt), unix(m.UpdatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert model %q: %w", m.PublicName, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM models WHERE public_name = ?", m.PublicName).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve model id %q: %w", m.PublicName, err)
	}
	m.ID = id
	return id, nil
}

const mappingCols = `id, kind, pattern, target_model, target_provider_id, target_upstream_model,
	priority, enabled, note, created_at`

func scanMapping(row rowScanner) (*domain.ModelMapping, error) {
	var (
		m         domain.ModelMapping
		enabled   int
		createdAt int64
	)
	if err := row.Scan(&m.ID, &m.Kind, &m.Pattern, &m.TargetModel, &m.TargetProviderID,
		&m.TargetUpstreamModel, &m.Priority, &enabled, &m.Note, &createdAt); err != nil {
		return nil, err
	}
	m.Enabled = enabled != 0
	m.CreatedAt = timeFromUnix(createdAt)
	return &m, nil
}

// ListModelMappings returns mapping rules ordered by evaluation priority.
func (db *DB) ListModelMappings(ctx context.Context) ([]*domain.ModelMapping, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+mappingCols+" FROM model_mappings ORDER BY priority, id")
	if err != nil {
		return nil, fmt.Errorf("store: list model mappings: %w", err)
	}
	defer rows.Close()

	out := []*domain.ModelMapping{}
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan model mapping: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate model mappings: %w", err)
	}
	return out, nil
}

// UpsertModelMapping inserts or updates a mapping rule (matched by kind+pattern).
func (db *DB) UpsertModelMapping(ctx context.Context, m *domain.ModelMapping) (int64, error) {
	if m == nil || m.Kind == "" || m.Pattern == "" {
		return 0, domain.ErrInvalidRequest("mapping kind and pattern are required")
	}
	switch m.Kind {
	case "exact", "prefix", "glob", "regex":
	default:
		return 0, domain.ErrInvalidRequest("mapping kind must be exact|prefix|glob|regex")
	}
	if m.TargetModel == "" && m.TargetProviderID == 0 {
		return 0, domain.ErrInvalidRequest("mapping requires target_model or target_provider_id")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}

	var existing int64
	err := db.write.QueryRowContext(ctx,
		"SELECT id FROM model_mappings WHERE kind = ? AND pattern = ?", m.Kind, m.Pattern).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := db.write.ExecContext(ctx, `
INSERT INTO model_mappings(kind, pattern, target_model, target_provider_id, target_upstream_model,
  priority, enabled, note, created_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
			m.Kind, m.Pattern, m.TargetModel, m.TargetProviderID, m.TargetUpstreamModel,
			m.Priority, boolInt(m.Enabled), m.Note, unix(m.CreatedAt))
		if err != nil {
			return 0, fmt.Errorf("store: insert model mapping %s/%s: %w", m.Kind, m.Pattern, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("store: model mapping id: %w", err)
		}
		m.ID = id
		return id, nil
	case err != nil:
		return 0, fmt.Errorf("store: lookup model mapping: %w", err)
	default:
		if _, err := db.write.ExecContext(ctx, `
UPDATE model_mappings SET target_model = ?, target_provider_id = ?, target_upstream_model = ?,
  priority = ?, enabled = ?, note = ?
WHERE id = ?`,
			m.TargetModel, m.TargetProviderID, m.TargetUpstreamModel, m.Priority,
			boolInt(m.Enabled), m.Note, existing); err != nil {
			return 0, fmt.Errorf("store: update model mapping %d: %w", existing, err)
		}
		m.ID = existing
		return existing, nil
	}
}

// DeleteModelMapping removes a mapping rule.
func (db *DB) DeleteModelMapping(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM model_mappings WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete model mapping %d: %w", id, err)
	}
	return nil
}

const routeCols = `id, model_id, provider_id, upstream_model, priority, weight, enabled,
	policy_json, cooldown_until`

func scanRoute(row rowScanner) (*domain.Route, error) {
	var (
		r             domain.Route
		enabled       int
		cooldownUntil sql.NullInt64
	)
	if err := row.Scan(&r.ID, &r.ModelID, &r.ProviderID, &r.UpstreamModel, &r.Priority,
		&r.Weight, &enabled, &r.PolicyJSON, &cooldownUntil); err != nil {
		return nil, err
	}
	r.Enabled = enabled != 0
	r.CooldownUntil = timePtrFromNull(cooldownUntil)
	return &r, nil
}

// ListRoutes returns all model→provider routes.
func (db *DB) ListRoutes(ctx context.Context) ([]*domain.Route, error) {
	rows, err := db.read.QueryContext(ctx,
		"SELECT "+routeCols+" FROM routes ORDER BY model_id, priority, id")
	if err != nil {
		return nil, fmt.Errorf("store: list routes: %w", err)
	}
	defer rows.Close()

	out := []*domain.Route{}
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan route: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate routes: %w", err)
	}
	return out, nil
}

// UpsertRoute inserts or updates a route (unique per model+provider).
func (db *DB) UpsertRoute(ctx context.Context, r *domain.Route) (int64, error) {
	if r == nil || r.ModelID == 0 || r.ProviderID == 0 {
		return 0, domain.ErrInvalidRequest("route requires model_id and provider_id")
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO routes(model_id, provider_id, upstream_model, priority, weight, enabled, policy_json, cooldown_until)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(model_id, provider_id) DO UPDATE SET
  upstream_model = excluded.upstream_model,
  priority = excluded.priority,
  weight = excluded.weight,
  enabled = excluded.enabled,
  policy_json = excluded.policy_json,
  cooldown_until = excluded.cooldown_until`,
		r.ModelID, r.ProviderID, r.UpstreamModel, r.Priority, r.Weight, boolInt(r.Enabled),
		r.PolicyJSON, unixPtr(r.CooldownUntil)); err != nil {
		return 0, fmt.Errorf("store: upsert route %d/%d: %w", r.ModelID, r.ProviderID, err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx,
		"SELECT id FROM routes WHERE model_id = ? AND provider_id = ?", r.ModelID, r.ProviderID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve route id: %w", err)
	}
	r.ID = id
	return id, nil
}

// SetRouteCooldown persists a cooldown deadline (upstream quota exhaustion etc.).
func (db *DB) SetRouteCooldown(ctx context.Context, id int64, until *time.Time) error {
	if _, err := db.write.ExecContext(ctx,
		"UPDATE routes SET cooldown_until = ? WHERE id = ?", unixPtr(until), id); err != nil {
		return fmt.Errorf("store: set route %d cooldown: %w", id, err)
	}
	return nil
}

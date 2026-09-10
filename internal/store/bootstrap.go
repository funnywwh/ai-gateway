package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

// BootstrapResult summarises the seeding step.
type BootstrapResult struct {
	Mode                string
	AccountsCreated     int
	AccountsUpdated     int
	APIKeysCreated      int
	APIKeysUpdated      int
	ProvidersCreated    int
	ProvidersUpdated    int
	ProviderModelsAdded int
	ModelsCreated       int
	RoutesAdded         int
	TagsAdded           int
}

// Bootstrap seeds accounts, API keys, providers, models, routes and tags from the
// configuration file.
//
// mode:
//   - "off"    : do nothing
//   - "upsert" : create only what is missing (default; never overwrites live data)
//   - "merge"  : also update existing rows from the configuration
//
// API keys are stored as SHA-256 hashes only; the plaintext from the file never persists.
// pluginStateDir is the base directory for per-provider plugin state.
func (db *DB) Bootstrap(ctx context.Context, cfg config.Bootstrap, pluginStateDir string) (*BootstrapResult, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = "upsert"
	}
	res := &BootstrapResult{Mode: mode}
	if mode == "off" {
		return res, nil
	}
	if mode != "upsert" && mode != "merge" {
		return nil, domain.ErrInvalidRequest("bootstrap.mode must be off|upsert|merge")
	}
	merge := mode == "merge"

	accountIDs, err := db.seedAccounts(ctx, cfg.Accounts, merge, res)
	if err != nil {
		return nil, err
	}
	if err := db.seedAPIKeys(ctx, cfg.APIKeys, accountIDs, merge, res); err != nil {
		return nil, err
	}
	providerIDs, err := db.seedProviders(ctx, cfg.Providers, pluginStateDir, merge, res)
	if err != nil {
		return nil, err
	}
	if err := db.seedModels(ctx, cfg.Models, merge, res); err != nil {
		return nil, err
	}
	if err := db.seedRoutes(ctx, cfg.Routes, providerIDs, merge, res); err != nil {
		return nil, err
	}
	if err := db.seedTags(ctx, cfg.Tags, merge, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (db *DB) seedAccounts(ctx context.Context, accounts []config.BootstrapAccount, merge bool, res *BootstrapResult) (map[string]int64, error) {
	ids := map[string]int64{}
	for _, acc := range accounts {
		if acc.Name == "" {
			continue
		}
		existing, err := db.GetAccountByName(ctx, acc.Name)
		switch {
		case err == nil:
			ids[acc.Name] = existing.ID
			if !merge {
				continue
			}
			existing.BillingMode = billingMode(acc.BillingMode)
			existing.CreditLimitMicros = usdToMicros(acc.CreditLimitUSD)
			if _, err := db.UpsertAccount(ctx, existing); err != nil {
				return nil, err
			}
			res.AccountsUpdated++
		case domain.IsNotFound(err):
			created := &domain.Account{
				Name:              acc.Name,
				BillingMode:       billingMode(acc.BillingMode),
				CreditLimitMicros: usdToMicros(acc.CreditLimitUSD),
			}
			id, err := db.UpsertAccount(ctx, created)
			if err != nil {
				return nil, err
			}
			ids[acc.Name] = id
			res.AccountsCreated++
		default:
			return nil, err
		}
	}
	return ids, nil
}

func (db *DB) seedAPIKeys(ctx context.Context, keys []config.BootstrapAPIKey, accountIDs map[string]int64, merge bool, res *BootstrapResult) error {
	for _, key := range keys {
		if key.Key == "" || key.Name == "" {
			continue
		}
		accountID, ok := accountIDs[key.Account]
		if !ok {
			acc, err := db.GetAccountByName(ctx, key.Account)
			if err != nil {
				return fmt.Errorf("bootstrap: api key %q references unknown account %q: %w", key.Name, key.Account, err)
			}
			accountID = acc.ID
		}
		tags, err := tagsJSON(key.Tags)
		if err != nil {
			return fmt.Errorf("bootstrap: api key %q tags: %w", key.Name, err)
		}
		candidate := &domain.APIKey{
			AccountID:       accountID,
			Name:            key.Name,
			KeyPrefix:       secret.Prefix(key.Key),
			KeyHash:         secret.Hash(key.Key),
			RecordInputMode: "inherit",
			Status:          "active",
			CreatedBy:       "bootstrap",
			TagsJSON:        tags,
		}
		existing, err := db.GetAPIKeyByPrefix(ctx, candidate.KeyPrefix)
		switch {
		case err == nil:
			if !merge {
				continue
			}
			candidate.ID = existing.ID
			candidate.CreatedAt = existing.CreatedAt
			if _, err := db.UpsertAPIKey(ctx, candidate); err != nil {
				return err
			}
			res.APIKeysUpdated++
		case domain.IsUnauthorized(err):
			if _, err := db.UpsertAPIKey(ctx, candidate); err != nil {
				return err
			}
			res.APIKeysCreated++
		default:
			return err
		}
	}
	return nil
}

func (db *DB) seedProviders(ctx context.Context, providers []config.BootstrapProvider, stateDirBase string, merge bool, res *BootstrapResult) (map[string]int64, error) {
	ids := map[string]int64{}
	for _, bp := range providers {
		if bp.Name == "" || bp.Kind == "" {
			continue
		}
		configJSON, err := json.Marshal(orEmptyMap(bp.Config))
		if err != nil {
			return nil, fmt.Errorf("bootstrap: provider %q config: %w", bp.Name, err)
		}
		stateDir := ""
		if stateDirBase != "" {
			stateDir = filepath.Join(stateDirBase, bp.Name)
		}

		existing, err := db.GetProviderByName(ctx, bp.Name)
		switch {
		case err == nil:
			ids[bp.Name] = existing.ID
			if merge {
				existing.Kind = bp.Kind
				existing.DisplayName = bp.DisplayName
				existing.Enabled = boolOr(bp.Enabled, existing.Enabled)
				existing.ConfigJSON = string(configJSON)
				if bp.Priority != 0 {
					existing.Priority = bp.Priority
				}
				if bp.Weight != 0 {
					existing.Weight = bp.Weight
				}
				if stateDir != "" {
					existing.StateDir = stateDir
				}
				if _, err := db.UpsertProvider(ctx, existing); err != nil {
					return nil, err
				}
				res.ProvidersUpdated++
			}
		case domain.IsNotFound(err):
			created := &domain.Provider{
				Name:        bp.Name,
				Kind:        bp.Kind,
				DisplayName: bp.DisplayName,
				Enabled:     boolOr(bp.Enabled, true),
				Priority:    defaultInt(bp.Priority, 100),
				Weight:      defaultInt(bp.Weight, 100),
				ConfigJSON:  string(configJSON),
				StateDir:    stateDir,
			}
			id, err := db.UpsertProvider(ctx, created)
			if err != nil {
				return nil, err
			}
			ids[bp.Name] = id
			res.ProvidersCreated++
		default:
			return nil, err
		}

		providerID := ids[bp.Name]
		for _, bm := range bp.Models {
			if bm.Public == "" {
				continue
			}
			if !merge {
				existingModels, err := db.ListProviderModels(ctx, providerID)
				if err != nil {
					return nil, err
				}
				if hasProviderModel(existingModels, bm.Public) {
					continue
				}
			}
			caps, err := json.Marshal(orEmptyBoolMap(bm.Capabilities))
			if err != nil {
				return nil, fmt.Errorf("bootstrap: provider %q model %q capabilities: %w", bp.Name, bm.Public, err)
			}
			upstream := bm.Upstream
			if upstream == "" {
				upstream = bm.Public
			}
			if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
				ProviderID:       providerID,
				PublicModel:      bm.Public,
				UpstreamModel:    upstream,
				Enabled:          boolOr(bm.Enabled, true),
				CapabilitiesJSON: string(caps),
				PricingRulesJSON: bm.PricingRules,
				MaxOutputTokens:  bm.MaxOutputTokens,
				ContextWindow:    bm.ContextWindow,
				Source:           "bootstrap",
			}); err != nil {
				return nil, err
			}
			res.ProviderModelsAdded++
		}
	}
	return ids, nil
}

func (db *DB) seedModels(ctx context.Context, models []config.BootstrapModel, merge bool, res *BootstrapResult) error {
	for _, bm := range models {
		if bm.PublicName == "" {
			continue
		}
		aliases, err := tagsJSON(bm.Aliases)
		if err != nil {
			return err
		}
		existing, getErr := db.GetModelByName(ctx, bm.PublicName)
		switch {
		case getErr == nil:
			if !merge {
				continue
			}
			existing.AliasesJSON = aliases
			existing.Enabled = boolOr(bm.Enabled, existing.Enabled)
			if bm.SalePricing != "" {
				existing.SalePricingJSON = bm.SalePricing
			}
			if _, err := db.UpsertModel(ctx, existing); err != nil {
				return err
			}
		case domain.IsNotFound(getErr):
			if _, err := db.UpsertModel(ctx, &domain.Model{
				PublicName:      bm.PublicName,
				AliasesJSON:     aliases,
				Enabled:         boolOr(bm.Enabled, true),
				SalePricingJSON: bm.SalePricing,
			}); err != nil {
				return err
			}
			res.ModelsCreated++
		default:
			return getErr
		}
	}
	return nil
}

func (db *DB) seedRoutes(ctx context.Context, routes []config.BootstrapRoute, providerIDs map[string]int64, merge bool, res *BootstrapResult) error {
	for _, br := range routes {
		if br.Model == "" || br.Provider == "" {
			continue
		}
		model, err := db.GetModelByName(ctx, br.Model)
		if err != nil {
			return fmt.Errorf("bootstrap: route references unknown model %q: %w", br.Model, err)
		}
		providerID, ok := providerIDs[br.Provider]
		if !ok {
			prov, err := db.GetProviderByName(ctx, br.Provider)
			if err != nil {
				return fmt.Errorf("bootstrap: route references unknown provider %q: %w", br.Provider, err)
			}
			providerID = prov.ID
		}
		if !merge {
			existingRoutes, err := db.ListRoutes(ctx)
			if err != nil {
				return err
			}
			if hasRoute(existingRoutes, model.ID, providerID) {
				continue
			}
		}
		if _, err := db.UpsertRoute(ctx, &domain.Route{
			ModelID:       model.ID,
			ProviderID:    providerID,
			UpstreamModel: br.UpstreamModel,
			Priority:      defaultInt(br.Priority, 100),
			Weight:        defaultInt(br.Weight, 100),
			Enabled:       boolOr(br.Enabled, true),
		}); err != nil {
			return err
		}
		res.RoutesAdded++
	}
	return nil
}

func (db *DB) seedTags(ctx context.Context, tags []config.BootstrapTag, merge bool, res *BootstrapResult) error {
	for _, bt := range tags {
		if bt.Name == "" {
			continue
		}
		grants, err := json.Marshal(map[string][]string{
			"models":    orEmptyStrings(bt.Models),
			"providers": orEmptyStrings(bt.Providers),
		})
		if err != nil {
			return err
		}
		existing, getErr := db.GetTagByName(ctx, bt.Name)
		switch {
		case getErr == nil:
			if !merge {
				continue
			}
			existing.GrantsJSON = string(grants)
			if bt.Priority != 0 {
				existing.Priority = bt.Priority
			}
			if _, err := db.UpsertTag(ctx, existing); err != nil {
				return err
			}
		case domain.IsNotFound(getErr):
			if _, err := db.UpsertTag(ctx, &domain.Tag{
				Name:       bt.Name,
				GrantsJSON: string(grants),
				Priority:   defaultInt(bt.Priority, 100),
			}); err != nil {
				return err
			}
			res.TagsAdded++
		default:
			return getErr
		}
	}
	return nil
}

func hasProviderModel(list []*domain.ProviderModel, public string) bool {
	for _, pm := range list {
		if pm.PublicModel == public {
			return true
		}
	}
	return false
}

func hasRoute(list []*domain.Route, modelID, providerID int64) bool {
	for _, r := range list {
		if r.ModelID == modelID && r.ProviderID == providerID {
			return true
		}
	}
	return false
}

// tagsJSON renders a string list as a JSON array ("" when empty).
func tagsJSON(tags []string) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmptyBoolMap(m map[string]bool) map[string]bool {
	if m == nil {
		return map[string]bool{}
	}
	return m
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func billingMode(s string) domain.BillingMode {
	if s == string(domain.BillingPrepaid) {
		return domain.BillingPrepaid
	}
	return domain.BillingPostpaid
}

func usdToMicros(usd float64) int64 {
	return int64(usd * 1_000_000)
}

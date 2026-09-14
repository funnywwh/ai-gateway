package httpapi

import (
	"context"
	"encoding/json"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/runtime"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// The management surface is split into one narrow port per resource family so a
// test double only has to implement what it exercises, and so httpapi never gains
// a compile-time dependency on the shape of the concrete store.

// AccountAdmin manages billing subjects (tenants).
type AccountAdmin interface {
	ListAccounts(ctx context.Context) ([]*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	GetAccountByName(ctx context.Context, name string) (*domain.Account, error)
	UpsertAccount(ctx context.Context, a *domain.Account) (int64, error)
	SetAccountStatus(ctx context.Context, id int64, status string) error
}

// ProviderAdmin manages upstream provider instances and their model mappings.
type ProviderAdmin interface {
	ListProviders(ctx context.Context) ([]*domain.Provider, error)
	GetProvider(ctx context.Context, id int64) (*domain.Provider, error)
	GetProviderByName(ctx context.Context, name string) (*domain.Provider, error)
	UpsertProvider(ctx context.Context, p *domain.Provider) (int64, error)
	SetProviderFlags(ctx context.Context, id int64, enabled, draining bool, priority, weight int) error
	SetProviderDiscovered(ctx context.Context, id int64, discoveredJSON, healthJSON, lastError string) error
	DeleteProvider(ctx context.Context, id int64) error
	ListProviderModels(ctx context.Context, providerID int64) ([]*domain.ProviderModel, error)
	UpsertProviderModel(ctx context.Context, pm *domain.ProviderModel) (int64, error)
	DeleteProviderModel(ctx context.Context, id int64) error
}

// ModelAdmin manages canonical models, name-resolution rules and routes.
type ModelAdmin interface {
	ListModels(ctx context.Context) ([]*domain.Model, error)
	GetModelByName(ctx context.Context, name string) (*domain.Model, error)
	UpsertModel(ctx context.Context, m *domain.Model) (int64, error)
	ListModelMappings(ctx context.Context) ([]*domain.ModelMapping, error)
	UpsertModelMapping(ctx context.Context, m *domain.ModelMapping) (int64, error)
	DeleteModelMapping(ctx context.Context, id int64) error
	ListRoutes(ctx context.Context) ([]*domain.Route, error)
	UpsertRoute(ctx context.Context, r *domain.Route) (int64, error)
	DeleteRoute(ctx context.Context, id int64) error
}

// TagAdmin manages grants/policy groups.
type TagAdmin interface {
	ListTags(ctx context.Context) ([]*domain.Tag, error)
	GetTagByID(ctx context.Context, id int64) (*domain.Tag, error)
	UpsertTag(ctx context.Context, t *domain.Tag) (int64, error)
	UpdateTag(ctx context.Context, t *domain.Tag) error
	DeleteTag(ctx context.Context, id int64) error
}

// HookAdmin manages outbound event sinks.
type HookAdmin interface {
	ListHooks(ctx context.Context) ([]*domain.Hook, error)
	UpsertHook(ctx context.Context, h *domain.Hook) (int64, error)
	DeleteHook(ctx context.Context, id int64) error
}

// MCPTokenAdmin manages MCP tokens (scope decides query-only vs administrative).
type MCPTokenAdmin interface {
	ListMCPTokens(ctx context.Context, accountID int64) ([]*domain.MCPToken, error)
	UpsertMCPToken(ctx context.Context, tok *domain.MCPToken) (int64, error)
	RevokeMCPToken(ctx context.Context, id int64) error
}

// SettingsAdmin reads and writes key/value settings.
type SettingsAdmin interface {
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, valueJSON string) error
}

// Sealer encrypts provider credentials. Only this port touches plaintext secrets,
// and it never hands them back: KeyNames exists purely to tell the UI which fields
// are configured.
type Sealer interface {
	Seal(providerID int64, plaintext []byte) ([]byte, error)
	KeyNames(providerID int64, ciphertext []byte) []string
	Ready() bool
}

// Prober exercises a provider out of band (health, model discovery, actions, logs).
type Prober interface {
	Probe(ctx context.Context, providerID int64, mode string) *runtime.ProbeResult
	Actions(ctx context.Context, providerID int64) ([]pluginapi.Action, error)
	RunAction(ctx context.Context, providerID int64, name string, in json.RawMessage) (json.RawMessage, error)
	Logs(ctx context.Context, providerID int64, tail int) ([]string, bool, error)
	Restart(ctx context.Context, providerID int64) error
}

// Compiled-in defaults used by validation.
var (
	validBillingModes = map[string]bool{"prepaid": true, "postpaid": true}
	validDegradations = map[string]bool{"none": true, "fail_fast": true, "best_effort": true}
	validHookTypes    = map[string]bool{"webhook": true, "jsonl": true}
)

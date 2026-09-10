// Package registry keeps an immutable snapshot of the routing-relevant configuration
// in memory. Hot paths read the snapshot via atomic.Pointer and never touch the database;
// admin writes call Reload to build a new snapshot and swap it in atomically.
package registry

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// Snapshot is an immutable view of the configuration used by the request hot path.
type Snapshot struct {
	LoadedAt       time.Time
	Accounts       []*domain.Account
	Providers      []*domain.Provider
	ProviderModels []*domain.ProviderModel
	Models         []*domain.Model
	Mappings       []*domain.ModelMapping
	Routes         []*domain.Route
	Tags           []*domain.Tag

	AccountByID    map[int64]*domain.Account
	AccountByName  map[string]*domain.Account
	ProviderByID   map[int64]*domain.Provider
	ProviderByName map[string]*domain.Provider
	ModelByName    map[string]*domain.Model
	TagByName      map[string]*domain.Tag
	routesByModel  map[int64][]*domain.Route
	pmByProvider   map[int64][]*domain.ProviderModel
}

// Registry owns the current snapshot.
type Registry struct {
	store domain.Store
	cur   atomic.Pointer[Snapshot]
}

// New builds a registry backed by store. Call Reload to populate the first snapshot.
func New(store domain.Store) *Registry {
	r := &Registry{store: store}
	r.cur.Store(&Snapshot{LoadedAt: time.Now().UTC()})
	return r
}

// NewStatic returns a registry pinned to one snapshot. It is used by tests and by
// the admin "simulate routing" preview, where no reloading is wanted.
func NewStatic(snap *Snapshot) *Registry {
	r := &Registry{}
	if snap == nil {
		snap = &Snapshot{LoadedAt: time.Now().UTC()}
	}
	r.cur.Store(snap)
	return r
}

// Snapshot returns the current immutable snapshot (never nil).
func (r *Registry) Snapshot() *Snapshot { return r.cur.Load() }

// Reload rebuilds the snapshot from the store and swaps it in atomically.
func (r *Registry) Reload(ctx context.Context) (*Snapshot, error) {
	accounts, err := r.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	providers, err := r.store.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	providerModels, err := r.store.ListProviderModels(ctx, 0)
	if err != nil {
		return nil, err
	}
	models, err := r.store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	mappings, err := r.store.ListModelMappings(ctx)
	if err != nil {
		return nil, err
	}
	routes, err := r.store.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := r.store.ListTags(ctx)
	if err != nil {
		return nil, err
	}

	snap := NewSnapshot(accounts, providers, providerModels, models, mappings, routes, tags)
	r.cur.Store(snap)
	return snap, nil
}

// NewSnapshot builds a fully indexed immutable snapshot from slices.
// It is used by Reload, by tests and by admin previews.
func NewSnapshot(
	accounts []*domain.Account,
	providers []*domain.Provider,
	providerModels []*domain.ProviderModel,
	models []*domain.Model,
	mappings []*domain.ModelMapping,
	routes []*domain.Route,
	tags []*domain.Tag,
) *Snapshot {
	snap := &Snapshot{
		LoadedAt:       time.Now().UTC(),
		Accounts:       accounts,
		Providers:      providers,
		ProviderModels: providerModels,
		Models:         models,
		Mappings:       mappings,
		Routes:         routes,
		Tags:           tags,
		AccountByID:    make(map[int64]*domain.Account, len(accounts)),
		AccountByName:  make(map[string]*domain.Account, len(accounts)),
		ProviderByID:   make(map[int64]*domain.Provider, len(providers)),
		ProviderByName: make(map[string]*domain.Provider, len(providers)),
		ModelByName:    make(map[string]*domain.Model, len(models)),
		TagByName:      make(map[string]*domain.Tag, len(tags)),
		routesByModel:  make(map[int64][]*domain.Route),
		pmByProvider:   make(map[int64][]*domain.ProviderModel),
	}
	for _, a := range accounts {
		snap.AccountByID[a.ID] = a
		snap.AccountByName[a.Name] = a
	}
	for _, p := range providers {
		snap.ProviderByID[p.ID] = p
		snap.ProviderByName[p.Name] = p
	}
	for _, pm := range providerModels {
		snap.pmByProvider[pm.ProviderID] = append(snap.pmByProvider[pm.ProviderID], pm)
	}
	for _, m := range models {
		snap.ModelByName[m.PublicName] = m
	}
	for _, t := range tags {
		snap.TagByName[t.Name] = t
	}
	for _, rt := range routes {
		snap.routesByModel[rt.ModelID] = append(snap.routesByModel[rt.ModelID], rt)
	}
	// Mapping rules are evaluated in priority order (then by id for determinism),
	// so the snapshot guarantees that ordering regardless of the caller's input order.
	sort.SliceStable(snap.Mappings, func(i, j int) bool {
		if snap.Mappings[i].Priority != snap.Mappings[j].Priority {
			return snap.Mappings[i].Priority < snap.Mappings[j].Priority
		}
		return snap.Mappings[i].ID < snap.Mappings[j].ID
	})
	return snap
}

// RoutesFor returns the routes of one canonical model.
func (s *Snapshot) RoutesFor(modelID int64) []*domain.Route {
	if s == nil {
		return nil
	}
	return s.routesByModel[modelID]
}

// ProviderModelsFor returns the model mappings of one provider.
func (s *Snapshot) ProviderModelsFor(providerID int64) []*domain.ProviderModel {
	if s == nil {
		return nil
	}
	return s.pmByProvider[providerID]
}

// ProviderModel finds the mapping of a provider for a public model name.
func (s *Snapshot) ProviderModel(providerID int64, publicModel string) *domain.ProviderModel {
	if s == nil {
		return nil
	}
	for _, pm := range s.pmByProvider[providerID] {
		if pm.PublicModel == publicModel {
			return pm
		}
	}
	return nil
}

// Ready reports whether the snapshot holds any usable provider.
func (s *Snapshot) Ready() bool {
	if s == nil {
		return false
	}
	for _, p := range s.Providers {
		if p.Enabled && !p.Draining {
			return true
		}
	}
	return false
}

// String renders a short diagnostic description.
func (s *Snapshot) String() string {
	if s == nil {
		return "snapshot(nil)"
	}
	return fmt.Sprintf("snapshot(models=%d providers=%d provider_models=%d routes=%d mappings=%d tags=%d accounts=%d)",
		len(s.Models), len(s.Providers), len(s.ProviderModels), len(s.Routes), len(s.Mappings), len(s.Tags), len(s.Accounts))
}

// Package registry keeps an immutable snapshot of the routing-relevant configuration
// in memory. Hot paths read the snapshot via atomic.Pointer and never touch the database;
// admin writes call Reload to build a new snapshot and swap it in atomically.
package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/orgtree"
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
	OrgNodes       []*domain.OrgNode

	AccountByID    map[int64]*domain.Account
	AccountByName  map[string]*domain.Account
	ProviderByID   map[int64]*domain.Provider
	ProviderByName map[string]*domain.Provider
	ModelByName    map[string]*domain.Model
	TagByName      map[string]*domain.Tag
	OrgNodeByID    map[int64]*domain.OrgNode
	routesByModel  map[int64][]*domain.Route
	pmByProvider   map[int64][]*domain.ProviderModel

	// accountTagRecords and accountTagNames are the account side of tag resolution, computed
	// once per snapshot instead of once per request. They are the reason an organization
	// membership costs nothing on the hot path: the ancestor walk happened here, at build
	// time, and the request only looks up a map entry.
	//
	// Keyed only by accounts that actually carry something (an organization membership or
	// their own tags), so a deployment that uses neither pays nothing.
	//
	// READ-ONLY once the snapshot is published: ResolveTagRecords may hand these slices
	// straight to callers (that is what makes the fast path allocation-free). Nothing may
	// mutate them, and every current caller only ranges over the result.
	accountTagRecords map[int64][]*domain.Tag
	accountTagNames   map[int64]map[string]struct{}
	// orgTagNames holds the same information as accountTagRecords but as names, for the
	// management surface that reports what an account inherits.
	orgTagNames map[int64][]string
	// orgIndex is the tree index the snapshot was built with; it answers ancestry questions
	// (paths, subtree membership) without touching the database.
	orgIndex *orgtree.Index
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
	orgNodes, err := r.store.ListOrgNodes(ctx)
	if err != nil {
		return nil, err
	}
	memberships, err := r.store.ListOrgMemberships(ctx)
	if err != nil {
		return nil, err
	}

	snap := Build(Input{
		Accounts: accounts, Providers: providers, ProviderModels: providerModels,
		Models: models, Mappings: mappings, Routes: routes, Tags: tags,
		OrgNodes: orgNodes, Memberships: memberships,
	})
	r.cur.Store(snap)
	return snap, nil
}

// Input is everything a snapshot is built from. It is a struct rather than a parameter list
// because the set grows (the organization structure was the last addition) and a caller that
// only cares about models should not have to spell out eight nils to say so.
type Input struct {
	Accounts       []*domain.Account
	Providers      []*domain.Provider
	ProviderModels []*domain.ProviderModel
	Models         []*domain.Model
	Mappings       []*domain.ModelMapping
	Routes         []*domain.Route
	Tags           []*domain.Tag
	// OrgNodes and Memberships describe the organization structure. Both may be nil, which is
	// what a deployment without an organization tree looks like.
	OrgNodes    []*domain.OrgNode
	Memberships []domain.OrgMembership
}

// Build constructs a fully indexed immutable snapshot, including the per-account tag
// resolution described on Snapshot.
//
// This is the entry point for anything that has organization data (Reload, and tests that
// exercise inheritance). NewSnapshot above is the older, narrower signature kept for the many
// callers that only have routing configuration.
func Build(in Input) *Snapshot {
	snap := index(in.Accounts, in.Providers, in.ProviderModels, in.Models,
		in.Mappings, in.Routes, in.Tags)
	snap.OrgNodes = in.OrgNodes
	snap.OrgNodeByID = make(map[int64]*domain.OrgNode, len(in.OrgNodes))
	for _, node := range in.OrgNodes {
		if node != nil && node.ID != 0 {
			snap.OrgNodeByID[node.ID] = node
		}
	}
	snap.orgIndex = orgtree.NewIndex(in.OrgNodes)
	snap.materializeAccountTags(in.Memberships)
	return snap
}

// materializeAccountTags folds organization inheritance into the account side of tag
// resolution, once per snapshot.
//
// The work here is O(accounts with tags or memberships + nodes + tags): for each account it
// walks the ancestor chains of its nodes, unions the tag names with the account's own, looks
// each name up in the tag table and sorts by priority. Doing it now rather than per request
// is what keeps the data plane free of both the walk and the JSON decoding — at the cost of
// rebuilding it whenever the configuration changes, which is exactly when a reload happens
// anyway.
func (s *Snapshot) materializeAccountTags(memberships []domain.OrgMembership) {
	s.accountTagRecords = make(map[int64][]*domain.Tag)
	s.accountTagNames = make(map[int64]map[string]struct{})
	s.orgTagNames = make(map[int64][]string)
	if len(s.Accounts) == 0 {
		return
	}

	byAccount := make(map[int64][]int64, len(memberships))
	for _, m := range memberships {
		if m.AccountID == 0 || m.NodeID == 0 {
			continue
		}
		byAccount[m.AccountID] = append(byAccount[m.AccountID], m.NodeID)
	}
	inherited := orgtree.NewIndex(s.OrgNodes).InheritedTagNames(byAccount)

	for _, account := range s.Accounts {
		if account == nil || account.ID == 0 {
			continue
		}
		orgNames := inherited[account.ID]
		accountNames := appendTagNames(nil, account.TagsJSON)
		if len(orgNames) == 0 && len(accountNames) == 0 {
			// Nothing to resolve: leaving the account out of the maps is what keeps a
			// deployment that uses neither tags nor organizations at zero cost.
			continue
		}
		if len(orgNames) > 0 {
			s.orgTagNames[account.ID] = orgNames
		}
		records, names := s.resolveNames(orgNames, accountNames)
		if len(records) > 0 {
			s.accountTagRecords[account.ID] = records
		}
		if len(names) > 0 {
			s.accountTagNames[account.ID] = names
		}
	}
}

// resolveNames turns the inherited names and the account's own names into tag records,
// dropping names no tag answers to and de-duplicating by name with the first occurrence
// winning (an inherited name therefore beats the same name listed on the account, which is
// what makes the position of a tag in the evaluation order independent of how it got there).
//
// The result is sorted by priority with a stable sort, so equal priorities keep the input
// order: ancestors before descendants.
func (s *Snapshot) resolveNames(orgNames, accountNames []string) ([]*domain.Tag, map[string]struct{}) {
	names := make(map[string]struct{}, len(orgNames)+len(accountNames))
	records := make([]*domain.Tag, 0, len(orgNames)+len(accountNames))
	for _, name := range append(append([]string{}, orgNames...), accountNames...) {
		if _, repeat := names[name]; repeat {
			continue
		}
		names[name] = struct{}{}
		if tag := s.TagByName[name]; tag != nil {
			records = append(records, tag)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Priority < records[j].Priority })
	return records, names
}

// InheritedTagNames returns the tag names an account inherits from its organization
// memberships (ancestors included), without the account's own tags. It is the read side of
// the console's "why does this account have these permissions" question.
func (s *Snapshot) InheritedTagNames(accountID int64) []string {
	if s == nil || s.orgTagNames == nil {
		return nil
	}
	return s.orgTagNames[accountID]
}

// OrgNodePath renders a node's "根/…/自身" label path, which the console shows wherever a
// node is named outside its tree. An unknown node yields "".
func (s *Snapshot) OrgNodePath(id int64) string {
	if s == nil || s.orgIndex == nil {
		return ""
	}
	chain := s.orgIndex.Chain(id)
	if len(chain) == 0 {
		return ""
	}
	parts := make([]string, 0, len(chain))
	for _, nodeID := range chain {
		if node := s.OrgNodeByID[nodeID]; node != nil {
			parts = append(parts, node.Name)
		}
	}
	return strings.Join(parts, "/")
}

// NewSnapshot builds a fully indexed immutable snapshot from the routing configuration alone:
// no organization tree, so no inherited tags. It is the shorthand used by tests, previews and
// the many callers that have no organization data; it delegates to Build so there is exactly
// one constructor and the two can never disagree.
func NewSnapshot(
	accounts []*domain.Account,
	providers []*domain.Provider,
	providerModels []*domain.ProviderModel,
	models []*domain.Model,
	mappings []*domain.ModelMapping,
	routes []*domain.Route,
	tags []*domain.Tag,
) *Snapshot {
	return Build(Input{
		Accounts: accounts, Providers: providers, ProviderModels: providerModels,
		Models: models, Mappings: mappings, Routes: routes, Tags: tags,
	})
}

// index builds the snapshot's lookup tables and normalizes the ordering the hot path relies
// on. Organization data is not its concern: Build adds that afterwards.
func index(
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

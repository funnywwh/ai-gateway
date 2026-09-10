package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/store"
)

func newStore(t *testing.T) *store.DB {
	t.Helper()
	cfg := config.Default().Database
	cfg.Path = filepath.Join(t.TempDir(), "reg.db")
	db, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seed(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()

	accID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", BillingMode: domain.BillingPrepaid})
	if err != nil {
		t.Fatal(err)
	}
	_ = accID

	provA, err := db.UpsertProvider(ctx, &domain.Provider{Name: "openai-main", Kind: "openai-responses", Enabled: true, Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	provB, err := db.UpsertProvider(ctx, &domain.Provider{Name: "deepseek", Kind: "openai-chat", Enabled: true, Priority: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: provA, PublicModel: "gpt-x", UpstreamModel: "gpt-x-2026-01-01",
		CapabilitiesJSON: `{"stream":true}`, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: provB, PublicModel: "gpt-x", UpstreamModel: "deepseek-chat",
		CapabilitiesJSON: `{"stream":true}`, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	modelID, err := db.UpsertModel(ctx, &domain.Model{PublicName: "gpt-x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRoute(ctx, &domain.Route{ModelID: modelID, ProviderID: provA, UpstreamModel: "gpt-x-2026-01-01", Priority: 10, Weight: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRoute(ctx, &domain.Route{ModelID: modelID, ProviderID: provB, UpstreamModel: "deepseek-chat", Priority: 20, Weight: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertModelMapping(ctx, &domain.ModelMapping{Kind: "prefix", Pattern: "gpt-*", TargetModel: "gpt-x", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertTag(ctx, &domain.Tag{Name: "free", GrantsJSON: `{"models":["*"]}`, Priority: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestReloadBuildsCompleteSnapshot(t *testing.T) {
	db := newStore(t)
	seed(t, db)

	reg := New(db)
	snap, err := reg.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(snap.Accounts) != 1 || len(snap.Providers) != 2 || len(snap.Models) != 1 {
		t.Fatalf("snapshot counts wrong: %s", snap.String())
	}
	if len(snap.Routes) != 2 || len(snap.ProviderModels) != 2 || len(snap.Mappings) != 1 || len(snap.Tags) != 1 {
		t.Fatalf("snapshot detail wrong: %s", snap.String())
	}
	if snap.ProviderByName["openai-main"] == nil || snap.ModelByName["gpt-x"] == nil || snap.TagByName["free"] == nil {
		t.Fatal("lookup maps not populated")
	}
	if got := snap.ProviderModel(snap.ProviderByName["deepseek"].ID, "gpt-x"); got == nil || got.UpstreamModel != "deepseek-chat" {
		t.Fatalf("provider model lookup failed: %+v", got)
	}
	if routes := snap.RoutesFor(snap.ModelByName["gpt-x"].ID); len(routes) != 2 {
		t.Fatalf("routes for model = %d, want 2", len(routes))
	}
	if !snap.Ready() {
		t.Fatal("snapshot should be ready with an enabled provider")
	}
}

func TestReloadSwapsAtomicallyAndKeepsOldSnapshotUsable(t *testing.T) {
	db := newStore(t)
	seed(t, db)
	ctx := context.Background()

	reg := New(db)
	first, err := reg.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// add a third provider and reload
	if _, err := db.UpsertProvider(ctx, &domain.Provider{Name: "ollama-local", Kind: "openai-chat", Enabled: true, Priority: 30}); err != nil {
		t.Fatal(err)
	}
	second, err := reg.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if reg.Snapshot() != second {
		t.Fatal("current snapshot was not swapped")
	}
	if len(second.Providers) != 3 {
		t.Fatalf("new snapshot providers = %d, want 3", len(second.Providers))
	}
	// the old snapshot stays immutable and readable
	if len(first.Providers) != 2 {
		t.Fatalf("old snapshot mutated: %d providers", len(first.Providers))
	}
}

func TestEmptySnapshotIsSafe(t *testing.T) {
	reg := New(newStore(t))
	snap := reg.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() must never return nil")
	}
	if snap.Ready() {
		t.Fatal("empty snapshot must not be ready")
	}
	if snap.RoutesFor(1) != nil || snap.ProviderModel(1, "x") != nil {
		t.Fatal("lookups on empty snapshot must return nil")
	}
}

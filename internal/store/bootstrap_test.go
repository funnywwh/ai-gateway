package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/registry"
)

func TestBootstrapSeedsRoutingGraph(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	stateDir := t.TempDir()

	cfg := config.Bootstrap{
		Mode:     "upsert",
		Accounts: []config.BootstrapAccount{{Name: "internal", BillingMode: "postpaid", CreditLimitUSD: 100}},
		Providers: []config.BootstrapProvider{{
			Name:     "local-chat",
			Kind:     "openai-chat",
			Priority: 10,
			Weight:   100,
			Config:   map[string]any{"base_url": "http://127.0.0.1:11434/v1"},
			Models: []config.BootstrapProviderModel{{
				Public:       "llama-local",
				Upstream:     "llama3.1:8b",
				Capabilities: map[string]bool{"stream": true, "tools": false},
			}},
		}},
		Models: []config.BootstrapModel{{
			PublicName: "llama-local",
			Aliases:    []string{"local"},
			Reasoning:  &config.BootstrapModelReasoning{Mode: "force", Effort: "high"},
		}},
		Routes: []config.BootstrapRoute{{Model: "llama-local", Provider: "local-chat", UpstreamModel: "llama3.1:8b", Priority: 10, Weight: 100}},
		Tags:   []config.BootstrapTag{{Name: "free", Models: []string{"llama-local"}, Providers: []string{"local-chat"}, Priority: 10}},
	}

	res, err := db.Bootstrap(ctx, cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProvidersCreated != 1 || res.ProviderModelsAdded != 1 || res.ModelsCreated != 1 || res.RoutesAdded != 1 || res.TagsAdded != 1 {
		t.Fatalf("bootstrap counts: %+v", res)
	}

	prov, err := db.GetProviderByName(ctx, "local-chat")
	if err != nil {
		t.Fatal(err)
	}
	wantStateDir := filepath.Join(stateDir, "local-chat")
	if prov.StateDir != wantStateDir {
		t.Errorf("state dir = %q, want %q", prov.StateDir, wantStateDir)
	}
	if prov.ConfigJSON == "" {
		t.Error("provider config json must be stored")
	}

	models, err := db.ListProviderModels(ctx, prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].UpstreamModel != "llama3.1:8b" {
		t.Fatalf("provider models: %+v", models)
	}
	if models[0].CapabilitiesJSON == "" {
		t.Error("capabilities must be stored as JSON")
	}

	model, err := db.GetModelByName(ctx, "llama-local")
	if err != nil {
		t.Fatal(err)
	}
	if model.AliasesJSON != `["local"]` {
		t.Errorf("aliases json = %q", model.AliasesJSON)
	}
	if model.ReasoningJSON != `{"mode":"force","effort":"high"}` {
		t.Errorf("reasoning json = %q", model.ReasoningJSON)
	}

	// Merge only changes reasoning when the field was explicitly supplied. This lets a
	// partial bootstrap file update aliases/pricing without accidentally clearing the
	// live model override.
	mergeOmitted := config.Bootstrap{
		Mode: "merge",
		Models: []config.BootstrapModel{{
			PublicName: "llama-local",
			Aliases:    []string{"local", "retained"},
		}},
	}
	if _, err := db.Bootstrap(ctx, mergeOmitted, stateDir); err != nil {
		t.Fatal(err)
	}
	model, err = db.GetModelByName(ctx, "llama-local")
	if err != nil {
		t.Fatal(err)
	}
	if model.ReasoningJSON != `{"mode":"force","effort":"high"}` {
		t.Fatalf("omitted merge reasoning must retain existing value, got %q", model.ReasoningJSON)
	}

	mergeReplacement := config.Bootstrap{
		Mode: "merge",
		Models: []config.BootstrapModel{{
			PublicName: "llama-local",
			Reasoning:  &config.BootstrapModelReasoning{Mode: "default", Effort: "minimal"},
		}},
	}
	if _, err := db.Bootstrap(ctx, mergeReplacement, stateDir); err != nil {
		t.Fatal(err)
	}
	model, err = db.GetModelByName(ctx, "llama-local")
	if err != nil {
		t.Fatal(err)
	}
	if model.ReasoningJSON != `{"mode":"default","effort":"minimal"}` {
		t.Fatalf("explicit merge reasoning must replace existing value, got %q", model.ReasoningJSON)
	}

	routes, err := db.ListRoutes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].ModelID != model.ID || routes[0].ProviderID != prov.ID {
		t.Fatalf("routes: %+v", routes)
	}

	tags, err := db.ListTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "free" {
		t.Fatalf("tags: %+v", tags)
	}

	// Second run must not create anything new (upsert semantics).
	res2, err := db.Bootstrap(ctx, cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if res2.ProvidersCreated != 0 || res2.ProviderModelsAdded != 0 || res2.ModelsCreated != 0 || res2.RoutesAdded != 0 || res2.TagsAdded != 0 {
		t.Fatalf("bootstrap must be idempotent: %+v", res2)
	}

	// The registry sees the seeded graph and reports readiness.
	reg := registry.New(db)
	snap, err := reg.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Ready() {
		t.Fatal("snapshot should be ready after seeding an enabled provider")
	}
	if snap.ProviderModel(prov.ID, "llama-local") == nil {
		t.Fatal("provider model lookup failed after reload")
	}
}

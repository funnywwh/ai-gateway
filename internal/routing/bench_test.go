package routing

import (
	"testing"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

const grantsAllJSON = `{"models":["*"],"providers":["*"]}`

// benchmarkSnapshot builds a small but realistic graph: one model served by four
// providers, which is the shape a load-balanced deployment actually has.
func benchmarkSnapshot() *registry.Snapshot {
	providers := []*domain.Provider{}
	providerModels := []*domain.ProviderModel{}
	routes := []*domain.Route{}
	const modelID int64 = 1
	for index := 0; index < 4; index++ {
		id := int64(index + 1)
		providers = append(providers, &domain.Provider{ID: id, Name: "provider-" + string(rune('a'+index)), Enabled: true})
		providerModels = append(providerModels, &domain.ProviderModel{
			ID: id, ProviderID: id, PublicModel: "bench-model", UpstreamModel: "upstream", Enabled: true,
		})
		routes = append(routes, &domain.Route{
			ID: id, ModelID: modelID, ProviderID: id, UpstreamModel: "upstream",
			Priority: 10, Weight: 100, Enabled: true,
		})
	}
	return registry.NewSnapshot(
		nil, providers, providerModels,
		[]*domain.Model{{ID: modelID, PublicName: "bench-model", Enabled: true}},
		nil, routes,
		[]*domain.Tag{{ID: 1, Name: "free", GrantsJSON: grantsAllJSON}},
	)
}

func BenchmarkPlan(b *testing.B) {
	snapshot := benchmarkSnapshot()
	router := New(Config{DefaultGrant: "all"}, registry.NewStatic(snapshot), balancer.New(balancer.DefaultConfig()))
	key := &domain.APIKey{ID: 1, Status: "active", TagsJSON: `["free"]`}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := router.Plan(domain.RouteRequest{Model: "bench-model", Key: key})
		if err != nil {
			b.Fatal(err)
		}
		if len(result.Candidates) == 0 {
			b.Fatal("no candidates")
		}
	}
}

func BenchmarkPlanParallel(b *testing.B) {
	snapshot := benchmarkSnapshot()
	router := New(Config{DefaultGrant: "all"}, registry.NewStatic(snapshot), balancer.New(balancer.DefaultConfig()))
	key := &domain.APIKey{ID: 1, Status: "active", TagsJSON: `["free"]`}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := router.Plan(domain.RouteRequest{Model: "bench-model", Key: key}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

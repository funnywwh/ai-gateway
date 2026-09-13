package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// TestModelReasoningReachesSharedResponsesSupplier drives the real HTTP handler,
// router, dispatcher, and openai-responses builtin. The upstream records wire bodies,
// proving two canonical models sharing one supplier do not share reasoning policy.
func TestModelReasoningReachesSharedResponsesSupplier(t *testing.T) {
	var mu sync.Mutex
	var captured []map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		captured = append(captured, body)
		mu.Unlock()
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_upstream","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	f := newFixture(t)
	ctx := context.Background()
	config, err := json.Marshal(map[string]any{"base_url": upstream.URL, "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	providerID, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: "shared-responses", Kind: "openai-responses", Enabled: true, Weight: 100, ConfigJSON: string(config),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reasoning string
	}{
		{"reasoning-high", `{"mode":"force","effort":"high"}`},
		{"reasoning-medium", `{"mode":"default","effort":"medium"}`},
	} {
		modelID, err := f.db.UpsertModel(ctx, &domain.Model{PublicName: tc.name, Enabled: true, ReasoningJSON: tc.reasoning})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
			ProviderID: providerID, PublicModel: tc.name, UpstreamModel: "shared-upstream-" + tc.name,
			Enabled: true, CapabilitiesJSON: `{"stream":true,"reasoning":true}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.UpsertRoute(ctx, &domain.Route{ModelID: modelID, ProviderID: providerID, Enabled: true, Weight: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	request := func(model string, stream bool, effort string) map[string]json.RawMessage {
		t.Helper()
		body := `{"model":"` + model + `","input":"hello"`
		if effort != "" {
			body += `,"reasoning":{"effort":"` + effort + `"}`
		}
		if stream {
			body += `,"stream":true`
		}
		body += `}`
		mu.Lock()
		before := len(captured)
		mu.Unlock()
		resp := f.do(t, http.MethodPost, "/v1/responses", body, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			got, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s stream=%t status=%d: %s", model, stream, resp.StatusCode, got)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		mu.Lock()
		defer mu.Unlock()
		if len(captured) != before+1 {
			t.Fatalf("%s stream=%t upstream calls=%d, want exactly one new call after %d", model, stream, len(captured), before)
		}
		return captured[before]
	}
	assertEffort := func(body map[string]json.RawMessage, want string) {
		t.Helper()
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(body["reasoning"], &reasoning); err != nil || reasoning.Effort != want {
			t.Fatalf("upstream reasoning=%s decoded=%+v err=%v, want %q", body["reasoning"], reasoning, err, want)
		}
	}

	for _, tc := range []struct {
		name, model, effort string
		stream              bool
		want                string
	}{
		{"force nonstream overrides low", "reasoning-high", "low", false, "high"},
		{"force stream overrides low", "reasoning-high", "low", true, "high"},
		{"default nonstream fills configured medium", "reasoning-medium", "", false, "medium"},
		{"default stream fills configured medium", "reasoning-medium", "", true, "medium"},
		{"default nonstream preserves none", "reasoning-medium", "none", false, "none"},
		{"default stream preserves none", "reasoning-medium", "none", true, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := request(tc.model, tc.stream, tc.effort)
			if got := string(body["stream"]); got != map[bool]string{true: "true", false: "false"}[tc.stream] {
				t.Fatalf("upstream stream=%s, want %t", got, tc.stream)
			}
			assertEffort(body, tc.want)
		})
	}
	provider := f.registry.Snapshot().ProviderByID[providerID]
	if provider == nil || provider.ConfigJSON != string(config) {
		t.Fatalf("provider config mutated: %+v, want %s", provider, config)
	}

	// Clearing a canonical model's policy must return it to the supplier's inherited
	// behavior (no reasoning object), rather than retaining a prior sibling's force.
	if _, err := f.db.UpsertModel(ctx, &domain.Model{PublicName: "reasoning-high", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	cleared := request("reasoning-high", false, "")
	if _, ok := cleared["reasoning"]; ok {
		t.Fatalf("cleared model leaked reasoning to shared supplier: %s", cleared["reasoning"])
	}
}

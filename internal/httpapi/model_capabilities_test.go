package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/responses"
)

// capabilityEntry is the wire shape these tests read back. Every capability field is a
// pointer or a container so an *omitted* key is distinguishable from an empty one: the
// contract is "undeclared is omitted", and a zero standing in for unknown would tell a
// client the model has no context at all.
type capabilityEntry struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	ContextWindow   *int            `json:"context_window"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	InputModalities []string        `json:"input_modalities"`
	Capabilities    map[string]bool `json:"capabilities"`
	Reasoning       *struct {
		Mode   string `json:"mode"`
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

func listModels(t *testing.T, f *fixture) map[string]capabilityEntry {
	t.Helper()
	resp := f.do(t, "GET", "/v1/models", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/models = %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []capabilityEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	byID := map[string]capabilityEntry{}
	for _, entry := range list.Data {
		byID[entry.ID] = entry
	}
	return byID
}

// capabilityFixture adds a second provider (the vision route) and two more models to the
// standard fixture, so one listing exercises every branch of the disclosure: a union of
// capabilities, capacities that differ per route, a display name, a reasoning policy, and a
// model that declares nothing at all.
func capabilityFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()

	visionProvider, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: "echo-vision", Kind: "testecho", Enabled: true, Priority: 20, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same public model on a second route: 272000/4096 and image support. The union of
	// capabilities and the minimum of capacities are both visible in the listing.
	if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: visionProvider, PublicModel: "echo-model", UpstreamModel: "echo-model", Enabled: true,
		ContextWindow: 272000, MaxOutputTokens: 4096,
		CapabilitiesJSON: `{"stream":true,"tools":true,"image":true}`,
	}); err != nil {
		t.Fatal(err)
	}
	echoModel, err := f.db.GetModelByName(ctx, "echo-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertRoute(ctx, &domain.Route{
		ModelID: echoModel.ID, ProviderID: visionProvider, Priority: 20, Weight: 100, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	echoProvider, err := f.db.GetProviderByName(ctx, "echo")
	if err != nil {
		t.Fatal(err)
	}
	addModel := func(name, displayName, caps, reasoning string, contextWindow, maxOutput int) {
		t.Helper()
		if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
			ProviderID: echoProvider.ID, PublicModel: name, UpstreamModel: name, Enabled: true,
			ContextWindow: contextWindow, MaxOutputTokens: maxOutput, CapabilitiesJSON: caps,
		}); err != nil {
			t.Fatal(err)
		}
		modelID, err := f.db.UpsertModel(ctx, &domain.Model{
			PublicName: name, DisplayName: displayName, Enabled: true, ReasoningJSON: reasoning,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.UpsertRoute(ctx, &domain.Route{
			ModelID: modelID, ProviderID: echoProvider.ID, Priority: 10, Weight: 100, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	addModel("reasoning-model", "Reasoning One", `{"stream":true,"reasoning":true}`,
		`{"mode":"force","effort":"high"}`, 1000000, 65536)
	addModel("bare-model", "", "", "", 0, 0)

	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestModelsEndpointDisclosesCapabilities is the M68 contract: the listing carries what the
// deployment knows (capacities, modalities, declared capabilities, reasoning policy, display
// name) so a client can size its own budget and offer the levels the model takes.
func TestModelsEndpointDisclosesCapabilities(t *testing.T) {
	f := capabilityFixture(t)
	models := listModels(t, f)

	echo, ok := models["echo-model"]
	if !ok {
		t.Fatalf("echo-model missing from %v", models)
	}
	// Two routes disagree: 0/1024 + {stream,tools} and 272000/4096 + {stream,tools,image}.
	// The declared capacities fold to the minimum and the undeclared 0 never wins it; the
	// capabilities are a union because declaring one is what routes a request there.
	if echo.ContextWindow == nil || *echo.ContextWindow != 272000 {
		t.Errorf("echo-model context_window = %v, want 272000", echo.ContextWindow)
	}
	if echo.MaxOutputTokens == nil || *echo.MaxOutputTokens != 1024 {
		t.Errorf("echo-model max_output_tokens = %v, want the smallest declared 1024", echo.MaxOutputTokens)
	}
	for _, want := range []string{"stream", "tools", "image"} {
		if !echo.Capabilities[want] {
			t.Errorf("echo-model capabilities = %v, want %q", echo.Capabilities, want)
		}
	}
	if len(echo.InputModalities) != 2 || echo.InputModalities[0] != "text" || echo.InputModalities[1] != "image" {
		t.Errorf("echo-model input_modalities = %v, want [text image]", echo.InputModalities)
	}
	if echo.Name != "" {
		t.Errorf("echo-model name = %q, want omitted (no display name)", echo.Name)
	}
	if echo.Reasoning != nil {
		t.Errorf("echo-model reasoning = %+v, want omitted (no policy configured)", echo.Reasoning)
	}

	reasoner, ok := models["reasoning-model"]
	if !ok {
		t.Fatalf("reasoning-model missing from %v", models)
	}
	if reasoner.Name != "Reasoning One" {
		t.Errorf("name = %q, want the display name", reasoner.Name)
	}
	if reasoner.ContextWindow == nil || *reasoner.ContextWindow != 1000000 {
		t.Errorf("context_window = %v, want 1000000", reasoner.ContextWindow)
	}
	if reasoner.MaxOutputTokens == nil || *reasoner.MaxOutputTokens != 65536 {
		t.Errorf("max_output_tokens = %v, want 65536", reasoner.MaxOutputTokens)
	}
	if !reasoner.Capabilities["reasoning"] || reasoner.Capabilities["image"] {
		t.Errorf("capabilities = %v, want reasoning without image", reasoner.Capabilities)
	}
	if len(reasoner.InputModalities) != 1 || reasoner.InputModalities[0] != "text" {
		t.Errorf("input_modalities = %v, want [text]", reasoner.InputModalities)
	}
	if reasoner.Reasoning == nil || reasoner.Reasoning.Mode != "force" || reasoner.Reasoning.Effort != "high" {
		t.Errorf("reasoning = %+v, want the configured force/high policy", reasoner.Reasoning)
	}

	// A model whose provider model declares nothing discloses no capacities and no
	// capabilities: "unknown" must not arrive as a zero a client would treat as a fact.
	bare, ok := models["bare-model"]
	if !ok {
		t.Fatalf("bare-model missing from %v", models)
	}
	if bare.ContextWindow != nil || bare.MaxOutputTokens != nil || bare.Capabilities != nil {
		t.Errorf("bare-model disclosed undeclared facts: %+v", bare)
	}
	if len(bare.InputModalities) != 1 || bare.InputModalities[0] != "text" {
		t.Errorf("bare-model input_modalities = %v, want the text floor", bare.InputModalities)
	}
}

// An image-bearing request must require the image capability, or a vision request is routed
// exactly like a text one and the upstream decides what happens to the image.
func TestFeaturesOfRequiresImageForImageInput(t *testing.T) {
	parse := func(body string) *responses.Request {
		t.Helper()
		req, apiErr := responses.Parse([]byte(body))
		if apiErr != nil {
			t.Fatalf("Parse(%s): %v", body, apiErr)
		}
		return req
	}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "message image",
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_text","text":"what is this"},` +
				`{"type":"input_image","image_url":"data:image/png;base64,aGk="}]}]}`,
			want: true,
		},
		{
			name: "tool result image",
			body: `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[` +
				`{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`,
			want: true,
		},
		{
			name: "text only",
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_text","text":"hello"}]}]}`,
			want: false,
		},
		{
			// The pre-filter is a substring search; the decision must still be made on
			// content parts, so a conversation quoting the type name is text.
			name: "text quoting the part type",
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_text","text":"send me an input_image part"}]}]}`,
			want: false,
		},
		{name: "string input", body: `{"model":"m","input":"hi"}`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := parse(tc.body)
			if got := req.HasImageInput(); got != tc.want {
				t.Fatalf("HasImageInput() = %t, want %t", got, tc.want)
			}
			if got := featuresOf(req)["image"]; got != tc.want {
				t.Fatalf("featuresOf image = %t, want %t", got, tc.want)
			}
		})
	}
}

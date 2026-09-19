package tenancy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"gopkg.in/yaml.v3"
)

// dshProfile is the rendered shape these tests read back: the fields the official
// dsh-llm-pi-ai documentation defines, so the assertions are about the document dsh parses
// rather than about the Go structs that produced it. A key the renderer spells differently
// lands in Extra instead of the named field, which is what makes a typo fail here.
type dshProfile struct {
	LLMPiAi struct {
		Providers map[string]dshProvider `yaml:"providers"`
	} `yaml:"llm-pi-ai"`
	AgentDefaultModel map[string]string `yaml:"agent-default-model"`
}

type dshProvider struct {
	APIKeyEnv            string          `yaml:"apiKeyEnv"`
	API                  string          `yaml:"api"`
	BaseURL              string          `yaml:"baseURL"`
	MaxRequestImageBytes int             `yaml:"maxRequestImageBytes"`
	Models               []dshModelEntry `yaml:"models"`
	Compat               map[string]bool `yaml:"compat"`
	Extra                map[string]any  `yaml:",inline"`
}

type dshModelEntry struct {
	ID               string         `yaml:"id"`
	Name             string         `yaml:"name"`
	ContextWindow    int            `yaml:"contextWindow"`
	MaxTokens        int            `yaml:"maxTokens"`
	Input            []string       `yaml:"input"`
	ReasoningEfforts any            `yaml:"reasoningEfforts"`
	Extra            map[string]any `yaml:",inline"`
}

func renderProfile(t *testing.T, models []aigw.Model) (dshProfile, string) {
	t.Helper()
	cfg, _ := renderFixture(t)
	data, err := renderSettings(cfg, nil, models)
	if err != nil {
		t.Fatal(err)
	}
	var profile dshProfile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		t.Fatalf("rendered settings are not valid YAML: %v\n%s", err, data)
	}
	return profile, string(data)
}

// TestRenderSettingsCarriesModelParameters is the M68 contract on the dshgw side: every fact
// aigw disclosed reaches the tenant's profile under the field name the official documentation
// defines, and nothing is invented for a fact aigw did not disclose.
func TestRenderSettingsCarriesModelParameters(t *testing.T) {
	profile, _ := renderProfile(t, []aigw.Model{
		{
			ID: "deepseek-flash", Name: "DeepSeek Flash", ContextWindow: 1000000, MaxOutputTokens: 65536,
			Images: true, ReasoningSupported: reasoningSupport(true),
		},
		{ID: "forced-model", ReasoningSupported: reasoningSupport(true), ReasoningForced: true},
		{ID: "plain-model", ReasoningSupported: reasoningSupport(false)},
		{ID: "legacy-model"},
		{ID: "u2-flash", Name: "u2-flash（unisound）", ReasoningSupported: reasoningSupport(false)},
	})

	provider, ok := profile.LLMPiAi.Providers["aigw"]
	if !ok {
		t.Fatalf("aigw provider missing: %+v", profile)
	}
	if provider.API != "openai-responses" || provider.APIKeyEnv != "AIGW_API_KEY" || provider.BaseURL != "http://aigw:8088/v1" {
		t.Errorf("route facts changed: %+v", provider)
	}
	if !provider.Compat["supportsStrictMode"] {
		t.Errorf("compat.supportsStrictMode must stay: %+v", provider.Compat)
	}
	if provider.MaxRequestImageBytes != 7340032 {
		t.Errorf("maxRequestImageBytes = %d, want the 7 MiB default", provider.MaxRequestImageBytes)
	}
	if len(provider.Models) != 5 {
		t.Fatalf("models = %+v, want all five", provider.Models)
	}
	// Sorted by id, so a sync produces a diff a person can read.
	wantOrder := []string{"deepseek-flash", "forced-model", "legacy-model", "plain-model", "u2-flash"}
	for i, want := range wantOrder {
		if provider.Models[i].ID != want {
			t.Fatalf("model order = %v, want %v", modelIDsOf(provider.Models), wantOrder)
		}
	}

	flash := provider.Models[0]
	if flash.Name != "DeepSeek Flash" || flash.ContextWindow != 1000000 || flash.MaxTokens != 65536 {
		t.Errorf("flash entry = %+v", flash)
	}
	if len(flash.Input) != 2 || flash.Input[0] != "text" || flash.Input[1] != "image" {
		t.Errorf("flash input = %v, want [text image]", flash.Input)
	}
	if !isReasoningTable(flash.ReasoningEfforts) {
		t.Errorf("flash reasoningEfforts = %#v, want the full level table", flash.ReasoningEfforts)
	}

	// A model whose policy forces an effort offers no menu: the gateway overrides whatever
	// the client picks, so a selectable level would be a lie.
	forced := provider.Models[1]
	if forced.ReasoningEfforts != nil {
		t.Errorf("forced model reasoningEfforts = %#v, want the key omitted", forced.ReasoningEfforts)
	}
	if forced.Name != "forced-model" {
		t.Errorf("a model without a display name keeps its id: %+v", forced)
	}

	if plain := provider.Models[3].ReasoningEfforts; plain != false {
		t.Errorf("declared non-reasoning model = %#v, want false", plain)
	}
	// Unknown is not the same claim as unsupported: the key is left out so dsh inherits.
	if legacy := provider.Models[2].ReasoningEfforts; legacy != nil {
		t.Errorf("model with no disclosed capability set = %#v, want the key omitted", legacy)
	}
	if legacy := provider.Models[2]; legacy.ContextWindow != 0 || legacy.MaxTokens != 0 || legacy.Input != nil {
		t.Errorf("undeclared facts were rendered as values: %+v", legacy)
	}
	if provider.Models[4].Name != "u2-flash（unisound）" {
		t.Errorf("display name = %q", provider.Models[4].Name)
	}
	if profile.AgentDefaultModel["model"] != "deepseek-flash" || profile.AgentDefaultModel["provider"] != "aigw" {
		t.Errorf("agent-default-model = %v", profile.AgentDefaultModel)
	}
}

// modelIDsOf renders the ids of a rendered model list for failure messages.
func modelIDsOf(models []dshModelEntry) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

// isReasoningTable reports whether the rendered value is the documented seven-level table
// with `off` spelled `none` — the spelling that actually turns thinking off (M20 §3).
func isReasoningTable(value any) bool {
	table, ok := value.(map[string]any)
	if !ok || len(table) != 7 {
		return false
	}
	for level, spelling := range map[string]string{
		"off": "none", "minimal": "minimal", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "xhigh", "max": "max",
	} {
		if table[level] != spelling {
			return false
		}
	}
	return true
}

// A route with no image-capable model carries no image bound: the setting exists to fit
// aigw's request body cap, and without images there is nothing to fit.
func TestRenderSettingsOmitsImageBoundWithoutImages(t *testing.T) {
	profile, text := renderProfile(t, []aigw.Model{{ID: "text-only"}})
	if got := profile.LLMPiAi.Providers["aigw"].MaxRequestImageBytes; got != 0 {
		t.Errorf("maxRequestImageBytes = %d, want it omitted", got)
	}
	if strings.Contains(text, "maxRequestImageBytes") {
		t.Errorf("image bound rendered for a text-only route:\n%s", text)
	}
}

// The rendered document is the artifact an operator diffs and the schema test validates, so
// it is pinned byte for byte. Update it deliberately with UPDATE_GOLDEN=1 and review the diff:
// a change here is a change to what every tenant reads.
func TestRenderSettingsGolden(t *testing.T) {
	profile := []aigw.Model{
		{
			ID: "deepseek-flash", Name: "DeepSeek Flash", ContextWindow: 1000000, MaxOutputTokens: 65536,
			Images: true, ReasoningSupported: reasoningSupport(true),
		},
		{ID: "forced-model", Name: "Forced Model", ReasoningSupported: reasoningSupport(true), ReasoningForced: true},
		{ID: "legacy-model"},
		{ID: "plain-model", ReasoningSupported: reasoningSupport(false)},
		{ID: "u2-flash", Name: "u2-flash（unisound）", ReasoningSupported: reasoningSupport(false)},
	}
	_, text := renderProfile(t, profile)
	golden := filepath.Join("testdata", "aigw-settings.golden.yaml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 go test ./internal/dshgw/tenancy -run Golden to create it): %v", err)
	}
	if text != string(want) {
		t.Fatalf("rendered settings differ from %s:\n--- rendered ---\n%s\n--- golden ---\n%s", golden, text, want)
	}
}

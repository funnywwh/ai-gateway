package modelmap

import (
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// regexPattern exercises a named capture group.
const regexPattern = `^gpt-(?P<version>[0-9]+)-suffix$`

func snap(models []*domain.Model, providers []*domain.Provider, pms []*domain.ProviderModel, mappings []*domain.ModelMapping) *registry.Snapshot {
	return registry.NewSnapshot(nil, providers, pms, models, mappings, nil, nil)
}

func baseSnap() *registry.Snapshot {
	return snap(
		[]*domain.Model{{ID: 1, PublicName: "gpt-x", Enabled: true, AliasesJSON: `["gpt-legacy"]`}},
		[]*domain.Provider{{ID: 10, Name: "openai-main", Kind: "openai-responses", Enabled: true}},
		[]*domain.ProviderModel{{ID: 100, ProviderID: 10, PublicModel: "gpt-x", UpstreamModel: "gpt-x-2026-01-01", Enabled: true}},
		nil,
	)
}

func TestResolveExactAndAlias(t *testing.T) {
	r := New("")
	s := baseSnap()

	got, err := r.Resolve(s, "gpt-x", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Canonical != "gpt-x" || got.Pinned || !strings.HasPrefix(got.MatchedRule, "model:") {
		t.Fatalf("exact resolution mismatch: %+v", got)
	}

	got, err = r.Resolve(s, "gpt-legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Canonical != "gpt-x" || !strings.HasPrefix(got.MatchedRule, "alias:") {
		t.Fatalf("alias resolution mismatch: %+v", got)
	}
}

func TestResolveMappingKindsAndPriority(t *testing.T) {
	s := snap(
		[]*domain.Model{{ID: 1, PublicName: "canonical-a", Enabled: true}},
		nil, nil,
		[]*domain.ModelMapping{
			{ID: 1, Kind: "glob", Pattern: "*", TargetModel: "canonical-a", Priority: 900, Enabled: true},
			{ID: 2, Kind: "prefix", Pattern: "gpt-", TargetModel: "canonical-a", Priority: 50, Enabled: true},
		},
	)
	r := New("")

	got, err := r.Resolve(s, "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Canonical != "canonical-a" || got.MatchedRule != "mapping:prefix:gpt-" {
		t.Fatalf("prefix mapping must win over the glob catch-all: %+v", got)
	}
	if got.Groups["1"] != "4o" {
		t.Fatalf("prefix capture group missing: %+v", got.Groups)
	}

	got, err = r.Resolve(s, "other-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.MatchedRule != "mapping:glob:*" {
		t.Fatalf("glob catch-all must apply: %+v", got)
	}
}

func TestResolveRegexNamedGroupAndTemplate(t *testing.T) {
	s := snap(
		[]*domain.Model{{ID: 1, PublicName: "target", Enabled: true}},
		nil, nil,
		[]*domain.ModelMapping{{
			ID: 1, Kind: "regex", Pattern: regexPattern,
			TargetModel: "target", Priority: 10, Enabled: true,
		}},
	)
	r := New("")
	got, err := r.Resolve(s, "gpt-42-suffix", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Groups["version"] != "42" || got.Groups["1"] != "42" {
		t.Fatalf("named/numbered groups missing: %+v", got.Groups)
	}
	if out := ApplyTemplate("upstream-{version}-{model}", "gpt-42-suffix", got.Groups); out != "upstream-42-gpt-42-suffix" {
		t.Fatalf("template expansion mismatch: %s", out)
	}
}

func TestResolvePinnedMappingSkipsRouting(t *testing.T) {
	s := snap(
		nil,
		[]*domain.Provider{{ID: 7, Name: "local", Kind: "openai-chat", Enabled: true}},
		nil,
		[]*domain.ModelMapping{{
			ID: 1, Kind: "glob", Pattern: "llama-*", TargetModel: "llama", Priority: 10, Enabled: true,
			TargetProviderID: 7, TargetUpstreamModel: "llama3-{1}",
		}},
	)
	r := New("")
	got, err := r.Resolve(s, "llama-8b", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pinned || got.ProviderID != 7 || got.Upstream != "llama3-8b" {
		t.Fatalf("pinned mapping mismatch: %+v", got)
	}
}

func TestResolveProviderPinFromSuffix(t *testing.T) {
	r := New("")
	got, err := r.Resolve(baseSnap(), "gpt-x@openai-main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pinned || got.ProviderName != "openai-main" || got.Upstream != "gpt-x-2026-01-01" {
		t.Fatalf("pin mismatch: %+v", got)
	}

	if _, err := r.Resolve(baseSnap(), "gpt-x@missing", ""); !domain.IsNotFound(err) {
		t.Fatalf("unknown pinned provider must be not found, got %v", err)
	}
}

func TestResolveFallbackAndNotFound(t *testing.T) {
	s := baseSnap()
	r := New("gpt-x")
	got, err := r.Resolve(s, "unknown-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Canonical != "gpt-x" || !strings.HasPrefix(got.MatchedRule, "fallback:") {
		t.Fatalf("fallback mismatch: %+v", got)
	}

	strict := New("")
	if _, err := strict.Resolve(s, "unknown-model", ""); !domain.IsNotFound(err) {
		t.Fatalf("expected model_not_found, got %v", err)
	}
}

func TestSplitPin(t *testing.T) {
	cases := []struct{ in, model, provider string }{
		{"gpt-x@openai", "gpt-x", "openai"},
		{"gpt-x", "gpt-x", ""},
		{"@openai", "@openai", ""},
		{"gpt-x@", "gpt-x@", ""},
		{"a@b@c", "a@b", "c"},
	}
	for _, tc := range cases {
		m, p := SplitPin(tc.in)
		if m != tc.model || p != tc.provider {
			t.Errorf("SplitPin(%q) = (%q,%q), want (%q,%q)", tc.in, m, p, tc.model, tc.provider)
		}
	}
}

func TestValidateRule(t *testing.T) {
	if err := ValidateRule("prefix", "gpt-", "gpt-x", 0); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	if err := ValidateRule("bogus", "gpt-", "gpt-x", 0); err == nil {
		t.Fatal("invalid kind must be rejected")
	}
	if err := ValidateRule("regex", "([", "gpt-x", 0); err == nil {
		t.Fatal("invalid regex must be rejected")
	}
	if err := ValidateRule("exact", "x", "", 0); err == nil {
		t.Fatal("rule without target must be rejected")
	}
	if err := ValidateRule("exact", "", "gpt-x", 0); err == nil {
		t.Fatal("empty pattern must be rejected")
	}
}

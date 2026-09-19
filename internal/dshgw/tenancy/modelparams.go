// Model parameters rendered into a tenant's dsh provider profile (M68).
//
// Everything here maps facts aigw disclosed on GET /v1/models onto the field names the
// official dsh-llm-pi-ai documentation defines (`contextWindow`, `maxTokens`, `input`,
// `reasoningEfforts`), and nothing else: the entry a tenant reads is the entry the docs
// describe, filled from what the deployment actually knows.
package tenancy

import (
	"sort"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
)

// providerModel is one entry of `llm-pi-ai.providers.aigw.models`.
//
// `omitempty` is load-bearing rather than cosmetic. dsh's own schema refuses a capacity of 0
// (`number >= 1`), so an undeclared context window or output cap has to be left out and the
// adapter's documented fallback taken; the same goes for `input`, where an omitted list is
// how a text-only model inherits the route default.
type providerModel struct {
	ID            string   `yaml:"id"`
	Name          string   `yaml:"name,omitempty"`
	ContextWindow int      `yaml:"contextWindow,omitempty"`
	MaxTokens     int      `yaml:"maxTokens,omitempty"`
	Input         []string `yaml:"input,omitempty"`
	// ReasoningEfforts is either an omitted key (the model's reasoning ability is unknown, so
	// dsh inherits), `false` (declared: this model does not reason) or the level table below.
	// It is untyped because the documented field accepts both shapes.
	ReasoningEfforts any `yaml:"reasoningEfforts,omitempty"`
}

// reasoningLevels is the level table dsh renders as the model picker's Effort menu. Each key
// is a level the menu offers and its value is the spelling sent on the wire, so the two
// vocabularies meet here: dsh's `off` is aigw's `none`, and every other name is the same
// word on both sides.
//
// `off: none` is deliberate and measured (docs/design/m20-dsh-reasoning-effort.md §3):
// pi-ai sends the `off` spelling whenever a request picks no level, an empty `off` would
// send nothing at all and let the upstream's own default (thinking on, for DeepSeek) decide,
// while `none` really does turn thinking off. A struct rather than a map so the rendered
// order is fixed and the golden file stays readable.
type reasoningLevels struct {
	Off     string `yaml:"off"`
	Minimal string `yaml:"minimal"`
	Low     string `yaml:"low"`
	Medium  string `yaml:"medium"`
	High    string `yaml:"high"`
	XHigh   string `yaml:"xhigh"`
	Max     string `yaml:"max"`
}

// fullReasoningLevels is the complete level table a reasoning model offers. aigw publishes
// whether a model reasons and which effort its own policy applies, but not which levels the
// upstream accepts — that is a measured fact per model (M20 §6), so the table states the
// gateway's whole closed set and an upstream that refuses one level refuses it out loud.
func fullReasoningLevels() reasoningLevels {
	return reasoningLevels{
		Off: "none", Minimal: "minimal", Low: "low", Medium: "medium",
		High: "high", XHigh: "xhigh", Max: "max",
	}
}

// dshModel renders one disclosed model as its dsh profile entry.
func dshModel(model aigw.Model) providerModel {
	entry := providerModel{ID: model.ID, Name: model.Name}
	if entry.Name == "" {
		entry.Name = model.ID
	}
	// Only declared facts are written: 0 is aigw's word for "not disclosed", and dsh refuses
	// a capacity of 0 outright.
	if model.ContextWindow > 0 {
		entry.ContextWindow = model.ContextWindow
	}
	if model.MaxOutputTokens > 0 {
		entry.MaxTokens = model.MaxOutputTokens
	}
	if model.Images {
		entry.Input = []string{"text", "image"}
	}
	entry.ReasoningEfforts = dshReasoningEfforts(model)
	return entry
}

// dshReasoningEfforts answers what the model picker may offer for reasoning.
//
// The three answers are three different settings, which is why aigw's reply distinguishes
// them (aigw.Model.ReasoningSupported is nil, false, or true):
//
//   - unknown (nil): the key is omitted. A hand-declared model inherits nothing, so dsh
//     offers no Effort entry and the endpoint's own default decides — the honest answer when
//     the deployment never said either way.
//   - declared non-reasoning (false): `reasoningEfforts: false`, the documented way to state
//     that a model does not reason.
//   - reasoning (true): the full level table — unless the model's own policy is `force`,
//     where aigw overrides whatever effort the client asks for. A selectable menu would then
//     be a lie, so the key is omitted and dsh sends no effort at all, leaving the gateway to
//     apply the policy it was configured with.
func dshReasoningEfforts(model aigw.Model) any {
	if model.ReasoningSupported == nil {
		return nil
	}
	if !*model.ReasoningSupported {
		return false
	}
	if model.ReasoningForced {
		return nil
	}
	return fullReasoningLevels()
}

// dshModels renders a disclosed model list, sorted by id and de-duplicated.
//
// Order and uniqueness are part of the artifact's contract: the tenant's settings.yaml is
// rewritten on every sync, and a stable document is what lets a person diff it, the golden
// test compare it byte for byte, and `agent-default-model` keep naming the same first model.
// A duplicate id keeps its first disclosure rather than merging two rows, because aigw's own
// listing is already de-duplicated and a second row for one id would mean one of them is
// stale.
func dshModels(models []aigw.Model) []providerModel {
	sorted := append([]aigw.Model(nil), models...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make([]providerModel, 0, len(sorted))
	for _, model := range sorted {
		if model.ID == "" || len(out) > 0 && out[len(out)-1].ID == model.ID {
			continue
		}
		out = append(out, dshModel(model))
	}
	return out
}

// anyImages reports whether at least one rendered model accepts image input, which is what
// decides whether the route needs an image payload bound at all.
func anyImages(models []providerModel) bool {
	for _, model := range models {
		for _, modality := range model.Input {
			if modality == "image" {
				return true
			}
		}
	}
	return false
}

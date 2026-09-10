// Package modelmap resolves a requested model name into a canonical model,
// optionally pinned to a specific provider with a concrete upstream model name.
//
// Resolution order (first hit wins):
//  1. explicit provider pin (model@provider or X-Gateway-Provider)
//  2. exact models.public_name
//  3. model_mappings by priority (exact | prefix | glob | regex)
//  4. legacy models.aliases_json
//  5. configured fallback model
//  6. 404 model_not_found
package modelmap

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// Resolver resolves model names against a registry snapshot.
type Resolver struct {
	fallback string
}

// New builds a resolver. fallback is the routing.model_fallback value (may be empty).
func New(fallback string) *Resolver {
	return &Resolver{fallback: strings.TrimSpace(fallback)}
}

// SplitPin splits "model@provider" into its parts (either may be empty).
func SplitPin(requested string) (model string, provider string) {
	idx := strings.LastIndex(requested, "@")
	if idx <= 0 || idx == len(requested)-1 {
		return requested, ""
	}
	return requested[:idx], requested[idx+1:]
}

// Resolve maps requested to a canonical model.
//
// providerHint comes from the X-Gateway-Provider header; a "@provider" suffix in
// requested takes precedence over it.
func (r *Resolver) Resolve(snap *registry.Snapshot, requested, providerHint string) (*domain.ResolvedModel, error) {
	if snap == nil {
		return nil, domain.ErrModelNotFound(requested)
	}
	name, pinnedProvider := SplitPin(strings.TrimSpace(requested))
	if name == "" {
		return nil, domain.ErrInvalidRequest("model must not be empty")
	}
	if pinnedProvider == "" {
		pinnedProvider = strings.TrimSpace(providerHint)
	}

	out := &domain.ResolvedModel{Requested: requested, Groups: map[string]string{}}

	// 2. exact canonical model
	if m := snap.ModelByName[name]; m != nil && m.Enabled {
		out.Canonical = m.PublicName
		out.MatchedRule = "model:" + m.PublicName
		return r.finish(snap, out, pinnedProvider)
	}

	// 3. mapping rules, in priority order
	for _, rule := range snap.Mappings {
		if !rule.Enabled {
			continue
		}
		groups, ok := matchRule(rule.Kind, rule.Pattern, name)
		if !ok {
			continue
		}
		for k, v := range groups {
			out.Groups[k] = v
		}
		out.MatchedRule = fmt.Sprintf("mapping:%s:%s", rule.Kind, rule.Pattern)
		if rule.TargetProviderID != 0 {
			// direct pin to a provider + upstream model
			provider := snap.ProviderByID[rule.TargetProviderID]
			if provider == nil {
				return nil, domain.ErrNotFound(fmt.Sprintf("mapping %s targets unknown provider %d", rule.Pattern, rule.TargetProviderID))
			}
			out.Pinned = true
			out.ProviderID = provider.ID
			out.ProviderName = provider.Name
			out.Canonical = applyTemplate(rule.TargetModel, name, out.Groups)
			if out.Canonical == "" {
				out.Canonical = name
			}
			out.Upstream = applyTemplate(rule.TargetUpstreamModel, name, out.Groups)
			return out, nil
		}
		if rule.TargetModel != "" {
			out.Canonical = applyTemplate(rule.TargetModel, name, out.Groups)
			return r.finish(snap, out, pinnedProvider)
		}
	}

	// 4. legacy exact aliases
	for _, m := range snap.Models {
		if !m.Enabled || m.AliasesJSON == "" {
			continue
		}
		var aliases []string
		if err := json.Unmarshal([]byte(m.AliasesJSON), &aliases); err != nil {
			continue
		}
		for _, alias := range aliases {
			if alias == name {
				out.Canonical = m.PublicName
				out.MatchedRule = "alias:" + alias
				return r.finish(snap, out, pinnedProvider)
			}
		}
	}

	// 5. fallback
	if r.fallback != "" {
		out.Canonical = r.fallback
		out.MatchedRule = "fallback:" + r.fallback
		return r.finish(snap, out, pinnedProvider)
	}

	return nil, domain.ErrModelNotFound(requested)
}

// finish applies a provider pin (if any) and validates it against the snapshot.
func (r *Resolver) finish(snap *registry.Snapshot, out *domain.ResolvedModel, pinnedProvider string) (*domain.ResolvedModel, error) {
	if pinnedProvider == "" {
		return out, nil
	}
	provider := snap.ProviderByName[pinnedProvider]
	if provider == nil {
		return nil, domain.ErrNotFound("provider " + pinnedProvider)
	}
	out.Pinned = true
	out.ProviderID = provider.ID
	out.ProviderName = provider.Name
	if pm := snap.ProviderModel(provider.ID, out.Canonical); pm != nil {
		out.Upstream = pm.UpstreamModel
	}
	return out, nil
}

// matchRule reports whether name matches pattern for the given kind and returns capture groups.
func matchRule(kind, pattern, name string) (map[string]string, bool) {
	re, err := Compile(kind, pattern)
	if err != nil {
		return nil, false
	}
	m := re.FindStringSubmatch(name)
	if m == nil {
		return nil, false
	}
	groups := map[string]string{}
	names := re.SubexpNames()
	for i := 1; i < len(m); i++ {
		groups[fmt.Sprintf("%d", i)] = m[i]
		if i < len(names) && names[i] != "" {
			groups[names[i]] = m[i]
		}
	}
	return groups, true
}

// Compile turns a mapping rule into a matcher regexp.
func Compile(kind, pattern string) (*regexp.Regexp, error) {
	switch kind {
	case "exact":
		return regexp.Compile("^" + regexp.QuoteMeta(pattern) + "$")
	case "prefix":
		return regexp.Compile("^" + regexp.QuoteMeta(pattern) + "(.*)$")
	case "glob":
		return regexp.Compile("^" + globToRegex(pattern) + "$")
	case "regex":
		return regexp.Compile(pattern)
	default:
		return nil, domain.ErrInvalidRequest("mapping kind must be exact|prefix|glob|regex")
	}
}

// globToRegex converts a shell-style glob into a regex with capture groups per '*'.
func globToRegex(pattern string) string {
	var sb strings.Builder
	for _, r := range pattern {
		if r == '*' {
			sb.WriteString("(.*)")
			continue
		}
		sb.WriteString(regexp.QuoteMeta(string(r)))
	}
	return sb.String()
}

// applyTemplate expands {model} and capture-group placeholders.
func applyTemplate(tmpl, model string, groups map[string]string) string {
	if tmpl == "" {
		return ""
	}
	out := strings.ReplaceAll(tmpl, "{model}", model)
	for k, v := range groups {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

// ApplyTemplate is the exported form used by the router for route-level upstream names.
func ApplyTemplate(tmpl, model string, groups map[string]string) string {
	return applyTemplate(tmpl, model, groups)
}

// ValidateRule checks a mapping rule at write time (used by the admin API).
func ValidateRule(kind, pattern, targetModel string, targetProviderID int64) error {
	switch kind {
	case "exact", "prefix", "glob", "regex":
	default:
		return domain.ErrInvalidRequest("kind must be exact|prefix|glob|regex")
	}
	if pattern == "" {
		return domain.ErrInvalidRequest("pattern must not be empty")
	}
	if _, err := Compile(kind, pattern); err != nil {
		return domain.ErrInvalidRequest("invalid pattern: " + err.Error())
	}
	if targetModel == "" && targetProviderID == 0 {
		return domain.ErrInvalidRequest("mapping requires target_model or target_provider_id")
	}
	return nil
}

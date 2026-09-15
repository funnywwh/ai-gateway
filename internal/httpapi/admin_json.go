package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/modelmap"
	"github.com/winger/ai-gateway/internal/providers"
)

// portReady returns a configured port or writes 501 and reports false. Every port
// in Deps is an interface, so a nil interface means "feature not wired here".
func portReady[T any](w http.ResponseWriter, port T, label string) (T, bool) {
	if any(port) == nil {
		writeAPIError(w, domain.ErrUnsupported(label+" is disabled in this deployment"))
		var zero T
		return zero, false
	}
	return port, true
}

// portReadyNoWrite is portReady for internal helpers that must not write a response.
func portReadyNoWrite[T any](port T) (T, bool) {
	if any(port) == nil {
		var zero T
		return zero, false
	}
	return port, true
}

var resourceNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validResourceName(name string) bool { return resourceNameRE.MatchString(name) }

// jsonObjectString validates an optional JSON object field and returns the string to
// store. An absent or null field yields an empty string, which downstream readers
// treat as "no configuration".
func jsonObjectString(raw json.RawMessage, field string) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if !json.Valid([]byte(trimmed)) {
		return "", domain.ErrInvalidRequest(field + " is not valid JSON")
	}
	if trimmed[0] != '{' {
		return "", domain.ErrInvalidRequest(field + " must be a JSON object")
	}
	return trimmed, nil
}

// jsonArrayString validates an optional JSON array field (aliases, event filters).
func jsonArrayString(raw json.RawMessage, field string) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if !json.Valid([]byte(trimmed)) {
		return "", domain.ErrInvalidRequest(field + " is not valid JSON")
	}
	if trimmed[0] != '[' {
		return "", domain.ErrInvalidRequest(field + " must be a JSON array")
	}
	return trimmed, nil
}

// modelReasoningString validates the model-level reasoning override. Omission is
// handled by the caller; null explicitly clears the independent stored setting.
func modelReasoningString(raw json.RawMessage) (string, error) {
	value, err := jsonObjectString(raw, "reasoning")
	if err != nil || value == "" {
		return value, err
	}
	if _, err := domain.ParseModelReasoning(value); err != nil {
		return "", domain.ErrInvalidRequest(err.Error())
	}
	return value, nil
}

func validateProviderKind(kind string) error {
	if providers.IsBuiltin(kind) {
		return nil
	}
	suffix := strings.TrimPrefix(kind, "plugin:")
	if suffix == kind || suffix == "" {
		return domain.ErrInvalidRequest("provider kind must be a builtin kind or plugin:<name> (known builtins: " +
			strings.Join(providers.BuiltinKinds(), ", ") + ")")
	}
	if !validResourceName(suffix) {
		return domain.ErrInvalidRequest("plugin name must match [A-Za-z0-9._-] and be at most 64 characters")
	}
	return nil
}

func validateNonNegative(field string, v *int) error {
	if v != nil && *v < 0 {
		return domain.ErrInvalidRequest(field + " must not be negative")
	}
	return nil
}

func validateDegradation(v string) error {
	if v == "" || validDegradations[v] {
		return nil
	}
	return domain.ErrInvalidRequest("degradation must be none|fail_fast|best_effort")
}

// ---------------------------------------------------------------------------
// response mappers: one place decides what the management API exposes
// ---------------------------------------------------------------------------

// accountJSON renders one account.
//
// nodeIDs/orgs are the account's organization memberships. They are passed in rather than
// looked up here because the caller has already read them once for the whole page; both are
// empty when the deployment has no organization port, which keeps the account contract stable
// (the fields are always present, just empty).
func accountJSON(a *domain.Account, nodeIDs []int64, orgs []map[string]any) map[string]any {
	if nodeIDs == nil {
		nodeIDs = []int64{}
	}
	if orgs == nil {
		orgs = []map[string]any{}
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "billing_mode": string(a.BillingMode),
		"tags":           jsonOrEmptyArray(a.TagsJSON),
		"org_node_ids":   nodeIDs,
		"org_nodes":      orgs,
		"balance_micros": a.BalanceMicros, "credit_limit_micros": a.CreditLimitMicros,
		"low_balance_threshold_micros": a.LowBalanceThresholdMicros,
		"overdraft_limit_micros":       a.OverdraftLimitMicros,
		"markup_override_bp":           a.MarkupOverrideBP,
		"auto_suspend":                 a.AutoSuspend, "auto_resume": a.AutoResume,
		"inflight_policy_override": a.InflightPolicyOverride,
		"price_overrides":          jsonOrNil(a.PriceOverridesJSON),
		"status":                   a.Status, "note": a.Note,
		"created_at": a.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": a.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func providerJSON(p *domain.Provider, credentialKeys []string) map[string]any {
	if credentialKeys == nil {
		credentialKeys = []string{}
	}
	return map[string]any{
		"id": p.ID, "name": p.Name, "kind": p.Kind, "display_name": p.DisplayName,
		"enabled": p.Enabled, "draining": p.Draining,
		"priority": p.Priority, "weight": p.Weight, "max_inflight": p.MaxInflight,
		"degradation": p.Degradation, "state_dir": p.StateDir,
		"config_version": p.ConfigVersion,
		"config":         jsonOrNil(p.ConfigJSON), "meta": jsonOrNil(p.MetaJSON),
		"timeout_overrides": jsonOrNil(p.TimeoutOverrides),
		"discovered":        jsonOrNil(p.DiscoveredJSON), "health": jsonOrNil(p.HealthJSON),
		"last_error": p.LastError, "cooldown_until": timeOrNil(p.CooldownUntil),
		"has_credentials": len(p.CredentialsEnc) > 0, "credential_keys": credentialKeys,
		"updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// providerDetailJSON is providerJSON plus the configuration documentation of the
// kind. The list endpoint stays lean; the detail endpoint is what the console
// renders as a field table, which is how an operator learns what may be configured
// and where the API key belongs.
func providerDetailJSON(p *domain.Provider, credentialKeys []string) map[string]any {
	out := providerJSON(p, credentialKeys)
	for key, value := range providerDocsJSON(p) {
		out[key] = value
	}
	return out
}

// providerDocsJSON describes the configuration surface of one provider's kind.
//
// Builtin kinds carry their schema in this binary, so this is free of side effects.
// A plugin kind can only be described by the plugin itself, during a handshake, so
// the schemas of the last recorded probe are reused when present — never by
// starting a process here (opening a page must not manage process lifetimes).
func providerDocsJSON(p *domain.Provider) map[string]any {
	ks := providers.SchemaFor(p.Kind)
	out := map[string]any{
		"schema_source":      ks.Source,
		"kind_note":          ks.Note,
		"config_schema":      jsonOrNil(string(ks.Config)),
		"credentials_schema": jsonOrNil(string(ks.Credentials)),
		"config_template":    jsonOrNil(string(ks.Template)),
	}
	if ks.Source == providers.SchemaSourceBuiltin || len(p.DiscoveredJSON) == 0 {
		return out
	}
	var discovered struct {
		ConfigSchema      json.RawMessage `json:"config_schema"`
		CredentialsSchema json.RawMessage `json:"credentials_schema"`
	}
	if err := json.Unmarshal([]byte(p.DiscoveredJSON), &discovered); err != nil {
		return out
	}
	if schema := nonNullJSON(discovered.ConfigSchema); schema != nil {
		out["config_schema"] = schema
	}
	if schema := nonNullJSON(discovered.CredentialsSchema); schema != nil {
		out["credentials_schema"] = schema
	}
	return out
}

// nonNullJSON drops the JSON literal null: a probe stores it whenever the plugin
// declared no schema, and a null schema would read as "no documentation exists"
// rather than "not declared yet" in the console.
func nonNullJSON(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	return raw
}

func providerModelJSON(pm *domain.ProviderModel) map[string]any {
	return map[string]any{
		"id": pm.ID, "provider_id": pm.ProviderID,
		"public_model": pm.PublicModel, "upstream_model": pm.UpstreamModel,
		"enabled": pm.Enabled, "priority": pm.Priority, "weight": pm.Weight,
		"context_window": pm.ContextWindow, "max_output_tokens": pm.MaxOutputTokens,
		"capabilities":          jsonOrNil(pm.CapabilitiesJSON),
		"capabilities_override": pm.CapabilitiesOverride,
		"pricing_rules":         jsonOrNil(pm.PricingRulesJSON),
		"source":                pm.Source,
		"updated_at":            pm.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func modelJSON(m *domain.Model) map[string]any {
	return map[string]any{
		"id": m.ID, "public_name": m.PublicName, "display_name": m.DisplayName,
		"aliases": jsonOrEmptyArray(m.AliasesJSON), "enabled": m.Enabled,
		"sale_pricing": jsonOrNil(m.SalePricingJSON), "policy": jsonOrNil(m.PolicyJSON),
		"reasoning":  jsonOrNil(m.ReasoningJSON),
		"updated_at": m.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func mappingJSON(m *domain.ModelMapping) map[string]any {
	return map[string]any{
		"id": m.ID, "kind": m.Kind, "pattern": m.Pattern,
		"target_model": m.TargetModel, "target_provider_id": m.TargetProviderID,
		"target_upstream_model": m.TargetUpstreamModel,
		"priority":              m.Priority, "enabled": m.Enabled, "note": m.Note,
		"created_at": m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func routeJSON(r *domain.Route, modelName, providerName string) map[string]any {
	return map[string]any{
		"id": r.ID, "model_id": r.ModelID, "model": modelName,
		"provider_id": r.ProviderID, "provider": providerName,
		"upstream_model": r.UpstreamModel,
		"priority":       r.Priority, "weight": r.Weight, "enabled": r.Enabled,
		"policy": jsonOrNil(r.PolicyJSON), "cooldown_until": timeOrNil(r.CooldownUntil),
	}
}

func tagJSON(t *domain.Tag) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "description": t.Description,
		"grants": jsonOrNil(t.GrantsJSON), "policy": jsonOrNil(t.PolicyJSON),
		"priority":   t.Priority,
		"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func hookJSON(h *domain.Hook) map[string]any {
	return map[string]any{
		"id": h.ID, "name": h.Name, "type": h.Type, "url": h.URL,
		"has_secret": h.Secret != "", "events": jsonOrEmptyArray(h.EventsJSON),
		"include_content": h.IncludeContent, "max_bytes": h.MaxBytes,
		"sample_rate": h.SampleRate, "enabled": h.Enabled,
		"created_at": h.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func mcpTokenJSON(t *domain.MCPToken) map[string]any {
	return map[string]any{
		"id": t.ID, "account_id": t.AccountID, "name": t.Name,
		"token_prefix": t.TokenPrefix, "scope": mcpsrv.NormalizeScope(t.Scope), "status": t.Status,
		"last_used_at": timeOrNil(t.LastUsedAt), "expires_at": timeOrNil(t.ExpiresAt),
		"created_by": t.CreatedBy, "note": t.Note,
		"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// validateMappingRule delegates rule semantics (kind, pattern, template) to the
// modelmap package so the admin API cannot accept a rule the router would reject.
func validateMappingRule(kind, pattern, targetModel string, targetProviderID int64) error {
	if err := modelmap.ValidateRule(kind, pattern, targetModel, targetProviderID); err != nil {
		return domain.ErrInvalidRequest(fmt.Sprintf("invalid mapping rule: %v", err))
	}
	return nil
}

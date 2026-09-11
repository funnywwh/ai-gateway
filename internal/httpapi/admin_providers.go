package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/providers"
	"github.com/winger/ai-gateway/internal/runtime"
)

// ---------------------------------------------------------------------------
// providers
// ---------------------------------------------------------------------------

// providerBody is the shared create/update payload. Pointer fields keep the
// "absent means unchanged" semantics that PATCH needs.
type providerBody struct {
	Name             *string         `json:"name"`
	Kind             *string         `json:"kind"`
	DisplayName      *string         `json:"display_name"`
	StateDir         *string         `json:"state_dir"`
	Config           json.RawMessage `json:"config"`
	Meta             json.RawMessage `json:"meta"`
	TimeoutOverrides json.RawMessage `json:"timeout_overrides"`
	Credentials      json.RawMessage `json:"credentials"`
	Enabled          *bool           `json:"enabled"`
	Draining         *bool           `json:"draining"`
	Priority         *int            `json:"priority"`
	Weight           *int            `json:"weight"`
	MaxInflight      *int            `json:"max_inflight"`
	Degradation      *string         `json:"degradation"`
	ResetCooldown    bool            `json:"reset_cooldown"`
}

func (s *Server) credentialKeys(p *domain.Provider) []string {
	if s.deps.Secrets == nil || len(p.CredentialsEnc) == 0 {
		return []string{}
	}
	names := s.deps.Secrets.KeyNames(p.ID, p.CredentialsEnc)
	if len(names) == 0 {
		// A non-empty blob that will not decrypt means the wrong credentials_key is
		// loaded (or the record was sealed under a different provider id).
		s.deps.Log.Warn("provider credentials could not be decrypted for display", "provider", p.Name, "provider_id", p.ID)
		return []string{}
	}
	sort.Strings(names)
	return names
}

// credentialChange is a deferred credential write. Sealing cannot happen inside
// applyProviderBody because AES-GCM binds the provider id as additional data, and a
// brand-new provider has no id until the row is inserted.
type credentialChange struct {
	// Object is the canonical JSON of the credential object to seal; nil with
	// Clear set means "remove the stored credentials".
	Object []byte
	Clear  bool
}

// apply fills a provider record from the request body. It reports whether the
// credentials/config changed so the caller can bump the config version and stop
// the running plugin process.
func (s *Server) applyProviderBody(p *domain.Provider, body *providerBody, creating bool) (restart bool, creds *credentialChange, err error) {
	if body.Name != nil {
		p.Name = strings.TrimSpace(*body.Name)
	}
	if body.Kind != nil {
		p.Kind = strings.TrimSpace(*body.Kind)
	}
	if p.Name == "" {
		return false, nil, domain.ErrInvalidRequest("provider name is required")
	}
	if !validResourceName(p.Name) {
		return false, nil, domain.ErrInvalidRequest("provider name must match [A-Za-z0-9._-] and be at most 64 characters")
	}
	if err := validateProviderKind(p.Kind); err != nil {
		return false, nil, err
	}
	if body.DisplayName != nil {
		p.DisplayName = *body.DisplayName
	}
	if body.StateDir != nil {
		p.StateDir = strings.TrimSpace(*body.StateDir)
	}
	if p.StateDir == "" && s.deps.Config != nil && s.deps.Config.Plugins.StateDir != "" {
		p.StateDir = filepath.Join(s.deps.Config.Plugins.StateDir, p.Name)
	}
	if body.Config != nil {
		raw, err := jsonObjectString(body.Config, "config")
		if err != nil {
			return false, nil, err
		}
		if raw != p.ConfigJSON {
			p.ConfigJSON = raw
			restart = true
		}
	}
	if body.Meta != nil {
		raw, err := jsonObjectString(body.Meta, "meta")
		if err != nil {
			return false, nil, err
		}
		p.MetaJSON = raw
	}
	if body.TimeoutOverrides != nil {
		raw, err := jsonObjectString(body.TimeoutOverrides, "timeout_overrides")
		if err != nil {
			return false, nil, err
		}
		p.TimeoutOverrides = raw
	}
	if body.Enabled != nil {
		p.Enabled = *body.Enabled
	} else if creating {
		p.Enabled = true
	}
	if body.Draining != nil {
		p.Draining = *body.Draining
	}
	if body.Priority != nil {
		p.Priority = *body.Priority
	}
	if body.Weight != nil {
		p.Weight = *body.Weight
	}
	if body.MaxInflight != nil {
		p.MaxInflight = *body.MaxInflight
	}
	if body.Degradation != nil {
		p.Degradation = *body.Degradation
	}
	if err := validateDegradation(p.Degradation); err != nil {
		return false, nil, err
	}
	for field, v := range map[string]*int{"priority": body.Priority, "weight": body.Weight, "max_inflight": body.MaxInflight} {
		if err := validateNonNegative(field, v); err != nil {
			return false, nil, err
		}
	}
	if body.ResetCooldown {
		p.CooldownUntil = nil
	}

	if len(body.Credentials) > 0 {
		trimmed := strings.TrimSpace(string(body.Credentials))
		if trimmed != "null" {
			var obj map[string]any
			if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
				return false, nil, domain.ErrInvalidRequest("credentials must be a JSON object")
			}
			if len(obj) == 0 {
				// An empty object is the documented way to clear stored credentials.
				creds = &credentialChange{Clear: true}
			} else {
				canonical, err := json.Marshal(obj)
				if err != nil {
					return false, nil, domain.ErrInvalidRequest("credentials must be a JSON object")
				}
				creds = &credentialChange{Object: canonical}
			}
		}
	}
	return restart, creds, nil
}

// sealCredentials applies a deferred credential change. It needs the provider's
// final id, so callers must insert a new provider row before calling it.
func (s *Server) sealCredentials(p *domain.Provider, change *credentialChange) error {
	if change == nil {
		return nil
	}
	if change.Clear {
		p.CredentialsEnc = nil
		return nil
	}
	if p.ID == 0 {
		return domain.ErrInternal("cannot seal provider credentials before the provider row exists")
	}
	if s.deps.Secrets == nil || !s.deps.Secrets.Ready() {
		return domain.ErrInvalidRequest("credentials_key is not configured; set it before storing provider credentials")
	}
	sealed, err := s.deps.Secrets.Seal(p.ID, change.Object)
	if err != nil {
		return domain.ErrInvalidRequest(err.Error())
	}
	p.CredentialsEnc = sealed
	return nil
}

func (s *Server) handleAdminListProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	list, err := store.ListProviders(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, providerJSON(p, s.credentialKeys(p)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (s *Server) handleAdminGetProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	p, err := store.GetProvider(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, providerDetailJSON(p, s.credentialKeys(p)))
}

// handleAdminListProviderKinds returns the configuration documentation of every
// builtin kind, so the create form can explain a kind before any instance of it
// exists. Plugin kinds are absent on purpose: their schema only exists inside a
// handshake, and it is not worth starting a process to render a help panel.
func (s *Server) handleAdminListProviderKinds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": providers.Schemas()})
}

func (s *Server) handleAdminCreateProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	var body providerBody
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	created := true
	p := &domain.Provider{}
	if body.Name != nil {
		if existing, err := store.GetProviderByName(r.Context(), strings.TrimSpace(*body.Name)); err == nil {
			p = existing
			created = false
		}
	}
	restart, creds, err := s.applyProviderBody(p, &body, created)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if creds != nil && !creds.Clear && p.ID == 0 {
		// Credentials are sealed with the provider id as additional data, so the row
		// must exist first. This first write only assigns the id.
		if _, err := store.UpsertProvider(r.Context(), p); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	if err := s.sealCredentials(p, creds); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if restart || creds != nil {
		p.ConfigVersion++
	}
	id, err := store.UpsertProvider(r.Context(), p)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	action := "update"
	status := http.StatusOK
	if created {
		action = "create"
		status = http.StatusCreated
	}
	s.audit(r.Context(), actor.Username, action, "provider", strconv.FormatInt(id, 10), map[string]any{
		"name": p.Name, "kind": p.Kind, "enabled": p.Enabled,
		"credentials_set": len(p.CredentialsEnc) > 0,
	}, "ok")
	s.reload(r.Context(), "provider "+action, false)
	if restart && !created {
		s.restartProvider(p)
	}
	writeJSON(w, status, providerDetailJSON(p, s.credentialKeys(p)))
}

func (s *Server) handleAdminPatchProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	p, err := store.GetProvider(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body providerBody
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	restart, creds, err := s.applyProviderBody(p, &body, false)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := s.sealCredentials(p, creds); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if restart || creds != nil {
		p.ConfigVersion++
	}
	if _, err := store.UpsertProvider(r.Context(), p); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "provider", strconv.FormatInt(id, 10), map[string]any{
		"enabled": p.Enabled, "draining": p.Draining, "priority": p.Priority, "weight": p.Weight,
		"config_version": p.ConfigVersion, "credentials_set": len(p.CredentialsEnc) > 0,
	}, "ok")
	s.reload(r.Context(), "provider updated", false)
	if restart {
		s.restartProvider(p)
	}
	writeJSON(w, http.StatusOK, providerDetailJSON(p, s.credentialKeys(p)))
}

// restartProvider stops the plugin process so the next attempt picks up the new
// configuration. Failures are logged, never surfaced: the write already succeeded.
func (s *Server) restartProvider(p *domain.Provider) {
	if s.deps.Prober == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.deps.Prober.Restart(ctx, p.ID); err != nil {
		s.deps.Log.Warn("restarting provider after a configuration change failed", "provider", p.Name, "err", err)
	}
}

func (s *Server) handleAdminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	if _, err := store.GetProvider(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	force := r.URL.Query().Get("force") == "true"
	if routes, err := s.routesForProvider(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	} else if len(routes) > 0 && !force {
		writeAPIError(w, domain.ErrConflict(fmt.Sprintf(
			"provider %d is referenced by %d route(s); disable it or delete with ?force=true", id, len(routes))))
		return
	}
	if err := store.DeleteProvider(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "provider", strconv.FormatInt(id, 10),
		map[string]any{"force": force}, "ok")
	if s.deps.Prober != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.deps.Prober.Restart(ctx, id)
		cancel()
	}
	s.reload(r.Context(), "provider deleted", true)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true, "cascaded_routes": force})
}

// routesForProvider counts route references (used for the delete guard).
func (s *Server) routesForProvider(ctx context.Context, providerID int64) ([]*domain.Route, error) {
	store, ok := portReadyNoWrite(s.deps.Models)
	if !ok {
		return nil, nil
	}
	all, err := store.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	out := []*domain.Route{}
	for _, route := range all {
		if route.ProviderID == providerID {
			out = append(out, route)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// provider probe / discovery / actions / logs
// ---------------------------------------------------------------------------

func (s *Server) handleAdminProbeProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	prober, ok := portReady(w, s.deps.Prober, "provider probing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	mode := r.URL.Query().Get("mode")
	var body struct {
		Mode string `json:"mode"`
	}
	if r.Method == http.MethodPost && r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Mode != "" {
			mode = body.Mode
		}
	}
	switch mode {
	case "", "health", "models", "info":
	default:
		writeAPIError(w, domain.ErrInvalidRequest("mode must be health, models or info"))
		return
	}

	// A probe may be a real upstream call, not just a status ping: the codex
	// adapter probes with a streaming completion, measured at 9.6-16.4s (worst
	// sample 18.8s) through a cross-border egress, plus plugin start and token
	// refresh. The previous 10s deadline made a healthy provider look broken.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res := prober.Probe(ctx, id, mode)
	s.recordProbe(r.Context(), store, id, res)
	result := "ok"
	if !res.OK {
		result = "failed"
	}
	s.audit(r.Context(), actor.Username, "probe", "provider", strconv.FormatInt(id, 10),
		map[string]any{"mode": res.Mode, "ok": res.OK, "latency_ms": res.LatencyMS, "error": res.Error}, result)
	writeJSON(w, http.StatusOK, res)
}

// recordProbe persists the last probe outcome so the UI can show it without
// re-running the probe. Failures to persist must not fail the request.
func (s *Server) recordProbe(ctx context.Context, store ProviderAdmin, id int64, res *runtime.ProbeResult) {
	if store == nil || res == nil {
		return
	}
	discovered, _ := json.Marshal(map[string]any{
		"mode": res.Mode, "ok": res.OK, "at": time.Now().UTC().Format(time.RFC3339),
		"info": res.Info, "actions": res.Actions, "models": len(res.Models),
		"config_schema": res.ConfigSchema, "credentials_schema": res.CredentialsSchema,
	})
	health, _ := json.Marshal(map[string]any{
		"ok": res.OK, "latency_ms": res.LatencyMS, "error": res.Error,
		"at": time.Now().UTC().Format(time.RFC3339),
	})
	if err := store.SetProviderDiscovered(ctx, id, string(discovered), string(health), res.Error); err != nil {
		s.deps.Log.Warn("persisting provider probe result failed", "provider", id, "err", err)
	}
}

func (s *Server) handleAdminRefreshProviderModels(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	prober, ok := portReady(w, s.deps.Prober, "provider probing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res := prober.Probe(ctx, id, "models")
	s.recordProbe(r.Context(), store, id, res)
	if !res.OK {
		s.audit(r.Context(), actor.Username, "refresh", "provider_model", strconv.FormatInt(id, 10),
			map[string]any{"error": res.Error}, "failed")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": res.Error, "added": 0, "kept": 0})
		return
	}
	existing, err := store.ListProviderModels(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	byName := map[string]*domain.ProviderModel{}
	for _, pm := range existing {
		byName[pm.PublicModel] = pm
	}
	added, kept := 0, 0
	for _, m := range res.Models {
		name := strings.TrimSpace(m.ID)
		if name == "" {
			continue
		}
		pm := byName[name]
		if pm == nil {
			pm = &domain.ProviderModel{
				ProviderID: id, PublicModel: name, Enabled: true,
				Priority: 100, Weight: 100, Source: "discovered",
			}
			added++
		} else {
			kept++
		}
		// Discovery only fills blanks: manual names, weights and overrides win.
		if pm.UpstreamModel == "" {
			pm.UpstreamModel = firstNonEmpty(m.UpstreamModel, m.ID)
		}
		if pm.ContextWindow == 0 {
			pm.ContextWindow = m.ContextWindow
		}
		if pm.MaxOutputTokens == 0 {
			pm.MaxOutputTokens = m.MaxOutputTokens
		}
		if pm.CapabilitiesJSON == "" && len(m.Capabilities) > 0 {
			if raw, err := json.Marshal(m.Capabilities); err == nil {
				pm.CapabilitiesJSON = string(raw)
			}
		}
		if pm.PricingRulesJSON == "" && len(m.PricingRules) > 0 {
			pm.PricingRulesJSON = string(m.PricingRules)
		}
		if _, err := store.UpsertProviderModel(r.Context(), pm); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	s.audit(r.Context(), actor.Username, "refresh", "provider_model", strconv.FormatInt(id, 10),
		map[string]any{"discovered": len(res.Models), "added": added, "kept": kept}, "ok")
	s.reload(r.Context(), "provider models refreshed", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "discovered": len(res.Models), "added": added, "kept": kept,
		"latency_ms": res.LatencyMS,
	})
}

func (s *Server) handleAdminListProviderModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	providerID := int64(0)
	if raw := r.PathValue("id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
			return
		}
		providerID = parsed
	}
	rows, err := store.ListProviderModels(r.Context(), providerID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, pm := range rows {
		out = append(out, providerModelJSON(pm))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (s *Server) handleAdminUpsertProviderModel(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	providerID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	if _, err := store.GetProvider(r.Context(), providerID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body struct {
		PublicModel          string          `json:"public_model"`
		UpstreamModel        string          `json:"upstream_model"`
		Enabled              *bool           `json:"enabled"`
		Priority             *int            `json:"priority"`
		Weight               *int            `json:"weight"`
		ContextWindow        *int            `json:"context_window"`
		MaxOutputTokens      *int            `json:"max_output_tokens"`
		Capabilities         json.RawMessage `json:"capabilities"`
		CapabilitiesOverride string          `json:"capabilities_override"`
		PricingRules         json.RawMessage `json:"pricing_rules"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if strings.TrimSpace(body.PublicModel) == "" {
		writeAPIError(w, domain.ErrInvalidRequest("public_model is required"))
		return
	}
	pm := &domain.ProviderModel{
		ProviderID: providerID, PublicModel: strings.TrimSpace(body.PublicModel),
		UpstreamModel: firstNonEmpty(strings.TrimSpace(body.UpstreamModel), strings.TrimSpace(body.PublicModel)),
		Enabled:       true, Priority: 100, Weight: 100, Source: "manual",
	}
	if body.Enabled != nil {
		pm.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		pm.Priority = *body.Priority
	}
	if body.Weight != nil {
		pm.Weight = *body.Weight
	}
	if body.ContextWindow != nil {
		pm.ContextWindow = *body.ContextWindow
	}
	if body.MaxOutputTokens != nil {
		pm.MaxOutputTokens = *body.MaxOutputTokens
	}
	if body.CapabilitiesOverride != "" {
		if !validCapabilityOverride(body.CapabilitiesOverride) {
			writeAPIError(w, domain.ErrInvalidRequest("capabilities_override must be inherit, strip or reject"))
			return
		}
		pm.CapabilitiesOverride = body.CapabilitiesOverride
	}
	if raw, err := jsonObjectString(body.Capabilities, "capabilities"); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	} else {
		pm.CapabilitiesJSON = raw
	}
	if raw, err := jsonObjectString(body.PricingRules, "pricing_rules"); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	} else {
		pm.PricingRulesJSON = raw
	}
	if err := validateNonNegative("priority", body.Priority); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := validateNonNegative("weight", body.Weight); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	id, err := store.UpsertProviderModel(r.Context(), pm)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	payload := providerModelJSON(pm)
	if modelStore, ok := portReadyNoWrite(s.deps.Models); ok {
		if _, err := modelStore.GetModelByName(r.Context(), pm.PublicModel); err != nil {
			payload["warning"] = "no canonical model with this public name exists yet; add one so it becomes routable"
		}
	}
	s.audit(r.Context(), actor.Username, "update", "provider_model", strconv.FormatInt(id, 10),
		map[string]any{"provider_id": providerID, "public_model": pm.PublicModel, "enabled": pm.Enabled}, "ok")
	s.reload(r.Context(), "provider model upserted", true)
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAdminDeleteProviderModel(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider model id"))
		return
	}
	if err := store.DeleteProviderModel(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "provider_model", strconv.FormatInt(id, 10), nil, "ok")
	s.reload(r.Context(), "provider model deleted", true)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (s *Server) handleAdminProviderActions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	prober, ok := portReady(w, s.deps.Prober, "provider probing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	actions, err := prober.Actions(ctx, id)
	if err != nil {
		writeAPIError(w, domain.ErrUpstream(http.StatusBadGateway, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": actions, "count": len(actions)})
}

func (s *Server) handleAdminRunProviderAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	prober, ok := portReady(w, s.deps.Prober, "provider probing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeAPIError(w, domain.ErrInvalidRequest("action name is required"))
		return
	}
	var input json.RawMessage
	if raw := r.URL.Query().Get("params"); raw != "" {
		input = json.RawMessage(raw)
	} else if r.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(strings.TrimSpace(string(body))) > 0 {
			input = json.RawMessage(body)
		}
	}
	if len(input) > 0 && !json.Valid(input) {
		writeAPIError(w, domain.ErrInvalidRequest("action params must be valid JSON"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out, err := prober.RunAction(ctx, id, name, input)
	if err != nil {
		s.audit(r.Context(), actor.Username, "action", "provider", strconv.FormatInt(id, 10),
			map[string]any{"name": name, "error": err.Error()}, "failed")
		writeAPIError(w, domain.ErrUpstream(http.StatusBadGateway, err.Error()))
		return
	}
	s.audit(r.Context(), actor.Username, "action", "provider", strconv.FormatInt(id, 10),
		map[string]any{"name": name}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"action": name, "result": json.RawMessage(out)})
}

func (s *Server) handleAdminProviderLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	prober, ok := portReady(w, s.deps.Prober, "provider probing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	lines, running, err := prober.Logs(r.Context(), id, adminLimit(r, 200, 2000))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": lines, "count": len(lines), "running": running})
}

func (s *Server) handleAdminRestartProvider(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	prober, ok := portReady(w, s.deps.Prober, "provider probing")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid provider id"))
		return
	}
	if err := prober.Restart(r.Context(), id); err != nil {
		writeAPIError(w, domain.ErrUpstream(http.StatusBadGateway, err.Error()))
		return
	}
	s.audit(r.Context(), actor.Username, "restart", "provider", strconv.FormatInt(id, 10), nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "restarted": true})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func validCapabilityOverride(v string) bool {
	switch v {
	case "inherit", "strip", "reject":
		return true
	}
	return false
}

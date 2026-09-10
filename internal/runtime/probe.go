// Probe support: the management API needs to ask a provider whether it is alive,
// which models it serves, what interactive actions it offers, and what its process
// recently logged. Keeping this here means httpapi never imports pluginhost and the
// hot path stays untouched.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/providers"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Probe modes.
const (
	ProbeHealth = "health"
	ProbeModels = "models"
	ProbeInfo   = "info"
)

// ProbeResult is the outcome of one provider probe. A failed probe is a business
// result, not an HTTP error: the caller returns it with status 200 and ok=false.
type ProbeResult struct {
	OK         bool                  `json:"ok"`
	Mode       string                `json:"mode"`
	ProviderID int64                 `json:"provider_id"`
	Provider   string                `json:"provider"`
	Kind       string                `json:"kind"`
	Plugin     bool                  `json:"plugin"`
	Binary     string                `json:"binary,omitempty"`
	LatencyMS  int64                 `json:"latency_ms"`
	Info       *pluginapi.Info       `json:"info,omitempty"`
	Models     []pluginapi.ModelInfo `json:"models,omitempty"`
	Actions    []pluginapi.Action    `json:"actions,omitempty"`
	// ConfigSchema/CredentialsSchema let the admin UI render a plugin-provided
	// configuration form instead of falling back to raw JSON.
	ConfigSchema      json.RawMessage `json:"config_schema,omitempty"`
	CredentialsSchema json.RawMessage `json:"credentials_schema,omitempty"`
	Note              string          `json:"note,omitempty"`
	Error             string          `json:"error,omitempty"`
}

// Probe exercises one provider out of band. mode selects what is exercised;
// health is the default. The caller owns the deadline (ctx).
func (d *Dispatcher) Probe(ctx context.Context, providerID int64, mode string) *ProbeResult {
	if mode == "" {
		mode = ProbeHealth
	}
	res := &ProbeResult{Mode: mode, ProviderID: providerID}
	provider, err := d.provider(ctx, providerID)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Provider = provider.Name
	res.Kind = provider.Kind
	res.Plugin = !providers.IsBuiltin(provider.Kind)
	if res.Plugin {
		res.Binary = d.binaryFor(provider)
	}

	started := time.Now()
	defer func() { res.LatencyMS = time.Since(started).Milliseconds() }()

	if res.Plugin {
		client, err := d.pluginClient(ctx, provider)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		hs := client.Handshake()
		info := pluginapi.Info{Name: hs.Name, Version: hs.Version, Capabilities: hs.Capabilities}
		res.Info = &info
		res.Actions = hs.Capabilities.Actions
		res.ConfigSchema = hs.ConfigSchema
		res.CredentialsSchema = hs.CredentialsSchema
		switch mode {
		case ProbeModels:
			if !hs.Capabilities.ListModels {
				res.Note = "plugin does not declare list_models"
				res.OK = true
				return res
			}
			models, err := client.ListModels(ctx)
			if err != nil {
				res.Error = err.Error()
				return res
			}
			res.Models = models
		case ProbeInfo:
		default:
			if !hs.Capabilities.Health {
				res.Note = "plugin does not declare health"
				res.OK = true
				return res
			}
			if err := client.Health(ctx); err != nil {
				res.Error = err.Error()
				return res
			}
		}
		res.OK = true
		return res
	}

	builtin, err := d.builtin(provider)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	info := builtin.Info()
	res.Info = &info
	res.Actions = builtin.Actions()
	switch mode {
	case ProbeModels:
		models, err := builtin.ListModels(ctx)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		res.Models = models
	case ProbeInfo:
	default:
		if err := builtin.Health(ctx); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	res.OK = true
	return res
}

// Actions lists the interactive actions one provider exposes.
func (d *Dispatcher) Actions(ctx context.Context, providerID int64) ([]pluginapi.Action, error) {
	provider, err := d.provider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !providers.IsBuiltin(provider.Kind) {
		client, err := d.pluginClient(ctx, provider)
		if err != nil {
			return nil, err
		}
		return handshakeActions(client), nil
	}
	builtin, err := d.builtin(provider)
	if err != nil {
		return nil, err
	}
	return builtin.Actions(), nil
}

// RunAction executes one provider action (interactive login, token refresh, ...).
func (d *Dispatcher) RunAction(ctx context.Context, providerID int64, name string, in json.RawMessage) (json.RawMessage, error) {
	if name == "" {
		return nil, domain.ErrInvalidRequest("action name is required")
	}
	provider, err := d.provider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !providers.IsBuiltin(provider.Kind) {
		client, err := d.pluginClient(ctx, provider)
		if err != nil {
			return nil, err
		}
		return client.RunAction(ctx, name, in)
	}
	builtin, err := d.builtin(provider)
	if err != nil {
		return nil, err
	}
	return builtin.RunAction(ctx, name, in)
}

// Logs returns recent stderr lines of a plugin process. A provider that is not
// running (or is builtin) reports running=false with no lines instead of failing.
func (d *Dispatcher) Logs(ctx context.Context, providerID int64, tail int) ([]string, bool, error) {
	provider, err := d.provider(ctx, providerID)
	if err != nil {
		return nil, false, err
	}
	if providers.IsBuiltin(provider.Kind) || d.host == nil {
		return []string{}, false, nil
	}
	proc, ok := d.host.Get(provider.Name)
	if !ok {
		return []string{}, false, nil
	}
	if tail <= 0 || tail > 2000 {
		tail = 200
	}
	return proc.LogTail(tail), true, nil
}

// Restart stops the plugin process so the next attempt starts it fresh with the
// current configuration. A stopped process always restarts lazily, so this is
// synchronous and cheap.
func (d *Dispatcher) Restart(ctx context.Context, providerID int64) error {
	provider, err := d.provider(ctx, providerID)
	if err != nil {
		return err
	}
	if providers.IsBuiltin(provider.Kind) {
		d.mu.Lock()
		delete(d.builtins, provider.ID)
		d.mu.Unlock()
		return nil
	}
	if d.host == nil {
		return nil
	}
	if _, ok := d.host.Get(provider.Name); !ok {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := d.host.Stop(stopCtx, provider.Name); err != nil {
		return fmt.Errorf("stopping plugin %s: %w", provider.Name, err)
	}
	return nil
}

func handshakeActions(c *pluginapi.Client) []pluginapi.Action {
	if c == nil {
		return nil
	}
	return c.Handshake().Capabilities.Actions
}

// Package runtime executes provider attempts: it dispatches to in-process builtin
// providers or to plugin subprocesses, and keeps the balancer's runtime state current.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/balancer"
	"github.com/winger/ai-gateway/internal/creds"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/pluginhost"
	"github.com/winger/ai-gateway/internal/providers"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Store is the persistence subset the dispatcher needs.
type Store interface {
	GetProvider(ctx context.Context, id int64) (*domain.Provider, error)
	SetRouteCooldown(ctx context.Context, id int64, until *time.Time) error
}

// Config configures the dispatcher.
type Config struct {
	CredentialsKey  []byte
	CooldownDefault time.Duration

	// QueueWait bounds how long one attempt waits for a provider capacity slot
	// (providers.max_inflight). Zero or negative disables queueing: an attempt that
	// finds the provider full fails immediately, which is what a deployment that
	// prefers fast failover over waiting wants.
	QueueWait time.Duration
	// QueueMaxWaiters bounds how many attempts may queue for one provider. Zero or
	// negative means the queue depth is bounded only by QueueWait.
	QueueMaxWaiters int
}

// Attempt carries the gateway-side facts of one dispatch that the provider answer itself
// does not. QueueWaitMS is how long the attempt waited for a provider capacity slot before
// the upstream call started; a caller subtracts it from its own wall-clock measurement so
// recorded latency keeps meaning upstream latency rather than gateway queueing.
type Attempt struct{ QueueWaitMS int }

// Dispatcher runs attempts.
type Dispatcher struct {
	cfg   Config
	store Store
	reg   *registry.Registry
	host  *pluginhost.Host
	bal   *balancer.State
	log   *slog.Logger
	// gate enforces providers.max_inflight with a FIFO waiting line.
	gate *gate

	mu       sync.Mutex
	builtins map[int64]*builtinEntry
}

type builtinEntry struct {
	provider pluginapi.Provider
	version  int
}

// New builds a dispatcher. host may be nil when only builtin providers are used.
func New(cfg Config, store Store, reg *registry.Registry, host *pluginhost.Host, bal *balancer.State, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	if cfg.CooldownDefault <= 0 {
		cfg.CooldownDefault = 30 * time.Minute
	}
	if cfg.QueueMaxWaiters < 0 {
		cfg.QueueMaxWaiters = 0
	}
	return &Dispatcher{
		cfg:      cfg,
		store:    store,
		reg:      reg,
		host:     host,
		bal:      bal,
		log:      log,
		gate:     newGate(cfg),
		builtins: map[int64]*builtinEntry{},
	}
}

// ProviderKey is the balancer/runtime key for one provider.
func ProviderKey(providerID int64) string { return fmt.Sprintf("provider:%d", providerID) }

// Complete performs one non-streaming attempt.
func (d *Dispatcher) Complete(ctx context.Context, providerID int64, req *pluginapi.Request) (*pluginapi.Response, Attempt, error) {
	provider, permit, err := d.admit(ctx, providerID)
	if err != nil {
		return nil, Attempt{}, err
	}
	waited := permit.waitedFor()
	defer permit.Release()

	key := ProviderKey(providerID)
	d.bal.Acquire(key)
	started := time.Now()
	defer d.bal.Release(key)

	resp, err := d.complete(ctx, provider, req)
	d.bal.Observe(key, float64(time.Since(started).Milliseconds()), err == nil)
	return resp, Attempt{QueueWaitMS: int(waited.Milliseconds())}, err
}

// Stream performs one streaming attempt. emit is called for every canonical event.
func (d *Dispatcher) Stream(ctx context.Context, providerID int64, req *pluginapi.Request, emit func(pluginapi.Event) error) (*pluginapi.StreamEnd, Attempt, error) {
	provider, permit, err := d.admit(ctx, providerID)
	if err != nil {
		return nil, Attempt{}, err
	}
	waited := permit.waitedFor()
	defer permit.Release()

	key := ProviderKey(providerID)
	d.bal.Acquire(key)
	started := time.Now()
	defer d.bal.Release(key)

	end, err := d.stream(ctx, provider, req, emit)
	d.bal.Observe(key, float64(time.Since(started).Milliseconds()), err == nil)
	return end, Attempt{QueueWaitMS: int(waited.Milliseconds())}, err
}

// admit loads the provider record and waits for a capacity slot. The gate is entered
// *before* the balancer's in-flight counter, so requests that are only waiting are not
// counted as load by least_inflight or by the latency EWMA — they have not touched the
// upstream yet.
func (d *Dispatcher) admit(ctx context.Context, providerID int64) (*domain.Provider, *permit, error) {
	provider, err := d.provider(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	permit, err := d.gate.acquire(ctx, provider.ID, provider.MaxInflight, provider.Name)
	if err != nil {
		return nil, nil, err
	}
	return provider, permit, nil
}

// CapacityStats reports the live provider capacity gates, keyed by provider id.
func (d *Dispatcher) CapacityStats() map[int64]CapacityStat { return d.gate.stats() }

// CapacityPolicy reports the queue policy the data path applies, so an operator (or an
// agent through admin_stats) can tell how long a queued request waits without reading the
// configuration file.
func (d *Dispatcher) CapacityPolicy() CapacityPolicy {
	waitS := 0
	if d.cfg.QueueWait > 0 {
		waitS = int(d.cfg.QueueWait / time.Second)
		if waitS < 1 {
			waitS = 1
		}
	}
	return CapacityPolicy{QueueWaitS: waitS, QueueMaxWaiters: d.cfg.QueueMaxWaiters}
}

// SyncLimits pushes the ceilings of the current registry snapshot into the gates. The data
// path already reads the limit per attempt (so a change lands on the next request); this
// is what makes a *raised* limit release the requests already waiting, instead of leaving
// them queued until the next release. Called by the composition root after a reload.
func (d *Dispatcher) SyncLimits() {
	snap := d.reg.Snapshot()
	if snap == nil {
		return
	}
	for _, provider := range snap.Providers {
		d.gate.setLimit(provider.ID, provider.MaxInflight)
	}
}

func (d *Dispatcher) complete(ctx context.Context, provider *domain.Provider, req *pluginapi.Request) (*pluginapi.Response, error) {
	if providers.IsBuiltin(provider.Kind) {
		p, err := d.builtin(provider)
		if err != nil {
			return nil, err
		}
		return p.Complete(ctx, req)
	}
	client, err := d.pluginClient(ctx, provider)
	if err != nil {
		return nil, err
	}
	return client.Complete(ctx, req)
}

func (d *Dispatcher) stream(ctx context.Context, provider *domain.Provider, req *pluginapi.Request, emit func(pluginapi.Event) error) (*pluginapi.StreamEnd, error) {
	if providers.IsBuiltin(provider.Kind) {
		p, err := d.builtin(provider)
		if err != nil {
			return nil, err
		}
		// Builtin providers emit events through a callback; the terminal reason
		// arrives as a finish event and is reported as the stream's end. A builtin
		// that produced no terminal event was cut off mid-answer (the upstream
		// connection died, the body was truncated, ...): reporting that as a
		// finished stream would serve a half sentence as a complete answer, so it
		// fails instead — failover when nothing was emitted yet, a client-visible
		// failure when the client already saw partial content.
		var end *pluginapi.StreamEnd
		err = p.Stream(ctx, req, func(ev pluginapi.Event) error {
			if ev.Type == pluginapi.EventFinish {
				end = &pluginapi.StreamEnd{FinishReason: ev.Reason}
				return nil
			}
			return emit(ev)
		})
		if err != nil {
			return nil, err
		}
		if end == nil {
			return nil, pluginapi.NewRetryableError("upstream_stream_incomplete",
				"the upstream stream ended without reporting why it stopped", 502)
		}
		return end, nil
	}
	client, err := d.pluginClient(ctx, provider)
	if err != nil {
		return nil, err
	}
	return client.Stream(ctx, req, emit)
}

// provider loads a provider record (snapshot first, database fallback).
func (d *Dispatcher) provider(ctx context.Context, providerID int64) (*domain.Provider, error) {
	if snap := d.reg.Snapshot(); snap != nil {
		if p := snap.ProviderByID[providerID]; p != nil {
			return p, nil
		}
	}
	return d.store.GetProvider(ctx, providerID)
}

// builtin returns (and caches) the in-process provider for a builtin kind.
func (d *Dispatcher) builtin(provider *domain.Provider) (pluginapi.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if entry, ok := d.builtins[provider.ID]; ok && entry.version == provider.ConfigVersion {
		return entry.provider, nil
	}
	creds, err := d.credentials(provider)
	if err != nil {
		return nil, err
	}
	p, err := providers.Build(provider.Kind, provider.Name, provider.ConfigJSON, provider.StateDir, creds)
	if err != nil {
		return nil, domain.ErrInvalidRequest(err.Error())
	}
	d.builtins[provider.ID] = &builtinEntry{provider: p, version: provider.ConfigVersion}
	return p, nil
}

// pluginClient returns the running plugin client, starting the process on demand.
func (d *Dispatcher) pluginClient(ctx context.Context, provider *domain.Provider) (*pluginapi.Client, error) {
	if d.host == nil {
		return nil, pluginapi.NewError("no_plugin_host", "plugin host is not configured")
	}
	if proc, ok := d.host.Get(provider.Name); ok && !proc.Draining() {
		return proc.Client, nil
	}
	creds, err := d.credentials(provider)
	if err != nil {
		return nil, err
	}
	proc, err := d.host.Start(ctx, pluginhost.Options{
		Instance:    provider.Name,
		Kind:        provider.Kind,
		Binary:      d.binaryFor(provider),
		ConfigJSON:  provider.ConfigJSON,
		Credentials: creds,
		AutoRestart: true,
	})
	if err != nil {
		return nil, pluginapi.NewRetryableError("plugin_start_failed", err.Error(), 0)
	}
	return proc.Client, nil
}

// binaryFor resolves the plugin executable for a provider kind (plugin:<name>).
func (d *Dispatcher) binaryFor(provider *domain.Provider) string {
	name := strings.TrimPrefix(provider.Kind, "plugin:")
	if name == "" {
		name = provider.Name
	}
	if d.host == nil {
		return name
	}
	return d.host.ResolveBinary(name)
	// The host caches directory scans for 30s, so this stays cheap on the hot path.
}

// credentials decrypts the provider credentials (empty when none are configured).
func (d *Dispatcher) credentials(provider *domain.Provider) (map[string]string, error) {
	if len(provider.CredentialsEnc) == 0 {
		return nil, nil
	}
	if len(d.cfg.CredentialsKey) == 0 {
		return nil, domain.ErrInvalidRequest("credentials_key is not configured; cannot decrypt provider credentials")
	}
	plaintext, err := creds.Decrypt(d.cfg.CredentialsKey, provider.ID, provider.CredentialsEnc)
	if err != nil {
		return nil, domain.ErrInvalidRequest(err.Error())
	}
	if len(plaintext) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal(plaintext, &out); err != nil {
		return nil, domain.ErrInvalidRequest("provider credentials are not a JSON object")
	}
	return out, nil
}

// Cooldown excludes a route until the given deadline and persists it so restarts
// do not resurrect an exhausted upstream.
func (d *Dispatcher) Cooldown(ctx context.Context, routeID int64, until time.Time, note string) {
	key := fmt.Sprintf("route:%d", routeID)
	d.bal.SetCooldown(key, until, note)
	if err := d.store.SetRouteCooldown(ctx, routeID, &until); err != nil {
		d.log.Warn("persisting route cooldown failed", "route", routeID, "err", err)
	}
	d.log.Info("route cooled down", "route", routeID, "until", until.UTC().Format(time.RFC3339), "note", note)
}

// CooldownForProvider applies a cooldown derived from an upstream quota error.
func (d *Dispatcher) CooldownForProvider(ctx context.Context, routeID int64, resetAt int64, note string) {
	until := time.Now().UTC().Add(d.cfg.CooldownDefault)
	if resetAt > 0 {
		if t := time.Unix(resetAt, 0).UTC(); t.After(time.Now().UTC()) {
			until = t
		}
	}
	d.Cooldown(ctx, routeID, until, note)
}

// Retryable reports whether an attempt error allows failing over to the next candidate.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if apiErr, ok := pluginapi.IsError(err); ok {
		return apiErr.Retryable
	}
	// A capacity refusal is not evidence about the upstream: the request was simply not
	// admitted here, and another candidate may have room.
	if errors.Is(err, ErrProviderBusy) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return false
}

// QuotaError extracts quota exhaustion details from an attempt error.
func QuotaError(err error) (resetAt int64, ok bool) {
	apiErr, isAPI := pluginapi.IsError(err)
	if !isAPI {
		return 0, false
	}
	if apiErr.Kind == pluginapi.KindQuotaExhausted {
		return apiErr.ResetAt, true
	}
	return 0, false
}

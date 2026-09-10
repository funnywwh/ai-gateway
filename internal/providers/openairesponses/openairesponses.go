// Package openairesponses implements a builtin provider for upstreams that speak the
// OpenAI Responses API natively (OpenAI, Azure OpenAI, vLLM Responses, subscription backends).
package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/providers/httpx"
	"github.com/winger/ai-gateway/pkg/pluginapi"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

// Config is the provider configuration.
type Config struct {
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Headers  map[string]string `json:"headers"`
	TimeoutS int               `json:"timeout_s"`
	Models   []ModelConfig     `json:"models"`
}

// ModelConfig declares one upstream model.
type ModelConfig struct {
	Public          string          `json:"public"`
	Upstream        string          `json:"upstream"`
	ContextWindow   int             `json:"context_window"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Capabilities    map[string]bool `json:"capabilities"`
}

// Provider talks to an upstream /responses endpoint.
type Provider struct {
	name     string
	cfg      Config
	client   *http.Client
	stateDir string

	mu    sync.RWMutex
	creds map[string]string
}

// New builds the provider from config JSON and credentials.
func New(name, configJSON, stateDir string, creds map[string]string) (*Provider, error) {
	cfg := Config{TimeoutS: 120}
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return nil, fmt.Errorf("openai-responses: bad config: %w", err)
		}
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai-responses: base_url is required")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.APIKey == "" && creds != nil {
		cfg.APIKey = creds["api_key"]
	}
	return &Provider{
		name:     name,
		cfg:      cfg,
		client:   httpx.ClientWithTimeout(time.Duration(cfg.TimeoutS) * time.Second),
		stateDir: stateDir,
		creds:    creds,
	}, nil
}

// Info reports capabilities.
func (p *Provider) Info() pluginapi.Info {
	return pluginapi.Info{
		Name: "openai-responses", Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{
			Complete: true, Stream: true, ListModels: true, Health: true,
			UsageDimensions: true,
		},
	}
}

// StateDir returns the state directory.
func (p *Provider) StateDir() string { return p.stateDir }

// SetCredentials receives rotated credentials.
func (p *Provider) SetCredentials(creds map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creds = creds
	if creds != nil && creds["api_key"] != "" {
		p.cfg.APIKey = creds["api_key"]
	}
}

// ListModels returns the configured catalogue.
func (p *Provider) ListModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	out := make([]pluginapi.ModelInfo, 0, len(p.cfg.Models))
	for _, m := range p.cfg.Models {
		upstream := m.Upstream
		if upstream == "" {
			upstream = m.Public
		}
		out = append(out, pluginapi.ModelInfo{
			ID: m.Public, UpstreamModel: upstream,
			ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
			Capabilities: m.Capabilities,
		})
	}
	return out, nil
}

// Health probes the upstream models endpoint.
func (p *Provider) Health(ctx context.Context) error {
	req, err := httpx.NewRequest(ctx, http.MethodGet, p.cfg.BaseURL+"/models", nil)
	if err != nil {
		return pluginapi.NewRetryableError("bad_request", err.Error(), 0)
	}
	p.applyHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return httpx.ErrorFromResponse(resp)
	}
	return nil
}

// Actions returns no interactive actions.
func (p *Provider) Actions() []pluginapi.Action { return nil }

// RunAction is unsupported.
func (p *Provider) RunAction(ctx context.Context, name string, in json.RawMessage) (json.RawMessage, error) {
	return nil, pluginapi.NewError("unknown_action", "unknown action: "+name)
}

// usageWire mirrors the Responses usage object.
type usageWire struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Details      *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
}

// responseWire mirrors the parts of a Responses object the gateway needs.
type responseWire struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Model  string            `json:"model"`
	Output []json.RawMessage `json:"output"`
	Usage  *usageWire        `json:"usage"`
	Error  *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

func (u *usageWire) dimensions() pluginapi.Usage {
	dims := map[string]int64{}
	if u == nil {
		return pluginapi.Usage{Dimensions: dims}
	}
	cached := int64(0)
	if u.Details != nil {
		cached = u.Details.CachedTokens
	}
	if cached > 0 {
		dims["input_cache_hit"] = cached
		dims["input_cache_miss"] = u.InputTokens - cached
	} else {
		dims["input"] = u.InputTokens
	}
	dims["output"] = u.OutputTokens
	return pluginapi.Usage{Dimensions: dims}
}

// Complete performs a non-streaming Responses call.
func (p *Provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	if req == nil {
		return nil, pluginapi.NewError("bad_request", "openai-responses: nil request")
	}
	body, err := p.body(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := httpx.NewRequest(ctx, http.MethodPost, p.cfg.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, pluginapi.NewError("bad_request", err.Error())
	}
	p.applyHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, pluginapi.NewRetryableError("upstream_timeout", err.Error(), 504)
		}
		return nil, pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, httpx.ErrorFromResponse(resp)
	}

	var wire responseWire
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&wire); err != nil {
		return nil, pluginapi.NewRetryableError("upstream_bad_response", err.Error(), 502)
	}
	if wire.Error != nil {
		return nil, pluginapi.NewError("upstream_error", wire.Error.Message)
	}
	return convertResponse(&wire), nil
}

// Stream performs a streaming Responses call and forwards canonical events.
func (p *Provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	if req == nil {
		return pluginapi.NewError("bad_request", "openai-responses: nil request")
	}
	body, err := p.body(req, true)
	if err != nil {
		return err
	}
	httpReq, err := httpx.NewRequest(ctx, http.MethodPost, p.cfg.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return pluginapi.NewError("bad_request", err.Error())
	}
	p.applyHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return pluginapi.NewRetryableError("upstream_timeout", err.Error(), 504)
		}
		return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return httpx.ErrorFromResponse(resp)
	}

	reader := providerkit.NewSSEReader(resp.Body, 0)
	estimator := providerkit.NewCharEstimator(0)
	var finalUsage *pluginapi.Usage

	for {
		var frame struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Text     string `json:"text"`
			ItemID   string `json:"item_id"`
			CallID   string `json:"call_id"`
			Name     string `json:"name"`
			Item     json.RawMessage `json:"item"`
			Response *responseWire   `json:"response"`
			Error    *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error,omitempty"`
		}
		ev, err := reader.NextJSON(&frame)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return pluginapi.NewRetryableError("upstream_stream_error", err.Error(), 502)
		}
		if ev.Name == "done" {
			break
		}

		switch frame.Type {
		case "response.output_text.delta":
			estimator.Add(frame.Delta)
			if err := emit(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: frame.Delta}); err != nil {
				return err
			}
		case "response.reasoning_summary_text.delta", "response.reasoning.delta":
			if err := emit(pluginapi.Event{Type: pluginapi.EventReasoningDelta, Text: frame.Delta}); err != nil {
				return err
			}
		case "response.refusal.delta":
			if err := emit(pluginapi.Event{Type: pluginapi.EventRefusalDelta, Text: frame.Delta}); err != nil {
				return err
			}
		case "response.output_item.added":
			var item struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			}
			if err := json.Unmarshal(frame.Item, &item); err == nil && item.Type == "function_call" {
				if err := emit(pluginapi.Event{
					Type: pluginapi.EventToolCallStart, CallID: item.CallID, ItemID: item.ID, Name: item.Name,
				}); err != nil {
					return err
				}
			}
		case "response.function_call_arguments.delta":
			if err := emit(pluginapi.Event{
				Type: pluginapi.EventToolArgsDelta, CallID: frame.CallID, ItemID: frame.ItemID, Text: frame.Delta,
			}); err != nil {
				return err
			}
		case "response.completed", "response.incomplete":
			if frame.Response != nil {
				u := frame.Response.Usage.dimensions()
				finalUsage = &u
			}
		case "response.failed", "error":
			msg := "upstream reported a failed response"
			if frame.Error != nil && frame.Error.Message != "" {
				msg = frame.Error.Message
			}
			return pluginapi.NewRetryableError("upstream_stream_error", msg, 502)
		}
	}

	if finalUsage == nil {
		estimated := pluginapi.Usage{
			Dimensions: map[string]int64{
				"input":  providerkit.EstimateInputTokens(req, 0),
				"output": estimator.Tokens(),
			},
			Estimated: true,
		}
		finalUsage = &estimated
	}
	return emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: finalUsage})
}

// body renders the upstream request, passing through unknown client fields.
func (p *Provider) body(req *pluginapi.Request, stream bool) ([]byte, error) {
	payload := map[string]any{}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	for k, v := range req.Extra {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	payload["stream"] = stream
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	return out, nil
}

func convertResponse(wire *responseWire) *pluginapi.Response {
	out := &pluginapi.Response{Status: wire.Status}
	if out.Status == "" {
		out.Status = "completed"
	}
	for _, raw := range wire.Output {
		var item pluginapi.Item
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		item.Extra = nil
		out.Items = append(out.Items, item)
	}
	out.Usage = wire.Usage.dimensions()
	return out
}

func (p *Provider) applyHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	p.mu.RLock()
	apiKey := p.cfg.APIKey
	headers := p.cfg.Headers
	p.mu.RUnlock()

	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}

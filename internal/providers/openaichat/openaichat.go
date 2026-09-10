// Package openaichat implements a builtin provider for OpenAI-compatible
// /chat/completions upstreams (DeepSeek, Qwen, Ollama, vLLM, LM Studio, ...).
package openaichat

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

// Config is the provider configuration (stored as JSON on the provider record).
type Config struct {
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Headers  map[string]string `json:"headers"`
	TimeoutS int               `json:"timeout_s"`
	// Models lets the operator declare the upstream catalogue when the upstream has no list endpoint.
	Models []ModelConfig `json:"models"`
}

// ModelConfig declares one upstream model.
type ModelConfig struct {
	Public          string          `json:"public"`
	Upstream        string          `json:"upstream"`
	ContextWindow   int             `json:"context_window"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Capabilities    map[string]bool `json:"capabilities"`
}

// Provider talks to an OpenAI-compatible chat-completions endpoint.
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
			return nil, fmt.Errorf("openai-chat: bad config: %w", err)
		}
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai-chat: base_url is required")
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
		Name: "openai-chat", Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{
			Complete: true, Stream: true, ListModels: true, Health: true,
			UsageDimensions: true,
		},
	}
}

// StateDir returns the state directory.
func (p *Provider) StateDir() string { return p.stateDir }

// SetCredentials receives rotated credentials from the host.
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

// Health probes the upstream with a cheap request.
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

// Complete performs a non-streaming chat completion.
func (p *Provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	chatReq, err := providerkit.ResponsesToChat(req)
	if err != nil {
		return nil, pluginapi.NewError("bad_request", err.Error())
	}
	chatReq.Stream = false

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	httpReq, err := httpx.NewRequest(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
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

	var chatResp providerkit.ChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&chatResp); err != nil {
		return nil, pluginapi.NewRetryableError("upstream_bad_response", err.Error(), 502)
	}
	out, err := providerkit.ChatResponseToResponses(&chatResp)
	if err != nil {
		return nil, pluginapi.NewError("upstream_bad_response", err.Error())
	}
	return out, nil
}

// Stream performs a streaming chat completion and translates chunks into canonical events.
func (p *Provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	chatReq, err := providerkit.ResponsesToChat(req)
	if err != nil {
		return pluginapi.NewError("bad_request", err.Error())
	}
	chatReq.Stream = true
	chatReq.StreamOptions = &providerkit.ChatStreamOptions{IncludeUsage: true}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return pluginapi.NewError("encode_error", err.Error())
	}
	httpReq, err := httpx.NewRequest(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
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

	state := providerkit.NewChatStreamState()
	reader := providerkit.NewSSEReader(resp.Body, 0)
	estimator := providerkit.NewCharEstimator(0)

	for {
		var chunk providerkit.ChatResponse
		ev, err := reader.NextJSON(&chunk)
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
		if _, err := state.Translate(&chunk, func(ev pluginapi.Event) error {
			if ev.Type == pluginapi.EventTextDelta {
				estimator.Add(ev.Text)
			}
			return emit(ev)
		}); err != nil {
			return pluginapi.NewRetryableError("upstream_stream_error", err.Error(), 502)
		}
	}

	usage := providerkit.ChatUsageToDimensions(state.Usage)
	if state.Usage == nil {
		usage = pluginapi.Usage{
			Dimensions: map[string]int64{
				"input":  providerkit.EstimateInputTokens(req, 0),
				"output": estimator.Tokens(),
			},
			Estimated: true,
		}
	}
	return emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: &usage})
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

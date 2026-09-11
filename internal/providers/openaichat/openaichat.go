// Package openaichat implements a builtin provider for OpenAI-compatible
// /chat/completions upstreams (DeepSeek, Qwen, Ollama, vLLM, LM Studio, ...).
//
// The wire dialect is OpenAI's, but deployments differ in ways that are silent
// when guessed wrong: whether a chain of thought is returned and must be replayed,
// how thinking is switched on, which response_format levels exist, and what a 402
// means. Everything beyond the common denominator is therefore an explicit opt-in,
// and the defaults keep the previous behaviour byte for byte.
package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/providers/httpx"
	"github.com/winger/ai-gateway/pkg/pluginapi"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

// Thinking modes (config `thinking.mode`).
const (
	// ThinkingAuto lets the request decide: a client asking for reasoning gets it.
	ThinkingAuto = "auto"
	// ThinkingEnabled turns the mode on even when the client did not ask.
	ThinkingEnabled = "enabled"
	// ThinkingDisabled turns the mode off even when the client did ask.
	ThinkingDisabled = "disabled"
)

// Thinking styles (config `thinking.style`), the upstream parameter shape.
const (
	// ThinkingStyleNone sends no extra field: the generic OpenAI-compatible dialect.
	ThinkingStyleNone = "none"
	// ThinkingStyleDeepSeek sends {"thinking":{"type":"enabled|disabled"}}.
	ThinkingStyleDeepSeek = "deepseek"
)

// response_format levels (config `response_format`), what the upstream really supports.
const (
	ResponseFormatText       = "text"
	ResponseFormatJSONObject = "json_object"
	ResponseFormatJSONSchema = "json_schema"
)

// quotaCooldown is how long a candidate is cooled when the upstream reports an
// exhausted balance without saying when it recovers.
const quotaCooldown = 30 * time.Minute

// maxUpstreamBody caps how much of a response body is buffered.
const maxUpstreamBody = 32 << 20

// Config is the provider configuration (stored as JSON on the provider record).
type Config struct {
	BaseURL  string            `json:"base_url"`
	APIKey   string            `json:"api_key"`
	Headers  map[string]string `json:"headers"`
	TimeoutS int               `json:"timeout_s"`
	// Models lets the operator declare the upstream catalogue when the upstream has no list endpoint.
	Models []ModelConfig `json:"models"`

	// Thinking configures the chain-of-thought dialect. The zero value sends no
	// extra field at all, which is what generic OpenAI-compatible upstreams expect.
	Thinking ThinkingConfig `json:"thinking"`
	// ResponseFormat declares the highest response_format level the upstream accepts.
	ResponseFormat string `json:"response_format"`
	// DefaultMaxOutputTokens is applied only when the client sent no max_output_tokens.
	// It bounds the in-flight reservation without overriding the upstream default
	// (DeepSeek, for example, defaults to 64K in thinking mode).
	DefaultMaxOutputTokens int `json:"default_max_output_tokens"`
}

// ThinkingConfig is the chain-of-thought dialect of the upstream.
type ThinkingConfig struct {
	// Mode is auto | enabled | disabled.
	Mode string `json:"mode"`
	// Style is none | deepseek.
	Style string `json:"style"`
	// ReplayReasoningContent writes the chain of thought of earlier tool-calling
	// turns back as assistant.reasoning_content. Upstreams that reason before
	// calling tools reject the request when it is missing.
	ReplayReasoningContent bool `json:"replay_reasoning_content"`
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
	if err := cfg.validate(); err != nil {
		return nil, err
	}
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

// validate rejects unknown enum values instead of silently falling back: a typo in
// `thinking.style` would otherwise look like a working configuration that quietly
// never switches thinking off.
func (c *Config) validate() error {
	switch c.Thinking.Mode {
	case "", ThinkingAuto, ThinkingEnabled, ThinkingDisabled:
	default:
		return fmt.Errorf("openai-chat: thinking.mode must be auto|enabled|disabled, got %q", c.Thinking.Mode)
	}
	switch c.Thinking.Style {
	case "", ThinkingStyleNone, ThinkingStyleDeepSeek:
	default:
		return fmt.Errorf("openai-chat: thinking.style must be none|deepseek, got %q", c.Thinking.Style)
	}
	switch c.ResponseFormat {
	case "", ResponseFormatText, ResponseFormatJSONObject, ResponseFormatJSONSchema:
	default:
		return fmt.Errorf("openai-chat: response_format must be text|json_object|json_schema, got %q", c.ResponseFormat)
	}
	if c.Thinking.ReplayReasoningContent && c.thinkingStyle() == ThinkingStyleNone {
		return fmt.Errorf("openai-chat: thinking.replay_reasoning_content needs thinking.style=deepseek")
	}
	if c.DefaultMaxOutputTokens < 0 {
		return fmt.Errorf("openai-chat: default_max_output_tokens must not be negative")
	}
	return nil
}

func (c *Config) thinkingStyle() string {
	if c.Thinking.Style == "" {
		return ThinkingStyleNone
	}
	return c.Thinking.Style
}

func (c *Config) thinkingMode() string {
	if c.Thinking.Mode == "" {
		return ThinkingAuto
	}
	return c.Thinking.Mode
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
		return p.classify(resp, nil)
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
	body, err := p.requestBody(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := httpx.NewRequest(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, pluginapi.NewError("bad_request", err.Error())
	}
	p.applyHeaders(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, p.classify(resp, nil)
	}

	var chatResp providerkit.ChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&chatResp); err != nil {
		return nil, pluginapi.NewRetryableError("upstream_bad_response", err.Error(), 502)
	}
	if chatResp.Error != nil {
		return nil, pluginapi.NewError("upstream_error", chatResp.Error.Message)
	}
	if len(chatResp.Choices) > 0 && providerkit.UpstreamFailure(chatResp.Choices[0].FinishReason) {
		return nil, pluginapi.NewRetryableError("upstream_"+chatResp.Choices[0].FinishReason,
			"upstream could not produce the answer: "+chatResp.Choices[0].FinishReason, 503)
	}
	out, err := providerkit.ChatResponseToResponsesWithOptions(&chatResp, convertOptions(p.snapshot()))
	if err != nil {
		return nil, pluginapi.NewError("upstream_bad_response", err.Error())
	}
	return out, nil
}

// Stream performs a streaming chat completion and translates chunks into canonical events.
func (p *Provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	body, err := p.requestBody(req, true)
	if err != nil {
		return err
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
		return transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return p.classify(resp, nil)
	}

	state := providerkit.NewChatStreamState()
	reader := providerkit.NewSSEReader(resp.Body, 0)
	estimator := providerkit.NewCharEstimator(0)

	// A chat stream ends in exactly two ways: a chunk that carries finish_reason, or
	// the [DONE] sentinel. Reaching the end of the body without either means the
	// upstream connection died mid-answer — the text seen so far is a fragment, so
	// the stream must not be reported as a finished response (io.EOF alone is what
	// a truncated body looks like to a reader).
	terminal := false
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
			terminal = true
			break
		}
		finished, err := state.Translate(&chunk, func(ev pluginapi.Event) error {
			if ev.Type == pluginapi.EventTextDelta {
				estimator.Add(ev.Text)
			}
			return emit(ev)
		})
		if err != nil {
			return pluginapi.NewRetryableError("upstream_stream_error", err.Error(), 502)
		}
		// Keep reading after finish_reason: usage arrives in the trailing chunk.
		if finished {
			terminal = true
		}
	}

	// The upstream can end the stream by admitting it could not answer; failing over
	// is the only useful reaction, so it must not be reported as a finished response.
	if providerkit.UpstreamFailure(state.FinishReason) {
		return pluginapi.NewRetryableError("upstream_"+state.FinishReason,
			"upstream could not produce the answer: "+state.FinishReason, 503)
	}
	if !terminal {
		return pluginapi.NewRetryableError("upstream_stream_incomplete",
			"the upstream stream ended before the answer was finished", 502)
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
	// Report why the upstream stopped: "length" and "content_filter" mean the answer
	// is a fragment even though the stream itself ended cleanly, and only the
	// provider knows the difference.
	if err := emit(pluginapi.Event{Type: pluginapi.EventFinish, Reason: state.FinishReason}); err != nil {
		return err
	}
	return emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: &usage})
}

// payload is the upstream request body: the shared chat-completions request plus
// the two fields that only some deployments accept.
type payload struct {
	*providerkit.ChatRequest
	Thinking       *thinkingField  `json:"thinking,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

// thinkingField is the thinking switch DeepSeek-compatible deployments accept.
type thinkingField struct {
	Type string `json:"type"`
}

// requestBody renders the upstream request body for one attempt.
func (p *Provider) requestBody(req *pluginapi.Request, stream bool) ([]byte, error) {
	return renderBody(p.snapshot(), req, stream)
}

// snapshot reads the mutable configuration under the lock once per attempt.
func (p *Provider) snapshot() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// convertOptions carries the opt-in translation behaviours from the config.
func convertOptions(cfg Config) providerkit.ChatConvertOptions {
	return providerkit.ChatConvertOptions{
		KeepReasoningContent:   cfg.thinkingStyle() == ThinkingStyleDeepSeek,
		ReplayReasoningContent: cfg.Thinking.ReplayReasoningContent,
	}
}

func renderBody(cfg Config, req *pluginapi.Request, stream bool) ([]byte, error) {
	if req == nil {
		return nil, pluginapi.NewError("bad_request", "openai-chat: nil request")
	}
	chatReq, err := providerkit.ResponsesToChatWithOptions(req, convertOptions(cfg))
	if err != nil {
		return nil, pluginapi.NewError("bad_request", err.Error())
	}
	chatReq.Stream = stream
	if stream {
		chatReq.StreamOptions = &providerkit.ChatStreamOptions{IncludeUsage: true}
	}
	if chatReq.MaxTokens == nil && cfg.DefaultMaxOutputTokens > 0 {
		limit := cfg.DefaultMaxOutputTokens
		chatReq.MaxTokens = &limit
	}

	out := payload{ChatRequest: chatReq}
	if cfg.thinkingStyle() == ThinkingStyleDeepSeek {
		enabled := false
		switch cfg.thinkingMode() {
		case ThinkingEnabled:
			enabled = true
		case ThinkingDisabled:
			enabled = false
		default: // auto: the request decides
			enabled = req.Reasoning != nil && req.Reasoning.Effort != "" && req.Reasoning.Effort != "none"
		}
		typeName := "disabled"
		if enabled {
			typeName = "enabled"
		} else {
			// An explicit "no reasoning" must not carry a stale effort hint.
			chatReq.ReasoningEffort = ""
		}
		out.Thinking = &thinkingField{Type: typeName}
	}
	if raw := responseFormat(cfg.ResponseFormat); len(raw) > 0 {
		out.ResponseFormat = raw
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	return body, nil
}

// responseFormat maps the configured level onto the upstream field. The default
// (text) sends nothing: the upstream then applies its own default.
func responseFormat(level string) json.RawMessage {
	switch level {
	case ResponseFormatJSONObject:
		return json.RawMessage(`{"type":"json_object"}`)
	case ResponseFormatJSONSchema:
		return json.RawMessage(`{"type":"json_schema"}`)
	default:
		return nil
	}
}

// transportError classifies a failed round trip.
func transportError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return pluginapi.NewRetryableError("upstream_timeout", err.Error(), 504)
	}
	return pluginapi.NewRetryableError("upstream_unreachable", err.Error(), 0)
}

// errorEnvelope is the error shape OpenAI-compatible upstreams return.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
	Message string `json:"message"`
}

// decodeErrorEnvelope reads the upstream error body (never fails: an unparsable
// body simply leaves both fields empty).
func decodeErrorEnvelope(body []byte) errorEnvelope {
	var envelope errorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return envelope
}

func (e errorEnvelope) message() string {
	if e.Error.Message != "" {
		return e.Error.Message
	}
	return e.Message
}

// code returns the upstream error code, when the body carries one. Some
// deployments signal an exhausted balance with a body-level code instead of relying
// on the HTTP status alone.
func (e errorEnvelope) code() any { return e.Error.Code }

// classify converts a non-2xx upstream response into a protocol error.
//
// Two upstream habits matter here. A provider that runs out of balance reports it
// as 402 (DeepSeek, whose body repeats 402 in error.code), sometimes with a 200
// envelope and the code in the body instead. Descriptions differ per deployment,
// so the classification is deliberately explicit rather than "any 402 is quota".
func (p *Provider) classify(resp *http.Response, code any) *pluginapi.Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxErrorBody))
	envelope := decodeErrorEnvelope(body)
	message := envelope.message()
	if code == nil {
		code = envelope.code()
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}

	status := resp.StatusCode
	statusText := fmt.Sprintf("upstream_%d", status)
	resetAt := retryAfter(resp)

	if isQuotaStatus(status) || isQuotaCode(code) {
		if resetAt == 0 {
			resetAt = time.Now().Add(quotaCooldown).Unix()
		}
		err := pluginapi.NewQuotaError(message, resetAt)
		err.HTTPStatus = status
		return err
	}

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// A credential problem: failing over to another provider fails identically.
		err := pluginapi.NewError("token_invalid", message)
		err.HTTPStatus = status
		return err
	case status == http.StatusRequestTimeout, status == http.StatusConflict,
		status == http.StatusTooEarly, status >= 500:
		return pluginapi.NewRetryableError(statusText, message, status)
	default:
		// 400/422 and friends are the client's request: report it, do not fail over,
		// and keep the upstream message so the operator can act on it.
		err := pluginapi.NewError(statusText, message)
		err.HTTPStatus = status
		return err
	}
}

func isQuotaStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusPaymentRequired
}

// isQuotaCode reports whether a body error code means "out of quota/balance".
// The JSON decoder yields float64 for numbers, so both forms are accepted.
func isQuotaCode(code any) bool {
	switch v := code.(type) {
	case float64:
		return int(v) == http.StatusPaymentRequired || int(v) == http.StatusTooManyRequests
	case int:
		return v == http.StatusPaymentRequired || v == http.StatusTooManyRequests
	case string:
		return v == "402" || v == "429" || v == "insufficient_balance" || v == "insufficient_quota"
	default:
		return false
	}
}

// retryAfter reads the upstream cooldown hint, when it sends a usable one.
func retryAfter(resp *http.Response) int64 {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Now().Add(time.Duration(secs) * time.Second).Unix()
	}
	if when, err := http.ParseTime(raw); err == nil {
		return when.Unix()
	}
	return 0
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

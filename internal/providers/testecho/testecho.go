// Package testecho is a deterministic in-process provider used by integration tests
// and load benchmarks. It accepts no upstream dependency and echoes the last user input.
package testecho

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
	"github.com/winger/ai-gateway/pkg/providerkit"
)

// doubleQuote is the ASCII double-quote as a string (escape-free construction);
// it strips JSON quoting from raw content blobs.
var doubleQuote = string([]byte{0x22})

// Config configures the echo provider.
type Config struct {
	Prefix  string `json:"prefix"`
	Chunks  int    `json:"chunks"`
	DelayMS int    `json:"delay_ms"`
	// FailMode forces failures: "" | "retryable" | "quota" | "fatal".
	FailMode string `json:"fail_mode"`
	// FailAfter emits N deltas before failing (used for mid-stream failure tests).
	FailAfter int `json:"fail_after"`
}

// Provider implements pluginapi.Provider.
type Provider struct {
	cfg      Config
	stateDir string

	mu    sync.Mutex
	creds map[string]string
	calls int
}

// New builds an echo provider from config JSON (may be empty).
func New(configJSON, stateDir string) (*Provider, error) {
	p := &Provider{cfg: Config{Prefix: "echo:", Chunks: 1}, stateDir: stateDir}
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &p.cfg); err != nil {
			return nil, fmt.Errorf("testecho: bad config: %w", err)
		}
	}
	if p.cfg.Chunks <= 0 {
		p.cfg.Chunks = 1
	}
	return p, nil
}

// Info reports capabilities.
func (p *Provider) Info() pluginapi.Info {
	return pluginapi.Info{
		Name: "testecho", Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{
			Complete: true, Stream: true, ListModels: true, Health: true,
			UsageEstimated: true, UsageDelta: true, UsageDimensions: true,
		},
	}
}

// StateDir returns the plugin state directory.
func (p *Provider) StateDir() string { return p.stateDir }

// SetCredentials stores credentials pushed by the host.
func (p *Provider) SetCredentials(creds map[string]string) {
	p.mu.Lock()
	p.creds = creds
	p.mu.Unlock()
}

// Calls returns how many calls were served (test helper).
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ListModels returns the synthetic model catalogue.
func (p *Provider) ListModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	return []pluginapi.ModelInfo{{
		ID: "testecho", UpstreamModel: "testecho", DisplayName: "Test echo",
		ContextWindow: 8192, MaxOutputTokens: 2048,
		Capabilities: map[string]bool{"stream": true, "tools": true, "reasoning": false, "vision": false},
	}}, nil
}

// Health always succeeds.
func (p *Provider) Health(ctx context.Context) error { return nil }

// Actions returns no interactive actions.
func (p *Provider) Actions() []pluginapi.Action { return nil }

// RunAction is unsupported.
func (p *Provider) RunAction(ctx context.Context, name string, in json.RawMessage) (json.RawMessage, error) {
	return nil, pluginapi.NewError("unknown_action", "unknown action: "+name)
}

// Complete returns the echoed payload.
func (p *Provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	if req == nil {
		return nil, pluginapi.NewError("bad_request", "testecho: nil request")
	}
	p.bump()
	if err := p.maybeFail(); err != nil {
		return nil, err
	}
	text := p.reply(req)
	content, err := json.Marshal([]map[string]string{{"type": "output_text", "text": text}})
	if err != nil {
		return nil, pluginapi.NewError("encode_error", err.Error())
	}
	return &pluginapi.Response{
		Items:  []pluginapi.Item{{Type: "message", Role: "assistant", Content: content, Status: "completed"}},
		Usage:  p.usage(req, text),
		Status: "completed",
	}, nil
}

// Stream emits the reply in chunks with incremental usage.
func (p *Provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	if req == nil {
		return pluginapi.NewError("bad_request", "testecho: nil request")
	}
	p.bump()
	text := p.reply(req)
	delay := time.Duration(p.cfg.DelayMS) * time.Millisecond
	size := (len(text) + p.cfg.Chunks - 1) / p.cfg.Chunks
	if size <= 0 {
		size = len(text)
	}

	estimator := providerkit.NewCharEstimator(0)
	emitted := 0
	for i := 0; i < len(text); i += size {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		end := i + size
		if end > len(text) {
			end = len(text)
		}
		piece := text[i:end]
		if err := emit(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: piece}); err != nil {
			return err
		}
		emitted++
		if p.cfg.FailAfter > 0 && emitted >= p.cfg.FailAfter {
			return p.failError()
		}
		tokens := estimator.Add(piece)
		usage := pluginapi.Usage{Dimensions: map[string]int64{"output": tokens}, Estimated: true}
		if err := emit(pluginapi.Event{Type: pluginapi.EventUsageDelta, Usage: &usage, Estimated: true}); err != nil {
			return err
		}
	}
	final := p.usage(req, text)
	return emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: &final})
}

func (p *Provider) bump() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
}

func (p *Provider) reply(req *pluginapi.Request) string {
	var last string
	for _, item := range req.Input {
		if item.Type == "message" && item.Role == "user" {
			last = string(item.Content)
		}
	}
	last = strings.Trim(last, doubleQuote)
	if last == "" {
		last = req.Model
	}
	prefix := p.cfg.Prefix
	if prefix == "" {
		prefix = "echo:"
	}
	if !strings.HasSuffix(prefix, " ") {
		prefix += " "
	}
	return prefix + last
}

func (p *Provider) usage(req *pluginapi.Request, text string) pluginapi.Usage {
	return pluginapi.Usage{Dimensions: map[string]int64{
		"input":  providerkit.EstimateInputTokens(req, 0),
		"output": providerkit.EstimateTokens(text, 0),
	}}
}

func (p *Provider) maybeFail() error {
	if p.cfg.FailMode == "" {
		return nil
	}
	return p.failError()
}

func (p *Provider) failError() error {
	switch p.cfg.FailMode {
	case "retryable":
		return pluginapi.NewRetryableError("upstream_5xx", "testecho: forced retryable failure", 503)
	case "quota":
		return pluginapi.NewQuotaError("testecho: forced quota exhaustion", time.Now().Add(30*time.Minute).Unix())
	default:
		return pluginapi.NewError("testecho_failure", "testecho: forced fatal failure")
	}
}

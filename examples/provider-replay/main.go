// Command provider-replay is a reference ai-gateway provider plugin.
//
// It speaks the plugin protocol over stdin/stdout and replays a canned answer, which
// makes it useful as an end-to-end test target, a load-test upstream and a template
// for real adapters.
//
// Config (GW_PLUGIN_CONFIG):
//
//	{"reply":"hello from replay","chunks":3,"delay_ms":5}
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

type config struct {
	Reply   string `json:"reply"`
	Chunks  int    `json:"chunks"`
	DelayMS int    `json:"delay_ms"`
}

type provider struct {
	mu       sync.Mutex
	cfg      config
	creds    map[string]string
	stateDir string
	calls    int
}

var configSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reply":    {"type": "string", "title": "Reply text", "default": "hello from replay", "x-group": "content"},
    "chunks":   {"type": "integer", "title": "Chunks", "default": 3, "minimum": 1, "maximum": 64, "x-group": "content"},
    "delay_ms": {"type": "integer", "title": "Delay per chunk (ms)", "default": 5, "minimum": 0, "maximum": 5000, "x-advanced": true}
  }
}`)

var credentialsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "api_key": {"type": "string", "title": "API key", "x-secret": true}
  }
}`)

func main() {
	p := &provider{cfg: config{Reply: "hello from replay", Chunks: 3, DelayMS: 5}}

	if raw := os.Getenv(pluginapi.EnvConfig); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.cfg); err != nil {
			fmt.Fprintln(os.Stderr, "provider-replay: bad GW_PLUGIN_CONFIG:", err)
			os.Exit(2)
		}
	}
	if p.cfg.Chunks <= 0 {
		p.cfg.Chunks = 1
	}
	p.stateDir = os.Getenv(pluginapi.EnvStateDir)

	if err := pluginapi.Serve(p); err != nil {
		fmt.Fprintln(os.Stderr, "provider-replay:", err)
		os.Exit(1)
	}
}

func (p *provider) Info() pluginapi.Info {
	return pluginapi.Info{
		Name:    "replay",
		Version: "0.1.0",
		Capabilities: pluginapi.Capabilities{
			Complete:        true,
			Stream:          true,
			ListModels:      true,
			Health:          true,
			UsageDelta:      true,
			UsageDimensions: true,
			Actions: []pluginapi.Action{
				{Name: "echo", Title: "Echo the given payload"},
			},
		},
	}
}

func (p *provider) ConfigSchema() json.RawMessage      { return configSchema }
func (p *provider) CredentialsSchema() json.RawMessage { return credentialsSchema }

func (p *provider) StateDir() string { return p.stateDir }

func (p *provider) SetCredentials(creds map[string]string) {
	p.mu.Lock()
	p.creds = creds
	p.mu.Unlock()
}

func (p *provider) ListModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	return []pluginapi.ModelInfo{
		{
			ID: "replay", UpstreamModel: "replay", DisplayName: "Replay (test provider)",
			ContextWindow: 8192, MaxOutputTokens: 1024,
			Capabilities: map[string]bool{"stream": true, "tools": false, "vision": false, "reasoning": false},
		},
		{
			ID: "replay-slow", UpstreamModel: "replay-slow", DisplayName: "Replay (slow, for in-flight tests)",
			ContextWindow: 8192, MaxOutputTokens: 1024,
			Capabilities: map[string]bool{"stream": true, "tools": false},
		},
	}, nil
}

func (p *provider) Health(ctx context.Context) error {
	if p.stateDir == "" {
		return pluginapi.NewError("no_state_dir", "GW_PLUGIN_STATE_DIR is not set")
	}
	return nil
}

func (p *provider) Actions() []pluginapi.Action {
	return []pluginapi.Action{{Name: "echo", Title: "Echo the given payload"}}
}

func (p *provider) RunAction(ctx context.Context, name string, in json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "echo":
		return in, nil
	default:
		return nil, pluginapi.NewError("unknown_action", "unknown action: "+name)
	}
}

func (p *provider) Complete(ctx context.Context, req *pluginapi.Request) (*pluginapi.Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()

	if err := p.recordCall(req); err != nil {
		return nil, err
	}
	if strings.Contains(req.Model, "fail") {
		return nil, pluginapi.NewRetryableError("upstream_5xx", "model requested a failure", 503)
	}

	text := p.replyFor(req)
	return &pluginapi.Response{
		Items: []pluginapi.Item{{
			Type:    "message",
			Role:    "assistant",
			Content: outputText(text),
			Status:  "completed",
		}},
		Usage:  p.usageFor(req, text),
		Status: "completed",
	}, nil
}

func (p *provider) Stream(ctx context.Context, req *pluginapi.Request, emit func(pluginapi.Event) error) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()

	if err := p.recordCall(req); err != nil {
		return err
	}
	if strings.Contains(req.Model, "fail") {
		// Emit one delta and then fail: the host must NOT fail over after the first byte.
		if err := emit(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: "partial "}); err != nil {
			return err
		}
		return pluginapi.NewRetryableError("upstream_5xx", "stream broke mid-flight", 503)
	}

	text := p.replyFor(req)
	delay := time.Duration(p.cfg.DelayMS) * time.Millisecond
	chunks := p.cfg.Chunks
	if chunks > len(text) {
		chunks = len(text)
	}
	if chunks < 1 {
		chunks = 1
	}
	size := (len(text) + chunks - 1) / chunks

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
		if err := emit(pluginapi.Event{Type: pluginapi.EventTextDelta, Text: piece, Index: 0}); err != nil {
			return err
		}
		emitted += len(piece)

		// Incremental usage makes in-flight accounting possible before the final usage.
		usage := pluginapi.Usage{Dimensions: map[string]int64{"output": int64(emitted / 4)}, Estimated: true}
		if err := emit(pluginapi.Event{Type: pluginapi.EventUsageDelta, Usage: &usage, Estimated: true}); err != nil {
			return err
		}
	}

	final := p.usageFor(req, text)
	if err := emit(pluginapi.Event{Type: pluginapi.EventUsage, Usage: &final}); err != nil {
		return err
	}
	return nil
}

func (p *provider) replyFor(req *pluginapi.Request) string {
	if p.cfg.Reply != "" {
		return p.cfg.Reply
	}
	return "echo: " + req.Model
}

func (p *provider) usageFor(req *pluginapi.Request, text string) pluginapi.Usage {
	input := 0
	for _, item := range req.Input {
		input += len(item.Content)/4 + 1
	}
	if req.Instructions != "" {
		input += len(req.Instructions) / 4
	}
	return pluginapi.Usage{Dimensions: map[string]int64{
		"input":  int64(input),
		"output": int64(len(text) / 4),
	}}
}

// recordCall proves the state directory is writable by the plugin (used by E2E tests).
func (p *provider) recordCall(req *pluginapi.Request) error {
	if p.stateDir == "" {
		return nil
	}
	path := filepath.Join(p.stateDir, "calls.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return pluginapi.NewError("state_dir_error", err.Error())
	}
	defer f.Close()

	p.mu.Lock()
	n := p.calls
	hasCreds := len(p.creds) > 0
	p.mu.Unlock()

	_, err = fmt.Fprintf(f, "call=%d model=%s creds=%t"+string([]byte{10}), n, req.Model, hasCreds)
	if err != nil {
		return pluginapi.NewError("state_dir_error", err.Error())
	}
	return nil
}

// outputText builds a Responses-shaped content array without needing escapes.
func outputText(text string) json.RawMessage {
	part := map[string]string{"type": "output_text", "text": text}
	raw, err := json.Marshal([]map[string]string{part})
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

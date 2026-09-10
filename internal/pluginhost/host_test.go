package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// buildExamplePlugin compiles examples/provider-replay so the tests exercise a real
// plugin process rather than an in-process fake.
func buildExamplePlugin(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(wd, "..", "..", "examples", "provider-replay")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("example plugin sources missing: %v", err)
	}
	out := filepath.Join(t.TempDir(), "provider-replay")
	cmd := exec.Command(goBin, "build", "-o", out, src)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build example plugin: %v: %s", err, stderr.String())
	}
	return out
}

func newTestHost(t *testing.T) *Host {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.StartTimeout = 5 * time.Second
	cfg.CancelGrace = 500 * time.Millisecond
	return New(cfg, nil)
}

func testRequest(t *testing.T, model string) *pluginapi.Request {
	t.Helper()
	content, err := json.Marshal("hello")
	if err != nil {
		t.Fatal(err)
	}
	return &pluginapi.Request{
		Model:    model,
		Input:    []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
		Metadata: map[string]string{"test": "e2e"},
	}
}

func TestPluginLifecycleEndToEnd(t *testing.T) {
	bin := buildExamplePlugin(t)
	host := newTestHost(t)
	ctx := context.Background()
	defer func() { _ = host.StopAll(ctx) }()

	cfgJSON, err := json.Marshal(map[string]any{"reply": "hello plugin", "chunks": 3, "delay_ms": 0})
	if err != nil {
		t.Fatal(err)
	}

	proc, err := host.Start(ctx, Options{
		Instance:    "replay-e2e",
		Kind:        "plugin:replay",
		Binary:      bin,
		ConfigJSON:  string(cfgJSON),
		Credentials: map[string]string{"api_key": "sekret-value"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	hs := proc.Client.Handshake()
	if hs.Name != "replay" || hs.Protocol != pluginapi.ProtocolVersion {
		t.Fatalf("handshake mismatch: %+v", hs)
	}
	if !hs.Capabilities.Stream || !hs.Capabilities.UsageDelta || !hs.Capabilities.UsageDimensions {
		t.Fatalf("capabilities missing: %+v", hs.Capabilities)
	}
	if len(hs.ConfigSchema) == 0 || len(hs.CredentialsSchema) == 0 {
		t.Fatal("plugin must advertise config and credentials schemas")
	}
	if _, ok := host.Get("replay-e2e"); !ok {
		t.Fatal("host must track the running instance")
	}
	if proc.PID() == 0 {
		t.Fatal("expected a live pid")
	}

	// models + health + action
	models, err := proc.Client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "replay" {
		t.Fatalf("models mismatch: %+v", models)
	}
	if err := proc.Client.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	raw, err := proc.Client.RunAction(ctx, "echo", json.RawMessage([]byte("42")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "42" {
		t.Fatalf("action result = %q", string(raw))
	}

	// non-streaming
	resp, err := proc.Client.Complete(ctx, testRequest(t, "replay"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "completed" || len(resp.Items) != 1 {
		t.Fatalf("complete response mismatch: %+v", resp)
	}
	if got := string(resp.Items[0].Content); !strings.Contains(got, "hello plugin") {
		t.Fatalf("content mismatch: %s", got)
	}
	if resp.Usage.Dimensions["output"] == 0 {
		t.Fatalf("usage dimensions missing: %+v", resp.Usage)
	}

	// streaming: deltas in order, then end
	var text strings.Builder
	var sawUsageDelta, sawFinalUsage bool
	end, err := proc.Client.Stream(ctx, testRequest(t, "replay"), func(ev pluginapi.Event) error {
		switch ev.Type {
		case pluginapi.EventTextDelta:
			text.WriteString(ev.Text)
		case pluginapi.EventUsageDelta:
			sawUsageDelta = true
		case pluginapi.EventUsage:
			sawFinalUsage = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "hello plugin" {
		t.Fatalf("streamed text = %q", text.String())
	}
	if end.FinishReason != "stop" || end.Partial {
		t.Fatalf("stream end mismatch: %+v", end)
	}
	if !sawUsageDelta || !sawFinalUsage {
		t.Fatalf("expected incremental and final usage events (delta=%t final=%t)", sawUsageDelta, sawFinalUsage)
	}

	// the plugin proved it received the state dir and the credentials file
	calls, err := os.ReadFile(filepath.Join(host.cfg.StateDir, "replay-e2e", "calls.log"))
	if err != nil {
		t.Fatalf("plugin did not write into its state dir: %v", err)
	}
	if !strings.Contains(string(calls), "creds=true") {
		t.Fatalf("credentials were not delivered to the plugin: %s", string(calls))
	}

	if err := host.Stop(ctx, "replay-e2e"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, ok := host.Get("replay-e2e"); ok {
		t.Fatal("instance must be gone after Stop")
	}
}

func TestPluginAutoRestartAfterCrash(t *testing.T) {
	bin := buildExamplePlugin(t)
	host := newTestHost(t)
	ctx := context.Background()
	defer func() { _ = host.StopAll(ctx) }()

	proc, err := host.Start(ctx, Options{
		Instance:    "replay-crash",
		Kind:        "plugin:replay",
		Binary:      bin,
		AutoRestart: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPID := proc.PID()

	// simulate a crash
	if err := proc.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var restarted *Process
	for time.Now().Before(deadline) {
		if p, ok := host.Get("replay-crash"); ok && p.PID() != firstPID {
			restarted = p
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if restarted == nil {
		t.Fatal("plugin was not restarted after a crash")
	}
	if restarted.PID() == firstPID {
		t.Fatal("restarted process must have a new pid")
	}
	if _, err := restarted.Client.Complete(ctx, testRequest(t, "replay")); err != nil {
		t.Fatalf("restarted plugin is not usable: %v", err)
	}
}

func TestMiddlewareTimeoutAndFailureFrames(t *testing.T) {
	bin := buildExamplePlugin(t)
	host := newTestHost(t)
	ctx := context.Background()
	defer func() { _ = host.StopAll(ctx) }()

	proc, err := host.Start(ctx, Options{Instance: "replay-fail", Kind: "plugin:replay", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}

	// The example plugin fails when the model name contains "fail".
	_, err = proc.Client.Complete(ctx, testRequest(t, "please-fail"))
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if !apiErr.Retryable || apiErr.HTTPStatus != 503 {
		t.Fatalf("error mapping mismatch: %+v", apiErr)
	}

	// Streaming failure after the first delta must surface as an error, not as a silent end.
	seen := 0
	text := ""
	_, err = proc.Client.Stream(ctx, testRequest(t, "please-fail"), func(ev pluginapi.Event) error {
		if ev.Type == pluginapi.EventTextDelta {
			seen++
			text += ev.Text
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected a mid-stream error")
	}
	if seen == 0 || text == "" {
		t.Fatalf("expected the first delta before the failure, got %d/%q", seen, text)
	}
}

func TestDiscoverFindsExecutables(t *testing.T) {
	bin := buildExamplePlugin(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "provider-replay")
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
	// a non-executable file must be ignored
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Dir = dir
	host := New(cfg, nil)
	found, err := host.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || filepath.Base(found[0]) != "provider-replay" {
		t.Fatalf("discover mismatch: %v", found)
	}
}

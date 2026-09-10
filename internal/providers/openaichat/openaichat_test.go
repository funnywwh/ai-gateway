package openaichat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// lf is the line feed used to build SSE frames (escape-free construction).
var lf = string([]byte{0x0A})

const nonStreamBody = `{
  "id": "chatcmpl-1",
  "model": "upstream-model",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello from upstream"}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 40, "completion_tokens": 6, "total_tokens": 46,
            "prompt_cache_hit_tokens": 32, "prompt_cache_miss_tokens": 8}
}`

func newUpstream(t *testing.T, handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg, err := json.Marshal(map[string]any{"base_url": srv.URL + "/v1", "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New("upstream", string(cfg), t.TempDir(), map[string]string{"api_key": "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func userRequest() *pluginapi.Request {
	content, _ := json.Marshal("hi")
	return &pluginapi.Request{
		Model: "public-model",
		Input: []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
	}
}

func TestCompleteTranslatesUpstreamResponse(t *testing.T) {
	var (
		path     string
		auth     string
		bodyText string
	)
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		bodyText = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nonStreamBody)
	})

	resp, err := p.Complete(context.Background(), userRequest())
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", path)
	}
	if auth != "Bearer secret-key" {
		t.Fatalf("authorization header = %q", auth)
	}
	if !strings.Contains(bodyText, "public-model") {
		t.Fatalf("model must be forwarded: %s", bodyText)
	}
	if resp.Status != "completed" || len(resp.Items) != 1 {
		t.Fatalf("response mismatch: %+v", resp)
	}
	if got := string(resp.Items[0].Content); !strings.Contains(got, "hello from upstream") {
		t.Fatalf("content mismatch: %s", got)
	}
	dims := resp.Usage.Dimensions
	if dims["input_cache_hit"] != 32 || dims["input_cache_miss"] != 8 || dims["output"] != 6 {
		t.Fatalf("usage dimensions mismatch: %+v", dims)
	}
}

func TestStreamTranslatesSSEChunks(t *testing.T) {
	var sawIncludeUsage bool
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent map[string]any
		_ = json.Unmarshal(raw, &sent)
		if opts, ok := sent["stream_options"].(map[string]any); ok {
			if v, ok := opts["include_usage"].(bool); ok && v {
				sawIncludeUsage = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	var text string
	var usageDims map[string]int64
	err := p.Stream(context.Background(), userRequest(), func(ev pluginapi.Event) error {
		switch ev.Type {
		case pluginapi.EventTextDelta:
			text += ev.Text
		case pluginapi.EventUsage:
			usageDims = ev.Usage.Dimensions
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello" {
		t.Fatalf("streamed text = %q", text)
	}
	if !sawIncludeUsage {
		t.Fatal("stream_options.include_usage must be requested")
	}
	if usageDims["input"] != 9 || usageDims["output"] != 2 {
		t.Fatalf("final usage mismatch: %+v", usageDims)
	}
}

func TestRateLimitMapsToQuotaCooldown(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	})

	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindQuotaExhausted || !apiErr.Retryable {
		t.Fatalf("error classification mismatch: %+v", apiErr)
	}
	if apiErr.ResetAt == 0 {
		t.Fatal("Retry-After must be translated into reset_at")
	}
	if !strings.Contains(apiErr.Message, "rate limited") {
		t.Fatalf("upstream message must be preserved: %q", apiErr.Message)
	}
}

func TestServerErrorIsRetryable(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream down"}}`)
	})
	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindRetryable || apiErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("classification mismatch: %+v", apiErr)
	}
}

func TestClientErrorIsFatal(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad model"}}`)
	})
	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindFatal || apiErr.Retryable {
		t.Fatalf("400 must be fatal: %+v", apiErr)
	}
}

func TestListModelsFromConfig(t *testing.T) {
	cfg, err := json.Marshal(map[string]any{
		"base_url": "http://127.0.0.1:1/v1",
		"models": []map[string]any{{
			"public": "pub", "upstream": "up", "context_window": 4096,
			"max_output_tokens": 1024, "capabilities": map[string]bool{"stream": true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New("x", string(cfg), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].UpstreamModel != "up" || models[0].ContextWindow != 4096 {
		t.Fatalf("models mismatch: %+v", models)
	}
}

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
	return newUpstreamWith(t, handler, nil)
}

// newUpstreamWith builds a provider pointed at a test upstream, with extra config
// keys merged in (thinking dialect, response_format, ...).
func newUpstreamWith(t *testing.T, handler http.HandlerFunc, extra map[string]any) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := map[string]any{"base_url": srv.URL + "/v1", "timeout_s": 5}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New("upstream", string(raw), t.TempDir(), map[string]string{"api_key": "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

// captureBody returns a handler that records the decoded request body and answers
// with the given non-streaming response body.
func captureBody(t *testing.T, response string, into *map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var sent map[string]any
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Errorf("upstream body is not JSON: %v (%s)", err, raw)
		}
		*into = sent
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}
}

func userRequest() *pluginapi.Request {
	content, _ := json.Marshal("hi")
	return &pluginapi.Request{
		Model: "public-model",
		Input: []pluginapi.Item{{Type: "message", Role: "user", Content: content}},
	}
}

// reasoningRequest is a request that asks for a chain of thought.
func reasoningRequest(effort string) *pluginapi.Request {
	req := userRequest()
	req.Reasoning = &pluginapi.Reasoning{Effort: effort}
	return req
}

// toolTurnRequest is a two-turn tool-calling conversation whose first turn carries
// a chain of thought, which is what reasoning-replay upstreams require back.
func toolTurnRequest() *pluginapi.Request {
	content, _ := json.Marshal("what is the weather")
	req := &pluginapi.Request{
		Model: "public-model",
		Input: []pluginapi.Item{
			{Type: "message", Role: "user", Content: content},
			{Type: "reasoning", ID: "rs_1", Summary: []pluginapi.SummaryPart{
				{Type: "summary_text", Text: "I should look it up. "},
				{Type: "summary_text", Text: "Use the tool."},
			}},
			{Type: "function_call", CallID: "call_1", Name: "weather", Arguments: `{"city":"hz"}`},
			{Type: "function_call_output", CallID: "call_1", Output: "cloudy"},
		},
	}
	return req
}

// stringField reads a string field from an upstream request body.
func stringField(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key].(string)
	if !ok {
		t.Fatalf("field %q is not a string in %v", key, body)
	}
	return value
}

// thinkingType reads thinking.type from an upstream request body ("" when absent).
func thinkingType(t *testing.T, body map[string]any) string {
	t.Helper()
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		return ""
	}
	return stringField(t, thinking, "type")
}

// assistantToolCallReasoning returns reasoning_content of the assistant message
// that carries tool calls, or "" when there is none.
func assistantToolCallReasoning(t *testing.T, body map[string]any) string {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing from upstream body: %v", body)
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			text, _ := message["reasoning_content"].(string)
			return text
		}
	}
	return ""
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

// ---------------------------------------------------------------------------
// thinking dialect (M17)
// ---------------------------------------------------------------------------

const deepseekThinking = "deepseek"

func TestThinkingConfigRejectsUnknownValues(t *testing.T) {
	cases := []map[string]any{
		{"thinking": map[string]any{"mode": "sometimes"}},
		{"thinking": map[string]any{"style": "openai"}},
		{"response_format": "xml"},
		{"thinking": map[string]any{"style": "none", "replay_reasoning_content": true}},
		{"default_max_output_tokens": -1},
	}
	for _, extra := range cases {
		extra["base_url"] = "http://127.0.0.1:1/v1"
		raw, err := json.Marshal(extra)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New("x", string(raw), "", nil); err == nil {
			t.Fatalf("config %v must be rejected instead of silently ignored", extra)
		}
	}
}

func TestDefaultDialectSendsNoThinkingFields(t *testing.T) {
	var body map[string]any
	p, _ := newUpstream(t, captureBody(t, nonStreamBody, &body))

	// No reasoning requested and no thinking config: the generic OpenAI-compatible shape.
	if _, err := p.Complete(context.Background(), userRequest()); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("default config must not send thinking: %v", body)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("default config must not send reasoning_effort: %v", body)
	}
	if _, ok := body["response_format"]; ok {
		t.Fatalf("response_format=text (default) must not send the field: %v", body)
	}
}

func TestDeepSeekThinkingSwitch(t *testing.T) {
	cases := []struct {
		name        string
		config      map[string]any
		effort      string
		wantType    string
		wantEffort  string
		wantEffort_ bool // whether reasoning_effort must be present
	}{
		{
			name:   "auto follows a reasoning request",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking}},
			effort: "low", wantType: "enabled", wantEffort: "low", wantEffort_: true,
		},
		{
			// Silence is not "off": the upstream's default decides (DeepSeek defaults to
			// thinking on). Sending type=disabled here used to strip the chain of thought
			// from every client that does not speak the reasoning field — DSH pointed at
			// this gateway does not — and the downgraded model stops tasks mid-way.
			name:   "auto leaves the upstream default alone for a silent request",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking}},
			effort: "", wantType: "",
		},
		{
			name:   "auto treats effort=none as off",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking}},
			effort: "none", wantType: "disabled",
		},
		{
			name:   "mode=enabled overrides a silent request",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking, "mode": "enabled"}},
			effort: "", wantType: "enabled",
		},
		{
			name:   "mode=enabled ignores an explicit off and drops its hint",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking, "mode": "enabled"}},
			effort: "none", wantType: "enabled",
		},
		{
			name:   "mode=disabled overrides a loud request",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking, "mode": "disabled"}},
			effort: "high", wantType: "disabled",
		},
		{
			name:   "mode=disabled clears the effort hint",
			config: map[string]any{"thinking": map[string]any{"style": deepseekThinking, "mode": "disabled"}},
			effort: "none", wantType: "disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			p, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &body), tc.config)

			req := userRequest()
			if tc.effort != "" {
				req = reasoningRequest(tc.effort)
			}
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if got := thinkingType(t, body); got != tc.wantType {
				t.Fatalf("thinking.type = %q, want %q (body %v)", got, tc.wantType, body)
			}
			gotEffort, present := body["reasoning_effort"]
			if present != tc.wantEffort_ {
				t.Fatalf("reasoning_effort presence = %t, want %t (body %v)", present, tc.wantEffort_, body)
			}
			if tc.wantEffort_ && gotEffort != tc.wantEffort {
				t.Fatalf("reasoning_effort = %v, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

func TestNonDeepSeekDialectKeepsEffortOnly(t *testing.T) {
	var body map[string]any
	p, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &body), map[string]any{
		"thinking": map[string]any{"style": "none"},
	})

	if _, err := p.Complete(context.Background(), reasoningRequest("medium")); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("style=none must not send thinking: %v", body)
	}
	if got := stringField(t, body, "reasoning_effort"); got != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium (previous behaviour)", got)
	}
}

func TestReasoningContentReplayedOnlyForToolTurns(t *testing.T) {
	var body map[string]any
	p, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &body), map[string]any{
		"thinking": map[string]any{"style": deepseekThinking, "replay_reasoning_content": true},
	})

	if _, err := p.Complete(context.Background(), toolTurnRequest()); err != nil {
		t.Fatal(err)
	}
	want := "I should look it up. Use the tool."
	if got := assistantToolCallReasoning(t, body); got != want {
		t.Fatalf("reasoning_content = %q, want %q", got, want)
	}

	// A conversation without tool calls must not carry the field at all.
	var plain map[string]any
	p2, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &plain), map[string]any{
		"thinking": map[string]any{"style": deepseekThinking, "replay_reasoning_content": true},
	})
	if _, err := p2.Complete(context.Background(), reasoningRequest("low")); err != nil {
		t.Fatal(err)
	}
	for _, raw := range plain["messages"].([]any) {
		if message, ok := raw.(map[string]any); ok {
			if _, present := message["reasoning_content"]; present {
				t.Fatalf("reasoning_content must not appear without tool calls: %v", message)
			}
		}
	}
}

func TestStreamForwardsReasoningBeforeText(t *testing.T) {
	p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"hard"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}, map[string]any{"thinking": map[string]any{"style": deepseekThinking}})

	var order []string
	var reasoning, text string
	var dims map[string]int64
	err := p.Stream(context.Background(), reasoningRequest("high"), func(ev pluginapi.Event) error {
		switch ev.Type {
		case pluginapi.EventReasoningDelta:
			order = append(order, "reasoning")
			reasoning += ev.Text
		case pluginapi.EventTextDelta:
			order = append(order, "text")
			text += ev.Text
		case pluginapi.EventUsage:
			dims = ev.Usage.Dimensions
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reasoning != "think hard" || text != "answer" {
		t.Fatalf("reasoning=%q text=%q", reasoning, text)
	}
	if len(order) != 3 || order[0] != "reasoning" || order[1] != "reasoning" || order[2] != "text" {
		t.Fatalf("event order = %v, want reasoning before text", order)
	}
	if dims["input"] != 5 || dims["output"] != 3 {
		t.Fatalf("usage dimensions = %v", dims)
	}
}

func TestCompleteKeepsReasoningAsItem(t *testing.T) {
	const body = `{
      "id": "chatcmpl-2",
      "choices": [{"index": 0, "finish_reason": "stop",
                   "message": {"role": "assistant", "content": "answer", "reasoning_content": "because"}}],
      "usage": {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14,
                "completion_tokens_details": {"reasoning_tokens": 2}}
    }`
	p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}, map[string]any{"thinking": map[string]any{"style": deepseekThinking}})

	resp, err := p.Complete(context.Background(), reasoningRequest("high"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected a reasoning item and a message item, got %+v", resp.Items)
	}
	if resp.Items[0].Type != "reasoning" || len(resp.Items[0].Summary) != 1 || resp.Items[0].Summary[0].Text != "because" {
		t.Fatalf("reasoning item mismatch: %+v", resp.Items[0])
	}
	if resp.Items[1].Type != "message" {
		t.Fatalf("answer item mismatch: %+v", resp.Items[1])
	}
	if resp.Usage.Dimensions["reasoning"] != 2 || resp.Usage.Dimensions["output"] != 4 {
		t.Fatalf("usage dimensions = %v", resp.Usage.Dimensions)
	}
}

func TestUpstreamFailureFinishReasonIsRetryable(t *testing.T) {
	for _, reason := range []string{"insufficient_system_resource", "aborted"} {
		t.Run(reason, func(t *testing.T) {
			body := `{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"` + reason + `"}]}`
			p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}, nil)

			_, err := p.Complete(context.Background(), userRequest())
			apiErr, ok := pluginapi.IsError(err)
			if !ok {
				t.Fatalf("expected a protocol error, got %v", err)
			}
			if apiErr.Kind != pluginapi.KindRetryable {
				t.Fatalf("%s must allow failover: %+v", reason, apiErr)
			}
		})
	}
}

func TestStreamUpstreamFailureIsRetryable(t *testing.T) {
	p, _ := newUpstreamWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"insufficient_system_resource"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, chunk+lf+lf)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}, nil)

	err := p.Stream(context.Background(), userRequest(), func(pluginapi.Event) error { return nil })
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindRetryable {
		t.Fatalf("a truncated stream must allow failover: %+v", apiErr)
	}
}

// ---------------------------------------------------------------------------
// error classification (M17)
// ---------------------------------------------------------------------------

func TestInsufficientBalanceIsQuota(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"Insufficient Balance","type":"unknown_error","code":402}}`)
	})

	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindQuotaExhausted || !apiErr.Retryable {
		t.Fatalf("402 must cool the candidate down: %+v", apiErr)
	}
	if apiErr.ResetAt == 0 {
		t.Fatal("402 without Retry-After must still carry a cooldown deadline")
	}
	if !strings.Contains(apiErr.Message, "Insufficient Balance") {
		t.Fatalf("upstream message must be preserved: %q", apiErr.Message)
	}
}

func TestQuotaCodeInsideBodyIsQuota(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"balance exhausted","code":402}}`)
	})

	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindQuotaExhausted {
		t.Fatalf("a body-level 402 must be treated as quota: %+v", apiErr)
	}
}

func TestUnauthorizedIsFatalWithoutFailover(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Authentication Fails"}}`)
	})

	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindFatal || apiErr.Retryable || apiErr.Code != "token_invalid" {
		t.Fatalf("401 is a credential problem, not a reason to fail over: %+v", apiErr)
	}
}

func TestRateLimitWithoutRetryAfterStaysRetryable(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})

	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if !apiErr.Retryable {
		t.Fatalf("429 must be retryable: %+v", apiErr)
	}
}

func TestUnprocessableEntityKeepsUpstreamMessage(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"message":"reasoning_content is required with tools"}}`)
	})

	_, err := p.Complete(context.Background(), userRequest())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindFatal || apiErr.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("422 must stay fatal and keep its status: %+v", apiErr)
	}
	if !strings.Contains(apiErr.Message, "reasoning_content is required") {
		t.Fatalf("the upstream message must survive classification: %q", apiErr.Message)
	}
}

// ---------------------------------------------------------------------------
// response_format and output budget (M17)
// ---------------------------------------------------------------------------

func TestResponseFormatLevels(t *testing.T) {
	cases := []struct {
		level string
		want  string
	}{
		{level: "text", want: ""},
		{level: "json_object", want: "json_object"},
		{level: "json_schema", want: "json_schema"},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			var body map[string]any
			p, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &body), map[string]any{"response_format": tc.level})

			if _, err := p.Complete(context.Background(), userRequest()); err != nil {
				t.Fatal(err)
			}
			format, ok := body["response_format"].(map[string]any)
			if tc.want == "" {
				if ok {
					t.Fatalf("level %q must not send response_format: %v", tc.level, format)
				}
				return
			}
			if !ok || format["type"] != tc.want {
				t.Fatalf("response_format = %v, want type %q", body["response_format"], tc.want)
			}
		})
	}
}

func TestDefaultMaxOutputTokensOnlyFillsBlanks(t *testing.T) {
	var body map[string]any
	p, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &body), map[string]any{"default_max_output_tokens": 4096})

	if _, err := p.Complete(context.Background(), userRequest()); err != nil {
		t.Fatal(err)
	}
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != 4096 {
		t.Fatalf("max_tokens = %v, want the configured default", body["max_tokens"])
	}

	// An explicit client value always wins over the fallback.
	req := userRequest()
	limit := 64
	req.MaxOutputTokens = &limit
	var explicit map[string]any
	p2, _ := newUpstreamWith(t, captureBody(t, nonStreamBody, &explicit), map[string]any{"default_max_output_tokens": 4096})
	if _, err := p2.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got, ok := explicit["max_tokens"].(float64); !ok || int(got) != 64 {
		t.Fatalf("max_tokens = %v, want the client value 64", explicit["max_tokens"])
	}
}

func TestHealthClassifiesUnauthorized(t *testing.T) {
	p, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Authentication Fails"}}`)
	})

	err := p.Health(context.Background())
	apiErr, ok := pluginapi.IsError(err)
	if !ok {
		t.Fatalf("expected a protocol error, got %v", err)
	}
	if apiErr.Kind != pluginapi.KindFatal || apiErr.Code != "token_invalid" {
		t.Fatalf("health must report a credential problem as fatal: %+v", apiErr)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// newTestProvider builds a provider wired to the given test servers.
func newTestProvider(t *testing.T, baseURL, sessionURL, tokenURL string, creds map[string]string) *provider {
	t.Helper()
	p := &provider{
		cfg: config{
			BaseURL: baseURL, SessionURL: sessionURL, TokenURL: tokenURL,
			HealthPrompt: defaultHealthPrompt, TimeoutMS: 5000,
			Models: []modelConfig{{ID: "codex", UpstreamModel: "gpt-5-codex", ContextWindow: 1000, MaxOutputTokens: 100}},
		},
		now:      func() time.Time { return time.Now().UTC() },
		stateDir: t.TempDir(),
		// Hermetic by default: never consult the developer's ambient proxy
		// variables. The environment-fallback test injects its own hook.
		envProxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}
	p.installTransport()
	p.http.Timeout = 5 * time.Second
	p.applyProxy()
	p.SetCredentials(creds)
	return p
}

// recordingProxy is a plain-HTTP forwarding proxy: requests for http:// targets
// arrive with an absolute URI, so no CONNECT or TLS interception is needed. It
// counts the traffic that actually went through it and forwards it upstream.
func recordingProxy(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	direct := &http.Client{Transport: &http.Transport{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := direct.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	return server
}

// userInput is the minimal request input the proxy tests need.
func userInput(text string) []pluginapi.Item {
	content, _ := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
	return []pluginapi.Item{{Type: "message", Role: "user", Content: content}}
}

func TestConfigProxyIsUsedForUpstreamCalls(t *testing.T) {
	var upstreamCalls, proxyCalls atomic.Int64
	upstream := sseServer(t, 200, happyFrames, &upstreamCalls)
	defer upstream.Close()
	proxy := recordingProxy(t, &proxyCalls)

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	p.cfg.Proxy = proxy.URL
	p.applyProxy()

	if _, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex", Input: userInput("hi")}); err != nil {
		t.Fatalf("Complete through the proxy: %v", err)
	}
	if proxyCalls.Load() == 0 {
		t.Fatal("the configured proxy was not used")
	}
	if upstreamCalls.Load() == 0 {
		t.Fatal("the request never reached the upstream")
	}
}

func TestCredentialsProxyOverridesConfig(t *testing.T) {
	upstream := sseServer(t, 200, happyFrames, nil)
	defer upstream.Close()
	var configCalls, credCalls atomic.Int64
	configProxy := recordingProxy(t, &configCalls)
	credProxy := recordingProxy(t, &credCalls)

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token", "proxy": credProxy.URL})
	p.cfg.Proxy = configProxy.URL
	p.applyProxy()

	if _, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex", Input: userInput("hi")}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if credCalls.Load() == 0 {
		t.Fatal("the proxy from credentials was not used")
	}
	if configCalls.Load() != 0 {
		t.Fatalf("the configured proxy was used %d times despite the credential override", configCalls.Load())
	}
}

func TestProxyUnsetDelegatesToEnvironment(t *testing.T) {
	upstream := sseServer(t, 200, happyFrames, nil)
	defer upstream.Close()
	var envCalls, proxyCalls atomic.Int64
	envProxy := recordingProxy(t, &proxyCalls)

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	// t.Setenv would be unreliable here: net/http memoizes the environment on the
	// first ProxyFromEnvironment call (envProxyOnce), so the outcome would depend
	// on test ordering. Inject the hook instead.
	p.envProxy = func(*http.Request) (*url.URL, error) {
		envCalls.Add(1)
		return url.Parse(envProxy.URL)
	}
	p.applyProxy()

	if _, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex", Input: userInput("hi")}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if envCalls.Load() == 0 {
		t.Fatal("an unset proxy must still consult the environment hook")
	}
	if proxyCalls.Load() == 0 {
		t.Fatal("the proxy returned by the environment hook was not used")
	}
}

func TestInvalidProxyIsRejectedAndFailsClosed(t *testing.T) {
	upstream := sseServer(t, 200, happyFrames, nil)
	defer upstream.Close()

	for _, bad := range []string{"127.0.0.1:2334", "ftp://127.0.0.1:2121", "http://", "socks5://127.0.0.1"} {
		t.Run(bad, func(t *testing.T) {
			p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
				map[string]string{"access_token": "static-token"})
			p.cfg.Proxy = bad
			p.applyProxy()

			if err := p.proxyError(); err == nil {
				t.Fatal("an unusable proxy must be reported")
			}
			// A bad proxy value is reported rather than crashing the plugin: a dead
			// plugin reaches the operator only as "handshake: EOF", which names no
			// cause. Health is where the console looks.
			healthErr, ok := pluginapi.IsError(p.Health(context.Background()))
			if !ok || healthErr.Code != "proxy_invalid" || healthErr.Kind != pluginapi.KindFatal {
				t.Fatalf("Health error = %+v, want a fatal proxy_invalid", healthErr)
			}
			if !strings.Contains(healthErr.Error(), "config") {
				t.Fatalf("the error should name where the bad value came from: %v", healthErr)
			}
			// The request path must fail closed rather than quietly going direct.
			_, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex", Input: userInput("hi")})
			if err == nil {
				t.Fatal("an unusable proxy must fail the request")
			}
			apiErr, ok := pluginapi.IsError(err)
			if !ok || apiErr.Code != "proxy_invalid" || apiErr.Kind != pluginapi.KindFatal {
				t.Fatalf("error = %+v, want a fatal proxy_invalid", err)
			}
		})
	}
}

func TestInvalidProxyFromCredentialsSurfacesInHealth(t *testing.T) {
	upstream := sseServer(t, 200, happyFrames, nil)
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	// A socks5 URL without a port is the realistic mistake; SetCredentials cannot
	// return an error, so it must surface through Health.
	p.SetCredentials(map[string]string{"access_token": "static-token", "proxy": "socks5://127.0.0.1"})

	err := p.Health(context.Background())
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Code != "proxy_invalid" || apiErr.Kind != pluginapi.KindFatal {
		t.Fatalf("Health error = %+v, want a fatal proxy_invalid", err)
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("the error should name where the bad value came from: %v", err)
	}
}

func TestDescribeMasksProxyCredentials(t *testing.T) {
	p := newTestProvider(t, "http://unused", "http://unused", "http://unused",
		map[string]string{"access_token": "static-token"})
	p.cfg.Proxy = "http://alice:s3cret@127.0.0.1:2334"
	p.applyProxy()

	state, err := p.currentState()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p.describe(state))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"s3cret", "alice"} {
		if strings.Contains(text, secret) {
			t.Fatalf("describe leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, `"proxy":"http://127.0.0.1:2334"`) {
		t.Fatalf("describe should report the masked proxy: %s", text)
	}
	if !strings.Contains(text, `"proxy_source":"config"`) {
		t.Fatalf("describe should report where the proxy came from: %s", text)
	}
}

// sseServer answers the responses endpoint with a fixed event sequence.
func sseServer(t *testing.T, status int, frames []string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"bad_request","message":"nope"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = fmt.Fprint(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

var happyFrames = []string{
	"event: response.output_text.delta" + string([]byte{0x0A}) + `data: {"type":"response.output_text.delta","delta":"hello ","item_id":"msg_1","output_index":0}` + string([]byte{0x0A}) + string([]byte{0x0A}),
	"event: response.reasoning_summary_text.delta" + string([]byte{0x0A}) + `data: {"type":"response.reasoning_summary_text.delta","delta":"thinking","item_id":"rs_1","output_index":0}` + string([]byte{0x0A}) + string([]byte{0x0A}),
	"event: response.output_text.delta" + string([]byte{0x0A}) + `data: {"type":"response.output_text.delta","delta":"world","item_id":"msg_1","output_index":0}` + string([]byte{0x0A}) + string([]byte{0x0A}),
	"event: response.completed" + string([]byte{0x0A}) + `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100,"output_tokens":40,"input_tokens_details":{"cached_tokens":60},"output_tokens_details":{"reasoning_tokens":10}}}}` + string([]byte{0x0A}) + string([]byte{0x0A}),
}

func TestCompleteAssemblesFromTheStream(t *testing.T) {
	server := sseServer(t, 200, happyFrames, nil)
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL+"/session", server.URL+"/token", map[string]string{"access_token": "static-token"})

	response, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex", Instructions: "be brief"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("items = %+v", response.Items)
	}
	if !strings.Contains(string(response.Items[0].Content), "hello world") {
		t.Fatalf("content = %s", response.Items[0].Content)
	}
	dims := response.Usage.Dimensions
	if dims["input_cache_hit"] != 60 || dims["input_cache_miss"] != 40 {
		t.Fatalf("cache dimensions = %v", dims)
	}
	if dims["output"] != 30 || dims["reasoning"] != 10 {
		t.Fatalf("output dimensions = %v", dims)
	}
}

func TestStreamTranslatesEvents(t *testing.T) {
	server := sseServer(t, 200, happyFrames, nil)
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL+"/session", server.URL+"/token", map[string]string{"access_token": "static-token"})

	var types []string
	reasons := []string{}
	err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(event pluginapi.Event) error {
		types = append(types, event.Type)
		if event.Type == pluginapi.EventFinish {
			reasons = append(reasons, event.Reason)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	want := []string{pluginapi.EventTextDelta, pluginapi.EventReasoningDelta, pluginapi.EventTextDelta, pluginapi.EventUsage, pluginapi.EventFinish}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", types, want)
	}
	if len(reasons) != 1 || reasons[0] != "stop" {
		t.Fatalf("finish reasons = %v, want [stop]", reasons)
	}
}

// TestStreamEndedWithoutTerminalEventIsRetryable: the subscription backend declares
// the end of the answer with response.completed / response.incomplete. A body that just
// stops must fail the attempt — the fragment must not be forwarded as a complete answer.
func TestStreamEndedWithoutTerminalEventIsRetryable(t *testing.T) {
	server := sseServer(t, 200, happyFrames[:3], nil)
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL+"/session", server.URL+"/token", map[string]string{"access_token": "static-token"})

	err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(pluginapi.Event) error { return nil })
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Code != "upstream_stream_incomplete" || apiErr.Kind != pluginapi.KindRetryable {
		t.Fatalf("error = %+v, want a retryable upstream_stream_incomplete", err)
	}
}

// TestStreamIncompleteCarriesTheUpstreamReason: an answer the upstream cut short (token
// limit, content filter) must reach the host with the reason attached, or the client is
// told the model finished.
func TestStreamIncompleteCarriesTheUpstreamReason(t *testing.T) {
	frames := []string{
		happyFrames[0],
		"event: response.incomplete" + string([]byte{0x0A}) + `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":5}}}` + string([]byte{0x0A}) + string([]byte{0x0A}),
	}
	server := sseServer(t, 200, frames, nil)
	defer server.Close()
	p := newTestProvider(t, server.URL, server.URL+"/session", server.URL+"/token", map[string]string{"access_token": "static-token"})

	var reasons []string
	if err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(event pluginapi.Event) error {
		if event.Type == pluginapi.EventFinish {
			reasons = append(reasons, event.Reason)
		}
		return nil
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(reasons) != 1 || reasons[0] != "max_output_tokens" {
		t.Fatalf("finish reasons = %v, want [max_output_tokens]", reasons)
	}

	// The non-streaming façade must agree, so a JSON client sees status=incomplete too.
	response, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Status != "incomplete" || response.FinishReason != "max_output_tokens" {
		t.Fatalf("status=%q finish_reason=%q", response.Status, response.FinishReason)
	}
}

func TestRefreshTokenRotationIsPersistedAndSingleFlight(t *testing.T) {
	var tokenCalls atomic.Int64
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	p := newTestProvider(t, "http://unused", "http://unused/session", tokenServer.URL, map[string]string{
		"refresh_token": "initial-refresh",
	})

	var wait sync.WaitGroup
	for index := 0; index < 5; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := p.ensureToken(context.Background(), true); err != nil {
				t.Errorf("ensureToken: %v", err)
			}
		}()
	}
	wait.Wait()

	if tokenCalls.Load() != 1 {
		t.Fatalf("token endpoint called %d times, want exactly 1 (single flight)", tokenCalls.Load())
	}
	state, err := p.currentState()
	if err != nil {
		t.Fatal(err)
	}
	if state.RefreshToken != "rotated-refresh" {
		t.Fatalf("refresh token = %q, want the rotated one", state.RefreshToken)
	}
	raw, err := os.ReadFile(filepath.Join(p.stateDir, sessionFile))
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if !strings.Contains(string(raw), "rotated-refresh") {
		t.Fatalf("rotation was not persisted: %s", raw)
	}
	if state.ExpiresAt == nil || state.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expiry not recorded: %+v", state.ExpiresAt)
	}
}

func TestSessionCookieIsExchangedForAToken(t *testing.T) {
	var seenAuth atomic.Value
	responses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range happyFrames {
			_, _ = fmt.Fprint(w, frame)
		}
	}))
	defer responses.Close()

	sessionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "__Secure-next-auth.session-token=cookie-value") {
			t.Errorf("session cookie missing: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"token-from-session","expires":"2030-01-01T00:00:00Z","authProvider":"openai"}`))
	}))
	defer sessionServer.Close()

	p := newTestProvider(t, responses.URL, sessionServer.URL, "http://unused/token", map[string]string{
		"session_cookie": "cookie-value",
	})
	if err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(pluginapi.Event) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got, _ := seenAuth.Load().(string); got != "Bearer token-from-session" {
		t.Fatalf("authorization = %q", got)
	}
}

func TestUpstream401RefreshesOnceThenSucceeds(t *testing.T) {
	var responseCalls atomic.Int64
	var tokenCalls atomic.Int64
	responses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if responseCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range happyFrames {
			_, _ = fmt.Fprint(w, frame)
		}
	}))
	defer responses.Close()

	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"refreshed","expires_in":3600}`))
	}))
	defer tokens.Close()

	p := newTestProvider(t, responses.URL, "http://unused", tokens.URL, map[string]string{"refresh_token": "r1"})
	// Seed a still-valid token so the first attempt uses it without refreshing: the
	// point of this test is that a 401 triggers exactly one refresh.
	future := time.Now().UTC().Add(time.Hour)
	if err := p.saveState(session{Mode: "refresh_token", AccessToken: "stale-token", RefreshToken: "r1", ExpiresAt: &future}); err != nil {
		t.Fatal(err)
	}
	if err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(pluginapi.Event) error { return nil }); err != nil {
		t.Fatalf("stream after refresh: %v", err)
	}
	if responseCalls.Load() != 2 {
		t.Fatalf("responses calls = %d, want 2 (original + one retry)", responseCalls.Load())
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls.Load())
	}
}

func TestInvalidGrantIsFatal(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
	}))
	defer tokens.Close()

	p := newTestProvider(t, "http://unused", "http://unused", tokens.URL, map[string]string{"refresh_token": "dead"})
	_, err := p.ensureToken(context.Background(), true)
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Kind != pluginapi.KindFatal || apiErr.Code != "token_expired" {
		t.Fatalf("error = %+v", err)
	}
	if strings.Contains(err.Error(), "dead") {
		t.Fatal("the error message must not contain the credential")
	}
}

func TestQuotaErrorCarriesResetTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()
	p := newTestProvider(t, server.URL, "http://unused", "http://unused", map[string]string{"access_token": "static"})

	err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(pluginapi.Event) error { return nil })
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Kind != pluginapi.KindQuotaExhausted {
		t.Fatalf("error = %+v", err)
	}
	if apiErr.ResetAt < time.Now().Add(time.Minute).Unix() {
		t.Fatalf("reset_at = %d, want roughly two minutes out", apiErr.ResetAt)
	}
}

func TestImportTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	payload := map[string]any{
		"tokens": map[string]any{"access_token": "file-access", "refresh_token": "file-refresh", "account_id": "acct_1"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	p := newTestProvider(t, "http://unused", "http://unused", "http://unused", map[string]string{"token_file": path})
	if _, err := p.ensureToken(context.Background(), true); err == nil {
		t.Fatal("expected the import to be followed by a refresh attempt")
	}
	state, err := p.currentState()
	if err != nil {
		t.Fatal(err)
	}
	if state.RefreshToken != "file-refresh" || state.AccessToken != "file-access" || state.AccountID != "acct_1" {
		t.Fatalf("imported state = %+v", state)
	}
}

func TestWhoamiNeverLeaksSecrets(t *testing.T) {
	p := newTestProvider(t, "http://unused", "http://unused", "http://unused", map[string]string{
		"access_token": "super-secret", "refresh_token": "also-secret", "session_cookie": "cookie-secret",
	})
	raw, err := p.RunAction(context.Background(), "whoami", nil)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	text := string(raw)
	for _, secret := range []string{"super-secret", "also-secret", "cookie-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("whoami leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "has_refresh_token") {
		t.Fatalf("whoami should report presence: %s", text)
	}
}

// ---------------------------------------------------------------------------
// M10c: the health probe is a real streaming completion
// ---------------------------------------------------------------------------

func TestHealthSendsAStreamingProbe(t *testing.T) {
	var (
		method, path, rawBody string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range happyFrames {
			_, _ = fmt.Fprint(w, frame)
		}
	}))
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if method != http.MethodPost || path != "/responses" {
		t.Fatalf("probe hit %s %s, want POST /responses", method, path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(rawBody), &body); err != nil {
		t.Fatal(err)
	}
	if stream, _ := body["stream"].(bool); !stream {
		t.Fatalf("the probe must stream, body = %s", rawBody)
	}
	if _, present := body["max_output_tokens"]; present {
		t.Fatalf("the probe must not send max_output_tokens (the upstream rejects it): %s", rawBody)
	}
	// The probe asks for the configured model ID and buildRequest maps it upstream.
	if got := body["model"]; got != "gpt-5-codex" {
		t.Fatalf("probe model = %v, want the configured upstream model", got)
	}
	if !strings.Contains(rawBody, `"text":"`+defaultHealthPrompt+`"`) {
		t.Fatalf("probe must send the configured prompt, body = %s", rawBody)
	}
}

func TestHealthRejectsATruncatedStream(t *testing.T) {
	// A body that ends before the terminal event is what Stream alone reports as
	// success (io.EOF); the probe must not call that healthy.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, happyFrames[0])
	}))
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	err := p.Health(context.Background())
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Code != "health_stream_incomplete" || apiErr.Kind != pluginapi.KindFatal {
		t.Fatalf("error = %+v, want a fatal health_stream_incomplete", err)
	}
}

func TestHealthSurfacesTheUpstreamDetailMessage(t *testing.T) {
	// The upstream uses the FastAPI shape for this class of error; dropping it made
	// a misconfigured model look like a bare "upstream returned 400 Bad Request".
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Unsupported parameter: max_output_tokens"}`))
	}))
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	err := p.Health(context.Background())
	if err == nil {
		t.Fatal("expected the probe to fail")
	}
	if !strings.Contains(err.Error(), "Unsupported parameter: max_output_tokens") {
		t.Fatalf("the upstream detail must reach the caller, got: %v", err)
	}
}

func TestHealthDoesNotMistakeAChallengeForCredentials(t *testing.T) {
	responses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><head><title>Just a moment...</title></head></html>"))
	}))
	defer responses.Close()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
	}))
	defer tokens.Close()

	// Refresh-capable credentials: Stream refreshes once on 403 and only then
	// classifies, which is exactly the path that used to yield token_expired.
	p := newTestProvider(t, responses.URL, "http://unused", tokens.URL,
		map[string]string{"refresh_token": "r1"})
	err := p.Health(context.Background())
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Code != "upstream_challenge" {
		t.Fatalf("error = %+v, want upstream_challenge (not a credential failure)", err)
	}
	if !apiErr.Retryable {
		t.Fatal("a challenge is a blocked egress, not a dead credential: it must be retryable")
	}
}

func TestHealthWithoutModelsIsExplicit(t *testing.T) {
	p := newTestProvider(t, "http://unused", "http://unused", "http://unused",
		map[string]string{"access_token": "static-token"})
	p.cfg.Models = nil

	err := p.Health(context.Background())
	apiErr, ok := pluginapi.IsError(err)
	if !ok || apiErr.Code != "health_unconfigured" {
		t.Fatalf("error = %+v, want health_unconfigured naming the missing configuration", err)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Fatalf("the error should say what is missing: %v", err)
	}
}

func TestMaxOutputTokensIsNotForwarded(t *testing.T) {
	var rawBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range happyFrames {
			_, _ = fmt.Fprint(w, frame)
		}
	}))
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	limit := 64
	if _, err := p.Complete(context.Background(), &pluginapi.Request{Model: "codex", MaxOutputTokens: &limit}); err != nil {
		t.Fatalf("a client max_output_tokens must not fail the call: %v", err)
	}
	if strings.Contains(rawBody, "max_output_tokens") {
		t.Fatalf("max_output_tokens must not reach the upstream: %s", rawBody)
	}
}

func TestSystemRoleIsRewrittenForTheUpstream(t *testing.T) {
	var rawBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range happyFrames {
			_, _ = fmt.Fprint(w, frame)
		}
	}))
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	// The shape a client like the harness sends: system prompt as an input item,
	// plus tools. This backend answers it with "System messages are not allowed"
	// unless the adapter renames the role.
	req := &pluginapi.Request{
		Model: "codex",
		Input: []pluginapi.Item{
			{Type: "message", Role: "system", Content: json.RawMessage(`[{"type":"input_text","text":"You are a software engineer."}]`)},
			{Type: "message", Role: "user", Content: json.RawMessage(`[{"type":"input_text","text":"hi"}]`)},
		},
		Tools: []pluginapi.Tool{{Type: "function", Name: "get_weather"}},
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var body struct {
		Input []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(rawBody), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 2 {
		t.Fatalf("input = %+v", body.Input)
	}
	if body.Input[0].Role != "developer" {
		t.Fatalf("upstream role = %q, want developer (this backend rejects system)", body.Input[0].Role)
	}
	if !strings.Contains(string(body.Input[0].Content), "You are a software engineer.") {
		t.Fatalf("the message text must survive the rename: %s", body.Input[0].Content)
	}
	if body.Input[1].Role != "user" {
		t.Fatalf("the user item must be untouched, role = %q", body.Input[1].Role)
	}
	if len(body.Tools) != 1 {
		t.Fatalf("tools must still be sent, got %d", len(body.Tools))
	}
	// The gateway may reuse the request (retry, recording), so the caller's slice
	// must come back unchanged.
	if req.Input[0].Role != "system" {
		t.Fatalf("rewriteSystemRoles mutated the caller's request: role = %q", req.Input[0].Role)
	}
}

func TestAdditionalToolsInputItemIsPreservedForTheUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range happyFrames {
			_, _ = fmt.Fprint(w, frame)
		}
	}))
	defer upstream.Close()

	p := newTestProvider(t, upstream.URL, upstream.URL+"/session", upstream.URL+"/token",
		map[string]string{"access_token": "static-token"})
	var req pluginapi.Request
	if err := json.Unmarshal([]byte(`{"model":"codex","input":[{"type":"additional_tools","tools":[{"type":"function","name":"bash"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), &req); err != nil {
		t.Fatal(err)
	}

	raw, err := p.buildRequest(&req, true)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var body struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if got := string(body.Input[0]["tools"]); got != `[{"type":"function","name":"bash"}]` {
		t.Fatalf("additional_tools.tools = %s", got)
	}
}

func TestOtherRolesAreLeftAlone(t *testing.T) {
	items := []pluginapi.Item{
		{Type: "message", Role: "developer"},
		{Type: "message", Role: "user"},
		{Type: "message", Role: "assistant"},
		{Type: "function_call_output", CallID: "call_1", Output: "42"},
	}
	got := rewriteSystemRoles(items)
	for i := range items {
		if got[i].Role != items[i].Role {
			t.Fatalf("item %d role = %q, want %q", i, got[i].Role, items[i].Role)
		}
	}
	if &got[0] != &items[0] {
		t.Fatal("with nothing to rewrite the original slice should be returned as-is")
	}

	withSystem := append([]pluginapi.Item{{Type: "message", Role: "system"}}, items...)
	rewritten := rewriteSystemRoles(withSystem)
	if rewritten[0].Role != "developer" {
		t.Fatalf("system must be renamed, got %q", rewritten[0].Role)
	}
	if withSystem[0].Role != "system" {
		t.Fatalf("rewriteSystemRoles must not mutate its input, got %q", withSystem[0].Role)
	}
	for i := 1; i < len(rewritten); i++ {
		if rewritten[i].Role != items[i-1].Role {
			t.Fatalf("item %d was changed: %q != %q", i, rewritten[i].Role, items[i-1].Role)
		}
	}
}

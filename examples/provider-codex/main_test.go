package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			HealthPath: "/me", TimeoutMS: 5000,
			Models: []modelConfig{{ID: "codex", UpstreamModel: "gpt-5-codex", ContextWindow: 1000, MaxOutputTokens: 100}},
		},
		http:     &http.Client{Timeout: 5 * time.Second},
		now:      func() time.Time { return time.Now().UTC() },
		stateDir: t.TempDir(),
	}
	p.SetCredentials(creds)
	return p
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
	err := p.Stream(context.Background(), &pluginapi.Request{Model: "codex"}, func(event pluginapi.Event) error {
		types = append(types, event.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	want := []string{pluginapi.EventTextDelta, pluginapi.EventReasoningDelta, pluginapi.EventTextDelta, pluginapi.EventUsage}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", types, want)
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

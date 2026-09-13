package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

// These tests drive the real data plane (the same handler every client uses) against
// several in-process testecho providers. testecho prefixes its answer with the configured
// name, so "which provider served this request" is read straight out of the reply instead
// of being inferred from internal state.

// addEchoProvider registers another testecho provider for echo-model, gives it a route and
// reloads the registry, exactly as the console would after an operator added one.
//
// testecho answers with "prefix + input", so the prefix is set to "<name>:": the answer text
// then names the provider that produced it, exactly like the x-gateway-provider header does.
func addEchoProvider(t *testing.T, f *fixture, name string, priority int, failMode string) int64 {
	t.Helper()
	ctx := context.Background()

	cfg := map[string]any{"prefix": name + ":"}
	if failMode != "" {
		cfg["fail_mode"] = failMode
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	providerID, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: name, Kind: "testecho", Enabled: true, Priority: priority, Weight: 100,
		ConfigJSON: string(raw),
	})
	if err != nil {
		t.Fatalf("upsert provider %s: %v", name, err)
	}
	if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: providerID, PublicModel: "echo-model", UpstreamModel: "echo-model",
		Enabled: true, MaxOutputTokens: 1024, CapabilitiesJSON: capabilitiesJSON,
	}); err != nil {
		t.Fatalf("upsert provider model for %s: %v", name, err)
	}
	model := f.registry.Snapshot().ModelByName["echo-model"]
	if model == nil {
		t.Fatal("the fixture's model is missing")
	}
	if _, err := f.db.UpsertRoute(ctx, &domain.Route{
		ModelID: model.ID, ProviderID: providerID, Priority: priority, Weight: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("upsert route for %s: %v", name, err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	return providerID
}

// disableProvider turns one provider off and reloads, standing in for an operator taking an
// upstream out of service.
func disableProvider(t *testing.T, f *fixture, name string) {
	t.Helper()
	provider := f.registry.Snapshot().ProviderByName[name]
	if provider == nil {
		t.Fatalf("provider %s is not in the snapshot", name)
	}
	if err := f.db.SetProviderFlags(context.Background(), provider.ID, false, provider.Draining, provider.Priority, provider.Weight); err != nil {
		t.Fatalf("disable %s: %v", name, err)
	}
	if _, err := f.registry.Reload(context.Background()); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
}

// createKey registers another API key with its own grants.
func createKey(t *testing.T, f *fixture, token, grants string) string {
	t.Helper()
	key := &domain.APIKey{
		AccountID: f.key.AccountID, Name: "narrow",
		KeyPrefix: secret.Prefix(token), KeyHash: secret.Hash(token),
		Status: "active", RecordInputMode: "inherit", GrantsJSON: grants,
	}
	if _, err := f.db.UpsertAPIKey(context.Background(), key); err != nil {
		t.Fatalf("upsert key: %v", err)
	}
	return token
}

// postSession sends one non-streaming request as token, optionally carrying a session key,
// and reports the status, the provider that answered and its answer text.
func postSession(t *testing.T, f *fixture, token, session string) (int, string, string) {
	t.Helper()
	body := `{"model":"echo-model","input":"ping"`
	if session != "" {
		body += `,"prompt_cache_key":"` + session + `"`
	}
	body += `}`

	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, resp.Header.Get("x-gateway-provider"), ""
	}
	var payload struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	text := ""
	if len(payload.Output) > 0 && len(payload.Output[0].Content) > 0 {
		text = payload.Output[0].Content[0].Text
	}
	return resp.StatusCode, resp.Header.Get("x-gateway-provider"), text
}

// answerPrefix reads the answering provider out of testecho's prefixed answer; the helper
// above makes that prefix the provider's name.
func answerPrefix(t *testing.T, text string) string {
	t.Helper()
	prefix, _, found := strings.Cut(text, ": ")
	if !found {
		t.Fatalf("unexpected answer %q", text)
	}
	return prefix
}

// TestSessionSticksToTheProviderThatAnsweredIt is the whole point of M38: two equivalent
// providers of one model sit in the same tier, so without stickiness a session's requests
// would be scattered by the weighted draw. With it, every later request of that session
// goes back to the one that answered first.
func TestSessionSticksToTheProviderThatAnsweredIt(t *testing.T) {
	f := newFixture(t)
	addEchoProvider(t, f, "echo-b", 10, "")

	status, provider, text := postSession(t, f, testToken, "session-sticky-1")
	if status != http.StatusOK || provider == "" {
		t.Fatalf("the first request must be served: status=%d provider=%q", status, provider)
	}
	if got := answerPrefix(t, text); got != provider {
		t.Fatalf("the answer came from %q while the header named %q", got, provider)
	}

	for i := 0; i < 5; i++ {
		status, next, _ := postSession(t, f, testToken, "session-sticky-1")
		if status != http.StatusOK {
			t.Fatalf("request %d failed with %d", i+2, status)
		}
		if next != provider {
			t.Fatalf("request %d was served by %q; the session is bound to %q", i+2, next, provider)
		}
	}

	stats := f.srv.deps.Router.AffinityStats()
	if !stats.Enabled || stats.Entries != 1 || stats.Hits != 5 {
		t.Fatalf("one session must hold exactly one binding and hit it five times: %+v", stats)
	}
}

// TestFailoverStaysInsideWhatTheKeyMayUse pins the boundary: the first candidate fails
// retryably, and the request moves on — but only to a provider this key is authorised for.
func TestFailoverStaysInsideWhatTheKeyMayUse(t *testing.T) {
	f := newFixture(t)
	// Priority 5 puts the failing provider first, so serving the request at all proves the
	// attempt was retried elsewhere.
	addEchoProvider(t, f, "flaky", 5, "retryable")

	status, provider, text := postSession(t, f, testToken, "session-sticky-1")
	if status != http.StatusOK || provider != "echo" {
		t.Fatalf("the request must fail over to the other authorised provider: status=%d provider=%q", status, provider)
	}
	if got := answerPrefix(t, text); got != "echo" {
		t.Fatalf("the answer came from %q", got)
	}

	// A key that may only use the failing provider must fail: it may not borrow the
	// healthy provider, and it may not inherit another key's session binding either.
	// (The tokens must differ inside the first secret.PrefixLen characters: the key
	// prefix is the unique index, so a shared prefix would rewrite the fixture's key.)
	narrow := createKey(t, f, "sk-narrow-token-0003",
		`{"models":["echo-model"],"providers":["flaky"]}`)
	status, provider, _ = postSession(t, f, narrow, "session-sticky-1")
	if status == http.StatusOK || provider != "" {
		t.Fatalf("an unauthorised provider must never serve a request: status=%d provider=%q", status, provider)
	}

	// The same failure, with a healthy provider granted too, does fail over inside the grant.
	wide := createKey(t, f, "sk-wide-token-0004",
		`{"models":["echo-model"],"providers":["flaky","echo"]}`)
	if status, provider, _ = postSession(t, f, wide, "session-sticky-2"); status != http.StatusOK || provider != "echo" {
		t.Fatalf("failover inside the grant failed: status=%d provider=%q", status, provider)
	}
}

// TestStickyTargetThatGoesAwayIsNotUsed: an upstream that is taken out of service must not
// keep a session hostage, and the binding must be forgotten rather than kept for the day it
// comes back.
func TestStickyTargetThatGoesAwayIsNotUsed(t *testing.T) {
	f := newFixture(t)
	addEchoProvider(t, f, "echo-b", 10, "")

	status, bound, _ := postSession(t, f, testToken, "session-away-1")
	if status != http.StatusOK || bound == "" {
		t.Fatalf("the first request must be served: status=%d provider=%q", status, bound)
	}

	disableProvider(t, f, bound)

	status, next, _ := postSession(t, f, testToken, "session-away-1")
	if status != http.StatusOK {
		t.Fatalf("the session must still be served by the provider that is up, got %d", status)
	}
	if next == bound || next == "" {
		t.Fatalf("the disabled provider %q must not serve the session, got %q", bound, next)
	}
	if stats := f.srv.deps.Router.AffinityStats(); stats.Stale != 1 {
		t.Fatalf("the unusable binding must be dropped: %+v", stats)
	}

	// The session re-binds to the provider that actually served it, so the next request
	// stays there even if the disabled one comes back.
	if status, again, _ := postSession(t, f, testToken, "session-away-1"); status != http.StatusOK || again != next {
		t.Fatalf("the session must re-bind to its new provider: status=%d provider=%q want %q", status, again, next)
	}
}

// TestRequestsWithoutASessionKeyDoNotTouchAffinity: a client that sends no prompt_cache_key
// (any plain OpenAI SDK) must keep exactly the behaviour it had before M38.
func TestRequestsWithoutASessionKeyDoNotTouchAffinity(t *testing.T) {
	f := newFixture(t)

	status, provider, _ := postSession(t, f, testToken, "")
	if status != http.StatusOK || provider == "" {
		t.Fatalf("the request must be served: status=%d provider=%q", status, provider)
	}
	if stats := f.srv.deps.Router.AffinityStats(); stats.Entries != 0 || stats.Hits != 0 || stats.Misses != 0 {
		t.Fatalf("a request without a session key must not take part: %+v", stats)
	}
}

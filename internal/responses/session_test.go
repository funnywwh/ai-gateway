package responses

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLogSessionUsesExplicitRootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, extra, want string
		headers           http.Header
	}{
		{"root survives child cache", `"client_metadata":{"session_id":"root","thread_id":"child"}`, "root", nil},
		{"canonical snapshot", `"client_metadata":{"session_id":"flat","x-codex-turn-metadata":"{\"session_id\":\"root\",\"thread_id\":\"child\"}"}`, "root", nil},
		{"flat survives malformed snapshot", `"client_metadata":{"session_id":"root","x-codex-turn-metadata":"{"}`, "root", nil},
		{"thread fallback", `"client_metadata":{"thread_id":"thread"}`, "thread", nil},
		{"generic metadata", `"metadata":{"session_id":"root"}`, "root", nil},
		{"header snapshot", `"client_metadata":{}`, "root", http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"root","thread_id":"child"}`}}},
		{"header root before body thread", `"client_metadata":{"thread_id":"child"}`, "root", http.Header{"Session-Id": {"root"}}},
		{"thread header", `"client_metadata":{}`, "thread", http.Header{"Thread-Id": {"thread"}}},
		{"DSH header", `"client_metadata":{}`, "root", http.Header{"Session_id": {"root"}}},
		{"body before header", `"client_metadata":{"session_id":"root"}`, "root", http.Header{"Session-Id": {"header-root"}}},
		{"do not mistake request id for session", `"client_metadata":{}`, "cache", http.Header{"X-Client-Request-Id": {"request"}}},
		{"parent is not the root", `"client_metadata":{"thread_id":"child","x-codex-parent-thread-id":"parent"}`, "child", nil},
		{"cache fallback", `"client_metadata":{}`, "cache", nil},
		{"bad metadata type", `"client_metadata":[]`, "cache", nil},
		{"bad identity type", `"client_metadata":{"session_id":42}`, "cache", nil},
		{"empty explicit id", `"client_metadata":{"session_id":"  "}`, "cache", nil},
		{"trim id", `"client_metadata":{"session_id":" root "}`, "root", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse([]byte(`{"model":"m","input":"ping","prompt_cache_key":"cache",` + tc.extra + `}`))
			if err != nil {
				t.Fatal(err)
			}
			r.SetSessionHeaders(tc.headers)
			if got := r.Dimensions("codex").SessionID; got != tc.want {
				t.Fatalf("session = %q, want %q", got, tc.want)
			}
			if r.SessionKey() != "cache" {
				t.Fatal("log identity changed routing affinity")
			}
			provider, apiErr := r.ToProviderRequest("m")
			if apiErr != nil || provider.PromptCacheKey != "cache" {
				t.Fatalf("upstream cache key changed: %+v / %v", provider, apiErr)
			}
			if string(provider.Extra["client_metadata"]) != string(r.Extra["client_metadata"]) {
				t.Fatal("metadata was rewritten")
			}
		})
	}
}

func TestLogSessionMetadataBoundsAndTitle(t *testing.T) {
	metadata, _ := json.Marshal(map[string]string{"session_id": strings.Repeat("会", 100), "x-codex-turn-metadata": `{"turn_trigger":"thread_title"}`})
	r := &Request{Input: json.RawMessage(`"ping"`), Extra: map[string]json.RawMessage{"client_metadata": metadata}}
	d := r.Dimensions("")
	if d.SessionID != strings.Repeat("会", 42) || d.Client != ClientCodex || d.CallKind != CallKindTitle {
		t.Fatalf("dimensions = %+v", d)
	}
	r.SetSessionHeaders(http.Header{"Session-Id": {"private-header-session"}})
	raw, err := json.Marshal(r)
	if err != nil || strings.Contains(string(raw), "private-header-session") {
		t.Fatalf("transport headers leaked into payload: %s / %v", raw, err)
	}
}

func BenchmarkLogSessionKey(b *testing.B) {
	for _, tc := range []struct{ name, metadata string }{
		{"cache_fallback", ""},
		{"flat", `{"session_id":"root-session","thread_id":"child-thread"}`},
		{"codex_snapshot", `{"session_id":"root-session","thread_id":"child-thread","x-codex-turn-metadata":"{\"installation_id\":\"installation\",\"session_id\":\"root-session\",\"thread_id\":\"child-thread\",\"agent_name\":\"/root/worker\",\"turn_id\":\"turn-123\",\"parent_thread_id\":\"root-thread\",\"window_id\":\"child-thread:1\",\"window_number\":1,\"request_kind\":\"turn\",\"root_turn_id\":\"root-turn\",\"thread_source\":\"subagent\",\"sandbox_mode\":\"workspace-write\"}"}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := &Request{PromptCacheKey: "cache", Extra: map[string]json.RawMessage{"client_metadata": json.RawMessage(tc.metadata)}}
			b.ReportAllocs()
			for b.Loop() {
				r.LogSessionKey()
			}
		})
	}
}

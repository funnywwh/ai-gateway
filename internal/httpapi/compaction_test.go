package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/responses"
)

// compactionUpstream is an OpenAI-compatible upstream with no compaction of its own: it answers
// every /responses call with prose, exactly like the deepseek route that produced
// "remote compaction v2 expected exactly one compaction output item, got 0 from 1 output items".
type compactionUpstream struct {
	mu      sync.Mutex
	bodies  []map[string]json.RawMessage
	reply   string
	native  bool // answer natively, i.e. produce a compaction item of its own
	failure bool
}

func (u *compactionUpstream) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		reply, native, failure := u.reply, u.native, u.failure
		u.mu.Unlock()

		if failure {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream exploded"}}`)
			return
		}
		if string(body["stream"]) != "true" {
			t.Errorf("the compaction turn must be streamed, got %s", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeFrame := func(eventType, data string) {
			_, _ = io.WriteString(w, "event: "+eventType+"\ndata: "+data+"\n\n")
		}
		switch {
		case native:
			writeFrame("response.output_item.done", `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"native-blob"}}`)
		case reply != "":
			writeFrame("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"message","id":"msg_1","role":"assistant"}}`)
			writeFrame("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg_1","delta":%q}`, reply))
			writeFrame("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, reply))
		}
		writeFrame("response.completed", `{"type":"response.completed","response":{"id":"resp_up","status":"completed","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`)
	}))
}

func compactionFixture(t *testing.T, upstream *compactionUpstream) (*httptest.Server, *fixture) {
	t.Helper()
	server := upstream.start(t)
	f := newFixture(t)
	ctx := context.Background()
	config, err := json.Marshal(map[string]any{"base_url": server.URL, "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	providerID, err := f.db.UpsertProvider(ctx, &domain.Provider{
		Name: "compat-responses", Kind: "openai-responses", Enabled: true, Weight: 100, ConfigJSON: string(config),
	})
	if err != nil {
		t.Fatal(err)
	}
	modelID, err := f.db.UpsertModel(ctx, &domain.Model{PublicName: "compat-model", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertProviderModel(ctx, &domain.ProviderModel{
		ProviderID: providerID, PublicModel: "compat-model", UpstreamModel: "compat-upstream",
		Enabled: true, CapabilitiesJSON: `{"stream":true,"tools":true}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertRoute(ctx, &domain.Route{ModelID: modelID, ProviderID: providerID, Enabled: true, Weight: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	return server, f
}

// sseEvents reads a streamed response into (type, raw data) pairs.
func sseEvents(t *testing.T, body io.Reader) [][2]string {
	t.Helper()
	var out [][2]string
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, [2]string{line, data})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return out
}

// TestRemoteCompactionTurnSynthesizesExactlyOneCompactionItem is the reported bug, end to end:
// Codex sends a normal /v1/responses call whose input ends with a compaction trigger, the
// upstream answers with prose, and the client counts compaction output items. Without the
// synthesis it counts zero and fails the thread.
func TestRemoteCompactionTurnSynthesizesExactlyOneCompactionItem(t *testing.T) {
	upstream := &compactionUpstream{reply: "## Progress\n- fixed the parser"}
	server, f := compactionFixture(t, upstream)
	defer server.Close()

	body := `{"model":"compat-model","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"compaction_trigger"}]}`
	resp := f.do(t, http.MethodPost, "/v1/responses", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d: %s", resp.StatusCode, got)
	}
	events := sseEvents(t, resp.Body)

	var compactionItems, textDeltas []string
	completed := false
	for _, event := range events {
		data := event[1]
		switch {
		case strings.Contains(data, `"type":"response.output_item.done"`) && strings.Contains(data, `"type":"compaction"`):
			compactionItems = append(compactionItems, data)
		case strings.Contains(data, `"type":"response.output_text.delta"`):
			textDeltas = append(textDeltas, data)
		case strings.Contains(data, `"type":"response.completed"`):
			completed = true
		}
	}
	if len(compactionItems) != 1 {
		t.Fatalf("compaction output items = %d, want exactly 1 (events: %d)", len(compactionItems), len(events))
	}
	if len(textDeltas) != 0 {
		t.Fatalf("the model's own prose must not be streamed as an answer: %v", textDeltas)
	}
	if !completed {
		t.Fatal("the stream must still end with response.completed")
	}
	var item struct {
		Item struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(compactionItems[0]), &item); err != nil {
		t.Fatalf("decode compaction item: %v (%s)", err, compactionItems[0])
	}
	summary, ok := responses.DecodeCompactionSummary(item.Item.EncryptedContent)
	if !ok || !strings.Contains(summary, "fixed the parser") {
		t.Fatalf("encrypted_content = %q (decoded %q, %v)", item.Item.EncryptedContent, summary, ok)
	}

	// The upstream side: the trigger item is Codex's own compact protocol and no upstream models
	// it, and a summarization turn must not be able to answer with a tool call instead.
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(upstream.bodies))
	}
	sent := string(mustMarshal(t, upstream.bodies[0]["input"]))
	if strings.Contains(sent, "compaction_trigger") {
		t.Fatalf("the trigger item reached the upstream: %s", sent)
	}
	if !strings.Contains(sent, "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("the summarization prompt is missing: %s", sent)
	}
	if tools, ok := upstream.bodies[0]["tools"]; ok && string(tools) != "null" && string(tools) != "[]" {
		t.Fatalf("tools must be cleared for a compaction turn: %s", tools)
	}
}

// TestCompactionEnvelopeIsLocalizedOnReplay: Codex replays the item it was given in every later
// request. An upstream that cannot read the envelope would silently lose the compacted history,
// which is the difference between compaction and forgetting the conversation.
func TestCompactionEnvelopeIsLocalizedOnReplay(t *testing.T) {
	upstream := &compactionUpstream{reply: "ignored reply"}
	server, f := compactionFixture(t, upstream)
	defer server.Close()

	envelope := responses.EncodeCompactionSummary("the parser bug is fixed; tests are green")
	body := fmt.Sprintf(`{"model":"compat-model","stream":true,"input":[`+
		`{"type":"compaction","encrypted_content":%q},`+
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"carry on"}]}]}`, envelope)
	resp := f.do(t, http.MethodPost, "/v1/responses", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d: %s", resp.StatusCode, got)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(upstream.bodies))
	}
	sent := string(mustMarshal(t, upstream.bodies[0]["input"]))
	// A later turn is not a compaction turn: no summarization prompt, and the history is intact.
	if strings.Contains(sent, "CONTEXT CHECKPOINT COMPACTION") {
		t.Fatalf("a normal turn must not be turned into a compaction request: %s", sent)
	}
	if !strings.Contains(sent, "the parser bug is fixed; tests are green") {
		t.Fatalf("the compacted summary must reach the upstream as text: %s", sent)
	}
	if !strings.Contains(sent, responses.CompactionSummaryPrefix[:40]) {
		t.Fatalf("the summary must carry codex's summary prefix: %s", sent)
	}
	if !strings.Contains(sent, "carry on") {
		t.Fatalf("the rest of the input must be untouched: %s", sent)
	}
}

// TestUpstreamThatAnswersNativelyIsNotDuplicated: a client that receives two compaction items
// fails exactly like one that receives none ("got 2 from N"), so the native path — the
// subscription backend or real OpenAI behind this gateway — must travel untouched.
func TestUpstreamThatAnswersNativelyIsNotDuplicated(t *testing.T) {
	upstream := &compactionUpstream{native: true}
	server, f := compactionFixture(t, upstream)
	defer server.Close()

	body := `{"model":"compat-model","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"compaction_trigger"}]}`
	resp := f.do(t, http.MethodPost, "/v1/responses", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d: %s", resp.StatusCode, got)
	}
	events := sseEvents(t, resp.Body)
	var blobs []string
	for _, event := range events {
		// Codex counts output_item.done frames; added/done carry the same item.
		if !strings.Contains(event[1], `"type":"response.output_item.done"`) || !strings.Contains(event[1], `"type":"compaction"`) {
			continue
		}
		var item struct {
			Item struct {
				EncryptedContent string `json:"encrypted_content"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(event[1]), &item); err != nil {
			t.Fatalf("decode item: %v (%s)", err, event[1])
		}
		blobs = append(blobs, item.Item.EncryptedContent)
	}
	if len(blobs) != 1 || blobs[0] != "native-blob" {
		t.Fatalf("compaction blobs = %v, want exactly the upstream's own [native-blob]", blobs)
	}
}

// TestCompactionTurnWithoutTextFails: an empty summary would tell the client the history it
// replaced was empty. Failing the turn is the lesser evil, and the code names the cause.
func TestCompactionTurnWithoutTextFails(t *testing.T) {
	upstream := &compactionUpstream{}
	server, f := compactionFixture(t, upstream)
	defer server.Close()

	body := `{"model":"compat-model","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"compaction_trigger"}]}`
	resp := f.do(t, http.MethodPost, "/v1/responses", body, nil)
	defer resp.Body.Close()
	events := sseEvents(t, resp.Body)
	if len(events) == 0 {
		t.Fatal("expected a streamed failure")
	}
	joined := ""
	for _, event := range events {
		joined += event[1] + "\n"
	}
	if !strings.Contains(joined, "compaction_empty_summary") {
		t.Fatalf("failure must name the cause: %s", joined)
	}
	if strings.Contains(joined, `"type":"compaction"`) {
		t.Fatalf("an empty summary must not be published as a compaction item: %s", joined)
	}
}

func mustMarshal(t *testing.T, value json.RawMessage) []byte {
	t.Helper()
	if len(value) == 0 {
		return []byte("null")
	}
	return value
}

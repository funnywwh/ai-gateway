package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/responses"
	"github.com/winger/ai-gateway/internal/store"
)

func TestExplicitSessionGroupsDifferentCacheKeys(t *testing.T) {
	f := newFixture(t)
	f.cfg.Recording.RecordInput = "off"
	for i := 0; i < 3; i++ {
		trigger := ""
		if i == 2 {
			trigger = `,"x-codex-turn-metadata":"{\"turn_trigger\":\"thread_title\"}"`
		}
		body := fmt.Sprintf(`{"model":"echo-model","stream":%t,"instructions":"You are a coding agent running in the Codex CLI","input":"ping","prompt_cache_key":"cache-%d","client_metadata":{"session_id":"root-session","thread_id":"thread-%d"%s}}`, i == 1, i, i, trigger)
		resp := f.postResponses(t, body)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	ctx := context.Background()
	groups, err := f.db.ListRequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "session", store.RequestLogDimensionDefaultSort, 20, 0)
	if err != nil || len(groups) != 1 || groups[0].Key != "root-session" || groups[0].Requests != 3 {
		t.Fatalf("groups = %+v / %v", groups, err)
	}
	rows, err := f.db.ListRequestLogsPage(ctx, domain.RequestLogFilter{SessionID: "root-session"}, 20, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("session drilldown = %d / %v", len(rows), err)
	}
	for _, row := range rows {
		if row.RequestJSON != "" {
			t.Fatal("off policy recorded a body")
		}
	}
	if rows[0].CallKind != "title" || rows[0].Client != "codex" || rows[0].Title == "" {
		t.Fatalf("title helper identity = %+v", rows[0])
	}
}

func TestSessionHeadersRecordedAndRedacted(t *testing.T) {
	for _, redact := range []bool{false, true} {
		t.Run(fmt.Sprint(redact), func(t *testing.T) {
			f := newFixture(t)
			if redact {
				f.cfg.Recording.RedactPaths = []string{"session_id"}
			}
			resp := f.do(t, http.MethodPost, "/v1/responses", `{"model":"echo-model","input":"ping","prompt_cache_key":"cache"}`, map[string]string{
				"Authorization": "Bearer " + testToken, "Content-Type": "application/json", "Session-Id": "root-header", "Thread-Id": "child-header",
			})
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			want := "root-header"
			if redact {
				want = ""
			}
			if row := f.latestLog(t); row.SessionID != want {
				t.Fatalf("session = %q, want %q", row.SessionID, want)
			}
		})
	}
}

func TestTitlePromptLinksIndependentThreads(t *testing.T) {
	for _, titleFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(titleFirst), func(t *testing.T) {
			f := newFixture(t)
			f.cfg.Recording.RecordInput = "user"
			f.cfg.Recording.RedactPaths = nil
			main := `{"model":"echo-model","prompt_cache_key":"main","input":[{"role":"user","content":"<environment_context><cwd>/repo</cwd></environment_context>"},{"role":"user","content":"fix this issue\n"}]}`
			title := `{"model":"echo-model","prompt_cache_key":"helper","input":[{"role":"user","content":"<environment_context><cwd>/repo</cwd></environment_context>"},{"role":"user","content":"You are a helpful assistant. You will be presented with a user prompt, and your job is to provide a short title for a task that will be created from that prompt.\n\nUser prompt:\nfix this issue"}]}`
			bodies := []string{main, title}
			if titleFirst {
				bodies = []string{title, main}
			}
			for _, body := range bodies {
				resp := f.postResponses(t, body)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatal(resp.StatusCode)
				}
			}
			groups, err := f.db.ListRequestLogDimensionsPage(context.Background(), domain.RequestLogFilter{}, "session", store.RequestLogDimensionDefaultSort, 20, 0)
			if err != nil || len(groups) != 1 || groups[0].Key != "main" || groups[0].Requests != 2 {
				t.Fatalf("groups: %+v %v", groups, err)
			}
			rows, err := f.db.ListRequestLogsPage(context.Background(), domain.RequestLogFilter{SessionID: "main"}, 20, 0)
			if err != nil || len(rows) != 2 {
				t.Fatalf("drilldown: %d %v", len(rows), err)
			}
		})
	}
}

func TestTitleFingerprintHonorsRecordingPolicy(t *testing.T) {
	f := newFixture(t)
	req, apiErr := responses.Parse([]byte(`{"model":"echo-model","prompt_cache_key":"root","input":[{"role":"user","content":"<environment_context><cwd>/repo</cwd></environment_context>"},{"role":"user","content":"hello"}]}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	for _, mode := range []string{"off", "metadata", "user", "full"} {
		f.cfg.Recording.RecordInput = mode
		f.cfg.Recording.RedactPaths = nil
		got := f.srv.recordInput(context.Background(), &domain.APIKey{}, req, "")
		if (got.TitleFingerprint != "") != (mode == "user" || mode == "full") {
			t.Fatalf("mode %s fingerprint %q", mode, got.TitleFingerprint)
		}
	}
	f.cfg.Recording.RedactPaths = []string{"input"}
	if got := f.srv.recordInput(context.Background(), &domain.APIKey{}, req, ""); got.TitleFingerprint != "" {
		t.Fatal("redacted input fingerprinted")
	}
}

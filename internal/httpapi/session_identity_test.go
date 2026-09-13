package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
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

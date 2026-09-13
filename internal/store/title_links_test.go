package store

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

func TestTitleLinksOrderAmbiguityAndIsolation(t *testing.T) {
	for _, titleFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent-first", true: "title-first"}[titleFirst], func(t *testing.T) {
			db := testDB(t)
			ctx := context.Background()
			at := time.Now().UTC()
			put := func(id, session, kind string, account int64, delta time.Duration) {
				t.Helper()
				if err := db.PutRequestLog(ctx, &domain.RequestLogRecord{RequestID: id, SessionID: session, CallKind: kind, Client: "codex", Workspace: "/repo", AccountID: account, APIKeyID: 1, TitleFingerprint: "digest", StartedAt: at.Add(delta), CreatedAt: at.Add(delta)}); err != nil {
					t.Fatal(err)
				}
			}
			check := func(want string) {
				t.Helper()
				r, err := db.GetRequestLog(ctx, "title")
				if err != nil || r.SessionID != want {
					t.Fatalf("got %+v, %v want %s", r, err, want)
				}
			}
			if titleFirst {
				put("title", "helper", "title", 1, 0)
			}
			put("main", "root", "agent", 1, time.Second)
			if !titleFirst {
				put("title", "helper", "title", 1, 0)
			}
			check("root")
			put("other-account", "foreign", "agent", 2, 0)
			check("root")
			put("later", "later-session", "agent", 1, 10*time.Minute)
			check("root")
			put("retry", "root", "agent", 1, 2*time.Second)
			check("root")
			put("competitor", "another-root", "agent", 1, 3*time.Second)
			check("helper")
			// Idempotent retries cannot override the recorded original evidence.
			put("title", "helper", "title", 1, 0)
			check("helper")
		})
	}
}

func TestTitleLinksInvalidatePublishedRollupsAndPruneEvidence(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	title := &domain.RequestLogRecord{RequestID: "title", SessionID: "helper", CallKind: "title", Client: "codex", Workspace: "/repo", AccountID: 1, APIKeyID: 1, TitleFingerprint: "digest", StartedAt: at, CreatedAt: at, Title: "Fix issue"}
	if err := db.PutRequestLog(ctx, title); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshDimensionRollups(ctx); err != nil {
		t.Fatal(err)
	}
	main := *title
	main.RequestID = "main"
	main.SessionID = "root"
	main.CallKind = "agent"
	main.Title = ""
	// Exercise the batched writer's transaction path, with separately committed evidence.
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.putRequestLogTx(ctx, tx, &main); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		page, err := db.RequestLogDimensionsPage(ctx, domain.RequestLogFilter{}, "session", "requests", 20, 0)
		if err != nil || page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].Key != "root" || page.Rows[0].Requests != 2 {
			t.Fatalf("page %+v %v", page, err)
		}
		if err = db.RefreshDimensionRollups(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.PruneRequestLogs(ctx, at.Add(time.Second), 100); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.read.QueryRow("SELECT COUNT(*) FROM request_title_links").Scan(&count); err != nil || count != 0 {
		t.Fatalf("evidence retained: %d %v", count, err)
	}
}

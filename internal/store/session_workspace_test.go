package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

func TestSessionDimensionLatestNonemptyWorkspace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	// Insert out of chronological order, with the lexicographically greatest
	// workspace on the oldest request and an empty workspace on the newest.
	for i, rec := range []domain.RequestLogRecord{
		{SessionID: "moving", Workspace: "/a-new", CreatedAt: h.Add(time.Hour)},
		{SessionID: "moving", Workspace: "/z-old", CreatedAt: h},
		{SessionID: "moving", CreatedAt: h.Add(2 * time.Hour)},
		{SessionID: "empty", CreatedAt: h},
		{SessionID: "tied", Workspace: "/a", CreatedAt: h},
		{SessionID: "tied", Workspace: "/z", CreatedAt: h},
	} {
		rec.RequestID = fmt.Sprintf("workspace-%d", i)
		seedDimensionRow(t, db, &rec)
	}
	cases := []struct {
		name   string
		filter domain.RequestLogFilter
		want   map[string]string
	}{
		{"all", domain.RequestLogFilter{}, map[string]string{"moving": "/a-new", "empty": "", "tied": "/z"}},
		{"time-filter", domain.RequestLogFilter{To: h.Add(time.Minute)}, map[string]string{"moving": "/z-old", "empty": "", "tied": "/z"}},
		{"workspace-filter", domain.RequestLogFilter{Workspace: "/z-old"}, map[string]string{"moving": "/z-old"}},
	}
	for _, mode := range []string{"raw", "rolled", "mixed", "disabled"} {
		if mode == "rolled" {
			if err := db.RefreshDimensionRollups(ctx); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := db.read.QueryRow(`SELECT COUNT(*) FROM request_dimension_rollups`).Scan(&n); err != nil || n == 0 {
				t.Fatalf("rollups not populated: %d %v", n, err)
			}
		}
		if mode == "mixed" {
			seedDimensionRow(t, db, &domain.RequestLogRecord{RequestID: "workspace-live", SessionID: "moving", Workspace: "/0-live", CreatedAt: h.Add(3 * time.Hour)})
			cases[0].want["moving"] = "/0-live"
		}
		if mode == "disabled" {
			db.SetDimensionRollupsEnabled(false)
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				got, err := db.RequestLogDimensionsPage(ctx, tc.filter, "session", "", 20, 0)
				if err != nil {
					t.Fatal(err)
				}
				workspaces := map[string]string{}
				for _, row := range got.Rows {
					workspaces[row.Key] = row.Workspace
				}
				if !reflect.DeepEqual(workspaces, tc.want) {
					t.Fatalf("workspaces = %v, want %v", workspaces, tc.want)
				}
				if want := dimensionOracle(t, db, tc.filter, "session", "", 20, 0); !reflect.DeepEqual(got, want) {
					t.Fatalf("page = %+v, reference = %+v", got, want)
				}
			})
		}
	}
}

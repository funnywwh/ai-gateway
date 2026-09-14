package store

import (
	"context"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// A tag is addressed by id for updates. The importer created rows whose name is not an
// ASCII identifier (蓝精灵1/2/3), and an upsert keyed by name could not touch them.
func TestTagUpdateByIDHandlesNonASCIINames(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	id, err := db.UpsertTag(ctx, &domain.Tag{
		Name: "蓝精灵1", Description: "绑定 Codex 供应商",
		GrantsJSON: `{"providers":["codex-1"],"models":["*"]}`, Priority: 100,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("upsert returned id %d", id)
	}

	got, err := db.GetTagByID(ctx, id)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Name != "蓝精灵1" || got.GrantsJSON != `{"providers":["codex-1"],"models":["*"]}` {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	got.GrantsJSON = `{"providers":["codex-1","deepseek"],"models":["*"]}`
	got.Priority = 30
	if err := db.UpdateTag(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := db.GetTagByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.GrantsJSON, "deepseek") || after.Priority != 30 {
		t.Fatalf("update did not land: %+v", after)
	}

	// The row keeps its id and is still reachable by its (unchanged) name.
	byName, err := db.GetTagByName(ctx, "蓝精灵1")
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != id {
		t.Fatalf("update moved the row: id %d, want %d", byName.ID, id)
	}

	if _, err := db.GetTagByID(ctx, 424242); !domain.IsNotFound(err) {
		t.Fatalf("unknown id error = %v, want not found", err)
	}
	if err := db.UpdateTag(ctx, &domain.Tag{ID: 424242, Name: "gone"}); !domain.IsNotFound(err) {
		t.Fatalf("update of an unknown id error = %v, want not found", err)
	}
	if err := db.UpdateTag(ctx, &domain.Tag{Name: "no-id"}); err == nil {
		t.Fatal("update without an id must be refused")
	}
}

func TestTagUpdateRejectsDuplicateName(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	first, err := db.UpsertTag(ctx, &domain.Tag{Name: "first", GrantsJSON: `{"models":["a"]}`, Priority: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertTag(ctx, &domain.Tag{Name: "second", GrantsJSON: `{"models":["b"]}`, Priority: 100}); err != nil {
		t.Fatal(err)
	}

	row, err := db.GetTagByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	row.Name = "second"
	if err := db.UpdateTag(ctx, row); !domain.HasStatus(err, 409) {
		t.Fatalf("renaming onto an existing name error = %v, want conflict", err)
	}
}

package registry

import (
	"context"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

func TestModelReasoningReloadKeepsOldSnapshot(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	model := &domain.Model{PublicName: "reasoning", Enabled: true, ReasoningJSON: `{"mode":"force","effort":"high"}`}
	if _, err := db.UpsertModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	reg := New(db)
	first, err := reg.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	model.ReasoningJSON = ""
	if _, err := db.UpsertModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	second, err := reg.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.ModelByName[model.PublicName].ReasoningJSON != "" {
		t.Fatal("clear was not reloaded")
	}
	if first.ModelByName[model.PublicName].ReasoningJSON != `{"mode":"force","effort":"high"}` {
		t.Fatal("old snapshot was mutated")
	}
}

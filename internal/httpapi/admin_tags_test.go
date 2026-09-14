package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// The bug these tests pin: a tag whose name is not an ASCII identifier could not be
// edited at all. The only write path was POST /tags — an upsert keyed by that very
// name — so every edit of 蓝精灵1/2/3 answered 400 and the console's 编辑 button was
// useless for exactly the tags a deployment had imported.

func TestAdminTagUpsertAcceptsNonASCIIName(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	resp := f.call(t, http.MethodPost, "/admin/api/v1/tags",
		`{"name":"蓝精灵3","description":"绑定 Codex 供应商","grants":{"providers":["codex"],"models":["*"]}}`, cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("creating a CJK-named tag status = %d: %v", resp.StatusCode, payload)
	}
	if payload["name"] != "蓝精灵3" {
		t.Fatalf("name round trip = %v", payload["name"])
	}
	if _, ok := payload["id"].(float64); !ok {
		t.Fatalf("created tag has no numeric id: %v", payload)
	}
}

func TestAdminTagNameValidation(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"empty name", `{"name":"   "}`, http.StatusBadRequest},
		{"too long", `{"name":"` + strings.Repeat("x", 65) + `"}`, http.StatusBadRequest},
		{"too long in runes, not bytes", `{"name":"` + strings.Repeat("中", 65) + `"}`, http.StatusBadRequest},
		{"64 characters is the limit", `{"name":"` + strings.Repeat("x", 64) + `"}`, http.StatusOK},
		{"64 CJK characters are 64 characters", `{"name":"` + strings.Repeat("中", 64) + `"}`, http.StatusOK},
		{"hyphen and dot stay valid", `{"name":"team.a-b"}`, http.StatusOK},
		{"CJK with punctuation", `{"name":"蓝精灵 3 号（测试）"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.call(t, http.MethodPost, "/admin/api/v1/tags", tc.body, cookie)
			payload := decodeJSONBody(t, resp)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %v", resp.StatusCode, tc.status, payload)
			}
		})
	}
}

func TestAdminTagPatchUpdatesNonASCIINamedTagByID(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	// Seed the row the way the importer did: straight into the store, with a name the
	// ASCII rule would have rejected.
	id, err := f.db.UpsertTag(ctx, &domain.Tag{
		Name: "蓝精灵1", Description: "绑定 Codex 供应商",
		GrantsJSON: `{"providers":["liuhui-wisskys-8-expiry"],"models":["*"]}`, Priority: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The same edit through the name-keyed upsert still works (the name is accepted now),
	// but it cannot express "these grants on that row" — that is what the id path is for.
	resp := f.call(t, http.MethodPatch, "/admin/api/v1/tags/"+itoa(id),
		`{"grants":{"providers":["liuhui-wisskys-8-expiry","deepseek"],"models":["*"]},"description":"绑定 Codex 供应商 + deepseek 官方"}`, cookie)
	payload := decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d: %v", resp.StatusCode, payload)
	}
	if payload["name"] != "蓝精灵1" {
		t.Fatalf("patch changed the name: %v", payload["name"])
	}
	if got := payload["description"]; got != "绑定 Codex 供应商 + deepseek 官方" {
		t.Fatalf("description = %v", got)
	}
	assertJSONStrings(t, payload["grants"], `{"models":["*"],"providers":["liuhui-wisskys-8-expiry","deepseek"]}`)

	// The datapath reads the registry snapshot, so the patch must have reloaded it.
	snap := f.snapshot(t)
	tag := snap.TagByName["蓝精灵1"]
	if tag == nil {
		t.Fatal("patched tag is missing from the reloaded snapshot")
	}
	if !strings.Contains(tag.GrantsJSON, "deepseek") {
		t.Fatalf("snapshot still has the old grants: %q", tag.GrantsJSON)
	}

	// Partial update: fields left out keep their stored value.
	partial := f.call(t, http.MethodPatch, "/admin/api/v1/tags/"+itoa(id), `{"priority":30}`, cookie)
	partialPayload := decodeJSONBody(t, partial)
	if partial.StatusCode != http.StatusOK {
		t.Fatalf("partial patch status = %d: %v", partial.StatusCode, partialPayload)
	}
	if partialPayload["priority"] != float64(30) {
		t.Fatalf("priority = %v", partialPayload["priority"])
	}
	if partialPayload["description"] != "绑定 Codex 供应商 + deepseek 官方" {
		t.Fatalf("an omitted field was not preserved: %v", partialPayload["description"])
	}
	assertJSONStrings(t, partialPayload["grants"], `{"models":["*"],"providers":["liuhui-wisskys-8-expiry","deepseek"]}`)
}

func TestAdminTagPatchRejectsRenameAndUnknownID(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	id, err := f.db.UpsertTag(ctx, &domain.Tag{
		Name: "蓝精灵2", GrantsJSON: `{"providers":["lizhichao-wisskys-3-expiry"],"models":["*"]}`, Priority: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Echoing the current name is what the console form does; it must stay a no-op.
	echo := f.call(t, http.MethodPatch, "/admin/api/v1/tags/"+itoa(id), `{"name":"蓝精灵2"}`, cookie)
	_ = decodeJSONBody(t, echo)
	if echo.StatusCode != http.StatusOK {
		t.Fatalf("echoing the current name status = %d, want 200", echo.StatusCode)
	}

	// A real rename is refused: bindings live in accounts.tags_json / api_keys.tags_json by
	// name, so renaming would silently drop them.
	renamed := f.call(t, http.MethodPatch, "/admin/api/v1/tags/"+itoa(id), `{"name":"renamed"}`, cookie)
	if renamed.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename status = %d, want 400", renamed.StatusCode)
	}
	if msg := apiErrorMessage(t, renamed); !strings.Contains(msg, "cannot be renamed") {
		t.Fatalf("rename error does not explain itself: %q", msg)
	}

	missing := f.call(t, http.MethodPatch, "/admin/api/v1/tags/424242", `{"priority":5}`, cookie)
	_ = decodeJSONBody(t, missing)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown tag status = %d, want 404", missing.StatusCode)
	}

	// The rejected writes must not have touched the row.
	stored, err := f.db.GetTagByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "蓝精灵2" || stored.Priority != 100 {
		t.Fatalf("a rejected write changed the row: %+v", stored)
	}
}

func TestAdminTagPatchValidatesBody(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	ctx := context.Background()

	id, err := f.db.UpsertTag(ctx, &domain.Tag{Name: "grants-check", GrantsJSON: `{"models":["*"]}`, Priority: 100})
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/api/v1/tags/" + itoa(id)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"grants must be an object", `{"grants":["deepseek"]}`},
		{"grants must be valid JSON", `{"grants":`},
		{"policy rejects fields the gateway never reads", `{"policy":{"rate_limit":{"rpm":5}}}`},
		{"negative priority", `{"priority":-1}`},
		{"blank name", `{"name":"  "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.call(t, http.MethodPatch, path, tc.body, cookie)
			_ = decodeJSONBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}

	// Clearing is explicit, not accidental: null removes the field.
	cleared := f.call(t, http.MethodPatch, path, `{"grants":null,"policy":null}`, cookie)
	clearedPayload := decodeJSONBody(t, cleared)
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("clearing status = %d: %v", cleared.StatusCode, clearedPayload)
	}
	if clearedPayload["grants"] != nil || clearedPayload["policy"] != nil {
		t.Fatalf("null did not clear the fields: %v / %v", clearedPayload["grants"], clearedPayload["policy"])
	}
}

func TestAdminTagPatchRequiresAdminRole(t *testing.T) {
	f := newAdminFixture(t)
	viewer := f.login(t, "reader", adminPassword)
	ctx := context.Background()

	id, err := f.db.UpsertTag(ctx, &domain.Tag{Name: "role-check", GrantsJSON: `{"models":["*"]}`, Priority: 100})
	if err != nil {
		t.Fatal(err)
	}
	resp := f.call(t, http.MethodPatch, "/admin/api/v1/tags/"+itoa(id), `{"grants":{"models":["other"]}}`, viewer)
	_ = decodeJSONBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer patch status = %d, want 403", resp.StatusCode)
	}
}

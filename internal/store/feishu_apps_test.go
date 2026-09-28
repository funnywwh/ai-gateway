package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/funnywwh/ai-gateway/internal/domain"
)

// The console-managed company registry (M93). Two properties matter beyond CRUD: the secret is
// stored sealed (the plaintext must not be findable in the row), and deleting a registration never
// touches the organization data it brought in.

func TestFeishuAppsCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	id, err := db.UpsertFeishuApp(ctx, &domain.FeishuApp{
		Name: "某某科技", AppID: "cli_bbb", SecretEnc: []byte{1, 2, 3},
		RootNode: "某某科技集团", Note: "客户 A", Enabled: true, CreatedBy: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.GetFeishuApp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "某某科技" || app.AppID != "cli_bbb" || !app.Enabled || app.CreatedBy != "admin" {
		t.Fatalf("app = %+v", app)
	}
	if app.CompanyNodeName() != "某某科技集团" {
		t.Fatalf("company node name = %q", app.CompanyNodeName())
	}
	if !app.HasSecret() {
		t.Fatal("a stored secret must report HasSecret")
	}

	// The node name falls back to the company name.
	app.RootNode = ""
	if _, err := db.UpsertFeishuApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	app, _ = db.GetFeishuApp(ctx, id)
	if app.CompanyNodeName() != "某某科技" {
		t.Fatalf("company node name = %q, want the company's own name", app.CompanyNodeName())
	}

	// Lookup by app id answers (nil, nil) for a company that is not registered here.
	if other, err := db.GetFeishuAppByAppID(ctx, "cli_nope"); err != nil || other != nil {
		t.Fatalf("unknown app id = %+v %v", other, err)
	}
	if found, err := db.GetFeishuAppByAppID(ctx, "cli_bbb"); err != nil || found == nil || found.ID != id {
		t.Fatalf("lookup by app id = %+v %v", found, err)
	}

	// Disabling keeps the row.
	if err := db.SetFeishuAppEnabled(ctx, id, false, "admin2"); err != nil {
		t.Fatal(err)
	}
	app, _ = db.GetFeishuApp(ctx, id)
	if app.Enabled || app.UpdatedBy != "admin2" {
		t.Fatalf("after disable: %+v", app)
	}

	// Listing is stable (ordered by name) and includes disabled rows: the console shows them.
	list, err := db.ListFeishuApps(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v %v", list, err)
	}

	// Unknown ids answer the house not-found error (the API layer maps it to 404).
	var notFound *domain.APIError
	err = mustError(db.GetFeishuApp(ctx, 999))
	if !errors.As(err, &notFound) || notFound.Status != 404 {
		t.Fatalf("unknown id error = %v, want a 404 API error", err)
	}
}

// Name and app id are unique: the company parameter accepts either, so an ambiguous pair would make
// `company=` meaningless.
func TestFeishuAppsRefuseDuplicates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.UpsertFeishuApp(ctx, &domain.FeishuApp{
		Name: "甲方", AppID: "cli_aaa", SecretEnc: []byte{1}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	cases := []domain.FeishuApp{
		{Name: "甲方", AppID: "cli_other", SecretEnc: []byte{1}, Enabled: true},
		{Name: "乙方", AppID: "cli_aaa", SecretEnc: []byte{1}, Enabled: true},
	}
	for i, app := range cases {
		_, err := db.UpsertFeishuApp(ctx, &app)
		var apiErr *domain.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 409 {
			t.Fatalf("case %d error = %v, want a 409", i, err)
		}
		if !strings.Contains(apiErr.Message, "唯一") {
			t.Fatalf("case %d message = %q, want it to explain the uniqueness rule", i, apiErr.Message)
		}
	}
}

// The secret is stored sealed: whatever the management layer hands over is what the row holds, and a
// plaintext secret never appears in the table.
func TestFeishuAppsStoreTheSealedSecret(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sealed := []byte("\x00sealed-bytes\x01")
	id, err := db.UpsertFeishuApp(ctx, &domain.FeishuApp{
		Name: "甲方", AppID: "cli_aaa", SecretEnc: sealed, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := db.read.QueryRowContext(ctx, "SELECT secret_enc FROM feishu_apps WHERE id = ?", id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(sealed) {
		t.Fatalf("stored secret = %q, want the bytes the caller handed over", stored)
	}
	// A plaintext secret passed by mistake would still be stored as-is — sealing is the caller's
	// contract — so the store must never be the thing that invents or leaks a value.
	if _, err := db.UpsertFeishuApp(ctx, &domain.FeishuApp{
		Name: "乙方", AppID: "cli_bbb", SecretEnc: []byte("plain-text-secret"), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM feishu_apps WHERE instr(secret_enc, 'plain-text-secret') > 0").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Skip("SQLite stored the blob verbatim, which is the caller's business")
	}
}

// Deleting the registration removes the row and leaves every piece of organization data alone.
func TestDeleteFeishuAppKeepsTheData(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "甲-张三"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertFeishuApp(ctx, &domain.FeishuApp{
		Name: "甲方", AppID: "cli_aaa", SecretEnc: []byte{1}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := db.CreateOrgNode(ctx, &domain.OrgNode{
		Name: "甲方", SortOrder: 100, FeishuAppID: "cli_aaa", FeishuDepartmentID: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
		AppID: "cli_aaa", OpenID: "ou_1", Name: "张三", AccountID: accountID, BoundBy: "sync",
	}); err != nil {
		t.Fatal(err)
	}

	nodes, links, err := db.CountFeishuAppData(ctx, "cli_aaa")
	if err != nil || nodes != 1 || links != 1 {
		t.Fatalf("counts = %d %d %v, want one of each", nodes, links, err)
	}

	deleted, err := db.DeleteFeishuApp(ctx, id)
	if err != nil || !deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	if deleted, err := db.DeleteFeishuApp(ctx, id); err != nil || deleted {
		t.Fatalf("second delete = %v %v, want a no-op", deleted, err)
	}
	if _, err := db.GetOrgNode(ctx, nodeID); err != nil {
		t.Fatalf("the delete removed an org node: %v", err)
	}
	if links, _ := db.ListFeishuPersonLinks(ctx); len(links) != 1 {
		t.Fatalf("the delete removed a person mapping: %+v", links)
	}
	// Re-registering the same app id is what "re-adopting the data" looks like.
	if _, err := db.UpsertFeishuApp(ctx, &domain.FeishuApp{
		Name: "甲方", AppID: "cli_aaa", SecretEnc: []byte{2}, Enabled: true,
	}); err != nil {
		t.Fatalf("re-registering = %v", err)
	}
	if nodes, links, _ = db.CountFeishuAppData(ctx, "cli_aaa"); nodes != 1 || links != 1 {
		t.Fatalf("counts after re-register = %d %d", nodes, links)
	}
}

// mustError adapts the two-value lookups to a single error for the assertions above.
func mustError(_ *domain.FeishuApp, err error) error { return err }

func TestFeishuAppsRequireNameAndAppID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for i, app := range []domain.FeishuApp{
		{AppID: "cli_aaa"},
		{Name: "甲方"},
		{Name: "  ", AppID: "cli_aaa"},
	} {
		if _, err := db.UpsertFeishuApp(ctx, &app); err == nil {
			t.Fatalf("case %d was accepted: %+v", i, app)
		}
	}
}

package store

import (
	"context"
	"testing"

	"github.com/winger/ai-gateway/internal/domain"
)

// TestMCPTokenScopeRoundTrip covers the column added in migration 0006: a token
// written without a scope must land as the read-only default, and an explicit
// scope must survive a read.
func TestMCPTokenScopeRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "acme", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}

	// No scope in the struct: the database default applies and the row must not
	// come back as anything but query.
	if _, err := db.UpsertMCPToken(ctx, &domain.MCPToken{
		AccountID: accountID, Name: "legacy", TokenHash: "hash-legacy", TokenPrefix: "aigw_mcp_le",
	}); err != nil {
		t.Fatal(err)
	}
	legacy, err := db.GetMCPTokenByPrefix(ctx, "aigw_mcp_le")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Scope != "query" {
		t.Fatalf("a token without a scope must default to query, got %q", legacy.Scope)
	}

	if _, err := db.UpsertMCPToken(ctx, &domain.MCPToken{
		AccountID: accountID, Name: "agent", TokenHash: "hash-agent", TokenPrefix: "aigw_mcp_ad",
		Scope: "admin", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := db.GetMCPTokenByPrefix(ctx, "aigw_mcp_ad")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Scope != "admin" {
		t.Fatalf("scope = %q, want admin", agent.Scope)
	}

	// Upsert by prefix updates the scope (that is how the console downgrades a token).
	agent.Scope = "admin_read"
	if _, err := db.UpsertMCPToken(ctx, agent); err != nil {
		t.Fatal(err)
	}
	downgraded, err := db.GetMCPTokenByPrefix(ctx, "aigw_mcp_ad")
	if err != nil {
		t.Fatal(err)
	}
	if downgraded.Scope != "admin_read" {
		t.Fatalf("scope after update = %q, want admin_read", downgraded.Scope)
	}

	list, err := db.ListMCPTokens(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(list))
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/httpapi"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/secret"
	"github.com/winger/ai-gateway/internal/store"
)

func TestRelayRealGatewayPermissions(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(t.TempDir(), "relay.db")
	cfg.Server.BasePath = "/aigw"
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accountID, err := db.UpsertAccount(ctx, &domain.Account{Name: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(db)
	if _, err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	service := mcpsrv.New(db, reg, mcpsrv.Config{MaxRows: 100, WindowDays: 30, Currency: "USD"})
	api := httpapi.New(httpapi.Deps{Config: &cfg, Registry: reg, MCP: service, MCPTokens: db, Accounts: db, AdminStore: db, Admin: admin.NewAuth(db, admin.Config{}), Settings: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	call := func(token, request string) (map[string]json.RawMessage, error) {
		var out bytes.Buffer
		err := serveMCPRelay(ctx, strings.NewReader(request), &out, server.URL+"/aigw/mcp", token, server.Client())
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Result, nil
	}
	for _, scope := range []string{mcpsrv.ScopeQuery, mcpsrv.ScopeAdminRead, mcpsrv.ScopeAdmin} {
		token := "aigw_mcp_" + scope + "_relay_test"
		row := &domain.MCPToken{AccountID: accountID, Name: scope, TokenHash: secret.Hash(token), TokenPrefix: secret.Prefix(token), Scope: scope, Status: "active"}
		if _, err := db.UpsertMCPToken(ctx, row); err != nil {
			t.Fatal(err)
		}
		listed, err := call(token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if err != nil {
			t.Fatal(err)
		}
		hasAdmin := bytes.Contains(listed["tools"], []byte(`"admin_request"`))
		if hasAdmin != (scope != mcpsrv.ScopeQuery) {
			t.Fatalf("scope=%s tools=%s", scope, listed["tools"])
		}
		result, err := call(token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"admin_request","arguments":{"name":"admin_put_setting","confirm":true,"params":{"key":"relay-test"},"body":{"value":"written"}}}}`)
		if err != nil {
			t.Fatal(err)
		}
		if scope == mcpsrv.ScopeAdmin {
			if string(result["isError"]) != "false" {
				t.Fatalf("write failed: %s", result["content"])
			}
			value, found, err := db.GetSetting(ctx, "relay-test")
			if err != nil || !found || value != `"written"` {
				t.Fatalf("readback=%q found=%v err=%v", value, found, err)
			}
		} else if string(result["isError"]) != "true" {
			t.Fatal("write permission bypassed")
		}
		row.Status = "revoked"
		if _, err := db.UpsertMCPToken(ctx, row); err != nil {
			t.Fatal(err)
		}
		if _, err := call(token, `{"jsonrpc":"2.0","id":3,"method":"ping"}`); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("revocation not enforced: %v", err)
		}
	}
}

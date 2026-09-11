package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/secret"
)

// MCPTokens is the persistence subset needed by the MCP endpoint.
type MCPTokens interface {
	GetMCPTokenByPrefix(ctx context.Context, prefix string) (*domain.MCPToken, error)
	TouchMCPToken(ctx context.Context, id int64) error
}

// MCPQuery is the MCP protocol service (query tools plus, when the token scope
// allows it, the administrative tool surface).
type MCPQuery interface {
	Handle(ctx context.Context, principal mcpsrv.Principal, raw []byte) *mcpsrv.Response
}

// MCPBackendBinder is implemented by an MCP service that accepts the
// administrative tool surface. New() binds the bridge automatically, so the
// composition root does not have to know about it.
type MCPBackendBinder interface {
	SetBackend(backend mcpsrv.Backend)
}

// handleMCP implements POST /mcp.
//
// Authentication uses a dedicated MCP token (aigw_mcp_…), never an API key. The
// token's scope decides what it may do: query tokens only read their own account,
// admin_read/admin tokens also reach the administrative tool surface described in
// internal/httpapi/mcp_admin.go.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.deps.MCP == nil || s.deps.MCPTokens == nil {
		writeAPIError(w, domain.ErrUnsupported("the MCP query service is disabled"))
		return
	}
	if !s.deps.Config.MCP.Enabled {
		writeAPIError(w, domain.ErrUnsupported("the MCP query service is disabled"))
		return
	}

	token := secret.Normalize(r.Header.Get("Authorization"))
	if token == "" {
		token = secret.Normalize(r.Header.Get("x-api-key"))
	}
	if token == "" {
		writeAPIError(w, domain.ErrUnauthorized("missing MCP token"))
		return
	}
	row, err := s.deps.MCPTokens.GetMCPTokenByPrefix(r.Context(), secret.Prefix(token))
	if err != nil || !secret.Equal(row.TokenHash, secret.Hash(token)) {
		writeAPIError(w, domain.ErrUnauthorized("invalid MCP token"))
		return
	}
	if row.Status != "active" {
		writeAPIError(w, domain.ErrUnauthorized("MCP token is not active"))
		return
	}
	now := time.Now().UTC()
	if row.ExpiresAt != nil && now.After(*row.ExpiresAt) {
		writeAPIError(w, domain.ErrUnauthorized("MCP token has expired"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.deps.Config.Server.MaxBodyBytes))
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("failed to read the request body"))
		return
	}

	// An unknown scope is normalized to query, so a row written by an older version
	// can never escalate by accident.
	principal := mcpsrv.Principal{
		AccountID: row.AccountID,
		TokenID:   row.ID,
		Name:      row.Name,
		Scope:     mcpsrv.NormalizeScope(row.Scope),
	}
	resp := s.deps.MCP.Handle(r.Context(), principal, body)

	// last_used_at is best-effort and out of band.
	id := row.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.deps.MCPTokens.TouchMCPToken(ctx, id)
	}()

	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if resp != nil && resp.Error != nil && resp.Error.Code == mcpsrv.CodeUnauthorized {
		status = http.StatusUnauthorized
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

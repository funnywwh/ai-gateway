package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/mcpsrv"
	"github.com/funnywwh/ai-gateway/internal/secret"
)

// MCPTokens is the persistence subset needed by the MCP endpoint.
type MCPTokens interface {
	// GetMCPTokenByHash is the lookup the /mcp verifier performs: the presented token is
	// hashed and the row is fetched by that hash (M87), so a display prefix shared with
	// another token cannot make two tokens ambiguous.
	GetMCPTokenByHash(ctx context.Context, hash string) (*domain.MCPToken, error)
	// GetMCPTokenByID is what the console chat uses to resolve the token a conversation is
	// bound to. It looks the row up by id rather than by secret, because the chat never holds
	// the plaintext.
	GetMCPTokenByID(ctx context.Context, id int64) (*domain.MCPToken, error)
	TouchMCPToken(ctx context.Context, id int64) error
}

// mcpPrincipalKey carries an already-resolved principal into the MCP handler.
//
// It exists for the console chat, which is a client of this endpoint rather than a holder of
// privileges: it presents a token an administrator already issued, so the handler must not
// re-authenticate it from a header it never sent. The key is unexported and the setter is
// package-private, so nothing outside this package can mint an identity for /mcp.
type mcpPrincipalKey struct{}

func withMCPPrincipal(r *http.Request, p mcpsrv.Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), mcpPrincipalKey{}, p))
}

func mcpPrincipalFrom(ctx context.Context) (mcpsrv.Principal, bool) {
	p, ok := ctx.Value(mcpPrincipalKey{}).(mcpsrv.Principal)
	return p, ok
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

	body, err := io.ReadAll(io.LimitReader(r.Body, s.deps.Config.Server.MaxBodyBytes))
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("failed to read the request body"))
		return
	}

	// The console chat calls this handler with the token already resolved, because it holds
	// no credential of its own: it presents one an administrator issued. Everything below —
	// scope checks, confirm requirements, audit rows — is shared with the token path, so an
	// in-process caller cannot reach anything a socket client could not.
	if principal, ok := mcpPrincipalFrom(r.Context()); ok {
		s.serveMCP(w, r, principal, body)
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
	hash := secret.Hash(token)
	row, err := s.deps.MCPTokens.GetMCPTokenByHash(r.Context(), hash)
	if err != nil || !secret.Equal(row.TokenHash, hash) {
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

	// An unknown scope is normalized to query, so a row written by an older version
	// can never escalate by accident.
	principal := mcpsrv.Principal{
		AccountID: row.AccountID,
		TokenID:   row.ID,
		Name:      row.Name,
		Scope:     mcpsrv.NormalizeScope(row.Scope),
	}

	// last_used_at is best-effort and out of band.
	id := row.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.deps.MCPTokens.TouchMCPToken(ctx, id)
	}()

	s.serveMCP(w, r, principal, body)
}

// serveMCP answers one authenticated JSON-RPC request.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request, principal mcpsrv.Principal, body []byte) {
	resp := s.deps.MCP.Handle(r.Context(), principal, body)

	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if resp != nil && resp.Error != nil && resp.Error.Code == mcpsrv.CodeUnauthorized {
		status = http.StatusUnauthorized
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

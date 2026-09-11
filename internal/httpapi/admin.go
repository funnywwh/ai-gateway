package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/admin"
	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/secret"
	"github.com/winger/ai-gateway/internal/store"
)

// AdminStore is the persistence subset the management API needs.
type AdminStore interface {
	ListAccounts(ctx context.Context) ([]*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	GetAccountByName(ctx context.Context, name string) (*domain.Account, error)
	ListAPIKeys(ctx context.Context, accountID int64) ([]*domain.APIKey, error)
	UpsertAPIKey(ctx context.Context, k *domain.APIKey) (int64, error)
	ListRequestLogs(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.RequestLogRecord, error)
	ListRequestLogsPage(ctx context.Context, accountID int64, from, to time.Time, limit, offset int) ([]*domain.RequestLogRecord, error)
	CountRequestLogs(ctx context.Context, accountID int64, from, to time.Time) (int, error)
	GetRequestLog(ctx context.Context, requestID string) (*domain.RequestLogRecord, error)
	InsertAudit(ctx context.Context, e *AuditEntry) error
	ListAudit(ctx context.Context, limit int) ([]*AuditEntry, error)
	ListAuditPage(ctx context.Context, limit, offset int) ([]*AuditEntry, error)
	CountAudit(ctx context.Context) (int, error)
}

// AuditEntry is the store's audit record type (aliased so the port stays narrow).
type AuditEntry = store.AuditEntry

const adminCookieName = "aigw_admin"

// adminActor returns the authenticated administrator, or writes 401/403 and reports false.
//
// Two identities are accepted: a browser session cookie, and the synthetic
// principal the MCP admin bridge injects for an in-process call (mcp_admin.go).
// The latter can only be created inside this package, so a plain HTTP request
// cannot claim to be one.
func (s *Server) adminActor(w http.ResponseWriter, r *http.Request, requireAdmin bool) (*domain.AdminUser, bool) {
	if s.deps.Admin == nil || s.deps.AdminStore == nil {
		writeAPIError(w, domain.ErrUnsupported("the management API is disabled"))
		return nil, false
	}
	if actor, ok := mcpActorFrom(r.Context()); ok {
		if requireAdmin && !admin.RequireRole(&domain.AdminUser{Role: actor.Role}, "admin") {
			writeAPIError(w, domain.ErrForbidden("this MCP token may only call read-only management endpoints"))
			return nil, false
		}
		return &domain.AdminUser{Username: actor.Username, Role: actor.Role}, true
	}
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		writeAPIError(w, domain.ErrUnauthorized("missing admin session"))
		return nil, false
	}
	id, token, ok := admin.ParseCookie(cookie.Value)
	if !ok {
		writeAPIError(w, domain.ErrUnauthorized("malformed admin session"))
		return nil, false
	}
	user, err := s.deps.Admin.Authenticate(r.Context(), id, token)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return nil, false
	}
	if requireAdmin && !admin.RequireRole(user, "admin") {
		writeAPIError(w, domain.ErrForbidden("administrator role required"))
		return nil, false
	}
	return user, true
}

func (s *Server) audit(ctx context.Context, actor, action, targetType, targetID string, changes any, result string) {
	if s.deps.AdminStore == nil {
		return
	}
	raw, _ := json.Marshal(changes)
	entry := &AuditEntry{
		Actor: actor, Action: action, TargetType: targetType, TargetID: targetID,
		ChangesJSON: string(raw), Result: result, CreatedAt: time.Now().UTC(),
	}
	if err := s.deps.AdminStore.InsertAudit(ctx, entry); err != nil {
		s.deps.Log.Warn("writing audit entry failed", "err", err, "action", action)
	}
}

// reload refreshes caches and the routing snapshot after a write.
func (s *Server) reload(ctx context.Context, reason string, invalidateAll bool) {
	if s.deps.InvalidateKey != nil && !invalidateAll {
		s.deps.InvalidateKey("")
	}
	if s.deps.InvalidateAll != nil {
		s.deps.InvalidateAll()
	}
	if s.deps.Reload != nil {
		if _, err := s.deps.Reload(ctx); err != nil {
			s.deps.Log.Error("registry reload after admin write failed", "err", err, "reason", reason)
		}
	}
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeAPIError(w, domain.ErrUnsupported("the management API is disabled"))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	clientKey := clientIP(r) + "|" + strings.ToLower(body.Username)
	session, err := s.deps.Admin.Login(r.Context(), body.Username, body.Password, clientKey)
	if err != nil {
		s.audit(r.Context(), body.Username, "login", "admin_user", body.Username, nil, "failed")
		writeAPIError(w, toAPIError(err))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: admin.CookieValue(session),
		Path: "/admin", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: session.ExpiresAt, MaxAge: int(time.Until(session.ExpiresAt).Seconds()),
	})
	s.audit(r.Context(), session.User.Username, "login", "admin_user", session.User.Username, nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"username": session.User.Username, "role": session.User.Role,
		"expires_at": session.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(adminCookieName)
	if err == nil && cookie.Value != "" {
		if id, _, ok := admin.ParseCookie(cookie.Value); ok && s.deps.Admin != nil {
			_ = s.deps.Admin.Logout(r.Context(), id)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.adminActor(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": user.Username, "role": user.Role})
}

// ---------------------------------------------------------------------------
// api keys
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	accountID := int64(0)
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			accountID = parsed
		}
	}
	keys, err := s.deps.AdminStore.ListAPIKeys(r.Context(), accountID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{
			"id": key.ID, "name": key.Name, "account_id": key.AccountID,
			"key_prefix": key.KeyPrefix, "status": key.Status,
			"tags":               jsonOrEmptyArray(key.TagsJSON),
			"policy":             jsonOrNil(key.PolicyJSON),
			"record_input_mode":  key.RecordInputMode,
			"record_reasoning":   key.RecordReasoning,
			"record_output_text": key.RecordOutputText,
			"last_used_at":       timeOrNil(key.LastUsedAt),
		})
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Name      string          `json:"name"`
		AccountID int64           `json:"account_id"`
		Account   string          `json:"account"`
		Tags      []string        `json:"tags"`
		Grants    any             `json:"grants"`
		Policy    json.RawMessage `json:"policy"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	policy, apiErr := keyPolicyDocument(body.Policy)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	accountID := body.AccountID
	if accountID == 0 && body.Account != "" {
		account, err := s.deps.AdminStore.GetAccountByName(r.Context(), body.Account)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		accountID = account.ID
	}
	if accountID == 0 || body.Name == "" {
		writeAPIError(w, domain.ErrInvalidRequest("name and account are required"))
		return
	}

	token := ids.APIKey()
	key := &domain.APIKey{
		AccountID:       accountID,
		Name:            body.Name,
		KeyPrefix:       secret.Prefix(token),
		KeyHash:         secret.Hash(token),
		TagsJSON:        marshalOrEmpty(body.Tags),
		GrantsJSON:      marshalAny(body.Grants),
		PolicyJSON:      policy,
		RecordInputMode: "inherit",
		Status:          "active",
		CreatedBy:       actor.Username,
	}
	id, err := s.deps.AdminStore.UpsertAPIKey(r.Context(), key)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "create", "api_key", strconv.FormatInt(id, 10),
		map[string]any{"name": body.Name, "account_id": accountID}, "ok")
	s.reload(r.Context(), "api key created", false)

	// The plaintext token is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": body.Name, "key": token, "key_prefix": key.KeyPrefix,
		"note": "store this key now: it cannot be retrieved again",
	})
}

func (s *Server) handleAdminPatchKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid key id"))
		return
	}
	var body struct {
		Status           *string          `json:"status"`
		RecordReasoning  *bool            `json:"record_reasoning"`
		RecordOutputText *bool            `json:"record_output_text"`
		RecordInputMode  *string          `json:"record_input_mode"`
		Policy           *json.RawMessage `json:"policy"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.RecordInputMode != nil {
		if apiErr := validRecordInputMode(*body.RecordInputMode); apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
	}
	policy := ""
	if body.Policy != nil {
		raw, apiErr := keyPolicyDocument(*body.Policy)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		policy = raw
	}

	keys, err := s.deps.AdminStore.ListAPIKeys(r.Context(), 0)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var target *domain.APIKey
	for _, key := range keys {
		if key.ID == id {
			target = key
			break
		}
	}
	if target == nil {
		writeAPIError(w, domain.ErrNotFound(fmt.Sprintf("api key %d", id)))
		return
	}

	recOutput := target.RecordOutputText
	recReasoning := target.RecordReasoning
	inputMode := target.RecordInputMode
	status := target.Status
	if body.RecordOutputText != nil {
		recOutput = *body.RecordOutputText
	}
	if body.RecordReasoning != nil {
		recReasoning = *body.RecordReasoning
	}
	if body.RecordInputMode != nil {
		inputMode = *body.RecordInputMode
	}
	if body.Status != nil {
		status = *body.Status
	}

	// One write, not two: the upsert below rewrites every column from this struct, so
	// anything set on the row but not on the struct is reverted. That is exactly how the
	// recording switches used to be saved and then silently overwritten with the values
	// the row already had — the PATCH looked successful and changed nothing.
	if inputMode == "" {
		inputMode = "inherit"
	}
	target.RecordInputMode = inputMode
	target.RecordOutputText = recOutput
	target.RecordReasoning = recReasoning
	target.Status = status
	// A quota policy is only settable at creation otherwise, which left an operator with
	// no way to fix a limit on a key that already had traffic.
	if body.Policy != nil {
		target.PolicyJSON = policy
	}
	if _, err := s.deps.AdminStore.UpsertAPIKey(r.Context(), target); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	s.audit(r.Context(), actor.Username, "update", "api_key", strconv.FormatInt(id, 10), map[string]any{
		"record_output_text": recOutput, "record_reasoning": recReasoning,
		"record_input_mode": inputMode, "status": status,
		// The policy itself is not secret, but the audit trail records that it changed
		// rather than duplicating configuration into a second table.
		"policy_set": body.Policy != nil,
	}, "ok")
	if s.deps.InvalidateKey != nil {
		s.deps.InvalidateKey(target.KeyPrefix)
	}
	if recOutput != recReasoning {
		s.deps.Log.Info("recording policy changed",
			"key", target.Name, "output_text", recOutput, "reasoning", recReasoning)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": status, "record_output_text": recOutput,
		"record_reasoning": recReasoning, "record_input_mode": inputMode,
		"policy": jsonOrNil(target.PolicyJSON),
	})
}

// validRecordInputMode rejects a per-key recording mode the gateway cannot resolve, so a
// console that sends a stale value fails loudly instead of silently storing it.
func validRecordInputMode(mode string) *domain.APIError {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "inherit" {
		return nil
	}
	for _, known := range config.RecordingInputModes {
		if normalized == known {
			return nil
		}
	}
	return domain.ErrInvalidRequest(fmt.Sprintf("record_input_mode %q is not supported (use inherit, %s)",
		mode, strings.Join(config.RecordingInputModes, ", "))).WithParam("record_input_mode")
}

// keyPolicyDocument validates an optional key/tag policy document: it must be a JSON
// object, and every top-level field must be one the gateway reads. A stored-but-ignored
// policy is worse than a rejected one — the console used to advertise a nested shape
// ({"rate_limit":{"rpm":60}}) that nothing enforced, so a limit could look configured
// while the traffic kept flowing.
func keyPolicyDocument(raw json.RawMessage) (string, *domain.APIError) {
	text, err := jsonObjectString(raw, "policy")
	if err != nil {
		return "", toAPIError(err)
	}
	if text == "" {
		return "", nil
	}
	if _, unknown, parseErr := domain.ParsePolicy(text); parseErr != nil {
		return "", domain.ErrInvalidRequest(parseErr.Error()).WithParam("policy")
	} else if len(unknown) > 0 {
		return "", domain.ErrInvalidRequest(fmt.Sprintf(
			"policy has fields the gateway does not read: %s (accepted: %s)",
			strings.Join(unknown, ", "), domain.PolicyFieldList())).WithParam("policy")
	}
	return text, nil
}

// ---------------------------------------------------------------------------
// queries
// ---------------------------------------------------------------------------

func (s *Server) handleAdminRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	accountID := int64(0)
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			accountID = parsed
		}
	}
	page, err := pageRequests.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	from, to := adminWindow(r)
	rows, err := s.deps.AdminStore.ListRequestLogsPage(r.Context(), accountID, from, to, page.Limit, page.Offset)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	total, err := s.deps.AdminStore.CountRequestLogs(r.Context(), accountID, from, to)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"request_id": row.RequestID, "account_id": row.AccountID, "api_key_id": row.APIKeyID,
			"endpoint": row.Endpoint, "status": row.Status,
			"created_at":     row.CreatedAt.Format(time.RFC3339),
			"input_recorded": row.RequestJSON != "", "reasoning_recorded": row.ReasoningRecorded,
			"output_text_recorded": row.OutputTextRecorded, "truncated": row.Truncated,
			"request_bytes": row.RequestBytes,
		})
	}
	writeList(w, out, total, page)
}

func (s *Server) handleAdminRequestDetail(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	row, err := s.deps.AdminStore.GetRequestLog(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": row.RequestID, "account_id": row.AccountID, "api_key_id": row.APIKeyID,
		"endpoint": row.Endpoint, "status": row.Status,
		"created_at":     row.CreatedAt.Format(time.RFC3339),
		"input":          jsonOrNil(row.RequestJSON),
		"reasoning":      jsonOrNil(row.ResponseReasoning),
		"output":         jsonOrNil(row.ResponseText),
		"input_recorded": row.RequestJSON != "", "reasoning_recorded": row.ReasoningRecorded,
		"output_text_recorded": row.OutputTextRecorded, "truncated": row.Truncated,
		"request_bytes": row.RequestBytes, "response_bytes": row.ResponseBytes,
	})
}

func (s *Server) handleAdminAuditLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	page, err := pageAudit.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	rows, err := s.deps.AdminStore.ListAuditPage(r.Context(), page.Limit, page.Offset)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	total, err := s.deps.AdminStore.CountAudit(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id": row.ID, "actor": row.Actor, "action": row.Action,
			"target_type": row.TargetType, "target_id": row.TargetID,
			"changes": jsonOrNil(row.ChangesJSON), "result": row.Result,
			"created_at": row.CreatedAt.Format(time.RFC3339),
		})
	}
	writeList(w, out, total, page)
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	snap := s.deps.Registry.Snapshot()
	now := time.Now().UTC()
	payload := map[string]any{
		"registry": map[string]any{
			"models": len(snap.Models), "providers": len(snap.Providers),
			"provider_models": len(snap.ProviderModels), "routes": len(snap.Routes),
			"mappings": len(snap.Mappings), "tags": len(snap.Tags), "accounts": len(snap.Accounts),
			"ready": snap.Ready(),
		},
		"balancer":    s.deps.Router.Balancer().SnapshotMetrics(),
		"cooldowns":   s.deps.Router.Balancer().Cooldowns(now),
		"request_log": s.requestLogStats(),
		"version":     s.deps.Version,
	}
	if s.deps.KeyCacheSize != nil {
		payload["key_cache_entries"] = s.deps.KeyCacheSize()
	}
	writeJSON(w, http.StatusOK, payload)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("failed to read the request body")
	}
	if len(body) == 0 {
		return fmt.Errorf("request body is empty")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("malformed JSON body")
	}
	return nil
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx > 0 {
			return strings.TrimSpace(forwarded[:idx])
		}
		return strings.TrimSpace(forwarded)
	}
	host := r.RemoteAddr
	if idx := strings.LastIndexByte(host, ':'); idx > 0 {
		host = host[:idx]
	}
	return host
}

func adminLimit(r *http.Request, def, max int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return def
	}
	if parsed > max {
		return max
	}
	return parsed
}

func adminWindow(r *http.Request) (time.Time, time.Time) {
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -7)
	if raw := r.URL.Query().Get("days"); raw != "" {
		if days, err := strconv.Atoi(raw); err == nil && days > 0 && days <= 365 {
			from = now.AddDate(0, 0, -days)
		}
	}
	return from, now
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func jsonOrEmptyArray(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return []any{}
	}
	return jsonOrNil(raw)
}

// jsonOrNil embeds a stored payload in a response: JSON documents are handed over as
// JSON so the console can render their structure, and plain text is handed over as a
// string.
//
// The distinction is not cosmetic. A recorded column holds either a JSON document
// (request_json, output_json) or plain prose (response_text, response_reasoning — the
// model's own words, appended as text), and encoding/json *fails* on a json.RawMessage
// that is not valid JSON. That failure happens inside the encoder, after the status line
// has already gone out, so the answer was a 200 with a completely empty body — which the
// console's api.get() parses to null and then dereferences. Recording output text on a
// Key was therefore enough to make every one of its request details unopenable.
func jsonOrNil(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	// Text/plain columns and any payload truncated mid-escape: a JSON string always
	// encodes, and the console shows it verbatim (as it does for a plain string today).
	return raw
}

func marshalOrEmpty(v any) string {
	raw, err := json.Marshal(v)
	if err != nil || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

func marshalAny(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

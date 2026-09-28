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

	"github.com/funnywwh/ai-gateway/internal/admin"
	"github.com/funnywwh/ai-gateway/internal/config"
	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/ids"
	"github.com/funnywwh/ai-gateway/internal/secret"
	"github.com/funnywwh/ai-gateway/internal/store"
)

// AdminStore is the persistence subset the management API needs.
type AdminStore interface {
	ListAccounts(ctx context.Context) ([]*domain.Account, error)
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	GetAccountByName(ctx context.Context, name string) (*domain.Account, error)
	ListAPIKeys(ctx context.Context, accountID int64) ([]*domain.APIKey, error)
	// The Feishu identity of a key has its own read/write path (M60): a binding must be
	// written by one statement, never as a side effect of rewriting a whole row, and it is
	// resolved the other way round when someone signs in to the DSH portal.
	GetAPIKeyByID(ctx context.Context, id int64) (*domain.APIKey, error)
	FindAPIKeyByFeishuOpenID(ctx context.Context, openID string) (*domain.APIKey, error)
	BindAPIKeyFeishu(ctx context.Context, id int64, binding domain.FeishuBinding) error
	UnbindAPIKeyFeishu(ctx context.Context, id int64) (bool, error)
	// ListAPIKeyFeishuIdentities is the key-level read the M72 startup backfill uses to move
	// pre-M72 bindings onto their accounts.
	ListAPIKeyFeishuIdentities(ctx context.Context) ([]domain.KeyFeishuIdentity, error)
	// FindAccountByFeishuOpenID is how the DSH portal login resolves an identity from M72 on:
	// the binding belongs to the account. The key-level read above stays as the fallback for a
	// deployment whose backfill has not run yet.
	FindAccountByFeishuOpenID(ctx context.Context, openID string) (*domain.Account, error)
	// GetAdminUserByUsername re-reads the operator who started a Feishu binding, so a
	// demotion between starting and finishing it is honoured.
	GetAdminUserByUsername(ctx context.Context, username string) (*domain.AdminUser, error)
	// The administration of administrators (M66): the console keeps them in admin_users,
	// each with a role, a lifecycle state and at most one Feishu identity. The write paths
	// are column-scoped store methods rather than an upsert, because a role change and a
	// password reset must not be able to undo each other.
	ListAdminUsers(ctx context.Context) ([]*domain.AdminUser, error)
	GetAdminUser(ctx context.Context, id int64) (*domain.AdminUser, error)
	CreateAdminUser(ctx context.Context, u *domain.AdminUser) (int64, error)
	UpdateAdminUserRole(ctx context.Context, id int64, role string) error
	SetAdminUserStatus(ctx context.Context, id int64, status string) error
	SetAdminUserPassword(ctx context.Context, id int64, hash string) error
	RotateAdminUserInvite(ctx context.Context, id int64, nonce string) error
	DeleteAdminUser(ctx context.Context, id int64) error
	DeleteAdminSessions(ctx context.Context, userID int64) error
	// ActiveAdminCount is what keeps the console from removing the last administrator who
	// could undo the removal.
	ActiveAdminCount(ctx context.Context) (int, error)
	FindAdminUserByFeishuOpenID(ctx context.Context, openID string) (*domain.AdminUser, error)
	BindAdminUserFeishu(ctx context.Context, id int64, binding domain.FeishuBinding) error
	UnbindAdminUserFeishu(ctx context.Context, id int64) (bool, error)
	// FindAPIKeyByHash reports a missing row as (nil, nil): the key importer must tell "new"
	// from "already here" without the data plane's "unknown hash is a 401" rule. Since M87 the
	// hash — not the display prefix — is what "already here" means.
	FindAPIKeyByHash(ctx context.Context, hash string) (*domain.APIKey, error)
	// FindAPIKeyByPrefix reports any key wearing this display prefix, or (nil, nil) when none
	// does. The mint path uses it to keep labels distinct, which stopped being a correctness
	// requirement in M87 (the prefix is a label, not an identity).
	FindAPIKeyByPrefix(ctx context.Context, prefix string) (*domain.APIKey, error)
	// ListAPIKeysByPrefix returns every key wearing a prefix, because a shared prefix is now a
	// legitimate state: the lookup endpoint answers with a list instead of pretending there is
	// exactly one match.
	ListAPIKeysByPrefix(ctx context.Context, prefix string) ([]*domain.APIKey, error)
	UpsertAPIKey(ctx context.Context, k *domain.APIKey) (int64, error)
	// UpsertAPIKeys writes a whole batch in one transaction (M80). The batch import validates
	// every item first, so an error here means the database refused a row the checker
	// accepted; a half-applied batch would leave the operator guessing which keys are live.
	UpsertAPIKeys(ctx context.Context, keys []*domain.APIKey) ([]int64, error)
	ListRequestLogs(ctx context.Context, f domain.RequestLogFilter, limit int) ([]*domain.RequestLogRecord, error)
	ListRequestLogsPage(ctx context.Context, f domain.RequestLogFilter, limit, offset int) ([]*domain.RequestLogRecord, error)
	CountRequestLogs(ctx context.Context, f domain.RequestLogFilter) (int, error)
	GetRequestLog(ctx context.Context, requestID string) (*domain.RequestLogRecord, error)
	RequestUsages(ctx context.Context, requestIDs []string) (map[string]*domain.RequestUsage, error)
	// RequestAttempts lists every metered upstream attempt of each request, which is what
	// makes "which route did this request take" answerable: like the money, it is
	// one-to-many and owned by the metering table (a request that failed over has several),
	// so it cannot ride along on the log row. The providers that served a request are this
	// result deduplicated, in the order they were first tried.
	RequestAttempts(ctx context.Context, requestIDs []string) (map[string][]domain.RequestAttempt, error)
	// The two credential dimensions (M30) and the provider dimension (M53) read their labels
	// from the tables that own them; the log row keeps only the ids.
	AccountNames(ctx context.Context, ids []int64) (map[int64]string, error)
	APIKeyLabels(ctx context.Context, ids []int64) (map[int64]domain.APIKeyLabel, error)
	ProviderNames(ctx context.Context, ids []int64) (map[int64]string, error)
	// The dimension breakdown is read as a page of buckets plus the number of buckets the
	// filters matched, so the console's pager can say "共 N 个分组 · 第 x/y 页".
	RequestLogDimensionsPage(ctx context.Context, f domain.RequestLogFilter, groupBy, sort string, limit, offset int) (domain.RequestLogDimensionPage, error)
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

// reloadModel is the model-write variant of reload. Unlike the historical
// best-effort helper, it reports a registry failure to its caller: the database
// row is durable, but an unchanged registry means the requested model policy is
// not active yet. Key-cache invalidation remains best effort and mirrors reload.
func (s *Server) reloadModel(ctx context.Context, reason string) error {
	if s.deps.InvalidateAll != nil {
		s.deps.InvalidateAll()
	}
	if s.deps.Reload == nil {
		return nil
	}
	if _, err := s.deps.Reload(ctx); err != nil {
		s.deps.Log.Error("registry reload after model admin write failed", "err", err, "reason", reason)
		return err
	}
	return nil
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
	s.setAdminCookie(w, session)
	s.audit(r.Context(), session.User.Username, "login", "admin_user", session.User.Username,
		map[string]any{"method": "password"}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"username": session.User.Username, "role": session.User.Role,
		"expires_at": session.ExpiresAt.Format(time.RFC3339),
	})
}

// setAdminCookie hands the browser an administrator session. Both ways in — the password
// form and a verified Feishu identity — write the cookie through here, so the scope and the
// flags cannot drift apart: Path keeps it on the console's own subtree, HttpOnly keeps page
// script out of it, and SameSite=Lax is what lets it arrive on the redirect back from
// Feishu's consent page.
func (s *Server) setAdminCookie(w http.ResponseWriter, session *admin.Session) {
	if session == nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookieName, Value: admin.CookieValue(session),
		Path: s.url("/admin"), HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: session.ExpiresAt, MaxAge: int(time.Until(session.ExpiresAt).Seconds()),
	})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(adminCookieName)
	if err == nil && cookie.Value != "" {
		if id, _, ok := admin.ParseCookie(cookie.Value); ok && s.deps.Admin != nil {
			_ = s.deps.Admin.Logout(r.Context(), id)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: "", Path: s.url("/admin"), MaxAge: -1, HttpOnly: true})
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
		accountTags, effectiveTags := s.keyTagFields(key)
		out = append(out, map[string]any{
			"id": key.ID, "name": key.Name, "account_id": key.AccountID,
			"key_prefix": key.KeyPrefix, "status": key.Status,
			"tags":               jsonOrEmptyArray(key.TagsJSON),
			"account_tags":       accountTags,
			"effective_tags":     effectiveTags,
			"policy":             jsonOrNil(key.PolicyJSON),
			"record_input_mode":  key.RecordInputMode,
			"record_reasoning":   key.RecordReasoning,
			"record_output_text": key.RecordOutputText,
			"last_used_at":       timeOrNil(key.LastUsedAt),
			"feishu":             feishuBindingJSON(key),
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

// mintPrefixAttempts caps how many times a mint path retries when the freshly generated token's
// display prefix is already in use.
const mintPrefixAttempts = 3

// mintTokenWithFreePrefix generates a token and hands it back once no other row wears its
// 12-character display prefix.
//
// Since M87 the prefix is only a label — two keys may share one and each still authenticates by
// its own hash — so this is tidiness, not correctness. It is kept because (a) the console and
// support read prefixes as "which key is this", and (b) dshgw's legacy prefix→tenant fallback
// still resolves by prefix, so handing out distinct labels keeps that path unambiguous. mint()
// runs afresh on every attempt, so a collision simply yields another token; three collisions in
// a row is a ~30-bit coincidence three times over, and is reported instead of retried forever.
func mintTokenWithFreePrefix(mint func() string, taken func(prefix string) (bool, error)) (string, error) {
	var last string
	for attempt := 0; attempt < mintPrefixAttempts; attempt++ {
		token := mint()
		prefix := secret.Prefix(token)
		inUse, err := taken(prefix)
		if err != nil {
			return "", err
		}
		if !inUse {
			return token, nil
		}
		last = prefix
	}
	return "", fmt.Errorf("no free key prefix after %d attempts (last collision: %s)", mintPrefixAttempts, last)
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

	token, err := mintTokenWithFreePrefix(ids.APIKey, func(prefix string) (bool, error) {
		row, err := s.deps.AdminStore.FindAPIKeyByPrefix(r.Context(), prefix)
		return row != nil, err
	})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
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
		map[string]any{"name": body.Name, "account_id": accountID, "tags_set": body.Tags != nil}, "ok")
	s.reload(r.Context(), "api key created", false)

	// The plaintext token is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": body.Name, "key": token, "key_prefix": key.KeyPrefix,
		"note": "store this key now: it cannot be retrieved again",
	})
}

// importedKeyPrefix marks the keys that entered through an import path, so the console and the
// audit trail can tell "an operator registered this credential" from "the console minted it".
//
// It used to double as an ownership rule ("a re-import may only take over a row it owns"), which
// existed solely because the display prefix was the table's lookup key. Since M87 a row is
// identified by its hash, so the marker is exactly what it looks like: provenance.
const importedKeyPrefix = "import:"

// handleAdminImportKey registers a key whose plaintext lives somewhere else: the caller sends
// the SHA-256 of the secret (and optionally a display label), never the secret itself.
//
// This is the migration path for keys that already work against another gateway. The console's
// create endpoint mints a fresh token and shows it once; an import has nothing to show, because
// the gateway never learns the plaintext. Two consequences are worth stating plainly: a key
// registered this way can only be revoked by status (its plaintext cannot be re-displayed to
// anyone), and the optional label is exactly that — a label, not an identity (M87).
func (s *Server) handleAdminImportKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	tags, ok := portReady(w, s.deps.Tags, "tag management")
	if !ok {
		return
	}
	var body struct {
		Name      string          `json:"name"`
		AccountID int64           `json:"account_id"`
		Account   string          `json:"account"`
		KeyPrefix string          `json:"key_prefix"`
		KeyHash   string          `json:"key_hash"`
		Tags      []string        `json:"tags"`
		Grants    any             `json:"grants"`
		Policy    json.RawMessage `json:"policy"`
		Status    string          `json:"status"`
		ExpiresAt string          `json:"expires_at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	label, hash, apiErr := importedCredential(body.KeyPrefix, body.KeyHash)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeAPIError(w, domain.ErrInvalidRequest("name is required").WithParam("name"))
		return
	}
	status := strings.TrimSpace(body.Status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		writeAPIError(w, domain.ErrInvalidRequest("status must be active or disabled").WithParam("status"))
		return
	}
	var expiresAt *time.Time
	if raw := strings.TrimSpace(body.ExpiresAt); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("expires_at must be RFC3339").WithParam("expires_at"))
			return
		}
		expiresAt = &parsed
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
	if accountID == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("account_id or account is required"))
		return
	}
	// An id that names no account must fail here rather than at insert time: without this
	// check the foreign key surfaces as a 500, which reads like a gateway fault instead of
	// "you pointed the import at the wrong account".
	if _, err := s.deps.AdminStore.GetAccount(r.Context(), accountID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// An unknown tag name is not a cosmetic typo: tag resolution drops names it cannot
	// find, and a key left without any grant falls back to the default grant (every
	// provider). The import therefore refuses names that do not exist instead of storing
	// a key whose authorization silently widens. The console's create endpoint predates
	// this check and still accepts them.
	known, err := tags.ListTags(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if apiErr := unknownTagName(body.Tags, known); apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	policy, apiErr := keyPolicyDocument(body.Policy)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	// One row per secret (M87): the hash is the identity. The same hash again is an idempotent
	// update — it may correct the label, the tags or the account — while a *different* hash is a
	// new row even when somebody else's key already wears that label. That is the point of the
	// milestone: sub2api-style sources let two people hold keys sharing their first 12
	// characters, and a migration must not force one of them to change keys.
	existing, err := s.deps.AdminStore.FindAPIKeyByHash(r.Context(), hash)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	created := existing == nil

	key := &domain.APIKey{
		AccountID:       accountID,
		Name:            strings.TrimSpace(body.Name),
		KeyPrefix:       label,
		KeyHash:         hash,
		TagsJSON:        marshalOrEmpty(body.Tags),
		GrantsJSON:      marshalAny(body.Grants),
		PolicyJSON:      policy,
		RecordInputMode: "inherit",
		Status:          status,
		ExpiresAt:       expiresAt,
		CreatedBy:       importedKeyPrefix + actor.Username,
	}
	id, err := s.deps.AdminStore.UpsertAPIKey(r.Context(), key)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The hash stays out of the audit trail on purpose: an operator reading the log needs
	// to know that a key was imported and which label it took, and nothing more.
	s.audit(r.Context(), actor.Username, "import", "api_key", strconv.FormatInt(id, 10),
		map[string]any{"name": key.Name, "account_id": accountID, "key_prefix": label,
			"tags_set": body.Tags != nil, "status": status, "created": created}, "ok")
	s.reload(r.Context(), "api key imported", false)

	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": key.Name, "account_id": accountID, "key_prefix": label,
		"status": status, "tags": jsonOrEmptyArray(key.TagsJSON), "created": created,
		"note": "only the label and its hash were written: the gateway does not know the plaintext",
	})
}

// maxKeyLabelLen bounds the display label an import may attach to a key. It keeps the column and
// the console cell readable; since M87 it has no security meaning, because the label is no longer
// what a request is looked up by.
const maxKeyLabelLen = 64

// keyLabel validates the optional display label of an imported credential (M87).
//
// Before M87 this rule demanded exactly secret.PrefixLen characters, because the label *was* the
// data plane's lookup key: a shorter or padded one would have indexed a row no bearer token could
// ever match. The row is found by hash now, so an importer may attach any printable label (or
// none), and an empty label simply shows as blank in the console.
func keyLabel(label string) (string, *domain.APIError) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", nil
	}
	if len(label) > maxKeyLabelLen {
		return "", domain.ErrInvalidRequest(fmt.Sprintf(
			"key_prefix must be at most %d characters", maxKeyLabelLen)).WithParam("key_prefix")
	}
	if apiErr := requirePrintable(label, "key_prefix"); apiErr != nil {
		return "", apiErr
	}
	return label, nil
}

// importedCredential validates the two halves of an imported key: the hash is mandatory (it is
// the row's identity, and the verifier compares against it), the label is optional.
func importedCredential(label, hash string) (string, string, *domain.APIError) {
	label, apiErr := keyLabel(label)
	if apiErr != nil {
		return "", "", apiErr
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !isHexSHA256(hash) {
		return "", "", domain.ErrInvalidRequest(
			"key_hash must be the 64-character hex SHA-256 of the key").WithParam("key_hash")
	}
	return label, hash, nil
}

// isHexSHA256 reports whether the value is 64 lowercase hex characters, i.e. a SHA-256 digest in
// the one spelling the verifier produces.
func isHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range []byte(value) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// unknownTagName reports the first tag name that does not exist.
func unknownTagName(names []string, known []*domain.Tag) *domain.APIError {
	if len(names) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(known))
	for _, tag := range known {
		if tag != nil {
			have[tag.Name] = struct{}{}
		}
	}
	for _, name := range names {
		if _, ok := have[strings.TrimSpace(name)]; !ok {
			return domain.ErrInvalidRequest("unknown tag: " + name).WithParam("tags")
		}
	}
	return nil
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
		Tags             *[]string        `json:"tags"`
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
	if body.Tags != nil {
		target.TagsJSON = marshalOrEmpty(*body.Tags)
	}
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
		"tags_set": body.Tags != nil,
		// The policy itself is not secret, but the audit trail records that it changed
		// rather than duplicating configuration into a second table.
		"policy_set": body.Policy != nil,
	}, "ok")
	if s.deps.InvalidateKey != nil {
		// The cache is keyed by the token's hash (M87), and the row just written names it: the
		// next request with this key re-reads status/expiry instead of riding a cached verdict.
		s.deps.InvalidateKey(target.KeyHash)
	}
	if recOutput != recReasoning {
		s.deps.Log.Info("recording policy changed",
			"key", target.Name, "output_text", recOutput, "reasoning", recReasoning)
	}
	accountTags, effectiveTags := s.keyTagFields(target)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": status, "tags": jsonOrEmptyArray(target.TagsJSON),
		"account_tags": accountTags, "effective_tags": effectiveTags,
		"record_output_text": recOutput, "record_reasoning": recReasoning, "record_input_mode": inputMode,
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
	page, err := pageRequests.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	filter, err := requestLogFilterFromQuery(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	rows, err := s.deps.AdminStore.ListRequestLogsPage(r.Context(), filter, page.Limit, page.Offset)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	total, err := s.deps.AdminStore.CountRequestLogs(r.Context(), filter)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// One query for the whole page: the token and money columns live in usage_records, and
	// joining them into the page query would put a group-by in front of the ORDER BY the
	// console's paging depends on.
	usages, err := s.requestUsages(r, rows)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The same rule for the two credential dimensions: the row keeps the ids, and the
	// names come from one batched lookup per page (ownerLabels).
	accountIDs := make([]int64, 0, len(rows))
	keyIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		accountIDs = append(accountIDs, row.AccountID)
		keyIDs = append(keyIDs, row.APIKeyID)
	}
	accounts, keys, err := s.ownerLabels(r.Context(), accountIDs, keyIDs)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The attempts that served each row are a third batched read (one indexed query over the
	// page's request ids), then one lookup for the provider names.
	requestIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		requestIDs = append(requestIDs, row.RequestID)
	}
	attempts, err := s.deps.AdminStore.RequestAttempts(r.Context(), requestIDs)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	providerIDs := make([]int64, 0, len(rows))
	for _, list := range attempts {
		providerIDs = append(providerIDs, attemptProviderIDs(list)...)
	}
	providers, err := s.providerLabels(r.Context(), providerIDs)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		key := keys[row.APIKeyID]
		payload := map[string]any{
			"request_id": row.RequestID, "account_id": row.AccountID, "api_key_id": row.APIKeyID,
			"account_name": accounts[row.AccountID],
			"api_key_name": key.Name, "api_key_prefix": key.Prefix,
			"providers": providerPayloads(attemptProviderIDs(attempts[row.RequestID]), providers),
			"attempts":  attemptPayloads(attempts[row.RequestID], providers),
			"endpoint":  row.Endpoint, "status": row.Status,
			"created_at":     row.CreatedAt.Format(time.RFC3339),
			"input_recorded": row.RequestJSON != "", "reasoning_recorded": row.ReasoningRecorded,
			"output_text_recorded": row.OutputTextRecorded, "truncated": row.Truncated,
			"request_bytes": row.RequestBytes,
			"client":        row.Client, "model": row.Model, "resolved_model": row.ResolvedModel,
			"matched_rule":     row.MatchedRule,
			"reasoning_effort": row.ReasoningEffort,
			"workspace":        row.Workspace, "session_id": row.SessionID, "call_kind": row.CallKind,
			"title": row.Title,
		}
		// The usage object is always present: "no usage row" (a locally rejected
		// request) and "consumed nothing" are different statements, and a missing key
		// would collapse them into the same blank cell.
		payload["usage"] = usagePayload(usages[row.RequestID])
		out = append(out, payload)
	}
	writeList(w, out, total, page)
}

// requestLogFilterFromQuery reads the account/key, window and identity filters the console
// and MCP pass. Unknown or empty dimension values are simply "no filter"; a dimension
// filter is an exact match, which is what the indexed columns can serve.
//
// The two numeric filters are rejected when they are not numbers. Silently ignoring a
// malformed id answers "every account" to a question that asked for one, which is the
// failure mode /invoices?account_id= was already fixed for; a 400 names the parameter
// instead (docs/design/m30-request-log-owner-dimensions.md D8).
func requestLogFilterFromQuery(r *http.Request) (domain.RequestLogFilter, error) {
	query := r.URL.Query()
	filter := domain.RequestLogFilter{
		Client:        query.Get("client"),
		Model:         query.Get("model"),
		ResolvedModel: query.Get("resolved_model"),
		Workspace:     query.Get("workspace"),
		SessionID:     query.Get("session_id"),
		CallKind:      query.Get("call_kind"),
	}
	for _, id := range []struct {
		param  string
		target *int64
	}{
		{"account_id", &filter.AccountID},
		{"api_key_id", &filter.APIKeyID},
		// The provider filter is an id like the two credentials above, and it is rejected the
		// same way when it is not one: silently ignoring a malformed provider id answers "every
		// provider" to a question that asked for one provider's traffic.
		{"provider_id", &filter.ProviderID},
	} {
		raw := query.Get(id.param)
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return domain.RequestLogFilter{}, domain.ErrInvalidRequest(id.param + " must be an integer").WithParam(id.param)
		}
		*id.target = parsed
	}
	filter.From, filter.To = adminWindow(r)
	return filter, nil
}

// ownerLabels resolves the account and API-key labels of the rows (or dimension buckets)
// in hand, one batched point lookup per table. Both maps are keyed by id and simply lack
// an entry for an id that has no row: a request log keeps its id and the console renders
// the id, rather than blanking the cell and hiding that the row exists.
func (s *Server) ownerLabels(ctx context.Context, accountIDs, keyIDs []int64) (map[int64]string, map[int64]domain.APIKeyLabel, error) {
	accounts, err := s.deps.AdminStore.AccountNames(ctx, accountIDs)
	if err != nil {
		return nil, nil, err
	}
	keys, err := s.deps.AdminStore.APIKeyLabels(ctx, keyIDs)
	if err != nil {
		return nil, nil, err
	}
	return accounts, keys, nil
}

// providerLabels resolves provider ids to their names the same way, with the same rule for a
// missing row: an id whose provider was deleted keeps its id on screen.
func (s *Server) providerLabels(ctx context.Context, ids []int64) (map[int64]string, error) {
	return s.deps.AdminStore.ProviderNames(ctx, ids)
}

// attemptProviderIDs lists the providers that served a request, deduplicated and in the
// order they were first tried. A request that failed over has several, and each of them
// metered its own attempts — collapsing that to one value would state that the request had
// one provider when it did not.
func attemptProviderIDs(attempts []domain.RequestAttempt) []int64 {
	out := make([]int64, 0, len(attempts))
	seen := map[int64]bool{}
	for _, attempt := range attempts {
		if seen[attempt.ProviderID] {
			continue
		}
		seen[attempt.ProviderID] = true
		out = append(out, attempt.ProviderID)
	}
	return out
}

// providerPayloads renders those providers as {id, name} pairs. The order is the order of
// attemptProviderIDs (who was tried first), so the list and the route path read the same way.
func providerPayloads(ids []int64, names map[int64]string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "name": names[id]})
	}
	return out
}

// attemptPayloads renders the route path of one request: one object per metered upstream
// attempt, in the order they were tried. route_id and upstream_model are the snapshots the
// gateway recorded at the time (a route can be edited or deleted afterwards, and a request
// that failed over may have run on another route with another upstream model name), so the
// path stays a statement about what happened rather than about today's configuration.
func attemptPayloads(attempts []domain.RequestAttempt, names map[int64]string) []map[string]any {
	out := make([]map[string]any, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, map[string]any{
			"attempt_no": attempt.AttemptNo, "route_id": attempt.RouteID,
			"provider_id": attempt.ProviderID, "provider_name": names[attempt.ProviderID],
			"upstream_model": attempt.UpstreamModel,
			"status":         attempt.Status, "error_code": attempt.ErrorCode,
			"terminated_reason": attempt.TerminatedReason,
			"latency_ms":        attempt.LatencyMS, "ttft_ms": attempt.TTFTMS,
			"cost_micros": attempt.CostMicros, "charge_micros": attempt.ChargeMicros,
			"created_at": attempt.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}

// requestUsages loads the page's metered consumption in one query.
func (s *Server) requestUsages(r *http.Request, rows []*domain.RequestLogRecord) (map[string]*domain.RequestUsage, error) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RequestID)
	}
	return s.deps.AdminStore.RequestUsages(r.Context(), ids)
}

// usagePayload renders one request's consumption. metered=false is reported as such
// instead of as zero: a locally rejected request has no usage row on purpose, and showing
// it as "0 tokens" would read as "it consumed nothing".
func usagePayload(usage *domain.RequestUsage) map[string]any {
	if usage == nil {
		return map[string]any{"metered": false}
	}
	return map[string]any{
		"metered":          usage.Metered,
		"attempts":         usage.Attempts,
		"input_tokens":     usage.InputTokens,
		"cached_tokens":    usage.CachedTokens,
		"output_tokens":    usage.OutputTokens,
		"reasoning_tokens": usage.ReasoningTokens,
		"cost_micros":      usage.CostMicros,
		"charge_micros":    usage.ChargeMicros,
		"latency_ms":       usage.LatencyMS,
		"ttft_ms":          usage.TTFTMS,
	}
}

// validRequestLogDimension reports whether the store can group by this dimension. The
// check lives here so an unknown value is a 400 naming the accepted set, rather than a
// store error surfacing as a 500.
func validRequestLogDimension(groupBy string) bool {
	for _, name := range store.RequestLogDimensionNames {
		if name == groupBy {
			return true
		}
	}
	return false
}

// validRequestLogDimensionSort reports whether the store can order the breakdown by this
// key, for the same reason as validRequestLogDimension: "which bucket is busiest" and
// "which bucket was active last" are different questions about the same buckets, and a
// silently ignored sort answers neither.
func validRequestLogDimensionSort(sort string) bool {
	for _, name := range store.RequestLogDimensionSorts {
		if name == sort {
			return true
		}
	}
	return false
}

// handleAdminRequestDimensions groups recorded requests by one identity dimension and sums
// what they consumed. It is the "statistics" half of the request log: which client, model,
// workspace, session, account (user) or API key is producing the traffic and the spend.
//
// It answers with a page of buckets plus the number of buckets the filters matched, so the
// console can page it like every other list (M31). The envelope's identifiers are the ones
// M24 defined; the list itself stays under "rows", which is this endpoint's documented
// shape since M27 (the console and the MCP tool admin_request_dimensions both read it).
func (s *Server) handleAdminRequestDimensions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	groupBy := strings.TrimSpace(r.URL.Query().Get("group_by"))
	if groupBy == "" {
		groupBy = "client"
	}
	if !validRequestLogDimension(groupBy) {
		writeAPIError(w, toAPIError(domain.ErrInvalidRequest(
			"group_by must be one of "+strings.Join(store.RequestLogDimensionNames, ", ")).WithParam("group_by")))
		return
	}
	sortKey := strings.TrimSpace(r.URL.Query().Get("sort"))
	if sortKey == "" {
		sortKey = store.RequestLogDimensionDefaultSort
	}
	if !validRequestLogDimensionSort(sortKey) {
		writeAPIError(w, toAPIError(domain.ErrInvalidRequest(
			"sort must be one of "+strings.Join(store.RequestLogDimensionSorts, ", ")).WithParam("sort")))
		return
	}
	page, err := pageDimensions.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	filter, err := requestLogFilterFromQuery(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	result, err := s.deps.AdminStore.RequestLogDimensionsPage(r.Context(), filter, groupBy, sortKey, page.Limit, page.Offset)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	rows, total := result.Rows, result.Total
	// The credential groupings bucket on ids; their names are labels read from the tables
	// that own them, resolved for the buckets actually being returned (never for the whole
	// window — the aggregate query would have to join per row to do that). The provider
	// grouping follows the same rule, for the same reason: a provider is renamed in the
	// providers table, and a copied name would split one provider into two buckets.
	var accountIDs, keyIDs, providerIDs []int64
	switch groupBy {
	case "account":
		accountIDs = dimensionGroupIDs(rows)
	case "api_key":
		keyIDs = dimensionGroupIDs(rows)
	case "provider":
		providerIDs = dimensionGroupIDs(rows)
	}
	accounts, keys, err := s.ownerLabels(r.Context(), accountIDs, keyIDs)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	providers, err := s.providerLabels(r.Context(), providerIDs)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		key := row.Key
		// An id of 0 (or below) is the unknown bucket — a historical row, or a row written
		// without a credential. It is reported as an empty key, the same "（未知）" bucket
		// the other dimensions use, and its counts are kept.
		if id, err := strconv.ParseInt(key, 10, 64); err == nil && id <= 0 {
			key = ""
		}
		payload := map[string]any{
			"key": key, "requests": row.Requests, "metered": row.Metered,
			"first_seen":   row.FirstSeen.Format(time.RFC3339),
			"last_seen":    row.LastSeen.Format(time.RFC3339),
			"input_tokens": row.InputTokens, "output_tokens": row.OutputTokens,
			"cached_tokens":    row.CachedTokens,
			"reasoning_tokens": row.ReasoningTokens,
			"cost_micros":      row.CostMicros, "charge_micros": row.ChargeMicros,
		}
		// A session owns one title and one workspace; for the other groupings these say
		// nothing, so they are only reported where they mean something.
		switch groupBy {
		case "session":
			payload["title"] = row.Title
			payload["workspace"] = row.Workspace
		case "account":
			if id, err := strconv.ParseInt(row.Key, 10, 64); err == nil {
				payload["account_id"] = id
				payload["account_name"] = accounts[id]
			}
		case "api_key":
			if id, err := strconv.ParseInt(row.Key, 10, 64); err == nil {
				payload["api_key_id"] = id
				payload["api_key_name"] = keys[id].Name
				payload["api_key_prefix"] = keys[id].Prefix
			}
		case "provider":
			// A metered request whose provider the gateway could not name (or a request that
			// never reached an upstream) buckets under 0, which is reported as the empty key —
			// the same 「未知」 bucket the other dimensions use.
			if id, err := strconv.ParseInt(row.Key, 10, 64); err == nil {
				payload["provider_id"] = id
				payload["provider_name"] = providers[id]
			}
		}
		out = append(out, payload)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_by": groupBy, "sort": sortKey, "days": adminDays(r),
		"limit": page.Limit, "offset": page.Offset, "count": len(out),
		"total": total, "has_more": page.Offset+len(out) < total,
		"dimensions": store.RequestLogDimensionNames, "rows": out,
	})
}

// dimensionGroupIDs reads the numeric group keys of a credential grouping. The store
// returns them as text (SQLite has no other way to group a leftover-typed column), and a
// bucket whose key is not a number cannot be looked up by id.
func dimensionGroupIDs(rows []domain.RequestLogDimensionRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if id, err := strconv.ParseInt(row.Key, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
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
	usage, err := s.deps.AdminStore.RequestUsages(r.Context(), []string{row.RequestID})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	accounts, keys, err := s.ownerLabels(r.Context(), []int64{row.AccountID}, []int64{row.APIKeyID})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// A failed-over request is the case the detail page exists to explain, so the attempt
	// path that metered it is part of the answer here too.
	attempts, err := s.deps.AdminStore.RequestAttempts(r.Context(), []string{row.RequestID})
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	path := attempts[row.RequestID]
	providers, err := s.providerLabels(r.Context(), attemptProviderIDs(path))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	key := keys[row.APIKeyID]
	payload := map[string]any{
		"request_id": row.RequestID, "account_id": row.AccountID, "api_key_id": row.APIKeyID,
		"account_name": accounts[row.AccountID],
		"api_key_name": key.Name, "api_key_prefix": key.Prefix,
		"providers": providerPayloads(attemptProviderIDs(path), providers),
		"attempts":  attemptPayloads(path, providers),
		"endpoint":  row.Endpoint, "status": row.Status,
		"created_at":     row.CreatedAt.Format(time.RFC3339),
		"input":          jsonOrNil(row.RequestJSON),
		"reasoning":      jsonOrNil(row.ResponseReasoning),
		"output":         jsonOrNil(row.ResponseText),
		"input_recorded": row.RequestJSON != "", "reasoning_recorded": row.ReasoningRecorded,
		"output_text_recorded": row.OutputTextRecorded, "truncated": row.Truncated,
		// The recording policy this row was written under. Under the default "user" policy an
		// empty input can mean "there was nothing to keep" as well as "recording was off", so
		// the console needs the mode to say which — the row itself has no body to tell.
		"record_input_mode": row.RecordInputMode,
		"request_bytes":     row.RequestBytes, "response_bytes": row.ResponseBytes,
		"client": row.Client, "model": row.Model, "resolved_model": row.ResolvedModel,
		"matched_rule":     row.MatchedRule,
		"reasoning_effort": row.ReasoningEffort,
		"workspace":        row.Workspace, "session_id": row.SessionID, "call_kind": row.CallKind,
		"title": row.Title,
		"usage": usagePayload(usage[row.RequestID]),
	}
	writeJSON(w, http.StatusOK, payload)
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
		"affinity":    s.deps.Router.AffinityStats(),
		"request_log": s.requestLogStats(r.Context()),
		"version":     s.deps.Version,
	}
	// Provider concurrency and queueing (M44). Omitted when no dispatcher was wired, so the
	// block never claims "nobody is queueing" on no evidence.
	if capacity := s.capacityBlock(); capacity != nil {
		payload["provider_capacity"] = capacity
	}
	// Provider cost caps (M56): the reader's freshness first, then every capped provider's
	// reading. Omitted for the same reason as the capacity block above.
	if cost := s.costBlock(); cost != nil {
		payload["provider_cost"] = cost
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
	return now.AddDate(0, 0, -adminDays(r)), now
}

// adminDays is the effective window of adminWindow, in days, so a response can echo the
// window it actually used rather than the one that was asked for.
func adminDays(r *http.Request) int {
	if raw := r.URL.Query().Get("days"); raw != "" {
		if days, err := strconv.Atoi(raw); err == nil && days > 0 && days <= 365 {
			return days
		}
	}
	return 7
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

func (s *Server) keyTagFields(key *domain.APIKey) (any, []string) {
	accountTags := any([]any{})
	if key == nil {
		return accountTags, []string{}
	}
	effective := jsonStringArray(key.TagsJSON)
	if s.deps.Registry != nil {
		snap := s.deps.Registry.Snapshot()
		if snap != nil {
			if account := snap.AccountByID[key.AccountID]; account != nil {
				accountTags = jsonOrEmptyArray(account.TagsJSON)
			}
			if s.deps.Router != nil {
				effective = make([]string, 0)
				for _, tag := range s.deps.Router.ResolveTags(snap, key) {
					effective = append(effective, tag.Name)
				}
			}
		}
	}
	return accountTags, effective
}

func jsonStringArray(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []string{}
	}
	return values
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

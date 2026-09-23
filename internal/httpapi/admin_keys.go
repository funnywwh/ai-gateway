package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/secret"
)

// This file holds the two credential-ownership endpoints added in M80: a batch import that
// takes a whole list of keys (custom plaintext values or M43's prefix+hash pair) in one
// call, and a lookup that answers "whose key is this".
//
// They live next to the M43 single import on purpose — the conflict rules, the tag checks
// and the audit shape are deliberately the same, and only the two things M43 did not need
// are new: a caller-supplied plaintext value (which the gateway hashes itself) and a batch
// that must be all-or-nothing. See docs/design/m80-key-batch-import-and-lookup.md.

const (
	// maxBatchImportKeys caps one batch. 200 items are roughly 40 KB of JSON, well inside
	// decodeJSON's 1 MiB, and keep the response inside mcp.admin_max_response_bytes (256 KB
	// by default, so an agent reads per-item results instead of a truncated blob).
	maxBatchImportKeys = 200
	// minPlaintextKeyLen keeps the prefix column from holding the whole secret. key_prefix
	// stores the first secret.PrefixLen characters in clear, and secret.Prefix returns the
	// whole token for anything shorter — so a 12-character key would be written to the
	// database, and shown in the console's key list, in plaintext.
	minPlaintextKeyLen = secret.PrefixLen + 4
	// maxPlaintextKeyLen bounds a caller-supplied key. There is no real upper bound on a
	// bearer token, but an unbounded column is a way to fill a database through an API.
	maxPlaintextKeyLen = 512
)

// importBatchItem is one entry of a batch import body. The credential is given either as a
// plaintext api_key or as the key_prefix/key_hash pair; giving both, or neither, is a 400.
type importBatchItem struct {
	Name      string          `json:"name"`
	AccountID int64           `json:"account_id"`
	Account   string          `json:"account"`
	APIKey    string          `json:"api_key"`
	KeyPrefix string          `json:"key_prefix"`
	KeyHash   string          `json:"key_hash"`
	Tags      []string        `json:"tags"`
	Grants    any             `json:"grants"`
	Policy    json.RawMessage `json:"policy"`
	Status    string          `json:"status"`
	ExpiresAt string          `json:"expires_at"`
}

// handleAdminImportKeys implements POST /admin/api/v1/keys/import-batch (M80).
//
// The whole batch is validated before anything is written: a batch that fails validation
// leaves the table exactly as it was, and the error names the offending item
// (keys[i].<field>). That is the decision recorded in the design document — M43 refused a
// batch endpoint because "partial success" needs a state machine to describe, and this
// endpoint keeps that refusal while giving the caller one call and one error report.
func (s *Server) handleAdminImportKeys(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	tags, ok := portReady(w, s.deps.Tags, "tag management")
	if !ok {
		return
	}
	var body struct {
		Keys   []importBatchItem `json:"keys"`
		DryRun bool              `json:"dry_run"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if len(body.Keys) == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("keys must hold at least one item").WithParam("keys"))
		return
	}
	if len(body.Keys) > maxBatchImportKeys {
		writeAPIError(w, domain.ErrInvalidRequest(fmt.Sprintf(
			"keys holds %d items and the limit is %d per call",
			len(body.Keys), maxBatchImportKeys)).WithParam("keys"))
		return
	}
	known, err := tags.ListTags(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	keys := make([]*domain.APIKey, 0, len(body.Keys))
	created := make([]bool, 0, len(body.Keys))
	sources := make([]string, 0, len(body.Keys))
	seen := make(map[string]int, len(body.Keys))
	for i := range body.Keys {
		key, isNew, source, apiErr := s.prepareImportedKey(r.Context(), body.Keys[i], known, actor.Username)
		if apiErr != nil {
			writeAPIError(w, locateBatchError(apiErr, i))
			return
		}
		// A prefix repeated inside one batch is ambiguous by construction: the second write
		// would silently overwrite the first, so the batch is refused and both indexes named.
		if first, dup := seen[key.KeyPrefix]; dup {
			writeAPIError(w, batchItemError(i, fmt.Sprintf(
				"key prefix %s is already used by item %d in this batch", key.KeyPrefix, first)))
			return
		}
		seen[key.KeyPrefix] = i
		keys = append(keys, key)
		created = append(created, isNew)
		sources = append(sources, source)
	}

	if body.DryRun {
		writeJSON(w, http.StatusOK, batchImportPayload(true, keys, created, nil))
		return
	}
	ids, err := s.deps.AdminStore.UpsertAPIKeys(r.Context(), keys)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// One audit row per key, exactly like the single import: the trail has to answer "who
	// brought in this prefix, and when". Neither the plaintext nor the hash is stored.
	for i, key := range keys {
		s.audit(r.Context(), actor.Username, "import", "api_key", strconv.FormatInt(ids[i], 10),
			map[string]any{"name": key.Name, "account_id": key.AccountID, "key_prefix": key.KeyPrefix,
				"created": created[i], "batch": true, "credential": sources[i],
				"tags_set": body.Keys[i].Tags != nil, "status": key.Status}, "ok")
	}
	s.reload(r.Context(), "api keys imported (batch)", false)

	writeJSON(w, http.StatusOK, batchImportPayload(false, keys, created, ids))
}

// prepareImportedKey validates one batch item and turns it into the row to write. It returns
// whether the row is new (an existing prefix updated in place is not) and which credential
// form the caller used, which is what the audit trail records.
//
// Every check the single import performs is here, in the same order and with the same
// wording, so a key that one endpoint accepts is not rejected by the other for a reason the
// operator cannot see.
func (s *Server) prepareImportedKey(ctx context.Context, item importBatchItem, known []*domain.Tag, actor string) (*domain.APIKey, bool, string, *domain.APIError) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return nil, false, "", domain.ErrInvalidRequest("name is required").WithParam("name")
	}
	plain := strings.TrimSpace(item.APIKey) != ""
	hashed := strings.TrimSpace(item.KeyPrefix) != "" || strings.TrimSpace(item.KeyHash) != ""
	switch {
	case plain && hashed:
		return nil, false, "", domain.ErrInvalidRequest(
			"give either api_key or key_prefix+key_hash, not both").WithParam("api_key")
	case !plain && !hashed:
		return nil, false, "", domain.ErrInvalidRequest(
			"api_key or key_prefix+key_hash is required").WithParam("api_key")
	}
	source := "hash"
	var prefix, hash string
	var apiErr *domain.APIError
	if plain {
		source = "plaintext"
		prefix, hash, apiErr = plaintextCredential(item.APIKey)
	} else {
		prefix, hash, apiErr = importedCredential(item.KeyPrefix, item.KeyHash)
	}
	if apiErr != nil {
		return nil, false, "", apiErr
	}

	status := strings.TrimSpace(item.Status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		return nil, false, "", domain.ErrInvalidRequest("status must be active or disabled").WithParam("status")
	}
	var expiresAt *time.Time
	if raw := strings.TrimSpace(item.ExpiresAt); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, false, "", domain.ErrInvalidRequest("expires_at must be RFC3339").WithParam("expires_at")
		}
		expiresAt = &parsed
	}

	accountID := item.AccountID
	if accountID == 0 && strings.TrimSpace(item.Account) != "" {
		account, err := s.deps.AdminStore.GetAccountByName(ctx, strings.TrimSpace(item.Account))
		if err != nil {
			return nil, false, "", toAPIError(err)
		}
		accountID = account.ID
	}
	if accountID == 0 {
		return nil, false, "", domain.ErrInvalidRequest("account_id or account is required").WithParam("account")
	}
	// An id that names no account must fail here rather than at insert time: without this
	// check the foreign key surfaces as a 500, which reads like a gateway fault instead of
	// "you pointed the import at the wrong account".
	if _, err := s.deps.AdminStore.GetAccount(ctx, accountID); err != nil {
		return nil, false, "", toAPIError(err)
	}
	if apiErr := unknownTagName(item.Tags, known); apiErr != nil {
		return nil, false, "", apiErr
	}
	policy, apiErr := keyPolicyDocument(item.Policy)
	if apiErr != nil {
		return nil, false, "", apiErr
	}

	// The prefix is the table's lookup key, so an import may only take over a row it owns:
	// the same secret again (same hash) or a row that already carries the import marker.
	// Anything else would silently replace a key the console handed to somebody.
	existing, err := s.deps.AdminStore.FindAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		return nil, false, "", toAPIError(err)
	}
	created := existing == nil
	if existing != nil && !secret.Equal(existing.KeyHash, hash) &&
		!strings.HasPrefix(existing.CreatedBy, importedKeyPrefix) {
		return nil, false, "", domain.ErrConflict(fmt.Sprintf(
			"key prefix %s already belongs to key %q created by %q; disable or remove it first",
			prefix, existing.Name, existing.CreatedBy))
	}

	return &domain.APIKey{
		AccountID:       accountID,
		Name:            name,
		KeyPrefix:       prefix,
		KeyHash:         hash,
		TagsJSON:        marshalOrEmpty(item.Tags),
		GrantsJSON:      marshalAny(item.Grants),
		PolicyJSON:      policy,
		RecordInputMode: "inherit",
		Status:          status,
		ExpiresAt:       expiresAt,
		CreatedBy:       importedKeyPrefix + actor,
	}, created, source, nil
}

// batchImportPayload renders the batch response: per item the caller needs to reconcile
// (index, id, name, account, prefix, status, tags) and nothing that is secret. ids is nil
// for a dry run, where created rows do not exist yet.
func batchImportPayload(dryRun bool, keys []*domain.APIKey, created []bool, ids []int64) map[string]any {
	rows := make([]map[string]any, 0, len(keys))
	createdCount, updatedCount := 0, 0
	for i, key := range keys {
		if created[i] {
			createdCount++
		} else {
			updatedCount++
		}
		row := map[string]any{
			"index": i, "name": key.Name, "account_id": key.AccountID,
			"key_prefix": key.KeyPrefix, "status": key.Status,
			"created": created[i], "tags": jsonOrEmptyArray(key.TagsJSON),
		}
		if len(ids) > i {
			row["id"] = ids[i]
		}
		rows = append(rows, row)
	}
	note := "明文与哈希都不返回、不落库、不进审计；只有前缀与 SHA-256 写入"
	if dryRun {
		note = "dry_run：只做了校验与冲突判定，没有写库、没有写审计；created/updated 表示真实调用会发生什么"
	}
	return map[string]any{
		"dry_run": dryRun, "total": len(keys),
		"created": createdCount, "updated": updatedCount,
		"keys": rows, "note": note,
	}
}

// handleAdminLookupKey implements POST /admin/api/v1/keys/lookup (M80): given a key, or the
// 12-character prefix that indexes it, answer which account it belongs to and in what state.
//
// It is a read: the route requires no more than the viewer role, and it deliberately does
// not decide whether the key may be *used*. status and expires_at are reported verbatim —
// the data plane's verifier is the only place that turns them into an admission decision,
// and two implementations of that decision would eventually disagree in public.
func (s *Server) handleAdminLookupKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	var body struct {
		APIKey    string `json:"api_key"`
		KeyPrefix string `json:"key_prefix"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	plain := strings.TrimSpace(body.APIKey) != ""
	byPrefix := strings.TrimSpace(body.KeyPrefix) != ""
	switch {
	case plain && byPrefix:
		writeAPIError(w, domain.ErrInvalidRequest("give either api_key or key_prefix, not both").WithParam("api_key"))
		return
	case !plain && !byPrefix:
		writeAPIError(w, domain.ErrInvalidRequest("api_key or key_prefix is required").WithParam("api_key"))
		return
	}

	prefix, hash, matched := "", "", ""
	var apiErr *domain.APIError
	if plain {
		prefix, hash, apiErr = lookupPlaintext(body.APIKey)
		matched = "hash"
	} else {
		prefix, apiErr = importedPrefix(body.KeyPrefix)
		matched = "prefix"
	}
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	row, err := s.deps.AdminStore.FindAPIKeyByPrefix(r.Context(), prefix)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if row == nil {
		// A prefix nobody has ever stored. This is an answer, not an error, so an agent reads
		// one shape whether or not the key exists.
		writeJSON(w, http.StatusOK, map[string]any{
			"found": false, "reason": "unknown_prefix",
			"note": "没有任何 Key 用过这个前缀（明文分支给的哈希也没有可比较的行）",
		})
		return
	}
	if plain && !secret.Equal(row.KeyHash, hash) {
		// The prefix exists but the plaintext is not its owner. Naming the row here would turn
		// the endpoint into "learn any account from a prefix"; the prefix branch is the
		// supported way to ask that question, and the prefix is not a secret anyway.
		writeJSON(w, http.StatusOK, map[string]any{
			"found": false, "reason": "hash_mismatch", "matched": "prefix",
			"note": "该前缀已有行，但你给的明文与它的哈希不匹配；要按标识查询请改用 key_prefix",
		})
		return
	}

	accountTags, effectiveTags := s.keyTagFields(row)
	payload := map[string]any{
		"found": true, "matched": matched,
		"key": map[string]any{
			"id": row.ID, "name": row.Name, "account_id": row.AccountID,
			"key_prefix": row.KeyPrefix, "status": row.Status,
			"tags": jsonOrEmptyArray(row.TagsJSON), "account_tags": accountTags,
			"effective_tags": effectiveTags, "created_by": row.CreatedBy,
			"expires_at": timeOrNil(row.ExpiresAt), "last_used_at": timeOrNil(row.LastUsedAt),
			"created_at": row.CreatedAt.UTC().Format(time.RFC3339),
			"feishu":     feishuBindingJSON(row),
		},
		"note": "这只回答归属与状态，不做鉴权判定：status/expires_at 如实报告，能不能用由数据面 verifier 决定",
	}
	account, err := s.deps.AdminStore.GetAccount(r.Context(), row.AccountID)
	switch {
	case err == nil:
		payload["account"] = map[string]any{
			"id": account.ID, "name": account.Name, "status": account.Status,
			"tags":        jsonOrEmptyArray(account.TagsJSON),
			"dsh_enabled": account.DSHEnabled, "dsh_tenant": account.DshTenant,
			"feishu": accountFeishuJSON(account),
		}
	case domain.IsNotFound(err):
		// A key row whose account is gone is a real state a support call can land on; saying
		// "the account no longer exists" beats a 404 that hides the key's prefix.
		payload["account"] = nil
		payload["note"] = payload["note"].(string) + "；该 Key 指向的账户已不存在"
	default:
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// plaintextCredential derives the lookup prefix and the hash from a key the caller supplied.
//
// This is where M80 hands the gateway the plaintext — a deliberate reversal of the M43 rule,
// which exists because "import my own key" cannot mean "and also compute its SHA-256 for me"
// for a person at a console. The guards are what make the reversal survivable: the value
// must travel in an Authorization header (printable ASCII, no whitespace), must be longer
// than the prefix that is stored in clear, and must not be unbounded. The plaintext itself
// is never stored, returned or audited — only the two derived values are.
func plaintextCredential(raw string) (string, string, *domain.APIError) {
	key := secret.Normalize(raw)
	if key == "" {
		return "", "", domain.ErrInvalidRequest("api_key is empty").WithParam("api_key")
	}
	if len(key) < minPlaintextKeyLen {
		return "", "", domain.ErrInvalidRequest(fmt.Sprintf(
			"api_key must be at least %d characters: the first %d are stored in clear for lookup, "+
				"so a shorter key would put the whole secret into that column",
			minPlaintextKeyLen, secret.PrefixLen)).WithParam("api_key")
	}
	if len(key) > maxPlaintextKeyLen {
		return "", "", domain.ErrInvalidRequest(fmt.Sprintf(
			"api_key must be at most %d characters", maxPlaintextKeyLen)).WithParam("api_key")
	}
	if apiErr := requirePrintable(key, "api_key"); apiErr != nil {
		return "", "", apiErr
	}
	return secret.Prefix(key), secret.Hash(key), nil
}

// lookupPlaintext derives the same two values for the read path. It accepts a shorter value
// than the write path on purpose: a caller holding a malformed or truncated key must get
// "not ours" (found=false) rather than a 400 that reveals how the gateway stores keys.
func lookupPlaintext(raw string) (string, string, *domain.APIError) {
	key := secret.Normalize(raw)
	if key == "" {
		return "", "", domain.ErrInvalidRequest("api_key is empty").WithParam("api_key")
	}
	if len(key) > maxPlaintextKeyLen {
		return "", "", domain.ErrInvalidRequest(fmt.Sprintf(
			"api_key must be at most %d characters", maxPlaintextKeyLen)).WithParam("api_key")
	}
	if apiErr := requirePrintable(key, "api_key"); apiErr != nil {
		return "", "", apiErr
	}
	return secret.Prefix(key), secret.Hash(key), nil
}

// requirePrintable rejects anything that could not have travelled as a bearer token.
func requirePrintable(value, param string) *domain.APIError {
	for _, c := range []byte(value) {
		if c < 0x21 || c > 0x7e {
			return domain.ErrInvalidRequest(
				param + " must be printable ASCII without whitespace").WithParam(param)
		}
	}
	return nil
}

// batchItemError locates one item of a batch body, so a 200-item call reports the single
// item that needs fixing instead of "invalid request".
func batchItemError(index int, message string) *domain.APIError {
	return domain.ErrInvalidRequest(fmt.Sprintf("keys[%d]: %s", index, message)).
		WithParam(fmt.Sprintf("keys[%d]", index))
}

// locateBatchError re-points an error raised by an item validator (whose Param names the
// field, e.g. "api_key") at the batch item that carried it: keys[3].api_key.
func locateBatchError(apiErr *domain.APIError, index int) *domain.APIError {
	if apiErr == nil {
		return nil
	}
	apiErr.Message = fmt.Sprintf("keys[%d]: %s", index, apiErr.Message)
	if apiErr.Param == "" {
		apiErr.Param = fmt.Sprintf("keys[%d]", index)
	} else {
		apiErr.Param = fmt.Sprintf("keys[%d].%s", index, apiErr.Param)
	}
	return apiErr
}

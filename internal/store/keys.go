package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const apiKeyCols = `id, account_id, name, key_prefix, key_hash, tags_json, grants_json, policy_json,
	record_input_mode, record_output_text, record_reasoning, status, expires_at, last_used_at,
	created_by, created_at, feishu_open_id, feishu_union_id, feishu_name, feishu_bound_at,
	feishu_bound_by`

func scanAPIKey(row rowScanner) (*domain.APIKey, error) {
	var (
		k                         domain.APIKey
		recordOutput, recordThink int
		expiresAt, lastUsedAt     sql.NullInt64
		feishuBoundAt             sql.NullInt64
		createdAt                 int64
	)
	if err := row.Scan(&k.ID, &k.AccountID, &k.Name, &k.KeyPrefix, &k.KeyHash, &k.TagsJSON,
		&k.GrantsJSON, &k.PolicyJSON, &k.RecordInputMode, &recordOutput, &recordThink, &k.Status,
		&expiresAt, &lastUsedAt, &k.CreatedBy, &createdAt, &k.FeishuOpenID, &k.FeishuUnionID,
		&k.FeishuName, &feishuBoundAt, &k.FeishuBoundBy); err != nil {
		return nil, err
	}
	k.RecordOutputText = recordOutput != 0
	k.RecordReasoning = recordThink != 0
	k.ExpiresAt = timePtrFromNull(expiresAt)
	k.LastUsedAt = timePtrFromNull(lastUsedAt)
	k.FeishuBoundAt = timePtrFromNull(feishuBoundAt)
	k.CreatedAt = timeFromUnix(createdAt)
	return &k, nil
}

// GetAPIKeyByPrefix loads the key row used for lookup (hash comparison happens in the caller).
func (db *DB) GetAPIKeyByPrefix(ctx context.Context, prefix string) (*domain.APIKey, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+apiKeyCols+" FROM api_keys WHERE key_prefix = ?", prefix)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrUnauthorized("invalid API key")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get api key by prefix: %w", err)
	}
	return k, nil
}

// FindAPIKeyByPrefix is the administrative variant of GetAPIKeyByPrefix: a missing row is
// reported as (nil, nil) rather than as an authentication failure.
//
// The two differ on purpose. On the data plane an unknown prefix is indistinguishable from
// a wrong secret and must not be described to the caller; the key importer, on the other
// hand, has to tell "this key is new" from "this key is already here" before it overwrites
// a row, and it is already behind an administrator session.
func (db *DB) FindAPIKeyByPrefix(ctx context.Context, prefix string) (*domain.APIKey, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+apiKeyCols+" FROM api_keys WHERE key_prefix = ?", prefix)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find api key by prefix: %w", err)
	}
	return k, nil
}

// ListAPIKeys lists the keys of one account (accountID <= 0 means all accounts).
func (db *DB) ListAPIKeys(ctx context.Context, accountID int64) ([]*domain.APIKey, error) {
	query := "SELECT " + apiKeyCols + " FROM api_keys"
	args := []any{}
	if accountID > 0 {
		query += " WHERE account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY id"

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()

	out := []*domain.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate api keys: %w", err)
	}
	return out, nil
}

// UpsertAPIKey inserts a new key or updates an existing one (matched by key_prefix).
func (db *DB) UpsertAPIKey(ctx context.Context, k *domain.APIKey) (int64, error) {
	if k == nil || k.KeyPrefix == "" || k.KeyHash == "" {
		return 0, domain.ErrInvalidRequest("api key prefix and hash are required")
	}
	if k.AccountID == 0 {
		return 0, domain.ErrInvalidRequest("api key requires an account")
	}
	now := time.Now().UTC()
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now
	}
	if k.Status == "" {
		k.Status = "active"
	}
	if k.RecordInputMode == "" {
		k.RecordInputMode = "inherit"
	}

	if _, err := db.write.ExecContext(ctx, `
INSERT INTO api_keys(account_id, name, key_prefix, key_hash, tags_json, grants_json, policy_json,
  record_input_mode, record_output_text, record_reasoning, status, expires_at, last_used_at, created_by, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(key_prefix) DO UPDATE SET
  account_id = excluded.account_id,
  name = excluded.name,
  key_hash = excluded.key_hash,
  tags_json = excluded.tags_json,
  grants_json = excluded.grants_json,
  policy_json = excluded.policy_json,
  record_input_mode = excluded.record_input_mode,
  record_output_text = excluded.record_output_text,
  record_reasoning = excluded.record_reasoning,
  status = excluded.status,
  expires_at = excluded.expires_at`,
		k.AccountID, k.Name, k.KeyPrefix, k.KeyHash, k.TagsJSON, k.GrantsJSON, k.PolicyJSON,
		k.RecordInputMode, boolInt(k.RecordOutputText), boolInt(k.RecordReasoning), k.Status,
		unixPtr(k.ExpiresAt), unixPtr(k.LastUsedAt), k.CreatedBy, unix(k.CreatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert api key %q: %w", k.Name, err)
	}

	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM api_keys WHERE key_prefix = ?", k.KeyPrefix).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve api key id: %w", err)
	}
	k.ID = id
	return id, nil
}

// SetAPIKeyRecording toggles the per-key content recording switches
// (record_output_text / record_reasoning are the two admin checkboxes).
func (db *DB) SetAPIKeyRecording(ctx context.Context, id int64, recordOutputText, recordReasoning bool, inputMode string) error {
	if inputMode == "" {
		inputMode = "inherit"
	}
	_, err := db.write.ExecContext(ctx, `
UPDATE api_keys SET record_input_mode = ?, record_output_text = ?, record_reasoning = ?
WHERE id = ?`, inputMode, boolInt(recordOutputText), boolInt(recordReasoning), id)
	if err != nil {
		return fmt.Errorf("store: set api key %d recording: %w", id, err)
	}
	return nil
}

// GetAPIKeyByID loads one key row by id. The console's Feishu binding endpoints address
// keys by id, and a missing row is reported as not-found rather than as an auth failure.
func (db *DB) GetAPIKeyByID(ctx context.Context, id int64) (*domain.APIKey, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+apiKeyCols+" FROM api_keys WHERE id = ?", id)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound(fmt.Sprintf("api key %d", id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get api key %d: %w", id, err)
	}
	return k, nil
}

// BindAPIKeyFeishu writes the key's Feishu identity, replacing any previous one.
//
// It is deliberately a single-column UPDATE rather than a reuse of UpsertAPIKey: that
// path rewrites a whole row from a struct, and it is exactly how the per-key recording
// switches were once silently reverted. A binding must be written by one statement that
// cannot touch anything else, and upsert must not touch the binding (see the columns it
// lists). The unique index over feishu_open_id (empty values excluded, so unbound keys
// never collide) makes "one Feishu identity, one key" a database invariant; the violation
// is reported as a conflict.
func (db *DB) BindAPIKeyFeishu(ctx context.Context, id int64, binding domain.FeishuBinding) error {
	if binding.OpenID == "" {
		return domain.ErrInvalidRequest("a Feishu binding requires an open_id")
	}
	boundAt := binding.BoundAt
	if boundAt.IsZero() {
		boundAt = time.Now().UTC()
	}
	result, err := db.write.ExecContext(ctx, `
UPDATE api_keys SET feishu_open_id = ?, feishu_union_id = ?, feishu_name = ?,
  feishu_bound_at = ?, feishu_bound_by = ?
WHERE id = ?`,
		binding.OpenID, binding.UnionID, binding.Name, unix(boundAt), binding.BoundBy, id)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrConflict("this Feishu account is already bound to another API key")
		}
		return fmt.Errorf("store: bind api key %d to Feishu: %w", id, err)
	}
	// A binding that matched no row would otherwise look like a success, and the console
	// would report a bound key that does not exist.
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: bind api key %d to Feishu: %w", id, err)
	}
	if affected == 0 {
		if _, err := db.GetAPIKeyByID(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// UnbindAPIKeyFeishu clears the key's Feishu identity and reports whether anything
// changed, so the caller can answer idempotently instead of guessing.
func (db *DB) UnbindAPIKeyFeishu(ctx context.Context, id int64) (bool, error) {
	result, err := db.write.ExecContext(ctx, `
UPDATE api_keys SET feishu_open_id = '', feishu_union_id = '', feishu_name = '',
  feishu_bound_at = NULL, feishu_bound_by = ''
WHERE id = ? AND feishu_open_id <> ''`, id)
	if err != nil {
		return false, fmt.Errorf("store: unbind api key %d from Feishu: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unbind api key %d from Feishu: %w", id, err)
	}
	return affected > 0, nil
}

// FindAPIKeyByFeishuOpenID resolves a bound Feishu identity to its key. A missing row is
// (nil, nil): an unbound person is an ordinary answer on the login path, not an error
// worth a log line.
func (db *DB) FindAPIKeyByFeishuOpenID(ctx context.Context, openID string) (*domain.APIKey, error) {
	if strings.TrimSpace(openID) == "" {
		return nil, nil
	}
	row := db.read.QueryRowContext(ctx, "SELECT "+apiKeyCols+" FROM api_keys WHERE feishu_open_id = ?", openID)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: find api key by Feishu open id: %w", err)
	}
	return k, nil
}

// ListAPIKeyFeishuIdentities returns every bound key as (key, account, identity): the
// directory sync (M70) reads them all at once — a deployment holds dozens of keys, not
// thousands — to recognize people that were bound at the key level (M60) before the
// account-level mapping existed. Unbound keys are simply not in the answer.
func (db *DB) ListAPIKeyFeishuIdentities(ctx context.Context) ([]domain.KeyFeishuIdentity, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT id, account_id, feishu_open_id, feishu_union_id,
		feishu_name, feishu_bound_at, feishu_bound_by FROM api_keys
		WHERE feishu_open_id <> '' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list bound api keys: %w", err)
	}
	defer rows.Close()

	out := []domain.KeyFeishuIdentity{}
	for rows.Next() {
		var (
			identity domain.KeyFeishuIdentity
			binding  domain.FeishuBinding
			boundAt  sql.NullInt64
		)
		if err := rows.Scan(&identity.KeyID, &identity.AccountID, &binding.OpenID, &binding.UnionID,
			&binding.Name, &boundAt, &binding.BoundBy); err != nil {
			return nil, fmt.Errorf("store: scan bound api key: %w", err)
		}
		if boundAt.Valid {
			binding.BoundAt = timeFromUnix(boundAt.Int64)
		}
		identity.Binding = binding
		out = append(out, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate bound api keys: %w", err)
	}
	return out, nil
}

// TouchAPIKey records the last usage timestamp of a key.
func (db *DB) TouchAPIKey(ctx context.Context, id int64) error {
	_, err := db.write.ExecContext(ctx,
		"UPDATE api_keys SET last_used_at = ? WHERE id = ?", unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: touch api key %d: %w", id, err)
	}
	return nil
}

const mcpTokenCols = `id, account_id, name, token_hash, token_prefix, scope, status, last_used_at,
	expires_at, created_by, note, created_at`

func scanMCPToken(row rowScanner) (*domain.MCPToken, error) {
	var (
		tok                 domain.MCPToken
		lastUsed, expiresAt sql.NullInt64
		createdAt           int64
	)
	if err := row.Scan(&tok.ID, &tok.AccountID, &tok.Name, &tok.TokenHash, &tok.TokenPrefix,
		&tok.Scope, &tok.Status, &lastUsed, &expiresAt, &tok.CreatedBy, &tok.Note, &createdAt); err != nil {
		return nil, err
	}
	tok.LastUsedAt = timePtrFromNull(lastUsed)
	tok.ExpiresAt = timePtrFromNull(expiresAt)
	tok.CreatedAt = timeFromUnix(createdAt)
	return &tok, nil
}

// GetMCPTokenByPrefix loads an MCP token row for hash comparison.
func (db *DB) GetMCPTokenByPrefix(ctx context.Context, prefix string) (*domain.MCPToken, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+mcpTokenCols+" FROM mcp_tokens WHERE token_prefix = ?", prefix)
	tok, err := scanMCPToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrUnauthorized("invalid MCP token")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get mcp token: %w", err)
	}
	return tok, nil
}

// GetMCPTokenByID loads an MCP token row by its primary key.
//
// The console chat binds a token by id rather than by value (migration 0011), so it needs a
// lookup that does not start from the secret: the plaintext only exists in the response that
// issued it. Revocation is expressed by this row's status and expiry, which is why the chat
// re-reads it on every tool call instead of caching the scope on the session.
func (db *DB) GetMCPTokenByID(ctx context.Context, id int64) (*domain.MCPToken, error) {
	if id <= 0 {
		return nil, domain.ErrNotFound("MCP token")
	}
	row := db.read.QueryRowContext(ctx, "SELECT "+mcpTokenCols+" FROM mcp_tokens WHERE id = ?", id)
	tok, err := scanMCPToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound("MCP token")
	}
	if err != nil {
		return nil, fmt.Errorf("store: get mcp token by id: %w", err)
	}
	return tok, nil
}

// ListMCPTokens lists MCP tokens (accountID <= 0 means all accounts).
func (db *DB) ListMCPTokens(ctx context.Context, accountID int64) ([]*domain.MCPToken, error) {
	query := "SELECT " + mcpTokenCols + " FROM mcp_tokens"
	args := []any{}
	if accountID > 0 {
		query += " WHERE account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY id"

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp tokens: %w", err)
	}
	defer rows.Close()

	out := []*domain.MCPToken{}
	for rows.Next() {
		tok, err := scanMCPToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan mcp token: %w", err)
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate mcp tokens: %w", err)
	}
	return out, nil
}

// UpsertMCPToken inserts or updates an MCP token (matched by token_prefix).
func (db *DB) UpsertMCPToken(ctx context.Context, tok *domain.MCPToken) (int64, error) {
	if tok == nil || tok.TokenPrefix == "" || tok.TokenHash == "" {
		return 0, domain.ErrInvalidRequest("mcp token prefix and hash are required")
	}
	if tok.AccountID == 0 {
		return 0, domain.ErrInvalidRequest("mcp token requires an account")
	}
	if tok.CreatedAt.IsZero() {
		tok.CreatedAt = time.Now().UTC()
	}
	if tok.Status == "" {
		tok.Status = "active"
	}
	if tok.Scope == "" {
		// The database default is the read-only scope; spelling it out here keeps the
		// in-memory value and the stored row in agreement.
		tok.Scope = "query"
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO mcp_tokens(account_id, name, token_hash, token_prefix, scope, status, last_used_at, expires_at, created_by, note, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(token_prefix) DO UPDATE SET
  account_id = excluded.account_id,
  name = excluded.name,
  token_hash = excluded.token_hash,
  scope = excluded.scope,
  status = excluded.status,
  expires_at = excluded.expires_at,
  note = excluded.note`,
		tok.AccountID, tok.Name, tok.TokenHash, tok.TokenPrefix, tok.Scope, tok.Status,
		unixPtr(tok.LastUsedAt), unixPtr(tok.ExpiresAt), tok.CreatedBy, tok.Note, unix(tok.CreatedAt)); err != nil {
		return 0, fmt.Errorf("store: upsert mcp token: %w", err)
	}
	var id int64
	if err := db.write.QueryRowContext(ctx, "SELECT id FROM mcp_tokens WHERE token_prefix = ?", tok.TokenPrefix).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: resolve mcp token id: %w", err)
	}
	tok.ID = id
	return id, nil
}

// TouchMCPToken records the last use of an MCP token.
func (db *DB) TouchMCPToken(ctx context.Context, id int64) error {
	if _, err := db.write.ExecContext(ctx,
		"UPDATE mcp_tokens SET last_used_at = ? WHERE id = ?", unix(time.Now()), id); err != nil {
		return fmt.Errorf("store: touch mcp token %d: %w", id, err)
	}
	return nil
}

// RevokeMCPToken marks a token as revoked.
func (db *DB) RevokeMCPToken(ctx context.Context, id int64) error {
	_, err := db.write.ExecContext(ctx, "UPDATE mcp_tokens SET status = 'revoked' WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: revoke mcp token %d: %w", id, err)
	}
	return nil
}

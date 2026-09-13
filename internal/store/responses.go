package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const responseCols = `id, api_key_id, account_id, model, provider_id, status, request_json,
	output_json, usage_json, instructions, created_at, completed_at, expires_at`

// PutResponse inserts or replaces a stored response.
func (db *DB) PutResponse(ctx context.Context, rec *domain.ResponseRecord) error {
	args, err := responseArgs(rec)
	if err != nil {
		return err
	}
	if _, err := db.stmts.do(ctx, db.write, putResponseSQL, args...); err != nil {
		return fmt.Errorf("store: put response %s: %w", rec.ID, err)
	}
	return nil
}

// putResponseTx writes one response inside a caller-owned transaction (audit batching).
func (db *DB) putResponseTx(ctx context.Context, tx *sql.Tx, rec *domain.ResponseRecord) error {
	args, err := responseArgs(rec)
	if err != nil {
		return err
	}
	if _, err := db.stmts.execInTx(ctx, db.write, tx, putResponseSQL, args...); err != nil {
		return fmt.Errorf("store: put response %s: %w", rec.ID, err)
	}
	return nil
}

// responseArgs validates the record and renders its positional arguments.
func responseArgs(rec *domain.ResponseRecord) ([]any, error) {
	if rec == nil || rec.ID == "" {
		return nil, domain.ErrInvalidRequest("response record requires an id")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	return []any{
		rec.ID, rec.APIKeyID, rec.AccountID, rec.Model, rec.ProviderID, rec.Status, rec.RequestJSON,
		rec.OutputJSON, rec.UsageJSON, rec.Instructions, unix(rec.CreatedAt),
		unixPtr(rec.CompletedAt), unixPtr(rec.ExpiresAt),
	}, nil
}

const putResponseSQL = `
INSERT INTO responses(id, api_key_id, account_id, model, provider_id, status, request_json,
  output_json, usage_json, instructions, created_at, completed_at, expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  status = excluded.status,
  output_json = excluded.output_json,
  usage_json = excluded.usage_json,
  provider_id = excluded.provider_id,
  completed_at = excluded.completed_at,
  expires_at = excluded.expires_at`

// GetResponse loads a stored response.
func (db *DB) GetResponse(ctx context.Context, id string) (*domain.ResponseRecord, error) {
	row := db.read.QueryRowContext(ctx, "SELECT "+responseCols+" FROM responses WHERE id = ?", id)

	var (
		rec                    domain.ResponseRecord
		createdAt              int64
		completedAt, expiresAt sql.NullInt64
	)
	if err := row.Scan(&rec.ID, &rec.APIKeyID, &rec.AccountID, &rec.Model, &rec.ProviderID,
		&rec.Status, &rec.RequestJSON, &rec.OutputJSON, &rec.UsageJSON, &rec.Instructions,
		&createdAt, &completedAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound("response " + id)
		}
		return nil, fmt.Errorf("store: get response %s: %w", id, err)
	}
	rec.CreatedAt = timeFromUnix(createdAt)
	rec.CompletedAt = timePtrFromNull(completedAt)
	rec.ExpiresAt = timePtrFromNull(expiresAt)
	return &rec, nil
}

// DeleteResponse removes a stored response.
func (db *DB) DeleteResponse(ctx context.Context, id string) error {
	if _, err := db.write.ExecContext(ctx, "DELETE FROM responses WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: delete response %s: %w", id, err)
	}
	return nil
}

// PutRequestLog stores one recorded request/response pair (upsert by request id).
func (db *DB) PutRequestLog(ctx context.Context, rec *domain.RequestLogRecord) error {
	if rec != nil && rec.TitleFingerprint != "" {
		tx, err := db.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = db.putRequestLogTx(ctx, tx, rec); err != nil {
			return err
		}
		return tx.Commit()
	}
	args, err := requestLogArgs(rec)
	if err != nil {
		return err
	}
	if _, err := db.stmts.do(ctx, db.write, putRequestLogSQL, args...); err != nil {
		return fmt.Errorf("store: put request log %s: %w", rec.RequestID, err)
	}
	return nil
}

// putRequestLogTx writes one request log inside a caller-owned transaction.
func (db *DB) putRequestLogTx(ctx context.Context, tx *sql.Tx, rec *domain.RequestLogRecord) error {
	args, err := requestLogArgs(rec)
	if err != nil {
		return err
	}
	if _, err := db.stmts.execInTx(ctx, db.write, tx, putRequestLogSQL, args...); err != nil {
		return fmt.Errorf("store: put request log %s: %w", rec.RequestID, err)
	}
	return db.linkTitleSessionTx(ctx, tx, rec)
}

func requestLogArgs(rec *domain.RequestLogRecord) ([]any, error) {
	if rec == nil || rec.RequestID == "" {
		return nil, domain.ErrInvalidRequest("request log requires a request id")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	return []any{
		rec.RequestID, rec.APIKeyID, rec.AccountID, rec.Endpoint, rec.RequestJSON,
		rec.ResponseReasoning, rec.ResponseText, boolInt(rec.ReasoningRecorded),
		boolInt(rec.OutputTextRecorded), rec.RequestBytes, rec.ResponseBytes, boolInt(rec.Truncated),
		rec.RecordInputMode, boolInt(rec.RecordReasoning), boolInt(rec.RecordOutputText),
		rec.Status, unix(rec.CreatedAt), rec.Client, rec.Model, rec.ResolvedModel,
		rec.Workspace, rec.SessionID, rec.CallKind, rec.Title,
	}, nil
}

// The identity columns are inserted but never refreshed on conflict: a second write of the
// same request id (the content-free skeleton retry) must not blank the identity the first
// write captured.
const putRequestLogSQL = `
INSERT INTO request_logs(request_id, api_key_id, account_id, endpoint, request_json,
  response_reasoning, response_text, reasoning_recorded, output_text_recorded,
  request_bytes, response_bytes, truncated, record_input_mode, record_reasoning,
  record_output_text, status, created_at, client, model, resolved_model, workspace,
  session_id, call_kind, title)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(request_id) DO UPDATE SET
  response_reasoning = excluded.response_reasoning,
  response_text = excluded.response_text,
  reasoning_recorded = excluded.reasoning_recorded,
  output_text_recorded = excluded.output_text_recorded,
  response_bytes = excluded.response_bytes,
  truncated = excluded.truncated,
  status = excluded.status`

// GetRequestLog loads one recorded request/response pair.
func (db *DB) GetRequestLog(ctx context.Context, requestID string) (*domain.RequestLogRecord, error) {
	row := db.read.QueryRowContext(ctx, `
SELECT `+requestLogColumns+`
FROM request_logs WHERE request_id = ?`, requestID)

	rec, err := scanRequestLog(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound("request log " + requestID)
		}
		return nil, fmt.Errorf("store: get request log %s: %w", requestID, err)
	}
	return rec, nil
}

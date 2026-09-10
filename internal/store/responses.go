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
	if rec == nil || rec.ID == "" {
		return domain.ErrInvalidRequest("response record requires an id")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO responses(id, api_key_id, account_id, model, provider_id, status, request_json,
  output_json, usage_json, instructions, created_at, completed_at, expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  status = excluded.status,
  output_json = excluded.output_json,
  usage_json = excluded.usage_json,
  provider_id = excluded.provider_id,
  completed_at = excluded.completed_at,
  expires_at = excluded.expires_at`,
		rec.ID, rec.APIKeyID, rec.AccountID, rec.Model, rec.ProviderID, rec.Status, rec.RequestJSON,
		rec.OutputJSON, rec.UsageJSON, rec.Instructions, unix(rec.CreatedAt),
		unixPtr(rec.CompletedAt), unixPtr(rec.ExpiresAt)); err != nil {
		return fmt.Errorf("store: put response %s: %w", rec.ID, err)
	}
	return nil
}

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
	if rec == nil || rec.RequestID == "" {
		return domain.ErrInvalidRequest("request log requires a request id")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if _, err := db.write.ExecContext(ctx, `
INSERT INTO request_logs(request_id, api_key_id, account_id, endpoint, request_json,
  response_reasoning, response_text, reasoning_recorded, output_text_recorded,
  request_bytes, response_bytes, truncated, record_input_mode, record_reasoning,
  record_output_text, status, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(request_id) DO UPDATE SET
  response_reasoning = excluded.response_reasoning,
  response_text = excluded.response_text,
  reasoning_recorded = excluded.reasoning_recorded,
  output_text_recorded = excluded.output_text_recorded,
  response_bytes = excluded.response_bytes,
  truncated = excluded.truncated,
  status = excluded.status`,
		rec.RequestID, rec.APIKeyID, rec.AccountID, rec.Endpoint, rec.RequestJSON,
		rec.ResponseReasoning, rec.ResponseText, boolInt(rec.ReasoningRecorded),
		boolInt(rec.OutputTextRecorded), rec.RequestBytes, rec.ResponseBytes, boolInt(rec.Truncated),
		rec.RecordInputMode, boolInt(rec.RecordReasoning), boolInt(rec.RecordOutputText),
		rec.Status, unix(rec.CreatedAt)); err != nil {
		return fmt.Errorf("store: put request log %s: %w", rec.RequestID, err)
	}
	return nil
}

// GetRequestLog loads one recorded request/response pair.
func (db *DB) GetRequestLog(ctx context.Context, requestID string) (*domain.RequestLogRecord, error) {
	row := db.read.QueryRowContext(ctx, `
SELECT id, request_id, api_key_id, account_id, endpoint, request_json, response_reasoning,
       response_text, reasoning_recorded, output_text_recorded, request_bytes, response_bytes,
       truncated, record_input_mode, record_reasoning, record_output_text, status, created_at
FROM request_logs WHERE request_id = ?`, requestID)

	var (
		rec                                 domain.RequestLogRecord
		reasoningRecorded, outputRecorded   int
		reasoningFlag, outputFlag, truncate int
		createdAt                           int64
	)
	if err := row.Scan(&rec.ID, &rec.RequestID, &rec.APIKeyID, &rec.AccountID, &rec.Endpoint,
		&rec.RequestJSON, &rec.ResponseReasoning, &rec.ResponseText, &reasoningRecorded,
		&outputRecorded, &rec.RequestBytes, &rec.ResponseBytes, &truncate, &rec.RecordInputMode,
		&reasoningFlag, &outputFlag, &rec.Status, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound("request log " + requestID)
		}
		return nil, fmt.Errorf("store: get request log %s: %w", requestID, err)
	}
	rec.ReasoningRecorded = reasoningRecorded != 0
	rec.OutputTextRecorded = outputRecorded != 0
	rec.RecordReasoning = reasoningFlag != 0
	rec.RecordOutputText = outputFlag != 0
	rec.Truncated = truncate != 0
	rec.CreatedAt = timeFromUnix(createdAt)
	return &rec, nil
}

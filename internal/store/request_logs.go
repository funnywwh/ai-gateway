package store

import (
	"context"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// ListRequestLogs returns recorded requests of one account (newest first).
func (db *DB) ListRequestLogs(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.RequestLogRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `
SELECT id, request_id, api_key_id, account_id, endpoint, request_json, response_reasoning,
       response_text, reasoning_recorded, output_text_recorded, request_bytes, response_bytes,
       truncated, record_input_mode, record_reasoning, record_output_text, status, created_at
FROM request_logs WHERE account_id = ?`
	args := []any{accountID}
	if !from.IsZero() {
		query += " AND created_at >= ?"
		args = append(args, unix(from))
	}
	if !to.IsZero() {
		query += " AND created_at <= ?"
		args = append(args, unix(to))
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list request logs: %w", err)
	}
	defer rows.Close()

	out := []*domain.RequestLogRecord{}
	for rows.Next() {
		var (
			rec                                 domain.RequestLogRecord
			reasoningRecorded, outputRecorded   int
			reasoningFlag, outputFlag, truncate int
			createdAt                           int64
		)
		if err := rows.Scan(&rec.ID, &rec.RequestID, &rec.APIKeyID, &rec.AccountID, &rec.Endpoint,
			&rec.RequestJSON, &rec.ResponseReasoning, &rec.ResponseText, &reasoningRecorded,
			&outputRecorded, &rec.RequestBytes, &rec.ResponseBytes, &truncate, &rec.RecordInputMode,
			&reasoningFlag, &outputFlag, &rec.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan request log: %w", err)
		}
		rec.ReasoningRecorded = reasoningRecorded != 0
		rec.OutputTextRecorded = outputRecorded != 0
		rec.RecordReasoning = reasoningFlag != 0
		rec.RecordOutputText = outputFlag != 0
		rec.Truncated = truncate != 0
		rec.CreatedAt = timeFromUnix(createdAt)
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate request logs: %w", err)
	}
	return out, nil
}

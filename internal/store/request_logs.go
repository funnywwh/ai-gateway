package store

import (
	"context"
	"fmt"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// requestLogFilter builds the WHERE clause shared by the request-log page query and
// its count, so a page can never disagree with the total it reports.
func requestLogFilter(accountID int64, from, to time.Time) (string, []any) {
	where := " WHERE 1 = 1"
	args := []any{}
	// account_id <= 0 means "every account": the management console lists requests across
	// tenants, so the filter has to be optional (it used to be applied unconditionally,
	// which silently returned nothing for the console).
	if accountID > 0 {
		where += " AND account_id = ?"
		args = append(args, accountID)
	}
	if !from.IsZero() {
		where += " AND created_at >= ?"
		args = append(args, unix(from))
	}
	if !to.IsZero() {
		where += " AND created_at <= ?"
		args = append(args, unix(to))
	}
	return where, args
}

// ListRequestLogs returns the newest recorded requests of one account (first page).
func (db *DB) ListRequestLogs(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.RequestLogRecord, error) {
	return db.ListRequestLogsPage(ctx, accountID, from, to, limit, 0)
}

// requestLogListSQL returns the page query for the console's request-log list. It is a
// named function rather than an inline literal so the order-and-plan test can EXPLAIN the
// very statement the store runs; see historyPageOrder for why the ORDER BY is a contract.
func requestLogListSQL(where string) string {
	return `
SELECT id, request_id, api_key_id, account_id, endpoint, request_json, response_reasoning,
       response_text, reasoning_recorded, output_text_recorded, request_bytes, response_bytes,
       truncated, record_input_mode, record_reasoning, record_output_text, status, created_at
FROM request_logs` + where + historyPageOrder + " LIMIT ? OFFSET ?"
}

// ListRequestLogsPage returns one page of recorded requests (newest first).
func (db *DB) ListRequestLogsPage(ctx context.Context, accountID int64, from, to time.Time, limit, offset int) ([]*domain.RequestLogRecord, error) {
	limit = normalizeLimit(limit, 100, 1000)
	if offset < 0 {
		offset = 0
	}
	where, args := requestLogFilter(accountID, from, to)
	query := requestLogListSQL(where)
	args = append(args, limit, offset)

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

// CountRequestLogs counts the recorded requests the same filters select. The console
// needs it to show "共 N 条" and to know whether a next page exists.
func (db *DB) CountRequestLogs(ctx context.Context, accountID int64, from, to time.Time) (int, error) {
	where, args := requestLogFilter(accountID, from, to)
	return db.countRows(ctx, "request_logs", where, args, "request logs")
}

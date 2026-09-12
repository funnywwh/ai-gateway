package store

import (
	"context"

	"fmt"
	"github.com/winger/ai-gateway/internal/domain"
	"sort"
	"time"
)

// UsageWindowTotals and UsageBreakdownRow alias the domain types: persistence returns
// the shared vocabulary instead of defining its own.
type UsageWindowTotals = domain.UsageTotals

type UsageBreakdownRow = domain.UsageBreakdownRow

// UsageWindowTotals aggregates one account's attempts inside a window.
//
// The aggregation happens in SQL so the numbers never depend on how many detail rows
// a caller is allowed to read.
func (db *DB) UsageWindowTotals(ctx context.Context, accountID int64, from, to time.Time) (*UsageWindowTotals, error) {
	totals := &UsageWindowTotals{}
	row := db.read.QueryRowContext(ctx, `
SELECT COUNT(*),
  COALESCE(SUM(CASE WHEN status <> 'completed' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN usage_source = 'estimated' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(json_extract(dimensions_json, '$.input')), 0)
    + COALESCE(SUM(json_extract(dimensions_json, '$.input_cache_hit')), 0)
    + COALESCE(SUM(json_extract(dimensions_json, '$.input_cache_miss')), 0),
  COALESCE(SUM(json_extract(dimensions_json, '$.output')), 0)
    + COALESCE(SUM(json_extract(dimensions_json, '$.reasoning')), 0),
  COALESCE(SUM(cost_micros), 0), COALESCE(SUM(charge_micros), 0),
  COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0)
FROM usage_records WHERE account_id = ? AND created_at >= ? AND created_at <= ?`,
		accountID, unix(from), unix(to))
	if err := row.Scan(&totals.Attempts, &totals.Failed, &totals.Estimated, &totals.PromptTokens,
		&totals.CompletionTokens, &totals.CostMicros, &totals.ChargeMicros, &totals.TTFTAvgMS); err != nil {
		return nil, fmt.Errorf("store: usage window totals: %w", err)
	}

	samples, capped, err := db.ttftSamples(ctx, accountID, from, to, 2000)
	if err != nil {
		return nil, err
	}
	totals.TTFTP95MS = percentile(samples, 0.95)
	totals.TTFTP95Estimated = capped
	return totals, nil
}

// ttftSamplesSQL returns the newest-first sample query; named for the same reason as
// requestLogListSQL (see historyPageOrder).
func ttftSamplesSQL() string {
	return `
SELECT ttft_ms FROM usage_records
WHERE account_id = ? AND created_at >= ? AND created_at <= ? AND ttft_ms > 0` +
		historyPageOrder + " LIMIT ?"
}

func (db *DB) ttftSamples(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]int64, bool, error) {
	rows, err := db.read.QueryContext(ctx, ttftSamplesSQL(), accountID, unix(from), unix(to), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: ttft samples: %w", err)
	}
	defer rows.Close()
	samples := []int64{}
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, false, fmt.Errorf("store: scan ttft sample: %w", err)
		}
		samples = append(samples, value)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: iterate ttft samples: %w", err)
	}
	capped := len(samples) > limit
	if capped {
		samples = samples[:limit]
	}
	return samples, capped, nil
}

func percentile(values []int64, fraction float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * fraction)
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

// UsageBreakdown groups one account's attempts by model, key or day.
func (db *DB) UsageBreakdown(ctx context.Context, accountID int64, from, to time.Time, groupBy string) ([]UsageBreakdownRow, error) {
	expression := "model"
	switch groupBy {
	case "key":
		expression = "CAST(api_key_id AS TEXT)"
	case "day":
		expression = "date(created_at, 'unixepoch')"
	case "model", "":
	default:
		return nil, fmt.Errorf("store: group_by must be model, key or day")
	}
	query := fmt.Sprintf(`
SELECT %s AS group_key, COUNT(*),
  COALESCE(SUM(CASE WHEN status <> 'completed' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(json_extract(dimensions_json, '$.input')), 0)
    + COALESCE(SUM(json_extract(dimensions_json, '$.input_cache_hit')), 0)
    + COALESCE(SUM(json_extract(dimensions_json, '$.input_cache_miss')), 0),
  COALESCE(SUM(json_extract(dimensions_json, '$.output')), 0)
    + COALESCE(SUM(json_extract(dimensions_json, '$.reasoning')), 0),
  COALESCE(SUM(cost_micros), 0), COALESCE(SUM(charge_micros), 0)
FROM usage_records WHERE account_id = ? AND created_at >= ? AND created_at <= ?
GROUP BY group_key ORDER BY charge_micros DESC`, expression)
	rows, err := db.read.QueryContext(ctx, query, accountID, unix(from), unix(to))
	if err != nil {
		return nil, fmt.Errorf("store: usage breakdown: %w", err)
	}
	defer rows.Close()
	out := []UsageBreakdownRow{}
	for rows.Next() {
		var row UsageBreakdownRow
		if err := rows.Scan(&row.GroupKey, &row.Requests, &row.Failed, &row.PromptTokens,
			&row.CompletionTokens, &row.CostMicros, &row.ChargeMicros); err != nil {
			return nil, fmt.Errorf("store: scan usage breakdown: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate usage breakdown: %w", err)
	}
	return out, nil
}

// ListUsageCounters returns the rollups of one account for a period (YYYY-MM).
func (db *DB) ListUsageCounters(ctx context.Context, accountID int64, period string) ([]UsageCounter, error) {
	query := "SELECT account_id, api_key_id, tag, period, requests, tokens, cost_micros, charge_micros FROM usage_counters"
	args := []any{}
	where := []string{}
	if accountID > 0 {
		where = append(where, "account_id = ?")
		args = append(args, accountID)
	}
	if period != "" {
		where = append(where, "period = ?")
		args = append(args, period)
	}
	if len(where) > 0 {
		query += " WHERE " + joinAnd(where)
	}
	query += " ORDER BY period DESC, api_key_id, tag"
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list usage counters: %w", err)
	}
	defer rows.Close()
	out := []UsageCounter{}
	for rows.Next() {
		var counter UsageCounter
		if err := rows.Scan(&counter.AccountID, &counter.APIKeyID, &counter.Tag, &counter.Period,
			&counter.Requests, &counter.Tokens, &counter.CostMicros, &counter.ChargeMicros); err != nil {
			return nil, fmt.Errorf("store: scan usage counter: %w", err)
		}
		out = append(out, counter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate usage counters: %w", err)
	}
	return out, nil
}

func joinAnd(parts []string) string {
	result := ""
	for index, part := range parts {
		if index > 0 {
			result += " AND "
		}
		result += part
	}
	return result
}

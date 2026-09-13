package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/winger/ai-gateway/internal/domain"
)

// requestLogColumns is the recorded-request projection, shared by the page query and the
// detail lookup so the two can never disagree about which column is which.
const requestLogColumns = `id, request_id, api_key_id, account_id, endpoint, request_json,
       response_reasoning, response_text, reasoning_recorded, output_text_recorded,
       request_bytes, response_bytes, truncated, record_input_mode, record_reasoning,
       record_output_text, status, created_at, client, model, resolved_model, workspace,
       session_id, call_kind, title`

// requestLogFilter builds the WHERE clause shared by every request-log read. prefix is the
// table alias the columns carry ("" for the single-table queries, "r." when the query
// joins usage_records).
func requestLogFilter(prefix string, f domain.RequestLogFilter) (string, []any) {
	where := " WHERE 1 = 1"
	args := []any{}
	// account_id <= 0 means "every account": the management console lists requests across
	// tenants, so the filter has to be optional (it used to be applied unconditionally,
	// which silently returned nothing for the console). api_key_id follows the same rule.
	if f.AccountID > 0 {
		where += " AND " + prefix + "account_id = ?"
		args = append(args, f.AccountID)
	}
	if f.APIKeyID > 0 {
		where += " AND " + prefix + "api_key_id = ?"
		args = append(args, f.APIKeyID)
	}
	if !f.From.IsZero() {
		where += " AND " + prefix + "created_at >= ?"
		args = append(args, unix(f.From))
	}
	if !f.To.IsZero() {
		where += " AND " + prefix + "created_at <= ?"
		args = append(args, unix(f.To))
	}
	for _, dim := range []struct {
		column string
		value  string
	}{
		{"client", f.Client},
		{"model", f.Model},
		{"resolved_model", f.ResolvedModel},
		{"workspace", f.Workspace},
		{"session_id", f.SessionID},
		{"call_kind", f.CallKind},
	} {
		if dim.value == "" {
			continue
		}
		where += " AND " + prefix + dim.column + " = ?"
		args = append(args, dim.value)
	}
	return where, args
}

// ListRequestLogs returns the newest recorded requests of one account (first page).
func (db *DB) ListRequestLogs(ctx context.Context, f domain.RequestLogFilter, limit int) ([]*domain.RequestLogRecord, error) {
	return db.ListRequestLogsPage(ctx, f, limit, 0)
}

// requestLogListSQL returns the page query for the console's request-log list. It is a
// named function rather than an inline literal so the order-and-plan test can EXPLAIN the
// very statement the store runs; see historyPageOrder for why the ORDER BY is a contract.
func requestLogListSQL(where string) string {
	return `
SELECT ` + requestLogColumns + `
FROM request_logs` + where + historyPageOrder + " LIMIT ? OFFSET ?"
}

// ListRequestLogsPage returns one page of recorded requests (newest first).
func (db *DB) ListRequestLogsPage(ctx context.Context, f domain.RequestLogFilter, limit, offset int) ([]*domain.RequestLogRecord, error) {
	limit = normalizeLimit(limit, 100, 1000)
	if offset < 0 {
		offset = 0
	}
	where, args := requestLogFilter("", f)
	query := requestLogListSQL(where)
	args = append(args, limit, offset)

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list request logs: %w", err)
	}
	defer rows.Close()

	out := []*domain.RequestLogRecord{}
	for rows.Next() {
		rec, err := scanRequestLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate request logs: %w", err)
	}
	return out, nil
}

// scanRequestLog reads one row of requestLogColumns in order. It takes the rowScanner
// declared in accounts.go so the page query and the detail lookup share one projection.
func scanRequestLog(row rowScanner) (*domain.RequestLogRecord, error) {
	var (
		rec                                 domain.RequestLogRecord
		reasoningRecorded, outputRecorded   int
		reasoningFlag, outputFlag, truncate int
		createdAt                           int64
	)
	if err := row.Scan(&rec.ID, &rec.RequestID, &rec.APIKeyID, &rec.AccountID, &rec.Endpoint,
		&rec.RequestJSON, &rec.ResponseReasoning, &rec.ResponseText, &reasoningRecorded,
		&outputRecorded, &rec.RequestBytes, &rec.ResponseBytes, &truncate, &rec.RecordInputMode,
		&reasoningFlag, &outputFlag, &rec.Status, &createdAt, &rec.Client, &rec.Model,
		&rec.ResolvedModel, &rec.Workspace, &rec.SessionID, &rec.CallKind, &rec.Title); err != nil {
		return nil, fmt.Errorf("store: scan request log: %w", err)
	}
	rec.ReasoningRecorded = reasoningRecorded != 0
	rec.OutputTextRecorded = outputRecorded != 0
	rec.RecordReasoning = reasoningFlag != 0
	rec.RecordOutputText = outputFlag != 0
	rec.Truncated = truncate != 0
	rec.CreatedAt = timeFromUnix(createdAt)
	return &rec, nil
}

// CountRequestLogs counts the recorded requests the same filters select. The console
// needs it to show "共 N 条" and to know whether a next page exists.
func (db *DB) CountRequestLogs(ctx context.Context, f domain.RequestLogFilter) (int, error) {
	where, args := requestLogFilter("", f)
	return db.countRows(ctx, "request_logs", where, args, "request logs")
}

// usageTokenExpr is the token breakdown every aggregate uses, kept in one place so the
// request log and the billing breakdown cannot drift apart: `input` covers providers that
// report a single input count, while the two cache buckets are what the DeepSeek and
// OpenAI responses APIs report instead. prefix is the table alias the usage table carries
// ("" when the query has only one table).
func usageTokenExpr(prefix string) string {
	return fmt.Sprintf(`COALESCE(json_extract(%[1]sdimensions_json, '$.input'), 0)
    + COALESCE(json_extract(%[1]sdimensions_json, '$.input_cache_hit'), 0)
    + COALESCE(json_extract(%[1]sdimensions_json, '$.input_cache_miss'), 0)`, prefix)
}

// requestUsageSQL aggregates one request's usage rows. A request can have several attempts
// (failover), so token counts and money are summed while latency keeps the worst case.
// It is a function so the token expression stays in one place.
func requestUsageSQL(placeholders string) string {
	return `
SELECT request_id, COUNT(*), COALESCE(SUM(` + usageTokenExpr("") + `), 0),
       COALESCE(SUM(COALESCE(json_extract(dimensions_json, '$.input_cache_hit'), 0)), 0),
       COALESCE(SUM(COALESCE(json_extract(dimensions_json, '$.output'), 0)), 0),
       COALESCE(SUM(COALESCE(json_extract(dimensions_json, '$.reasoning'), 0)), 0),
       COALESCE(SUM(cost_micros), 0), COALESCE(SUM(charge_micros), 0),
       COALESCE(MAX(latency_ms), 0), COALESCE(MAX(ttft_ms), 0)
FROM usage_records WHERE request_id IN (` + placeholders + `) GROUP BY request_id`
}

// RequestUsage returns one request's metered consumption. A request with no usage row (a
// locally rejected one) reports Metered=false rather than an error: the caller is showing
// an audit row, and "no meter" is a fact about it.
func (db *DB) RequestUsage(ctx context.Context, requestID string) (*domain.RequestUsage, error) {
	if requestID == "" {
		return &domain.RequestUsage{}, nil
	}
	rows, err := db.RequestUsages(ctx, []string{requestID})
	if err != nil {
		return nil, err
	}
	if usage, ok := rows[requestID]; ok {
		return usage, nil
	}
	return &domain.RequestUsage{RequestID: requestID}, nil
}

// RequestUsages returns the metered consumption of many requests, keyed by request id.
// The list view uses one query per page instead of a join, so the page query keeps the
// index-driven ORDER BY that historyPageOrder depends on.
func (db *DB) RequestUsages(ctx context.Context, requestIDs []string) (map[string]*domain.RequestUsage, error) {
	out := map[string]*domain.RequestUsage{}
	ids := make([]string, 0, len(requestIDs))
	for _, id := range requestIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	query := requestUsageSQL(idPlaceholders(len(ids)))
	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read request usage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		usage := &domain.RequestUsage{Metered: true}
		if err := rows.Scan(&usage.RequestID, &usage.Attempts, &usage.InputTokens,
			&usage.CachedTokens, &usage.OutputTokens, &usage.ReasoningTokens, &usage.CostMicros,
			&usage.ChargeMicros, &usage.LatencyMS, &usage.TTFTMS); err != nil {
			return nil, fmt.Errorf("store: scan request usage: %w", err)
		}
		out[usage.RequestID] = usage
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate request usage: %w", err)
	}
	return out, nil
}

// RequestLogDimensionSorts are the accepted sort keys of the dimension breakdown, the
// default first. Every one of them is descending and ends with the group key as its
// tiebreaker: created_at is stamped in whole seconds, so "最近一次" has many ties, and an
// ORDER BY without a unique tail would let LIMIT/OFFSET repeat or skip a bucket — the same
// rule historyPageOrder follows with id.
var RequestLogDimensionSorts = []string{"last_seen", "requests", "charge"}

// RequestLogDimensionDefaultSort is what a caller that passes no sort gets: the buckets
// that were active most recently, which is the question the console's card opens with.
const RequestLogDimensionDefaultSort = "last_seen"

// requestLogDimensionSortExpr maps a sort key onto the ORDER BY tail of the breakdown.
//
// It spells the aggregate out instead of naming a SELECT alias on purpose: usage_records
// owns columns called charge_micros and cost_micros, so an ORDER BY identifier that could
// resolve to either the output column or the input column is exactly the ambiguity that
// silently changes an order. group_key is unique inside one grouping, which is what makes
// the sort total — and a total order is what pagination needs.
func requestLogDimensionSortExpr(sort string) (string, error) {
	switch sort {
	case "last_seen", "":
		return "MAX(r.created_at) DESC, group_key ASC", nil
	case "requests":
		return "COUNT(*) DESC, group_key ASC", nil
	case "charge":
		return "COALESCE(SUM(u.charge_micros), 0) DESC, group_key ASC", nil
	default:
		return "", fmt.Errorf("store: sort must be one of %s", strings.Join(RequestLogDimensionSorts, ", "))
	}
}

// ListRequestLogDimensionsPage groups recorded requests by one identity dimension, sums what
// they consumed, and returns one page of the buckets in the requested order. Title and
// Workspace only carry meaning in the session grouping (a session owns one title and one
// workspace); elsewhere they are whatever the group's last row had.
//
// The query deliberately selects only dimension columns and created_at: picking any
// content column would make SQLite read the body pages of every row in the window, which
// is the cost M24 removed from the console's list.
func (db *DB) ListRequestLogDimensionsPage(ctx context.Context, f domain.RequestLogFilter, groupBy, sort string, limit, offset int) ([]domain.RequestLogDimensionRow, error) {
	expression, err := requestLogGroupExpr(groupBy)
	if err != nil {
		return nil, err
	}
	order, err := requestLogDimensionSortExpr(sort)
	if err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit, 20, 200)
	if offset < 0 {
		offset = 0
	}
	where, args := requestLogFilter("r.", f)
	query := requestLogDimensionsSQL(expression, order, where)
	args = append(args, limit, offset)

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: request log dimensions: %w", err)
	}
	defer rows.Close()

	out := []domain.RequestLogDimensionRow{}
	for rows.Next() {
		var (
			row                 domain.RequestLogDimensionRow
			firstSeen, lastSeen int64
		)
		if err := rows.Scan(&row.Key, &row.Requests, &row.Metered, &firstSeen, &lastSeen,
			&row.Title, &row.Workspace, &row.InputTokens, &row.CachedTokens, &row.OutputTokens,
			&row.ReasoningTokens, &row.CostMicros, &row.ChargeMicros); err != nil {
			return nil, fmt.Errorf("store: scan request log dimension: %w", err)
		}
		row.FirstSeen = timeFromUnix(firstSeen)
		row.LastSeen = timeFromUnix(lastSeen)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate request log dimensions: %w", err)
	}
	return out, nil
}

// CountRequestLogDimensionGroups counts the buckets the same filters select — the number
// the console's pager shows as "共 N 个分组". It takes no sort key: the number of buckets is
// a property of the set, not of the order it is read in.
//
// It also omits the usage join the paged query needs, and that is not an approximation:
// the WHERE clause only names request_logs columns, and a LEFT JOIN can neither add nor
// drop a left-hand row, so the bucket set is identical either way. Dropping the join is
// what keeps this an index-only scan — a covering dimension index where the planner picks
// one, the time index plus a small GROUP BY sort otherwise, but never the table body or the
// metering table (docs/design/m31-request-log-stats-pagination.md §8).
func (db *DB) CountRequestLogDimensionGroups(ctx context.Context, f domain.RequestLogFilter, groupBy string) (int, error) {
	expression, err := requestLogGroupExpr(groupBy)
	if err != nil {
		return 0, err
	}
	where, args := requestLogFilter("r.", f)
	var total int
	if err := db.read.QueryRowContext(ctx, requestLogDimensionCountSQL(expression, where), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count request log dimension groups: %w", err)
	}
	return total, nil
}

// requestLogDimensionsSQL builds the breakdown query. It is a named function rather than
// an inline literal so the body-free assertion in the store's tests EXPLAINs the very
// statement production runs: selecting any content column would pull the recorded bodies
// of the whole window into the scan.
func requestLogDimensionsSQL(expression, order, where string) string {
	return fmt.Sprintf(`
SELECT %s AS group_key, COUNT(*), COUNT(u.request_id),
       MIN(r.created_at), MAX(r.created_at), MAX(r.title), MAX(r.workspace),
       COALESCE(SUM(`+usageTokenExpr("u.")+`), 0),
       COALESCE(SUM(COALESCE(json_extract(u.dimensions_json, '$.input_cache_hit'), 0)), 0),
       COALESCE(SUM(COALESCE(json_extract(u.dimensions_json, '$.output'), 0)), 0),
       COALESCE(SUM(COALESCE(json_extract(u.dimensions_json, '$.reasoning'), 0)), 0),
       COALESCE(SUM(u.cost_micros), 0), COALESCE(SUM(u.charge_micros), 0)
FROM request_logs r LEFT JOIN usage_records u ON u.request_id = r.request_id`+where+
		` GROUP BY group_key ORDER BY `+order+` LIMIT ? OFFSET ?`, expression)
}

// requestLogDimensionCountSQL counts the buckets of the breakdown. It is a named function
// for the same reason as requestLogDimensionsSQL: the test that pins "no bodies, no usage
// join" has to EXPLAIN the statement production runs, not a copy of it.
func requestLogDimensionCountSQL(expression, where string) string {
	return fmt.Sprintf(`
SELECT COUNT(*) FROM (
  SELECT %s AS group_key FROM request_logs r%s GROUP BY group_key
)`, expression, where)
}

// RequestLogDimensionNames are the accepted group_by values, in the order the console and
// MCP describe them.
var RequestLogDimensionNames = []string{"client", "model", "resolved_model", "workspace", "session", "call_kind", "account", "api_key"}

// requestLogGroupExpr maps a group_by value onto its column.
//
// The two credential dimensions (M30) group on the id cast to text, not on the name: the
// name lives in another table, is mutable, and is not unique for api_keys. The caller
// resolves ids to names for the buckets it is about to return.
func requestLogGroupExpr(groupBy string) (string, error) {
	switch groupBy {
	case "client", "":
		return "r.client", nil
	case "model":
		return "r.model", nil
	case "resolved_model":
		return "r.resolved_model", nil
	case "workspace":
		return "r.workspace", nil
	case "session":
		return "r.session_id", nil
	case "call_kind":
		return "r.call_kind", nil
	case "account":
		return "CAST(r.account_id AS TEXT)", nil
	case "api_key":
		return "CAST(r.api_key_id AS TEXT)", nil
	default:
		return "", fmt.Errorf("store: group_by must be client, model, resolved_model, workspace, session, call_kind, account or api_key")
	}
}

// idPlaceholders renders "?,?,…" for one IN list. It is shared by the request log's batch
// lookups so their placeholder building cannot drift apart.
func idPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// positiveIDs dedupes and drops the ids that name nothing (0 and below): a log row whose
// account or key id is 0 is the unknown bucket, and looking it up would only waste a scan.
func positiveIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// AccountNames resolves account ids to names for the request log's 用户 (account)
// dimension: one batched point lookup per page, the same shape as RequestUsages.
//
// It is a second query rather than a join in the page query on purpose: that query's
// ORDER BY (historyPageOrder) is satisfied by idx_request_logs_time, and joining a table
// that also has id/created_at would both make those names ambiguous and invite the temp
// B-tree M24 removed. An id with no row is simply absent from the map — the log row keeps
// its id and the console shows the id instead of a blank.
func (db *DB) AccountNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	unique := positiveIDs(ids)
	if len(unique) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(unique))
	for _, id := range unique {
		args = append(args, id)
	}
	rows, err := db.read.QueryContext(ctx,
		"SELECT id, name FROM accounts WHERE id IN ("+idPlaceholders(len(unique))+")", args...)
	if err != nil {
		return nil, fmt.Errorf("store: read account names: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: scan account name: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate account names: %w", err)
	}
	return out, nil
}

// APIKeyLabels resolves API key ids to their read-time labels (name + prefix). Same
// contract as AccountNames: batched, missing ids absent, empty input an empty map.
func (db *DB) APIKeyLabels(ctx context.Context, ids []int64) (map[int64]domain.APIKeyLabel, error) {
	out := map[int64]domain.APIKeyLabel{}
	unique := positiveIDs(ids)
	if len(unique) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(unique))
	for _, id := range unique {
		args = append(args, id)
	}
	rows, err := db.read.QueryContext(ctx,
		"SELECT id, name, key_prefix FROM api_keys WHERE id IN ("+idPlaceholders(len(unique))+")", args...)
	if err != nil {
		return nil, fmt.Errorf("store: read api key labels: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id    int64
			label domain.APIKeyLabel
		)
		if err := rows.Scan(&id, &label.Name, &label.Prefix); err != nil {
			return nil, fmt.Errorf("store: scan api key label: %w", err)
		}
		out[id] = label
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate api key labels: %w", err)
	}
	return out, nil
}

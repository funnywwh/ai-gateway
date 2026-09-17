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
       record_output_text, status, created_at, client, model, resolved_model, reasoning_effort,
       workspace, session_id, call_kind, title`

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
	// The provider filter is the one that needs the metering table: request_logs has no
	// provider column, because a request that failed over has several. The correlated EXISTS
	// keeps this a filter on requests (the same rows in the list, the count and the breakdown)
	// and point-looks-up idx_usage_request once per candidate row.
	//
	// The outer column is named through its table rather than left bare, because inside a
	// correlated subquery an unqualified request_id resolves to the subquery's own table
	// first: "pu.request_id = request_id" is a tautology that stops filtering altogether, and
	// SQLite reports nothing — the filter would answer "requests of every provider that
	// happens to have a metering row" while looking like it works.
	if f.ProviderID > 0 {
		outer := prefix + "request_id"
		if prefix == "" {
			// The single-table callers (the list page and its count) filter over request_logs
			// with no alias, which requestLogListSQL and countRows both spell that way.
			outer = "request_logs.request_id"
		}
		where += " AND EXISTS (SELECT 1 FROM usage_records pu WHERE pu.request_id = " +
			outer + " AND pu.provider_id = ?)"
		args = append(args, f.ProviderID)
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
		&rec.ResolvedModel, &rec.ReasoningEffort, &rec.Workspace, &rec.SessionID, &rec.CallKind, &rec.Title); err != nil {
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

// RequestProviders returns the upstream providers that metered each request: ascending,
// deduplicated provider ids keyed by request id. A request with no usage row (a locally
// rejected one) is simply absent, which is the same statement RequestUsages makes with
// Metered=false — "no metering row" is not "consumed nothing".
//
// It is a second query rather than a column on the page query, for the reason the money
// columns are: a request can have several attempts on several providers, so the fact is
// one-to-many and belongs to the metering table. The ids are returned, not names — a name is
// a mutable label owned by the providers table, and the caller resolves it for the rows in
// hand (ProviderNames) instead of this query joining it into every page.
func (db *DB) RequestProviders(ctx context.Context, requestIDs []string) (map[string][]int64, error) {
	out := map[string][]int64{}
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
	rows, err := db.read.QueryContext(ctx, `
SELECT request_id, provider_id FROM usage_records
WHERE request_id IN (`+idPlaceholders(len(ids))+`)
GROUP BY request_id, provider_id ORDER BY request_id, provider_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read request providers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		var providerID int64
		if err := rows.Scan(&requestID, &providerID); err != nil {
			return nil, fmt.Errorf("store: scan request provider: %w", err)
		}
		out[requestID] = append(out[requestID], providerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate request providers: %w", err)
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
		return "COUNT(DISTINCT r.request_id) DESC, group_key ASC", nil
	case "charge":
		return "COALESCE(SUM(u.charge_micros), 0) DESC, group_key ASC", nil
	default:
		return "", fmt.Errorf("store: sort must be one of %s", strings.Join(RequestLogDimensionSorts, ", "))
	}
}

// ListRequestLogDimensionsPage is the rows-only convenience wrapper. API callers use
// RequestLogDimensionsPage so rows and total are read in one snapshot.
func (db *DB) ListRequestLogDimensionsPage(ctx context.Context, f domain.RequestLogFilter, groupBy, sort string, limit, offset int) ([]domain.RequestLogDimensionRow, error) {
	page, err := db.RequestLogDimensionsPage(ctx, f, groupBy, sort, limit, offset)
	return page.Rows, err
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
//
// Two things take that shortcut away, and both are recorded in the filters rather than chosen
// here. group_by=provider takes it because the bucket key itself lives in the metering table,
// so the joined reference query answers it. A provider filter takes the index-only part of it
// because the filter is a correlated EXISTS on usage_records, which is what "which provider
// served this request" costs. Neither changes the bucket set the rows come from: the
// production count reads the attempt-grain contribution query inside the same snapshot as its
// rows (RequestLogDimensionsPage), and this path exists for the compatibility probes and the
// independent comparisons.
func (db *DB) CountRequestLogDimensionGroups(ctx context.Context, f domain.RequestLogFilter, groupBy string) (int, error) {
	expression, from, err := requestLogDimensionReference(groupBy, false)
	if err != nil {
		return 0, err
	}
	where, args := requestLogFilter("r.", f)
	var total int
	if err := db.read.QueryRowContext(ctx, requestLogDimensionCountSQL(expression, from, where), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count request log dimension groups: %w", err)
	}
	return total, nil
}

// requestLogDimensionReference resolves the bucket expression of the direct-join reference
// queries, together with the FROM clause that expression has to be grouped against. Both
// carry the r alias; the metering table is joined as u whenever the query needs it.
//
// It resolves the expression itself rather than taking one, because the provider dimension's
// key is spelled differently in the two contexts: the contribution query groups the
// already-coalesced provider_id its CTE carries, while these queries group the joined metering
// rows directly, where a request with no metering row still has to land in the provider 0
// bucket (a request that never reached an upstream was served by no provider).
//
// joinUsage is what the caller needs, not a choice: the paged reference sums tokens and money,
// so it always joins; the bucket count deliberately drops the join (the shortcut documented on
// CountRequestLogDimensionGroups) and only takes it back for a dimension whose key lives in the
// metering table.
func requestLogDimensionReference(groupBy string, joinUsage bool) (expression, from string, err error) {
	expression, err = requestLogGroupExpr(groupBy)
	if err != nil {
		return "", "", err
	}
	if providerDimension(groupBy) {
		expression = `CAST(COALESCE(u.provider_id,0) AS TEXT)`
		joinUsage = true
	}
	from = "request_logs r"
	if joinUsage {
		from += " LEFT JOIN usage_records u ON u.request_id = r.request_id"
	}
	return expression, from, nil
}

// requestLogDimensionsSQL retains the direct-join reference query for compatibility
// probes and independent correctness comparisons against the contribution query.
func requestLogDimensionsSQL(groupBy, order, where string) (string, error) {
	expression, from, err := requestLogDimensionReference(groupBy, true)
	if err != nil {
		return "", err
	}
	prefix, workspace := "", "MAX(r.workspace)"
	if groupBy == "session" {
		prefix = `WITH session_requests AS (
 SELECT r.*, FIRST_VALUE(r.workspace) OVER (
 PARTITION BY r.session_id ORDER BY (r.workspace<>'') DESC,r.created_at DESC,r.workspace DESC
 ) AS session_workspace FROM request_logs r` + where + `)`
		from, workspace, where = "session_requests r LEFT JOIN usage_records u ON u.request_id = r.request_id",
			"MAX(r.session_workspace)", ""
	}
	return prefix + fmt.Sprintf(`
SELECT %s AS group_key, COUNT(DISTINCT r.request_id), COUNT(DISTINCT u.request_id),
       MIN(r.created_at), MAX(r.created_at), MAX(r.title), `+workspace+`,
       COALESCE(SUM(`+usageTokenExpr("u.")+`), 0),
       COALESCE(SUM(COALESCE(json_extract(u.dimensions_json, '$.input_cache_hit'), 0)), 0),
       COALESCE(SUM(COALESCE(json_extract(u.dimensions_json, '$.output'), 0)), 0),
       COALESCE(SUM(COALESCE(json_extract(u.dimensions_json, '$.reasoning'), 0)), 0),
       COALESCE(SUM(u.cost_micros), 0), COALESCE(SUM(u.charge_micros), 0)
FROM `+from+where+
		` GROUP BY group_key ORDER BY `+order+` LIMIT ? OFFSET ?`, expression), nil
}

// requestLogDimensionCountSQL is the raw bucket count used by rows-only compatibility
// callers and independent comparisons. The paged API counts its selected contributions
// in RequestLogDimensionsPage instead, within the same snapshot as the returned rows. from
// is the table (and, for the provider dimension, the join) the expression is resolved against.
func requestLogDimensionCountSQL(expression, from, where string) string {
	return fmt.Sprintf(`
SELECT COUNT(*) FROM (
  SELECT %s AS group_key FROM %s%s GROUP BY group_key
)`, expression, from, where)
}

// RequestLogDimensionNames are the accepted group_by values, in the order the console and
// MCP describe them.
//
// provider is the only one of them that is not an identity column of request_logs: it is
// read from the metering rows, because one request may be served by several providers and
// each provider prices its own attempts (see providerDimension).
var RequestLogDimensionNames = []string{"client", "model", "resolved_model", "workspace", "session", "call_kind", "account", "api_key", "provider"}

// providerDimension reports whether a grouping is answered from the metering rows' provider
// instead of from the request's identity columns.
//
// It is a predicate over the group_by value rather than a property of one expression because
// the answer changes which source can be read at all: the hourly rollups hold one
// contribution per request with the metering summed across its attempts, so they cannot say
// which provider was paid — and the attempt-grain source is what can
// (docs/design/m53-request-provider-dimension.md §3).
func providerDimension(groupBy string) bool { return groupBy == "provider" }

// requestLogGroupExpr maps a group_by value onto its column.
//
// The two credential dimensions (M30) group on the id cast to text, not on the name: the
// name lives in another table, is mutable, and is not unique for api_keys. The caller
// resolves ids to names for the buckets it is about to return. The provider dimension
// follows the same rule for the same reason.
//
// The expression is written against the contributions CTE, which owns a provider_id column
// only when it was built by providerDimensionSource; grouping by provider against any other
// source is a programming error the SQLite planner will report (no such column), not a
// silently wrong answer.
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
	case "provider":
		return "CAST(r.provider_id AS TEXT)", nil
	default:
		return "", fmt.Errorf("store: group_by must be client, model, resolved_model, workspace, session, call_kind, account, api_key or provider")
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

// ProviderNames resolves provider ids to names for the request log's 供应商 (provider)
// dimension and for the provider ids on a page of rows: one batched point lookup per page,
// the same shape as AccountNames.
//
// An id with no row is simply absent from the map — a provider may be deleted while the
// usage rows it metered stay (they are billing records, not configuration), and the caller
// then shows the id rather than blanking the cell and hiding that a provider served the
// request.
func (db *DB) ProviderNames(ctx context.Context, ids []int64) (map[int64]string, error) {
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
		"SELECT id, name FROM providers WHERE id IN ("+idPlaceholders(len(unique))+")", args...)
	if err != nil {
		return nil, fmt.Errorf("store: read provider names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: scan provider name: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate provider names: %w", err)
	}
	return out, nil
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

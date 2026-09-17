package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

const dimensionKeys = `account_id, api_key_id, client, model, resolved_model, workspace, session_id, call_kind`
const dimensionValues = `title, requests, metered, first_seen, last_seen, input_tokens, cached_tokens, output_tokens, reasoning_tokens, cost_micros, charge_micros`
const dimensionColumns = dimensionKeys + ", " + dimensionValues

// dimensionProviderProjection is the key set of the attempt-grain source: the identity
// columns plus the provider, which is the only key that does not come from request_logs.
// The alias must be spelled exactly provider_id, because the aggregate groups on it through
// the CTE (requestLogGroupExpr). It requires the calling query to alias usage_records as u.
const dimensionProviderProjection = `COALESCE(u.provider_id,0) AS provider_id`

func prefixedColumns(columns, prefix string) string {
	parts := strings.Split(columns, ", ")
	for i := range parts {
		parts[i] = prefix + parts[i]
	}
	return strings.Join(parts, ", ")
}

// rawDimensionSource emits one contribution per request, regardless of attempts.
// Ranges drive the time index: excluded historical hours are never scanned.
func rawDimensionSource(where string, countOnly bool) string {
	from := ` FROM json_each(?) bounds CROSS JOIN request_logs r INDEXED BY idx_request_logs_time`
	bounds := ` AND r.created_at >= json_extract(bounds.value, '$[0]') AND r.created_at <= json_extract(bounds.value, '$[1]')`
	if countOnly {
		return `SELECT ` + prefixedColumns(dimensionKeys, "r.") + from + where + bounds
	}
	return `SELECT ` + prefixedColumns(dimensionKeys, "r.") + `, r.title, 1 AS requests,
 CASE WHEN COUNT(u.request_id)>0 THEN 1 ELSE 0 END AS metered,
 r.created_at AS first_seen, r.created_at AS last_seen,
 COALESCE(SUM(` + usageTokenExpr("u.") + `),0) AS input_tokens,
 COALESCE(SUM(json_extract(u.dimensions_json,'$.input_cache_hit')),0) AS cached_tokens,
 COALESCE(SUM(json_extract(u.dimensions_json,'$.output')),0) AS output_tokens,
 COALESCE(SUM(json_extract(u.dimensions_json,'$.reasoning')),0) AS reasoning_tokens,
 COALESCE(SUM(u.cost_micros),0) AS cost_micros, COALESCE(SUM(u.charge_micros),0) AS charge_micros` +
		from + ` LEFT JOIN usage_records u ON u.request_id=r.request_id` + where + bounds + ` GROUP BY r.id`
}

// providerDimensionSource emits one contribution per (request, provider) pair, which is the
// grain the provider dimension is a question about: a provider is named by the metering rows,
// and a request that failed over carries money on more than one of them. What a bucket then
// means is "what this provider served", so its request count is the number of requests that
// provider handled — the same request legitimately appears in two buckets, and the buckets no
// longer sum to the window's request count. That is the honest reading of "what did each
// provider cost me": attributing a failed-over request's money to the provider that finally
// answered would move one provider's cost onto another
// (docs/design/m53-request-provider-dimension.md §2).
//
// Unlike rawDimensionSource this keeps the join for the bucket count too. Its documented
// shortcut — a LEFT JOIN can neither add nor drop a left-hand row, so the bucket set is the
// same without it — does not hold here: the grouping key comes from usage_records, so
// dropping the join would collapse every provider of a request into one row. A request with
// no metering row still contributes one row, with provider 0 (the 「未计量」 bucket), because
// "never reached an upstream" is a fact about the request that the provider dimension has to
// keep rather than silently drop.
func providerDimensionSource(where string, countOnly bool) string {
	from := ` FROM json_each(?) bounds CROSS JOIN request_logs r INDEXED BY idx_request_logs_time
 LEFT JOIN usage_records u ON u.request_id=r.request_id`
	bounds := ` AND r.created_at >= json_extract(bounds.value, '$[0]') AND r.created_at <= json_extract(bounds.value, '$[1]')`
	group := ` GROUP BY r.id, COALESCE(u.provider_id,0)`
	keys := prefixedColumns(dimensionKeys, "r.") + `, ` + dimensionProviderProjection
	if countOnly {
		return `SELECT ` + keys + from + where + bounds + group
	}
	return `SELECT ` + keys + `, r.title, 1 AS requests,
 CASE WHEN COUNT(u.request_id)>0 THEN 1 ELSE 0 END AS metered,
 r.created_at AS first_seen, r.created_at AS last_seen,
 COALESCE(SUM(` + usageTokenExpr("u.") + `),0) AS input_tokens,
 COALESCE(SUM(json_extract(u.dimensions_json,'$.input_cache_hit')),0) AS cached_tokens,
 COALESCE(SUM(json_extract(u.dimensions_json,'$.output')),0) AS output_tokens,
 COALESCE(SUM(json_extract(u.dimensions_json,'$.reasoning')),0) AS reasoning_tokens,
 COALESCE(SUM(u.cost_micros),0) AS cost_micros, COALESCE(SUM(u.charge_micros),0) AS charge_micros` +
		from + where + bounds + group
}

// dimensionReadIsExact reports whether the read has to come from the raw source over the
// whole window instead of from the hourly rollups.
//
// Two things force it, and both are structural rather than a tuning choice:
//
//   - group_by=provider: a rollup row is one request with the metering already summed over
//     its attempts, so the provider information is gone by construction.
//   - a provider filter: the rollups are keyed by request_logs columns only, so they hold no
//     request_id to correlate a metering row with, and an hour that cannot be filtered cannot
//     be used for a filtered answer.
//
// The cost is a window scan for these reads; that is the price of an answer the summary
// tables cannot hold, and returning a cheaper wrong number is not an option
// (docs/design/m53-request-provider-dimension.md §3).
func dimensionReadIsExact(groupBy string, f domain.RequestLogFilter) bool {
	return providerDimension(groupBy) || f.ProviderID > 0
}

type dimensionSelection struct {
	generations []int64
	ranges      [][2]int64
}

func hourFloor(secs int64) int64 { return secs - ((secs%3600 + 3600) % 3600) }

// selectDimensionSources runs inside the same read snapshot as rows and total.
//
// exact asks for a read the rollups cannot serve (dimensionReadIsExact): the whole window is
// then handed back as one raw range, so the rows, the total and the buckets all come from the
// same attempt-grain source. Rollup eligibility is left untouched — a later read that the
// summaries can answer still uses them.
func (db *DB) selectDimensionSources(ctx context.Context, tx *sql.Tx, f domain.RequestLogFilter, now time.Time, exact bool) (dimensionSelection, error) {
	lo, hi := int64(math.MinInt64), int64(math.MaxInt64)
	if !f.From.IsZero() {
		lo = unix(f.From)
	}
	if !f.To.IsZero() {
		hi = unix(f.To)
	}
	s := dimensionSelection{}
	if lo > hi {
		return s, nil
	}
	if exact {
		s.ranges = append(s.ranges, [2]int64{lo, hi})
		return s, nil
	}
	if db.dimensionRollupsDisabled.Load() || !db.dimensionWAL || hi < math.MinInt64+3599 {
		s.ranges = append(s.ranges, [2]int64{lo, hi})
		return s, nil
	}
	// Unknown (not yet discovered by backfill) hours remain in the raw complement.
	rows, err := tx.QueryContext(ctx, `SELECT hour,published_generation FROM request_dimension_hours
 WHERE published_version=version AND published_generation IS NOT NULL
 AND hour>=? AND hour<=? AND hour<? ORDER BY hour`, lo, hi-3599, hourFloor(now.Unix()))
	if err != nil {
		return s, err
	}
	defer rows.Close()
	next := lo
	for rows.Next() {
		var h, g int64
		if err := rows.Scan(&h, &g); err != nil {
			return s, err
		}
		if next < h {
			s.ranges = append(s.ranges, [2]int64{next, h - 1})
		}
		next = h + 3600
		s.generations = append(s.generations, g)
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if next <= hi {
		s.ranges = append(s.ranges, [2]int64{next, hi})
	}
	return s, nil
}

// dimensionSourceSQL builds the contribution source. byProvider selects the attempt-grain
// provider source; the selection it is given must then hold raw ranges only, which
// selectDimensionSources guarantees for the same flag.
func dimensionSourceSQL(s dimensionSelection, f domain.RequestLogFilter, countOnly, byProvider bool) (string, []any) {
	// Bounds are enforced by the selected intervals; dimension filters apply equally.
	f.From = time.Time{}
	f.To = time.Time{}
	where, filters := requestLogFilter("r.", f)
	if byProvider {
		if len(s.ranges) == 0 {
			// Nothing matched the window bounds (the caller asked for a window that ends
			// before it starts). An empty bounds array answers it without touching either
			// table, and the filter clause has to go with it: it would otherwise keep
			// placeholders that no longer have arguments to bind.
			return providerDimensionSource(` WHERE 1 = 0`, countOnly), []any{"[]"}
		}
		encoded, _ := json.Marshal(s.ranges)
		return providerDimensionSource(where, countOnly), append([]any{string(encoded)}, filters...)
	}
	sources := []string{}
	args := []any{}
	if len(s.generations) > 0 {
		columns := dimensionColumns
		if countOnly {
			columns = dimensionKeys
		}
		sources = append(sources, `SELECT `+prefixedColumns(columns, "r.")+` FROM json_each(?) gens
 CROSS JOIN request_dimension_rollups r INDEXED BY idx_dimension_rollups_generation`+where+` AND r.generation=gens.value`)
		encoded, _ := json.Marshal(s.generations)
		args = append(args, string(encoded))
		args = append(args, filters...)
	}
	if len(s.ranges) > 0 {
		sources = append(sources, rawDimensionSource(where, countOnly))
		encoded, _ := json.Marshal(s.ranges)
		args = append(args, string(encoded))
		args = append(args, filters...)
	}
	if len(sources) == 0 {
		columns := dimensionColumns
		if countOnly {
			columns = dimensionKeys
		}
		// Empty relation with the right names, without touching original tables.
		return `SELECT ` + columns + ` FROM request_dimension_rollups WHERE 0`, args
	}
	return strings.Join(sources, " UNION ALL "), args
}

func dimensionAggregateSQL(source, expression, sortKey string) string {
	order := "MAX(r.last_seen) DESC"
	if sortKey == "requests" {
		order = "SUM(r.requests) DESC"
	}
	if sortKey == "charge" {
		order = "SUM(r.charge_micros) DESC"
	}
	workspace := "MAX(r.workspace)"
	if expression == "r.session_id" {
		// Workspace is a rollup key, so last_seen also identifies its latest
		// occurrence. Prefer a nonempty workspace; break timestamp ties stably.
		source = `SELECT c.*, FIRST_VALUE(c.workspace) OVER (
 PARTITION BY c.session_id ORDER BY (c.workspace<>'') DESC,c.last_seen DESC,c.workspace DESC
 ) AS session_workspace FROM (` + source + `) c`
		workspace = "MAX(r.session_workspace)"
	}
	return `WITH contributions AS (` + source + `) SELECT ` + expression + ` AS group_key,
 SUM(r.requests),SUM(r.metered),MIN(r.first_seen),MAX(r.last_seen),MAX(r.title),` + workspace + `,
 SUM(r.input_tokens),SUM(r.cached_tokens),SUM(r.output_tokens),SUM(r.reasoning_tokens),SUM(r.cost_micros),SUM(r.charge_micros)
 FROM contributions r GROUP BY group_key ORDER BY ` + order + `,group_key ASC LIMIT ? OFFSET ?`
}

// RequestLogDimensionsPage reads both the page and its count in one snapshot.
//
// Which source answers it is decided once, here, and the same decision feeds both queries:
// a page whose count came from a different source than its rows could report a total that
// describes buckets the caller can never page to.
func (db *DB) RequestLogDimensionsPage(ctx context.Context, f domain.RequestLogFilter, groupBy, sortKey string, limit, offset int) (domain.RequestLogDimensionPage, error) {
	out := domain.RequestLogDimensionPage{Rows: []domain.RequestLogDimensionRow{}}
	expression, err := requestLogGroupExpr(groupBy)
	if err != nil {
		return out, err
	}
	if _, err = requestLogDimensionSortExpr(sortKey); err != nil {
		return out, err
	}
	limit = normalizeLimit(limit, 20, 200)
	if offset < 0 {
		offset = 0
	}
	exact := dimensionReadIsExact(groupBy, f)
	byProvider := providerDimension(groupBy)
	tx, err := db.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	selection, err := db.selectDimensionSources(ctx, tx, f, time.Now(), exact)
	if err != nil {
		return out, err
	}
	source, args := dimensionSourceSQL(selection, f, false, byProvider)
	rows, err := tx.QueryContext(ctx, dimensionAggregateSQL(source, expression, sortKey), append(args, limit, offset)...)
	if err != nil {
		return out, fmt.Errorf("store: dimension page: %w", err)
	}
	for rows.Next() {
		var row domain.RequestLogDimensionRow
		var first, last int64
		if err := rows.Scan(&row.Key, &row.Requests, &row.Metered, &first, &last, &row.Title, &row.Workspace, &row.InputTokens, &row.CachedTokens, &row.OutputTokens, &row.ReasoningTokens, &row.CostMicros, &row.ChargeMicros); err != nil {
			rows.Close()
			return out, err
		}
		row.FirstSeen = timeFromUnix(first)
		row.LastSeen = timeFromUnix(last)
		out.Rows = append(out.Rows, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	source, args = dimensionSourceSQL(selection, f, true, byProvider)
	err = tx.QueryRowContext(ctx, `WITH contributions AS (`+source+`) SELECT COUNT(*) FROM (SELECT `+expression+` AS group_key FROM contributions r GROUP BY group_key)`, args...).Scan(&out.Total)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// SetDimensionRollupsEnabled switches query acceleration; invalidation stays active so
// a later enable cannot expose obsolete summaries.
func (db *DB) SetDimensionRollupsEnabled(enabled bool) { db.dimensionRollupsDisabled.Store(!enabled) }

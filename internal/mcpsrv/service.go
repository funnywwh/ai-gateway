// Package mcpsrv implements the MCP server: an MCP client (an LLM agent) connects
// with an MCP token, reads its own account through the query tools, and — when the
// token scope allows it — drives the management API through the administrative tool
// surface that the transport layer injects as a Backend.
package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/registry"
)

// Store is the read-only persistence subset the service needs.
type Store interface {
	GetAccount(ctx context.Context, id int64) (*domain.Account, error)
	ListLedger(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.LedgerEntry, error)
	ListUsage(ctx context.Context, accountID int64, from, to time.Time, limit int) ([]*domain.UsageRecord, error)
	GetBalance(ctx context.Context, accountID int64) (int64, error)
	GetRequestLog(ctx context.Context, requestID string) (*domain.RequestLogRecord, error)
	ListRequestLogs(ctx context.Context, f domain.RequestLogFilter, limit int) ([]*domain.RequestLogRecord, error)
	ListAPIKeys(ctx context.Context, accountID int64) ([]*domain.APIKey, error)
	UsageWindowTotals(ctx context.Context, accountID int64, from, to time.Time) (*domain.UsageTotals, error)
	UsageBreakdown(ctx context.Context, accountID int64, from, to time.Time, groupBy string) ([]domain.UsageBreakdownRow, error)
	ListUsageCounters(ctx context.Context, accountID int64, period string) ([]domain.UsageCounter, error)
	ListInvoices(ctx context.Context, accountID int64, limit int) ([]*domain.Invoice, error)
	GetInvoice(ctx context.Context, id int64) (*domain.Invoice, error)
}

// Config tunes the service.
type Config struct {
	MaxRows    int
	WindowDays int
	Currency   string
}

// Principal identifies the caller of one MCP request: which token, which
// account, and what that token is allowed to do.
type Principal struct {
	AccountID int64
	TokenID   int64
	Name      string
	Scope     string
}

// Actor is the identity recorded in audit logs and hooks. Naming the token (not
// just the account) is what makes a write traceable afterwards — including a write made by
// the console chat, which acts as a token like any other client.
func (p Principal) Actor() string {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "token"
	}
	if p.TokenID == 0 {
		return "mcp:" + name
	}
	return fmt.Sprintf("mcp:%s#%d", name, p.TokenID)
}

// AllowsAdmin reports whether the scope may see and call the administrative tools.
func (p Principal) AllowsAdmin() bool { return AllowsAdminTools(p.Scope) }

// PrincipalLookup resolves an MCP token row by id. It is what lets a caller holding no secret
// — the console chat, which presents a token an administrator already issued — learn the
// account, name and scope that token carries. It is a read-only port: issuing, mutating and
// revoking tokens stay behind the admin API.
type PrincipalLookup interface {
	GetMCPTokenByID(ctx context.Context, id int64) (*domain.MCPToken, error)
}

// Backend is the administrative tool surface. It is implemented by the transport
// layer (httpapi), because executing a management endpoint means dispatching into
// the HTTP handlers, and this package must not know how that works.
type Backend interface {
	// AdminTools lists the administrative tools visible to this principal.
	AdminTools(p Principal) []Tool
	// CallAdmin runs one administrative tool. A returned error means the call
	// never reached an endpoint (unknown name, bad arguments); a call that reached
	// an endpoint and failed comes back as ToolResult with IsError set.
	CallAdmin(ctx context.Context, p Principal, name string, args map[string]any) (ToolResult, error)
}

// ToolResult is a tool outcome whose isError flag is decided by the producer, so an
// endpoint answering 403 is reported as a failed call rather than a broken tool.
type ToolResult struct {
	Value   any
	IsError bool
}

// Service answers the read-only tools.
type Service struct {
	store Store
	reg   *registry.Registry
	cfg   Config
	now   func() time.Time
	// reservations reports the account's in-flight holds. It is injected so this
	// package does not depend on the billing package.
	reservations func(accountID int64) int64
	// backend is the optional administrative surface (nil disables it entirely).
	backend Backend
}

// New builds the service.
func New(store Store, reg *registry.Registry, cfg Config) *Service {
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = 1000
	}
	if cfg.WindowDays <= 0 {
		cfg.WindowDays = 30
	}
	if cfg.Currency == "" {
		cfg.Currency = "USD"
	}
	return &Service{store: store, reg: reg, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

// limitBound is the row cap the deployment configured (mcp.max_query_rows). The tool
// descriptions state it, because "how many rows will I get" decides whether a model can trust
// a count it just read: a cap that is not documented turns a truncated answer into a wrong one.
func (s *Service) limitBound() int {
	if s.cfg.MaxRows > 0 {
		return s.cfg.MaxRows
	}
	return 1000
}

// SetReservationReporter installs the in-flight reader used by get_dashboard.
func (s *Service) SetReservationReporter(report func(accountID int64) int64) { s.reservations = report }

// SetBackend installs the administrative tool surface. Without it the service
// serves the read-only query tools only (the stdio entry point relies on that).
func (s *Service) SetBackend(backend Backend) { s.backend = backend }

// ToolsFor lists the tools a principal may call: the eleven read-only query tools,
// plus the administrative entry points when the token scope allows them.
func (s *Service) ToolsFor(p Principal) []Tool {
	tools := s.Tools()
	if s.backend != nil && p.AllowsAdmin() {
		tools = append(tools, s.backend.AdminTools(p)...)
	}
	return tools
}

// CounterPeriod is the "YYYY-MM" rollup bucket for a time.
func CounterPeriod(at time.Time) string { return at.UTC().Format("2006-01") }

// Tool is one exposed MCP tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// queryToolNames is the closed set of read-only query tool names. The console
// chat uses it (via IsQueryTool) to tell a direct query-tool call apart from a
// management endpoint name the model abbreviated, so the read-only tools are
// passed straight to the MCP handler instead of being misrouted through
// admin_request. It is derived from queryTools below, which is the same table
// Tools() serves, so the two cannot drift apart.
var queryToolNames = queryToolNameSet()

// IsQueryTool reports whether name is one of the read-only query tools.
func IsQueryTool(name string) bool {
	_, ok := queryToolNames[name]
	return ok
}

// periodEnum is the closed set of windows every period-taking tool accepts. It is declared
// once because a tool that accepts a period but does not list the values makes the model
// guess, and a guessed window produces a confidently wrong number.
var periodEnum = []string{"today", "yesterday", "last_7_days", "last_30_days", "this_month", "last_month"}

// periodProperty is the shared description of a period parameter. The default matters: a model
// that omits period gets 7 days, and the answer must say so rather than imply "everything".
var periodProperty = map[string]any{
	"type": "string", "enum": periodEnum,
	"description": "时间窗口（UTC）：today/yesterday 是整天，last_7_days/last_30_days 是最近 7/30 天，" +
		"this_month/last_month 是自然月（last_month 为上一个完整自然月）。省略时按 last_7_days 处理。" +
		"所有窗口都会被 mcp.request_window_days 从更早一侧裁剪，因此更早的数据查不到，这是配置限制而不是没有数据",
}

// limitProperty describes a row cap. When limit is omitted the tool returns up to the
// deployment's cap (mcp.max_query_rows), so the description says so rather than implying an
// unbounded list.
func limitProperty(maxRows int, unit string) map[string]any {
	return map[string]any{
		"type": "integer", "minimum": 1, "maximum": maxRows,
		"description": fmt.Sprintf("最多返回多少%s；可省略，省略时返回本部署上限（%d）内的全部，返回体里的 count 是实际条数", unit, maxRows),
	}
}

// queryTool is one read-only tool: its name, the description an agent reads (see the four
// required parts in docs/mcp.md §4.5), and its input schema.
type queryTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// queryTools is the single declaration of the read-only surface: Tools() serves it and
// queryToolNames is derived from it. A tool added here but not dispatched in callRead (or the
// reverse) fails TestQueryToolSetIsConsistent.
//
// The descriptions are the interface documentation an agent actually reads — there is no other.
// Each one states what it answers, when to use it instead of its neighbour, the defaults and
// units of its parameters, and what comes back. See docs/mcp.md §4.5.
func (s *Service) queryTools() []queryTool {
	return []queryTool{
		{
			Name: "get_balance",
			Description: "查本令牌所属账户的余额与信用状况，没有参数（省略一切即可）。" +
				"用在「我还有多少钱」「会不会被停」这类问题上；要的是用量与花费就用 get_dashboard。" +
				"返回：account（账户名）、billing_mode（postpaid 后付/prepaid 预付）、status（active/suspended）、" +
				"currency（账本币种）、balance（余额，微单位）、credit_limit（后付授信上限）、low_balance_threshold（低余额告警阈值）。" +
				"金额字段都是微单位整数，同时给出同层 currency，不要当成元或美元。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name: "get_ledger",
			Description: "查账本流水：每一次充值、消费、调整、退款、过期。用于回答「钱花到哪去了」" +
				"「这笔充值什么时候到账」；只看汇总金额用 get_dashboard，看某次请求的详情用 get_request。" +
				"period 指定时间窗口、limit 限制条数，两者都可省略（省略 period 按 last_7_days）。" +
				"返回：period（起止时间）、currency、entries[]（created_at、kind（charge/topup/adjust/refund/expire）、" +
				"amount（正数=入账，负数=扣费，微单位）、balance（该笔之后的余额）、ref_type/ref_id（关联的请求或发票）、note）、count。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":` + periodJSON() + `,"limit":` + limitJSON(s.limitBound(), "条流水") + `}}`),
		},
		{
			Name: "get_usage_summary",
			Description: "按时间窗口汇总用量明细：上游尝试次数、各 token 维度、状态分布、按模型与按天的请求数。" +
				"用在「这段时间大致用了多少」这类粗略判断上。" +
				"注意它是「把明细读进来再累加」，行数受 mcp.max_query_rows 限制，**总量会随行数上限失真**；" +
				"要准确的总额与金额请用 get_dashboard（SQL 聚合）或 get_usage_breakdown。period 可省略，省略按 last_7_days。" +
				"返回：period、attempts（上游尝试次数，不是请求数）、tokens（维度→数量）、statuses、by_model、requests_by_day、note。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":` + periodJSON() + `}}`),
		},
		{
			Name: "list_requests",
			Description: "列出最近请求（本账户），带身份维度与内容录制标记。用于「昨天谁调了什么」「哪个 Key 在报错」，" +
				"再看单条详情要用 get_request（用这里返回的 request_id）。period 与 limit 都可省略，省略 period 按 last_7_days。" +
				"返回：requests[]（request_id、created_at、endpoint、status、client、model、resolved_model、workspace、" +
				"session_id、call_kind、api_key_id/api_key_name、input_recorded、reasoning_recorded、output_text_recorded）、count。" +
				"三个 *_recorded 说明该请求的正文是否被录制，为 false 时 get_request 会给出原因而不是内容。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":` + periodJSON() + `,"limit":` + limitJSON(s.limitBound(), "条请求") + `}}`),
		},
		{
			Name: "get_request",
			Description: "看一条请求的内容：输入文本（脱敏后）、以及按录制开关决定是否可见的思考与最终输出。" +
				"用在「这条请求到底发了什么」的追问上（先 list_requests 拿到 id）。" +
				"request_id 必填且必须来自 list_requests，不要自己编 id（没有可省略的参数）。" +
				"跨账户的 id 一律返回「找不到」，这是权限不是缺失。" +
				"返回：request_id、endpoint、status、created_at、api_key_id/api_key_name，以及 input/reasoning/output_text 三项" +
				"（各自配 input_recorded/reasoning_recorded/output_text_recorded；未录制时给出 *_unavailable_reason 说明原因，不会用空串冒充内容）。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"request_id":{"type":"string","description":"请求 id（x-request-id，形如 req_…），来自 list_requests；必填"}},"required":["request_id"]}`),
		},
		{
			Name: "get_dashboard",
			Description: "一次拿到账户概况，用于回答「最近怎么样」时优先用它：请求与失败数、错误率、token、花费(charge)、成本(cost)、" +
				"毛利、首字延迟(TTFT)均值与 P95、按模型分布、余额与在途预留。它是 SQL 聚合，不受行数上限影响。" +
				"只看单个模型/Key/每天的分组用 get_usage_breakdown；要逐笔流水用 get_ledger。period 可省略，省略按 last_7_days。" +
				"返回：period、currency、requests{attempts,failed,error_rate_bp}、tokens{input,output,total}、" +
				"money{charge,cost,margin}、latency{ttft_avg_ms,ttft_p95_ms,ttft_p95_estimated}、" +
				"balance{balance,in_flight,available}、estimated_ratio_bp（用量为估算的比例）、by_model[]。金额单位为微单位整数。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":` + periodJSON() + `}}`),
		},
		{
			Name: "get_usage_breakdown",
			Description: "按模型、Key 或天分组统计用量与金额，用于回答「哪个模型最贵」「哪把 Key 用得最多」。它是 SQL 聚合，" +
				"不受 mcp.max_query_rows 影响，因此要准确总额时用它而不是 get_usage_summary。" +
				"period 与 group_by 都可省略，省略 period 按 last_7_days、省略 group_by 按 model。" +
				"返回：period、group_by、currency、groups[]（group、requests、failed、input_tokens、output_tokens、charge、cost）、count。" +
				"group_by=key 时 group 是 api_key_id（数字），名字要对照 get_rate_limits 或 list_requests。金额为微单位整数。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"period":` + periodJSON() + `,"group_by":{"type":"string","enum":["model","key","day"],"description":"分组维度：model 对客模型（默认）/ key API Key（值为 api_key_id）/ day 每天；省略按 model"}}}`),
		},
		{
			Name: "get_rate_limits",
			Description: "查每个 API Key 配置的限额与本月已用，用于回答「这个 Key 会不会被限流」「配额还剩多少」；没有参数（省略一切即可）。要的是用量趋势而不是限额时用 get_usage_summary 或 get_usage_breakdown。" +
				"返回：period（本月 YYYY-MM）、keys[]（api_key_id、name、status、tags（Key 自有）、account_tags（账号继承）、effective_tags（实际生效并集）、configured_limits（Key 自身策略里的限额字段）、" +
				"used_this_period{requests,tokens,charge}、not_enforced[]（配了但尚未执行的字段，例如 monthly_*）、" +
				"ignored_policy_fields[]（网关不读的字段）、policy_error（策略文档解析失败时））、note。" +
				"两点口径：这里只报 Key 自身策略，tag 策略在准入时合并、此处不合并；实时滑动窗口余量是进程内状态，跨进程查不到，" +
				"所以不要用它推断「此刻还能发多少」。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name: "list_invoices",
			Description: "列出本账户的账期账单（新的在前），用于回答「上个月的账在哪」；明细用 get_invoice。limit 可省略（账单总量很小）。" +
				"返回：invoices[]（id、status、currency、period_start/period_end、total_charge、total_cost）、count。金额为微单位整数。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":` + limitJSON(s.limitBound(), "张账单") + `}}`),
		},
		{
			Name: "get_invoice",
			Description: "看一张账单的明细行（按模型、Key 或天分组），用于回答「这张账单为什么这么多」；账单列表来自 list_invoices，id 必填（没有可省略的参数）。别人的账单 id 一律返回「找不到」。" +
				"返回：id、status、currency、period_start/period_end、total_charge、total_cost，以及 lines[]" +
				"（group、group_type、requests、input_tokens、output_tokens、charge）。金额为微单位整数。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"账单 id，来自 list_invoices 的 invoices[].id；必填"}},"required":["id"]}`),
		},
		{
			Name: "get_models",
			Description: "列出本账户当前可用的对客模型及售价，用于回答「有哪些模型」「这个模型什么价」；没有参数（省略一切即可）。要改价格请看 admin_list_models / admin_update_model（需要 admin scope）。" +
				"返回：models[]（id、object、pricing（售价规则文档）、currency）、count。" +
				"currency 是该模型自己的售价币种，缺省才等于账本币种，所以不同模型的金额不要直接相加。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

// queryToolNameSet derives the closed name set from the declarations.
func queryToolNameSet() map[string]struct{} {
	tools := new(Service).queryTools()
	out := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		out[tool.Name] = struct{}{}
	}
	return out
}

// periodJSON and limitJSON render the shared parameter fragments. They exist so the eleven
// schemas state the same window and the same row cap instead of eleven near-copies; the values
// are static because a schema is served as a raw JSON document.
func periodJSON() string {
	raw, _ := json.Marshal(periodProperty)
	return string(raw)
}

func limitJSON(maxRows int, unit string) string {
	raw, _ := json.Marshal(limitProperty(maxRows, unit))
	return string(raw)
}

// Tools lists the read-only tools.
func (s *Service) Tools() []Tool {
	declared := s.queryTools()
	out := make([]Tool, 0, len(declared))
	for _, tool := range declared {
		out = append(out, Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return out
}

// Call executes one read-only tool for the authenticated account.
//
// This is the account-scoped entry point used by the stdio server and by tests: it
// has no token and therefore no administrative surface. The HTTP endpoint goes
// through CallAs with the caller's real scope.
func (s *Service) Call(ctx context.Context, accountID int64, name string, args map[string]any) (any, error) {
	result, err := s.CallAs(ctx, Principal{AccountID: accountID, Scope: ScopeQuery}, name, args)
	if err != nil {
		return nil, err
	}
	return result.Value, nil
}

// CallAs executes one tool on behalf of a principal. Read-only tools keep their
// account scope; administrative tools are delegated to the backend and are only
// reachable when the token scope allows them (an unknown tool and a forbidden one
// are reported identically, so the surface is not enumerable from a query token).
func (s *Service) CallAs(ctx context.Context, p Principal, name string, args map[string]any) (ToolResult, error) {
	if value, err, known := s.callRead(ctx, p.AccountID, name, args); known {
		return ToolResult{Value: s.withLedgerKeys(value)}, err
	}
	if s.backend != nil && p.AllowsAdmin() {
		return s.backend.CallAdmin(ctx, p, name, args)
	}
	return ToolResult{}, fmt.Errorf("unknown tool %q", name)
}

// withLedgerKeys makes every amount self-describing. Amounts are micros of the
// ledger currency (billing.currency), which is not necessarily USD, so each
// *_usd field also appears under a currency-neutral name and the *_usd alias is
// dropped when the ledger currency is something else. An agent reading
// "balance": "7.092199" with "currency": "CNY" cannot mistake it for dollars.
func (s *Service) withLedgerKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return s.ledgerKeysInMap(typed)
	case []map[string]any:
		for _, entry := range typed {
			s.ledgerKeysInMap(entry)
		}
		return typed
	case []any:
		for _, entry := range typed {
			s.withLedgerKeys(entry)
		}
		return typed
	default:
		return value
	}
}

func (s *Service) ledgerKeysInMap(payload map[string]any) map[string]any {
	for key, raw := range payload {
		if !strings.HasSuffix(key, "_usd") {
			continue
		}
		neutral := strings.TrimSuffix(key, "_usd")
		if _, exists := payload[neutral]; !exists {
			payload[neutral] = raw
		}
		if s.ledgerCurrency() != "USD" {
			delete(payload, key)
		}
	}
	return payload
}

// ledgerCurrency is the currency every amount in this service is denominated in.
func (s *Service) ledgerCurrency() string {
	if code := strings.ToUpper(strings.TrimSpace(s.cfg.Currency)); code != "" {
		return code
	}
	return "USD"
}

// callRead dispatches the account-scoped query tools; known reports whether the
// name belongs to this set at all.
func (s *Service) callRead(ctx context.Context, accountID int64, name string, args map[string]any) (any, error, bool) {
	switch name {
	case "get_balance":
		value, err := s.getBalance(ctx, accountID)
		return value, err, true
	case "get_ledger":
		value, err := s.getLedger(ctx, accountID, args)
		return value, err, true
	case "get_usage_summary":
		value, err := s.getUsageSummary(ctx, accountID, args)
		return value, err, true
	case "list_requests":
		value, err := s.listRequests(ctx, accountID, args)
		return value, err, true
	case "get_request":
		value, err := s.getRequest(ctx, accountID, args)
		return value, err, true
	case "get_dashboard":
		value, err := s.getDashboard(ctx, accountID, args)
		return value, err, true
	case "get_usage_breakdown":
		value, err := s.getUsageBreakdown(ctx, accountID, args)
		return value, err, true
	case "get_rate_limits":
		value, err := s.getRateLimits(ctx, accountID)
		return value, err, true
	case "list_invoices":
		value, err := s.listInvoices(ctx, accountID, args)
		return value, err, true
	case "get_invoice":
		value, err := s.getInvoice(ctx, accountID, args)
		return value, err, true
	case "get_models":
		value, err := s.getModels(ctx, accountID)
		return value, err, true
	default:
		return nil, nil, false
	}
}

func (s *Service) getBalance(ctx context.Context, accountID int64) (any, error) {
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	balance, err := s.store.GetBalance(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"account":          account.Name,
		"billing_mode":     string(account.BillingMode),
		"status":           account.Status,
		"currency":         s.cfg.Currency,
		"balance_micros":   balance,
		"balance_usd":      microsToUSD(balance),
		"credit_limit_usd": microsToUSD(account.CreditLimitMicros),
		"low_balance_usd":  microsToUSD(account.LowBalanceThresholdMicros),
	}, nil
}

func (s *Service) getLedger(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	limit := s.limit(args)
	rows, err := s.store.ListLedger(ctx, accountID, from, to, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"created_at":    row.CreatedAt.Format(time.RFC3339),
			"kind":          row.Kind,
			"amount_usd":    microsToUSD(row.AmountMicros),
			"amount_micros": row.AmountMicros,
			"balance_usd":   microsToUSD(row.BalanceAfterMicros),
			"ref_type":      row.RefType,
			"ref_id":        row.RefID,
			"note":          row.Note,
		})
	}
	return map[string]any{
		"period":   map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"currency": s.cfg.Currency,
		"entries":  out,
		"count":    len(out),
		"note":     "amounts are micro-USD (1e-6 USD); positive means credit",
	}, nil
}

func (s *Service) getUsageSummary(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	rows, err := s.store.ListUsage(ctx, accountID, from, to, s.cfg.MaxRows)
	if err != nil {
		return nil, err
	}
	dims := map[string]int64{}
	statuses := map[string]int{}
	models := map[string]int{}
	byDay := map[string]int{}
	for _, row := range rows {
		var d map[string]int64
		if err := json.Unmarshal([]byte(row.DimensionsJSON), &d); err == nil {
			for k, v := range d {
				dims[k] += v
			}
		}
		statuses[row.Status]++
		models[row.Model]++
		byDay[row.CreatedAt.Format("2006-01-02")]++
	}
	return map[string]any{
		"period":          map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"attempts":        len(rows),
		"tokens":          dims,
		"statuses":        statuses,
		"by_model":        models,
		"requests_by_day": byDay,
		"note":            "one row per upstream attempt; token dimensions are provider-reported when available",
	}, nil
}

func (s *Service) listRequests(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	from, to := s.period(args)
	limit := s.limit(args)
	rows, err := s.store.ListRequestLogs(ctx, domain.RequestLogFilter{AccountID: accountID, From: from, To: to}, limit)
	if err != nil {
		return nil, err
	}
	// Which of the account's keys produced this traffic is part of the answer; the ids are
	// on the row and the names come from one list per call (M30).
	keys := s.apiKeyNames(ctx, accountID)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"request_id":           row.RequestID,
			"created_at":           row.CreatedAt.Format(time.RFC3339),
			"endpoint":             row.Endpoint,
			"status":               row.Status,
			"client":               row.Client,
			"model":                row.Model,
			"resolved_model":       row.ResolvedModel,
			"workspace":            row.Workspace,
			"session_id":           row.SessionID,
			"call_kind":            row.CallKind,
			"api_key_id":           row.APIKeyID,
			"api_key_name":         keys[row.APIKeyID],
			"input_recorded":       row.RequestJSON != "",
			"reasoning_recorded":   row.ReasoningRecorded,
			"output_text_recorded": row.OutputTextRecorded,
		})
	}
	return map[string]any{"requests": out, "count": len(out)}, nil
}

// apiKeyNames maps one account's key ids to their names for the query tools.
//
// A failure to read the labels must not fail the tool: the rows are the answer, and the
// key ids alone are actionable (the caller can name a key from that id). So the map comes
// back empty and the payload carries an empty name rather than a tool error.
func (s *Service) apiKeyNames(ctx context.Context, accountID int64) map[int64]string {
	keys, err := s.store.ListAPIKeys(ctx, accountID)
	if err != nil {
		return nil
	}
	out := make(map[int64]string, len(keys))
	for _, key := range keys {
		out[key.ID] = key.Name
	}
	return out
}

func (s *Service) getRequest(ctx context.Context, accountID int64, args map[string]any) (any, error) {
	requestID, _ := args["request_id"].(string)
	if strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	row, err := s.store.GetRequestLog(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("request not found")
	}
	// Cross-account access is reported as "not found" so existence is not leaked.
	if row.AccountID != accountID {
		return nil, fmt.Errorf("request not found")
	}
	out := map[string]any{
		"request_id":   row.RequestID,
		"endpoint":     row.Endpoint,
		"status":       row.Status,
		"created_at":   row.CreatedAt.Format(time.RFC3339),
		"api_key_id":   row.APIKeyID,
		"api_key_name": s.apiKeyNames(ctx, accountID)[row.APIKeyID],
	}
	if row.RequestJSON != "" {
		out["input"] = json.RawMessage(row.RequestJSON)
		out["input_recorded"] = true
	} else {
		out["input_recorded"] = false
		out["input_unavailable_reason"] = "input recording is disabled for this API key"
	}
	if row.ReasoningRecorded && row.ResponseReasoning != "" {
		out["reasoning"] = row.ResponseReasoning
		out["reasoning_recorded"] = true
	} else {
		out["reasoning_recorded"] = false
		out["reasoning_unavailable_reason"] = "thinking text is not recorded for this API key (enable it in the admin console)"
	}
	if row.OutputTextRecorded && row.ResponseText != "" {
		out["output_text"] = row.ResponseText
		out["output_text_recorded"] = true
	} else {
		out["output_text_recorded"] = false
		out["output_text_unavailable_reason"] = "final output text is not recorded for this API key (enable it in the admin console)"
	}
	return out, nil
}

func (s *Service) getModels(ctx context.Context, accountID int64) (any, error) {
	snap := s.reg.Snapshot()
	names := make([]string, 0, len(snap.ModelByName))
	for name, model := range snap.ModelByName {
		if model.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		entry := map[string]any{"id": name, "object": "model", "currency": s.cfg.Currency}
		if raw := strings.TrimSpace(snap.ModelByName[name].SalePricingJSON); raw != "" {
			var pricing map[string]any
			if err := json.Unmarshal([]byte(raw), &pricing); err == nil {
				entry["pricing"] = pricing
			}
			// A model may be priced in a currency of its own (M22); the advertised
			// currency is then the model's, not the ledger's.
			if code, ok := pricing["currency"].(string); ok && strings.TrimSpace(code) != "" {
				entry["currency"] = strings.ToUpper(strings.TrimSpace(code))
			}
		}
		out = append(out, entry)
	}
	return map[string]any{"models": out, "count": len(out)}, nil
}

// period resolves the requested window, clamped to the configured maximum.
func (s *Service) period(args map[string]any) (time.Time, time.Time) {
	now := s.now()
	name, _ := args["period"].(string)
	var from time.Time
	switch name {
	case "today":
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "yesterday":
		day := now.AddDate(0, 0, -1)
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
			time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "last_30_days":
		from = now.AddDate(0, 0, -30)
	case "this_month":
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "last_month":
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return first.AddDate(0, -1, 0), first
	case "last_7_days", "":
		from = now.AddDate(0, 0, -7)
	default:
		from = now.AddDate(0, 0, -7)
	}
	if max := now.AddDate(0, 0, -s.cfg.WindowDays); from.Before(max) {
		from = max
	}
	return from, now
}

func (s *Service) limit(args map[string]any) int {
	limit := s.cfg.MaxRows
	if raw, ok := args["limit"]; ok {
		switch v := raw.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		}
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > s.cfg.MaxRows {
		limit = s.cfg.MaxRows
	}
	return limit
}

func microsToUSD(micros int64) float64 {
	return float64(micros) / 1_000_000
}

# MCP 查询服务（网关作为 MCP Server）

> 状态：**已实现（M6，首批 6 个工具）**。实现见 `internal/mcpsrv`（工具与 JSON-RPC）与 `internal/httpapi/mcp.go`（`POST /mcp` 与令牌鉴权）。
> 尚未实现（依赖后续里程碑）：`get_dashboard`、`get_usage_breakdown`、`list_invoices`/`get_invoice`（M11/M12 的账单与聚合查询）、`get_rate_limits`、stdio 模式 `bin/aigw mcp-serve`。

## 1. 定位与接入

- 面向外部 LLM/Agent（Claude、Cursor、自研 agent）：用它自己的账户凭据连入，查询**该账户的统计与请求内容**。
- 传输：`POST /mcp`（MCP Streamable HTTP）。
- 本地 agent 亦可使用 `bin/aigw mcp-serve`（stdio）。
- 鉴权：`Authorization: Bearer aigw_mcp_<token>`；令牌存 SHA-256 哈希 + 前缀索引（`mcp_tokens` 表），
  创建时**明文只显示一次**，支持轮换/吊销/过期。

## 2. 作用域与安全边界

- **所有工具强制 `WHERE account_id = <token.account_id>`**：跨账户查询返回空/拒绝。
- **只读**：没有任何写操作或副作用工具。
- **不泄露**：不返回成本价、上游/供应商细节、其他账户数据、其他 Key 的明文。
- 查询上限：`mcp.max_query_rows`（默认 1000）、`mcp.request_window_days`（默认 30）。
- 令牌吊销后立即 401。

## 3. 工具清单

| # | 工具 | 参数 | 返回 |
|---|---|---|---|
| 1 | `get_dashboard` | `period` | 请求数/错误率、输入/输出 token、收入(charge)、成本、毛利、TTFT 与 P95、按模型分布、估算占比、在途 |
| 2 | `get_usage_summary` | `period` | 区间汇总（分维度 token、cost、charge、请求数） |
| 3 | `get_usage_breakdown` | `period, group_by` | `group_by ∈ model\|key\|day\|tag\|tier\|window` |
| 4 | `get_balance` | — | 余额、信用额度、计费模式、状态、低水位 |
| 5 | `get_ledger` | `period, kind?, limit, cursor?` | 账本流水（充值/消费/调整/退款/过期，分页） |
| 6 | `list_invoices` / `get_invoice` | `period` / `id` | 账单与明细 |
| 7 | `get_models` | — | 该账户可用模型与**对客售价** |
| 8 | `list_requests` | `period, api_key_id?, model?, status?, limit, cursor?` | 请求列表（含 `reasoning_recorded` / `output_text_recorded` 标记） |
| 9 | `get_request` | `request_id` | 该请求**输入文本**（脱敏后）；思考文本与最终输出**各按对应开关**返回 |
| 10 | `get_rate_limits` | — | 当前限额与已用（可选） |

返回为结构化 JSON；**金额同时给出 USD 可读字符串与 micros 原始值**，并附口径说明（时间范围、聚合方式、币种）。

## 4. 内容可见性（与录制策略联动）

| 字段 | 默认 | 返回条件 |
|---|---|---|
| 输入文本 | **记录**（`record_input=full`，脱敏） | 默认可见；`record_input=off` 时返回"不可用"说明 |
| 思考文本 | **不记录**（`record_reasoning=false`） | 仅当该 Key 勾选"保存思考文本" |
| 最终输出文本 | **不记录**（`record_output_text=false`） | 仅当该 Key 勾选"保存最终输出文本" |

未录制时 `get_request` 返回 `{"reasoning_recorded":false}` / `{"output_text_recorded":false}` 及原因说明，
**不会**返回空字符串冒充内容。脱敏：始终剔除 `Authorization`/密钥字段，并按 `recording.redact_paths` 移除指定路径。

## 5. 实现要点

- 使用官方 `modelcontextprotocol/go-sdk` 的 **server** 端（`mcp.NewServer` + `AddTool`，处理 `tools/list` 与 `tools/call`）。
- 查询走**读连接池**（WAL 并发读），并优先读预聚合（`usage_counters`）+ 索引 + `LIMIT`，不做全表扫描。
- 每次调用记录 `mcp_tokens.last_used_at`，并可发 hook `mcp.call`。

## 6. 示例对话

用户对 LLM 说："我上个月花了多少钱？哪个模型最贵？"
LLM 依次调用 `get_usage_summary({period:"last_month"})` 与
`get_usage_breakdown({period:"last_month", group_by:"model"})`，再据此作答。

用户问："我昨天问过什么？" → LLM 调 `list_requests({period:"yesterday", limit:20})`，
必要时用 `get_request({request_id:…})` 取回输入文本（脱敏后）。

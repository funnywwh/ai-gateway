# MCP-2 设计：补齐只读工具与 stdio 模式

> 规格：`docs/mcp.md` §3（工具清单）。M6 已实现 6 个工具，本文补齐剩下的 4 类与本地 stdio 接入。

## 1. 补什么

| 工具 | 数据来源 | 要点 |
| --- | --- | --- |
| `get_dashboard` | `usage_records` + `usage_counters` | 请求数/错误率、输入输出 token、charge/cost/毛利、TTFT 均值与 P95、按模型分布、估算占比、在途预留 |
| `get_usage_breakdown` | `usage_records` SQL 聚合 | `group_by ∈ model\|key\|day`，每行含请求数/失败数/双列金额/双维度 token |
| `get_rate_limits` | `api_keys.policy_json` + `usage_counters` | 每个 Key 的配置限额 + 本月已用（请求数/token/金额） |
| `list_invoices` / `get_invoice` | `invoices` + `invoice_lines` | 账期、状态、总额、行明细（按 group 聚合） |

## 2. 为什么聚合放 SQL 而不是内存

M6 的 `get_usage_summary` 把行读进来再在 Go 里累加，行数被 `mcp.max_query_rows` 截断——**聚合结果会随行数上限而失真**。
本轮新增两个存储侧聚合方法，聚合在 SQLite 内完成，`LIMIT` 只约束明细行，不再影响汇总额：

- `store.UsageWindowTotals(ctx, accountID, from, to)`：单行汇总 + TTFT 样本（≤2000 条，用于 P95，越界时用 `ttft_p95_estimated=true` 标注）；
- `store.UsageBreakdown(ctx, accountID, from, to, groupBy)`：按 model/key/day 分组，返回请求数、失败数、双向 token、cost、charge。

## 3. 在途与限额的口径

- **在途**：`get_dashboard` 返回 `in_flight_micros`（当前预留总额）与 `available_micros`（余额 − 在途），口径与准入判定一致；
- **限额**：`get_rate_limits` 报**静态配置**（Key/Tag 策略里的 rpm/tpm/并发）与**本月已用**（`usage_counters` 的 requests/tokens/charge）。
  滑动窗口的实时余量属于进程内状态，MCP 是跨进程只读查询，不承诺该数字——**宁可说明口径，不给会误导的近似值**。

## 4. stdio 模式

`bin/aigw mcp-serve --config <file> --account <name>`：

- 打开数据库（只读语义、复用现有 store）、解析账户名 → 账户 id，然后从 stdin 按行读 JSON-RPC、把响应写回 stdout；
- **不做 token 鉴权**：stdio 模式由本机用户直接启动，等价于「我这台机器上的操作者」；
- 每个请求的 `tools/call` 仍强制 `account_id` 作用域，并复用与 HTTP 模式完全相同的 `mcpsrv.Service`，
  因此不存在两套实现漂移的可能；
- 只读：底层是同一个 store，写路径不暴露给任何工具。

## 5. 测试

1. 每个新工具的端到端调用（真实 store），校验金额/口径字段；
2. 跨账户不可见：用账户 B 的令牌查账户 A 的账单 → 返回空/不存在；
3. 聚合不受行数上限影响：造 5 条用量 + `max_rows=2`，汇总仍是 5 条；
4. `get_rate_limits` 读到策略里的 rpm 与本月已用计数；
5. stdio：用 `io.Pipe` 灌入 initialize/tools/list/tools/call，校验输出是合法 JSON-RPC 且作用域正确。

## 6. 实现与设计差异

1. **聚合下沉到 SQL**：`get_dashboard`/`get_usage_breakdown` 由新增的 `store.UsageWindowTotals` 与
   `store.UsageBreakdown` 在数据库内聚合，**不再受 `mcp.max_query_rows` 影响**；M6 的 `get_usage_summary`
   仍是「读明细再累加」（保留它是因为它要展示按天分布），测试里用 `max_rows=2` + 5 条用量明确验证了
   新工具不受限。
2. **TTFT P95 标注精度**：样本上限 2000 条，超出时返回 `ttft_p95_estimated=true`——宁可说明近似，不给假精确。
3. **`get_rate_limits` 只报静态配置 + 月度汇总**：实时滑动窗口余量是进程内状态，跨进程给不出准确值，
   因此在返回里写明口径而不是给一个会误导的近似数。
4. **在途预留用注入的函数**（`SetReservationReporter`）而不是让 mcpsrv 依赖 billing 包：
   保持分层（见 M15 的允许边表），main 负责把 billing 的预留表接到 MCP 上。
5. **stdio 模式不做令牌鉴权**：`aigw mcp-serve --account <name>` 由本机用户启动，等价于本机操作者；
   作用域仍由 `accountID` 强制，且**复用同一个 `mcpsrv.Service`**，HTTP 与 stdio 的工具集不可能漂移。
6. **工具数 6 → 11**：新增 `get_dashboard`、`get_usage_breakdown`、`get_rate_limits`、`list_invoices`、`get_invoice`；
   规格里的 10 个工具现已全部落地（`get_models` 与 `list_invoices` 各自独立计）。
7. **`docs/mcp.md` 里「使用官方 go-sdk」一条仍不成立**：实现是手写的 JSON-RPC 子集（M6 的既有偏差），
   规格该行已改为如实描述。
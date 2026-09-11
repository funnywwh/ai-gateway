# MCP 服务（网关作为 MCP Server）

> 状态：**已实现（M6 首批 6 个 + MCP-2 补齐 5 个 + M21 后台工具，共 11 个查询工具 + 3 个后台工具）**。
> 实现见 `internal/mcpsrv`（工具与 JSON-RPC）、`internal/httpapi/mcp.go`（`POST /mcp` 与令牌鉴权）、
> `internal/httpapi/mcp_admin.go`（后台工具桥）、`internal/httpapi/admin_routes.go`（管理面路由表）、
> `cmd/aigw mcpstdio.go`（stdio）。
> 设计：`docs/design/m6-mcp-server.md`、`docs/design/mcp2-tools.md`、`docs/design/m21-mcp-admin-tools.md`。

## 1. 定位与接入

- 面向外部 LLM/Agent（Claude、Cursor、DSH、自研 agent）：用它自己的凭据连入。
  **scope=query** 时查询该账户的统计与请求内容；**scope=admin_read/admin** 时还可以执行管理面接口。
- 传输：`POST /mcp`（MCP Streamable HTTP）。
- 本地 agent 亦可使用 `bin/aigw mcp-serve --config <cfg> --account <name>`（stdio；本机信任，不走令牌；
  **只提供 11 个只读查询工具**，见第 8 节）。
- 鉴权：`Authorization: Bearer aigw_mcp_<token>`；令牌存 SHA-256 哈希 + 前缀索引（`mcp_tokens` 表），
  创建时**明文只显示一次**，支持轮换/吊销/过期。

## 2. 令牌 scope（权限的唯一闸门）

| scope | 可见工具 | 能力 |
|---|---|---|
| `query`（默认） | 11 个查询工具 | 只能读**本账户**的数据；后台工具连 `tools/list` 都不出现 |
| `admin_read` | 11 + 3 | 后台接口**只读**（角色 viewer）：写接口返回 403 |
| `admin` | 11 + 3 | 执行**全部**后台接口（角色 admin），含删除、充值、备份恢复等 |

- 既有令牌与迁移后的旧行一律为 `query`：**升级不会给已签发的凭据加权限**。
- 签发与改权限都在控制台「MCP 令牌」页：`POST /admin/api/v1/mcp-tokens`（带 `scope`）、
  `PATCH /admin/api/v1/mcp-tokens/{id}`（改 `scope`/`status`，降级无需重签）。
- `admin_read`/`admin` 令牌等同管理员凭据（网关级，不按账户作用域），请配合最短 `expires_at` 使用。
- 部署级开关 `mcp.admin_tools: false` 可整体关停后台工具（即便令牌 scope=admin）。

## 3. 账户查询工具（11 个）

| # | 工具 | 参数 | 返回 |
|---|---|---|---|
| 1 | `get_dashboard` | `period` | 请求数/错误率、输入/输出 token、收入(charge)、成本、毛利、TTFT 与 P95、按模型分布、在途 |
| 2 | `get_usage_summary` | `period` | 区间汇总（分维度 token、请求数） |
| 3 | `get_usage_breakdown` | `period, group_by` | `group_by ∈ model\|key\|day` |
| 4 | `get_balance` | — | 余额、信用额度、计费模式、状态、低水位 |
| 5 | `get_ledger` | `period, limit` | 账本流水（充值/消费/调整/退款/过期） |
| 6 | `list_invoices` / `get_invoice` | `limit` / `id` | 账单与明细 |
| 7 | `get_models` | — | 该账户可用模型与**对客售价**（`currency` 为该模型的售价币种，缺省账本币种） |
| 8 | `list_requests` | `period, limit` | 请求列表（含录制标记） |
| 9 | `get_request` | `request_id` | 该请求**输入文本**（脱敏后）；思考与最终输出按开关返回 |
| 10 | `get_usage_breakdown` / `get_rate_limits` | — | 分组统计 / 当前限额与已用。`configured_limits` 读的是**扁平**策略字段（与实际生效路径同一解析器）；`monthly_*` 只解析不执行，会在 `not_enforced` 里列出；读不懂的字段进 `ignored_policy_fields`。标签策略在生效时合并，此处不合并 |

返回为结构化 JSON；金额同时给出可读值与 micros 原始值，并附口径说明（时间范围、聚合方式、币种）。

**金额币种（M22）**：所有账户金额都是**账本币种**（`billing.currency`）的微单位，字段名不带币种后缀（`balance`/`charge`/`cost`/`margin`/`amount`/`in_flight`/`available` …），
并在同一层给出 `currency`。历史字段名 `*_usd` 只在账本币种真的是 `USD` 时保留（兼容旧客户端）；
账本币种是 `CNY` 之类时不再输出 `*_usd`，避免把人民币金额读成美元。`get_models` 的 `currency` 是该**模型的售价币种**（缺省为账本币种）。

## 4. 后台工具：渐进披露的三个入口（scope ≠ query 时出现）

刻意**不是**"每个接口一个工具"：管理面有 86 条路由，一次性塞进客户端上下文既昂贵又难发现。
代之以三个入口，模型先看概要、再查用法、最后执行：

| 工具 | 入参 | 返回 |
|---|---|---|
| `admin_endpoints` | `filter?`（name/path/summary 子串）、`group?`（system/keys/requests/audit/accounts/models/providers/billing/backups/portal/pricing/mcp/hooks/settings）、`limit?` | `{count,total,groups,endpoints:[{name,method,path,summary,group,role,params[],query[],has_body,dangerous,tool,reason?}]}` |
| `admin_describe` | `name` 或 `names[]` | 该接口的 method/path/摘要/所需角色、路径参数与查询参数说明、**请求体 JSON Schema**、可直接照抄的 `example`、危险接口的 `confirm_reason` |
| `admin_request` | `name`、`params?`（路径参数）、`query?`（查询参数）、`body?`（JSON 对象）、`confirm?` | `{endpoint,method,path,status,ok,body|text|meta,truncated?}` |

用法（模型侧的三步）：

```jsonc
// 1) 找接口
{"name":"admin_endpoints","arguments":{"filter":"provider"}}
// 2) 查怎么用
{"name":"admin_describe","arguments":{"name":"admin_create_provider"}}
// 3) 执行（危险接口要 confirm）
{"name":"admin_request","arguments":{"name":"admin_create_provider","confirm":true,
  "body":{"name":"echo","kind":"testecho","enabled":true}}}
```

- **危险接口**（删除类、账本重建、备份恢复/删除/清理、充值/赠送冲销/兑换码、账单 issue\|void\|pay、
  设置写入、账户与 Key 状态变更、供应商凭据覆盖、密钥/MCP 令牌/门户口令签发）必须 `confirm: true`；
  否则拒绝并原文说明原因，让模型先向用户交代将要做什么。标记与原因都在路由表里，`admin_describe` 会提前给出。
- 路径参数缺失/类型不对 → 结构化报错并点名缺哪个；查询参数支持数组（展开为重复键，如 `?key=a&key=b`）。
- 注册但不暴露的 3 条：`auth/login`、`auth/logout`（Cookie 语义）、`backups/{id}/download`（二进制大文件）。
  它们仍出现在 `admin_endpoints` 里，`tool=null` 并附原因；调用会被拒并说明。
- 常见排障路径都在里面：`admin_provider_logs`（插件 stderr）、`admin_test_provider`（真实探测）、
  `admin_explain_router`（为什么这个模型不可用）、`admin_billing_invariants`、`admin_list_audit_logs`。

## 5. 执行语义、审计与安全

- **同一份 handler**：`admin_request` 在进程内直接调用控制台用的那个 handler（`admin_routes.go` 的同一张表既是
  注册来源也是工具来源），因此校验、热更新（凭据缓存失效/registry 重载/审计）与界面完全一致。
- **合成主体**：`actor = mcp:<令牌名>#<令牌id>`，角色由 scope 决定（`admin`→admin，`admin_read`→viewer）。
  handler 自己的审计行也归到这个 actor，运维能一眼看出"这是 agent 干的"。
- **审计**：每次调用写两条 `action=mcp.admin_call`（`started` 与 `ok`/`failed`），
  记录 method/path/**参数名**/body 的**键名**/status/耗时/scope —— **绝不记 body 的值**（可能含供应商凭据）。
- **hook**：可发 `mcp.call` 事件（`actor, endpoint, method, path, status, ok, duration_ms, scope, token_id`），
  同样不含 body。
- **响应上限**：`mcp.admin_max_response_bytes`（默认 262144）；超出则截断并标 `truncated: true`。
  非 JSON 响应按 `text/*` 原样返回文本，其它类型只回元数据（不把二进制塞进对话）。
- **失败即失败**：HTTP ≥400 的调用以 MCP `isError: true` 返回，正文里带原始状态码与错误体；
  端口未接线（如未配置 Prober）时同样透传 handler 自己的错误。
- 令牌吊销/过期立即 401；`query` 令牌调用后台工具与"未知工具"同样处理，避免探测管理面结构。

## 6. 内容可见性（与录制策略联动）

| 字段 | 默认 | 返回条件 |
|---|---|---|
| 输入文本 | **记录用户输入**（`record_input=user`，脱敏） | 默认可见；返回的是录制文档（用户消息 + `omitted` 计数 + `request_bytes`），系统指令、工具定义与工具输出不落正文；`record_input=off` 时返回"不可用"说明 |
| 思考文本 | **不记录**（`record_reasoning=false`） | 仅当该 Key 勾选"保存思考文本" |
| 最终输出文本 | **不记录**（`record_output_text=false`） | 仅当该 Key 勾选"保存最终输出文本" |

未录制时返回 `{"reasoning_recorded":false}` 及原因说明，**不会**用空字符串冒充内容。
脱敏：始终剔除 `Authorization`/密钥字段，并按 `recording.redact_paths` 移除指定路径。

## 7. 实现要点

- 手写 JSON-RPC 2.0 子集（`initialize`/`ping`/`tools/list`/`tools/call`），协议版本 `2025-06-18`。
- 查询走**读连接池**（WAL 并发读），优先读预聚合（`usage_counters`）+ 索引 + `LIMIT`，不做全表扫描。
- 令牌 scope 存 `mcp_tokens.scope`（迁移 0006，默认 `query`）；未知取值一律按 `query` 处理。
- 后台工具通过 `mcpsrv.Backend` 端口注入（`httpapi` 实现），`internal/mcpsrv` 不认识 HTTP 管理面，分层断言不变。
- 每次调用记录 `mcp_tokens.last_used_at`。

## 8. 已知限制

- **stdio 模式没有后台工具**：`aigw mcp-serve` 是本机信任的只读入口（11 个工具）。
  加后台能力需要给出管理员主体，并把它依赖的构造从 `cmd/aigw/main.go` 抽成共用函数，本轮不做。
- 后台工具**不按账户作用域**：`admin_read`/`admin` 令牌是网关级凭据。
- 单接口一个 MCP 工具的形态不做（有意为之，见第 4 节）。

## 9. 示例对话

用户："我上个月花了多少钱？哪个模型最贵？"
→ `get_usage_summary({period:"last_month"})` + `get_usage_breakdown({period:"last_month", group_by:"model"})`。

用户："昨天问过什么？" → `list_requests({period:"yesterday"})`，必要时 `get_request({request_id:…})`。

（scope=admin 令牌）用户："把 deepseek 供应商的权重降到 50，然后告诉我为什么 gpt-x 不可用。"
→ `admin_endpoints({filter:"provider"})` → `admin_describe({name:"admin_update_provider"})` →
`admin_request({name:"admin_update_provider", params:{id:15}, body:{weight:50}})` →
`admin_request({name:"admin_explain_router", query:{model:"gpt-x"}})`，把 `excluded` 里的原因翻译成人话。

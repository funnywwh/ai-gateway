# MCP 服务（网关作为 MCP Server）

> 状态：**已实现（M6 首批 6 个 + MCP-2 补齐 5 个 + M21 后台工具，共 11 个查询工具 + 3 个后台工具；
> M40 起工具说明统一为中文并带完整形状）**。
> 实现见 `internal/mcpsrv`（工具与 JSON-RPC）、`internal/httpapi/mcp.go`（`POST /mcp` 与令牌鉴权）、
> `internal/httpapi/mcp_admin.go`（后台工具桥）、`internal/httpapi/admin_routes.go`（管理面路由表）、
> `cmd/aigw mcpstdio.go`（stdio）。
> 设计：`docs/design/m6-mcp-server.md`、`docs/design/mcp2-tools.md`、`docs/design/m21-mcp-admin-tools.md`、
> `docs/design/m40-tool-descriptions-and-forms.md`。
> **新增工具或字段前请先读第 4.5 节「工具说明标准」。**

## 1. 定位与接入

- 面向外部 LLM/Agent（Claude、Cursor、DSH、自研 agent）：用它自己的凭据连入。
  **scope=query** 时查询该账户的统计与请求内容；**scope=admin_read/admin** 时还可以执行管理面接口。
- 传输：`POST /mcp`（MCP Streamable HTTP）。
- 本地 agent 亦可使用 `bin/aigw mcp-serve --config <cfg> --account <name>`（stdio；本机信任，不走令牌；
  **只提供 11 个只读查询工具**；后台工具可使用第 10 节的 HTTP 转发模式）。
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
| 8 | `list_requests` | `period, limit` | 请求列表（含录制标记、身份维度与 `api_key_id`/`api_key_name`） |
| 9 | `get_request` | `request_id` | 该请求**输入文本**（脱敏后）；思考与最终输出按开关返回；含它用的是哪个 Key（`api_key_id`/`api_key_name`） |
| 10 | `get_usage_breakdown` / `get_rate_limits` | — | 分组统计 / 当前限额与已用。`configured_limits` 读的是**扁平**策略字段（与实际生效路径同一解析器）；`monthly_*` 只解析不执行，会在 `not_enforced` 里列出；读不懂的字段进 `ignored_policy_fields`。标签策略在生效时合并，此处不合并 |

返回为结构化 JSON；金额同时给出可读值与 micros 原始值，并附口径说明（时间范围、聚合方式、币种）。

M40 起每条工具说明都写清了**默认值与口径**，因为"省略参数会得到什么"是这些工具最容易被误读的部分：

- `period` 一律可省略，省略按 `last_7_days`；取值集合在 11 个工具里完全一致
  （`today`/`yesterday`/`last_7_days`/`last_30_days`/`this_month`/`last_month`），
  且每个窗口都会被 `mcp.request_window_days` 从更早一侧裁剪——查不到更早的数据是配置限制，不是没有数据。
- `limit` 可省略，省略时返回本部署上限（`mcp.max_query_rows`，默认 1000）以内的行；
  返回体里的 `count` 是**实际条数**。
- **聚合口径决定数字能不能信**：`get_dashboard` 与 `get_usage_breakdown` 在 SQL 里聚合，
  不受行数上限影响；`get_usage_summary` 是"把明细读进来再累加"，**总量会随上限失真**。
  工具说明里写明了这条分工，避免模型拿被截断的汇总当总额。

**金额币种（M22）**：所有账户金额都是**账本币种**（`billing.currency`）的微单位，字段名不带币种后缀（`balance`/`charge`/`cost`/`margin`/`amount`/`in_flight`/`available` …），
并在同一层给出 `currency`。历史字段名 `*_usd` 只在账本币种真的是 `USD` 时保留（兼容旧客户端）；
账本币种是 `CNY` 之类时不再输出 `*_usd`，避免把人民币金额读成美元。`get_models` 的 `currency` 是该**模型的售价币种**（缺省为账本币种）。

## 4. 后台工具：渐进披露的三个入口（scope ≠ query 时出现）

刻意**不是**"每个接口一个工具"：管理面有 143 条路由，一次性塞进客户端上下文既昂贵又难发现。
代之以三个入口，模型先看概要、再查用法、最后执行：

| 工具 | 入参 | 返回 |
|---|---|---|
| `admin_endpoints` | `filter?`（name/path/summary 子串）、`group?`（system/keys/requests/audit/accounts/org/models/providers/billing/backups/portal/admins/pricing/mcp/hooks/settings/dshgw）、`limit?` | `{count,total,groups,endpoints:[{name,method,path,summary,group,role,params[],query[],has_body,body_fields[],dangerous,tool,reason?}]}`（`body_fields` 是 M40 新增：概览行直接给出请求体的顶层字段名） |
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
- **列表统一分页**：所有 `admin_list_*` 与其它列表型接口都接受 `limit` + `offset`，返回
  `{data,count,total,limit,offset,has_more}`（`limit` 的默认值与上限逐接口不同，`admin_describe` 会写出来；
  `offset` 必须是 >= 0 的整数，否则 400）。取下一页就是 `offset += count`，`has_more=false` 表示到底。
  账户自助查询工具 `get_ledger` / `list_requests` / `list_invoices` 不在此约定内，仍只有 `limit`。
- 注册但不暴露的接口：`auth/login`、`auth/logout`、`auth/methods`（都是浏览器 Cookie 语义）、
  `backups/{id}/download`（二进制大文件），以及整个控制台智能问答组 `chat/*`（按登录账号隔离，令牌没有这种账号）。
  它们仍出现在 `admin_endpoints` 里，`tool=null` 并附原因；调用会被拒并说明。
- 常见排障路径都在里面：`admin_provider_logs`（插件 stderr）、`admin_test_provider`（真实探测）、
  `admin_explain_router`（为什么这个模型不可用）、`admin_billing_invariants`、`admin_list_audit_logs`。
- **供应商并发上限是后台可写的**（M44）：`admin_update_provider` 的 `body.max_inflight` 设置该供应商
  **同时在途的上游调用数**（`0` = 不限，默认；路径参数传供应商 id，`admin_describe` 给出实时 schema）。
  超出并发的请求**排队等待**，等待上限是部署配置（只读，见 `admin_stats` 的
  `provider_capacity.queue_wait_s`，默认 30 秒；`0` 表示不排队、超限直接失败）；等待超时或队列已满时
  该请求按可重试失败换下一个候选，全部候选耗尽返回 HTTP 429 `provider_busy`。
  写入后用 `admin_get_provider` / `admin_list_providers` 读回：每行带 `capacity`
  （`limit`/`inflight`/`waiting`/`admitted`/`queue_full`/`timed_out`/`waited_total_ms`），
  `admin_stats.provider_capacity` 给出全部供应商的实时在途与排队；**读这些只需要 `admin_read`**，写才需要 `admin`。
- **组织架构是后台可写的**（M49，`group=org`）：`admin_list_org_nodes`（`include_accounts=true` 时每节点带成员账号；
  返回扁平列表 + `parent_id`/`depth`/`path`，模型不必自己算层级）、`admin_create_org_node`（`name` 必填，
  `parent_id` 为空即根节点）、`admin_update_org_node`（改名 / 换父 / 改标签 / 排序；移到自身或子孙、或超过
  16 层会被拒）、`admin_delete_org_node`（有子节点时必须显式传 `query.cascade=true`，会删除整棵子树，
  成员关系随之消失，**账号本身不受影响**）、`admin_set_org_node_accounts`（`body.account_ids` 整表替换该节点的成员）。
  账号侧用 `admin_update_account` 的 `body.org_node_ids` 设置该账号的归属（同样是整表替换）。
  **节点上的标签会被整棵子树继承**：挂一个带 `grants` 的标签等于给该子树下所有账号的**全部 API Key** 放权，
  所以这几条路由标记为危险接口、`admin_describe` 会给出 `confirm_reason`。

- **控制台管理员与飞书扫码登录是后台可管理的**（M66，`group=admins`）：`admin_list_admin_users` 列出每个
  管理员账号（角色 `admin`/`viewer`、状态 `pending`/`active`/`disabled`、飞书绑定、是否已发出邀请、
  是否由 `bootstrap.admin` 配置重建）；`admin_create_admin_user` 建号（`body.password` 可省略 ——
  省略即 `pending`，只能用邀请链接激活）；`admin_update_admin_user` 改角色/状态（停用会立即注销该账号
  已登录的会话）；`admin_reset_admin_password` 发一次性口令并注销会话；`admin_invite_admin_user` 生成
  **邀请链接**（链接本身就是凭据：谁先打开并完成飞书授权，谁就获得这个账号；重新生成即作废上一条）；
  `admin_unbind_admin_user_feishu` 解绑身份；`admin_delete_admin_user` 删除账号（连带它的控制台问答记录）。
  部署始终保留至少一个 `role=admin & status=active` 的账号，因此最后一名不能被降级/停用/删除（409），
  也不能删除自己或 `bootstrap.admin` 重建的那一行。**客户侧的飞书身份不是管理员身份**：
  扫码登录只按 `admin_users` 里的绑定解析，客户身份永远拿不到控制台会话。

- **多机 DSH 的节点与租户放置是后台可管理的**（M77，`group=dshgw`）：`admin_list_dshgw_nodes` 列出节点
  （名称、监听地址、状态 `pending|deploying|ready|failed|unreachable`、阶段、版本与 revision、协议版本、
  承载租户数与运行 worker 数、漂移提示）以及新租户缺省节点（`default_node`）与 dshgw 管理通道状态；
  `admin_create_dshgw_node` 登记一个节点（名称、监听地址、SSH 主机/端口/用户、私钥来源、部署目录、
  端口段与 `host_shares` 等覆盖项；**接口只回 SHA256 指纹，不回显私钥**）、`admin_update_dshgw_node` 改记录、
  `admin_delete_dshgw_node` 删除记录（默认**保留**远端数据，`body.purge=true` 才删）、
  `admin_deploy_dshgw_node` 就是**一键经 SSH 部署/升级**（异步作业：返回阶段，配 `admin_describe` 里的
  日志尾部字段轮询），另有 `admin_probe_dshgw_node`（立即探测）、`admin_reconcile_dshgw_node`
  （推送权威租户分配表并修正漂移）、`admin_rotate_dshgw_node_token`（轮换节点令牌并重新部署）。
  租户侧：`admin_list_dshgw_tenants`（分页 + `node` 过滤，含 worker 状态与 `models_pending`）、
  `admin_start_dshgw_tenant` / `admin_stop_dshgw_tenant` / `admin_restart_dshgw_tenant`、
  `admin_move_dshgw_tenant`（改放置；受守卫：租户已停 + 目标节点就绪 + 目标机已有数据）。
  危险接口的 `confirm_reason` 写明后果（升级会重启该节点上运行中的租户；停用不改数据；删除默认不碰远端）。
  细节见 [docs/dshgw.md](dshgw.md) §8。

- **客户的飞书身份绑定在账号上**（M72，`group=accounts`）：`admin_bind_account_feishu` 把一个飞书身份
  （`body.open_id` 必填，`union_id`/`name` 可选）写到某个账号上，`admin_unbind_account_feishu` 解除
  （幂等）。这个身份**就是 DSH 门户的登录身份**：一个账号一个身份，一个身份只能属于一个账号（数据库
  唯一索引；已被别的账号占用时 409），所以它是 `role=admin` 的危险接口、全量审计。
  `admin_list_accounts` 每行带 `feishu`（`{bound,open_id,name,union_id,bound_by,bound_at}`）、
  `dsh_effective`、`dsh_disabled_at`、`key_count`、`active_key_count`；`admin_set_account_dsh` 的
  `enabled=false` 会记下「管理员显式停用」（`dshgw.auto_enable` 不会撤销它），`enabled=true` 会清掉该标记。
  另外两条**已废弃**：`admin_bind_key_feishu`（Key 级绑定入口，现回答 400 `unsupported_parameter` 并指出
  替代接口）与 `admin_unbind_key_feishu`（仍可用，只用于清理升级前的历史绑定）。新绑定不要再用它们 ——
  绑定是"选人"，而人只在飞书通讯录里（`admin_list_feishu_directory`）。

### 模型级推理强度示例

模型 reasoning 是规范/对外模型级设置，所有该模型的路由共享；不是供应商级开关，也不会重启插件。先调用
`admin_describe({name:"admin_update_model"})` 获取实时 schema。路径模型名必须放进 `params`，`reasoning` 放进 `body`：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_model",
  "params":{"name":"canonical-model"},
  "body":{"reasoning":{"mode":"default","effort":"medium"}}
}}
```

`default` 只在客户端没有显式 `reasoning.effort` 时补入 `medium`（明确的 `none` 仍保留）；`force` 则覆盖客户端 effort，但保留 reasoning 的 `summary`。对象必须同时有 `mode` 与 `effort`，且没有未知字段。可写 effort 为 `none|minimal|low|medium|high|xhigh|max`；这不是上游逐模型兼容性保证，上游不支持会拒绝请求。

清空该模型覆写并恢复继承时，显式写 `null`，不是省略字段：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_model",
  "params":{"name":"canonical-model"},
  "body":{"reasoning":null}
}}
```

模型行先持久化、后重载 registry。重载失败时调用返回 500 并说明“已保存但未应用”；修复原因后重试更新。完整架构、bootstrap 与测试边界见 [`docs/design/model-reasoning.md`](design/model-reasoning.md)。

### 供应商并发上限示例

设置某供应商最多 2 个在途上游调用（`params` 是路径参数 `id`，`body` 是部分更新）：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_provider",
  "params":{"id":3},
  "body":{"max_inflight":2}
}}
```

```json
{"name":"admin_request","arguments":{
  "name":"admin_get_provider",
  "params":{"id":3}
}}
```

读回的行里 `max_inflight` 是配置值，`capacity` 是实时状态：

```json
{"max_inflight":2,
 "capacity":{"limit":2,"inflight":1,"waiting":3,"admitted":12,
             "queue_full":0,"timed_out":1,"cancelled":0,"wait_total_ms":3450}}
```

`waiting` 长非零说明这家上游在排队；`timed_out` / `queue_full` 计数上升说明排队已触界（客户端会看到 429 `provider_busy`）。
`max_inflight` 写 `0` 即取消限制（也是默认值）。等待上限是**部署级配置**（`admin_stats.provider_capacity.queue_wait_s`），
MCP 只读不可写；「谁在排队」看 `admin_stats.provider_capacity.providers`。

### 供应商成本上限与复位示例（M56）

`admin_update_provider` 的 `body.cost_limit_micros` 设置该供应商的**累计成本上限**（我们付给上游的钱，
单位 = 账本币种微单位；`0` = 不限，默认），`body.cost_period` 选统计周期（`none` / `daily` / `monthly`，
默认 `none` = 累计自上次复位）。达到上限后该供应商**从候选里被剔除**（自动换下一个候选；全部候选都超限时
客户端拿到 HTTP 503 `provider_cost_capped`），所以这是一个"会改变路由行为"的字段：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_provider",
  "params":{"id":3},
  "body":{"cost_limit_micros":50000000,"cost_period":"monthly"}
}}
```
（`50000000` 微单位 = 50 个账本币种单位。**首次**把上限从 0 改为正数时，起算点自动设为当前时刻，
不会拿历史成本把这家供应商立刻判超限。）

复位（把起算点挪到当前时刻，**不删除任何计量数据**）——`body.reset_cost=true`：

```json
{"name":"admin_request","arguments":{
  "name":"admin_update_provider",
  "params":{"id":3},
  "body":{"reset_cost":true}
}}
```

读回：`admin_get_provider` / `admin_list_providers` 的行里 `cost_limit_micros` / `cost_period` /
`cost_window_start` 是配置值，`cost` 是实时读数（有上限时才出现）：

```json
{"cost_limit_micros":50000000,"cost_period":"monthly","cost_window_start":"2026-09-01T00:00:00Z",
 "cost":{"limit_micros":50000000,"used_micros":12400000,"period":"monthly",
         "window_start":"2026-09-01T00:00:00Z","exceeded":false,"currency":"CNY"}}
```

`used_micros` **就是路由判定用的那个数**（进程内后台每 5 秒按计量表重读；`admin_stats.provider_cost`
给出 `refresh_s`/`as_of`/`last_error`/`tracked` 与每个有上限供应商的读数）。读数失败时**不阻断流量**
（保留最后一次成功读数），因此 `used_micros` 与上游侧的账单可能有几秒的时差——它是运营护栏，不是账务凭证。
**读这些只需要 `admin_read`**，写（含复位）需要 `admin`。

### Key 批量导入与归属查询示例（M80）

两个入口，都随路由表自动出现在 `admin_endpoints` 里（`group=keys`）：`admin_import_keys`（危险接口，
需要 `confirm:true` 且令牌 scope=admin）与 `admin_lookup_key`（只读，`admin_read` 就够）。

**批量导入**：每项凭据二选一——`api_key`（**明文**，网关自己算前缀与 SHA-256）或 `key_prefix`+`key_hash`
（与 `admin_import_key` 相同的迁移形式，明文不进网关）；两种形式可以混在同一批：

```json
{"name":"admin_request","arguments":{
  "name":"admin_import_keys","confirm":true,
  "body":{"keys":[
    {"name":"laptop","account":"acme","api_key":"sk-live-…"},
    {"name":"phone","account_id":4,"key_prefix":"sk-live-abcd","key_hash":"<64 位 hex>","tags":["blue"]}
  ]}
}}
```
返回：`{dry_run,total,created,updated,keys:[{index,id?,name,account_id,key_prefix,status,created,tags}]}`。
**整批原子**：任何一项校验失败或前缀被别人占用（409）就整体拒绝，错误点名 `keys[i].<字段>`
（`error.param` 形如 `"keys[1].api_key"`），一行都不写。先演练用 `"dry_run":true`：同样校验、同样报错，
但**不写库、不写审计**。上限 200 项/次。

**归属查询**：给明文或 12 字符前缀，回答"这把 key 是谁的"：

```json
{"name":"admin_request","arguments":{"name":"admin_lookup_key","body":{"api_key":"sk-live-…"}}}
```
返回：`{found,matched:"hash"|"prefix",key:{id,name,key_prefix,status,tags,account_tags,effective_tags,
created_by,expires_at,last_used_at,feishu},account:{id,name,status,tags,dsh_enabled,dsh_tenant,feishu}}`；
未命中 `{found:false,reason:"unknown_prefix"}`，明文与哈希不匹配
`{found:false,reason:"hash_mismatch"}`（**不回显命中的那一行**；要按标识查询请改用 `key_prefix`，
前缀本来就在 `admin_list_keys` 里可见）。它**不做鉴权判定**：`status`/`expires_at` 如实报告，
"能不能用"仍由数据面决定。用 POST + body 而不是 GET + query，是为了不让明文进 URL。

## 4.5 工具说明标准（新增工具/字段必读）

工具说明（`description` 与 `inputSchema` 的属性说明、`admin_describe` 的 `body_schema`/`example`）
**就是模型唯一的接口文档**。它不完整时，模型不会报错，而是**拒绝执行或猜错字段**。

**一个真实的反例**（M40 之前，见 `docs/design/m40-tool-descriptions-and-forms.md`）：
`admin_upsert_provider_model` 的 `pricing_rules` 只被标为 `{"type":"object"}`、描述只有"成本侧计价规则"，
示例是 `{}`。运维让 agent 配一次成本价，模型查完 `admin_list_models`/`admin_list_routes`/
`admin_describe` 后停下来要求管理员补文档——因为写入侧 `pricing.ParseRuleSet` 是
`DisallowUnknownFields`，**猜字段名必然 400**。它拒绝写入是正确行为。

### 查询工具（11 个）的描述四要素

缺一不可，由 `internal/mcpsrv` 的 contract 测试逐条钉住：

1. **一句话**：这个工具回答什么问题；
2. **何时用 / 与相邻工具的分工**（例如汇总要 SQL 聚合的 `get_usage_breakdown`，不要用会受
   `mcp.max_query_rows` 截断的 `get_usage_summary`；举一反三地写清"另一个工具更适合什么场景"）；
3. **参数**：`inputSchema` 里**每个**属性都要有 `description`，并写清含义、**缺省值**、单位、枚举取值；
4. **以「返回：」开头的段落**：返回体里会出现哪些字段、金额的币种与 micros 口径、以及哪些字段
   可能缺失（例如"未录制时给出原因而不是空串"）。

### 后台路由表的 body 字段标准

`internal/httpapi/admin_routes.go` 的每条 `adminField`：

| 要求 | 为什么 | 怎么满足 |
|---|---|---|
| 必须有 `Desc`，且说清**单位/取值范围/缺省行为** | 名词式描述等于没写（"成本侧计价规则"就是反例） | 直接写进 `Desc` |
| `Type: "object"` **必须带形状** | `{"type":"object"}` 让 agent 只能看到 `{}` | `schemaField(...)`，或整个 body 改用 `RawBody: objectSchema(...)` |
| 复杂对象**必须给示例** | `sampleBody` 对 object 生成 `{}`，"照抄示例"会被拒 | `exampleField(...)`，且示例必须真能被端点接受（见下） |
| 枚举用 `enumField`、路径参数用 `pathParam` | 取值只写一遍，不靠 `Desc` 复述 | 既有辅助函数 |
| `Dangerous` 必须有 `ConfirmReason` | 模型要先交代后果 | 既有测试已钉 |
| 与代码的校验语义一致 | 写入侧 `DisallowUnknownFields` 的文档要 `additionalProperties:false` | 两者写在一起 |
| **行为型字段要写清"生效语义"** | 只写单位/范围仍不够：agent 要知道写下去会发生什么 | 例：`max_inflight` 必须写明「0=不限（默认）；超出后请求排队等待，等待上限来自部署配置（默认 30s，0=不排队），超时或队列已满 → 该请求重试下一候选，全耗尽返回 429 `provider_busy`」；`cost_limit_micros` 必须写明「单位=账本微单位，0=不限（默认）；达到上限后该供应商从候选里被剔除、自动换下一个候选，全耗尽返回 503 `provider_cost_capped`；首次启用自动起算；`reset_cost` 只挪起算点、不删数据」 |
| **只存不用的字段不写进 schema** | 写进去等于教 agent 写无效配置 | 描述里标明"当前不生效" |

### 写错了会怎样

- 新增 body 字段不写 `Desc`、或 `object` 不给形状 → **`make test` 直接红**；
- `object` 不给形状在运行期也会 **panic**（构造工具元数据时，见 `adminRoute.bodySchema`），
  因此"没跑测试就上线"也藏不住；
- 失败信息统一指向本节，并说明下一步怎么做；
- 复杂对象的示例由 `TestMCPPricingExampleIsWritable` 这类**端到端**测试证明可写：
  示例先喂 `admin_validate_pricing`，再真实写库，读回比对。

**改动工具说明时同步更新**：`docs/mcp.md` 本节所描述的口径、`docs/design/m40-tool-descriptions-and-forms.md`
的差异回填，以及 `docs/PROCESS.md` 的自检项。

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
- **明文只在写入那一刻存在**（M80）：`admin_import_keys` 的 `api_key` 形式会让明文经过网关进程，但网关只写它的前 12 字符与 SHA-256 —— 明文不落库、不回显、不进审计（审计行里只有前缀，`changes` 记录的是 `credential:"plaintext"` 这个标签）。调用方那一侧无法由网关保证：明文会成为 MCP 客户端与模型上下文的一部分；在控制台智能问答里调用时，它还会按该会话绑定的 Key 的输入录制策略进入请求日志。因此**迁移/批量搬运仍推荐 `key_prefix`+`key_hash` 形式**（或单条 `admin_import_key`），`api_key` 形式留给"客户端不改 key"的自定义值场景。

## 6. 内容可见性（与录制策略联动）

| 字段 | 默认 | 返回条件 |
|---|---|---|
| 输入文本 | **记录用户输入**（`record_input=user`，脱敏） | 默认可见；返回的是录制文档（用户消息 + `omitted` 计数 + `request_bytes`），系统指令、工具定义与工具输出不落正文；`record_input=off` 时返回"不可用"说明 |
| 思考文本 | **不记录**（`record_reasoning=false`） | 仅当该 Key 勾选"保存思考文本" |
| 最终输出文本 | **不记录**（`record_output_text=false`） | 仅当该 Key 勾选"保存最终输出文本" |

未录制时返回 `{"reasoning_recorded":false}` 及原因说明，**不会**用空字符串冒充内容。
脱敏：始终剔除 `Authorization`/密钥字段，并按 `recording.redact_paths` 移除指定路径。

**输入文本的形状与上限**：`user` 档返回的文档里，每条 user 消息最多保留前
`recording.input_max_chars` 个字符（默认 100，`0` = 不限）；被截断时文档带
`input_max_chars` 与 `input_truncated=true`，消息里的非文本部分（图片等）不落库、只在 `omitted`
里计数（`over_cap` 表示因超上限未落库的文本 part）。因此**不要**把 `input` 当成提问全文：
它是有意收窄过的视图，`request_bytes` 才是这次请求的真实体积。

## 7. 实现要点

- 手写 JSON-RPC 2.0 子集（`initialize`/`ping`/`tools/list`/`tools/call`），协议版本 `2025-06-18`。
- 查询走**读连接池**（WAL 并发读），优先读预聚合（`usage_counters`）+ 索引 + `LIMIT`，不做全表扫描。
- 令牌 scope 存 `mcp_tokens.scope`（迁移 0006，默认 `query`）；未知取值一律按 `query` 处理。
- 后台工具通过 `mcpsrv.Backend` 端口注入（`httpapi` 实现），`internal/mcpsrv` 不认识 HTTP 管理面，分层断言不变。
- 每次调用记录 `mcp_tokens.last_used_at`。
- **控制台「智能问答」就是一个 MCP 客户端**：每个会话绑定一个 MCP 令牌（只存 `mcp_token_id`，
  不存明文），每次工具调用都构造 `POST /mcp` 请求交给同一个 `handleMCP`。它的权限因此没有第二套实现——
  令牌 scope 是唯一决定点，撤销令牌后未完成的会话在下一次调用立即失效。会话发起的写入在审计里记为
  `mcp:<令牌名>#<id>`，与外部 agent 完全一致。

## 8. 已知限制

- **stdio 本地账户模式只读**：后台操作使用第 10 节的令牌转发模式，依赖运行中的网关。
- 供应商**排队策略**（`routing.provider_queue_wait_s` / `routing.provider_queue_max_waiters`）是部署级配置，
  MCP 只读（`admin_stats.provider_capacity` 给出生效值）不可写；可写的是**每个供应商的并发上限**
  （`admin_update_provider` 的 `max_inflight`）。排队状态在进程内，多实例部署各自计数。
- 后台工具**不按账户作用域**：`admin_read`/`admin` 令牌是网关级凭据。
- 单接口一个 MCP 工具的形态不做（有意为之，见第 4 节）。
- `admin_import_keys` 一次最多 200 项：更大的搬运要分多次调用（每批一次事务、一份错误报告）。
- 批内**没有部分成功**：一项不合格整批拒绝。这样调用方永远不必理解"半成品"状态，代价是一把坏 key 会挡住整批。
- 控制台聊天要求 `mcp.enabled` 与 `mcp.admin_tools` 均为 `true`：它走的就是 `/mcp`，
  MCP 关掉了聊天也就没有工具可用。
- 会话只能绑定**已存在**的令牌，且只按 id 引用：明文只在签发时出现一次，聊天不持有它，
  撤销才是收回权限的手段。

## 9. 示例对话

用户："我上个月花了多少钱？哪个模型最贵？"
→ `get_usage_summary({period:"last_month"})` + `get_usage_breakdown({period:"last_month", group_by:"model"})`。

用户："昨天问过什么？" → `list_requests({period:"yesterday"})`，必要时 `get_request({request_id:…})`。

（scope=admin 令牌）用户："把 deepseek 供应商的权重降到 50，然后告诉我为什么 gpt-x 不可用。"
→ `admin_endpoints({filter:"provider"})` → `admin_describe({name:"admin_update_provider"})` →
`admin_request({name:"admin_update_provider", params:{id:15}, body:{weight:50}})` →
`admin_request({name:"admin_explain_router", query:{model:"gpt-x"}})`，把 `excluded` 里的原因翻译成人话。

## 10. stdio 后台工具转发（M42 已实现）

现有 `aigw mcp-serve --config <cfg> --account <name>` 保持本地只读查询。
新增 `aigw mcp-serve --endpoint https://gateway.example/aigw/mcp --token-env GW_MCP_TOKEN`，
从指定环境变量读取已签发的 MCP 令牌，将 stdio JSON-RPC 转发给运行中网关；不打开本地数据库。
`--endpoint` 与 `--account` 互斥。工具权限、账户隔离、吊销、危险操作 confirm、审计和热更新均由现有 HTTP 服务决定。
远端必须 HTTPS，HTTP 仅允许 loopback/localhost；禁止重定向和 URL 内嵌凭据、query、fragment。
每条输入与响应上限为 10 MiB，每次 HTTP 请求超时为 2 分钟；网络、HTTP 或协议错误以非零状态退出，不自动重试写操作。通知不产生 stdout 响应；每个有 id 的响应压成单行并立即刷新。SIGINT/SIGTERM 可中断空闲读取与在途 HTTP。仅支持本网关 JSON 响应。

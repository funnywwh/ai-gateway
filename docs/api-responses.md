# Responses API 兼容面

> 状态：**已实现（M5）**。实现见 `internal/responses`（装配器/校验）、`internal/httpapi`（路由与 SSE）、`internal/runtime`（执行）。

## 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/responses` | 主接口；`stream:true` 返回 `text/event-stream` |
| GET | `/v1/responses/{id}` | 读取已存响应（含 `output`、`usage`、`status`） |
| DELETE | `/v1/responses/{id}` | 删除存储的响应 |
| GET | `/v1/models` | 对该 Key 可见的模型（**只含对客售价**，绝不返回成本价） |
| GET | `/v1/models/{id}` | 单个模型 |
| POST | `/v1/responses/{id}/cancel` | v1 返回 400 `unsupported`（不支持 background） |
| GET | `/healthz` `/readyz` `/metrics` | 运维面（无需鉴权） |

鉴权：`Authorization: Bearer sk-gw-…`（亦兼容 `x-api-key`）。

## 认证与限速

- **凭据形式**：`sk-gw_` / `sk-gw-` 前缀的高熵随机串；服务端只存 **SHA-256 哈希 + 前 12 字符前缀**。
- **校验路径**：前缀唯一索引取行 → 常量时间比较哈希 → 校验状态/有效期 → 校验账户状态。
- **缓存**：成功结果缓存 30s（`auth.key_cache_ttl_s`）；失败结果短缓存 5s（防爆破/防打库）；
  管理面写操作后立即失效（`Invalidate`/`InvalidateAll`）。
- **账户停用**：凭据正确但账户 `suspended` → **402 `billing_hard_limit_reached`**（不是 401）。
- **限速维度**：`rpm`（请求数）、`tpm`（token，完成后结算）、`concurrency`（进行中）。
  有效限额 = Key 自身策略与**账号绑定标签 + Key 自有标签的生效并集**逐项**取最严**（账户级没有 rpm/tpm 配额；账户侧是计费授信：
  授信上限、低额阈值、在途透支上限、`inflight_policy_override`）。
- **策略形状是扁平的**：配额字段放在 Key/标签 `policy` 的**顶层**（`{"rpm":60,"concurrency":4}`）。
  写入时会校验：非对象、配额字段非数字、或出现 `PolicyFields` 之外的顶层字段（例如嵌套的
  `{"rate_limit":{...}}`）一律 400，并在错误信息里列出可接受字段——存下来却被忽略的策略比被拒绝更危险。
- **`monthly_requests` / `monthly_tokens` / `monthly_cost_micros` 已解析、未执行**：它们会被解析、会在
  `GET /keys`、控制台与 MCP `get_rate_limits`（`not_enforced`）里如实展示，但准入不检查它们。
  实现（读 `usage_counters` + 缓存 + 结算后失效）见 `docs/TODO.md`。
- **限速响应**：429，附
  `x-ratelimit-limit-requests`、`x-ratelimit-remaining-requests`、`x-ratelimit-reset-requests`、
  `x-ratelimit-limit-tokens`、`x-ratelimit-remaining-tokens`、`x-ratelimit-reset-tokens` 与 `Retry-After`。
- **计量**：只有**实际出网**的尝试才写 `usage_records`；本地拒绝（401/402/403/404/429/400 未出网）
  只写 `request_logs`、审计与 hook，**不计费**。其中**限速拒绝（本地滑动窗口 429）不写 `request_logs`**
  ——它在准入之前就返回了；计费与配额拒绝（402，`RejectAs429` 时 429）走 `rejectForQuota`，
  会写一行请求日志（同样受录制策略与脱敏约束）。**进入尝试循环之后**失败的尝试一律写一行
  `status=failed` 的用量行（`usage_source=unavailable`、零 token、**零费用**）——包括还没出网的失败
  （插件启动失败、**供应商并发排队超时/队列已满**），它们与成功尝试一样按 attempt 计数，便于在请求日志里
  看到"在谁那里失败、失败了几次"。`latency_ms` / `ttft_ms` **不含**供应商并发排队时长（M44）。
- **内容清理**：请求日志与存储响应按 `recording.retention_days` 每日清理（分批删除，见
  `docs/design/m25-log-retention.md`），也可由管理员用 `POST /admin/api/v1/requests/prune` 立即触发。
  `usage_records` / `ledger_entries` / `audit_logs` **不在清理范围内**——计费与审计历史必须保留。
  请求日志写入失败时会退化为「无正文的骨架行」（保住 request_id/状态/字节数），`/stats` 与 `/metrics`
  暴露失败与丢弃计数。
- **写入批量化（M26）**：存储响应与请求日志由后台线程成批提交（默认 250 ms / 256 请求一个事务；
  `recording.batch_writes` 可关回逐请求同步写）。审计行因此最多晚一个 flush 间隔落库，
  **硬杀进程会丢最后这个窗口的行**；SIGTERM / `scripts/local-run.sh stop` 是优雅关闭，
  会先排空队列再退出。客户端可见行为不变：POST 返回的 `id` 立刻可以
  `GET /v1/responses/{id}` 取回 —— 该 id 若仍在队列里，读路径会先等它落库（不会 404）。

## 请求字段

透传（进入供应商请求）：`model`、`input`、`instructions`、`max_output_tokens`、
`temperature`、`top_p`、`stream`、`tools`、`tool_choice`、`parallel_tool_calls`、
`previous_response_id`、`store`、`metadata`、`reasoning{effort,summary}`、`text{format}`、
`truncation`、`user`、`include`、`service_tier`、`safety_identifier`、`prompt_cache_key`。

`prompt_cache_key` 会原样进入标准供应商请求，并由 Codex 插件发送至上游；未提供或为空时不发送。
该值不使用网关会话粘性键的裁剪结果，也不保证上游一定命中缓存。

`include` 同样透传到上游（Codex 插件会原样发给订阅后端）。它不是装饰：无状态上游
（`store:false`）只有在被要求时才返回 `reasoning.encrypted_content`，而客户端续接下一轮要
靠这个加密块——丢了它，客户的下一轮会收到
`Item with id 'rs_…' not found. Items are not persisted when store is set to false.`。

**条目（item）的身份与空值都是请求的一部分**：

- 客户端显式发的空值不会被省略：`"summary":[]`、`"arguments":""`、`"output":""` 原样送进供应商请求
  （丢 `summary` 键会让 codex 订阅后端回 `Missing required parameter: 'input[N].summary'`）；
- 网关回给客户端的条目保留**上游自己的 `id`** 与上游附带的字段（`encrypted_content`、
  `phase` 等），因此客户端可以把收到的条目原样回灌。上游流式发的 delta 仍照旧逐字下发，
  完成条目按 id 就地补齐（同一 `output_index` 只出现一次完成事件）。

**未识别字段会被保留并原样透传**（前向兼容）。

明确拒绝：

| 输入 | 结果 |
|---|---|
| `background: true` | 400 `unsupported_parameter` |
| `tools[].type == "mcp"` | v1 返回 400（网关作为 MCP **client** 的能力属 v2；查询服务见 `docs/mcp.md`） |
| `max_output_tokens < 16` | 400 `invalid_request` |
| `input` 为空 | 400 `invalid_request` |

**工具类型不再被网关拒绝**：`web_search`、`file_search`、`computer_use`、`namespace` 等在 Responses
规范内合法、但目前不建模的类型，会**连原始结构一起**交给 provider 层（见 `pluginapi.Tool.Raw`），
由它按自己上游的方言翻译或不下发。理由：这些工具是可选的，而真实客户端（Codex CLI 默认就带
`web_search` 与 multi-agent `namespace`）不会为某个网关改写请求；替上游拒绝整个请求，等于把
"上游能力"误当成"客户端合法性"。代价是模型可能拿不到该工具——属**能力降级**，不是错误。

`input` 为字符串时视为单条 user message。

### 输入项类型

`message`（`input_text`/`input_image`/`input_file`/`refusal`/`output_text`）、
`function_call`、`function_call_output`、`reasoning`、`mcp_call`、`mcp_list_tools`、`item_reference`。

消息项的 `role` 可取 `user`、`assistant`、`system`、`developer`，网关**原样透传**（与其它字段一致）。
个别供应商的后端不接受其中某些角色——例如订阅型 codex 后端直接拒绝 `system`
（`400 System messages are not allowed`）——这类**后端约束由适配器负责翻译**，客户端无需为此改写请求，
也不必为不同供应商准备两套报文。参考实现见 `examples/provider-codex/README.md`。

## 模型解析与路由扩展

- 请求的 `model` 先经**自由映射**（`docs/routing` 与 `model_mappings` 表）解析为 canonical 模型，
  再按 routes 选择供应商；也支持 `model@provider_name` 与 `X-Gateway-Provider` 显式钉死。
- 网关扩展头（响应）：`x-gateway-provider`（实际使用的供应商实例）、`x-gateway-model`（解析后的 canonical 模型）、
  `x-gateway-degraded`（被剥离的字段）。
- `GET /v1/models` 的每项可带 `x-gateway-pricing`：**仅对客售价**（按当前命中规则预估）与币种；
  币种是该模型的**售价币种**（模型售价文档里的 `currency`，缺省为账本币种 `billing.currency`，默认 USD）。
  多币种下不同模型的 `currency` 可以不同，客户端按各自币种解读单价。

## 流式事件序列

每条事件一行 `event: <type>` + `data: <json>`，并带自增 `sequence_number`；**不发 `[DONE]`**；
响应头 `X-Accel-Buffering: no` 且逐帧 Flush。

索引字段按协议**恒在**：凡是作用在单个输出项上的事件都带 `output_index`（第一项就是 `0`，不省略），
项内内容事件另带 `content_index`，reasoning 摘要事件另带 `summary_index`。客户端以 `output_index`
作为自己的 item 表主键（Codex 会报 `OutputTextDelta without active item`；pi-ai 对找不到 slot 的
增量直接丢弃），省略 0 会让第一项的所有增量挂到不存在的项上。

```
response.created
response.in_progress
  ├─ (每个输出项)
  │   response.output_item.added
  │     message   : response.content_part.added
  │                 response.output_text.delta * N
  │                 response.output_text.done
  │                 response.content_part.done
  │     reasoning : response.reasoning_summary_part.added
  │                 response.reasoning_summary_text.delta * N
  │                 response.reasoning_summary_text.done
  │                 response.reasoning_summary_part.done
  │     function  : response.function_call_arguments.delta * N
  │                 response.function_call_arguments.done
  │   response.output_item.done
response.completed      (含完整 response 与 usage)
```

异常终止：

| 事件 | 场景 |
|---|---|
| `response.failed` | 上游错误 / 中途失败 / **流被切断** / 额度中断（此前先发 `event: error`） |
| `response.incomplete` | 上游声明答案被截断（`length`→`max_output_tokens`、`content_filter`） |
| `event: error` | 流内错误（`type`/`code` 与错误封装一致） |

**截断与切流的区分**（决定客户端看到 `completed` 还是别的终态）：

| 上游实际发生的事 | 客户端看到 | 判据 |
|---|---|---|
| 模型自己说完 | `response.completed` | provider 的终态原因 = `stop` |
| 达到 token 上限 / 被内容过滤 | `response.incomplete` + `incomplete_details.reason` | 终态原因 = `length`/`content_filter`/`incomplete` |
| 上游连接中途断开（没有终态） | `response.failed`（`error.message` 含 `upstream_stream_incomplete`） | provider 一个终态都没给 |

第三种尤其重要：客户端（Codex、DSH/pi-ai 等）是**从终态反推自己的 stop reason 的**
（`completed` → `stop`），所以把切流报成 `completed` 会让它把半句话当作模型的最终答复，任务就此
"无声中断"。因此这条路径宁可失败：客户端已经收到增量时不做故障切换（会重复/矛盾），直接以
`response.failed` 收尾；一个增量的没有，还能切换候选。
`usage_records.terminated_reason` 同步记录 `incomplete`（`status` 仍为 `completed`，因为上游确实产出了这些 token）。

**额度中断**（详见 `docs/billing.md`）：`event: error{type:"insufficient_quota", code:"billing_hard_limit_reached"}`
后接 `response.failed`；已产出的部分内容保留，`terminated_reason=aborted_quota`。

`GET /v1/responses/{id}` 返回的 `output` 与流式聚合结果**逐字段一致**（共用同一个 responseAssembler）。

## 会话续接

`previous_response_id`：读取存储的响应（须属于同一 Key/账户，否则 404），把其 `input + output` 作为上下文前缀；
`instructions` 缺省时继承。`store:false` 且无续接时不落库。保留期由 `recording.retention_days` 决定
（默认 30 天，写入 `responses.expires_at`，由每日清理任务删除）；把它设为 `0` 表示**不清理**，此时
`expires_at` 为 NULL，存储的响应永久可取回。

## 错误封装

```json
{"error":{"message":"...","type":"invalid_request_error","param":"input","code":"invalid_request"}}
```

| HTTP | type / code |
|---|---|
| 400 | `invalid_request_error` / `invalid_request`、`unsupported_parameter` |
| 401 | `authentication_error` / `invalid_api_key` |
| 403 | `permission_error` / `permission_denied`（模型或供应商未授权） |
| 404 | `not_found_error` / `model_not_found` |
| 429 | `rate_limit_error` / `rate_limit_exceeded`（带 `Retry-After` 与 `x-ratelimit-*`） |
| 429 | `rate_limit_error` / `provider_busy`（**供应商并发上限**：排队超时或队列已满；带 `Retry-After`，**不带** `x-ratelimit-*`——它与你的 Key/标签配额无关，见 `docs/routing.md` §4.5） |
| 402 | `rate_limit_error` / `billing_hard_limit_reached`（余额/信用额度不足或账户停用） |
| 502/504 | `api_error` / `upstream_error`、`upstream_timeout` |

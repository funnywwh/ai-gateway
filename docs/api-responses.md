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
  有效限额 = key / 标签 / 账户三处**取最严**。
- **限速响应**：429，附
  `x-ratelimit-limit-requests`、`x-ratelimit-remaining-requests`、`x-ratelimit-reset-requests`、
  `x-ratelimit-limit-tokens`、`x-ratelimit-remaining-tokens`、`x-ratelimit-reset-tokens` 与 `Retry-After`。
- **计量**：只有**实际出网**的尝试才写 `usage_records`；本地拒绝（401/402/403/404/429/400 未出网）
  只写 `request_logs`、审计与 hook，**不计费**。

## 请求字段

透传（进入供应商请求）：`model`、`input`、`instructions`、`max_output_tokens`、
`temperature`、`top_p`、`stream`、`tools`、`tool_choice`、`parallel_tool_calls`、
`previous_response_id`、`store`、`metadata`、`reasoning{effort,summary}`、`text{format}`、
`truncation`、`user`、`include`、`service_tier`、`safety_identifier`、`prompt_cache_key`。

**未识别字段会被保留并原样透传**（前向兼容）。

明确拒绝：

| 输入 | 结果 |
|---|---|
| `background: true` | 400 `unsupported_parameter` |
| `tools[].type` ∈ web_search / file_search / computer_use / code_interpreter / image_generation | 400 `unsupported_parameter` |
| `tools[].type == "mcp"` | v1 返回 400（网关作为 MCP **client** 的能力属 v2；查询服务见 `docs/mcp.md`） |
| `max_output_tokens < 16` | 400 `invalid_request` |
| `input` 为空 | 400 `invalid_request` |

`input` 为字符串时视为单条 user message。

### 输入项类型

`message`（`input_text`/`input_image`/`input_file`/`refusal`/`output_text`）、
`function_call`、`function_call_output`、`reasoning`、`mcp_call`、`mcp_list_tools`、`item_reference`。

## 模型解析与路由扩展

- 请求的 `model` 先经**自由映射**（`docs/routing` 与 `model_mappings` 表）解析为 canonical 模型，
  再按 routes 选择供应商；也支持 `model@provider_name` 与 `X-Gateway-Provider` 显式钉死。
- 网关扩展头（响应）：`x-gateway-provider`（实际使用的供应商实例）、`x-gateway-model`（解析后的 canonical 模型）、
  `x-gateway-degraded`（被剥离的字段）。
- `GET /v1/models` 的每项可带 `x-gateway-pricing`：**仅对客售价**（按当前命中规则预估）与币种。

## 流式事件序列

每条事件一行 `event: <type>` + `data: <json>`，并带自增 `sequence_number`；**不发 `[DONE]`**；
响应头 `X-Accel-Buffering: no` 且逐帧 Flush。

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
| `response.failed` | 上游错误 / 中途失败 / 额度中断（此前先发 `event: error`） |
| `response.incomplete` | 达到 `max_output_tokens` 等正常截断 |
| `event: error` | 流内错误（`type`/`code` 与错误封装一致） |

**额度中断**（详见 `docs/billing.md`）：`event: error{type:"insufficient_quota", code:"billing_hard_limit_reached"}`
后接 `response.failed`；已产出的部分内容保留，`terminated_reason=aborted_quota`。

`GET /v1/responses/{id}` 返回的 `output` 与流式聚合结果**逐字段一致**（共用同一个 responseAssembler）。

## 会话续接

`previous_response_id`：读取存储的响应（须属于同一 Key/账户，否则 404），把其 `input + output` 作为上下文前缀；
`instructions` 缺省时继承。`store:false` 且无续接时不落库。默认保留 30 天。

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
| 402 | `rate_limit_error` / `billing_hard_limit_reached`（余额/信用额度不足或账户停用） |
| 502/504 | `api_error` / `upstream_error`、`upstream_timeout` |

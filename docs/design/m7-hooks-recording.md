# M7 设计文档：Hooks 分发与请求内容查询

## 目标
把"请求生命周期事件"异步推送给外部系统（Webhook / JSONL），并让运维能按账户、Key、时间查询
录制下来的请求与响应内容——**两者都绝不阻塞请求路径**。

## 关键决策
1. **有界队列 + worker 池**：`Emit` 只做"序列化 + 入队"，队列满即**丢弃并计数**（metric + 日志），
   绝不阻塞网关请求。录制与 hook 都是"可丢的观测数据"，与计费（durable + 幂等）严格解耦。
2. **Webhook 签名**：`X-AIGW-Signature: t=<unix>,v1=<hex>`，其中
   `v1 = HMAC-SHA256(secret, "<t>.<body>")`；同时带 `X-AIGW-Event` 与 `X-AIGW-Delivery`（幂等键）。
3. **重试与死信**：HTTP 失败按指数退避重试（默认 5 次），最终失败**追加到死信 JSONL**（含事件名、
   投递 ID、目标、错误、时间）供人工重放；JSONL 类型不做重试（本地文件写入失败即记为失败）。
4. **配置可热更新**：`SetHooks` 原子替换 hook 列表（管理面写操作后调用），进行中的投递不受影响。
5. **采样与事件过滤**：`events_json`（空=全部）与 `sample_rate`（0–1）在**入队前**判定，
   避免为被过滤的事件做序列化。
6. **信封字段固定**：`{id, event, ts, request_id, account, api_key, model, provider, status,
   latency_ms, ttft_ms, usage, cost_micros?, charge_micros?, input?, output?}`；
   `include_content` 为真时才附带内容，并同样受脱敏与 `max_bytes` 约束。
7. **v1 不阻断请求**：hook 不能拒绝或修改请求（仅观测）；预留后续"gate hook"扩展点。
8. **查询 API**：管理面按账户/Key/时间/状态查询 `request_logs` 与单条详情；
   MCP 侧已提供账户作用域的 `list_requests`/`get_request`（M6）。

## 事件清单（v1）
`request.received`、`request.denied`、`response.completed`、`response.failed`、`stream.aborted`、
`provider.attempt`、`provider.circuit_open`、`provider.quota_cooldown`、`apikey.created|disabled|rotated`、
`apikey.record_output_changed`、`config.reloaded`、`backup.completed|failed`（后续里程碑接入）。

## 接口（M7 产出）
- `internal/hook.Dispatcher`：`Emit(ctx, *domain.Event)`、`SetHooks([]*domain.Hook)`、
  `Stats() Stats`（queued/delivered/failed/dropped）、`Close()`。
- `store.ListHooks/UpsertHook`：hook 配置持久化（`hooks` 表）。
- `httpapi.Deps.Hooks`：请求路径在完成/失败/拒绝时发出事件。

## 数据流
```
handler 完成响应 → hook.Emit(response.completed, payload)
   └─ 事件过滤/采样 → 序列化 → 入队（满则丢弃并计数）
        └─ worker：POST url（HMAC 签名）→ 2xx 成功 / 失败退避重试 → 死信 JSONL
```

## 异常与边界
- 队列满：丢弃 + `dropped` 计数 + 采样日志（不 panic、不阻塞）。
- 目标返回 4xx：不重试（配置错误，重试无意义）；5xx/超时/连接错误：重试。
- 死信文件不可写：记录错误日志，不影响其它 hook。
- `Close()`：等待在途投递完成（有上限），然后退出 worker。

## 测试策略
- Webhook：2xx 成功、HMAC 签名可校验、事件头正确；5xx → 重试后进死信；4xx → 不重试。
- JSONL：按行追加且可解析。
- 队列：小队列 + 慢目标 → 丢弃计数增加且 `Emit` 不阻塞。
- 过滤：事件白名单与采样率生效（不匹配则不投递）。
- 热更新：`SetHooks` 后新事件走新配置。

## 依赖
标准库 + `internal/{domain,store,logx}`。

## 实现与设计差异
- **签名格式实现细节**：`Signature(secret, ts, body) = "t=<ts>,v1=<hmac(secret, ts + '.' + body)>"`；
  解析时按 `,` 切分并匹配 `t=`（2 字符）/ `v1=`（3 字符）前缀——首版误写成 3 字符前缀导致校验失败，
  已修复并由测试锁定。
- **4xx 不重试**：目标明确拒绝（配置错误/鉴权失败）时重试没有意义，直接进死信；只有 5xx/超时/连接错误才退避重试。
- **JSONL 投递不做重试**：本地追加失败即记 failed + 死信，避免无意义的循环。
- **`include_content` 在信封层过滤**：内容字段在序列化前就被丢弃或按 `max_bytes` 截断，
  避免"先发送再丢弃"造成泄露风险。
- **本轮只接入 completion/failed 两类事件**：其余事件（provider.attempt、apikey.*、backup.*）
  依赖对应里程碑的状态位，避免为凑数量而发出信息量不足的事件。
- **管理面请求日志查询**并入 M8（与其它管理端点一起实现会话鉴权与分页）。

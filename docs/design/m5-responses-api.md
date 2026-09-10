# M5 设计文档：Responses API HTTP 层与执行运行时

## 目标
把前面各层拼成可用的对外接口：`POST /v1/responses`（非流式 + SSE 全事件序列）、
`GET/DELETE /v1/responses/{id}`、`GET /v1/models`，并把"鉴权→授权→限速→路由→执行→计量→录制"
串成一次请求的完整链路。

## 关键决策

1. **两个新包 + 一个运行时**：
   - `internal/creds`：AES-GCM 加解密供应商凭据（密钥来自 `credentials_key`，AAD=provider id）。
   - `internal/runtime`：**执行器**——按 provider.kind 分派到"内置进程内实现"或"插件子进程"，
     并负责熔断/在途计数/延迟观测/额度冷却持久化。
   - `internal/responses`：canonical 请求解析与校验、事件装配器（`responseAssembler`）、存储。
   - `internal/httpapi`：路由、中间件、错误封装、SSE 写出。
2. **执行器与路由解耦**：路由（M3）只产出**有序候选**；执行器只负责"按候选跑一次尝试"。
   调用方（handler）负责按 retryable 决定是否尝试下一个候选——这样重试策略集中在一处。
3. **内置与插件同构**：`providers.Build`（内置）与 `pluginhost`（子进程）都实现同一个调用形态
   （`Complete` / `Stream`），执行器只按 `kind` 前缀分派（`plugin:` vs 内置）。
4. **错误分类驱动重试**：`retryable` → 尝试下一候选；`quota_exhausted` → 冷却该 route 至 `reset_at`
   并**持久化** `routes.cooldown_until`；`fatal` → 直接返回客户端。
5. **流式"绝不跨候选降级"**：一旦向客户端写出首个事件（`response.created` 之后的任何 delta），
   后续失败只能以 `event: error` + `response.failed` 收尾，不再切换供应商。
6. **事件序列由装配器统一产生**：非流式响应与流式事件**共用同一个 assembler**，
   保证 `GET /v1/responses/{id}` 的 `output` 与流式聚合结果**逐字段一致**。
7. **sequence_number 自增**：每个 SSE 事件带单调递增序号，从 1 开始。
8. **录制三通道分离**：`request_json`（输入，默认记录、脱敏）、`response_reasoning`（思考，默认不记）、
   `response_text`（最终输出，默认不记）；各自开关独立，未录制时置空并写 `recorded=false`。
9. **计量粒度=尝试**：每个候选尝试写一行 `usage_records`；最终成功的那次计费（M11 接入）。
10. **限速前置**：`Reserve` 在任何出网之前；本地拒绝（401/402/403/404/429/400）**不写 usage_records**。
11. **模型目录**：`GET /v1/models` 只返回**该 Key 可解析且有可用候选**的模型，且
    `x-gateway-pricing` **仅含对客售价**（成本价仅管理面可见）。

## 接口（M5 产出）
- `internal/creds`：`Encrypt(key []byte, providerID int64, plaintext []byte) ([]byte, error)`、`Decrypt`、
  `DeriveKey(passphrase string) []byte`。
- `internal/runtime.Dispatcher`：
  `Stream(ctx, providerID, req, emit) (*pluginapi.StreamEnd, error)`、
  `Complete(ctx, providerID, req) (*pluginapi.Response, error)`、
  `Cooldown(routeID, until)`、`Snapshot()`（运行时状态）。
- `internal/responses`：
  `Parse(raw []byte) (*Request, *domain.APIError)`（校验）、`ToProviderRequest`、
  `Assembler`（`Created/OutputItem/TextDelta/Done/Completed/Failed`）、`Store`（put/get/delete）。
- `internal/httpapi.Server`：`Handler() http.Handler`；中间件：request id、鉴权、限速、错误封装、日志。

## 数据流（一次流式请求）
```
POST /v1/responses
 └─ 中间件：request_id → apikey.Verify → quota.Reserve(scope=key:<id>)
     └─ handler：responses.Parse → routing.Plan → 候选列表
         └─ for each candidate (attempt 1..N):
              dispatcher.Stream(providerID, req, emit)
                 ├─ 首个 delta 之前失败 && retryable → 下一候选
                 ├─ 首个 delta 之后失败 → event:error + response.failed（不切换）
                 └─ 成功 → assembler 产出 response.completed
         └─ 收尾：meter.Record(attempt) → recorder.Record(三通道) → ticket.Settle(tokens) → Release
```

## 异常与边界
- `background:true`、托管工具类型、`max_output_tokens<16`、空 `input` → 400（校验层，未出网）。
- 无候选：403（未授权）/ 400（能力不足）/ 502（熔断/冷却全占）。
- 客户端断连（ctx 取消）→ 取消上游（`provider.cancel`），`terminated_reason=client_cancelled`，
  仍写用量（已发生的部分）与录制。
- SSE 写出失败（客户端断开）→ 不再继续写，直接收尾释放资源。
- `previous_response_id` 不属于同一 Key/账户 → 404。

## 测试策略
- 校验层：字段边界与不允许项逐条断言（不出网）。
- 装配器：非流式与流式事件序列一致性；sequence_number 单调；失败/截断状态映射。
- 端到端（`httptest` + 内置 testecho）：401、429、非流式 200、流式完整事件序列、
  `GET/DELETE /v1/responses/{id}`、`GET /v1/models` 仅售价、录制三通道开关生效。
- 运行时：内置与插件分派、错误分类→冷却持久化、首字节后不降级。

## 依赖
标准库 + `internal/{domain,store,registry,routing,balancer,apikey,quota,usage,providers,pluginhost,creds,ids,logx}` + `pkg/pluginapi`。

## 实现与设计差异
- **能力特征集收敛为 5 项**：实际参与路由过滤的是 `stream/tools/parallel_tools/reasoning/json_schema`；
  `instructions/max_output_tokens/metadata` 属于字段级差异，由 `degradation` 在适配器层处理，
  不参与候选过滤（否则几乎每个候选都会被标成 degraded，噪声过大）。
- **装配器承担"首个字节"信号**：`Assembler.Deltas()` 同时用于两个判断——流式不再降级的门槛，
  以及 TTFT 的采样点。
- **失败尝试也计量**：`recordAttempt` 在每次候选尝试后调用（成功或失败），
  失败行带 `status=failed` 与 `terminated_reason=upstream_error|aborted_quota`。
- **额度冷却双层落地**：内存（balancer 冷却表，立即生效）+ `routes.cooldown_until`（持久化，重启不复活）。
- **`GET /v1/models` 走完整候选校验**：只有"已授权且存在可用候选"的模型才会出现在列表里，
  避免把不可用模型暴露给调用方。
- **录制开关读取缓存中的 key**：管理面写操作后必须失效鉴权缓存（测试里显式验证了这一点），
  与 M8 的管理端点行为一致。
- **脱敏实现**：解析 JSON 后按敏感键名（api_key/token/authorization/...）与配置路径删除，
  解析失败时原样保留（宁可记录也不破坏内容）。

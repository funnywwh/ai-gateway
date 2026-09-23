# M23 设计文档：请求日志默认只保留用户输入 + 配额口径收口

> 状态：已实现（本文件在实现前已在对话中输出并通过评审）。
> 规格同步：`docs/api-responses.md`（限速维度）、`docs/mcp.md`（内容可见性、限额工具）、
> `config.example.yaml`（`recording.record_input`）、`docs/TODO.md`（M23 清单）。
> **后续修订**：`user` 档不再原样保留 user 消息——M81 起先收成「每条消息前 100 字符」，M82 再收成
> 「只保留最新一条有文本、且不超过 `recording.input_max_chars`（默认 100）的 user 消息的**纯文本**」；
> 非文本 part、系统指令、工具内容一概不落库，连计数也不留。要保留全部就把该 Key 切 `full`。
> 见 `docs/design/m82-user-input-tail-only-text.md`。

## 目标

两件事同源：**「后台能存什么」必须与「后台真正读什么」一致**。

1. **录制**：请求日志的输入通道从「整份客户端请求体」收窄为「用户自己写的输入」，并定为默认口径；
   思考文本与最终输出文本仍默认不落。
2. **配额**：把控制台/管理面能保存的配额字段，与实际生效的字段对齐——写入时就拒绝读不懂的形状，
   把「已解析但未执行」的字段如实标注出来。

验收标准：

- 默认（`recording.record_input: user` 或 Key 为 `inherit`）下，`request_logs.request_json` 只含
  `input` 里 `type=message && role=user` 的条目 + `omitted` 计数 + `request_bytes`；系统/开发者指令、
  工具定义、`function_call`/`function_call_output`（真实流量里是整份文件内容）、历史 assistant 轮次
  只留计数。需要整份正文时在 Key 上切 `full`。
- 所有落库路径（正常完成、失败、本地拒绝）走同一策略，且都脱敏。
- 写 Key/标签策略时，不认识的顶层字段当场 400 并列出可接受字段；`rpm/tpm/concurrency` 生效，
  `monthly_*` 明确标注为未执行。
- Key 的配额建完能改；控制台能看能改；MCP `get_rate_limits` 报的字段与实际生效的是同一套形状。

## 关键决策

1. **「用户输入」= `input` 里 `type=message, role=user` 的条目**（字符串简写已被 `Items()` 合成为一条
   user 消息）。developer/system 指令、assistant 轮次、工具调用与工具输出、reasoning 条目都算
   「非用户输入」。理由：真实 agent 请求里用户消息只占极小一部分（抽样：67 个条目中 2 条），
   其余是工具定义与文件内容；把它们默认落库等于给每台机器再存一份读过的文件。
2. **四档语义，`user` 为默认**（`full` / `user` / `metadata` / `off`）。`full` 保留，因为上游 400 的排障
   确实需要客户端原样的字节（M19d/M19e 的教训），但它不再是默认，而是一次点击的显式选择。
3. **`metadata` 修正为「不落正文」**：它此前与 `full` 等价（`if inputMode != "off"` 就落整份正文），
   模式名与实际行为矛盾。现在它只保留体积与状态等列内元数据，`off` 才是什么都不留。这是有意的
   **行为变更**：把 `record_input` 写成 `metadata` 的部署语义从「整份正文」变为「只记元数据」。
4. **记录口径只影响记录，不影响转发**：上游仍收到完整请求体。
5. **配额规范形状 = 扁平顶层**。生效路径（`quota.LimitsFromPolicy`）只读顶层字段，控制台却示例了
   `{"rate_limit":{"rpm":60}}`；选择让**生效路径的形状成为唯一形状**，而不是让嵌套写法开始生效——
   后者会让一批「以为关了其实没开」的配额突然开始 429 线上流量。
6. **写入即校验**（Key 与标签的 policy）：非对象、配额字段非数字、未知顶层字段一律 400，并在信息里
   列出可接受字段。存下来却被忽略的策略比被拒绝的策略更糟。
7. **`monthly_*` 如实标注**：解析与展示保留，但接口、工具与文档都写明「已解析、未执行」，
   实现（需要读 `usage_counters` + 缓存 + 结算失效）另立在 `docs/TODO.md`。
8. **解析器放在 `internal/domain`**：`internal/mcpsrv` 的分层允许集只有 `{domain, registry}`，
   而 `quota` 与 `mcpsrv` 都必须读同一份形状，因此 `domain.ParsePolicy` 是唯一能共享的位置。
9. **一次计算，三处使用**：`persist` 只算一次 `recordInput`，落库（请求日志 + `responses.request_json`）
   与 hook 事件的 `input` 字段共用同一份文档，三者不可能互相矛盾。
10. **Key 的 PATCH 改成单次写**：`UpsertAPIKey` 会按结构体重写整行，而此前的写法先调
    `SetAPIKeyRecording` 再 `UpsertAPIKey(target)`，把刚写入的录制开关又用行里的旧值覆盖回去——
    PATCH 看起来成功、实际什么都没改。现在只改结构体再 upsert 一次。

## 接口

```go
// internal/config
var RecordingInputModes = []string{"full", "user", "metadata", "off"}
func (r Recording) InputModeFor(keyMode string) string // ""|inherit|meta|未知 → 归一化

// internal/domain
var PolicyFields = []string{"rpm","tpm","concurrency","monthly_requests","monthly_tokens",
                           "monthly_cost_micros","strategy","provider_order","margin_bp"}
type RateLimits struct{ RPM int; TPM, MonthlyRequests, MonthlyTokens, MonthlyCostMicros int64; Concurrency int }
func ParsePolicy(raw string) (RateLimits, unrecognised []string, err error)
func (rl RateLimits) Configured() map[string]any
func (rl RateLimits) UnenforcedFields() []string

// internal/responses
type UserInput struct {
    Model   string           `json:"model,omitempty"`
    Input   []pluginapi.Item `json:"input"`   // 无用户条目时序列化为 []
    Omitted map[string]int   `json:"omitted,omitempty"`
    Bytes   int              `json:"request_bytes,omitempty"`
}
func (r *Request) UserInputDocument(bodyBytes int) (*UserInput, error)

// internal/httpapi
type inputRecord struct{ Mode, Payload string; Bytes int; Truncated bool }
func (s *Server) recordInput(ctx context.Context, key *domain.APIKey, req *responses.Request) inputRecord
func keyPolicyDocument(raw json.RawMessage) (string, *domain.APIError)
func validRecordInputMode(mode string) *domain.APIError

// 管理面新增：PATCH /admin/api/v1/keys/{id} 接受 policy；GET /keys 返回 policy；
// 请求列表/详情返回 request_bytes（详情另有 response_bytes）。
// MCP get_rate_limits 新增 not_enforced / ignored_policy_fields / policy_error。
```

## 数据流

```
POST /v1/responses
  └─ recordInput(key, req)                     ← recording.InputModeFor(key.RecordInputMode)
       ├─ full     : 整份请求体 → redact() → truncate(max_bytes, rune 安全)
       ├─ user     : responses.UserInputDocument() → redact() → truncate()
       ├─ metadata : 正文为空，request_bytes 仍记录
       └─ off      : 正文与 request_bytes 都为 0
  └─ persist() 用同一份 payload 写 request_logs、responses.request_json 与 hook 事件的 input
  └─ rejectForQuota() → recordDenied() 走同一个 recordInput（拒绝路径同样脱敏）

管理面写 Key/标签策略
  └─ keyPolicyDocument() → domain.ParsePolicy() → 有未知字段则 400（列出 PolicyFieldList）
  └─ 通过后落库；MCP get_rate_limits 用 domain.ParsePolicy 读同一形状
```

## 异常与边界

- 只有非 user 条目（如纯 `previous_response_id` 续接）：文档 `input: []` + 计数，仍落库。
- 输入是字符串简写：合成一条 user 消息，正常保留。
- 本地拒绝（401/402/403/429）：同策略、同脱敏，仍保留状态行。
- 超大输入：仍由 `recording.max_bytes`（默认 1 MiB）截断，`truncated=true`，切点落 rune 边界
  （此前按字节切会把中文切成非法 UTF-8）。
- 模式值脏数据：`meta` → `metadata`，未知值 → 部署默认（回落到 `user`），不 panic、不放宽。
- 历史策略里已有嵌套写法：下次编辑该 Key/标签会 400 并提示改成扁平；不改写已有行、不擅自开始执行。
- `request_bytes` 语义由「落库正文长度」改为「请求体序列化长度」；无 schema 迁移，仓库外无消费者
  （grep 确认只在 store 读写）。
- MCP `query` 令牌仍只能读 `get_rate_limits`；写策略需要 `admin` scope。

## 测试策略

- `internal/config`：默认 `user`；`InputModeFor` 表驱动（inherit/空/meta/未知/大小写）。
- `internal/domain`：扁平解析、未知字段列表（`rate_limit`/`recording` 被列出）、配额字段非数字与
  非对象文档报错、空文档、`Configured`/`UnenforcedFields`。
- `internal/responses`：混合条目只留 user、工具输出/系统指令/文件路径不出现在序列化结果里、
  字符串简写、无 user 条目时 `input` 为 `[]`。
- `internal/httpapi`：默认文档只含用户输入且带计数、`metadata`/`off` 不落正文、`full` 落整份、
  拒绝路径脱敏且解析策略、扁平 `{"rpm":1}` 第二次请求 429、PATCH `record_input_mode:"meta"` 400、
  嵌套 policy 400 且错误里带字段名、policy 可改且能从 `GET /keys` 读回、`truncate` rune 边界。
- `internal/quota`：`LimitsFromPolicy` 委托后行为不变（既有测试即回归）。
- `internal/mcpsrv`：`get_rate_limits` 报扁平字段 + `not_enforced`。
- `internal/webui`：控制台枚举与服务端一致（含不得出现 `'meta'`）、设置页不再建议无读取方的键。
- `scripts/ui-harness`：新增 `keys.page.html` 承载 `#keys` / `#requests` 两个视图（Key 表格与配额列、
  编辑弹框的四档选择与策略回填、提交后 PATCH 载荷、请求日志详情的 `request_bytes` 与三栏），
  fixtures 增加 `/keys`、`/requests`、`/requests/{id}`，`capture.py` 同步重取。
- `make verify` 全绿；`make ui-check` 8 个视图全绿；`internal/arch` 无需改表（新文件都在既有包内）。

## 依赖

标准库 + 既有包（`internal/{config,domain,responses,quota,mcpsrv,webui}`）；不新增外部依赖。

## 实现与设计差异

- 计划里写「测试 `recordDenied` 走配额拒绝」：实际 fixture 未接 `Billing`，配额拒绝路径不可达，
  改为直接调用 `s.recordDenied(...)`（并在 fixture 上暴露 `*Server`），另外用 `{"rpm":1}` 的真实
  429 覆盖扁平策略生效。
- 计划外新增（同一根因，随本里程碑修掉）：`PATCH /admin/api/v1/keys/{id}` 的录制开关此前会被随后的
  `UpsertAPIKey` 用旧值覆盖，等于保存不上；`AdminStore` 端口里已无调用方的 `SetAPIKeyRecording`
  随之移除（store 方法保留，测试仍在用）。
- plan 里「设置页只留 `billing.fx_rates`」按计划执行；`recording.default` 等键确认无读取方。
- 计划外新增：`scripts/ui-harness` 增加 `#keys` / `#requests` 两个真实浏览器视图（无 node 环境下唯一能
  证明控制台改动可用的手段）；本机还用一个独立实例（`:8099`、临时库）跑了端到端冒烟：默认文档只含用户
  输入、嵌套 policy 400、`'meta'` 400、扁平 `rpm=1` 生效得 429。

# M17 设计：完善内置供应商（`openai-chat`）—— DeepSeek 适配与思考模式

> 计划：对话中评审通过（「不要有插件，要完善内置供应商」）。本文是设计记录，实现完成后回填第 7 节差异。
> 范围：内置 `openai-chat` 供应商 + 共享翻译层 `pkg/providerkit`，另加网关侧思考正文持久化（第 5 节）。

## 1. 目标与非目标

**问题**：DeepSeek 当前的 OpenAI 兼容 API 与「教科书式 OpenAI 兼容」有四处实质差异，内置 `openai-chat` 全部没覆盖，
表现为一类静默降级而不是报错：

| DeepSeek 语义（官方文档） | 内置现状 | 后果 |
|---|---|---|
| 思考模式**默认开启**，用 `thinking:{type:enabled\|disabled}` 与 `reasoning_effort` 控制 | 无 `thinking` 概念，只在客户端给 `reasoning.effort` 时才发 `reasoning_effort` | 无法关闭思考；每次请求都付思考 token |
| 思考正文经 `reasoning_content` 返回（assistant 消息与流式 delta 同层） | `providerkit.ChatMessage` 没有该字段 | 思考内容整段丢弃，客户端拿不到 |
| 带 `tools` 的多轮**必须原样回传** reasoning_content，否则 400 | 出站丢弃 reasoning 项；网关持久化又丢正文 | thinking+tools 多轮必失败 |
| 402 = 余额耗尽（`error.code` 也是 402） | `httpx.ErrorFromResponse` 把 402 归入 `default` → `fatal` | 不冷却、不故障切换，持续打同一个空钱包 |

**目标**：把上述差异变为显式可配行为，且在放行 `reasoning` 能力后，思考内容能沿 canonical 链路端到端流动（含 `previous_response_id` 续接）。

**非目标**：
- 不新增插件、不新增 `deepseek` 内置 kind（理由见第 2 节）；
- 不改 `/responses` 路径（DeepSeek 的 `/responses` 由内置 `openai-responses` 负责，缺口见第 9 节）；
- 不实现 FIM / Chat Prefix Completion / Files API；
- 不做供应商侧 prompt 改写、不代改 `temperature`/`top_p`。

## 2. 关键决策

### 2.1 内置而非插件

`pkg/providerkit` 的 chat↔responses 翻译（367 行）、`internal/providers/httpx`（99 行）与 `openaichat`（274 行）合计 740 行，
且**插件作者不能引用 `internal/`**（`internal/arch` 的允许依赖表把 `examples/` 限定为 `pkg/pluginapi` + `pkg/providerkit`）。
做成插件意味着把这些逐份复制，而修复也要在插件里再写一遍、测试另起一处；对照 `examples/provider-codex`（1216 行）与
`openaichat.go`（274 行）的体量差，这笔重复账是可量化的。

插件的真正价值是**敌意隔离**：非官方、可能随时失效、带服务条款风险的上游（M10 的订阅后端就是这一类）。DeepSeek 是公开有文档的官方 API，
属于内置的判据，而不是需要隔离的对象。另一个佐证：内置配置改动的生效路径更短——`admin_providers.go` 只递增 `ConfigVersion`，
`runtime/dispatcher.go` 在下次使用时就地重建内置实例，插件则要显式重启子进程。

### 2.2 不新增 `deepseek` kind：能力靠显式开关，不靠猜

若新增 `deepseek` kind，它只能是 `openai-chat` 的预设壳（同样的 `base_url` + `models` + 同样的开关），
却要在 `internal/providers/registry.go` 的 `BuiltinKinds/IsBuiltin/Build` 三处与 `internal/arch` 依赖表各加一行，
而同样的修复仍要写两遍。零配置接入改用文档 + `config.example.yaml` 的现成片段。

反过来说，把 `thinking` 无条件发给所有上游会破坏 ollama / vLLM / qwen 等不认识该字段的部署。
因此**新增开关的默认值一律等于当前行为**，升级不改变任何现有部署；DeepSeek 接入需要显式打开三个开关。
这与 `openai-chat` 既有的「操作员声明上游目录」（`models` 由配置声明、不猜）保持一致的设计取向。

### 2.3 用带选项的变体扩展，而不是改旧签名

`ResponsesToChat` / `ChatResponseToResponses` / `ChatStreamState.Translate` 只被 `internal/providers/openaichat` 与其自身测试调用
（`examples/provider-codex` 只用 `providerkit.NewSSEReader` / `EstimateTokens`），所以改动面可控。
即便如此，**默认参数路径必须逐字节等于旧行为**：旧签名保留、旧测试不动即是对「默认不变」的证明。

### 2.4 思考内容走统一 Items 通道

思考正文落成 `pluginapi.Item{Type:"reasoning", Summary:[{Type:"summary_text",Text:…}]}`，而不是新增协议字段：
`Item` 是协议里现成的类型，`pluginapi` 的 JSON 编解码与 `internal/httpapi/v1.go` 的 `decodeStoredItems` 都已保留 `Summary`，
网关 `assembler.addReasoning` 也会把它渲染成 `response.reasoning_summary_text.*` 事件流。**不动公开 SDK 形状。**

## 3. 接口

### 3.1 供应商配置（`internal/providers/openaichat`）

```go
type Config struct {
	BaseURL   string            `json:"base_url"`
	APIKey    string            `json:"api_key"`
	Headers   map[string]string `json:"headers"`
	TimeoutS  int               `json:"timeout_s"`
	Models    []ModelConfig     `json:"models"`

	Thinking          ThinkingConfig `json:"thinking"`
	ResponseFormat    string         `json:"response_format"`             // text | json_object | json_schema
	DefaultMaxOutput  int            `json:"default_max_output_tokens"`   // 0 = 不补
}

type ThinkingConfig struct {
	Mode                   string `json:"mode"`                     // auto | enabled | disabled
	Style                  string `json:"style"`                    // none | deepseek
	ReplayReasoningContent bool   `json:"replay_reasoning_content"`
}
```

枚举非法 → `New()` 返回错误（`ErrInvalidRequest`），不静默降级。`default_max_output_tokens` 只在客户端未给
`max_output_tokens` 时补，仅影响在途额度预留，不吃掉上游默认（DeepSeek thinking 默认 64K / max 时 128K）。

### 3.2 翻译层（`pkg/providerkit/chatcompat.go`，全部为新增，旧符号保留）

```go
type ChatMessage struct {
	Role             string
	Content          string
	ReasoningContent string         `json:"reasoning_content,omitempty"` // 新增
	ToolCalls        []ChatToolCall
	ToolCallID       string
	Refusal          string
}

type ChatConvertOptions struct {
	ReplayReasoningContent bool // 出站：把 reasoning 项正文写成 assistant.reasoning_content
	KeepReasoningContent   bool // 入站：把 reasoning_content 落成 reasoning 项
}

func ResponsesToChatWithOptions(req *pluginapi.Request, opts ChatConvertOptions) (*ChatRequest, error)
func ChatResponseToResponsesWithOptions(resp *ChatResponse, opts ChatConvertOptions) (*pluginapi.Response, error)
```

`ChatStreamState` 增加 `ReasoningText string`（累积）与 `toolID map[int]string`（修 bug：首块记下 `call_id`，
后续 `tool_call.arguments.delta` 沿用同一 id；此前发的是空 id）。`Translate` 对 `delta.ReasoningContent` 发
`reasoning.delta`。`finish_reason` 归一化补充 `insufficient_system_resource` / `aborted`（归为需要故障切换的失败），
空 `choices` 不再产出空 `Status`。

## 4. 数据流

### 4.1 出站（canonical → 上游 `/chat/completions`）

沿用既有 `ResponsesToChat` 主干（`instructions`→system、`function_call`→`assistant.tool_calls`、
`function_call_output`→`role:tool`），在其上按开关追加：

1. 思考参数（仅 `style=deepseek`）：
   `effort := req.Reasoning.Effort`；`none` 或 `mode=disabled` → `thinking:{type:"disabled"}` 且不下发 `reasoning_effort`；
   否则 → `thinking:{type:"enabled"}` + `reasoning_effort: effort`（`minimal/medium/xhigh` 由上游自行映射）；
   `mode=enabled` 时即使客户端没给 effort 也发 `thinking:{type:"enabled"}`。
2. `reasoning_content` 回传（仅 `replay_reasoning_content=true` 且请求带 `tools`）：
   每个 `reasoning` 项的 `Summary[].Text` 顺序拼接，附着到其后第一条带 `tool_calls` 的 assistant 消息。
3. `response_format`（`json_object` / `json_schema` 档位才下发）。
4. `max_tokens` 补缺、`stream_options.include_usage=true`（流式）。

上游专有字段 `thinking` / `response_format` 用**本地 payload 结构体**包裹 `providerkit.ChatRequest` 承载，不改共享
`ChatRequest` 的语义（`ChatRequest` 里已有的 `reasoning_effort` 直接复用）。

### 4.2 入站（非流式）

`choices[0].message`：`reasoning_content` → `reasoning` 项（按开关）；`content` → `message`/`output_text`；
`tool_calls` → `function_call` 项。`finish_reason`：`stop`/`tool_calls` → `completed`，`length` → `incomplete`，
`content_filter` → `completed`，`insufficient_system_resource`/`aborted` → 返回可重试错误（上游资源不足允许切换供应商，
不伪装成正常完成）。

用量维度（复用 `ChatUsageToDimensions`）：`prompt_cache_hit_tokens`/`prompt_cache_miss_tokens` → `input_cache_hit`/`input_cache_miss`，
否则 `input`；`output` = `completion_tokens`；`completion_tokens_details.reasoning_tokens` → `reasoning`（与内置 provider 同口径，
是否单列计价由定价引擎决定）。

### 4.3 入站（流式）

`reasoning_content` delta 先于 `content` delta 自然到达，直接翻译为 `reasoning.delta`；工具调用首块发 `tool_call.start`，
其后参数增量沿用 call_id。在途用量按文本增量发 `usage.delta{output, estimated:true}`，结束时发**最终** `usage`
（`usage.Accumulator` 的语义是 final 覆盖 delta，因此顺序必须是 delta 在前、final 在后）。上游未给 usage 时用
`providerkit.EstimateInputTokens` + `CharEstimator` 兜底并标 `estimated:true`。

### 4.4 错误分类（`classify(resp)`）

| 上游 | 返回 | 路由行为 |
|---|---|---|
| 402（HTTP 状态或响应体 `error.code == 402`） | `quota_exhausted`（`reset_at` = now+1800 或 `Retry-After`） | 冷却该候选，允许切换 |
| 429 | 有 `Retry-After` → `quota_exhausted` + `reset_at`；否则 `retryable` | 冷却 / 切换 |
| 401 / 403 | `fatal` `token_invalid` | 不切换（凭据问题） |
| 400 / 422 | `fatal` `upstream_400`，**保留上游 `error.message`** | 不切换，回客户端 |
| 5xx / 超时 / 网络 | `retryable` | 切换 |

`httpx.ErrorFromResponse` 的公开语义保持不变，新逻辑作为其上的一层补充（402 与体 code 两条）。

## 5. 网关侧：思考正文持久化

`internal/httpapi/v1.go` 的 `decodeStoredItems` 对 `reasoning` 项只产出 `{Type:"reasoning", ID:…}`，正文在 `persist()`
时就被丢掉，`previous_response_id` 续接因此无法回传 reasoning_content。改动：

1. 迁移给 `responses` 表加 `reasoning_text TEXT NOT NULL DEFAULT ''`（沿用既有迁移框架与幂等写法）；
2. `domain.ResponseRecord` 加 `ReasoningText`，`internal/store` 读写该列；
3. `persist()` 写 `assembler.Reasoning()`，`decodeStoredItems` 回填 `Summary[0].Text`。

其它供应商不受影响：拿不到思考正文时为空串，历史记录退化为「无思考正文」而不是报错。

## 6. 异常与边界

- 未打开 `style=deepseek` → 不发 `thinking`（对 ollama / vLLM / qwen 零影响）。
- 「客户端不要思考」≠「上游不思考」：DeepSeek thinking 默认开启，省思考 token 只能靠 `mode=disabled`，README 写明。
- thinking 开启时上游忽略 `temperature`/`presence_penalty`，`top_p` 下限被抬到 0.95：原样透传不代改，README 记录。
- thinking + `tool_choice=required`/具名工具 → 上游 400：不拦截，保证 message 原样透出。
- 带 `tools` 的多轮若历史没有 reasoning 项（客户端未回传）→ 不补空，交由上游报 400，错误原样透出。
- 上游在非余额语义上使用 402 → 只在 `style=deepseek` 或体 `code==402` 时按配额冷却。
- 流中断 / 坏帧 → retryable；`ctx` 取消 → 返回 `ctx.Err()`（`provider.cancel` 路径不变）。
- 迁移在既有库执行：新列默认空串，历史记录续接不报错。

## 7. 测试策略

**`pkg/providerkit`**：思考增量 → `reasoning.delta` 且默认参数下不产生该事件（默认不变断言）；工具调用参数增量带**同一 call_id**（回归）；
出站回传有/无 `tools` 两种分支；`stop`/`length`/`tool_calls`/`insufficient_system_resource`/`aborted` 的状态映射；空 `choices` 不产生空状态。

**`internal/providers/openaichat`**（httptest 假上游）：请求体开关矩阵（`style=deepseek` × effort 取值、`effort=none`、`mode=disabled`、
默认 `mode=auto` 不发任何思考字段）；`replay_reasoning_content` 真/假；`response_format` 三档；用量三维度与无 usage 兜底；
错误分类 401/402（两形态）/429（带与不带 Retry-After）/400（message 保留）/500。

**网关**：`internal/httpapi` 一例——存储的思考正文经 `decodeStoredItems` 回填、空值兼容；迁移幂等沿用既有测试模式。

**真机**（需 `DEEPSEEK_API_KEY`）：`scripts/deepseek-smoke.sh` 走非流式、流式（思考先于正文）、thinking+tools 多轮续接，
核对 `usage_records` 维度与账本 cost/charge。

## 8. 依赖与假设

- 无新增第三方依赖（标准库 + 既有 `pkg/*`）。
- 假设：DeepSeek 模型名/上下文/输出上限（`deepseek-flash` 1M/384K、`deepseek-v4-pro`）由 provider model 配置覆盖，不改代码；
  上游差异通过开关与配置吸收。
- 假设：`config.yaml` 属本地未跟踪文件，示例块仅本机演示。

## 9. 暂不纳入

内置 `openai-responses` 接 DeepSeek `/responses` 的三处缺口（需单独评审，其中第 3 项涉及公开 SDK 形状）：
`response.reasoning_text.delta` 事件名未识别、`usage.output_tokens_details.reasoning_tokens` 未映射、
思考正文在 `pluginapi.Item` 中缺承载字段（`Summary` 属摘要语义）。

## 10. 实现与设计差异

1. **没有新增迁移，也没有新增列**。设计里打算给 `responses` 表加 `reasoning_text`，实现时发现思考正文可以直接放进它所属的输出项：
   `responses.OutputItem` 增加 `content:[{type:"reasoning_text",text:…}]`（与上游 Responses 形状一致，DeepSeek 的 reasoning 项就是这个形状），
   随既有 `output_json` 一起落库。这样 `previous_response_id` 续接、`GET /v1/responses/{id}`、实时 SSE 三者自然一致，
   少一个真源、少一次迁移。`summary` 字段保持原样驱动 `reasoning_summary_text.*` 事件。
2. **`FeedItems` 读取思考正文时以 `summary` 优先、`content` 兜底**：网关自己产出的项两个字段都写，
   上游 Responses 形状的项只写 `content`（如 openai-responses 转发的 DeepSeek 输出），二者取其一即可且不会重复。
3. **`finish_reason` 映射比设计多改了一处**：`content_filter` 也归为 `incomplete`（与 `length` 同理：
   不是模型说完的答案）；`insufficient_system_resource` / `aborted` 由 `providerkit.UpstreamFailure` 判定为上游失败，
   `openaichat` 在流式收尾与非流式响应两处都转成 `retryable`，不伪装成 `completed`。
4. **`style=none` 时 `reasoning_effort` 仍按旧逻辑透传**（设计未细化）：非 DeepSeek 上游（o-series 等）认这个字段，
   只有 `style=deepseek` 走到"关闭思考"分支时才清空它，避免旧行为变化。
5. **顺手修了分层断言**：`internal/arch` 的允许依赖表遗漏了 M14(1) 引入的 `internal/sessionauth` / `internal/portal`，
   该里程碑提交后 `make verify` 一直是红的。本轮补上两行与 `internal/admin`/`internal/httpapi` 的对应依赖，
   否则 M17 的验收标准（`make verify` 全绿）无法成立。
6. **验收走查的形态**：没有用插件或真实上游，而是 `scripts/deepseek-smoke.sh` 起一个 DeepSeek 形状的假上游 + 真实 `bin/aigw` 二进制，
   校验请求形状（`thinking` / `reasoning_effort` / `tools`）、流式顺序（思考先于正文）、用量维度（缓存 80 / reasoning 5）、
   错误映射（400 → fatal 且保留 message、402 → 配额）与**续接回传**（假上游记录 `replay=True`）。
   真机走查（`--live`）已执行，见第 8 条。
8. **真机走查结果（2026-09-11，`api.deepseek.com`，`deepseek-flash`）**：
   - 非流式响应带回 `reasoning` 项；usage 为 `input_tokens=36, output_tokens=29, output_tokens_details.reasoning_tokens=27`
     （该次 `cached_tokens=0`，故网关按既有口径不下发 `input_tokens_details`）。
   - 工具轮的思考正文经存储后续接成功：`previous_response_id` 续接**被上游接受**（若丢掉 `reasoning_content` 即 400），
     说明"思考正文随 output item 落库 → `decodeStoredItems` 回填 → 出站回传"整条链路在真机成立。
   - 流式：思考增量先于正文增量，`response.completed` 带 usage。
   - 真机走查暴露并修掉了两个**验收脚本自身**的问题（与网关实现无关）：凭据必须经凭据通道（`PATCH /admin/api/v1/providers/{id}` 的
     `credentials`）注入，不能写进 provider config；以及 `response_format=json_object` 会让上游拒绝任何未提及 json 的提示词，
     故走查脚本改用默认档位（该档位的覆盖在单测里做）。
   - 另据实测：DeepSeek 对"带 tools 但不回传 reasoning_content"的续接**没有**报 400（文档如此要求，实测宽容）。
     网关仍按文档回传——回传是上游明确要求的行为，且不增加成本。
7. **测试规模**：`pkg/providerkit` 新增 8 个用例（含"默认参数不产生思考事件"的不变断言），
   `internal/providers/openaichat` 新增 10 个用例（思考开关矩阵 6 个子例、思考回传 2 例、流式 2 例、
   上游失败原因 2 例、错误分类 5 例、response_format 3 例、max_tokens 补缺、Health，共 30 个断言点），
   `internal/httpapi` 新增 3 个用例（续接往返、空值兼容表、上游 reasoning 形状）。

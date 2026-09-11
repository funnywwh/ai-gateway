# openai-chat 供应商：OpenAI 兼容上游与 DeepSeek 适配

> 状态：**已实现（M17）**。适用于内置供应商 kind `openai-chat`（`internal/providers/openaichat`）。

## 1. 范围

`openai-chat` 对接 **OpenAI 兼容的 `/chat/completions`** 上游：DeepSeek、Qwen、Ollama、vLLM、LM Studio、以及任何自称 OpenAI 兼容的服务。
它**不**对接 OpenAI 的 `/responses`——那是内置 `openai-responses` 的职责（见 §7）。

上游之间在"教科书式 OpenAI 兼容"之外常有实质差异，且**猜错是静默降级而不是报错**，因此除公共部分外一律显式开关，
默认值等于通用 OpenAI 兼容行为：升级不会改变既有部署的行为。

## 2. 配置

```json
{
  "base_url": "https://api.deepseek.com/v1",
  "api_key": "",
  "headers": {},
  "timeout_s": 120,
  "models": [
    {"public": "deepseek-flash", "upstream": "deepseek-flash",
     "context_window": 1000000, "max_output_tokens": 65536,
     "capabilities": {"stream": true, "tools": true, "reasoning": true}}
  ],
  "thinking": {"mode": "auto", "style": "deepseek", "replay_reasoning_content": true},
  "response_format": "json_object",
  "default_max_output_tokens": 8192
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `base_url` | 必填 | 上游根地址；`/chat/completions` 与 `/models` 拼在其后（`https://api.deepseek.com/v1` 与 `https://api.deepseek.com` 都可，取决于该部署是否要求 `/v1`） |
| `api_key` | 空 | 也可由凭据通道下发（控制台凭据里的 `api_key`，落库加密） |
| `headers` | 空 | 额外请求头（给需要自定义头或改协议的上游留后路） |
| `timeout_s` | 120 | HTTP 客户端超时；单次尝试的最终上限由路由的 `per_attempt_timeout_s` 决定 |
| `models` | 空 | **上游目录只能声明，不能猜**：`public`/`upstream`/`context_window`/`max_output_tokens`/`capabilities` |
| `thinking.mode` | `auto` | `auto` 由客户端 `reasoning.effort` 决定；`enabled` 强制开启；`disabled` 强制关闭 |
| `thinking.style` | `none` | `deepseek` → 下发 `{"thinking":{"type":"enabled\|disabled"}}`；`none` → 不下发（通用形态） |
| `thinking.replay_reasoning_content` | `false` | 把历史 `reasoning` 项正文回传为 assistant 的 `reasoning_content`（带工具的多轮必需，见 §4） |
| `response_format` | `text` | 上游真实支持到哪一档：`text`（不下发）/`json_object`/`json_schema`。**能力申报要与它一致** |

> ⚠️ **当前实现是"配置即下发"，与上表的"能力申报"语义不一致**（已记账待定夺）：
> `response_format` 会被**无条件**写入每个上游请求（`internal/providers/openaichat/openaichat.go:432`），
> 而该 provider **不读取请求里的 `text.format`**。因此声明 `json_object` 会把**所有**请求变成 JSON 模式。
> 真实后果：DeepSeek 对任何不含 "json" 字样的提示词直接 400
> （`Prompt must contain the word 'json' in some form to use 'response_format' of type 'json_object'`），
> 该供应商于是只能服务 JSON 类请求。**除非你确实要让全部流量走 JSON 模式，否则保持 `text`（默认）。**
| `default_max_output_tokens` | `0` | `>0` 且客户端未给 `max_output_tokens` 时才补，只影响在途额度预留，不吃掉上游默认 |

枚举取值非法 → 供应商构建失败（不静默降级）：`thinking.mode`、`thinking.style`、`response_format` 均校验；
`replay_reasoning_content` 需要 `style=deepseek` 才允许打开。

**控制台是本节的可读版本**：供应商详情页的「配置说明」按 kind 渲染上述字段（名称 / 类型 / 默认值 / 取值 / 说明）
与凭据字段，并在 `api_key` 上标出「建议填凭据栏」；新建供应商前可在「内建类型说明」里查看全部内建类型并一键套用模板
（行为规格见 `docs/provider-ui.md`，字段说明与代码同源、由 `internal/providers/*/schema.go` 提供）。

`capabilities` 决定路由：客户端请求 `reasoning.effort` 会要求候选具备 `reasoning`；
`text.format` 会要求 `json_schema`。因此 DeepSeek 的模型应声明 `{"stream":true,"tools":true,"reasoning":true}`，
而**不要**声明 `json_schema`（`/chat/completions` 不支持，声明了会让请求带着降级标记继续打到上游）。

## 3. DeepSeek 接入（现成片段）

```yaml
providers:
  - name: deepseek
    kind: openai-chat
    enabled: true
    config:
      base_url: "https://api.deepseek.com/v1"
      thinking: {mode: auto, style: deepseek, replay_reasoning_content: true}
      response_format: json_object
      default_max_output_tokens: 8192
    models:
      - public: deepseek-flash
        upstream: deepseek-flash
        context_window: 1000000
        max_output_tokens: 65536
        capabilities: {stream: true, tools: true, reasoning: true}
      - public: deepseek-v4-pro
        upstream: deepseek-v4-pro
        context_window: 1000000
        max_output_tokens: 65536
        capabilities: {stream: true, tools: true, reasoning: true}
```

**DeepSeek 的语义要点（官方文档）**

- **思考默认开启**：不发 `thinking` 就是开启。想省思考 token 只能显式关（`mode=disabled` 或客户端 `reasoning.effort="none"`）；
  "客户端不要思考"≠"上游不思考"。
- `reasoning_effort` 支持 `minimal/low/medium/high/xhigh/max`，上游自行映射（`minimal→low`、`medium/xhigh→high`）。
- thinking 模式下 `temperature` / `presence_penalty` **被忽略**（不报错），`top_p` 下限被抬到 0.95；网关原样透传，不代改。
- thinking 模式下 `tool_choice` 不支持 `required` 与具名工具（上游 400）；网关不拦截，错误原样透出。
- `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` 拆出缓存维度；
  `completion_tokens_details.reasoning_tokens` 单列 `reasoning`（是否单独计价由定价规则决定）。
- `finish_reason` 出现的 `insufficient_system_resource` / `aborted` 视为**上游失败**（可故障切换），不伪装成正常完成。
- 402 = 余额耗尽：映射为 `quota_exhausted` 并冷却该候选（默认 1800s）。

## 4. 思考内容（reasoning）

- **入站**：`reasoning_content`（非流式消息字段 / 流式 delta）→ 思考增量事件，最终落成 `reasoning` 输出项；
  客户端在 SSE 上看到 `response.reasoning_summary_text.delta`。
- **出站**：`replay_reasoning_content=true` 时，历史 `reasoning` 项正文写入其后第一条带 `tool_calls` 的 assistant 消息的
  `reasoning_content`。上游要求**全量原样回传**，缺失即 400——这是 thinking + tools 多轮的硬约束。
- **续接**：`reasoning` 输出项把正文存在 `content: [{type:"reasoning_text"}]`（与上游 Responses 形状一致），
  与 `summary`（网关的事件形状）同时写入；因此 `previous_response_id` 续接、`GET /v1/responses/{id}` 与实时事件三者一致。
  `reasoning_content` 只在存在工具调用时回传：没有工具调用的历史无需回传，上游也会忽略。

## 5. 事件与用量映射

| 上游 | 网关 |
|---|---|
| `delta.reasoning_content` | `reasoning.delta`（先于正文） |
| `delta.content` | `text.delta` |
| `delta.refusal` | `refusal.delta` |
| `tool_calls[].function.name`（首块） | `tool_call.start`（记下 `call_id`） |
| 后续 `tool_calls[].function.arguments` | `tool_call.arguments.delta`（**沿用首块的 `call_id`**） |
| 末尾块 `usage` | 最终 `usage`（流式先发估算 `usage.delta`，最终值覆盖） |
| 无 `usage` | 字符估算 + `estimated:true` |

## 6. 错误分类

| 上游 | 返回 | 路由行为 |
|---|---|---|
| 402（HTTP 状态或响应体 `error.code==402`） | `quota_exhausted` | 冷却该候选（`Retry-After` 优先，否则 1800s），允许切换 |
| 429 | 有 `Retry-After` → `quota_exhausted` + `reset_at`；否则 `retryable` | 冷却 / 切换 |
| 401 / 403 | `fatal` `token_invalid` | 不切换（换供应商同样失败，是凭据问题） |
| 400 / 422 | `fatal` `upstream_4xx`，**保留上游 message** | 不切换，回客户端（如 thinking + 具名 tool_choice） |
| 5xx / 超时 / 网络 | `retryable` | 切换下一个候选 |

客户端可见的 HTTP 映射见 `docs/api-responses.md`（致命上游错误经网关统一封装）。

## 7. 与 `openai-responses` 的分工

- `/chat/completions` → `openai-chat`（本文档）。**接不了 OpenAI Responses API**。
- `/responses` → `openai-responses`。DeepSeek 也提供 `/responses`（为 Codex 提供），但该内置实现目前有三处缺口未覆盖
  （`response.reasoning_text.delta` 事件名、`usage.output_tokens_details.reasoning_tokens`、思考正文的承载字段），
  故 DeepSeek 的推荐接入路径是本文档的 `/chat/completions`。

## 8. 验收

离线（无需密钥，假上游校验请求形状、思考开关、流式顺序、用量维度、错误映射与续接回传）：

```sh
bash scripts/deepseek-smoke.sh
```

真机（需要 `DEEPSEEK_API_KEY`）：

```sh
DEEPSEEK_API_KEY=sk-... bash scripts/deepseek-smoke.sh --live
```

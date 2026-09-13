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
| `thinking.mode` | `auto` | `auto`：客户端给了 `reasoning.effort` 就照办（`none`→关，其余→开）；**没给就根本不下发该字段**，由上游默认决定（DeepSeek 默认就是开）。`enabled` / `disabled` 分别强制开关 |
| `thinking.style` | `none` | `deepseek` → 下发 `{"thinking":{"type":"enabled\|disabled"}}`；`none` → 不下发（通用形态） |
| `thinking.replay_reasoning_content` | `false` | 把历史 `reasoning` 项正文回传为 assistant 的 `reasoning_content`（带工具的多轮必需，见 §4） |
| `response_format` | `text` | 上游真实支持到哪一档：`text`（不下发）/`json_object`/`json_schema`。**能力申报要与它一致** |

### 出站方言翻译（`openai-chat` 一系）

Responses 表面比 chat completions 宽，翻译层负责把宽的那一侧收敛到上游认的形状——**客户端不需要
为某个上游改写请求**（见 `docs/design/m19-client-dialects.md`）：

- **系统级角色归一化为 `system`**。`developer` 只是该槽位在 OpenAI 新 API 里的名字，而 OpenAI 兼容
  上游（DeepSeek、vLLM、Ollama…）普遍只认 `system`，直接透传会得到
  `unknown variant 'developer'`。注意 Codex 订阅后端正好相反（拒绝 `system`、接受 `developer`），
  那个方言由 `examples/provider-codex` 负责。
- **只有 function 工具下发**。chat completions 表达不了 `web_search`/`namespace` 等类型：它们被
  跳过（而不是塞进 function 字段变成无名工具）。模型因此拿不到该工具——属能力降级，不是错误；
  请求其余部分照常完成。
- 未建模类型的原始结构由 `pluginapi.Tool.Raw` 带到 provider，插件类 provider 可自行翻译或丢弃。

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
      - public: deepseek-v4-flash
        upstream: deepseek-v4-flash
        context_window: 1000000
        max_output_tokens: 65536
        capabilities: {stream: true, tools: true, reasoning: true}
      - public: deepseek-v4-pro
        upstream: deepseek-v4-pro
        context_window: 1000000
        max_output_tokens: 65536
        capabilities: {stream: true, tools: true, reasoning: true}
```

两个 `v4` id 都已对着真实上游验证过（2026-09-11，运行中的 `:8088` 实例）：`deepseek-v4-pro` 回
`completed` + `output_text`，`deepseek-v4-flash` 在 `max_output_tokens:16` 下把额度全花在思考上、状态
`incomplete`（不是错误）——这恰好说明思考默认开启，要给正文留额度。

**只加供应商模型不够，对客可用需要三层齐全**：`provider_models`（能力申报与上游映射，路由缺它即
`not_mapped`）、`models`（对客模型名，`GET /v1/models` 只列它）、`routes`（模型 → 供应商 + 上游模型）。
管理 API 的 `POST /admin/api/v1/providers/{id}/models` 在缺 canonical model 时会回
`warning: no canonical model with this public name exists yet; add one so it becomes routable` —— 这句就是提醒还差后两层。

三层里最容易漏的是**第一层**，而且漏了不报错：控制台原来没有管理 `provider_models` 的界面，
「模型」页加的是第二层、「模型路由」页加的是第三层，于是「都配了却不显示」只能靠
`GET /admin/api/v1/router/explain?model=<名字>`（回 `excluded: [{provider, reason: "not_mapped"}]`）才能定位。
M28 起「模型供应商 → 详情 → **模型映射**」直接列出该供应商的映射，并把**指向本供应商却没有映射的路由**
标红点名（含 `not_mapped` 的字样），可以在那里直接新建/编辑/删除。

`POST /admin/api/v1/providers/{id}/models` 是**部分更新**：请求体里**没写的字段保持原值**，
写 `null` 才是清空（数值字段写 `0` 是把它设成 0）。这条语义是 M28 改的——
在这之前它是整行覆盖，而控制台定价页只发三个字段，于是保存一次成本规则就会把
capabilities、context_window、max_output_tokens 静默清零（映射仍然可用，但客户端一带
tools/reasoning 就会被打上 `X-Gateway-Degraded`）。只发 `public_model` 仍可新建一行，新行的默认值不变
（`upstream_model` = 对客名、`enabled` = true、`priority`/`weight` = 100）。

**DeepSeek 的语义要点（官方文档）**

- **思考默认开启**：不发 `thinking` 就是开启。想省思考 token 只能显式关（`mode=disabled` 或客户端 `reasoning.effort="none"`）；
  "客户端不要思考"≠"上游不思考"。
- 因此 `auto` 对**沉默的客户端**（请求里没有 `reasoning` 字段）不下发该字段，而不是下发 `disabled`：
  后者会把"客户端没提这件事"翻译成"关掉思考"，等于替上游做了一个它没被要求做的决定。
  实测后果（2026-09-11）：DSH 指向本网关时不发 `reasoning`，于是每一步都在无思维链下回答，
  模型能力明显下降、任务做一半就停；而同一直连 DeepSeek 的客户端（会发 `thinking.enabled`）不受影响。
  修复后同一形态请求恢复思考（reasoning_tokens 19 vs 0），`effort="none"` 仍然能关掉。
- `reasoning_effort` 支持 `minimal/low/medium/high/xhigh/max`，上游自行映射（`minimal→low`、`medium/xhigh→high`）。
- **带工具的一轮必须把 `reasoning_content` 原样回传**，而且**要的是这个键本身**：缺键直接 400
  （`The `reasoning_content` in the thinking mode must be passed back to the API.`），空串可以。
  这条约束在**非流式**请求上即使没显式发 `thinking` 也照样生效（实测：`thinking=disabled` 时不需要，
  其余情况——包括不传该字段——都必须带）。
  网关的策略：**客户端给了就回放原文**（`reasoning` 项，正文在 `summary` 或 `content` 里都认）；
  **客户端没给就回放空串**，而不是拿"缺键"去换一个整条请求失败——无状态网关无法恢复客户端没有回传的思维链，
  空串是它能诚实给出的值，上游也确实接受（实测 4/4 通过）。
  真实后果（2026-09-11）：DSH 指向网关时从不回传 `reasoning` 项，于是每遇到"工具轮 + 思考开启"就整条请求 400；
  修复后同一形态 4/4 通过。
- **进入工具轮的那一整段 assistant 内容算"一轮"**：紧挨在工具调用之前的纯文本 assistant 消息**也必须带该键**，
  否则同样 400 —— 实测：`[用户, 文本(无键), 工具轮(带键), 工具结果]` 报 400；把文本与工具调用**并成同一条**
  assistant 消息（`content` + `reasoning_content` + `tool_calls`，也就是上游自己产出的形态）则通过。
  Responses 客户端把文本与工具调用作为两条 item 发来（DSH 就是这样），网关翻译时会**折成一条** assistant 消息
  （`ReplayReasoningContent` 打开时），而不是留下两条让上游去校验。
- **这条约束覆盖的是"整段 assistant 侧"，不只是那条带工具调用的消息**（M37 修正）：模型先调工具、看到结果、
  再写一段话说明发现——那段话属于**同一轮**，但中间隔着 `tool` 消息，**折叠够不到它**，于是带着"缺键"出行，
  整条请求照样 400。这个形态在 agent 循环里比"文本在调用之前"更常见：每次工具返回后模型都会写一段总结，
  而当轮请求的第一条 assistant 消息恰恰就是它。网关的做法是在翻译收尾时对**工具轮涉及的全部 assistant 消息**
  统一补键（已有正文的保留正文，没有的用空串；纯聊天、历史上没有工具调用的请求完全不发该字段）。
  真实后果（2026-09-13）：DSH 指向网关时，凡是"工具结果 → 模型叙述"的一步就报
  `upstream_400: The \`reasoning_content\` in the thinking mode must be passed back to the API.`；
  修复后同一形态通过。离线冒烟里的假上游现在**按这条规则真判 400**，所以这类回归不会再静默通过。
- thinking 模式下 `temperature` / `presence_penalty` **被忽略**（不报错），`top_p` 下限被抬到 0.95；网关原样透传，不代改。
- thinking 模式下 `tool_choice` 不支持 `required` 与具名工具（上游 400）；网关不拦截，错误原样透出。
- `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` 拆出缓存维度；
  `completion_tokens_details.reasoning_tokens` 单列 `reasoning`（是否单独计价由定价规则决定）。
- `finish_reason` 出现的 `insufficient_system_resource` / `aborted` 视为**上游失败**（可故障切换），不伪装成正常完成。
- 402 = 余额耗尽：映射为 `quota_exhausted` 并冷却该候选（默认 1800s）。

### 让 agent 客户端能选推理档位（两端各一半，缺一不可）

客户端（DSH、Codex 等）要在界面上给出「推理等级」，需要**两件事同时成立**：

1. **网关侧如实申报能力**：模型行声明 `reasoning: true`，否则客户端带 `reasoning.effort` 的请求会被打上
   `X-Gateway-Degraded: reasoning`（`degradation: strip` 只做标记、不剥离参数，所以功能照样跑，失真的是可观测性与
   `least_latency` 对推理模型的降权）。改法：`config.yaml` 的 `bootstrap.providers[].models[].capabilities`、
   控制台「供应商 → 模型 → 能力」、或管理 API `POST /admin/api/v1/providers/{id}/models`。
   **只改 config.yaml 对运行中的实例不生效**——bootstrap 不回填非空 capabilities、也不覆盖已存在 provider 的 enabled。
2. **客户端侧声明可选档位**：DSH 的模型目录只在 model 带 reasoning 元数据时才提供档位选择，而手写路由
   （pi-ai 目录里没有的网关）必须逐模型写出来；DSH 的档位 key 与出线拼写是分开的，因此可以逐模型裁剪：

```yaml
# ~/.dsh/settings.yaml → llm-pi-ai.providers.<route>.models[]
- id: deepseek-flash
  reasoningEfforts: {off: none, minimal: minimal, low: low, medium: medium, high: high, xhigh: xhigh, max: max}
```

**档位表必须按上游实测填，不要照抄全集**：本网关的 DeepSeek 接受全部拼写，但 codex 订阅后端的
`gpt-5.6-luna` 明确拒绝 `minimal`（`unsupported_value`，HTTP 500），而 `off` 这种非法拼写回的是
`invalid_value`——两者可区分，说明不是"取值随便填"的宽容后端。把 `off` 写成 `none`（而不是留空）才能让
「提供方默认」= 显式关闭思考；留空表示"支持但不发参数"，会落到上游自己的默认（DeepSeek 默认是开启思考），
于是「Off」这一档名不副实。完整实测矩阵与取舍见 `docs/design/m20-dsh-reasoning-effort.md`。

## 4. 思考内容（reasoning）

- **入站**：`reasoning_content`（非流式消息字段 / 流式 delta）→ 思考增量事件，最终落成 `reasoning` 输出项；
  客户端在 SSE 上看到 `response.reasoning_summary_text.delta`。
- **出站**：`replay_reasoning_content=true` 时，历史 `reasoning` 项正文写入其后第一条 assistant 消息的
  `reasoning_content`；**工具轮涉及的全部 assistant 消息都会带上这个键**（有正文用正文、没有用空串），
  因为上游校验的是整段 assistant 侧。上游要求**全量原样回传**，缺失即 400——这是 thinking + tools 多轮的硬约束。
- **续接**：`reasoning` 输出项把正文存在 `content: [{type:"reasoning_text"}]`（与上游 Responses 形状一致），
  与 `summary`（网关的事件形状）同时写入；因此 `previous_response_id` 续接、`GET /v1/responses/{id}` 与实时事件三者一致。
  没有工具调用的请求完全不发该字段：没有工具调用的历史无需回传，上游也会忽略。
  续接时网关会**等这条响应落库**（与 `GET /v1/responses/{id}` 同样的等待）：审计行由后台批量写入，
  不等就会把"刚拿到 `id` 就接着发下一轮"的 agent 循环判成 `response not found`——而这正是
  thinking + tools 的必经形态（工具结果必须在下一个请求里回传）。M37 之前这里恒有竞态。

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

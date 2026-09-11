# M20 设计：让 DSH 里的 aigw 模型可以设置推理强度（reasoning effort）

> 计划：对话中评审通过（「如何让 dsh 里添加 aigw 的模型能设置强度？」）。本文是设计记录，实现完成后回填第 7 节复验结果。
> 前置：`docs/design/m17-openaichat-deepseek.md`（思考模式的出线语义）、`docs/design/m19-client-dialects.md`（面向真实客户端的方言）。
> 范围：**只动配置与文档，不动任何客户端实现、不动网关代码路径**。

## 1. 目标与非目标

**目标**：DSH 的模型菜单里，选到 `aigw/deepseek-flash` 或 `aigw/gpt-5.6-luna` 时能像 DeepSeek 官方那样弹出
「推理等级」子菜单，并且选中的档位真的过线到上游；同时让网关侧的能力声明与实测行为一致（不再回 `X-Gateway-Degraded: reasoning`）。

**非目标**：

- 不改 DSH 客户端（`dsh-client-ui-model-selection` / `dsh-client-ui-settings-models`）——现成的模型菜单本来就按模型的
  reasoning 元数据渲染档位，缺的只是**能力声明**；
- 不动网关的数据面代码（`internal/routing`、`internal/httpapi/v1.go`），不改 `routing.degradation` 的既有语义；
- 不给网关加"按模型裁剪 effort 取值"的新逻辑：上游拒绝某个档位时**如实报错**，档位表在客户端侧维护；
- 不处理 replay 系列（本地假上游）。

## 2. 机制：三个环节缺一不可

```
DSH 模型菜单的档位
  └─ 需要 model.reasoning === true  ── 由用户设置里的 per-model reasoningEfforts 提供
       └─ 需要网关已声明 capabilities.reasoning ── 否则每次请求被标记降级
            └─ 需要上游真的接受 reasoning.effort ── 否则 500，如实暴露
```

1. **DSH 侧（缺的就是这一步）**：`@deepseek-ai/dsh-llm-pi-ai` 只在 `model.reasoning` 为真时才向界面暴露
   `reasoning.efforts`（`reasoningInfo()`，README 也写明"模型不携带 reasoning 元数据 = 能力不可用，界面只提供提供方默认"）。
   手写路由（aigw 这种 pi-ai 目录里没有的网关）若不在 model 条目上声明 `reasoningEfforts`，解析结果是
   `{ reasoning: base?.reasoning ?? false }` 即 `false`，模型菜单里**根本没有等级入口**。
   声明方式（`reasoningEfforts` 的 key = 菜单里的等级，value = 过线拼写；只有 `off` 允许留空）：

   ```yaml
   - id: deepseek-flash
     reasoningEfforts: {off: none, minimal: minimal, low: low, medium: medium, high: high, xhigh: xhigh, max: max}
   ```

2. **网关侧**：客户端带 `reasoning.effort` 时，`featuresOf()` 会要求候选声明 `reasoning` 能力
   （`internal/routing/routing.go` 的 `checkCapabilities`）。注意 `degradation=strip` **只做标记**：
   它把 `reasoning` 记进 `usage_records.degraded_features` 并回 `X-Gateway-Degraded` 响应头，
   **不会**从请求里剥掉参数——所以漏报能力时功能照样能跑，失真的是可观测性与 `least_latency` 对推理模型的降权。

3. **上游**：档位是否被接受由上游决定，且**逐模型不同**（见第 3 节），因此档位表只能靠实测填。

## 3. 实测矩阵（决定档位表的关键证据，2026-09-11）

对运行中的 8088 实例直接打 `/v1/responses`，非流式、同一提示词、逐档位试：

| 模型 | 上游路线 | none | minimal | low | medium | high | xhigh | max | 其他观察 |
|---|---|---|---|---|---|---|---|---|---|
| `deepseek-flash` | 内置 `openai-chat`（`thinking.mode=auto, style=deepseek`） | 200 | 200 | 200 | 200 | 200 | 200 | 200 | `high` 产出 `reasoning` 输出项 + `output_tokens_details.reasoning_tokens`；`none` 关闭思考 |
| `gpt-5.6-luna` | `plugin:codex`（ChatGPT 订阅后端） | 200 | **500** | 200 | 200 | 200 | 200 | 200 | `high` 比 `none` 明显更慢、`xhigh` 超过 60s，说明档位确实透传生效 |
| `replay` / `replay-slow` | `plugin:replay`（本地假上游） | — | — | — | — | — | — | — | 忽略 effort，稳定回 `X-Gateway-Degraded: reasoning` |

两条从实测里读出来的硬结论：

- **`gpt-5.6-luna` 的 `minimal` 必须从档位表里去掉**。上游回
  `unsupported_value: Unsupported value: 'minimal' is not supported with the 'gpt-5.6-luna' model. Supported values are: 'none', 'low', 'medium', 'high', 'xhigh', and 'max'.`
  （HTTP 500）。同一模型对 `off` 这种非法拼写回的是 `invalid_value`，两者可区分，因此这不是"取值随便填"的宽容后端。
- **`off` 的过线拼写是 `none` 而不是留空**。pi-ai 在 `model.reasoning` 为真、且本次请求没有指定档位时，会按
  `thinkingLevelMap.off` 补发一次 reasoning 参数；`off: none` 因此让「提供方默认」= 显式关闭思考，
  行为确定且省 token；而 `off:`（留空）表示"支持、但不发参数"，会让请求落到上游自己的默认（DeepSeek 默认是开启思考），
  于是「Off」这一档名不副实。

## 4. 实际改动

### 4.1 DSH 用户设置 `~/.dsh/settings.yaml`（唯一的客户端侧改动，且是配置不是代码）

给 `llm-pi-ai.providers.aigw` 的 `deepseek-flash` 与 `gpt-5.6-luna` 声明 `reasoningEfforts`（档位表见第 3 节，
`gpt-5.6-luna` 不含 `minimal`），并补全两个模型的 `contextWindow`/`maxTokens`（否则落路由默认 262144/32768）。
`replay` / `replay-slow` 保持不声明。**不设路由级 `reasoning:` 默认档**——按第 3 节的结论，`off: none` 已经让默认行为确定。

生效方式：`dsh-settings-file` 默认开着文件监听（chokidar + 100ms 去抖），改完无需重启 DSH，刷新页面重开模型菜单即可。

### 4.2 网关配置 `config.yaml`

- `bootstrap.providers[codex].config.models[0]`：补 `capabilities: {stream: true, tools: true, reasoning: true}`。
  插件的 `ListModels` 直接透传该字段，所以控制台「刷新发现」能带出正确能力。
- `bootstrap.providers[codex].models`：补 `public: gpt-5.6-luna` 条目（此前该模型行是当时在控制台手建的 `source=manual`，
  文件里没有它 → 文件不是可信基线）。
- `bootstrap.providers[deepseek].enabled`：`false` → `true`，修掉"文件说关着、运行态其实开着"的漂移
  （bootstrap 在 merge 语义下不覆盖已存在 provider 的 enabled，所以写错不报错，只会误导排查）。

### 4.3 运行态

`provider_models` 里 `gpt-5.6-luna`（id 12）的 `capabilities` 由 `{stream, tools}` 更新为
`{stream, tools, reasoning}`（管理 API `POST /admin/api/v1/providers/{id}/models`，同时把该行的
`context_window`/`max_output_tokens` 从 0 补到 272000/128000 与文件一致）。
与 4.2 是**同一件事的两个入口**：文件管下一次 bootstrap，管理 API 管当前这个进程（bootstrap 不覆盖已存在 provider 的 enabled，
也不回填非空 capabilities，所以只改文件对运行中的实例不生效）。

### 4.4 测试

`internal/providers/openairesponses/body_test.go`：钉住"客户端的 reasoning 控制原样进入出站请求体"这一条契约
（`pkg/providerkit` 在 chat 侧已有等价断言）。四例：无 reasoning 时字段缺席、`high` 原样、`none` 是真实取值而非省略、
`effort` 与 `summary` 并存。变异验证：在 `body()` 里加一行 `delete(payload, "reasoning")` 后该测试精确失败，恢复即通过。

## 5. 客户端侧的两个已知副作用（写在这里以免日后当成 bug 排查）

1. 一旦模型声明了档位，DSH 每次请求都会带上 reasoning 参数（含 `effort: none`）——请求形态变了，不只是 UI 变了。
2. 在模型菜单里做一次选择会调 `agentDefaultModel.saveSelection()` **回写 `settings.yaml`**：选「提供方默认」会把
   `agent-default-model` 命名空间里的 `reasoningEffort` 键删掉。要恢复，从备份或手动写回。

## 6. 为什么"添加模型时自动识别推理支持"在纯配置层面做不成（读 DSH 0.1.2-rc.1 源码的结论）

这是本任务里最自然的追问，答案是**不能**，而且不是"网关少暴露了字段"那么简单：

1. **发现链路只搬运四个字段**。`dsh-llm-pi-ai` 的 `readListing()` 从 `GET /models` 的 `data[]` 里只读
   `id / name(display_name) / context_window(context_length) / max_output_tokens(max_tokens)`；对 pi-ai 自带目录的路由，
   `discoverModels()` 同样只返回 `{id, name, contextWindow, maxTokens}`。**能力字段根本没有位置**——
   即便本网关在 `/v1/models` 里多回一个 `capabilities`，这里也会被丢掉。
2. **客户端"采纳"候选时写死同样四个键**。`dsh-client-ui-settings-models` 的 `adopt()` 构造成
   `{id, name?, contextWindow?, maxTokens?}`——所以没有任何"把能力带进配置"的路径。
3. **唯一能自动带来 reasoning 元数据的来源是 pi-ai 自带目录**：`@earendil-works/pi-ai` 的
   `dist/providers/data/*.json` 里逐模型带 `reasoning` 与 `thinkingLevelMap`（例如 `deepseek-v4-flash` 是
   `{minimal: null, low: "low", medium: null, high: "high", max: "max"}`）。但要用上它，**路由名与 model id 都得命中目录**：
   本网关的 `aigw`、`gpt-5.6-luna`、`deepseek-flash` 都不在其中（目录里是 `deepseek` 路由 + `deepseek-v4-flash` 这样的 id）。
4. **档位是否被接受本质上只能实测**：`minimal` 这种"同一模型部分不接受"的情况，任何分级启发式都猜不出来
   （第 3 节实测：`gpt-5.6-luna` 接受 6 档、拒绝 1 档；对非法拼写回的是另一种错误码）。所以真要做，也必须是
   "逐个模型逐个档位发一次真实补全"的探针，而不是查表。

因此"自动识别"只有两条路，都超出本仓库范围：改 DSH 自身（发现响应与采纳逻辑要能承载并写入能力元数据），
或者按 DSH 插件接口写一个自带模型目录的适配器（`llm.registerAdapter(route, models)` 那一路，
即等价于自建一个 pi-ai 适配器）。**当前采用的折中**：档位表在 `settings.yaml` 里按实测显式声明（本文件第 4.1 节），
新增模型时用本文件第 3 节的矩阵方法补一行即可。

## 7. 复验结果（2026-09-11，运行中的 8088 实例）

| # | 动作 | 结果 |
|---|---|---|
| 1 | `gpt-5.6-luna` + `high` | 200，`X-Gateway-Degraded` **消失**（改前是 `reasoning`） |
| 2 | `deepseek-flash` + `high` | 200，无降级头，输出项 `[reasoning, message]`，`reasoning_tokens=13` |
| 3 | `deepseek-flash` + `none` | 200，只输出 `message`（思考确实关闭） |
| 4 | `replay` + `high` | 200，仍带 `X-Gateway-Degraded: reasoning`（说明没假声明能力） |
| 5 | `gpt-5.6-luna` + `minimal` | 500 `unsupported_value`（这就是档位表里不给 minimal 的理由，已写进 config.yaml 注释） |
| 6 | 用 `@deepseek-ai/dsh-llm-pi-ai` 自己的 `Config` schema 解析改后的设置段 | 通过；两个模型的 `reasoningEfforts` 与第 4.1 节一致 |
| 7 | `go test ./internal/providers/openairesponses/` | 通过（含新增四例） |

未覆盖：DSH GUI 里菜单文案与「选档位后跑一轮任务」的人工确认（第 7 节 1–5 已证明过线与降级语义，GUI 部分留待使用中确认）。

## 8. 回滚

- DSH：还原 `~/.dsh/settings.yaml.bak-effort-<日期>`，或删掉两个模型的 `reasoningEfforts` 块 → 菜单里的等级入口立即消失，
  请求恢复成不带 reasoning 参数的旧形态。
- 网关：把 id 12 的 `capabilities` 改回 `{stream: true, tools: true}`；`config.yaml` 用 `git checkout config.yaml` 还原。

全程不涉及数据库迁移、协议/Schema 变更与代码路径改动，回滚无残留状态。

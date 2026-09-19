# M68 设计文档：aigw 供应商的模型参数由 `/v1/models` 决定（能力 / 上下文 / 最大输出 / 图片 / 推理档位）

> 状态：**已实现（M68，代码与单测完成；真机验收见 §9 末与 `docs/TODO.md`）**。
> 前置：[M20 DSH 侧可设推理档位](m20-dsh-reasoning-effort.md)（档位表只能显式声明的结论）、
> [模型级推理覆写](model-reasoning.md)。
> 规格：[docs/dshgw.md](../dshgw.md) §6、[docs/api-responses.md](../api-responses.md)、
> [docs/routing.md](../routing.md)、[docs/api-providers.md](../api-providers.md)。
> 官方口径：[DSH 模型配置指南](https://raw.githubusercontent.com/deepseek-ai/deepseek-harness/master/docs/user/guide/providers.md)、
> [生成的 dsh-llm-pi-ai 配置目录](https://raw.githubusercontent.com/deepseek-ai/deepseek-harness/master/docs/config-catalog.md)。
>
> 需求原话：「实现给 dsh 添加的 aigw 供应商的模型，根据 /v1/models 返回的数据设置模型的推理强度、
> 上下文大小、最大输出、是否支持图片，等」＋「根据官方文档配置 aigw 的模型参数」。

## 1. 目标与非目标

**目标**：dshgw 渲染给租户 dsh 的 `llm-pi-ai.providers.aigw.models[]` 里，每个模型按**官方文档的字段**
带上真实参数，事实全部经网关 `GET /v1/models` 披露：

| dsh 字段（官方文档） | 来源 | 网关侧事实 |
|---|---|---|
| `contextWindow` | `context_window` | `provider_models.context_window`（候选间取最小已知值） |
| `maxTokens` | `max_output_tokens` | `provider_models.max_output_tokens`（同上） |
| `input: [text, image]` | `input_modalities` | 新能力键 `capabilities.image` |
| `reasoningEfforts` | `capabilities.reasoning` | `capabilities.reasoning` + `models.reasoning_json` |
| `name` | `name` | `models.display_name` |
| 路由级 `maxRequestImageBytes` | —（dshgw 配置） | 网关 `server.max_body_bytes`（默认 10 MiB） |

**验收**：
1. `GET /v1/models` 每项带 `name` / `context_window` / `max_output_tokens` / `input_modalities` /
   `capabilities` / `reasoning` 六个**可选**字段；只读旧字段的客户端行为不变。
2. `dshgw sync-models <tenant>`（或 worker 启动前自动同步、登录时）之后，租户 `settings.yaml` 的 aigw 段
   模型条目带上述参数，且**被已安装 dsh 自己的 schema 接受**，租户 dsh 正常启动：模型菜单出现推理档位，
   声明图片的模型能附图片。
3. 没有任何能力数据的模型只写 `id`/`name`，与今天的输出逐字节一致。

**非目标**：

- 不改 DSH 自身。GUI「拉取模型 / 采纳」只搬 `id/name/contextWindow/maxTokens` 四个字段（M20 §6 与官方
  指南的发现链路），本次不碰；本里程碑走的是**配置文件渲染**这条 dshgw 本来就掌握的路。
- 不新增「逐模型档位表」的存储/管理面/控制台（M20 §6.4：档位是否被上游接受本质只能实测）。本次采用
  「声明了 reasoning 就发全 7 档」，个档被上游拒绝时仍由上游如实报错。
- 不自动探测图片能力：声明制（operator 在控制台给 provider model 打 `capabilities.image`）。
- 不改本机 `~/.dsh/settings.yaml`（那是手写配置，不是 dshgw 租户）；文档给出同一套规则的手写示例。
- 不修 `docs/api-responses.md` 里并不存在的 `GET /v1/models/{id}`（仅记录在 §10）。

## 2. 现状与证据（2026-09-20 实测）

- `GET /v1/models` 目前只回 `id/object/created/owned_by/x-gateway-pricing`：
  [`internal/responses/types.go`](../../internal/responses/types.go) 的 `Model`、
  [`internal/httpapi/v1.go`](../../internal/httpapi/v1.go) 的 `handleListModels`；对本机 8088 用真实 Key
  实测确认（返回体里没有任何能力字段）。
- [`internal/dshgw/tenancy/render.go`](../../internal/dshgw/tenancy/render.go) 的 `renderSettings` 渲染
  `models: [{id, name}]`，`name` 恒等于 id；路由级只有 `apiKeyEnv` / `api` / `baseURL` /
  `compat.supportsStrictMode`。
- 事实源已存在，只是没有出口：`provider_models.context_window|max_output_tokens|capabilities_json|
  capabilities_override`、`models.display_name`、`models.reasoning_json`。本机运行态实测：
  `deepseek-flash`=1000000/65536 且 `reasoning:true`；`u2-flash`=0/0 且无 reasoning；
  `stealth/union-alpha` 为 `capabilities_override: inherit`。
- 链路：`aigw.Client.ValidateKey` →（worker 启动前 `modelRefreshHook` / `dshgw sync-models` / 门户登录）
  → `EnsureProvisioned`/`SyncModels` → `renderSettings`；dsh 的 `settings-file` 有文件监听，
  改写在下一次请求生效，无需重启。
- 官方文档口径（指南 + 生成的配置目录）：`contextWindow` / `maxTokens` / `input` / `reasoningEfforts`；
  `reasoningEfforts` 的键是 `off|minimal|low|medium|high|xhigh|max`，值是过线拼写，**只有 `off` 允许空值**；
  `input` 只接受 `text`/`image`，省略即回落路由 `defaultInput`（默认 `[text]`）。
- 用**已安装的 0.1.2-rc.1** schema 实测（node 直接 import 其 `Config`）：
  - 本次要渲染的形状被接受（含 `off: none` 与 7 档全表）；
  - `contextWindow: 0`、`maxTokens: 0`、`maxRequestImageBytes: 0` **被拒** → 未知必须**省略**，不能写 0；
  - `input` 出现 `text`/`image` 之外的值被拒；档名不在 7 档内被拒。

## 3. 关键决策

### D1 事实由 `/v1/models` 披露，dshgw 只做映射

网关是能力事实的唯一权威（provider model 声明 + 模型级策略），dshgw 不该自己去查库或复制一套口径，
所以新字段全部加在**数据面的 `GET /v1/models`** 上：任何消费者（dshgw、控制台、第三方客户端）看到同一份
事实，且 dshgw 与 aigw 之间的集成仍然只有公开 HTTP（`internal/dshgw/aigw` 不 import 任何 aigw 实现包）。

**被否决的替代**：把能力塞进 `POST /v1/dshgw/authorize`（那是账号准入的响应，与模型目录无关）；
让 dshgw 直接读 aigw 的库（破坏 `docs/dshgw.md` §1 的双向解耦与契约测试）。

### D2 容量取「候选间最小已知值」，`0 = 未申报」

一个对客模型可能有多条路由，路由是按权重/优先级挑的，没有「按上下文长度过滤」这回事，所以**过报容量
会让 dsh 攒出一段某条路由读不完的历史**。取已申报值的最小值；`0` 表示未申报（网关其它地方也这么读，
如 `maxOutput()`），不参与取值，避免一条手工建的空行把真实值拉成 0。全部未知 → 省略字段，
让 dsh 用适配器默认（262144/32768）。

**被否决的替代**：取最大值（乐观，但混合部署下必然误报）；有任一未知就整体省略（本机
`deepseek-v4.1-flash` 正好长这样：一条 1000000/65536、一条 0/0，整体省略等于白白丢掉已知事实）。

### D3 推理档位：声明了 reasoning 就发全 7 档，`off: none`

`reasoningEfforts` 四态：

| 网关披露 | 渲染 | 理由 |
|---|---|---|
| `capabilities.reasoning: true`，模型策略非 `force` | 全 7 档 `{off: none, minimal: minimal, …, max: max}` | 与 M20 实测通过的 `deepseek-flash` 表一致；`off: none` 让「不选档位」= 显式关思考（M20 §3 实测 `none` 真能关） |
| 同上但 `reasoning.mode: force` | **省略** | 网关会覆盖客户端选择，给出可选的菜单等于说谎；省略后 dsh 不发 reasoning 参数，由网关强制，行为仍然正确 |
| 响应里有 `capabilities` 但没有 `reasoning` | `reasoningEfforts: false` | 官方文档推荐用法：明确声明非推理模型 |
| 响应里没有 `capabilities`（能力未知） | 省略 | 不把「不知道」谎报成「不支持」 |

**被否决的替代**：给每个模型存一张档位表（数据库迁移 + 管理 API + 控制台，M20 已判定收益不抵成本）；
只发「支持 + 默认档」（本次要的正是菜单里的档位入口）。

### D4 图片能力是能力键 `image`，并参与路由能力检查

能力表本来就是「客户端请求的特性必须被候选声明」的字典（`stream`/`tools`/`reasoning`/`json_schema`…），
所以图片用同一词汇：provider model 声明 `capabilities: {…, "image": true}`，请求里带图片时
`featuresOf` 置 `features["image"]`。这样声明与事实一致：`degradation=strip`（本机现状）只打降级标记，
`reject` 才对未声明的候选过滤。

**代价与逃生口**：`reject` 模式下，带图片的请求在没有任何声明图片的路由上会以
`missing_capability:image` 被排除（模型若全无候选则 502）。逃生口是既有的
`capabilities_override: inherit`（解析失败 → 能力未知 → 放行）。本机是 `strip`，行为变化只有
`X-Gateway-Degraded` 与 `usage_records.degraded_features`。

**被否决的替代**：图片只做装饰性披露、不进路由（声明与实际路由脱节，M20 原则里明确反对过报能力）。

### D5 租户 settings.yaml 归 dshgw 渲染，`SyncModels` 整段重写 aigw 段

沿用现状（渲染器已经拥有这一段），因此能力参数随每次同步刷新。副作用要写进文档：**租户手改 aigw 段会
在下次同步丢失**，唯一权威入口是网关控制台的 provider model 声明。

### D6 图片载荷上限：路由级 `maxRequestImageBytes` 由 dshgw 配置决定

dsh 默认 `maxRequestImageBytes` 是 20 MiB，而网关 `server.max_body_bytes` 默认 10 MiB（本机实测
10485760），超出时 `io.LimitReader` 截断请求体 → 客户端看到的是 JSON 解析错误而不是「请求太大」。
所以：只要有模型声明图片，就在 aigw 路由上写 `maxRequestImageBytes`（新配置键
`image_request_max_bytes`，默认 7 MiB），让 dsh 按文档语义把最旧的图片换成占位符，而不是把整个请求
送进一个必然失败的体积。

## 4. 契约一：`GET /v1/models` 的能力扩展

每项新增字段（全部 `omitempty`，纯增量；旧客户端只读旧键，行为不变）：

```json
{
  "id": "deepseek-flash", "object": "model", "created": 1789433433, "owned_by": "aigw",
  "name": "DeepSeek Flash",
  "context_window": 1000000,
  "max_output_tokens": 65536,
  "input_modalities": ["text", "image"],
  "capabilities": {"stream": true, "tools": true, "reasoning": true},
  "reasoning": {"mode": "force", "effort": "high"},
  "x-gateway-pricing": {"currency": "USD", "basis": "cost_follow", "markup_bp": 10000}
}
```

| 字段 | 类型 | 语义 |
|---|---|---|
| `name` | string | `models.display_name`；空则省略（客户端回落 id） |
| `context_window` | int ≥ 1 | 候选已申报值的最小值；**省略 = 未申报** |
| `max_output_tokens` | int ≥ 1 | 同上 |
| `input_modalities` | string[] | 恒含 `text`；任一候选声明 `image` 才含 `image` |
| `capabilities` | object(bool) | 候选声明键的并集（`true` 键）；一个都没有则省略 |
| `reasoning` | object | `{"mode": "default"\|"force", "effort": "…"}`，来自 `models.reasoning_json`；未配置省略 |

聚合位置：[`internal/routing/capabilities.go`](../../internal/routing/capabilities.go)（新文件）
- `EffectiveCapabilities(pm *domain.ProviderModel) map[string]bool`：现 `capabilitiesOf` 的导出改名
  （`capabilities_override` 优先、空/坏 JSON → nil），内部调用点保持一处真源。
- `type ModelFacts struct { ContextWindow, MaxOutputTokens int; Capabilities map[string]bool }`
- `ModelFactsFor(snap *registry.Snapshot, canonical string, candidates []domain.Candidate) ModelFacts`

`handleListModels` 改用 `Router.Plan`（而非 `Candidates`），一次拿到 `Resolved.Canonical`（按规范名查
provider model）与已解析的 `Result.Reasoning`；其余授权过滤、排序、`x-gateway-pricing` 不变。

`featuresOf` 新增：`if req.HasImageInput() { features["image"] = true }`；
`internal/responses/parse.go` 新增 `(*Request).HasImageInput()`，语义是「任一条目的内容里存在
`type == "input_image"` 的部分」（消息 `content` 与工具结果的 `output` 数组都算），实现先用
`bytes.Contains(r.Input, []byte("input_image"))` 预筛再精确核对，避免给每个请求加一次全量解析。

## 5. 契约二：`/v1/models` → 租户 settings.yaml

`internal/dshgw/aigw/client.go`：`ValidateKey` 返回 `[]aigw.Model`：

```go
// Model is one model aigw advertises for a key, with the facts it discloses.
type Model struct {
    ID              string
    Name            string // display name; "" = aigw disclosed none
    ContextWindow   int    // 0 = not disclosed
    MaxOutputTokens int    // 0 = not disclosed
    Images          bool   // input_modalities contained "image"
    Reasoning       bool   // capabilities.reasoning
    ReasoningForced bool   // the model policy forces an effort
}
```

解析容错：除 `id` 外每个字段都是 `json.RawMessage`，逐字段小工具解析（`int ≥ 1`、布尔、字符串数组里只认
`text`/`image`），一行坏数据既不影响其它模型也不影响 Key 校验；老 aigw（无新字段）解析成零值。

`internal/dshgw/tenancy/modelparams.go`（新文件，纯函数）把 `Model` 变成 dsh 条目（官方字段名）：

```yaml
- id: deepseek-flash
  name: DeepSeek Flash
  contextWindow: 1000000
  maxTokens: 65536
  input: [text, image]
  reasoningEfforts:
    off: none
    minimal: minimal
    low: low
    medium: medium
    high: high
    xhigh: xhigh
    max: max
- id: u2-flash
  name: u2-flash（unisound）
  reasoningEfforts: false
- id: stealth/union-alpha
  name: stealth/union-alpha
```

- `name`：有 display name 用它，否则 id（与今天一致）。
- `contextWindow`/`maxTokens`：`≥1` 才写（schema 拒 0）。
- `input`：声明了图片才写 `[text, image]`；纯文本省略，按文档回落 `defaultInput`（默认 `[text]`）。
- `reasoningEfforts`：D3 的四态；档位用固定顺序的结构体渲染（保证 golden 稳定）。

路由级（`aigwProvider`）：`apiKeyEnv`/`api: openai-responses`/`baseURL: <root>/v1`/
`compat.supportsStrictMode: true` 不变；任一模型带图片时加
`maxRequestImageBytes: <image_request_max_bytes>`（默认 7340032）。

签名贯通（`[]string` → `[]aigw.Model`）：`internal/dshgw/tenancy/{render.go,manager.go}`、
`cmd/dshgw/{runtime.go,modelrefresh.go,admin_serve.go,ops.go}`、`internal/dshgw/proxy/proxy.go`
（`Validator` 接口；登录路径只用 `len(models)`）。排序/去重/空列表删除 aigw 段与
`agent-default-model` 清理逻辑不变。

## 6. 数据流

```
aigw registry: provider_models(capabilities/context_window/max_output_tokens)
              + models(display_name/reasoning_json)
        │  GET /v1/models（按 Key 授权过滤 + Plan 的候选集）
        ▼
   aigw.Client.ValidateKey → []aigw.Model
        │  modelRefreshHook（worker 启动前）/ dshgw sync-models / 门户登录
        ▼
   tenancy.EnsureProvisioned / SyncModels → renderSettings
        ▼
   <tenant>/.dsh/settings.yaml  llm-pi-ai.providers.aigw
        │  dsh settings-file 监听（无需重启）
        ▼
   dsh 模型菜单：推理档位入口 / 图片模型可附图片 / 上下文与最大输出按真实值参与压缩
```

## 7. 异常与边界

- 所有新字段 `omitempty`：旧 dshgw + 新 aigw、新 dshgw + 旧 aigw 两个方向都退化成今天的行为。
- 容量为 0（未申报）→ 省略；绝不写 0（已安装 schema 会拒，整段 settings 会被判不可服务）。
- 坏字段/坏行 → 只丢那一项的事实，不影响 Key 校验与其它模型。
- 模型级策略 `mode: default` 的已知交互：一旦声明了档位表，dsh 每次请求都带显式 effort（未选档位时按
  `off: none`），网关的 `default` 策略因此不会生效；要强制请用 `mode: force`（此时 dshgw 不发档位表）。
- 上游拒绝某档（如 `gpt-5.6-luna` 的 `minimal`）：本次不做逐模型档位表，仍由上游如实报错。
- `reject` 模式 + 图片请求：见 D4。
- 同一模型多条路由容量不等：见 D2；想更准就在控制台给 provider model 补 `context_window`。

## 8. 测试策略

- `internal/routing/capabilities_test.go`：并集、最小值、`capabilities_override` 优先、未知 → nil。
- `internal/httpapi/v1_test.go`：`/v1/models` 新字段、未知省略、`reasoning` 披露、授权过滤不变。
- `internal/httpapi/features_test.go`：含 `input_image`（含工具输出里的图片）→ `image` 特性；纯文本 → 无。
- `internal/dshgw/aigw/client_test.go`：能力解析、老响应、坏字段容错、1 MiB 上限与错误语义不变。
- `internal/dshgw/tenancy/render_test.go` + golden
  `internal/dshgw/tenancy/testdata/aigw-settings.golden.yaml`：四态档位、图文模型、未知容量省略、
  无能力时与今天输出一致、保留其它 provider 与顶层键。
- `internal/dshgw/tenancy/settings_schema.test.mjs`（新，`make dshgw-test` 接线）：用**已安装 dsh 自己的**
  `@deepseek-ai/dsh-llm-pi-ai` `Config` schema 校验 golden 里的 `llm-pi-ai` 段——这是「按官方文档配置」
  的硬门：字段名、档名、模态值写错都在这里失败。
- `internal/dshgw/contract/contract.go`：dsh 契约夹具换成带参数的条目，证明真 dsh 能启动。
- 签名跟改：`cmd/dshgw/modelrefresh_test.go`、`internal/dshgw/proxy/proxy_test.go`、
  `internal/dshgw/tenancy/manager_test.go`。

验收命令（本机 `make verify` 会遍历 `./data` 挂住，见 M66 记录，故用显式包）：

```bash
go test ./internal/responses/ ./internal/routing/ ./internal/httpapi/ ./internal/arch/
make dshgw-test && make dshgw-build
KEY=$(sed -n 's/^  AIGW_API_KEY:[[:space:]]*//p' ~/.dsh/.credentials.yaml | head -1)
curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:8088/v1/models | python3 -m json.tool
bin/dshgw --config dshgw.yaml sync-models <tenant>
sed -n '/aigw:/,/agent-default-model:/p' <tenant>/.dsh/settings.yaml
```

## 9. 复验结果（2026-09-20，本机）

| # | 检查 | 结果 |
|---|---|---|
| 1 | `go test -count=1 ./internal/responses/ ./internal/routing/ ./internal/httpapi/ ./internal/providers/... ./internal/arch/` | 通过（httpapi 20.9s） |
| 2 | `make dshgw-test`（go test + 5 个 node 单测/校验 + 2 个 python 计划校验） | 通过；新增 `settings schema check: …/testdata/aigw-settings.golden.yaml is accepted by /home/winger/.local/dsh-0.1.2-rc.1` |
| 3 | `bin/dshgw --config dshgw.yaml contract dsh`（真 dsh 0.1.2-rc.1 起 web worker） | 8/8 通过，含 `credentials-schema-and-mode`（夹具已换成带能力字段的 provider 段） |
| 4 | 用已安装 dsh 自己的 `Config` schema 校验渲染结果（`internal/dshgw/tenancy/settings_schema.test.mjs`） | 通过；负例有牙：`contextWindow: 0` 与未知档名都被 schema 拒绝 |
| 5 | 渲染样例（golden） | `contextWindow: 1000000` / `maxTokens: 65536` / `input: [text, image]` / 7 档 `reasoningEfforts`（`"off": none`）/ `maxRequestImageBytes: 7340032`；未申报的模型只剩 `id`/`name` |

未覆盖（留给 M68 真机验收，见 `docs/TODO.md`）：重启运行中的 8088 实例后，`GET /v1/models` 的实际返回、
`dshgw sync-models` 写出的租户 settings.yaml，以及浏览器里模型菜单的档位入口与图片附件。

## 10. 实现与设计差异

- **`renderSettings` 收 `*config.Config` 而不是 base URL 字符串**：图片载荷上限是第二个部署事实，
  与其再加一个位置参数，不如把配置本身传进来（`cfg.AigwBaseURL` + `cfg.EffectiveImageRequestMaxBytes()`）。
  访问器在未跑过 `Validate` 的配置上也解析默认值——否则测试里手搓的 `Config` 会渲染出「没有上限」，
  静默退回 dsh 自己的 20 MiB，正是这个配置项要消除的东西。
- **`Model` 用 `ReasoningSupported *bool` 表达三态**：解析侧的「三态」比渲染侧更靠前，放在 aigw 客户端
  类型上最自然；渲染侧仍是 D3 的四态（`true` + `force` 与 `true` + 非 `force` 分出两支）。
- **`docs/api-responses.md` 里并不存在的 `GET /v1/models/{id}`**：写规格文档时发现表格里有这一行、
  但 `server.go` 只注册了 `GET /v1/models`；属于既有文档漂移，本次不改（记录在此，避免下次又当成新发现）。
- **`capabilitiesSchema()`（MCP/控制台的能力对象说明）**：顺手补齐了路由真正读取的请求特征键
  （`tools`/`reasoning`/`image`/`json_object`/`json_schema`/`parallel_tools`）——原来只列了插件级键，
  操作者按它声明 `image` 会找不到位置。没有新增 body 字段，因此 `docs/mcp.md` §4.5 的形状要求不变。
- **YAML 里 `off` 被渲染成带引号的 `"off"`**：yaml.v3 按 YAML 1.1 给这类「像布尔的词」加引号；已安装 dsh
  用的 js-yaml 4 按 YAML 1.2 解析，裸写也能得到字符串键。保留引号是给所有解析器的保险，golden 文件里能看到。
- **`maxRequestImageBytes` 的默认值落在 dshgw 配置（7 MiB）而不是 dsh 侧**：网关的
  `server.max_body_bytes` 是部署事实，只有 dshgw 知道（且必须有人把它写进租户文件）。
  没有加 `doctor` 检查——dshgw 读不到 aigw 的配置，假装能校验比不校验更糟，改成在示例配置与
  `docs/dshgw.md` 里写明两者关系。

## 11. 回滚

纯增量：`git revert` 后重跑 `dshgw sync-models` 即恢复今天的 settings.yaml；`features["image"]` 只影响
降级标记与 `reject` 模式的过滤，回滚即消失；无数据库迁移、无协议破坏（新字段全部 `omitempty`）。

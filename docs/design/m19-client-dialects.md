# M19 设计：面向真实客户端的方言翻译（核心只接受，翻译在 provider 层）

> 计划：M19（已批准）。本文是设计记录，实现完成后回填第 7 节差异。
> 触发：把 Codex CLI 指向本网关后无法使用（客户端不允许改动）。

## 1. 目标与非目标

让网关**原样接受**真实客户端发出的、符合 Responses 规范的请求，把"这个上游到底认什么"的判断
下沉到 provider 层。

触发场景是实测出来的：Codex CLI（0.153.4）指向网关后依次撞上三处，每一处都不是客户端的错：

| # | 客户端发出 | 网关/上游反应 |
|---|---|---|
| 1 | `tools[].type = "namespace"`（multi_agent_v1，含嵌套子工具） | 网关 400 `unsupported_parameter` |
| 2 | `tools[].type = "web_search"` | 网关 400 `unsupported_parameter` |
| 3 | `input[].role = "developer"` | 网关放行 → **DeepSeek** 400 `unknown variant 'developer'` |

约束是**不能改客户端**：`namespace`/`web_search`/`developer` 都是 Codex CLI 的默认行为，且都是
Responses 规范内的合法形状。而三种后端对同一语义的方言互不相同，实测对照：

| 语义槽位 | Codex 后端（订阅） | DeepSeek（chat completions） |
|---|---|---|
| 系统级角色 | 拒绝 `system`，接受 `developer` | 拒绝 `developer`，接受 `system` |
| `max_output_tokens` | 拒绝（任何取值） | 接受 |
| 流式 | **强制** `stream: true` | 可选 |

非目标：不追求"网关认识所有工具类型"（类型会继续增加）；不改任何客户端；不为每种方言加开关。

## 2. 关键决策

### D1 核心只做"接受"，不做方言裁决

`internal/responses` 是**面向客户端的表面**：它按 Responses 规范接受请求，不再判断某个工具类型
"本网关是否支持"。原先 `parse.go` 对非 function 类型直接 400，理由是"网关执行不了"——但那把
**上游能力**当成了**客户端合法性**，代价是整条请求失败（而工具是可选的）。

### D2 协议层保真：`pluginapi.Tool.Raw`

未建模的工具类型（`web_search`/`namespace`/…）连结构一起带到 provider：`Tool` 增加 `Raw`
字段 + 自定义 `MarshalJSON`/`UnmarshalJSON`——`Raw` 非空时逐字节原样进出，function 工具仍走
结构化字段。

**取舍**：为什么不是"核心丢弃 + 上报降级"（我最初的实现）。丢弃是不可逆的：`namespace` 里嵌套的
子工具、`web_search` 的 `external_web_access` 开关都会永久消失，将来某个上游支持时也无法利用；
而且"哪些类型能丢"还是核心在替上游做判断，等于把 D1 要消除的东西换了个位置。保真 + 下游决定，
两者都解决了。

### D3 方言翻译归 provider 层

- **chat 方言**（`pkg/providerkit`，被内建 `openai-chat` 与所有 chat 类插件复用）：
  - 角色：把**系统级角色归一化为 `system`**。`developer` 只是它在 OpenAI 新 API 里的名字，而
    OpenAI 兼容上游（DeepSeek、vLLM、Ollama…）普遍只认 `system`。这不是"给某个别名打补丁"，
    而是"按目标方言输出该槽位的规范名"。
  - 工具：chat completions 只能表达 function 工具 → 其余不下发（而不是把 `web_search` 塞进
    function 字段变成一个无名工具，那才是真正的错误翻译）。
- **codex 方言**（`examples/provider-codex`，M10d/M10c 已落地）：`system → developer`、
  丢弃 `max_output_tokens`、恒以流式发送。
- 插件类 provider 通过 `pluginapi.Tool.Raw` 拿到原始结构，自行决定翻译还是丢弃——协议不再替它丢。

### D4 `featuresOf` 只统计可路由的工具能力

`tools` 能力特征改为"存在 function 工具"（`HasFunctionTools()`）：路由的能力校验针对的是
"这个模型会不会用工具"，而本网关无法执行的那些类型不该让候选被判定为缺能力。

## 3. 接口

```go
// pkg/pluginapi/types.go
type Tool struct {
    Type, Name, Description string
    Parameters json.RawMessage
    Strict     *bool
    Raw        json.RawMessage `json:"-"` // 未建模类型：原样进出
}
func (t Tool) MarshalJSON() ([]byte, error)   // Raw 非空即原样输出
func (t *Tool) UnmarshalJSON(data []byte) error // 非 function 时保存原始字节

// internal/responses
type Tool struct { ...; Raw json.RawMessage `json:"-"` } // 同样保存原始字节
func (r *Request) HasFunctionTools() bool
func toolType(raw string) string // "" 视为 function

// pkg/providerkit：ResponsesToChat 内部
//   role "developer" → "system"
//   非 function 工具不下发
```

## 4. 数据流

```
客户端（Codex CLI，零改动）
  → internal/responses：接受全部形状；function 工具结构化、其余带 Raw 原样透传
  → routing：features["tools"] 仅由 function 工具决定
  → provider 层按目标方言处理：
       openai-chat：developer→system，非 function 工具不下发
       codex 插件：system→developer，丢 max_output_tokens，流式
  → 上游
```

## 5. 异常与边界

1. **function 工具缺 name**：仍是 400（`tools[i].name`），这是客户端错误且能精确定位。
2. **只有非 function 工具**：`tools` 能力不置位；chat 路径不下发任何工具；请求照常完成。
3. **provider 忽略 `Raw`**：等价于丢弃该工具，请求仍可完成——最坏情况退化为原先的"剥掉"，不会 400。
4. **`Raw` 与结构化字段并存**：`MarshalJSON` 以 `Raw` 为准（未建模类型本就没有结构化字段）。
5. **协议编解码**：`json:"-"` 保证 `Raw` 不出现在结构化分支里，避免把原始字节再包一层。

## 6. 测试策略

- `internal/responses/parse_test.go`（新增）：`web_search` + `namespace` + `function` 混合请求 →
  解析通过、三个工具都在、前两个 `Raw` 与客户端字节逐字相同且未被折成 function、function 仍结构化；
  function 缺 name 仍 400；`HasFunctionTools` 忽略未建模类型。
- `pkg/pluginapi`：`Raw` 经协议往返逐字节不变；function 工具仍输出结构化 JSON。
- `pkg/providerkit`：`developer → system` 且其它角色不动；非 function 工具不下发。
- `internal/httpapi/v1_test.go`：删掉"非 function 工具必须 400"这条（行为已变更）。

端到端验收：**Codex CLI 零改动**（`multi_agent`、`web_search` 保持默认开启）指向网关、
模型 `deepseek-flash`，应正常出字且退出码 0。

## 7. 实现与设计差异

1. **端到端验收通过（Codex CLI 零改动）**：`multi_agent`、`web_search` 均保持默认开启，只把
   `base_url` 指向新网关、模型设为 `deepseek-flash`：
   ```
   codex
   1+1 等于 2。
   tokens used  5,645
   退出码       0
   ```
   此前同一调用会在网关侧 400（`unsupported_parameter`）；关掉那两个工具后又会在上游 400
   （`unknown variant 'developer'`）。两处都已在**网关侧**解决，客户端一行未改。

2. **实现与设计一致**：`Raw` 保真（协议编解码两处）、核心只接受、方言翻译下沉到 `providerkit`
   与 codex 插件、`featuresOf` 改用 `HasFunctionTools()`。

3. **`namespace` 的保真在真实报文上验证过**：抓取 Codex CLI 的原始请求（记录型假上游）确认它发的是
   `{"type":"namespace","name":"multi_agent_v1","tools":[ … 嵌套 function … ]}` 与
   `{"type":"web_search","external_web_access":false}`；新实现下两者都以客户端原始字节传给 provider。

4. **观察项（未处理，非阻塞）**：Codex CLI 在这次成功运行的 stderr 里刷了 7 条
   `codex_core::util: OutputTextDelta without active item`。我核对了网关的输出：事件顺序是规范的
   （`response.created` → `in_progress` → `output_item.added` → `content_part.added` → 多个
   `output_text.delta` → `done` → `completed`），且 delta 的 `item_id` 正是 added 过的那个；
   答案与退出码均正确。判断为客户端侧的噪声或它对某类流式形状的额外期待，已记入 `docs/TODO.md`
   待观察，不在本里程碑内追查。

5. **没有为降级新增协议通道**：原先的实现把"被丢弃的工具类型"上报为 degraded feature；改为保真透传后，
   "丢不丢"由 provider 决定，而协议里**没有** provider 上报降级的字段（`DegradedFeatures` 目前只由
   路由的能力校验写入）。本次不做协议扩展，工具被 provider 跳过的可见性作为后续议题记入 TODO。

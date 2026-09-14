# M45 设计：输入项的显式空值保真（codex 长上下文 400 的根因）

> 前置：`docs/codex-input-array-fix.md`（数组型工具输出）、`docs/TODO.md` 的
> 「gptjp 插件版本错配修复」（`additional_tools.tools` 被吞导致 `input[0].tools`）。
> 本文是设计记录，实现完成后回填第 8 节差异。

## 1. 现象与证据

用户报（2026-09-14）：codex 里上下文一长就断流，
`stream disconnected before completion: missing_required_parameter: Missing required parameter: 'input[N].summary'.`

**线上证据（gptjp，用户 codex 走的那台）**

- 17:48:06–17:49:30 同一会话 `01a09f08-03f3-7721-8047-d10cb870d17d` 连续 **6 次**失败，
  工作区 `D:\code\python\售后系统`（与截图里那条消息一致），模型 `gpt-6-astra`，
  请求体 **1,323,524 字节**（每次完全相同：客户端在重试同一个请求）。
- `usage_records`：`error_code=missing_required_parameter`、`terminated_reason=upstream_error`、
  `latency_ms` 0.5–0.6 s —— 上游**校验拒绝**，不是超时、不是过载。
- 当天同实例同类失败 **26 条**，分四组（11:40 / 14:13 / 16:23 / 17:48），**全部 `client=codex`**；
  最长一组同一会话连续失败 10 次。请求体从 56 KB 到 1.48 MB 都有，
  但**每一组都是"同一份请求反复重试"**——历史里一旦有了坏项，后续每次重试都失败。

**客户端原样字节**（codex app 自己的 rollout 记录，`~/.codex/sessions/2026/09/10/*.jsonl`）：

```json
{"type":"reasoning","id":"rs_0c79a075e204c247016aa1ffce676c87d0a2eafec3280392fc",
 "summary":[],"encrypted_content":"gAAAAABqof_P98vX…","internal_chat_message_metadata_passthrough":{…}}
```

`summary` 是**显式空数组**（订阅后端在没有摘要文本时就是这么返回的），不是缺键。

**本地复现**（`pkg/pluginapi` 往返）

```
in : {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"abc"}
out: {"encrypted_content":"abc","id":"rs_1","type":"reasoning"}      ← summary 被抹掉
```

**线上复现**（gpt001，真上游，含 summary/encrypted_content 的完整历史项）

| 形态 | 上游裁决 |
|---|---|
| `summary: []`（codex 的形状） | `missing_required_parameter: Missing required parameter: 'input[1].summary'.` |
| `summary` 非空 | 越过 summary 校验 → `array_above_max_length: Invalid 'input[1].content': array too long. Expected an array with maximum length 0, but got an array with length 1 instead.` |
| 带 `status`（网关自己的输出项就带） | `unknown_parameter: Unknown parameter: 'input[1].status'.` |

## 2. 根因

`pluginapi.Item.Summary` 是 `[]SummaryPart` + `json:"summary,omitempty"`。
Go 的 `omitempty` 不区分"空 slice"和"nil slice"：**客户端显式发的 `"summary":[]`
在网关 → 插件这一跳被抹掉了**。codex 订阅后端把 `summary` 当 reasoning 输入项的
**必填**字段，缺键即 400。

于是故障形状完全对得上：

- 某一轮里模型没产出摘要文本 → 该 reasoning 项的 `summary` 是空数组；
- codex 在后续轮次把它原样放回 `input`（它按自己的 serde 序列化，空 vec 也写键）；
- 网关抹掉键 → 上游 400；
- 该项留在历史里不再消失，**该会话此后每个请求都失败**，而会话越用越长、进程内积累的
  reasoning 项越多 —— 用户看到的就是"长一点的上下文就出错"。

同类先例（同一类"网关把客户端显式给的形状改小/改没了"）：
`additional_tools.tools` 被吞（`input[0].tools`，见 `docs/TODO.md`）、
数组型工具输出被拒（`docs/codex-input-array-fix.md`）。

## 3. 关键决策

**D1 核心层只做保真，不发明字段。**
`Item` 记录"哪些键在输入 JSON 里出现过"，Marshal 时对**空值**把这些键补回来
（`summary` → `[]`，`arguments`/`output` → `""`）。

- 取舍：另一种做法是给 `Summary` 去掉 `omitempty`，让每个项都带 `"summary":[]`——
  但该后端对多余字段同样报错（`unknown_parameter`，本次实测），
  message/function_call 项会被自己的网关注成非法请求。所以必须按「出现过的键」而不是「所有键」。

**D2 覆盖字段：`summary`、`arguments`、`output`。**
三者都是条目类型里的必填字符串/数组，客户端可能显式发空
（无参数工具调用 `"arguments":""`、空文本工具结果 `"output":""`）。
`content` 不需要：它是 `json.RawMessage`，`[]` 的字节长度非 0，`omitempty` 丢不掉它。

**D3 插件侧按该后端的输入 schema 规范化 reasoning 项。**
`provider-codex` 增加 `normalizeInputItems`：

- 补 `summary: []`——客户端连键都没发时也补（上游必填；空值不承载信息）；
- 去 `content`——该后端输入 reasoning 的 content 上限是 **0**（实测）；
- 去 `status`——输出字段，该后端对输入项里的 status 报 `unknown_parameter`（实测）。

理由：**网关自己的输出项就带 `content` 与 `status`**，客户端把收到的项原样回灌是
很自然的写法。把网关自己造出来的形状当成非法输入拒掉，是网关的锅，不是客户端的。
（与既有 `rewriteSystemRoles`、`explicitToolStrictness` 同一位置、同一性质。）

**D4 本次不改 `include`。**
客户端的 `include: ["reasoning.encrypted_content"]` 目前不会下发
（`pluginapi.Request` 根本没有该字段），所以经网关拿不到 `encrypted_content`。
它与本次 400 **无关**（复现里只缺 summary 键就足以 400），但会让 codex 的无状态续接
（chain-of-thought）退化，另行评估，记入待办。

## 4. 接口与数据流

- `pkg/pluginapi.Item`：新增未导出的出现集合（`sent`）+ 在 `UnmarshalJSON` 里记录 +
  在 `MarshalJSON` 里回填空值。对外 JSON 形状**只在"客户端发过空值"这一种情况下变化**。
- 数据流：
  `客户端 → /v1/responses → responses.Items() → pluginapi.Item(记 sent)`
  `→ 宿主帧 JSON(回填空值) → 插件 Item(再记 sent) → provider-codex.buildRequest(规范化) → 上游`
- 输出方向同样受益：上游 item（`Raw`）经 SSE 回给客户端时，`"summary":[]` 不再被抹掉，
  客户端收到的形状与上游一致。

## 5. 异常与边界

- 客户端完全没发 summary 的 reasoning 项 → 插件补 `[]`（D3），不丢任何历史信息。
- message / function_call 项不受影响：没发过的键不会凭空出现。
- `Extra`（`encrypted_content`、`internal_chat_message_metadata_passthrough` 等）与
  `OutputContent` 的既有保真逻辑不变；`itemJSONFields` 仍是 Extra 的排除表。
- 空 `output` 与 `OutputContent` 同时存在时以 OutputContent 为准（既有语义）。
- 插件与网关**必须同版本部署**：保真逻辑两侧都跑；旧插件 + 新网关仍能工作
  （新网关会把空值补齐后写进帧），新插件 + 旧网关则会退回旧行为（插件补 summary 可兜底）。

## 6. 测试策略

- `pkg/pluginapi`：往返保真——`summary` 空数组 / 非空 / 缺失、`arguments` 空串、
  `output` 空串、message 项不长出 `summary`、`Extra` 与 `OutputContent` 不回归。
- `examples/provider-codex`：`normalizeInputItems` 单测（补 summary、去 content/status、
  其他项原样、不改调用方切片）+ 既有 `buildRequest` 测试。
- `internal/responses`：客户端输入项 → provider 请求的保真用例（字节在网关这一跳不被抹平）。
- 线上验收：同一实例、同一历史项形状，修复前 400 / 修复后 `completed`；
  再用真实 DSH 请求确认主客户端不受影响（回归）。

## 7. 依赖

无新依赖。

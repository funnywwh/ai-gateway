# M10d 设计：codex 适配器把 `system` 角色翻译为 `developer`

> 计划：M10d（已批准）。本文是设计记录，实现完成后回填第 7 节差异。
> 前置：`docs/design/m10-subscription-adapter.md`（适配器本体）。

## 1. 目标与非目标

让 codex 适配器接受**客户端把系统提示词作为 `input` 里 `role: "system"` 消息项**这一（合法且常见的）
写法。当前该写法会被订阅后端直接拒绝：

```
HTTP 400  {"detail":"System messages are not allowed"}
```

**为什么必须修**：DSH（DeepSeek Harness）就是以这种形状发请求的，而它是本项目的主要客户端。
换言之，**凡是把系统提示词作为 system 消息发送的 agent 类客户端，目前完全无法使用 codex 供应商**——
不是偶发失败，而是整条链路不可用。实测数据佐证：最近 18 条请求中 5 条带 system 项，走 codex 的全部失败，
同形状走 `replay` 的全部成功（说明不是网关问题，是后端约束）。

非目标：不改客户端的请求形状；不做网关级的角色翻译层；不改 `instructions` 的既有语义。

## 2. 实测矩阵（决定方案的关键证据）

对订阅后端逐一实测（同一模型、同一代理、同一凭据）：

| # | 形态 | 结果 |
|---|---|---|
| 1 | `developer` 在首位 | **200** |
| 2 | `developer` 在中间（用户消息之后） | **200** |
| 3 | `developer` 带 `tools` | **200** |
| 4 | `developer` 在末尾 | **200** |
| 5 | `system` 在首位 | 400 `System messages are not allowed` |
| 6 | `system` 在中间 | 400 同上 |
| 7 | `system` 与 `instructions` 并存 | 400 同上 |
| 8 | `instructions` + 用户输入 | **200** |
| 9 | 只有 `instructions`、无 `input` | 400 `Missing required parameter: 'input'.` |

结论：**`system` 在所有位置都被拒；`developer` 在所有位置都被接受**（含带 tools）。
注意第 9 条：`input` 必须存在且非空。

（过程备注：首轮脚本里 JSON 变量漏了外层花括号，导致全部形态统一报 `{"detail":"Bad Request"}`——
那是**报文非法**造成的假象，不是上游行为变化。修正后得到上表。这类"看起来像上游变了"的假信号，
正是需要先自证报文合法性的原因。）

## 3. 关键决策

### D1 把 `role: "system"` 就地改写为 `role: "developer"`

`developer` 是 OpenAI 对 `system` 角色的**新名称**（同一语义槽位），后端实测在任意位置都接受。
因此这是一次**纯角色重命名**：不解析内容、不合并消息、不改变顺序。

**取舍——为什么不是折叠进 `instructions`**（这是我最初的方案）：

| | 角色重命名（采用） | 折叠进 `instructions`（否决） |
|---|---|---|
| 消息位置 | **保留**（多轮里 system 出现在中间也能保序） | 被提升为全局槽位，位置信息丢失 |
| 内容处理 | 不需要（不碰 content 结构） | 需要解析 `content` 各部件并拼接 |
| `input` 是否可能变空 | **不会**（长度不变） | 可能——若 system 是唯一输入项，折叠后 `input` 为空，触发矩阵第 9 条的 400 |
| 多条 system 的处理 | 各自保留 | 需要人为约定拼接顺序与分隔符 |

角色重命名在四个维度上都更简单且更忠实，故采用。

### D2 只改 `input` 顶层项的 `role`，其余一律不动

`user`/`assistant`/`tool` 输出项、`developer`（已经是新名字）、`instructions` 字段全部原样透传。
将来若后端开始接受 `system`，这次改写仍然无害——`developer` 本就是该角色的现行名称。

### D3 不修改调用方的请求对象

`buildRequest` 收到的是网关传下来的 `*pluginapi.Request`，网关可能复用（重试、录制）。
因此改写作用在**切片的副本**上，且在没有 `system` 项时不产生任何拷贝。

### D4 不做成可配置开关

该适配器专用于此订阅后端，而 `developer` 对官方 Responses API 同样合法。加开关只会多一种
"配错了就整条链路不可用"的状态。

## 4. 接口与实现

`examples/provider-codex/main.go`：

```go
// rewriteSystemRoles 返回一份副本，把 role 为 "system" 的输入项改名为 "developer"。
// 不改动调用方的切片：网关可能复用该请求。
func rewriteSystemRoles(items []pluginapi.Item) []pluginapi.Item
```

`buildRequest` 中把 `Input: req.Input` 换成 `Input: rewriteSystemRoles(req.Input)`（D1/D2/D3）。

## 5. 数据流

```
客户端（DSH）  input=[{role:"system",...},{role:"user",...}]
  → 网关原样透传（见 docs/api-responses.md「输入项类型」）
  → 适配器 buildRequest → rewriteSystemRoles → 上游 input=[{role:"developer",...},{role:"user",...}]
  → 200（此前为 400 System messages are not allowed）
```

## 6. 测试策略

`examples/provider-codex/main_test.go`：

1. `TestSystemRoleIsRewrittenForTheUpstream` —— 捕获上游收到的报文，断言第一个输入项角色为
   `developer`；同时断言**调用方请求未被修改**（仍为 `system`，守住 D3）；并带上 `tools`
   以复刻真实失败请求的形状。
2. `TestOtherRolesAreLeftAlone` —— `developer`/`user`/`assistant` 混合输入时角色全部不变。

回归：既有插件用例与全仓库测试保持全绿；`make verify`。

端到端验收（真实上游）：用 DSH 那两个真实失败请求的形状（system 项 + tools + `max_output_tokens`）
经网关请求 `gpt-5.6-luna`，应返回正常文本而非 400。

## 7. 实现与设计差异

1. **实现与设计一致**：`rewriteSystemRoles` 按 D1–D3 实现——就地改角色名、不改其它角色、无 system 项时
   直接返回原切片（不产生拷贝）、有 system 项时在副本上改写（调用方请求不被修改）。变异验证：把
   `buildRequest` 里的改写去掉后，`TestSystemRoleIsRewrittenForTheUpstream` 精确失败（
   `upstream role = "system", want developer`），恢复后全绿——说明用例不是空转。

2. **端到端实测通过（真实上游）**：用 DSH 那两个真实失败请求的形状（`input[0].role=system` + `tools` +
   `max_output_tokens`）经网关请求 `gpt-5.6-luna`，**3/3 返回 200** 并给出正常中文回答；同报文直连上游
   **2/2 返回 200**。此前该形状必然 400 `System messages are not allowed`。

3. **语义确实被保留，不只是"骗过校验"**：系统提示词写的是 "You are a helpful software engineer assistant."，
   模型回答把自己描述为"由人工智能驱动的**软件工程助手**"。如果只是丢弃或吞掉 system 消息，模型不会这样回答——
   这是 D1 选择"改名"而非"丢弃/提升"的直接收益。

4. **修复后曾出现一次 `server_is_overloaded`，与本改动无关**：首次请求遇到上游瞬时过载。该错误以 SSE
   `error` 事件的形式到达，`translate` 的 `case "error"` 会**如实沿用上游自己的错误码**（因此库里记的是
   `server_is_overloaded` 而不是 `upstream_5xx`），重试即成功。记录在此以免被误归因于角色改写。

5. **横向对照（同形状换供应商验证）**：同一条 DSH 形状的请求打 `deepseek`（`openai-chat` → api.deepseek.com）
   **不再出现角色问题**，说明"system 被拒"是该订阅后端特有的约束，翻译放在**适配器**（而非网关或客户端）
   是正确的层次：网关只管透传，每个适配器负责自己后端的方言。

6. **验证过程中顺带发现一个与本里程碑无关的配置问题**（已记入 `docs/TODO.md`）：deepseek 供应商的
   `config.response_format = "json_object"` 是**供应商级全局套用**（`internal/providers/openaichat/openaichat.go:432`
   无条件写入，且该 provider 从不读取请求里的 `text.format`），导致任何不含 "json" 字样的提示词都被
   DeepSeek 拒绝：
   `upstream_400: Prompt must contain the word 'json' in some form to use 'response_format' of type 'json_object'.`
   这会让该供应商只能服务 JSON 类请求，普通流量全部 400。

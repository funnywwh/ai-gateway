# 修复：DeepSeek `/responses` 的流式思考在网关上被丢掉（2026-09-18）

## 1. 现象

`deepseek` 供应商（本机 `:8088`，provider id=3）2026-09-15 从 `openai-chat` 改成
`kind: openai-responses` 之后，客户端看不到任何思考：

- DSH 会话（当前这一条，`client=aigw` / `model=deepseek-flash`）47 条 assistant 消息里
  **块类型只有 `tool-call`，一个 reasoning 块都没有**（`~/.dsh/sessions/.../session.jsonl.zstd` 逐块统计）；
- 同一条供应商的**非流式**记录里思考是完整存在的：`responses` 表（provider_id=3）的
  `output_json` 里有 `{"type":"reasoning","id":"rs_bob43…","content":[{"type":"reasoning_text","text":"…"}]}`。

也就是说：**上游确实在思考，只有流式客户端看不到**。这解释了为什么这个缺陷能长期不被发现——
落库的正文里有思考，看库的人以为一切正常，而 DSH / Codex 都是流式客户端。

## 2. 根因

DeepSeek 的 `/responses` 把思考正文发在 **`response.reasoning_text.delta`** 上（reasoning 项的
`content: [{type: "reasoning_text"}]` 形状）；OpenAI 系的订阅后端发的是
`response.reasoning_summary_text.delta`（摘要语义）。而
`internal/providers/openairesponses/openairesponses.go` 的流式分支只认后一个：

```go
case "response.reasoning_summary_text.delta", "response.reasoning.delta":
```

于是 DeepSeek 的思考增量**一个都没进**网关的事件流：宿主组装器（`internal/responses/assembler.go`）
只在收到 `reasoning.delta` 时才建 reasoning 项，没有增量就没有思考块，客户端自然什么都看不到。

## 3. 修复

`internal/providers/openairesponses/openairesponses.go` 的流式分支补上这个名字，并把上游的
`item_id` 一起带上：

```go
case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.reasoning.delta":
    if err := emit(pluginapi.Event{
        Type: pluginapi.EventReasoningDelta, ItemID: frame.ItemID, Text: frame.Delta,
    }); err != nil {
```

两点取舍：

1. **三个事件名并列，不做开关**。它们表达的是同一件事（上游在流式输出思考），差别只是摘要还是正文；
   不同上游用哪个名字是上游的方言，网关的职责是都听懂。既有订阅后端走的是 summary 名，行为不变。
2. **带上 `item_id`**。这是 M47（条目身份保真）在思考路径上的延伸：客户端回灌上一轮的 reasoning 项时，
   带的是它收到的那个 id。如果网关自造 id（`rs_<随机>`），无状态上游会回
   `Item with id 'rs_…' not found`——M47 在 gptjp 上踩过的正是这个。上游没给 id 时仍走兜底生成，
   `itemID(ev.ItemID, ids.Reasoning)` 的行为不变。

## 4. 测试

- **单元**：`internal/providers/openairesponses/stream_test.go`
  - `TestStreamRelaysBothReasoningDeltaDialects`：三个事件名逐个验，断言**第一个**事件是 reasoning 增量、
    文本完整、`item_id` 是上游的，且正文增量排在其后（客户端按 output_index 建块，顺序反了就得丢）；
  - `TestStreamKeepsThinkingWithoutAnItemID`：上游不给 id 时照旧工作，id 留给宿主生成。
  - **变异验证**：把 `response.reasoning_text.delta` 从 case 里删掉，两个测试**精确失败**
    （`first event = {Type:text.delta …}` / 事件流里只剩 finish+usage），恢复即通过。
- **端到端**：新增 `scripts/responses-thinking-smoke.sh`（真实 `bin/aigw` + 假 `/responses` 上游，
  按 DeepSeek 的事件顺序发流：reasoning 项先 added、正文按 `response.reasoning_text.delta` 流出、
  然后才是回答的 message 项）。断言客户端实际收到的 SSE：
  - `response.output_item.added(type=reasoning)` 出现在它自己的 delta **之前**；
  - `response.reasoning_summary_text.delta` 拼起来等于完整思考正文，`item_id` = 上游的 `rs_upstream`；
  - 思考整体在正文之前，流以 `response.completed` 收尾；
  - 非流式路径仍返回 `[reasoning, message]` 两项（回归：这条路本来就对）。

实测输出（本机，2026-09-18）：

```
  response.output_item.added         {"item": {"type": "reasoning", "id": "rs_upstream", …}}
  response.reasoning_summary_part.added
  response.reasoning_summary_text.delta   ×2
  response.reasoning_summary_text.done
  response.output_item.done          {"item": {"type": "reasoning", "id": "rs_upstream", …}}
  response.output_item.added         {"item": {"type": "message", …}}
  response.output_text.delta         "4"
  response.completed

ok: DeepSeek's response.reasoning_text.delta reaches a streaming client as reasoning
    events (item added first, upstream id kept, thinking before the answer)
```

- **既有走查回归**：`scripts/format-smoke.sh`（response_format 语义）、`scripts/deepseek-smoke.sh`（离线
  openai-chat 接入）、`scripts/codex-input-fidelity-smoke.sh`（M47 条目身份与加密块）全绿；
  `go test ./internal/... ./pkg/...`、`go vet`、`gofmt` 干净（`make verify` 通过）。

## 4.1 真实 DSH 客户端的前后对照（同一提示词、同一假上游）

只测网关发的事件还不够——要证明的正是「DSH 能看到思考」。用**真实 DSH 二进制**
（`dsh --profile headless`，隔离的 `DSH_HOME` + 指向临时网关的 `llm-pi-ai.providers.aigw` 配置）
打同一套假 `/responses` 上游，读它自己的会话记录（`sessions/*/session.jsonl.zstd` 的
`assistant/chunk.block-start` 块类型）：

| 二进制 | DSH stdout | DSH 会话里的 assistant 块 |
|---|---|---|
| 修复前（`git show HEAD` 版源码编出的对照二进制） | 只有 `4` | `{text: 1}` —— **思考整块不存在** |
| 修复后 | `dsh: reasoning:` / `先看天气` / `4` | `{reasoning: 1, text: 1}`，块内容 `{"type":"reasoning","text":"先看天气"}` |

对照实验就是本次诊断里那次统计（本机 DSH 会话 47 条 assistant 消息只有 `tool-call` 块）在受控环境下的复现与消除。
对照二进制用 `make build-src` 生成（不碰 `bin/aigw`），实验后已删除；临时目录与测试进程已清理，
两个测试端口已关闭。


## 5. 影响面与未做

- 影响面只有 `openai-responses` 一系的流式思考：**OpenAI / 订阅后端不会变**（它们用 summary 名，
  行为逐字不变）；`openai-chat` 路径不受影响（它有自己的 `reasoning_content` 翻译）。
- 同一供应商还剩两处已知缺口（本次**未做**，仍在 `docs/TODO.md`）：
  1. `usage.output_tokens_details.reasoning_tokens` 没被读取 → `/responses` 的 usage 只有 input/output，
     计费少一个 `reasoning` 维度（思考 token 按 output 价少收一次）。属计费口径，需单独评审；
  2. 思考正文的承载字段：本 provider 转发的是 `reasoning_text` 内容项，网关自己产出时写
     `content` + `summary` 两份（见 `assembler.addReasoning`），已足够回放，未再改动。
- **部署尚未生效**：修复在 `bin/aigw` 里，但本机 `:8088` 上运行的实例是修复前启动的进程，
  要重启才吃到（重启会打断在用的人，本次未擅自做）。

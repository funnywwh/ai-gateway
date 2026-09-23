# M83 设计文档：输入档只留「人说的话」（样板按标记跳过，长度只作防呆上限）

> 状态：已实现（本文件在实现前已在对话中输出并通过评审）。
> 规格同步：`docs/request-log.md`、`config.example.yaml`、`docs/mcp.md`、`docs/api-responses.md`、`README.md`、
> `docs/TODO.md`。取代关系：**再次修订 M82/M82.1**（它们按「长度 ≤N」判定），本文把判定依据换成**样板标记**。

## 目标

2026-09-23 的现场：某浏览器 DSH 租户（`dshgw-dsh-chengjinfeng-8e59`）的**每一条请求日志正文都是空的**——
阈值生效（14:11）之前它有 352 行、0 空；之后 71 行、**71 空**。查它的会话文件（只读长度与开头标记）：

| 它的 user 消息 | 字符数 |
|---|---|
| 人问的那句 | 117 / 243 / 360 / 521 |
| runtime context 快照 | 542 / 914 |
| 系统提示那类（`You are …`） | 2167+477 / 2791+477 … |
| 回放里的长粘贴 | 58+9855 / 58+18745 … |

**没有一条 ≤100**，所以「只留 ≤100 字符的最新一条」在那台机器上永远留不下任何东西。而同一个仓库里，
我自己的 DSH 会话（提问 2–44 字符）行行都有正文——所以这不是代码坏了，是**用长度去区分「样板」与「人话」
本身不成立**：真实提问可以比短的样板长、比长的样板短（117 字符的提问 vs 100 字符的阈值 vs 542 字符的样板，
三个量谁大谁小都说不准）。更糟的是「把阈值调大」并不解决问题：末尾那条 542 字符的样板会先被选中落库。

本次把判定换成**标记**：

- **保留**：从末尾往回找，第一条同时满足「有文本（`TrimSpace` 后非空）」「不是样板」「不超过
  `recording.input_max_chars`」的 user 消息，**整条原样**保留（不截断）。
- **样板**（按消息开头判定）：`Current runtime context.`（DSH runtime 快照）、`<environment_context>`
  （Codex 环境块）、`You are an AI agent powered by DeepSeek Harness.` 与
  `You are a coding agent running in the Codex CLI`（两个 agent 自己的系统提示）、`<system-reminder>`、
  `<skills_instructions>`、以及两个标题调用的提示词（`Generate the session title from this JSON array…` /
  `You are a helpful assistant. You will be presented with a user prompt…`）。这份词表**复用身份提取器
  （`internal/responses/dimensions.go`）已有的常量**，不新造一套。
- **上限**：`recording.input_max_chars` 默认从 100 改成 **2000**，角色从「判定器」变成「防呆」——它只挡住
  「整份文件粘贴」这类超大消息（`0` = 不限长度，样板仍然跳过）。
- 空消息与超长消息都**跳过并继续往前找**；一条都没有 → 正文为空（行照写，`request_bytes` 照记）。
- `full` / `metadata` / `off` / 控制台流量 / 上游转发 / 数据库 schema：全部不变。

验收标准：

- 该租户（以及任何提问 >100 字符的会话）在部署后**有正文**，内容正是那句人工提问；
- 样板在任何长度下都不落库（含 `0` = 不限长度时）；
- 一条都不合格时行为与 M82.1 相同：空正文 + 身份与体积仍在；
- `full` 仍是「保留全部」的出口，且不受上述任何规则限制。

## 关键决策

1. **判定依据从长度换成标记**。理由是上表：长度无法区分，标记可以。代价是词表要维护：新客户端出现新样板
   （或旧客户端改前缀）时要补一条；不补的后果是**多记**（样板被当成用户输入），而不是少记——这是有意的
   安全方向。词表集中在 `boilerplatePrefixes` 一处，且**只判开头**（消息中间提到 `Current runtime context`
   的人话不会被误杀，测试钉住）。
2. **上限只作防呆，默认 2000**。真实提问 117–521 字符必须能落库；而 18KB 的回放粘贴不该成为日志正文。
   2000 字符≈一页散文，远大于任何提问。仍然是「要么整条留、要么不留」，不截断。
3. **跳过而不是停住**：空消息、超长消息、样板都继续往前找（沿用 M82.1 的选择）——这样「末尾是样板」这个
   DSH 的常态不会让整行失去正文。
4. **`0` 不清除样板过滤**：`0` 的语义是「不限长度」，而不是「什么都记」。把 agent 自己的脚手架当成
   「用户说了什么」记进日志，正是这条策略存在的理由。
5. **不改正文形状**：仍是纯文本、仍是 `request_json` 一个 TEXT 列，因此 MCP 的 string/object 判定、
   详情接口的 `record_input_mode`、`/stats` 都不需要动；只有控制台「未保留」的说明文案改成
   「只有样板或超长用户消息」。

## 接口

```go
// internal/responses
var boilerplatePrefixes = []string{ dshRuntimePrefix, codexEnvContextTag, dshDeveloperPrefix,
    codexInstructionPrefix, dshTitleUserPrefix, codexTitleUserPrefix, dshTitleSystemPrefix,
    "<system-reminder>", "<skills_instructions>" }
func boilerplateText(text string) bool

type recordedUserMessage struct { text string; runes int; readable, boilerplate bool }
func (m recordedUserMessage) carriesText() bool

// UserInputText(maxChars int) (string, error)：从末尾往回找，跳过
// !carriesText() || boilerplate || runes > maxChars 的消息，返回第一条的全文。

// internal/config：InputMaxChars 默认 100 → 2000（键名与校验不变）
```

## 数据流（`user` 档）

```
UserInputText(recording.input_max_chars)
  ├─ 无 user 消息 → ""
  └─ 从末尾往前：
       空/纯空白/只有图片/读不懂   → 跳过
       以样板标记开头             → 跳过
       字符数 > 上限              → 跳过
       否则                       → 该消息全文（多 part 用 "\n" 连接）  ← 命中即返回
  └─ 一条都没有 → ""
```

## 异常与边界

- 消息中间（非开头）提到样板文字 → 不算样板（测试覆盖）。
- 人话恰好等于上限 → 保留（`<=`）；上限 0 → 不限长度。
- 只有样板、只有空消息、或全部超长 → 正文为空；行照写。
- 新客户端的新样板：**多记**不会少记；发现后补词表即可。
- 历史行（M23–M82 期间）不回填；控制台/MCP 两种形状都能读。
- `max_bytes` 兜底、`redact_paths` 的 `input` 兼容规则、身份维度口径：均不变。

## 测试策略

- `internal/responses`：① 现场回归（117 字符提问 + 542 字符 runtime 快照 → 记提问）；② 八个客户端的样板各一条
  （DSH runtime/系统提示、Codex 环境块/指令、system-reminder、skills、两个标题提示词）→ 全部跳过、记前面的人话；
  ③ 超过上限的人话被跳过并回退到更早的人话；④ `0` 时样板仍跳过；⑤ 只在中间提到标记的人话不被误杀；
  ⑥ 既有的空消息/边界/多 part/字符串简写/不可读 content 用例保持。
- `internal/httpapi`：端到端加两条——现场回归（117 字符提问在样板之后也必须落库）、只有样板 → 空正文；
  既有断言（默认档取人话、空白尾消息回退、超长回退、`full` 保留全部）保持。测试改用
  `postAndLog` 辅助（排空响应 + 轮询等行），因为大响应下「关闭 body 不读」会让断言与 handler 的收尾抢跑。
- `internal/config`：默认值断言 100 → 2000。
- `internal/webui/tests/requests_test.mjs` + `scripts/ui-harness/keys.page.html`：「未保留」新文案。
- `make verify` 等价的 go 命令 + `make ui-base` + `make build` 全绿。

## 依赖

标准库 + 既有包；不新增依赖、不改 schema。

## 环境说明（本次实现）

独立工作区 `ai-gateway-m83`（分支 `m83-human-only`），Go/node 复用主工作区工具链与缓存；
`make ui-check` 仍按设计在沙箱里跳过（firefox 是 snap 包装器）。

## 实现与设计差异

（实现完成后回填。）

## 实现与设计差异

- **测试辅助 `postAndLog`**（计划外）：写端到端用例时发现，大响应下「`resp.Body.Close()` 之后立刻查日志」
  会与 handler 的收尾抢跑——`persist` 在响应写完之后执行，测试必须**排空响应**再查，并容忍最后几毫秒
  （轮询 3 秒）。这是测试侧的抢跑，不是产品缺陷：`s.persist(...)` 在非流式路径上是无条件执行的
  （`internal/httpapi/v1.go` 写响应之后那一行），与 M19d/M19e 那条「挂断也要留行」的保证一致。
  这个辅助函数顺便让用例短了一截（6 处 `GetRequestLog` + 手写 body 变成一行调用）。
- **`<system-reminder>` / `<skills_instructions>` 是本次新加的常量**（在 `record.go` 内部，没有塞进
  `dimensions.go`）：身份提取器不需要它们，而输入录制需要——DSH 把技能目录与运行时提醒塞在这两个标签里。
  将来若第三个消费者也要用，再提到 `dimensions.go` 与既有 `dshRuntimePrefix` 并列。
- **`carriesText` 与 `boilerplate` 都在构造 `recordedUserMessage` 时就地算好**，选择循环里只读字段：
  循环要能一眼看懂「跳过什么」，而把 `TrimSpace`/前缀匹配留在循环里会让它变成三段判断。
- **优先级没有按「先样板后长度」写死**：三个条件（有文本、非样板、不超上限）是并列的 `continue` 条件，
  顺序不影响结果；写在一起是为了不让人以为某一条件更重要。
- **现场证据只读长度与开头标记**：诊断时没有读任何租户正文（打印的是每条消息的字符数与开头标记），
  这一点也写进了发布记录——日志/会话文件里的用户内容不该为了排障被搬来搬去。
- **`input_max_chars` 默认值 100 → 2000**：属于行为变更（配置默认值变了），在发布记录里写明；
  两台线上都没显式设过这个键，所以生效即新默认。

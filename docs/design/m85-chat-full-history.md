# M85 设计文档：智能问答把整段会话历史都发给模型（历史窗口默认关闭）

> 状态：已实现（本文件在实现前已在对话中作为计划输出并通过评审）。
> 规格同步：`docs/chat.md`（§9 配置、「历史窗口」、排障两行、§10 自查）、`config.example.yaml`、
> `README.md`、`docs/TODO.md`。取代关系：**修订 M32 的「历史按整轮裁剪」**（该条仍然实现着，但不再是
> 默认行为）。
>
> 里程碑编号说明：本条最初按 **M84** 编号并已提交（`2b7a3f7`、`a089114`），随后发现并行工作区
> 的 **Images API** 已经占用 M84（分支 `m84-images`，且已以预览形态部署在 gptjp），因此改号 **M85**；
> 已发布的 v4.4.0 构建物只含注释里的旧编号，行为不受影响。

## 目标

需求原话：「智能问答要把会话里的所有历史记录都发给模型」。

今天的控制台问答只把**最近的一段**会话回放给模型：`chat.max_history_messages`（默认 40 条消息）与
`chat.max_history_bytes`（默认 256 KiB）按整轮从新到旧装填，装不下就把更早的整轮丢掉，并在页面上提示
「为控制请求大小，更早的 N 轮对话没有随本次请求发送」。后果是：会话一长，模型就看不见前面查过什么、
用户纠正过什么、工具返回过什么——而这些都是同一个会话里的上下文。

本次把默认改成**不裁剪**：一次提问把该会话已存的**全部**消息按原顺序交给模型。窗口仍然存在，但退化成
一个可选的运维手段（`0` = 不限制，是新的默认值）。

验收标准：

- 未配置窗口时，`buildHistory` 返回该会话的每一条消息对应的 provider items（含工具调用与结果），没有
  `dropped` 提示、没有 `tooLarge` 拒绝；
- 配置了正整数窗口时，行为与今天逐字一致（整轮裁剪、条数提示、最新一轮装不下时该轮明确失败）；
- 回放出去的历史**自洽**：不存在没有 `function_call_output` 的 `function_call`；
- 会话长到超过模型上下文时，用户看到的是「上游原文 + 可操作的中文提示」，而不是一段英文 400；
- 线上（rag-server 与 gptjp）用「针」验证：第 1 条消息里埋的数字，21 轮之后仍被模型答出，且全程没有
  「更早的 N 轮…」提示。

## 关键决策

1. **窗口改成 `0 = 不限制`，默认 0**。沿用本仓库既有的同类约定（`chat.max_steps` / `chat.max_tool_calls`
   都是「0 = 不限制且默认 0」，见 `docs/design/m32-console-smart-chat.md` 差异第 5 条）：这种「先查 A 再查 B
   最后汇总」的会话本来就不该被一个猜出来的数字截断，代价（每次提问重发整段历史带来的输入 token 与延迟）
   写进配置注释与 `docs/chat.md` 的配置一节。`0` 在今天是被配置校验**拒绝**的值（`>=2` / `>0`），
   所以没有任何线上配置会被静默改义。
2. **超过上游上下文时明确失败，不自动截断重试**。延续 M32 差异第 4 条「发半个问题会让模型基于不存在的
   上下文作答，那比明确失败更糟」。失败轮保留上游原文（任何客户端都会看到的那段），只在后面追加一句
   中文可操作提示（新建会话 / 由运维设窗口）。不做「失败后用窗口重试」：那会双倍计费，而且等于把
   「发全部历史」这条需求偷偷改回去。
3. **回放前做一次配对修形**。无窗口之后，一条坏条目会被**永久**回放：被中断的轮次里，模型已经广播、
   但服务端没来得及执行的 `function_call`（取消、步数/工具预算打断、参数不完整都会产生）留在存储的
   provider items 里，上游对「没有输出的 function_call」是直接 400 的。今天窗口滑动还有机会把它挤出去，
   不裁剪之后就没有了——所以回放边界必须保证配对完整：丢弃没有输出的 `function_call` 与没有调用的
   `function_call_output`（保持顺序）。这与 chat 方言的同类修形
   （`pkg/providerkit/chatcompat.go:repairToolSequences`）以及 2026-09-14 codex 事故的教训
   （「历史里一旦有坏条目，该会话此后每个请求都失败」，见 `docs/todo_done.md`）是同一条规则。
   不合成假输出：`refuseToolCall` 那类合成是**本轮内**的语义（这一步确实发生了、只是没执行），
   回放历史里丢掉更简单也更接近 chat 方言的处理。
4. **技能草稿路径不动**。`internal/chat/draft.go:draftTranscript` 有自己的裁剪（按 `chat.max_skill_bytes`
   保留最新消息），那是「把这次对话沉淀成技能」的输入，不是问答的上下文；本次明确出界。
5. **只发会话内历史，控制台不改**。控制台本来就显示全部消息；`buildHistory` 的调用方只有
   `internal/chat/turn.go` 一处，没有别的消费者，也没有新的接口/字段/schema。

## 接口

```go
// internal/chat/items.go
//
// buildHistory keeps the newest whole turns while a bound is configured. A bound of 0 (or
// less) on a side disables that side: the whole conversation is replayed.
func buildHistory(messages []*domain.ChatMessage, maxMessages, maxBytes int) historyPlan

// pairToolItems drops the halves of a tool exchange that lost their partner, so an
// unbounded replay cannot hand the provider a call it will reject.
func pairToolItems(items []pluginapi.Item) []pluginapi.Item

// internal/chat/turn.go
//
// contextOverflowHint appends an actionable sentence when the upstream rejected the request
// because the conversation no longer fits its context window.
func contextOverflowHint(message string) string

// internal/chat/chat.go：Config.MaxHistoryMessages / MaxHistoryBytes 的语义改为
// 「0 或负 = 不限制」；withDefaults 不再把 0 填成 40 / 256 KiB，负数归一为 0。
// internal/config：两个键同名同类型，默认值 40/262144 → 0/0，校验放开 0。
```

## 数据流

```
每次提问（runTurn）：
  ListChatMessages(session, 0)                      # 全部已存消息，含本轮刚写入的 user 消息
    └─ buildHistory(messages, max_history_messages, max_history_bytes)
         ├─ 按 turn_id 分组（缺 turn id 的消息各自成组，保持顺序）
         ├─ 从新到旧装填：maxMessages>0 且超出 → 丢更早的整轮（dropped）
         │                maxBytes>0   且超出 → 同上（两个界各自独立判断）
         │                没配界 → 一个都不丢
         ├─ 最新一轮本身就超界 → tooLarge（只有配了界才可能），该轮明确失败
         └─ pairToolItems(items)：丢弃无输出的 function_call 与无调用的 function_call_output
  Step.Items = history.items + 本轮已产生的 run.providerItems
```

## 异常与边界

- **只配了一个界**：另一个界不参与判断。这是实现里必须拆开的点——今天的组合判断
  （`plan.messages > 0 || plan.turns > 0` 时比较两个界）在「只配 bytes」时会因为 `maxMessages == 0`
  把除了最新一轮以外的所有历史都丢掉。拆开后各有测试钉住。
- **最新一轮本身就超界**：与今天相同——不发送任何模型请求，该轮 `failed`，文案改成「历史窗口」口径
  （旧文案说的是「单次请求的上下文上限」，在窗口模式下不准确）。
- **会话超过模型上下文**：上游 400 → 该轮 `failed`，错误是「上游原文 + 中文提示」。同会话后续提问同样
  失败：要么新建会话，要么由运维把窗口调成正整数。这是「发全部历史」的必然代价，写进 `docs/chat.md`
  的排障表。
- **历史里的未回答调用 / 孤儿输出**：回放前丢弃（决策 3）。被丢弃的调用在控制台的卡片上仍是 `unknown`，
  用户看到的事实不变。
- **会话极长**：每步重发整段历史，输入 token、延迟与费用随之增长；chat 的内部请求不走
  `server.max_body_bytes`（`chatRunner` 直接调用数据面 handler），网关侧没有硬上限。文档写明这是默认
  行为的代价与观测点；要不要做自动摘要/压缩是另一个里程碑。
- **部署期**：重启会中断在途 SSE 问答，择时进行。

## 测试策略

- `internal/chat/items_test.go`（新增，纯函数级）：不配界 → 全部轮次原样按序；只配 messages / 只配 bytes
  各自的裁剪与 `dropped` 计数；两界都配 → 与今天一致；最新一轮超界 → `tooLarge`；`pairToolItems`
  丢弃无输出的 call 与孤儿 output、保留成对条目与顺序。
- `internal/chat/chat_test.go`：不配界时 `runner.calls[0].Items` 含最早一条且无 dropped 提示、默认配置的
  `MaxHistoryMessages == 0`；既有两条窗口用例保持（超限文案断言改「历史窗口」）；回放丢弃未回答的
  `function_call`；上游上下文超限的失败既保留原文也带中文提示，其它失败不追加。
- `internal/config/config_test.go`：0 合法、负数报错、`1`（不满一轮）仍报错、默认值断言 40 → 0。
- `make verify` 等价的 go 命令 + `make ui-base` + `make build` 全绿（在宿主工具链上跑）。
- 真机：`scripts/verify-m85.sh`（针测试，默认要显式打开才跑，会计费）。

## 依赖

标准库 + 既有包；不新增依赖、不改 schema、不改接口形状。

## 环境说明（本次实现）

工作区仓库在沙箱内没有 Go 工具链，也没有通用 DNS：`go vet` / `go test` / `make build` / 部署都通过
`ssh rag-server`（可达；工作区路径与宿主在同一文件系统，`~/.ssh/config` 里有 `Host gptjp`）执行。
真机目标是 rag-server（`systemctl --user aigw-local`）与 gptjp（`aigw.service`），gpt001 本次不在范围。

## 实现与设计差异

实现与设计基本一致，有六点按代码事实收紧或补充：

1. **「两个界各自独立」不只是语义问题，还修掉了一个真缺陷**。旧实现的判定是
   `if plan.messages > 0 || plan.turns > 0 { … compare both … }`，两界必须同时非零才成立；一旦只配
   `max_history_bytes`（messages = 0），`plan.messages + n > 0` 恒为真 → 除了最新一轮，**整段历史都被丢掉**。
   这是设计里没写、写测试时才撞见的分支，实现改成两个 `> 0` 判定，并单独钉一条测试
   （`TestBuildHistoryBoundsAreIndependent`）。
2. **配对修形用「丢」而不是「补」**。设计只写了「自洽」。实现选择丢弃失去配对的那一半（与 chat 方言的
   `providerkit.repairToolSequences` 一致），不合成一条「这一步没有执行」的假输出：合成会让模型以为工具
   有结论，而 `refuseToolCall` 那类合成是**本轮内**的语义（这一步确实发生了、只是没执行），不是历史回放的
   语义。另外 `call_id` 为空的 `function_call` / `function_call_output` 也一并丢弃（上游把它当必填键）。
3. **上下文超限的提示靠「上游措辞匹配」，不靠状态码或上下文窗口推算**。chat 层拿不到路由事实
   （`ContextWindow` 在 routing 里），而按状态码推断（400）会把「参数错」也误判。所以 `contextOverflowHint`
   用 7 个大小写不敏感的措辞匹配，匹配不到就原样返回上游文本 —— 最坏情况是少一句中文提示，不会丢事实。
4. **负值两层处理**：`internal/config` 校验层直接拒绝（运维笔误要在启动时报错），`internal/chat.withDefaults`
   再把负值归一为 0（防御性，与 `MaxSteps` / `MaxToolCalls` 同形）。
5. **真机验收脚本加了 `ACCOUNT_ID` / `KEY_ID` / `MODEL` 覆盖**。原设计只说「跑针测试」，但在生产实例上
   「第一个能用的 Key」往往是客户的（gptjp 上 111 个账户、132 把 Key），一次 22 轮的小验证不该记到客户头上；
   显式指定后，两台都用了运维自己的账户（rag-server #4/#8、gptjp #1/#117）。
6. **交付被拆成两次发布**（设计里没预见）：v4.4.0 只上了 rag-server —— 部署前发现 gptjp 正跑着并行工作区的
   **Images API 预览版**，覆盖会把它撤掉；用户决定先合并 `m84-images` 再一起部署，于是有了 v4.5.0
   （`main` 同时含 M84 + M85，rag-server 与 gptjp 同版本）。里程碑编号也因此从 M84 改成 M85。

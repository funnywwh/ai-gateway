# M48 Codex 远端压缩 v2（`compaction_trigger` → 恰好一个 `compaction` 输出项）

> 状态：**已实现（2026-09-15，未发版）**。相关规格：`docs/api-responses.md`「Codex 远端压缩 v2」一节。
> 真机验收：VSCode 扩展自带的 codex（`openai.chatgpt-26.803.61601` = codex-cli 0.147.0-alpha.6.5）
> 指向本机 `:8088`，第二轮自动压缩输出 `context compacted`；同一条路径在改动前的二进制上
> 失败（见 §7 验收记录）。
> 触发现场：VSCode 里的 codex 报
> `Error running remote compact task: Fatal error: remote compaction v2 expected exactly one
> compaction output item, got 0 from 1 output items`。

## 1. 症状与现场证据

- 本机 `:8088` 的 `request_logs#1170`（`req_gq657z7hugi6ave5jpmowrri`）就是那一次压缩轮：
  `client=codex`、`model=deepseek-flash`、`input` 的 `omitted` 摘要里含 **`'compaction_trigger': 1`**、
  1.3 MB、`status=completed` —— 网关自己认为这一轮成功了，客户端却 fatal。**失败的是协议形状，不是传输。**
- codex 侧契约（`codex-rs/core/src/compact_remote_v2.rs::collect_compaction_output`，
  见 openai/codex PR [#20773](https://github.com/openai/codex/pull/20773) 与
  [#22809](https://github.com/openai/codex/pull/22809)）：远端压缩 v2 **就是一次普通的
  `POST /v1/responses`**，`input` 末尾追加 `{"type":"compaction_trigger"}`，然后要求这条流里
  **恰好一个** `{"type":"compaction","encrypted_content":…}` 输出项；别的输出项（正文、思考）允许存在，
  只被忽略（错误文案里的 `from N output items` 就是总数）。
- 上游 deepseek 没有原生压缩：它把这轮当成普通对话回答（reasoning + message），既没有 compaction 项，
  网关也没有替它补 —— `compaction_count = 0` → fatal。而 codex 认为"provider 名字是 OpenAI 就支持远端压缩"，
  客户端侧关不掉，所以线程会一直卡在满上下文上（自动压缩反复失败）。
- 随后一轮（压缩失败后）的历史里仍有 `compaction` 项的**回放**问题：即使压缩成功，
  后续每一轮都会把 `{"type":"compaction","encrypted_content":…}` 放回 `input`，
  非原生上游读不懂它 —— 不处理的话压缩就等于**把上下文丢掉**。

## 2. 目标

1. 非原生上游（deepseek、任意 OpenAI 兼容 `openai-responses`、`openai-chat`）也能完成 codex 的压缩 v2：
   收到触发项 → 真的去总结 → 回**恰好一个** `compaction` 项。
2. 压缩后的历史在后续轮次里**必须被上游看得见**（信封 → 摘要文本）。
3. 原生路径零回归：真 OpenAI / 订阅 codex 插件自己产出的 `compaction` 项**原样透传**，网关不再合成第二个
   （两个项同样 fatal：`got 2 from N`）。

## 3. 关键决策（含取舍）

1. **网关自己当压缩后端**（总结 + 自签信封），而不是拒绝或降级。
   取舍：codex 侧无法关闭 v2（按 provider 名判定），不改网关线程就废掉；改了只是一个普通的上游调用。
2. **信封格式 `gw1:` + base64(UTF-8 摘要)**。
   取舍：`encrypted_content` 对客户端是不透明字符串，base64 让摘要里的换行/引号/非 UTF-8 不可能破坏 JSON，
   且**自识别** —— 只有我们签发的 blob 会被解回文本，不需要额外记账。代价是体积 +33%。
   参考同类实现（opencodex 的 `ocx1:` 信封）用的是同一思路。
3. **只改写 `gw1:` 信封，其它密文原样透传**。
   取舍：跨路由时真 OpenAI 密文我们读不懂，但原生上游读得懂；替换成"此前历史已压缩"的提示会让原生路径退化。
   代价：非原生上游拿到读不懂的项时静默忽略（与今天"完全不发"相比没有退步）；
   已实测 deepseek 不会因未知 `compaction` 项返回 400（只忽略）。
4. **压缩轮对客户端只放行一个 compaction 输出项**。
   模型正文/思考照常进 assembler（用量、限流结算、`GET /v1/responses/{id}`、日志都还要用），但**不发给客户端**：
   codex 要的是检查点，不是回答。取舍：这一轮客户端看不到增量（无 TTFT），换来的是协议形状干净、
   摘要不会以助手消息的样子出现在 UI 里。
5. **摘要为空判失败**（不写空信封）。
   取舍：空信封会让 codex 用"没有历史"继续跑 —— 静默丢上下文比报错更坏。codex 自己的本地压缩在同样
   情况下写 "(no summary available)"，我们选择显式失败并把 `compaction_empty_summary` 记进错误码。
6. **压缩指令用 user 消息追加**（逐字对应 codex 本地压缩用的 `prompts/templates/compact/prompt.md`），
   并**清空 `tools`/`tool_choice`/`parallel_tool_calls`**：与 codex 本地压缩发的请求形状一致
   （`Prompt { ..Default::default() }` 不带工具）。取舍：不依赖任何上游对 `instructions` 的特殊处理。

## 4. 接口（`internal/responses/compaction.go`，新增）

```go
// 输出项类型（codex protocol 的 compaction 家族）
const (
    ItemTypeCompactionTrigger = "compaction_trigger" // 触发项；codex ≥ PR#22809
    ItemTypeCompaction        = "compaction"         // 客户端要的那一个项
    ItemTypeCompactionSummary = "compaction_summary" // codex 的 serde alias，等价 compaction
    ItemTypeContextCompaction = "context_compaction" // 旧版触发形态 / 另一种原生项
)

// CompactionEnvelopePrefix 标记本网关签发的摘要（`gw1:` + base64）。
const CompactionEnvelopePrefix = "gw1:"

// CompactionPrompt / CompactionSummaryPrefix 逐字取自 codex-rs prompts/templates/compact/*.md。
const CompactionPrompt = "You are performing a CONTEXT CHECKPOINT COMPACTION. …"
const CompactionSummaryPrefix = "Another language model started to solve this problem. …"

// IsCompactionRequest 判断这一轮是不是 codex 的远端压缩轮（input 里有触发项）。
func IsCompactionRequest(items []pluginapi.Item) bool

// PrepareCompactionRequest 就地改写上游请求：剔除触发项、清空工具、追加压缩指令。
func PrepareCompactionRequest(req *pluginapi.Request)

// LocalizeCompactionItems 把历史里 `gw1:` 信封换回上游看得懂的 user 消息；
// 其它 compaction 家族项原样保留。
func LocalizeCompactionItems(items []pluginapi.Item) []pluginapi.Item

// EncodeCompactionSummary / DecodeCompactionSummary 信封编解码（解码失败 = 不是我们的）。
func EncodeCompactionSummary(summary string) string
func DecodeCompactionSummary(encryptedContent string) (string, bool)

// CompactionItem 构造客户端要的那一个输出项：{"type":"compaction","encrypted_content":"gw1:…"}。
func CompactionItem(summary string) pluginapi.Item

// IsNativeCompactionItem 判断上游是否已经自己产出了原生 `compaction` 项（有则不合成）。
func IsNativeCompactionItem(item pluginapi.Item) bool

// CompactionObserver 包一层 SSE 发射器：压缩轮里只放行响应级事件与 compaction 类输出项。
func CompactionObserver(send func(*Event) error) func(*Event) error
```

`internal/responses/assembler.go` 增加一个只读方法（不改变行为）：

```go
func (a *Assembler) HasNativeCompactionItem() bool
```

`internal/httpapi/v1.go`：检测一次（复用 `req.Items()`）、每候选改写、发射器包裹、终局合成。
`internal/responses/dimensions.go`：`CallKindCompaction = "compaction"`。

## 5. 数据流

**压缩轮**（codex → 网关 → 上游 → codex）：

1. `IsCompactionRequest(items)` 命中 → 这一轮标记为压缩轮（`call_kind=compaction`）。
2. 每个候选：`ToProviderRequest` → `LocalizeCompactionItems`（历史里的 `gw1:` 信封先还原成摘要消息，
   与普通轮共用同一条改写） → `PrepareCompactionRequest`（剔除触发项、清空工具、追加压缩指令 user 消息）。
3. SSE 发射器包一层 `CompactionObserver`：`response.created/in_progress/completed/…` 与
   compaction 类输出项放行，模型正文/思考/工具项不外发。
4. 上游 `usage` 照常进 assembler → 计量、限流结算、`usage_records`、请求日志与今天完全一致。
5. 成功且**上游没有自己产出** `compaction` 项 → 用 `assembler.Text()` 造 `CompactionItem`，
   经 assembler 发布会 `response.output_item.added` + `output_item.done`（**恰好一个**），随后照常 `response.completed`。
6. 上游自己产出了 `compaction` 项（原生路径：订阅 codex 插件 / 真 OpenAI）→ **不合成**，原样透传。

**后续轮次**（信封回放）：

7. codex 把 `{"type":"compaction","encrypted_content":"gw1:…"}` 放回 `input`；
   `LocalizeCompactionItems` 解出摘要并**原位**替换为
   `{"type":"message","role":"user","content":[{"type":"input_text","text":"<SUMMARY_PREFIX>\n<摘要>"}]}`，
   形状与 codex 本地压缩写进历史的摘要消息一致（模型是按这个形状训练的）。

## 6. 异常与边界

- **两种触发形态**：`{"type":"compaction_trigger"}`（新）与 `{"type":"context_compaction"}`（无
  `encrypted_content`，PR#22809 之前）。历史里的 `context_compaction`（**带**密文）不是触发。
- **信封损坏**：前缀是 `gw1:` 但 base64/UTF-8 坏 → 当作外来项透传，不猜、不改写。
- **空摘要**：`assembler.Text()` 去空白后为空 → 走既有失败路径（流式 `response.failed`，
  非流式 502 `api_error/upstream_error`），错误码 `compaction_empty_summary`，不写空信封。
- **故障转移**：抑制的是"发给客户端的事件"，assembler 仍照常累加正文，
  所以「已有增量就不再换供应商」「没有增量可以换」的既有语义不变；压缩轮的候选过滤、
  熔断、并发排队、计量全部复用普通轮。
- **续接**：`previous_response_id` 的 `priorItems` 与本次 `input` 合并后再统一改写，
  两条路径（客户端自己带历史 / 网关补历史）都不会漏掉信封。
- **`/responses/compact`（v1，unary，上游 `/responses/compact`）不在本次范围**：
  codex 只在 provider 支持时走 v1，本网关面向的是 v2 流式路径；v1 若被调用仍是普通响应，
  不会替换客户端历史 —— 记进 TODO 后续跟进。
- **原生密文透传的边界**：非原生上游拿到真 OpenAI 密文时读不懂（今天也读不懂，只是今天连"发"都没发）。
  风险已知、已实测不会 400，文档写明。

## 7. 可观测性

- `call_kind` 新增 `compaction`：控制台请求日志能看到"上下文压缩"轮，它不再计入"会话轮次"，
  也不再参与会话标题指纹（`title_links` 只认 `call_kind='agent'`）。
- 压缩轮的 `response_text` 录制到的就是摘要本身（有录制开关时），便于排查"压缩后模型看到了什么"。

### 7.1 真机验收记录（2026-09-15，本机 :8088）

用 VSCode 扩展自带的 codex 二进制（`~/.vscode/extensions/openai.chatgpt-26.803.61601-linux-x64/
bin/linux-x86_64/codex`，`codex-cli 0.147.0-alpha.6.5`）配临时 `CODEX_HOME`：
`model_provider = "OpenAI"`（名字必须是 `OpenAI`，远端压缩 v2 就是这么选的）、
`base_url = http://127.0.0.1:8088/v1`、`model_auto_compact_token_limit = 2000/3000`。

| 场景 | 改动后（本提交） | 改动前（0.12.4，回滚点） |
|---|---|---|
| 两轮纯文本会话，第二轮触发自动压缩 | `context compacted`，无 fatal | `Failed to run pre-sampling compact` + `Error running remote compact task`（那条 1.3 MB 的真实历史同样如此，见 `request_logs#1170`） |
| 工具轮会话（3 次 shell 调用）后触发压缩 | `context compacted`，随后工具轮正常继续 | 未复测（同上路径） |
| 网关侧记录 | `request_logs#1511`：`call_kind=compaction`、`status=completed`；`#1512` 的 `input` 里含 `compaction` 项、不含 `compaction_trigger`（信封回放成功） | 无 compaction 行，且注入的 `context_compaction`/compaction 项导致上游 400 |

顺带发现（**既有问题，本次未改**）：该 codex 版本在普通轮次也会打印
`ERROR codex_core::util: OutputTextDelta without active item`；用改动前的二进制同样复现（2 次），
所以与本修复无关，另行跟进。

## 8. 测试策略

- 单元（`internal/responses/compaction_test.go`）：识别（触发项命中；只有历史 compaction 项时**不**命中）、
  信封往返、损坏信封不解析、输入改写（触发项剔除 + 指令追加 + 外来项保留）、本地化
  （信封 → user 消息且顺序不变、外来项不动）。
- 端到端（`internal/httpapi`）：httptest 上游（内置 `openai-responses`）返回正文 + usage →
  断言 SSE **恰好一个** `output_item.done` 且 `item.type=="compaction"`、没有 `output_text.delta`、
  `response.completed` 带 usage、上游请求体含压缩指令且**不含** `compaction_trigger`、工具被清空；
  第二阶段把返回的信封放进 `input` 再请求一次，断言上游收到的是解码后的 user 消息。
  原生路径回归：上游返回自己的 `compaction` 项 → 断言客户端收到的是**上游那一个**（网关没有合成第二个）。
- 真机走查：用 VSCode 扩展自带的 codex 二进制（`openai.chatgpt-26.803.61601-linux-x64`）+
  临时 `CODEX_HOME` 指向本机 `:8088`，把 `model_auto_compact_token_limit` 调低触发自动压缩，
  确认不再出现 fatal，且压缩后的后续轮次仍能引用压缩前的内容。

## 9. 依赖与影响面

- 只动 `internal/responses`（新增 `compaction.go`、assembler 一个只读方法、`dimensions.go` 一个常量）、
  `internal/httpapi/v1.go`（检测/改写/包裹/合成）、控制台 `requests.js` 一个徽标分支。
- **不改** provider 层与插件协议：上游看到的只是"多了一条 user 消息、没有工具的普通请求"。

## 10. 实现与设计差异

- **内置 `openai-responses` provider 多了一处改动（设计外）**：它原先**完全丢弃**
  `response.output_item.done`（只把 function_call 的 added 帧转成 tool_call.start），
  而原生 compaction 项**只存在于 done 帧**里、没有增量可重建。结果是「上游原生支持」这条路
  在走内置 provider 时永远拿不到项 → `HasNativeCompactionItem` 恒为假 → 我们会在原生项之外
  **再合成一个**（客户端 `got 2 from N`，与 `got 0` 一样致命）。现在只对 compaction 家族
  （`compaction`/`compaction_summary`/`context_compaction`）转发 done 帧，普通项的 done 帧
  仍留在增量路径上（否则每个 item 会被发布两次）。这条是被端到端测试逼出来的：
  `TestUpstreamThatAnswersNativelyIsNotDuplicated` 一开始红，才去查为什么原生项没到网关。
- **`ToProviderRequestWithItems`**（设计里写的是「复用 `req.Items()` 直接构造」）：解析一次、每候选
  复用同一份 items，避免每个候选重复解析 1.3 MB 的 `input`；`ToProviderRequest` 变成它的薄封装。
- **续接前缀也做本地化**（设计未提）：`previous_response_id` 读回的历史里同样可能是我们的信封，
  少了这一步，压缩项会以密文形式回到上游。
- **观察者放行的是「响应级事件 + compaction 家族输出项」**，而不是设计里那句"只放行终局事件"：
  `response.created/in_progress/completed/failed/incomplete/error` 都要过去，否则客户端连终态都收不到。
- **`call_kind=compaction` 的连带效果**：`title_links` 的配对查询只认 `call_kind='agent'`
  （`internal/store/title_links.go`），所以压缩轮不再充当"会话标题指纹"的来源。这是想要的
  ——压缩轮不是会话的第一条提示词——但确实是本次行为变化，记在这里。
- 端到端测试里上游返回的 usage 照常进 `usage_records`、SSE `response.completed` 里带 usage；
  压缩轮记录的类型是 `compaction`（本机实测 `request_logs#1511`）。

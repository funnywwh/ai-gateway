# M82 设计文档：`record_input=user` 只留最后一条合格 user 消息的纯文本

> 状态：已实现（本文件在实现前已在对话中输出并通过评审）。
> 规格同步：`docs/request-log.md`（录制通道矩阵）、`config.example.yaml`（`recording.input_max_chars`）、
> `docs/mcp.md`（内容可见性）、`docs/api-responses.md`、`README.md`、`docs/TODO.md`（M82 清单）。
> 取代关系：本里程碑**取代 M81**（M81 的口径是「每条 user 消息截断到前 100 字符」，正文是一份 JSON 文档），
> 也顺带收窄 M23 的「只记用户输入」——从「所有 user 消息 + 计数」变成「最后一条的纯文本」。
> 编号说明：M78/M79/M80 已被并行工作区占用，M81 是上一个里程碑，故本项为 **M82**。

> **M83 再次修订（v4.3.3）**：判定依据从「长度 ≤N」换成**样板标记**——真实提问常常比短的样板长、比长的样板短，
> 长度无法区分；某浏览器 DSH 租户因此每条请求正文都是空的。上限（`input_max_chars`）默认 100 → 2000，只作防呆。
> 见 `docs/design/m83-user-input-human-only.md`。

## 目标

请求日志的输入通道（`record_input=user`）在 M81 之后仍有两个问题：

1. **留下了「假头」**：截断到前 100 字符，读起来像完整提问——一个 100 字符的提问与一个 1000 字符
   提问的前 100 字符在日志里长得一模一样，操作者容易误读。
2. **正文是一份 JSON 文档**：`{"model":…,"input":[{"type":"message","role":"user","content":[…]}]}`。
   这份结构对读日志的人（以及用 `get_request` 的 agent）几乎全是噪声——里面只有一段文字是有用的。

本次把口径收成一句话：**只保留最后一条 user 消息的纯文本，且它必须短于阈值**；要保留全部（原样正文）
就把该 Key 切 `full`。

验收标准：

- `record_input=user`（默认）下，`request_logs.request_json` 就是**一段纯文本**：最后一条 user 消息的
  文本（多条文本 part 用 `\n` 连接），**不含**任何 JSON 结构、不含图片、不含工具定义/输出/系统指令。
- 最后一条 user 消息 **≥ `recording.input_max_chars`（默认 100）字符** → 该行正文为**空**（没有截断的假头）。
- **`full` 档 = 「保留全部」**：整份原样 JSON 正文，**不受**阈值、不受「只留最后一条」、不受「纯文本」三条限制，
  仍只受 `recording.max_bytes` 兜底。
- `metadata`/`off`/控制台流量（强制 off）行为不变；上游转发零变化；无 schema 迁移，历史行不回填。
- 读侧（控制台、MCP、hook、stored response）在新形状下都正确，且能读 M81 期间写的老行。

## 关键决策

1. **只留最后一条，且必须 `< N`**。需求原话：`>=100 个字符的 user 消息就不用保持了`、
   `只保持排在最后面的 <=100 字符的 user 消息`。真实 DSH 请求里 user 消息是
   `[813 字符 runtime context][78 字符提问]` 这种形状——最后一条就是人写的那句。前面那些
   （runtime context 快照、压缩前的历史轮次）是样板，留它们只会把日志变成噪音。
   边界取**严格小于**：恰好 100 字符丢弃、99 字符保留（用户选择的是「必须 `<N`」）。这条边界在测试里钉死，
   因为它是最容易被读成 `<=` 的地方。
2. **整条保留或整条不留，不再截断**。截断制造的「假头」是本次要消掉的误读来源；保留就逐字符原样保留，
   让读日志的人能确信自己看到的是完整的用户输入。
3. **正文只有文本**。需求原话：`只保持文本内容，不需要 Json 格式`。于是 M81 那份 JSON 文档整体退场：
   没有 `input[]`/`omitted`/`request_bytes`/`input_max_chars` 字段，没有 `type`/`role`/`content`/part 结构。
   代价（用户明确接受）：**看不出「为什么这次没有输入」**——`input_recorded=false` 加日志行上已有的
   `record_input_mode` 列是仅有的线索。要看全部就切 `full`。
4. **`full` 是「保留全部」的官方出口，且不受本规则任何限制**。它存在的理由就是上游 400 时要看客户端发出的
   原样字节（M19d/M19e 的教训）；给它加阈值、加「只留最后一条」或改成纯文本，等于把排障出口废掉。
   控制台档位文案、MCP 工具说明与 `docs/request-log.md` 都把这句话写出来，避免操作者以为「默认口径已经是全部」。
5. **`N = 0` = 不过滤**：把所有 user 消息的文本按顺序用 `\n` 连接成一段（仍然只是文本）。这是「不想按条过滤」
   的部署级出口；它不是 `full` 的替代品（依旧不含图片/工具内容），`full` 才是字节级出口。
6. **`recording.input_max_chars` 键名不变**（默认 100、0 不过滤、负值启动报错），只改语义与注释：
   从「截断上限」变成「保留阈值（严格小于）」。好处是零配置迁移（两台线上都没设这个键），坏处是名字里的
   「max chars」要读文档才准确——值得，因为改名意味着一次无意义的配置迁移。
7. **`recording.redact_paths` 加一条兼容规则**：纯文本没有 JSON 路径可指，若路径列表里出现 `input`
   （历史上就是「不要留输入」的意思）→ 正文清空；其余路径继续只作用于身份列（M27/M30 口径不变）。
   静默丢掉操作者已配置的保护比多一个分支更糟。
8. **MCP `get_request` 必须改**：它现在把 `request_json` 塞进 `json.RawMessage`；裸文本会让 `json.Marshal`
   报 `invalid character` 而**坏掉整条 JSON-RPC 响应**。改为「内容合法 JSON 才用 `RawMessage`，否则作为字符串」，
   于是纯文本行返回字符串、`full` 行与历史行返回对象。
9. **管理 API 详情新增 `record_input_mode`**（读时镜像日志行已有的列，不新增存储）：控制台据此区分
   「策略是 user 但没有可保留的文本」与「未录制」。
10. **`/stats` 去掉 `input_max_chars`**：它是 M81 今天刚加的阈值回显，属于「除文本外什么都不要」的范围；
    阈值在 `config.yaml` 里能看到。

## 接口

```go
// internal/config（键名/默认/校验都不变，只有注释与语义变）
type Recording struct {
    // InputMaxChars 是 user 档保留 user 消息所需的长度阈值：只有【最后一条】user 消息短于它才保留，
    // 且保留的是整条文本；0 = 不过滤（把所有 user 消息文本拼接保存）。full 档不受它影响。
    InputMaxChars int `yaml:"input_max_chars"`
}

// internal/responses（M23/M81 的文档机制整体退场）
// UserInputText 返回 user 档要落的正文：最后一条 user 消息的纯文本；不满足条件时返回 ""。
// 第二返回值留作将来需要「为什么没留」时使用的位置（当前实现只用错误）。
func (r *Request) UserInputText(maxChars int) (string, error)

// userMessageText 取一条 user 消息的文本、字符数与可读性。
// content 是字符串 → 就是它；是 part 数组 → 其文本 part 用 "\n" 连接；对象/坏 JSON → 不可读。
func userMessageText(content json.RawMessage) (text string, runes int, readable bool)

// internal/httpapi
//   recordInput 的 user 分支：text, err := req.UserInputText(cfg.InputMaxChars)
//   redactPaths 含 "input" → 正文清空（兼容规则）
//   handleAdminRequestDetail：新增 "record_input_mode": row.RecordInputMode
//   requestLogStats：删掉 "input_max_chars"

// internal/mcpsrv
//   getRequest：input 字段按内容是否合法 JSON 决定 json.RawMessage 还是 string
```

数据流（`user` 档）：

```
POST /v1/responses
  └─ recordInput(key, req, clientHint)
       ├─ full     : 整份请求体 → redact() → truncate(max_bytes)        【保留全部：不受阈值/末条/纯文本限制】
       ├─ user     : UserInputText(recording.input_max_chars)
       │               ├─ U 为空                      → ""
       │               ├─ 末条字符数 >= N             → ""
       │               ├─ 末条 content 不可读          → ""
       │               └─ 末条字符数 <  N             → 该条文本（多 part 用 "\n" 连接）
       │             → redact_paths 含 "input" ? 清空 : 原样 → truncate(max_bytes)
       ├─ metadata : 正文为空，request_bytes 仍记录
       └─ off      : 正文与 request_bytes 都为 0
  └─ persist() 用同一份 payload 写 request_logs、responses.request_json 与 hook 事件的 input
  └─ recordDenied() 走同一个 recordInput（本地拒绝路径同样只留纯文本）
```

## 异常与边界

- 恰好 N 字符 → 不留；N-1 → 原样保留（测试各一条）。
- 末条消息只有图片/空文本 → 正文 `""`（`input_recorded=false`），行照写。
- 没有 user 消息（`previous_response_id` 续接、纯工具结果）→ `""`。
- content 不可读（对象、非法 JSON）→ `""`；不 panic、不失败，行照写（沿用 M25 的取向）。
- `N < 0`：配置校验期报错；`N = 0`：不过滤（全部 user 消息文本拼接）。
- `recording.max_bytes` 仍是兜底：对纯文本按 rune 边界截断并置 `truncated`（默认阈值下几乎不可能触发，
  `full` 档依赖它）。
- **历史行兼容**：M81 期间（今天 14:11 到 v4.3.1 部署之间）写的行是 JSON 文档、可能带
  `input_truncated`/`over_cap`。这些行**不回填**；控制台对它们仍按 JSON 展示并沿用 M81 的「已截断」文案，
  MCP 按内容是否合法 JSON 决定 object/string。
- 与身份维度（M27/M30）、`reasoning_effort`、hook、stored response、`/stats` 其余字段的交互均不变。
- `full` 档的正文形状、`metadata`/`off` 的空正文、控制台强制 off：全部不变。

## 测试策略

- `internal/responses`：`UserInputText` 表驱动 —— ① `[长样板][短提问]` → 恰好是提问文本；
  ② 末条恰好 100 → `""`；③ 末条 99 → 原样；④ 多文本 part → `\n` 连接；⑤ 末条带图片 → 只有文本、
  不含 `data:image`；⑥ 末条 content 是对象 → `""`；⑦ 无 user 消息 → `""`；⑧ `N=0` → 全部 user 消息文本
  依次 `\n` 连接；⑨ 返回值不含 `{`、`"input"`、`omitted` 之类结构痕迹；⑩ 字符串 content 形状。
- `internal/httpapi`：默认口径 → `RequestJSON` 恰为提问文本、`RecordInputMode="user"`；末条 ≥100 →
  `RequestJSON == ""` 且 `RequestBytes > 0`；**`full` 档 → 整份原样正文（工具输出哨兵、工具参数、
  长提问都在），`Truncated=false`**；拒绝路径同样纯文本；`redact_paths=["input"]` → 正文清空；
  详情返回 `record_input_mode`；`/stats` 不再有 `input_max_chars`。
- `internal/mcpsrv`：纯文本行的 `input` 是 **string**，`full` 行（合法 JSON）是 **object**，两者都不破坏
  JSON-RPC 响应。
- `internal/config`：既有四条断言（默认 100 / 显式 0 / 缺省 100 / 负值报错）保持不变。
- `internal/webui/tests/requests_test.mjs`：`inputPanelTitle` 的新分支 + 历史 `input_truncated` 行。
- `scripts/ui-harness/keys.page.html`：`#requests` 视图断言纯文本标题与「未保留」说明；真浏览器
  `make ui-check` 仍留宿主（沙箱的 firefox 是 snap 包装器，按设计跳过）。
- `make verify` 等价的 go 命令 + `make ui-base` + `make build` 全绿。

## 依赖

标准库 + 既有包（`internal/{config,responses,httpapi,mcpsrv,webui}`）；不新增外部依赖，不改数据库 schema。

## 环境说明（本次实现）

沙箱里 Go 走工作区自带工具链（`.cache/go`；`scripts/goenv.sh` 指向的 `$HOME/sdk/go` 在此不存在），
node v22 可用（`make ui-base` 真跑），`make ui-check` 会带原因跳过（firefox 是 snap 包装器）。
实现与测试都在独立工作区 `ai-gateway-m82`（分支 `m82-user-input-tail-text`）里完成，主工作区不被触碰。

## 实现与设计差异

- **`redactInputText` 是包级函数**而不是 `Server` 的方法：它不需要任何服务端状态（只有一份路径列表），
  写成方法会让人以为将来会读配置之外的东西。
- **MCP 的 `input_unavailable_reason` 文案扩了**：原来只有一句「recording is disabled for this API key」，
  现在要同时覆盖「策略是 user 但这次没有可保留的文本」，否则一个默认策略的请求会得到一句错的原因。
- **控制台保留了 M81 的旧文案分支**（计划里没写）：4.3.0 到 4.3.1 之间写的行仍是「截断过的 JSON 文档」，
  对它们「已截断：每条用户消息只留前 N 字符」是唯一诚实的描述；纯文本行不会走到这个分支。
- **一个 M27 测试必须改**：`TestRequestLogRecordsDSHIdentity` 用的 body 里，最后一条 user 消息是
  330 字符的 runtime context，按新规则「一条都不留」——于是它现在断言「身份照记、正文为空」。
  这不是测试被削弱：身份在此刻更没有别的东西可依赖（正文可能整段不存在），这正是 M27 的论点。
- **`TestRecordingSwitchesAreIndependent` 去掉了 omitted 计数的断言**（那些键已不存在），改为断言
  默认档正文**恰好等于** `ping`（而不是「包含 ping」）——把「不多一个字符、不多一层结构」也钉住。
- **harness 从两次回放变成三次**：`未保留（策略 user）`／`未录制（off）`／`历史截断行`，因为新形状下
  「空面板」有两种完全不同的含义，而它只能靠行上的 `record_input_mode` 区分。
- **类型名撞车**：`internal/responses/compaction.go` 已有一个 `userMessage`（构造 Codex 摘要用的 item），
  新加的内部结构体因此叫 `recordedUserMessage`。
- **`0`（不过滤）语义按计划实现**：把全部 user 消息的文本按顺序用 `\n` 连接；读不懂的消息在拼接里
  贡献空串（因此可能出现空行），这一点写在 `UserInputText` 的注释与测试里。

## M82.1 修订（v4.3.2）：往回找，取「最新一条有文本且不超过上限」的 user 消息

> 需求原话：「现在是不是保持最后一条不为空的<=100的user消息？」——确认后定了三条：空消息要跳过、
> 恰好 100 保留（`<=`）、末条非空但超长时继续往前找。

口径从 M82 的「字面最后一条 + 严格 `<N`」改成：**从末尾往回找，取第一条「有文本且字符数 `<= N`」的
user 消息**；空消息（空文本、纯空白、只有图片、content 读不懂）与超长消息都**跳过**；一条都没有 →
正文为空。

关键决策：

1. **为什么跳过而不是「停住」**：agent 客户端经常在回合末尾追加一条没有人类文本的消息（runtime context
   快照、只有工具结果的续接、一个空 continuation）。M82 只看字面末条，于是那些行**整行没有正文**——
   而日志里「最新的、说得出话的一条」几乎总是人刚写的那句。跳过比留空有用，也比往前找更早的旧消息克制
   （只在末条确实没有可记录文本时才往回走；这是用户明确选择的行为）。
2. **边界改成 `<= N`**：用户原话是「<=100字符」。M82 实现的是严格 `<`（我按当时选项里的「必须 `<N`」做的），
   本次按用户最终口径改为「恰好 100 也保留」。
3. **仍不截断**：超长消息整条跳过、不回退到它的前 100 字符——那条规则是 M81 的、也正是 M82 要废除的。
4. **`N = 0`（不过滤）也跳过没有文本的消息**：以前会把它们拼成空行（读起来像用户发了一轮空白），
   现在只拼接「有文本」的那些。

读侧与兼容：正文**形状不变**（仍是纯文本），所以 MCP 的 string/object 判定、详情接口的
`record_input_mode`、`/stats` 都不需要动；只有控制台「未保留」的说明文案改成
「没有不超过上限的用户消息」。历史行仍不回填。

测试变化：`internal/responses` 新增「跳过空消息（5 种形态）」「末条超长 → 回退取前一条」「一条都不合格 → 空」
「恰好 N 保留 / N+1 跳过」；`internal/httpapi` 的端到端用例改为断言「取最新一条合适的」并补了空白末条与
「全不合格」两种；M27 的身份测试跟着改（它的 body 末条是 330 字符 runtime context，现在会回退取到短消息，
比断言「正文为空」更贴近真实行为）。

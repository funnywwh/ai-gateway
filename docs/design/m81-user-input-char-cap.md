# M81 设计文档：请求日志（`record_input=user`）每条用户消息只留前 100 字符

> 状态：已实现（本文件在实现前已在对话中输出并通过评审）。
> 规格同步：`docs/request-log.md`（录制通道矩阵）、`config.example.yaml`（`recording.input_max_chars`）、
> `docs/mcp.md`（内容可见性）、`docs/api-responses.md`（拒绝路径同样受录制约束）、
> `README.md`（里程碑叙述与文档表）、`docs/TODO.md`（M81 清单）。
> 里程碑编号：M78（请求日志的路由路线）、M79（沙箱工作区视图）、M80（Key 批量导入）已由并行工作区占用，
> **已被 M82 取代**：『每条 user 消息截断到前 100 字符』改为『只保留最新一条有文本且不超过阈值的 user
> 消息的纯文本』（正文不再是 JSON 文档；v4.3.2 起空消息与超长消息会被跳过），见
> `docs/design/m82-user-input-tail-only-text.md`。
> 因此本工作编号 **M81**。

## 目标

M23 把请求日志的输入通道收窄为「用户自己写的输入」并定为默认口径，但那一档**原样保留** user 消息：
真实的 DSH 请求里每条 user 消息常有几百字符（`<environment_context>` / runtime context 快照、长提问），
于是 `request_logs.request_json` 仍然是一份完整的提问副本——一次提问的全部文字都进了日志，而日志是
**全管理员可读**、按保留期清理、并会被备份带走的。

本次把默认口径再收一档：**`record_input=user` 时，每条 user 消息最多保留前 100 个字符**（按字符计，
中文算 1 个字符），超出的文本丢弃，并在录制文档里留下可读的标记；上限可配置、可回退到旧行为。

验收标准：

- 默认（`recording.record_input: user` 或 Key 为 `inherit`）下，一条长提问的日志行里每条 user 消息的
  文本 ≤ `recording.input_max_chars`（默认 100）个字符，文档带 `input_max_chars` 与 `input_truncated`；
  操作者能看到问题的开头，看不到第 101 个字符。
- user 消息里的非文本部分（`input_image` 等）不落库，只在 `omitted` 里按类型计数。
- `full` / `metadata` / `off` 三档、控制台智能问答（服务端强制 off）行为**完全不变**；
  `recording.input_max_chars: 0` 回到 M23 的旧行为（整条 user 消息原样落库）。
- 所有落库路径（正常完成、失败、本地拒绝）走同一策略；上游收到的请求体与转发零变化。
- 历史行不重写、不回填；无 schema 迁移。

## 关键决策

1. **上限按「每条 user 消息」计，不是整份文档合计**。DSH 一个请求里通常有 2–3 条 user 消息，第一条
   往往是 runtime context 样板；若全文档共用一个 100 字符预算，被记下来的会是那段样板，真正的问题一个字
   都看不到——那样这条上限就只是把日志变成了噪音。按条计的上界是「消息条数 × 上限」，仍然远小于一份
   完整正文（实测 DSH 请求体常 100 KB 级，正文里绝大多数是工具定义与工具输出，那些本来就不落库）。
2. **超限是截断，不是整条丢弃**。日志的用途是「这条请求到底发了什么」；只留一个计数会让排障退化成
   「有人提了一个长问题」。截断同时给出「问题的开头」与 `request_bytes`（整份请求体的字节数），
   两个信息合起来足够判断这次请求的性质。
3. **默认 100，且可配置（`recording.input_max_chars`，`0` = 不限）**。「100」是产品默认值而不是常量：
   线上要临时看清一整条提问（或反过来收紧）时不该改代码重发版；`0` 是一条明确的回退通道，
   语义与 `recording.retention_days` 的 `0` 一致（关掉这个机制）。
4. **`full` 档不受字符上限影响**。它是为上游 400 的排障存在的：需要的是客户端发出去的**原样字节**，
   给它加一层截断等于把这个档位废掉。`full` 仍只受 `recording.max_bytes`（默认 1 MiB）约束。
5. **非文本部分不落库，只留计数**。这与该档位既有口径一致（「客户端发的、网关有意不存的」都进
   `omitted`）。实际影响是请求日志不再包含 base64 图片：一张 100 KB 的图会让「每条消息 100 字符」
   形同虚设，而 base64 图片对排障的价值远低于它带来的体积与隐私代价。
6. **上限按字符（rune）计，切点取精确前缀，不做 TrimSpace**。中文按 1 个字符算，符合「前 100 个字符」
   的字面含义；不复用 `internal/responses` 里已有的 `clampRunes`（它 TrimSpace 且不回传用量，
   而这里需要按消息共享一份预算、逐 part 递减）。
7. **一条消息里多个文本 part 共享这份预算**（按 part 顺序递减）：否则「每条消息 ≤ 100 字符」不成立。
   预算用尽后仍未落库的 part 单独计 `omitted["over_cap"]`，读者能区分「这类内容网关从不存」
   （`function_call_output`、`instructions`）与「这条消息太长、后面的文本没进来」（`over_cap`）。
8. **上限在 `internal/responses` 里执行，值由参数传入**。该包（`docs/design/m23` 起就是如此）
   不得 import `internal/config`（`internal/arch/layering_test.go` 的分层表），所以 `UserInputDocument`
   多收一个 `maxChars int` 参数，由 `internal/httpapi` 从配置里取。
9. **一次计算，三处使用**（M23 的既有约定）：`persist` 只算一次 `recordInput`，请求日志、
   `responses.request_json`（`store:true`）与 hook 事件里的 `input` 共用同一份文档。
   因此 hook 收到的也是被截断的文档——这是有意的：hook 发的本来就是「按录制策略产生的文档」，
   让它拿到比日志更多的内容等于绕过录制开关。已 grep 确认没有任何逻辑回读 `request_json` 做解析
   （只有展示与透传），存储响应的续接只读 `output_json`，所以截断不影响功能。
10. **读时可见性**：文档里带上 `input_max_chars` 与 `input_truncated` 两个字段，控制台详情把
    「已截断：每条用户消息只留前 N 字符」写进输入面板标题。没有这句话，一个被截断的提问读起来就是
    提问全文——这正是本次要修的那类误读。

## 接口

```go
// internal/config
type Recording struct {
    // ...既有的 record_input / record_reasoning / record_output_text / record_title / max_bytes...
    // InputMaxChars 限定 record_input=user 时【每条 user 消息】保留的字符数；0 = 不限（M81）。
    InputMaxChars int `yaml:"input_max_chars"`
}

// internal/responses
type UserInput struct {
    Model     string           `json:"model,omitempty"`
    Input     []pluginapi.Item `json:"input"`
    Omitted   map[string]int   `json:"omitted,omitempty"`
    Bytes     int              `json:"request_bytes,omitempty"`
    MaxChars  int              `json:"input_max_chars,omitempty"`  // 本次生效的上限；0/缺省 = 不限
    Truncated bool             `json:"input_truncated,omitempty"`  // 至少一条 user 消息被截断
}

// UserInputDocument 收一个上限参数；maxChars <= 0 表示不限（旧行为）。
func (r *Request) UserInputDocument(bodyBytes, maxChars int) (*UserInput, error)

// clampUserMessage 按上限改写一条 user 消息的 content（其余字段不动）。
// 返回：改写后的 content、被丢弃内容的计数（并入 omitted）、是否发生了截断。
func clampUserMessage(content json.RawMessage, budget int) (json.RawMessage, map[string]int, bool)

// takeText 取前 budget 个字符，并回传实际用掉的字符数（供多条 part 共享预算）。
func takeText(text string, budget int) (kept string, used int, cut bool)

// internal/httpapi
// recordInput 的 user 分支：req.UserInputDocument(len(raw), cfg.InputMaxChars)
// requestLogStats：新增 "input_max_chars"（/stats 的 request_log 块）

// internal/webui
// pages/requests.js：inputPanelTitle(row) → '输入' | '输入（未录制）' | '输入（已截断：…前 N 字符）'
```

`omitted` 计数键的语汇（沿用 M23 的 `type[:role]` 风格，新增两个明确的非类型键）：

| 键 | 含义 |
|---|---|
| `function_call` / `function_call_output` / `reasoning` / `message:assistant` / `message:developer` / `tools` / `instructions` | 既有的「这类内容网关从不落库」计数 |
| `input_image` / `input_file` / `unknown`（part 的 `type`） | user 消息里的非文本 part 被丢弃 |
| `over_cap` | 文本 part 因超出字符上限未落库（消息本身仍在 `input` 里，带它的前 N 字符） |
| `message:user:content` | content 形状读不懂（对象/非法 JSON），整段不落库（`null` 与缺省原样保留：里面没有内容） |

## 数据流

```
POST /v1/responses
  └─ recordInput(key, req, clientHint)              ← recording.InputModeFor(key.RecordInputMode)
       ├─ full     : 整份请求体 → redact() → truncate(max_bytes, rune 安全)      【不受上限影响】
       ├─ user     : UserInputDocument(len(body), recording.input_max_chars)
       │               └─ 每条 user 消息：clampUserMessage(content, maxChars)
       │                    ├─ 文本 part（input_text/text）：按剩余预算截断（rune 精确前缀）
       │                    ├─ 预算耗尽的文本 part：丢弃 + omitted["over_cap"]
       │                    ├─ 非文本 part（input_image/…）：丢弃 + omitted[<type>]
       │                    └─ content 读不懂：丢弃 + omitted["message:user:content"]
       │             → redact(redact_paths) → truncate(max_bytes)
       ├─ metadata : 正文为空，request_bytes 仍记录
       └─ off      : 正文与 request_bytes 都为 0（控制台流量强制这一档）
  └─ persist() 用同一份 payload 写 request_logs、responses.request_json 与 hook 事件的 input
  └─ recordDenied() 走同一个 recordInput（本地拒绝路径同样受限、同样脱敏）
```

顺序上：**字符上限在序列化之前**（改的是文档内容），`redact_paths` 与 `max_bytes` 在其后
（它们作用于整份序列化后的 JSON），三者不互相干扰。

## 异常与边界

- 上限 `0`、或历史行（写这些行的进程还没有本机制）：旧行为，文档里不出现 `input_max_chars` /
  `input_truncated`；两种进程共存期间日志格式是「新行多两个键」，读侧只多不少。
- 上限为负：配置校验期即报错（`recording.input_max_chars must be >= 0`），不会走到请求路径。
- 无 user 条目（例如纯 `previous_response_id` 续接）：仍是 `input: []` + 计数，行为不变。
- 输入是字符串简写 `{"input":"…"}`：`Items()` 已合成为一条 user 消息，照常受上限约束。
- 一条 user 消息的 content 是 JSON 字符串（而非 part 数组）：截断后仍序列化为字符串，形状不变。
- 图文混排：图片 part 丢弃并计数（此前会连同 base64 一起落库——本里程碑顺带修掉了这个体积/隐私口子）。
- content 是不可读形状（对象、非法 JSON）：整段不落库并计 `message:user:content`；不 panic、不失败，
  行照样写（沿用「写不成正文也要留一行」的既有取向，M25）。`null` 与缺省的 content 原样保留：
  里面没有内容，没什么可截的。
- 切点：按 rune 切，不会产生非法 UTF-8；`max_bytes` 那条路径的 rune 边界保护保持不动。
- 与 `redact_paths` 的交互不变：脱敏在截断之后作用于整份文档（`redact_paths=["input"]` 仍整列打码）。
- 与身份维度无关：客户端/模型/工作区/会话/调用类型/标题与凭据维度都不受本次影响（M27/M30 口径不变）。
- 与标题指纹无关：`TitlePromptFingerprint` 读的是**解析后的请求**，不是落库文档（M27），
  因此指纹与关联逻辑不受截断影响。

## 测试策略

- `internal/config`：默认 100；`input_max_chars: 0` 合法；负值报错（表驱动 + 默认值断言）。
- `internal/responses`：既有三个 `UserInputDocument` 用例改为传参（不限的传 `0`，断言不变）；新增
  1. 两条各 150 字符（含中文）的消息、上限 100 → 各留前 100 rune、`input_truncated`/`input_max_chars`
     正确、序列化结果仍是合法 JSON 与合法 UTF-8；
  2. 上限 0 → 长文本原样、无新字段（旧行为回归）；
  3. 图文混排 → `omitted["input_image"] == 1`，且文档里不出现 `data:image`；
  4. 一条消息两个文本 part 共享预算 → 第二个计 `over_cap`；
  5. 字符串简写长输入 → 被截断且仍是 JSON 字符串；
  6. content 为对象 → 计 `message:user:content`、不 panic。
- `internal/httpapi`：默认口径下发一条 300 字符提问 —— 日志含前 100 字符、不含第 120 字符之后的哨兵、
  含 `"input_max_chars":100` 与 `"input_truncated":true`；切 `full` 后哨兵仍在（与
  `TestRecordingSwitchesAreIndependent` 同风格）。既有短文本断言（`v1_test.go`、`truncation_test.go`、
  `request_dimensions_test.go`、`session_identity_test.go`、`mcp_test.go`）逐个核对不受影响；
  `/stats` 的 `request_log.input_max_chars == 100`。
- `internal/webui/tests/requests_test.mjs`（node，`make ui-base` 覆盖）：按 `reasoningEffortCell` 的既有
  vm 模式求值 `inputPanelTitle`，覆盖「已截断 + 有上限」「已截断无上限」「未截断」「未录制」四种输入。
- `scripts/ui-harness/keys.page.html`（真浏览器，`make ui-check`）：`#requests` 视图里就地改 fixture
  （`input_truncated = true; input_max_chars = 100`）后开「详情」，断言弹窗文本含
  「每条用户消息只留前 100 字符」，然后还原 fixture——不动 `fixtures.json` 快照、不需要 `--refresh`。
- `make verify`（vet + test + ui-base + build）全绿；`make ui-check` 33 视图全绿（需真浏览器，见下）。

## 依赖

标准库 + 既有包（`internal/{config,responses,httpapi,webui}`）；不新增外部依赖、不改数据库 schema
（录制文档本来就是 JSON 列，历史行不回填）。

## 环境说明（本次实现）

本会话沙箱里：Go 走工作区自带工具链（`.cache/go`，`scripts/goenv.sh` 指向的 `$HOME/sdk/go` 在此不存在），
node v22 可用（`make ui-base` 真跑），但 `make ui-check` 会**带原因跳过**（`/usr/bin/firefox` 是 snap
包装器，答不出 Mozilla 版本）。所以浏览器走查（含本次新增的 harness 断言）需要在宿主终端跑一次，
「跳过」不当作「通过」。

## 实现与设计差异

- **里程碑编号**：计划里写的是 M78，落地时发现 M78/M79/M80 已被并行工作区占用（分别是请求日志路由路线、
  沙箱工作区视图、Key 批量导入），因此改为 **M81**。设计与规格文档里的编号同步成了 M81。
- **数组分支拆成了 `clampContentParts`**：计划里只列了 `clampUserMessage`。实现时把「content 是 part 数组」
  这一段拆成独立函数，是因为它自己带预算循环、丢弃计数与「part 无 text 字段则原样保留」三条规则，
  混在一个函数里读不出边界。
- **文本载体的判定写死了两种**：计划里说「文本 part」，实现时按 `itemText`/`TitlePromptFingerprint` 的既有
  语汇定成 `input_text` 与 `text`（见 `textPart`），其余类型一律算「本包不猜的内容」→ 丢弃 + 计数。
- **`inputRecord.Truncated` 仍是单字段**：它现在有两种来源（每条消息的字符上限、`recording.max_bytes`）。
  没有为字符上限新增独立字段，因为那会动日志列/读取路径；改为在字段注释里写明语义，文档层用录制文档自己的
  `input_truncated`/`input_max_chars` 区分（`request_logs.request_json` 是 JSON 列，无需迁移）。
- **`/stats` 的 `input_max_chars` 从「可选」变成了实做项**：写测试时确认了它必须存在——否则「这台机器到底
  有没有上限」只能去翻配置文件或数据库正文，而正文里被截断的行看起来就是一句短提问。
- **harness 断言多了一条反面判据**：除「截断行必须写明上限」，还钉了「同一份文档去掉 `input_truncated`
  后不得出现该提示」——否则将来把这句话写成无条件文案，第一条断言仍然会通过。
- **控制台文案多覆盖了一处**：`full` 档的标签也补了「不受上限影响」，因为操作者选它时最需要知道
  自己拿到的是原样字节。
- **环境**：`make build`（ui-dist 压缩镜像 + overlay）在本沙箱里需要下载 overlay 构建路径缺的模块
  （network 可用），构建成功；`make ui-check` 如 §环境说明所述在本会话跳过，真浏览器走查留在
  `docs/TODO.md` 的 M81 未完成项里。


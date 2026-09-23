# 请求日志（规格）

请求日志是网关的可观测面：每条 `/v1/responses` 请求留下一行，回答「谁在什么时候调用了什么、
结果如何、花了多少」。本文件描述**目标行为**；实现里程碑见括号标注。

相关：`docs/design/m23-input-recording.md`（录制口径）、`docs/design/m25-log-retention.md`
（写入兜底与保留期）、`docs/design/m27-request-dimensions.md`（身份维度与消耗度量）、
`docs/design/m30-request-log-owner-dimensions.md`（用户/账户与 API Key 维度）、
`docs/billing.md`（计量与账本口径）。

## 1. 录制通道

三条通道互相独立，各自有开关：

| 通道 | 开关 | 默认 | 内容 |
|---|---|---|---|
| 输入 | `recording.record_input`（可被 Key 覆盖） | `user` | `full` 整份正文（**保留全部**）/ `user` 只留最新一条「人说的话」的**纯文本** / `metadata` 不落正文 / `off` 不落正文 |
| 输入长度上限 | `recording.input_max_chars` | 2000 | 防呆上限：只保留不超过它的 user 消息；样板另有标记判定；`0` = 不限长度 |
| 思考文本 | `recording.record_reasoning`（可被 Key 覆盖） | 关 | 模型的思考文本 |
| 最终输出 | `recording.record_output_text`（可被 Key 覆盖） | 关 | 模型的最终回答 |
| 会话标题 | `recording.record_title` | **开** | 标题调用产出的会话标题 |

`record_input=user`（默认）只保留用户自己写的输入，而且**只保留其中一条 user 消息的纯文本**（最新一条合格的）：
系统/开发者指令、工具定义、工具调用与工具输出、历史 assistant 轮次、压缩历史、以及消息里的图片
**都不落库**（M82 起连计数也不留）。

**输入口径（M82，M83 修订）**：`request_json` 就是**一段纯文本**——不是 JSON 文档、没有
`type`/`role`/`content` 结构、没有计数与阈值字段。规则：

- **只留最新一条「人说的话」**：从末尾往回找，取第一条同时满足三条的 user 消息——有文本（去掉首尾空白后非空）、
  **不是客户端样板**、且**不超过** `recording.input_max_chars`（默认 2000，按字符计、中文算 1；恰好等于也保留）。
- **样板由标记判定，不由长度判定**：以 `Current runtime context.`（DSH 的 runtime 快照）、
  `<environment_context>`（Codex 的环境块）、`You are an AI agent powered by DeepSeek Harness.` /
  `You are a coding agent running in the Codex CLI`（两个 agent 自己的系统提示）、`<system-reminder>`、
  `<skills_instructions>`、以及两个标题调用提示词开头的消息一律跳过。**为什么必须是标记**：真实提问常常比短的
  样板长、比长的样板短——2026-09-23 的现场是某浏览器 DSH 租户的提问 117–521 字符、末尾 runtime 快照 542 字符，
  在「100 字符才留」的口径下**它每一条请求的正文都是空的**（那台实例 14:11 之后 71 行 71 空；调大阈值还会把
  542 字符的样板当成正文落库）。改成标记判定后，它的 117 字符提问正常落库。
- 保留就**整条原样保留**（多条文本 part 用换行连接），不再截断——日志里不会出现「看起来完整、其实是前 N
  字符」的假头。上限只是防呆：不让人整份粘贴的文件变成日志正文。
- 空的（空文本/纯空白/只有图片/读不懂）与超长的消息都会**跳过**并继续往前找；一条都没有 → 正文为空
  （行照写：状态、体积、身份维度都在）。`input_recorded=false` 且该行 `record_input_mode=user` 的含义是
  「策略要记，但没有可保留的用户输入」——**看不出具体原因**（这是有意的收窄）。
- `recording.input_max_chars: 0` = **不限长度**（样板仍然跳过）：把所有非样板且带文本的消息按顺序换行拼接。
- **要保留全部（原样字节）就把该 Key 的输入录制切成 `full`**：该档存整份请求正文的 JSON，含工具定义、
  工具输出、系统指令与图片，**不受**上述任何规则限制，仍只受 `recording.max_bytes` 兜底。
  `metadata`/`off` 不落正文；控制台智能问答流量服务端强制 `off`。
- `recording.redact_paths` 管的是身份列；纯文本没有 JSON 路径可指，因此**唯一的兼容规则**是：
  路径列表里出现 `input`（历史上就是「不要留输入」的意思）→ 正文清空。
- 历史行不重写、不回填：M23–M81 期间写的行是 JSON 文档（M81 的还可能带 `input_truncated`/`over_cap`），
  控制台与 MCP 对新旧两种形状都能读。

**身份维度不受上表影响**（M27）：客户端、模型、工作区、会话、调用类型、标题这七列是元数据，
只要该请求写了日志行就一并记录——包括 `record_input=off`（那一行只有身份、没有正文）。
操作者若要让某一列不留痕，把列名（如 `workspace`、`session_id`）写进 `recording.redact_paths`；
下表的最后两行是**凭据维度**（M30），来源与脱敏规则都不一样，见本节末尾。

## 2. 身份维度（M27 / M30）

| 列 | 含义 | 取值来源 |
|---|---|---|
| `client` | 哪个编码 agent 在调用 | `dsh` / `codex` / `console` / `unknown`（请求体结构优先，User-Agent 仅兜底；`console` 是控制台智能问答自己发的请求，User-Agent 由服务端设置） |
| `model` | 客户端请求的模型名 | 请求的 `model` 字段；**与账单口径一致**（发票按 `usage_records.model` 分组） |
| `resolved_model` | 路由后的规范模型名 | 路由结果；被本地拒绝的请求为空 |
| `workspace` | 客户端的工作区根路径 | DSH 的沙箱策略行 / Codex 的 `<environment_context><cwd>` |
| `session_id` | 客户端的根会话标识 | 优先取 Codex `client_metadata` 权威快照/平铺字段及兼容请求头中的 `session_id`；通用 `metadata.session_id` 也可提供。无根标识时回退 `thread_id`，最后回退 `prompt_cache_key` |
| `call_kind` | 会话轮次还是辅助调用 | `agent` / `title` |
| `title` | 会话标题 | DSH 的标题响应文本 / Codex 结构化响应的 `title` 字段（兼容纯文本）；**只写在标题调用那一行** |
| `account_id` / **用户** | 这笔消耗算在哪个账户（租户） | 该请求使用的 API Key 的所属账户；控制台列头写「用户」，详情写「用户（账户）」 |
| `api_key_id` / **API Key** | 用的是哪个 Key | 该请求的凭据自身（`api_key_id`），服务路径与本地拒绝路径都写 |
| **供应商**（M53） | 每一次上游尝试分别由哪家服务 | **不在日志行上**：来自计量行的 `usage_records.provider_id`。一个请求可能被多家服务（故障转移），所以它是计量行维度，不是日志列 |
| **上游模型**（M78） | 那一次尝试真正发给上游的模型名 | **不在日志行上**：计量行的 `usage_records.upstream_model`，**写时快照**（路由是可编辑的配置，历史不随配置漂移） |
| **路由路线**（M78） | 每一跳走的是哪条路由、结果如何 | **不在日志行上**：计量行的 `route_id` + 该次尝试的结果（`status`/`error_code`/延迟/成本），按 `attempt_no` 排序；一个请求可以有多跳 |
| **映射规则**（M78） | 请求的模型经哪条映射规则变成规范模型 | 日志行的 `matched_rule`：`model:<public>` / `mapping:<kind>:<pattern>` / `alias:<x>` / `fallback:<y>`；本地拒绝的请求为空（它没有走到模型） |

**用户与 API Key 是凭据维度**（M30）：它们不来自请求正文，而是鉴权时已知的事实，因此与其余
维度一样不受录制口径影响（`record_input=off` 也写），也**不参与 `redact_paths`**——脱敏管的是
正文解析出来的列，要「让某个 Key 不留痕」，手段是停用该 Key。

**供应商是计量行维度**（M53），与上表其余维度有一条根本区别：日志行只有一行，而一个请求可以有
多次上游尝试、落在不同供应商上（同一模型的不同供应商各自计价，见 `docs/pricing.md`）。因此
「供应商」既不能存在 `request_logs` 上，也不能由「最终是谁答的」推断——把一次失败转移的成本算到
最后成功那家头上，等于把一家的成本搬到另一家。它的口径是**谁服务的算谁的**：按
`usage_records.provider_id` 归属那一次尝试的 token 与成本。相应地，列表/详情返回的 `providers`
是 `[{id, name}]` 数组而不是单个字段。

**名字是读时标签**：日志行只存 id，列表/详情/统计里的 `account_name`、`api_key_name`、
`api_key_prefix`、`provider_name` 是查询时从 `accounts` / `api_keys` / `providers` 现取的
（与 token、成本不落列同一条规矩）。
好处是改名立刻生效、不会把同一个 Key 在统计里裂成两桶；两表都没有硬删除，所以按 id 一定取得到
名字。`api_keys.name` 不唯一（同一账户可重名），因此**分组按 id**、名字只作显示。

**路由路线是计量行维度**（M78），与供应商同一条理由、同一张表：一次请求可以有多次上游尝试，
所以「走了哪条路由、发给哪家的哪个上游模型名」是**一对多**的事实，不能压进日志行，也不能由
「最后成功的那一跳」代表（失败那跳的成本与路线都真实存在）。它与供应商的关系是**同一份事实的
两种读法**：`providers` 是这些尝试里出现过的供应商集合（按首次尝试顺序去重），`attempts` 是它们
的**顺序与结果**（`route_id`、`upstream_model`、`status`、`error_code`、延迟、成本）。

两个快照字段的含义要分清：`route_id` 与 `upstream_model` 记的是**当时**选了哪条路由、发了哪个
模型名，读时不 join `routes` 表——路由可以被编辑、也可以随供应商删除而消失，历史不该跟着变。
因此路由被删后界面只显示 `#<id>`（可在「模型与路由」页对照），上游模型仍是当时的字符串。
迁移 0027 之前写下的计量行这两个字段是 `0`/`''`，界面显示「（未知路由）」与「—」，不回填。

识别是**结构性**的：只看请求里该出现的位置（顶层 `instructions`、首条 developer 消息、以
`<environment_context>` 开头的消息……），不做全文匹配——实测本机库里 265 行含 `Codex CLI`
的记录里有 259 行其实是 DSH 请求，那段文字出现在它的工具输出里。

已知边界：DSH 只在 `workspace-write` 模式下把工作区写进请求，`read-only` 与
`danger-full-access` 下 `workspace` 为空；会话经压缩后 runtime context 可能被替换，此时
`session_id` 仍可用于归组。

### 推理强度（执行元数据）

列表与详情返回独立字段 `reasoning_effort`，控制台在「模型」列后显示「推理强度」。
它记录网关应用规范模型的 default/force 策略后、交给供应商适配器的 `reasoning.effort`，
不是当前模型配置的读时值，也不是思考 token 数；供应商仍可能映射参数或采用自身默认值。
多次上游尝试时记录最后一次尝试的值。

该字段独立持久化，不依赖请求正文录制；`record_input=off` 时仍保留。
`recording.redact_paths` 包含 `reasoning_effort`、`reasoning.effort` 或 `reasoning` 时会清空该字段。
显式 `none` 原样显示，不与未指定混同；未指定、未进入上游尝试的本地拒绝请求及旧日志返回空字符串，
页面显示 `—`，不猜测上游实际采用的强度。历史日志不回填。

## 3. 消耗（token / 成本）

token 与成本**不复制**到日志表，而是按 `request_id` 从计量表 `usage_records` 关联
（一个请求可能因 failover 有多次上游尝试，计数求和、延迟取最差）。因此：

- 日志里的消耗与账单**同源**，不会出现两个口径；
- 保留期清理日志、不清理计费，历史消耗不会因为日志过期而消失；
- **被本地拒绝的请求没有计量行**（它从未到达上游），接口返回 `usage.metered=false`，
  控制台显示「未计量」——这与「消耗为 0」是两句不同的话。

token 口径与计费一致：输入 = `input + input_cache_hit + input_cache_miss`，输出 = `output`，
思考 = `reasoning`。列表与详情的 `usage.cached_tokens`、维度统计每行的 `cached_tokens`
均汇总计量行 `dimensions_json.input_cache_hit`，缺失该维度时为 0。缓存命中 token **已经包含在
`input_tokens` 中**，是输入的子集，不应再加到输入或总 token 上；多次尝试的缓存命中数同样求和。
没有计量行的请求仍返回 `usage.metered=false`，不把「未计量」当作零消耗。

## 4. 查询与统计

| 端点 | 用途 |
|---|---|
| `GET /admin/api/v1/requests` | 分页列表；可按 `account_id`/`api_key_id`/`provider_id`/`days` 与六个身份维度过滤；每行含 7 个身份字段、`matched_rule`、`account_name`/`api_key_name`/`api_key_prefix`、`providers`（`[{id, name}]`，失败转移的行有多项，按首次尝试顺序）、`attempts`（每次上游尝试的 `route_id`/`provider_id`/`provider_name`/`upstream_model`/`status`/`error_code`/`terminated_reason`/`latency_ms`/`ttft_ms`/`cost_micros`/`charge_micros`/`created_at`，按 `attempt_no` 升序）与 `usage` |
| `GET /admin/api/v1/requests/{id}` | 单条详情：输入/思考/输出（按录制开关）＋身份＋`matched_rule`＋用户/Key/供应商的名字＋同上的 `attempts`（路由路线）＋消耗 |
| `GET /admin/api/v1/requests/dimensions` | 维度统计（**可分页、可排序**）：`group_by=client\|model\|resolved_model\|workspace\|session\|call_kind\|account\|api_key\|provider`，汇总请求数、已计量数、token、成本与首次/最近出现时间；`session` 分组额外带标题与工作区，`account`/`api_key`/`provider` 分组额外带名字（`api_key` 还带前缀）；`sort=last_seen\|requests\|charge`（默认 `last_seen`），`limit`/`offset` 同列表契约，响应 `total` 是**分组数** |
| `POST /admin/api/v1/requests/prune` | 立即执行保留期清理（admin） |

`account_id`/`api_key_id`/`provider_id` 是**精确匹配**的数字过滤；非数字取值返回 400 并指出参数名
（M30 起，之前是静默忽略——「筛了却返回全部」比报错更难查）。

`account`/`api_key` 分组的 `key` 是**数字 id 的字符串形式**（名字随行返回，因为 `api_keys.name`
不唯一）；`key` 为空串表示未知桶：`account_id`/`api_key_id` ≤ 0 的历史行或兜底行，
控制台显示「（未知）」，计数与其他桶一样保留。

### 路由路线（M78）

列表与详情都返回 `attempts`，同一份数据在两处读：「上游模型」列取其中去重后的模型名（按尝试顺序
用 ` → ` 连接，失败转移的行会有两个），「路由路线」列把每一跳写成 `<路由 #id> <供应商><结果标记>`
（`✓` = `completed`，`✗ <error_code>` = `failed`，其他状态显示状态名）。详情另给一行模型解析链：
`请求的模型 →[matched_rule]→ 路由到的模型`，随后每次尝试一行（route id、供应商 #id、上游模型、
status、错误码、结束原因、延迟、首字延迟、成本与对客）。

口径与边界：

1. **顺序即路线**：`attempts` 按 `attempt_no` 升序，正是网关尝试的顺序；`providers` 由它去重得到，
   所以两个字段读起来是同一个故事（谁先被试、结果如何）。
2. **没有计量行就没有路线**：本地拒绝（余额不足、账户停用等）的请求从未到达上游，`attempts` 为空，
   控制台显示「无上游尝试」而不是「一跳空白」或 `#0`；`matched_rule` 与 `resolved_model` 同为 `''`。
3. **快照不随配置变**：`route_id`/`upstream_model` 是当时写下的值（见 §2），`matched_rule` 同理——
   路由与映射规则改过之后再看旧日志，答案仍是当时那一跳。
4. **独立于正文录制**：路线与规则是路由事实，`record_input=off` 也记，也不在 `recording.redact_paths`
   的作用范围内（同 `resolved_model`）。

### 供应商维度与筛选（M53）

`group_by=provider` 是唯一**口径不同**的分组，读它之前必须知道三件事：

1. **成本按计量行归属**：同一模型在不同供应商上的单价不同，这正是不分组就看不见的那部分。
   一次失败转移的请求会在每一家各出现一次，**各分组「请求数」之和可能大于窗口总请求数**——
   这是设计，不是错误。控制台把这句写在「请求数」表头与卡片说明里，API 的 MCP 工具描述同样写明。
2. **恒读原始计量行**：小时汇总 `request_dimension_rollups` 按请求预聚合（一行一个请求、
   计量已跨尝试求和），供应商信息在那里已经不存在，因此该分组不走汇总表，而是扫窗口的计量行。
   带 `provider_id` 过滤时同理（汇总表只有日志列，没有 `request_id` 可关联计量行）。
   代价是这两种读比走汇总慢，换来的是不会给出「已完成的小时里一个供应商都没有」这种看似合理的错答案。
3. **`key` 为空串是未计量桶**：本地拒绝的请求没有计量行，因此不属于任何供应商；它按
   `provider_id=0` 归入未知桶（`provider_id`/`provider_name` 为 0/空），控制台显示「（未知）」。
   把它丢掉会让供应商视角与请求日志对窗口的大小各说一套。

`provider_id` 过滤的口径是**请求**（与列表一致）：选中有该供应商计量行的请求。所以
`group_by=provider&provider_id=N` 得到的是「N 服务过的那些请求，再按供应商拆开」——N 自己的桶，
以及这些请求在别家上留下的尝试。

MCP 侧：查询工具 `list_requests` / `get_request` 同样返回身份字段、`api_key_id`/`api_key_name`
与 `providers`；后台工具 `admin_request_dimensions` 由路由表自动暴露，`group_by` 与
`provider_id` 取值同步扩展，其工具描述写明上述第 1 条的计数口径。

维度统计的**排序与分页**（M31）：三种排序键都是**降序**，并以分组键升序兜底（保证分页不重不漏）：

| `sort` | 含义 | 对应列 |
|---|---|---|
| `last_seen`（默认） | 最近一次请求时间，最近活跃的排最前 | 「最近一次」 |
| `requests` | 请求数最多（M27 起的原顺序） | 「请求数」 |
| `charge` | 对客成本 `charge_micros` 最高（与列表「成本」列同源） | 「成本」 |

未知 `sort` 返回 400 并列出取值（不静默忽略）；`limit` 默认 20、上限 200，`offset` 语义与列表一致。
`total` 是**分组数**（不是请求数），且与 `sort` 无关；`first_seen`/`last_seen` 是**窗口内**的极值
（窗口外的请求不参与，保留期清理会让它前移）。

**小时汇总与实时性**：`recording.dimension_rollup_enabled` 默认开启。完整、已结束且版本有效的
UTC 小时读取八维实际组合的汇总表；当前小时、查询边界、回填尚未覆盖的区间及失效小时，按时间范围
查明细补算。两部分互斥后合并，统计行与分组总数在同一数据库快照内读取，因此后台每 30 秒更新
不意味着统计有 30 秒延迟。准确性以已提交的日志及计量为准，不包括仍在审计写入队列中的请求。

`requests` 按请求计一次，`metered` 表示存在至少一条计量记录的请求数；一个请求发生多次上游尝试，
两个计数仍最多各加 1，token、缓存命中和费用则累计全部尝试。这修正了旧版 JOIN 后重试被重复计数的问题。

新增、迟到计量、统计字段修改和删除，在源数据事务内使受影响小时失效；正文更新和重复结算不失效。
日志清理后立即按剩余日志补算，后台重建并分批删除旧汇总贡献，计费明细依然不清理。
首次升级只建立结构，后台按每批 500 行发现历史小时并持久化游标；未完成汇总不会参与查询，重启可继续。
关闭 `dimension_rollup_enabled`（或设置 `GW_RECORDING_DIMENSION_ROLLUP_ENABLED=false`）可回退到准确的
明细查询，失效跟踪仍然保留，重新开启无需担心旧汇总被误用。关闭 `database.wal` 时也使用明细查询，
诊断中说明原因；后台流式构建依赖 WAL 的并发读写。

`/admin/api/v1/stats` 的 `request_log.dimension_rollup` 提供有效开关、历史发现游标及完成标记、
待处理的已结束小时数、进程内最近成功时间和错误。`backfill_complete` 仅表示历史小时发现完成，
还要结合 `pending_hours` 判断汇总是否已追上；当前小时不计入待处理小时。`/metrics` 提供对应游标与待处理数。
设计、恢复机制和测量方法见 [小时汇总设计](design/request-dimension-rollups.md)。

控制台「请求日志」页：「维度统计」卡在**列表卡之上**（M31），按客户端/模型/工作区/会话/
用户（账户）/API Key/供应商筛选（筛选栏在下方列表卡内，两张表共用），统计卡工具栏可在三种排序键之间切换
（切换回到第 1 页），表格顶部标出当前排序列（`↓`），底部分页器写「共 N 个分组」以区别于列表分页器的
「共 N 条」——两个分页器各说各的口径。列表显示身份、用户与 Key、供应商、token（入/出）与成本（按展示币种渲染，
换算值带「≈」），列表底部有一行**本页汇总**（M29）。Key 下拉随账户联动（选中账户只列该账户的 Key），
供应商下拉列 `/providers`（名字 + `#id`），超过 1000 个 Key 的部署下拉只列前 1000
（配置类列表的既有上限），API 过滤对任意 id 仍精确。

供应商的展示口径（M53）：列表「供应商」列显示该请求的**全部**计量供应商（失败转移的行是
「A、B」，tooltip 写明两家都在这一行上有计量行），没有计量行的行显示「未计量」而不是空白；
统计卡选「供应商」时行显示「名字 #id」，`id=0` 显示「（未知）」，且「请求数」表头写明各分组之和
可能大于窗口总数的原因——一组比总数还大的数字，旁边没有这句话看起来就是缺陷。

路由路线的展示口径（M78）：列表在「供应商」之后新增「上游模型」与「路由路线」两列。前者按尝试
顺序列出真正发出去的上游模型名（`a → b`，tooltip 逐行给出每次尝试的模型与供应商），后者把每一跳
写成「`#路由id` 供应商 + 结果标记」（`✓` 成功 / `✗ <错误码>` 失败 / 其他状态显示状态名），
tooltip 给出每次尝试的全部字段并提示路由 id 可在「模型与路由」页对照；没有计量行的行显示
「未计量」与「无上游尝试」，迁移前的行显示「—」与「（未知路由）」。详情在身份块里补「上游模型」
与「映射规则」，并新增「路由路线」区块（首行模型解析链，其后每次尝试一行）。

汇总行的口径（M29）：**只合计当前页已加载的行**（卡片上「本页过滤」生效时就是屏幕上剩下的行），
tokens 与成本落在它们各自表头列的正下方；未计量的行只计入行数（标签写「已计量 M · 未计量 K」），
两格显示「未计量」而不是 0。它**不是**筛选窗口的合计——窗口口径看「维度统计」卡
（分页器给出本窗口的分组总数，各组之和要翻完各页才是窗口合计），窗口行数看分页器的「共 N 条」。

## 5. 保留期与写入兜底

- `recording.retention_days`（默认 30，0 = 不清理）决定日志与已存响应的寿命；
  计费（usage/ledger）与审计记录不受影响。
- 清理是每日任务，也可在控制台手动触发；分批删除以免长时间占住唯一的写连接。
- 内容写入失败时退化为**无正文骨架行**：身份、状态、体积、请求 id 全部保留，失败与丢弃
  计数在 `/stats` 的 `request_log` 块与 `/metrics` 中可见。身份维度**不参与**冲突更新，
  因此骨架行重试不会抹掉第一次写入捕获的身份（`account_id`/`api_key_id` 同样如此，M30 起有
  测试钉住）。

## 6. 状态

**已实现（M27）**：身份七列、消耗读时关联、维度筛选与统计、控制台展示与 UI 走查断言。
**已实现（M29）**：控制台列表底部的本页汇总行（tokens 入/出与成本，按当前页合计）。
**已实现（M30）**：用户（账户）与 API Key 维度——列表/详情带名字、按 `api_key_id` 过滤、
维度统计两个新分组、控制台两列与两个下拉、MCP 查询工具返回 Key 归属，以及
`(account_id|api_key_id, created_at, id)` 两条索引（迁移 0009）。
**已实现（M31）**：维度统计卡置顶；统计表服务端分页（`offset` + 精确分组总数 `total`）与三种排序键
（默认 `last_seen` 降序，`sort=requests|charge` 可切换），控制台排序下拉、`↓` 标记与「最近一次」列，
分页器写「共 N 个分组」；计数查询走覆盖/时间索引，不 join 计量表。
**已实现（M53）**：供应商维度——`group_by=provider`（按 `usage_records.provider_id` 归属成本，
恒读原始计量行）、`provider_id` 过滤（请求级、走 `EXISTS`）、列表「供应商」列与详情字段
（`providers` 为数组）、`/providers` 下拉与统计分组、MCP 后台工具同步暴露；口径与取舍见
`docs/design/m53-request-provider-dimension.md`。
**已实现（M78）**：路由路线——计量行新增 `route_id`/`upstream_model`（迁移 0027，写时快照）、
日志行新增 `matched_rule`；列表/详情返回 `attempts`（按尝试顺序的完整路线）、`providers` 改由它
去重得出（顺序变为「首次尝试」）、控制台新增「上游模型」「路由路线」两列与详情「路由路线」区块、
后台工具描述同步；`RequestProviders` 由 `RequestAttempts` 取代。设计与取舍见
`docs/design/m78-request-log-route-path.md`。
历史行（迁移 0008 之前）的七列为空，控制台显示「—」，聚合归入「（未知）」桶；
`account_id`/`api_key_id` ≤ 0 的行归入「（未知）」桶，计数同样保留；迁移 0027 之前的计量行
`route_id`/`upstream_model` 为空，控制台显示「（未知路由）」与「—」，同样不回填。

相关设计：`docs/design/m27-request-dimensions.md`、`docs/design/m29-request-log-page-summary.md`、
`docs/design/m30-request-log-owner-dimensions.md`、`docs/design/m31-request-log-stats-pagination.md`、
`docs/design/m53-request-provider-dimension.md`、`docs/design/m78-request-log-route-path.md`。

Codex 标题辅助请求根据元数据 `turn_trigger=thread_title` 或 user 消息开头的专用任务标题提示词识别为 `call_kind=title`。标题和描述一起返回时仅记录 `title`；损坏的 JSON 对象或缺失标题时留空。标题辅助请求可能使用独立的 `prompt_cache_key`，若携带显式根会话 ID 则归于根会话；没有明确的根会话标识时，已知 Codex 标题模板可通过同账户、Key、工作区内 ±120 秒的唯一首条提示词精确指纹候选关联；候选冲突时恢复独立分组。正文录制关闭/仅元数据、启用脱敏或混合媒体输入时不推断，详见 `session-grouping-fix.md`。历史日志未录制响应正文时不能恢复标题。

会话标识字段与源码依据见 `docs/session-grouping-fix.md`。日志分组标识与缓存路由键独立；读取显式元数据不改上游 `prompt_cache_key`，不增加统计查询或 SQL 关联。

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
| 输入 | `recording.record_input`（可被 Key 覆盖） | `user` | `full` 整份正文 / `user` 只留用户自己写的输入 / `metadata` 不落正文 / `off` 不落正文 |
| 思考文本 | `recording.record_reasoning`（可被 Key 覆盖） | 关 | 模型的思考文本 |
| 最终输出 | `recording.record_output_text`（可被 Key 覆盖） | 关 | 模型的最终回答 |
| 会话标题 | `recording.record_title` | **开** | 标题调用产出的会话标题 |

`record_input=user`（默认）只保留用户自己写的 user 消息，系统/开发者指令、工具定义、工具调用与
工具输出、历史 assistant 轮次只留 `omitted` 计数与 `request_bytes`。

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
| `session_id` | 客户端的会话键 | 请求的 `prompt_cache_key`（DSH 形如 `session-<uuid>`，Codex 为裸 uuid） |
| `call_kind` | 会话轮次还是辅助调用 | `agent` / `title` |
| `title` | 会话标题 | 标题调用的响应文本；**只写在标题调用那一行** |
| `account_id` / **用户** | 这笔消耗算在哪个账户（租户） | 该请求使用的 API Key 的所属账户；控制台列头写「用户」，详情写「用户（账户）」 |
| `api_key_id` / **API Key** | 用的是哪个 Key | 该请求的凭据自身（`api_key_id`），服务路径与本地拒绝路径都写 |

**用户与 API Key 是凭据维度**（M30）：它们不来自请求正文，而是鉴权时已知的事实，因此与其余
维度一样不受录制口径影响（`record_input=off` 也写），也**不参与 `redact_paths`**——脱敏管的是
正文解析出来的列，要「让某个 Key 不留痕」，手段是停用该 Key。

**名字是读时标签**：日志行只存 id，列表/详情/统计里的 `account_name`、`api_key_name`、
`api_key_prefix` 是查询时从 `accounts` / `api_keys` 现取的（与 token、成本不落列同一条规矩）。
好处是改名立刻生效、不会把同一个 Key 在统计里裂成两桶；两表都没有硬删除，所以按 id 一定取得到
名字。`api_keys.name` 不唯一（同一账户可重名），因此**分组按 id**、名字只作显示。

识别是**结构性**的：只看请求里该出现的位置（顶层 `instructions`、首条 developer 消息、以
`<environment_context>` 开头的消息……），不做全文匹配——实测本机库里 265 行含 `Codex CLI`
的记录里有 259 行其实是 DSH 请求，那段文字出现在它的工具输出里。

已知边界：DSH 只在 `workspace-write` 模式下把工作区写进请求，`read-only` 与
`danger-full-access` 下 `workspace` 为空；会话经压缩后 runtime context 可能被替换，此时
`session_id` 仍可用于归组。

## 3. 消耗（token / 成本）

token 与成本**不复制**到日志表，而是按 `request_id` 从计量表 `usage_records` 关联
（一个请求可能因 failover 有多次上游尝试，计数求和、延迟取最差）。因此：

- 日志里的消耗与账单**同源**，不会出现两个口径；
- 保留期清理日志、不清理计费，历史消耗不会因为日志过期而消失；
- **被本地拒绝的请求没有计量行**（它从未到达上游），接口返回 `usage.metered=false`，
  控制台显示「未计量」——这与「消耗为 0」是两句不同的话。

token 口径与计费一致：输入 = `input + input_cache_hit + input_cache_miss`，输出 = `output`，
思考 = `reasoning`。

## 4. 查询与统计

| 端点 | 用途 |
|---|---|
| `GET /admin/api/v1/requests` | 分页列表；可按 `account_id`/`api_key_id`/`days` 与六个身份维度过滤；每行含 7 个身份字段、`account_name`/`api_key_name`/`api_key_prefix` 与 `usage` |
| `GET /admin/api/v1/requests/{id}` | 单条详情：输入/思考/输出（按录制开关）＋身份＋用户与 Key 的名字＋消耗 |
| `GET /admin/api/v1/requests/dimensions` | 维度统计（**可分页、可排序**）：`group_by=client\|model\|resolved_model\|workspace\|session\|call_kind\|account\|api_key`，汇总请求数、已计量数、token、成本与首次/最近出现时间；`session` 分组额外带标题与工作区，`account`/`api_key` 分组额外带名字（`api_key` 还带前缀）；`sort=last_seen\|requests\|charge`（默认 `last_seen`），`limit`/`offset` 同列表契约，响应 `total` 是**分组数** |
| `POST /admin/api/v1/requests/prune` | 立即执行保留期清理（admin） |

`account_id`/`api_key_id` 是**精确匹配**的数字过滤；非数字取值返回 400 并指出参数名
（M30 起，之前是静默忽略——「筛了却返回全部」比报错更难查）。

`account`/`api_key` 分组的 `key` 是**数字 id 的字符串形式**（名字随行返回，因为 `api_keys.name`
不唯一）；`key` 为空串表示未知桶：`account_id`/`api_key_id` ≤ 0 的历史行或兜底行，
控制台显示「（未知）」，计数与其他桶一样保留。

MCP 侧：查询工具 `list_requests` / `get_request` 同样返回身份字段与 `api_key_id`/`api_key_name`；
后台工具 `admin_request_dimensions` 由路由表自动暴露，`group_by` 取值同步扩展。

维度统计的**排序与分页**（M31）：三种排序键都是**降序**，并以分组键升序兜底（保证分页不重不漏）：

| `sort` | 含义 | 对应列 |
|---|---|---|
| `last_seen`（默认） | 最近一次请求时间，最近活跃的排最前 | 「最近一次」 |
| `requests` | 请求数最多（M27 起的原顺序） | 「请求数」 |
| `charge` | 对客成本 `charge_micros` 最高（与列表「成本」列同源） | 「成本」 |

未知 `sort` 返回 400 并列出取值（不静默忽略）；`limit` 默认 20、上限 200，`offset` 语义与列表一致。
`total` 是**分组数**（不是请求数），且与 `sort` 无关；`first_seen`/`last_seen` 是**窗口内**的极值
（窗口外的请求不参与，保留期清理会让它前移）。

控制台「请求日志」页：「维度统计」卡在**列表卡之上**（M31），按客户端/模型/工作区/会话/
用户（账户）/API Key 筛选（筛选栏在下方列表卡内，两张表共用），统计卡工具栏可在三种排序键之间切换
（切换回到第 1 页），表格顶部标出当前排序列（`↓`），底部分页器写「共 N 个分组」以区别于列表分页器的
「共 N 条」——两个分页器各说各的口径。列表显示身份、用户与 Key、token（入/出）与成本（按展示币种渲染，
换算值带「≈」），列表底部有一行**本页汇总**（M29）。Key 下拉随账户联动（选中账户只列该账户的 Key），
超过 1000 个 Key 的部署下拉只列前 1000（配置类列表的既有上限），API 过滤对任意 id 仍精确。

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
历史行（迁移 0008 之前）的七列为空，控制台显示「—」，聚合归入「（未知）」桶；
`account_id`/`api_key_id` ≤ 0 的行归入「（未知）」桶，计数同样保留。

相关设计：`docs/design/m27-request-dimensions.md`、`docs/design/m29-request-log-page-summary.md`、
`docs/design/m30-request-log-owner-dimensions.md`、`docs/design/m31-request-log-stats-pagination.md`。

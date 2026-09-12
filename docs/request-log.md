# 请求日志（规格）

请求日志是网关的可观测面：每条 `/v1/responses` 请求留下一行，回答「谁在什么时候调用了什么、
结果如何、花了多少」。本文件描述**目标行为**；实现里程碑见括号标注。

相关：`docs/design/m23-input-recording.md`（录制口径）、`docs/design/m25-log-retention.md`
（写入兜底与保留期）、`docs/design/m27-request-dimensions.md`（身份维度与消耗度量）、
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
操作者若要让某一列不留痕，把列名（如 `workspace`、`session_id`）写进 `recording.redact_paths`。

## 2. 身份维度（M27）

| 列 | 含义 | 取值来源 |
|---|---|---|
| `client` | 哪个编码 agent 在调用 | `dsh` / `codex` / `unknown`（请求体结构优先，User-Agent 仅兜底） |
| `model` | 客户端请求的模型名 | 请求的 `model` 字段；**与账单口径一致**（发票按 `usage_records.model` 分组） |
| `resolved_model` | 路由后的规范模型名 | 路由结果；被本地拒绝的请求为空 |
| `workspace` | 客户端的工作区根路径 | DSH 的沙箱策略行 / Codex 的 `<environment_context><cwd>` |
| `session_id` | 客户端的会话键 | 请求的 `prompt_cache_key`（DSH 形如 `session-<uuid>`，Codex 为裸 uuid） |
| `call_kind` | 会话轮次还是辅助调用 | `agent` / `title` |
| `title` | 会话标题 | 标题调用的响应文本；**只写在标题调用那一行** |

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
| `GET /admin/api/v1/requests` | 分页列表；可按 `account_id`/`days` 与六个身份维度过滤；每行含 7 个身份字段与 `usage` |
| `GET /admin/api/v1/requests/{id}` | 单条详情：输入/思考/输出（按录制开关）＋身份＋消耗 |
| `GET /admin/api/v1/requests/dimensions` | top-N 维度统计：`group_by=client\|model\|resolved_model\|workspace\|session\|call_kind`，汇总请求数、已计量数、token、成本；`session` 分组额外带标题与工作区 |
| `POST /admin/api/v1/requests/prune` | 立即执行保留期清理（admin） |

MCP 侧：查询工具 `list_requests` / `get_request` 同样返回身份字段；后台工具
`admin_request_dimensions` 由路由表自动暴露。

控制台「请求日志」页：按客户端/模型/工作区/会话筛选，列表显示身份、token（入/出）与成本
（按展示币种渲染，换算值带「≈」），列表底部有一行**本页汇总**（M29），并有「维度统计」卡片。

汇总行的口径（M29）：**只合计当前页已加载的行**（卡片上「本页过滤」生效时就是屏幕上剩下的行），
tokens 与成本落在它们各自表头列的正下方；未计量的行只计入行数（标签写「已计量 M · 未计量 K」），
两格显示「未计量」而不是 0。它**不是**筛选窗口的合计——窗口口径看「维度统计」卡
（分组数不超过 limit 时，各组之和即窗口合计），窗口行数看分页器的「共 N 条」。

## 5. 保留期与写入兜底

- `recording.retention_days`（默认 30，0 = 不清理）决定日志与已存响应的寿命；
  计费（usage/ledger）与审计记录不受影响。
- 清理是每日任务，也可在控制台手动触发；分批删除以免长时间占住唯一的写连接。
- 内容写入失败时退化为**无正文骨架行**：身份、状态、体积、请求 id 全部保留，失败与丢弃
  计数在 `/stats` 的 `request_log` 块与 `/metrics` 中可见。身份维度**不参与**冲突更新，
  因此骨架行重试不会抹掉第一次写入捕获的身份。

## 6. 状态

**已实现（M27）**：身份七列、消耗读时关联、维度筛选与统计、控制台展示与 UI 走查断言。
**已实现（M29）**：控制台列表底部的本页汇总行（tokens 入/出与成本，按当前页合计）。
历史行（迁移 0008 之前）的七列为空，控制台显示「—」，聚合归入「（未知）」桶。

相关设计：`docs/design/m27-request-dimensions.md`、`docs/design/m29-request-log-page-summary.md`。

# M27：请求日志的身份维度与消耗度量

> 编号说明：M26 已被 `e42b066`（审计写入批量化 + 预编译语句复用）占用，本里程碑顺延为 M27。

## 1. 目标

请求日志过去只能回答「有一条请求、它多大、成功没有」。要回答**谁在调用、用哪个模型、
在哪个工作区、属于哪个会话、花了多少**，只能去翻正文——而正文恰好是最不可靠的一列：
它按 `recording.max_bytes`（默认 1 MiB）截断，实际库里 397/2297 行的 JSON 直接断在中间；
它又按保留期被清掉，而计费记录不会。

本里程碑把这些身份**在请求解析后立即提取**，落成 `request_logs` 的一等列；把 token 与成本
**读时**从计量真值 `usage_records` 关联；并为新的筛选与聚合补三条实测过的索引。

### 成功标准

| # | 标准 |
|---|---|
| 1 | DSH 请求记下 `client=dsh`、`workspace`、`session_id=session-<uuid>`、`call_kind=agent` |
| 2 | 标题调用记下 `call_kind=title` 与 `title`（模型产出的会话标题） |
| 3 | Codex 请求记下 `client=codex`、`workspace`（取自 `<environment_context><cwd>`）、裸 uuid 会话 |
| 4 | 模型双身份：`model`（客户端请求名，账单口径）与 `resolved_model`（路由后的规范名） |
| 5 | `record_input=off` 的行仍带身份（正文为空） |
| 6 | 列表/详情显示 token 与成本；维度统计卡按 client/model/workspace/session 汇总 |
| 7 | 带维度筛选的列表查询在 EXPLAIN 下无 temp B-tree；清理语句计划与改动前一致 |

## 2. 关键决策

| # | 决策 | 理由与取舍 |
|---|---|---|
| D1 | 身份维度**独立于正文口径**，`record_input=off` 也记 | 维度是元数据不是正文。代价：`off` 的语义从「什么都不留」变成「不留正文、只留维度」，控制台文案与配置注释同步改口径 |
| D2 | 标题（模型输出）默认收录，新增部署级开关 `recording.record_title`（默认 true） | 标题是会话级元数据，是让日志一眼可读的那一条信息；它由用户输入派生，因此单独给开关、不做 per-key 覆盖 |
| D3 | token / 成本**读时关联** `usage_records`，不落列 | 见 §2.1 |
| D4 | 模型两个身份都记 | 实测同一批流量里 `luna → gpt-5.6-luna`、`ds → deepseek-flash`、`demo → replay` 都存在；`usage_records` 用 `model`（请求名，`store/invoices.go` 按它分组做发票），`responses` 表存 canonical |
| D5 | 加 3 条索引：`(client, created_at, id)`、`(session_id, created_at, id)`、`(model, created_at, id)` | 见 §2.2；workspace 与 call_kind 不建索引 |
| D6 | 标题只写在标题调用那一行 | 其他行没有产出过标题，复制出去就是伪造来源；会话级由 `MAX(title)` 归并 |

### 2.1 为什么 token 不落列

1. `usage_records` 是计量唯一真值，钱相关事实已有不变量测试守着（`sum(ledger.charge) == sum(usage.charge_micros)`）；复制一份到日志表等于造一个不参与校验、会静默漂移的副本。
2. 生命周期不同：保留期删日志、**不删计费**。实测库里已有 60 条 usage 行没有对应日志行。
3. 被本地拒绝的请求按设计不写 usage 行（`recordDenied` 注释：请求没到上游就不能进计费）；关联天然表达「未计量」，落列则需要额外标志位。
4. 技术上可行：token 在 `recordContent` 时已可从 `assembler.Usage().Dimensions` 拿到，`Meter.Build` 只是原样 `json.Marshal`。收益只是省一次索引点查，代价是 owner 唯一性——不划算。

### 2.2 索引的实测取舍

探针：与 `request_logs` 同构的表，行形状取默认口径（2.5 KB 正文），每 256 行一个事务
（对齐 `store.LogWriter` 的批次），基线 6 万行，数据按真机局部性生成（同一会话连续 200 行、
客户端成段稳定）。指标用 **WAL 页字节/行**——它是确定性的，不受 fsync/checkpoint 抖动影响；
墙钟在 470 MB 的库上会被 checkpoint 淹没（实测 p95 到过 7–8 秒，同一档在不同行数上抖动 20 倍）。

| 方案 | 插入 | 相对 | 索引占用 |
|---|---|---|---|
| 现状（time + request） | 4449 B/行 | 1.00× | 30 B/行 |
| +client +session | 4820 B/行 | 1.08× | 118 B/行 |
| **+client +session +model（采用）** | 5008 B/行 | **1.13×** | 150 B/行 |

悲观上界（每行随机 session/client）是 1.43×（+1 条）与 1.54×（+4 条）。墙钟三档都在
13–52 µs/行，差异落在噪声里：多写的页先进 WAL 缓冲、同一个 commit 边界，代价转移到
checkpoint I/O 与磁盘。

查询计划（用真实列表列清单 EXPLAIN，不只是 `id`）：

| 语句 | 改动前 | 改动后 |
|---|---|---|
| 清理 `DELETE … created_at < ? ORDER BY id LIMIT 500` | `SCAN` 子查询 + 主键点查 | **完全一致** |
| 列表（无维度筛选） | `idx_request_logs_time` | 一致，无 temp B-tree |
| 列表（`client=?`） | 时间索引 + 残余过滤 | `idx_request_logs_client`，无 temp B-tree |
| 列表（`model=?`） | 时间索引 + 残余过滤 | `idx_request_logs_model`，无 temp B-tree |

三个索引都带 `id` 列，就是上表最后一列成立的原因：列表的 `ORDER BY` 是
`created_at DESC, id DESC`（`store/historyPageOrder`），把 `id` 放进索引才能让等值前缀
筛选继续走反向扫描；否则 SQLite 退回 temp B-tree，把整个窗口物化后再排序——正是 M24 修掉的
那个坑（`docs/design/m24-console-pagination.md` §8.10）。

不加 workspace 索引：基数高→索引大，而聚合查询还要取 title/workspace 必然回表、覆盖不了。
不加 call_kind 索引：取值几乎没有区分度（M48 起为 agent/title/compaction 三个）。

随数据量增长这件事：B-tree 插入是 O(log N)，按 4 KB 页与 28–70 B 的索引键估算，1 万–10 万行
都是 3 层、100 万–1000 万行 4 层、1 亿行才 5 层，每行代价近似常数。而 `request_logs` 的增长由
保留期兜住：稳态行数 = 保留期内的请求数。今天这个库 743 MB 里 689 MB 是 1192 行旧 `full`
正文（平均 578 KB/行），默认口径的行只有 2.5 KB——增长的大头从来是正文，不是索引。

## 3. 提取规则

结构性匹配，**不做裸字符串匹配**。这条规矩来自实测：库里 265 行含 `Codex CLI`，其中
**259 行其实是 DSH 请求**——那段文本出现在工具输出（agent 读到的文档/提交信息）里；
`<cwd>` 在库文本里是 `\u003ccwd\u003e`，裸 LIKE 也搜不到。

| 维度 | DSH | Codex |
|---|---|---|
| client | 首条 developer/system 消息以 `You are an AI agent powered by DeepSeek Harness.` 开头；或标题调用 | 顶层 `instructions` 以 `You are a coding agent running in the Codex CLI` 开头；或某条消息文本**以** `<environment_context>` 开头 |
| workspace | user 消息里的 `session workspace: "<path>"`（JSON 字符串，需反转义） | `<environment_context>` 的 `<cwd>…</cwd>`，退回 `<workspace_roots><root>…</root>` |
| session | `prompt_cache_key` 原样 | 同左 |
| call_kind | 任一消息文本以 `Generate the session title from this JSON array of human messages:` 开头 → `title` | user 消息以 Codex 专用任务标题提示词开头 → `title`；input 含 `compaction_trigger`（或旧形态 `context_compaction` 且无密文）→ `compaction`（M48）；否则 `agent` |
| title | 标题调用的响应文本 | 标题调用响应的 JSON `title` 字段，兼容纯文本 |

- 标题调用的角色在 DSH 版本间变过（旧 `system`、新 `developer`），因此只用文本前缀判定。
- 消息 `content` 有两种形态：字符串（DSH 的 developer 段）与 typed parts 数组（两家的 user 段），提取器都处理。
- `client` 的兜底来自 User-Agent（DSH 发 `deepseek-harness/<ver> (+url)`；Codex 的自定义 provider 可能什么都不发），**原始 UA 不落库**，只映射成枚举值。
- 上限：`workspace` 512 字节、`session_id` 128 字节、`title` 200 字符（按 rune 截断，避免切出非法 UTF-8）。

## 4. 接口

### 4.1 提取器（`internal/responses/dimensions.go`，新增）

```go
const (
    ClientDSH, ClientCodex, ClientUnknown = "dsh", "codex", "unknown"
    CallKindAgent, CallKindTitle          = "agent", "title"
    CallKindCompaction                    = "compaction" // M48
)

type Dimensions struct{ Client, Workspace, SessionID, CallKind string }

func (r *Request) Dimensions(clientHint string) Dimensions
func TitleOf(text string) string
```

放 `internal/responses` 的原因：分层断言（`internal/arch/layering_test.go`）已允许它只依赖
`domain/ids/pluginapi`，不新增包、不改 arch 表。`Dimensions` 不返回错误：识别不出就降级为
空值/`unknown`，一个识别不了的请求仍然是必须看得见的请求。

### 4.2 行记录与筛选（`internal/domain`）

`domain.RequestLogRecord` 增 `Client / Model / ResolvedModel / Workspace / SessionID / CallKind / Title`。
新增 `domain.RequestLogUsage` 与 `domain.RequestLogFilter`（放 domain 是因为 `internal/mcpsrv`
按 arch 规则不能 import store，而它同样要描述这个查询）；`domain.RequestLogDimensionRow` 是聚合行。

### 4.3 存储（`internal/store/request_logs.go`、`responses.go`）

- `RequestUsages(ctx, ids)`：一次批量点查（`WHERE request_id IN (…)` 走 `idx_usage_request`），
  按 request id 求和 attempt；`RequestUsage(ctx, id)` 是它的单条特例。
- `RequestLogDimensions(ctx, filter, groupBy, limit)`：白名单 `client|model|resolved_model|workspace|session|call_kind`，
  `LEFT JOIN usage_records` 汇总 token/成本，**只选维度列与 `created_at`**（选任何正文列都会把
  窗口内每行的溢出页读进来）。
- token 表达式与 `UsageBreakdown` 同源：输入 = `input + input_cache_hit + input_cache_miss`，
  输出 = `output`，思考 = `reasoning`。
- 身份列**不参与** `ON CONFLICT DO UPDATE`：骨架行重试不会抹掉第一次写入捕获的身份。

### 4.4 管理面

- `GET /admin/api/v1/requests`：新增 `client`/`model`/`resolved_model`/`workspace`/`session_id`/`call_kind`
  过滤；返回行增 7 个身份字段与恒存在的 `usage` 对象（未计量为 `{"metered": false}`）。
- `GET /admin/api/v1/requests/{id}`：同上。
- `GET /admin/api/v1/requests/dimensions?days=&group_by=&limit=`：top-N 维度统计，
  `limit` 默认 20、上限 200；`group_by=session` 时额外返回 `title` 与 `workspace`。
  未知 `group_by` 返回 400 并列出取值（不是 500）。
- 路由表新增条目，MCP 自动暴露为 `admin_request_dimensions`；字面量路径优先于 `{id}`，
  与既有 `/requests/prune` 同形（注释里写明 request id 恰为 `dimensions` 时详情不可达这个边界）。

## 5. 数据流

```
client ──POST /v1/responses──▶ 解析 ──▶ recordInput(ctx, key, req, clientHint)
                                            │  Dimensions(req, hint) → 7 个身份值
                                            │  过 recording.redact_paths（逐列）
                                            └▶ inputRecord{Dims, Model, Payload…}
persist ──▶ input.Resolved = plan.Resolved.Canonical ──▶ recordContent
                                            ├─ identity 7 列
                                            ├─ call_kind==title && record_title → TitleOf(assembler.Text())
                                            └─ LogWriter 批处理落库
read ──▶ ListRequestLogsPage(filter) ──▶ 第二查 RequestUsages(ids) ──▶ 合并出 usage
     ──▶ RequestLogDimensions(filter, groupBy) ──▶ 维度 × token/成本
```

## 6. 异常与边界

| 场景 | 行为 |
|---|---|
| 1 MiB 正文截断 | 身份在请求时提取、token 来自计量表，都不受影响 |
| 请求解析失败 | 不进入该路径；行照写，维度为空 |
| DSH `read-only`/`danger-full-access` | `workspace` 为空（`dsh-sandbox-policy` 只在这两档之外渲染路径），其余维度正常 |
| 会话压缩后 context 被替换 | `workspace` 可能缺失，`session_id` 仍可归组 |
| 本地拒绝的请求 | 有身份与 `model`，`resolved_model` 为空，`usage.metered=false`，UI 显示「未计量」而非 0 |
| 重试（多 attempt） | token/成本按 request_id 求和，延迟取最差一次 |
| 历史行（0008 之前） | 7 列空 → 控制台显示「—」，聚合归入「（未知）」桶，计数仍对得上 |
| `redact_paths` 命中某列 | 该列整体置空（路径是操作者最可能列进去的东西） |
| 标题调用失败/无输出 | `title` 为空，不写假值 |

## 7. 测试策略

- `internal/responses/dimensions_test.go`：真实抓包形状的表驱动（DSH agent / DSH 标题的两种角色变体 / Codex / 字符串式 `input` / `<workspace_roots>` 兜底），以及三个反例：工具输出里的 `Codex CLI` 与 `<cwd>` 不得参与识别、标题前缀必须位于消息开头、`read-only` 模式不得有 workspace。
- `internal/store/request_dimensions_test.go`：身份列读写往返（页面与详情投影一致）、冲突更新不抹身份、六个维度筛选对分页与计数一致、usage 跨 attempt 求和与未计量、维度聚合求和与排序、白名单拒绝非法 `group_by`、聚合 SQL 不含 `request_json`。
- `internal/httpapi/request_dimensions_test.go`：served/denied 两条路径的身份、别名（`model` vs `resolved_model`）、`record_input=off` 仍带身份、标题只在标题行且独立于输出录制、`record_title=false`、`redact_paths` 逐列生效、骨架行保留身份、列表/详情的 `usage`（含未计量）、维度端点分组与过滤、非法 `group_by` 返回 400、身份筛选与总数一致。
- `internal/store/pagination_test.go`：`TestHistoryListsAreOrderedByAnIndexNotASorter` 增加 client/model/session 三种筛选用例。
- `make ui-check`：requests 视图从 18 项断言扩到 38 项（新列、筛选控件、别名显示、tokens/未计量、统计卡与重新分组）。
- `make verify` 全绿。

## 8. 依赖

无新增包、无新增第三方依赖。改动落在 `internal/responses`、`internal/domain`、`internal/store`、
`internal/httpapi`、`internal/mcpsrv`、`internal/config`、`internal/webui` 与 `scripts/ui-harness`。

## 9. 实现与设计差异

计划里写"本期不加索引"，实现时改成加 3 条——这是被 §2.2 的实测推翻的：真实局部性下
三条索引只让页写放大 1.13×，而带维度筛选的列表查询从"窗口内残余过滤"变成索引驱动且
**仍然没有 temp B-tree**。计划留下的"加索引需重新论证"这一条，论证结果就是采纳。

计划里的 `store.RequestLogFilter` / `store.RequestLogDimensionRow` 最终放在 `internal/domain`：
`internal/mcpsrv` 按 arch 断言只能依赖 `domain/registry`，而它的查询工具同样要描述这个筛选。

计划中"`RequestUsages` 按 `pageRequests` 上限截断入参"未实现：入参来自当页行数，
`pageRequests` 的 cap 已经界定了它，再加一层截断只会掩盖调用方的问题。

其余按计划落地，无其他偏差。

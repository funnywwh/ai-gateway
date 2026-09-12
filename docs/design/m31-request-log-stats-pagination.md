# M31：请求日志页「维度统计」卡片置顶 + 排序 + 分组列表分页

> 起因（用户原话）：「将后台请求日志页面的维度统计卡片放上面，并且列表要加分页」。
> 对话中澄清并确认：**「列表」=「维度统计」表**（「请求日志」列表在 M24 已有服务端分页，本期不动）；
> 排序补充确认为**默认按「最近一次请求时间」降序**，并保留可切换的排序入口（服务端 `sort` 参数）。
> 相关：`docs/request-log.md`（请求日志规格）、`docs/design/m24-console-pagination.md`（分页契约与 pager 位置）、
> `docs/design/m27-request-dimensions.md`（维度聚合的来源，本期改的就是它的 `ORDER BY` 与 `LIMIT`）、
> `docs/design/m29-request-log-page-summary.md`（列表底部汇总行，本期不动）、
> `docs/design/m30-request-log-owner-dimensions.md`（凭据维度按 id 分组，本期排序白名单受它约束）。

## 1. 目标与成功标准

「维度统计」卡是请求日志页唯一没有分页的表：它写死 `limit=20`、没有总数、没有翻页控件，
分组数一超过 20（会话、工作区分组很容易超）第 21 个组就永远看不到，也判断不出「是到底了还是被截断了」。
同时它被压在请求日志列表下面——而列表本身已经有分页，页面上最需要判断「还有没有」的那张表反而没有。

本里程碑把统计卡置顶、给它分页，并把排序从写死的「请求数降序」变成**默认按最近一次请求时间降序、
可切换排序键**的服务端排序。

### 成功标准

| # | 标准 |
|---|---|
| 1 | `/admin/ui/#/requests` 第一张卡片是「维度统计」，请求日志列表在其下方（DOM 顺序） |
| 2 | 统计表有独立分页器：`共 N 个分组 · 本页 a–b · 第 x/y 页` + 20/50/100 条每页 + 上一页/下一页/跳转 |
| 3 | 翻页请求带 `limit/offset`，两页不重叠；`共 N 个分组` 是**分组数**，不是请求数 |
| 4 | 默认排序 = 最近一次请求时间降序；工具栏可切换「按最近一次请求 / 按请求数 / 按成本」，切换后回到第 1 页 |
| 5 | 统计表新增「最近一次」列；当前排序列的表头带 `↓` → 排序依据永远看得见 |
| 6 | `GET /admin/api/v1/requests/dimensions` 支持 `offset` 与 `sort`；响应增 `count/total/offset/has_more/sort`；非法 `offset`/未知 `sort` → 400 并列出取值；`limit` 默认 20 / 上限 200 不变 |
| 7 | 筛选或 `group_by` 变化 → 回到第 1 页；「刷新」保持当前页与排序键；「清理过期日志」回到第 1 页 |
| 8 | 分组计数查询不 join `usage_records`、不读正文页（EXPLAIN 钉住）；改动后时延 ≤ 1.5× 改动前，30 天窗口 < 1s |
| 9 | `make verify` 全绿；`make ui-check` 全绿，requests 视图断言 61 → ≥ 75 |

## 2. 关键决策

| # | 决策 | 理由与取舍 |
|---|---|---|
| 1 | **统计卡置顶**（放在「请求日志」卡之前） | 用户要求。它在页面上回答的是「谁在用、用哪个模型、花了多少」，是进页面先看的问题；列表回答的是「具体哪一条」。代价是筛选控件仍在下方列表卡里（见 #2） |
| 2 | 筛选栏**不**单独提成一张卡放到两张卡之上 | 备选方案：把 `days/account/key/client/model/session/workspace` 提出成独立「筛选」卡。否掉的理由：筛选在语义上属于它筛的那张列表（M24 起就在列表卡工具栏里，控制台里所有页面都是这个形制），提出来会让「改条件 → 看结果」的距离变远，还会改动两张卡的 DOM 结构并牵连 harness 的位置断言。折中是**在统计卡描述里写明**「筛选条件在下方「请求日志」卡片」 |
| 3 | **默认排序改为 `last_seen DESC`**（用户选定） | 「按时间排序」= 最近活跃的排最前，与请求日志列表的「最新在前」一致。这是**对既有端点的行为变更**（M27 起是写死的「请求数降序」），控制台与 MCP `admin_request_dimensions` 都能看见 → 所以必须回显 `sort`、在路由表声明枚举、在规格文档写明，而不是悄悄换掉 |
| 4 | 保留「按请求数」排序入口，且**服务端**排 | M27 的原始问题是「哪个维度最热」，那个入口不能丢；排序放服务端是因为客户端只能排当前页——会把「第 1 页里最大的」当成全局最大，是假结论（M24 §「不做列排序」记的正是这条） |
| 5 | 排序白名单就三个：`last_seen`（默认）/ `requests` / `charge` | 分别对应三个都有列的可见键：最近一次、请求数、成本。**不做「按分组名」**：凭据维度（account/api_key）按 id 转文本分组，字典序会把 `10` 排在 `2` 前面，是假的顺序；要做得先把 id 数值化，留作后续项。也不做 tokens 排序：它与请求数高度同向，收益低于多一个键的维护成本 |
| 6 | 每条排序都以 **`group_key ASC` 兜底** | `created_at` 是秒级整数，`last_seen` 大量并列；没有唯一兜底键，`LIMIT/OFFSET` 分页会重复或漏组（M24 的 `historyPageOrder` 用 `id` 兜底是同一个理由）。`group_key` 在一种分组内唯一 → 全序 |
| 7 | `ORDER BY` 写**完整聚合表达式**，不用 SELECT 的输出别名 | `usage_records` 里本来就有 `charge_micros` / `cost_micros` 同名列，输出别名参与名字解析会踩歧义（SQLite 的 ORDER BY 名字解析优先输出列）；用表达式则与 SELECT 同源、语义确定，代价只是同一段 SQL 出现两次 |
| 8 | 未知 `sort` → **400 并列出取值**（不静默忽略） | 静默忽略会让「换了排序但顺序没变」变成一个看不见的错，与 M24 修的 `/invoices?account_id=` 静默返回全部、M30 修的 `/dimensions?account_id=` 同一类。空值才取默认 |
| 9 | `total` = **精确分组数**，用 `COUNT(*)` 套一层分组子查询 | 分页器需要「共 N / 第 x/y 页」。计数查询**不 join `usage_records`**：`WHERE` 只涉及 `r.`，而 `LEFT JOIN` 不会增删左表的行 ⇒ 分组键集合与分页查询恒等（§8 用 EXPLAIN 证明它只走索引、不读正文页） |
| 10 | `total` **与排序键无关** | 分组数是集合性质，`CountRequestLogDimensionGroups` 的签名里不带 `sort`；测试钉住（换排序不改变 `total`） |
| 11 | 统计表加「最近一次」列，排序列表头加 `↓` | 排序依据必须在屏幕上：三种排序键各自都有对应列（最近一次 / 请求数 / 成本），所以任何一档排序都是「看得见依据的排序」，不需要猜 |
| 12 | 复用 `ui.js` 的 `pager()`，新增可选 `unit`（默认 `'条'`） | 分页控件只有一份实现（M24 的契约）；`unit` 让统计卡说「共 N 个分组」，与列表的「共 N 条」区分开——页面上同时有两个分页器时，这是它们各自口径的第一句话 |
| 13 | **删掉** `store.RequestLogDimensions(ctx,f,groupBy,limit)` | 它除 handler 外没有生产调用点（已 grep）。保留一个「默认排序」的旧包装会把排序口径藏在包装里；删掉后 store 测试每处都显式写排序键，读起来就是断言本身。这与 M24「旧 DAL 签名不动 + 一行包装」的做法不同，区别在于那边旧方法还有别的调用方（registry/mcpsrv），这边没有 |
| 14 | 不做 keyset/游标分页，不改索引 | 聚合本来就要扫整个窗口、再对**分组行**排序，`offset` 只是丢弃若干组行 → 深 offset 不额外变慢（也不会变快）。索引一层：M27/M30 建的 `(维度, created_at, id)` 已覆盖计数查询（§8） |
| 15 | `first_seen` 继续随响应返回，但界面只展示「最近一次」 | 数据已经在响应里（`admin.go` 一直在回 `first_seen`），少一列宽度，需要时再加 |

### 2.1 已知限制（写清楚，不藏着）

- **排序键换了 = 第 1 页的内容换了**：默认从「最忙的 20 个分组」变成「最近活跃的 20 个分组」。
  要原来的视图就切「按请求数」。这条写进 §「行为变更」，因为它对 MCP 调用方同样成立。
- **没有快照**：两次翻页之间有新请求落库，`last_seen` 会变、桶可能跨页边界移动（与 M24 历史列表
  同样的取舍）。要完全稳定得开事务或做快照，本期的取舍是不做。
- **`first_seen`/`last_seen` 是窗口内的极值**：`days=7` 时它们是最近 7 天的 MIN/MAX，
  窗口外的请求不参与；保留期清理会让某个桶的时间前移甚至整组消失（分页的「末页删空回退」兜底）。
- **`requests`/`metered` 是 attempt 计数**：聚合是 `LEFT JOIN usage_records` 后的 `COUNT(*)` /
  `COUNT(u.request_id)`，failover 多 attempt 的请求会被计两次。本机库当前 0 例，本期不改
  （改口径会改动现有页面上的数字），记在 `docs/TODO.md` 的观察项里。
- **不做按分组名排序**（见 #5）；**不做列宽/列多选**（M24 §6 已定）。

## 3. 接口

### 3.1 `internal/store/request_logs.go`

```go
// 排序白名单，默认值在首位
var RequestLogDimensionSorts = []string{"last_seen", "requests", "charge"}
const RequestLogDimensionDefaultSort = "last_seen"

// 形制同 requestLogGroupExpr：未知值报 "sort must be one of …"
func requestLogDimensionSortExpr(sort string) (string, error)

func (db *DB) ListRequestLogDimensionsPage(ctx context.Context, f domain.RequestLogFilter,
        groupBy, sort string, limit, offset int) ([]domain.RequestLogDimensionRow, error)
func (db *DB) CountRequestLogDimensionGroups(ctx context.Context, f domain.RequestLogFilter,
        groupBy string) (int, error)
```

`requestLogDimensionsSQL(expression, order, where)` 的尾子句变为 `GROUP BY group_key ORDER BY <order> LIMIT ? OFFSET ?`；
排序映射：

| `sort` | ORDER BY |
|---|---|
| `last_seen`（默认） | `MAX(r.created_at) DESC, group_key ASC` |
| `requests` | `COUNT(*) DESC, group_key ASC` |
| `charge` | `COALESCE(SUM(u.charge_micros), 0) DESC, group_key ASC` |

计数查询（新增 `requestLogDimensionCountSQL(expression, where)`）：

```sql
SELECT COUNT(*) FROM (
  SELECT <维度表达式> AS group_key FROM request_logs r <where> GROUP BY group_key
)
```

`limit` 走 `normalizeLimit(limit, 20, 200)`；`offset < 0` 归 0。投影与扫描列**一个字都没改**。

### 3.2 `internal/httpapi`

```go
// pagination.go
pageDimensions = pageSpec{Def: 20, Max: 200}   // 行是分组，不是记录
// pageSpec 增可选 Noun（空 = 现状「返回条数上限」），维度端点写「返回分组数上限，默认 20，最大 200」

// admin.go（接口，唯一实现是 *store.DB）
ListRequestLogDimensionsPage(ctx, f, groupBy, sort string, limit, offset int) ([]domain.RequestLogDimensionRow, error)
CountRequestLogDimensionGroups(ctx, f, groupBy string) (int, error)
```

响应（**保留** `rows/group_by/days/limit/dimensions`，新增 5 个字段）：

```json
{
  "group_by": "account", "sort": "last_seen", "days": 7,
  "limit": 20, "offset": 20, "count": 3, "total": 23, "has_more": true,
  "dimensions": ["client", "…"],
  "rows": [{ "key": "1", "requests": 12, "metered": 12, "first_seen": "…", "last_seen": "…",
             "input_tokens": 0, "output_tokens": 0, "reasoning_tokens": 0,
             "cost_micros": 0, "charge_micros": 0, "account_id": 1, "account_name": "acme" }]
}
```

- `rows` 不改名为 `data`：它自 M27 起就是这个端点的文档形状，控制台与 MCP `admin_request_dimensions` 都读它；
  信封里其余标识（`count/total/limit/offset/has_more`）与 M24 的列表信封逐字一致。
- 路由表：`limit` 从手写字段换成 `pageDimensions.fields()`（自动带 `offset`），新增
  `enumField(queryParam("sort", …), store.RequestLogDimensionSorts...)` → MCP `admin_describe` /
  `admin_endpoints` / 后台工具参数自动获得这两个参数，**MCP 侧零代码改动**。

### 3.3 `internal/webui/static/js`

```js
// ui.js
export function pager({ limit, offset, total, pageSizes, unit, onChange })   // unit 默认 '条'

// pages/requests.js
const statsWindow = { limit: 20, offset: 0, total: 0 };
async function loadStats({ reset = false } = {})   // reset 时 offset 归 0
```

- 顺序：`page.append(statsCard); page.append(card('请求日志', …))`。
- 统计卡工具栏：`[分组下拉] [排序下拉]`，排序选项 `按最近一次请求 | 按请求数 | 按成本`。
- 表头：`分组 [标题 工作区] 最近一次 请求数 已计量 输入 tokens 输出 tokens 成本`，当前排序列加 `↓`。
- 每次成功加载都 `statsHost.replaceChildren(table, pagerNode)`（整块重建 → 不会残留重复监听器；
  分页器查询限定在 `statsHost` 内，避免与列表那个 `.pager` 混淆）；失败时整块换成错误行（分页器一并消失，
  不留一个指向空窗口的控件）。

## 4. 数据流

```
                     ┌─ 筛选（列表卡工具栏，仍驱动两张表）
                     ▼
GET /admin/api/v1/requests/dimensions?days&group_by&sort&limit&offset&<筛选>
        │
        ├─▶ ListRequestLogDimensionsPage(...)   → 分组聚合（LIMIT/OFFSET 在 SQL 里）
        └─▶ CountRequestLogDimensionGroups(...) → 分组总数（同一 WHERE，不 join usage_records）
                     │
                     ▼
        {rows, count, total, limit, offset, has_more, sort, group_by, days}
                     │
                     ▼
   requests.js  statsWindow ← limit/offset/total（服务端回显，不本地推算）
                     │
                     ├─▶ 表体：分组 │ [标题 工作区] │ 最近一次↓ │ 请求数 │ 已计量 │ 输入 │ 输出 │ 成本
                     └─▶ pager(unit:'个分组') → 共 N 个分组 · 本页 a–b · 第 x/y 页
```

「请求日志」列表一路完全不变：仍是 `pagedTable` + `/requests` + 底部本页汇总行。

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| 窗口内 0 个分组 | 表显示「该窗口内没有可统计的请求」，**不渲染分页器**——空态本身已经说明没有可翻的页（与 M29 不渲染空汇总行同一取舍；列表分页器保留是因为页大小与跳转对它有别的用处） |
| `offset` 落在末页之后（筛选/清理把分组删空） | 自动回退一页重取一次（与 `pagedTable` 同规则），最多退到 0 |
| 服务端无 `total`（新旧不匹配） | `total = offset + rows.length`，按「只有本页」渲染、下一页禁用，不猜 |
| 非法 `offset` / 未知 `sort` | 400，报错文本指出参数名与取值（不静默当 0、不静默忽略） |
| 排序键并列（秒级时间戳） | `group_key ASC` 兜底 → 全序，分页不重不漏 |
| 两次请求间新请求落库 | 桶可能跨页移动（无快照，见 §2.1） |
| 窗口外请求 / 保留期清理 | `first_seen`/`last_seen` 只反映窗口内；整组消失时由回退规则兜底 |
| 深 `offset` | 聚合本就要扫窗口并对分组行排序，`offset` 只丢弃组行 → 不额外变慢，不做游标分页 |
| 计数查询成本 | 门槛见 §8；若某天不达标，退回「只有 has_more」的 `pager`（`total == null` → 显示「还有更多」） |
| 切换排序/分组/筛选 | 一律 `offset = 0`：旧 offset 属于另一套排序与筛选，位置无意义（M24 同规则） |
| 「刷新」 | 保持当前页与排序键；若窗口已缩到该页之外，由回退规则纠正 |

## 6. 测试策略

仓库没有 node/npm（`docs/TODO.md` 记着），控制台没有 JS 单测，界面靠 `scripts/ui-harness`
（headless firefox + 快照夹具 + `/report` 断言）。

| 关注点 | 在哪里验证 |
|---|---|
| 分页窗口、`total`、`offset` 越界、`limit` 夹取、未知桶 | `internal/store/request_dimensions_test.go`（新增 `TestRequestLogDimensionsPaging`） |
| 三种排序的首桶、并列时的 `group_key` 兜底、未知 `sort` 报错、`total` 不随 sort 变 | 同上（新增 `TestRequestLogDimensionSorts`） |
| 计数 SQL 不含正文列、不含 `usage_records` | 同上（扩展 `TestRequestLogDimensionQueryAvoidsBodies`） |
| 响应信封、`sort` 回显、400 的两条路径、过滤后 `total` 一致 | `internal/httpapi/request_dimensions_test.go`（扩展 + 新增两例） |
| 控制台排序下拉取值 == 服务端白名单 | `internal/webui/embed_test.go`（与既有「维度选项不漂移」同形） |
| 卡片顺序、统计分页器、排序切换、时间列、离线交互 | `scripts/ui-harness/keys.page.html`（requests 视图，新增约 14 项断言） |

UI harness 的夹具要求：stub 在「按 `group_by` 取快照」之上**再按 `limit/offset` 切片**，
并给 `workspace` 分组合成 45 个桶（3 页）——真实快照的分组数不够翻页。
合成夹具的硬要求是**三种排序的首桶互不相同**，否则排序断言会假绿。

## 7. 依赖

无新增依赖、无新增包、无数据库迁移。改动落在 `internal/store/request_logs.go`、
`internal/httpapi/{admin.go,admin_routes.go,pagination.go}`、`internal/webui/static/js/{ui.js,pages/requests.js}`、
测试与 `scripts/ui-harness/{keys.page.html,README.md,fixtures.json}`、文档。

## 8. 实测（EXPLAIN QUERY PLAN）

只读副本 `data/aigw-local.db`（3712 行日志 / 3858 行计量、正文很重），30 天窗口。

**新增的分组计数查询**（8 个维度全部走索引，无正文页、无 `usage_records`）：

| 分组 | 计划 |
|---|---|
| `client` | `SCAN r USING COVERING INDEX idx_request_logs_client` |
| `model` | `SCAN r USING COVERING INDEX idx_request_logs_model` |
| `session` | `SCAN r USING COVERING INDEX idx_request_logs_session` |
| `resolved_model` / `workspace` / `call_kind` / `account` / `api_key` | `SEARCH r USING INDEX idx_request_logs_time (created_at>?)` + `USE TEMP B-TREE FOR GROUP BY` |

带筛选时更省：`client=?` 走 `idx_request_logs_client (client=? AND created_at>?)`、
`account_id=?` 走 `COVERING INDEX idx_request_logs_account`（M27/M30 建的索引直接用上了）。
60k 行探针库上的形状略有不同（数据量变了，计划器换了索引，但**同样只走索引**）：
`client` 变成 `SEARCH r USING COVERING INDEX idx_request_logs_client (ANY(client) AND created_at>?)`，
`api_key` / `workspace` 仍是 `idx_request_logs_time` + 分组 temp B-tree。
要保证的从来不是「命中哪个索引」，而是「不读正文页、不 join 计量表」——这两条在两种形状下都成立。

**三种排序键的计划形状完全一致**（client 分组、30 天窗口）：

```
SCAN r USING INDEX idx_request_logs_client
SEARCH u USING INDEX idx_usage_attempt (request_id=?) LEFT-JOIN
USE TEMP B-TREE FOR ORDER BY      ← 今天按 COUNT(*) DESC 排时就已经存在
```

即：换排序键不新增扫描，`LIMIT/OFFSET` 之前的工作量与今天相同；临时 B-tree 排的是**分组行**，
不是窗口里的每一行（M24 §8.10 修掉的正是后者）。

**时延实测**（每次 15–25 次取 p50/p95，同一台机器、只读连接）：

| 数据 | 查询 | p50 | p95 |
|---|---|---|---|
| 真库副本（3.8k 行、正文很重），7 天 | 聚合（旧排序 `COUNT(*) DESC`） | 6.51 ms | 6.86 ms |
| 同上 | 聚合（新排序 `last_seen DESC`） | 6.54 ms | 6.65 ms |
| 同上 | 分组计数（本方案新增） | 0.18 ms | 0.18 ms |
| 60k 行 / 2.5 KB 正文探针库，30 天 | 聚合（旧排序） | 80.3 ms | 81.2 ms |
| 同上 | 聚合（新排序） | 80.4 ms | 81.1 ms |
| 同上 | 分组计数（本方案新增） | 2.4 ms | 2.5 ms |

结论：**换排序键是零成本**（1.00×，远低于 1.5× 的门槛），新增的分组计数约是聚合耗时的 **3%**
（60k 行上 2.4 ms vs 80.4 ms），所以整个端点的成本 ≈ 1.03×，30 天窗口远在 1 s 以内。
探针库由 `data/aigw-local.db` 导出 schema 后灌入 60k 行日志（正文 2.5 KB）+ 12k 行计量，
脚本式构建、用后即删（`.cache/m31/probe.db`，`ANALYZE` 过）。

**隔离实例走查**（`:8099` + 全新库 + 新二进制，一个沙箱调用内完成）：
默认排序首桶 = 最近一次的桶、`sort=requests`/`sort=charge` 首桶各不相同、两页不重叠且
`total` 是**分组数**（3 个桶 / 6 条请求）、`offset=abc|-1` 与 `sort=latency` 全部 400 且报错文本列出取值。

## 9. 实现与设计差异

按计划落地，无方向性偏差。实现时才定下来/被实测纠正的几点：

- **`pageSpec` 加的是 `Noun` 字段**（空 = 旧的「返回条数上限」），维度端点写「返回分组数上限」。
  比一开始想的「加一个 `fieldsOf(noun)` 方法」少一处重复的方法，其余 12 个端点的描述逐字不变。
- **既有测试 `TestAdminRequestDimensionsGroupByOwner` 补了 `&sort=requests`**：它断言的是桶的身份与
  计数（`rows[0]` 是 acme、是 key-a），而默认排序换成时间后，所有种子行共享同一个时间戳，
  位置就落到了 `group_key` 兜底上——那属于排序测试的职责，不属于这个用例。改法是让它显式说自己
  按什么排，而不是把断言降级成按 key 找桶。
- **并列兜底是「按 SQL 文本钉住」而不是「按行为钉住」**：`TestRequestLogDimensionsPaging` 用 201 个
  同秒同计数的桶翻页，本以为它会抓住「去掉 `group_key ASC`」这个变异，实测它**通过**了——SQLite 在
  并列时的实际顺序恰好也是扫描顺序。所以真正的钉子写在了 `TestRequestLogDimensionSorts` 里
  （每个排序键的 ORDER BY 必须以 `group_key ASC` 结尾），并用变异验证过：去掉尾键该断言立刻失败。
  这一条值得记下来：**行为断言对「未定义顺序」是无效的**，只有契约文本能钉住它。
- **`store.RequestLogDimensions` 直接删除**（设计时定的），store 测试改为显式传排序键；顺带把
  `TestRequestLogDimensionsGroupAndSum` 里「bucket order is by request count」的注释改成「计数才是
  本用例关心的，读进来的顺序无关」，免得注释与代码说的不是一件事。
- **多加了一个 MCP 用例** `TestMCPDescribesTheDimensionBreakdownWindow`：设计里声称「MCP 侧零代码改动，
  `offset`/`sort` 自动出现在 `admin_describe`」，那就得有断言钉住它（列出的参数集合、`limit` 描述里的
  「分组数」、`sort` 的 enum 与 store 白名单一致），否则哪天路由表被精简掉一个参数没人会发现。
- **UI harness 的断言数 61 → 76**（计划 ≥ 75）：除计划内的 14 项外，补了一项
  `listPagerUnchanged`（列表分页器仍然说「共 2 条」）——页面上现在同时有两个分页器，
  「统计卡改造没有顺手改坏列表那个」值得留一条回归。
- **计数查询的计划在不同数据量下会选不同的索引，但都只走索引**：真库副本（3.8k 行）上
  `client`/`model`/`session` 走 `SCAN USING COVERING INDEX idx_request_logs_<维度>`，其余 5 个维度走
  `idx_request_logs_time` + 分组 temp B-tree；60k 行探针库上 `client` 变成
  `SEARCH ... USING COVERING INDEX idx_request_logs_client (ANY(client) AND created_at>?)`。
  两种形状都不读正文页、不 join `usage_records`（D9 要的正是这两条），所以 §8 的结论不变，
  但「一定命中维度索引」这句话是错的，文档里改成了「只走覆盖索引或时间索引」。

验收记录：

- `make verify` 全绿；`make ui-check` 10 个视图全绿：
  `docs` 23 / `detail` 20 / `models` 17 / `create` 6 / `plugin` 17 / `plugin-cached` 16 / `currency` 16 /
  `keys` 17 / `requests` **76** / `paging` 30。统计卡的可读样本：
  `共 45 个分组 · 本页 1–20 · 第 1/3 页` + 首行 `/w/023 9/11/2026, 7:38:00 PM 23 23 2,300 23 0.045000 USD`。
- 隔离实例走查 `ALL CHECKS PASSED`（见 §8 末）。
- 运行中的 8088 需要 `make build` + `scripts/local-run.sh restart` 才看得到（资源是 `//go:embed` 进
  二进制的；JS 带 `public, max-age=300`，还要硬刷新），这一条留给宿主终端。

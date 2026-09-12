# M24 设计：管理后台所有列表支持分页

> 前置：`docs/design/m8-admin-api.md`（管理面端点与分页承诺）、`docs/design/m9-web-console.md`（零构建控制台）。
> 需求原话：「使用列表要支持分页显示」→ 澄清为「管理后台的所有列表」。
> 本文档在编码前输出；实现完成后回填第 8 节差异。

## 1. 目标与约束

目标：管理控制台的**每一张列表**都能分页浏览——总数、当前窗口、上一页/下一页、每页条数、跳页；
后端列表接口统一接受 `limit` + `offset` 并返回窗口元数据，使数据量增长后不再受固定 `limit=500`
与「一次把全表读进内存」的限制。

约束（环境与既有设计决定，不是偏好）：
- 管理面是会话 Cookie + 同源 API；前端零构建、原生 ES 模块、无 CDN（M9），所以分页控件必须是
  自写 DOM 组件，不引入任何库；
- `internal/arch/layering_test.go` 的分层规则不变，本轮不新增包；
- 现有调用方（MCP 查询工具、注册表快照、bootstrap、计费读侧）不能被牵连改动：它们读的是
  `store.ListX(ctx, …, limit)` 的旧签名，属于另一条产品线；
- 后端响应只允许**增字段**：`data` / `count` 已被控制台、MCP 桥与测试使用。

## 2. 关键决策（含取舍）

### 2.1 统一契约，两种落地

所有列表端点都接受 `limit`（默认值/上限见 §3.2）与 `offset`（默认 0），返回同一信封（§3.1）。

| 数据性质 | 端点 | 落地方式 | 取舍 |
|---|---|---|---|
| 历史/无界（请求日志、审计、账本、额度、账单、兑换码、对账） | 7 个 | SQL `LIMIT ? OFFSET ?` + `COUNT(*)` | 必须窗口化：这些表会一直长 |
| 配置表/有界（账户、Key、标签、模型、路由、映射、Hooks、MCP 令牌、门户用户、供应商、上游模型、备份） | 14 个 | 保持整表读取，handler 内切片 | 省掉 12 个 DAL 方法与 registry/bootstrap 的连锁改动；代价是这些端点仍全表读一次（量级＝配置条数，注册表每次 reload 本来也这么读） |

备选方案 A：把 `ListAccounts`/`ListModels`/`ListRoutes`… 全部加上 `limit/offset`。
否决理由：这些方法被 `registry`（每次热更新读全表）、`bootstrap`、`billing.Service` 共用，
为一个 UI 需求改造它们的签名，收益是「少读几行配置」，代价是把数据面路径一起卷进来。

备选方案 B：只做前端分页（把所有列表一次拉全再切片）。
否决理由：请求日志/账本/账单本来就不该整表拉；`limit=500` 的截断会让「共 N 条」是假的。

### 2.2 不改旧 DAL 签名：新增 Page 变体 + Count，旧方法一行包装

```go
// 旧签名保持不动（mcpsrv / domain.Store / registry 继续用）
func (db *DB) ListRequestLogs(ctx, accountID int64, from, to time.Time, limit int) ([]*domain.RequestLogRecord, error) {
    return db.ListRequestLogsPage(ctx, accountID, from, to, limit, 0)
}
// 新增
func (db *DB) ListRequestLogsPage(ctx, accountID int64, from, to time.Time, limit, offset int) ([]*domain.RequestLogRecord, error)
func (db *DB) CountRequestLogs(ctx, accountID int64, from, to time.Time) (int, error)
```

取舍：多 7 个 Page 方法 + 7 个 Count 方法，换来 `domain.Store`、`billing.Service` 的
`store` 端口、`mcpsrv.Store`、`internal/backup`、`registry`、`bootstrap` **零改动**。
反过来（直接给旧方法加 `offset`）要改 4 个包里的接口声明与 10 处调用点，且每一处都要判断
"这里传 0 对吗"——爆炸半径更大、评审更累。

### 2.3 信封只增字段

```json
{ "data": [ … ], "count": 20, "total": 137, "limit": 20, "offset": 20, "has_more": true }
```
- `count` = 本页条数（不变，现有断言继续成立）；
- `total` = **过滤之后**的全部行数（不是本页、不是本窗口）；
- `has_more = offset + count < total`；
- 端点自有字段（`/backups` 的 `dir`/`total_bytes`/`next_run`、`/providers/{id}/logs` 的 `running`）保留。

### 2.4 额度明细的过滤下沉到 SQL

`GET /accounts/{id}/credits` 现在的实现是「取 limit 条账本 → 丢掉 `kind == "charge"`」，
分页之后会导致每页条数不均、`total` 无意义。改为窗口查询带 `ExcludeKinds`，由 SQL 完成过滤：

```go
// LedgerWindow 描述账本的一个窗口。ExcludeKinds 非空时在 SQL 里剔除这些 kind
// （额度明细页排除 charge），保证页大小与 total 描述的是同一批行。
type LedgerWindow struct {
    AccountID    int64
    From, To     time.Time
    ExcludeKinds []string
    Limit, Offset int
}
func (db *DB) ListLedgerPage(ctx context.Context, w LedgerWindow) ([]*domain.LedgerEntry, error)
func (db *DB) CountLedger(ctx context.Context, w LedgerWindow) (int, error)
```

账本流水页传 `ExcludeKinds: nil`（全部 kind），额度明细页传 `["charge"]`。
用「排除」而不是「白名单」，是为了让以后新增的 kind 默认出现在额度明细里，而不是被静默隐藏。

### 2.5 分页控件渲染在 `<table>` 之外

`scripts/ui-harness` 的断言与若干页面的路由都用 `tbody tr` 取值（`rowFor`、`rows()`）。
分页控件因此是一个 `<div class="pager">` 兄弟节点，**不允许**新增 `<tr>`/`<tfoot>` 行。

### 2.6 配置类端点仍要服务「选择器」

账户/供应商/模型下拉框用的是同一批列表端点。它们的调用显式带 `limit: 1000`（＝上限），
并在 §6 记录已知限制：选择器最多 1000 项；主表格分页不受影响。

### 2.6b 顺手修掉的「文档里有、代码里没有」的过滤器

`GET /invoices` 的路由表从 M12 起就写着 `account_id` 查询参数，而 handler 只读路径参数
`r.PathValue("id")`，也就是说 `/invoices?account_id=7` 会**静默返回所有账户的账单**。
控制台的账单页正好需要它（原来是在浏览器里 `.filter`），所以本轮让它真正生效：
路径与查询二者取一（路径优先），非整数/负数 → 400，`accountID <= 0` 仍表示全部账户。
这是 M23「后台能存什么必须等于后台真正读什么」同一类问题，不修就会在分页后变成可见回归。

### 2.7 `offset` 非法即 400，`limit` 沿用宽容语义

`limit` 继续用 `adminLimit`（缺省/非法/≤0 → 默认值，超上限 → 夹住），保持既有行为；
`offset` 是新参数，静默当成 0 会让「请求第 5 页却拿到第 1 页」成为最糟的失败模式，
因此非整数或负数一律 `400 invalid_request`。

## 3. 接口

### 3.1 分页原语：`internal/httpapi/pagination.go`（新）

```go
type pageParams struct{ Limit, Offset int }

// adminPage 解析 limit/offset。limit 沿用 adminLimit 的宽容语义；
// offset 必须是 >= 0 的整数，否则返回 domain.ErrInvalidRequest（400）。
func adminPage(r *http.Request, def, max int) (pageParams, error)

// sliceWindow 对已加载切片取窗口，并报告是否还有下一页（配置类端点用）。
func sliceWindow[T any](rows []T, p pageParams) ([]T, bool)

// writeList 是唯一的列表信封出口：data/count/total/limit/offset/has_more。
func writeList(w http.ResponseWriter, data []map[string]any, total int, p pageParams)
```

`adminLimit` 保留给非列表调用（`/providers/{id}/logs` 的行数上限）。

### 3.2 默认值与上限

| 端点 | 默认 | 上限 | 方式 |
|---|---|---|---|
| `GET /requests` | 50 | 500 | SQL |
| `GET /audit-logs` | 100 | 500 | SQL |
| `GET /accounts/{id}/ledger` | 100 | 1000 | SQL |
| `GET /accounts/{id}/credits` | 100 | 1000 | SQL（`ExcludeKinds:["charge"]`） |
| `GET /invoices`、`GET /accounts/{id}/invoices` | 50 | 200 | SQL |
| `GET /redemption-codes` | 100 | 500 | SQL |
| `GET /billing/reconciliations` | 30 | 200 | SQL |
| `GET /keys`、`GET /accounts`、`GET /tags`、`GET /models`、`GET /model-mappings`、`GET /routes`、`GET /hooks`、`GET /mcp-tokens`、`GET /portal-users`、`GET /accounts/{id}/portal-users`、`GET /providers`、`GET /provider-models`、`GET /providers/{id}/models`、`GET /backups` | 200 | 1000 | 内存窗口 |

（前 7 行的默认值/上限沿用 M8 已实现的值，只是新增 `offset`。）

### 3.3 前端组件：`internal/webui/static/js/ui.js`

```js
// pager 只负责展示与回调：范围/总条数、每页大小、上一页/下一页、跳页。
export function pager({ limit, offset, total, pageSizes, onChange })

// pagedTable 把 table() 与 pager() 组合起来，并持有 {limit, offset, total}。
// load({ limit, offset }) 返回一个带 data / total 的负载。
// 返回 { node, refresh(), reset(), state() }。
export function pagedTable({ columns, load, pageSize = 20, pageSizes = [20, 50, 100],
                             rowActions, empty, filter = true, onError })
```

行为约定：
- `offset === 0` → 「上一页」禁用；`has_more === false` → 「下一页」禁用；
- 改每页大小或过滤器 → `reset()`（offset 归 0 后重载）；
- `refresh()` 保持当前窗口；**当前页为空且 `offset > 0` 时自动回退一页**（删除末页最后一条）；
- 加载失败 → `onError(err)`（页面弹 toast），分页控件仍可继续操作；
- `table()` 里一直空着的 `#count` span 填成「本页 N 行」，过滤框 placeholder 改「本页过滤…」
  （客户端过滤只作用于当前页，必须在界面上说清楚）。

## 4. 数据流

```
页面 load({limit, offset})
  → api.get('/requests', { days, account_id, limit, offset })
  → handler: adminPage(r, 50, 500) → ErrInvalidRequest 时为 400
  → store.ListRequestLogsPage(..., limit, offset) + CountRequestLogs(...)
  → writeList(w, out, total, p)
  → pagedTable 渲染行 + pager（共 N 条 / 本页 X–Y / 第 P/Q 页）
```

## 5. 异常与边界

- **删除末页最后一条** → `pagedTable.refresh()` 回退一页；`total` 由服务端重算；
- **越界 offset**（含 `total = 0`）→ 200 + 空 `data` + `has_more: false`，前端显示空态并允许「上一页」；
- **`limit` 超上限** → 服务端夹住；前端页大小选项不超过上限，避免「请求 500 拿回 200」；
- **翻页期间新增行**：列表倒序且主要是追加写入，新行插入会让相邻页重复/跳过一条 —— 已知限制，
  界面上提供「刷新」，文档记录；
- **`account_id = 0`** 语义不变（管理面「全部账户」视图），窗口在过滤器之后套用；
- **`days` / `batch_id` / `account_id` 变化** → 前端 offset 归零；
- **MCP**：`admin_request` 走同一批 handler，非法 `offset` 得到同样的 400；
  路由表补 `offset` 说明后，`admin_describe` / `admin_endpoints` 自动带上。

## 6. 明确不做（非目标）

- MCP 账户查询工具（`get_ledger` / `list_requests` / `list_invoices`）不加 `offset`：它们是账户自助查询、
  受 `max_query_rows` 约束，属另一条产品线；
- 服务端搜索/排序参数：本轮不做（现有客户端过滤保留，界面上标注只作用于当前页）；
- 游标（cursor）分页：列表按 id/时间倒序且基本只追加，offset 足够；
- 门户（portal）页面尚未实现，不在本轮；
- 不分页并写明理由的界面：概览页（数据来自 `/stats` 快照）、设置页（键值 + 汇率表）、
  定价页（`/pricing/targets` 是选择器网格 + 搜索，不是表格）、供应商日志弹框
  （环形缓冲尾部，`limit` 语义更贴切）、`/provider-kinds`（内建类型固定集）。

## 7. 测试策略

1. `internal/httpapi/pagination_test.go`：配置类端点窗口/`total`/`has_more`/越界；历史类端点第二页内容与
   `total`；额度明细的 `total` 只数非 charge 行；`offset` 非法 400；`limit` 超上限夹住；`count` 兼容。
2. `internal/store/dal_test.go`：`ListAuditPage`/`CountAudit`、`ListRequestLogsPage`/`CountRequestLogs`、
   `ListLedgerPage`/`CountLedger`（含 `ExcludeKinds`）的窗口与计数。
3. `scripts/ui-harness`：新增 `paging.page.html` + `paging` 视图，断言首屏请求 URL 带
   `limit=20&offset=0`、范围与总条数文案、首页「上一页」禁用、点「下一页」发出 `offset=20`、
   改每页大小/过滤器后 offset 归零、空页自动回退一页。页面模板自己记录**带 query 的原始 URL**
   （现有 stub 会剥掉 query）。
4. 真机走查：隔离实例 + 全新库，用 replay 供应商造 ≥3 页请求日志与账本流水，curl 校验分页字段，
   浏览器逐页翻请求日志/审计/账本/账单/兑换码。
5. `make verify`（vet + test + build）与 `make ui-check` 全绿。

## 8. 实现与设计差异

1. **顺手修掉两处「文档里有、代码里没有」的过滤器**（都是走查时撞上的，不修就会在分页后变成可见回归）：
   - `GET /invoices` 的路由表从 M12 起就写着 `account_id` 查询参数，handler 却只读路径参数
     `r.PathValue("id")` —— 也就是说 `/invoices?account_id=7` **静默返回所有账户**。控制台账单页要在
     服务端按账户过滤（分页后不能再在浏览器 `.filter`），所以让它真正生效：路径/查询二者取一（路径优先），
     非整数/负数 → 400（`accountID <= 0` 仍表示全部账户）。
   - `POST /accounts/{id}/invoices` 的 `period` 文档写「自然月账期，例如 2026-08」，实现只认
     `current|previous|last30`，文档里的写法一律 400。现在 `YYYY-MM` 真正可用（`time.Parse("2006-01")`
     → 用该月走 `billing.PeriodFor`），报错文案也列出全部可接受形式。
   两处都有测试：`internal/httpapi/pagination_test.go`（账户过滤）与
   `internal/httpapi/admin_billing_contract_test.go`（自然月 + 非法值 400）。
2. **页面大小的默认值与上限集中成一个 `pageSpec`**：设计里只说"默认值/上限按端点不同"。
   实现把每个族的 `{Def, Max}` 放在 `internal/httpapi/pagination.go`，handler 用 `spec.params(r)` 解析、
   路由表用 `spec.fields()` 生成文档 —— 于是 `admin_describe` 里写的数字与 handler 强制的数字**不可能漂移**。
3. **`writeList` 之外多了一个 `listPayload`**：`/backups` 要在信封上带 `dir`/`total_bytes`/`next_run`，
   因此提供"返回 map 版本"的同一个渲染函数，避免该端点自己拼信封造成字段不一致。
   `/backups` 归到**内存窗口**（保留策略把行数封顶），这样 `total_bytes` 仍描述整个备份目录；
   控制台的「份数」随之改用 `total`（`count` 现在是本页条数）。
4. **配置类端点仍整表读取**（设计 §2.1 的取舍），只有 `/backups` 由 SQL 窗口改为内存窗口（同上）。
5. **控制台细节**：分页默认 20 条/页，可选 20/50/100；`table()` 新增 `filterPlaceholder`，
   分页表格的过滤框标成「本页过滤…」，并把一直空着的 `#count` span 填成「本页 N 行」；
   `pager` 渲染在 `<table>` 之外的兄弟 `<div>`，`tbody tr` 选择器与既有样式不受影响。
6. **`ui.js` 的多余能力没有引入**：不做列排序/列宽/多选；窗口状态只由 `pagedTable` 持有，
   页面拿到的句柄只有 `refresh()` / `reset()` / `state()`。
7. **走查工具**：`scripts/ui-harness` 新增 `paging.page.html`（`#paging` 视图）：它是唯一**按窗口**回答
   `/accounts` 的 harness 页（解析 URL 里的 `limit`/`offset` 再切片），断言读的是**带查询串的原始 URL**
   —— 旧的两个 harness 页会把 query 丢掉，分页在那里无从验证。`run.sh` 的视图列表与 README 同步更新。
8. **MCP 侧**：`admin_list_*` 通过路由表拿到 `offset`（`admin_describe`/`admin_endpoints` 自动可见）；
   账户自助查询工具（`get_ledger`/`list_requests`/`list_invoices`）按设计**没有**加 `offset`。
9. **DAL 取舍落地**：旧方法（`ListAudit`/`ListRequestLogs`/`ListLedger`/`ListInvoices`/
   `ListRedemptionCodes`/`ListReconciliations`）都改成一行包装，`registry`/`bootstrap`/`mcpsrv`/
   `billing` 的调用点**一行未改**；`LedgerWindow.ExcludeKinds` 按设计在 SQL 里排除 charge。
10. **（追加修复）翻页排序改成 `ORDER BY created_at DESC, id DESC`**：M24 把这批列表从「整表读」
    改成「SQL 窗口读」，但 ORDER BY 沿用了 `id DESC`。带 `created_at` 上下界的窗口查询因此被 SQLite
    规划成「created_at 索引扫描 + `USE TEMP B-TREE FOR ORDER BY`」——**排序器会把窗口内每一行都物化**，
    之后才套 `LIMIT`。request_logs 的行带着录制的请求正文（真库实测：1853 行、`request_json` 合计
    692 MB、均值 376 KB），于是控制台取 50 行要先把 ~700 MB 塞进排序器：

    | 查询 | 计划 | 实测 |
    |---|---|---|
    | `... AND created_at >= ? AND created_at <= ? ORDER BY id DESC` | 索引范围 + TEMP B-TREE | **1.21–1.23 s** |
    | 同上，仅去掉上界（计划退化为倒序全表扫描） | SCAN（无排序器） | 0.00 s |
    | 同上，仅去掉正文列（排序器只剩元数据） | 索引范围 + TEMP B-TREE | 0.11 s |
    | `... ORDER BY created_at DESC, id DESC` | 索引范围（无排序器） | **0.00 s** |

    现有 `idx_request_logs_time(created_at)`（以及账本的 `idx_ledger_account_time`、用量的
    `idx_usage_account_time`）就能提供 `created_at DESC, rowid DESC` 这个顺序，所以**不需要新增索引或迁移**。
    同一模式在账本列表与 `ttftSamples` 里也存在（当时各约 1500 行、行很窄所以还不慢），一并改掉。
    排序语义不变：`created_at` 与 `id` 在写入时一起打戳（`recordRequestLog`），两者顺序一致，
    而控制台展示的正是 `created_at`；`id` 保留为同秒内的稳定 tiebreaker。
    回归防线是 `internal/store/pagination_test.go` 的 `TestHistoryListsAreOrderedByAnIndexNotASorter`：
    直接 `EXPLAIN QUERY PLAN` 三个列表的**真实语句**（由 `requestLogListSQL`/`ledgerListSQL`/`ttftSamplesSQL`
    构造），断言计划里不出现 `TEMP B-TREE`。断言计划而不是断言耗时，因为计划选择是确定性的，
    而 M13 明确不把绝对数字放进 CI。该用例在旧排序下三条全部失败（已验证），改回即通过。

### 验证与实测结论（2026-09-11）

- `internal/httpapi/pagination_test.go`（7 个用例）：配置类窗口/越界/夹上限/`offset` 非法 400、
  历史类第二页内容与 `total`、额度明细只数非 charge、账单账户过滤（路径与查询两种写法）、
  `/backups` 的 `total_bytes` 覆盖全部作业；`internal/store/pagination_test.go`（4 个用例）覆盖
  Page/Count/`ExcludeKinds` 与旧方法的"第一页"语义。
- `scripts/ui-harness` 九个视图全绿（`docs 23 / detail 20 / create 6 / plugin 17 / plugin-cached 16 /
  currency 16 / keys 17 / requests 10 / paging 30` 项断言）；`paging` 视图证明了首屏
  `limit=20&offset=0`、范围与总条数文案、首页禁用上一页、下一页发 `offset=20`、
  改页大小回到第 1 页、**删空末页自动回退一页**、跳页越界夹到最后一页。
- 隔离实例真机走查（`:8098` + 全新库 + 新二进制，`ALL CHECKS PASSED`）：55 条请求日志 3 页且互不重叠、
  45 张兑换码单批次分页、账本 67 行（含 55 条真实 charge）而额度明细 `total=12` 且不含 charge、
  3 张账单按账户过滤、审计流水跨页、13 个账户的窗口切片、`offset=abc`/`-1` → 400、
  `limit=99999` → 夹到 500；并确认二进制里内嵌的 `ui.js` 带 `pagedTable`、`accounts.js` 会发 `offset`。

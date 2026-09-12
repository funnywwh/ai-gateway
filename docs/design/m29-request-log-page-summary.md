# M29：请求日志列表底部的本页汇总行（tokens 入/出 + 成本）

> 起因（用户原话）：「管理后台的请求日志列表在列表底部添加一列汇总行：tokens（入/出），成本，汇总这两列」。
> 口径已当场澄清并确认：**只汇总当前页已加载的行**，不做筛选窗口级合计（见 §2.1）。
> 相关：`docs/request-log.md`（请求日志规格）、`docs/design/m27-request-dimensions.md`（身份维度与消耗度量，
> 本里程碑汇总的正是它加的那两列）、`docs/design/m24-console-pagination.md`（列表分页与 pager 的位置约定）。

## 1. 目标与成功标准

「请求日志」列表每行已经有 tokens（入/出）与成本两列（M27），但一页 20 行（可切 50/100）
的数字只能一行行看。本里程碑在**表格底部**加一行汇总，把这两列按当前页求和。

### 成功标准

| # | 标准 |
|---|---|
| 1 | 列表底部出现一行汇总；tokens 与成本落在它们各自表头列的正下方，单元格数与表头一致（不错行） |
| 2 | 翻页 / 改每页条数 / 点「刷新」→ 汇总跟着当前页的行变化 |
| 3 | 卡片上的「本页过滤…」框输入时，汇总跟着屏幕上剩下的行变化 |
| 4 | 本页没有任何行 → 不显示汇总行（空态本身已说明「该窗口内没有请求日志」） |
| 5 | 本页全部未计量 → 两格显示「未计量」而不是 0（沿用行内既有规则：未计量 ≠ 消耗 0） |
| 6 | 后端零改动：`/admin/api/v1/requests` 响应、store、迁移、MCP 全部不动 |
| 7 | `make verify` 全绿；`make ui-check` 全部视图全绿，requests 视图断言数从 38 增加 |

## 2. 关键决策

| # | 决策 | 理由与取舍 |
|---|---|---|
| 1 | **汇总口径 = 当前页**（用户在两个选项里选定） | 数字与它上方的行可以肉眼核对；代价是它**不是**「共 N 条」那一批的合计，翻页即变。窗口级口径由「维度统计」卡承担。备选（窗口级）被否掉不是因为它更难，而是因为它会让「底部数字 ≠ 上面这页」成为默认观感 |
| 2 | 汇总行放 `<tfoot>`，**按列 key 落位**（`footer(rows) -> {列key: 节点}`），未命名的列渲染空 `<td>` | 与「数组 + 在调用点算 colspan」相比，列数一旦增减，colspan 算术必然过期，而 keyed 落位不可能错行。落位逻辑只写一次，放在 `table()` 里 |
| 3 | 标签放第一列（时间） | 汇总行的标签放在首列是通行读法；12 列里给标签一个 `colspan` 反而要引入算术（见 #2） |
| 4 | 未计量的行**计入行数、不贡献数字**，并写在标签里 | 与行内单元格同一句话（`tokensCell`/`costCell` 对未计量显示「未计量」）。底部数字与肉眼相加不一致时，标签上的「已计量 M · 未计量 K」就是解释 |
| 5 | 全部已计量时不写「已计量 N」 | 与「维度统计」卡同一写法（`metered === requests` 时不重复报数），避免每个数字都说两遍 |
| 6 | 成本列汇总**对客 `charge_micros`** | 与它上方列同源（`costCell` 用的就是 `charge_micros`）；`cost_micros`（上游成本）在这张列表里从不出现 |
| 7 | 用 `<td>` 不用 `<th>` | `th` 带 muted 颜色和 `cursor:pointer`（可排序表头的样式），会让汇总行看起来可点 |
| 8 | 后端不加 `summary` 字段 | 本页口径的数据前端已全部拿到（每行的 `usage`），加接口只会多一次窗口聚合与一处要同步的契约 |

### 2.1 已知限制（写清楚，不藏着）

- 本页合计**不是**筛选窗口的合计：`共 137 条` 的窗口里，底部汇总只描述当前这 20 行。
  使用者要窗口级数字时看「维度统计」卡——它按 6 个身份维度分组汇总，分组数 ≤ 20 时加起来即窗口合计。
- 已经量过的「窗口级」实现路径（本次不做，留作后续）：在 `/admin/api/v1/requests` 响应上增
  `summary`（一次窗口聚合）。在真库副本（`data/aigw-local.db`，3061 行日志 / 3193 行计量、7 天窗口）上实测：
  计划为 `SEARCH r USING INDEX idx_request_logs_time` + `SEARCH u USING INDEX idx_usage_attempt (request_id=?) LEFT-JOIN`
  （与维度聚合同形），无 temp B-tree 时为 11 ms；把「未计量条数」写成 `SUM(CASE WHEN u.request_id IS NULL …)`
  而不是 `COUNT(DISTINCT u.request_id)` 就完全不需要 temp B-tree（后者 +5 ms）。
  那条路一旦要走，footer 机制不用改，只换数据源。

## 3. 接口

### 3.1 `internal/webui/static/js/ui.js`

```js
// table(): 新增可选 footer
export function table({ columns, rows, filter, onFilter, empty, rowActions, filterPlaceholder, footer })
//   footer(visibleRows) -> { [columnKey]: Node } | null
//     返回 null 或可见行数为 0 → 不渲染 footer 行
// pagedTable(): 透传
export function pagedTable({ columns, load, pageSize, pageSizes, rowActions, empty, filter, onError, footer })
```

- `table()` 在 `render()` 里按 `columns` 顺序生成 footer 行：命名的列放节点，其余列空 `<td>`，
  `rowActions` 存在时行尾补一个空 `<td>`；`<tfoot>` 只在传了 `footer` 时才创建。
- 不传 `footer` 的页面（keys / models / accounts / …）DOM 与行为完全不变。

### 3.2 `internal/webui/static/js/pages/requests.js`

```js
// 返回值直接交给 table() 的 footer
function summaryCells(rows) -> { created_at: Node, usage: Node, charge: Node } | null
```

- 单次遍历求 `metered` 计数与 `input_tokens` / `output_tokens` / `charge_micros` 之和。
- 金额先 `Math.round(Number(...))` 再累加：`money()` 内部走 `BigInt()`，小数会抛 `RangeError`。
- 标签：`本页汇总 · 共 2 行`；有未计量行时补 `（已计量 1 · 未计量 1）`。
  带 `title`：只合计当前页已加载的行（本页过滤生效时就是屏幕上剩下的行），不是整个筛选窗口。

## 4. 数据流

```
GET /admin/api/v1/requests?limit=&offset=&<筛选>   （不变）
        │
        ▼
pagedTable 持有本页 rows ──▶ table.render()
        │                        │  可见行（含「本页过滤」筛选后的结果）
        │                        ▼
        │                 footer(rows) = summaryCells(rows)
        │                        │
        │                        ▼
        └───────────────▶ <tfoot><tr> 标签 │ … │ tokens │ 成本 │ … </tr></tfoot>
```

窗口级数据仍走既有的两条：pager 的 `共 N 条` 来自响应信封的 `total`，
「维度统计」卡来自 `/requests/dimensions`。

## 5. 异常与边界

| 场景 | 行为 |
|---|---|
| 空窗口 / 空页 | 不渲染汇总行（`rows.length === 0` 时 `footer` 返回 `null`） |
| 本页全部未计量 | 两格「未计量」（muted），标签写「已计量 0 · 未计量 N」，不报 0 |
| 部分未计量 | 数字只来自已计量行，条数写在标签里 |
| `usage` 缺失 / `metered` 非真（历史行、本地拒绝的请求） | 按未计量处理，不抛异常（沿用 `tokensCell`/`costCell` 的判定） |
| 「本页过滤…」框筛掉部分行 | 合计跟随可见行（标签已写明口径），不会静默地描述看不见的行 |
| 最后一页不足 `limit` 行 | 合计只描述这一页 |
| 金额量级 | `charge_micros` 是整数微元，一页最多 100 行；JS 数值在 2^53 内精确，`money()` 的 BigInt 换算不受影响 |
| 列增减 | keyed 落位：新列自动获得空单元格；被删列的 key 自动消失 |

## 6. 测试策略

仓库没有 node/npm（`docs/TODO.md` 记着这条限制），控制台没有 JS 单测，验证靠
`scripts/ui-harness`（headless firefox + 快照夹具 + `tbody tr` 级断言）。

- `scripts/ui-harness/keys.page.html`（requests 视图）新增断言：汇总行存在且只有一行、
  标签文案、tokens 数值、成本金额、**单元格数 === `thead th` 数**（错行会直接失败）、
  刷新后随新数据变化；并把 `<tfoot>` 文本作为 `checks.sample` 回报，让 `make ui-check` 的输出
  里留下人类可读的证据（harness README 已声明本环境截图不可信，验证只认 `/report`）。
- 夹具不用改：现有两行（一行计量 1200/34/charge 2468、一行未计量）已覆盖「部分未计量」。
- 逆向断言（已有，继续成立）：pager 仍在表格之外，「共 N 条」不因 footer 而改变。
- `make verify` 全绿：本次不动 Go 代码，用于确认嵌入资源与既有测试未受牵连。

## 7. 依赖

无新增依赖、无新增包、无后端改动。改动落在 `internal/webui/static/`（`js/ui.js`、
`js/pages/requests.js`、`app.css`）、`scripts/ui-harness/keys.page.html` 与文档。

## 8. 实现与设计差异

按计划落地，无偏差。两处实现时才定下来的细节：

- 断言里读表头必须限定在**汇总行自己那张表**（`foot.closest('table')`）：页面上还有「维度统计」卡的表，
  `document.querySelectorAll('thead th')` 会把两张表的表头一起数进来（首次运行就是这样失败的，
  失败信息记在 `docs/TODO.md` 的 M29 段里）。这一条本身也说明「单元格数 === 表头数」这个断言是有效的：
  它抓到了一个真实的错位读法。
- 请求日志页新增 9 项布尔断言 + 1 项 `sample` 文本，requests 视图从 38 项变为 **48** 项
  （`summaryRow` / `summaryLabel` / `summaryTokens` / `summaryCost` / `summaryAligned` /
  `summaryFollowsFilter` / `summaryHiddenWhenEmpty` / `summaryRestored` / `summaryFollowsRefresh`）。
  §6 只列了前 5 项与「刷新后跟随」，实现时补了过滤框的三项：既然汇总的口径是「屏幕上这几行」，
  那么过滤框就是这条口径唯一的观察窗口，不测它等于没测。

验收记录：

- `make verify` 全绿（无 Go 改动）。
- `make ui-check` 10 个视图全绿：`docs` 23 / `detail` 20 / `models` 17 / `create` 6 / `plugin` 17 /
  `plugin-cached` 16 / `currency` 16 / `keys` 17 / `requests` **48** / `paging` 30，
  请求日志页的 tfoot 文本样本：
  `本页汇总 · 共 3 行（已计量 2 · 未计量 1）2,400 / 680.004936 USD`。
- 运行中的 8088 需要 `make build` + `scripts/local-run.sh restart` 才看得到（资源是 `//go:embed` 进
  二进制的；JS 带 `public, max-age=300`，还要硬刷新），这一条留给宿主终端。

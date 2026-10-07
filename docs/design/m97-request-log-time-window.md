# M97 设计文档：请求日志的时间窗口（当天 / 本周 / 本月 / 自定义时间段，按本地时间）

> 状态：**已实现（M97）**（代码、测试、文档已完成，走查断言已在本地浏览器里跑过；
> 官方入口 `make ui-check` 与重启后的线上验收待宿主执行，见 `docs/TODO.md` 的 M97 小节）。
> 规格同步：`docs/request-log.md`（新增「时间窗口」一节）、`docs/TODO.md`、`docs/PROCESS.md`。
> 前序：[M24 控制台分页](m24-console-pagination.md)、[M27 身份维度](m27-request-dimensions.md)、
> [M31 维度统计分页与排序](m31-request-log-stats-pagination.md)。
>
> 需求原话（截图批注，2026-10-07）：`#/requests` 页上三行红字 + 一条箭头指向列表卡工具栏的时间窗口下拉——
> **「添加:当天、本周、本月、时间段」/「时间段点击后，弹出选择开始结束日期」/「按本地时间计算」**。

## 1. 问题

页面上的窗口下拉只有 `最近 1 天 / 3 天 / 7 天 / 30 天`（`internal/webui/static/js/pages/requests.js`），
四个选项都是**滚动窗口**：服务端在 `internal/httpapi/admin.go` 的 `adminWindow` 里算
`now.AddDate(0,0,-days) ~ now`（UTC 时刻差）。

运营要问的问题里有一大半不是滚动窗口：

- 「**今天**出了多少错」——早上打开页面问的是本地 00:00 起的量，不是"最近 24 小时"（后者会把昨天的量算进来）；
- 「**这周**花了多少」——本周一 00:00 起，不是最近 168 小时；
- 「**本月**的用量」——本月 1 日 00:00 起；
- 「**10 月 1 日到 10 月 3 日那次故障期间**」——任意区间。

这四件事用 `days=N` 一个数根本表达不出来，而现有的时间列是按**浏览器本地时间**渲染的
（`ui.js` 的 `formatTime` 走 `toLocaleString()`），于是当前的滚动窗口还有一个更难查的毛病：
跨零点的会话在页面上显示「10-06 23:40」，却仍在"最近 1 天"里，运营按屏幕上的日期对不上。

## 2. 目标

1. 下拉里**追加**四个选项：`当天` / `本周（周一起）` / `本月` / `时间段…`；四个滚动窗口原样保留。
2. 选「时间段…」→ 弹窗选开始日期与结束日期，「确定」后按该区间查询。
3. 四个日历窗口（当天/本周/本月/时间段）的边界按**本地时间**（浏览器的时区）计算，
   与列表「时间」列的渲染口径一致。
4. 列表与维度统计两张表共用同一个窗口（既有行为，不新开第二个窗口控件）。
5. 生效窗口**看得见**：选中后显示「窗口：2026-10-01 00:00 ~ 10-07 23:59（本地时间 · Asia/Shanghai）」，
   并且服务端在响应里回显它实际使用的 `window`（时刻，UTC，RFC3339）。

### 非目标（本次）

- 不改账本/发票页（`adminWindow` 的另一批调用方：`/accounts/{id}/ledger`、`/credits`）——它们继续只有 `days`。
- 不加服务器端 `timezone` 配置项（见 D9）。
- 不做小时/分钟粒度的自定义窗口（日期粒度的区间已经是运营问的那件事）。
- 不做窗口的 URL 回显/share、不做 localStorage 记忆（见 D8）。
- 不改「清理过期日志」的保留期口径（那是 `recording.retention_days`，与本窗口无关）。

## 3. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | 「本地时间」= **操作员浏览器的时区**，不是服务器时区 | 列表/详情的每个时间戳都是 `toLocaleString()`（浏览器本地）。边界若按服务器时区算，屏幕上就会出现「10-06 23:40 这一行属于『当天』」——正是本次要消除的困惑。**否决**服务器时区：同一台网关国内/海外运营同时看，服务器只有一个时区，屏幕上却是一堆人的本地时间 |
| D2 | 边界由**前端**算好，作为显式 `from`/`to`（RFC3339 **时刻**）发给服务端 | 服务端只做 `created_at >= ? AND <= ?`——`domain.RequestLogFilter` 的 `From/To` 通道早已存在（M24/M27），store 与小时汇总层**零改动**。时区知识留在唯一知道它的地方（浏览器），服务端不新增配置、不猜语义。**否决**给服务端传 `window=today&tz=Asia/Shanghai`：那要在服务端实现一份日历算法、还要校验 IANA 名字，而它算出来的 `now` 仍是服务器时钟 |
| D3 | 当天/本周/本月**只发 `from`**，`to` 留给服务端「现在」 | 语义就是「本地 00:00 到此刻」。若前端也发 `to=now`，浏览器时钟偏差（慢 5 分钟）会让窗口右端停在过去，屏幕上最新的行被无声地排除。「现在」由服务端定义，与既有 `days` 窗口的右端完全一致 |
| D4 | 时间段**收发两端**：`from` = 起始日**本地 00:00:00**，`to` = 结束日**本地 23:59:59** | 含尾整天。`created_at` 是**整秒**（`store.unix`），`<= 23:59:59` 不会漏掉该日最后一秒的行；`23:59:59.999` 与 `次日 00:00` 之间没有可落入的行。**否决**半开区间 `to = 次日 00:00`（要求 `created_at < to`，而 `requestLogFilter` 是 `<=`；改比较符会同时改动 8 个读路径的语义） |
| D5 | 本周 = **周一起**（ISO-8601 / 中文习惯） | 「本周」在中文语境里是周一到周日。周日起算会让周日上午的运营看到"本周"只剩下当天 |
| D6 | 控件位置**不动**：仍在列表卡工具栏，统计卡共用 | M31 决策 #2 已定「筛选条件在下方『请求日志』卡片里改」，箭头也指在那里。另起一张筛选卡会改两张卡片的 DOM 结构并牵连 121 项走查里按位置读的断言 |
| D7 | 四个滚动窗口**原样保留**，新选项追加在后 | 与「添加」的原话一致；且两者语义不同（"最近 24 小时排除了今天 00:00 之前的量吗"不是同一个问题）。默认仍是 `最近 7 天`（既有行为不变） |
| D8 | 不做 URL/hash 回显、不做 localStorage 记忆 | 没要求。窗口是「这次看什么」而不是配置；记在本地反而会出现「别人发来的链接打开是另一个窗口」 |
| D9 | **不给请求日志引入服务器端 `timezone` 配置** | 请求日志是"屏幕上这些行"，时区必须是屏幕的时区。仓库已有的 `billing.timezone` 服务于**账期/发票**的边界（钱的口径，必须全局唯一），两者的正确取值可以不同，不该合并成一个 |
| D10 | 显式窗口与 `days` **同时给时以显式窗口为准**，并在响应里回显生效窗口 | 与既有 `adminDays` 的口径一致（"回显实际使用的窗口，而不是被问的窗口"）。回显让"`days` 被忽略"成为**看得见**的事实，而不是静默丢弃 |
| D11 | 显式窗口有**跨度上限 366 天**与 `from > to` 拒绝（400） | 无上限的窗口等于允许一次全表扫描（`idx_request_logs_time` 走一遍的历史）。边界是防呆，取 366 与 `days` 的 365 上限同量级（多一天是给闰年留的） |
| D12 | 时间段弹窗**用 `type=date` 原生控件**，不引第三方日期选择器 | 控制台零构建、零依赖（`internal/webui` 的设计），`app.css` 里已有表单样式；原生 date 控件自带本地日历与键盘可用性。`chat_form.js` 已在用 `type=date`，不是新形态 |
| D13 | 取消时间段弹窗 = **回退到上一个选项**，窗口不变 | 点开弹窗再取消，若下拉停在一个没生效的「时间段…」上，屏幕上就会出现"控件说的窗口"与"表里的数据"不一致 |

## 4. 接口

### 4.1 后台 API（`GET /admin/api/v1/requests`、`GET /admin/api/v1/requests/dimensions`）

两个端点共用 `requestLogFilterFromQuery`（`internal/httpapi/admin.go`），新增两个可选查询参数：

| 参数 | 类型 | 语义 |
|---|---|---|
| `from` | string | 窗口起点，**RFC3339 时刻**（例：`2026-10-05T16:00:00.000Z`）；非法 → 400 `param=from` |
| `to` | string | 窗口终点，RFC3339；省略 = 服务端「现在」 |

组合规则（进路由表描述与规格文档）：

| 输入 | 生效窗口 |
|---|---|
| 都不给 | `now − days ~ now`（`days` 默认 7、1..365，既有行为不变） |
| 只给 `from` | `from ~ now`（当天/本周/本月） |
| 只给 `to` | `now − days ~ to` |
| 两个都给 | `from ~ to`，`days` 被忽略（回显可见） |
| `from > to` | 400 `param=from`（"from must not be after to"） |
| `to − from > 366 天` | 400 `param=from`（防全表扫） |

响应新增**生效窗口回显**（纯增量字段）：

```json
{"window": {"from": "2026-09-30T16:00:00Z", "to": "2026-10-07T06:12:33Z"}}
```

- 列表端点：`listPayload(...)` 之后加 `payload["window"]`（`/backups` 已是这个形状）。
- dimensions 端点：保留既有 `days` 参数回显，另加 `window`。
- 两个时刻都是 **UTC RFC3339**（与 `created_at` 等其他时间字段同一种写法），前端按本地时间渲染。

`days`/`from`/`to` 的解析集中在新函数 `adminRequestWindow(r)`；`adminWindow(r)` **保持原样**
（账本/发票的调用方不受影响），两者的共享部分（`days` 的范围校验）抽成一个小函数。

### 4.2 控制台（`internal/webui/static/js/pages/requests.js`）

```js
// 纯函数，可被 node 直接测：把「窗口选项 + 本机时区 + 自定义区间」翻译成查询参数。
// kind: 'd1' | 'd3' | 'd7' | 'd30' | 'today' | 'week' | 'month' | 'custom'
// 返回 {days} 或 {from} 或 {from, to}（ISO 字符串，带本地时区偏移）
windowParamsOf(kind, now = new Date(), range = null) -> object

// 由它派生的两个显示用值：
windowLabelOf(kind, range)  // '当天' / '时间段：10-01 ~ 10-07'
localZoneName()             // 'Asia/Shanghai'（Intl 解析，拿不到时返回 ''）
```

- 下拉值从 `1/3/7/30` 改为 `d1/d3/d7/d30/today/week/month/custom`（**值变了**，但没有任何
  持久化引用它；`loadModelOptions` 里那处直读 `days.value` 的老写法改走 `filterParams()`，
  否则模型下拉会带着 `days=today` 去问服务端——那会被当作默认 7 天）。
- `filterParams()` 成为窗口的唯一出口：列表 `load`、`loadStats`、`loadModelOptions` 三处都调它。
- 选 `custom` → `modal({fields:[{type:'date'...},{type:'date'...}]})`；提交校验
  `from <= to`（在弹窗内报错、不丢输入），返回 `{from, to}` 两个 `YYYY-MM-DD` 字符串。
- 应用后：下拉选项文案变 `时间段：10-01 ~ 10-07`；工具栏出现 muted 提示
  `窗口：2026-10-01 00:00 ~ 10-07 23:59（本地时间 · Asia/Shanghai）`；出现「改时间段」按钮重开弹窗。
- 任何窗口变化 = `view.reset()` + `loadStats({reset:true})` + `loadModelOptions()`（与既有筛选一致）。
- 自定义区间是**页面状态**（`let windowRange = null`），不是 DOM 状态：`<option>` 的文案会被重写，
  读回它会依赖于渲染顺序。

## 5. 数据流

```
浏览器（本地时区）
  windowParamsOf('week', new Date())
      ├─ from = 本周一 00:00:00 本地 → new Date(...).toISOString()   // 带 Z 的绝对时刻
      └─ （不带 to）
        ↓
  GET /admin/api/v1/requests?from=2026-10-04T16:00:00.000Z&limit=20&offset=0
        ↓
  requestLogFilterFromQuery → adminRequestWindow
      f.From = 2026-10-04T16:00:00Z
      f.To   = time.Now().UTC()          ← 服务端定义「现在」
        ↓
  store.requestLogFilter → "AND r.created_at >= ? AND r.created_at <= ?"   （既有 SQL，未改）
        ↓
  响应 {data:[...], window:{from,to}}
        ↓
  控制台按本地时间渲染时间列（formatTime，既有）+ 工具栏提示「窗口：…（本地时间 · Asia/Shanghai）」
```

## 6. 异常与边界

| 情况 | 行为 |
|---|---|
| `from`/`to` 不是 RFC3339 | 400，`error.param` = `from` 或 `to`，消息 `from must be RFC3339` |
| `from > to` | 400，`param=from`，消息 `from must not be after to` |
| 跨度 > 366 天 | 400，`param=from`，消息 `from and to must span at most 366 days` |
| `from`/`to` 为**空串**（`?from=`） | 等同未给（`api.js` 的 `qs()` 本来就不发空值，但手写 URL 会） |
| 窗口内没有任何行 | 既有表现：列表 `该窗口内没有请求日志`、统计卡 `该窗口内没有可统计的请求` |
| 跨夏令时 | 前端的 `new Date(y, m, d)` 交给平台日历（本地时区规则），边界落在**当地**的 00:00；春令时那天不存在 02:00，与本功能无关（只用 00:00 与 23:59:59） |
| 浏览器时区名拿不到（老 Safari / 隐私模式） | `localZoneName()` 返回 `''`，提示退化为 `窗口：… ~ …（本地时间）`——**不**编造时区名 |
| 结束日期选在今天 | `to` = 今天 23:59:59，落在未来；服务端 `created_at <= to` 不会因此多算任何行（未来没有行），提示里如实显示 23:59 |
| 用户在弹窗里只改开始、结束留空 | `type=date` 的 `required` 生效，`modal()` 的 `collect()` 会 toast「必填」并保持弹窗打开 |
| 窗口区间与保留期冲突 | 提示里照常显示保留期（既有 `hint`），窗口超出保留期的部分自然是空的（既有行为） |

## 7. 测试策略

| 层 | 内容 |
|---|---|
| Go 单测（新 `internal/httpapi/request_window_test.go`） | 只给 `from` → 只筛出该时刻之后的行（且包含恰好等于的行）；`from`+`to` 秒级闭区间；非法值/`from>to`/超 366 天 → 400 且 `param` 正确；两端点都回显 `window` 且与种子时刻一致；显式窗口下 `days` 被忽略（`days=365` 也筛不掉窗口外的旧行） |
| Go 守卫（`internal/httpapi/mcp_admin_test.go`） | 既有 `TestMCPDescribesTheDimensionBreakdownWindow` 的名单加 `from`/`to`，并断言两者有描述（参数在 handler 里被接受却没进路由表 → 对 agent 不可见，是该测试存在的理由） |
| Node（`internal/webui/tests/requests_test.mjs`，随 `make ui-base` 跑） | 用 `vm` 提取 `windowParamsOf` 并钉住：`today` = 本地 00:00 且**只带 from**；`week` = 周一 00:00（周日/周一各试一次）；`month` = 1 日 00:00；`custom` = 起始 00:00 + 结束 23:59:59；`d7` = `{days:'7'}`；源码里四个新选项存在 |
| Makefile | `ui-base` 里该行加 `TZ=Asia/Shanghai`（见"环境说明"），否则在 UTC 主机上"本地时间"与 UTC 无法区分 |
| 浏览器走查（`scripts/ui-harness/keys.page.html` 的 `requests` 视图） | `windowOptions`、`windowTodaySendsFrom`（原始 URL 带 `from=` 且**不带** `days=`）、`windowBothTables`（列表与统计两张表都带）、`rangeDialogOpens`（两个 `type=date`）、`rangeDefaultsToday`、`customRangeSendsBounds`（from=起始日 00:00、to=结束日 23:59:59 且都带）、`customCancelRestores`（取消后回到上一个选项、URL 不再带 from）、`windowHintLocalTime`、`rangeButtonHiddenByDefault`、`noPageErrors` |
| 线上（宿主执行） | 本沙箱没有可用的 firefox（`/usr/bin/firefox` 是 snap 壳子，`run.sh` 会跳过），走查与"重启后看见新选项"记进 `docs/TODO.md` 的「待宿主执行」 |

## 8. 依赖

- 既有 `domain.RequestLogFilter.From/To` 与 `store.requestLogFilter` 的 `created_at >= ? / <= ?`（M24 起）。
- 既有 `adminWindow`/`adminDays` 的 `days` 解析与回显口径。
- 既有 `modal()`（`ui.js`）与 `type=date` 控件（`chat_form.js` 已有先例）。
- 既有 `filterParams()` 作为两张表的唯一查询参数出口（M30/M53 建立）。

## 9. 环境说明（本次实现）

- 本沙箱**没有可用的 firefox**（`scripts/ui-harness/run.sh` 会自检并跳过），所以走查的官方入口
  （`make ui-check`）仍要宿主执行；node 侧（`make ui-base`）与 Go 侧（`make test`）在这里全跑。
- **但走查的断言本身在本沙箱跑过**：本机有 chromium，用一份仓库外的临时脚本复刻了 run.sh 的三步
  （`render_page.py` 生成 keys.html → `server.py` 起服务 → 无头浏览器加载 `#requests`），
  146 项检查全绿、`errors` 为空，其中包含本文新增的 16 项；`#keys` 视图的 17 项同样全绿
  （确认这次改动没有碰坏同一页的另一半）。这一步只是把"新断言是否真的成立"从宿主提前到本地，
  不替代 `make ui-check`。
- node 测试中"本地时间"必须是**非 UTC** 才有意义，所以 Makefile 的该行显式设 `TZ`。

## 10. 实现与设计差异

实现与设计逐条一致，只有四处细节在写代码时定得更细：

1. **提示语的格式多了一级秒**：设计里写 `窗口：2026-10-01 00:00 ~ 10-07 23:59`，实现是
   `窗口：2026-10-01 00:00:00 ~ 2026-10-07 23:59:59`（两侧都带秒与完整日期）。理由是结束日那一秒
   （23:59:59）正是「含整天」这句话的落点，省略它会让「到某日为止到底含到哪一刻」重新变得不可见。
2. **时区限定只标在真的按本地时间算的窗口上**：滚动窗口（最近 N 天）是绝对的 N×24 小时，与任何
   时区无关，给它标一个时区名是把噪声当信息。实现里抽了 `windowUsesLocalTime(kind)` 一处判断
   （四个日历窗口为真），node 测试分别钉住两组取值。
3. **模型下拉读的是窗口，不是整套筛选**：设计里说「`filterParams()` 成为窗口的唯一出口，三处都调它」。
   实现把窗口抽成了 `windowParams()`，`filterParams()` 在它之上再加身份维度；模型下拉只取窗口那一半。
   原因是模型下拉的选项来自这个查询本身——把自己的 `model` 过滤也带上，选完一个模型之后下拉里就
   只剩那一个，没法再切回来。三处调用的**窗口**仍然完全一致（同一个函数算出来的）。
4. **「改时间段」不走 `applyWindow('custom')`**：那条路会再弹一次窗（下拉此时已在 custom 上）。
   实现把它拆成 `applyRange(range)`；取消弹窗时另外重画一次工具栏，否则下拉回退了而提示还停在上一个
   窗口的文案上。

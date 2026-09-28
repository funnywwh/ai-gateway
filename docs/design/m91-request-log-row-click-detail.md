# M91 设计文档：请求日志列表整行可点开详情

> 状态：已实现（本文件在实现前已在对话中输出并通过评审）。
> 规格同步：`docs/request-log.md`（控制台展示口径与状态）、`docs/TODO.md`、`docs/PROCESS.md`。
> 需求原话：「请求日志的列表 点击 显示详情」。

## 目标

请求日志列表的每行末尾**已经有一个「详情」按钮**，点击打开详情弹窗（输入/思考/输出分栏 + 身份块 +
路由路线块 + 消耗块，`pages/requests.js` 的 `detail()`）。缺的不是详情本身，而是**点击面**：

- 这张表有 19 个数据列 + 1 个操作列，操作列在最右边；内容面板（`.main`）要横向滚动才够得着它。
- 操作者读的是左边那几列（时间、请求 ID、状态、模型、成本），鼠标停在那一带时没有任何入口。

本次把**整行**变成点击目标，打开**同一个**详情弹窗；「详情」按钮保留。

验收标准：

1. 点数据行的任意非交互区域 → 打开该行详情，标题 `请求 <request_id>`，内容与点「详情」逐字一致。
2. 点「详情」按钮只开**一个**弹窗（按钮的点击会冒泡到行）。
3. 拖选文本（复制请求 id）之后的那次点击**不**打开弹窗。
4. 行内控件（按钮/链接/输入）自己吃掉点击。
5. 其它列表页零行为变化；维度统计卡的行不是手型。
6. 既有 `#requests` 走查 121 项与全部视图仍全绿；压缩镜像同样全绿。

## 关键决策

1. **能力加在 `ui.js` 的 `table()`/`pagedTable()` 上，且是可选的 `onRowClick`，默认关。**
   行是 `table()` 内部造的，页面拿不到 `<tr>`；在页面里事后去 DOM 上挂监听会把「表格长什么样」
   的知识复制到调用方，还会在每次翻页/页内过滤重绘后失效。传了参数才有行为、没传的 16 个
   `pagedTable` 调用点与 4 个 `table()` 直调**连 DOM 都不变**，是这次改动最要紧的性质。
2. **保留「详情」按钮。** 它是键盘与读屏的路径（行本身不可聚焦，见决策 4），也是既有走查与文档
   所指的入口。整行可点是**多一个**入口，不是换一个。
3. **两处守卫都在 `ui.js` 一处实现**：①事件目标属于自带点击语义的元素（`button, a, input, select,
   textarea, label, summary, [role="button"], [role="link"]`）→ 忽略；②`window.getSelection()` 非空 →
   忽略。理由分别是：不守卫就会「点按钮开两个弹窗」（按钮的 handler 与冒泡上来的行 handler 各一次）；
   而这页的请求 ID 是给人复制的，拖选之后松手那一下是「复制」不是「打开」，弹窗还会盖住选区。
4. **不给行加 `tabindex`/`role="button"`。** 一页 20 行 = 20 个新 Tab 停留点，每个都要按一遍才能走过
   列表；行点击是**指针便利**，「详情」按钮继续承担键盘可达性。这条要写进文档，免得后来者「补齐」它。
5. **不加行级 `title`。** 这一页的格子自带信息量很大的 tooltip（路由路线逐跳、供应商、缓存口径），
   行级 title 会跟它们抢。
6. **失败处理只有一个入口。** 整行与按钮都走 `openDetail(row)`，它内部 `.catch` 转 toast——沿用
   `rowActions` 上那条既有注释的口径：未捕获的 promise 会在控制台报 `Uncaught`，而不是在屏幕上给人看。
7. **顺手收一个双击竞态。** 弹窗是**接口回来之后**才挂进 `#modal-root` 的，所以两次点击（双击，或
   按钮 + 冒泡到行）会开两个弹窗。行点击把点击面放大之后这件事更容易发生，用一个 in-flight 标记挡掉。
   这不是新机制，是这个入口本来就该有的性质。
8. **样式落在 `app.css`，不写行内 style。** 控制台 CSP 是 `style-src 'self'`，行内 style 属性会被整条
   丢弃（组织树缩进那次事故）；`style_csp_test.mjs` 钉着这条。

## 接口

```js
// internal/webui/static/js/ui.js —— 唯一新增的公共 API（可选参数，默认关）

// 自带点击语义的元素：行点击在它们身上不再触发一次。
const ROW_CLICK_OWNERS = 'button, a, input, select, textarea, label, summary, [role="button"], [role="link"]';

export function table({ columns, rows, filter, onFilter, empty, rowActions,
                        filterPlaceholder, footer, onRowClick })

export function pagedTable({ columns, load, pageSize, pageSizes, rowActions, empty,
                             filter = true, onError, footer, onRowClick })
```

`onRowClick(row)` 的契约：**只在一行被点击、且那次点击不属于行内控件、也不是选择文本的收尾时**调用一次，
参数是该行的数据对象（与 `columns[].render(row)`、`rowActions(row)` 拿到的同一个）。抛错由调用方负责
（`onError` 是加载错误的通道，不是它的）。

```js
// internal/webui/static/js/pages/requests.js

// 进入详情弹窗的唯一入口：整行与「详情」按钮共用，所以失败处理只有一份。
// 弹窗要等接口回来才挂进 DOM，pending 期间第二次点击直接丢掉（否则双击开两个）。
let detailPending = false;
function openDetail(row) { … detail(row.request_id).catch(…).finally(…) }
```

## 数据流

```
点击列表行
  └─ table() 的行监听
       ├─ 目标是 button/a/input/... → 忽略（行内控件自己处理）
       ├─ 有文本选择                  → 忽略（这是复制的收尾）
       └─ 否则                        → onRowClick(row)
                                          └─ requests.js: openDetail(row)
                                               ├─ detailPending → 丢弃
                                               └─ GET /admin/api/v1/requests/{id}
                                                    ├─ 成功 → 同一个详情弹窗（标题「请求 <id>」）
                                                    └─ 失败 → toast（api.errorMessage）
```

后端、数据库、MCP 工具描述**零改动**：详情接口与弹窗内容都是既有的。

## 异常与边界

- **详情读不到**（保留期刚清理掉这一行、响应空体）：既有逻辑抛错 → `openDetail` 的 `.catch` 转 toast，
  与今天点按钮的表现一致。
- **双击 / 按钮 + 冒泡**：行守卫拦住控件，pending 标记拦住重复请求，最多一个弹窗。
- **汇总行（`tfoot`）与空态行**：不是数据行，不带 handler、不加手型。
- **翻页 / 页内过滤重绘**：行随渲染重建，监听随行创建，不需要额外清理。
- **`onRowClick` 未提供的表**：DOM 与今天逐字相同（`class="row-click"` 都不加）。
- **只读角色**：`详情` 本来就对 viewer 可见，本次不改权限口径。
- **键盘**：不可聚焦（见决策 4）；「详情」按钮是键盘路径。

## 测试策略

- `internal/webui/tests/requests_test.mjs`（node，`make ui-base` 覆盖，沿用按源码取断言的既有风格）：
  断言 `onRowClick` 直通 `openDetail`、按钮与整行**同一个入口**、`openDetail` 带 `.catch` 与 pending 守卫。
- `scripts/ui-harness/keys.page.html` 的 `#requests` 视图新增 5 项检查（夹具用 `req_demo0001`——它是
  fixtures 里唯一有详情快照的行）：
  1. `rowClickOpensDetail`：点该行第一个 `<td>` → 弹窗出现且文本含 `请求 req_demo0001`。
  2. `rowClickCursor`：`getComputedStyle(行).cursor === 'pointer'`（类与 CSS 规则两边都得住）。
  3. `statsRowsStayPlain`：统计卡第一行 `cursor !== 'pointer'`（钉住范围只限列表）。
  4. `detailButtonOpensOnce`：点「详情」后 `#modal-root .modal-backdrop` 恰好 1 个。
  5. `rowClickIgnoresSelection`：用 `Range`/`Selection` 选中请求 ID 那一格后点击 → 没有弹窗。
- 回归：`run.sh`（全视图）、`make build` 后的压缩镜像 `UI_STATIC_DIR=.cache/ui-dist/static` 走查、
  `make test`、`make desensitize-check`。

## 依赖

标准库与既有包；不新增依赖、不改 schema、不改后端。

## 环境说明（本次实现）

- 工作区即当前会话工作区（`main` 分支）。**工作树里另有一条在飞的改动**（`chat_ui.js`、
  `scripts/ui-harness/{render_page.py,run.sh,server.py}`、`preview_artifact.page.html`、
  `cmd/dshgw/plugin/ssh-workspace/*`）：本次只提交自己的文件，不用 `git add -A`。
- `make ui-check` 在本沙箱**会静默跳过**（`/usr/bin/firefox` 是 snap 壳，答不出 Mozilla 版本）。
  真实证据来自 `.cache/ff/firefox/firefox`（Firefox 156.0.1）在 `PATH` 前面时的走查：
  `PATH="$PWD/.cache/ff/firefox:$PATH" bash scripts/ui-harness/run.sh`。

## 实现与设计差异

设计（目标/决策/接口/边界）实现时**没有改口**，以下是实现时才出现、值得记下来的部分。

- **走查断言：121 → 130（+9 项）**，名字是 `rowClickCellsFound`、`rowClickOpensDetail`、
  `rowClickDialogCloses`、`rowClickCursor`、`statsRowsStayPlain`、`detailButtonOpensOnce`、
  `detailButtonSameDialog`、`rowClickIgnoresSelection`、`statsRowClickDoesNothing`。
- **做了反向对照（新增的证据，计划里只说了"要加检查"）**：把 `onRowClick` 那一行从**静态资源的副本**里删掉
  （`/tmp/static-norowclick`，工作树一字未改），同一个走查跑出
  `[FAIL] requests: 130 checks, failed=['rowClickOpensDetail','rowClickCursor']`——断言真的会咬人，
  不是"功能没做也全绿"。这条对照同时暴露了断言写法上的一个问题，见下一条。
- **断言必须逐项独立成立**：反向对照的第一版在 `rowClickOpensDetail` 为 false 之后，
  `closeButton()` 返回 null、`null.click()` 抛出，把后面 7 项一起带走了——「功能没做」因此读成
  「这一页炸了」。改成每个断言先判元素存在（`if (rowCell('时间')) …click()`、`closeIfOpen()`），
  现在缺功能只会让相关那几项 FAIL。这与走查里"跳过 ≠ 通过"是同一条规矩。
- **全量走查在这次工作树上不是全绿，但与本改动无关**，用 `git archive HEAD` 的**干净树**对照复现过：
  `chat` 的失败来自并行会话正在改的 `chat.page.html`/`chat_ui.js`（HEAD 上 chat 110 项全绿）；
  `nodes`（8 项：`threeRows`/`moveDialogShown`/…/`purgeNeedsTheName`）与 `nodes-readonly`
  （`readOnlyHidesAdd`）在 HEAD 上以**同样的名字**失败，是既有失败（`.cache/ui-harness/server.log`
  里 2026-09-23 那次记录也有 `readOnlyHidesAdd: false`）。本次改动的 `requests` 视图在源码树与
  **压缩镜像**（`UI_STATIC_DIR=.cache/ui-dist-m91/static`）两处都是 130/130。
- **`detailPending` 放在模块作用域**（`openDetail` 与 `detail` 都在模块级），而不是 `render()` 里：
  同一页面同时只可能有一个详情请求在飞，标记跟着入口走最省事。
- **卡片上多了一行说明**「点击任意一行打开该请求的详情…」：这一页的说明文字一直在解释每个列的口径，
  入口也是口径的一部分，藏在行为里没人知道。
- **没有加 `tabindex`、没有加行级 `title`**（见决策 4/5），实现与设计一致；`row-click` 类只由 `table()`
  在 `onRowClick` 存在时打，其它 16 个 `pagedTable` 调用点与 4 个 `table()` 直调的 DOM 逐字未变。
- **环境**：`make ui-check` 在本沙箱默认**静默跳过**（`/usr/bin/firefox` 是 snap 壳，答不出 Mozilla
  版本，脚本按设计以 0 退出）。上面所有走查证据来自把 `.cache/ff/firefox`（Firefox 156.0.1）挂到
  `PATH` 前面、并显式指定 `UI_HARNESS_PORT`/`UI_HARNESS_WORK` 的那几次运行（端口与别的工作区隔离）。

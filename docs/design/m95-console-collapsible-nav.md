# M95 设计文档：控制台主菜单改成可折叠（分组手风琴 + 记忆折叠状态）

> 状态：**已实现（M95）**。设计决策 D1–D8 与实现一致；实现阶段多出来的三件事记在第 11 节。
> 前序：[M9 Web 管理界面](m9-web-console.md)、[M50 控制台前端压缩混淆](m50-frontend-minify.md)、
> [M55 控制台传输压缩](m55-console-transfer-compression.md)。
> 面向使用者的规格：**无**（本次只动控制台自己的导航 chrome，没有 API、配置、字段或数据变化；
> README 文档表里控制台自身的规格就是 M9 设计文档，故按 `docs/PROCESS.md` 第 2 步的口径不新增规格文档）。
>
> 需求原话（2026-09-28）：「管理后台的主菜单改成可折叠」。
> 已与用户确认的两个选择：**分组可折叠（手风琴）**（不是整栏折叠）、**折叠状态记在浏览器里**。

## 1. 问题

左栏主菜单（`renderNav()`，`internal/webui/static/js/router.js`）把 `routes` 表里的 **24 个路由按 6 个
分组**平铺进一个滚动区：`.sidebar nav { flex:1 1 auto; min-height:0; overflow:auto }`，而侧栏本身是
`height:100vh` 的列（`app.css:14-18`）。分组标题（`总览 / 访问控制 / 路由配置 / 计费 / 可观测 / 运维`）
今天只是 `<div class="group">` 文本标签，**没有任何折叠能力**——运营想找「设置」，要么滚过 20 来行，
要么在整列里扫一遍。

## 2. 目标

1. 每个分组标题成为**可点击的控件**：点一下收起该组条目、再点展开；收起后条目不占位。
2. 折叠状态**记住**：刷新、重开浏览器、跨会话保持（浏览器本地偏好）。
3. **当前路由所在分组永远展开**：点导航、页面内 `navigate()`、深链接、刷新落在某页，
   都不会把"我在哪"从菜单里藏起来。
4. 默认行为与今天**逐像素一致**（首次打开 6 组全展开），不使用行内样式（控制台的严格 CSP），
   不引入字体依赖的图标字形。

### 非目标

- **不做**整栏折叠成图标栏/窄条（那是另一件事：需要为 24 个条目设计一套图标，且收起后无法导航）；
- **不做**移动端抽屉、不改 `.app` 的 `220px` 栅格、不加任何 `@media`；
- 不改路由表结构、不改 `renderNav` 的签名与调用方（`app.js`）、不改任何 Go 代码、配置、数据库；
- 不复用也不重构 `tree.js` 的 `arrowIcon`（它的几何被 `tree` 视图量着，属无关范围）。

## 3. 现状（已核对的事实）

| 事实 | 位置 |
|---|---|
| 导航由 `renderNav(container)` 每次导航重建：`app.js` 的 `showRoute()` 开头调它 | `js/app.js:124`、`js/router.js:62-77` |
| 分组靠**相邻同 `group` 值**聚合，没有嵌套结构；组标题是 `<div class="group">`，条目是平铺的 `<a>` | `js/router.js:66-76` |
| 侧栏链接样式是 `.sidebar nav a { display:block; … }` —— 作者样式**会盖掉** UA 的 `[hidden]{display:none}` | `app.css:29` |
| 控制台 CSP 为 `style-src 'self'`（无 `unsafe-inline`），`style` **属性**会被浏览器整条丢弃；行内样式只能走 CSSOM | `webui/embed.go:92`、`tests/style_csp_test.mjs`（2026-09-23 组织树丢缩进的现场事故） |
| "图标要画、不要打字形"是既有纪律：`✕`（U+2715）与 `▸/▾`（U+25B8/25BE）在缺字体的机器上都是空框 | `js/ui.js:127-146`、`js/tree.js:448-470` |
| CSS 画图标已有先例（`.spinner` 是 CSS 画的圆圈） | `app.css:58-60` |
| `hidden` 属性的既有用法：`el('div', { hidden: true })` + `box.hidden = !open` | `js/pages/org.js:227,231` |
| 本地偏好已有先例：`aigw.display_currency`，读写都包 `try/catch`（隐私模式降级） | `js/money.js:17,60-71` |
| 发布构建用 esbuild 压缩：**不重命名 class/id**（`MinifyIdentifiers` 不开），且有一条"选择器必须存活"的测试 | `internal/webui/minify/minify.go:113-135`、`minify_test.go` |

## 4. 关键决策

| # | 决策 | 理由 / 否决的备选 |
|---|---|---|
| D1 | **分组手风琴**：6 个分组标题各自可折叠 | 菜单的瓶颈在纵向（24 项 / 100vh 滚动区），横向 220px 不是问题；**否决**整栏折叠（收起后不可导航，且要为 24 个条目设计图标），**否决**两者都做（两条 CSS 状态要一起测，收益不叠加） |
| D2 | 状态存 `localStorage`，键 **`aigw.nav_folded`**，值是 JSON 数组、元素是分组名（`["访问控制","可观测"]`） | 沿用 `aigw.display_currency` 的同一种做法（浏览器本地视图偏好，服务端不需要知道）；用分组名而不是下标，读起来自解释，分组改名时旧记录自然失效、不做迁移 |
| D3 | **活动分组恒展开**：`open = (group === activeGroup) \|\| !folded.has(group)`；渲染时**不回写**存储 | 把"我在哪"从菜单里藏起来是最糟的失败模式（深链接/页面内跳转都会踩到）。不回写是为了让启动只读、不写盘；代价是"在 A 组里显式点收起 A 组"会在下一次导航（同组）时被重新展开——显式点击当下仍被尊重（D6） |
| D4 | 条目包一层 `div.nav-group`，用 **`hidden` 属性**收起；并**显式**写一条 `.sidebar nav .nav-group[hidden]{display:none}` | UA 的 `[hidden]{display:none}` 是**作者样式可覆盖**的，而 `.sidebar nav a{display:block}` 正是作者样式：把 `hidden` 直接打在 `<a>` 上会"属性在、样式不在"（同一类事故在组织树里丢的是缩进）。包一层没有 display 规则的容器即可绕开，显式规则把它钉死；**否决**用类名 `.nav-group-folded`（`hidden` 同时把它移出无障碍树与页内查找，语义更准） |
| D5 | 箭头用 **CSS border 三角**，状态由 `[aria-expanded="true"]` 驱动旋转 | 不打字形（D 见现状：`▸` 在缺字体机器上是空框）；CSS 画图标已有先例；状态只有一个真源（`aria-expanded`），CSS 只是它的表现。**否决**在 `ui.js` 加 `chevronIcon()`（多一条 `router.js → ui.js` 依赖与一份 SVG 代码，收益只是"和 `closeIcon` 同款"），**否决**复用 `tree.js` 的 `arrowIcon`（会把 20KB 的树控件拉进每次页面加载的壳层） |
| D6 | 点击标题**就地更新**（改 `aria-expanded` 与 `box.hidden`，不整块重渲染） | 重新渲染会丢焦点与滚动位置；`renderNav` 本来就会在每次导航时重建，状态放在模块级 `Set` 而不是 DOM 里，两条路径不会打架 |
| D7 | 折叠是**纯渲染层状态**：`routes` 不变、`renderNav(container)` 签名不变、`app.js` 不改 | 路由表是"有哪些页面"的真源，折叠是"怎么看"，不该混进表里；调用方不需要知道这件事 |
| D8 | `localStorage` 的任何失败（隐私模式、被禁用、JSON 损坏、值不是数组）都**静默降级**成"全展开 + 本次会话内有效" | 与 `money.js` 同口径：`localStorage` 是可选能力，抛错或坏值不该让控制台白屏 |

## 5. DOM 与存储契约

```html
<nav>
  <button class="group" type="button" aria-expanded="true|false" aria-controls="nav-group-0">
    <span>访问控制</span>
    <span class="nav-chevron" aria-hidden="true"></span>
  </button>
  <div class="nav-group" id="nav-group-0">     <!-- 收起时带 hidden（box.hidden = true） -->
    <a href="#/keys">API Keys</a>
    …
  </div>
  …
</nav>
```

```js
localStorage['aigw.nav_folded'] === '["访问控制","可观测"]'   // JSON 数组，元素为分组名
```

索引按渲染顺序从 0 递增，`aria-controls` 与 `id` 一一对应（`nav-group-<i>`）。

## 6. 行为规格

| 场景 | 行为 |
|---|---|
| 首次打开（无存储 / 存储损坏 / `localStorage` 抛错） | 6 组全展开，与今天一致；不报错、不写盘 |
| 点击非活动分组的标题 | 就地翻转 `aria-expanded` 与 `hidden`，写回存储 |
| 在 A 组页面里点击 A 组标题 | 允许收起（显式操作被尊重），存储里记下；导航回 A 组时下一次渲染重新展开（D3） |
| 导航进 A 组的任意路由（点链接 / `navigate()` / 深链接 / 刷新落在 A） | A 组渲染为展开，存储不被改写 |
| 存储里有已不存在的分组名 | 按名字匹配不到 → 自然忽略，不清理、不报错 |
| 键盘 | 标题是原生 `button`：Tab 可达、Enter/Space 原生切换；`aria-controls` 指向真实存在的容器 |

## 7. 接口（模块内，无 HTTP 变化）

```js
// js/router.js —— 对外签名不变
export function renderNav(container)            // 分组标题由 div 变为 button，条目移入 .nav-group

// js/router.js —— 新增的内部函数（不导出）
const FOLD_KEY = 'aigw.nav_folded';
function foldedGroups()                         // → Set<string>，首次读 localStorage，之后用缓存
function persistFolded(set)                     // 写回 localStorage，失败静默
function toggleGroup(head, box, group)          // 就地翻转 + 持久化
```

不改：`routes`、`currentRoute()`、`startRouter()`、`loadPage()`、`navigate()`、本地 `el2()`、
`app.js` 的 `renderShell()`/`showRoute()`。

## 8. 异常与边界

1. **`[hidden]` 被作者样式盖掉** → D4 的包装容器 + 显式规则。
2. **CSP 丢行内样式** → 全程类名与 `hidden` 属性，零行内 style；由既有 `style_csp_test.mjs` 与新的
   harness `noInlineStyles` 检查双双兜住。
3. **`renderNav` 每次导航重建 DOM** → 状态在模块级 `Set`（不是 DOM），点击就地更新（D6）。
4. **压缩/嵌入**：新规则只用到 esbuild 已支持的普通/属性选择器，且不重命名 class/id；`static/` 下
   **不新增文件**，所以 `AssetCount`、`ui-dist`、gzip 副本、`-overlay`、CSP 头都不受影响。
5. **窄屏**：本次不改窄屏表现（不引入回归），整栏折叠/抽屉是另一件事。
6. **行数不变量**：6 个分组、24 条链接由 harness 断言；路由表结构未动，`embed_test.go` 里
   "路由必须注册"那组测试不受影响。
7. **`billing.js` 的表格列也叫 `group`**（`row.group`）：新 CSS 选择器全部限定在 `.sidebar nav` 下，
   不会波及。

## 9. 测试策略

1. **源码不变式（node，本环境可跑）**：`internal/webui/tests/nav_fold_test.mjs`
   —— 存储键、`try/catch`、`button[aria-expanded][aria-controls]`、`.nav-group` + `hidden`、
   活动分组恒展开的判据、三条 CSS 规则且选择器限定在 `.sidebar nav` 下、`app.js` 的壳层接线仍在、
   harness 视图已注册。挂进 `Makefile` 的 `ui-base`（⇒ `make verify` 会跑）。
2. **真机几何与交互（headless firefox，宿主上跑）**：`scripts/ui-harness/sidebar.page.html`，
   视图 `sidebar`。它自己搭与 `renderShell` 同形的骨架（`.app > aside.sidebar > nav`）后调用
   **真实的 `renderNav`**；"导航"= 改 `location.hash` 后再渲染一次（`renderNav` 自己读 hash，
   因此不需要 stub fetch、也不用等 `hashchange` 任务）。冷启动用
   `await import('/js/router.js?reload=N')` 取全新模块实例（模块级缓存失效），这是"刷新后仍收起"
   唯一诚实的验法。断言含：`foldHidesItems`（该组链接 `offsetHeight === 0`，其余组 > 0）、
   `coldLoadHonoursStorage`、`activeGroupAlwaysOpen`、`foldActiveGroupHonoured`、`keyboardReachable`、
   `noInlineStyles`。
3. **回归**：`go test ./internal/webui/`（嵌入/CSP/路由注册）、`make ui-base`、`make desensitize-check`。
4. **本环境的限制**：这里的 `/usr/bin/firefox` 是 snap 壳子（`--version` 答不出 Mozilla 版本），
   `run.sh` 会按既有逻辑跳过并以 0 退出 —— 与 M94 同样处理：新视图的浏览器验证作为**宿主待执行项**
   写进 `docs/TODO.md`（`scripts/ui-harness/run.sh --views sidebar`）。

## 10. 依赖

- 浏览器能力：`localStorage`（可选，失败降级 D8）、CSS 属性选择器、`hidden` 属性、flex。
- 仓库内：无新增依赖；不新增静态资源文件；不改 `Makefile` 的 `ui-dist`/`build` 流程。
- 部署含义：控制台资源**内嵌在二进制里**，本改动必须重新构建并重启网关才在线上可见。

## 11. 实现与设计差异

1. **多了一半"行为"测试：`vm.SourceTextModule` 跑真模块。** 第 9 节原本把行为验证全押在浏览器视图上，
   但本环境没有可用的 firefox —— 也就是说按原计划，折叠的**逻辑**在本地一次都不会被执行。
   于是 `internal/webui/tests/nav_fold_test.mjs` 除了 ①–⑦ 的源码不变式，又用 `vm.SourceTextModule`
   加载**真的 `router.js`**（它没有 import，替身 DOM 只要 ~40 行）+ 一个内存 `localStorage`，
   把"点击→收起→写存储→冷启动读回→导航进被收起的那组→当前组自动展开→再点开→从存储里删掉"
   连同**存储抛错/JSON 坏掉/值不是数组**三种降级跑了一遍。这一半当场抓到两个真 bug（见第 2 条），
   浏览器视图则继续负责只有它能证明的事：真实几何、CSS 画的箭头、`[hidden]` 有没有被盖掉。

2. **渲染循环里的闭包陷阱（测试抓到的真 bug，两处）。** 第一版把 `box` 与 `group` 都声明在
   `renderNav` 的函数作用域、在循环里反复赋值，而 click 处理器闭包引用它们：**点任何一组都会去
   折叠并记住最后一组（运维）**。源码读起来完全正常（处理器就写在它自己的节点旁边）。修法是抽出
   `appendGroup(container, name, open, index)`：标题、容器、处理器都在这一次调用自己的作用域里建，
   处理器能拿到的只有它自己那一组的绑定。测试里同时钉了行为（点哪组收哪组、存储里是哪个名字）与
   形状（`head.addEventListener('click', () => toggleGroup(head, items, name))`）。

3. **esbuild 会去掉属性选择器值的引号**，而 `internal/webui/minify` 的 `TestMirrorKeepsCSSSelectors`
   把源码与镜像的选择器逐条对比（只归一化空白与组合符）——写 `[aria-expanded="true"]` 会让那条守卫
   变红（"lost the selector … / invented the selector …"，已实测）。取舍：**不动守卫**，源码就按镜像
   的写法写成 `.group[aria-expanded=true]`（两种写法匹配完全相同的元素；`true` 是合法标识符），
   并在 `app.css` 里写明原因。若将来有人给它加回引号，守卫会以"丢失/凭空多出选择器"的形式报出来。

4. **两处测量与断言上的调整（都不改契约）**：
   - 走查窗口是 1500×2400，菜单没有溢出时 `nav.scrollHeight` 恒等于视口高度，"收起后菜单变短"会
     变成一条恒假断言 —— 改成量**最后一组容器的下边缘**（与视口高度无关）。
   - 走查页不 import `app.js`（那会带进一次永不返回的页面请求）：它照 `renderShell` 的三行骨架自己搭，
     壳层与骨架的一致性由静态断言（`renderNav(nav)` + `aside.sidebar`）钉住；"导航"= 改 hash 后再渲染
     一次（`renderNav` 自己读 hash），冷启动用 `import('/js/router.js?reload=N')` 换模块实例。
   - 额外加了一条设计里没写的不变量：新 CSS 选择器**全部**限定在 `.sidebar nav` 之下，免得波及
     别处同名的 `.group`（`billing.js` 的表格列）。


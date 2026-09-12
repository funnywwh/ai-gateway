# M35 内联声明式表单（气泡内渲染 = 提交 = 模型原地更新）

已实现，`make verify` 与 `make ui-check`（新增 `form` 视图 65 项）全绿。

## 目标

M34 已经让模型能"生成一个页面 → 你在沙箱预览里填 → 提交回会话"，但它要求模型输出**整页 HTML**，
并要求用户**点一下「预览」**才能开始填。对"我要问用户三个字段"这种最常见的场景，这个代价不合理：
表单应该像一条消息一样出现在对话里，用户就地填、就地交，回答也就地长在那张表单上。

M35 用一份**字段规格**取代整页 HTML：模型输出 ```` ```form ```` JSON，控制台**用自己的元素**把
它渲染成气泡里的表单。**沙箱那条链路不动**，需要自由排版或页面脚本的模型仍然走 `html`。

## 关键决策（含取舍）

### 1. 不用 iframe：规格进、DOM 出，而不是 HTML 进

去掉 iframe 的唯一干净做法是**不让模型提供标记**。模型给数据（字段、类型、选项、说明），控制台
用 `createElement` 造元素；任何模型文本只经 `textContent` 落地。

这直接继承了仓库既有的口径（M34 设计里"有意不做 `html`/`attr`"那一条）：控制台到目前为止没有
一处 `innerHTML`，`internal/webui/embed_test.go` 把这条钉在 `chat_ui.js` / `chat_artifact.js` 上。
M35 把 `chat_form.js` **加进同一份禁令**（并连 `html:` 这个键一起禁），因为它是渲染模型规格的文件——
一旦它出现一处标记写入，选择内联而不是沙箱的理由就不成立了。

被否决的是"净化后注入"：净化器是一个会出错的信任边界，而且净化过的 HTML 里脚本仍然不能跑，
于是既没有沙箱的保证，也没有自由页面的收益。

### 2. 去掉 iframe 消掉的三类问题（这是本次最实在的收益）

| M34 沙箱链路的问题 | M35 为什么不存在 |
| --- | --- |
| 父页面读不到子帧 DOM（不透明源），量不到高度，要新增高度帧 | 表单就在控制台的文档里，样式与高度都是本地的事 |
| 流式增量要一个 `[data-aigw-live]` 标记，而契约里从未写过它（**已实测确认的死机制**） | 增量直接写控制台自己造的节点，模型不需要知道任何标记 |
| 握手/票据/nonce/注入/端口认领 | 全都不需要：没有第二个文档，也就没有通道 |

### 3. 提交走既有的轮次端点，不新增任何服务端接口

一次表单提交就是一条 `ui_event` 提问，与 M34 完全同形（`uiEventContent` 原样复用）：

```
表单提交：目标设备信息

```json
{"source":"ui_event","event":"submit","data":{"account":"demo","region":"cn-north-1"}}
```
```

因此计费、幂等、请求日志、中断恢复全部自动一致，服务端**零改动**（无迁移、无新端点、无新配置）。

### 4. `ui` 指令复用，而不是给内联表单另写一套

"回答结束后原地更新表单"这件事 M34 已经解决了：白名单操作（text/set/class/style/show/hide/
remove/focus/disable/message/svg），元素构造，无标记注入。内联表单**直接复用**
`applyUIOps`，契约里也没有新增操作。

### 5. 顺带修掉 `applyUIOps` 的一个真实缺陷：root 自身匹配不到

`applyUIOps` 用 `root.querySelectorAll(selector)` 解析选择器，而 **`querySelectorAll` 只搜后代**。
内联表单的 root 是表单元素本身，于是模型用最自然的写法 `#form_0` 定位表单（例如挂一条 `message`）
永远失败——现象是"指令报错说没有节点匹配"，而节点明明存在。

修法是给 `applyUIOps` 增加一个可选的 `resolve` 钩子，内联表单传入"先匹配 root 自身、再搜子树"的
解析器（`resolveIn`）。没有改成"把 root 包进一个 wrapper"：那会在每次指令时重新挂载节点，
导致 iframe 重载与焦点丢失，代价远大于收益。

这条是 harness 的 `form` 视图在第一次运行时就抓出来的（`applyReportsCount` 只有 2/3）。

### 6. 指令的应用必须推迟到转录重建之后

`runTurn` 结束时会 `openSession()` **重建整个转录**。原先"在 `onFinish` 里应用指令"的写法会把更新
打在**即将被丢弃的旧节点**上：用户看到模型的更新闪一下然后消失，比不更新更糟。

实现把指令停放在 `state.pendingFormOps`，在 `openSession()` 之后由 `applyPendingFormOps()` 按
`#form_<块序号>` 在**新转录**里找目标再应用。表单 id 由代码块序号推导，所以重建前后指的是同一张表。

### 7. 流式期间不报"不是合法 JSON"

回答是流式的：模型还在写的时候，最后一个围栏没有收尾，块体是规格的**前缀**。若照常解析，
整个回答过程都会挂着一条"表单未渲染：不是合法 JSON"——一条完全由"读得太早"造成的、关于模型的假报错。

实现让 `markdown.js` 把围栏是否收尾标在 `data-closed` 上，`renderFormBlock` 跳过未收尾的块。
这条也被 Go 测试钉住（两侧都要出现该标记）。

### 8. 凭据：明确拒绝，而不是降级

`password` 与 `file` 不在支持列表里，而且是**带原因的拒绝**（不是静默降级成文本框）。理由与
docs/chat.md §6 一致：表单值会成为会话转录里的一条提问，表单不是收凭据的地方。

## 接口

### 模型契约（`internal/chat/prompt.go`）

`DefaultInlineFormInstructions`：格式、字段类型表、收到的 `ui_event` 形状、更新表单的写法、
两条硬边界。它**追加**到系统提示词，与 M34 的沙箱契约同样处理，因此运维自定义
`chat.system_prompt` 时仍然拿得到。

与 `chat.ui_bridge_enabled` 不同，这一段**不挂部署开关**：内联表单不需要票据、沙箱或桥接，
是控制台自己的渲染能力。

### 控制台（`internal/webui/static/js/pages/chat_form.js`，新增）

```js
export const FORM_LIMITS = { maxFields: 40, maxOptions: 100, maxEventBytes: 8 * 1024, … };
export function parseFormSpec(text, key)      // → { spec, error }；key 决定 #form_<key>
export function renderForm(spec, { onSubmit, onAction, onState })
//   → { node, spec, setBusy, setStatus, collect, apply, destroy }
export function collectFormValues(spec, registry)   // → 提交给模型的对象
export function resolveIn(root, selector)           // root 自身优先的选择器解析
export function describeFormOps(result)             // → 一行中文结果
```

`chat.js` 的变化：`form` 代码块内联渲染（`renderFormBlock`）、提交（`onFormSubmit` /
`sendFormEvent`）、轮次结束后的指令应用（`applyPendingFormOps`）、未提交草稿的保留
（`state.formDrafts` + `captureFormDrafts`）、以及队列条目改为带来源的对象
（原本只认"一个打开的预览"）。

## 数据流

```
模型: ```form {…}``` ──markdown 解析──▶ 气泡里出现一张表单（createElement，无标记）
用户: 填写 → 提交
   控制台: content = "表单提交：…\n```json{source:ui_event,…}```"
        └─ 同一条 POST /chat/sessions/{id}/turns（SSE、幂等、计费、请求日志）
   模型: 继续执行（可再调 MCP 工具）
        ├─ 文本增量 ──▶ 表单状态行（实时）
        └─ ```ui``` 指令 ──▶ 转录重建之后原地应用到这张表单
```

## 异常与边界

| 情况 | 行为 |
| --- | --- |
| 规格非法（未知类型、缺 name/label、选项重复、默认值不在选项里） | 不画半张表单：保留原始块 + 说明原因 |
| 围栏尚未收尾（回答还在流） | 跳过，不报错 |
| `password` / `file` | 拒绝并说明理由 |
| `name` 是 `__proto__` / `constructor` / `prototype` | 拒绝该字段（与沙箱侧同一套判断） |
| 单次提交超过 8 KiB | 拒绝并说明上限，不静默丢弃 |
| 未填的可选字段 | **不出现在 data 里**（而不是空串），"没回答"与"回答了空"可区分 |
| 模型还在回答时提交 | 排队（最多 5 条），本轮结束后按顺序发出；不做并发计费 |
| 提交中再点提交 | 按钮与控件禁用，点击被忽略（一轮一次） |
| 指令的 target 不存在 | 逐条报错并显示在表单状态行；原始 JSON 留在气泡里 |
| 轮次结束、转录重建 | 指令在重建**之后**应用；未提交的填写内容从草稿恢复 |
| 会话未绑定计费 Key | 提交被拒绝并说明原因（与手动提问同一套话术） |

## 测试策略

- `internal/webui/embed_test.go`：
  - `chat_form.js` 已内嵌；
  - **`chat_form.js` 加入无标记写入禁令**（`innerHTML`/`insertAdjacentHTML`/`outerHTML`/
    `document.write`/`html:`）；
  - `chat.js` 里 `form` 路径的三个函数与 `data-closed` 两侧都在；
  - `TestInlineFormContractMatchesTheRenderer`：**解析渲染器自己的 `FIELD_TYPES`**，与提示词承诺的
    类型双向比对；拒绝类型必须被点名；id 方案与两条硬边界必须出现。这条测试的价值在于它是"上一节
    实测发现的那类漂移"（能力存在、契约没写）的机械化防线——已实测：临时从提示词里删掉
    `textarea`/`radio` 会让它变红。
- `scripts/ui-harness/chat.page.html` 新增 `form` 视图（65 项，注册进 `run.sh` 的 `VIEWS`）：
  解析与拒绝矩阵（含凭据类型、原型污染、选项/数字/action 冲突、字段数上限）、渲染断言
  （label 与控件绑定、占位项、选项标签、无标记落地）、取值形状（数字为数字、复选为布尔、
  可选项省略）、超限拒绝、忙碌禁用、`ui` 指令应用与逐条报错、以及**一次完整的
  `内联表单渲染 → 提交 → 断言请求体 → SSE 回答 → 指令原地更新`** 链路。
- `make ui-check`：**14 个视图全绿**（`chat` 78 → 98 项）。

## 未验证的部分（如实记录）

- **真实模型会不会输出 `form`**：与 M34 同样的限制——内建 `testecho` 供应商不产出表单，所以
  "模型侧"由脚本化 SSE 与人工走查覆盖，不是自动测试证明的。契约在提示词里（含完整示例），
  但"模型是否照做"只能人工走查。
- **真实浏览器里的草稿保留**：harness 覆盖了同一页面内的重建（轮次结束后转录重建时草稿仍在），
  但"切到别的会话再切回来"的草稿保留属于同一份内存状态，没有单独走查。
- **必填校验只在界面上**：`required` 会标出必填，但提交时**不做拦截**——空值的必填文本字段会作为
  空串提交，必填复选框勾不勾都会提交布尔值。判断"用户到底答了没有"仍然是模型的事（这也是设计
  选择：控制台的提交路径只有一条，不在这里加一层可能与模型预期不一致的校验）。
- **长表单的视觉**：字段很多时的排版只在 harness 的截图里看过（截图本身不是可靠信号，
  见 `scripts/ui-harness/README.md`），没有在窄屏上人工确认过。

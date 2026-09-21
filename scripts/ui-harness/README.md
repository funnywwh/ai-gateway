# 控制台行为走查（无 node 环境下的唯一 UI 验证手段）

## 为什么有这个东西

管理控制台是**原生 ES 模块 + fetch**、源码零构建（`internal/webui`，见 M9 设计），而本仓库的构建环境
**没有 node/npm**（`scripts/goenv.sh` 只用 Go 工具链，`Makefile` 的 `test-race` 也为此写了 skip 说明）。
于是「界面改完到底跑不跑得起来」长期只能靠人肉点页面，或者干脆不验证——
`docs/TODO.md` 记着这条限制。这个目录把它变成一个可重复执行的命令。

## 它做什么

1. 把 `internal/webui/static/`（页面、`js/pages/*.js`、`app.css`）复制到工作目录，再用五个 harness 页
   （`providers.page.html` → `harness.html`、`currency.page.html` → `currency.html`、
   `keys.page.html` → `keys.html`、`paging.page.html` → `paging.html`、
   `chat.page.html` → `chat.html`）+ 一份 **API 快照**（`fixtures.json`）生成页面；
2. 起一个本地静态服务器，用 **headless firefox** 打开 harness，逐个视图（`#docs`、`#detail`、`#capacity`、
   `#create`、`#plugin`、`#plugin-cached`、`#currency`、`#keys`、`#requests`、`#paging`、`#chat`、`#skills`）渲染真实页面；
   `#capacity` 的供应商并发块是**就地合成**的：快照来自一个没有设并发上限的网关，而 `capacity` 只对设了
   `max_inflight` 的供应商出现（M44），所以 harness 在 stub 里给 `deepseek` 那一行补上实时名额与排队数；
3. harness 里 **stub 掉 `window.fetch`**，用快照回答所有 `/admin/api/v1/*` 调用——所以它不需要会话、
   不需要数据库、不碰任何上游，只验证「界面拿到这些数据会渲染成什么」；
   两个例外是**会动的**那两张表：`paging.page.html` **按窗口**回答 `/accounts`（解析请求 URL 里的
   `limit`/`offset` 并切片），`keys.page.html` 的 `/requests/dimensions` 同样按 `limit`/`offset`/`sort`
   应答（M31）——分页与排序只能对着一个"真的会动"的服务端验证，断言读的也是**带查询串的原始 URL**；
4. 断言结果通过 HTTP 回报给 runner，runner 打印每个视图的检查项并在失败时以非 0 退出。

## 用法

```sh
scripts/ui-harness/run.sh                        # 全部视图
scripts/ui-harness/run.sh --views docs detail    # 只跑指定视图（--views 后面可以跟多个名字）
make ui-check                                    # 同 run.sh（在 Makefile 里）
```

`UI_STATIC_DIR` 换掉被复制的那份控制台资源（默认 `internal/webui/static`，即源码）：

```sh
make ui-dist                                                             # 生成压缩混淆镜像 + gzip 副本
UI_STATIC_DIR=$PWD/.cache/ui-dist/static scripts/ui-harness/run.sh       # 对压缩产物跑同一套走查
# M55：连传输层压缩一起走查（静态服务器按 Accept-Encoding 发 .gz，浏览器必须能跑）
UI_STATIC_DIR=$PWD/.cache/ui-dist/static UI_HARNESS_GZIP=1 scripts/ui-harness/run.sh
```

`UI_HARNESS_GZIP=1` 要求 `UI_STATIC_DIR` 指向**镜像**（源码树没有 `.gz`），脚本在起浏览器之前先自检：
带 `Accept-Encoding: gzip` 的响应必须带 `Content-Encoding: gzip`、必须比原字节小、且 `gzip -dc` 后与磁盘文件
逐字节相同——否则直接以非 0 退出。没有这条自检，"压缩模式"很容易变成在未压缩字节上跑一遍而报全绿。

**M50 起这是"压缩没有改变行为"的主要证据**：Go 侧的静态合约测试（`internal/webui/embed_test.go`、
`internal/webui/tests/*.mjs`）读的是源码，看不到"只有压缩后才发生"的回归；对镜像跑一遍同样的
全部视图（当前 32 个）、比对逐视图检查项，才是端到端的保证。harness 页按绝对路径 import（`/js/pages/chat.js`），
而压缩保持同名文件与同名导出，所以两者走的是同一条路径。

没有 firefox 或 python3 时脚本**跳过并以 0 退出**（与 `test-race` 的处理方式一致），
所以可以放心挂在 CI/verify 流程里。

快照来自真实网关，可用 `--refresh` 重取（需要管理会话）：

```sh
GW_ADMIN_USER=admin GW_ADMIN_PASSWORD=... scripts/ui-harness/run.sh --refresh   # 写入 fixtures.json
GW_BASE=http://127.0.0.1:8099 GW_COOKIE=... scripts/ui-harness/capture.py       # 或直接用 cookie
```

`fixtures.json` 里有两类条目：

- **真实快照**：`/providers`、`/provider-kinds`、每个供应商的详情/日志/动作（由 `capture.py` 覆盖写入）；
- **合成条目**（为了覆盖插件的两条路径，故意不依赖真实插件）：
  `/providers/2`（插件，尚未握手 → 只有「读取插件声明」按钮）、`/providers/2-after`（握手后带 schema）、
  `/providers/1`（插件，已有握手记录 → 直接渲染字段表）、`/providers/*/test`（返回 `ok:true`）；
- **M23 起新增**：`/keys`（带扁平配额策略与录制模式的 Key）与 `/requests`、`/requests/{id}`
  （录制文档 + `request_bytes`），供 `#keys`、`#requests` 两个视图使用，`capture.py` 会一并重取。
- **M30 起**：`/requests` 的行带 `account_name`/`api_key_name`/`api_key_prefix`，`/accounts` 供
  「用户」筛选用，另有两份**按分组切片**的维度快照 `/requests/dimensions?group_by=account` 与
  `?group_by=api_key`——stub 会读 URL 里的 `group_by` 去取对应条目（取不到就回落到共享那份），
  因为「按 id 分组、按名字显示」只有各自的快照能描述；`capture.py --refresh` 会一并重取。
- **M24 起**：快照条目可以带分页信封（`total`/`limit`/`offset`/`has_more`）；缺 `total` 时
  `pagedTable` 退化成"只有本页"（下一页禁用），所以旧快照不会因为分页改造而报错。
  `#paging` 视图不用快照，它自带 45 行的虚拟账户表（见上）。
- **M31 起**：`/requests/dimensions` 由 stub **按 `limit`/`offset` 切片、按 `sort` 排序**之后再应答
  （照抄真实端点做的两件事）——「分页器发了什么」「排序开关发了什么」只有对着一个**会动**的服务端
  才看得出来，一个忽略查询串的 stub 会让两者都变成不可观测。`group_by=workspace` 用**合成的 45 个
  分组**（真实快照的分组数不够翻一页），并且故意让三种排序键的**首桶互不相同**：若它们指向同一个桶，
  「切了排序但顺序没变」也会全绿。合成数据写在 `keys.page.html` 的 `syntheticWorkspaces()` 里，
  `capture.py --refresh` 不会覆盖它（它只重写自己列出的键）。

- **M49 起**：`tree.page.html` 与 `org.page.html` 两个新页，共 4 个视图：
  - **M49 后续（拼音过滤等）**：`#org` 还断言成员过滤支持拼音（全拼/首字母/中英混合/多音字，按**集合**
    比较而非行序）、过滤器**不随列表滚动**（滚到底后量过滤器位置不变）、勾选成员**置顶**；
    `#tree` 断言拼音匹配器是**注入**的——默认实例只做子串匹配，传 `matchesQuery` 的实例才认拼音。
    另外 runner 支持**按视图声明严格模式**（报表里 `strictChecks: true`）：声明后 checks 里任何**非布尔/数值**
    的项都按失败处理（`sample`/`fieldRows` 除外），因为一个失败时返回描述字符串的断言 helper 会"失败却显示
    通过"——组织页就发生过一次。**默认不严格**：models/chat/bridge 等视图故意把 checks 当成证据草稿纸
    （`rowActions` 是按钮列表、`parseError` 为空表示解析通过、`uiApplyThrew` 为空表示没抛错），判定另用
    布尔项；改这些视图的语义是另一件事，不该由一条新规则在背后完成。未声明严格模式时，这些非判定项会
    打印成"仅供参考"。
  - `#tree` **不需要任何快照**——树控件（`js/tree.js`）是纯视图，输入是节点数组加回调。它把**同一个
    控件挂两处**（侧边栏 `.sidebar` 容器 + 工作区容器），断言两种情况下的层级缩进、折叠/展开、键盘
    ↑↓←→Enter/Home/End、roving tabindex、行内 action 回调、过滤（保留祖先）、空态、孤儿节点与
    **成环数据不挂死**。断言读的是**可见行**（`visibleIds` 由标签文本推出），因为「折叠是否真的少画了行」
    只有对着 DOM 才看得出来。
  - `#org` / `#org-readonly` / `#org-accounts`：stub 里有一份**会变的**组织树（写操作会改它），
    账号夹具同样是**会变的**（新建成员/编辑/分配组织都真的改它，成员关系由账号侧派生），
    并断言组织树**只出现在工作区**（页面不得挂侧边栏实例、shell 不得再提供 `.sidebar-slot` 插槽），
    所以"保存成员"是否真的落库、删除是否带 `cascade=true`、账户页筛选是否把 `org_node_id` 发到了
    服务端，读的都是**带查询串的原始 URL 与请求体**。`org-readonly` 用 `session.role=viewer` 渲染同一页，
    断言写入口整体消失、成员复选框禁用。
  - **M72 后续（用户反馈的人员表与三类弹窗）**：`#org` 断言人员列表是**多列表格**（表头文案、不同行的
    同一列左沿 x 必须相等、详情行横跨整行、勾选框落在自己的列里且没被全局 `input{width:100%}` 撑满）、
    「新建成员」在建号请求里就带上当前节点 id 且建完立刻出现在第一行并已勾选；`#org-person` 断言
    「收起」真的收起、「编辑」在「展开」前面且 PATCH 的 body 正确、「分配组织」弹窗里当前归属按名字
    预勾选、**勾父节点不连带勾子节点**、拼音过滤、整表替换的 body 与空选警告；`#org-accounts` 断言账户页
    的新建/编辑表单用的是同一棵勾选树（不再手填节点 id）；`#org-bind` 断言选人弹窗**先弹出、再读通讯录**，
    读取期间显示进度、失败/空目录/缺权限都在框内说明。

- **M70 起**：`org_feishu.page.html`（组织架构页的「同步飞书」弹窗），共 3 个视图：
  - **`#org-sync`**：stub 里有一份**照真机形状**编的飞书通讯录（部门 BFS 序、一个人可属两个部门、
    三条匹配通道各一个样本、一个未匹配的人），断言的是：弹窗打开时读目录**不带** `refresh`
    （第一次走缓存）、同步按钮先二次确认再 `POST /org/feishu/sync`、确认后重新预览**带** `refresh=true`、
    人员过滤支持拼音、选中「市场部」后人员列表只剩该部门的人。
    两个写操作各自断言到**请求体**：「创建用户」`POST /org/feishu/users/ou_new/account` 且 body 是手填的名字；
    「绑定账号」弹窗里同名候选**预选**、已绑给他人的账户**不可选**（`disabled` + 文案），
    拼音过滤后选「张三」→ `PUT …/account` 的 body 是 `{"account_id":3}`；「解绑」先确认再 `DELETE`。
  - **`#org-sync-readonly`**：`session.role=viewer` 渲染同一页，「同步飞书」按钮存在但**禁用**，弹窗打不开。
  - **`#org-sync-nonames`**：飞书缺「获取部门基础信息/获取用户基本信息」数据权限时（这是本机部署的**真实状态**）
    名称全空：弹窗顶部必须给出明确警告（不是静默画空白行）、部门用 `od_…` 编号显示、
    未匹配的人仍然能点「创建用户 / 绑定账号」由人来决定。
  - 三个视图都用 `strictChecks: true`，所以 checks 里任何非布尔项都算失败（`sample` 除外）。


- **M66 起**：`admins.page.html`（管理员页 + 登录卡片），共 3 个视图：
  - **`#admins`**：stub 里有一份**会变的管理员表**——建号、改角色/状态、重置口令、邀请、解绑、删除
    都真的改它，所以「刷新之后那一行没了」是对着一个确实少了那行的服务端看的，而不是对着页面自己
    画过的东西。夹具那四行分别覆盖：已绑定飞书且由 bootstrap 配置重建的管理员、等邀请的 viewer、
    被停用的账号、以及一个**只有飞书凭据**的管理员（解绑它必须退回 `pending`，这正是 D3/D6 的口径）。
    断言读的是**发出去的东西**：建号的请求体、PATCH 的角色与状态、邀请/重置/解绑/删除的路径与方法，
    以及分页器把 `limit`/`offset` 真的发到了服务端（带查询串的原始 URL）。三处只有浏览器看得见的：
    邀请链接字段**只读且打开即选中**（Ctrl+C 不用鼠标）、停用账号点「邀请链接」显示的是**服务端的
    原话**（409 而不是前端重写一遍）、以及**同一页再渲染一次"没有飞书"的部署**（D8：邀请与解绑消失，
    建号/编辑/重置口令/删除照常）。
  - **`#admins-readonly`**：`session.role=viewer` 渲染同一页，断言写入口整体消失（行内一个按钮都不剩）。
  - **`#login`**：直接 `import('/js/app.js')`（动态导入，另外两个视图不受它的 `boot()` 影响），stub 对
    `/auth/me` 回 401，于是渲染出登录卡片：断言「飞书扫码登录」入口在口令表单**下面**、服务端给的
    `hint` 在屏幕上、口令表单本身一字未改，并且**没有任何请求打到 `/feishu/login`**——那是整页导航的
    地址（授权页与它的 cookie 是浏览器的事），谁把它改成 fetch，这条就是唯一会红的地方。
  - stub 对**没有夹具的请求返回 500，并把方法+路径记进 `unmatched`**（`noUnmatchedRequests` 因此失败）：
    静默回 `{}` 会把"页面调用了一个没人注册的接口"伪装成"服务端没什么可说的"。

## 前三个坑（改这个 harness 前先读）

1. **`--screenshot` 的产物不可信**：本环境下所有视图的 PNG 字节完全相同（浏览器在页面还没画完时就截了图），
   所以**不要**用图片判断成败——一切以 `/report` 回报为准。
2. **页面会在截图之后被拆掉**：因此 harness 里**不能有 `setTimeout` 等待**（等它等于把控制权交还给浏览器，
   随后的断言可能再也跑不到）。所有等待都是纯微任务（`settle()`），并在每个阶段立刻回报。
3. **回报必须用 `navigator.sendBeacon`**：同步 XHR 在页面被拆掉时会被丢弃（实测同一份断言，XHR 版本
   一条都收不到，beacon 版本全部收到）。另外 firefox 的 snap 封装要求 `HOME`/`XDG_RUNTIME_DIR` 可写，
   且每次运行要换一个 `--profile`，脚本都已处理。

## 第四个坑：`$!` 不是服务器的 PID（重复运行会失败）

`run.sh` 曾经这样起服务器：

```sh
(cd "$WORK/site" && python3 server.py "$PORT" >log 2>&1 & echo $! >"$WORK/server.pid")
```

这里 `&` 作用在整个 `cd … && python3 …` **列表**上，所以 shell 会 fork 一个子 shell 去跑这个列表，
`$!` 记下的是**那个子 shell 的 PID**，而不是 python 的（实测：pidfile 写 21，真正的服务器是 22，
且被 reparent 到 PID 1）。子 shell 很快退出，于是结尾的 `kill $(cat server.pid)` 打在一个**已死的 PID** 上、
静默失败（`2>/dev/null` 把错误也吞了），真正的服务器一直活着占着 8097。

后果是 **`make ui-check` 不是幂等的**：第二次运行会 `rm -rf` 掉工作目录、新服务器 bind 失败
（`OSError: [Errno 98] Address already in use`），然后报"the harness server did not start"。
在 DSH 沙箱里每次命令结束会回收后台进程，所以这个 bug 只在**同一条命令里连跑两次**才暴露
（实测 20 次连跑挂了 19 次）；在宿主终端上它就是"第二次 `make ui-check` 必挂"。

三处修好，缺一不可：

1. **用 `exec` 让 `$!` 就是服务器本身**：`(cd "$WORK/site" && exec python3 server.py "$PORT") &`。
   这与 `scripts/local-run.sh` 早就用的写法一致（那边是内层 bash 先 `echo $$` 再 `exec`，注释里写明了原因），
   也就是说 harness 才是那个例外。
2. **readiness 检查要对 HTTP 错误失败**：原判据 `curl -sS -o /dev/null …/harness.html` **没带 `-f`**，
   于是 404 也算"起来了"——一个上轮残留、正从已被删除的目录里应答 404 的服务器能骗过它，
   接着每个视图都会以"no report"失败（这很可能就是当时 `brand` 视图偶发失败的真身）。
   现在改为请求一个**每次运行都重新生成的哨兵文件**并比对内容：只有"正在服务本次运行的目录"才算就绪。
3. **退出时确保端口真的释放**：`stop_server` 会 kill 后轮询到端口不再应答（最多 5 秒），
   并挂 `trap … EXIT`，所以 `^C`、超时、断言失败都不会给下一次运行留下监听者；
   入口处还有一道"端口已被占用就明确报错（exit 2）"的检查，而不是甩一段 Python traceback 给人读。

验证：`brand` 视图连跑 20 次全绿；完整 `make ui-check` 连跑 2 次全绿（修复前第二次必挂）；
`SIGTERM` 打断运行后残留服务器进程数为 0、端口已释放；端口被占用时明确拒绝并给出处理办法。

## 与 Go 测试的分工

| 关注点 | 在哪里验证 |
|---|---|
| schema 与 `Config` 字段不漂移、字段必须有说明 | `internal/providers/schema_test.go`（反射，双向差集） |
| 管理面契约：`/provider-kinds`、详情带 schema、读文档不启动插件 | `internal/httpapi/admin_test.go` |
| 界面渲染与交互（字段表、模板、插件握手按钮、Key 配额/录制编辑、请求日志详情） | 本目录（真实浏览器 + API 快照） |
| 控制台录制枚举与服务端一致、设置页不出现死键 | `internal/webui/embed_test.go`（读内嵌资源） |

也就是说：**Go 测试保证数据对，这里保证界面把数据讲明白了。**

## M32 起：`chat` 与 `skills` 两个视图

`chat.page.html` 与其它 harness 页有两个不同点，都是被功能本身逼出来的：

- **回答流是真的流**。`api.js` 的 `streamPost` 从 `resp.body` 逐帧读 SSE，所以 stub 必须返回一个真的
  `ReadableStream`（`sseResponse()`）而不是一段字符串——否则测的就不是页面，而是 stub。
  帧序列照抄服务端：`turn/step/text/reasoning/tool_call/tool_result/usage/notice/message/done`。
- **预览是一次往返**。点「预览」会上传待预览的正文、拿回票据、再把票据拼进 iframe 的 `src`；
  断言读的是**发出去的请求体**（必须是用户看到的那份字节）与 iframe 的 `sandbox`/`src`，
  而不是"有没有弹出窗口"。

两个视图的断言都在**纯函数**上起步：`markdown.js` 的转义与协议白名单、`chart.js` 的规格校验
（非法 JSON、长度不一致、超过 8 序列、饼图负值、全 null），因为这些规则是安全相关的，
坏了以后只会表现成"图有点怪"。随后才是页面行为：`＋` 菜单里技能的勾选态、停止按钮、
usage 脚注、`#/chat?session=` 深链、`#/skills` 的编辑/删除确认与空态。

技能草稿在两个页面之间用 `sessionStorage` 交接（`aigw.chat.draft`）：草稿是**未保存**的东西，
没有理由先发到服务端再取回来，所以 harness 里也是同一个进程内的这一个小箱子。

## M34 起：可交互预览的桥接断言（仍在 `chat` 视图内）

模型生成的 HTML5 页面提交表单要回到会话，这条链路横跨四个地方：服务端注入的脚本、iframe 的沙箱、
控制台的 `MessagePort`、以及"提交就是一条正常提问"的计费路径。harness 覆盖的是**中间两段加上最后一段**：

- **握手是一个拒绝矩阵**：`hello` 必须来自这个 frame、带这个 frame 的 token（从帧自己的文档里读，
  不是 URL 里的票据）、且必须带 `MessagePort`。错 token、错 source、无 port、错帧类型四种都被断言
  "状态没变"，因为这条通道的鉴权全部在这里。
- **`MessageChannel` 是真的**：harness 建一条真 `MessageChannel`，把 `port2` 随 `hello` 交给控制台，
  再用 `port1` 发事件、读回灌。这样测的是 `chat_ui.js` 的真代码，而不是一个模仿它的替身。
- **流可以被按住不放**：`sseStream()` 返回一个由测试推动的 `ReadableStream`（`window.__stream.frame/finish`），
  用来制造"模型还在回答时用户又提交了一次"——排队路径只能在那种时刻被观测到。
- **提交就是提问**：断言读的是发往 `/chat/sessions/{id}/turns` 的**请求体**，必须是
  `label` + ```` ```json {"source":"ui_event",…} ````，并且带自己的幂等 `turn_id`。
- **回灌**：`window.__uiReply` 让 stub 的回答带一个 ```` ```ui ```` 指令块，然后断言端口收到
  `{k:'d'}` 与 `{k:'done', ops:[…]}`。

两处与真实环境的差异要记住（见 `docs/design/m34-ui-bridge.md`「未验证的部分」）：

1. **帧内的注入脚本没有真跑**：harness 的静态服务器不提供带票据的产物 URL，所以
   `contentDocument` 的 token 读取用一个**读取帧属性**的 stub 代替（也因此"错 token"这一格测的是
   真代码）。注入脚本本身由 Go 侧测试钉住（无 fetch/eval/innerHTML、ES5、握手标记）。
2. **没有真实模型**：表单是测试直接构造的 HTML，`ui_event` 的"模型如何响应"由脚本化 SSE 回答。

`scripts/ui-harness/server.py` 取代了 `python3 -m http.server`：它给每个响应加 `no-store`。
console 自己的 `max-age=300` 在生产里是对的，在这里会让浏览器跑**上一版**模块——第一次调试时
就因此追着一个已经修好的报错跑了一轮。

## `bridge` 视图：注入脚本的浏览器语法检查

第 13 个视图不是控制台页面，而是**服务端注入脚本**的检查：注入脚本由 Go 字符串拼接生成
（`internal/httpapi/chat_ui_bridge.go`），"Go 代码能编译"完全不能说明它是合法的 JavaScript。
`bridge_syntax.page.html` 用 `new Function(source)` **只编译不执行**（这条脚本会绑定 submit 监听并向
父窗口发端口，不能真跑），并顺带断言：无 `fetch`/`XMLHttpRequest`/`eval`/`innerHTML`/`document.write`/
存储访问、保持 ES5（无箭头函数、无模板字符串、无 const/let）、含握手标记、绑定
`submit`/`data-aigw-send`、以及 SVG 走 `createElementNS`。

脚本正文来自 `fixtures.json` 的合成条目 `/js/pages/chat_ui_bridge_script.js`。它之所以不会漂移，
是因为 `internal/httpapi` 的 `TestUIBridgeScriptDumpForTheHarness` 在**每次 `go test`** 时把
`uiBridgeScript()` 重写进该夹具——那个测试会写仓库，这是刻意的：一个不更新的夹具会变成
"自己和自己一致"的假检查，比没有检查更糟。

改了注入脚本之后的正确顺序是 `make test`（刷新夹具）→ `make ui-check`（浏览器判读）。

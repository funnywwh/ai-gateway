# 控制台行为走查（无 node 环境下的唯一 UI 验证手段）

## 为什么有这个东西

管理控制台是**原生 ES 模块 + fetch**、零构建（`internal/webui`，见 M9 设计），而本仓库的构建环境
**没有 node/npm**（`scripts/goenv.sh` 只用 Go 工具链，`Makefile` 的 `test-race` 也为此写了 skip 说明）。
于是「界面改完到底跑不跑得起来」长期只能靠人肉点页面，或者干脆不验证——
`docs/TODO.md` 记着这条限制。这个目录把它变成一个可重复执行的命令。

## 它做什么

1. 把 `internal/webui/static/`（页面、`js/pages/*.js`、`app.css`）复制到工作目录，再用五个 harness 页
   （`providers.page.html` → `harness.html`、`currency.page.html` → `currency.html`、
   `keys.page.html` → `keys.html`、`paging.page.html` → `paging.html`、
   `chat.page.html` → `chat.html`）+ 一份 **API 快照**（`fixtures.json`）生成页面；
2. 起一个本地静态服务器，用 **headless firefox** 打开 harness，逐个视图（`#docs`、`#detail`、`#create`、
   `#plugin`、`#plugin-cached`、`#currency`、`#keys`、`#requests`、`#paging`、`#chat`、`#skills`）渲染真实页面；
3. harness 里 **stub 掉 `window.fetch`**，用快照回答所有 `/admin/api/v1/*` 调用——所以它不需要会话、
   不需要数据库、不碰任何上游，只验证「界面拿到这些数据会渲染成什么」；
   两个例外是**会动的**那两张表：`paging.page.html` **按窗口**回答 `/accounts`（解析请求 URL 里的
   `limit`/`offset` 并切片），`keys.page.html` 的 `/requests/dimensions` 同样按 `limit`/`offset`/`sort`
   应答（M31）——分页与排序只能对着一个"真的会动"的服务端验证，断言读的也是**带查询串的原始 URL**；
4. 断言结果通过 HTTP 回报给 runner，runner 打印每个视图的检查项并在失败时以非 0 退出。

## 用法

```sh
scripts/ui-harness/run.sh                        # 全部视图
scripts/ui-harness/run.sh --views docs detail    # 只跑指定视图
make ui-check                                    # 同 run.sh（在 Makefile 里）
```

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

## 三个踩过的坑（改这个 harness 前先读）

1. **`--screenshot` 的产物不可信**：本环境下所有视图的 PNG 字节完全相同（浏览器在页面还没画完时就截了图），
   所以**不要**用图片判断成败——一切以 `/report` 回报为准。
2. **页面会在截图之后被拆掉**：因此 harness 里**不能有 `setTimeout` 等待**（等它等于把控制权交还给浏览器，
   随后的断言可能再也跑不到）。所有等待都是纯微任务（`settle()`），并在每个阶段立刻回报。
3. **回报必须用 `navigator.sendBeacon`**：同步 XHR 在页面被拆掉时会被丢弃（实测同一份断言，XHR 版本
   一条都收不到，beacon 版本全部收到）。另外 firefox 的 snap 封装要求 `HOME`/`XDG_RUNTIME_DIR` 可写，
   且每次运行要换一个 `--profile`，脚本都已处理。

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

# 实现 TODO（里程碑检查清单）

> 维护规则：每完成一项即勾选；每里程碑开工前先写 `docs/design/{milestone}-*.md`
> **并贴到对话中确认后再写代码**（详见 `docs/PROCESS.md`，提交前按其中的检查项自检）。
> 状态：`[ ]` 未开始 · `[~]` 进行中 · `[x]` 完成

> 已完成项已拆到 `docs/todo_done.md`：**勾选后就把该条（连同缩进子条目）搬到那边**，整节做完搬整节；本文件只留 `[ ]` / `[~]` 与各节引言。

## M7 Hooks 与录制
> 本节已完成的 8 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 其余事件（provider.*、apikey.*）在需要时补齐（当前没有消费方，避免无谓的事件量）

## M9 Web 管理界面
> 本节已完成的 9 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 浏览器人工走查（当前环境无浏览器；已用 Node 对新页面做语法与导入图校验，接口逐条 curl 验证）

## M13 性能与并发
> 本节已完成的 8 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 优化项（未做）：减少每请求的写入行数（response/request log 合并或异步），以压低 p99 长尾

## M16 数据库自动备份
> 本节已完成的 13 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] v2：备份到对象存储/异地同步（规格已声明不在 v1 范围）

## M17 完善内置供应商（openai-chat）：DeepSeek 适配与思考模式
> 本节已完成的 15 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 暂不纳入：内置 `openai-responses` 接 DeepSeek `/responses` 的三处缺口（`response.reasoning_text.delta` 事件名、`output_tokens_details.reasoning_tokens`、思考正文承载字段）

## M19 面向真实客户端的方言翻译（核心只接受，翻译在 provider 层）
> 本节已完成的 14 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 观察项（非阻塞，未追查）：上述成功运行的 stderr 有 1–7 条 `codex_core::util: OutputTextDelta without active item`。已核对网关输出**事件顺序规范**且 delta 的 `item_id` 确为已 added 的那个，答案与退出码均正确 → 判断为客户端侧噪声或其对某类流式形状的额外期待

- [ ] 后续议题：provider 跳过工具（如 chat 路径丢掉 `web_search`）目前**无上报通道** —— 协议里没有 provider 声明降级的字段（`DegradedFeatures` 只由路由的能力校验写入）。要为"能力降级可见"补协议字段，另立里程碑

## 可选
> 本节已完成的 100 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

### M10b 出网代理（`proxy`）
> 本节已完成的 12 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 范围外（另立）：内建 provider 接代理，需新增 `internal/providers/httpx → pkg/providerkit` 分层边

### M10c 健康探测改为真实流式补全
> 本节已完成的 15 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 已知限制（本次不改行为，仅记录）：`reasoning.effort:"minimal"` 被上游 400 拒绝（`"low"` 可用但实测仍 14–17s 且 `reasoning_tokens:0`）；插件仍会透传客户端/配置的 `reasoning_effort`，配成 `minimal` 会导致 400

- [ ] **新发现的运维隐患（比本里程碑更严重，另立处理）**：真实部署里 refresh_token **已被轮换**，而插件**不把轮换后的凭据回写到数据库** —— 有效值只在 `$GW_PLUGIN_STATE_DIR/<instance>/session.json`，而数据库/控制台里显示"已设置"的那份是轮换前的失效值。状态目录一旦丢失（清 `data/`、改实例名、重装插件），供应商会以"凭据已配置"的姿态持续失败。协议里的 `notify`（设计用途正是凭据回写）插件未使用，属 M10 设计缺口

- [ ] 部署动作（需在宿主执行）：运行中的网关仍是旧二进制（10s deadline + 旧 `/me` 探测 + 旧界面资源；界面资源内嵌在二进制里，所以这次 UI 改动同样要重启才生效），需 `./scripts/local-run.sh restart`

### M10d codex 适配器翻译 `system` 角色
> 本节已完成的 15 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] M14(2) 门户 API（`/portal/api/v1/*`：读自有数据 + 建/轮换自己的 Key + 兑换码 + 改口令）与第二套嵌入式 UI

- [ ] M14(2) 前端共享化：`internal/webui/shared/{api.js,ui.js}` 被 admin 与门户两套 UI 复用（各自前缀下服务）

### M19b 流式终态：截断不得伪装成完成（DSH 实测报告）
> 本节已完成的 12 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待宿主执行**：运行中的 8088 仍是旧二进制（本次会话所在的沙箱与宿主不同 PID namespace，无法向该进程发信号），
  需在启动它的终端执行 `./scripts/local-run.sh restart`；插件二进制已重建（`bin/` 与 `plugins/aigw-provider-codex`），重启后生效

### M19f 审计落库不跟随请求 context（诊断盲点修复）
> 本节已完成的 3 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **同类问题（未改，涉及计费语义，待定）**：`recordAttempt` 写用量/结算也跟随请求 context ——
  客户端挂断时该次尝试的用量行同样会丢（上条测试的日志里就能看到
  `recording usage failed err="store: insert usage record: context canceled"`），
  即上游已经产出的 token 不会被计量。改法与审计一致（派生 detached context），但会改变用量/账本里出现的行数，需先定口径

### M20 DSH 侧可设推理档位：能力申报与档位表对齐（DSH 实测报告）
> 本节已完成的 7 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 未覆盖：DSH GUI 里菜单文案与"选档位跑一轮任务"的人工确认

- [ ] 可选（未做）：让 DSH 在"添加模型"时自动识别推理能力。**已查明这条在纯配置层面走不通**（2026-09-11 读 DSH 0.1.2-rc.1 源码）：
  ① 发现链路只搬运四个字段——`llm-pi-ai` 的 `readListing()` 只读 `id/name/context_window/max_output_tokens`，
  `discoverModels()` 对目录路由也只返回 `id/name/contextWindow/maxTokens`；② 客户端"采纳"候选时写死同样四个键
  （`dsh-client-ui-settings-models/lib/client.js` 的 `adopt()`：`{id, name?, contextWindow?, maxTokens?}`）；
  ③ 唯一能带 reasoning 元数据的来源是 pi-ai 自带目录（40 个 provider 的 `dist/providers/data/*.json` 里有
  `reasoning` 与 `thinkingLevelMap`），但那要求路由名与 model id 都命中目录——本网关的 `aigw`/`gpt-5.6-luna`/`deepseek-flash`
  都不在其中。所以"自动识别"要么改 DSH 自身，要么写 DSH 插件自带模型目录；两者都超出本仓库范围

## M22 多币种：模型级币种 + 账本换算 + 可选显示币种
> 本节已完成的 22 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计：`docs/design/m22-currency.md`（已批准的口径：单一账本币种 + 结算换算；
> 成本/售价各自可配币种；汇率 config 静态表 + 设置页可覆盖；显示币种覆盖控制台并修正对外币种字段）。

### M22 运维：8088 运行态加人民币（展示币种）
> 本节已完成的 6 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> `config.yaml` 被 gitignore，运行态改动在这里留痕（沿用 M20b 的做法）。

- [ ] **待人工执行**（必须在自己终端里跑，DSH 沙箱启动的进程会被回收）：
  `scripts/local-run.sh restart` —— 让「默认展示币种 = CNY」与最终 M22 二进制生效

## M23 请求日志默认只保留用户输入 + 配额口径收口
> 本节已完成的 24 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计文档 `docs/design/m23-input-recording.md`（实现前已在对话中输出并确认）。
> `config.yaml` 被 gitignore，运行态改动在这里留痕（沿用 M22 的做法）。

- [ ] 观察项（已量化，2026-09-11）：在 708 MB 真库的**副本**上用同一驱动与同一 pragma
  （WAL + synchronous=NORMAL + busy_timeout=5000，写入池 `SetMaxOpenConns(1)`）实测单行插入：
  * 新形态 1.8 KB：p50 ≈ 40 µs、p95 ≈ 0.3 ms、max ≈ 0.4 s
  * 旧形态 985 KB：p50 ≈ 7 ms、p95 ≈ 0.5 ms、max ≈ 0.7 s
  * `wal_checkpoint(PASSIVE)` 0.26–0.49 s；计费批 256 行 0.12 s
  结论：写入池按设计串行，单行成本 × 并发 = 队首等待；**旧默认每请求塞 ~1 MB 是主导项**，
  收窄到 `user` 后余量有数量级级别，因此不需要改异步/背压架构。
  但仍有两条口子：① 失败即丢行（5s 截止 + 任何一次多秒级停顿，实测外部大 IO 刷脏页时
  1.8 KB 行也能到 5.5 s）；② `recording.retention_days` 至今无效 → 请求日志永不清理
  （真库 708 MB 里 689 MB 是历史正文，占 97%）

- [ ] 建议的小改动（待定）：写入失败时**降级重写骨架行**（只留 request_id/status/bytes，正文为空）
  并加一个 dropped 计数，保证最坏情况下请求仍可见——M19 的教训正是「要排障的请求恰恰没记录」

- [ ] 另立：**实现月度配额**（`monthly_*` 目前只解析不执行）——需要读 `usage_counters` + 缓存 +
  结算后失效，注意不要给请求路径增加无谓的打库开销

- [ ] 另立：死设置键（`recording.default`/`recording.max_bytes`/`mcp.max_query_rows`/`backup.retention`）
  要么实现读取方，要么从设置页文案里彻底移除

## M25 请求日志的写入兜底与保留期清理
> 本节已完成的 11 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计文档 `docs/design/m25-log-retention.md`（实现前已在对话中输出确认；编号顺延说明见文档开头）。
> 起因：M23 收尾时把那次写入超时量化了——真库 708 MB 里 689 MB 是历史正文，且 20 次失败里至少 2 个
> 请求**整行都没落库**（详见 M23 的观察项）。

- [ ] **待人工执行**（宿主终端）：`scripts/local-run.sh restart` —— 让 M24（列表分页）与 M25 一起生效

- [ ] **待人工执行（宿主终端，需停实例）**：VACUUM 回收空间。删行只把页还给 freelist，文件不会自己缩小：
  `scripts/local-run.sh stop` →
  `python3 -c "import sqlite3;c=sqlite3.connect('data/aigw-local.db');c.execute('VACUUM');c.close()"` →
  `scripts/local-run.sh start`（VACUUM 需要独占访问；本机没有 sqlite3 CLI，用 python 的 sqlite3 即可）

- [ ] 观察项：清理后若仍有 `put request log` 超时，先看 `/stats` 的 `request_log.dropped`——
  它是「连骨架行都没写进去」的权威计数，比翻日志可靠

## M26 请求路径的审计写入批量化（CPU 与尾延迟）
> 本节已完成的 15 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

设计文档：`docs/design/m26-audit-batching.md`（含取舍、接口、异常边界与实测数据；实现了 M25 里
「不需要异步队列或背压」那个结论的反例）。

起因：用宿主 `--pid=host` 容器采样 8088 实例（沙箱 PID namespace 看不到宿主进程），再用
带 pprof 的副本实例压测归因，结果指向同一个瓶颈 —— **请求路径上的落盘**。

- [ ] 观察项：若 N 个请求对应的事务数接近 N，说明批没合上（看 `batch_writes` 是否被关、或并发太低 /
  间隔太短）——注意**低频逐个请求时 1:1 是正常的**（每个请求自己触发一次 flush）

- [ ] 观察项：`aigw_audit_queued_requests` 持续 >0 且不降说明写侧堵了；若同时
  `/stats` 的 `request_log.batching.backpressure` 在涨，就是队列满了在背压，需要调大
  `batch_queue_bytes` / `batch_max_bytes`，或排查请求体为何变得很大

## M27 请求日志的身份维度与消耗度量
> 本节已完成的 14 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计文档 `docs/design/m27-request-dimensions.md`，规格文档 `docs/request-log.md`（编号顺延说明见设计文档开头：M26 已被 `e42b066` 占用）。
> 起因：请求日志只能回答「有一条请求、它多大、成功没有」——要回答「谁在用、用哪个模型、在哪个工作区、
> 属于哪个会话、花了多少」只能去翻正文，而正文会撞 1 MiB 截断（实测 397/2297 行）又会按保留期被清掉。

- [ ] **待人工执行**（宿主终端）：重启 8088 实例让迁移 0008 生效，然后发一条 DSH 请求核对身份列

- [ ] 观察项：维度统计卡在窗口很大时的耗时（当前是窗口扫描 + usage 点查 join）；若 p95 超过 1 秒，
  按设计文档 §2.2 的同一形态补 workspace/call_kind 索引

- [ ] 观察项：`unknown` 桶的占比。持续偏高说明出现了新的客户端（或某个客户端改了提示词），
  需要在 `dimensions.go` 补一条结构性规则，而不是放宽全文匹配

## M28 控制台补齐「供应商模型」管理 + 映射写入改成部分更新
> 本节已完成的 7 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 起因（用户报告）：在控制台「模型」页加了 `gpt-6-astra`、在「模型路由」页加了指向 codex 的路由，
> `/v1/models` 里却始终没有它。查下来是三层里漏了第一层 `provider_models`，而**控制台根本没有管理它的界面**：
> 「模型」页管 `models`、「模型路由」页管 `routes`，供应商详情页只有配置/凭据/探测/日志。
> 前端里唯一碰 provider model 的两处是详情弹窗的「刷新模型发现」（只能落库"插件 config 里已声明的模型"）
> 和定价页的「保存为成本规则」（见下条，它会毁字段）。

- [ ] **待人工执行**（宿主终端）：`scripts/local-run.sh restart` 让 8088 用上新二进制，
  然后在控制台验证「模型供应商 → codex → 详情 → 模型映射」能看到 luna 与 astra 两行、
  且不再出现「路由缺映射」告警

- [ ] 观察项：同类"静默排除"还有没有别处——候选过滤的其它原因（`not_granted` / `missing_capability` /
  `circuit_open`）在 `/v1/models` 里同样是无声丢弃，是否也要在控制台集中暴露

## M29 请求日志列表底部的本页汇总行（tokens 入/出 + 成本）
> 本节已完成的 10 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计文档 `docs/design/m29-request-log-page-summary.md`，规格文档 `docs/request-log.md` §4/§6。
> 起因（用户原话）：「管理后台的请求日志列表在列表底部添加一列汇总行：tokens（入/出），成本，汇总这两列」。
> 口径当场澄清并选定：**只汇总当前页已加载的行**，不做筛选窗口级合计（窗口口径看「维度统计」卡）。

- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart` 后看
  http://127.0.0.1:8088/admin/ui/#/requests —— 控制台资源是 `//go:embed` 进二进制的，
  且 JS 带 `Cache-Control: public, max-age=300`，要硬刷新（Ctrl+Shift+R）才不会看到旧脚本

- [ ] 观察项：本页合计是否被误读成窗口合计。若确实有人这么读，按设计文档 §2.1 的路径补窗口级
  `summary`（真库实测 11 ms；未计量计数写成 `SUM(CASE WHEN u.request_id IS NULL …)` 可避开
  `COUNT(DISTINCT)` 的 temp B-tree），footer 机制不用改，只换数据源

## M30 请求日志的用户（账户）与 API Key 维度
> 本节已完成的 19 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计文档 `docs/design/m30-request-log-owner-dimensions.md`，规格文档 `docs/request-log.md` §2/§4/§6。
> 起因（用户原话）：「请求日志添加用户,api key 维度统计」。
> 口径当场澄清并选定：**「用户」= 账户**（`accounts` 表）——API 请求只携带凭据，
> `api_key_id → account_id` 是唯一可归因的身份；门户用户与 API Key 没有绑定，今天无法判定。

- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart`，让 8088 应用迁移
  0009 并载入新控制台资源，然后硬刷新（Ctrl+Shift+R）看 http://127.0.0.1:8088/admin/ui/#/requests
  ——确认两列/两个下拉/两个分组/详情里的用户与 Key，并发一条真实 DSH 请求核对显示的是名字

- [ ] 观察项：控制台下拉只列前 1000 个 Key（配置类列表的既有上限）——若出现超过该规模的部署，
  按设计文档 D2 的路径给 `/keys` 加分页搜索，而不是把名字落到日志行上

## M31 请求日志页「维度统计」卡片置顶 + 排序 + 分组列表分页
> 本节已完成的 11 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

> 设计文档 `docs/design/m31-request-log-stats-pagination.md`，规格文档 `docs/request-log.md` §4/§6。
> 起因（用户原话）：「将后台请求日志页面的维度统计卡片放上面，并且列表要加分页」。
> 口径当场澄清并选定：「列表」=「维度统计」表（「请求日志」列表在 M24 已有服务端分页）；
> 排序补充确认为**默认按最近一次请求时间降序**，并保留可切换的排序入口（服务端 `sort` 参数）。

- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart`，然后硬刷新
      （Ctrl+Shift+R）http://127.0.0.1:8088/admin/ui/#/requests ——确认统计卡在列表之上、
      排序下拉能切换（表头 `↓` 跟着走）、分页器写「共 N 个分组」而不是「共 N 条」

- [ ] 观察项：`requests`/`metered` 是**上游尝试**计数（LEFT JOIN 后的 `COUNT(*)` / `COUNT(u.request_id)`），
      failover 多 attempt 的请求会被计两次（本机库当前 0 例）。要收口就改成
      `COUNT(DISTINCT r.request_id)`——会引入 temp B-tree，改前先实测

- [ ] 观察项：大窗口下分组计数的耗时（60k 行实测 2.4 ms）。若某天超过聚合耗时的 1/3，
      按设计文档退回「只有 `has_more`」的形态（`pager` 已支持 `total == null` 显示「还有更多」）

- [ ] 观察项：「按分组名排序」未做：凭据维度按 id 转文本分组，字典序会把 `10` 排在 `2` 前面。
      要做需先把 id 数值化（`CAST(... AS INTEGER)`），并让分组与排序用同一套键

## M32 控制台「智能问答」+ 私有技能库 + 图表与 HTML5 预览
> 本节已完成的 59 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

设计：`docs/design/m32-console-smart-chat.md`；使用者规格：`docs/chat.md`（均先行落盘，见 `docs/PROCESS.md`）。

### 测试与验收
> 本节已完成的 13 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待人工执行**（宿主终端）：硬刷新
      http://127.0.0.1:8088/admin/ui/#/chat ——在真实部署上用有余额的账户选模型提问，
      确认回答流式出现、图表与 HTML5 预览可打开、`console` 出现在请求日志

- [ ] 观察项：不限制步数意味着一次点击的花费上限由模型的步数决定。若某个账户余额敏感，
      给它的部署设 `max_steps`/`max_tool_calls` 正整数上界

- [ ] 观察项：工具循环的端到端（模型真的发起 `admin_request`）目前由 `internal/chat` 与
      `internal/httpapi` 的测试用脚本化 runner/假上游覆盖——内建 `testecho` **不会**发起工具调用，
      真机验证需要接一个会调用工具的模型

### M33：智能问答改为 MCP 客户端（按令牌 scope 执行）
> 本节已完成的 12 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待人工执行**（宿主终端）：硬刷新
      http://127.0.0.1:8088/admin/ui/#/chat ——新建会话时选一个 `admin` scope 的令牌，
      在智能问答里建账户/发 Key，确认账户出现在账户页、明文 Key 只返回一次并带提示、
      `chat_tool_calls` 有记录、审计 actor 为 `mcp:<令牌名>#<id>`；再撤销该令牌，
      确认同一会话的下一次提问明确失效

- [ ] 观察项：明文凭据（API Key / MCP 令牌）现在会落进 `chat_tool_calls.result` 与会话记录。
      当前只在响应里追加一次性提示，不做脱敏——静默改写会让「已落盘」这一事实不可见。
      若日后要收紧，应做的是「返回后可选地从转录中清除」，而不是悄悄改内容

## M34 智能问答的可交互 HTML5 界面（表单 → 提交 → 模型继续）
> 本节已完成的 45 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

设计：`docs/design/m34-ui-bridge.md`；规格：`docs/chat.md` §4（可交互的界面）、§8（配置）、§9（排障）。

### 测试与验收
> 本节已完成的 8 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待人工执行**（宿主终端）：硬刷新 `http://127.0.0.1:8088/admin/ui/#/chat`，用真模型要一次
      "需要用户选择的信息"（例如"帮我建一个账户，先问我账户名和限额"）→ 确认模型输出表单 →
      点「预览（可交互）」→ 填写提交 → 确认工具栏计数 +1、会话里出现带 `ui_event` 的新提问、
      模型继续执行；再确认请求日志里这一条是 `client=console`、正文为空

- [ ] 观察项：界面提交与手动提问走同一条计费路径，所以**一次预览里的连续提交会连续计费**。
      限流（1.5s / 40 次）是控制台侧的上界，不是服务端强制的；若某个部署需要硬上界，应加服务端
      配额而不是依赖控制台

- [ ] 观察项：`chat.ui_bridge_enabled` 是**部署级**开关，不是"关掉模型写表单的能力"——模型仍然
      可能输出表单，只是提交没有出口（工具栏会说明）。文档已按此口径描述

---

## M35 内联声明式表单（气泡内渲染 = 提交 = 模型原地更新）
> 本节已完成的 17 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

起于一个实测结论：M34 的**流式回灌整条链路是死机制**——服务端注入脚本把增量写进页面的
`[data-aigw-live]` 节点，而这个标记在给模型的契约、文档与测试里**一处都没有**（全仓库仅有实现处
与它的自动 dump）。增量帧确实送达页面、然后被静默丢弃，harness 的断言只检查"`d` 帧到了端口"，
所以它一直是绿的。这是"能力存在、契约没写"的典型，M35 顺带把它变成一条机械化的检查。

方向经用户确认：**先只做内联声明式表单这一半，把 iframe 那条链路暂时冻结**。

### 测试与验收
> 本节已完成的 5 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待人工执行**（宿主终端）：用真模型要一次"需要几个字段的信息"（例如"帮我建一个账户，
      先问我账户名和限额"）→ 确认模型输出 `form` 而不是整页 HTML → 填写提交 → 确认表单显示
      「模型正在处理…」、状态行实时出现回答、结束后表单原地更新、会话里出现带 `ui_event` 的提问；
      再到「请求日志」确认这一条是 `client=console`、正文为空

- [ ] 观察项：内联表单与手动提问走同一条计费路径，所以表单上的连续提交会连续计费。一轮一次的
      禁用与 5 条排队是控制台侧的上界，不是服务端强制的

- [ ] 观察项：模型可能该用内联表单时仍输出整页 `html`（或反之）。契约里写了选择口径
      （"要几个字段就用表单；要自由排版或页面脚本才用 html"），但**模型是否照做只能人工走查**

## 修：`＋` 菜单技能行的勾选框被撑满整行 + 勾选技能后留空也能直接发送
> 本节已完成的 12 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

现象（用户报）：智能问答输入框 `＋` 菜单里，技能勾选框的**文字离勾选框很远**；并且希望
**加载技能后文本留空也能直接发送**。

### 测试与验收
> 本节已完成的 4 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] 观察项：菜单勾选一个技能后会关闭（`setSkills` → 重载会话 → 重渲染），多选需要再点一次 `＋`。
      本次没改这个交互（不在用户要求内）；若要做"菜单内多选 + 一次运行"，需要把技能的 PATCH 做成
      乐观更新而不是整页重渲染

## M38 会话粘性路由 + 授权范围内故障转移
> 本节已完成的 22 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

同一个客户端会话的连续请求，在加权随机下会在同一层的多个供应商之间来回跳：前缀缓存命中率被稀释，
上游账号侧看到的"一个会话"也被打散。M38 把"上一次真正服务成功的 route"与该会话绑定，
并明确一条边界：**粘性只重排已授权的候选，永远不放宽授权**。

### 观察项 / 待人工执行
> 本节已完成的 1 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待人工执行**（宿主终端）：用真实客户端（DSH 或 Codex）对同一会话连发 3 轮以上，到「请求日志」确认同一
      `session_id` 的候选供应商保持一致。
      **注意**：部署当天的 4 条路由是"一模型一供应商"（`e2e-echo`/`gpt-5.6-luna`/`gpt-6-astra`/`deepseek-flash`
      各 1 条），此时同会话本来就只会落到同一家，所以这条检查现在**证明不了粘性**；
      要真正验收，得先给某个模型加上**同层第二供应商**（第二个 codex 订阅号或第二个 deepseek key），
      再看两个供应商之间的会话是否各自稳定。可观测口径：`/stats` 的 `affinity.hits` 增长 + 请求日志里
      同一 `session_id` 的 provider 不变

- [ ] 观察项：粘性只在**层内**生效，所以"主供应商 × 备用层"的部署里，一次失败转移**不会**改变下一次的首选——
      坏路由改由既有熔断（60s 窗口 5 次失败）与冷却剔除。这是刻意的取舍（见设计文档 D4）；
      若某个部署希望"一次失败就长期走备用"，需要把 `promoteWithinTier` 改成整体提升

- [ ] 观察项：会话恢复（DSH `--continue`）会换一个 `prompt_cache_key`，因此旧粘性不会被继承。
      这符合"会话 = 客户端给的键"的定义，但运营者若按 `workspace` 期待粘性，会发现不生效

## M40 MCP 工具说明的完整性 + 智能问答优先用表单
> 本节已完成的 49 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

设计：`docs/design/m40-tool-descriptions-and-forms.md`；标准：`docs/mcp.md` §4.5。
触发点是一次真实失败：运维让 agent 给 `codex-sub` 的 `gpt-5.6-luna` 配成本价，
模型查完 `admin_list_models`/`admin_list_routes`/`admin_describe(admin_upsert_provider_model)` 后
**拒绝写入并要求管理员补文档**——因为 `pricing_rules` 只被标为 `{"type":"object"}`、示例是 `{}`，
而写入侧 `pricing.ParseRuleSet` 是 `DisallowUnknownFields`。模型没错，说明不完整。

### 观察项（本轮不做，如实记录）

- [ ] `accounts.price_overrides` 无读取方，而 `docs/pricing.md` §3 把它写进售价来源优先级——两者需要收敛

- [ ] 模型级 `models.policy_json` 与路由级 `routes.policy_json` 同样只存不读

- [ ] tag 的 `policy` 未走 `keyPolicyDocument` 校验（Key 的 policy 走），因此 tag 上的未知字段不会被拒

- [ ] **试算器与数据面的倍率口径不一致**（M40 走查时发现，未修）：`admin_simulate_pricing` /
      `POST /pricing/simulate` 用 `(sale==nil || sale.MarkupBP == 0) → default_markup_bp` 推断加价，
      看不到 Key/tag/账户级 `margin_bp`；而数据面走 `billing.ResolveMarkup` 的完整优先级链。
      于是"某把 Key 设了 `margin_bp: 0`"时，试算器显示 1.0× 而实际按 0 计费。
      影响面：只影响预览数值，不影响计费。修法需要接口新参数（key/tag/account），属接口变更，另开里程碑

## M52 aigw 后台“启用 DSH / 停用 DSH”（账号级 dsh 开关）

设计：`docs/design/m52-dsh-enable.md`（已实现，差异与验收步骤见 §8/§9；完成项已归档 todo_done.md）。
定位：accounts 增 `dsh_enabled` 真值；新增 `POST /v1/dshgw/authorize`；dshgw 增 `dsh_enforce`
（login/interval/per-request）执行点；后台账号列表按钮 + 审计。不动 dsh 发行包、不做自动建户。

- [ ] 主机验收（rev2）：root 配置 `admin_socket`/`admin_allowed_uids` 并启动 `dshgw-admin.service`、重启 aigw 与 dshgw 后：后台启用→自动建租户+worker，账号下旧 Key 与新建 Key 均可登录并跳转租户端口；停用→worker 停止、新登录拒绝、内部 Key 吊销、数据保留；重启用→worker 恢复、新 Key 生效；A/B 互不影响；审计无 Key 明文
- [ ] 浏览器人工走查账号列表 DSH 徽标与启用/停用对话框（租户名预填/可指定）
- [ ] 决策：DSH 上游限制（域名访问时设置/模型目录视图不可用，上游 0.1.2-rc.1 设计）——A 接受并文档明示（推荐，保持零垫片）或 B 网关注入 __DSH_TRANSPORT__ 垫片（突破 D5，不推荐）；确定后回填规格与用户指引
- [ ] M52 提交（独立于 M51 commit；发布/版本号决定由用户另议）

## M51 dshgw 多租户 dsh 网关（写进本仓库、与 aigw / dsh 双向解耦）

设计：`docs/design/m51-dshgw.md`；规格：`docs/dshgw.md`；部署：`deploy/dshgw/README.md`。
定位：独立 `cmd/dshgw` 与 `internal/dshgw/**`；只走 HTTP/CLI 外部契约，不改 aigw 核心或 dsh 发行包。
已完成实现与无特权自动化的条目已移入 `docs/todo_done.md`；以下仅列尚未验收/收尾项。

### 主机验收与收尾（不以 mock 或 loopback 代替）

> 2026-09-16：用户决定 M51 先提交、发布另议；后续产品方向为 M52（aigw 后台“启用 DSH/停用 DSH”，设计稿 docs/design/m52-dsh-enable.md）。下列未完成验收不再阻塞提交，但未完成的仍保持未勾选，不得视为已通过。

- [ ] 实际浏览器退出复验：退出返回门户后，访问B租户应重新要求登录；随后重新登录仍可自动跳转（登录的Origin与CSP两阶段修复已确认）
- [ ] 真实 Key 轮换热载（A停用后模型401且key_revalidate:off保留既有UI会话已由用户验收；同一DSH/PID dummy-Key热载已自动验证，真实新Key热载仍待验收）
- [ ] 从外部机器验证门户及租户端口的 TLS/防火墙可达（宿主回环 TLS 与实际 handshake 文件 owner/mode 已通过 baseline；回环结果不代表外部可达）
- [ ] 10–30 worker 的实际 RSS/cgroup、单实例与汇总资源限额命中；不能把 unit 文本验证当作资源实测
- [ ] 备份恢复主机演练与浏览器人工授权本机目录走查（模板供应、client HTTP200、普通 WS426/upgrade101 已自动验证）
- [ ] 可选：有 C 编译器后运行 CGO_ENABLED=1 go test -race ./internal/dshgw/... ./cmd/dshgw；本机缺 gcc，普通并发回归已通过
- [~] 收尾与提交：设计/规格状态回填为“已提交（M51，剩余主机验收项见 docs/design/m51-host-acceptance-remaining.md，部分被 M52 方向取代）”；单一 M51 commit 引用设计文档；不升 VERSION、不打 tag、不发布

## M53 请求日志的供应商维度（按供应商统计成本）

设计：`docs/design/m53-request-provider-dimension.md`；规格：`docs/request-log.md` §2/§4。
起因（用户原话）：「修复同样的模型，不同的供应商，没有办法按供应商统计成本」。
口径当场澄清并选定：**按计量行归属，谁服务的算谁的**；范围选定「维度统计 + 供应商筛选 +
列表列 + 详情 + MCP」。实现与自动化验收已完成的条目见 `docs/todo_done.md` 同名小节，
下面只列尚未验收与观察项。

- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart`，硬刷新
      http://127.0.0.1:8088/admin/ui/#/requests ——确认列表出现「供应商」列（失败转移的行显示两家）、
      筛选栏出现「全部供应商」、统计卡选「供应商」能按名字 + `#id` 拆出成本，且「请求数」表头
      写明各分组之和可能大于窗口总数

- [ ] 观察项：`group_by=provider` 与 `provider_id` 过滤**恒走原始扫描**（小时汇总按请求预聚合、
      不含供应商，汇总的「请求数」也正是 M31 观察项里那个按尝试计数的问题）。大窗口下的耗时未实测；
      若 p95 超标，需要新建一张按 (hour, request, provider) 聚合的汇总表——与现行 rollup 的请求粒度
      不同，属独立设计

- [ ] 决策（未做）：不带账号作用域的 MCP 用量工具 `get_usage_breakdown` 是否支持 `group_by=provider`。
      给客户自己的 MCP token 暴露供应商 id/名字与数据面「不回供应商标识」的立场相冲突，
      要做先定口径（只给 id？只给平台内部自定义名？）

## M54 控制台资源形态的运行态自述与部署产物隔离

设计：`docs/design/m54-console-asset-shape.md`（§9 差异、§10 实测已回填）；实现与自动化验收完成的条目见
`docs/todo_done.md` 同名小节，下面只列尚未执行项。

- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart`，让 `:8088` 上的 `/version`
      出现 `ui` 字段（`local-run.sh status` 应打印 `console: minified`）。本沙箱与宿主不同 PID namespace，
      无法向宿主进程发信号；在跑的仍是 `0.16.0/9dc4ed2`（早于 M54），所以现在查 `ui` 会得到 `unknown`


## M55 控制台资源的传输层压缩（gzip sidecar + Content-Encoding 协商）

设计：`docs/design/m55-console-transfer-compression.md`（§10 差异、§11 实测已回填）；实现与自动化验收
完成的条目见 `docs/todo_done.md` 同名小节，下面只列尚未执行项。

- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart`，让 `:8088` 同时带上 M54 的
      `ui` 与 M55 的 `ui_encoding` 字段，并真的按 `Accept-Encoding` 发 gzip
      （`curl -sD- -o /dev/null -H 'Accept-Encoding: gzip' http://127.0.0.1:8088/admin/ui/js/pages/chat.js`
      应出现 `Content-Encoding: gzip`，`local-run.sh status` 应打印 `console: minified · transfer: gzip`）。
      本沙箱与宿主不同 PID namespace，无法向宿主进程发信号
- [ ] 决策（另立）：是否做 brotli——需新增 Go 依赖，相对 gzip 约再省 15%，但 sidecar 集合与协商表都要翻倍；
      若边缘/nginx 已能压缩静态资源，该项收益还需重新论证


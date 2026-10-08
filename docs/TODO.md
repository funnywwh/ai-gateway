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

- [ ] 暂不纳入：内置 `openai-responses` 接 DeepSeek `/responses` 剩余两处缺口（`output_tokens_details.reasoning_tokens` 未映射成 `reasoning` 计费维度、思考正文承载字段）。**第三处（`response.reasoning_text.delta` 事件名）已于 2026-09-18 修好**，见 `docs/deepseek-responses-thinking-stream.md` 与 `scripts/responses-thinking-smoke.sh`

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

- [ ] 观察项（2026-09-17，v0.17.0 发布时发现）：**SIGTERM 停机时审计队列在宽限内排不空**。
  实测日志：`msg="shutting down"` → 15 秒后 `level=WARN msg="graceful shutdown incomplete"
  err="context deadline exceeded"` + `level=ERROR msg="audit write queue could not be drained"
  err="store: log writer drain timed out with work left: context deadline exceeded"`（库 5.4 GB，
  当次由沙箱回收触发 SIGTERM）。现象是**停机慢**而不是丢数据（未排空的行按 M26 的兜底留在队列/重试路径，
  下次启动的 `batching_*` 指标可核对），但"关服务要等十几秒且仍然超时"本身值得单独看：
  是 drain 的实现问题，还是宽限期对真实库偏短。判据：在 8088 上发一次 SIGTERM，量 `shutting down`
  到进程退出的耗时，并核对重启后 `request_log.batching` 的丢弃/重试计数是否为 0

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

**2026-09-28 补记**：整页预览的通道从 M34 起就没通过，一共四处，界面上的表现都是「不可交互」：
握手把帧窗口提前读成 `null`；`state.preview` 槽里放的是 Promise 而不是句柄；**注入脚本在
`window.top` 上判断"我是不是嵌套框架"，而预览本来就是 iframe，于是每次都在第一句就返回**；
**端口两侧都在等对方建，谁都没有调用过 `MessageChannel`**。四条都已修，"浏览器里那条 port 真的接通"
是自动断言（`chat` 视图 live 阶段），不需要浏览器的那一半由
`chat_preview_handshake_test.mjs`（`make verify` 内，**两侧一起跑**）钉住。详见设计文档「决策 2c / 2d / 2e」。

### 测试与验收
> 本节已完成的 8 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待人工执行**（宿主终端）：硬刷新 `http://127.0.0.1:8088/admin/ui/#/chat`，用真模型要一次
      "需要用户选择的信息"（例如"帮我建一个账户，先问我账户名和限额"）→ 确认模型输出表单 →
      点「预览（可交互）」→ **徽章停在「已连接」** → 填写提交 → 确认工具栏计数 +1、会话里出现带
      `ui_event` 的新提问、模型继续执行且回答原地更新页面；再确认请求日志里这一条是
      `client=console`、正文为空。
      注意控制台资源内嵌在二进制里，**先重新构建并重启**再看，否则你测的还是旧 JS
      （`make build && scripts/local-run.sh restart`；harness 走查用 `make ui-check`）

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

设计：`docs/design/m54-console-asset-shape.md`（§9 差异、§10 实测已回填）；实现、自动化验收与
`:8088` 部署验证均已完成，条目见 `docs/todo_done.md` 同名小节（含 v0.17.0 发布记录）。本节无未完成项。


## M55 控制台资源的传输层压缩（gzip sidecar + Content-Encoding 协商）

设计：`docs/design/m55-console-transfer-compression.md`（§10 差异、§11 实测已回填）；实现、自动化验收与
`:8088` 部署验证均已完成，条目见 `docs/todo_done.md` 同名小节（含 v0.17.0 发布记录）。本节未完成项：

- [ ] 决策（另立）：是否做 brotli——需新增 Go 依赖，相对 gzip 约再省 15%，但 sidecar 集合与协商表都要翻倍；
      若边缘/nginx 已能压缩静态资源，该项收益还需重新论证


## M56 供应商成本上限与复位

设计：`docs/design/m56-provider-cost-cap.md`（§8 差异与实测已回填）；实现与自动化验收完成的条目见
`docs/todo_done.md` 同名小节，下面只列尚未执行项。

口径（用户原话「供应商成本要能设置上限，可以复位」当场确认）：按供应商累计**我们付给上游的成本**
设上限 + 可复位；周期每供应商可选（不限/每天/每月）；达到上限后从**路由候选**中剔除。

- [ ] **待人工验证**（需要管理员会话 + 真实供应商配置）：在控制台给一个真实供应商设一个极小的上限
      （例如 `1` 微单位），发一次请求确认它被剔除并落到别家（`/admin/ui/#/requests` 的供应商列能看到换了家）；
      点「复位成本」后请求恢复。v0.17.0 已部署到 `:8088`（`/version` = `0.17.0/c8df8b8`、迁移 21 已应用、
      6 个供应商读回 `0/none/NULL` = 不限），但**没有**动线上任何供应商的上限——改配置不是发布的一步
- [ ] **待人工验证**：在 `:8088` 上做一次"周期"验证（把某供应商设成 `daily`/`monthly`，确认 UTC 零点/月初
      读数自动重新起算；读数最多滞后 5 秒）


## M57 dshgw 严格租户隔离（bubblewrap 模式）

**已由 M58 取代**：隔离机制（bwrap profile、空 tmpfs 根、逐路径绑定）保留并成为唯一模式；
原来的"user / bwrap 双模式 + `tenant re-isolate` 迁移"随 root 特权面一起删除。
本节原有待办（user 模式回归、两种模式互迁）不再适用，剩余工作见下面的 M58。

## M58 aigw 监督的 rootless dshgw（同目录、启动时拉起）

设计：`docs/design/m58-aigw-supervised-dshgw.md`；部署：`deploy/dshgw/README.md`。
代码、单测、真实 bwrap staging 与本机端到端已完成，**尚未在宿主部署**。

已完成（可复现）：

- [x] aigw 生成子进程配置、按同目录规则拉起 dshgw、等 ready、随自身退出停掉它
- [x] dshgw 以 bwrap 子进程管理租户 worker（无 systemd、无 per-tenant 账号、无 root）
- [x] 租户生命周期经同 UID admin socket；`suspended` 取代 systemd enablement；启动时自动恢复未停用租户
- [x] worker 启动前自动同步租户模型（401/403 拒启；aigw 不可达则告警后用现有清单启动）
- [x] 删除旧形态：install.sh、6 个 systemd 单元、nginx 渲染、requireRoot、root admin 通道、
      每租户 OS 用户、`tenant re-isolate`、`upgrade-dsh`、root 宿主验收脚本
- [x] 本机无特权端到端：建户 → bwrap worker → `/api` 401 → stop/start → aigw 重启自愈 → 停止无残留

未完成：

- [ ] **宿主迁移（脚本已就绪、等待执行窗口）**：`scripts/migrate_dshgw_to_supervised.sh`
      （`--apply`/`--rollback`，均先 `--dry-run` 审阅；计划有自动化测试）。本机检测到 6 个旧
      worker 单元 + `dshgw.service`。执行需要 root 与维护窗口：会停这些单元、改三棵目录树的属主、
      按 registry 改写租户标记，然后逐个租户探测 `/api` 是否恢复。旧单元只 disable 不删除，
      确认后再手工清理 `/opt/dshgw`、`/etc/dshgw` 与 `dsh-*` 账号
- [x] 公开面：dshgw 自己绑定门户端口与每个租户公开端口（无 nginx），edge 头由进程内注入且覆盖
      客户端伪造；`dshgw.tls_certificate` / `tls_certificate_key` 可启用 HTTPS
- [x] **开机自启**：`scripts/aigw_user_service.sh` 生成并启用用户级单元 `aigw-local.service`
      （本机已切换为文件单元、`enabled`、`linger=yes`；实测切换只造成秒级中断）
- [x] **单域名入口反代** `bin/gwproxy`（`make gwproxy-build`）：一个端口按路径前缀分开
      aigw(`/aigw`，前缀原样转发)/门户(`/dshgw`)/租户 dsh(`/t/<t>`)；租户映射读 dshgw 的 registry；
      对 dsh HTML 只做标签内根绝对引用的窄改写（dsh 无 base-path 选项，实测其资源为相对路径）。
      单测覆盖路由/拒绝/窄改写/config 校验，并对真实 aigw 端到端验证过 `/aigw/version`
- [x] **路径模式（不能分配子域名时的方案）**：dshgw 增加 `public_base_url` / `tenant_path_prefix` /
      `portal_path_prefix`，公开 URL 与会话 cookie Path 都变成路径式；反代 `/dshgw/`、`/t/<t>/` 实测打通
      （验收 24 步：门户 200、租户路径 302 回门户路径、`POST .../api` 401）
- [x] **本机部署验证**：`gwproxy-verify`（0.0.0.0:8090）+ `dshgw-verify`（路径模式）两个常驻用户单元；
      实测 `/aigw/version` 经反代拿到真实 aigw 版本、门户 200、租户路径 302 回路径式门户、`POST .../api` 401、
      注入会话后 `/t/verify1/` 200 且资源经前缀可取（423KB bundle）、worker 限额落在
      `dshgw-worker-verify1.scope`；`:8088` 直连与旧 `dshgw.service` 全程未受影响
- [x] **真浏览器验证与结论：dsh UI 必须独占 origin**（headless chromium + CDP 实测）：
      端口模式下 UI 正常渲染（30 请求全 200、零异常）；路径模式下 shell/资源都 200 但应用自身
      的 `location.origin` + `/api` 调用落到域名根 → 白屏。结论：路径前缀只适用于门户，
      无子域名时多租户只能"同一域名 + 每租户一个端口"，域名做前门跳转（`portal_redirect`/`tenant_redirect`）
- [ ] **剩余可选**：① 若只有一个租户，可把租户放在域名根路径（零改写、单端口）；
      ② 若要真正做多租户同端口，需要改写 dsh 客户端 bundle（生成物、随升级而变，不推荐）
- [x] **查明「设置/模型」面板报错**（实测 + 代码）：dsh 只在 **loopback 页面**启用 host 持久化
      （`persistence = isLoopback ? "host" : "memory"`），非 loopback 页面该面板必然报
      "settings are unavailable in this browser"。同租户对照：`127.0.0.1` 页面正常、
      `192.0.2.101` 页面报错。**模型本身可用**（清单由 dshgw 按账号授权自动配置）。
      文档见 `deploy/dshgw/README.md` §10（含三条可选应对）
- [ ] **上游反馈（可选）**：若希望 LAN 页面也能改设置，需要 dsh 提供"受信公网 host 视同 loopback"
      的开关（客户端 `transport.ownsHost` 目前是留白）——可向上游提需求，而不是本地打补丁
- [ ] **对外暴露的剩余决策**：防火墙策略（worker 段端口必须不可达）、是否仍在前面放 nginx 反代
- [x] **资源限额**：每 worker 一个 systemd 用户 scope（`systemd-run --user --scope -p MemoryMax=…`），
      实测 `memory.max`/`pids.max`/`cpu.max` 全部生效；部署级汇总上限通过
      `scripts/aigw_user_service.sh --memory-max/--tasks-max/--cpu-quota` 写进单元属性。
      设计取舍：不用自建子 cgroup —— cgroup v2 的"无内部进程"规则使服务 cgroup 无法下放控制器（本机实测）
- [x] 宿主验收脚本：`scripts/dshgw_supervised_e2e.py` + `make dshgw-supervised-test`（17 步：
      aigw 拉起子进程 → 同 UID admin socket → 建户 → bwrap worker → `/api` 401 → 无 per-tenant
      账号 → stop/start（含启动前模型同步）→ aigw 重启自愈 → 停止零残留；缺 bwrap/dsh 时自我跳过）
- [x] 历史设计文档标注：`m51-dshgw.md`、`m52-dsh-enable.md`、`m51-host-acceptance-remaining.md`
      文首加"已被 M58 取代（部署形态）"说明，保留历史记录本身

### M58 验收脚本的边界（写清楚，避免被当成"全部验收已过"）

`scripts/dshgw_supervised_e2e.py` 证明的是**本机、单租户、无特权**的那条链路；它**不**声称：

- 公网可达性（TLS/防火墙/端口暴露）——当前形态没有 nginx，证书终止尚未设计；
- 多租户并发与资源压测（也没有 cgroup 限额可压）；
- 租户之间的实际越权尝试（跨租户端口/cookie/文件）——这部分由 `internal/dshgw/proxy` 的回归测试
  与 `make dshgw-sandbox-test` 的宿主隐藏断言覆盖，但都不是"真实浏览器 + 真实网络"的验收；
- 宿主重启后的自愈（当前是用户级 transient 单元，重启机器需要手动或落一份常驻 unit）。

## M60 aigw 的 API Key 飞书绑定与解绑

设计：`docs/design/m60-aigw-key-feishu-binding.md`（§6 差异已回填）；规格：`docs/feishu.md`。
定位：`api_keys` 上加飞书身份（`open_id` 1:1 唯一）、控制台 API Keys 页绑定/解绑、管理 API 与 MCP 解绑工具、
飞书 OAuth（`/feishu/login` + 唯一回调 `/feishu/callback`，两种 flow）；绑定不参与数据面鉴权。

- [ ] **真机验收**（需要飞书自建应用的 app_id/app_secret 与已登记的 `http://192.0.2.101:8090/feishu/callback`）：
      控制台绑定 → 列表显示姓名 → 对该 Key 做一次「编辑」(PATCH) 后绑定仍在 → 解绑 →
      同一飞书账号绑第二把 Key 冲突且不覆盖 → 日志与审计中 `grep -cE 'access_token|app_secret'` = 0
- [ ] **飞书后台输入框是否接受 IP 形式的重定向 URL**（官方文档允许 http 与非 443 端口，但未明确 IP）：
      先试存；若被拒，回退方案是给 gwproxy 开 TLS（`*.example.net` 私钥当前账号可读）并把 `callback_url`
      改成 `https://chat.example.com:8090/feishu/callback`（只改配置，不改代码）

## M61 dshgw 门户飞书登录（消费 aigw 的票据）

设计：`docs/design/m61-dshgw-feishu-login.md`（§6 差异已回填）；规格：`docs/feishu.md` §5。
定位：aigw 出一次性票据（HMAC），dshgw 验票后下发既有会话 cookie 进租户；dshgw 不持有飞书凭据、
不注册第二个回调、不需要出站访问飞书。实现与自动化验收（含监督形态端到端 15 步）完成的条目见
`docs/todo_done.md` 同名小节，下面只列未完成项。

- [ ] **真机验收**（需先完成 M60 的真机步骤）：门户「飞书登录」进入自己租户；未绑定账号被拒并看到明确提示；
      控制台「停用 DSH」后飞书登录被拒、重新启用恢复；A/B 两账号互不影响
- [ ] **（可选，后续）客户门户自助绑定**：`internal/portal` 的 portal 用户与 API Key 目前没有绑定关系，
      要先设计那层关联，才能让使用者自己绑定/解绑而不是找管理员

## M62 绑定飞书即自动启用 DSH

设计：`docs/design/m62-feishu-auto-enable-dsh.md`（§6 差异已回填）；规格：`docs/feishu.md`。
定位：绑定成功即调用与「启用 DSH」按钮完全相同的供应流程（只在账号从未启用过时），
供应失败不回滚绑定。实现与自动化验收完成的条目见 `docs/todo_done.md` 同名小节。

- [ ] **真机验收**：控制台绑一把从未启用过 DSH 的账号的 Key → 控制台提示"已自动启用（租户 …）"
      且账户页显示已启用 → 直接门户飞书登录进入该租户；再绑一个曾被停用的账号的 Key → 提示
      "曾被显式停用，未自动启用"且账号保持停用

## M63 单一数据根（数据默认落在 ./data）

设计：`docs/design/m63-data-root.md`；规格：`docs/deployment-layout.md`。
定位：把所有运行态数据的默认值收进**部署根**的 `./data`（aigw 库/日志/pid/备份/插件状态 + dshgw 的
state/template/tenant/workspace/backup），配置留在部署根，运行时安装（node/dsh/bwrap/证书）保持绝对路径；
同时把本机验证栈搬进 `./data`、把历史二进制归拢到 `./data/prev`、把遗留 root 形态归档下线。
实现与自动化验收完成的条目见 `docs/todo_done.md` 同名小节。

- [ ] **待宿主执行**（需要交互式 `sudo`，本会话无法执行）：遗留 root 形态的归档下线
      `sudo scripts/decommission_legacy_dshgw.sh --apply`（计划已在本机 dry-run 复核：归档 3 棵树 +
      nginx 转发 + 旧单元文件 → 停 8 个单元 → 校验归档含 `registry.json` → 删 `/opt/dshgw`、`/etc/dshgw`、
      `/var/lib/dshgw`）。注意它会停掉旧形态里的真实租户 **K7QX**（`dsh_tenant=dsh-k7qx`），
      该租户的数据只留在 `data/prev/legacy-dshgw/`，需要时按归档重建
- [ ] **可选**：`--remove-accounts`（删 `dsh-*` 账号与家目录）默认不执行，确认无其它用途后再单独跑
- [ ] **可选（未做）**：把本机独立 dshgw 收进 aigw 监督形态（`dshgw.enabled: true`）。收益是少一个单元、
      真正"一个部署"；代价是 aigw 每次重启都会带走全部 DSH 会话，故未纳入本次范围
- [ ] **可选（未做）**：`data/backups` 的保留期策略（当前 12G，主库 5.8G）与 `data/aigw.db`
      （旧示例库）的去留，另立话题
- [x] ~~待定：本次未发版~~ → 已于 2026-09-18 发 **v2.0.0** 并部署本机（major，用户确认；记录见 `docs/todo_done.md`）

## M64 aigw 账号的 SSH 工作区（远端目录 → 挂载 → 该账号 DSH 里的工作区）

设计：`docs/design/m64-ssh-workspace.md`（§3 阶段 0 实测、§13 差异、§14 真机验收与它抓到的四个缺陷、
§15 别名改成一账号一份 +「我的主机」）；规格：`docs/dshgw.md` §7b。已完成的条目见 `docs/todo_done.md`
同名小节，下面只列未完成项。

- [x] **别名改一账号一份：本机部署（2026-09-21，v3.0.0）**：`scripts/ssh_config_adopt.sh` 收编了 6 个
      账号的 `<workspace>/.ssh/config` → `data/dshgw-verify/ssh-configs/<账号>`；`dshgw.yaml` 的
      `ssh_config_source` 换成 `ssh_config_dir: ./data/dshgw-verify/ssh-configs`；`bin/dshgw` 重建并在
      12:20:44 重启 `dshgw-verify`（2.9.1 `b561b0c` → 3.0.0 `f8d20d8`），6 个 worker 全就绪、gw-d 挂载被
      `Reconcile` 重挂、账号 config 一个字节没动（与各自种子 `cmp` 一致）。完整记录见 `docs/todo_done.md`
      的 v3.0.0 小节；后续仍可逐账号裁剪种子并按需重置（改种子 → 删 `<workspace>/.ssh/config` → 重启该
      账号 worker）
- [x] **观察项（v3.0.0 重启时发现）**：ssh 工作区服务自己的日志在现网是丢掉的 —— 已于 2026-09-21 修：
      `cmd/dshgw/runtime.go` 的 `sshWorkspaceService(cfg, manager, slog.Default())`（每个命令形态的进程
      logger，serve 也在内）。此前 `sshworkspace.New` 收到 nil logger 就落到 `io.Discard`，`Reconcile`
      成功重挂了挂载日志里却没有一行；本次事故里「拒绝自嵌套挂载」与 `breakWedge` 的告警同样走这条 logger
- [ ] **「我的主机」浏览器验收（人工）**：租户 A 添加一台主机 → 复核 A 的 `<workspace>/.ssh/config`
      出现该别名 → 选用它「挂载并打开」→ 删除（在用被拒、卸载后成功）→ 别名与该主机专用私钥都消失；
      租户 B 的列表里看不到 A 的主机
- [ ] **监督形态真机验收（`aigw-local.service` + 门户）**：在 aigw 的 `dshgw.ssh_workspaces` 里启用 →
      `systemctl --user restart aigw-local.service` → 经门户进某个账号的 dsh → 侧栏「SSH 工作区」→
      浏览/新建远端目录 → 挂载并打开 → 会话里写文件 → 远端 `cat` 复核（浏览器动作需人工）
- [ ] **跨账号不可见（真机断言）**：账号 A 挂载后，账号 B 的沙箱里 `<A 的 workspace>/ssh/**` 不存在；
      并确认挂载参数里没有 `allow_other`（两账号同 UID，共享挂载即跨账号可读）
- [ ] **缺 sshfs 时拒绝启动（真机）**：把 `sshfs_bin` 指到不存在的路径 → `serve` 必须拒绝启动并点名该
      配置键（CLI 命令只告警，这是刻意的差异，见 §14 第 4 条）
- [ ] **观察项**：FUSE 上 `git status`/`grep` 的耗时基线（文档已声明会慢，但未测量）
- [x] **缺陷（2026-09-20 发布 M68 时两次撞到）：死挂载条目** —— **已由 M76 修**（`Restore`/`Reconcile`
      对已记录的挂载点按"守护进程是否还在"探测，死条目先摘掉再重挂，记录与挂载点保留；本机现网 2026-09-22
      重启时实测自愈）。原文与操作教训已归档到 `docs/todo_done.md` 的 M76 小节

- [x] **自嵌套挂载（2026-09-21 事故，已修）**：租户 dsh-tenant 挂 `gw-c:/home/operator/work/ai_gateway`
      （`gw-c` 的 `HostName` 就是本机 `192.0.2.101`），而挂载点
      `<workspace>/ssh/gw-c/home/operator/work/ai_gateway` 就在这个目录里 —— 挂载树包含挂载点本身。
      一个会话在工作区根上跑 `grep -rn 扫码\|二维码\|qr … .` 之后：FUSE 连接 `834` 上积压 8 个无人应答
      请求，`grep`（`/proc/<pid>/fd` 已指向第二层同一目录）、`ls <挂载点>`、`ls <挂载点父目录>` 三个进程进
      **D 态**（`kill -9` 无效），该租户所有会话同时卡死。现场解救（无需 root）：`echo 1 > /sys/fs/fuse/
      connections/834/abort`（waiting 8→0，D 态进程立即释放，sshfs 守护进程随之退出）→
      `fusermount3 -u -z <挂载点>` → 清 `state/ssh-mounts.json` 与该账号 `ssh-mounts.json` 的这条记录
      （否则下次 `Reconcile` 会把它重挂回来）。**已修**：`internal/dshgw/sshworkspace/selfnest.go` 在
      `mount()` 里拒绝「远端是本机且远端路径是挂载点祖先」的挂载（按设备号+inode 比较，因此
      `/data/home/operator/work` 这个同 fs 的第二个挂载点也认得；地址或 `machine-id` 证明「本机」，
      别的机器上同样路径不受影响，本机上不含工作区的目录照常可挂），错误码 `mount/forbidden`、审计
      `ssh-mount-refused`；同时 sshfs 默认补 `max_conns=4`（配置可覆盖），一条挂载不再只有一个 sftp 通道。
- [x] **宿主目录直挂（M71，2026-09-21 已实现）**：要挂本机目录不再走 sshfs —— `host_shares` 配置声明
      `{name, path, read_only, tenants}`，worker 的 bwrap profile 把宿主目录直接 `--ro-bind-try`/`--bind-try`
      到 `<workspace>/<subdir>/<name>`（容器先只读绑定），没有内核挂载、没有 FUSE、没有可挂死的东西。
      默认只读（写授权要显式 `read_only: false`），`tenants` 必填非空，共享目录与 `state_dir` 必须不相交
      （否则等于把一个账号的工作区/私钥/会话交给另一个账号），配置加载即校验。镜像
      `<dsh_home>/host-shares.json` 给租户面板用（不含宿主路径）。真机 bwrap staging 验收：
      只读共享不可写、可写共享写穿宿主、宿主路径在沙箱内不可见。规格 `docs/dshgw.md` §7e。
      未做：侧栏面板行（当前用目录选择器进 `<workspace>/host/<name>`）。
- [ ] **未做（本次事故的后续）**：②**工作期间的挂死看门狗** —— 现有 `fuse.go` 的 `breakWedge`（杀守护进程
      + sysfs abort）只在卸载路径上跑，正常工作时没有「请求多久没应答」的巡检；③`sshfs -o auto_unmount`
      （守护进程退出即自动摘挂载）能否免掉下面那条「死挂载条目」缺陷；④递归工具（`grep -r`/`find`/索引）
      撞上合法挂载仍然慢
- [ ] **观察项（本次事故实测到的反面基线）**：FUSE 上 `git status`/`grep` 的耗时基线仍未测；已知的是
      自嵌套时不是「慢」而是**永久挂死**（D 态、不可杀）

## M66 控制台多管理员与管理员飞书扫码登录

设计：`docs/design/m66-console-admin-feishu-login.md`；规格：`docs/feishu.md` §5b。
定位：`admin_users` 支持多行（角色 admin/viewer、状态 pending/active/disabled），每个管理员可绑定自己的
飞书身份；控制台登录页多一个「飞书扫码登录」（二维码由飞书授权页提供），新增管理员用**一次性邀请链接**
完成绑定并首次进入控制台，不需要口令。实现与自动化验收完成的条目见 `docs/todo_done.md` 同名小节。

- [ ] **真机验收（需要手机 + 已登记的飞书自建应用）**：用**操作者实际访问的控制台地址**打开控制台
      （本机部署是 `http://aigw.internal:8088/admin/ui/`，即 `feishu.console_url` 的值）→ 在「管理员」页
      给一个账号生成邀请链接 → 在正确身份的手机/浏览器打开 → 扫码或点同意 → 回到控制台即为登录态；
      随后在登出状态下点「飞书扫码登录」再进一次；再把同一链接重开确认提示「已失效」
- [ ] **真机验收（拒绝面）**：用一个只绑定了 API Key 的飞书账号走控制台登录 → 必须看到「尚未绑定任何
      管理员账号」且拿不到会话；把一个管理员「停用」后确认它已登录的浏览器立刻掉线

## M67 租户侧栏的账号行与退出

设计：`docs/design/m67-dshgw-account-card.md`；规格：`docs/dshgw.md` §7d。
定位：租户 dsh 侧栏底部（「SSH 工作区」下面）多一行——飞书名（回退账号名、再回退租户名）与「退出」
按钮；两个数据面是租户 origin 下的 `GET /dshgw/session/` 与 `POST /dshgw/logout/`，开关是 aigw 的
`dshgw.account_card.enabled`。实现与单测完成的条目见 `docs/todo_done.md` 同名小节。

- [x] **接口面真机验收**（2026-09-19 本机，逐条记录见 `docs/design/m67-dshgw-account-card.md` §8）：
      authorize 带 `account`/`feishu_name`；`/dshgw/session/` 200 且 `name` 是飞书名、无 cookie 302；
      `POST /dshgw/logout/` 跨源 403 / GET 405 / 正确 Origin 303 且会话失效；旧租户经 admin 通道回填账号名；
      反例（关开关）端点 404、页面无该 bundle
- [ ] **浏览器人工确认（剩下的一步）**：经门户进自己租户 → 侧栏「SSH 工作区」下面出现「<飞书名> ⏻ 退出」→
      点退出回到门户登录页 → 原租户地址要求重新登录，同一浏览器里另一个租户仍在线；顺带看一眼折叠（rail）
      状态下只留图标的观感

## M68 aigw 供应商的模型参数来自 `/v1/models`（能力 / 上下文 / 最大输出 / 图片 / 推理档位）

设计：`docs/design/m68-aigw-model-capabilities.md`；规格：`docs/dshgw.md` §6a、`docs/api-responses.md`、
`docs/routing.md` §4.1、`docs/api-providers.md` §2。定位：`GET /v1/models` 增补能力字段，dshgw 渲染租户
`settings.yaml` 时按官方文档字段（`contextWindow`/`maxTokens`/`input`/`reasoningEfforts`）写入。

- [x] 实现与单测（清单见设计文档 §4–§8；已完成记录见 `docs/todo_done.md` 同名小节）
- [x] **deepseek 供应商声明图片能力**（2026-09-20，`config.yaml` 被 gitignore，故在此留痕）：
      文件基线两处都写全 —— 供应商 `config.models[]`（插件目录，控制台「刷新发现」用的那份）与
      bootstrap `models[]`（映射行的新建基线）各四条，`capabilities` 补 `image: true`；
      运行态用管理 API 逐条局部更新（`POST /admin/api/v1/providers/3/models`，
      body 只带 `public_model` + `capabilities`，行的 context_window/max_output_tokens/upstream/价格
      原样保留，整改前后逐行读回核对）：`deepseek-flash` / `deepseek-v4-flash` / `deepseek-v4-pro` /
      `deepseek-v4.1-flash` 四条现在都是 `{stream,tools,reasoning,image}`。
      **未包含** `deepseek-aliyun`（id 13，`enabled: false`，只存在于运行态）：它同样有这四个对客名，
      一旦启用，图片请求在 `strip` 下会被标记降级、在 `reject` 下会被该候选过滤——要启用就先给它补声明
- [x] **端到端复验（隔离实例，2026-09-20）**：用 `bin/aigw-src` + `config.yaml` 起一个一次性实例
      （独立临时库/端口 18099，不动运行态），`GET /v1/models` 四条 deepseek 模型都带
      `context_window: 1000000`/`max_output_tokens: 65536`/`input_modalities: ["text","image"]`；
      带 `input_image` 的请求打到声明图片的模型**没有** `X-Gateway-Degraded`，打到未声明的 `replay`
      则有 `X-Gateway-Degraded: image`（文本请求两者都没有）；再用临时 dshgw state 跑
      `bin/dshgw sync-models alice`，产出的 settings.yaml 里四条 deepseek 模型带
      `input: [text, image]` + 全 7 档 `reasoningEfforts`，`gpt-5.6-luna` 等仍只有 `[text]`
      （未声明），`replay` 是 `reasoningEfforts: false`，路由级出现 `maxRequestImageBytes: 7340032`
- [x] **真机验收（2026-09-20 08:53–08:55，本机三单元）**：`make build` 后 `systemctl --user restart
      aigw-local` + `dshgw-verify`（`/version` → revision `64fd34a`）→ 运行态 `GET /v1/models` 六条模型
      带新字段（四条 deepseek：`context_window`/`max_output_tokens`/`input_modalities:["text","image"]`/
      `capabilities`；`stealth/union-alpha`、`u2-flash` 口径为「能力未知」→ 只回 `input_modalities:["text"]`）；
      四个真实租户的 settings.yaml 全部落到官方字段（含 `input: [text, image]`、全 7 档、路由级
      `maxRequestImageBytes: 7340032`），`u2-flash`/`stealth/union-alpha` 只写 `id`/`name`（未知不写），
      控制台资源里也带了新的能力提示。细节与当时的 dsh-tenant 插曲见 `docs/todo_done.md` 同名小节
- [ ] **浏览器人工确认（剩下的一步）**：模型菜单里 deepseek 四个模型出现推理档位、能附图片；
      顺带把 dsh-tenant 的 SSH 工作区重新挂上（见 `docs/todo_done.md` M68 小节的说明）

## M69 登录驱动的租户生命周期与「平台段 / 租户段」设置合并

设计：`docs/design/m69-login-lifecycle-and-settings-merge.md`；规格：`docs/dshgw.md` §3b；
运维说明：`deploy/dshgw/README.md` §12b。
需求（2026-09-21 用户原话）：dshgw 要合并租户手动设置——平台的模型限制用平台的、其他用租户的、
不碰宿主机的；同步要在用户每次登录时发生；用户点击退出要强制退出 dsh 服务。
实现、单测与真机验收（2026-09-21 本机三单元）的记录见 `docs/todo_done.md` M69 小节与 v2.9.1 发布记录。

- [ ] **浏览器人工确认（剩下的一步）**：门户登录 → 租户页直接可用（无 502/长时间白屏）；
      租户侧栏「退出」→ 回到门户登录页且不再占用 dsh 进程。本机验收都是用 HTTP 客户端跑通的，
      还缺一次真人点界面

## M70 飞书通讯录同步（组织架构页「同步飞书」）

设计：`docs/design/m70-feishu-org-sync.md`（§11 差异已回填）；规格：`docs/feishu.md` §5c、`docs/org.md` §3/§5/§6。
需求（2026-09-21 用户原话）：「http://aigw.internal:8088/admin/ui/#/org 右上角添加一个"同步飞书"，
弹出组织机构树和人员，人员可以有"创建用户""绑定账号"操作，合并时同名合并，人员id相同合并」，
随后补充「绑定账号时，弹出账号列表，可以拼音过滤」与「自动按人名匹配，不匹配的用户决定」。
定位：把飞书通讯录（部门树 + 人员）合并进本地的组织节点与账户；一个已确认的「同步」动作 = 补建缺失部门
节点 + 自动合并已匹配人员，人员身份写在 `accounts.feishu_*`（**只是同步映射，不授予登录能力**：门户登录
仍按 M60 的 Key 级绑定判定）。

- [x] 迁移 `0024_feishu_directory_links.sql`：`org_nodes.feishu_department_id/feishu_synced_at`、
      `accounts.feishu_open_id/union_id/name/bound_at/bound_by`，两处 `NULLIF(…, '')` 唯一索引
- [x] 存储：列级读（`accountCols`/`orgNodeCols`）、`BindAccountFeishu` / `UnbindAccountFeishu` /
      `FindAccountByFeishuOpenID` / `ListAPIKeyFeishuIdentities` / `SetOrgNodeFeishuDepartment` /
      `AddAccountOrgNodes`（加性 `INSERT OR IGNORE`）；`UpsertAccount` 与 `UpdateOrgNode` **不写**这些列
- [x] 飞书客户端 `internal/feishu/directory.go`：tenant token 缓存（提前 60 s、被拒后重取一次一次重试）、
      部门 BFS 遍历（父先于子，根 `"0"` 只取成员不算部门）、跨部门同人按 open_id 合一、
      上限（PageSize 50 / MaxDepartments 500 / MaxPages 40）→ `Truncated`、
      名字全空 → `NamesAvailable=false`（当前部署的真实状态：缺两个数据权限）
- [x] 配置：`feishu.tenant_token_url` / `feishu.contact_url`（默认即飞书文档地址）+ env
      `GW_FEISHU_TENANT_TOKEN_URL` / `GW_FEISHU_CONTACT_URL` + https 校验（`config.example.yaml` 已注明）
- [x] 管理接口 5 条（`admin_list_feishu_directory` / `admin_sync_feishu_org` /
      `admin_create_account_from_feishu_user` / `admin_bind_account_feishu_user` /
      `admin_unbind_account_feishu_user`，全部 `role=admin`）：合并规则只写在 `planFeishuOrg` 一处，
      预览与同步共用同一份计划；同步幂等（第二次 0 写入）；未启用飞书 → 400 `unsupported_parameter`；
      飞书失败 → 502 + 中文原因
- [x] 控制台：组织架构页右上角「同步飞书」（只读角色置灰）→ `pages/org_feishu.js` 弹窗
      （左飞书部门树 + 右人员列表，人员/账号两处都支持拼音过滤；未匹配行给「创建用户」「绑定账号」，
      已匹配行给「解绑」；缺名称权限时弹窗顶部明确说明并保留按编号的操作）
- [x] 测试：store（绑定唯一/覆盖/解绑幂等/upsert 不清绑定/打标/加性挂节点）、feishu 目录
      （翻页、BFS 序、同人合一、token 缓存与一次性重试、上限截断、名称缺失、错误分类）、
      httpapi（三条匹配通道矩阵、同名先到先得、缺权限时只合并 id 通道、502、409/404/403/未启用）、
      `internal/webui/tests/org_feishu_test.mjs`（发出去的 URL 与请求体）、ui 夹层三个视图
- [x] **真机预览验收（2026-09-21，v2.10.0 部署后）**：飞书侧的「获取部门基础信息」
      `contact:department.base:readonly` 与「获取用户基本信息」`contact:user.base:readonly` 已生效
      （名称可读），`GET /admin/api/v1/org/feishu/directory` 实测 22 个部门 / 90 人 /
      13 人自动匹配（5 人走 `api_key`、8 人走同名）/ 77 人待决定，首次 15.4 s、60 秒内缓存 1.4 ms；
      `FEISHU_LIVE_CONFIG=config.yaml go test ./internal/feishu/ -run TestLiveDirectory -v` 可复现这条探针
- [ ] **真机「同步」由操作员在控制台点击**（本版没有替用户写库）：它会在线上组织架构里创建 22 个节点、
      给 13 个账户写飞书身份并挂进部门节点。点完要核对：节点层级与部门树一致、5 个 M60 绑过的账号
      身份固化到账户（Key 上的绑定保持不动）、同名账户被合并、**第二次点同步是 0 写入**。
      改动前后可对比 `GET /admin/api/v1/org/nodes?limit=1000` 与 `GET /admin/api/v1/accounts`
- [x] **可选择同步哪些部门（2026-09-21 追加，设计 §12）**：部门树每行一个作用域复选框（含合成根
      「飞书根组织」＝公司层人员），**勾选/取消父部门连同整棵子树一起**（半选表示"这一行与子树不一致"，
      半选不进接口）、另有「全选 / 清空」；
      勾选部门的**上级**自动补建（标「为层级补建」，其人员不在范围内）；人员按**自己的部门**判定范围，
      范围外的行置灰并注明原因；预览与同步都带范围（`departments=` / `department_ids`），
      因此确认框里的数字恒等于服务端计划；空选 400、未知 id 忽略并回报；不传 = 全量（兼容 MCP/脚本）
- [ ] 未做：飞书侧的部门改名/删除**不传播**到本地（设计如此：本地节点与账户只能由人来改）；
      人员离职/停用不自动停账户（飞书 `status` 字段本轮没读）

## M72 账号级飞书身份、组织页整合、多 Key 登录选择

设计：`docs/design/m72-account-feishu-identity.md`（§11 真机验收记录、§12 差异已回填）；
规格：`docs/feishu.md` §1/§3/§4/§5/§5c.4/§5c.5/§6/§7/§8、`docs/org.md` §5、`docs/dshgw.md` §3、`docs/mcp.md` §4。
需求（2026-09-21 用户原话，五条）：①「Key、账号、组织架构在管理后台界面整合」；②「飞书绑定到账号
（不再只绑 Key）」；③「只要配置文件开启了 dsh，所有激活账号都能用」；④「绑定飞书不需要扫码，
弹窗让管理员选择飞书人员」；⑤「dshgw 登录时，账号有多个 Key 就弹选择框」。
用户四项决策：整合以**组织架构为中心**（人员列表项带账号操作、可展开看详情与 Key 列表）；
多 Key 弹窗覆盖两种登录路径且**只影响归属与审计**；DSH **默认全开 + 首次登录按需建租户**，
保留「停用」为显式例外；Key 级扫码绑定**替换**为账号级选人，存量**迁移后清空**。
**已部署并完成本机验收（2026-09-21，逐条记录见设计 §11）**；代码实现与自动化验收见 `docs/todo_done.md` 同名小节。

- [ ] **人工走查（用户反馈后的六处界面改动）**：已 `make build` 并重启 `aigw-local`（控制台资源内嵌在
      二进制里），`/version` = `d655c0e`、新资源已在线；剩浏览器侧**硬刷新**（静态资源 `max-age=300`）
      `http://aigw.internal:8088/admin/ui/#/org`，逐条确认：人员列表是多列表格且列对齐；展开后点「收起」
      真的收起；「分配组织」弹出组织树勾选（勾父不连带子）；节点详情「新建成员」建完立刻在成员列表里；
      人员行「编辑」能改账号字段；「绑定飞书」点下去先看到弹窗与读取进度。自动化证据见 `docs/todo_done.md`
      的「修掉组织页人员表与三类弹窗的六处问题」小节

- [ ] **唯一剩下的验收：飞书真链路走一次**（需要本人的手机/飞书身份）：在飞书里点一次「飞书登录」，
      确认落进本人账号的租户、侧栏显示飞书名；再在控制台把该账号「停用 DSH」，确认同一身份再登被拒
      （自动化侧已验到"账号级身份是判定真值 + 选择页 + 按需建租户 + 显式停用不被撤销"这些不需要手机的部分）
- [ ] **部署后续（本机已做，其他环境照做）**：`config.yaml` 的 `dshgw` 块加 `auto_enable: true`（**本机已打开**）；
      `bin/aigw` 与 `bin/dshgw` 都要重建并重启（选择页在 dshgw 里，只更新 aigw 会让 Key 登录直接进租户）；
      回滚点 `data/prev/bin/{aigw,dshgw}.prev-running-3.1.0-69da1dd`。
      **版本号仍是 3.1.0 而 revision 是 M72 的提交**——下次发版按 `release-version` 技能正常升版本即可
- [ ] **给账号补模型授权**（现在是"所有激活账号都能用 DSH"的实际瓶颈）：本部署 `auth.default_grant: none`，
      多数账号没有标签/节点授权，首登会以 403 `provision_failed` 被拒（门户与控制台都会说明原因）。
      控制台组织页的人员行现在直接写着「需该账号有可用模型」；批量补授权建议用组织节点标签或账号标签
- [ ] 未做（明确记下）：选中的 Key 不影响 worker 的模型凭据（用户选 A；要做是另一个里程碑：
      凭据热更新 + 并发会话冲突 + 额度归属）；`/accounts`、`/keys` 两页保留未合并（组织页是主入口）；
      飞书侧离职/停用仍不自动停账户（沿用 M70 口径）；`GET /org/nodes/{id}/accounts` 的 Key 计数
      是每账号一次本地读（账号数上千时应改成聚合查询，见设计 §12 第 12 条）；
      验收留下一个测试租户 `dsh-m51-test-a`（账号 98，端口 18307）与两把已吊销的临时 Key（118/119）

## M73 控制台智能问答的联网能力（`web_search` / `web_fetch`）

设计：`docs/design/m73-chat-web-access.md`；规格：`docs/chat.md` §12（使用、后端选择、安全边界、限额）、
§9（配置）、§11（排障）；配置清单：`config.example.yaml` 与 `config.yaml` 的 `chat.web_access`。
用户决策（2026-09-21）：① 四个后端都要（searxng / bocha / tavily / bing）；② 部署级 + 会话级双层开关；
③ 搜索 **+ 抓取网页正文**；④ **只给控制台智能问答**，不开放给外部 MCP 客户端。
用户决策（2026-09-21，验收时追加）：⑤ 本机 `config.yaml` 打开 `chat.web_access`（`provider: bing`，
免密钥、只适合验证链路）。

真机验收（2026-09-21，临时实例 :8099 跑新二进制、共用同一个 `data/aigw-local.db`）：

- `scripts/verify-m73.sh`：**通过 16 / 失败 0 / 跳过 1**（跳过的那条是"搜索密钥不出现在响应里"，
  因为 `bing` 本来就不需要密钥；用 bocha/tavily 时把 `GW_CHAT_WEB_API_KEY` 传进去即可验证）。
  覆盖：部署事实字段、开关往返与落库、"只改标题不会关掉联网"、归属隔离 404。
- `RUN_TURN=1` 的真实一轮（账户 #4 / Key #8 / `deepseek-flash`）：模型**先 `web_search` 再两次
  `web_fetch`**，自己抓到了 `api-docs.deepseek.com` 的模型价格页，回答里带中英文两条链接与输入价格表，
  并主动说明"网页正文只作资料看待"（§12 的第 4 条防注入规则生效）；费用记入请求日志，工具调用记入
  `chat_tool_calls`（控制台会画成工具卡片）。
- 部署产物自证：:8099 服务的 `js/pages/chat.js` 里能读到联网角标文案与工具中文名，`app.css` 里有
  `field-inline` 规则（内嵌资源确属新版本）。

- [ ] **人工走查（宿主终端，需要你来做）**：8088 上跑的还是旧二进制，且控制台资源内嵌在二进制里，
      所以要在启动它的终端执行 `./scripts/local-run.sh restart`（`config.yaml` 的联网开关已经打开）。
      随后硬刷新 `http://127.0.0.1:8088/admin/ui/#/chat`，点开会话头部的「联网：已关闭 · 开启」，
      问一个需要外部信息的问题，确认工具卡片显示「联网搜索 / 抓取网页」、回答里的来源 URL 可点。
- [ ] **给账号补模型授权**（与 M72 同一条）：本部署 `auth.default_grant: none`，多数控制台账号没有
      模型授权，联网问题会因为「没有可用模型」而问不出来——先按 M72 的方式补授权
      （验收时用的是账户 #4 的 Key #8，它已授权 deepseek 系列）
- [ ] **`bing` 后端的结构漂移要靠人复检**：它是唯一解析别人页面的后端（本机验收用它，因为免密钥），
      复检命令是 `GW_WEBACCESS_LIVE=1 go test ./internal/webaccess/ -run TestLiveSearchAndFetch -v`
      （默认跳过、会真出网）；2026-09-21 首跑就发现"九条结果并成一条"，已修，但下一次改版仍只能靠它发现
- [ ] 未做（明确记下）：上游原生 `web_search` 透传与「能力降级上报通道」（M19 观察项）；
      多编码（GBK）正文解码；阅读器级正文抽取、PDF/Office 解析、站点爬取、搜索缓存；
      联网调用不计费、不记账、不做域名黑白名单；检索词不落审计与日志（有测试钉住）；
      配置了出网代理时，IP 级 SSRF 校验退化为本地预解析（可达范围由代理决定）

## M74 租户名自动用 `dsh-<账号拼音>-<账号ID>`

设计：`docs/design/m74-tenant-name-from-account.md`；规格：`docs/dshgw.md` §3（账号级 dsh 开关）、
`docs/org.md` §拼音表。用户原话：「租户名自动用 `dsh-<账号>` 格式」（确认口径：`dsh-账号拼音-id`，
弹窗预填但**仍可手改**）。
定位：租户名候选只有服务端一份实现（`dshTenantNameForAccount`，拼音来自 `internal/pinyin` 的生成表，
与控制台过滤用**同一张** blob）；控制台弹窗预填账号行下发的 `dsh_tenant_suggested`，删掉两页各自的
`slugFromAccount`；中文名不再退化成共享的 `dsh-tenant`。已有映射与显式请求名仍然优先，**不重命名**
任何既有租户；控制台的租户名正则改为与 dshgw 的 `ValidTenantName` 逐字符相同（因此现在允许以数字结尾）。

- [ ] **真机/浏览器人工走查**：本机 `:8088` 跑的是旧二进制，需 `./scripts/local-run.sh restart` 后对某个
      未启用的中文名账号点「启用 DSH」，确认①预填 `dsh-<拼音>-<id>`；②启用后徽标与审计
      `dsh_enable.tenant` 一致；③`data/dshgw-verify/state/admin.sock` 的 `tenant-list` 里能看到该租户
- [ ] 决策（本里程碑明确不做）：是否给历史租户名做一次性"改名/补 ID"。改名等于换租户——旧租户数据
      （dsh 主目录、工作区）留在旧名字下，且需要挪目录才能继续用；要做就另开里程碑

## M76 点「退出」后强制卸载挂载文件系统，最后强制退出 dsh

设计：`docs/design/m76-dsh-exit-force-teardown.md`；规格：`docs/dshgw.md` §3b / §7b / §7d。
用户原话（2026-09-22）：「dsh 点击退出按钮后，强制 umount 使用挂载文件系统，最后强制退出 dsh」。
定位：退出顺序改为**排除 → 强制卸载（浏览器 FUSE + sshfs）→ 最后强杀 dsh worker**；浏览器侧补强制阶梯
（限时优雅卸载 → `-u -z` 惰性摘除 → abort FUSE 连接 → 重试），SSH 侧退出新增 `DetachTenant`（保留记录）
与登录 `Restore`（自动重挂），顺带修掉 M64 记的死挂载缺陷；失败不再短路，也不再只记错误类型。
实现、单测、e2e 与现网验收的记录见 `docs/todo_done.md` 同名小节。

- [ ] **浏览器人工确认（只剩这一步）**：经门户进某个租户 → 侧栏点「⏻ 退出」→ 回到门户登录页；
      随后断言该账号 `/proc/self/mounts` 无挂载、`ps` 无它的 worker、`ss` 无它的 worker 端口、
      `data/dshgw-verify/state/audit.jsonl` 出现 `logout_mount_detach` + `logout_worker_stop`
      （无 `logout_worker_stop_failed` / `logout_mount_leftover`），再登录一次确认 worker 与 SSH 工作区
      挂载都回来。本机验收与 e2e 都是 HTTP 客户端/脚本跑的，没有真人点界面；需要用户自己的会话

## 缺陷：`dshgw.admin_socket` 与 M63 状态根脱节（2026-09-20 修）

现象：飞书首次登录（绑定了 Key 的账号）在日志里报
`enabling dsh for a bound key failed err="… dshgw admin channel unavailable at
/home/winger/.local/share/dshgw-verify/state/admin.sock: dial unix …: connect: no such file or directory"`，
控制台的「启用/停用 DSH」同理。

根因：M63 把 dshgw 状态根搬进了部署根（`./data/dshgw-verify/state`），但
`scripts/move_dshgw_state.sh` 的改写范围**只覆盖状态树内部**（`registry.json` + 租户产物，见其
`targets` 列表与 `$NEW_STATE` 那段自检），而 aigw 侧 `config.yaml` 的 `dshgw.admin_socket` 是树**外**的
绝对路径，于是它还指着搬家前的 `~/.local/share/…`。`dshgw.enabled: false`（本机是独立 `dshgw-verify`
单元提供 socket），所以 `cmd/aigw` 不会走「监督形态从 state_dir 推导」那条路，这个显式值就是唯一的来源。

已做（本机）：

- [x] `config.yaml` → `dshgw.admin_socket: /home/operator/work/ai_gateway/data/dshgw-verify/state/admin.sock`
      （该文件被 gitignore，故在此留痕）；`systemctl --user restart aigw-local.service` 后
      `aigw-local`/`dshgw-verify`/`gwproxy-verify` 三个单元 active，`/version` 仍是 2.7.0 / `6790dff`，
      `healthz`/`readyz` 200，重启后 `level=ERROR` 0 条
- [x] 按真实接线复验：用 `config.Load("config.yaml")` + `dshgwAdminSocket(cfg, nil)` +
      `internal/localdshgw.Client` 拨号，`ListTenants` 返回 5 个租户（临时测试跑完已删）
- [x] `cmd/aigw` 新增启动自检 `warnIfAdminSocketMissing`：**仅独立形态**在 socket 缺失/不是 socket 时打
      `WARN`（点名路径）。监督形态刻意不打——子进程是在 aigw 开始服务**之后**才绑定 socket，
      那一刻「还没有」是正常态，真失败由 supervisor 自己报
- [x] `docs/deployment-layout.md` §7 的搬迁清单补上「状态树之外的消费者」，点名 aigw 的这个键
- [x] **已随 v2.7.1 发布（2026-09-20）**：`bin/aigw` 重建并重启后这条 `WARN` 在线上生效——
      启动日志里 `dshgw admin channel` 0 条（守卫静默即在证明配置的路径存在且是 socket）。
      发布记录见 `docs/todo_done.md` 的 v2.7.1 小节。当时的取舍也一并记下：抢修时**故意不重建**
      二进制，否则会带上未发布的代码却仍标 `6790dff`，反而污染 `/version` 的版本自证

## M66 的 make verify 在本机会挂住

M66 验收期间发现：`make vet` / `make test`（内部是 `go vet ./...` / `go test ./...`）会把工作区的 `./data`
也走一遍，而 M63 起运行态数据就落在那里（本机是 6.2 GB 库 + 备份，以及 dshgw state 下 GB 级的浏览器工作区
FUSE 挂载）。本机实测：`go list ./internal/...` 秒回，`go list ./...` 与 `go list ./data/...` 十几分钟不返回
（进程停在 FUSE 读取上）；因此 `make verify` 在本机等同于挂住。M66 的验收改用显式包模式跑完了等价的
`go vet` + `go test`（全绿），但 Makefile 本身没动，等确认后再改。

- [ ] 把 Makefile 的 `vet` / `test` / `test-race` 改成显式包模式
      （`./cmd/... ./internal/... ./pkg/... ./examples/...`），并在注释里写明为什么不用 `./...`；
      顶层新增含 Go 包的目录时要同步这个列表（当前只有这四个）

## 缺陷：租户终端无色（`/etc` 白名单缺交互 shell 启动文件，2026-09-22 修）

现象：侧栏「终端」面板里 `ls` 输出单色，提示符也是 bash 的裸默认值（`bash-5.3$`）。同一台机器上
`git log`/`vim` 有颜色（它们的颜色不依赖 shell 配置），所以"终端不支持彩色"是错觉。

根因：租户沙箱的 `/etc` 是白名单（bwrap 从空 tmpfs 根起），宿主的 `/etc/bash.bashrc`、`/etc/profile`、
`/etc/DIR_COLORS` 都不在其中，而租户的 `HOME`（workspace）也没有 `~/.bashrc` —— 交互 shell 起步时既没有
`alias ls='ls --color=auto'`，也没有 `LS_COLORS`。终端本身彩色能力完好：`TERM=xterm-256color`、
`COLORTERM=truecolor`、`tput colors`=256，缺的只是"谁去要求程序上色"。证据：`plugin-state/web-tty.trace.jsonl`
里敲 `ls` 那次 `read` 是 194 B，而真 PTY 实测无色 ≈207 B、有色 ≈330 B。

已做（本机）：

- [x] profile 新增 `Tenant.BashrcFile`：非空时 `--ro-bind … /etc/bash.bashrc`（严格绑定，缺文件即拒启）
- [x] tenancy 每租户渲染 `<DshHome>/sandbox/bashrc`（0644，每次渲染 profile 时重写），内容是 `sandbox.Bashrc` 常量
- [x] 单测：sandbox 的内容不变量 / `/etc` 白名单 / 绑定与不绑定；tenancy 的绑定与「每次重渲染」
- [x] staging 探针新增 `bashrc-readable` / `shell-ls-alias` / `shell-ls-colors`（真实 bwrap 里用 `bash -ic` 验）；
      本机嵌套 userns 被禁 → 明确 SKIP，需在允许嵌套的宿主上跑 `make dshgw-sandbox-test`
- [x] 文档：`docs/design/m57-dshgw-strict-isolation.md` §4.1 / §6.1、`docs/dshgw.md` §7 与「终端」插件行
- [x] **发布并升级 8088（2026-09-22）**：`VERSION` 4.1.0 → 4.1.1（`6f68aeb`，tag `v4.1.1`），
      `bin/aigw`/`bin/dshgw` 已换进宿主部署根并重启 `aigw-local`——`/version` 4.1.1/`6f68aeb`、
      `healthz`/`readyz`/`/admin/ui/` 200、启动行 `aigw starting version=4.1.1 revision=6f68aeb`。
      回滚点 `data/prev/bin/*.prev-running-4.1.0-52796d4`。详见 `docs/todo_done.md` 的 v4.1.1 小节
- [ ] **待人工择时（会重启所有租户 worker，含发起部署的会话）**：
      `bash /home/operator/work/ai_gateway/data/dshgw-verify/state/workspaces/dsh-tenant/deploy-aigw-4.1.1.sh --with-dshgw`
      —— 重启 `dshgw-verify` 让租户 worker 按新 profile 起（`/etc/bash.bashrc` 随之挂入）；随后在「终端」里确认
      `ls` 有色、提示符是绿 `user@host` + 蓝 `cwd`（回归：agent bash 工具里 `ls` 仍无色、`git log` 分页正常）

## M77 dshgw 多机分布式运行（控制面 + 工作节点 + 控制台节点管理 + SSH 一键部署）

> 设计：`docs/design/m77-dshgw-multi-node.md`；规格：`docs/dshgw.md` §8、`deploy/dshgw/README.md` §14/§15、
> `docs/deployment-layout.md` §2/§4.4、`docs/mcp.md`（`group=dshgw`）；样例：`deploy/dshgw/node.example.yaml`。
> 定位：控制面（`dshgw serve`，仍由 aigw 监督）独占门户/公开端口/会话/租户表/审计；租户 dsh worker 可分布到
> 局域网多台工作节点（`dshgw node serve`，同一份二进制）。**不配 `nodes:` 时行为与今天逐字节等价。**
> 需求原话：「实现dshgw 可以在局域网内的多台机器分布式运行」+「节点管理没有管理后台webui」+「后台支持一键通过ssh部署」。

> P1（协议与节点模式骨架）、P2（生命周期分派、节点分配表与对账）、P3（数据面：转发、握手委托与失败语义）、
> P1–P7 已完成，记录见 `docs/todo_done.md` 的 M77 小节；下面只列 M77 收尾与待宿主执行的项。
> P7（控制台「DSH 节点」页、账户页节点列与启用弹窗落点选择、harness 与 UI 测试）已完成，
> 记录见 `docs/todo_done.md` 的 M77 小节。**P1–P7 全部完成。**
- [ ] 回归（M77 全量收尾）：`make dshgw-test dshgw-sandbox-test dshgw-supervised-test`、`go vet ./...`、导入闸门
      （已完成：全仓 `go test ./internal/... ./cmd/...`、`make dshgw-node-test`、`make ui-base`、
      `make ui-dist`、两个验收脚本）
- [ ] **待宿主执行（UI 走查）**：`make ui-check`（需要真 firefox）。2026-09-23 起 `run.sh` 改成
      **逐个候选试版本**后，本机已经能自己跑（`/snap/firefox/...` 在受限会话里 unshare EPERM，PATH 上的
      `firefox` 156.0 可用）：`--views csp` 与全量 33 视图都已跑过并全绿，见 `docs/todo_done.md`
      「组织树没有缩进」。**仍未做**：`make ui-dist` 之后按 README 对**压缩镜像**再跑一遍同一套视图
      （UI_STATIC_DIR；顺带 UI_HARNESS_GZIP=1 走一遍传输层压缩）。
- [ ] **待宿主执行（控制台实机走查）**：重建 `bin/aigw` 并重启 `aigw-local`，在真实 `:8088` 上走查
      「DSH 节点」页的七种状态、一次真实的一键部署（含首次指纹确认）与一次租户迁移
- [ ] **待宿主执行（真机 bwrap 版验收一组）**：本会话沙箱禁非特权用户 namespace，因此
      `make dshgw-node-e2e`（多机，已用 `--passthrough-bwrap` 跑过 35 步）、`make dshgw-sandbox-test`
      （按设计 SKIP 并打印原因）、`make dshgw-supervised-test`（M58 监督形态，tenant-create 因 bwrap 失败）
      三者都需要在允许 `bwrap --unshare-pid` 的宿主上各跑一次真机版
- [ ] 观察项（真实部署前的运维提醒）：`admin_socket` 有 UNIX 路径 108 字节上限，深层 state_dir 会让
      `serve` 起不来（本阶段验收脚本因此把它指到 /tmp）；可在配置校验里提前报错，但不在 M77 范围
- [ ] 实测记录回填：每请求 +1 跳 LAN 的 p50/p99 增量、流式无缓冲区证据、64 MiB 体与 WS 直通、
      一次部署耗时与载荷大小、节点/控制面重启恢复时间（写进设计文档 §11 与 `docs/dshgw.md` §8）
- [ ] 收尾：单一 M77 提交（提交信息引用设计文档路径）

## 控制台行内样式（CSP）修复（2026-09-23）

> 现场与根因见 `docs/todo_done.md`「组织树没有缩进：控制台 CSP 丢弃行内 style 属性」。
> 一句话：控制台 `style-src 'self'` 丢弃 style **属性**，组织树的缩进（以及另外 25 处行内样式）
> 在线上等于不存在；修法是 `ui.js` 的 `el()` 改走 CSSOM。
> **已随 v4.3.0（`dccf435`）发版并部署到 gw-c（`aigw-local`）与 gw-b（`aigw.service`）**，
> 见 `docs/todo_done.md` 的「v4.3.0 发布记录」；下面只剩线上复验与观察项。

- [ ] **发版后复验（必须在真实浏览器里看一次）**：打开 `:8088/admin/ui/#/org`，确认组织树每层缩进
      22px（子节点明显右移）；再开一个 `wide` 弹窗（例如供应商新建/请求详情）确认它的宽度从 680px
      变成 900px —— 后者是"修的是 `el()` 这条通路"的旁证
- [ ] 未纳入本次范围（观察项）：`tree({nodes})` 构造参数被忽略，必须 `refresh(nodes)` 才渲染

## M79 沙箱内工作区短路径视图

> 设计：`docs/design/m79-sandbox-workspace-view.md`；规格：`docs/dshgw.md` §7a、`docs/deployment-layout.md` §4.2/§4.4。
> 需求原话：「实现 `~` 就等于 `<工作区>` 呢？并在沙箱里缩短路径」。默认关闭（`deploy.sandbox_workspace` 留空时
> 行为与之前逐字节一致）。

已完成（可复现）：

- [x] `deploy.sandbox_workspace`（如 `/workspace`）：工作区在沙箱里绑两次（宿主长路径保留），`--chdir` 让
      worker 进程 cwd 也落在视图上
- [x] **镜像子挂载**：bwrap `--bind` 不递归，browser 容器/挂载、`host_shares`、ssh 工作区在视图下逐条再绑一次，
      容器类的只读保护同样镜像（否则短路径可以替换容器）
- [x] 租户可见渲染统一走 `sandboxWorkspacePath`：`HOME`、`/etc/passwd` 家目录、picker clamp root、终端
      `cwd`/`cwdRoot`、文件管理器与变更审阅 `root`、`workspace_seed`
- [x] **已存在租户重启即生效**：新增 `EnsureDirectoryPickerRow`，worker 启动时刷新 picker 行（只改 patch 行，
      不重新渲染 artifacts，`workspace.json` 里用户加的工作区不受影响）
- [x] 加载期校验：拒绝隐藏根、运行时树、非绝对/非 clean、与 state/tenant/workspace 根及插件目录互相包含；
      profile 侧再拒绝与 node/dsh release、工作区本身重叠的值
- [x] 监督形态透传（`dshgw.sandbox_workspace` → 生成的子配置）；多节点部署写在各节点配置里
- [x] 单测：`sandbox`（视图绑定/镜像/校验/基线回归）、`config`（加载与重叠规则）、`tenancy`（HOME、passwd、
      插件行、种子、picker 刷新）、`cmd/dshgw`（`sandbox-exec --print`）、`dshgwsup`/`cmd/aigw`（透传）
- [x] `make dshgw-test` 全绿（Go + 插件 Node 测试 + python 计划测试）

未完成 / 待宿主执行：

- [ ] **真机 bwrap staging**（本会话沙箱禁非特权 userns）：`make dshgw-sandbox-test` 需要新增视图断言后在使用
      `bwrap --unshare-pid` 的宿主上跑一次真机版
- [ ] **生效**：把 `deploy.sandbox_workspace: /workspace` 写进 `$ROOT/dshgw.yaml` + 换 `bin/dshgw` +
      重启 `dshgw-verify`（会重启所有租户 worker，含发起部署的会话）。脚本：工作区里的
      `deploy-aigw-4.2.0.sh`（幂等加键、带备份与就绪门禁、失败装回旧二进制）
- [ ] 观察项：browser-pick / `host_shares` / ssh 面板里**显示**的路径仍是宿主长路径（功能上两个视图都通，
      显示长度是次要问题）；若要一起变短，需要把这些网关侧记录里的路径按视图映射渲染
- [ ] 观察项：picker-clamp 用 `realpath` 校验，工作区内"指向长路径的符号链接"会被判为根外而拒绝（已知边界，
      文档已写明）

## M80 API Key 批量导入与按 Key 查账户

> 需求原话：「实现 mcp 导入自定 apikey admin 接口，实现通过 apikey 查询账户的 admin 接口，mcp 客户端要能调用」。
> 设计：`docs/design/m80-key-batch-import-and-lookup.md`；规格：`docs/mcp.md` §4「Key 批量导入与归属查询示例（M80）」、
> `docs/sub2api-migration.md` §4.5。

> 代码、路由表、测试与文档已完成，记录见 `docs/todo_done.md` 的 M80 小节；下面只列未完成项。

- [ ] 控制台 UI（本里程碑明确不做）：API Keys 页加「导入自定义 Key」与「按 Key 查归属」两个入口
- [ ] **待宿主执行（主机走查）**：在真实 `:8088` 上用 admin scope 的 MCP 令牌按 `docs/mcp.md` §4 的三段式走一遍
      （`dry_run` → 真导入 → `lookup`），并抽查明文不落任何日志：`grep -E 'sk-[A-Za-z0-9_-]{20,}'`
      搜 aigw 日志与 `request_logs` 的正文（控制台智能问答路径按会话绑定 Key 的输入录制策略落正文）
- [ ] 待宿主执行（可选）：拿一批真实迁移 key（哈希形式）跑一次 200 项批量，核对 `created/updated` 计数与
      逐把 `admin_lookup_key` 的归属
- [ ] 发版：随下一个版本发布（本提交不升 `VERSION`、不部署）

## M81 请求日志 `user` 档每条用户消息只留前 100 字符
> 设计文档 `docs/design/m81-user-input-char-cap.md`（里程碑编号说明：M78/M79/M80 已被并行工作区占用）。
> 本节已完成的 8 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待宿主执行（真浏览器走查）**：`make ui-check` —— 本会话沙箱里 `/usr/bin/firefox` 是 snap 包装器
      （答不出 Mozilla 版本），脚本按设计带原因跳过。新增断言在 `scripts/ui-harness/keys.page.html` 的
      `#requests` 视图：详情弹窗必须出现「输入（已截断：每条用户消息只留前 100 字符）」，未截断的行不得
      出现该提示；压缩镜像（`UI_STATIC_DIR` + `UI_HARNESS_GZIP=1`）再走一遍同样待宿主
- [ ] **待安排（发版/部署）**：本改动目前只在代码与文档里，线上 `:8088` 仍是旧行为（需要时走
      `release-version` 流程）

## M78 请求日志的路由路线（供应商 / 上游模型 / 路由 id）

> 设计：`docs/design/m78-request-log-route-path.md`；规格：`docs/request-log.md` §2/§4/§6。
> 本节已完成的实现与验证记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待宿主执行（控制台实机走查）**：`make build` 后重启本机 `aigw-local`，在真实 `:8088` 上打开
      「请求日志」页，确认「上游模型」「路由路线」两列出现、点开一条失败转移的请求能看到两跳与映射规则、
      迁移 0027 之前的旧日志显示「（未知路由）」与「—」而不是报错或 0
- [ ] 观察项：`RequestAttempts` 是每页一次 `request_id IN (…)` 点查，未做大窗口压测；一个请求的
      `attempts` 条数由候选数决定，暂不需要对它分页

## M82 `user` 档只留最后一条合格 user 消息的纯文本
> 设计文档 `docs/design/m82-user-input-tail-only-text.md`（取代 M81 的「每条截断前 100 字符」）。
> 本节已完成的 9 项记录见 `docs/todo_done.md` 的同名小节；下面只列未完成项。

- [ ] **待宿主执行（真浏览器走查）**：`make ui-check` —— 本会话沙箱里 `/usr/bin/firefox` 是 snap 包装器
      （答不出 Mozilla 版本），脚本按设计带原因跳过。`#requests` 视图新增三条回放断言（未保留 / 未录制 /
      历史截断行），压缩镜像（`UI_STATIC_DIR` + `UI_HARNESS_GZIP=1`）同样待宿主
- [ ] **待安排**：随 v4.3.1 发版并部署 gw-c + gw-b 的线上验证（见 `docs/todo_done.md` 的
      v4.3.1 发布记录）；`dshgw-verify` 的待重启事项仍单列在 M77/M79 小节

## M82.1 修订：往回找「最新一条有文本且不超过上限」的 user 消息（v4.3.2）
> 设计文档见 `docs/design/m82-user-input-tail-only-text.md` 的「M82.1 修订」节。

- [ ] **待宿主执行（真浏览器走查）**：`make ui-check` 的 `#requests` 视图（「未保留」文案已改）
- [ ] **待安排**：随 v4.3.2 发版并部署 gw-c + gw-b 的线上验证（见 `docs/todo_done.md`）

## M83 输入档只留「人说的话」（样板按标记跳过）
> 设计文档 `docs/design/m83-user-input-human-only.md`。现场：某浏览器 DSH 租户 14:11 后 71 行请求日志全空。

- [ ] **待宿主执行（真浏览器走查）**：`make ui-check`（「未保留」文案已改成「只有样板或超长用户消息」）
- [ ] **待安排**：随 v4.3.3 发版并部署 gw-c + gw-b，并确认该租户的行恢复有正文（见 `docs/todo_done.md`）

## M84 图片生成（gpt-image 系列 · Images API · `openai-images` 供应商 · `provider.images` 协议）

> 需求原话：「aigw 支持 gpt-image 系列模型，支持这类模型的 provider 接口」。
> 设计：`docs/design/m84-image-generation.md`；规格：`docs/api-images.md`、
> `docs/plugin-protocol-v1.md`、`docs/routing.md`、`docs/pricing.md` §1、`docs/billing.md`、
> `docs/request-log.md`、`docs/dshgw.md`。代码与实测记录见 `docs/todo_done.md` 的同名小节；
> 本里程碑在**独立工作区** `../ai-gateway-m84`（分支 `m84-images`）实现；**已于 2026-09-23 并入 `main`**
> （见 `docs/todo_done.md` 的 v4.5.0 发布记录），二进制随 v4.5.0 一起部署。

- [ ] **待宿主执行（线上验证）**：在 gw-b 建 `openai-images` 实例（上游写官方或中转的 `/v1/images/*`）
      并映射 `gpt-image-*`，用真实 Key 生图一次，核对 `usage_records` 的 `input`/`image_input`/`image_output`
      与 `cost_micros`（= 图像输出价 × 输出 token；gw-b 现有规则集不改也能算对），并确认租户 DSH 菜单
      不再出现该模型（`dshgw sync-models` 后看 `settings.yaml`）
- [ ] **待宿主执行（真浏览器走查）**：控制台「供应商」页新建 `openai-images` 类型时，字段说明与模板正常渲染
      （本会话沙箱无可用 firefox，`make ui-check` 自带跳过）


## M85 智能问答把整段会话历史都发给模型（历史窗口 0 = 不限制，默认 0）
> 设计文档 `docs/design/m85-chat-full-history.md`（编号 M84 → M85：M84 被并行工作区的 Images API 占用）。
> 需求原话：「智能问答要把会话里的所有历史记录都发给模型」。
> 本节已完成的 8 项与两条发布记录（v4.4.0 / v4.5.0）见 `docs/todo_done.md` 的同名小节。

- [ ] **观察项（非阻塞）**：整段回放让每次提问的输入 token 随会话增长（22 轮的小会话已到 7.5K 输入）。
      线上若开始出现「本会话历史已超出该模型的上下文上限」的失败，说明到了该给该部署设窗口、换大上下文模型、
      或做历史摘要压缩（压缩是另一个里程碑，本次明确不做）
## M86 「变更」插件跟随当前会话的 git 工作区
> 需求原话：「变更 插件 应该和当前会话的工作区关联」→「跟随当前会话的 git 工作区」→
> 「不能准确的到当前会话对应的 git 工作区吗？」。设计：`docs/design/m86-session-worktree-scope.md`；
> 规格：`docs/dshgw.md` §7f、`deploy/dshgw/README.md`「默认开启：租户侧 web 插件」、
> `cmd/dshgw/plugin/git-diff/README.md`。零 Go 改动（行渲染与 `tenant_plugins` 开关不变）。

- [x] 宿主半 `git-service.js`：新增 `scopeOf(workspace)`（夹紧 → `git -C <W> rev-parse --show-toplevel` →
      判不出就退「该目录」），发现缓存改 per-root（30s/8 条），`resolveRepo` 增加按作用域的 `root` 与
      新的缓存键，`hello`/`diag` 报 `root`/`anchor`/`roots`
- [x] `rpc-handlers.js`：仓库作用域端点统一先解析作用域并回传 `{root, workspace, anchor, reason}`；
      `index.js` VERSION 0.1.0 → 0.2.0（`package.json` 同步），激活日志标明 root 是夹紧/回退根
- [x] 浏览器半 `client.src.js` → `client.js`：从 `props.sessionId` + `useSessions(...cwd)` 取会话工作区，
      每次调用带上；换工作区即清空并重新引导（generation 守卫丢弃迟到响应、取消旧扫描）；
      仓库记忆改按工作区存（`repoByWorkspace`，v1 旧值丢弃）；作用域标注（仓库名 / 该目录下的仓库 /
      回退告警）与「不在 git 工作区里」的空态 + 可点仓库名
- [x] 测试：宿主 +9（仓库/子目录/非工作区目录/四种回退/top 在夹紧之外/`.git` 文件的 linked worktree/
      两作用域的 `rel`/按根发现缓存与 `hello` 口径/一次解析的缓存），客户端 +5（每次调用带工作区、
      无 props 时不带、换工作区重新引导、非工作区目录不自动选仓库且可点选、回退提示）；
      `npm test` **48/48**（基线 34 + 新增 14），`node build-client.mjs` 重建 `client.js`
- [x] 文档：设计文档、插件 README（作用域/接口/限制/验证）、`docs/dshgw.md` §7a/§7f、
      `deploy/dshgw/README.md`（含宿主半改动需重启 worker 的口径）
- [ ] **待宿主执行（部署 + 线上验收）**：把 `cmd/dshgw/plugin/git-diff/` 同步到
      `/home/operator/work/ai_gateway/cmd/dshgw/plugin/git-diff/`（沙箱内该目录只读），
      `cmp` 确认后按租户重启 worker：
      `printf '%s\n' '{"id":1,"op":"tenant-restart","name":"dsh-tenant"}' |
      nc -U /home/operator/work/ai_gateway/data/dshgw-verify/state/admin.sock`，再刷新页面
- [ ] **待宿主执行（真浏览器走查）**：① 本会话（workspace=`…/work/ai-gateway`）打开「变更」只看到
      `ai-gateway` 一个仓库；② 会话开在仓库子目录时仍是该仓库；③ workspace=`…/work` 的会话显示
      「该目录下的仓库」且不自动选中；④ 老会话（M79 之前的长路径 cwd）显示回退提示；
      ⑤ `plugin-state/git-diff.trace.jsonl` 出现 `version=0.2.0` 与作用域事件

## M88 dshgw 只按账户映射租户（退役 key 前缀绑定）
> 设计：`docs/design/m88-dshgw-account-tenant-binding.md`；需求原话：「dshgw 也用 key hash 找租户」，
> 评审结论是不哈希化、直接删掉本地解析。**代码、测试、文档、v4.7.0 发布（aigw 侧已部署 gw-b +
> gw-c）都已完成**，完整清单与发布记录见 `docs/todo_done.md` 同名小节；本节只留未完成项。

- [ ] **待宿主执行（dshgw 控制面部署 + 线上验收）**：把 `bin/dshgw` 4.7.0/`11e8254` 换入
      `/home/operator/work/ai_gateway/bin/dshgw`（备份到 `data/prev/bin/`）并
      `systemctl --user restart dshgw-verify`——**会重启全部租户 worker，含本会话，需择时**。
      完整命令与回滚见 `docs/todo_done.md` 的 v4.7.0 发布记录。验收：① 门户用一把 Key 登录已启用
      DSH 的账号能进（账户映射）；② `journalctl --user -u dshgw-verify --since "-5min" |
      grep tenant_unmapped` 无输出；③ `state/keys.map` 已不存在；④
      `bin/dshgw whereis <账号名>` 有输出
- [ ] **可选后续**：把 sub2api 的 #24/#51 真数据导入（现在两把都能原样进，没人需要换 key）：
      `python3 /opt/aigw/sub2api_migrate.py plan` → `apply`；完成后
      `shred -u /opt/aigw/data/K7QX-reissue-key.txt`（那把重签明文就没用了）
- [ ] **可选后续**：清理 4 把已 disabled 的验收 key（gw-b #162/#163/#166/#167，网关没有删除 key 的路由，
      需手工 `DELETE`）
- [x] tag 已推 origin（2026-09-28）：`main` 快进到 `7cc5297`，并补齐 **v4.4.0 / v4.5.0 / v4.6.0 /
      v4.7.0 / v4.7.2 / v4.7.3** 与新的 **v4.8.0 / v4.8.1**（远端 v4.x 标签从 v4.0.0 到 v4.8.1 无缺口）。
      本地只剩 worktree 分支 `m84-images` 未推。

## M89 代码脱敏（全仓清洗 + 发版强制检查 + 历史重写）
> 设计：`docs/design/m89-code-desensitization.md`；需求原话：「项目代码脱敏」「修改 skill 要求发布版本时脱敏」。
> 口径经确认：全仓库清洗、发布与部署入口列入例外保留真值、连 git 历史一起重写并强推 origin、
> Go 模块路径改为 `github.com/funnywwh/ai-gateway`。规则表是 `scripts/desensitize.py`（单一真源）。
> **工具与工作树清洗已完成并落袋**（提交 `6a99f63`，随 v4.7.2 重建 `bin/`），记录见
> `docs/todo_done.md` 的 M89 小节；本文件只留未完成项。

- [~] 发版强制：技能新增「脱敏检查」步 + `scripts/release.sh` 硬门禁（本次 v4.7.2 发版是**手工**跑的
      `--check` 与 `--require-table`，脚本里还没接线）
- [ ] 历史重写：`git bundle` 备份 → `filter-branch --tree-filter` 跑同一份规则 → `--check-history` = 0 → 强推 main 与 tags → 新克隆复核（**需操作者确认窗口，会换掉全部 sha，其它克隆必须重新 clone**）
- [ ] 归档：设计文档「实现与设计差异」回填、`docs/todo_done.md` 发布记录
- [ ] 已知后果（记录，不修）：文档里的历史 revision 短 sha 重写后不可解析；例外文件（`.dsh/skills/**`）刻意保留真实主机名与域名
- [ ] 待定：v4.7.1（release 提交 `645dbac`）**仍未打 tag** —— v4.7.2 已打，补不补由操作者定

## M90 租户插件自己挂浏览器 RPC 通道（修 dsh 0.1.7 下的 `transport failure … HTTP 405`）
> 需求原话：「修复 dsh transport failure for /ssh-workspace/hosts: HTTP 405」「transport failure for
> /dshgw-git-diff/hello: HTTP 405」。设计：`docs/design/m90-tenant-plugin-rpc-channel.md`。
> 根因：`dsh-client-connection@0.1.7-alpha.2` 的 `HostConnectionService.register()` 读的是**服务自己**
> context 的 `webServer`（该插件 `inject = ['credentials']`），任何调用方调 `ctx.connection.rpc.handle`
> 都抛 `cannot get property "webServer" without inject` ⇒ 四个插件的通道一个也没挂上，浏览器 POST
> 掉进 SPA 兜底座位的 405。宿主半（`cmd/dshgw/plugin/`）改一行 Go 都不需要，aigw 侧只多了体检项
> `tenant-plugins-lib`，随 **v4.7.2 部署到 gw-c**（见 `docs/todo_done.md` 的 v4.7.2 发布记录）。

- [x] 新增 `cmd/dshgw/plugin/lib/rpc-channel.js`：用 dsh 自己挂 `/api` 的原语
      （`webServer.register({kind:'prefix',path,handler})` + `connection.admit`）挂插件通道，
      信封与状态码逐条对齐 `rpcFetchHandler`（404/415/400/bad-request/500/413、abort 传 signal）；
      四个宿主半（ssh-workspace / git-diff / web-tty / workspace-files）改用它，浏览器半一行不改
- [x] 测试：`lib/rpc-channel.test.mjs` 11 项（真实 socket）并入 `make dshgw-test`；
      `ssh-workspace`（248 断言）、`web-tty`（14 项）改为从真路由发请求；`workspace-files` 19 项、
      `git-diff` 48 项照旧全绿
- [x] 文档：设计文档、插件 README（git-diff / web-tty / workspace-files）、`docs/dshgw.md` §7f、
      `deploy/dshgw/README.md`、两份 `config.example.yaml`（部署形状多一个 `lib/`，旧的三目录同步法会漏）
- [x] 部署前置检查：`dshgw doctor` 与 `dshgw node doctor` 新增 `tenant-plugins-lib`
      （`tenancy.SharedPluginModulePath` / `SharedPluginModuleRequired` + Go 测试），漏同步 `lib/` 时
      在体检就 FAIL，而不是等四个面板的行一起加载失败
- [x] 宿主同步（2026-09-28，本会话经 `ssh gw-c` 执行；宿主部署根
      `/home/operator/work/ai_gateway`）：整个 `cmd/dshgw/plugin/`（含新增的 `lib/`，不含运行期
      `git-diff/trace.jsonl`）同步过去，`diff -rq` 逐个文件确认与工作区一致；备份在宿主
      `/tmp/plugin-before-20260928-093550`（回滚：`cp -a` 回去或 `git checkout -- cmd/dshgw/plugin`
      ＋ `rm -rf cmd/dshgw/plugin/lib`）。同步后宿主侧跑通：`lib` 11/11、`ssh-workspace` 248 断言、
      `web-tty` 14/14、`workspace-files` 19/19、`git-diff` 48/48
- [ ] **待宿主执行（重启 + 线上验收）**：宿主上执行（**会中断该租户正在进行的回合，含本会话**）：
      `printf '%s\n' '{"id":1,"op":"tenant-restart","name":"dsh-tenant"}' | nc -U /home/operator/work/ai_gateway/data/dshgw-verify/state/admin.sock`
      验收：① 刷新页面后「终端 / 文件 / 变更 / 我的主机」四块面板都能出数据；② 对 web 口打
      `/ssh-workspace/hosts` 与 `/dshgw-git-diff/hello` 回 **401**（要鉴权）而不是 405；③
      `plugin-state/{git-diff.trace.jsonl,workspace-files.trace.jsonl,web-tty.trace.jsonl}` 出现新的
      `rpc` 事件；④ 升级 dsh 版本后按设计文档 §3 的探针重跑一次。其余租户（dsh-alex、dsh-liuyang 等）
      用的是同一份插件目录，下一次 worker 启动/重启即生效

## M92 导入多家公司的组织架构（每家公司一个飞书自建应用）
> 设计：`docs/design/m92-multi-company-feishu-org-sync.md`；需求原话：「如何实现导入不同公司的组织架构？」；
> 评审确认：数据来源 = 各家公司的飞书企业（每家一个自建应用 App ID/Secret），导入范围 = 部门节点（每家一棵根）
> + 人员 → 账号（自动合并已存在账号、缺失的按需创建）。
> 规格：`docs/org.md` §1/§3/§5/§6、`docs/feishu.md` §2b/§3/§5c.6、`docs/mcp.md` §4、`config.example.yaml`。
> **代码、测试、文档都已完成**，完整清单与验收证据见 `docs/todo_done.md` 同名小节；本节只留未完成项。

- [x] 升级到 **v4.8.1** 并部署 gw-c：`/version` = 4.8.1/`dd4398a`，启动日志出现
      `legacy feishu org nodes adopted … nodes=9`（迁移 0030/0031 已应用）。见 `docs/todo_done.md` 发布记录
- [ ] **待操作员（需要管理员会话）**：同步本公司（确认"新建公司节点 + 移入 N 个顶层部门"，再同步一次显示
      0 写入）→ 配置第二家公司的 `feishu.companies` 并重启 → 同步 → 核对：两家公司的树互不干扰、
      账号 JSON 的 `feishu.links` 正确、门户飞书登录无回归（身份应用的路径未改）
- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox，`make ui-check` 跑不了。宿主上执行
      `make ui-check`（或 `scripts/ui-harness/run.sh --views org-sync org-sync-readonly org-sync-nonames`），
      验收新增检查项：`companyPickerShown`、`companyPickerListsBoth`、`directoryNamesCompany`、
      `headerNamesCompany`、`syncBodyCarriesCompany`、`syncConfirmNamesCompany`、`createPathCarriesOpenId`

## M93 在控制台管理公司的飞书应用（新增/编辑/停用/删除 + 测试连接）
> 设计：`docs/design/m93-console-managed-feishu-companies.md`；需求原话：「用户可以在管理后台添加每个公司的应用吗？」；
> 评审选择：**做成控制台可管理**。规格：`docs/feishu.md` §2b/§3/§5c.6、`docs/org.md` §3/§5、`docs/mcp.md` §4、`config.example.yaml`。
> **代码、测试、文档都已完成**，完整清单见 `docs/todo_done.md` 同名小节；本节只留未完成项。

- [ ] **待宿主执行（部署 + 线上验收）**：升级后在控制台「公司」页登记一家真实客户公司 →
      「测试连接」通过 → 保存 → 同步 → 停用再启用 → 删除登记（确认节点与映射保留）；
      **全程不重启网关**。复核：`GET /admin/api/v1/org/feishu/companies` 的 `secret_configured` 为 true，
      且 `sqlite3 data/aigw.db "SELECT hex(secret_enc) FROM feishu_apps"` 里**看不到**明文
- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox。宿主上 `make ui-check`（或
      `scripts/ui-harness/run.sh --views companies companies-readonly`），验收：三行公司、来源徽标、
      停用行、「测试连接」只发 id、新建对话框密钥字段不预填、viewer 无写入口

## M94 在控制台改公司名（含身份应用与配置里的公司）
> 设计：`docs/design/m94-editable-company-names.md`；需求原话：「`…/admin/ui/#/companies` 要支持能修改公司名」。
> 规格：`docs/org.md` §5、`docs/feishu.md` §5c.6、`docs/mcp.md` §4、`config.example.yaml`。
> **代码、测试、文档都已完成**，完整清单见 `docs/todo_done.md` 同名小节；本节只留未完成项。

- [ ] **待宿主执行（部署 + 线上验收）**：升级后在「公司」页点「本公司」那一行的**改名** → 输入「客户组一」保存
      （此时会提示根层已有同名节点）→ 按提示**先点一次「同步飞书」**（公司节点建出、根层「客户组一」被移入其下）
      → 再改名成「客户组一」→ 公司节点随之改名（树变成 客户组一 → 客户组一(部门) → 软件部…）→ 再同步一次应为 0 写入
- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox。宿主上 `make ui-check`（或
      `scripts/ui-harness/run.sh --views companies companies-readonly`），验收 M94 新增检查项：
      `nameOverrideBadge`、`identityRowOffersRename`、`renameDialogEditable`、`renameDialogSecretLocked`、
      `renameDialogHint`、`renameDialogNoProbe`

## M95 控制台主菜单改成可折叠（分组手风琴 + 记忆打开状态）
> 设计：`docs/design/m95-console-collapsible-nav.md`；需求原话：「管理后台的主菜单改成可折叠」，
> 上线当天追加「菜单默认折叠」（粒度经确认：**只展开当前分组**）→ 修订见设计文档 §12（M95.1）。
> 已确认的选择：**分组可折叠**（不是整栏折叠）、**状态记在浏览器里**、**默认折叠**。
> 规格：**无**（只动控制台自己的导航 chrome，没有 API/配置/字段/数据变化；理由见设计文档开头）。
> **代码、测试、文档、发布都已完成**（v4.10.0 折叠 + v4.10.1 默认折叠，均已部署到 gw-c，
> 见 `docs/todo_done.md` 同名小节的两条发布记录）；本节只留未完成项。

- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox（`/usr/bin/firefox` 是 snap 壳子，`run.sh` 会跳过）。
      宿主上 `scripts/ui-harness/run.sh --views sidebar`（或整轮 `make ui-check`），验收 M95/M95.1 的检查项：
      `groupCount`、`groupNames`、`groupIsButton`、`linkCount`、`defaultShowsOnlyActiveGroup`、
      `defaultFoldsTheRest`、`ariaControlsResolves`、`chevronDrawn`、`openChevronPointsDown`、
      `foldedChevronPointsRight`、`openGroupFlipsAria`、`openGroupRevealsItems`、`openGroupLengthensMenu`、
      `openKeepsOtherGroups`、`openGroupChevronTurnsDown`、`openPersisted`、`closeGroupHidesItems`、
      `closeClearsStorage`、`coldLoadHonoursStorage`、`coldLoadKeepsOthersFolded`、
      `activeGroupOpensThoughNeverOpened`、`navigationFoldsTheRest`、`activeLinkHighlighted`、
      `autoOpenDidNotWriteStorage`、`foldActiveGroupInPlace`、`openAgainRestores`、`activeGroupReopensOnRender`、
      `keyboardReachable`、`noInlineStyles`、`noPageErrors`
- [ ] **待操作员（人工看一眼，线上已是 v4.10.1）**：打开控制台 → 菜单默认只剩 6 个分组标题 + **当前分组**的
      条目；点别的分组展开、再点收起；刷新后仍记得打开过哪些；直接访问 `#/keys` 这类深链接时对应分组
      自动展开且当前项高亮

## M96 公司页的「编辑」：配置来源的公司也能改字段
> 设计：`docs/design/m96-company-edit-fields.md`；需求原话：「"改名"应该改成"编辑"，编辑可编辑字段」
> （确认时追加"密钥也要能改"）。规格：`docs/org.md` §5、`docs/feishu.md` §5c.6、`docs/mcp.md` §4。
> **代码、测试、文档都已完成**，完整清单见 `docs/todo_done.md` 同名小节；本节只留未完成项。

- [ ] **待宿主执行（部署 + 线上验收）**：升级到含本里程碑的版本后，在「公司」页点「本公司」→ **编辑**：
      公司名 / 公司根节点名 / 备注 / 启用都能改（本公司的密钥只读）；改完点「恢复配置值」应还原；
      另配一家客户公司时，密钥字段应可覆盖（留空 = 用配置里的）
- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox。宿主上 `make ui-check`（或
      `scripts/ui-harness/run.sh --views companies companies-readonly`），验收检查项：
      `identityRowOffersEdit`、`editDialogEditable`、`identitySecretLocked`、`overriddenBadge`、
      `resetOverrideOffered`、`clientSecretEditable`

## M97 请求日志的时间窗口：当天 / 本周 / 本月 / 时间段（按本地时间）
> 设计：`docs/design/m97-request-log-time-window.md`；规格：`docs/request-log.md`「时间窗口（M97）」。
> 需求原话（2026-10-07 截图批注）：「添加:当天、本周、本月、时间段」/「时间段点击后，弹出选择
> 开始结束日期」/「按本地时间计算」。
> **代码、测试、文档与「部署到本机」都已完成**（`make ui-base`/`make test`/`make build` 在本沙箱全绿，
> 本机 `aigw-local` 已换到 4.11.1/`76568ca` 并验过，见 `docs/todo_done.md` 的 M97 小节与 v4.11.1
> 部署记录；差异见设计文档 §10）。本节只留未完成项。

- [ ] **待操作员肉眼确认**（本机 `:8088` 已在跑新版本，浏览器需硬刷新 Ctrl+Shift+R 拿新资源）：
      打开 `http://127.0.0.1:8088/admin/ui/#/requests`，下拉里应出现「当天 / 本周（周一起）/ 本月 /
      时间段…」；选「当天」两张表都只显示本地 00:00 起的行，工具栏写明窗口与本地时区；
      选「时间段…」能选开始/结束日期，确定后选项变成「时间段：MM-DD ~ MM-DD」，取消则回到原窗口
- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox（`run.sh` 会跳过；断言本身已用 chromium 在
      本机跑过一轮全绿，见 `docs/todo_done.md`，但官方入口仍要宿主跑）。宿主上 `make ui-check`
      （或 `scripts/ui-harness/run.sh --views requests`），验收检查项：`windowOptions`、
      `windowTodaySendsFrom`、`windowTodayIsLocalMidnight`、`windowBothTables`、`windowHintLocalTime`、
      `windowHintShowsBoundary`、`windowScrollLabelsKept`、`rangeButtonHiddenByDefault`、`rangeDialogOpens`、`rangeDefaultsToday`、
      `customRangeSendsBounds`、`customRangeLabel`、`rangeButtonShown`、`customCancelRestores`、
      `windowScrollRestoresDays`、`customLabelCleared`
## M98 `uya-agent` 作为一个独立的客户端维度取值
> 设计：`docs/design/m98-uya-agent-client.md`；需求原话：「识别出来的是 dsh 客户端，要是 uya-agent」。
> 规格：`docs/request-log.md` §2（身份维度表 + `uya-agent` 那条说明）。
> **代码、测试、文档都已完成**（清单见 `docs/todo_done.md` 同名小节）；本节只留未完成项。

- [x] **部署 + 线上验收**（2026-10-07 11:27，用户确认后执行）：已装 `407c003` 到
      `/media/winger/_dde_data/aigw` 并重启 `aigw-local.service`（回滚点
      `bin/aigw.prev-20261007-112739`）；线上真机读数 `client=uya-agent`、
      `workspace=/home/winger/uya-agent`，控制台资源已带新下拉项。
      完整记录见 `docs/todo_done.md` 的「M98 部署记录」
- [ ] **待宿主执行（浏览器走查）**：本沙箱没有 firefox（`make ui-check` 会自动跳过；不需要浏览器的
      `internal/webui/tests/requests_test.mjs` 已并入 `make ui-base` 并跑过）。宿主上执行
      `make ui-check`，确认 `keys.page.html` 的 `clientFilter`（五个取值都能选，本次已改成逐项
      断言）与 `clientShown` 通过。**未做的事**：没往 `fixtures.json` 加 `uya-agent` 行 ——
      那份夹具是 2 行，而走查里有若干只认「共 2 行」的断言（都属于浏览器路径，本沙箱跑不到），
      加一行会连带动它们；留到宿主上能真跑 `ui-check` 时一起改更稳。

## M99 发布 v4.12.0 并升级本机 `:8088`
> 需求原话：「帮我升级本机 rag-server 8088」。发布记录见 `docs/todo_done.md` 的
> 「发布 v4.12.0 并升级本机 `:8088`」小节；本节只留未完成项。

- [ ] **待宿主执行（换二进制 + 重启）**：沙箱看不到宿主进程、部署根未挂载、`systemctl --user`
      不可达，故部署交给宿主终端：
      `bash deploy-aigw-4.12.0.sh --check` → `bash deploy-aigw-4.12.0.sh`（A 段，只重启
      `aigw-local`，不打断租户 worker）。期望 `/version` = `4.12.0` / `2651b56`，
      `healthz`/`readyz`/`/admin/ui/` 均 200，启动窗口 0 条 `level=ERROR`。
      回滚点会自动拍成 `<部署根>/aigw.prev-<时间戳>`。
- [ ] **待宿主择时（B 段）**：`bash deploy-aigw-4.12.0.sh --with-dshgw` 重启 `dshgw-verify`，
      让盘上的 `bin/dshgw` 也在进程里生效（门户 18300 / 网关 18299 / 租户 18301+ / worker 18400+）。
      **会重启所有租户 worker，含发起本次部署的会话**，所以默认不跑。
- [ ] **待操作员肉眼确认（浏览器）**：硬刷新 `http://127.0.0.1:8088/admin/ui/#/requests`，
      确认 M97 的时间窗口下拉（当天 / 本周 / 本月 / 时间段）与 M98 的 `uya-agent` 客户端下拉项
      都在；`autodl-api` 下应能看到 `GLM-5.3-flash` 与 `hy4-preview`。
      （控制台资源 `//go:embed` 进二进制，A 段重启后即生效。）

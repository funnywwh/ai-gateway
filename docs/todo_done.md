# 已完成记录（自 `docs/TODO.md` 归档）

> 本文件保存 `docs/TODO.md` 里所有已勾选 `[x]` 的条目：整节做完的小节整节搬来；仍有未完成项的
> 小节只搬已勾选的那部分（同名标题与引言两边各留一份，便于对照）。搬运时原文一字未改。
> 维护规则：`docs/TODO.md` 里完成一项后，把该条（连同缩进子条目）搬到本文件同名小节下；本文件只增不改。
> 归档时间：2026-09-15。

---

## M0 脚手架 + git 仓库
- [x] git init（默认分支 main）+ 本地提交身份
- [x] .gitignore（忽略 .cache/ data/ bin/ *.db billing-fallback.jsonl hooks-dead.jsonl）
- [x] go.mod（module github.com/winger/ai-gateway, go 1.25）
- [x] scripts/goenv.sh（工作区本地 GOPATH/GOMODCACHE/GOCACHE + file:// 代理 + CGO_ENABLED=0）
- [x] Makefile（build/test/vet/fmt/tidy/verify/run/plugin-example/smoke/clean）
- [x] docs/design/m0-scaffold.md 设计文档
- [x] internal/logx、internal/ids
- [x] internal/config（YAML 加载 + 默认值 + GW_* 环境变量覆盖 + 校验）
- [x] internal/domain 接口骨架（实体 / 接口 / 错误）
- [x] cmd/aigw（--version / --config）
- [x] make verify 全绿（vet + test + build）
- [x] 首次提交

## M1 存储与注册表
- [x] 迁移框架（schema_migrations，内嵌 SQL，幂等）
- [x] DAL（accounts/api_keys/mcp_tokens/providers/provider_models/models/model_mappings/routes/tags/ledger/usage/audit/settings）
- [x] YAML 引导（bootstrap.mode=off/upsert/merge，含 providers/models/routes/tags）
- [x] registry：内存快照 + 原子换入
- [x] audit 日志
- [x] 读写双连接池（WAL 并发读 + 单写者）

## M2 插件协议与宿主
- [x] pkg/pluginapi：帧编解码 / schema 子集 / Serve / stdout 背压 / Client
- [x] 设计文档 docs/design/m2-plugin-protocol.md + 规格文档 docs/plugin-protocol-v1.md（已在对话中输出）
- [x] internal/pluginhost：启动/握手/心跳/取消/draining/重启退避/凭据文件/日志环形缓冲
- [x] 示例插件 examples/provider-replay（可控慢流、增量用量、可控失败）
- [x] 真实子进程 E2E（生命周期/崩溃重启/流中失败/发现可执行文件）
- [x] pkg/providerkit：SSEReader / chat↔responses 双向转换 / ChatUsage→维度映射 / CharEstimator
- [x] 内置 provider：openai-responses / openai-chat / testecho + registry（Build/IsBuiltin）
- [x] 错误分类映射（retryable/quota_exhausted/fatal，Retry-After→reset_at）
- [x] openai-chat httptest 覆盖（非流式/流式/429 冷却/5xx 可重试/4xx 致命）

## M3 路由 / 权限 / 模型自由映射
- [x] 设计文档 docs/design/m3-routing.md + 规格文档 docs/routing.md（已产出）
- [x] 模型自由映射：exact/prefix/glob/regex、priority 抢占、{model} 与捕获组、兜底、@provider 钉死、直接钉死供应商
- [x] 鉴权与授权并集（key + tags，`*` 通配，default_grant all/none）+ 策略合并（tag→key 覆盖）
- [x] 候选过滤：disabled/draining/not_granted/冷却/熔断/not_mapped/能力缺失（含 degradation strip|reject）
- [x] 优先级分层 + 5 种层内策略（加权随机/轮询/最少连接/每 token 延迟/严格顺序）
- [x] 熔断（连续失败 + 失败率双条件、冷却、半开探测、探测失败重开）
- [x] Explain（解析链路 + 有序候选 + 排除原因），与数据面共用同一纯函数

## M4 鉴权 / 限速 / 计量
- [x] 设计文档 docs/design/m4-auth-quota-metering.md；规格补入 docs/api-responses.md「认证与限速」
- [x] API Key 前缀索引 + 常量时间哈希比较 + 正/负缓存（30s/5s）+ 容量上限 + Invalidate
- [x] last_used_at 节流异步更新（>60s 才写）
- [x] 账户 suspended → 402（区别于 401）
- [x] 分片滑动窗口限速（64 分片 × 60 个每秒槽位）：rpm/tpm/并发，最严合并，票据 Release/Settle
- [x] ExceededError 携带维度/上限/剩余/重置时刻，可生成 x-ratelimit-* 与 Retry-After
- [x] usage_records（attempt 粒度、分维度 JSON、degraded features、overshoot、来源标记）
- [x] 流式用量累加器（delta 累加 → 最终值覆盖；只有增量则标 estimated）

## M5 Responses API
- [x] 设计文档 docs/design/m5-responses-api.md；规格 docs/api-responses.md 状态改为已实现
- [x] POST /v1/responses（非流式 + SSE 全事件序列、sequence_number 自增、逐帧 Flush）
- [x] 请求校验（不允许项不出网）：background / 托管工具 / max_output_tokens<16 / 空 input / 温度范围
- [x] responseAssembler：流式与非流式共用，保证 GET 与流式聚合逐字段一致
- [x] GET/DELETE /v1/responses/{id}（按 Key 隔离所有权）、previous_response_id 续接
- [x] GET /v1/models（仅对客售价，无成本/上游泄露）
- [x] 输入/思考/最终输出分离录制（默认：输入 full、思考与最终输出 off）+ 敏感字段脱敏
- [x] 执行运行时（`internal/runtime`）：内置/插件分派、熔断在途与延迟观测、额度冷却持久化
- [x] 凭据 AES-GCM 加解密（`internal/creds`，AAD=provider id）
- [x] 逐次尝试计量 + 限速票据结算；本地拒绝不计量
- [x] 端到端验证：httptest 全链路测试 + 真实二进制 curl 走查（非流式/流式/401/400/模型列表/用量落库）

## M6 MCP 查询服务（网关为 MCP Server）
- [x] 设计文档 docs/design/m6-mcp-server.md；规格 docs/mcp.md 状态更新
- [x] POST /mcp（JSON-RPC 2.0：initialize / ping / tools/list / tools/call）+ mcp_tokens 鉴权 + 账户强作用域
- [x] 首批 6 个只读工具：get_balance / get_ledger / get_usage_summary / list_requests / get_request / get_models
- [x] 跨账户访问按"不存在"处理；API Key 不能用于 MCP（必须账户级 MCP 令牌）；吊销/过期即 401
- [x] 内容可见性绑定录制开关（未录制返回 recorded=false + 原因）
- [x] 测试：6 个端到端用例（缺令牌/API Key 拒绝、initialize+tools/list、工具数据、未知工具与方法、跨账户隔离、吊销）
- [x] MCP-2：补齐 5 个工具（get_dashboard / get_usage_breakdown / get_rate_limits / list_invoices / get_invoice），共 11 个
- [x] MCP-2：聚合下沉到 SQL（不再受 max_query_rows 截断影响）+ TTFT P95 精度标注
- [x] MCP-2：`bin/aigw mcp-serve --account <name>` stdio 模式，复用同一个 Service（工具集不会漂移）
- [x] MCP-2：测试 5 个（聚合不受行数限制、分组、限额+账单、跨账户不可见、tools/list 覆盖新工具）+ 端到端实测（HTTP 11 工具、stdio 2 响应）

## M21 MCP 后台工具（让 MCP 能执行全部后台 API）
- [x] 设计文档 docs/design/m21-mcp-admin-tools.md；规格 docs/mcp.md 状态更新（对话中已展示并确认）
- [x] 单一路由表 `internal/httpapi/admin_routes.go`：86 条管理面路由的声明式表（Method/Path/Handler + 工具名/摘要/
  分组/角色/危险标记/路径参数/查询参数/请求体 schema/备注/不暴露原因），`routes()` 按表注册
- [x] 完整性测试：表 == 字面清单 86 条、注册 == 表、名称唯一且带 `admin_` 前缀、摘要/分组/角色齐备、
  `{param}` 与声明双向一致、危险必带原因、describe 负载可序列化
- [x] `mcp_tokens.scope`（迁移 0006，默认 query）+ 签发/列表/PATCH 改权限；scope 常量与角色映射在 `internal/mcpsrv/scope.go`
- [x] `mcpsrv`：`Principal`（含 `Actor()` 审计身份）、`Backend` 端口、`ToolResult`（isError 由执行方决定）、
  `ToolsFor(principal)`；`Handle` 改收 Principal；stdio 走 `ScopeQuery`
- [x] 渐进披露三工具：`admin_endpoints`（概要/过滤/分组）、`admin_describe`（参数+body schema+example+confirm 原因）、
  `admin_request`（进程内直调同一 handler）
- [x] 权限与安全：query 令牌看不到后台工具（调用报"未知工具"）、admin_read 写接口 403 并说明、
  危险接口必须 `confirm:true`、合成主体经未导出 context key 注入（HTTP 无法伪造）
- [x] 审计与可观测：每次调用两条 `mcp.admin_call`（started/ok|failed，只记 body 的键名不记值）、
  handler 自身审计归到 `mcp:<名称>#<id>`、`mcp.call` hook 事件
- [x] 响应处理：`mcp.admin_max_response_bytes`（默认 262144）截断并标记；text/* 原样文本；二进制只回元数据
- [x] 配置：`mcp.admin_tools`（默认 true，部署级熔断）、`mcp.admin_max_response_bytes` + `GW_MCP_ADMIN_*` 环境覆盖
- [x] 控制台：MCP 令牌页 scope 列（彩色徽标）、签发时选择 scope（含人话解释）、"改权限"弹框、admin 令牌签发后的警示
- [x] 测试：路由表 4 例 + 桥 11 例（含 schema 合法性、截断、隐藏接口、端口未接线、审计、非伪造）+ mcpsrv 4 例 +
  store 1 例 + config 1 例；`make verify` 全绿、`make ui-check` 82 项断言全绿
- [x] 真机走查（隔离实例 8093 + 全新库）：签发 admin 令牌 → `initialize`/`tools/list`（14 工具）→
  `admin_endpoints`（total=86）→ `admin_describe`（body schema + confirm 原因）→ 用 MCP 建供应商/上游模型/对客模型/路由/
  定价倍数/API Key → `admin_explain_router` 候选为空、`excluded` 为空 → 真实 `POST /v1/responses` 返回 `echo: ping` →
  `admin_account_balance` 读计费 → 吊销令牌后 401；query 令牌 `tools/list` 只有 11 个工具
- [x] stdio 模式的后台工具（M42）：设计已展示并确认；新增 `--endpoint` / `--token-env` 转发运行中网关，复用令牌权限和热更新。输入/响应限长、超时、取消、禁止重定向及不重试；三种 scope、真实管理写入读回、吊销与 base_path 集成测试通过，`make verify` 通过。设计：`docs/design/m42-stdio-admin.md`。

---

## M7 Hooks 与录制

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m7-hooks-recording.md

- [x] 异步 worker 池 + 有界队列（满则丢弃并计数，绝不阻塞请求）

- [x] Webhook HMAC-SHA256 签名（`t=…,v1=…`）+ 事件头 + 投递 ID；提供 VerifySignature 供接收方校验

- [x] 指数退避重试；4xx 不重试；最终失败写死信 JSONL；JSONL 投递器（本地审计）

- [x] 事件过滤（白名单/通配）与采样率；`include_content` 控制内容附带并受 max_bytes 截断

- [x] hook 配置持久化（`hooks` 表 List/Upsert/Delete）+ SetHooks 热替换

- [x] 请求路径接入：response.completed / response.failed 事件

- [x] 事件接入：response.completed/failed、request.denied、billing.inflight_warn/throttle/abort、billing.reconcile_mismatch、backup.finished/failed

## M8 管理面 REST API
- [x] 设计文档 docs/design/m8-admin-api.md（面隔离、会话、热更新三件套、审计、分页）
- [x] 口令哈希：标准库 PBKDF2-HMAC-SHA256（21 万次迭代 + 16 字节盐，格式可平滑升级）
- [x] 会话：`sess_<随机>` + 每会话随机令牌，**库里只存哈希**，12h 过期；`admin_users`/`admin_sessions` 表
- [x] 登录限速（按客户端键的失败窗口，超限 429）；失败信息统一（不区分用户不存在/口令错误）
- [x] 测试：哈希往返与加盐、登录/鉴权/退出全链路、错误口令与限速、过期会话、角色检查
- [x] 管理面 HTTP 路由与中间件（Cookie 鉴权、401/403、admin 角色校验、与公开面隔离）
- [x] 端点：auth/login|logout|me、stats、keys（列表/创建/改录制开关与状态）、requests（列表/详情）、audit-logs
- [x] 热更新三件套接线：凭据缓存失效（定向/全局）+ registry 原子换新快照 + 审计
- [x] 引导管理员账户（口令 PBKDF2 哈希入库）；API Key 明文仅在创建时返回一次
- [x] 资源 CRUD：providers / models / model-mappings / routes / tags / keys（双勾选）/ mcp-tokens / hooks
- [x] 热更新接线：授权相关写操作全量失效 key 缓存、路由类写操作换新 registry 快照、hook 写操作重载投递器

### M8b 管理面资源 CRUD（设计：docs/design/m8b-admin-resources.md）
- [x] 端口化：AccountAdmin / ProviderAdmin / ModelAdmin / TagAdmin / HookAdmin / MCPTokenAdmin / SettingsAdmin / Sealer / Prober（每个资源族一个窄接口，假实现只需 5-8 个方法）
- [x] providers：列表/新建/单条/更新/删除（引用守卫 409，?force=true 级联）、探测 test、模型发现 refresh、actions、logs、restart
- [x] 凭据：明文只在内存、AES-GCM 密封后入库、响应只回 has_credentials 与字段名；不传=保持不变、{} = 清空；配置/凭据变更则 config_version++ 并停止插件进程
- [x] models / model-mappings / routes / tags：按名或按 id 定位、引用校验（model/provider 必须存在）、映射规则复用 modelmap.ValidateRule
- [x] mcp-tokens：签发（明文只回一次，aigw_mcp 前缀）与吊销；hooks：webhook 强制 https（hooks.allow_insecure 可放开）、sample_rate ∈ (0,1]、写后重载投递器
- [x] settings：GET ?key=a&key=b / PUT /settings/{key}（接受裸值或 {"value":…}）
- [x] runtime.Probe/Actions/RunAction/Logs/Restart：httpapi 不依赖 pluginhost，探测失败以 200 + ok=false 表达为业务结果
- [x] 测试：11 个管理面用例（未带 Cookie 401、viewer 写 403、凭据生命周期、kind/JSON/负数校验、路由引用与删除守卫、探测失败持久化、模型发现只补空值、hook 重载与校验、settings/MCP 令牌往返、映射校验）
- [x] gofmt 全仓清理（34 个文件的对齐差异）

---

## M9 Web 管理界面

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m9-web-console.md（已在对话中输出）

- [x] internal/webui：go:embed 静态资源 + SPA 回落 + CSP + 缓存头；`/admin/ui/` 与 API 同源同会话

- [x] 原生 HTML + ES modules + fetch，零构建零依赖（环境无 node/npm，界面必须浏览器直跑）
      ——**M50 起**：源码仍是零构建（浏览器直跑、测试直读），但发布二进制里的副本改由 `make build` 压缩混淆后嵌入

- [x] 页面：概览 / API Keys（双勾选录制）/ 账户 / 标签 / MCP 令牌 / 供应商（详情·探测·发现·日志·动作·重启）/ 模型与路由 / 映射（含试算器）/ 请求日志（输入·思考·输出分栏）/ Hooks / 审计 / 设置

- [x] 定价 / 账本 / 账单 / 对账 / 备份 五个占位页（明确标注 M11/M12/M16，不做假交互）

- [x] 新增 `GET /admin/api/v1/router/explain`：复用数据面 Plan，返回解析结果、有序候选、排除原因与失败原因

- [x] 管理面 CSRF 收紧：POST/PATCH/PUT 必须 `Content-Type: application/json`

- [x] 测试：webui 资源/回落/CSP 用例 + CSRF 与 explain 端点用例（真实 store）

- [x] Node 语法检查与导入图校验（13 个页面模块，0 问题）

## M11a 计价引擎
- [x] 设计文档 docs/design/m11a-pricing.md（已在对话中输出）；规格 docs/pricing.md 状态改为已实现
- [x] internal/pricing：纯函数 Evaluate（成本侧/售价侧同一条路径），int64 + ceil，禁浮点
- [x] 有序规则集：首命中、catch-all 强制、时段（含跨午夜与星期归属、内嵌 tzdata）、档位半开、valid_from/to、变体
- [x] 成本/售价双规则集：cost_follow（含按维度覆写倍率）| absolute，互斥语义明确
- [x] 校验（400）+ 遮蔽检测（告警，不拒绝）；快照内联命中规则完整副本，可脱离规则表复算
- [x] 管理面：POST /pricing/simulate（可内联规则做「改了会怎样」预览）与 POST /pricing/validate
- [x] 测试：13 个引擎用例（首命中/时段/跨午夜/档位/取整/倍率/最低收费/校验/遮蔽/快照复算）+ 2 个 HTTP 用例
- [x] 对客价以倍数为主路径：生效链 key > tag > account > model > default，快照记录 markup_source
- [x] `PATCH /pricing/markup`（只改倍数、保留规则数组）与 `GET /pricing/targets`（一次拉全、含成本规则缺失与生效来源）
- [x] 迁移 0005：`accounts.markup_override_set`（区分「未设置」与「设为 0」）
- [x] 控制台 Pricing 页：倍数标签页（×↔bp 联动、按维度覆写、实时试算、空转红色告警）+ 高级标签页（规则 JSON、校验、遮蔽告警、阶梯预览、模板）
- [x] 测试：ResolveMarkup 优先级 6 例 + 坏策略容错、targets/markup 端点用例；端到端验证 1.0×/1.5×/账户 2.0× 三档与账本一致性

## M11b 账本与在途额度
- [x] 设计文档 docs/design/m11b-ledger-inflight.md（已在对话中输出，分两批提交）
- [x] **M11b-1**：`store.SettleBatch` 单事务结算（用量 + 账本 + 余额 + 计数器一次落盘）
- [x] 迁移 0002：`usage_records(request_id, attempt_no)` 唯一索引（重放幂等的前提，测试抓到的真缺陷）
- [x] 单写者批处理写入器：攒批/定时 flush、队列满同步直写、批失败降级逐条、再失败落兜底
- [x] 兜底：`billing-fallback.jsonl`（fsync）+ `billing_failures` 表 + 启动与每 60s 幂等重放 + 清空已重放行
- [x] 在途预留表（TTL + 心跳 + GC）与准入判定（预付 ≥0、后付到授信、overdraft 策略）
- [x] `pricing.WorstCaseRates` + `billing.EstimateReserve`：按最贵档位/时段预留，防止预付超卖
- [x] 四条不变量巡检 + `rebuild-ledger`（默认 dry-run，apply 单事务重写 charge 并重算余额）
- [x] 管理面：/billing/invariants、/billing/status、/billing/rebuild-ledger、/accounts/{id}/balance、/accounts/{id}/ledger
- [x] 测试：6 个用例（原子性+幂等、批量回滚、写入器批量与兜底重放、预留与准入、预留估算、巡检与重建）
- [x] **M11b-2**：接入 `/v1/responses`——余额准入（402 在发起上游之前）+ 单次预留与释放 + 逐尝试计价（成本/售价/快照）+ 结算走批处理写者
- [x] 失败尝试记成本不计费（`charge_on_error` 控制），并把 `usage.charge_micros` 同步置 0 以维持不变量
- [x] 端到端实测：402 拒付不写用量；有余额时 cost=7 / charge=11（1.5× ceil）、账本 topup→charge、余额 4999989、四条不变量全绿
- [x] **M11b-3**：流中在途策略——纯函数决策（continue/warn/throttle/abort）、`throttle` 用上游背压实现、`abort` 经 ctx 取消触发 `provider.cancel`
- [x] abort 决策点冻结可计费用量，之后的用量只记 `overshoot_cost_micros` 不 charge；配额中断仍按已计量部分计费
- [x] 长调用预留心跳（按 `reservation_heartbeat_s` 续期，避免 GC 误收）
- [x] 测试：`billing.DecideInflight` 表驱动 10 例、限额计算、guard 无上限时惰性、overshoot 只算成本、端到端流式 abort
- [x] 端到端实测：`response.failed` + `insufficient_quota`、usage(42/42, aborted_quota)、账本 charge −42、余额 999958、不变量成立

## M12 账单 / 充值 / 对账补偿
- [x] 设计文档 docs/design/m12-invoices-credits-reconcile.md（已在对话中输出）；规格 docs/billing.md 状态改为已实现
- [x] 账期纯函数 `billing.PeriodFor`（自然月 / period_start_day / 时区，复用 `pricing.LoadLocation`）
- [x] `invoice_lines` 物化（json_extract 聚合 model/key/day）+ 幂等 `(account, period)` + draft 可重算 / issued 冻结
- [x] 状态流转 issue / void / pay（后付还款写 `topup`，幂等键 `invoice:{id}:payment`）+ CSV 导出
- [x] 充值四类：topup / credit_grant / adjustment / refund，幂等键 = kind:ref_id；到账按 auto_resume 自动恢复账户
- [x] 兑换码：批量生成（明文只回一次、库里只存哈希）、条件更新核销（并发仅一次成功）、失败释放占位、过期拒绝
- [x] 对账：用量 vs 账本汇总、差异样例（≤100）、估算占比、不变量结果并入 details、差异触发 `billing.reconcile_mismatch` hook
- [x] 失败重放：兜底文件 + `billing_failures` 表，成功标记 resolved、失败累加 retries
- [x] 修复 `AppendLedger` 幂等判定缺陷（重复 ref_id 曾被判为已入账）
- [x] 测试：账期（含时区边界）、账单全生命周期、充值幂等与自动恢复、兑换码单次核销、对账发现差异与记录
- [x] 端到端实测：充值（重复提交不重复入账）、兑换码（二次核销 409）、对账 diff=0、账单生成/issue/CSV
- [x] 控制台页：账本与充值（余额/在途/流水/充值表单）、账单（生成/签发/已付/作废/CSV）、对账（触发/历史/不变量/重放失败结算）
- [x] 赠送额度到期冲销：迁移 0003（`ledger_entries.expires_at`）+ FIFO 归属模型 + 幂等键 `expire:<grant>` + 启动与每 24h 调度 + 手动触发端点
- [x] 测试与实测：只冲销未用部分、余额不为负、二次运行幂等、不变量保持 ok（设计记录 docs/design/m12b-credit-expiry.md，**本轮为先实现后补文档**）

---

## M13 性能与并发

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m13-performance.md（已在对话中输出，含实测数字与两处修复）

- [x] 微基准：pricing.Evaluate、routing.Plan（含并行）、httpapi 端到端（非流式/并行/模型列表）

- [x] `cmd/loadgen`：标准库压测器（并发、时长、流式开关、rps 与 p50/p90/p95/p99）

- [x] `scripts/load.sh`：一键起服务 + 建资源 + 压测 + 账单/不变量自检

- [x] `server.pprof: true` 时挂载 `/debug/pprof/`（默认关闭）

- [x] **压测抓到并修复**：写入器重试复用过期 context → 整批落兜底文件（11 条）

- [x] **压测抓到并修复**：不变量巡检跨两次查询读快照 → 高并发下误报差异（改为单只读事务快照）

- [x] 实测：32 并发 × 10s → 619.6 rps、p50 1.73ms、p95 44.5ms；usage 与 ledger 逐条对齐、不变量全绿、无兜底文件

## M15 模块解耦验证
- [x] 设计文档 docs/design/m15-decoupling.md（已在对话中输出）
- [x] `internal/arch` 分层断言：读取 `go list` 真实依赖图，逐包比对允许的模块内依赖
- [x] 三条关键禁令：httpapi 不得直接 import pluginhost/creds；只有 cmd/aigw 能同时 import store+httpapi；任何包不得 import cmd/
- [x] `examples/` 与 `pkg/` 同样受检（插件作者代码不得依赖 internal；新增 provider-codex 已登记）
- [x] 接口替身：`billing` 用 `proxyStore` 证明只依赖端口；httpapi 的端口假实现已在 M8b/M16 覆盖
- [x] 规则表修正 5 处与实际 import 图的偏差（pricing→domain、providers 子包、examples、arch 自身）

---

## M16 数据库自动备份

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m16-backup.md（已在对话中输出）；规格 docs/backup.md 状态改为已实现

- [x] 一致点：`VACUUM INTO` 产出紧凑单文件（不阻塞写入）+ `quick_check` 校验 + `backup_jobs` 落库

- [x] cron 子集解析（`*` / 列表 / 区间 / 步长 / mon-jan 名称）+ 下次触发计算 + 调度器（重启不补跑）

- [x] 保留策略：每日 N + 每周 M + 每月 K 的并集，校验失败的快照永不自动删除

- [x] 崩溃自愈：启动时把遗留 `running` 任务标记为 failed(interrupted)

- [x] 两阶段冷恢复：restore 暂存 `<db>.restore-pending`，启动时替换并保留 `<db>.pre-restore-<ts>`

- [x] 管理面：列表（含目录/占用/下次触发）、手动触发、删除、下载（仅 admin）、暂存恢复、手动清理

- [x] 测试：cron 下次触发 5 例+非法表达式、保留计划、备份产出与校验、校验失败保留文件、冷恢复替换（含 WAL 清理与二次调用幂等）

- [x] 端到端实测：手动备份(quick_check=ok, 274KB)、下载后 quick_check=ok 且数据完整、restore 缺 confirm 400、带 confirm 暂存、重启后自动替换且数据可读

- [x] 控制台页：备份（列表/手动触发/下载/两阶段恢复/删除/按策略清理，含目录占用与下次触发时刻）

- [x] 控制台页：兑换码（批量生成一次性明文、按批次过滤、核销到指定账户）——至此所有资源都有界面

- [x] 收尾修复：管理面请求日志列表把 `account_id=0` 当字面量匹配，导致「全部账户」视图恒为空（端到端走查发现）

- [x] 收尾：README 状态与文档索引刷新；`UsageTotals`/`UsageBreakdownRow`/`UsageCounter` 上移到 `domain` 以保持分层断言通过

---

## M17 完善内置供应商（openai-chat）：DeepSeek 适配与思考模式

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] **实测发现的语义缺陷 → 已修复（M39，2026-09-13）**：`response_format` 被**无条件下发**到每个上游请求
  （`internal/providers/openaichat/openaichat.go:432`），且该 provider **从不读取请求里的 `text.format`**。
  但 `docs/api-providers.md` 把它定义为"上游真实支持到哪一档"的**能力申报**——两者语义冲突：一旦声明
  `json_object`，**所有**请求都被强制 JSON 模式。真实后果（2026-09-11 实测）：DeepSeek 对任何不含 "json"
  字样的提示词直接 400（`Prompt must contain the word 'json' in some form to use 'response_format' of type
  'json_object'`），该供应商因此只能服务 JSON 类请求、普通流量全失败（DSH 的真实流量即如此）。
  方案定为"**能力上限 + 按请求档位下发**"：档位由 `req.Text.Format` 决定，配置只作能力申报；
  `config.example.yaml` 的 DeepSeek 片段与 `docs/api-providers.md` §2/§3 同步改写。
  **2026-09-13 复现**：线上 deepseek 供应商的该键又被加回（经管理面写入数据库，不在 `config.yaml` 里），
  DSH 选 `deepseek-flash` 首轮即 `upstream_400`——这正是只改部署、不改代码的代价，故本次一并修代码。

- [x] **用 DSH 本体做端到端验证（deepseek-flash，2026-09-11）**：DSH 默认模型是 `aigw/gpt-5.6-luna`
  （`~/.dsh/settings.yaml` 的 `agent-default-model`），验证时改用 `deepseek-flash`。沙箱内 `~/.dsh` 只读、
  DSH 启动 profile 要写 `profiles/*/package.json`，故用 `DSH_HOME` 重定向搭等价配置（拷 `settings.yaml`
  与 headless profile、`node_modules` 用符号链接、密钥只走 `AIGW_API_KEY` 环境变量不落盘）：
  ```sh
  export DSH_HOME=<工作区内的临时目录> AIGW_API_KEY=<网关 Key>
  node <DSH>/lib/bin.js --profile headless "用一句话回答：1+1 等于几？不要使用任何工具"
  ```
  - 结果：stdout `1+1 等于 2。`、**退出码 0**；网关侧 `model=deepseek-flash provider_id=15 status=completed`，
    真实用量 `input_cache_miss=7909 output=8`。
  - 带工具的任务同样通过：读 `README.md` 首行 → 输出 `# ai-gateway`、退出码 0；网关侧是标准多轮工具往返
    （14:24:44→45→46 三次调用，缓存命中 138 → 7552 → 7808），说明 system 提示词（约 7900 token）、
    tools 定义、工具结果回灌、缓存计量四条链路都正常。

- [x] 设计文档 docs/design/m17-openaichat-deepseek.md（已在对话中输出）+ 规格文档 docs/api-providers.md（状态：已实现 M17）

- [x] 决策：内置而非插件（`pkg/providerkit` 翻译层 367 行 + `internal/` 97 行插件不可引用，做插件等于复制两份并各修一遍）；不新增 `deepseek` kind（只能是预设壳，却要改 registry 三处 + 分层表）

- [x] 能力开关（默认=既有行为，升级不改任何部署）：`thinking.mode/style/replay_reasoning_content`、`response_format`、`default_max_output_tokens`，枚举非法即构建失败

- [x] 出站翻译：`reasoning_content` 回传（存在工具调用时）、`thinking:{"type":enabled|disabled}` + `reasoning_effort`、`response_format` 按档位下发

- [x] 入站翻译：`reasoning_content` → `reasoning` 项（非流式）与 `reasoning.delta`（流式，先于正文）

- [x] 共享翻译层修复：流式工具参数增量**沿用首块 call_id**（原先发空）、`insufficient_system_resource`/`aborted` 视为上游失败可切换、`content_filter` → incomplete、空 `choices` 不再产出空状态

- [x] 错误分类：402（HTTP 或体 `code`）→ `quota_exhausted` 冷却、429 保留 `Retry-After`、401/403 → `fatal token_invalid`、400/422 → fatal 且保留上游 message、5xx/超时/网络 → retryable

- [x] 网关侧思考续接：`reasoning` 输出项正文写入 `content:[{type:"reasoning_text"}]`（与上游 Responses 形状一致）并随 output JSON 落库，`decodeStoredItems` 回填 → `previous_response_id` 的 thinking+tools 多轮不再 400（**未新增迁移/列**，见设计文档第 7 节差异）

- [x] 测试：providerkit 8 例（含「默认不变」断言）、openaichat 15 例（开关矩阵、用量、错误分类、response_format、Health）、httpapi 3 例（续接往返、空值兼容、上游 reasoning 形状）

- [x] 走查脚本 `scripts/deepseek-smoke.sh`：离线假上游校验请求形状/思考开关/流式顺序/用量维度/错误映射/续接回传；`--live` 打真机

- [x] 真机走查（2026-09-11，api.deepseek.com / deepseek-flash）：非流式带回 reasoning 项与 reasoning_tokens=27、工具轮思考经 `previous_response_id` 续接被上游接受（丢正文即 400）、流式思考先于正文且 `response.completed` 带 usage

- [x] 文档：`config.example.yaml` 与本地 `config.yaml` 增加 DeepSeek 现成片段；README 文档表与状态刷新

- [x] 顺手修复：`internal/arch` 分层表缺 M14(1) 新增的 `internal/sessionauth`/`internal/portal`（该里程碑提交后 `make verify` 一直红）

## M18 控制台展示供应商配置说明（内建 kind 的字段文档）
- [x] 设计文档 docs/design/m18-provider-config-docs.md + 规格文档 docs/provider-ui.md（先行，已在对话中展示）
- [x] 内建供应商包各加 `schema.go`：`Schema()`（config + credentials 的 JSON Schema 子集）/ `Note()`（kind 级说明）/ `Template()`（可复制模板）
- [x] `internal/providers`：`KindSchema{Kind,Note,Config,Credentials,Template,Source}` + `Schemas()` / `SchemaFor(kind)`；未知与 `plugin:` kind → `Source=unknown|plugin` 且 note 指向插件 handshake
- [x] `GET /admin/api/v1/provider-kinds`（新增，内建 kind 全集）；`GET /providers/{id}` 增 `config_schema`/`credentials_schema`/`kind_note`/`schema_source`（内建即有；插件复用 `discovered` 里上次握手的 schema，**不隐式启动进程**）；列表端点保持精简
- [x] 字段级事实写进 schema：`default`/`x-required`/`x-advanced`/`x-prefer-credential`；`api_key` 在 config 里标注为「建议填凭据栏」并写明「只填一处」
- [x] 控制台：列表工具栏「内建类型说明」（note + 字段表 + 凭据字段 + 模板 + 用此模板新建预填）；详情页「配置说明」区块（builtin 直渲 / plugin 未声明时给「读取插件声明」按钮 / 未声明但已握手时直接渲染 / unknown 明确提示）
- [x] 测试 internal/providers：kind 全覆盖、反射双向防漂移（Config 的 json tag == schema properties）、schema 合法性（type/default/enum/x-required/x-secret）、模板键 ⊆ schema 键、凭据 schema 含 `api_key`；两处变异验证（加字段不写说明、删 description 各自精确失败）
- [x] 测试 internal/httpapi：`/provider-kinds` 200 且含三个内建 kind、字段与密钥标记齐全；详情返回非空 `config_schema` 且无凭据明文；`plugin:does-not-exist` → `schema_source=plugin`、无 schema、**Prober 调用次数不变**
- [x] make verify 全绿（含 internal/arch 分层断言，本轮未新增分层边）
- [x] **验证方式升级为真实浏览器自动化**（原计划人工走查）：`scripts/ui-harness/`（API 快照 + headless firefox + `/report` 回报）与 `make ui-check`；5 个视图 40 项断言全绿（docs/detail/create/plugin/plugin-cached），无 firefox 或 python3 时自行 skip
- [x] 弹框统一在右上角带圆形关闭按钮：`ui.js` 新增 `closeButton()`/`modalHead()`，`modal()`、`confirmDialog()` 与 8 处页面手写弹框（providers 详情/内建类型说明/动作结果、billing 账单详情、keys 密钥明文、codes 兑换码、mcp 令牌、requests 详情）全部改走同一套头部；`app.css` 补 `.modal-head`/`.modal-close`（28px、`border-radius:50%`、hover 与 focus-visible，确认类弹框用 danger 配色）。`closeButton`/`modalHead` 是本次新增的公共 API，`modal()`/`confirmDialog()` 的签名与「取消 → `null`/`false`」语义未变
- [x] 上述弹框改动的验证：`make ui-check` 新增 5 类断言（✕ 存在、按渲染后的 border-radius 与实际尺寸判定为圆形、位于头部且是 dialog 首个元素、在标题右侧、`aria-label="关闭"`；detail 视图另加「点击 ✕ 后整个 backdrop 被移除」与「关闭后可重新打开」），5 个视图 62 项断言全绿；真实浏览器另核对了 `modal()`/`confirmDialog()` 的关闭返回值（`null`/`false`，与「取消」一致）及按钮几何（28×28，距弹框上/右各 19px = 18px padding + 1px 边框，即贴齐右上角）；`go test ./...` 与 `make build` 全绿
- [x] **修复：✕ 会跟着内容一起滚走**（用户实机反馈「关闭按钮要在弹窗的框上，不能滚动」）。根因：滚动容器是 `.modal` 本身（`overflow:auto` + `max-height:86vh`），头部在它内部，于是长弹框（请求详情 33 字段表、供应商详情、账单明细）往下滚时 ✕ 一起被推出画面。现在 `.modal` 改为 `flex-column` + `overflow:hidden`（不再是滚动容器），只有新增的 `.modal-body` 滚动，头部与 `.modal-actions` 用 `flex:0 0 auto` 固定在框上；`ui.js` 相应新增 `modalBody()`/`modalActions()`，11 个弹框全部改为此三段结构
- [x] 上述修复的验证（含变异测试）：`ui-check` 新增 `closeOnFrame`/`bodyScrollable`/`dialogNotScrollable`/`closeStaysPutAfterScroll`/`closeVisibleInFrame` —— 先经 CSSOM 把弹框压到 160px 逼出真实溢出，再同时滚动 body 与 dialog，断言 ✕ 位置不变且仍在弹框可视框内，并断言「body 确实滚动了」（否则位置断言无意义）。**变异验证**：把 CSS 还原成修复前写法，恰好 4 项断言失败（`bodyScrollable`/`dialogNotScrollable`/`closeStaysPutAfterScroll`/`closeVisibleInFrame`）并 exit 1，确认断言对该缺陷有咬合力；修复版下 5 视图 82 项断言全绿，`go test ./...`/`make build` 全绿
- [x] 回填设计文档「实现与设计差异」，单提交并引用设计文档路径
- [x] 顺带修复：`docs/plugin-protocol-v1.md` 第 3 节引用的 `docs/provider-ui` 此前并不存在（本轮补上），并把「建议每个插件声明 schema」写进协议文档（不改协议、不加校验）

---

## M19 面向真实客户端的方言翻译（核心只接受，翻译在 provider 层）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m19-client-dialects.md + 规格文档 docs/api-responses.md（工具类型不再被拒）与 docs/api-providers.md（出站方言翻译小节）

- [x] 触发场景（约束：**不能改客户端**）：Codex CLI 0.153.4 指向网关后撞三处 —— `tools[].type="namespace"` 与 `"web_search"` 被网关 400；`input[].role="developer"` 被 DeepSeek 400（`unknown variant 'developer'`）

- [x] 决策 D1：`internal/responses` 是面向客户端的表面，只按 Responses 规范接受，不再判断"本网关支不支持某工具类型"（把上游能力误当客户端合法性，会让整条请求因一个可选工具失败）

- [x] 决策 D2：协议层保真 —— `pluginapi.Tool` 与 `responses.Tool` 增加 `Raw`，自定义 `MarshalJSON`/`UnmarshalJSON` 让未建模类型**逐字节**进出；否决"核心丢弃 + 上报降级"（不可逆，且仍是核心替上游裁决）

- [x] 决策 D3：方言翻译归 provider —— `providerkit`（chat 方言）：系统级角色归一化为 `system`、非 function 工具不下发；codex 插件（codex 方言，M10c/M10d）保持 `system → developer`、丢 `max_output_tokens`、恒流式

- [x] 决策 D4：`featuresOf` 的 `tools` 特征改为"存在 function 工具"（`HasFunctionTools()`），避免让无法执行的工具类型影响候选能力判定

- [x] 测试：responses 3 例（未建模类型逐字节保留且未被折成 function / function 缺 name 仍 400 / `HasFunctionTools` 忽略未建模类型）、pluginapi 1 例（`Raw` 经协议往返不变、function 仍结构化）、providerkit 2 例（`developer→system` 且其它角色不动 / 非 function 工具不下发）；同步删除 httpapi 里"非 function 工具必须 400"的旧断言

- [x] make verify 全绿（vet + 全量测试 + build + `internal/arch` 分层断言）

- [x] **端到端验收：Codex CLI 零改动跑通** —— `multi_agent`、`web_search` 保持默认开启，仅把 `base_url` 指向新网关、模型设为 `deepseek-flash`：输出 `1+1 等于 2。`、退出码 0（此前网关 400，关掉工具后上游 400）

- [x] **端到端验收（带工具往返，2026-09-11，跑在重启后的真实实例 8088 上）**：任务「读取 README.md 第一行」→ Codex 实际执行 `exec /bin/bash -lc 'head -n 1 README.md'` → `# ai-gateway` → 回答 `# ai-gateway`、退出码 0。网关侧时间线正是**两次模型调用**（工具往返的定义）：
  `14:43:55 in=383 out=70 hit=5248`（模型产出工具调用）→ `14:43:56 in=251 out=5 hit=5504`（工具结果回灌后给出终答）。
  这一次把 Responses↔Chat 的双向翻译整条链路都验证到了：Responses 工具定义 → chat `tools`；chat `tool_calls` → Responses `function_call` 输出项；`function_call_output` → chat `tool` 消息；终答 → Responses 文本。

- [x] 环境备注（非网关问题）：在 DSH/harness 沙箱内跑 Codex 时，它自带的 bubblewrap 起不来（`No permissions to create a new namespace`，嵌套 user namespace 不被允许），工具执行会失败——此时**协议往返其实已经成功**（模型产出调用、Codex 尝试执行）。本轮用 `--dangerously-bypass-approvals-and-sandbox` 让闭环走完（任务严格只读，外层仍有 harness 沙箱兜底）；**在普通终端里不需要该参数**，`-s read-only` 即可。

- [x] 同时确认既有修复在重启后的实例上仍然有效：codex 供应商探测 `ok=true`（`latency_ms=31149`，印证 60s deadline 的必要性——31s 远超旧的 10s）；`gpt-5.6-luna` 带 system 消息的请求返回 `好的，1+1=2。`（M10d）；界面资源含 `.spinner`/`withBusy`/`探测中`（M10c 的探测动画）

- [x] 回填设计文档「实现与设计差异」（含验收结果与两项观察），单提交并引用设计文档路径

- [x] **修复：并行工具调用导致上游 400（DSH 实测报告）** —— 报错
  `upstream_400: An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'`。
  根因：Responses→Chat 翻译**每个 `function_call` 项各生成一条 assistant 消息**，于是相邻的并行调用变成
  `assistant(tool_calls=[a])` → `assistant(tool_calls=[b])` → `tool(a)` → `tool(b)`，而 chat 协议要求带
  tool_calls 的 assistant 消息**必须紧跟**应答其每个 tool_call_id 的 tool 消息——第一条永远无应答，整条请求被拒。
  复现（隔离实例）：两个相邻 `function_call` → 500；单个 → 200。
  修复（`pkg/providerkit`）：① `mergeParallelToolCalls` 把相邻并行调用合并进**同一条** assistant 消息；
  ② `repairToolSequences` 从另一侧补齐同一不变量——未被应答的 tool_call 剪掉、无对应调用的孤儿 tool 消息丢弃，
  并让 tool 消息按调用顺序紧随其后。后两条覆盖客户端无法避免的输入形态：**中断的轮次**（调用没有输出）与
  **被裁剪的历史**（输出还在、调用已被截断），它们此前会让整条请求一起失败。
  修复后实测：并行调用 200（原 500）、单调用、未应答调用、孤儿输出四种形态全部 200。
  同步修正两条夹具（`TestReasoningReplayStopsAtATurnBoundary` 与 `TestStoredReasoningSurvivesContinuation`）：
  它们此前构造的正是这种非法序列（未应答的 call / 只翻译存储项而不含其输出）；真实续接流程里存储项是**前置**
  再拼新请求项（`v1.go:154-155`），调用与输出成对出现，故夹具按真实形态补上输出，测试本意（reasoning 不跨轮次、
  存储思维链存活）不变。

---

## 可选

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] M10 订阅后端参考适配器 `examples/provider-codex`（默认禁用、非官方）：设计文档 docs/design/m10-subscription-adapter.md

- [x] 三种凭据形态与自动续期：refresh_token（OAuth 刷新，轮换落盘 + 单飞）> session_cookie（/api/auth/session 换取）> 静态 access_token

- [x] token_file 一次性导入（CLI auth.json 等）；到期前 60s/启动前 5min 刷新；401 后只重试一次

- [x] 流式逐事件翻译（文本/思考/工具调用/用量）、Complete 复用同一 Stream 聚合、缓存命中与 reasoning 维度拆分

- [x] 动作 whoami/refresh_session/set_token（挂在既有 Providers 详情）；凭据永不回显、不进日志

- [x] 测试 9 个（假端点覆盖轮换落盘、单飞、invalid_grant、429 reset、session 换取、401→刷新→重试、导入、whoami 不泄露）+ README

- [x] 端到端实测：真实子进程经宿主拉起，探测 ok=true、流式 2+1 增量、非流式文本与 usage 正确、账本 cost116/charge174（1.5×）、轮换 token 落盘

### M10b 出网代理（`proxy`）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m10b-codex-egress-proxy.md + 规格文档 plugin-protocol-v1.md §12（先行，已在对话中展示）

- [x] pkg/providerkit：`ParseProxyURL` / `MaskProxyURL` + 表驱动单测（含「忘记写 scheme」的友好提示）

- [x] 插件：`config.proxy` 与 `credentials.proxy`（凭据优先）、clone `DefaultTransport` + 动态 `Proxy` 函数（atomic 读，零竞争）、生效值变化时 `CloseIdleConnections`

- [x] 插件：非法代理**记录并上报**（原计划的 exit 2 被实测推翻：宿主只报 `handshake: EOF`，原因在控制台/网关日志/`/providers/{id}/logs` 三处都看不到）；`Health()` 与请求入口报 fatal `proxy_invalid`；`whoami` 回报脱敏代理与来源

- [x] 测试：插件 6 例（配置代理命中 / 凭据优先 / 未配置委派环境变量 / 非法配置经 Health+请求双路 fail-closed / 凭据非法 / 不泄露 userinfo）+ providerkit 2 例；并用变异验证测试非空转

- [x] make verify 全绿（vet + 全量测试 + build，含 `internal/arch` 分层断言）

- [x] 端到端（真实代理 `http://192.168.140.252:2334`，2026-09-11）：**全链路打通，网关已服务真实请求**。代理侧：出口国家 CN → PH，探测不再报 `unsupported_country_region_territory`；凭据侧：经代理**真实刷新成功**（新的 `expires_at`/`last_refresh_at`，轮换后的 refresh_token 已落盘）；模型侧：`gpt-5.6-luna` 下 `/v1/responses` 非流式与流式均返回正确文本，`usage_records` 落库（含 `reasoning` 维度拆分），零价不产生账本分录

- [x] **更正一处先前错误结论**：我曾依据 `GET /backend-api/codex/models?client_version=…` 返回 `{"models":[]}` 判定"该账号无 Codex 授权"，这是**错的** —— 同一账号用 `gpt-5.6-luna` 完全可用。该目录端点对这类账号**不能作为授权判据**。被拒的 id 实为 `gpt-5`/`gpt-5-codex`/`codex-mini-latest`/`o3`/`gpt-5.1-codex`（均为 `not supported when using Codex with a ChatGPT account`）。另：`/responses` 强制要求 `stream:true`（`{"detail":"Stream must be set to true"}`），插件恒以流式发送，故不受影响

- [x] 实测发现（M10 适配器缺陷）→ **在 M10c 修复**：`upstreamMessage` 只认 `{"error":{"message"}}` 与 `{"message"}`，不认上游实际使用的 `{"detail":"…"}`（FastAPI 形状），把「模型不受支持」「必须开流式」这类关键原因丢成无信息量的 `the upstream returned 400 Bad Request`；本次定位是靠手工 curl 才拿到 `detail` 原文

- [x] 实测发现（M10 适配器缺陷）→ **在 M10c 修复**：健康检查走 `base_url + /me`，该路径在此出口被 Cloudflare 挑战（403 + `cf-mitigated: challenge`，HTML 正文），被 `classifyResponse` 映射成误导性的 `token_expired`。**可见症状：业务请求全部正常，控制台却把该供应商显示为不健康**

- [x] 回填设计文档「实现与设计差异」（含 D7 修正与 7 个实现期发现），单提交并引用设计文档路径

- [x] 环境前置（已解决）：代理最初从本机不可达（0.12s 快速 RST，疑似只绑回环）；在客户端开启局域网监听后 `192.168.140.252:2334` 于 0.11s 连通

### M10c 健康探测改为真实流式补全

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m10c-codex-health-probe.md + 规格文档 plugin-protocol-v1.md §5（`provider.health` 三条约定）

- [x] 探测改为复用 `p.Stream` 发一次流式 "hi"；判据 = 请求成功 **且** 观测到终态 `EventUsage`（`Stream` 单独用会把被截断的流当成功）

- [x] 新增 `health_prompt`（默认 `hi`）与 `health_model`（默认首个配置模型）；删除 `health_path`（该端点在本出口被 Cloudflare 挑战，无法区分"健康"与"被拦"）

- [x] 探测 deadline 10s → 60s：实测流式 "hi" 经代理 TTFB 1.2–4.2s、总耗时 9.6–16.4s（另有 18.8s 样本），10s 必然误判

- [x] `classifyResponse` 识别 Cloudflare 挑战（`cf-mitigated` / HTML 正文）→ `upstream_challenge`（retryable），不再冒充 `token_expired`

- [x] `upstreamMessage` 增加 `detail` 解析（优先级 `error.message` → `detail` → `message`）

- [x] 不再向上游透传 `max_output_tokens`：实测该端点任何取值都 400（`{"detail":"Unsupported parameter: max_output_tokens"}`），且客户端带该参数经网关必然 500；改为丢弃并在 README 写明

- [x] 测试：新增 6 例（探测形状 / 截断流→`health_stream_incomplete` / `detail` 正文 / 挑战分类 / 无模型 / 不透传 `max_output_tokens`）；计划里的第 7 例（非法代理优先）已由 M10b 的用例覆盖，不重复新增。三道关键行为均用变异验证过（去掉终态判据、`detail` 解析、挑战识别各自精确失败）

- [x] make verify 全绿（含 `internal/arch` 分层断言）

- [x] 实测（隔离实例，新二进制）：探测 `ok=true`、`latency_ms=16601` —— **16.6s > 旧 10s deadline**，坐实 deadline 调整是必需项而非预防性；`last_error` 清空

- [x] 实测：带 `max_output_tokens` 的请求由 500 变为 200 + 正文 `好`；不带该参数与流式调用同时回归通过

- [x] 实测：刻意配错模型后探测直接显示上游原文（`The 'gpt-5-codex' model is not supported when using Codex with a ChatGPT account.`），不再是 `returned 400 Bad Request`

- [x] 实测的隔离方式（值得复用）：用 `:8099` + 库快照（sqlite backup API）+ 独立 `GW_PLUGINS_STATE_DIR`，并把凭据换成 **`access_token`-only**（该模式永不调用 token 端点），从机制上排除 refresh_token 轮换风险

- [x] 控制台「探测」加处理中动画（M10c 的直接后果）：`ui.js` 新增可复用的 `withBusy`（禁用按钮 + spinner + 秒数递增 + 结束/异常都还原）、`toast` 支持 `{sticky}`、`probe()` 补上原先缺失的错误捕获并在探测后刷新列表；用 node + 最小 DOM 桩跑 10 项行为断言，并在隔离实例核对新二进制的内嵌资源

- [x] 回填设计文档「实现与设计差异」（含实测结果与 8 条差异），单提交并引用设计文档路径

### M10d codex 适配器翻译 `system` 角色

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计文档 docs/design/m10d-codex-system-role.md + 规格文档 docs/api-responses.md「输入项类型」（网关原样透传角色，后端方言由适配器翻译）+ 适配器 README

- [x] 根因：客户端（DSH）把系统提示词作为 `input` 里 `role:"system"` 的消息项发送，订阅后端直接拒绝（`400 System messages are not allowed`）→ 凡用该形状的客户端**完全无法使用 codex 供应商**

- [x] 实测矩阵决定方案：`system` 在任意位置（首/中/并存的 instructions）都被拒；`developer` 在任意位置（含带 tools）都被接受；另注意 `input` 必须非空

- [x] 实现：`rewriteSystemRoles` 就地改角色名 `system → developer`（保留消息位置与语义、无需解析内容、不会让 `input` 变空），其余角色与 `instructions` 不动，且不修改调用方请求

- [x] 否决的方案：折叠进 `instructions`（会提升位置、需拼接内容、可能让 `input` 为空而触发另一个 400）

- [x] 测试 2 例（上游收到 `developer` 且调用方请求未被改 / 其它角色不动且无 system 时不拷贝），并用变异验证非空转

- [x] 端到端：DSH 真实失败形状（system 项 + tools + `max_output_tokens`）经网关 **3/3 返回 200**，同报文直连上游 **2/2 返回 200**；且系统提示词确实影响回答（自称"软件工程助手"），证明是语义保留的翻译而非丢弃

- [x] 横向对照：同形状打 `deepseek`（`openai-chat`）无角色问题 → 该约束是订阅后端特有，翻译放在适配器这一层是对的

- [x] **顺带发现的配置问题 → 已修复（M39）**：deepseek 供应商的 `config.response_format="json_object"` 是**供应商级全局套用**（`openaichat.go:477` 曾无条件写入，且该 provider 从不读取请求的 `text.format`），导致任何不含 "json" 字样的提示词被 DeepSeek 拒绝（`Prompt must contain the word 'json' ...`）→ 该供应商当时只能服务 JSON 类请求，普通流量全 400。修复见 `docs/design/m39-version-and-format.md`：档位改由请求的 `text.format` 决定，配置退回能力申报

- [x] 回填设计文档「实现与设计差异」（含端到端结果、与 deepseek 的对照、以及一次与本改动无关的瞬时 `server_is_overloaded`），单提交并引用设计文档路径

- [x] M14(1) 会话层抽取 `internal/sessionauth`（口令/会话/限速），`internal/admin` 改为薄适配器（既有测试全绿）

- [x] M14(1) 迁移 0004：`portal_users`/`portal_sessions`（用户名全局唯一、绑定唯一账户、级联删除）

- [x] M14(1) `internal/portal` 认证适配器（禁用账号拒登、按用户吊销会话）+ 管理侧门户用户端点（创建/重置/停用，一次性口令只回一次）

- [x] M14(1) 配置 `portal.{enabled,session_ttl_h,login_attempts,allowed_tags}`（默认关闭）+ 分层表新增两条边

- [x] M14(1) 测试：门户登录/鉴权/登出/全局登出/禁用拒登/限速 429 + 管理侧生命周期（一次性口令、不回显、重复 409、非法名 400）

### M19b 流式终态：截断不得伪装成完成（DSH 实测报告）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 触发场景：DSH 经 8088 的 aigw 供应商跑任务（`provider: aigw` / `deepseek-flash`），14:56–15:00 那次在
  第 60 步"没做完就停了"，网关侧**一条错误都没有**。排查结论：上游那次只产出 15 token 就结束（`usage_source=provider`、`output=15`），
  模型自己收尾；但顺着这条线索挖出网关流式路径的两个"把截断伪装成完成"的缺陷（客户端是从终态反推 stop reason 的，
  `response.completed` → `stop`，所以半句话会被当成模型的最终答复）

- [x] 缺陷 1（静默截断）：`io.EOF` 被当作流正常结束 —— `openaichat`/`openairesponses`/codex 插件三处
  `errors.Is(err, io.EOF) → break/return nil`，之后只要没有 `finish_reason` 就按成功上报，`v1.go` 再无条件 `assembler.Complete`
  → 上游连接中途断开 = 客户端收到半截文本 + `response.completed`，任务"无声中断"

- [x] 缺陷 2（截断无标记）：`finish_reason=length`/`content_filter` 在**流式路径**从不映射成 `incomplete`
  （`mapFinishReason` 只作用于非流式，`Assembler.Incomplete` 从未被调用）→ 被 token 上限截断也报 `completed`

- [x] 缺陷 3（协议字段）：`responses.Event` 的 `output_index`/`content_index` 带 `omitempty`，第 0 项被省略
  （`index>0` 才带）；Codex 的 `OutputTextDelta without active item` 即此，pi-ai 还会因 slot 查不到而丢弃增量

- [x] 协议：`pluginapi.Event` 新增 `finish`（`reason` 为上游终止原因原文）；SDK 把它写进 end 帧的 `finish_reason`，
  内建 provider 经 dispatcher 同样归一为 `StreamEnd`；`pluginapi.IncompleteReason` 统一"哪些原因算截断"
  （`length`/`max_tokens`/`max_output_tokens`/`content_filter`/`incomplete` → 截断；未知原因一律按正常结束，避免把健康回答标成截断）

- [x] provider：`openaichat`、`openairesponses` 要求"见到 `finish_reason`/`[DONE]`/`response.completed|incomplete`"才算结束，
  否则 retryable `upstream_stream_incomplete`；codex 插件同口径，并映射 `response.incomplete` 的 `incomplete_details.reason`

- [x] dispatcher：内建 provider 未上报终态即视为被截断（插件路径兼容旧 SDK：无 `finish` 事件时 end 帧仍为 `stop`）

- [x] host：终态原因 → `response.incomplete`（流式与非流式都覆盖）；`usage_records.terminated_reason=incomplete`
  （`status` 仍 `completed`，上游确实产出了这些 token）；钩子事件 `response.incomplete`

- [x] 前端/协议字段：`responses.Event` 自定义 `MarshalJSON`，按事件类型决定索引字段是否出现（`created`/`completed`/`error` 不带）

- [x] 测试：provider 3 例（截断流必 retryable、`length` 必上报、正常 stop 不受影响）+ openairesponses 4 例（新文件）+ host 5 例
  （切流 → `response.failed`、`length` → `response.incomplete`、非流式 `incomplete`、索引恒在、用量标记）+ SDK 2 例 + 事件序列化 2 例；
  6 处修复各做一次变异验证（改回即精确失败）

- [x] 文档：`docs/plugin-protocol-v1.md` §6.1（finish 事件与"流必须声明自己为什么结束"）、`docs/api-responses.md`（异常终止表 + 截断/切流判据 + 索引字段恒在）

- [x] 真机复验（隔离实例 `:8099` + 库快照，跑的就是新二进制；2026-09-11）：
  切流（`cut_stream`）→ `response.failed`，`error.message` 点名 `upstream_stream_incomplete`，且**不再出现**
  `response.completed`；`finish_reason=length` → 流式 `response.incomplete`（`incomplete_details.reason=max_output_tokens`）、
  非流式 `status=incomplete`；`usage_records.terminated_reason=incomplete`（`status` 仍 `completed`）。
  索引字段复验：`response.output_item.added`/`response.output_text.delta`/`response.content_part.added` 全部带
  `output_index:0`（`content_index:0`），不再有缺字段的事件。
  无回归复验（同一实例打真实 DeepSeek）：两轮工具往返都 `response.completed`（第 1 轮产出 `function_call`，
  第 2 轮回灌 `function_call_output` 后给出终答）。

### M19c 思考开关：客户端沉默不等于"关掉思考"（DSH 实测报告）
- [x] 触发：M19b 修复后 DSH 经 8088 仍会"任务做一半就停"，而**同一直连 DeepSeek 的客户端不会**。
  排查发现 `usage_records` 里经网关的所有 deepseek-flash 请求**几乎完全没有 reasoning 维度**
  （40+ 次调用里只有几个显式带 `reasoning.effort` 的探针请求有），而同模型的直连路径每步都有
- [x] 根因：`openaichat` 在 `thinking.mode=auto` 下把"请求里没有 `reasoning` 字段"与"客户端要求关闭"混为一谈，
  于是**恒发** `{"thinking":{"type":"disabled"}}`。DSH 指向网关的供应商（`api: openai-responses`，
  models 未声明 reasoning）**不发** `reasoning` 字段，因此每一步都在无思维链下回答；而 DSH 的内置 deepseek
  供应商按 pi-ai 的 deepseek 方言发 `thinking:{type:"enabled"}` + `reasoning_effort`，所以不受影响
- [x] 直连实测（同一提示词，`api.deepseek.com`）：不发 `thinking` 字段 → `reasoning_tokens=235`；
  `disabled` → 0；`enabled`+`effort=high` → 151。即 **DeepSeek 默认就是开思考**，"不发"与"关"完全是两回事
- [x] 修复：`auto` 的三种输入分开处理——有 `reasoning.effort` → 照办；显式 `none` → `disabled`（并清掉会误导的
  `reasoning_effort`）；**完全没有该字段 → 不下发 thinking 字段**，由上游默认决定。
  `mode=enabled/disabled` 的强制语义不变（`enabled` 下客户端给 `none` 时不再透传该 effort）
- [x] 测试：`TestDeepSeekThinkingSwitch` 矩阵改为 7 个子例（新增"沉默 → 不下发"与"mode=enabled 忽略显式 off"）；
  `scripts/deepseek-smoke.sh` 增加"沉默客户端 → 上游收到 `thinking=<absent>`"断言
- [x] 顺手修掉 `make smoke` 一条**过期断言**（与本次改动无关）：离线分支的续接用"紧接着发新用户消息"的形态，
  而该形态下上一轮工具调用未被应答，会被 M19b 的 `repairToolSequences` 按设计剪掉（上游拒绝没有 tool 应答的调用），
  没有调用自然没有思维链可回放 → 断言恒红。改成按真实形态回灌 `function_call_output` 后恢复有意义（`replay=True`）
- [x] 真机复验（隔离实例 `:8097` + 库快照，新二进制，真实 DeepSeek）：
  同一形态请求（不发 `reasoning`）reasoning 事件 19、`reasoning_tokens=19`（修复前 0）、工具调用正常；
  `effort=high` 20、`effort=none` 0（显式关闭仍然有效）；
  再用 DSH 本体（`DSH_HOME` 重定向 + `--profile headless`，供应商指向隔离实例）跑"读 README 首行"：
  退出码 0，网关侧三次调用 `reasoning=50/27` —— 整条 DSH→网关→DeepSeek 链路恢复思考
- [x] 文档：`docs/api-providers.md`（`thinking.mode=auto` 的三种输入 + 沉默即不干预的理由与实测数据）、
  `docs/design/m17-openaichat-deepseek.md` 差异节补记第 6 条、`config.example.yaml` 注释

### M19d 思考模式下工具轮的 `reasoning_content` 必须带键（DSH 实测报告）
- [x] 触发：M19c 之后 DSH 经网关报错
  `upstream_400: The \`reasoning_content\` in the thinking mode must be passed back to the API.`
  —— 思考一打开，上游的硬约束立刻显形（此前思考被关掉，所以从没触发过）
- [x] 真机把规则测清楚（`api.deepseek.com/deepseek-flash`，逐格 4 次）：
  `thinking=disabled` → 工具轮不需要该字段；**`thinking=enabled` 或缺省** → 带 `tool_calls` 的 assistant 消息
  **必须有 `reasoning_content` 键**（值可以为空串），缺键 4/4 报同一条 400；这与消息 `content` 是否为空无关。
  即"要的是键本身"，不是"要有正文"
- [x] 复现（隔离实例，修复前）：非流式 + DSH 形态（不回传 `reasoning` 项）→ HTTP 500 + 上述 400；
  带 `reasoning` 项 → completed；流式同形态 → `response.failed`（M19b 让它不再静默）
- [x] 根因：`ChatMessage.ReasoningContent` 带 `omitempty`，而"客户端没给思维链"与"不发这个键"在
  `providerkit` 里是同一件事 —— 无状态网关无法恢复客户端没有回传的思维链，于是只能不发键，整条请求被上游拒掉
- [x] 修复（`pkg/providerkit`）：
  ① `ChatMessage` 增加 `ReasoningRequired`（不参与 JSON）+ 自定义 `MarshalJSON`：需要时**带键写出**（值允许为空串）；
  ② `ResponsesToChatWithOptions` 在 `ReplayReasoningContent` 打开时，给**每个带 tool_calls 的 assistant 消息**打上该标记，
     有原文就用原文（客户端发了 `reasoning` 项），没有就用空串；
  ③ `itemReasoningText` 增加 `content` 兜底（正文放在 `reasoning_text` 内容项里的客户端同样能回放原文），
     与 M17「summary 优先、content 兜底」的口径一致
- [x] 测试：`providerkit` 3 条（缺 reasoning 项 → 必带空键；未开开关 → 完全不出该字段；正文在 `content` 里 → 回放原文）
  + `openaichat` 1 条（走 `renderBody` 的配置路径），两处都用变异验证过（改回即精确失败）；
  `scripts/deepseek-smoke.sh` 增加"工具轮不带 reasoning 项 → 上游仍看到该键"的离线断言
- [x] 真机复验（隔离实例 `:8095`，新二进制，真实 DeepSeek）：四种组合（流式/非流式 × 带/不带 reasoning 项）**全部 completed**
  （修复前"不带"的两种必失败）；再用 DSH 本体（headless，供应商指向隔离实例）跑"读 README 首行 + `wc -l`"：
  同一轮两次工具调用、退出码 0，网关侧 `reasoning=89/79/64/37/13/12` —— 思考与工具往返同时成立
- [x] 文档：`docs/api-providers.md`（该约束的实测结论 + 网关"有原文用原文、没有就空串"的策略）

### M19e 工具轮的「整段 assistant」都要带 reasoning_content（DSH 实测报告）
- [x] 触发：M19d 之后仍报同一条 400，但**只在模型先写一句话再调工具的那一步**失败（时好时坏）
- [x] 真机把规则补全（`api.deepseek.com/deepseek-flash`）：thinking 模式下**进入工具轮的整段 assistant 内容算一轮**——
  `[用户, 文本(无键), 工具轮(带键), 工具结果]` → 400；`[用户, 文本(带空键), 工具轮(带键), 工具结果]` → 通过；
  连续多条文本消息同样必须都有键；把文本与工具调用**合成一条** assistant 消息（`content`+`reasoning_content`+`tool_calls`，
  即上游自己产出的形态）→ 通过
- [x] 取证过程（值得复用）：失败请求因为客户端随即断开，`recordContent` 用请求 context 落库被取消，
  **请求日志没写成**（日志里只有 `recording request content failed ... context canceled`），所以一开始误判成"非流式"。
  改为从 DSH 会话记录里**重建**了那一步的请求（用第 8 步那条已落库的真实请求 + 第 8 步的输出与工具结果），
  在线上复现了同一失败，再离线用 `pkg/providerkit` 逐层定位
- [x] 根因：DSH 把 assistant 文本与工具调用作为**两条 item** 发来，网关翻成**两条** assistant 消息，
  第一条（纯文本）没有 `reasoning_content` → 上游按"这一轮缺思维链"拒掉整条请求
- [x] 修复（`pkg/providerkit`）：新增 `foldAssistantTextIntoCall` —— 在 `ReplayReasoningContent` 打开时，
  把紧邻工具调用之前的连续纯文本 assistant 消息**折进**工具调用那条消息（内容用换行拼接、空文本直接丢弃），
  使一轮 assistant 内容以**一条** chat 消息出行（与上游原生形态一致；未打开该开关时消息边界保持原样）
- [x] 测试：`providerkit` 1 条（一条 assistant 消息 + 内容保留 + 键与标记在位；未开开关时仍是 3 条 assistant 消息），
  变异验证过（改回即精确失败）；`scripts/deepseek-smoke.sh` 增加"文本 + 工具轮 → 上游看到 `assistant=1` 且带键"的离线断言
- [x] 真机复验（隔离实例 `:8094`，新二进制，真实 DeepSeek）：
  ① 重建出来的**那条真实失败请求** → `response.completed`（模型继续把任务做下去）；
  ② 回归：四种组合（流式/非流式 × 带/不带 reasoning 项）、"文本+工具轮"、DSH 第 8 步原样重放 → 全部 completed；
  ③ DSH 本体 headless 跑"先写一句 → 读 README 首行 → wc -l"（正是失败形态）→ 退出码 0
- [x] 文档：`docs/api-providers.md`（该约束的完整规则与网关的折叠策略）

### M19f 审计落库不跟随请求 context（诊断盲点修复）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 问题（M19d/M19e 定位时踩到）：请求硬失败（上游 400、流被切断）后客户端通常立刻断开，
  而 `persist` 用请求自身的 context 写请求日志与存储响应，于是**失败请求恰恰是唯一没有记录的**：
  日志里只留下 `recording request content failed ... context canceled`，请求正文得从客户端会话里重建才能定位

- [x] 修复（`internal/httpapi/v1.go`）：`persist` 用 `context.WithoutCancel` + 5s 超时派生的 context 写审计
  （保留 request id 等 context 取值，只丢掉取消）；钩子走内存队列本就不受影响

- [x] 测试：`TestFailedRequestIsRecordedAfterTheClientHungUp` —— 流式请求 + 300ms 延迟，在 provider 应答中途取消 context
  （模拟客户端挂断），断言请求日志仍然落库、状态 `failed`、正文完整、request id 保留；变异验证过（改回跟随 ctx 即失败）

### M20 DSH 侧可设推理档位：能力申报与档位表对齐（DSH 实测报告）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 症状：DSH 的模型菜单里，本网关（`aigw` 路由）的模型**没有「推理等级」入口**，而同为推理模型的
  DeepSeek 官方模型有。定位：`@deepseek-ai/dsh-llm-pi-ai` 只在 `model.reasoning` 为真时才暴露 `reasoning.efforts`；
  手写路由（pi-ai 目录里没有的网关）若不逐模型声明 `reasoningEfforts`，解析结果是 `reasoning: false` ——
  **客户端侧缺声明**，不是网关缺功能

- [x] 同时发现网关侧一处申报不准：`gpt-5.6-luna`（`provider_models` id 12）只声明了 `{stream,tools}`，
  于是每次带 `reasoning.effort` 的请求都被打上 `X-Gateway-Degraded: reasoning`。
  `degradation=strip` 只做标记、不剥离参数（`internal/routing/routing.go` 的 `checkCapabilities` 只产出名单，
  `v1.go` 只加响应头），所以功能能跑、失真在可观测性与 `least_latency` 对推理模型的降权

- [x] 实测矩阵（2026-09-11，运行中的 8088）：`deepseek-flash`（openai-chat）接受
  `none/minimal/low/medium/high/xhigh/max` 全部，`high` 产出 reasoning 项 + `reasoning_tokens`；
  `gpt-5.6-luna`（codex 插件）接受 `none/low/medium/high/xhigh/max`，`xhigh` 明显更慢（档位确实透传），
  但 **`minimal` 被上游 500 拒绝**（`unsupported_value`；同模型对非法拼写 `off` 回的是 `invalid_value`，两者可区分）；
  `replay` 忽略 effort 并稳定回报降级

- [x] DSH 侧改动（只改配置，不动客户端实现）：`~/.dsh/settings.yaml` 的 aigw 两个模型声明 `reasoningEfforts`
  （`gpt-5.6-luna` 刻意不含 `minimal`），补全 `contextWindow`/`maxTokens`，`off: none` 使「提供方默认」= 显式关闭思考；
  不设路由级默认档；`replay` 系列不声明（免得给出做不到的承诺）

- [x] 网关侧改动：`config.yaml` 的 codex 模型补 `capabilities.reasoning`（插件 `ListModels` 直接透传）、
  补 `public: gpt-5.6-luna` 发现条目、修 `deepseek.enabled` 的文件/运行态漂移；运行态用管理 API 把 id 12 的
  capabilities 更新为 `{stream,tools,reasoning}`（bootstrap 不回填非空 capabilities，只改文件对运行中的实例无效）

- [x] 测试：`internal/providers/openairesponses/body_test.go` 四例钉住"客户端 reasoning 原样进出站请求体"
  （chat 侧在 `pkg/providerkit` 已有等价断言）；变异验证过（`delete(payload,"reasoning")` 即精确失败）

- [x] 复验：`gpt-5.6-luna` 降级头消失；`deepseek-flash` 无降级头且输出 reasoning 项；`replay` 仍报降级；
  用 pi-ai 自己的 `Config` schema 解析改后设置段通过。设计记录：`docs/design/m20-dsh-reasoning-effort.md`

### M20b deepseek 供应商补 v4 系列模型（8088 运行态 + 文件基线）
- [x] 起点事实：运行中的 `:8088` 实例里 `deepseek` 供应商（provider id 15）只有
  `provider_models` 两条（`deepseek-flash` id 13、`deepseek-v4-pro` id 14）；对客 `models` 只有 `deepseek-flash`（id 6），
  `routes` 也只有它（id 5）。于是 `deepseek-v4-pro` **声明了却不可调用**（`not_mapped` 之外的模型根本不在
  `GET /v1/models` 里），`deepseek-v4-flash` 三层全缺
- [x] 改动（管理 API 直改运行态，**不需要重启**——bootstrap 在 upsert 语义下既不更新也不删除已存在的行）：
  `POST /admin/api/v1/providers/15/models` ×2（新增 `deepseek-v4-flash` id 16；把 id 14 刷成同一形状：
  `capabilities {stream,tools,reasoning}`、1M/65536、priority 30/weight 100）、`POST /admin/api/v1/models` ×2
  （对客模型 id 7/8）、`POST /admin/api/v1/routes` ×2（route id 6/7，priority 30/weight 100，均指 provider `deepseek` 且上游同名）
- [x] 验证（同一实例，改动后立即复验）：`GET /admin/api/v1/router/explain?model=…` 两个模型各出 1 个候选
  （provider `deepseek`、`excluded` 为空）；`GET /v1/models`（dev key）出现两个新 id；真实请求
  `POST /v1/responses`（`max_output_tokens:16`）——`deepseek-v4-pro` → `completed` + `output_text="ok"`，
  `deepseek-v4-flash` → `incomplete`（16 token 被思考吃满、非错误）+ reasoning 项，两者都带
  `X-Gateway-Provider: deepseek`、无降级头
- [x] 文件基线同步：`config.yaml` 的 `bootstrap.providers[deepseek].models` 补 `deepseek-v4-flash`，
  `bootstrap.models` / `bootstrap.routes` 各补两条（与运行态的 priority/weight 一致）；
  `docs/api-providers.md` §3 片段补 `deepseek-v4-flash` 并写明"只加供应商模型不够，三层齐全才对客可用"
- [x] 文件校验：临时程序 `.cache/cfgcheck`（在 gitignore 的 `.cache/` 下，未入库）用 `internal/config.Load`
  解析改后的 `config.yaml` → ok，`providers=3 models=6 routes=6`

---

## M22 多币种：模型级币种 + 账本换算 + 可选显示币种

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计：`docs/design/m22-currency.md`（已批准的口径：单一账本币种 + 结算换算；
> 成本/售价各自可配币种；汇率 config 静态表 + 设置页可覆盖；显示币种覆盖控制台并修正对外币种字段）。

- [x] `docs/design/m22-currency.md` 设计文档 + `docs/pricing.md` §9 / `docs/billing.md` §2 / `docs/api-responses.md` / `docs/mcp.md` 规格文档先行

- [x] `internal/config`：`billing.fx_rates`（整数微账本币种/单位）、`billing.display_currency`、删除死配置 `display_currency_rate`、五条新增校验

- [x] `internal/pricing/currency.go`：`FXTable`（ToLedger ceil / FromLedger half-up / 零值关闭换算）+ `FXStore`（atomic 替换）+ 单测

- [x] `internal/pricing`：`RuleSet.Currency` 校验与规整；`cost_follow` 跨币种「先换币再乘倍数」；`Result`/`Snapshot` 增币种、原生金额、入账金额与汇率；缺汇率 `fx_unavailable`

- [x] `internal/billing`：`EstimateReserve` 两侧各自换算后取 max、缺汇率回退 `reserve_micros_default`；`NewCharge` 写账本币种金额

- [x] `internal/httpapi`：Deps 注入 FX store、请求路径换算、写入校验 400、`GET /admin/api/v1/billing/currency`（路由表条目 + `expectedAdminPatterns`）、targets/simulate/`/v1/models` 币种字段、`PUT /settings/billing.fx_rates` 校验 + 热重载 + 审计

- [x] `cmd/aigw`：装配 FXStore / `ReloadFX`；bootstrap 里缺汇率只告警；`config.yaml` 与 `config.example.yaml` 同步

- [x] 控制台：`js/money.js`（BigInt 整数换算 + `≈` + 币种码）、顶栏显示币种选择器（记住选择）、accounts/billing/codes/pricing 四页替换硬编码 USD、定价页售价币种下拉与缺汇率徽标

- [x] `scripts/ui-harness`：新增 `currency` 视图（fixtures + 视图→模板映射），断言默认无 `≈`、切 CNY 后 `≈` + 正确数值 + 后缀、缺汇率红色徽标、无页面错误

- [x] MCP：`get_models` 的 `currency` 取模型售价币种；金额工具补不带 `_usd` 的字段与 `currency`（放在最后，避让 M21(2) 的并发改动）

- [x] 测试：`make verify` 全绿；变异验证（ceil 改截断 / 先乘倍数改回去 / `missing_rates` 恒空各自精确失败）

- [x] 端到端实测：CNY 售价模型 → `usage_records.charge_micros == ceil(原生 × 汇率)`、快照含原生金额与汇率、`/v1/models` 报 CNY、控制台切币种显示 `≈`

- [x] 回填设计文档「实现与设计差异」、提交（引用设计文档路径）

### M22 运维：8088 运行态加人民币（展示币种）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> `config.yaml` 被 gitignore，运行态改动在这里留痕（沿用 M20b 的做法）。

- [x] 口径：只做**展示币种**（不改任何定价与账本）；控制台默认展示币种改为 CNY

- [x] 汇率：`CNY: 147567`（1 CNY = 0.147567 USD，即市场中间价 1 USD = 6.7766 CNY，2026-09-11；
  来源 [中间价 6.7766](https://sjqcj.com/news/detail?id=104727)）。**不用** 7.1：那是既有 DeepSeek 成本价折算成美元时用的口径，
  与当前市场偏离约 4.6%，会污染所有美元金额的 ¥ 展示

- [x] 运行态（免重启）：`PUT /admin/api/v1/settings/billing.fx_rates {"CNY":147567}` →
  `GET /billing/currency` 显示 `CNY rate_micros=147567 rate_source=settings`，控制台顶栏可选 ¥

- [x] 文件基线：`config.yaml` 的 `billing.display_currency: CNY`、`billing.fx_rates.CNY: 147567`，
  用 `internal/config.Load` 校验通过（`ledger=USD display=CNY fx=map[CNY:147567]`）

- [x] `make build` 已把 `bin/aigw` 更新到 `6a8967d`（运行中的进程仍是旧 inode，报 `06265d2-dirty`）

- [x] 注意：汇率在两处（config.yaml 文件 + settings 覆盖），**覆盖优先**；以后只在控制台改就地生效，
  想让文件成为唯一来源，就在「设置 → 汇率表」清空保存一次

### M22 实测与收尾
- [x] `make verify` 全绿（vet + test + build）；`internal/arch` 分层测试未新增包、无需改表
- [x] 控制台走查：`make ui-check`（headless firefox）新增 `currency` 视图 16 项断言（默认无 ≈、切 CNY 后 ≈ + 正确数值、缺汇率徽标、无页面错误）
- [x] 真实实例端到端：CNY 售价模型 → `usage_records.charge_micros == ceil(原生 × 汇率)`、快照含 `sale_currency`/`fx_sale_ledger`/原生金额、账本条目同额、`/v1/models` 报 CNY、`/billing/currency` 列表与缺汇率

---

## M23 请求日志默认只保留用户输入 + 配额口径收口

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计文档 `docs/design/m23-input-recording.md`（实现前已在对话中输出并确认）。
> `config.yaml` 被 gitignore，运行态改动在这里留痕（沿用 M22 的做法）。

- [x] 口径：请求日志输入通道默认只记**用户自己写的输入**（`input` 里 `type=message, role=user`），
  系统/开发者指令、工具定义、`function_call`/`function_call_output`（真实流量里是整份文件内容）、
  历史 assistant 轮次只留 `omitted` 计数与 `request_bytes`

- [x] 四档语义：`full`（整份正文，排障用）/ `user`（默认）/ `metadata`（只记元数据，不落正文）/
  `off`（什么都不留）；Key 级 `inherit` 继承部署默认，历史脏值 `meta` 归一化为 `metadata`

- [x] **行为变更**：`metadata` 此前与 `full` 等价（代码里 `if inputMode != "off"` 就落整份正文），
  现在真的只记元数据——把 `record_input` 写成 `metadata` 的部署需要改成 `user` 或 `full`

- [x] 落地位置：`config.Recording.InputModeFor` 解析策略；`responses.Request.UserInputDocument` 抽取
  用户输入；`httpapi.recordInput` 统一构造（脱敏 → rune 安全截断），正常/失败/本地拒绝三条路径共用

- [x] 修 `recordDenied`：本地拒绝路径此前落整份正文且**完全不脱敏**，现在走同一策略（含脱敏）

- [x] 修 hook `input` 字段：此前填的是模型输出（`assembler.Text()`），现在是与请求日志同一份文档

- [x] 修 `truncate`：按 rune 边界回退，不再把中文切成非法 UTF-8

- [x] `persist` 只算一次输入文档，落库（`request_logs` + `responses.request_json`）与 hook 事件共用

- [x] 配额口径：唯一形状 = **扁平顶层**（`rpm/tpm/concurrency/monthly_*/strategy/provider_order/margin_bp`），
  `domain.ParsePolicy` 为唯一解析器（`quota` 与 `mcpsrv` 共用，`domain` 是两者都能 import 的层）

- [x] 写入即校验：Key 创建/PATCH、标签 upsert 的 `policy` 必须是非空对象且不含未知顶层字段，
  否则 400 并列出可接受字段（控制台标签页示例的嵌套 `{"rate_limit":{"rpm":60}}` 从此会被拒绝）

- [x] Key 配额**建完可改**：`PATCH /admin/api/v1/keys/{id}` 接受 `policy`，`GET /keys` 返回 `policy`，
  控制台 Key 编辑弹框可直接改（此前只在创建时可写，建完只能重建）

- [x] 修 PATCH 录制开关**保存不上**：原实现先 `SetAPIKeyRecording` 再 `UpsertAPIKey(target)`，
  后者用行里的旧值把刚写入的模式与两个布尔开关覆盖回去；改为单次 upsert

- [x] `record_input_mode` 校验：只接受 `inherit|user|full|metadata|off`，控制台此前送 `'meta'` 会被静默存成未知值

- [x] 管理面请求列表/详情返回 `request_bytes`（详情另有 `response_bytes`），控制台详情显示「请求正文 N 字节」

- [x] MCP `get_rate_limits`：`configured_limits` 改读扁平字段（与生效路径同一解析器），新增
  `not_enforced` / `ignored_policy_fields` / `policy_error`，note 说明标签策略在生效时合并

- [x] 控制台：Key 页四档选择 + 配额列与编辑框、标签页示例改为能生效的扁平写法、请求日志页文案、
  设置页只建议有读取方的键（`recording.default` 等四个死键移除）

- [x] 测试：`InputModeFor` 表驱动、`ParsePolicy`（未知字段/非数字/非对象）、用户输入抽取（工具输出
  等不得出现在结果里）、默认文档只含用户输入、`metadata`/`off`、拒绝路径脱敏、扁平 `rpm=1` 生效、
  PATCH 校验与 policy 读回、`truncate` rune 边界、控制台枚举一致性

- [x] 控制台走查：`scripts/ui-harness` 新增 `keys.page.html`（`#keys`/`#requests` 视图，17 + 10 项断言：
  配额列与回填、四档枚举与后端一致、PATCH 载荷、详情 `request_bytes`、圆形 ✕ 可关闭），8 个视图全绿

- [x] 冒烟：独立实例（`:8099` + 临时库 + 新二进制）实测——默认文档只含用户输入、嵌套 policy 400、
  `'meta'` 400、扁平 `rpm=1` 生效得 429

- [x] `make verify` 全绿（vet + test + build）；`internal/arch` 未新增包、无需改表

- [x] 文件基线：`config.example.yaml` 的 `recording.record_input: user`；README 状态与设计文档行刷新

- [x] 运行态：`config.yaml` 的 `record_input` 由 `metadata` 改为 `user`（语义已变，必须显式改）

- [x] **已人工执行**：`scripts/local-run.sh restart` —— 新二进制（`/healthz` → `f990a75`）与
  `config.yaml` 的 `record_input: user` 已在 8088 生效，控制台内嵌资源同步（`/admin/ui/js/pages/keys.js`
  已是四档 + 无 `'meta'`）

- [x] 重启后实测（真库 `data/aigw-local.db`，未改任何策略、录制模式用完即复原为 inherit）：
  * 默认（inherit → user）：新行 `mode=user request_bytes=506`，正文只含 `M23 live check`，
    工具输出 `ZZ_LIVE_MARKER`、系统指令、assistant 轮次均不出现，`omitted` 计数齐全，思考/输出为空
  * PATCH `record_input_mode=metadata` → 新行 `mode=metadata request_json='' request_bytes=506`
    （证明 PATCH 真的落库，旧实现会被 `UpsertAPIKey` 覆盖回旧值）；随后 PATCH 回 `inherit` 并复验成功
  * 写守卫：嵌套 `{"rate_limit":{"rpm":60}}` → 400 且错误里列出可接受字段；`'meta'` → 400；
    `GET /keys` 能读回 `policy`（当前为 null）
  * 实测顺带发现：重启前日志里有 **20 次** `recording request content failed ... context deadline
    exceeded`，其中至少 2 个请求**整行都没落库**（正文 ~985 KB，撞上 `max_bytes` 上限），
    正是 M19 那类「要排障的请求恰恰没记录」；默认改 `user`（~500 B/行）后这类超时预期基本消失

## M24 管理后台所有列表支持分页

> 需求：「使用列表要支持分页显示」→ 澄清为「管理后台的所有列表」。
> 设计文档 `docs/design/m24-console-pagination.md`（实现前已在对话中输出并确认）。

- [x] 统一契约：所有列表端点接受 `limit` + `offset`，返回
  `{data, count, total, limit, offset, has_more}`（`count` = 本页条数，`total` = 过滤后总行数）；
  `limit` 超上限夹住，`offset` 非整数/负数 → 400（不静默当 0）
- [x] 分页原语 `internal/httpapi/pagination.go`：`pageParams` / `adminPage` / `sliceWindow` / `writeList`
- [x] 历史类列表（7 个端点）SQL 窗口化 + `COUNT(*)`：`/requests`、`/audit-logs`、
  `/accounts/{id}/ledger`、`/accounts/{id}/credits`、`/invoices`（含按账户路径）、
  `/redemption-codes`、`/billing/reconciliations`
- [x] `store.LedgerWindow.ExcludeKinds`：额度明细的 `kind != 'charge'` 下沉到 SQL
  （否则每页条数不均、`total` 无意义）
- [x] 旧 DAL 签名不动，新增 `ListXPage`/`CountX` + 一行包装（`registry`/`bootstrap`/`mcpsrv`/
  `billing` 零改动）
- [x] 配置类列表（14 个端点）handler 内窗口：账户/Key/标签/模型/路由/映射/Hooks/MCP 令牌/
  门户用户/供应商/上游模型/备份
- [x] `internal/billing/readers.go` 分页直通（Ledger/Invoices/Codes/Reconciliations）
- [x] 路由表补 `offset` 查询参数与 `limit` 默认/上限说明（MCP `admin_describe`/`admin_endpoints` 随之生效）
- [x] 控制台 `ui.js`：`pager()` + `pagedTable()`（持窗口状态、过滤/页大小变化归零、
  末页删空自动回退一页、失败不锁死）；`table()` 的 `#count` 填「本页 N 行」，过滤框标注「本页过滤…」
- [x] `app.css` 增加 `.pager` 样式；分页控件渲染在 `<table>` 之外（不动 `tbody tr` 选择器）
- [x] 13 个页面模块接入：keys/accounts/tags/mcp/codes/providers/models/mappings/billing(4 张表)/
  requests/hooks/audit/backups；选择器调用显式 `limit: 1000`
- [x] `models.js` 删掉客户端 `route_count` 推导（服务端已返回），账单改为服务端按账户过滤
  （顺手修 `GET /invoices` 的 `account_id`：路由表从 M12 起就写着这个参数，handler 却只读路径参数，
  等于**静默返回所有账户**；现在路径/查询二者取一，非法值 400）
- [x] 明确不分页并写明理由：概览（`/stats` 快照）、设置（键值/汇率表）、定价（选择器网格）、
  供应商日志弹框（环形缓冲）、`/provider-kinds`
- [x] 测试：`internal/httpapi/pagination_test.go`（7 例：窗口/`total`/`has_more`/越界/400/夹上限/
  额度明细只数非 charge/账单账户过滤/备份 `total_bytes`）、`internal/store/pagination_test.go`
  （4 例：Page + Count + `ExcludeKinds` + 旧方法的"第一页"语义）、
  `internal/httpapi/admin_billing_contract_test.go`（自然月账期 + 非法值 400）
- [x] 设计文档先于代码落盘（`docs/design/m24-console-pagination.md`），规格侧同步
  `docs/design/m8-admin-api.md`（分页契约）、`docs/design/m9-web-console.md`（§8 第 4 条）、
  `docs/mcp.md`（后台工具的分页约定）；「实现与设计差异」已回填
- [x] `make verify`（vet + test + build）与 `make ui-check` 全绿；`internal/arch` 未新增包，无需改表
- [x] 走查工具：`scripts/ui-harness` 新增 `paging.page.html`（30 项断言）+ 视图注册 + README 说明；
  九个视图全绿（docs 23 / detail 20 / create 6 / plugin 17 / plugin-cached 16 / currency 16 /
  keys 17 / requests 10 / paging 30）
- [x] 隔离实例真机走查（`:8098` + 全新库 + 新二进制，`ALL CHECKS PASSED`）：55 条请求日志 3 页互不重叠、
  45 张兑换码分批分页、账本 67 行（含 55 条真实 charge）而额度明细 `total=12` 且无 charge、
  3 张账单按账户过滤、审计跨页、13 个账户窗口切片、`offset=abc|-1` → 400、`limit=99999` → 夹到 500、
  二进制内嵌 `ui.js` 带 `pagedTable` + `accounts.js` 发 `offset`
- [x] 顺带修两处「文档里有、代码里没有」的契约（M24 走查撞上，不修会在分页后变成可见回归）：
  `GET /invoices` 的 `account_id` 查询参数此前被忽略（静默返回所有账户）、
  `POST /accounts/{id}/invoices` 的 `period` 文档写自然月却只认 `current|previous|last30`
- [x] 控制台页面大小 20 条/页（可选 20/50/100）；`table()` 的 `#count` 由死 span 变成「本页 N 行」，
  过滤框标注「本页过滤…」；分页控件渲染在 `<table>` 之外（不动 `tbody tr` 选择器）

---

## M25 请求日志的写入兜底与保留期清理

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计文档 `docs/design/m25-log-retention.md`（实现前已在对话中输出确认；编号顺延说明见文档开头）。
> 起因：M23 收尾时把那次写入超时量化了——真库 708 MB 里 689 MB 是历史正文，且 20 次失败里至少 2 个
> 请求**整行都没落库**（详见 M23 的观察项）。

- [x] 量化结论（先做，避免误改架构）：写入池 `SetMaxOpenConns(1)` 串行，单行成本 × 并发 = 队首等待；
  收窄到 `user` 后 1.8 KB 行 p50≈40 µs，余量有数量级级别 → **不改异步/背压**

- [x] 写入兜底：`storeRequestLog` 统一入口，首次失败 → `write_failures` 计数 + **去正文骨架行重写**
  （独立 15 s 超时），再失败 → `dropped` 计数 + ERROR 日志；`request_bytes`/状态/请求 id 全部保留

- [x] 两条写入路径共用该入口：`recordContent` 与 `recordDenied`；后者顺带**脱离请求 context**
  （客户端挂断不再丢掉「本地拒绝」那一行，与 `persist` 的 M19 修复对齐）

- [x] 计数可见：`/admin/api/v1/stats` 增加 `request_log` 块（retention_days / write_failures / dropped /
  pruned / enabled / last_run / last_error）；`/metrics` 增加
  `aigw_request_log_write_failures_total`、`aigw_request_log_dropped_total`、`aigw_request_log_pruned_total`

- [x] 保留期生效：新增 `internal/retention`（叶子包，端口注入）——启动跑一次 + 每 24 h 一次，
  **分批**删除（每批 500 行、每轮每表最多 200 批），一次 pass 用 `TryLock` 防重叠

- [x] 清理范围：`request_logs`（按 `created_at < now - retention_days`）与 `responses`
  （按 `expires_at < now`）；新增迁移 0007 给 `responses.expires_at` 建索引
  （`request_logs.created_at` 早有 `idx_request_logs_time`）

- [x] `responses.expires_at` 不再硬编码 30 天：由 `recording.retention_days` 推导；
  **0 = 关闭保留期**（不清理，且 `expires_at` 写 NULL = 永久可取回），负数在校验里被拒

- [x] 手动触发：`POST /admin/api/v1/requests/prune`（roleAdmin + Dangerous + ConfirmReason），
  返回删除条数并写审计（`prune` / `request_log`）；MCP 后台工具通过路由表自动获得该端点

- [x] 控制台：请求日志页显示保留期/已清理行数/写入失败与丢弃告警，并提供「清理过期日志」按钮
  （二次确认里写明将要删除的窗口）

- [x] 测试：骨架行（内容为空但事实齐全、计数正确）、两次都失败时 `dropped` 计数、客户端 context 已取消
  时拒绝行仍落库、批量删除的条数/上限/边界时间/NULL 永不到期、janitor 分批到上限/关闭/不重叠/错误传播、
  手动端点（viewer 403、admin 200、审计、`0` 时如实报 disabled）、`/stats` 字段

- [x] `make verify` 全绿；`make ui-check` 视图全绿（requests 视图 18 项断言，含保留期与清理按钮）

---

## M26 请求路径的审计写入批量化（CPU 与尾延迟）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

设计文档：`docs/design/m26-audit-batching.md`（含取舍、接口、异常边界与实测数据；实现了 M25 里
「不需要异步队列或背压」那个结论的反例）。

起因：用宿主 `--pid=host` 容器采样 8088 实例（沙箱 PID namespace 看不到宿主进程），再用
带 pprof 的副本实例压测归因，结果指向同一个瓶颈 —— **请求路径上的落盘**。

- [x] 测量基线（副本实例、离线 replay 上游、16 并发小请求 60s）：63 rps、每请求 2.57ms CPU、
  p50 16ms、p90 480ms；pprof 里 `handleCreateResponse` 占进程 CPU 51.7%，
  其中 `storeRequestLog` 18.5% + `store.PutResponse` 13.5%，后台计费结算另占 11%

- [x] 根因（goroutine dump 直接印证）：写连接池是 `write.SetMaxOpenConns(1)`，而每个请求要
  同步写两行（`responses` + `request_logs`），于是**全局串行**；6 个请求 goroutine 卡在
  `store.(*DB).PutRequestLog` → `database/sql.(*DB).conn` 等这条连接

- [x] 修复 1：`internal/store/stmtcache.go` —— 写路径复用 `database/sql` 的 `*sql.Stmt`
  （纯 Go 驱动每次 Exec 都要重跑 SQL 解析器；profile 里 `_sqlite3Prepare` 累计 14%）

- [x] 修复 2：`internal/store/logwriter.go` —— 后台批量化。请求把两行交给 `LogWriter`，
  由它在一个事务里成批提交（默认 250ms / 256 请求 / 16MiB 触发）

- [x] 审计语义不变：批量整体失败时逐行重试；单行仍失败则报回 transport，由它写内容为空的
  骨架行并计数（`request_log.write_failures` / `dropped` 口径与同步路径完全一致）

- [x] 读一致性：`GET /v1/responses/{id}` 在返回前会等该 id 的队列行落库（`AwaitResponse`），
  POST 后立刻按 id 取回不会 404；为此新增测试 `TestImmediateGetSeesItsOwnResponse`

- [x] 关闭排空：`main.go` 在 HTTP 优雅关闭之后、`db.Close()` 之前 `LogWriter.Close(ctx)`，
  排空队列再退出（实测：200 个请求后立刻 SIGTERM，200 行全部落库）

- [x] 顺带发现并修掉一个真缺陷：`tx.StmtContext(池级语句)` 与单连接写池**会自锁**
  （语句占着那条连接，`StmtContext` 又去要同一条），第一次执行就超时；改为事务内
  `tx.PrepareContext`，并留下回归测试 `TestStmtCacheInsideTxDoesNotDeadlock`

- [x] 开关与观测：`recording.batch_writes` / `batch_flush_ms` / `batch_max_rows` /
  `batch_max_bytes` / `batch_queue_rows` / `batch_queue_bytes`（默认开）；`/metrics` 增加
  `aigw_audit_batched_requests_total`、`aigw_audit_batches_total`、`aigw_audit_queued_requests`；
  `/stats` 的 `request_log` 块增加 `batching` 子块

- [x] 背压（生产规模压测暴露）：队列必须有上限，否则写侧跟不上时内存无界增长、优雅关闭也排不空
  （实测：700KB 请求持续 116 rps → 积压 1584 个请求、约 350MB，`docker stop` 15s 未排完）。
  现在 `Enqueue` 在队列满时等写入推进（`QueueRows`/`QueueBytes`，默认 4096 / 32MiB，
  上限 30s），等不到空位的那一行走骨架兜底并计入 dropped；背压次数在 `/stats` 的
  `batching.backpressure` 可见。测试 `TestFullQueueAppliesBackpressure`（用「写侧卡住」的
  store 制造真实满队列）

- [x] 正确性复验（修复版副本，32 并发 20s）：9616 个请求 → `request_logs` 恰好 9616 行、
  0 错误、`queued_requests` 归零，只用了 **61 个事务**（同步路径是 9616 个）

- [x] A/B 复验（同机同脚本，60s）：**186 rps 对 63 rps**；每请求 CPU **0.82ms 对 2.57ms**；
  p50 **4.3ms 对 16.3ms**；p90 **17ms 对 480ms**；`_full_fsync` 从可观占比降到 0.34%

- [x] `make verify` 全绿（33 个包；新增 8 条测试覆盖批量路径、回退、排空、读一致性、自锁回归）

- [x] **运行态生效**（2026-09-12 10:17 重启，PID 2783609）：启动日志出现
  `audit writes batched in the background flush_ms=250 max_rows=256 queue_rows=4096 queue_bytes=33554432`；
  `/metrics` 出现 `aigw_audit_batched_requests_total` / `aigw_audit_batches_total` /
  `aigw_audit_queued_requests`。实测：16 个并发请求只用了 **2 个事务**、`write_failures` 与
  `dropped` 均为 0、`queued_requests` 归零；带 `store:true` 的 POST 后立刻 GET 返回 **200**

- [x] 验证数据清理：验证用的 17 行 `request_logs`（`PROBE-DELETE-ME`）与 1 行 `responses` 已删除，
  残留 0；确认用户原有 38 条历史存储响应完好

---

## M27 请求日志的身份维度与消耗度量

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计文档 `docs/design/m27-request-dimensions.md`，规格文档 `docs/request-log.md`（编号顺延说明见设计文档开头：M26 已被 `e42b066` 占用）。
> 起因：请求日志只能回答「有一条请求、它多大、成功没有」——要回答「谁在用、用哪个模型、在哪个工作区、
> 属于哪个会话、花了多少」只能去翻正文，而正文会撞 1 MiB 截断（实测 397/2297 行）又会按保留期被清掉。

- [x] 迁移 0008：7 个身份列（client / model / resolved_model / workspace / session_id / call_kind / title）
  + 三条 `(维度, created_at, id)` 索引；`ADD COLUMN … DEFAULT ''` 在 SQLite 只改元数据，747 MB 库瞬时完成

- [x] 索引取舍实测（先做，避免拍脑袋）：同构表 + 默认口径行形状 + 256 行/事务，用 **WAL 页字节/行**
  这个确定性指标（墙钟被 checkpoint 抖动淹没，同档在不同行数上抖动到 20 倍）：
  现状 4449 B/行 → +client+session 4820（1.08×）→ +model 5008（**1.13×**）；悲观局部性上界 1.43×/1.54×

- [x] 查询计划对照：清理 `DELETE … ORDER BY id LIMIT 500` 三档**计划完全一致**；无筛选列表仍走时间索引；
  `client=?`/`model=?` 用新索引且**无 temp B-tree**（索引带 `id` 列是这一条成立的原因，否则退回 M24 修掉的排序器）

- [x] 提取器 `internal/responses/dimensions.go`：结构性识别（顶层 instructions / 首条 developer 消息 /
  以 `<environment_context>` 开头的消息 / 标题提示词前缀），**不做全文匹配**——实测库里 265 行含
  `Codex CLI`，其中 259 行其实是 DSH 请求的工具输出；原始 User-Agent 只作兜底且不落库

- [x] 接线：`recordInput` 把维度计算提到 `off` 早退之前（身份独立于正文口径）；
  `persist` 补 `input.Resolved = plan.Resolved.Canonical`；`recordDenied` 同样落身份但 `resolved_model` 为空

- [x] 标题：`recording.record_title`（默认 **true**）只写在 `call_kind=title` 那一行；与输出文本录制解耦

- [x] 脱敏：`recording.redact_paths` 逐列生效（`workspace`/`session_id`/… 命中即整列置空）

- [x] 消耗读时关联：`RequestUsages` 一次批量点查（跨 attempt 求和、延迟取最差），未计量返回
  `metered=false`（本地拒绝的请求按设计不写 usage，与「消耗为 0」区分）；token 表达式与
  `UsageBreakdown` 同源

- [x] 维度聚合 `RequestLogDimensions`：白名单 6 个维度，`LEFT JOIN usage_records` 汇总 token/成本，
  **只选维度列与 created_at**（选正文列会把窗口内每行溢出页读进来）；`session` 分组带标题与工作区

- [x] 管理面：列表/详情增 7 个身份字段与 `usage`；6 个过滤参数；新端点
  `GET /admin/api/v1/requests/dimensions`（未知 `group_by` 返回 400 并列出取值，不是 500）；
  路由表新增条目使 MCP 自动获得 `admin_request_dimensions`

- [x] MCP 查询面：`list_requests` 同样返回身份字段

- [x] 控制台：列表增客户端/模型（含 `gpt-5.6-luna ← luna` 别名显示）/工作区/会话/类型/标题/
  tokens/成本（按展示币种渲染），服务端筛选（客户端下拉、模型下拉、会话与工作区输入），
  「维度统计」卡片（6 个分组、已计量与请求数分列显示），详情弹框增身份与消耗

- [x] 测试：提取器表驱动 + 三个反例；store 往返/冲突不抹身份/六筛选一致性/跨 attempt 求和/聚合/白名单/
  聚合 SQL 不含 `request_json`；httpapi served+denied 两条路径、别名、`off`、标题开关、脱敏、
  骨架行保留身份、未计量、端点权限与参数校验；`TestHistoryListsAreOrderedByAnIndexNotASorter`
  增加 client/model/session 三种筛选用例

- [x] `make verify` 全绿；`make ui-check` requests 视图 18 → **38** 项断言全绿

---

## M28 控制台补齐「供应商模型」管理 + 映射写入改成部分更新

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 起因（用户报告）：在控制台「模型」页加了 `gpt-6-astra`、在「模型路由」页加了指向 codex 的路由，
> `/v1/models` 里却始终没有它。查下来是三层里漏了第一层 `provider_models`，而**控制台根本没有管理它的界面**：
> 「模型」页管 `models`、「模型路由」页管 `routes`，供应商详情页只有配置/凭据/探测/日志。
> 前端里唯一碰 provider model 的两处是详情弹窗的「刷新模型发现」（只能落库"插件 config 里已声明的模型"）
> 和定价页的「保存为成本规则」（见下条，它会毁字段）。

- [x] 后端 `POST /admin/api/v1/providers/{id}/models` 由整行覆盖改为**部分更新**：省缺字段保持原值、
  写 `null` 清空、新行默认值不变（`upstream_model` = 对客名、`enabled` = true、`priority`/`weight` = 100）。
  实测 bug：控制台定价页只发 3 个字段，保存一次成本规则就把 `capabilities`（→ null）、
  `context_window`/`max_output_tokens`（→ 0）静默清零——映射还能路由，但客户端带 tools/reasoning
  会被打上 `X-Gateway-Degraded`，是「看着不对却查不出为什么」的典型

- [x] 测试 `TestAdminProviderModelPartialUpdate`：省略即保留、`null`/`""` 清空且不误伤其他字段、新行默认值、
  非法 `capabilities_override` 仍 400

- [x] 控制台供应商详情新增「模型映射」区（M18 文档同步为必需项）：列出对客名/上游名/启用/能力/上下文/
  最大输出/成本规则/来源，可就地新建、编辑、删除；表单里对客名在编辑时只读（改名请新建再删旧）

- [x] **点名断链**：把「指向本供应商但没有映射的路由」标红列出并写上 `not_mapped`——
  这正是用户遇到的那种静默失败，之前只能靠 `GET /router/explain` 才发现

- [x] UI harness 新增 `models` 视图（17 项断言：区块渲染、三个映射行、能力徽标、成本规则有无、
  未映射告警点名 `ghost-model` + `not_mapped`、行内编辑/删除按钮、表单字段齐全）；
  `capture.py --refresh` 一并采集 `/providers/{id}/models` 与 `/routes`，刷新不会丢掉这两个 fixture

- [x] 文档：`docs/api-providers.md` 写清三层检查与新的写入语义；`docs/provider-ui.md` §2 增加第 6 条

- [x] `make verify` 全绿；`make ui-check` 11 个视图全绿（`docs` 23 / `detail` 20 / `models` 17 …）

---

## M29 请求日志列表底部的本页汇总行（tokens 入/出 + 成本）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计文档 `docs/design/m29-request-log-page-summary.md`，规格文档 `docs/request-log.md` §4/§6。
> 起因（用户原话）：「管理后台的请求日志列表在列表底部添加一列汇总行：tokens（入/出），成本，汇总这两列」。
> 口径当场澄清并选定：**只汇总当前页已加载的行**，不做筛选窗口级合计（窗口口径看「维度统计」卡）。

- [x] 设计文档先于代码落盘（含口径取舍与窗口级备选方案的实测数字）

- [x] `table()` / `pagedTable()` 增可选 `footer(visibleRows) -> {列key: 节点} | null`：按列 key 落位，
  未命名的列空单元格；返回 `null` 或本页无行则不渲染汇总行。用 keyed 落位而不是在调用点算 `colspan`——
  列数一旦增减，后者必然过期。不传 `footer` 的页面（keys/models/accounts/…）DOM 与行为不变

- [x] `requests.js` 新增 `summaryCells(rows)`：单次遍历求已计量行数与 tokens（入/出）/成本之和；
  金额取整后再求和（`money()` 内部走 `BigInt()`，小数会抛）；标签 `本页汇总 · 共 N 行`，
  有未计量行时补 `（已计量 M · 未计量 K）`；**本页全部未计量时两格写「未计量」而不是 0**
  （沿用行内 `tokensCell`/`costCell` 的规则：未计量 ≠ 消耗为 0）

- [x] 成本列汇总的是**对客 `charge_micros`**，与它上方那一列同源（`costCell` 用的就是它）

- [x] `app.css` 一条 `tfoot td { background: var(--panel-2) }`：汇总行用 `td` 不用 `th`
  （`th` 带 muted 颜色与可排序表头的 `cursor:pointer`，会让合计看起来可点）

- [x] 后端零改动：`/admin/api/v1/requests` 响应、store、迁移、MCP 查询与后台桥全部未动

- [x] UI harness requests 视图断言 38 → **48** 项：
  - `summaryRow` 汇总行存在且只有一行；`summaryLabel` 文案（共 2 行 / 已计量 1 / 未计量 1）；
  - `summaryTokens` = `1,200 / 34`、`summaryCost` = `0.002468 USD`（夹具里只有一行计量，故等于该行金额）；
  - `summaryAligned` 单元格数 === 本表 `thead th` 数（**错行会直接失败**；表头取自 footer 自己那张表，
    因为页面上还有「维度统计」卡的表）；
  - `summaryFollowsFilter`（本页过滤框输入 `codex` 后只剩那行未计量的：`共 1 行`、`已计量 0`，
    且不再出现 `1,200`）、`summaryHiddenWhenEmpty`（筛到空 → 没有汇总行，而不是 0/0）、
    `summaryRestored`（清空过滤后回到 `共 2 行`）；`summaryFollowsRefresh`（刷新后 3 行 → `2,400 / 68`、`已计量 2`）

- [x] `checks.sample` 回报 tfoot 文本，让 `make ui-check` 的输出里留下可读证据：
  `本页汇总 · 共 3 行（已计量 2 · 未计量 1）2,400 / 680.004936 USD`

- [x] 文档：`docs/request-log.md` §4 增汇总行口径、§6 状态补 M29；README 文档表设计文档区间改到 M0–M29

- [x] `make verify` 全绿（无 Go 改动，用于确认嵌入资源与既有测试未受牵连）；
  `make ui-check` 10 个视图全绿（`docs` 23 / `detail` 20 / `models` 17 / `create` 6 / `plugin` 17 /
  `plugin-cached` 16 / `currency` 16 / `keys` 17 / `requests` 48 / `paging` 30）

---

## M30 请求日志的用户（账户）与 API Key 维度

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计文档 `docs/design/m30-request-log-owner-dimensions.md`，规格文档 `docs/request-log.md` §2/§4/§6。
> 起因（用户原话）：「请求日志添加用户,api key 维度统计」。
> 口径当场澄清并选定：**「用户」= 账户**（`accounts` 表）——API 请求只携带凭据，
> `api_key_id → account_id` 是唯一可归因的身份；门户用户与 API Key 没有绑定，今天无法判定。

- [x] 现状核对：`account_id`/`api_key_id` 从迁移 0001 起就写在每一行（服务路径与本地拒绝路径都写），
  缺的是**名字、过滤与统计**——所以本期不新增身份列，只补读取面

- [x] 名字**读时解析**、不落列（同 token/成本的口径）：改名不该分裂/合并分组，`api_keys.name` 不唯一
  （同账户可重名），两表都没有硬删除 → 按 id 一定取得到名字

- [x] `store.AccountNames` / `store.APIKeyLabels`：批量点查（形制同 `RequestUsages`：去重、跳过 ≤0、
  空入参返回空 map、缺失 id 缺席而非报错），页面与维度卡共用**同一个取名机制**

- [x] **不把 join 塞进页面查询**：页面查询的 `ORDER BY created_at DESC, id DESC` 依赖
  `idx_request_logs_time`，而 `accounts`/`api_keys` 也有 `id`/`created_at`，join 会让列名歧义并
  动摇 M24/M27 修掉的排序器契约；维度聚合也不 join（那是窗口内每行一次 PK lookup，而 top-N 之后
  补标签最多 200 次）

- [x] 维度聚合 `group_by=account|api_key`：`CAST(account_id/api_key_id AS TEXT)` 作为 key，
  名字由 handler 补；id ≤ 0 归入「（未知）」桶（`key: ""`），**计数保留**

- [x] 过滤：`RequestLogFilter.APIKeyID` 精确匹配；`account_id`/`api_key_id` 非法值 **400**（此前
  `account_id` 是静默忽略，即「筛了却返回全部」——`/invoices?account_id=` 修过的同一类）

- [x] 迁移 0009：`idx_request_logs_account` / `idx_request_logs_key`，形状同 M27 三条
  `(维度, created_at, id)`

- [x] 索引取舍实测（先做，避免拍脑袋）：**WAL 页字节/行**（2.5 KB 行、256 行/事务、真机局部性）
  4709.8 → 4937.7（**1.048×**），悲观上界（每行不同账户/Key）6945.1（1.475×，落在 M27 为 4 条索引
  估的 1.54× 之内）；判定阈值 1.25× → **采纳**。探针保留在
  `internal/store/index_write_probe_test.go`（默认 skip，可用同一把尺子量下次的索引）

- [x] 索引收益实测：6 万行 / 5000 账户 / 2.5 KB 正文、投影全部列的一页 —— 59.9 ms → **92 µs**
  （没有索引时扫的是窗口内**每一行的正文页**）

- [x] 查询计划对照（真库与迁移后副本）：`api_key_id=?`/`account_id=?` 从时间索引窗口扫描变为
  新索引等值前缀；无筛选列表一致；清理 `DELETE … ORDER BY id LIMIT 500` 计划**完全一致**

- [x] 迁移代价实测（718 MB 真库副本）：`Open`（含两条 `CREATE INDEX`）758 ms，WAL +0.1 MB

- [x] 管理面：列表/详情增 `account_name`/`api_key_name`/`api_key_prefix`；`dimensions` 增两个分组
  取值与名字；`dimensionQueryFields` 增 `api_key_id` 并把 `account_id` 移入（此前 `/dimensions`
  接受 `account_id` 却没在路由表里声明，MCP `admin_describe` 看不到）

- [x] MCP：`list_requests`/`get_request` 返回 `api_key_id`/`api_key_name`（`ListAPIKeys` 一次建
  map，失败只回 id 不报错）；后台工具 `admin_request_dimensions` 的 `group_by` 自动多出两个取值

- [x] 控制台：列表增「用户」「API Key」两列（名字 + title 里的 id/前缀）、工具栏增账户与 Key 两个
  服务端筛选（Key 下拉随账户联动，切账户会按新账户重取 Key 列表）、「维度统计」增两个分组
  （默认分组改为「用户（账户）」）、详情弹框增「用户（账户）」与「API Key」

- [x] 测试：store（两个过滤、两个分组、白名单信息列全 8 个取值、批量取名容错、冲突不抹凭据、
  两条索引存在、两种筛选无 temp B-tree）；httpapi（列表/详情名称、`api_key_id` 过滤与 total 一致、
  非法值 400、两个分组与未知桶）；webui（控制台维度选项与 `store.RequestLogDimensionNames` 不漂移）

- [x] UI harness：fetch stub 记录**原始 URL**（断言筛选值真的发到服务端）并按 `group_by` 应答专用
  fixture；requests 视图断言 48 → **61** 项；`capture.py` 补抓 `/accounts` 与两份分组 fixture

- [x] `checks.sample` 回报两份分组表的文本，让输出里留下可读证据：
  `账户分组：…acme #1 3 3 3,600 102 0.007404 USD（未知）1 0 / 1 0 0 未计量`、
  `API Key 分组：…dev-key #1 3 3 3,600 102 0.007404 USD`

- [x] 文档：`docs/request-log.md` §2（两行维度 + 读时取名的规矩 + 不受 `redact_paths` 影响）、§4
  （过滤参数、分组取值、名字字段、下拉上限）、§6 状态补 M30；README 文档表改 M0–M30；
  `config.example.yaml` 注明凭据维度不参与脱敏

- [x] `make verify` 全绿；`make ui-check` 10 个视图全绿（`requests` 61 项）

---

## M31 请求日志页「维度统计」卡片置顶 + 排序 + 分组列表分页

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

> 设计文档 `docs/design/m31-request-log-stats-pagination.md`，规格文档 `docs/request-log.md` §4/§6。
> 起因（用户原话）：「将后台请求日志页面的维度统计卡片放上面，并且列表要加分页」。
> 口径当场澄清并选定：「列表」=「维度统计」表（「请求日志」列表在 M24 已有服务端分页）；
> 排序补充确认为**默认按最近一次请求时间降序**，并保留可切换的排序入口（服务端 `sort` 参数）。

- [x] 现状核对：统计接口只有 `LIMIT ?`、没有 `offset`/`total`，排序写死 `ORDER BY COUNT(*) DESC, group_key`；
      控制台写死 `limit: 20`、无分页控件、无时间列 → 分组数超过 20 时第 21 个永远看不到，
      也判断不出「到底了还是被截断了」。控制台全站没有可排序表头（M24 §6 明确不做列排序），
      所以排序要真做就得落到服务端（客户端只能排当前页，会把「第 1 页里最大的」当成全局最大）

- [x] store：`ListRequestLogDimensionsPage`（`LIMIT ? OFFSET ?` + 排序子句）与
      `CountRequestLogDimensionGroups`（`COUNT(*)` 套一层分组子查询，**不 join `usage_records`**：
      WHERE 只涉及 `r.`，而 LEFT JOIN 不增不减分组键，所以两者分组集合恒等）；
      删掉没有生产调用点的 `RequestLogDimensions`（保留旧的「默认排序」包装会把排序口径藏起来）

- [x] 排序白名单 `last_seen`（默认，最近一次请求时间）/ `requests` / `charge`，每条都以 `group_key ASC`
      兜底（`created_at` 是秒级整数，`last_seen` 大量并列——没有唯一兜底键就无法分页）；
      ORDER BY 写**完整聚合表达式**而不用输出别名（`usage_records` 有同名 `charge_micros` 列，
      别名参与名字解析会踩歧义）

- [x] httpapi：`pageDimensions = pageSpec{Def: 20, Max: 200, Noun: "分组数"}`（`pageSpec` 增可选 `Noun`）；
      响应增 `sort/count/total/offset/has_more`（`rows` 保留：它自 M27 起就是这个端点的文档形状）；
      未知 `sort` 与非法 `offset` 都是 **400 并列出取值**（静默忽略 = 「换了排序却没变」）；路由表声明
      `offset`/`sort` → MCP `admin_describe`/`admin_endpoints`/后台工具自动获得，MCP 侧零代码改动

- [x] 控制台：统计卡置顶（列表卡在其下）；工具栏增排序下拉（按最近一次请求/按请求数/按成本）；
      表格增「最近一次」列、当前排序列的表头带 `↓`；复用 `ui.js` 的 `pager()`（新增可选 `unit`，
      默认 `条`）→ 统计卡写「共 N 个分组」，与列表的「共 N 条」区分；切分组/排序/筛选都回到第 1 页，
      「刷新」保持当前页与排序，「清理过期日志」回到第 1 页；末页删空自动回退一页；
      响应缺 `total` 时退化成「只有本页」（旧服务端不会白屏）

- [x] 测试：store（201 个同秒桶翻页不重不漏、`limit` 默认 20 与夹取 200、`offset` 越界/负值、
      三种排序各自的首桶、并列时 `group_key` 兜底、未知 sort 报错列取值、计数 SQL 不含 `request_json`
      也不含 `usage_records`）；httpapi（信封与 `sort` 回显、两页不重叠、过滤后 `total` 一致、
      `offset=abc|-1` 400、未知 sort 400）；webui（控制台排序下拉的**取值与顺序** ==
      `store.RequestLogDimensionSorts`）；MCP（`admin_describe` 列出 `offset`/`sort`、
      limit 描述写「分组数」、enum 与 store 白名单一致）

- [x] UI harness：stub 对 `/requests/dimensions` **按 `limit/offset` 切片、按 `sort` 排序**后应答
      （照抄真实端点做的两件事；忽略查询串的 stub 会让分页与排序都变成不可观测），
      `group_by=workspace` 用合成的 45 个分组（三个排序键的**首桶互不相同**，否则「切了排序但顺序没变」
      也会全绿）；requests 视图断言 61 → **76** 项

- [x] 实测（真库只读副本）：新增的分组计数在 8 个维度上全部只走覆盖/时间索引，**不读正文页、不 join 计量表**
      （`client`/`model`/`session` 直接 `SCAN USING COVERING INDEX`；其余走 `idx_request_logs_time` + 分组 temp B-tree）；
      60k 行 / 2.5 KB 正文探针库上：聚合 p50 **80.3 → 80.4 ms**（换排序键零成本，临时 B-tree 今天就有），
      分组计数 p50 **2.4 ms**（≈ 聚合的 3%，不是翻倍）

- [x] 隔离实例走查（`:8099` + 全新库 + 新二进制，`ALL CHECKS PASSED`）：默认排序首桶=最近一次的桶、
      三种排序键首桶各不相同、两页不重叠且 `total` 是**分组数**（3 个桶 6 条请求）、
      `offset=abc`/`offset=-1`/`sort=latency` 全部 400 且报错文本列出取值

- [x] 文档：设计文档（决策 15 条 + 实测 + 边界）、`docs/request-log.md` §4（端点表 + 排序表 +
      控制台段）与 §6、README 文档表 M0–M31、harness README 的 fixture 约定

- [x] `make verify` 全绿；`make ui-check` 10 个视图全绿（`requests` 76 项）

---

## M32 控制台「智能问答」+ 私有技能库 + 图表与 HTML5 预览

### 权限与隐私（评审阻断项，先修后做）
- [x] 首版只有 `admin` 能绑定计费 Key、发起问答与生成技能草稿；`viewer` 仅能读自己的会话、管理自己的技能
      （Key 列表对 viewer 可见，只校验 Key 本身会把「能看」变成「能花」）
- [x] 每一步模型调用前重新解析登录会话与角色（`Store.AdminRole`）、账户与 Key 归属及状态，客户端提交的 id 不构成授权
- [x] 问答内容不进全局记录：请求日志只落身份维度/token/成本/状态，`store:false` 不落存储响应，
      hooks 不带 input/output，技能审计只记操作与不透明 id（不记技能名与指令）
- [x] `docs/chat.md` 明确「应用内私有 ≠ 不离开本机」：技能与问题会随请求发送给所选上游模型

### 计费与数据面复用
- [x] 进程内直连既有 `handleCreateResponse`：配额/限流/在途/结算/计量/请求日志/hooks 零重复实现
- [x] 不存在的 bearer 令牌 → 私有 context 身份（`verifiedIdentityFrom`，unexported key，外部无法伪造）
- [x] `apikey.VerifyID`：与 `Verify` 完全相同的检查（key 有效/未过期/账户 active），按 id 解析，节流 touch
- [x] 每步独立 `request_id` + `client=console`（服务端设置 User-Agent）+ `prompt_cache_key` = 会话 id
- [x] 内部 step 观察端口（`stepObserver`）回传真实 provider/canonical/degraded：流式分支不设这些响应头
- [x] `internal/responses`：`ClientConsole` 常量 + `clientFromHint` 识别；控制台请求日志客户端下拉同步

### 工具面（MCP 复用 + 默认拒绝）
- [x] 复用 `admin_endpoints`/`admin_describe`/`admin_request`；scope 由会话写开关与角色共同决定
- [x] 聊天专用允许清单：读接口（viewer+GET）全部可用，写接口只有显式列入的可用，**未列入的默认拒绝**（含未来新增）
- [x] 清单在 list/describe/execute 三处一致生效（`chatToolPolicy` 经 context 传入桥接层），不修改全局 MCP 可见性
- [x] 审计 actor 渲染为 `console:<用户名>`（`mcpsrv.Principal.Source`）
- [x] 被拒调用原样回给模型解释，并写入 `chat_tool_calls`（`is_error`）

### 轮次、幂等与恢复
- [x] 客户端幂等 `turn_id` + 唯一索引：重复提交返回既有轮次，不重复计费；同会话并发轮次 409
- [x] 工具执行前落 `pending` 行（唯一键 `turn_id+step+call_id`），崩溃后标记 `unknown` 且不自动重放
- [x] 重启把 `running` 轮次标记 `interrupted`（`cmd/aigw` 启动时调用 `RecoverChatTurns`）
- [x] 仅 `completed` 且参数完整合法的工具调用可执行；`incomplete`/取消/参数截断一律不执行并向用户说明
- [x] 取消/断线用 `context.WithoutCancel` + 5s 超时保存已得内容（`aborted`）
- [x] 历史按**整轮**裁剪（保留 `function_call`/`function_call_output` 配对），超限明确报错而不是发半个问题

### 预览与图表
- [x] HTML5/SVG 预览 = owner + 会话绑定的**短时票据**（HMAC、纳秒精度、进程内密钥 → 重启即失效）
- [x] 票据绑定登录会话：登出后票据立即失效（`AdminSessionUser` 校验）；无票据/伪造/过期一律 404
- [x] 预览响应 `sandbox`（HTML 带 `allow-scripts`，SVG 不带）+ `default-src 'none'` + `connect-src 'none'` + `no-store`/`nosniff`/`no-referrer`
- [x] 文档用「默认禁止外部资源加载与网络连接」而非「断网」（`connect-src` 管不到子框架自导航）
- [x] 图表：`chart` JSON → 手写 SVG（离线、不执行模型代码），上限 8 序列/500 点，非法规格退回代码块并说明原因
- [x] 图表附数据表、来源工具/请求 id 与本轮真实调用名；文档明确控制台**无法验证**模型是否如实使用工具数据
- [x] 边界：空数组/全零/负数/饼图负值/全 null/非有限数/超长标签/CSV 公式注入（`=+-@` 前缀转义）

### 控制台
- [x] 路由：`#/chat`、`#/skills` 紧随「概览」；hash 支持 `path?query`（`#/chat?session=<id>` 深链）
- [x] 页面 teardown：切页/登出时 abort 流并清定时器（`app.js` 支持 render 返回清理函数）
- [x] `markdown.js` 手写渲染（全程 `textContent`、链接协议白名单）；`chart.js` 本地 SVG + 导出
- [x] `pages/chat.js`：会话列表/流式气泡/思考折叠/工具卡片/usage 脚注/停止/`＋` 菜单（含技能勾选）/会话设置
- [x] `pages/skills.js`：分页列表 + 手写新建 + 编辑 + 删除确认 + 来源会话跳转 + 草稿交接（sessionStorage）
- [x] `chat_artifact.js`：外部资源预检查提示 + 沙箱 iframe + 新标签打开 + 复制源码

### 测试与验收

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] store：owner 隔离（会话/消息/技能/产物）、唯一约束、级联删除、分页、pending 恢复、产物 upsert/淘汰、`AdminRole`/`AdminSessionUser`

- [x] `internal/chat`：工具循环与配对回灌、步数/工具次数上限、`incomplete`/参数截断不执行、幂等、取消落库、
      整轮裁剪与超限拒绝、技能注入与悬空 id 跳过、草稿解析与兜底、角色每次工具调用重读

- [x] `internal/httpapi`：鉴权矩阵（匿名 401 / viewer 403 / MCP 合成身份 403 / 跨 owner 404）、
      **viewer 被拒请求不产生任何账本与计量记录**、端到端（testecho）计费与控制台录制口径、
      请求日志隐私字段断言、票据与 CSP、高危端点清单与 `console:admin` 审计、草稿计费且不落库

- [x] `internal/apikey`：`VerifyID` 四态（有效/停用/过期/账户挂起在既有用例中覆盖，fake 增 `GetAPIKey`）

- [x] `internal/config`：chat 越界值校验 + `artifact_allow_network` 默认关闭

- [x] `internal/webui`：chat/skills/chart/markdown 资源已内嵌；`chart.js` 上限常量 == `chat.MaxChartSeries/Points`；
      提示词含同一组数字；页面调用的路径与路由表一致

- [x] UI harness：新增 `chat`（45 项）与 `skills`（10 项）视图，SSE 用真实 `ReadableStream` 打桩，
      覆盖 Markdown/图表（含非法回退）/工具卡片/`＋` 菜单勾选/预览票据与沙箱/深链/流式通知

- [x] `make verify` 全绿；`make ui-check` 12 个视图全绿（`chat` 45 项、`skills` 10 项）

- [x] 隔离实例走查（`:8099` + 全新库 + 新二进制 `bin/aigw`，不动 8088）：登录 → `/chat/models` 列出该 Key
      可路由的模型 → 建会话 → 提问跑通真实流式回答（`event: turn/step/text/usage/message/done`）；
      请求日志 `client=console`、`session_id`=会话 id、正文为空、输出/思考未录制；账本出现
      `charge:req_...:1` 扣费；同 `turn_id` 重发**不产生新请求**（仍 1 行日志）；
      `skill-draft` 正常计费并给出「模型没返回可解析 JSON」的骨架草稿；
      HTML5 预览返回 `sandbox allow-scripts; default-src 'none'; connect-src 'none'` + `no-store`，
      无票据 404，**登出后同一票据 404**；SVG 预览只有 `sandbox`（无 `allow-scripts`）；
      `/admin/ui/` 下的 chat/skills/chart/markdown/chat_artifact 资源全部 200

- [x] 补充：会话头部的令牌徽章可点击**改绑** `mcp_token_id`（后端 PATCH 早已支持，之前没有
      界面入口，导致升级前的会话只能丢弃重建）。改绑只换身份，技能/消息/计费绑定不动；
      界面把派生出的 `write_mode` 交给服务端算，不自己推。harness 增加 7 项断言
      （徽章可点、只列可用令牌、预选当前、提示随选择更新、PATCH 只发一个字段、服务端派生值生效）

- [x] 已随用户重启生效：迁移 0011 已在开发库应用（`mcp_token_id` 列存在，旧会话保持 NULL）；
      实测改绑 `query`→`read_only`、`admin`→`allow_writes`、不存在的 id 返回 400 且不改动原绑定

- [x] 修正（真机反馈）：步数与工具次数改为 **`0 = 不限制` 且默认 0**——一次提问一直进行到模型
      不再调用工具为止；配置成正整数时才在达到上限处停止并提示（`chat.max_steps` /
      `chat.max_tool_calls`，负数仍是校验错误）

- [x] 修正（真机反馈）：模型把端点名当工具名调用时（`admin_request_dimensions` →
      `unknown tool`），`chatTools.Call` 现在按 `admin_request` + `name=<端点>` 转发，
      判定仍走同一条允许清单；既不是工具也不是端点的名字回复「本网关提供哪三个工具」

### M33：智能问答改为 MCP 客户端（按令牌 scope 执行）

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 设计决策：控制台「智能问答」不再拥有专属写白名单，而是**当作一个 MCP 客户端**——
      会话绑定 MCP 令牌，权限完全来自该令牌的 scope。旧的默认拒绝写允许清单、
      `chat.high_risk_tools`、`SourceConsole` 身份全部删除，权限只剩一个决定点

- [x] 迁移 `0011_chat_session_mcp_token.sql`：`chat_sessions.mcp_token_id`（可空、不回填，
      旧会话保持「未绑定」需改绑）；`store.GetMCPTokenByID`；会话读写带上该列

- [x] `internal/chat`：`Access`/`SessionInput` 改为携带 `MCPTokenID`；`CreateSession` 必须绑定
      可用令牌（不存在/已撤销/已过期都在表单阶段就拒绝）；`UpdateSession` 改为改绑令牌；
      `write_mode` 降级为按 scope 派生的展示字段（不再接受客户端设置）

- [x] 分层：`internal/chat` 不 import `internal/mcpsrv`（`internal/arch` 断言），scope 名在
      chat 侧本地拼写，并由 `internal/httpapi` 的 `TestChatScopeVocabularyMatchesMCP` 钉住等价

- [x] `internal/httpapi/chat_tools.go` 重写为 MCP 客户端：每次调用构造 `POST /mcp` 请求并交给
      同一个 `handleMCP`（复用仓库既有的「合成请求 + 注入身份」惯用法，见 `chatRunner`）；
      保留端点名当工具名的折叠转发；JSON-RPC 错误转成 `isError` 结果而不打断整轮

- [x] `handleMCP` 增加进程内 principal 注入分支（`withMCPPrincipal`，包内私有），
      外部令牌鉴权路径一行未改

- [x] 测试：`chat_test.go` 原「隐藏并拒绝高危接口」的断言整体重写为按 scope 分组的表驱动测试，
      另加撤销/过期中途失效、只会存令牌 id（不存明文与 hash）、审计 actor 为 `mcp:<name>#<id>`、
      工具面随 scope（14 / 11）、无令牌会话无法创建；`mcp_admin_test.go` 保持不动作为令牌路径不变量

- [x] 界面：新建会话的「写权限」开关换成 MCP 令牌选择器（只列 active 且未过期，选中后显示
      该 scope 的能力摘要）；会话头显示绑定的令牌与派生权限；`scripts/ui-harness` 同步更新

- [x] 补充：会话头部的令牌徽章可点击**改绑** `mcp_token_id`（后端 PATCH 早已支持，之前没有
      界面入口，导致升级前的会话只能丢弃重建）。改绑只换身份，技能/消息/计费绑定不动；
      界面把派生出的 `write_mode` 交给服务端算，不自己推。harness 增加 7 项断言
      （徽章可点、只列可用令牌、预选当前、提示随选择更新、PATCH 只发一个字段、服务端派生值生效）

- [x] 已随用户重启生效：迁移 0011 已在开发库应用（`mcp_token_id` 列存在，旧会话保持 NULL）；
      实测改绑 `query`→`read_only`、`admin`→`allow_writes`、不存在的 id 返回 400 且不改动原绑定

- [x] 修正（真机反馈）「新建会话时 MCP 令牌选不了」：`tokenSelect` 的 `change` 处理函数原本就是
      `loadTokens` 本身，而它先 `clear()` 再重新填充——移除选项会重置 select，随后 append 的第一个
      选项又成为选中项，于是点哪个令牌都弹回第一项（每点一次还多一次 `/mcp-tokens` 请求）。
      代价不只是「选不动」：`创建` 拿的是这个被重置的值，操作员以为选了 `query`，实际绑定的可能是
      列表里第一个 `admin` 令牌。现在选项只在开窗时填一次，`change` 只调 `describeToken()` 更新提示
      （与「改绑」弹窗同一写法）；`/mcp-tokens` 读取失败时提示「读取 MCP 令牌失败：<原因>」，
      不再把请求失败说成「请先去签发一个」（`fetchUsableTokens` 改回 `{ tokens, error }`）

- [x] `scripts/ui-harness`：`chat` 视图补上**此前从未被覆盖的新建会话弹窗**（8 项断言：选项=2 且不含
      revoked、开窗即描述第一个令牌的 scope、切换后选中值不弹回、提示随 scope 变化、切换不额外请求
      `/mcp-tokens`、`创建` 发出的 `mcp_token_id` 就是所选令牌），另加 `rebindModalOpens`；
      `chat` 52 → 61 项。反向验证过：把 `change` 改回 `loadTokens` 时其中 4 项转红
      （含 `newSessionCreateSendsChosenToken`），让 `tokenOption` 抛错时 8 项转红

---

## M34 智能问答的可交互 HTML5 界面（表单 → 提交 → 模型继续）

### 后端：票据 scope 与桥接注入
- [x] 票据 payload 变四元 `artifact|adminSession|expiryNanos|scope`；`scope` = artifact id（交互）
      或 `"view"`（只读）。旧三元 payload 按字段数自然拒绝（`TestUIPreviewTicketCarriesAnAudience`）
- [x] `POST …/artifacts` 与 `POST …/artifacts/{art}/ticket` 接受 `bridge:true`；SVG 与
      `chat.ui_bridge_enabled=false` 都被拒（400），错误文案说明原因
- [x] `GET /admin/chat-artifact/{id}?bridge=1` 只在**票据 scope == 本 artifact** 且是 html 且开关打开时
      注入桥接脚本；查询参数本身不构成升权（`?bridge=1` 打在只读票据上仍是只读）
- [x] 注入 `script#aigw-ui-bridge`（`data-token` = 每响应随机、CSP `nonce` = 每响应随机），
      插到 `<head>`/`<html>` 之后；正文里已有该 id 时幂等跳过
- [x] 响应头新增 `X-Aigw-Bridge: 1`；CSP 在交互预览上多一个 `'nonce-…'`，其余（`sandbox allow-scripts`、
      `default-src 'none'`、`connect-src 'none'`、`no-store`/`nosniff`/`no-referrer`）逐字不变
- [x] 注入脚本是 ES5、无 `fetch`/`XMLHttpRequest`/`eval`/`innerHTML`/`document.write`/存储访问；
      表单提交 `preventDefault` 后走桥接（沙箱没有 `allow-forms`，且**不**为它放开）
- [x] 字段扁平化：跳过 `file`、checkbox 归一为布尔、多选为数组、`__proto__`/`constructor`/`prototype`
      一律丢弃；单事件 8 KiB、单表单 64 字段上限
- [x] 配置 `chat.ui_bridge_enabled`（默认 true）+ `config.example.yaml` + 本机 `config.yaml` 显式写入
- [x] 提示词：契约文本 `chat.DefaultUIBridgeInstructions` 由 `chat.Config.UIBridge` 决定是否追加到系统提示词
      （运维自定义 `system_prompt` 时也生效；关掉开关时不再教模型建表单）

### 控制台：端口、工具栏与回灌
- [x] 新模块 `pages/chat_ui.js`：`createUIPort`（MessageChannel 握手、`doc`/`root` 分离的 `applyUIOps`）、
      `parseUISpec`、限流（最小间隔 1.5s / 单预览 40 次 / 单事件 8 KiB）、拒绝必带原因
- [x] 握手凭证 = 服务端注入的 `data-token`（从**帧自己的文档**读，不是 URL 里的票据，也不是第二份 fetch）；
      校验 `event.source === frame.contentWindow` + token + 必须带 port
- [x] 握手 3 秒无响应 → 工具栏「不可交互」并说明"脚本被沙箱或页面策略拦住"，预览本身仍可查看
- [x] 预览工具栏：桥接状态徽章、已提交计数、`停止生成`（abort 控制台自己那条流）、`重新加载`、
      `加载新版本`（**不自动换页**）、复制源码、新标签打开、关闭（关闭即销毁 port 与监听）
- [x] `chat.js` 抽出 `runTurn(content, { onDelta, onFinish })`：手动提问与界面提交**共用同一条**
      SSE + 计费 + 幂等 + 保存路径；导出 `codeBlocksOf` / `uiEventContent` / `partsText` 供测试直接断言
- [x] 提交内容 = `label` 一行人话 + ` ```json {"source":"ui_event","event":…,"data":{…}} ````（首行顺带成为会话标题）
- [x] 回答边流边回灌（`{k:'d'}`），结束时把 ` ```ui ```` 指令与状态发回页面（`{k:'done'}`）；
      模型若给出新的整页版本 → 工具栏出现「加载新版本」
- [x] 在途轮次时界面提交排队（上限 5）并提示，本轮结束后按序发出；`停止生成` 与手动停止同一语义
- [x] 转录里 ` ```ui ```` 代码块显示条数/规格错误，并提供「重新应用」（页面重开后重放补丁）

### 修正（真机反馈）：交互预览显示「不可交互」
- [x] 根因：控制台去读 `frame.contentDocument` 里的注入标签拿握手凭证，而沙箱（省略
      `allow-same-origin`）让文档变成不透明源，父窗口读到的 `contentDocument` 恒为 `null`
      → 凭证为空 → 直接落到「不可交互」。**这是我的设计错误：凭证被放在了一个父窗口读不到的地方**
- [x] 修法：凭证改由**控制台生成**、随上传请求体提交（`bridge_token`）、服务端注入进页面，
      页面 URL 带 `?aigw_token=`；注入脚本从**自己的 location** 读它并与标签上的 `data-token` 比对。
      凭证从"每响应一个"变成"每预览一个"并落库（迁移 `0012_chat_artifact_bridge.sql`），
      因此重载页面后控制台手里那份仍然有效；缺凭证的交互登记直接 400 并说明少了什么
- [x] 为什么第一版没测出来：harness 用 stub 顶替了 `contentDocument`，把"沙箱文档对父窗口不可见"
      这条**唯一在乎的约束**抹平了。stub 已删除，harness 改为读控制台写进 iframe `src` 的真实
      URL 参数（不替任何真实约束）；`chat` 视图 75 → 78 项，其中新增"URL 带凭证""上传带凭证"
      "凭证与 URL 一致""重载换新凭证"
- [x] `scripts/verify-m34.sh` 同步到新语义并扩到 **34 项**（新增凭证往返、另一份预览的凭证不同、
      不带凭证的交互登记被拒），在真二进制上 34/34 通过
- [x] `make verify` 全绿；`make ui-check` 13 视图全绿

### 修正（真机反馈二）：页面自己的内联脚本被 CSP 拦 + `destroy is not defined`
- [x] 现象 A：模型页面自己的 `<script>` 与 `onclick=` 被拦（"Note that 'unsafe-inline' is ignored if
      either a hash or nonce value is present in the source list"）。根因是上一版为了藏握手凭证给
      `script-src` 加了 nonce——**nonce/hash 一旦出现，`'unsafe-inline'` 被完全忽略**，而这类页面
      就是内联 HTML/JS。这是"用更严的策略把功能本身打掉"
- [x] 现象 B：`ReferenceError: destroy is not defined`（`chat_artifact.js` 的 `openPreview`）——
      重构时删了 `function destroy()` 却留着 `return { destroy, … }`，函数抛异常 → 控制台根本没建端口
      → 必然显示「不可交互」。已改为 `return { destroy: teardown, … }`，并在 harness 里加断言
      "预览弹窗渲染出了状态徽章与计数"（它若抛异常，这条就红）
- [x] 根本修法：**通道不再需要凭证**。交出端口的只能是加载该文档的那个 window；注入脚本只在
      `window.top === window` 时启动（挡住页面里的嵌套框架）；宿主只接受来自那个 frame 的 hello。
      凭证、`bridge_token`、迁移 0012 的列（保留但代码不读写，迁移文件写明缘由）与 CSP nonce 全部撤掉
- [x] 回归保护：`internal/httpapi` 断言 CSP 里既无 `nonce-` 也无 `sha256-`、注入标签不含 `data-token`、
      模型的 `onclick` 与内联脚本原样保留；`verify-m34.sh` 同步（36 项）；harness 删掉全部替身 stub
- [x] 顺手：控制台从来没有 favicon，浏览器每次打开都多一行 404；补一个内联 SVG 图标
- [x] `make verify` 全绿；`make ui-check` 13 视图全绿（chat 78 项）

### 修正（真机反馈三）：问候比监听器更早发出（竞态）
- [x] 根因：控制台先 `append(frame)` 再注册 `message` 监听器；iframe 一进 DOM 就开始加载，
      注入脚本的问候可能在监听器存在之前发出并**被浏览器丢掉**，而脚本只发一次 → 永远握不上手，
      工具栏「等待页面握手」→ 3 秒后「不可交互」
- [x] 两处修：监听器先注册再挂载 frame；问候重发（600ms × 最多 6 次，收到 port 即停，幂等）
- [x] 预览按钮的点击处理器改为 await + catch，失败弹「预览打开失败：<原因>」而不是留下无人处理的
      rejection（此前界面上什么都不说）
- [x] **删掉 harness 的 live 通道块（118 行）**，因为它在结构上无法真实驱动：沙箱 frame 的 window
      对父窗口是跨源对象，`dispatchEvent` 直接抛 `SecurityError`（实测）。以前它"通过"靠的是
      `contentDocument` 替身——那正是上一个缺陷上线的原因。原地写清它为何不可测、验证落在哪一层
- [x] `verify-m34.sh` 扩到 **40 项**（新增：问候会重发、收到 port 后停发、CSP 必须存在且
      `script-src 'unsafe-inline'`——这三条都是本次回归的判据）
- [x] 设计文档新增「哪些能自动测、哪些不能」一节，把四个层次的覆盖方式列成表
- [x] `make verify` 全绿；`make ui-check` 13 视图全绿（chat 98 项）

### 顺手修掉的既有缺陷
- [x] `UpsertChatArtifact` 冲突分支不覆盖 `id`，而上传处理器用自己新生成的 id 拼 URL 签票据 →
      同一代码块第二次预览拿到 **404 的 URL**。改成 `INSERT … RETURNING id` 并回写真实 id
      （`TestUIPreviewUploadUsesTheKeyAsItsIdentity` 从"红"变"绿"）

### 测试与验收

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] `internal/httpapi/chat_ui_bridge_test.go`：交互/只读票据矩阵（含 `?bridge=1` 打在只读票据上、
      票与 artifact 不匹配、旧三元 payload）、幂等注入、SVG 永不注入、nonce 与 token 每响应不同、
      开关关闭后**已签发的交互票据也失效**、注入脚本不含网络/求值/markup API 且保持 ES5、
      提示词契约 == 注入脚本实现的操作、`ui_event` 走正常计费且正文仍不进全局日志

- [x] `internal/webui/embed_test.go`：`chat_ui.js` 已内嵌、`chat_ui.js`/`chat_artifact.js` 不含
      `innerHTML`/`insertAdjacentHTML`/`document.write`（先剥注释，避免测试把自己的说明判成违规）

- [x] `scripts/ui-harness/chat.page.html`：`chat` 视图 61 → **75** 项，新增 `ui` 指令解析/应用/拒绝、
      SVG 属性白名单、握手拒绝矩阵（错 token / 错 source / 无 port / 错帧类型）、表单提交 →
      `ui_event` 请求体、限流与排队（含真实 `MessageChannel` 与延迟关闭的 SSE 流）、
      流式 delta 与 `done`（含 ops）回灌、「重新加载」后旧通道失效

- [x] `scripts/ui-harness/server.py`：harness 静态资源改 `no-store`（原来浏览器会缓存上一版 JS，
      让已修的报错反复出现）

- [x] `make verify` 全绿；`make ui-check` **13 个视图全绿**（`chat` 75 项 + 新增 `bridge` 17 项）

- [x] `bridge` 视图：真实浏览器用 `new Function(source)` 只编译不执行地验证**服务端注入的脚本**
      （Go 拼接出的 JS 没有别的办法证明合法），并断言无 fetch/XHR/eval/innerHTML/存储访问、保持 ES5、
      含握手与表单绑定；夹具由 `TestUIBridgeScriptDumpForTheHarness` 在每次 `go test` 时重写，
      不会退化成"自己和自己一致"的假检查

- [x] `scripts/verify-m34.sh`：部署侧自查 29 项（真实 HTTP、不产生模型费用、结束自动清理临时令牌与会话）。
      **做了方向性验证**：把 `chat.ui_bridge_enabled` 改成 false 重跑 → 10 项转红且退出码 1，
      并停在"服务端拒绝了交互票据"这一句上（不是让人对着一串 404 猜）；改回 true → 29/29、退出码 0

- [x] 写进 `README.md` 常用入口表与 `docs/chat.md` §9（自查一节），排障小节顺延为 §10

---

## M35 内联声明式表单（气泡内渲染 = 提交 = 模型原地更新）

### 设计（`docs/design/m35-inline-forms.md`）

- [x] 不用 iframe：模型给**字段规格**（JSON），控制台用 `createElement` 造元素；任何模型文本只经
      `textContent` 落地。被否决的是"净化后注入"——净化器是一个会出错的信任边界，而且净化过的
      HTML 里脚本仍不能跑，既没有沙箱的保证也没有自由页面的收益
- [x] `chat_form.js` 加入既有的**无标记写入禁令**（`innerHTML`/`insertAdjacentHTML`/`outerHTML`/
      `document.write`/`html:`），与桥接两个文件同列
- [x] 提交复用既有轮次端点（`ui_event` 同形），因此计费、幂等、请求日志与服务端**零改动**：
      无迁移、无新端点、无新配置
- [x] `ui` 指令复用 `applyUIOps`，不另写一套；契约里没有新增任何操作
- [x] 凭据类字段（`password`/`file`）**明确拒绝并给出理由**，而不是降级成文本框

### 顺手修掉的两个真实缺陷（都是 harness 抓出来的）

- [x] **`applyUIOps` 匹配不到 root 自身**：`root.querySelectorAll(selector)` 只搜后代，于是模型用最
      自然的写法 `#form_0` 定位表单（例如挂一条 `message`）永远失败，报"没有节点匹配"而节点明明在。
      修法是给 `applyUIOps` 加一个可选的 `resolve` 钩子，内联表单传入"root 自身优先"的解析器。
      **没有**改成把 root 包进 wrapper：那会在每次指令时重新挂载节点，导致 iframe 重载与焦点丢失
- [x] **指令打在了即将被丢弃的节点上**：`runTurn` 结束时会 `openSession()` 重建整个转录，原先在
      `onFinish` 里应用指令的写法会让更新"闪一下然后消失"——比不更新更糟。改成停放到
      `state.pendingFormOps`，在重建之后按 `#form_<块序号>` 重新找目标再应用

### 实现

- [x] `internal/webui/static/js/pages/chat_form.js`（新增，约 500 行）：`parseFormSpec` /
      `renderForm` / `collectFormValues` / `resolveIn` / `describeFormOps` + `FORM_LIMITS`
- [x] `chat.js`：`form` 块内联渲染（`renderFormBlock`）、提交（`onFormSubmit`/`sendFormEvent`）、
      重建后应用指令（`applyPendingFormOps`）、未提交草稿保留（`formDrafts` + `captureFormDrafts`）、
      队列条目改为带来源的对象（原本只认"一个打开的预览"）
- [x] `markdown.js`：把围栏是否收尾标成 `data-closed`，流式期间不解析半截规格（否则整个回答过程都
      挂着一条"不是合法 JSON"——由"读得太早"造成的、关于模型的假报错）
- [x] `app.css`：表单样式，全部限定在 `.chat-form` 之下（模型不能改控制台的样式）
- [x] `internal/chat/prompt.go`：`DefaultInlineFormInstructions`（格式、字段类型表、`ui_event` 形状、
      `#form_<n>`/`#f_<name>`、两条硬边界）。**不挂部署开关**——内联表单不需要票据、沙箱或桥接

### 测试与验收

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] `internal/webui/embed_test.go`：`chat_form.js` 内嵌断言 + 无标记写入禁令 +
      `chat.js` 的 `form` 路径与 `data-closed` 两侧都在

- [x] `TestInlineFormContractMatchesTheRenderer`：**解析渲染器自己的 `FIELD_TYPES`** 与提示词双向
      比对，拒绝类型点名，id 方案与硬边界必须出现。**做了方向性验证**：临时从提示词里删掉
      `textarea`/`radio` → 测试变红并指出缺哪一个；还原 → 绿

- [x] `scripts/ui-harness/chat.page.html` 新增 `form` 视图（65 项）并注册进 `run.sh` 的 `VIEWS`：
      解析/拒绝矩阵（含凭据与原型污染）、渲染断言（label 绑定、占位项、选项标签、无标记落地）、
      取值形状（数字为数字、复选为布尔、可选项省略）、超限拒绝、忙碌禁用、指令应用与逐条报错，
      以及**完整的 `内联表单 → 提交 → 断言请求体 → SSE 回答 → 指令原地更新` 链路**

- [x] `make verify` 全绿；`make ui-check` **14 个视图全绿**（`chat` 78 → **98** 项、新增 `form` 65 项）

- [x] `docs/chat.md` 新增第 4 节并重编号其后各节与交叉引用；`README.md` 更新；设计文档
      `docs/design/m35-inline-forms.md`

---

## 修：`＋` 菜单技能行的勾选框被撑满整行 + 勾选技能后留空也能直接发送

### 排版

- [x] 根因与 M35 的 radio 完全同源：`app.css` 的全局 `input, select, textarea { width:100% }` 命中了
      这个没有 class 的 checkbox，把它撑成整行宽度（实测把技能名推到菜单最右侧），
      `.plus-item` 的 `gap:8px` 因此成了"看起来的 200px 间距"
- [x] `.plus-skill input[type=checkbox]`：`flex:0 0 auto` + `width/height:16px` + `accent-color`，
      覆盖全局宽度；`.plus-skill span` `flex:1 1 0; min-width:0; overflow-wrap:anywhere` 让长技能名换行不溢出
- [x] 与既有做法一致：`.form-check` / `.form-radio` 就是这么修的，注释里写明是**同一条坑**，避免下次再踩

### 空文本发送

- [x] 控制台侧（`chat.js`）：`＋` 勾选后文本留空点「发送」= 按已加载技能执行。问题文本由页面拼
      （`skillRunText`：`按本会话已加载的技能执行：<技能名>…`）而不是发空字符串——转录与标题显示的就是
      模型真正收到的那句，请求体里也没有空内容
- [x] 没有可执行的技能时**一个请求都不发**，只说清原因：一次点击就是一次计费调用，不能白花。
      Enter 键在空文本框上更是什么都不做（同样的提示用点击给出就够）
- [x] 占位提示与输入框上方的提示随会话技能变化（`composerPlaceholder`/`composerHint`）：只在真能做到时
      才承诺"可留空直接发送"
- [x] 服务端侧（`turn.go`）：空 content 的判定**挪到读取会话之后**——技能 id 本身不算数，必须是
      `loadedSkills` 真能加载出来的技能（技能被删掉后 id 还留在会话上）。有技能则用
      `DefaultSkillRunText` 兜底（直接调接口的路径），否则仍然 400。空提问不是空请求，它是一次计费调用
- [x] 路由文档写清 content 可留空的前提与拒绝条件

### 测试与验收

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] `internal/chat/chat_test.go`：`TestEmptyQuestionRunsLoadedSkills`——空提问无技能必须被拒；
      有技能则存下来的 user 消息与 parts 都是那句可读文案、且**不重复技能正文**（正文仍走 system prompt，
      否则同一份指令会以"用户说的"和"策略"两种身份各到一次）；技能被删后同一个空提问重新被拒

- [x] `internal/webui/embed_test.go`：`TestEmptySendRunsLoadedSkills`——控制台与服务的两半必须同时存在
      （只改前端会发一个空字符串，服务端会以"type a question first"拒绝，用户看到的是关于自己空文本框的报错）

- [x] `scripts/ui-harness/chat.page.html`：`chat` 视图量**真实几何**（勾选框 16px、与技能名间距 8px、
      垂直居中、名字宽 206px 不溢出）、按住一轮断言请求体里是那句人话并断言提示语；新增 `noSkills` 视图
      （技能库为空，页面从**第一帧**就是空库——改 flag 再渲染会晚一轮，页面已经拿到技能了）断言
      「不发请求 + 说清原因」

- [x] `make ui-check` **15 个视图全绿**（`chat` 98 → 108 项、新增 `noSkills` 7 项）；`go test ./...` 全绿

## M37 工具轮的 `reasoning_content` 覆盖整段 assistant 侧（真机反馈）

- 触发：M19e 之后仍报同一条
  `upstream_400: The \`reasoning_content\` in the thinking mode must be passed back to the API.`
- 复现（离线，逐形态扫）：把 M19d/M19e 的规则**扩展到 DSH 真实会话的每一种形态**，发现两处漏网：
  ① `[用户, 工具轮(带键), 工具结果, 文本(无键)]` —— 模型看到工具结果后写的那段叙述；
  ② 历史上**先**出现过工具调用、当前请求里第一条 assistant 消息就是它的形态（① 的推广）。
  两者的共同点：这段文本**不紧挨**在工具调用之前（中间隔着 `tool` 消息），M19e 的折叠够不到它，
  而 M19d 的补键只打给"带 `tool_calls` 的那条消息"，于是它带着"缺键"出行
- 根因：约束的粒度是"**工具轮涉及的整段 assistant 侧**"，网关此前按"那一条带工具调用的消息"实现
- 修复（`pkg/providerkit`）：翻译收尾新增 `requireReasoningKeys`——请求里存在工具调用时，
  给**每一条 assistant 消息**补上该键（已有正文的保留正文，没有的用空串）。
  纯聊天请求（历史上没有工具调用）仍然完全不发该字段，所以"打开开关就让所有上游看到陌生字段"不成立
- 测试：`providerkit` 1 条（两轮工具 + 叙述：每条 assistant 都带键、正文不串台、纯聊天不出字段）
  + `openaichat` 1 条（"工具结果 → 叙述"形态走 `renderBody` 配置路径）；两处都变异验证过
- `scripts/deepseek-smoke.sh` 的假上游**改为真判 400**：此前它只 `print` 请求形态（`replay=`/`assistant=`），
  于是"上游会拒"的请求在这里照样绿灯——M19d/M19e 两次都是真机才发现。现在它按"thinking 未显式关闭 →
  每条 assistant 必须带键"的规则直接回 400，并新增 `keyed=` 计数；新增"工具结果后的叙述"一条断言
  （假上游开启真判后，这条断言在改回旧逻辑时确实红）
- 附带修掉冒烟里暴露的第二个真 bug（**续接 404 竞态**）：审计行由后台批量写入，
  `GET /v1/responses/{id}` 会等（`AwaitResponse`），但 `previous_response_id` **续接不等**，
  于是"刚拿到 `id` 就接着发下一轮"的 agent 循环会读到 `response not found`。
  离线冒烟里这条断言一直是红的（`curl: (22) ... 404`），只是此前没人追。
  修复：续接路径用同一个等待。这也正是 thinking + tools 的必经形态
- 文档：`docs/api-providers.md` §3（约束的完整范围 + 真实后果）、§4（出站与续接的等待）

---

## M38 会话粘性路由 + 授权范围内故障转移

### 设计（`docs/design/m38-session-affinity.md`）

- [x] 粘性键 = `api_key_id + session_id + canonical_model`；session_id 取 `prompt_cache_key`（128 字节截断），
      无该字段的请求完全不参与（无 session 客户端的字节级行为不变）
- [x] 只在**同一 `route.priority` 层内**提升到该层首位：跨层提升会让一次失败永久反转运营者写下的优先级意图
      （`docs/routing.md` §4.2「层间即优先级降级」），坏路由交给既有熔断/冷却剔除
- [x] 有效策略为 `strict_order` 时不重排——"永不打散"是该策略的全部意义
- [x] 只有 attempt **成功**才写粘性；只有**可重试失败**才清粘性，且只清"正好指向它"的记录
      （4xx 类客户端错误说明不了路由坏了）
- [x] 命中候选若已撤权/停用/draining/冷却/熔断/能力不足/不再映射 → 记录删除并按原生策略路由；
      重新启用不会复活旧粘性
- [x] 进程内、TTL（默认 1800s，命中即刷新）、容量上限（默认 10000，先清过期再淘汰最旧）；
      **不持久化**（重启后重新负载均衡，无迁移、无清理任务）
- [x] 钉死请求（`model@provider` / `X-Gateway-Provider`）既不读也不写粘性；参与与否由 `Plan` 决定，
      以不可构造的不透明标记 `Result.Affinity` 交给调用方，避免调用点忘记判断 pinned
- [x] 不新增响应头、请求日志列、迁移、端点、hook 字段：流式分支本就不设响应头（M32），
      可观测性走 `/stats` 聚合 + 结构化日志

### 实现

- [x] `internal/config`：`routing.session_affinity`（默认 true）、`session_affinity_ttl_s`（1800）、
      `session_affinity_max_entries`（10000）+ `GW_ROUTING_SESSION_AFFINITY*` + 仅开启时校验
- [x] `internal/responses`：`(*Request).SessionKey()`，`Dimensions()` 复用它（截断只有一处定义）
- [x] `internal/routing/affinity.go`（新）：有界 TTL 表 + 命中计数（hit/miss/stale/evict）
- [x] `internal/routing/routing.go`：`Config` 三字段、`Plan` 接入层内提升与 stale 删除、
      `Result.Affinity`、`NoteSuccess`/`NoteFailure`/`AffinityStats`
- [x] `internal/httpapi/v1.go`：`RouteRequest.SessionID`、成功写粘性、可重试失败清粘性
- [x] `internal/httpapi/admin.go` + `cmd/aigw`：`/stats` 的 `affinity` 块；启动日志报开关/TTL/容量

### 测试与验收

- [x] `internal/routing/affinity_test.go`：TTL 到期（注入 `now`）、命中刷新、容量淘汰最旧、键三轴隔离、
      `nil` store 空转、并发 put/get（`-race` 真跑）、统计计数
- [x] `internal/routing/affinity_test.go`（路由层用例与表用例同文件，同属 `routing` 包）：同层提升、
      跨层不提升、`strict_order` 不参与、stale 删除且不复活、`NoteFailure` 只清"就是它"的记录、
      pinned 不产出槽、无 session 时顺序等于策略输出、配置零值不开启
- [x] `internal/httpapi/affinity_test.go`：三个 `testecho` 供应商用不同 `prefix` 暴露"谁作答"——
      同层两家等价时的会话粘性（连发 6 次同一家）、flaky(priority 5) 失败后由已授权的好供应商作答、
      只授权 flaky 的 key 即使复用别人的 session_id 也**不能**借到好供应商、粘性目标被停用后同会话改由另一家作答
      并重新绑定、无 `prompt_cache_key` 的请求完全不碰粘性表
- [x] `internal/config/config_test.go`（默认值/YAML/env/仅开启时校验）+ `internal/responses/dimensions_test.go`
      （`SessionKey()` == `Dimensions().SessionID`）+ `/stats` 的 `affinity` 块形状断言
- [x] 变异验证（逐条临时改代码后必须精确变红）：① `Plan` 不应用粘性 → 路由用例 + 端到端用例同时红；
      ② 提升时越过 tier 边界 → 跨层用例红；③ 去掉 stale 删除 → 不复活用例红；④ `NoteFailure` 不清粘性 → 对应用例红；
      ⑤ pinned 分支也产出槽 → pinned 用例红；⑥ 成功回调不写粘性 → 端到端用例红
- [x] `make verify`（vet + 全量 test + build）全绿；`-race` 在 `internal/{routing,config,responses}` 与
      `internal/httpapi` 的 M38 用例上全绿。**注意**：`internal/httpapi` 的 5 个 `audit_batch_test.go` 用例在
      `-race` 下会失败（批处理落库来不及），这是**既有问题、与 M38 无关**——已用 `git worktree` 在 `08d6a81`
      （本里程碑之前）复现同样的 5 个失败，因此不把 `make test-race` 记作全绿
- [x] 回填设计文档「实现与设计差异」；`docs/routing.md` §4.4 与 `config.example.yaml` 同步

### 观察项 / 待人工执行

> 本节的未完成项仍在 `docs/TODO.md` 的同名小节。

- [x] 部署（2026-09-13）：gpt001 已换成 `d637209`（`make build` → `scp` → `install` → `systemctl restart aigw`），
      `readyz`/`healthz` 200、重启后 0 条 ERROR、公网 `mnl`/`gpt` 两个入口 200；
      启动日志 `strategy=weighted_random session_affinity=true affinity_ttl=30m0s affinity_max_entries=10000`；
      `/stats` 报 `version=d637209` 且 `affinity={entries,hits,...}` 随线上 DSH 会话增长。
      回滚点：`/opt/aigw/aigw.prev-20260913-154048`（旧版 `97ac621`），回滚 = `mv` 回去 + 重启。
      线上 `config.yaml` 没有 `routing:` 段，因此**靠默认值开启**（按用户决定不改配置）

## M39 对外版本号（`/version` + 控制台角标）与 `response_format` 语义修正

设计文档：`docs/design/m39-version-and-format.md`；规格文档：`docs/api-providers.md`（§2 字段表与警示、§3 DeepSeek 片段）、`README.md`（入口表 + 状态）。

### 版本号

- [x] `VERSION` 文件（`a.b.c`）作为版本真值，`Makefile` 的 `version-check` 拒绝非法写法（`1.2`/`v1.2.3`/空）
- [x] 构建注入改为 `-X main.version`（来自 `VERSION`）+ `-X main.revision`（`git rev-parse --short HEAD`）；
      `main.commit` 重命名为 `main.revision`，`-version` 输出 `aigw 0.1.0 (revision …, built …)`
- [x] `GET /version` → `{"version":"0.1.0","revision":"<短 sha>"}`，公开、带 `base_path` 前缀、不碰数据库
- [x] `/healthz` 增加 `revision`（既有 `status`/`version` 不变）
- [x] 控制台左上角 `AI Gateway` 旁显示 `v<a.b.c>` 与短 revision 两格（`js/brand.js`，模块级 Promise 缓存，读不到就留空不报错）
- [x] 测试：`internal/httpapi/version_test.go`（端点 + `/healthz`）、basepath 表加 `/aigw/version`、
      `scripts/ui-badge-test.mjs`（node，11 项，含变异验证）、`scripts/ui-harness/brand.page.html`（有浏览器时的同一批断言）、
      路由覆盖断言 9 → 10 条公开路由
- [x] 收尾：`make ui-base` 纳入 `make verify`；`README.md` 文档表/入口表/脚本表同步

### `response_format` 语义修正（线上故障）

- [x] 复现与定位：线上 gpt001 的 `deepseek` 供应商 `config.response_format="json_object"`（存数据库、不在 `config.yaml`），
      `openai-chat` 无条件下发 → DeepSeek 拒绝不含 "json" 的提示词；`journalctl -u aigw` 无该错误是因为
      **网关侧没有失败记录**（4xx 来自上游、请求被计费层正常收尾），只有 DSH 侧看到 `upstream_400`
- [x] 代码：档位改由请求 `text.format` 决定（`requestFormat`），配置只作能力申报并保留取值校验；
      `json_schema` 原样透传客户端的整个对象
- [x] 能力门槛按级：`featuresOf` 把 `json_object` 映射到 `json_object`、`json_schema` 映射到 `json_schema`，
      `text`/缺省不要求任何能力（否则会要求一个没人申报的 `text` 能力）
- [x] 解析层校验 `text.format.type`（未知取值/非对象 → 400），`text` 档位在 `ToProviderRequest` 归一化为"无格式"
- [x] 测试：`TestResponseFormatFollowsTheRequest`（openaichat，含"配了 json_object 也不下发"这条回归）、
      `TestParseValidatesTextFormat`、`ToProviderRequest` 归一化、`TestFeaturesOf*`、
      `internal/routing/format_capability_test.go`（`json_object` 与 `json_schema` 互不蕴含）
- [x] 变异验证：把 `renderBody` 改回"按配置下发"，openaichat 的三条用例立即失败（复现线上形状）
- [x] 真机形状回归脚本 `scripts/format-smoke.sh`：把 `config.response_format=json_object` 留在配置里，用真实二进制
      打假 DeepSeek 并逐条读上游收到的请求体。**变异验证**：用修复前的二进制跑，输出
      `#1 keys=…,response_format` + `response_format = {"type": "json_object"}` 并 FAIL（正是线上那条形状）；
      用修复后二进制跑：`#1 keys=max_tokens,messages,model`（无该字段）、`#2` 带 `{"type":"json_object"}`、非法档位 400
- [x] 线上配置清理（2026-09-13 16:02）：经管理面 `PATCH /admin/api/v1/providers/8` 删掉 deepseek 供应商的
      `response_format` 键（剩余键 `base_url/models/thinking/timeout_s`）；能力改由模型 `capabilities.json_object`
      申报，`router/explain?model=deepseek-flash` 显示候选 `deepseek`、`degraded=null`

### 发布 skill

- [x] `scripts/release.sh`：`patch|minor|major` 升 `VERSION` → 提交 → 打 `v<a.b.c>` tag → `make build`；
      脏工作区、tag 已存在、非法档位一律拒绝
- [x] `.dsh/skills/release-version/SKILL.md`：完整发布流程（定档位 → 升版本 → 部署 gpt001 → 用 `/version` 与
      角标验证 → 回滚点 → 记录），含本次这条 `response_format` 坑的提示

### 首次发布记录（v0.1.0 → v0.1.4）

- [x] `scripts/release.sh patch` 两次：`0.1.0`（`VERSION` 新建）→ `0.1.1` → `0.1.2`（tag 指向包含该版本号的 commit）
- [x] 部署 gpt001：`scp` → `cp aigw aigw.prev-<时间戳>` → `install` → `systemctl restart aigw`；
      回滚点 `/opt/aigw/aigw.prev-20260913-160120`（旧版 `d637209`）
- [x] 验证：`/aigw/version` = `{"revision":"be84cdf","version":"0.1.2"}`、`/aigw/healthz` 200（含 revision）、
      `readyz` 200、启动日志 `version=0.1.2 revision=be84cdf` 且无 ERROR、管理面仍要会话（`auth/me` 401）、
      公网 `https://mnl.iotalking.top/aigw/version` 同值、控制台 `admin/ui/js/brand.js` 已随二进制发布（200）
- [x] **上游真机对照（同一个 DeepSeek key，只差一个字段）**：无 `response_format` → 200；
      带 `{"type":"json_object"}` → 400 `Prompt must contain the word 'json' …`——线上那条失败的确切形状，
      证明删掉该字段是必需的，而不只是"看起来更干净"
- [x] **端到端验收（2026-09-13，用 DSH 本体而非人工点按）**：宿主终端搭隔离 `DSH_HOME`
      （拷 `settings.yaml`、把 aigw 供应商的 `baseURL` 指向 `ssh -L 18088:127.0.0.1:8088 gpt001` 隧道、
      `agent-default-model` 改成 `aigw/deepseek-flash`、密钥只走 `AIGW_API_KEY`），跑
      `node <DSH>/lib/bin.js --profile headless "用一句话回答：1+1 等于几？不要使用任何工具"`
      → 输出 `1+1 等于 2。`、**退出码 0**（此前同一形状首轮即 `upstream_400`）。
      网关侧 `request_logs` id=452/453 `client=dsh model=deepseek-flash`，请求体只有 `input/model`、
      **没有 `text`**，状态 `completed`/`incomplete`（不是 failed）。验收用的临时 Key（id=14）已 revoke，
      隧道与临时目录已清理
- [x] 顺带补一条永久防线：`TestResponseFormatFollowsTheRequest` 增加"带 tools + instructions + 工具轮历史的
      请求体里没有 `response_format`"用例（就是 DSH 那类流量），避免只有"无工具"形状被覆盖
- [x] 版本演进：`0.1.0`（新建 `VERSION`）→ `0.1.1`（角标 + 发布 skill）→ `0.1.2`（真机形状回归脚本）
      → `0.1.3`（部署记录）→ `0.1.4`（端到端验收记录 + 工具形状防线）。每个 tag 都指向内含该版本号的 commit，
      所以构建产物自报的版本号与 `git log` 里能查到的来源一一对应——这正是"线上跑的是哪个版本"能当依据的原因
- [x] 最终线上状态（2026-09-13 16:05）：`aigw 0.1.4 (revision 2d2d731)`，`readyz` 200、重启后 0 条 ERROR、
      回滚点 `/opt/aigw/aigw.prev-20260913-160527`；公网 `https://mnl.iotalking.top/aigw/version` =
      `{"revision":"2d2d731","version":"0.1.4"}` 与宿主 `HEAD` 一致（`0.1.3` → `0.1.4` 的差异只在测试与文档，
      运行时二进制逐字节相同，仍然发版是为了让"线上版本 = 仓库版本"这条不变量成立）

---

## M40 MCP 工具说明的完整性 + 智能问答优先用表单

### 形状（缺口的根因）
- [x] `adminField` 增加 `Schema`/`Example` + `schemaField`/`exampleField`/`structuredField` 辅助函数
- [x] `adminRoute.bodySchema()` 对「声明为 object 却没有形状」的字段 **panic**（原来的静默降级就是根因）
- [x] `bodySchema` 抽取 `schemaForFields(where, fields)`，panic 文案指向 `docs/mcp.md §4.5`
- [x] `example()`/`sampleBody()`/`sampleValue()` 优先使用声明的示例（不再给 object 生成 `{}`）
- [x] `summaryRow()` 增加 `body_fields`（概览行直接给出请求体字段名，少一次 describe 往返）

### 价格规则集（本次故障的字段）
- [x] 新增 `internal/httpapi/admin_pricing_schema.go`：`pricingRuleSetSchema` / `pricingRuleSetExample` /
      `salePricingExample` / `pricingRuleSetJSONSchema`，字段严格对应 `pricing.RuleSet/Rule/When/Tier/TimeWindow`
- [x] 单位写死在 schema 顶层与 rates 描述里：**微单位/百万 token（200000 = $0.20/1M）**
- [x] 维度名（input / input_cache_hit / input_cache_miss / output / reasoning）逐个带中文说明
- [x] `additionalProperties:false` 与 `DisallowUnknownFields` 对齐；`docs/pricing.md` 里**未实现**的
      `when.monthly_usage`、`when.region` **不写进 schema**（写了就是教 agent 写 400）
- [x] 接线四处：`admin_upsert_provider_model.pricing_rules`、`admin_upsert_model.sale_pricing`、
      `admin_update_model.sale_pricing`、`admin_validate_pricing`（RawBody 换成规则集 schema）
- [x] 修正 `admin_simulate_pricing.dimensions` 的既有错误说明（维度键是 `input`/`output`，不是 `input_tokens`）

### 其余结构化字段（同一缺陷族）
- [x] 新增 `internal/httpapi/admin_field_schemas.go`：policy / grants / capabilities / capabilities_override /
      provider config·credentials·meta·timeout_overrides / price_overrides / 模型与路由 policy /
      dimensions / dimension_markup_bp / 事件名与技能 id 数组
- [x] **只存不读的字段如实标注**：`accounts.price_overrides`、模型级 `policy`、路由级 `policy` 三处
      在描述里写明「当前不生效」，并指出该用什么替代（不假装它能配置）

### 守卫（防复发）
- [x] `TestStructuredBodyFieldsCarryTheirShape`：任何 object 字段没有 Schema 即失败（附修复指引）
- [x] `TestBodyFieldsAreDocumentedInTheCatalogue`：`body_fields` 与声明一致
- [x] `TestMCPDescribeCarriesThePricingRuleSchema`：describe 必须给出规则集字段名、单位与 catch-all 示例
- [x] `TestMCPPricingExampleIsWritable`（端到端回归）：示例 → `admin_validate_pricing` 通过 →
      `admin_upsert_provider_model` / `admin_upsert_model` 真实写库成功
- [x] `internal/mcpsrv/tools_contract_test.go`：11 个查询工具的说明必须含中文、「返回：」段、用到时机的表述、
      点名相邻工具，并覆盖自身 schema 的每个参数；period 枚举跨工具一致

### 工具说明本身
- [x] 11 个查询工具描述改中文并按四要素重写（用途 / 何时用与分工 / 参数默认值与单位 / 「返回：」段）
- [x] `queryToolNames` 由声明表派生（`queryTools()`），消灭"名字三处写"
- [x] 三个后台工具描述改中文：明确「先看有什么 → 查怎么用 → 执行」、返回形状、`truncated`、
      `tool=null` 的语义、`body_fields` 的用途、`admin_read` 的 403 说明
- [x] `initialize.instructions` 改中文并写入三步工作流与「按 body_schema 写、不猜字段名」

### 智能问答优先用表单
- [x] 基础提示词新增第 3、4 条规则：写配置前必须 describe 并按 body_schema 写、**不因"不确定字段名"而拒绝**、
      形状缺失就明说且绝不猜；参数不全先问再动手，可枚举项做成下拉，能推断的默认值写进 value 并说明
- [x] 反例一起写进去：纯查询且默认合理时**不要**拦着用户填表，直接给答案并注明口径
- [x] 输出格式一节写死选取顺序：`form`（要信息）→ `chart`/`svg`（要图）→ `html`（要自由排版/脚本）
- [x] 内联表单一节标明「这是默认手段」，新增「什么时候该出表单」「拿到填写结果之后」两节
      （先做完事再用 `ui` 原地更新，不要新开一张表重问）
- [x] `DefaultUIBridgeInstructions` 的标题与开头标注「仅在需要自由排版或页面脚本时用」
- [x] 表单里的按钮点击**不等于**危险接口的同意（`confirm` 规则不变）
- [x] `internal/chat/prompt_test.go` 钉住以上三条行为（读的是真正装配出来的提示词）

### 文档
- [x] 新增 `docs/design/m40-tool-descriptions-and-forms.md`
- [x] `docs/mcp.md` 新增 **§4.5 工具说明标准**（四要素模板、路由 body 字段判定表、反例、失败信息解读）
- [x] `docs/PROCESS.md` 提交前自检增加一条：改工具说明/字段必须同时补形状与示例
- [x] `docs/chat.md` §3/§4 补模型侧选取顺序与"不猜字段名 / 先 validate 再写"
- [x] `docs/pricing.md` 补 MCP 侧拿到规则集 schema 的路径 + 标注未实现字段

### M40 发布记录

- **版本**：`0.1.4` → **`0.2.0`**（minor）。M40 新增了对外可见的能力（`admin_endpoints` 响应新增
  `body_fields`、工具说明与 `initialize.instructions` 全量重写为中文、`admin_describe` 的
  `body_schema`/`example` 补齐所有结构化字段），没有改签名或默认语义，因此是 minor 而不是 patch。
  唯一的行为变化是智能问答侧：模型在缺关键参数时会用内联表单先问，而不是自行挑一个默认值。
- **提交与 tag**：`2e8d3c8`（M40 实现）→ `e77af79`（`release: v0.2.0`，tag `v0.2.0` 指向它）
- **构建物**：`aigw 0.2.0 (revision e77af79, built 2026-09-13T08:53:03Z)`，
  md5 `88b45d45999561928876c8dbd519e65a`（本地与上传后一致）
- **部署目标**：gpt001 `127.0.0.1:8088`；**回滚点** `/opt/aigw/aigw.prev-20260913-165328`（= 0.1.4）
- **部署后验证**：
  - `GET /aigw/version` = `{"revision":"e77af79","version":"0.2.0"}`（公网 https 同一值，
    控制台角标 `brand.js` 读的就是这个端点，因此角标显示 `v0.2.0 e77af79`）
  - `healthz=200`、`readyz=200`、`/admin/ui/` = 200
  - 启动日志 `msg="aigw starting" version=0.2.0 revision=e77af79`，近 5 分钟内 `level=ERROR` 计数 **0**
- **部署前在隔离实例上完成的端到端走查**（本地 `bin/aigw 0.2.0` + 临时库，跑完即删）：
  1. `admin_describe(admin_upsert_provider_model)` 返回的 `pricing_rules` 含
     `additionalProperties:false`、单位字样（`200000 = $0.20/1M`）、维度名与 catch-all 示例——
     正是这次故障里缺的那份形状；
  2. 用运维原场景的数值（$0.20/1M 输入、$1.20/1M 输出、$0.02/1M 缓存命中）走
     `admin_validate_pricing`（`valid:true`）→ `admin_upsert_provider_model`（200）→
     `admin_upsert_model`（201）→ `admin_simulate_pricing`：100 万 token × 3 维 = **1.6 USD**，
     与手算一致（顺带在这条链上发现模型级 `markup_bp: 0` 被静默当成未设置，见 `docs/pricing.md` §11）
- **部署后在线上能做的只读验证已全部通过**（上条）；线上 `admin_*` 工具的逐字段确认需要一个
  admin scope 的 MCP 令牌，本轮没有可用凭据，因此**未做**：操作侧只需在「MCP 令牌」页签一个
  admin 令牌，让会话问一句"给某个上游模型配成本价"，即可看到模型先 `admin_describe` 拿
  `body_schema` 再写，而不是像之前那样要求补文档
- 回滚方式：`cp /opt/aigw/aigw.prev-20260913-165328 /opt/aigw/aigw && systemctl restart aigw`

### Codex 兼容修复发布记录（2026-09-13）

- [x] 发布 **v0.2.4**（patch），修复 DSH 使用 Codex 订阅模型时可选工具参数被严格模式强制填写、导致 Full access 下反复无效提权的问题。
- [x] 修复提交 `4b4f554`；发布提交与标签 `bcdb192` / `v0.2.4`。
- [x] 网关与 Codex 插件均从发布提交构建，19:26 部署 gpt001；本地和线上 SHA-256 一致。
- [x] 验证：全量 Go 测试、go vet、format-smoke、版本角标测试通过；公网与本机 `/aigw/version` 返回 `0.2.4 / bcdb192`，healthz/readyz 正常；控制台页面与 brand.js 可访问，线上 brand.js 与发布源码一致。启动日志无 ERROR，Codex 插件成功启动。
- [x] 发版前已用线上 gpt-6-astra 验证修复前后差异，并实际执行 pwd、回传工具结果完成第二轮；见 `docs/dsh-codex-tool-strict-fix.md`。
- 回滚点：`/opt/aigw/aigw.pre-v0.2.4`、`/opt/aigw/plugins/provider-codex.pre-v0.2.4`；恢复两个二进制后重启 aigw。插件回滚点包含本次发版前已部署的 strict 修复。

### Codex 缓存键透传修复发布记录（2026-09-13）

- [x] 根据最新标签 v0.2.4 与本次缺陷修复性质，发布 **v0.2.5**（patch）：客户端 `prompt_cache_key` 经标准供应商请求、插件 JSON 协议与 Codex 请求构造原样发送上游，空值省略；不改变会话粘性键的处理。
- [x] 修复提交 `3e86f4a`；发布提交与标签 `eb8ca6b` / `v0.2.5`。
- [x] 网关与 Codex 插件从发布提交构建，19:35（UTC+08:00）部署 gpt001；配置与数据未修改。
- [x] 本地与线上 SHA-256 一致：网关 `43a71b46a3ea94aa02354c6adf045d22db4a300e2d9dc1c2c82f07b00bd43efb`；Codex 插件 `b5972c2b019fa4f6c66676cb1e38c5573afba45df21685badeeabeb1f1f6e17a`。
- [x] 验证：全量 Go 测试、go vet、format-smoke、base path（10 项）与版本角标（11 项）测试通过。缓存键回归覆盖流式/非流式、缺省/空值及带空格、中文、超过 128 字节的键。
- [x] 本机与公网 `/aigw/version` 返回 `0.2.5 / eb8ca6b`；healthz/readyz 正常；控制台页面 HTTP 200，线上 brand.js 与发布源码一致（通过资源与端点验证，未做浏览器目视验证）。启动日志无 ERROR，Codex 插件成功启动。
- 回滚点：`/opt/aigw/aigw.pre-v0.2.5`、`/opt/aigw/plugins/provider-codex.pre-v0.2.5`（均为上一版部署二进制）；恢复两个二进制后重启 aigw。
- 未做：真实上游缓存命中率对照实测；透传修复不保证每次请求命中缓存。

### 请求日志会话标识修复（2026-09-13）

- [x] 查阅 OpenAI 官方 Codex `rust-v0.153.4` 源码和 App-server 文档，确认根 `session_id`、当前 `thread_id`、父线程与缓存键的区别。
- [x] 日志优先读取显式根会话元数据，兼容请求头，保留缓存键回退；不增加关联表、历史匹配或数据库查询。
- [x] 元数据标题识别、分组/筛选、录制关闭与脱敏、流式/非流式、缓存透传回归；全量测试及 vet 通过。
- [x] 身份解析微基准：典型 Codex 元数据约 10 µs/request；无元数据回退零分配。见 `docs/session-grouping-fix.md`。
- 边界：当前临时标题线程及 DSH 子代理缺少已确认的根标识，旧日志未保存元数据，无法据此保证截图历史行自动归并；本次未部署。

## M41 请求维度小时汇总（2026-09-13）

- [x] 小时 × 八维实际组合汇总、源版本与发布世代、日志/计量事务内失效触发器；迁移 0013 不扫描历史。
- [x] 完整有效小时读汇总，当前小时、时间边界及失效区间按时间索引补算；统计行与分组总数来自同一读快照。
- [x] 请求数与已计量数按请求去重；token、缓存命中与费用累计全部尝试；日志清理后立即退出统计。
- [x] 每批 500 行历史发现和暂存写入，持久化游标、版本比较发布、未完成世代清理与重启恢复。
- [x] 默认开启 `recording.dimension_rollup_enabled`，支持环境变量覆盖；关闭 WAL 回退明细；stats/metrics 暴露进度与诊断。
- [x] 八维交叉筛选、三种排序、时间边界、迟到计量、生产批量结算、事务回滚、清理、迁移及恢复对照测试；race、`make verify` 与 format-smoke 通过。
- [x] 10 万/100 万 × 重复会话/独立会话/账户偏斜六组探针通过。百万重复会话 p95 14.50 s → 38.45 ms，独立会话 13.07 s → 4.26 s；小记录写入吞吐下降约 52%–62%，如实保留代价。
- [x] 设计与性能记录：`docs/design/request-dimension-rollups.md`；规格：`docs/request-log.md`。
- 边界：每请求独立会话时无行数压缩；关闭汇总仍保留失效触发器；浏览器 UI 检查因缺少 Firefox 跳过。

## M44 供应商并发上限与排队等待（2026-09-14）

- [x] 设计文档 `docs/design/m44-provider-concurrency-queue.md`；规格：`docs/routing.md` §4.5、
  `docs/api-responses.md`（429 `provider_busy` + 计量口径）、`docs/mcp.md`（后台可写并发上限、读回实时在途与排队、
  排队策略只读、`max_inflight` 示例与 §4.5「行为型字段」）、`docs/design/m8b-admin-resources.md`、
  `docs/architecture.md`。**编号从 M43 改为 M44**：`max_inflight` 的字段本来是"存了不生效"的死字段，本次让它在
  `internal/runtime` 的进程内闸门里真正生效。
- [x] `internal/config`：`routing.provider_queue_wait_s`（默认 30，`0` = 不排队、超限即失败）与
  `routing.provider_queue_max_waiters`（默认 100，`0` = 深度不限）+ `GW_ROUTING_PROVIDER_QUEUE_{WAIT_S,MAX_WAITERS}`；
  校验非负，并要求 `wait × max(1, max_attempts) < billing.reservation_ttl_s`（排队期间请求仍持有余额预留）。
- [x] `internal/runtime/capacity.go`：每供应商 FIFO 名额闸（`Release` **精确交付队首**、等待者取消/超时时把已交付的
  名额**转交下一位**、上限动态变更按"最近观察到者胜"并唤醒队列），`max_inflight = 0` 直接放行且不建状态；
  闸门在 `bal.Acquire` **之前**，因此排队的请求不计入 `least_inflight` 与延迟 EWMA（有测试钉住）。
- [x] 失败语义：等待超时 / 队列已满 / 不排队而超限 → `*runtime.CapacityError`（`errors.Is(err, ErrProviderBusy)`，
  `Retryable` 为真）→ 换下一个候选；全部候选耗尽 → **429 `rate_limit_error` / `provider_busy`** + `Retry-After`
  （流式走 `response.failed`）；客户端取消 → ctx 错误、不换候选。探测/重启等后台动作**不占名额**。
- [x] 排队时长不计入 `usage_records.latency_ms`/`ttft_ms`（`runtime.Attempt{QueueWaitMS}` 交回后扣除并 clamp）；
  被拒尝试照常写一行 `status=failed` / `error_code=provider_busy` / `terminated_reason=provider_capacity`、
  `usage_source=unavailable`、**零费用**（与既有"插件启动失败"等未出网尝试一致）。
- [x] 可观测：`/metrics` 的 `aigw_provider_capacity_{limit,inflight,waiting}` 与
  `..._{admitted,queue_full,timeouts,cancelled,wait_ms}_total`（`target="provider:<id>"`）；
  `/admin/api/v1/stats` 的 `provider_capacity`（含生效排队策略 `queue_wait_s`/`queue_max_waiters`）；
  供应商列表/详情/创建/更新行内 `capacity`；排队 ≥1s 记 Info、被拒记 Warn（带 `request_id`）。
- [x] **MCP 可设置并发数**：`admin_update_provider` / `admin_create_provider` 的 `max_inflight` 说明补齐
  （单位/范围/默认/排队与 429 语义，两处共用同一常量 `maxInflightDesc`）；`admin_list_providers` /
  `admin_get_provider` / `admin_stats` 摘要在内同步；`admin_read` 可读、写需 `admin`（端到端测试钉住）。
- [x] 控制台：编辑/新建表单「最大并发（0=不限）」+ 排队提示；列表「在途/排队」列（hover 给累计/超时/队满）；
  详情页「在途/排队」行。harness 新增 `#capacity` 视图（8 项检查通过）。
- [x] 测试：门单元 11 项（不限/恰好 N/队首 FIFO/超时/队满/不排队/取消转交/上限升降/Release 幂等/计数/
  200 并发 limit 8 峰值 ≤ 8）；派发层 7 项（`QueueWaitMS`、排队不计 inflight、可重试、无上限并发、取消后名额可复用、
  6 个请求在 limit 2 下峰值 ≤ 2）；HTTP 端到端 4 项（串行排队 + `latency_ms` 扣除排队 + 429 `provider_busy` +
  默认不限无异味）；MCP 写入-读回-生效 1 项；配置校验 8 项。`make verify` 通过，
  `go test ./internal/mcpsrv/ ./internal/httpapi/` 通过。
- 边界（如实记录）：
  - 进程内状态：多实例部署各自计数，有效上限 = N × 实例数；重启后排队与计数清零。
  - 缓冲：队列深度与等待时长是**部署级**配置（MCP 只读）；每个供应商只有"并发上限"可写。
  - `#plugin` 视图在 UI harness 里**失败**（`pluginTableShown`/`pluginFieldsAfterHandshake`），
    用 HEAD 版 `providers.js` 复跑结果相同 → 与本次改动无关（fixture/渲染路径的既有问题），未在本次修复。
- 非目标：跨进程/集群并发、按账户公平排队、route/model 级上限、队列持久化、MCP 写排队策略、
  账户级（query）暴露供应商并发、`usage_records` 加 `queue_ms` 列。

### v0.10.0 发布记录（2026-09-14）

- [x] 依据 `v0.9.0..HEAD` 的新增能力发布 minor：`0.9.0` → **`0.10.0`**。包含 M43 收尾（`93e5649`：sub2api 迁移自检、
  标签授权预检、report 定位修复）与 **M44 供应商并发上限 + 排队等待**（`224ec13`）。
- [x] 发布提交 `9368b07`，标签 `v0.10.0`；版本真值 `VERSION=0.10.0`，二进制 `aigw 0.10.0 (revision 9368b07)`。
- [x] `make verify`（vet + 全量 `go test ./...` + ui-base 跳过因无 node + build）、
  `go test ./internal/mcpsrv/ ./internal/httpapi/`、`scripts/format-smoke.sh`（普通请求不带 `response_format`、
  `json_object` 按需下发、非法 level 在解析期拒绝）全部通过。
- [x] 12:00:37（UTC+08:00）部署 gpt001：先 `scp` 到 `/opt/aigw/aigw.new`，备份线上二进制后 `install` 并重启；
  仅替换二进制，`/opt/aigw/config.yaml` 与数据目录未改动，服务 `active`。
- [x] 本地与线上二进制 SHA-256 一致：`aa123f292a902402579f8d5cac3fb83652ba7643feb2200f2ac575a935e94502`。
- [x] 验证：gpt001 `localhost:8088/aigw/version` 与公网 `https://mnl.iotalking.top/aigw/version` 均为
  `0.10.0 / 9368b07`；healthz/readyz 均 HTTP 200；启动日志 `version=0.10.0 revision=9368b07` 且重启后**无 ERROR**；
  控制台 HTTP 200，公网 `js/brand.js` 与源码 SHA-256 逐字节一致（`5c92efb8…`）→ 角标会显示 v0.10.0。
- [x] 启动日志同时确认新闸门已接线：`provider capacity ready queue_wait=30s queue_max_waiters=100 limited_providers=1`。
- [x] 12:03:28（UTC+08:00）部署 **gptjp**（`8.211.157.165`）同一个 `v0.10.0` 二进制：同样先落到 `/opt/aigw/aigw.new`、
  备份后 `install` 并重启；`/opt/aigw/config.yaml`（SHA-256 `d9b78f7f77f72377005c8c753e2c3ae6910aa306746ab4590195df1c68e75f46`）
  与数据目录未改动，`aigw.service` active。
- [x] gptjp 验证：本机 `localhost:8088/aigw/version` 与公网 `https://gpt.lagenio.xyz/aigw/version` 均为
  `0.10.0 / 9368b07`；healthz/readyz/控制台均 HTTP 200；启动日志 `version=0.10.0 revision=9368b07` 且重启后
  **无 ERROR**；公网 `js/brand.js` 与源码 SHA-256 一致（`5c92efb8…`）。
- [x] gptjp 的闸门接线：`provider capacity ready queue_wait=30s queue_max_waiters=100 limited_providers=0`
  —— 该机四个供应商（三个 codex 插件 + deepseek）的 `max_inflight` 全为 `0`，因此**行为与 0.9.0 完全一致**，
  没有任何排队；需要限流时再在控制台/MCP 设「最大并发」。
- **运维须知（本次发布后行为变化，仅 gpt001）**：gpt001 上 `deepseek`（id 8）的 `max_inflight` 一直是 **2500**——
  该字段在 M44 之前存了不生效，现在**真的生效**了：该供应商最多 2500 个在途上游调用，超出后在队列中等待
  （30s / 最多 100 个排队），等待超时或队满才会换候选/返回 429 `provider_busy`。2500 对当前流量等于"不限"，
  无需处理；若本意是"完全不限"，把该字段写成 `0`（控制台「最大并发」或 MCP `admin_update_provider`），
  0 表示不限且**不建闸门**。gptjp 上没有设过上限，不受影响。
- 回滚点：gpt001 `/opt/aigw/aigw.prev-20260914-120037`、gptjp `/opt/aigw/aigw.prev-20260914-120328`
  （均为 v0.9.0，`338d4cc`）；`cp` 回对应二进制后 `systemctl restart aigw`，不回退数据库。
- 两台机器的二进制 SHA-256 与本机构建一致：`aa123f292a902402579f8d5cac3fb83652ba7643feb2200f2ac575a935e94502`。
- 未做：未对线上付费上游做并发/排队实测（无付费模型验证）；未做浏览器目视检查（以资源哈希、`/version`
  与探针替代）；`#plugin` UI harness 视图的既有失败仍未修（见上文 M44 边界）。

### gptjp 插件版本错配修复与 deepseek-flash 别名下线（2026-09-14）

- 背景：gptjp 的 `/opt/aigw/plugins/aigw-provider-codex` 是 **9/11 15:24 的旧构建**（`9c80dec1…`），
  而网关已是 9/14 的 0.9.0/0.10.0。旧插件缺 `0db0c19`（保留输入项未知字段）、`f1452d2`（数组型工具输出）、
  `a2599a3`（自定义工具调用）、`3e86f4a`（prompt_cache_key）四个修复，造成 11:34–11:41 共 **13 条失败**：
  6 条 `bad_params`（`json: cannot unmarshal array into Go struct field Item.input.output of type string`，
  25ms 本地解码失败、fatal 不 failover，Codex CLI 自行重试 6 次）与 7 条 `missing_required_parameter`
  （插件吞掉 `additional_tools.tools`，上游报 `input[0].tools` 缺失）。两处都用「本地假上游 + 新旧插件二进制」
  对照复现：旧插件复现，新插件均正确转发。
- [x] 12:07:53 部署新插件（从 `21a6f10` 构建；插件相关代码与该机网关 `9368b07` 逐字节同源）：
  `/opt/aigw/plugins/aigw-provider-codex` SHA-256 `d210f6ea4a0fa9244a5d27e4c4a5c532ae8e3319fa163454ae96d7cab3218c61`；
  回滚点 `/opt/aigw/aigw-provider-codex.pre-plugin-skew-20260914-120753`（`9c80dec1…`），
  刻意放在插件扫描目录之外（宿主的 `ResolveBinary` 对目录内文件名做子串匹配）。
- [x] 12:08 三家 codex provider（1/3/5）配置：删除 `deepseek-flash → gpt-5.6-astra` 别名
  （该 ChatGPT 账号类型不支持该模型，健康探测与真实请求都被上游 400 拒绝，11:49–11:51 的 5 条 `upstream_error`
  就是它，DSH 会话因此中断），并显式设 `health_model=gpt-5.6-luna`；配置备份
  `/opt/aigw/data/provider-config-backup-20260914-120819.json`（0600），`config_version` 1→3。
  `provider_models` 目录项 1/18/35（三家 codex 的 `deepseek-flash`）已删除——只删路由不够，
  目录项还在就会在下次同步时把路由带回来。
- [x] 验证：三家 provider 健康探测 `ok:true`（此前全失败，`last_error` 已清空）；三个插件进程
  `/proc/<pid>/exe` 的 SHA-256 等于新二进制；真实请求四类全绿——流式 `pong`、**数组型工具结果
  （修复前必 `bad_params`）200 completed**、非流式 200 completed、`deepseek-flash` 落到 provider 7
  （真 deepseek）200 completed；12:07 之后新增失败 **0 条**（累计仍为 24 条历史失败）。
  验证用的临时 key 45/47/49 已停用，远端 admin cookie 已清理。
- [x] 本机 `bin/aigw-provider-codex` 与 `plugins/aigw-provider-codex` 一并刷新为新构建，旧产物留作
  `plugins/legacy-codex-plugin-20260911.bin`（旧名字会让宿主把它当成候选），避免下次部署再拷到旧文件。
- 未做（三条待办）：① 插件的 `Info().Version` 硬编码 `0.1.0`，控制台与启动日志看不出构建差异——
  这正是本次错配无人察觉的原因，建议照网关用 ldflags 注入 version/revision；
  ② `deepseek-flash` 现在只剩 provider 7，而三个标签（蓝精灵1/2/3）各自只授权一个 codex provider，
  持这些标签的 key 调 `deepseek-flash` 会得 403 `permission_denied`，若希望其继续可用需在标签授权里
  加 `deepseek` 或重新确定别名口径（属产品口径，未擅自改）；③ 非流式失败请求只写计量、不写请求日志
  （`internal/httpapi/v1.go` 失败分支仅在 `req.Stream` 时 persist），11:04–11:05 有 9 条这样的记录。

### gptjp「智能问答改不了供应商并发」的根因与修复（2026-09-14）

- 现象（用户报）：在 gptjp 的智能问答里让模型「把三个 codex 供应商的并发改成 3」，模型回一句
  「我先查一下…」之后**这一轮就断了**（会话消息 `status=failed`、`error=模型这一轮没有正常结束`），
  永远走不到 `admin_update_provider`。两个会话 `chat_coigrlhiwkgflserk3bwjfui` /
  `chat_6uvtp4tqu7e5ai7p2szthjfy` 都是这个形态；审计里只有 `mcp.admin_call … ok` 的只读调用，
  从没有一次 provider 写入。
- 根因：**不是权限、不是 MCP 工具面、也不是 M44 的并发闸门**，而是 gptjp 的 `deepseek`（id 7）
  供应商漏配 DeepSeek 的思考方言。每轮第 1 步（用户消息 → 模型决定调工具）成功，第 2 步
  （**回放工具结果**）被上游 400 拒绝：`usage_records.error_code=upstream_400` /
  `terminated_reason=upstream_error`（`req_ysojaxlkwaupdlrcvtzy64e3`、`req_dq2hdkw234xhc4zxp3po4krp`、
  `req_cquxxpil25j4bqm26t5zyijt`，provider_id=7、零 token、325–421ms）。
  上游的硬约束是「带工具的一轮 assistant 侧必须带 `reasoning_content` 键」（`docs/api-providers.md` §4），
  而网关的补键/文本折叠只在供应商配置打开 `thinking.replay_reasoning_content` 时生效
  （`pkg/providerkit/chatcompat.go`：`reasoningTurn := opts.ReplayReasoningContent && …`、
  `foldAssistantTextIntoCall`、`requireReasoningKeys`）。gptjp 的 config 只有
  `{"base_url":"https://api.deepseek.com","timeout_s":120}`；gpt001 的 deepseek（id 8）一直是对的
  （`{"mode":"auto","style":"deepseek","replay_reasoning_content":true}`），所以只有 gptjp 有这个病。
- 复现（gptjp，临时会话，跑完即删）：**"先写一句话 + 调 1 个工具" 必 400**；
  "不写话、直接调 1 个工具" 反而通过。即触发条件是**assistant 侧在工具轮里带了文本**，
  而不是工具个数或结果大小——所以用户看到的「有时能聊、真要动手就断」并不是随机的。
- [x] 修复：备份 `/opt/aigw/data/provider-config-backup-20260914-143714.json`（0600），
  `PATCH /admin/api/v1/providers/7` 只改 `config`（整块替换，必须连同 4 条 `models` 一起提交，
  否则会丢模型映射），补上 `thinking={"mode":"auto","style":"deepseek","replay_reasoning_content":true}`；
  读回确认 `models` 四条映射与 `api_key` 凭据键完好。
- [x] 顺手完成用户原本的目标：`PATCH providers/1`、`providers/3`、`providers/5` → `max_inflight=3`
  （三家 codex 的并发闸门在此之前都是 `0`＝不限），读回 `capacity.limit=3`，
  `/admin/api/v1/stats.provider_capacity` 显示三个闸门在线，排队策略 30s / 100。
- [x] 端到端验证（临时会话 `chat_6fiq2pw6ymosee4wpalv5f24`，绑 scope=admin 的 MCP 令牌 #7，跑完已删）：
  ①「先一句话 + 查供应商」的三步轮次 `completed`（修复前必死在第 2 步）；
  ② 让模型真实写入一次（id=1 的 `max_inflight` 3→2，带 `confirm=true`）→ 返回 200、
  独立 `admin_get_provider` 读回 `capacity.limit=2`；③ 再让它改回 3 → 读回 3。
  三个供应商最终都是 `max_inflight=3`、`capacity.limit=3`；服务 `active`，0.11.0/`460eee7`，重启后无 ERROR。
- 回滚：把 `/opt/aigw/data/provider-config-backup-20260914-143714.json` 里 id=7 的 `config` 原样 PATCH 回去
  （即删掉 `thinking` 块）即可；`max_inflight` 改回 `0` 即恢复不限。
- 观察项（未改）：gptjp 的 `deepseek` 只服务 `deepseek-flash` 一族，任何"带文本的工具轮"在没有该方言时都会
  400，属于**供应商配置与上游方言不匹配**这一类问题；这类校验只在真机才会显形（离线假上游按规则判 400，
  但配置缺 `thinking` 时假上游也一样会放行），值得在部署清单里加一条「openai-chat 指向 DeepSeek 官方
  endpoint 时必须带 `thinking.style=deepseek` + `replay_reasoning_content=true`」。

### v0.3.0 发布记录（2026-09-13）

- [x] 根据 `v0.2.5..HEAD` 的新增能力与配置发布 minor：`0.2.5` → **`0.3.0`**。包含缓存 token 展示 `c72bb52`、Codex 无输出断流恢复 `bcd6f1a`、根会话标识 `4b1c32e`、M41 小时汇总 `ecae61a`。
- [x] 发布提交 **`7e68bab`**，标签 **`v0.3.0`**；版本说明 `docs/releases/v0.3.0.md`。
- [x] `make verify`、format-smoke 通过；汇总相关 race 与六组规模探针已通过；UI harness 因无 Firefox 跳过。
- [x] 网关与 Codex 插件从发布提交构建，21:19:38（UTC+08:00）部署 gpt001；配置 SHA-256 保持一致，网关启动自动应用迁移 0013。
- [x] 本地/线上 SHA-256 一致：网关 `c31db6367b496503d167fa82c7420d35a509fac061447d0ecd832300475b353a`；插件 `834a98c5d650eb0da99185464dbeff872a12fbacf92f076920a2efc895fe9145`。
- [x] 本机/公网 `/aigw/version` = `0.3.0 / 7e68bab`，healthz/readyz/UI HTTP 200；线上 brand.js 与发布源码一致（资源与端点校验，无浏览器目视验证）。服务 active，插件成功启动，启动时段 ERROR 为 0。
- [x] 数据库 `quick_check=ok`；历史发现游标 1428、完成标记为 true，9 个已结束小时已发布、81 条组合汇总、待处理小时为 0；当前小时继续实时补算。
- [x] 在线数据库同一只读事务中，对已发布小时的八个分组逐一比较汇总和原始 JOIN：请求/计量数、各项 token、费用、首次/最近时间、标题/工作区完全一致。
- 回滚二进制：`/opt/aigw/aigw.pre-v0.3.0`、`/opt/aigw/plugins/provider-codex.pre-v0.3.0`；一致性数据库快照 `/opt/aigw/backups-release/v0.3.0/aigw.sqlite`（0600，已 quick_check）。二进制回退保留新数据及派生结构，不以旧快照覆盖发版后的计费记录。
- 未做：本次发布未额外发起付费上游请求；流式恢复修复的先前真机验证见 `docs/dsh-codex-stream-recovery-fix.md`。

### v0.3.1 发布记录（2026-09-13）

- [x] 发布 patch **0.3.1**：独立 Codex 标题线程精确提示词关联，支持乱序完成、候选冲突恢复及持久化证据；录制/脱敏策略限制见 `docs/releases/v0.3.1.md`。
- [x] 修复提交 `b45d411`；发布提交 `ad42b4a`，标签 `v0.3.1`；main 与标签已推送 origin。
- [x] `make verify`、format-smoke 通过；网关与 Codex 插件均从发布提交构建。
- [x] 部署 gpt001，本地/线上 SHA-256 一致：网关 `dca7ac14e9f55225a129c72ff8be23833f1121ec1c54a7245f552d7742b7833d`，插件 `2a8a3c9c726dfe5c97da4b62fea10d19a56fbf3a82e8b6f641d8be7646983a65`。
- [x] 本机 `/aigw/version` 返回 `0.3.1 / ad42b4a`，healthz、readyz、UI 均 HTTP 200；服务 active，数据库 quick_check=ok，迁移 0014 存在，配置 SHA-256 与部署前一致。
- 回滚点：`/opt/aigw/aigw.pre-v0.3.1`、`/opt/aigw/plugins/provider-codex.pre-v0.3.1`；数据库一致性备份 `/opt/aigw/backups-release/v0.3.1/aigw.sqlite`（0600，quick_check=ok）。回退不覆盖新计费数据。
- 未额外发起付费上游请求；未进行公网或浏览器目视验证。新自动关联以集成回归为验证依据，先前人工修正的历史例子不作为自动关联实测。

### v0.8.0 发布记录（2026-09-14）

- [x] 账户名支持邮箱、中文等非空 Unicode 字符（trim 首尾 Unicode 空白、非空、有效 UTF-8、最多 64 个 Unicode 字符；不改大小写、不做 NFC），发布 minor **0.8.0**；规则与语义边界见 `docs/design/m8b-admin-resources.md` §7。
- [x] 本版同时包含上一版之后的 **账号级标签继承**（`8a71c4e`）。实现提交 `29cb087`；发布提交 `c3ff39a`，标签 `v0.8.0`。
- [x] 全量 `go test -count=1 ./...` 与 `go vet ./...` 通过；控制台 UI 走查 16 个 view 通过（`brand` 首轮超时，单独重跑通过）。
- [x] 10:34:58（UTC+08:00）部署 gptjp（`8.211.157.165`）；仅替换 `/opt/aigw/aigw`，`/opt/aigw/config.yaml` 未改动（SHA-256 `d9b78f7f77f72377005c8c753e2c3ae6910aa306746ab4590195df1c68e75f46`），数据目录未动，`aigw.service` active。
- [x] 本机与 gptjp 二进制 SHA-256 一致：`9350e02eb1092697bb1c9e0b0cdba8b5b29480bdce2047ac88ec4aa2b6762f06`。
- [x] gptjp 本机与公网 `https://gpt.lagenio.xyz/aigw/version` 均返回 `0.8.0 / c3ff39a`；healthz/readyz 均 HTTP 200（上一版本记录中的 readyz 503 已恢复）；启动日志版本正确、重启后 `level=ERROR` 计数为 0。
- [x] 新能力生效证据（只读）：运行的实例已提供新控制台资源，`/aigw/admin/ui/js/pages/accounts.js` 第 36 行含「支持邮箱、中文和其他 Unicode 字符；去除首尾空白后最多 64 个字符」，公网同一资源同样命中。
- [x] 10:35:56（UTC+08:00）同步升级 gpt001（`0.7.1 / 235e193` → `0.8.0 / c3ff39a`）；仅替换 `/opt/aigw/aigw`，`/opt/aigw/config.yaml` SHA-256 `b3531983905d52a54e75e6d0baaeb500c7496d182425017fa93936ac66a24153` 部署前后一致，数据目录未动，`aigw.service` active。
- [x] gpt001 本机与公网 `https://mnl.iotalking.top/aigw/version` 均返回 `0.8.0 / c3ff39a`；healthz/readyz 均 HTTP 200；启动日志版本正确、重启后 `level=ERROR` 计数为 0；控制台公开页 HTTP 200，`/aigw/admin/ui/js/pages/accounts.js` 第 36 行含新提示（公网同一资源命中）。
- 未在线上创建测试账户（避免污染生产数据），接口层行为由 `internal/httpapi`、`internal/store`、`internal/config` 的集成测试覆盖；未发起付费上游请求，未做浏览器目视检查。
- 回滚点：gptjp `/opt/aigw/aigw.prev-20260914-103458`（v0.7.2）；gpt001 `/opt/aigw/aigw.prev-20260914-103556`（v0.7.1）。`cp` 回对应二进制后 `systemctl restart aigw`，不回退数据库。

### v0.7.2 发布记录（2026-09-14）

- [x] 发布 patch **0.7.2**；发布提交 `9cd209a`，标签 `v0.7.2`。
- [x] 构建二进制版本为 `0.7.2`，revision `9cd209a`；部署 gptjp（`8.211.157.165`），服务 `aigw.service` active。
- [x] 配置 `/opt/aigw/config.yaml` 使用 `server.base_path: /aigw`，公网前缀为 `https://gpt.lagenio.xyz/aigw/`；Nginx 配置已备份并重载。
- [x] 本机与公网 `https://gpt.lagenio.xyz/aigw/version` 返回 `0.7.2 / 9cd209a`；healthz HTTP 200。
- [x] 启动日志显示版本正确且无 ERROR；readyz 当前 HTTP 503，因为新实例尚无 providers/routes（服务本身已正常监听）。
- 回滚点：远程 `/opt/aigw/aigw.prev-*` 与 `/home/nginxWebUI/nginx.conf.pre-aigw-*`；恢复二进制/配置后重启 `aigw` 并重载 `nginxWebUI`。

### v0.7.1 发布记录（2026-09-14）

- [x] 修复管理后台布局：左侧导航与“退出”项固定，工作区顶部标题栏固定，页面内容独立滚动；发布 patch **0.7.1**。
- [x] 实现提交 `aa5b313`；发布提交 `235e193`，标签 `v0.7.1`；main 与标签已推送 origin。
- [x] `make ui-base`、`make test` 通过；构建二进制版本为 `0.7.1`，revision `235e193`。
- [x] 06:48（UTC+08:00）部署 gpt001，仅替换 `/opt/aigw/aigw`，配置与数据目录未修改，服务 active。
- [x] gpt001 与公网 `https://mnl.iotalking.top/aigw/version` 均返回 `0.7.1 / 235e193`；healthz/readyz 均 HTTP 200，启动日志无 ERROR。
- [x] 本地与线上二进制 SHA-256 一致：`0dd9f483d4bd2b8962dca869c7d75857659064152eea6c03bb71e9089e2e6bd2`。
- 回滚点：`/opt/aigw/aigw.prev-20260914-064852`；恢复该二进制后重启 aigw，不回退数据库。

### v0.7.0 发布记录（2026-09-14）

- [x] 新增智能问答 `update_session_title` 工具：模型可更新当前会话标题，沿用 owner 校验、统一标题清洗和审计记录；发布 minor **0.7.0**。
- [x] 实现提交 `935d0e8`；发布提交 `687aeae`，标签 `v0.7.0`。
- [x] 全量 `go test ./...` 通过；构建二进制版本为 `0.7.0`，revision `687aeae`。
- [x] 06:32（UTC+08:00）部署 gpt001；仅替换 `/opt/aigw/aigw`，配置与数据目录未修改，服务 active。
- [x] gpt001 `/aigw/version` 返回 `0.7.0 / 687aeae`；healthz/readyz 均 HTTP 200，启动日志无 ERROR；公网 `https://mnl.iotalking.top/aigw/version` 同值。
- [x] 本地与线上二进制 SHA-256 一致：`a6370b2e1b7fffaa5b5a17f8c7f9f091006580f9adb8a10f20a0fbda4547b848`。
- 回滚点：`/opt/aigw/aigw.prev-20260914-063249`；恢复该二进制后重启 aigw，不回退数据库。

### v0.6.1 发布记录（2026-09-14）

- [x] 根据 v0.6.0 后的修复提交发布 patch **0.6.1**：`bb8776c` 修复聊天工具状态实时更新并记录 reasoning effort；`415cebe` 修复 DSH 最新运行时工作区解析、新版 developer 工作目录识别及会话最新非空工作区聚合。
- [x] 发布提交 `6ca5f22`，标签 `v0.6.1`；`make verify`（vet、全量 Go 测试、UI base/badge/request-log 测试、构建）通过。
- [x] 06:13:07（UTC+08:00）部署 gpt001，仅替换网关二进制；配置 SHA-256 部署前后一致，服务 active。启动时首次探针遇到端口尚未监听，自动重试成功。
- [x] 本机二进制、gpt001 与公网 `/aigw/version` 均为 `0.6.1 / 6ca5f22`；healthz/readyz 正常，启动日志无 ERROR。公网 UI HTTP 200，角标 brand.js 与源码 SHA-256 一致（资源与版本端点验证，未做浏览器目视检查）。
- [x] 本地与线上二进制 SHA-256 一致：`361f5263d86e30db37a743d68e8bfad95de1b4e31df478e5c6ba19531573933e`。
- 回滚点：`/opt/aigw/aigw.prev-20260914-061307`（v0.6.0）；恢复该二进制后重启 aigw，不回退数据库。未回填历史工作区，未发起付费模型测试。

### v0.6.0 发布记录（2026-09-14）

- [x] 新增规范/对外模型级 reasoning 配置（inherit/default/force）、管理 API/MCP 与控制台表单，发布 minor **0.6.0**；设计与操作说明见 `docs/design/model-reasoning.md`、`docs/mcp.md`。
- [x] 实现提交 `9f88fee`；发布提交 `0af1c12`，标签 `v0.6.0`。全量 `go test -count=1 ./...` 与功能相关 race 测试通过。
- [x] 05:44（UTC+08:00）部署 gpt001；仅替换 `/opt/aigw/aigw`，未修改 `/opt/aigw/config.yaml` 或数据目录，服务 active。
- [x] 本机、gpt001 与公网 `/aigw/version` 均返回 `{"revision":"0af1c12","version":"0.6.0"}`；healthz/readyz 均 HTTP 200，启动日志无 ERROR。
- [x] 回滚点：`/opt/aigw/aigw.prev-20260914-054404`；恢复该二进制后重启 aigw，不回退数据库。

### v0.5.0 发布记录（2026-09-13）

- [x] 新增智能问答“创建技能”工具：通过 SSE 展示创建过程，生成草稿后由用户确认保存；发布 minor **0.5.0**。
- [x] 实现提交 `d10f9fb`；发布提交 `1e40420`，标签 `v0.5.0`。
- [x] 全量 `go test ./...` 通过；构建二进制版本为 `0.5.0`，revision `1e40420`。
- [x] 部署 gpt001；服务 active，`/aigw/version` 返回 `{"revision":"1e40420","version":"0.5.0"}`，healthz/readyz 均 HTTP 200，启动日志无 ERROR。
- [x] 公网 `https://mnl.iotalking.top/aigw/version` 返回 `0.5.0 / 1e40420`。
- 回滚点：本次部署生成的 `/opt/aigw/aigw.prev-20260913-223547`；恢复该二进制后重启 aigw，不回退数据库。

### v0.4.0 发布记录（2026-09-13）

- [x] 新增 M42 stdio MCP 转发能力，发布 minor **0.4.0**；发布说明 `docs/releases/v0.4.0.md`。
- [x] 实现提交 `97dfd6d`；发布提交 `13a120d`，标签 `v0.4.0`；main 与标签已原子推送 origin。
- [x] 实现阶段 `make verify` 与命令包 race 测试通过；发布提交构建成功，二进制帮助包含 endpoint/token-env。
- [x] 22:01:04（UTC+08:00）部署 gpt001；仅替换网关二进制，Codex 插件保留；配置 SHA-256 与部署前一致。
- [x] 本地/线上网关 SHA-256 一致：`7731c8bfa0a976d1109e4fabb7d1e6717721edc680c72183d74ce0a43471ac33`。
- [x] 本机与公网 `/aigw/version` 返回 `0.4.0 / 13a120d`；healthz/readyz 正常，UI 与 brand.js HTTP 200；公网 brand.js 与源码逐字节一致。服务 active，启动日志无 ERROR。重启后的首次连接尚未监听，自动重试后成功。
- 回滚点：`/opt/aigw/aigw.pre-v0.4.0`（v0.3.1）；恢复该二进制后重启 aigw，不回退数据库。
- 未发起付费模型验证，未做浏览器目视检查；新转发模式的权限和写入依赖真实 HTTP + 临时 SQLite 集成测试验证。

## M43 API Key 哈希导入 + sub2api「智天成」用户迁移（2026-09-14）

设计：`docs/design/m43-api-key-hash-import.md`；规格/运行手册：`docs/sub2api-migration.md`。

- [x] 管理接口 `POST /admin/api/v1/keys/import`：只收 `key_prefix` + `key_hash`，明文不进网关、不返回、不进审计。
- [x] 校验：前缀长度/字符集、哈希 64 位 hex、账户存在（404）、标签必须存在（400，避免静默回落到默认通配授权）、状态 active|disabled、RFC3339 过期时间。
- [x] 冲突规则：同哈希幂等更新；`created_by` 以 `import:` 开头可更新；控制台签发的 key 占用同前缀 → 409。
- [x] `store.FindAPIKeyByPrefix`（未命中返回 `(nil,nil)`，与数据面 `GetAPIKeyByPrefix` 的 401 语义分离）。
- [x] 路由表条目含完整 body 形状/示例（`docs/mcp.md` §4.5 守卫通过）；MCP `admin` scope 自动可见。
- [x] 测试：真实 bearer 认证/前缀与截断拒绝、响应与审计不含密钥材料、形状与错误表、幂等与 409、viewer 403、store 方法。
- [x] `scripts/sub2api-migrate.py`：plan/snapshot/apply/verify/report；哈希在源库 SQL 内计算，脚本从不 `SELECT key`。
- [x] 分配规则：显式意图分组（21/22/23 → 蓝精灵2/3/1，数据校验）+ 其余按 (全局最少, 该用户最少, 标签 id) 均衡。
- [x] 预检覆盖：key 形状、前缀唯一性与冲突处置（`--reissue-key`）、账户名冲突、标签授权完整性、目标库占用。
- [x] 迁移工具补齐：`selftest`（自造密钥走同一导入接口 + 真实数据面请求，证明前缀/哈希链路；用 0600 令牌文件复用同一把自检 key）、`snapshot` 忽略自检残留、`plan` 报告模型名覆盖。
- [x] gptjp 实测：`plan` 逐把核对分配（22 用户 / 32 key，蓝精灵1=11、蓝精灵2=11、蓝精灵3=10，其中 key #24 因前缀冲突重签）。
- [x] gptjp 迁移执行：快照 → 建 22 个账户（不带标签）→ 导入 31 把 key → 重签 1 把（aigw key #44，标签 蓝精灵2）→ `verify` 全绿。
- [x] 验证：三标签各用一把真实迁移 key 发一次请求，`usage_records.provider_id` 依次为 1/3/5；仅前缀与截断明文当 bearer 均 401；长 `sk-` 在 journal 命中 0、报告 0、数据库里 60 处命中全部是「前缀+该行 sha256」相邻字段。

### v0.9.0 发布记录（2026-09-14）

- [x] 新增管理接口 `POST /admin/api/v1/keys/import`（只收前缀与哈希），发布 minor **0.9.0**；实现提交 `13841c3`，发布提交 `338d4cc`，标签 `v0.9.0`。
- [x] `make verify`（vet + 全量 Go 测试 + 构建）通过；新增端点测试覆盖真机认证路径、形状表、409 冲突、幂等重跑、viewer 403 与审计无密钥材料。
- [x] 11:00:23（UTC+08:00）部署 gptjp；仅替换 `/opt/aigw/aigw`，配置与数据目录未动，服务 active，回滚点 `/opt/aigw/aigw.prev-20260914-110023`（上一位 `…-103458` 为 v0.8.0）。
- [x] 11:00:54（UTC+08:00）同步升级 gpt001（0.8.0 → 0.9.0），仅替换 `/opt/aigw/aigw`，配置与数据目录未动，服务 active，回滚点 `/opt/aigw/aigw.prev-20260914-110054`（上一位 `…-103556` 为 v0.8.0）。
- [x] 两台机本机与本地二进制 SHA-256 一致：`a612ae9642db7b12bee34dcd16e8e7c2652cd8029200040317334241f6b1b1f1`；`/aigw/version` 均为 `0.9.0 / 338d4cc`，healthz/readyz 200，重启后 `level=ERROR` 计数 0。

### M43 迁移执行记录：sub2api「智天成」→ gptjp ai_gateway（2026-09-14）

- 来源：gptjp（8.211.157.165）sub2api 库，`users.notes='智天成' AND deleted_at IS NULL` → **22 个用户**；其未删除且 active 的 key → **32 把**（6 把已删除、1 把 `quota_exhausted` 未迁移，报告已列出）。
- 目标：同机 aigw（`/opt/aigw`，0.9.0）；账户名=sub2api 用户名，备注记 `sub2api user #<id> · <email>`，账户一律**不带标签**（避免与 key 标签取并集放大授权）。
- 分配：显式意图分组 21/22/23 → 蓝精灵2/3/1（12 把），其余 20 把按 (全局最少, 该用户最少, 标签 id) 均衡 → **蓝精灵1=11 / 蓝精灵2=11 / 蓝精灵3=10**。
- 前缀冲突：sub2api key #24（收纳/E26Q）与 #51（郑晓婷）前 12 字符同为 `sk-f69aeca55`；保留 #51，**#24 在网关重签**为 aigw key #44（标签 蓝精灵2），明文只在控制台/响应出现一次，已暂存 `/opt/aigw/data/E26Q-reissue-key.txt`（0600，待交付本人后 `shred -u`）。
- 迁移前发现并修正的**目标侧缺陷**：三个已有标签的 `grants` 只有 `providers`、没有 `models`，因此绑上去的 key 每个请求都 403；管理 API 因标签名含中文而拒绝写入（名称校验是 ASCII-only），故按既有先例**直写 SQLite** 补 `"models":["*"]`（providers 原样），随后重启 reload 并用管理接口读回复核。改动前值：`蓝精灵1={"providers":["liuhui-wisskys-8-expiry"]}`、`蓝精灵2={"providers":["lizhichao-wisskys-3-expiry"]}`、`蓝精灵3={"providers":["lzhichao-lagenio-3-expiry"]}`。
- 回滚点：`/opt/aigw/data/aigw.db.pre-sub2api-20260914-110412`（标签修正前）、`…-110602`（导入前，0600，均 quick_check=ok）；二进制 `/opt/aigw/aigw.prev-20260914-110023`。
- 验证证据：`verify` 逐把比对源库重算的 prefix/hash 与 aigw 行（31/31 一致）、`effective_tags` 逐把等于预期单一标签、账户标签为空；三标签各用一把真实迁移 key 发一次请求，`usage_records.provider_id` = 1/3/5 且 status=completed（蓝精灵1 首轮遇到一次 `server_is_overloaded`，重试即成功）；前缀当 bearer、截断明文当 bearer、未知前缀、无 key 均 401。
- 保密：明文只在 postgres 进程内算哈希（`encode(sha256(convert_to(btrim(key),'UTF8')),'hex')`），脚本从不 `SELECT key`；迁移窗口 journal 长 `sk-` 命中 0；报告 JSON 命中 0；aigw 库内 60 处长 `sk-` 命中经逐条比对**全部**是记录中 `key_prefix` 与 `key_hash` 相邻字段（长度恒为 76），无一处是明文。
- 已知残留（自检工具产生，均停用）：账户 `zz-migration-selftest`（closed）+ 自检 key #1/#3/#5（disabled）、3 个已撤销的临时 MCP 令牌、`/opt/aigw/data/.selftest-token`（0600）。网关没有删除账户/key 的路由，故保留为记录。
- 未迁移/未修（待决策）：计费余额与额度；（模型面）源库近 30 天有 **13 个模型名、2390 次请求**在 aigw 无对应 models/映射，其中 `gpt-4o`(974)、`gpt-5.4-mini`(622)、`gpt-5.6`(412) 量最大；另外两个**供应商映射错误**已实测确认：`deepseek-flash` → 上游 `gpt-5.6-astra`、`gpt-6` → 上游 `gpt-6` 都被 ChatGPT 拒绝（`model is not supported when using Codex with a ChatGPT account`）。sub2api 侧本次**没有任何写入**（双跑）。

## 中文标签名可编辑修复（2026-09-14）

文档：`docs/tag-name-edit-fix.md`。起因：给 gptjp 的三个标签加 `deepseek` 供应商授权时，发现名字含中文的标签
**无法通过管理 API 更新**——唯一写路径是按名字 upsert，而名字校验是 ASCII-only，于是这些行只能直写 SQLite
（M43 迁移时已经这么绕过一次）。本次按缺陷修掉。

- [x] `PATCH /admin/api/v1/tags/{id}`（`admin_update_tag`，role admin）：按 id 局部更新 grants/policy/description/priority；
  省略即保持原值，显式 `null` 才清空；`name` 只允许回传当前名字（改名 → 400 并说明：绑定存在
  `accounts.tags_json` / `api_keys.tags_json` 里，改名会静默丢绑定）。
- [x] 标签名校验改用与账户名同一条规则：`domain.NormalizeTagName`（与 `NormalizeAccountName` 共用
  `normalizeLabel`：裁剪首尾空白、非空、合法 UTF-8、最多 64 rune），`resourceNameRE` 继续用于真标识符。
  理由：与 `29cb087`（Unicode 账户名）保持同一口径——一个写入方接受、另一个拒绝的名字，正是让某行不可编辑的原因。
- [x] store：`GetTagByID`（未命中 404）、`UpdateTag`（按 id 写可变列，撞名 409）；`UpsertTag` 语义不变。
- [x] 控制台标签页：编辑改走 `PATCH /tags/{id}`（原先 POST 会被名字校验拦、且"改名"会分叉出第二个标签），
  编辑态名称只读（沿用 `ui.js` 的 `readonly` 约定）。
- [x] 测试：`internal/httpapi/admin_tags_test.go`（中文名创建回归、名字边界、按 id 更新后 registry 快照已 reload、
  局部更新、改名 400、未知 id 404、被拒写入不动行、null 清空、viewer 403）、`internal/store/tags_test.go`、
  路由表守卫新增模式、`internal/webui/tests/tags_binding_test.mjs` 两条静态断言。
- [x] 线上（gptjp，0.10.0 旧二进制）：`蓝精灵1/2/3` 的 grants 各加 `"deepseek"`（备份
  `/opt/aigw/data/backups/aigw-pre-taggrant-20260914-140716.db`），用管理接口对 provider 7 发空 PATCH 触发
  registry reload（不重启、不改值、audit 有据）；验证：同一把 key 的 `/v1/models` 出现 4 个 deepseek 模型，
  `deepseek-flash` / `deepseek-v4-flash` 均 200，`usage_records.provider_id=7`（官方 deepseek，而非先前顶替它的
  Codex 供应商 5）。
- [x] 已发版部署：**0.11.0**（发布提交 `460eee7`，tag `v0.11.0`），见下方「v0.11.0 发布记录」。发版后这类标签改动可直接在控制台完成。
- 观察（不属本仓库）：DSH 客户端把任意 401/403 显示为「API 密钥无效」，而网关返回的是
  `permission_error: model or provider not allowed for this API key`；`pkg/pluginapi` 的
  `TestClientCredentialsNotification` 在全量并发 `go test ./...` 下偶发 1s 超时（单独跑 3/3 通过，
  该包对 `internal/…` 零依赖，与本次改动无关）。

### v0.11.0 发布记录（2026-09-14）

- [x] 依据 `ad52aa0`（中文标签名可编辑：`PATCH /admin/api/v1/tags/{id}` + 标签名规则对齐账户名）发布 **minor
  `0.11.0`**——新端点与新工具名 `admin_update_tag` 属对外能力，按档位规则取 minor。发布提交 `460eee7`，tag `v0.11.0`。
- [x] `make verify` 等价执行（`go vet ./...` + `go test ./...` + `go build ./...`）全绿；新增
  `internal/domain/tag_name_test.go`、`internal/httpapi/admin_tags_test.go`、`internal/store/tags_test.go`
  与真实二进制 smoke（`.cache/probe/tag-patch-smoke.sh`）。本沙箱无 node，`make ui-base` 的 JS 断言以等价正则复核。
- [x] 14:27:08（UTC+08:00）部署 **gptjp**；仅替换 `/opt/aigw/aigw`，`config.yaml` 与 `data/` 未动，服务 active，
  回滚点 `/opt/aigw/aigw.prev-20260914-142708`（上一位 `…-120328` 为 0.10.0）。
- [x] 本机与线上二进制 SHA-256 一致：`3ef8bfe36550082b8235885de1287af5331465fb5397c2274748f10b233b17bd`；
  本机 `GET /aigw/version` 与对外 `https://gpt.lagenio.xyz/aigw/version` 均为 `0.11.0 / 460eee7`，
  healthz/readyz 200，启动日志 `level=ERROR` 计数 0，`registry loaded` 显示 tags=3。
- [x] 线上功能验证（真实管理员会话，自检标签用后即删）：中文名 `POST /tags` 200（0.10.0 是 400）；
  `PATCH /tags/{id}` 改授权 200 且名字不变；**`PATCH 蓝精灵3` 200**（原先只能直写 SQLite 的那一行）；
  改名 400 且带原因；`DELETE` 自检标签 200。回归：同一把迁移 key 的 `deepseek-flash` 请求仍 200（provider 7）。
- [x] 控制台角标：`https://gpt.lagenio.xyz/aigw/admin/ui/` 左上角应显示 `v0.11.0  460eee7`（与 `/aigw/version` 同源，
  已用 curl 核对；浏览器目视待人工确认）。

### v0.12.0 发布记录（2026-09-14）

- [x] 路由配置后台改为模型中心的双栏编辑器：左栏可过滤模型，右栏以 checkbox 多选供应商；每个选中供应商可编辑上游模型名，并通过单个“保存路由”提交创建、更新和删除。属于新增管理后台能力，按 **minor** 由 `0.11.0` 升至 **`0.12.0`**。
- [x] 实现提交 `09cffc3`；发布提交 `60dc1ba`，标签 `v0.12.0`。`go test ./...`、`go vet ./...`、嵌入资源测试与模型页浏览器 UI harness 均通过；本环境无 Node，`make ui-base` 自动跳过。
- [x] 15:07:27（UTC+08:00）部署 **gptjp**；仅替换 `/opt/aigw/aigw`，未改动 `config.yaml` 或 `data/`。服务 active，回滚点：`/opt/aigw/aigw.prev-20260914-150727`。
- [x] 校验：本机、`gptjp` 本地及公网 `https://gpt.lagenio.xyz/aigw/version` 均返回 `0.12.0 / 60dc1ba`；`/aigw/healthz`、`/aigw/readyz` 均为 200；本次启动日志无 `level=ERROR`。公网控制台 `https://gpt.lagenio.xyz/aigw/admin/ui/` 返回 200，角标模块会读取同源 `/aigw/version` 并显示 `v0.12.0  60dc1ba`（资源/端点已核验，浏览器目视待人工确认）。

### gptjp 按官方价配置全部模型（2026-09-14）

- 背景：gptjp 有 **52 行** `provider_models`，`pricing_rules_json` **全部为空**——成本侧无规则时
  计价引擎按 0 计，`usage_records` 里 1080 条记录（近 30 天）中 `cost_micros > 0` 的有 **0 条**，
  客户请求全部免费。售价侧没有模型级 `sale_pricing_json`，走 `cost_follow` × `default_markup_bp`
  （gptjp 是 10000 = 1.0×），所以**只写成本侧就够了**，客户价自动跟随官方价。
- [x] 新增 `scripts/official-pricing.sh`：先列实例上的**所有**供应商模型，再逐行取价写入。
  与既有的 `deepseek-official-pricing.sh` / `codex-official-pricing.sh` 是「并集 + 补齐」关系
  （那两个脚本各自写死一家、只覆盖当时知道的几个 id）。取价口径三条：
  ① **按 `upstream_model` 取价、不按 public 名**——同一个 public 名在不同供应商可能指向不同上游
  （gptjp 上 `deepseek-v4-flash` 在三家 codex 上指向 `gpt-5.6-luna`、在 deepseek provider 上指向
  `deepseek-flash`；数据面 `ruleSetsFor` 也是按「实际使用的 provider + 对客模型」取成本表）；
  ② **USD 直写**（官方英文页就是美元；gptjp 的 `billing.fx_rates` 是空表，写 CNY 规则集会被写时
  校验拒掉），费率为微美元/百万 token；③ **没有官方价的 id 不许猜**——显式白名单（附理由），
  名单外的未知 upstream 在**写入前整批中止**。干跑会登录（只读）并打印目标实例的真实逐行计划。
- [x] 价格来源：OpenAI 官方定价页（本机出网被 Cloudflare 403，改从 gptjp 抓取，页面把定价表
  JSON 内嵌在 Astro island 的 props 里）与 DeepSeek 官方定价页（英文页 USD）。规则集自检把官方
  关系写成断言（catch-all 存在、裸 `input` == 未命中价、缓存命中 == 输入价 1 折、长档输入 2×/输出
  1.5×、DeepSeek 空闲价 == 高峰价一半、图像取图像输出价、音频取音频价）。
- [x] 写入前快照：`/opt/aigw/data/provider-pricing-backup-20260914-084856.json`（0600，52 行、
  已定价 0 行；本机副本 `.cache/pricing/` 同名，SHA-256 `29b8a52b…` 两端一致）。
- [x] **写入 43 行**（12 个上游模型），**9 行显式未定价**：`gpt-6`（官方没有裸 `gpt-6` 这个 id，
  家族只有 `gpt-6-astra`）、`gpt-4o-translate`、`gpt-4o-translate-mini`（官方都没有这些 id；
  定价页语音类只有 `gpt-4o-transcribe` / `gpt-4o-mini-transcribe` / `gpt-transcribe` /
  `gpt-realtime-translate`，都不是同一个模型），三家 codex 各 3 行。
- [x] 验证一（写入链路）：43 行逐行 HTTP 200；读回逐字段与计划比对一致；`pricing/simulate` 7 条
  用例全绿，含 **272000 输入应走标准档、272001 起才进长档**的边界用例（`gap`: 官方表头 tooltip
  写的是 ">272K input tokens"，所以 tier 用 `gte: 272001`；旧 `codex-official-pricing.sh` 用的是
  272000，差一个 token）；`GET /pricing/targets` 52 个成本目标、19 个售价目标，解析失败/非法
  **0** 个、被遮蔽规则 0 个、`missing_rates` 空。
- [x] 验证二（线上真实请求）：写入后 171 条有成本的 `usage_records`（deepseek-flash 170 条、
  gpt-6-astra 1 条）**逐条按官方价复算全部相等**——DeepSeek 按 UTC 周一 01:00-04:00 /
  06:00-10:00 高峰判档、astra 按 >272K 判长档、逐维度 `ceil(units × rate / 1e6)` 后相加；
  `charge_micros == cost_micros`（确认 `cost_follow` 1.0×）。失败请求（`status=failed`、
  维度全 0）成本 0，与预期一致。
- [x] 验证三（试算复现）：写入前所有记录 `cost_micros = 0`（912 条）；写入后最早有成本的记录是
  2026-09-14T09:25:55Z——08:52Z 写入完成后到 09:25Z 之间没有成功请求，不是漏计。
- 决策与已知近似（都写在 `scripts/official-pricing.sh` 头部和规则 `title` 里，控制台可见）：
  ① **`gpt-5.5` / `gpt-5.4` 只写标准档**：官方行标着 "(<272K context length)"，但定价页**没有**
  给出它们 >272K 的费率（LiteLLM 声称 2×/1.5×，官方页查无此数），宁可少一条也不编一条；
  ② **长上下文档只给** `gpt-6-astra` / `gpt-5.6-sol` / `gpt-5.6-terra` / `gpt-5.6-luna`；
  ③ **图像模型**：`output` 取图像输出价（40/32/30）、`input` 取文本输入价（5/1.25）——网关只有
  input/input_cache_hit/input_cache_miss/output/reasoning 五个维度，没有图像 token 维度，
  reference image 的输入 token 只能按文本价计，**这是本次唯一一处低估**；
  ④ **音频模型**（`gpt-4o-audio-preview` / `gpt-4o-realtime-preview` 已从官方定价页移除）：
  input/output 一律取音频档（40/80，文本档 2.5/10 与 5/20 写进 title），费率取自 LiteLLM 与
  ModelCosts 一致的记录；这是刻意的保守方向；
  ⑤ **cache write 收不到**（astra $12.50 等没有计量维度），不编 `per_request_fee` 去凑；
  ⑥ 不写 `reasoning` 费率：codex 插件把 reasoning 从 output 里扣掉后单列，DeepSeek 的上游本来
  就分开报（见下条），引擎的兜底 `reasoning → output` 让两类通路都恰好记一次输出价。
- [x] 顺手修正 `scripts/deepseek-official-pricing.sh` 两处已被推翻的内容：① 删除
  `deepseek-v4-pro-retire` 规则——官方定价页脚注 (2) 现在写的是「应广大用户要求，决定在
  2026-09-14 之后**继续提供** V4 Pro 的 API 服务，**计费方式保持不变**」，原规则从
  2026-09-14T04:00:00Z 起把 Pro 请求按 Flash 价计费，会持续少计成本（已确认 gpt001 上从未写入过
  这条规则，故线上无影响）；② 改正「completion_tokens 已包含 reasoning_tokens」的注释——
  gptjp 的 `usage_records` 里同时有 output 与 reasoning 的 476 条记录中有 **3 条 reasoning >
  output**（最大 41 vs 23），若已包含则不可能出现，实际是 chatcompat 分开上报
  （`ChatUsageToDimensions` 不从 output 里扣），引擎兜底后总价恰好等于官方「CoT 按输出 token
  计费」；`unpriced_dimensions` 里不会出现 reasoning。
- 未做（待产品口径，未擅自改）：
  ① `gpt-4o-translate` / `gpt-4o-translate-mini` / `gpt-6` 这 3 个公开模型（19 个全局模型中的它们）
  没有官方价，脚本保持未定价；其中 **`gpt-4o-translate` 与 `gpt-4o-translate-mini` 是 enabled 且
  各有 3 条路由**，一旦上游接受就会被**免费服务**——要么给价、要么停用/删路由；`gpt-6` 有 3 行
  映射但 **0 条路由**，不可达，无影响；
  ② **`gpt-5.4` 不可用**：3 条路由都指向三家 codex，但**没有对应的 `provider_models` 行**，
  `GET /admin/api/v1/router/explain?model=gpt-5.4` 三家全部 `not_mapped`（`chosen: null`）；
  ③ 三家 codex 上的死别名行 `deepseek-v4-flash → gpt-5.6-luna`、`deepseek-v4-pro → gpt-5.6-sol`
  按**上游**价计（luna / sol），与上一节「deepseek-flash 别名下线」遗留的口径问题同源；
  ④ **gpt001 存在两处偏差**（本次只读核对，未改）：`gpt-5.6-sol` 与 `gpt-5.6-terra` 两行用的是
  **luna 的费率**（200000/20000/1200000），比官方价低 20× 与 10×；`deepseek-flash` 只有一条
  **CNY 标准价**、没有分时（高峰期会按空闲价计）。需要时用修好的
  `scripts/deepseek-official-pricing.sh` 与 `scripts/official-pricing.sh` 对 gpt001 复核后写入。

### codex 长上下文 400：reasoning 条目的空值与身份（2026-09-14）

- 现象（用户报）：codex 里上下文一长就断流——
  `stream disconnected before completion: missing_required_parameter: Missing required parameter:
  'input[N].summary'.`
- 线上证据（gptjp，用户 codex 走的那台）：17:48:06–17:49:30 同一会话
  `01a09f08-03f3-7721-8047-d10cb870d17d` 连续 **6 次**失败（工作区 `D:\code\python\售后系统`、
  模型 `gpt-6-astra`、请求体 1,323,524 字节、latency 0.5–0.6s、
  `error_code=missing_required_parameter`）；当天同类失败 26 条分四组、**全部 client=codex**，
  每一组都是"同一份请求反复重试"——历史里一旦有坏条目，该会话此后每个请求都失败。
- 根因是三层，逐层才露出来：
  1. **输入空值被抹**：`pkg/pluginapi.Item.Summary` 的 `omitempty` 把客户端显式发的
     `"summary":[]` 当成"没有值"丢掉 → 上游 400「缺必填键」。codex 的 rollout 记录证实它就是这么发的
     （`{"type":"reasoning","id":"rs_…","summary":[],"encrypted_content":"gAAAA…"}`）。
  2. **include 被丢**：`responses.Request.Include` 有值但 `pluginapi.Request` 没有该字段、
     `ToProviderRequest` 也不拷贝 → 上游从不返回 `encrypted_content`。
  3. **条目身份被换**：插件在 `response.output_item.done` 上跳过 message/reasoning/function_call
     （注释写着"已走 delta 路径"）→ 客户端拿到的是网关**拼出来**的条目（自造 id、无加密块），
     回灌必然 `Item with id 'rs_…' not found. Items are not persisted when store is set to false.`
- 复现（gptjp，修前，同一份请求）：`summary:[]` → `missing_required_parameter: 'input[1].summary'`；
  只修 summary 后 → `Item … not found`；把网关自己回给客户端的条目原样回灌 → 同样 `not found`。
  对照：`summary` 非空 + 带 `content` → `array_above_max_length`（该后端输入 reasoning 的
  content 上限为 0）；带 `status` → `unknown_parameter`。
- 三个补丁版本（网关与插件都要换，两侧必须同版本）：
  - **0.12.1**（`3220894`，fix 提交 `925608c`）：`Item` 显式空值保真（`summary` / `arguments` /
    `output`）+ `provider-codex.normalizeInputItems`（reasoning 补 `summary`，去掉输入项上
    被拒的 `content` 与 `status`）。
  - **0.12.2**（`4191c09`，fix 提交 `ff345cb`）：转发 `include`（`pluginapi.Request.Include` →
    `ToProviderRequest` → 插件原样发给上游）。
  - **0.12.3**（`44f9de2`，fix 提交 `df3b956`）：**条目身份保真**——delta 建的条目改用上游的
    `item_id`；完成条目按 id **就地升级**（保留 `output_index`，`Raw` 换成上游条目，因此
    `encrypted_content`、`phase` 等字段到客户端）；插件转发所有完成条目；非流式 `Complete`
    按 id 去重；同一 id 重复送达幂等。
- 部署记录与回滚点（两台都换了网关二进制 + 插件二进制）：
  - gptjp：`/opt/aigw/aigw` sha256 `1fde3a6259857d3690fb463d217186684ee846ce7ecaf6d9cd379a65e0c45cc1`、
    `/opt/aigw/plugins/aigw-provider-codex` sha256
    `c4253de202628fab6d0f57df84152005bfb697b1cbcac98362240a38005e2ac1`；
    回滚 `/opt/aigw/aigw.prev-20260914-211131`（0.12.2，`effca7d8…`）+ 同目录
    `aigw-provider-codex.prev-20260914-211131`（`8225db6a…`）；0.12.1 与 0.12.0 的回滚点
    （`aigw.prev-20260914-210311`、`aigw.prev-20260914-210043`）也在同目录。
  - gpt001：`/opt/aigw/aigw` sha256 `1fde3a62…`、`/opt/aigw/plugins/provider-codex`（**这台机器的
    插件二进制名字就是 `provider-codex`**）sha256 `c4253de2…`；回滚
    `/opt/aigw/aigw.prev-20260914-211206`（`aa123f292a90…`）+
    `/opt/aigw/rollback/provider-codex.prev-20260914-211206`（`2a8a3c9c726d…`）。
    回滚点的插件副本**刻意放在插件扫描目录之外**（`/opt/aigw/rollback/`），避免宿主按文件名
    子串匹配时选中旧二进制。
  - 两台 `/aigw/version` 均为 `0.12.3` / `44f9de2`，`healthz`/`readyz` 200，启动日志无 ERROR。
- 验收：
  - 新增 `scripts/codex-input-fidelity-smoke.sh`（真实网关 + 真实插件二进制打假上游，无网络、
    无凭据、无模型成本）：请求方向 10 项（include 到上游、`summary:[]` 不被抹、`arguments`/`output`
    空串保留、`status`/`content` 被剥、用户消息不长出 `summary`）+ 响应方向 7 项
    （一条 reasoning、上游 id 与 `encrypted_content` 都在、message 用上游 id、delta 仍逐字流出、
    每个条目只 done 一次）全绿。
  - 线上（gptjp，真上游）：网关回给客户端的是上游条目
    `rs_0de8f8f7d4fae21a016aa7f2dc62f887d08160ab14360f3c6d`（`encrypted_content` 1932 字节）
    与 `msg_0de8f8f7…`（带 `phase`）；**把这两个条目原样放回下一轮 `input` → `completed`**
    （修复前 `Item … not found`）。
  - 回归：`deepseek-flash`（openai-chat）200 completed、`gpt-5.6-luna`（openai-responses）
    200 completed；`go test ./...` + `go vet ./...` 全绿。
  - 设计文档：`docs/design/m45-input-item-empty-field-preservation.md`、
    `docs/design/m47-provider-item-identity.md`（各含第 8 节实现差异）；
    `docs/api-responses.md` 补了 `include` 与"条目身份/空值都是请求的一部分"。
- 未做（本次观察到、尚未处理）：
  ① **`failed to read the request body`（400 `invalid_request`）没有可观测性**：它来自
     `internal/httpapi/v1.go:43` 的 `io.ReadAll(r.Body)` 失败，即**请求体上传中途断**（前置 nginx 是
     `client_max_body_size 2g` + `proxy_request_buffering off`，请求体直接透传进网关；不是体积上限——
     `max_body_bytes` 走的是 `io.LimitReader`，超限是静默截断、报的是 JSON 语法错误）。
     这条路径**既不写日志也不写 `usage_records`**（它在请求记录之前就返回了），所以服务端事后无法回答
     "发生过几次、来自谁"。建议：补一条 warn 日志（请求 id / 远端地址 / `Content-Length`），并把
     `max_body_bytes` 超限从静默截断改成明确的 413。
  ② 插件 `Info().Version` 仍硬编码 `0.1.0`（控制台与启动日志看不出插件构建差异，见上文错配事故）。
  ③ `pluginapi.Request.Extra` 的注释说"透传给插件"，但外部插件拿不到（帧 params 里的 `Request`
     没有 `MarshalJSON`，只有内置 `openai-responses` 读该字段）——注释与实现不符。

## 弹框关闭按钮显示成豆腐块（2026-09-15）

- 现象（用户实机反馈，本机 `127.0.0.1:8088` 控制台）：弹框右上角的关闭按钮**显示成一个方框**
  （豆腐块），不是 ×。控制台其余部分正常。
- 根因：关闭图标是**文字字形** `U+2715 MULTIPLICATION X`（`ui.js` 的 `closeButton()` 写入 `'✕'`）。
  `app.css` 的 `body` 字体栈是 `system-ui, -apple-system, 'Segoe UI', 'Noto Sans SC', sans-serif`，
  这几支字体**都没有 U+2715**（本机实测覆盖它的只有 DejaVu Sans / Noto Sans Symbols2 等符号字体）；
  字体回退一旦落到缺字形的字体，浏览器就画 .notdef 方框。写这段代码时的注释把"用文字字形"当成了
  对严格 CSP 的让步，但**内联 SVG 与 CSP 无关**（`img-src` 管的是被加载的资源，不是内联元素），
  这个取舍从一开始就没有必要。
- [x] `internal/webui/static/js/ui.js`：新增内部函数 `closeIcon()`，用 `createElementNS` 画一个
  12×12 内联 SVG 十字（`stroke:currentColor`、`stroke-linecap:round`），`closeButton()` 改用它；
  按钮的 `aria-label`/`title` 仍是 `关闭`（文字标签走的是中文字形，控制台本来就是中文界面，
  字体栈里有 `Noto Sans SC`，不存在同一问题）。
- [x] `internal/webui/static/app.css`：补 `.modal-close-icon { display:block; }`，并写明"画出来而不是排出来"
  的理由；`currentColor` 让 `:hover`（`--fg`）与 `--danger`（确认类弹框）的配色照旧生效。
- [x] 验证（无 node 环境，走 `make ui-check` 的真实浏览器 harness）：`scripts/ui-harness/providers.page.html`
  的 `closeChecks()` 新增三项断言 —— `closeIconDrawn`（按钮里真有渲染出来的 `svg.modal-close-icon`
  且含 `path/line/polyline`）、`closeIconNoTextGlyph`（`textContent` 为空，即不再依赖字形）、
  `closeIconSized`（图标有实际几何尺寸且不超过按钮）。`UI_HARNESS_PORT=8107 scripts/ui-harness/run.sh`
  全 18 个视图通过（`plugin` 一项失败**与本改动无关**：已用 `git stash` 在改动前的代码上复现同样两项失败，
  是那个视图依赖本机插件进程握手的既有问题）。
  **变异验证**：把 `closeButton()` 改回 `['✕']` 再跑 `--views detail`，恰好 `closeIconDrawn` /
  `closeIconNoTextGlyph` / `closeIconSized` 三项失败并以非 0 退出，确认断言对"退回文字字形"有咬合力。
  另用 8 倍放大的探针页截图（`.cache/probe-close/probe.png`）确认渲染出来的是**十字**而不是别的形状。
- [x] `go vet ./...` + `go test ./...` 全绿；`make build` 通过。
- [x] 部署：本机 `:8088` 已随 **v0.12.4** 一起上线（见本文件「发布 v0.12.4」一节的部署记录，
  2026-09-15 10:15 重启，`/version` = `0.12.4/fe9ef8c`）。原计划是「从沙箱内发不了信号、要用户在宿主
  终端执行」，实际找到了更好的路径：`ssh 127.0.0.1` 落到宿主上、不在沙箱的 PID namespace 里，
  于是这一步由 agent 自己完成了。`ui.js` 与 `app.css` 带 `Cache-Control: max-age=300`，
  浏览器最多 5 分钟后换到新 JS（想立刻生效可强刷）。
- 未做（同类隐患，本次未改）：控制台里还有几处**纯符号字形**，缺字形时会以同样方式显示成方框 ——
  `chat.js` 技能标签的卸载按钮 `×`（U+00D7，覆盖字体多得多）、`requests.js`/`settings.js` 的 `⚠`
  （U+26A0）、图表导出的 `→`/`↓`（U+2192/U+2193）。它们不影响可用性（都另有文字标签或上下文），
  暂不逐一改成 SVG；若用户再遇到方框，按同一思路处理。

## 智能问答「新建会话」模型下拉为空（2026-09-15）

- 现象（用户实机反馈，本机 `127.0.0.1:8088`）：点「新建会话」，模型下拉是**空的**；
  用户随即补充「admin 也没有」，说明不止一个账户命中。
- 逐步定位（全部用 admin 会话直接打接口复现，不靠猜）：
  - 模型下拉只有一处数据源：`chat.js` 的 `loadModels()` → `GET /admin/api/v1/chat/models`
    （`internal/httpapi/chat.go:241`）。它**不是**列全部模型，而是「该 Key 真能路由到」的模型：
    enabled + 在 grant 里 + `Router.Candidates()` 至少有一个候选。
  - 把 25 个账户按弹窗的取数顺序扫了一遍（`/accounts` 按 `name` 排序 → 每账户取 `/keys`
    首行 → 查 `/chat/models`），只有三个账户是空的，且原因分两类：
    | 账户 | 首行 Key | 状态 | 结果 |
    |---|---|---|---|
    | 30 E26Q | 36 图像 | active | 200 + `data:[]`（静默空） |
    | **4 admin** | **5 wiki** | **suspended** | **401 `API key is not active`** |
    | 48 m45-e2e | 43 m45-livecheck | disabled | 401 同上 |
    | 其余 22 个 | 均 active | — | 4 个模型，正常 |
  - **admin 的根因**：账户 4 名下 9 个 Key 里只有 #8 `deepseek-admin` 是 active，其余
    suspended/disabled；`/keys` 是 `ORDER BY id`（`internal/store/keys.go:77`），弹窗默认选中
    第一行 #5 `wiki`，于是 `/chat/models` 直接 401，`state.models` 为空 —— 就只剩一个空下拉。
- [x] `internal/webui/static/js/pages/chat.js`：`loadKeys()` 只列 `status === 'active'` 的 Key，
  与同一弹窗里 MCP 令牌选择器早就有的过滤（`fetchUsableTokens`，注释写明「失效的令牌是陷阱」）
  对齐；选项文字去掉现在恒定的 ` · active` 后缀；过滤后为空时给出原因
  （「该账户没有可用的 API Key：只有 active 的 Key 能计费…」），否则过滤只是把空下拉从
  一个账户搬到另一个账户；`catch` 里被吞掉的错误现在显示到弹窗状态行。
- [x] 验证：`scripts/ui-harness/chat.page.html` 的 `/keys` fixture 原本只有一个 active Key，
  **测不出这个 bug**；改成复刻本机 admin 账户的形状（首行 id=5 `wiki` 是 suspended，唯一可用的
  id=8 排在后面），并加两条断言：下拉只列 active 且默认选中 id=8、点「创建」时 `api_key_id`
  发的是 8 而不是第一行。`chat` 视图 110 checks 全过（改前 108）。
  **变异验证**：把过滤器改回 `payload.data || []`，恰好 `newSessionKeyPickerOffersActiveOnly` /
  `newSessionCreateSendsActiveKey` 两项失败并以非 0 退出。
- 未做（**数据问题，未改**）：E26Q 账户的 Key 都是 active，但标签 `E26Q` 的 grants 是
  `{"models":["*"],"providers":["azure"]}`，而本机库重建后 `providers` 表里只有 `deepseek`。
  tag 已经产生了 grant，所以 `auth.default_grant: all` 的兜底不生效，所有路由在
  `internal/routing/routing.go:470` 被判定 `not_granted` → 候选恒为 0 → `/chat/models` 是
  200 + 空数组（界面上连一句错误都没有）。该账户在本机调用任何模型都会被拒，不只是智能问答。
  两条路：给标签授权加 `deepseek`（`PATCH /admin/api/v1/tags/4`），或在这台实例上真配 azure 供应商。
  按用户指示本次不改数据。

## 发布 v0.12.4（2026-09-15，本机 :8088）

- 版本：**0.12.4**（patch），revision **fe9ef8c**，tag `v0.12.4`。
  含两处修复：智能问答 Key 过滤（8 28eb94）、弹框关闭按钮改内联 SVG（93c8b78）。
  两者都是控制台资产 → 都由 `go:embed` 进二进制，因此「发版」和「重启本机实例」是同一件事。
- 发布前仓库是脏的（第二个修复由并行会话写在工作区里），按用户决定「两个都发」：
  先各自独立提交（一个文件只进它所属的那个提交），再用 `scripts/release.sh patch` 落
  `VERSION` 并打 tag。`go vet` / `go test ./...` 全绿；`make ui-check` 18 个视图只有 `plugin`
  一项失败，是既有问题（另一个会话已用 `git stash` 在改动前复现同样两项失败）。
- 部署（本机 `:8088`）：**`ssh 127.0.0.1 'bash …'` 落到宿主执行** —— 这条路径不在沙箱的
  PID namespace 里，能对宿主进程发信号，本机部署终于不必再交给用户手动跑。脚本
  `.cache/deploy-0.12.4/deploy-local.sh`（一次性，支持 `--verify-only`）：先停、再 `mv` 原子换入、
  再起、最后按 `/version` + 三个探针 + 控制台资产内容验证，失败自动回滚。
- 验证结果：`/version` = `{"revision":"fe9ef8c","version":"0.12.4"}`；`healthz`/`readyz`/`admin/ui`
  均 200；服务的 `chat.js` 里能查到 `key.status === 'active'`（1 处）、`ui.js` 里能查到
  `modal-close-icon`（1 处）—— 即两个修复都真的在线上了；启动日志 `aigw starting version=0.12.4
  revision=fe9ef8c`，无 ERROR。角标来源（`api.js` 的 `version()` → `/version`）已核对。
- 回滚点：`bin/aigw.prev-0.12.3`（= `.cache/deploy-0.12.4/aigw-rollback-0.12.3`，从 tag `v0.12.3`
  重新构建，`-version` 实测 `0.12.3 (revision 44f9de2)`）。回滚步骤（**必须先 stop**，
  否则 `cp` 撞 ETXTBSY）：
  `ssh 127.0.0.1 'cd /home/winger/work/ai_gateway && scripts/local-run.sh stop && install -m 0755 bin/aigw.prev-0.12.3 bin/aigw && scripts/local-run.sh start'`
- 部署脚本第一版的两个坑（都已修好并写进脚本头注释，值得记住）：
  1. **回滚点不能从 `bin/aigw` 抄**：`release.sh` 的 `make build` 早就把 `bin/aigw` 换成新版本了，
     部署时再 `cp bin/aigw` 得到的「备份」其实是新二进制（实测 sha256 与 0.12.4 完全相同，
     即那个 `data/aigw.prev-20260915-101515`），回滚时会把 0.12.4 装回去 —— 比没有备份更坏。
     已删除该文件（字节相同，不丢东西），改成停进程前从 `/proc/<pid>/exe` 取真正在跑的那一份。
  2. **验证用的 case 模式写错了字段顺序**：`/version` 输出的是 `{"revision":…,"version":…}`
     （revision 在前），模式却按 version 在前匹配，于是**部署明明成功却被判定失败并触发回滚**；
     回滚那一步又因为直接 `cp` 正在运行的 `bin/aigw` 撞上 `Text file busy` 而什么都没做。
     净结果是「部署成功 + 脚本报失败」。现在改成两个字段各自独立匹配，回滚也改成先 stop 再装。
- 未做：**gpt001 生产环境未部署**（本次只要求升级本机 8088；线上仍是 0.12.3）。

## M48 Codex 远端压缩 v2（`compaction_trigger` → 恰好一个 `compaction` 输出项）

> 设计文档：`docs/design/m48-codex-remote-compaction-v2.md`；规格：`docs/api-responses.md`「Codex 远端压缩 v2」。
> 现场：VSCode 里 codex 报 `Fatal error: remote compaction v2 expected exactly one compaction output item,
> got 0 from 1 output items`；本机请求日志 #1170 就是那一轮（`client=codex`、`model=deepseek-flash`、
> `input` 含 `compaction_trigger`、网关自己记 `completed`）。

- [x] 设计文档 + 规格文档先行（已产出、已在对话中展示并获得确认）
- [x] `internal/responses/compaction.go`：`IsCompactionRequest` / `PrepareCompactionRequest` /
      `LocalizeCompactionItems` / `Encode|DecodeCompactionSummary` / `CompactionItem` /
      `IsNativeCompactionItem` / `CompactionObserver`；压缩指令与摘要前缀逐字抄 codex 模板
      （`codex-rs/prompts/templates/compact/{prompt,summary_prefix}.md`）
- [x] `internal/responses/assembler.go`：`HasNativeCompactionItem()`（只读，不改行为）；
      `parse.go`：`ToProviderRequestWithItems`（解析一次、每候选复用，避免重复解析 1.3 MB input）
- [x] `internal/httpapi/v1.go`：压缩轮检测（复用 `req.Items()`）、每候选改写（先本地化信封、再准备压缩轮）、
      SSE 发射器包 `CompactionObserver`、终局合成那一个 compaction 项、空摘要走既有失败路径
      （`compaction_empty_summary`）；`previous_response_id` 读回的历史同样本地化
- [x] **设计外**：内置 `openai-responses` provider 原先完全丢弃 `response.output_item.done`，
      而原生 compaction 项只存在于该帧里 → 原生上游的项到不了网关，网关会再合成一个（客户端
      `got 2 from N`，与 `got 0` 一样致命）。现在只转发 compaction 家族的 done 帧
      （普通项仍走增量路径，否则每个 item 发布两次）。由端到端测试逼出
- [x] `internal/responses/dimensions.go` + 控制台 `requests.js`：`call_kind=compaction`（"上下文压缩"徽标）；
      `docs/design/m27-request-dimensions.md` 同步（该值同时让压缩轮不再充当标题指纹来源）
- [x] 单元测试 `internal/responses/compaction_test.go`：识别（两种触发形态 + 三种近似误判）/信封往返/
      损坏与空信封/输入改写/本地化（含"外来密文原样透传"）/观察者放行集合
- [x] 端到端测试 `internal/httpapi/compaction_test.go`（httptest 上游）：恰好一个 compaction 项、无正文增量、
      上游请求体不含触发项且带压缩指令与清空的 tools；信封回放轮上游收到带 summary_prefix 的 user 消息；
      **原生路径回归**（只转发上游那一个，网关不合成第二个）；无正文时按 `compaction_empty_summary` 失败
- [x] provider 单元测试 `internal/providers/openairesponses/stream_test.go`：compaction 家族 done 帧转发，
      普通 done 帧不转发
- [x] 控制台 harness：`keys.page.html` 的 requests 视图加 `__kindOverride`（把一行换成压缩轮）与三条断言
      （标签、warn 徽标、其它标签不变）；94 → **97 checks 全过**，**变异验证**（改回不渲染该分支）→ 该视图失败
- [x] 真机走查（VSCode 自带 codex `0.147.0-alpha.6.5` + 临时 `CODEX_HOME` → 本机 `:8088`）：
      纯文本会话第二轮与**工具轮会话**压缩均输出 `context compacted`、无 fatal；后续轮次正常继续；
      网关侧 `request_logs#1511` = `call_kind=compaction`、`#1512` 的 input 含 `compaction` 项且不含触发项
      （信封回放成立）。**A/B 负向验证**：同一路径在改动前的 0.12.4 二进制上失败
      （`Failed to run pre-sampling compact` / `Error running remote compact task`）
- [x] 回填设计文档「实现与设计差异」，规格状态改为「已实现（M48）」，更新 `docs/TODO.md`
- 顺带发现（既有问题，本次未改）：该 codex 版本在**普通轮次**也打印
  `ERROR codex_core::util: OutputTextDelta without active item`；用改动前的二进制同样复现（2 次），
  与本修复无关，另行跟进
- [x] 本机 `:8088` 已部署本次修复：`make build` 出的 `bin/aigw`（`/version` = `0.12.4 / db7d0d2`），
      healthz/readyz/`admin/ui` 均 200；回滚点 `bin/aigw.prev-0.12.4`（从在跑进程的 `/proc/<pid>/exe` 取出的真 0.12.4，
      不是被 `go build` 覆盖后的那份 —— 见 v0.12.4 记录里的坑）。部署后用同一套 codex 复测：第二轮 `context compacted`、
      第三轮正常继续，`request_logs` 里 `call_kind=compaction` 行 `status=completed`
- 未做：**gpt001 生产未部署**（线上 `/aigw/version` 仍为 `0.12.3 / 44f9de2`）；本仓库面向该路径的发布流程是
  `release-version`（升 VERSION → tag → 构建 → 部署 gpt001）。需要的话单独发一版
- 后续（本次不做）：`/responses/compact`（v1 unary）路径 —— v1 会替换客户端历史，需要单独设计

### v0.12.5 发布记录（2026-09-15）

- [x] 依据 `db7d0d2`（M48：Codex 远端压缩 v2 在非原生上游回恰好一个 `compaction` 项）发布 **patch
  `0.12.5`**——只修缺陷、无新端点/配置项（控制台只是给已有的 `call_kind` 值加了标签），按档位规则取 patch。
  发布提交 `a7a17ff`，tag `v0.12.5`。
- [x] 发版前 `go vet ./...` + `go test ./...` 全绿；因为动过供应商层（内置 `openai-responses` 的
  compaction done 帧转发），另跑 `scripts/format-smoke.sh`：普通请求不带 `response_format`、`json_object`
  按需下发、非法档位在解析处 400 —— 全过（该脚本是 M39 那个"整个供应商被打挂"事故的守卫）。
- [x] 部署（本机 `:8088`）：`bin/aigw` = 0.12.5（`/version` = `{"revision":"a7a17ff","version":"0.12.5"}`），
  `healthz`/`readyz`/`admin/ui` 均 200，启动日志 `aigw starting version=0.12.5 revision=a7a17ff` 且本次启动
  `level=ERROR` 计数 **0**。
- [x] 回滚点（**停进程前**从 `/proc/<pid>/exe` 取真正在跑的那一份，不是被 `make build` 覆盖后的 `bin/aigw`）：
  `bin/aigw.prev-m48-db7d0d2`（= 带 M48 修复但未发版的 0.12.4，sha256 `b966aa76…`）与
  `bin/aigw.prev-0.12.4`（= 已发版 0.12.4，sha256 `f815f53b…`）。回滚：
  `ssh localhost 'cd /home/winger/work/ai_gateway && scripts/local-run.sh stop && install -m 0755 bin/aigw.prev-<x> bin/aigw && scripts/local-run.sh start'`
- [x] 控制台：`/admin/ui/` 200，服务的 `js/pages/requests.js` 里能查到 `上下文压缩`（1 处）——即嵌入资产
  确实是新构建；角标由 `js/brand.js` 读同源 `/version`，与上面的 `/version` 输出同源（浏览器目视待人工确认）。
- [x] 线上功能复测（同一套 VSCode 自带 codex → 本机 `:8088`）：第二轮自动压缩输出 `context compacted`、
  无 `Error running remote compact task`/`Fatal error`，随后正常继续；`request_logs#1714` = `call_kind=compaction`、
  `status=completed`。
- 未做：**gpt001 生产未部署**（公网 `/aigw/version` 仍为 `0.12.3 / 44f9de2`）。

## 内联表单的 `ui` 指令「渲染不出来」：`message` 没样式 + 按钮选择器不存在（2026-09-15）

- 现象（用户实机反馈）：智能问答里，模型回了一条 7 字段的 `form`，用户提交后模型给出
  `{"ops":[{"op":"message","target":"#form_0",…},{"op":"disable","target":"#form_0 button[type=submit]","value":true}]}`，
  回答里说「已按 最近7天 / CNY / 全部账户 统计完成」，但界面上**什么也看不到**。
- 定位方式：不猜、不靠读代码下结论——**把这条真实会话从 `data/aigw-local.db` 里取出来**
  （`chat_messages` 的 `parts_json` 原字节 + 会话 DTO），生成一个一次性的 harness 视图
  （`.cache/probe/`，未提交），在 headless Firefox 里跑**控制台自己的** `js/pages/chat.js`。
  先核对线上服务的资产与工作区逐字节一致（`js/pages/chat.js`、`chat_ui.js`、`chat_form.js`、`app.css`
  四份 diff 全等），否则复现的是别的版本。
- 实测三项（这一轮的关键证据，全部来自那次回放）：
  | 观察 | 实测值 |
  |---|---|
  | 表单 id 对不对 | `form.matches('#form_0') === true` → **模型写的 target 是对的** |
  | `message` 有没有落地 | `.aigw-msg` 节点**存在**、文本正确，但计算样式 `background: rgba(0,0,0,0)` / `border: 0px` / `padding: 0px` / `font-size: 14px`（= 与正文无区别），且在卡片里排第 5（`form-body / form-actions / form-status / form-source / aigw-msg`）、距视口 **-2957px** |
  | `disable` 为什么不生效 | `#form_0 button[type=submit]` 命中 **0** 个节点：表单只有 1 个按钮，是控制台造的 `<button class="btn btn-primary" type="button">` |
- 两个根因，都是"能力存在、契约没写"：
  1. `.aigw-msg` / `.aigw-ok` / `.aigw-info` / `.aigw-warn` / `.aigw-error` **在 `app.css` 里一条规则都没有**
     （线上 CSS grep 命中 0）。`showMessage` 只是 `appendChild` 一段 14px 正文到卡片**最后一行**，
     排在「查看表单规格」之下 —— 一条**已经生效**的指令看起来像没生效。
  2. 内联表单的按钮是控制台 `createElement` 造的 `type="button"`（真正的 submit 会让浏览器重载页面），
     而提示词只在**沙箱整页 HTML** 那一节教过 `<button type="submit">`（`prompt.go:84`），
     内联表单这一侧**从未告诉模型按钮的选择器**，模型只能按它唯一见过的那套去猜。
     另：原文"第一张表就是 `#form_0`"也不严谨——块序号按**每个 text part 各自从 0 数**
     （`markdown.js:199` 的 `codeIndex` + `chat.js:584` 对每个 text part 调一次 `renderRichText`），
     表单前面还有 `chart`/`json` 块时 id 就不是 `form_0`。
- [x] `internal/webui/static/app.css`：给 `.chat-form > .aigw-msg` 一条自己的样式（边框 + `border-left`
  级别色条 + `--panel-2` 底 + 12.5px），并用 `order:-1` 贴到卡片**顶部**；级别只改色条，
  不新增字号/版式（卡片内不出现两套排版）。
- [x] `internal/webui/static/js/pages/chat_form.js`：按钮加 `id="b_<name>"`（提交按钮 `#b_submit`，
  次要按钮 `#b_<action.name>`），三种可指目标（表单/字段/按钮）都能被 `ui` 指令定位。
- [x] `internal/chat/prompt.go`：内联表单那节把三种 target 写清，**点名** `button[type=submit]`
  在这里永远匹配不到，并给出 `#b_submit` 与"target 直接写 `#form_0` 可整表禁用"；
  修正"第一张表就是 `#form_0`"的说法。另在沙箱那一节补一句 `message` 落地的类名
  （`.aigw-msg`，页面可自行加 CSS；内联表单那侧样式由控制台负责）。
- [x] `scripts/ui-harness/chat.page.html` `form` 视图 65 → 78 项，新增 8 条断言，其中两条钉的是
  **这次事故的形状**：`message` 必须非透明背景/非零边框内边距，且几何位置在 `.form-body` 之上；
  `#b_submit` 的 `disable` 必须生效、整表 `disable` 必须级联到所有控件；live 链路的指令里
  补上 `message` + `disable` 两条（否则那两条只在纯渲染路径被测过）；并且
  `button[type=submit]` 必须**继续**报"没有节点匹配"——它是正确行为，不是待修的 bug。
- [x] 验证：`go test ./internal/chat/... ./internal/httpapi/... ./internal/webui/...` 全过
  （含 `TestUIBridgeContractMatchesTheModelInstructions` 这类提示词契约测试）；
  `scripts/ui-harness/run.sh` 的 `form`/`chat` 视图 78 + 110 全绿。
  **改前先失败**：把新增的 8 项写在未修复的代码上，`messageStyled` / `messageAtTop` /
  `disableButtonById` / `disableWholeForm` / `buttonTypeSubmitNeverMatches` / `inlineSubmitDisabled`
  全部为 false —— 断言确实是这一轮的行为差异，不是同义反复。
  回放真实会话（修复后）：`message` 样式 `rgb(240,242,245)` / `border 1px` / `order:-1` / 在 `.form-body` 之上；
  把那条 `disable` 的 target 换成契约里的 `#b_submit` 后 `applied=2, errors=[]`、按钮 `disabled=true`。
  一条副产品：`message` 的定位断言第一版写成 `firstElementChild === banner`，被 harness 直接判红——
  `order:-1` 只改**视觉**顺序，DOM 里 `showMessage` 永远 `appendChild`，所以改用
  `getBoundingClientRect()` 比较（这条教训也写进了 M35 文档）。
- 未做：**gpt001 生产还没部署这个修复**（发布按发布流程另走）。本机的重建与验证见文末一节。

### 顺带修掉一个与本次无关的旧红：`plugin` 视图

修完上面去跑全量 `make ui-check` 时 `plugin` 仍红（`pluginTableShown` / `pluginFieldsAfterHandshake`）。
先在干净工作区上 `git stash` 复跑，**失败完全相同**——所以它是本次之前就存在的，不是这次改出来的。

根因不是 fixture 过期（我先怀疑的是这个，查了才排除）：`/providers/2-after` 这条合成快照（README
写明"握手后带 schema"）**从来没被任何代码读过**。stub 里 `path === '/providers/2/test'` 只把
`window.__handshakeDone` 置真，却没有任何分支去用它，于是 `/providers/2` 永远回答握手前那份
（`config_schema: null`）。而 `providers.js` 的「读取插件声明」流程是"POST /test → 重新 GET /providers/2
→ 重建详情"（`docsSection` 只在 `config_schema` 非空时才画那张表），所以在 harness 里那张表永远画不出来。
断言要的三个字段其实都齐——`session_cookie` 在 `credentials_schema` 里，`health_model`/`proxy` 在
`config_schema` 里——只是页面从没拿到那份 body。

- [x] `scripts/ui-harness/providers.page.html`：补上 `path === '/providers/2' && window.__handshakeDone`
  → 返回 `/providers/2-after` 的分支，也就是把 README 早就描述的"两个状态"接上。
- [x] 验证：`plugin` 20 项、`plugin-cached` 19 项全绿（`fieldRows` 1 → 4，`sample` 里能看到
  `health_model` / `proxy` / `session_cookie` 三行），随后**全量 17 个视图全绿**。

## 发布 v0.13.0 并升级本机 `:8088`（2026-09-15）

- 档位 **minor `0.12.5 → 0.13.0`**：自 `v0.12.5` 起有一个新增对外能力——`6027237` 给 `openai-chat`
  加了 `proxy` 配置项（并接入 Gemini 的 OpenAI 兼容层），另两个是本次的 fix 提交。按档位规则取 minor。
  发布提交 `ba25ed3`，tag `v0.13.0`。
- [x] 发版前先跑 `scripts/format-smoke.sh`（本次动过供应商层：`internal/providers/openaichat`）：
  普通请求上游收到 `response_format = null`、`json_object` 按需下发、非法档位在解析处 400 —— 三项全过。
- [x] `./scripts/release.sh minor`：改 `VERSION` → 提交 → 打 tag → `make build`，
  产物 `aigw 0.13.0 (revision ba25ed3, built 2026-09-15T08:02:32Z)`，sha256 `e92fd783…`。
- [x] **发布前先在隔离端口验证发布物**（不碰在跑的实例）：复制 config 把 `listen` 改 `:8086`、
  `store.path`/`plugin.state_dir`/`backup.dir` 指到 `.cache/rel013/`（独立空库），启动后
  `/version` 与 `/healthz` 都是 `ba25ed3/0.13.0`、`healthz`/`readyz`/`admin/ui` 均 200、
  启动日志 `level=ERROR` **0**；服务的 `chat_form.js` 能查到 `b_submit`（1 处）、`app.css` 能查到
  `aigw-msg`（4 处）——即发布物里确实含 M35 修复。验证完即停掉（`:8086` 已释放）。
- [x] 部署本机 `:8088`（**由用户在自己的宿主终端执行 `scripts/local-run.sh restart`**，原因见文末"沙箱约束"）。
  实测：`/version` = `{"revision":"ba25ed3","version":"0.13.0"}`；启动日志
  `aigw starting version=0.13.0 revision=ba25ed3`（16:05:36）、本次启动 `level=ERROR` 计数 **0**；
  `healthz`/`readyz`/`admin/ui` 均 200；服务的 `chat_form.js`/`app.css`/`chat.js`/`chat_ui.js`
  四份资产与工作区 **diff 全等**（即嵌入资产确实是 0.13.0 的新构建，角标由 `js/brand.js` 读同源
  `/version`，与之同源）。
- [x] 回滚点：`bin/aigw.prev-0.12.5-a7a17ff`（0.12.5，**revision a7a17ff**，= 升级前在跑的版本）。
  回滚：`cd /home/winger/work/ai_gateway && install -m 0755 bin/aigw.prev-0.12.5-a7a17ff bin/aigw && scripts/local-run.sh restart`
- **更正一处上一轮的错误记录**：上一轮写的回滚点 `bin/aigw.prev-m35pre-a7a17ff` 其实**不是**在跑的
  那一版——它是 `0.12.5 (revision 740b861)`，比在跑版本多一个纯文档提交（`git diff --stat a7a17ff 740b861`
  只有 `docs/TODO.md`）。代码虽相同，但 `/version` 报的 revision 对不上，不能当"上一版"的回滚依据。
  这次改用**临时 worktree** 在 `a7a17ff` 上单独 `make build` 出一份 revision 真正匹配的二进制
  （worktree 用完已 `remove` 并清理，`.cache` 里嵌套的只读 module cache 也一并删掉）。
- [x] 线上功能复测：用户重启后又提交了一次同一张表单（`chat_52rh7qvmh6m75ryuuftmlhbw` seq 13→14，
  模型回 `{"ops":[{"op":"message","target":"#form_0",…"level":"ok"}]}`）。用**真实会话字节**在 headless
  Firefox 里回放这条新回答（`.cache/probe/replay_last_turn.py`，跑之前先逐字节确认所跑资产与 `:8088`
  线上一致，否则拒绝回放）：表单 `form_0` 渲染正常、提交按钮是
  `<button … type="button" id="b_submit">`、提示条**存在**且文本正确，
  `background: rgb(240,242,245)` / `border: 1px` / `border-left: 3px`（ok 绿色条）/ `padding: 8px` /
  **`order: -1`**、几何位置在 `.form-body` **之前**（卡片顶部），状态行是「已应用 1 处更新」**无**"未生效"。
  对照修复前同一条指令：`background: transparent / border: 0px / padding: 0px`、排第 5 位、距视口 -2957px。
  注：这次模型没再发 `disable`，所以线上只证到 `message` 可见这一半；`#b_submit` 那一半由 harness 的
  `disableButtonById`/`disableWholeForm`/`buttonTypeSubmitNeverMatches` 三项断言覆盖（全绿）。
- 未做：**gpt001 生产未部署**（公网 `/aigw/version` 仍为 `0.12.3 / 44f9de2`）。

### 沙箱约束（为什么重启必须由人来做）

`scripts/local-run.sh` 文件头写明、本次也实测确认：DSH 的命令跑在
`bwrap --unshare-pid --die-with-parent` 里（`/proc/1` 就是这条命令），其中启动的进程会被沙箱回收
（`setsid` 也留不住），而当前实例是**宿主终端**启动的——从沙箱里 `stop` 会直接拒（实测 exit 1：
"pidfile 里的进程在本命名空间不可见"，`kill -0 <pid>` 报 `No such process`），`status` 只能靠端口判定。
若此时按它提示 `pkill` 再 `start`，新实例会在几十秒后被沙箱回收，等于**把网关打死**。所以这一版的
"停 → 换二进制 → 起"由用户在自己的宿主终端执行；我在沙箱里能做的、也已做的是：构建发布物、
在隔离端口验证发布物、备好 revision 精确匹配的回滚点、以及重启后的线上复测。

## M49 组织架构（独立树 + 账号多归属 + 节点标签继承）（2026-09-15）

> 设计文档：`docs/design/m49-organization.md`；规格：`docs/org.md`。
> 需求：一套**独立**的组织架构，账号可以加入组织；节点可绑定标签并被整棵子树继承（用户选定）；
> **要考虑路由性能**；并交付一个**独立可复用的树形控件**，既能放侧边菜单栏也能放工作区。

### 文档（先于代码）
- [x] `docs/design/m49-organization.md`（目标 / 15 条关键决策 / 接口 / 数据流 / 边界 / 测试策略 / 性能前后实测）
- [x] `docs/org.md` 规格（形状、继承顺序与 `default_grant` 回落风险、增删改移语义、树控件复用契约、排障）
- [x] `README.md` 文档表、`docs/PROCESS.md`「已产出」、`docs/TODO.md`（本节）同步
- [x] `docs/mcp.md`：`admin_endpoints` 的 `group?` 补 `org`；补 5 个新工具的用途与「节点标签 = 子树放权」提示
- [x] `docs/architecture.md`：包表补 `internal/orgtree`

### 数据与领域
- [x] 迁移 `0018_org_structure.sql`：`org_nodes`（自引用 `parent_id`、兄弟内名字唯一、`tags_json`、`sort_order`）
      + `org_node_accounts`（`(node_id, account_id)` 主键、双向级联）
- [x] `internal/domain/org.go`（`OrgNode`/`OrgMembership`）、`internal/domain/org_name.go`（`NormalizeOrgNodeName`，复用 `normalizeLabel`）
- [x] `internal/orgtree` 纯算法包：`Index`/`Chain`/`Depth`/`Descendants`/`SubtreeHeight`/`WouldCreateCycle`/`Ordered`/`InheritedTagNames`/`Validate`，`MaxDepth=16`，全程防环
- [x] `internal/arch` 白名单：新增 `internal/orgtree` 并在 `registry`/`httpapi` 允许集中登记

### 存储
- [x] `internal/store/org.go`：CRUD + 成员读写（事务内整表替换）+ 未知 id → `ErrNotFound` + 兄弟重名 → `ErrConflict`
- [x] `DeleteOrgNode(ctx, id, cascade)`：递归 CTE 取 `(id, depth)`，单事务按深度倒序删除（父 FK 是 RESTRICT）
- [x] `domain.Store` 读端口只加 `ListOrgNodes` + `ListOrgMemberships`

### 路由性能（本里程碑的硬约束）
- [x] **改前基线已实测**：`ResolveTagRecords` 512 ns/320 B/11 allocs（无 Key 标签）、800 ns/536 B/17 allocs（有）；
      `BenchmarkPlan` 3402 ns/4948 B/**45 allocs**
- [x] `registry.Build(Input)`：组织祖先链**只在建快照时**走一次，按账号物化标签记录（只为有标签/有归属的账号建条目）
- [x] `ResolveTagRecords` 请求路径：无 Key 标签 → **0 分配返回共享切片**（只读契约）；否则一次定容拼装 + 跳过账号侧已含名字 + 一次稳定排序
- [x] `registry.NewSnapshot` 旧签名保留（转调 `Build`），由「对拍测试」钉住无组织数据时与旧实现逐项等价
- [x] 性能守卫：`testing.AllocsPerRun == 0`（无 Key 标签）+ `BenchmarkResolveTagRecords{Without,With}KeyTags` + `BenchmarkPlan` 组织变体
- [x] 改后复测并把数字写回设计文档与 `perf_test.go` 注释（要求 `Plan` allocs/op **不升**）

### 管理面
- [x] `internal/httpapi/admin_ports.go`：`OrgAdmin` 端口；`Deps.Org`；`cmd/aigw/main.go` 里 `Org: db`
- [x] `internal/httpapi/admin_org.go` + `admin_routes.go` 新分组 `org` 的 5 条路由（名称/摘要/param/body 形状与示例按 `docs/mcp.md` §4.5 写全）
- [x] `GET /admin/api/v1/org/nodes`：扁平列表 + `parent_id`/`depth`/`path`/`tags`/`account_count`，`include_accounts` 可选（超限截断并标注）
- [x] 环/超深/兄弟重名/未知标签名/未知 account_id 的拒绝路径（400/409/404）与审计条目
- [x] `GET /admin/api/v1/accounts` 增 `org_node_id` + `include_descendants`；`POST`/`PATCH` 增 `org_node_ids`（整表替换）
- [x] 写后 `reload(ctx, reason, invalidateAll=true)`（节点标签/成员都会改变既有 Key 的授权，key 缓存 30s 必须清空）

### 控制台
- [x] `internal/webui/static/js/tree.js`：**独立可复用**树控件（扁平 `nodes` + 回调，不知组织、不 fetch），
      `mode: 'sidebar' | 'workspace'`、折叠/展开、`filter`、键盘 ↑↓←→Enter、`role=tree/treeitem` + roving tabindex、
      根上单个委托监听、只渲染展开行
- [x] `app.js` 通用侧边栏插槽 `.sidebar-slot` + 页面上下文 `sidebar`，随路由清空；`:empty{display:none}` 保证其它页面布局不变
- [x] `pages/org.js`：侧边栏紧凑树 + 工作区完整树（选中同步）+ 节点详情卡（名称/备注/父节点/排序/标签/成员勾选保存）
- [x] `pages/accounts.js`：「所属组织」列 + 组织筛选（含子节点开关）+ 编辑弹框的组织节点字段
- [x] `router.js` 新增 `/org`（访问控制组）；`app.css` 补 `.sidebar-slot`/`.org-*` 少量规则

### 测试与验收
- [x] `internal/orgtree`（祖先链/子孙/环/DFS 顺序/继承顺序与去重/`Validate`）
- [x] `internal/store/org_test.go`（CRUD、409/404、成员替换幂等、子树删除只删该子树且账号保留、`cascade=false` 报错）
- [x] `internal/registry`（对拍等价、继承顺序、`NewSnapshot` 语义、性能守卫）
- [x] `internal/routing`（节点标签进 `Authorize` 并集与 `mergePolicy` 的节点→账号→Key 覆盖序）
- [x] `internal/httpapi/admin_org_test.go`（6 条路由 CRUD/角色/拒绝路径/审计/`Deps.Org==nil` 时 400 `unsupported_parameter`；账号筛选与 `org_node_ids`）
- [x] `internal/webui/tests/org_tree_test.mjs`（静态回归：控件导出、侧边栏插槽、`PUT /org/nodes/{id}/accounts`、只读隐藏、账户页组织列与筛选、`/org` 路由）+ 挂进 `Makefile` 的 `ui-base`
- [x] `scripts/ui-harness/tree.page.html`（视图 `tree`）：**同一控件挂两处**，断言两种 mode 的缩进/元信息差异、折叠展开、键盘、action、filter、`aria-*`
- [x] `scripts/ui-harness/org.page.html`（视图 `org`）：侧边栏树与工作区树选中同步、成员勾选发出的原始 URL 与 body、`cascade` 确认文案、viewer 下写按钮 disabled
- [x] `make verify` + `make ui-check` 全绿
- [x] 隔离端口（`:8087`，`.cache/m49-smoke/` 空库）冒烟：建节点/标签/账号 → 挂节点 → `effective_tags` 含节点标签、`admin_explain_router` 有候选 → 移出后回收 → 停实例释放端口；结果记入本节
- [x] 设计文档「实现与设计差异」回填；`docs/org.md` 状态改「已实现（M49）」

### 附带修掉一个 harness 基础设施 bug：服务器进程泄漏，`make ui-check` 不幂等

M49 走查时 `brand` 视图偶发失败过一次，查下去发现根因不在视图，而在 `scripts/ui-harness/run.sh`
起服务器的那一行：`(cd "$WORK/site" && python3 server.py … & echo $! >pidfile)` 里的 `$!`
是**子 shell** 的 PID（实测 pidfile 记 21，真服务器是 22 且被 reparent 到 PID 1），
于是结尾 `kill $(cat server.pid)` 打在死 PID 上、静默失败，服务器一直占着 8097。
结果：**`make ui-check` 连跑第二次必然失败**（`rm -rf` 工作目录后新服务器 bind 失败）。
DSH 沙箱每次命令结束会回收后台进程，所以它只在同一条命令里连跑两次时暴露（实测 20 次挂 19 次）；
在宿主终端上就是"第二次必挂"。让 `brand` 那次失败真正发生的大概率是它的**第二个**成因：
readiness 判据 `curl` 没带 `-f`，一个残留的、正从已删目录应答 404 的服务器能骗过检查。

- [x] `run.sh` 用 `exec` 让 `$!` 就是服务器 PID（与 `scripts/local-run.sh` 既有写法一致）
- [x] readiness 改为比对**每次运行重新生成的哨兵文件**（只有"正在服务本次目录"才算就绪）
- [x] `stop_server` + `trap … EXIT`：kill 后轮询到端口释放，`^C`/超时/断言失败都不留监听者；
      入口处端口被占用时明确报错（exit 2）并给出处理办法，而不是甩 Python traceback
- [x] 验证：`brand` 连跑 20 次全绿；完整 `make ui-check` 连跑 2 次全绿（修复前第二次必挂）；
      `SIGTERM` 打断后残留服务器进程 0、端口已释放；端口占用时明确拒绝
- [x] 记录进 `scripts/ui-harness/README.md`「第四个坑」

### v0.14.0 发布与部署记录（2026-09-15，**本机 8088 重启待执行**）

- [x] `scripts/release.sh minor`：`0.13.0 → 0.14.0`，提交 `6bf8dce`、打 tag `v0.14.0`、构建发布物
      （`bin/aigw -version` = `aigw 0.14.0 (revision 6bf8dce)`）。档位理由：M49 是新对外能力
      （6 个新管理端点 + MCP 工具、新控制台页、迁移 0018），不是缺陷修复。
- [x] 发布物内嵌资产抽查：`pages/org.js`/`js/tree.js`/`sidebar-slot`/`org/nodes` 均在二进制内。
- [x] 回滚点：`bin/aigw.prev-0.13.0-ba25ed3`（用临时 worktree 从 tag `v0.13.0` 原样重建，
      `-version` 确认 `0.13.0 / ba25ed3`，与正在跑的实例完全一致）。
- [x] 隔离端口验证（`:8087` + `.cache/v0140-release/` 全新空库）：`/version` = `{"revision":"6bf8dce","version":"0.14.0"}`、
      `/healthz` ok、`/admin/ui/` 与 `js/pages/org.js`/`js/tree.js` 均 200、登录后 `POST /org/nodes` 201
      （M49 端点在新库上从零可用）、启动日志 `level=ERROR` 为 0；验证完即释放端口。
- [x] 磁盘上的 `bin/aigw` 已是 v0.14.0（`scripts/local-run.sh` 的 `BIN` 默认就是它）；在跑的 `:8088`
      （`0.13.0 / ba25ed3`）在重启前不受影响。
- [x] **已重启（2026-09-15 19:27，用户在宿主终端执行 `scripts/local-run.sh restart`）**，线上复测全过：
      `/version` = `{"revision":"6bf8dce","version":"0.14.0"}`、`/healthz` ok、`/admin/ui/` 与
      `js/pages/org.js`/`js/tree.js` 均 200，且服务出的 `app.js` 含 `sidebar-slot`（确认不是旧嵌入资产）；
      启动日志 `aigw starting version=0.14.0 revision=6bf8dce`，**重启之后 `level=ERROR` 为 0**
      （日志文件里仅存的一条 ERROR 是重启前 17:39 的历史记录）。
- [x] 迁移确认：实际部署库是 **`data/aigw-local.db`**（启动日志指明；`data/aigw.db` 是另一份示例库，
      第一次核对查错了文件）——其 `schema_migrations` 已到 `(18, '0018_org_structure')`，
      `org_nodes`/`org_node_accounts` 两表与 4 个组织索引就位。
- [x] M49 端点在线上做了一次写路径往返：登录 → `GET /org/nodes`（空列表，端点已接线）→
      `POST /org/nodes` 201 → 列表可见 → `DELETE` 200 → 列表清空（不留残留）；
      审计里留下 `create`/`delete` 两条 `org_node` 记录（含 `nodes_deleted: 1`）。
- 回滚（如需）：`cp bin/aigw.prev-0.13.0-ba25ed3 bin/aigw && scripts/local-run.sh restart`。

### 组织相关界面的三项改进（用户反馈）

1. **过滤支持拼音与英文**；2. **成员过滤控件不跟着列表滚动**；3. **选中的成员自动排顶部**。

- [x] 生成拼音表：`scripts/gen-pinyin.py` → `internal/webui/static/js/pinyin.js`（20924 字、137KB）。
      数据源 [mozillazg/pinyin-data](https://github.com/mozillazg/pinyin-data)（**MIT**），文件头记录版本与
      输入 SHA-256；声调剥离、ü 写 v；覆盖 CJK 基本区，表外字符回退子串匹配（写进 `docs/org.md`）。
      控制台零构建、不能 import npm 包，所以表必须是仓库文件
- [x] `pinyin.js`：`matchesQuery(text, query)`（字面量优先，再查拼音）+ `candidates()`（全拼/首字母/多音字，
      组合数超 64 时退化为首选读音）；**去掉分隔符再算一遍**，所以 `devzs` 能搜到 `dev-张三`
- [x] `tree.js`：新增 **`matcher` 回调**（默认仍是大小写不敏感子串）。控件**不依赖**拼音表——
      拼音是调用方注入的能力，任何用树的地方都不必背 137KB。`labelText()` 顺带修掉"renderLabel 返回
      Node 时 String(node) 变成 [object …]"的过滤失效
- [x] `pages/org.js`：树过滤器与成员过滤器都走 `matchesQuery`
- [x] 成员面板：`.org-member-panel` = 固定过滤行 + 独立滚动列表（过滤框不再被滚走）；
      勾选的成员排序置顶（每次重绘排序，勾选后立即重绘），并显示「已选 N 个」
- [x] **先证伪再修**：harness 先加断言（拼音全拼/首字母/多音字/中英混合、过滤器滚到底后位置不变、
      勾选置顶/取消回位），跑在未改代码上为红，改后转绿
- [x] Go 测试 `internal/webui/pinyin_test.go`：表长度必须是 U+4E00–U+9FFF 的 20924 条（截断/偏移一位
      都会被抓住）、关键字的读音（含长/重两个多音字）、读音必须是纯 a-z（声调未剥离就是不可匹配）、
      文件头必须记录来源与许可、**两个过滤器都用了 matchesQuery 而 tree.js 不许 import 拼音表**
- [x] 静态回归 `org_tree_test.mjs`：拼音注入方式、成员面板结构、滚动/边框归属、置顶排序与"勾选即重绘"
- [x] 顺带修掉 harness 的一个**"测试不会失败"**漏洞：runner 现在支持视图声明 `strictChecks: true`，
      声明后 checks 里非布尔/数值的项按失败处理。原因是我的断言 helper 失败时返回描述字符串，
      在旧规则下**显示通过**（实测：故意写错期望值时旧规则全绿、新规则正确判红）。默认不严格，
      因为 models/chat/bridge 故意把 checks 当证据草稿纸——改它们的语义是另一件事
- [x] `make verify` + `make ui-check` 全绿（org 52 项、tree 57 项）
- 未做：8088 仍是旧构建，这批 UI 修复攒完再发版本（计划 patch 升 `0.14.1`）

### 修掉树形控件的展开箭头渲染成方框（用户反馈：父节点的左边有一个方块）

反馈现场同样是 v0.14.0 上线后的控制台。这不是布局问题，是**缺字形（tofu）**：树控件的展开/折叠
箭头原来用的是文字字形 `▸`/`▾`（U+25B8/U+25BE），这对冷门几何字符在不少运维字体栈里没有，
缺了就是个空方框。**本仓库同一条坑已有先例**：弹框关闭按钮的 `✕`（U+2715）当年也是因此改成
内联 SVG（commit `93c8b78`），`ui.js` 的 `closeIcon()` 注释把这个理由写得很清楚——"画出来的
图形不依赖读控制台那台机器装了什么字体"。我这次用了字形，等于把那条教训又踩了一遍。

- [x] `tree.js`：新增 `arrowIcon(expanded)` / `leafIcon()`，用 `createElementNS` 画内联 SVG
      （展开态朝下、折叠态朝右，叶子是圆点），照 `closeIcon()` 的范式；`aria-expanded` 仍然保留
- [x] `app.css`：`.tree-toggle` 由 `text-align:center` 改为 flex 居中（里面已是 SVG，没有文字），
      并新增 `.tree-arrow { display:block }`
- [x] **先证伪再修**：先在 `tree.page.html` 加断言（toggle 里必须有 `<svg>` 且**没有文字内容**，
      箭头形状随展开态变化），跑在未修复的代码上实测 `arrowIsDrawn`/`arrowHasNoGlyph` 为红；
      修复后转绿；再把 SVG 换回字形复测，两条再次变红——两个方向都验过
- [x] 补一条对齐断言：`arrowColumnIsUniform` —— 同一深度的父节点（有箭头）与叶子（圆点）的标签
      必须从同一 x 开始，避免换成 SVG 后树的标签列参差
- [x] 静态回归（`org_tree_test.mjs`）：**剥掉注释后**代码里不得再出现 `▸`/`▾`/`·`（注释保留说明是
      为了下一个人不再改回去），并钉住 `arrowIcon`/`leafIcon`/`createElementNS` 的存在
- [x] `make verify` 全绿、`make ui-check` 21 视图全绿（tree 由 44 项增至 51 项）
- 未做：**8088 仍是旧构建**——按你的选择这批 UI 修复攒完再发一个版本（计划 patch 升 `0.14.1`）

### 修掉组织相关界面的勾选框布局（用户反馈：节点详情里勾选框与文字隔太远、不对齐）

反馈现场是 v0.14.0 上线后的控制台。根因不在组织代码，在 `app.css` 第 89 行的全局规则
`input, select, textarea { … width:100% }`——它命中成员行里**没有 class 的 checkbox**，把勾选框
撑满整行，账号名被挤到面板最右缘。这是本仓库同一条坑的**第三处**（`.plus-skill`、`.form-radio`
的注释里都写着这条），我这次漏了 `width` 的覆盖，只写了 `margin:0`。

同一处还有第二个我引入的问题：账户页筛选行用了`.field inline`——`.inline` 根本没定义，而
`.field` 是块级布局、文字 span 是 `display:block`，于是勾选框和「含子节点」分成两行、勾选框同样被撑满。

- [x] `app.css`：`.org-member input[type=checkbox]` 显式 `flex:0 0 auto; width/height:16px`；
      新增 `.org-member-name`（`flex:0 1 auto; min-width:0`，长名换行而不是把 id 挤出去）与 `.org-member-id`
- [x] `app.css`：新增工具栏行内筛选范式 `.filter-field` / `.filter-check`（标签+控件/勾选框），
      并在注释里写明「不要用 `.field` 承载行内勾选框」
- [x] `pages/org.js`：成员行的名字与 id 改用专有 class（原来是无 class 的 `<span>`，CSS 只能按位置猜）
- [x] `pages/accounts.js`：筛选行改用 `.filter-field` / `.filter-check`
- [x] **先证伪再修**：先在 `org.page.html` 加几何断言（量 `getBoundingClientRect`）并跑在**未修复**的
      CSS 上，实测 `memberCheckboxNotStretched`、`filterCheckboxNotStretched`、`filterCheckboxTextGap`、
      `filterVerticallyAligned` **四项为红**；修复后转绿
- [x] 实测数字（同一 harness）：**改前 `box=789x14`**（勾选框 789px 宽，名字被挤到最右缘）→
      **改后 `box=16x16 gap=8 dy=0`**
- [x] 静态回归（`internal/webui/tests/org_tree_test.mjs`）：钉住"勾选框必须显式给尺寸"与
      "工具栏不得用 `.field inline`"，从源码一侧守住根因
- [x] `make ui-check` 21 个视图全绿（org 37 项、org-accounts 10 项）、`make verify` 全绿

### M49 验收记录（2026-09-15）

#### 性能：改前 / 改后实测（12th Gen i7-12700K，`-count=3`）

| 基准 | 改前 | 改后 |
|---|---|---|
| `ResolveTagRecords`（Key 无自有标签） | 512 ns / 320 B / **11 allocs** | 3.5 ns / 0 B / **0 allocs** |
| `ResolveTagRecords`（Key 有自有标签） | 800 ns / 536 B / 17 allocs | 370 ns / 288 B / 9 allocs |
| `routing.BenchmarkPlan` | 3402 ns / 4948 B / **45 allocs** | 3459 ns / 4948 B / **45 allocs** |

- 关键点：**组织祖先链只在建快照时走一次**，请求路径只做一次 map 查表；`Plan` 的 allocs/op 与改前**完全相同**，
  即"加了组织继承"没有让路由变贵。守卫测试 `TestResolveTagRecordsFastPathDoesNotAllocate`（`AllocsPerRun == 0`）
  在改前的代码上**实测为红**（11.0 allocs），改后转绿。
- 中途一次真实回归：把账号侧搬到建快照后，`Plan` 一度变成 46 allocs/op。定位到一般路径多物化了一份 Key 标签名，
  改成**原地过滤**（keep-idiom，且"一个都没加进来"时直接返回预计算切片）后回到 45。
  这条是"性能是硬约束"这条决策真正起作用的地方——没有基准就会带着 +1 分配上线。

#### 隔离端口冒烟（`:8087` + `.cache/m49-smoke/` 独立空库；**未触碰在跑的 `:8088`**）

命令串（单条命令内完成，因为 DSH 沙箱会回收跨命令的进程）：

```sh
go build -o bin/aigw ./cmd/aigw && ./bin/aigw --config .cache/m49-smoke/config.yaml &
# 登录 → 建标签 org-only → 建 总部/研发部（研发部绑定 org-only）→ 建账号 + 无 grants 的 Key
curl -X PUT .../org/nodes/2/accounts -d '{"account_ids":[2]}'   # 账号加入研发部
```

实测结果：

| 检查 | 结果 |
|---|---|
| 启动日志 `level=ERROR` | **0** |
| 建节点返回 | `{"id":2,...,"path":"总部/研发部","tags":["org-only"],"depth":1}` |
| 加入前 `effective_tags` | `[]`（账号与 Key 都没有标签） |
| **加入后 `effective_tags`** | **`['org-only']`** ← 继承自节点，账号自身无标签 |
| `admin_explain_router`（节点外） | `grant.Models = {"*": true}`（`default_grant` 回落）、无候选 |
| `admin_explain_router`（节点内） | **`grant.Models = {"org-only-model": true}`** ← 授权并集被节点标签改变 |
| 移出组织后 `effective_tags` | `[]`（立即回收，写路径 `invalidateAll=true` 清掉了 30s 的 Key 缓存） |
| `GET /accounts?org_node_id=1`（含子孙） | 命中 1（`org_nodes: ["总部/研发部"]`） |
| 同查询 `include_descendants=false` | 命中 0（账号挂在**子**节点上） |
| 拒绝路径 | 环 → 400「cannot be moved under itself or one of its own descendants」；兄弟重名 → 409「already named "研发部" under parent node 1」；未知标签 → 400「unknown tag: nope」；无 cascade 删子树 → 409「has 1 descendant(s)… Pass cascade=true」；未知账号 → 404；缺 `account_ids` → 400 |
| 迁移 | `schema_migrations` 出现 `(18, '0018_org_structure')`；`org_nodes`/`org_node_accounts` 落库；4 个索引都在 |
| 审计 | `target_type=org_node` 的 `create/update/assign/delete` 全部记录（含 `nodes_deleted`、`members_after`、`moved`） |
| 端口 | 验证后 `:8087` 已释放；`:8088` 在跑实例（`0.13.0 / ba25ed3`）**全程未受影响** |

**冒烟抓到一个单测与 harness 都没抓到的真 bug**：`account_count` 只在 `include_accounts=true` 时才算，
否则恒为 0——控制台树上的「N 个账号」会对一个有成员的部门显示 0。已修（成员关系总是读，只有**账号名**才按需解析），
并补了回归测试 `TestOrgNodeAccountCountIsAlwaysAccurate`；把修复回退后该测试**实测为红**，确认它盯的就是这个形状。
harness 之所以漏掉它，是因为 fixture 直接给了 `account_count` 字段——**真实网关才是这条字段的裁判**。

#### 控制台走查（`make ui-check`，21 个视图全绿，新增 4 个）

- `tree`（44 项）：同一控件挂侧边栏与工作区两处，断言两种模式的缩进/元信息差异、折叠展开、
  键盘 ↑↓←→Enter/Home/End、roving tabindex、行内 action 回调、过滤保留祖先、空态、孤儿节点、**成环数据不挂死**。
- `org`（32 项）/ `org-readonly`（7 项）/ `org-accounts`（7 项）：stub 是**会变的**组织树，
  断言侧边栏树与工作区树选中同步、成员保存发出的原始 URL 与请求体、删除带 `cascade=true`、
  账户页把 `org_node_id`/`include_descendants` 发到服务端、只读角色写入口整体消失。

走查期间修掉 3 个真 bug（都写在上面「可复用树形控件」与页面代码的注释里）：
1. `visibleRows` 把**被折叠**的节点误判成"不可达"，于是折叠后子节点又被补画回来（折叠看起来没生效）；
2. 键盘展开/折叠后 DOM 被重建，焦点掉到 `<body>`，**键盘导航只能用一次**；
3. 组织页调用了一个不存在的 `createChild`（应为 `createNode`）——「子节点」按钮点了没反应。

另外把账户页的组织筛选参数改成**只有选了节点才发送**，这样未筛选的请求 URL 与改动前逐字节相同
（`paging` 视图的 URL 断言因此保持全绿；那是"不改变既有行为"的可执行证据）。

> 范围外：按组织的用量/费用汇总、请求日志的组织维度分组、拖拽改父（用「父节点」下拉）、门户侧组织展示、
> `bootstrap` 初始化组织、生产（gpt001）部署与发版。

## M50 前端资源混淆压缩（2026-09-15）

> 设计文档：`docs/design/m50-frontend-minify.md`（含第 9 节实现差异、第 10 节验收记录）。
> 用户三项决策：强度 = esbuild 压缩 + 局部标识符重命名；交付 = 构建期生成 + `go build -overlay`（仓库只留源码）；
> 本次不做传输层预压缩（.br/.gz）。

- [x] 设计文档先行（`docs/design/m50-frontend-minify.md`）；「零构建」措辞在 6 处按实际行为修订
      （README、`docs/TODO.md` M9 条、`docs/design/m9-web-console.md` §1、`internal/webui/embed.go` 包注释、
      `scripts/gen-pinyin.py` 文件头说明、`scripts/ui-harness/README.md`）
- [x] `internal/webui/minify`（新包）：逐文件 esbuild 转译（`.js` 用 `FormatESModule`+`Target=ES2020`+
      三种 minify+`CharsetUTF8`；`.css` 只压空白与语法，**不**动标识符），HTML/SVG 逐字节复制
- [x] 契约：导出名保留（页面互调与 `embed_test.go` 静态断言的前提）、相对 import 说明符不变、
      文件集 1:1、DOM id/class 与 API 路径原样、镜像原子落盘（临时目录 → rename）、
      **任一文件解析失败即让构建失败**（不回落成源码）
- [x] `cmd/minifyui`：CLI + overlay 生成（单独成命令——esbuild 的 Go API 约 10 MB，不进 `cmd/aigw`）
- [x] `Makefile`：`ui-dist` 生成镜像与 overlay；`build` 依赖它并用 `-overlay` 嵌入；
      `build-src` 保留可读版并打印 `ui: source assets (not minified)`；`clean` 一并清理 `.cache/ui-dist`
- [x] `internal/webui/minify/minify_test.go`：7 项契约测试（1:1 覆盖、导出名、import 图闭合、CSS 选择器/
      自定义属性/`@media`、overlay 完整、确定性与旧目录替换、坏文件失败路径）
- [x] **变异验证**：让遍历跳过 `js/pages/*.js` → 5 项测试变红（"mirror file set differs"），断言有咬合力
- [x] `scripts/ui-harness/run.sh` 新增 `UI_STATIC_DIR`：同一套 21 个视图的浏览器走查可指向压缩产物
      ——**这是"压缩没有改变行为"的主要证据**（Go 侧静态断言读的是源码，看不到压缩后的回归）
- [x] `internal/arch/layering_test.go` 登记 `internal/webui/minify` 为叶子包
- [x] 依赖：新增 `github.com/evanw/esbuild v0.28.2`（纯 Go、无 CGO、仍然零 npm）

### 实测

| 检查 | 结果 |
|---|---|
| `make ui-dist` | 37 个文件 **559,442 → 329,702 B（-41%）**，overlay 35 条，约 150 ms |
| `make build` vs `make build-src` | `bin/aigw` **21,915,749 → 21,686,381 B（-229,368 B）** |
| `strings` 抽查 | `renderShell`/`const STATS_SORTS`/`api.post('/routes'` 压缩版全为 0；`record_output_text` 两边都是 21（字符串合约留下） |
| `go vet ./...` / `go test ./...` | 干净 / **38 ok**（改动前 37 ok） |
| `make ui-check`（源码树） | 21 个视图全绿 |
| `UI_STATIC_DIR=.cache/ui-dist/static make ui-check`（压缩镜像） | **21 个视图全绿，逐视图检查项数量与源码树完全相同**（工作目录里 `js/app.js` 实测 3007 B = 压缩版，确认被测对象没搞错） |
| 隔离端口 A/B（`:8093` 压缩版 / `:8094` 源码版，各自空库） | 状态码逐条相同（含 `/index.html` 的 301 与缺失资源的 404）、`index.html` 逐字节相同、`Content-Type`/缓存头/CSP 照旧；两实例启动日志 `level=ERROR` 均为 0；`:8088` 在跑实例全程未受影响 |
| `make ui-base`（node 在 PATH 上时） | 与改动前**同样 3 过 2 红**（既有红项），无新增失败 |

- [x] 一处**验证方式本身被修正**（写进设计文档 §9）：`UI_STATIC_DIR` 先写进文档、后写进脚本，
      于是第一次"对压缩产物跑走查"实际复制的仍是源码树，两边当然一致——补上变量后重跑才是真证据
- 未做：传输层预压缩（`.br`/`.gz` + `Content-Encoding`）、发布 sourcemap（会抵消混淆）、
  控制流扁平化/字符串加密（需 npm 工具链，且会让既有静态合约测试与 harness 断言大面积失效）
- 未做：**发版与部署**。本改动只在控制台资产与构建路径上，`VERSION` 未动（仍 0.14.0），
  按 `release-version` skill 升版本/打 tag/部署 gpt001 是独立一步

### v0.14.1 发布与部署记录（2026-09-15，本机 :8088 已升级）

- [x] `scripts/release.sh patch`：`0.14.0 → 0.14.1`，提交 `1a1295d`、tag `v0.14.1`、
      构建 `aigw 0.14.1 (revision 1a1295d)`。档位理由：本次带的 M50（控制台资源构建期压缩混淆）
      不改任何 API、配置语义与默认行为，只是产物与构建路径的改进；另附三个 M49 界面修复
      （组织过滤支持拼音/英文、树形控件展开箭头改内联 SVG、成员行勾选框布局）——都是缺陷修复，
      故取 patch 而非 minor。发布前工作区是脏的（另一会话的 M50 提交 + 本轮 TODO 拆分），
      先把 TODO 拆分提交为 `61ec746` 才满足 `release.sh` 的"干净工作区"前置。
- [x] `make verify` 全绿（vet + 全量测试 + build，38 个测试包；`make ui-base` 因 PATH 上没有 node
      而跳过——与既有记录一致，该派生仍由 `make ui-check` 覆盖）。
- [x] 隔离冒烟（`:8086` + 全新空库 `.cache/v0141-release/`，脚本 `.cache/v0141-release/smoke.sh`
      在同一条命令内起停，因为 DSH 沙箱会回收跨命令的进程）：**全部通过**——`/version` =
      `{"revision":"1a1295d","version":"0.14.1"}`、healthz/readyz/admin-ui 三个 200、
      服务出的 `app.js` 与压缩镜像**逐字节一致**（3007 B，源码 6097 B）且 `renderShell`/`STATS_SORTS`
      各 0 处、`js/ui.js`/`js/router.js`/`js/pages/org.js`/`app.css` 全 200、
      `schema_migrations` 到 `0018_org_structure` 且 `org_nodes`/`org_node_accounts` 就位、
      登录 + `POST/GET/DELETE /org/nodes` 写路径往返（跑完即删，无残留）、启动日志 `level=ERROR` 为 0。
- [x] 回滚点（本次的两个都留着，互为备份）：
      `data/aigw.prev-running-20260915-210057`（部署脚本从**运行进程** `/proc/<pid>/exe` 取，
      = 升级前线上真身 `0.14.0 / 6dc9082`，sha256 `87ab477e…`）；
      `bin/aigw.prev-0.14.0-6bf8dce`（用临时 worktree 从 tag `v0.14.0` 原样重建，sha256 `7cb4f809…`，
      与真身的差异只是它晚于 tag 的三个界面修复提交）。
      另有部署前预取的副本 `bin/aigw.prev-running-0.14.0-6dc9082`（与前者 sha256 相同）。
- [x] 部署：`ssh 127.0.0.1 'bash .cache/deploy-0.14.1/deploy-local.sh'`（**那条 ssh 登录到宿主，
      所以脚本不在沙箱里**；DSH 沙箱内的进程会被回收、也发不了信号）。顺序是
      「取回滚点 → `local-run.sh stop` → `install` + `mv` 原子换入 → `start` → 等 `/version` → 自检」，
      自检失败会自动换回回滚点再起。脚本沿用 v0.12.4 那两条教训：回滚点不从 `bin/aigw` 抄
      （它早被 `make build` 换成新版），回滚不直接 `cp` 覆盖在跑的二进制（会 ETXTBSY，必须先停）。
- [x] 线上验证（升级前 `0.14.0 / 6dc9082` → 现在 `0.14.1 / 1a1295d`）：
      `/version` = `{"revision":"1a1295d","version":"0.14.1"}`；`healthz`/`readyz`/`admin/ui/` 均 200；
      控制台资产是本版——服务出的 `app.js` 3007 B 与压缩镜像逐字节一致、`renderShell` 0 处、
      `js/pages/org.js` 200，且 `brand.js` 读 `/version` 渲染角标（左上角应为
      `AI Gateway  v0.14.1  1a1295d`）；运行库 `data/aigw-local.db` 最高迁移 0018、两张组织表就位；
      重启之后 `level=ERROR` 为 0。功能面只读走查：登录 200、`GET /org/nodes` 200（空树，端点已接线）、
      `GET /requests?limit=1` 200（返回真实 codex 行，身份列齐全）、`/v1/models` 401（未带 Key，符合预期）。
- 回滚（如需，在宿主终端执行）：`cp data/aigw.prev-running-20260915-210057 bin/aigw && scripts/local-run.sh restart`。
- 未做：**gpt001 生产未部署**（本次只要求升级本机 8088；线上仍为 `0.12.3 / 44f9de2`）。

---

## M51 dshgw 多租户 dsh 网关（写进本仓库、与 aigw / dsh 双向解耦）

设计：`docs/design/m51-dshgw.md`；规格：`docs/dshgw.md`。
定位：`cmd/dshgw` 独立二进制 + `internal/dshgw/**`，**只通过 HTTP 与 aigw 集成**，
不改 aigw Go 核心、不改 dsh 发行包；两者各自可独立升级。

### 文档先行（PROCESS.md 硬要求）

- [x] 设计文档 `docs/design/m51-dshgw.md` 已在对话中展示并获确认
- [x] 规格文档 `docs/dshgw.md` 已在对话中展示并获确认
- [x] README 文档表登记 `docs/dshgw.md`
- [x] `docs/TODO.md` 增 M51 小节

### M51 实现与无特权自动化验收

以下勾选表示代码、测试与一次性环境验证完成，**不代表已经在目标主机部署**；root/真实 Key/资源验收仍留在 TODO。

- [x] `cmd/dshgw` 子命令骨架（serve / tenant / bind / login-url / sync-models / revalidate /
      capture-url / contract / doctor / backup / migrate-nginx / upgrade-dsh）

- [x] `internal/dshgw/{config,registry,session,handshake,proxy,tenancy,aigw,contract}` 包骨架与类型

- [x] **导入闸门测试**：`cmd/dshgw/**`、`internal/dshgw/**` 依赖本仓库 `internal/dshgw/**` 之外的包即失败

- [x] Makefile：`dshgw-build` / `dshgw-test` / `dshgw-verify`（不并入 `verify`，避免拖慢日常）

- [x] config：yaml.v3 严格解码 + `TenantOrigin` / `SessionCookieName`（**cookie 不分端口，故按租户命名**）

- [x] registry：`registry.json`(0600) + `keys.map`(0640) 原子写、**双端口分配**（公开 TLS 段 + worker 回环段，registry ∪ `ss -ltn`）

- [x] session：只存 sha256、滑动续期、会话→上游 cookie 映射、flock + reload/merge 的跨进程原子持久化

- [x] handshake：`handshake/<t>.url` 读取、manual redirect、303 + Set-Cookie 解析、上游 401 自愈重握手

- [x] proxy：8 条不变量（剥 Cookie / 吞 Set-Cookie / 固定 Host / 清 Origin+Sec-Fetch-* / 401 重握手 /
      拒 absolute-form / **边缘 origin 闸门（按端口判跨源）** / WS 通道不缓冲）

- [x] 门户与分派：登录表单 + `prefix→tenant` + 跳转到租户端口 + 下发该租户 cookie；`Dispatch` 按 Host 端口分派（未知端口 404）

- [x] 登录限流取 `X-Real-IP`（不用可伪造的 XFF 链）

- [x] aigw 客户端：`ValidateKey` 的 401/200/空清单语义（空清单 ≠ Key 无效）

- [x] worker 单元模板：`/srv/dsh/%i`、`EnvironmentFile` + `${DSH_PORT}`、`ProtectHome=tmpfs`、
      `ExecStartPost=+dshgw capture-url`、`MemoryHigh=1.5G`/`MemoryMax=2G`、`Slice=dsh-workers.slice`

- [x] `dsh-workers.slice` 总账 `MemoryMax=40G`

- [x] 租户 `settings.yaml`（aigw provider + 模型清单）与 `.credentials.yaml`（**仅三键**、0600）渲染

- [x] 目录选择器（D13，默认 `clamp`）：租户 patch 层 disable `directory-picker-auto`，
      insert **两半**——`file:///opt/dshgw/share/dsh-plugin/picker-clamp.js`（自研夹紧 host 半，root 拥有）+
      `@deepseek-ai/dsh-client-ui-directory-picker-browse`（官方 browser 半；只插 host 半会导致"添加工作区"点了没反应）

- [x] 写 `picker-clamp.js`：`extends DirectoryPicker` 注册 `ctx.directoryPicker`，`kind: 'browse'`，
      把 `list` / `createDirectory` 夹到 `/srv/dsh/<t>`；`crumbs` 从租户根起；
      **必须用 `fs.realpath()` 做根检查以挡住 symlink 逃逸**；构造期自检失败则降级为"一律拒绝"（绝不回退到未夹紧实现）

- [x] `directory_picker: clamp | browse` 配置项（`browse` 仅调试；**不提供 `off`**）

- [x] 网关路径策略（D14 修正）：**不做路径白名单**——保留合法 Path/RawPath/query、拒 `.`/`..` 段与 absolute-form，其余一律转发
      （理由：插件可注册任意 upgrade 路由如 `/browser-fs/ws`，静态面还有 `/plugins/<pkg>/client.js`；写死白名单会挡掉插件）

- [x] WS upgrade 必须过边缘 origin 闸门并加测试：同源 101 / 跨源经网关 403 / 普通 GET 打 upgrade 路由 426

- [x] 可选插件 `dsh-browser-fs@0.2.0`（D14，**默认 on**，每租户 `plugin_browser_fs: on|off` 可关）：
      在**模板 home** 里 `dsh plugin --profile web add dsh-browser-fs@0.2.0` 后作为模板复制给新租户
      （保住"新 home 启动不需网络"）；契约测试断言行加载、DSH 页面广告的 browser-fs client 批量资源 URL 200、WS 426/101

- [x] 契约测试补一条：租户 patch 渲染后 `ctx.directoryPicker.capability().kind === 'browse'`
      且**夹紧生效**（`list('/etc')` → `directory-unreadable`、`list()` → 租户根、realpath 逃逸被拒）

- [x] `HOME=/srv/dsh/%i` 让选择器从自己的目录打开 + 预注册工作区（`WorkspaceSeed`）
      注：browse 列的是**服务端**文件系统（不是浏览器本机），夹紧 ≠ 隐藏操作系统可读文件

- [x] 建户全流程：useradd → 家目录 → DSH_HOME → unit → start → 回环 401 探针 → 再 enable

- [x] 入口：**宿主 nginx** `conf.d/dshgw/*.conf`（门户端口 + 各租户端口，TLS 复用现有 `*.tirisen.hk`）；
      **不动 nginxWebUI 数据库**（可选：经 WebUI 正规注册既有 `/dsh/` location 指向门户）

- [x] `dshgw.service` 绑 `127.0.0.1:3099`（不依赖 docker0）

- [x] dsh 契约 7 条（CLI / 启动行 / token 交换 / 栅栏三态 / `$DSH_HOME` 自举 / 凭据三键 / 沙箱 fail-closed）

- [x] aigw 契约（401/200/空清单/超时；`Bearer` 与 `x-api-key` 等价）

- [x] `upgrade-dsh`：候选契约通过后切软链 → 仅重启原先 active 的 worker，失败先恢复旧链再回滚已尝试 worker

- [x] `backup` + `tenant remove --purge` 先快照再二次确认

- [x] 复核修复 nginx 吞 gateway cookie、doctor 误判目录、失败建户误删保留数据、systemd 状态/回滚吞错、快照覆盖与缺失、升级错误启动停用 worker
- [x] 新增 CAS 迟到 cookie、会话缓存撤销、原样路径/合法百分号、cookie trailers、重复 cookie logout、源日志/CLI 密钥脱敏、逐级 nofollow 文件操作及容量边界回归
- [x] 生成支持 strict 字段的 DSH provider 兼容配置，保留普通工具参数可选性；通过公开配置接缝，不改 DSH 发行包/审批策略
- [x] `go test -count=1 ./...` 与 `go vet ./...` 通过；tenancy/upgrade `-count=5 -shuffle=on` 通过
- [x] 最新 `make dshgw-verify`：Go/import gate/vet/build、picker 15 项、DSH 7 条契约、临时 browser-fs 模板与 fresh HOME 无 pnpm 安装、client 200、WS 426/101 全通过；`file bin/dshgw` 确认为静态 ELF
- [x] 已回填当前实现差异与验收边界、提供部署/权限/恢复手册；规格保持实现中，未冒称 root 主机已验收

### M51 主机验收准备与真实热载协议（2026-09-16）

- [x] `login-url` 无参数返回门户；`tenant list --json` 追加 UID/路径/unit/origin/alias 等只读元数据，保持旧字段/表格兼容，CLI 回归通过
- [x] 分阶段 root 验收脚本 `scripts/dshgw_host_acceptance.py`：先读后建两临时租户、UID/EACCES/cgroup/loopback、TLS/cookie/WS、真实 worker RPC、重启/退出、停用 Key 观察、身份指纹保护与 snapshot-first 清理；公共 report 与私有 state/cli.log 分离
- [x] 脚本 13 项无特权 orchestration 回归通过；模型 RPC 用真实一次性 DSH + 假模型服务验证，通过公开游标 fence/rpcId/turn 证明完成，且未额外调用标题 LLM
- [x] 嵌入 `credentials_hotload.mjs`，新增必需升级闸门 `credentials-live-hotload`：同一 DSH/PID/session 上 Key A→B 实际生效；公开 reload 事件栅栏，不靠 sleep；成功/失败/SIGINT/SIGTERM 清理探针通过
- [x] 最新 `make dshgw-verify` 通过：基础 7 + 热载 1 条 DSH 契约、picker、模板及新增 Python 回归；root/真实凭据/资源等**未执行**项仍留在 TODO

### M51 宿主阶段 1 通过与后续启动修正

- [x] 用户 root 终端回传：nginx -t、/opt 安装及 browser-fs0.2.0 模板供应、8 项 DSH 契约全部通过；未启动公网入口/建户
- [x] 用户 root 终端回传：两把真实 Key 的认证、模型清单与 Bearer/X-API-Key 等价 contract 均 PASS（不等于模型调用已通过）
- [x] 修复共享父目录遍历与 umask077 问题，增加建户前真实 UID 读写探针和 doctor shared-root 检查；不增加 gateway 组成员、不放宽私有 Key 文件权限
- [x] gateway 改为目录级 RO 挂载，避免单文件状态挂载与原子 rename 冲突；serve 使用配置的 max_sessions；回归通过，宿主更新仍留 TODO
- [x] 用户确认现有 Key 均已授权并批准切 none；核对实际进程配置来源、私有备份后更新 gitignored config.yaml，仅 auth.default_grant 与注释，未重启运行中 aigw（运行态验收留 TODO）
- [x] 修正后全量 Go 测试/vet、独立 dshgw verify/静态构建/diff-check 通过
- [x] 提供 `scripts/dshgw_stage2.sh` 人工接续：显式确认、当前部署身份守卫、私有诊断、重启后 auth/Key/model 验证、仅启动回环 gateway；shell/Python 语法与未确认/非 root 拒绝测试通过，实际 root 执行仍未计为通过

### M51 宿主阶段 2：授权收口与回环 gateway 启动

- [x] 用户回传：本地 aigw 已以原身份重启，新日志确认 `auth.default_grant=none`；A/B 认证与 header 等价 contract 再次通过
- [x] 模型授权守卫正确阻止空清单 Key A 启动验收：A=[]、B 含 deepseek-flash；用户通过管理面补齐 A 显式测试标签后回传 A=[deepseek-flash]、两 Key 共同模型=[deepseek-flash]，未退回默认 all
- [x] 为这个可重试阶段添加 `scripts/dshgw_stage2.sh --verify-and-start`：跳过安装/重启，只复验模型与启动回环 gateway；语法和未确认/非 root 守卫测试通过
- [x] 用户执行 resume 回传 A/B deepseek-flash 可用、dshgw active、回环门户200；第一次 curl 连接尚未就绪，受控重试后成功。未由阶段 2 创建租户或开放 nginx 入口
- [x] agent 只读交叉核对：安装 state 根为751 root:dshgw、tenant根为711 root:root、Node/DSH可执行目录可遍历；systemctl show dshgw.service 为 active，RO=/etc/dshgw /var/lib/dshgw、RW=/var/lib/dshgw/gateway，已载入目录级挂载修正
- [x] 添加真实 nginx 非特权集成回归（临时可信 TLS/随机回环端口/真实渲染配置/假 worker）：登录/续期 cookie、同浏览器双租户、伪造 Host/端口头、跨源 HTTP/WS403、101帧回传与上游 cookie隔离通过；测试不触碰宿主 nginx、systemd 或现有 GUI

### M51 双真实租户 baseline 部分通过（模型请求仍失败）

- [x] 用户 root 回传 `render-nginx --reload` 成功（创建租户前0项），doctor 所有共享目录/文件模式属主/runtime/template/TLS/unit/include/nginx检查通过
- [x] run-001 已建立两真实租户 `m51-e2e-b405d9cc-a/b`；A/B实际Key头等价与deepseek-flash授权、两UID不同与互相EACCES通过
- [x] 两 worker 的进程UID、回环listener、实际 cgroup 1536M/2G/512/200% 与汇总40G检查通过（只读限额，不是压力命中证明）
- [x] 实际nginx链路门户登录/client200、browser-fs普通GET426/upgrade101、安全cookie与同浏览器租户绑定、跨端口HTTP/WS403通过
- [x] 验收在首个真实模型turn未完成时正确失败，未把已有部分PASS当成整体成功；agent只读systemctl核对两个测试unit均inactive/dead/MainPID0，数据与run目录保留
- [x] 修复验收脚本 WebSocket HTTPResponse 资源所有权：不再直接关闭其fp导致Python3.14最终析构flush已关闭对象；新增socketpair回归，14项脚本自测通过（模型turn失败尚未定位，不把它归因于资源关闭警告）

### M51 baseline 修复原因与保留租户续验通过

- [x] 安全限定匹配本地 aigw 日志确认失败原因：m51-test-a 的请求被 insufficient_quota 拒绝，可用0、预留42116微美元（0.042116USD），不是401授权/网络/代理错误；未猜测、未停计费检查、未改透支。用户确认已给两测试账户补齐小额额度
- [x] `resume-baseline --confirm-start-workers --allow-model-call` 已实现：复用原run目录、config/key指纹、两个精确UID/unit身份；拒绝新Key/model覆盖与未知/过渡态，保留历史证据，失败只停匹配的临时workers，不create/remove/purge、不重启主服务。30项无特权回归及真实一次性DSH协议测试通过
- [x] 用户root回传 run-001续验全部PASS：两Key认证/授权、两个UID/EACCES、实际cgroup、TLS/client/WS、跨端口/cookie绑定、A/B各一次真实模型turn、重启后请求恢复、退出只撤销当前浏览器会话
- [x] 公开报告保存在 `/root/dshgw-e2e/run-001/report.json`，私有state不共享；worker保留用于停用Key/外部可达/轮换/恢复等后续验收，尚未清理或提交


### M51 接续验收与门户故障定位

- [x] 用户root回传A停用验收 `disabled-key-model-401-and-configured-session-policy` PASS：模型401、key_revalidate:off保留既有UI会话，B未受影响。真实新Key轮换未纳入此通过项。
- [x] 临时Firefox 155.0.1原生表单对照：no-referrer使/login和/logout的Origin为字面量null；same-origin保留正确同源Origin。四例均为原生导航POST，不是手工Origin或fetch。临时回环/profile已清理；线上用户浏览器修复复验仍待执行。

- [x] gateway更新后真实浏览器 Key B 登录返回302并打开 `https://chat.tirisen.hk:32602/`。

- [x] 门户第二阶段CSP修复与用户最终复验：显式允许registry内租户origin作为form-action重定向目标；用户确认更新后的CSP头及浏览器自动跳转成功。此前“手工可打开32602”不代表自动跳转；最终确认发生于CSP修复部署后。门户保持:32600，不实施/dsh/。增加CSP精确来源无通配符断言，proxy测试通过。


## M52 aigw 后台“启用 DSH / 停用 DSH”

- [x] 设计确认：默认 `dsh_enforce: login`、不默认立即撤销既有会话、aigw 先行分两步交付（用户 2026-09-16 确认）
- [x] 迁移 0019_account_dsh_enabled + domain/store `dsh_enabled` 读写
- [x] aigw `POST /v1/dshgw/authorize`：Bearer/X-API-Key；suspended/closed 映射 403 `account_status`；停用 403 `dsh_disabled`；401 原样
- [x] admin `GET/POST /admin/api/v1/accounts/{id}/dsh` + 审计（幂等 no-op 不重复审计）+ 路由表守卫更新（公开路由 10→11）
- [x] 管理写后 InvalidateAll（账号行被 key verifier 缓存，不清缓存开关一个 TTL 内不生效）
- [x] webui 账号列表 DSH 徽标与“启用 DSH/停用 DSH”按钮（confirmDialog 语义说明停用影响）
- [x] dshgw `dsh_enforce`（login/interval/per-request）+ 登录/请求期执行点 + nil Authorizer fail-closed + 拒绝缓存
- [x] 回归：httpapi 新增 authorize/开关矩阵测试；dshgw 新增 7 项执行点测试；`go test ./...`、`go vet`、`make dshgw-verify`、`make dshgw-nginx-test`、UI harness 全部视图、30 项 fake 回归通过


## v0.15.0 发布记录（M51+M52，本机 8088）

- 2026-09-17：`scripts/release.sh minor` → 0.15.0，commit/tag `2616022`（含 255d89d M52 与 5180614 M51）。
- 部署：`scripts/local-run.sh restart` 本机 :8088；`GET /version` 返回 0.15.0/2616022，healthz/readyz 200，启动日志无 ERROR；v1/models 无凭据 401（数据面正常）；dshgw 门户 200；dshgw.service 与 dshgw-admin.service active；dshgw 二进制同版（0.15.0/2616022）。
- 回滚点：上一运行版本 0.14.1/dev（tag v0.14.1 构建可复现）；本发布未改动 config.yaml 与数据。
- 遗留：M52 rev2 主机验收（后台启用/停用全流程、新建 Key 免绑定登录）与 DSH 上游限制决策（A/B）仍在 TODO。

## M53 请求日志的供应商维度（按供应商统计成本，2026-09-17）

设计：`docs/design/m53-request-provider-dimension.md`；规格：`docs/request-log.md` §2/§4/§6。

- [x] 起因核查：写路径本就按供应商归属（每次尝试的 `provider_id` + 该供应商映射的成本规则），
      缺的是读路径——`RequestLogDimensionNames` 无 provider、日志行无 provider 列、小时汇总也没有。
- [x] 口径：贡献粒度 (request, provider)，成本/token 归到实际服务的那家；桶「请求数」= 该供应商服务过的
      请求数，故各桶之和可能大于窗口总数（失败转移在两家各计一次）；未计量请求进 `provider_id=0` 未知桶。
- [x] store：`providerDimension`/`requestLogGroupExpr("provider")`、`providerDimensionSource`（尝试粒度源，
      保留 join 以让桶集合正确）、`selectDimensionSources(..., exact)`、`dimensionSourceSQL(..., byProvider)`、
      参考查询 `requestLogDimensionReference`（probe/独立对照）、`RequestProviders`、`ProviderNames`。
- [x] 汇总表绕过：`dimensionReadIsExact = providerDimension(groupBy) || f.ProviderID > 0`——汇总按请求预聚合
      已丢掉供应商，带供应商过滤的查询也没有 `request_id` 可关联；行与总数仍由同一快照、同一源算出。
- [x] `provider_id` 过滤：请求级相关 `EXISTS`（列表/总数/统计同口径）。外层列显式写 `request_logs.request_id`——
      裸写会解析到内层别名，令过滤恒真且 SQLite 不报错（已由测试与走查覆盖）。
- [x] httpapi：`provider_id` 解析（非数字 400）、列表/详情 `providers: [{id, name}]`、统计行
      `provider_id`/`provider_name`、路由表摘要与 `group_by`/`provider_id` 描述（MCP 工具描述由此生成）。
- [x] 控制台：分组下拉「供应商」、`/providers` 筛选下拉、列表「供应商」列（失败转移显示两家、未计量行说
      「未计量」）、统计行「名字 #id」与「（未知）」、详情「供应商」字段、`请求数` 表头与卡片说明写明计数口径。
- [x] 回归：`provider_dimension_test.go`（两家各计其成本、模型桶不受影响、未计量桶、**汇总建好后仍正确**、
      筛选三处一致、汇总前后一致、id 升序去重/缺名不造名）；既有独立对照测试在 raw/rolled/dirty/disabled
      四种模式下覆盖 `provider`；httpapi 三项新测试 + MCP 端到端读回；`internal/webui/embed_test.go` 控制台/服务端
      维度契约；UI 走查 requests 视图 106 项（含 9 项供应商断言）通过。
- 未做（见 TODO）：`get_usage_breakdown` 的 provider 分组（涉及上游供应商身份口径）；`group_by=provider`
  的大窗口实测与专用汇总表。

## M54 控制台资源形态的运行态自述与部署产物隔离（2026-09-17）

设计：`docs/design/m54-console-asset-shape.md`（§9 实现差异、§10 验收记录已回填）。
起因：用户报告「本机 8088 的前端 js 没有混淆」——实测**复现不出来**（在跑的 `0.16.0/9dc4ed2` 逐文件
sha256 与压缩镜像全部相同），但"曾经不是"可证：M50 随 0.14.1 才上线的回滚点二进制里 `renderShell`=3。
真正的问题是**看到可读字节之后没有任何运行态信号能回答为什么**，且两条路径会静默把未混淆版换上 8088。

- [x] 形态由构建期声明承载：`-X main.uiAssets=minified` 与 `-overlay` **写在同一行**（两者无法单方面漂移），
      代码默认值是响亮的 `source`——"自称已混淆、实际是源码"在构造上不可能
- [x] `/version`、`/healthz` 新增 `ui` 字段（`minified`/`source`/空→`unknown`，缺字段与 unknown 必须可区分）；
      启动日志 `aigw starting` 加 `ui`；`aigw -version` 打印 `…, console minified`
      （`scripts/release.sh` 本就把它写进发布记录 → 每次发版自动留下形态证据）
- [x] 产物隔离：`make build-src` 改**写 `bin/aigw-src`**（调试构建碰不到部署产物）；`scripts/load.sh` 不再
      写 `bin/aigw`（压测脚本就地覆盖部署产物是纯粹的事故源），改为在 `$WORK` 下构建并运行
- [x] `scripts/local-run.sh`：`status` 增 `console: minified|source|unknown`（解析失败只影响这一行，
      不报错退出）；`start` 在非 minified 时显著告警但不拒绝（源码版实例仍是合法调试用法）
- [x] `internal/httpapi/version_test.go`：`/version` 与 `/healthz` 的 `ui` 一致 + 空值→`unknown`；
      `cmd/aigw/version_test.go`（实现差异 §9.2 补写）：`versionLine()` 两种形态 + 默认值必须是 `source`
- [x] 记录：README 构建小节与「常用入口」表、设计文档 §9/§10 回填

### 实测

| 检查 | 结果 |
|---|---|
| `./bin/aigw -version` / `./bin/aigw-src -version` | `…console minified` / `…console source` |
| `make build-src` 对 `bin/aigw` 的影响 | sha256 与 mtime **逐字节不变** |
| `strings` 指纹 | 压缩版 `renderShell`=0、`const STATS_SORTS`=0；源码版 3/1；`record_output_text` 两边 21 |
| 隔离端口 `:8111`/`:8112`（压缩版/源码版，各自空库） | `/version`、`/healthz`、启动日志三处的 `ui` 分别为 `minified`/`source`；状态码逐条相同（含 301/404）；CSP/缓存头照旧；两实例 `level=ERROR` 为 0 |
| `scripts/local-run.sh status` | `:8111` → `console: minified`；在跑的 `:8088`（早于 M54）→ `console: unknown（…早于 M54…）` 且不报错 |
| `scripts/load.sh 4 3s`（4663 请求，rps 1554） | 跑完 `bin/aigw` sha256 不变 |
| `go vet ./...` / `go test ./...` | 干净 / 全绿（48 个包）；在跑的 `8088` 全程未受影响 |

- [x] 提交：独立 M54 commit（引用设计文档），不改 `VERSION`、不打 tag、不发布
- 未做（保留在 `docs/TODO.md`）：宿主执行 `make build` + `scripts/local-run.sh restart` 让 `:8088` 上的
  `ui` 字段生效（本沙箱与宿主不同 PID namespace，无法向宿主进程发信号）


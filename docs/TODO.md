# 实现 TODO（里程碑检查清单）

> 维护规则：每完成一项即勾选；每里程碑开工前先写 `docs/design/{milestone}-*.md`
> **并贴到对话中确认后再写代码**（详见 `docs/PROCESS.md`，提交前按其中的检查项自检）。
> 状态：`[ ]` 未开始 · `[~]` 进行中 · `[x]` 完成

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
- [ ] 未覆盖：stdio 模式的后台工具（无管理员主体，需把 `cmd/aigw/main.go` 的依赖构造抽成共用函数；另立）

## M7 Hooks 与录制
- [x] 设计文档 docs/design/m7-hooks-recording.md
- [x] 异步 worker 池 + 有界队列（满则丢弃并计数，绝不阻塞请求）
- [x] Webhook HMAC-SHA256 签名（`t=…,v1=…`）+ 事件头 + 投递 ID；提供 VerifySignature 供接收方校验
- [x] 指数退避重试；4xx 不重试；最终失败写死信 JSONL；JSONL 投递器（本地审计）
- [x] 事件过滤（白名单/通配）与采样率；`include_content` 控制内容附带并受 max_bytes 截断
- [x] hook 配置持久化（`hooks` 表 List/Upsert/Delete）+ SetHooks 热替换
- [x] 请求路径接入：response.completed / response.failed 事件
- [x] 事件接入：response.completed/failed、request.denied、billing.inflight_warn/throttle/abort、billing.reconcile_mismatch、backup.finished/failed
- [ ] 其余事件（provider.*、apikey.*）在需要时补齐（当前没有消费方，避免无谓的事件量）

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

## M9 Web 管理界面
- [x] 设计文档 docs/design/m9-web-console.md（已在对话中输出）
- [x] internal/webui：go:embed 静态资源 + SPA 回落 + CSP + 缓存头；`/admin/ui/` 与 API 同源同会话
- [x] 原生 HTML + ES modules + fetch，零构建零依赖（环境无 node/npm，界面必须浏览器直跑）
- [x] 页面：概览 / API Keys（双勾选录制）/ 账户 / 标签 / MCP 令牌 / 供应商（详情·探测·发现·日志·动作·重启）/ 模型与路由 / 映射（含试算器）/ 请求日志（输入·思考·输出分栏）/ Hooks / 审计 / 设置
- [x] 定价 / 账本 / 账单 / 对账 / 备份 五个占位页（明确标注 M11/M12/M16，不做假交互）
- [x] 新增 `GET /admin/api/v1/router/explain`：复用数据面 Plan，返回解析结果、有序候选、排除原因与失败原因
- [x] 管理面 CSRF 收紧：POST/PATCH/PUT 必须 `Content-Type: application/json`
- [x] 测试：webui 资源/回落/CSP 用例 + CSRF 与 explain 端点用例（真实 store）
- [x] Node 语法检查与导入图校验（13 个页面模块，0 问题）
- [ ] 浏览器人工走查（当前环境无浏览器；已用 Node 对新页面做语法与导入图校验，接口逐条 curl 验证）

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

## M13 性能与并发
- [x] 设计文档 docs/design/m13-performance.md（已在对话中输出，含实测数字与两处修复）
- [x] 微基准：pricing.Evaluate、routing.Plan（含并行）、httpapi 端到端（非流式/并行/模型列表）
- [x] `cmd/loadgen`：标准库压测器（并发、时长、流式开关、rps 与 p50/p90/p95/p99）
- [x] `scripts/load.sh`：一键起服务 + 建资源 + 压测 + 账单/不变量自检
- [x] `server.pprof: true` 时挂载 `/debug/pprof/`（默认关闭）
- [x] **压测抓到并修复**：写入器重试复用过期 context → 整批落兜底文件（11 条）
- [x] **压测抓到并修复**：不变量巡检跨两次查询读快照 → 高并发下误报差异（改为单只读事务快照）
- [x] 实测：32 并发 × 10s → 619.6 rps、p50 1.73ms、p95 44.5ms；usage 与 ledger 逐条对齐、不变量全绿、无兜底文件
- [ ] 优化项（未做）：减少每请求的写入行数（response/request log 合并或异步），以压低 p99 长尾

## M15 模块解耦验证
- [x] 设计文档 docs/design/m15-decoupling.md（已在对话中输出）
- [x] `internal/arch` 分层断言：读取 `go list` 真实依赖图，逐包比对允许的模块内依赖
- [x] 三条关键禁令：httpapi 不得直接 import pluginhost/creds；只有 cmd/aigw 能同时 import store+httpapi；任何包不得 import cmd/
- [x] `examples/` 与 `pkg/` 同样受检（插件作者代码不得依赖 internal；新增 provider-codex 已登记）
- [x] 接口替身：`billing` 用 `proxyStore` 证明只依赖端口；httpapi 的端口假实现已在 M8b/M16 覆盖
- [x] 规则表修正 5 处与实际 import 图的偏差（pricing→domain、providers 子包、examples、arch 自身）

## M16 数据库自动备份
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
- [ ] v2：备份到对象存储/异地同步（规格已声明不在 v1 范围）

## M17 完善内置供应商（openai-chat）：DeepSeek 适配与思考模式
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
- [ ] 暂不纳入：内置 `openai-responses` 接 DeepSeek `/responses` 的三处缺口（`response.reasoning_text.delta` 事件名、`output_tokens_details.reasoning_tokens`、思考正文承载字段）

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

## M19 面向真实客户端的方言翻译（核心只接受，翻译在 provider 层）
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
- [ ] 观察项（非阻塞，未追查）：上述成功运行的 stderr 有 1–7 条 `codex_core::util: OutputTextDelta without active item`。已核对网关输出**事件顺序规范**且 delta 的 `item_id` 确为已 added 的那个，答案与退出码均正确 → 判断为客户端侧噪声或其对某类流式形状的额外期待
- [x] 同时确认既有修复在重启后的实例上仍然有效：codex 供应商探测 `ok=true`（`latency_ms=31149`，印证 60s deadline 的必要性——31s 远超旧的 10s）；`gpt-5.6-luna` 带 system 消息的请求返回 `好的，1+1=2。`（M10d）；界面资源含 `.spinner`/`withBusy`/`探测中`（M10c 的探测动画）
- [ ] 后续议题：provider 跳过工具（如 chat 路径丢掉 `web_search`）目前**无上报通道** —— 协议里没有 provider 声明降级的字段（`DegradedFeatures` 只由路由的能力校验写入）。要为"能力降级可见"补协议字段，另立里程碑
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

## 可选
- [x] M10 订阅后端参考适配器 `examples/provider-codex`（默认禁用、非官方）：设计文档 docs/design/m10-subscription-adapter.md
- [x] 三种凭据形态与自动续期：refresh_token（OAuth 刷新，轮换落盘 + 单飞）> session_cookie（/api/auth/session 换取）> 静态 access_token
- [x] token_file 一次性导入（CLI auth.json 等）；到期前 60s/启动前 5min 刷新；401 后只重试一次
- [x] 流式逐事件翻译（文本/思考/工具调用/用量）、Complete 复用同一 Stream 聚合、缓存命中与 reasoning 维度拆分
- [x] 动作 whoami/refresh_session/set_token（挂在既有 Providers 详情）；凭据永不回显、不进日志
- [x] 测试 9 个（假端点覆盖轮换落盘、单飞、invalid_grant、429 reset、session 换取、401→刷新→重试、导入、whoami 不泄露）+ README
- [x] 端到端实测：真实子进程经宿主拉起，探测 ok=true、流式 2+1 增量、非流式文本与 usage 正确、账本 cost116/charge174（1.5×）、轮换 token 落盘

### M10b 出网代理（`proxy`）
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
- [ ] 范围外（另立）：内建 provider 接代理，需新增 `internal/providers/httpx → pkg/providerkit` 分层边
- [x] 环境前置（已解决）：代理最初从本机不可达（0.12s 快速 RST，疑似只绑回环）；在客户端开启局域网监听后 `192.168.140.252:2334` 于 0.11s 连通

### M10c 健康探测改为真实流式补全
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
- [ ] 已知限制（本次不改行为，仅记录）：`reasoning.effort:"minimal"` 被上游 400 拒绝（`"low"` 可用但实测仍 14–17s 且 `reasoning_tokens:0`）；插件仍会透传客户端/配置的 `reasoning_effort`，配成 `minimal` 会导致 400
- [ ] **新发现的运维隐患（比本里程碑更严重，另立处理）**：真实部署里 refresh_token **已被轮换**，而插件**不把轮换后的凭据回写到数据库** —— 有效值只在 `$GW_PLUGIN_STATE_DIR/<instance>/session.json`，而数据库/控制台里显示"已设置"的那份是轮换前的失效值。状态目录一旦丢失（清 `data/`、改实例名、重装插件），供应商会以"凭据已配置"的姿态持续失败。协议里的 `notify`（设计用途正是凭据回写）插件未使用，属 M10 设计缺口
- [x] 控制台「探测」加处理中动画（M10c 的直接后果）：`ui.js` 新增可复用的 `withBusy`（禁用按钮 + spinner + 秒数递增 + 结束/异常都还原）、`toast` 支持 `{sticky}`、`probe()` 补上原先缺失的错误捕获并在探测后刷新列表；用 node + 最小 DOM 桩跑 10 项行为断言，并在隔离实例核对新二进制的内嵌资源
- [ ] 部署动作（需在宿主执行）：运行中的网关仍是旧二进制（10s deadline + 旧 `/me` 探测 + 旧界面资源；界面资源内嵌在二进制里，所以这次 UI 改动同样要重启才生效），需 `./scripts/local-run.sh restart`
- [x] 回填设计文档「实现与设计差异」（含实测结果与 8 条差异），单提交并引用设计文档路径

### M10d codex 适配器翻译 `system` 角色
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
- [ ] M14(2) 门户 API（`/portal/api/v1/*`：读自有数据 + 建/轮换自己的 Key + 兑换码 + 改口令）与第二套嵌入式 UI
- [ ] M14(2) 前端共享化：`internal/webui/shared/{api.js,ui.js}` 被 admin 与门户两套 UI 复用（各自前缀下服务）

### M19b 流式终态：截断不得伪装成完成（DSH 实测报告）
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
- [ ] **待宿主执行**：运行中的 8088 仍是旧二进制（本次会话所在的沙箱与宿主不同 PID namespace，无法向该进程发信号），
  需在启动它的终端执行 `./scripts/local-run.sh restart`；插件二进制已重建（`bin/` 与 `plugins/aigw-provider-codex`），重启后生效

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
- [x] 问题（M19d/M19e 定位时踩到）：请求硬失败（上游 400、流被切断）后客户端通常立刻断开，
  而 `persist` 用请求自身的 context 写请求日志与存储响应，于是**失败请求恰恰是唯一没有记录的**：
  日志里只留下 `recording request content failed ... context canceled`，请求正文得从客户端会话里重建才能定位
- [x] 修复（`internal/httpapi/v1.go`）：`persist` 用 `context.WithoutCancel` + 5s 超时派生的 context 写审计
  （保留 request id 等 context 取值，只丢掉取消）；钩子走内存队列本就不受影响
- [x] 测试：`TestFailedRequestIsRecordedAfterTheClientHungUp` —— 流式请求 + 300ms 延迟，在 provider 应答中途取消 context
  （模拟客户端挂断），断言请求日志仍然落库、状态 `failed`、正文完整、request id 保留；变异验证过（改回跟随 ctx 即失败）
- [ ] **同类问题（未改，涉及计费语义，待定）**：`recordAttempt` 写用量/结算也跟随请求 context ——
  客户端挂断时该次尝试的用量行同样会丢（上条测试的日志里就能看到
  `recording usage failed err="store: insert usage record: context canceled"`），
  即上游已经产出的 token 不会被计量。改法与审计一致（派生 detached context），但会改变用量/账本里出现的行数，需先定口径

### M20 DSH 侧可设推理档位：能力申报与档位表对齐（DSH 实测报告）
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
- [ ] 未覆盖：DSH GUI 里菜单文案与"选档位跑一轮任务"的人工确认
- [ ] 可选（未做）：让 DSH 在"添加模型"时自动识别推理能力。**已查明这条在纯配置层面走不通**（2026-09-11 读 DSH 0.1.2-rc.1 源码）：
  ① 发现链路只搬运四个字段——`llm-pi-ai` 的 `readListing()` 只读 `id/name/context_window/max_output_tokens`，
  `discoverModels()` 对目录路由也只返回 `id/name/contextWindow/maxTokens`；② 客户端"采纳"候选时写死同样四个键
  （`dsh-client-ui-settings-models/lib/client.js` 的 `adopt()`：`{id, name?, contextWindow?, maxTokens?}`）；
  ③ 唯一能带 reasoning 元数据的来源是 pi-ai 自带目录（40 个 provider 的 `dist/providers/data/*.json` 里有
  `reasoning` 与 `thinkingLevelMap`），但那要求路由名与 model id 都命中目录——本网关的 `aigw`/`gpt-5.6-luna`/`deepseek-flash`
  都不在其中。所以"自动识别"要么改 DSH 自身，要么写 DSH 插件自带模型目录；两者都超出本仓库范围


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

## M22 多币种：模型级币种 + 账本换算 + 可选显示币种

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
- [ ] **待人工执行**（必须在自己终端里跑，DSH 沙箱启动的进程会被回收）：
  `scripts/local-run.sh restart` —— 让「默认展示币种 = CNY」与最终 M22 二进制生效
- [x] 注意：汇率在两处（config.yaml 文件 + settings 覆盖），**覆盖优先**；以后只在控制台改就地生效，
  想让文件成为唯一来源，就在「设置 → 汇率表」清空保存一次

### M22 实测与收尾
- [x] `make verify` 全绿（vet + test + build）；`internal/arch` 分层测试未新增包、无需改表
- [x] 控制台走查：`make ui-check`（headless firefox）新增 `currency` 视图 16 项断言（默认无 ≈、切 CNY 后 ≈ + 正确数值、缺汇率徽标、无页面错误）
- [x] 真实实例端到端：CNY 售价模型 → `usage_records.charge_micros == ceil(原生 × 汇率)`、快照含 `sale_currency`/`fx_sale_ledger`/原生金额、账本条目同额、`/v1/models` 报 CNY、`/billing/currency` 列表与缺汇率

## M23 请求日志默认只保留用户输入 + 配额口径收口

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

## M25 请求日志的写入兜底与保留期清理

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
- [ ] **待人工执行**（宿主终端）：`scripts/local-run.sh restart` —— 让 M24（列表分页）与 M25 一起生效
- [ ] **待人工执行（宿主终端，需停实例）**：VACUUM 回收空间。删行只把页还给 freelist，文件不会自己缩小：
  `scripts/local-run.sh stop` →
  `python3 -c "import sqlite3;c=sqlite3.connect('data/aigw-local.db');c.execute('VACUUM');c.close()"` →
  `scripts/local-run.sh start`（VACUUM 需要独占访问；本机没有 sqlite3 CLI，用 python 的 sqlite3 即可）
- [ ] 观察项：清理后若仍有 `put request log` 超时，先看 `/stats` 的 `request_log.dropped`——
  它是「连骨架行都没写进去」的权威计数，比翻日志可靠

## M26 请求路径的审计写入批量化（CPU 与尾延迟）

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
- [ ] 观察项：若 N 个请求对应的事务数接近 N，说明批没合上（看 `batch_writes` 是否被关、或并发太低 /
  间隔太短）——注意**低频逐个请求时 1:1 是正常的**（每个请求自己触发一次 flush）
- [ ] 观察项：`aigw_audit_queued_requests` 持续 >0 且不降说明写侧堵了；若同时
  `/stats` 的 `request_log.batching.backpressure` 在涨，就是队列满了在背压，需要调大
  `batch_queue_bytes` / `batch_max_bytes`，或排查请求体为何变得很大

## M27 请求日志的身份维度与消耗度量

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
- [ ] **待人工执行**（宿主终端）：重启 8088 实例让迁移 0008 生效，然后发一条 DSH 请求核对身份列
- [ ] 观察项：维度统计卡在窗口很大时的耗时（当前是窗口扫描 + usage 点查 join）；若 p95 超过 1 秒，
  按设计文档 §2.2 的同一形态补 workspace/call_kind 索引
- [ ] 观察项：`unknown` 桶的占比。持续偏高说明出现了新的客户端（或某个客户端改了提示词），
  需要在 `dimensions.go` 补一条结构性规则，而不是放宽全文匹配

## M28 控制台补齐「供应商模型」管理 + 映射写入改成部分更新

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
- [ ] **待人工执行**（宿主终端）：`scripts/local-run.sh restart` 让 8088 用上新二进制，
  然后在控制台验证「模型供应商 → codex → 详情 → 模型映射」能看到 luna 与 astra 两行、
  且不再出现「路由缺映射」告警
- [ ] 观察项：同类"静默排除"还有没有别处——候选过滤的其它原因（`not_granted` / `missing_capability` /
  `circuit_open`）在 `/v1/models` 里同样是无声丢弃，是否也要在控制台集中暴露

## M29 请求日志列表底部的本页汇总行（tokens 入/出 + 成本）

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
- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart` 后看
  http://127.0.0.1:8088/admin/ui/#/requests —— 控制台资源是 `//go:embed` 进二进制的，
  且 JS 带 `Cache-Control: public, max-age=300`，要硬刷新（Ctrl+Shift+R）才不会看到旧脚本
- [ ] 观察项：本页合计是否被误读成窗口合计。若确实有人这么读，按设计文档 §2.1 的路径补窗口级
  `summary`（真库实测 11 ms；未计量计数写成 `SUM(CASE WHEN u.request_id IS NULL …)` 可避开
  `COUNT(DISTINCT)` 的 temp B-tree），footer 机制不用改，只换数据源

## M30 请求日志的用户（账户）与 API Key 维度

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
- [ ] **待人工执行**（宿主终端）：`make build` + `scripts/local-run.sh restart`，让 8088 应用迁移
  0009 并载入新控制台资源，然后硬刷新（Ctrl+Shift+R）看 http://127.0.0.1:8088/admin/ui/#/requests
  ——确认两列/两个下拉/两个分组/详情里的用户与 Key，并发一条真实 DSH 请求核对显示的是名字
- [ ] 观察项：控制台下拉只列前 1000 个 Key（配置类列表的既有上限）——若出现超过该规模的部署，
  按设计文档 D2 的路径给 `/keys` 加分页搜索，而不是把名字落到日志行上

## M31 请求日志页「维度统计」卡片置顶 + 排序 + 分组列表分页

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

设计：`docs/design/m32-console-smart-chat.md`；使用者规格：`docs/chat.md`（均先行落盘，见 `docs/PROCESS.md`）。

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
- [ ] **待人工执行**（宿主终端）：硬刷新
      http://127.0.0.1:8088/admin/ui/#/chat ——在真实部署上用有余额的账户选模型提问，
      确认回答流式出现、图表与 HTML5 预览可打开、`console` 出现在请求日志
- [x] 修正（真机反馈）：步数与工具次数改为 **`0 = 不限制` 且默认 0**——一次提问一直进行到模型
      不再调用工具为止；配置成正整数时才在达到上限处停止并提示（`chat.max_steps` /
      `chat.max_tool_calls`，负数仍是校验错误）
- [x] 修正（真机反馈）：模型把端点名当工具名调用时（`admin_request_dimensions` →
      `unknown tool`），`chatTools.Call` 现在按 `admin_request` + `name=<端点>` 转发，
      判定仍走同一条允许清单；既不是工具也不是端点的名字回复「本网关提供哪三个工具」
- [ ] 观察项：不限制步数意味着一次点击的花费上限由模型的步数决定。若某个账户余额敏感，
      给它的部署设 `max_steps`/`max_tool_calls` 正整数上界
- [ ] 观察项：工具循环的端到端（模型真的发起 `admin_request`）目前由 `internal/chat` 与
      `internal/httpapi` 的测试用脚本化 runner/假上游覆盖——内建 `testecho` **不会**发起工具调用，
      真机验证需要接一个会调用工具的模型

### M33：智能问答改为 MCP 客户端（按令牌 scope 执行）

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
- [ ] **待人工执行**（宿主终端）：硬刷新
      http://127.0.0.1:8088/admin/ui/#/chat ——新建会话时选一个 `admin` scope 的令牌，
      在智能问答里建账户/发 Key，确认账户出现在账户页、明文 Key 只返回一次并带提示、
      `chat_tool_calls` 有记录、审计 actor 为 `mcp:<令牌名>#<id>`；再撤销该令牌，
      确认同一会话的下一次提问明确失效
- [ ] 观察项：明文凭据（API Key / MCP 令牌）现在会落进 `chat_tool_calls.result` 与会话记录。
      当前只在响应里追加一次性提示，不做脱敏——静默改写会让「已落盘」这一事实不可见。
      若日后要收紧，应做的是「返回后可选地从转录中清除」，而不是悄悄改内容

## M34 智能问答的可交互 HTML5 界面（表单 → 提交 → 模型继续）

设计：`docs/design/m34-ui-bridge.md`；规格：`docs/chat.md` §4（可交互的界面）、§8（配置）、§9（排障）。

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

起于一个实测结论：M34 的**流式回灌整条链路是死机制**——服务端注入脚本把增量写进页面的
`[data-aigw-live]` 节点，而这个标记在给模型的契约、文档与测试里**一处都没有**（全仓库仅有实现处
与它的自动 dump）。增量帧确实送达页面、然后被静默丢弃，harness 的断言只检查"`d` 帧到了端口"，
所以它一直是绿的。这是"能力存在、契约没写"的典型，M35 顺带把它变成一条机械化的检查。

方向经用户确认：**先只做内联声明式表单这一半，把 iframe 那条链路暂时冻结**。

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
- [ ] **待人工执行**（宿主终端）：用真模型要一次"需要几个字段的信息"（例如"帮我建一个账户，
      先问我账户名和限额"）→ 确认模型输出 `form` 而不是整页 HTML → 填写提交 → 确认表单显示
      「模型正在处理…」、状态行实时出现回答、结束后表单原地更新、会话里出现带 `ui_event` 的提问；
      再到「请求日志」确认这一条是 `client=console`、正文为空
- [ ] 观察项：内联表单与手动提问走同一条计费路径，所以表单上的连续提交会连续计费。一轮一次的
      禁用与 5 条排队是控制台侧的上界，不是服务端强制的
- [ ] 观察项：模型可能该用内联表单时仍输出整页 `html`（或反之）。契约里写了选择口径
      （"要几个字段就用表单；要自由排版或页面脚本才用 html"），但**模型是否照做只能人工走查**

## 修：`＋` 菜单技能行的勾选框被撑满整行 + 勾选技能后留空也能直接发送

现象（用户报）：智能问答输入框 `＋` 菜单里，技能勾选框的**文字离勾选框很远**；并且希望
**加载技能后文本留空也能直接发送**。

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
- [ ] 观察项：菜单勾选一个技能后会关闭（`setSkills` → 重载会话 → 重渲染），多选需要再点一次 `＋`。
      本次没改这个交互（不在用户要求内）；若要做"菜单内多选 + 一次运行"，需要把技能的 PATCH 做成
      乐观更新而不是整页重渲染

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

同一个客户端会话的连续请求，在加权随机下会在同一层的多个供应商之间来回跳：前缀缓存命中率被稀释，
上游账号侧看到的"一个会话"也被打散。M38 把"上一次真正服务成功的 route"与该会话绑定，
并明确一条边界：**粘性只重排已授权的候选，永远不放宽授权**。

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

- [x] 部署（2026-09-13）：gpt001 已换成 `d637209`（`make build` → `scp` → `install` → `systemctl restart aigw`），
      `readyz`/`healthz` 200、重启后 0 条 ERROR、公网 `mnl`/`gpt` 两个入口 200；
      启动日志 `strategy=weighted_random session_affinity=true affinity_ttl=30m0s affinity_max_entries=10000`；
      `/stats` 报 `version=d637209` 且 `affinity={entries,hits,...}` 随线上 DSH 会话增长。
      回滚点：`/opt/aigw/aigw.prev-20260913-154048`（旧版 `97ac621`），回滚 = `mv` 回去 + 重启。
      线上 `config.yaml` 没有 `routing:` 段，因此**靠默认值开启**（按用户决定不改配置）
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
- [ ] 线上配置清理：删掉 deepseek 供应商的 `response_format` 键（能力改由模型 `capabilities.json_object` 申报）

### 发布 skill

- [x] `scripts/release.sh`：`patch|minor|major` 升 `VERSION` → 提交 → 打 `v<a.b.c>` tag → `make build`；
      脏工作区、tag 已存在、非法档位一律拒绝
- [x] `.dsh/skills/release-version/SKILL.md`：完整发布流程（定档位 → 升版本 → 部署 gpt001 → 用 `/version` 与
      角标验证 → 回滚点 → 记录），含本次这条 `response_format` 坑的提示
- [ ] 首次发布：`0.1.0`（`VERSION` 新建），部署 gpt001 后 `/version` 与角标读到的都是 `0.1.0` + 新 revision

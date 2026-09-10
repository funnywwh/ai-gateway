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
- [ ] 后续工具：get_dashboard / get_usage_breakdown / list_invoices / get_invoice / get_rate_limits（依赖 M11/M12）
- [ ] bin/aigw mcp-serve（stdio 模式）
- [x] MCP-2：补齐 5 个工具（get_dashboard / get_usage_breakdown / get_rate_limits / list_invoices / get_invoice），共 11 个
- [x] MCP-2：聚合下沉到 SQL（不再受 max_query_rows 截断影响）+ TTFT P95 精度标注
- [x] MCP-2：`bin/aigw mcp-serve --account <name>` stdio 模式，复用同一个 Service（工具集不会漂移）
- [x] MCP-2：测试 5 个（聚合不受行数限制、分组、限额+账单、跨账户不可见、tools/list 覆盖新工具）+ 端到端实测（HTTP 11 工具、stdio 2 响应）

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
- [ ] 管理面请求日志查询 API（M8；MCP 侧已提供账户作用域查询）

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
- [ ] 浏览器人工走查（当前环境无浏览器）

## M11a 计价引擎
- [x] 设计文档 docs/design/m11a-pricing.md（已在对话中输出）；规格 docs/pricing.md 状态改为已实现
- [x] internal/pricing：纯函数 Evaluate（成本侧/售价侧同一条路径），int64 + ceil，禁浮点
- [x] 有序规则集：首命中、catch-all 强制、时段（含跨午夜与星期归属、内嵌 tzdata）、档位半开、valid_from/to、变体
- [x] 成本/售价双规则集：cost_follow（含按维度覆写倍率）| absolute，互斥语义明确
- [x] 校验（400）+ 遮蔽检测（告警，不拒绝）；快照内联命中规则完整副本，可脱离规则表复算
- [x] 管理面：POST /pricing/simulate（可内联规则做「改了会怎样」预览）与 POST /pricing/validate
- [x] 测试：13 个引擎用例（首命中/时段/跨午夜/档位/取整/倍率/最低收费/校验/遮蔽/快照复算）+ 2 个 HTTP 用例
- [ ] 界面：Pricing 页面（规则表格编辑器、模板、阶梯预览）——依赖 M11b 的账本与真实用量展示

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
- [x] `examples/` 与 `pkg/` 同样受检（插件作者代码不得依赖 internal）
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
- [ ] v2：对象存储/异地同步（规格已声明不在 v1 范围）

## 可选
- [ ] M10 订阅后端参考适配器（examples/provider-codex，默认禁用）
- [ ] M14 客户自服务门户

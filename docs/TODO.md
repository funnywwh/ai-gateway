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
- [ ] /mcp（Streamable HTTP）+ mcp_tokens 鉴权 + 账户强作用域
- [ ] 10 个只读工具（含 list_requests / get_request）
- [ ] bin/aigw mcp-serve（stdio）

## M7 Hooks 与录制
- [ ] 异步 worker 池 + 有界队列 + HMAC 签名 + 重试 + 死信
- [ ] 请求日志查询 API

## M8 管理面 REST API
- [ ] 会话鉴权（argon2id + 会话 Cookie）
- [ ] 供应商端点（kinds/preview/validate/test/restart/rollback/logs/refresh/actions/state）
- [ ] model-mappings / mcp-tokens / keys（双勾选）/ audit

## M9 Web 管理界面
- [ ] 概览/Keys/Tags/Providers/Model Mappings/Models & Routes/Pricing/Accounts/Invoices/Reconciliation/MCP/Hooks/Request Logs/Usage/Backups/Audit/Settings

## M11a 计价引擎
- [ ] 计量维度 + 有序价格规则集（catch-all 强制、遮蔽检测、时段/档位/{model}）
- [ ] 成本/售价双规则集（cost_follow | absolute）+ 快照内联副本
- [ ] 试算器 + 定价诊断 + Pricing 界面

## M11b 账本与在途额度
- [ ] 单写者批处理 + fsync 兜底 + 幂等键
- [ ] 预付/后付、预留、在途策略、中断超支吸收、TTL/心跳
- [ ] rebuild-ledger 全量重放 + 四条不变量巡检

## M12 账单 / 充值 / 对账补偿
- [ ] invoice_lines 物化 + 状态流转 + 导出
- [ ] credits / 赠送到期 / 兑换码
- [ ] 每日对账 + 差异告警 + 失败重放

## M13 性能与并发
- [ ] scripts/load.sh + soak + pprof
- [ ] 基线：≥2000 rps、TTFT P95<150ms、结算 ≥5000 条/s

## M15 模块解耦验证
- [ ] 接口替身替换测试 + go list 依赖方向检查

## M16 数据库自动备份
- [ ] 一致点快照（wal_checkpoint + 排空 writer）+ quick_check + 保留策略
- [ ] /backups API + UI + 恢复流程

## 可选
- [ ] M10 订阅后端参考适配器（examples/provider-codex，默认禁用）
- [ ] M14 客户自服务门户

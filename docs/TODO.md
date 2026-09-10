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
- [ ] 鉴权与授权并集（key + tags）
- [ ] 候选过滤（能力/degradation/draining/冷却）+ 优先级分层 + 4 种 LB 策略 + 熔断
- [ ] 模型自由映射（exact/prefix/glob/regex、{model} 占位符、钉死供应商、兜底、跨模型校验）
- [ ] Explain（解析链路 + 排除原因）

## M4 鉴权 / 限速 / 计量
- [ ] API Key 哈希校验与缓存（TTL 30s）
- [ ] 分片滑动窗口限速（rpm/tpm/并发）
- [ ] usage_records（attempt 粒度、分维度、overshoot）

## M5 Responses API
- [ ] POST /v1/responses（非流式 + SSE 全事件序列、sequence_number）
- [ ] GET/DELETE /v1/responses/{id}、previous_response_id 续接
- [ ] GET /v1/models（仅对客售价）
- [ ] 输入/思考/最终输出分离录制（默认：输入 full、思考与最终输出 off）

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

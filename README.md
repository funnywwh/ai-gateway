# ai-gateway

类 OpenRouter 的 AI 网关：对外提供 OpenAI **Responses API** 兼容接口，对内把模型路由到多个供应商，
供应商以 **Go 插件（独立子进程 + stdio JSON 帧）** 提供，支持 API Key 分发与标签授权、完整计量与计费、
MCP 查询与后台操作、内容录制与 hooks、模型自由映射、数据库自动备份。

## 状态

计划中的里程碑 M0–M16 已全部落地并通过验证（M10 订阅后端参考适配器、M14 客户自助门户为计划内的可选项，未做）。
M17 完善了内置 `openai-chat` 供应商（思考模式、思考内容续接、DeepSeek 适配），不引入新插件。
M18 让控制台把「每个供应商类型支持哪些配置、密钥填哪一栏」直接讲清楚（内建 kind 自带字段说明，插件走 handshake），
并为无 node 环境补上真实浏览器里的控制台走查（`make ui-check`）。
M21 让 MCP 从"只读查询"变成"可执行全部后台接口"：令牌分 query / admin_read / admin 三档 scope，
agent 通过 admin_endpoints → admin_describe → admin_request 三个入口（渐进披露）驱动整张管理面。
M25 给请求日志补上写入兜底与保留期：内容写失败时退化为「无正文骨架行」（请求仍可见，失败/丢弃计数在
`/stats` 与 `/metrics`），`recording.retention_days` 从死配置变成真正的每日清理（手动端点
`POST /admin/api/v1/requests/prune`），存储响应与它同一个窗口到期。
M26 把审计写入搬出请求路径：存储响应与请求日志由后台线程成批提交（默认 250 ms / 256 请求一个事务），
写路径复用预编译语句，队列有界并在满时施加背压。实测在 8088 上每请求 CPU 2.57ms → 0.82ms、
p90 480ms → 17ms；旧实例上「1.2 MB 请求写失败退骨架行」的那类丢正文，修复后 42/42 完整落库。
M23 把「请求日志只留用户输入」定为默认口径：输入通道分 full / user / metadata / off 四档，默认 `user`
只保留用户自己写的输入（系统指令、工具定义与工具输出只留计数），思考与最终输出仍默认不落；同时把配额
策略的口径收敛成一个扁平形状并在写入时校验，`monthly_*` 如实标注为「已解析、未执行」。
M27 给请求日志补上身份维度与消耗度量：客户端（dsh/codex）、模型（请求名与路由名两个身份）、
工作区、会话、调用类型与会话标题在请求解析后立即提取并落列——与正文口径无关（`record_input=off` 也记），
因此不必再翻那份会截断、会过期的正文；token 与成本按 request_id 从计量表关联，与账单同源；
控制台可按这些维度筛选，并有 top-N 维度统计卡，三条 `(维度, created_at, id)` 索引让筛选查询
在 EXPLAIN 下仍无排序器（实测插入页写放大 1.13×）。
当前规模：约 3.5 万行 Go + 原生前端；24 个测试包、1 个分层断言包；真实二进制端到端走查覆盖数据面、计费、MCP、备份与控制台。

## 文档

流程约定：每个里程碑**先写设计文档并在对话中输出确认**，面向使用者的规格文档**同样先行**（spec first），
详见 `docs/PROCESS.md`。

| 文档 | 内容 | 状态 |
|---|---|---|
| `docs/PROCESS.md` | 实现流程约定（设计文档 / todo / 提交自检） | 生效中 |
| `docs/TODO.md` | 里程碑 → 任务 → 验收 的检查清单（随实现勾选） | 持续更新 |
| `docs/architecture.md` | 分层、模块边界与可替换扩展点 | 已落地（由 `internal/arch` 断言守护） |
| `docs/design/` | 每个里程碑的设计文档（接口、数据流、决策、异常、测试策略、实现差异） | M0–M27 全部产出 |
| `docs/plugin-protocol-v1.md` | 插件协议 v1（帧/方法/事件/取消/背压/错误分类） | **已实现（M2）** |
| `docs/api-responses.md` | Responses 兼容面（端点/字段/SSE 事件/错误封装/认证与限速） | **已实现（M5）** |
| `docs/api-providers.md` | openai-chat 供应商（配置开关、思考模式、用量维度、错误分类、DeepSeek 接入） | **已实现（M17）** |
| `docs/provider-ui.md` | 控制台如何展示供应商配置说明（字段语义、配置与凭据两条通道、密钥录入路径与优先级、排障） | **已实现（M18）** |
| `docs/routing.md` | 路由解析、候选过滤、负载均衡策略、熔断与冷却 | **已实现（M3）** |
| `docs/pricing.md` | 计量维度 × 有序价格规则集（分时/分档/分维度）；多币种（模型级币种 + 汇率换算） | **已实现（M11a / M22）** |
| `docs/billing.md` | 计量、账本、在途额度、账单、充值、对账、赠送到期；账本币种与显示币种 | **已实现（M11/M12/M22）** |
| `docs/mcp.md` | MCP 服务（11 个查询工具 + 3 个后台工具、令牌 scope、渐进披露、审计、内容可见性、stdio） | **已实现（M6 + MCP-2 + M21）** |
| `docs/backup.md` | 一致点备份、保留、校验与两阶段恢复 | **已实现（M16）** |
| `docs/request-log.md` | 请求日志：录制通道矩阵、身份维度（客户端/模型/工作区/会话/标题）、消耗口径、查询与统计、保留期与写入兜底 | **已实现（M27）** |

## 构建

本仓库构建必须使用工作区本地工具链环境（`$HOME` 只读、无 CGO）：

```sh
source scripts/goenv.sh
make build      # 产出 bin/aigw
make test       # 单元测试
make verify     # vet + test + build
```

## 运行

```sh
./bin/aigw --version
./bin/aigw --config config.yaml
```

配置项见 `config.example.yaml`；管理控制台在 `http://<listen>/admin/ui/`（首次用 bootstrap 里的管理员账号登录）。

## 常用入口

| 命令 / 路径 | 用途 |
|---|---|
| `POST /v1/responses` | Responses API（流式 / 非流式） |
| `GET /v1/models` | 可用模型与**对客售价** |
| `POST /mcp` | MCP 服务（账户查询 + 按 scope 可执行后台接口，`aigw_mcp_` 令牌） |
| `bin/aigw mcp-serve --account <name>` | 本地 stdio MCP（复用同一套工具） |
| `/admin/api/v1/*` | 管理面：账户/Key/标签/供应商/模型/路由/映射/定价/账单/充值/对账/备份/审计 |
| `/admin/ui/*` | 内置控制台（零构建，随二进制发布） |
| `scripts/load.sh` | 一键压测（自建 `cmd/loadgen`，输出 rps 与分位延迟） |
| `scripts/deepseek-smoke.sh` | DeepSeek 接入走查（离线假上游；加 `--live` 与 `DEEPSEEK_API_KEY` 打真机） |
| `scripts/ui-harness/run.sh`（`make ui-check`） | 控制台走查：API 快照 + headless firefox 渲染真实页面并断言（无 node 环境下的 UI 验证手段） |
| `make verify` | vet + 全量测试 + 构建 |

## 压测基线（本机 i7-12700K，testecho 供应商）

```
32 并发 × 10s：619.6 rps，p50 1.73ms，p95 44.5ms，p99 1.73s
结算：usage 行与账本 charge 行逐条对齐，四条不变量 ok，无兜底文件
微基准：pricing.Evaluate 4.2µs，routing.Plan 4.7µs（并行 4.0µs）
```

细节与两处压测发现的缺陷见 `docs/design/m13-performance.md`。

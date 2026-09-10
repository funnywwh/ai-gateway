# ai-gateway

类 OpenRouter 的 AI 网关：对外提供 OpenAI **Responses API** 兼容接口，对内把模型路由到多个供应商，
供应商以 **Go 插件（独立子进程 + stdio JSON 帧）** 提供，支持 API Key 分发与标签授权、完整计量与计费、
MCP 只读查询、内容录制与 hooks、模型自由映射、数据库自动备份。

## 状态

计划中的里程碑 M0–M16 已全部落地并通过验证（M10 订阅后端参考适配器、M14 客户自助门户为计划内的可选项，未做）。
当前规模：约 3.35 万行 Go + 原生前端；24 个测试包、1 个分层断言包；真实二进制端到端走查覆盖数据面、计费、MCP、备份与控制台。

## 文档

流程约定：每个里程碑**先写设计文档并在对话中输出确认**，面向使用者的规格文档**同样先行**（spec first），
详见 `docs/PROCESS.md`。

| 文档 | 内容 | 状态 |
|---|---|---|
| `docs/PROCESS.md` | 实现流程约定（设计文档 / todo / 提交自检） | 生效中 |
| `docs/TODO.md` | 里程碑 → 任务 → 验收 的检查清单（随实现勾选） | 持续更新 |
| `docs/architecture.md` | 分层、模块边界与可替换扩展点 | 已落地（由 `internal/arch` 断言守护） |
| `docs/design/` | 每个里程碑的设计文档（接口、数据流、决策、异常、测试策略、实现差异） | M0–M16 全部产出 |
| `docs/plugin-protocol-v1.md` | 插件协议 v1（帧/方法/事件/取消/背压/错误分类） | **已实现（M2）** |
| `docs/api-responses.md` | Responses 兼容面（端点/字段/SSE 事件/错误封装/认证与限速） | **已实现（M5）** |
| `docs/routing.md` | 路由解析、候选过滤、负载均衡策略、熔断与冷却 | **已实现（M3）** |
| `docs/pricing.md` | 计量维度 × 有序价格规则集（分时/分档/分维度） | **已实现（M11a）** |
| `docs/billing.md` | 计量、账本、在途额度、账单、充值、对账、赠送到期 | **已实现（M11/M12）** |
| `docs/mcp.md` | MCP 只读查询服务（11 个工具、作用域、内容可见性、stdio） | **已实现（M6 + MCP-2）** |
| `docs/backup.md` | 一致点备份、保留、校验与两阶段恢复 | **已实现（M16）** |

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
| `POST /mcp` | MCP 只读查询（账户级 `aigw_mcp_` 令牌） |
| `bin/aigw mcp-serve --account <name>` | 本地 stdio MCP（复用同一套工具） |
| `/admin/api/v1/*` | 管理面：账户/Key/标签/供应商/模型/路由/映射/定价/账单/充值/对账/备份/审计 |
| `/admin/ui/*` | 内置控制台（零构建，随二进制发布） |
| `scripts/load.sh` | 一键压测（自建 `cmd/loadgen`，输出 rps 与分位延迟） |
| `make verify` | vet + 全量测试 + 构建 |

## 压测基线（本机 i7-12700K，testecho 供应商）

```
32 并发 × 10s：619.6 rps，p50 1.73ms，p95 44.5ms，p99 1.73s
结算：usage 行与账本 charge 行逐条对齐，四条不变量 ok，无兜底文件
微基准：pricing.Evaluate 4.2µs，routing.Plan 4.7µs（并行 4.0µs）
```

细节与两处压测发现的缺陷见 `docs/design/m13-performance.md`。

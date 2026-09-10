# ai-gateway

类 OpenRouter 的 AI 网关：对外提供 OpenAI **Responses API** 兼容接口，对内把模型路由到多个供应商，
供应商以 **Go 插件（独立子进程 + stdio JSON 帧）** 提供，支持 API Key 分发与标签授权、完整计量与计费、
MCP 只读查询、内容录制与 hooks、模型自由映射、数据库自动备份。

## 文档

流程约定：每个里程碑**先写设计文档并在对话中输出确认**，面向使用者的规格文档**同样先行**（spec first），
详见 `docs/PROCESS.md`。

| 文档 | 内容 | 状态 |
|---|---|---|
| `docs/PROCESS.md` | 实现流程约定（设计文档 / todo / 提交自检） | 生效中 |
| `docs/TODO.md` | 里程碑 → 任务 → 验收 的检查清单（随实现勾选） | 持续更新 |
| `docs/design/` | 每个里程碑的设计文档（接口、数据流、决策、异常、测试策略） | M0–M2 已产出 |
| `docs/architecture.md` | 分层、模块边界与可替换扩展点 | M0–M1 已落地，后续补齐 |
| `docs/plugin-protocol-v1.md` | 插件协议 v1（帧/方法/事件/取消/背压/错误分类） | **已实现（M2）** |
| `docs/api-responses.md` | Responses 兼容面（端点/字段/SSE 事件/错误封装） | 规格（M5 实现） |
| `docs/billing.md` | 计量、账本、在途额度、账单、充值、对账 | 规格（M11/M12 实现） |
| `docs/pricing.md` | 计量维度 × 有序价格规则集（分时/分档/分维度） | 规格（M11a 实现） |
| `docs/mcp.md` | MCP 只读查询服务（工具、作用域、内容可见性） | 规格（M6 实现） |
| `docs/backup.md` | 一致点备份、保留、校验与恢复流程 | 规格（M16 实现） |

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

配置项见 `config.example.yaml`（M1 提供）。

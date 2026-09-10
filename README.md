# ai-gateway

类 OpenRouter 的 AI 网关：对外提供 OpenAI **Responses API** 兼容接口，对内把模型路由到多个供应商，
供应商以 **Go 插件（独立子进程 + stdio JSON 帧）** 提供，支持 API Key 分发与标签授权、完整计量与计费、
MCP 只读查询、内容录制与 hooks、模型自由映射、数据库自动备份。

## 文档

| 文档 | 内容 |
|---|---|
| `docs/design/` | 每个里程碑的设计文档（接口、数据流、决策、异常、测试策略） |
| `docs/TODO.md` | 里程碑 → 任务 → 验收 的检查清单（随实现勾选） |
| `docs/architecture.md` | 分层与模块边界（M15 完成后补齐） |
| `docs/plugin-protocol-v1.md` | 插件协议（M2） |
| `docs/api-responses.md` | Responses 兼容面（M5） |
| `docs/billing.md` / `docs/pricing.md` | 计费口径与分时/分档定价（M11/M12） |
| `docs/mcp.md` | MCP 查询服务（M6） |
| `docs/backup.md` | 备份与恢复（M16） |

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

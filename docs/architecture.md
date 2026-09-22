# 架构与模块边界

> 状态：**已实现部分（M0–M1）+ 目标结构**。分层规则自 M0 起生效，模块逐步填充。

## 分层（依赖单向，内层不依赖外层）

```
            ┌──────────────────────────────────────────────┐
 最外层     │ cmd/aigw            (composition root)        │  唯一装配点
            ├──────────────────────────────────────────────┤
            │ internal/httpapi    internal/web             │  编排 / 静态资源
            ├──────────────────────────────────────────────┤
            │ internal/pluginhost internal/mcpsrv          │  进程与协议适配
            │ internal/billing/*  internal/hook            │
            │ internal/retention  internal/quota           │
            ├──────────────────────────────────────────────┤
            │ internal/routing    internal/balancer        │  纯函数
            │ internal/registry                            │  不可变快照
            ├──────────────────────────────────────────────┤
            │ internal/store                               │  持久化实现
            ├──────────────────────────────────────────────┤
 最内层     │ internal/domain     (entities + ports)       │  零依赖
            └──────────────────────────────────────────────┘
                     pkg/pluginapi  pkg/providerkit         对外契约（可被插件引用）
```

规则：

1. **`internal/domain` 是依赖根**：只有纯类型与接口，不 import 任何其它 internal 包。
2. **只依赖接口**：模块通过构造函数注入 `domain.Xxx` 接口（`New(deps...)`），
   不在模块之间 import 具体实现。
3. **禁止反向依赖**：`store` 不 import `httpapi`；`routing` 不 import `store`。
   用 `go list -deps` 做静态检查（见 M15）。
4. **公开 SDK 只在 `pkg/`**：`pkg/pluginapi`（插件作者契约，协议版本握手 + 语义化版本）、
   `pkg/providerkit`（适配器工具）。`internal/` 一律不对外。
5. **唯一装配点** `cmd/aigw`：手动构造注入，不引入 DI 框架。

## 当前包清单（M0–M1 已落地）

| 包 | 职责 | 状态 |
|---|---|---|
| `internal/domain` | 实体、端口接口、OpenAI 兼容 `APIError` | 已实现 |
| `internal/config` | YAML + `GW_*` 环境覆盖 + 默认值 + 校验 | 已实现 |
| `internal/logx` | `log/slog` 封装（text/json） | 已实现 |
| `internal/ids` | 前缀 ID（`resp_`/'msg_'/...） | 已实现 |
| `internal/secret` | SHA-256 哈希、前缀提取、常量时间比较 | 已实现 |
| `internal/store` | SQLite（迁移、DAL、引导、审计、设置） | 已实现 |
| `internal/registry` | 不可变快照 + 原子换入（含账号侧标签预计算，热路径 0 分配） | 已实现 |
| `internal/orgtree` | 组织树的纯算法（祖先链/子孙/环与深度校验/继承标签名），只依赖 `domain` | M49 |
| `internal/pluginhost` | 插件进程生命周期 | M2 |
| `pkg/pluginapi` | 插件协议 SDK | M2 |
| `pkg/providerkit` | SSE 解析、chat↔responses 转换、估算 | M2 |
| `internal/routing`/`balancer` | 模型解析、候选选择、LB 策略 | M3 |
| `internal/apikey`/`quota` | 鉴权缓存、分片限速 | M4 |
| `internal/httpapi` | Responses/MCP/管理面 HTTP（含 MCP 后台工具桥） | M5/M6/M8/M21 |
| `internal/billing/*` | 计价、账本、在途、账单、对账、充值 | M11/M12 |
| `internal/backup` | 一致点备份、保留、恢复 | M16 |
| `internal/retention` | 录制数据（请求日志/存储响应）的保留期清理（叶子包，端口注入） | M25 |
| `internal/webaccess` | 控制台联网：搜索后端适配（searxng/bocha/tavily/bing）、网页正文抽取、SSRF 防护（叶子包，零内部依赖） | M73 |

## 可替换扩展点（SPI）

| 扩展点 | 当前实现 | 可替换为 |
|---|---|---|
| Store 后端 | SQLite（`modernc.org/sqlite`） | Postgres（v2 多副本） |
| Provider 传输 | 子进程 stdio NDJSON | gRPC / HTTP 回调 |
| 内置供应商 | `openai-responses`、`openai-chat`、`testecho` | 任意插件 |
| Hook 投递 | webhook（HMAC）+ JSONL | MQ / 对象存储 |
| LB 策略 | 加权随机/轮询/最少连接/最低延迟/严格顺序 | 注册表可加自定义策略 |
| 认证 | API Key（哈希 + 前缀索引） | OIDC / JWT |
| MCP 工具 | 11 个账户查询工具 + 3 个后台入口（渐进披露） | 管理面路由表驱动：新增接口自动出现在目录里 |
| 备份后端 | 本地目录 | 对象存储（v2） |

## 并发与性能要点（详见各模块设计文档）

- 热路径零 DB：鉴权、路由、限速、预留、模型映射全在内存快照上完成。
- 供应商容量闸门（M44）：每个供应商的同时在途上限（`providers.max_inflight`）在 `internal/runtime` 内
  用进程内 FIFO 名额实现，超限请求**排队等待**而不是失败（等待上限与队列深度来自 `routing.*` 配置）；
  排队中的请求不计入 `least_inflight` 与上游延迟 EWMA，排队时长也不计入 `latency_ms`；
  探测与后台动作不占名额。详见 `docs/routing.md` §4.5。
- 结算写：单写者批处理 + `fsync` 兜底文件 + 幂等键。
- 审计写（存储响应 + 请求日志）：同样单写者批处理，后台一个事务带多请求
  （M26，`recording.batch_writes`）；队列有界，满时对请求路径施加背压而不是无限堆积。
- 读写连接分离：`SetMaxOpenConns(1)` 写 + WAL 并发读。
- 写路径复用预编译语句（纯 Go 驱动每次 Exec 都会重跑 SQL 解析器）。
- 录制与 hook 异步有界队列，溢出丢弃并计数，绝不阻塞请求。

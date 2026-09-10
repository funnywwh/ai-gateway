# M15 设计：模块解耦验证

> 目的：把「分层」从口头约定变成**可执行的断言**，任何人加一行错误的 import 都会让测试红。

## 1. 规则从哪来

依赖方向来自 `docs/architecture.md`，落地成一张**允许边**的表；测试读取 `go list` 的真实依赖图并逐条比对。
约定：规则里写的是**允许**的模块内依赖，标准库与第三方包不参与判断（只有 `pkg/` 与 `internal/` 之间的边被检查）。

## 2. 允许边（模块内）

| 包 | 允许依赖（模块内） | 理由 |
| --- | --- | --- |
| `internal/domain` | 无 | 零依赖的类型与端口层 |
| `internal/{ids,secret,logx,balancer,pricing}` | 无 | 纯工具/纯算法 |
| `internal/config` | logx | 只依赖日志 |
| `internal/store` | domain, config, secret | 持久化不允许知道 HTTP/路由 |
| `internal/registry` | domain | 快照只认领域类型 |
| `internal/{modelmap,routing}` | balancer, domain, modelmap, registry | 路由不得依赖 store/httpapi |
| `internal/quota` | domain | 限速是纯内存算法 |
| `internal/{responses,usage}` | domain, ids, pluginapi | 面向协议与用量 |
| `internal/hook` | domain, ids, logx | 投递器不认识业务细节 |
| `internal/mcpsrv` | domain, registry | 只读查询 |
| `internal/backup` | domain | 备份只认库文件与领域类型 |
| `internal/webui` | 无 | 纯静态资源 |
| `pkg/pluginapi` | 无 | 协议层，禁止反向依赖 internal |
| `pkg/providerkit` | pluginapi | 同上 |
| `internal/providers/*` | pluginapi, providerkit | 内建供应商不认识网关内部 |
| `internal/pluginhost` | pluginapi | 进程宿主 |
| `internal/creds` | 无 | 加解密不依赖任何业务包 |
| `internal/runtime` | balancer, creds, domain, pluginhost, providers, registry, pluginapi | **唯一**允许同时碰插件宿主与凭据的执行层 |
| `internal/billing` | domain, ids, pricing, secret, store | 计费需要一个窄的存储端口 |
| `internal/httpapi` | 见下 | 传输层可以组合，但不得直接碰插件宿主与凭据 |
| `cmd/aigw` | 任意 | 唯一的组合根 |

## 3. 三条关键禁令（真正的解耦断言）

1. **`internal/httpapi` 不得 import `internal/pluginhost` 与 `internal/creds`**：
   传输层只能通过 `Prober`/`Sealer` 端口触达它们——这正是 M8b/M9 重构的成果，必须锁住；
2. **只有 `cmd/aigw` 可以同时 import `internal/store` 与 `internal/httpapi`**：
   否则存储细节会顺着 import 渗进 HTTP 层；
3. **任何包都不得 import `cmd/`**，且 `internal/` 与 `pkg/` 之间只有 `pkg → internal` 之外的合法方向，
   即 `internal` 可以 import `pkg`，`pkg` 不能 import `internal`。

## 4. 实现方式

- 测试位于 `internal/arch/layering_test.go`（`package arch_test`），
  用 `go list -f '{{.ImportPath}}|{{join .Imports " "}}' ./...` 取真实图；
- 规则表写在测试里，失败信息必须指出「哪个包、导入了谁、违反哪条规则、允许什么」；
- 环境没有 go 工具链时 `t.Skip`，避免在纯运行环境里误报。

## 5. 接口替身（第二条断言）

逐一验证「换掉实现，上层不受影响」：`billing.Service` 用假 store 跑通；
`httpapi` 的 `ProviderAdmin`/`Sealer`/`Prober`/`Backups` 等端口在测试里是可替换的假实现（M8b/M16 已覆盖）。
本轮补一条：`billing.ServiceStore` 的部分方法用**代理结构体**替换，证明计费不依赖 `*store.DB` 的具体行为。

## 6. 实现与设计差异

1. **规则表第一次跑就抓到 5 处「我以为是 A、实际是 B」**：`internal/pricing` 依赖 `internal/domain`、
   `internal/providers` 依赖三个子包、`examples/provider-replay` 与 `internal/arch` 未登记。
   这正是把口径变成断言的价值——口头分层永远对不上真实 import。
2. **`examples/` 也纳入检查**：示例插件是「发布给插件作者」的代码，只允许 `pkg/pluginapi` 与 `pkg/providerkit`，
   一旦有人让它 import `internal/*`，测试立刻失败。
3. **依赖图用 `go list -f '{{.ImportPath}}|{{join .Imports " "}}' ./...` 现取**，不维护第二份清单；
   没有 go 工具链时 `t.Skip`。
4. **额外加了三条结构性断言**：任何包不得 import `cmd/`；除 `cmd/aigw` 外不得同时 import `store` 与 `httpapi`；
   关键包（domain/store/routing/httpapi/billing/backup/webui/pluginapi/cmd）必须存在。
5. **分层断言又抓到一次真实回退**（MCP-2 期间）：`internal/mcpsrv` 为了用聚合类型 import 了 `internal/store`，
   测试立刻红。修法不是放宽规则，而是把 `UsageTotals`/`UsageBreakdownRow`/`UsageCounter` 三个类型上移到 `internal/domain`，
   由 store 提供别名——**第三次出现同一模式**（`BackupJob`、这次的三个聚合类型）：凡是被「非持久化层」消费的数据形状，
   都应定义在无依赖的领域包里。
6. **接口替身测试**：`billing` 用 `proxyStore`（内嵌端口、转发并计数）替换 `*store.DB`，
   证明 `Grant`/`Balance` 只经过端口方法；httpapi 侧的可替换端口（`ProviderAdmin`/`Sealer`/`Prober`/`Backups`）
   已由 M8b/M16 的假实现覆盖。
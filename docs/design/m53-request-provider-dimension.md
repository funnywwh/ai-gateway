# M53 请求日志的供应商维度（按供应商统计成本）

## 0. 起因与结论

用户原话：**「修复同样的模型，不同的供应商，没有办法按供应商统计成本」**。

现状核查（结论：数据面是对的，缺的是统计口径）：

- 写路径已经按供应商归属：每次上游尝试的 `usage_records.provider_id` 记录实际服务的那家，
  成本规则也从**该供应商的映射**取（`internal/httpapi/billing_path.go` 的 `ruleSetsFor(canonical, providerID)`），
  所以同一模型挂两家供应商时，两家的 `cost_micros` 本来就是各按各的价算好的。
- 读路径没有供应商维度：`store.RequestLogDimensionNames` 只有
  `client/model/resolved_model/workspace/session/call_kind/account/api_key`；
  日志行没有 provider 列；小时汇总 `request_dimension_rollups` 也没有。
  于是「同一个 `model` 桶花了多少」只能给出两家之和，答不出「哪家贵」。

结论：**加一个 `provider` 分组维度**，按计量行归属成本；并按 M30 的先例补齐列表列、详情、
筛选、控制台与 MCP。本次用户明确要求「选项1 + MCP 也要能按供应商统计」。

## 1. 为什么不能像 M30 那样做成日志列

M30 的 `account_id`/`api_key_id` 能落在 `request_logs` 上，是因为**一个请求只有一份凭据**。
供应商不是这样：

- 一个请求可以有多次上游尝试（M38 的故障转移），落在**不同**供应商上；
- 同一模型的多个供应商各自计价，失败那次的成本也是一家的真实成本。

所以「供应商」是**一对多**的事实，只能来自 `usage_records`（那里本来就是一行一次尝试）。
把它压成一个 `request_logs.provider_id` 列，无论取「第一家」「最后一家」还是「成功那家」，
都会把一家的成本搬到另一家头上。

## 2. 口径：谁服务的算谁的（已与用户确认）

`group_by=provider` 的贡献粒度是 **(request, provider)**，不是 request：

- 成本、charge、token 按 `provider_id` 归到实际执行那次尝试的供应商；
- 桶的「请求数」= **该供应商服务过的请求数**（`COUNT(DISTINCT request_id)`）；
- 因此**各桶请求数之和可能大于窗口总请求数**：一次失败转移的请求在两家各计一次。

这是刻意的：另一个选项（按「最终成功的供应商」归属整个请求）能让请求数加得起来，但会把失败
那家的成本记到成功那家名下，而「同一模型不同供应商的成本差异」正是这张表要回答的问题。
改数字让总和好看，等于让成本串味。

口径必须在用户能看到的地方写明，否则「一组比总数还大」看起来就是缺陷。已写在三处：
控制台「请求数」表头 tooltip 与统计卡说明、`group_by` 参数描述、MCP 工具描述（`Summary`）。

未计量请求（本地拒绝，从未到达上游）归入 `provider_id=0` 的桶，`key` 为空串、`provider_name` 为空，
控制台显示「（未知）」。丢掉它会让供应商视角与请求日志对同一窗口的请求数各说一套。

## 3. 汇总表在这条维度上用不了

`request_dimension_rollups` 是**按请求**预聚合的：一行一个请求，计量已在多次尝试上求和。
供应商信息在那里**已经不存在**——不是「没建索引」，而是被聚合掉了。所以：

- `group_by=provider` 恒读原始尝试粒度源（`providerDimensionSource`），扫窗口的 `request_logs`
  （时间索引）LEFT JOIN `usage_records`（`idx_usage_request` 点查），`GROUP BY r.id, COALESCE(u.provider_id,0)`；
- 带 `provider_id` 过滤时同理走原始源：汇总表只有日志列，没有 `request_id` 可供关联计量行，
  一小时无法被过滤就不能用来回答一次过滤查询。

`selectDimensionSources` 因此多了 `exact` 参数：为真时整窗口作为一个 raw 区间返回，汇总表一步不碰
（`dimensionReadIsExact = providerDimension(groupBy) || f.ProviderID > 0`）。

**代价**：这两种读比走汇总慢（窗口扫描）。换来的是不会给出「已完成的小时里一个供应商都没有」
这种看似正常的错答案。M27 留下的观察项（大窗口下维度统计耗时）在这里同样适用。

页面/总数的一致性按既有规矩：行与 `total` 在**同一个只读快照**里、由**同一个源**算出
（`RequestLogDimensionsPage` 决定一次 `exact`/`byProvider` 后供两次查询使用）。

## 4. 筛选是请求级，不是桶级

`provider_id` 过滤的语义与列表一致：**「至少有一次计量行落在这家的请求」**，
实现为相关子查询 `EXISTS (SELECT 1 FROM usage_records pu WHERE pu.request_id = <外层>.request_id AND pu.provider_id = ?)`。

两个实现细节值得记下来：

- **外层列必须带表名**。子查询里裸写 `request_id` 会先解析到内层 `pu.request_id`，
  于是 `pu.request_id = request_id` 变成恒真，过滤悄悄失效（SQLite 不报歧义）。
  单表调用方（列表与计数）因此显式写 `request_logs.request_id`。
- **筛选选请求、分组分钱**：`group_by=provider&provider_id=N` 是「N 服务过的那些请求，再按供应商拆开」，
  所以结果里既会有 N 自己的桶，也会有这些请求在别家留下的尝试。这与「同一批请求在列表、总数、
  统计三处口径一致」的既有约定一致（M30/M31）。

## 5. 改动面

| 层 | 改动 |
|---|---|
| `internal/domain` | `RequestLogFilter.ProviderID`（唯一的非日志列过滤，注释写明是尝试粒度匹配） |
| `internal/store` | `providerDimension`/`requestLogGroupExpr("provider")`、`providerDimensionSource`（尝试粒度源）、`selectDimensionSources(..., exact)`、`dimensionSourceSQL(..., byProvider)`、`requestLogFilter` 的 `EXISTS`、参考查询（probe/独立对照）的 `requestLogDimensionReference`、`RequestProviders`、`ProviderNames` |
| `internal/httpapi` | `provider_id` 查询参数解析（非数字 400）、列表/详情 `providers: [{id, name}]`、统计行 `provider_id`/`provider_name`、路由表摘要与 `group_by`/`provider_id` 描述（MCP 工具描述即由此生成） |
| 控制台 | 分组下拉新增「供应商」、`/providers` 筛选下拉、列表「供应商」列、统计行「名字 #id」、详情「供应商」字段、`请求数` 表头 tooltip 与卡片说明 |
| 文档 | 本文、`docs/request-log.md` |

**没有迁移**：不动表结构、不动写入路径、不动汇总表 —— 数据早就在 `usage_records.provider_id` 里。

## 6. 验收

- `internal/store/provider_dimension_test.go`：
  同一模型两家供应商各自成本（`107/250`，模型桶仍为 `357`）；未计量请求进 `provider_id=0` 桶；
  **小时汇总建好之后**供应商分组仍给出正确成本（本里程碑的核心回归：读汇总会给出「有小时、没供应商」）；
  供应商筛选选中正确的请求集，且列表/总数/统计三处一致；筛选下分组按请求拆分；
  筛选在汇总前后一致（`exact` 生效）；`RequestProviders` 升序去重、`ProviderNames` 缺行不造名字。
- `TestDimensionRollupAllFiltersAndBoundaries`（既有，独立对照查询）现在覆盖 `provider`：
  在 raw / rolled / dirty / disabled 四种模式下与直接 join 的参考实现逐字节一致。
- `internal/httpapi/request_dimensions_test.go`：分组行的名字与 id、未知桶、模型桶不受影响、
  筛选的列表/总数/统计一致、失败转移行列出两家、非数字 `provider_id` 返回 400、
  汇总前后同一个答案。
- `internal/httpapi/mcp_admin_test.go`：`admin_describe` 暴露 `group_by=provider` 与 `provider_id`，
  **工具描述里带着计数口径那句话**，并用 MCP 真的读回两家供应商各自的成本与筛选结果。
- `internal/webui/embed_test.go`（既有）：控制台下拉必须提供服务端接受的每个维度。
- UI 走查（`make ui-check`）：requests 视图 106 项断言，含 9 项供应商断言
  （列渲染两家、未计量行说「未计量」、下拉发出 `provider_id`、分组显示「名字 #id」与「（未知）」、
  表头写明计数口径）。

## 7. 已知边界与未做

- **不带账号作用域的 MCP 用量工具没动**：`get_usage_breakdown`（`group_by=model|key|day`）仍不支持
  `provider`。给客户自己的 MCP token 暴露供应商 id/名字会泄露上游供应商身份，而本项目对数据面
  一贯的立场是「不回供应商标识」（见 `internal/httpapi/v1_test.go`）。要做先定口径（只给 id？只给
  平台内部自定义名？），属于另一个决定。
- **`admin_list_requests` 的 MCP `providers` 字段**是数组，读它的调用方要按数组处理；MCP 描述已写明。
- **供应商被删除后**名字取不到，`provider_name` 为空、`provider_name` 缺失时控制台显示 `#id`；
  计量行是计费记录，不随配置删除，所以 id 永远有值。
- **大窗口下 `group_by=provider` 的耗时**未实测；与 M27 的观察项合并看待，若 p95 超标再考虑
  为供应商单独建小时汇总（那需要另一张按 (request, provider) 聚合的表，与现行 rollup 的请求粒度不同）。

## 8. 后续实现差异（M78）

M78 把「供应商」变成了同一份计量事实的一种读法，两处签名随之变化（口径不变）：

- `store.RequestProviders`（返回 `map[string][]int64`，按 provider id 升序去重）被
  `store.RequestAttempts`（返回 `map[string][]domain.RequestAttempt`，按 `attempt_no` 升序）取代：
  列表/详情一次查询同时供 `providers` 与 `attempts` 使用，`providers` 由 `attempts` 去重得出，
  顺序是**首次尝试顺序**而不是 id 升序——两者读起来才是同一个故事（这一条是本机构建时对显示顺序的
  唯一行为变化）。
- 计量行新增 `route_id`/`upstream_model`（迁移 0027），所以 §6 里那条 `RequestProviders` 升序去重的
  断言改成了 `RequestAttempts` 的顺序与字段断言；`ProviderNames` 的规则不变。
- §7 最后一条关于「不回供应商标识」的立场不变：M78 只扩展**管理面**（列表/详情/后台工具描述），
  MCP 账户自助查询工具仍然不暴露供应商与路由。

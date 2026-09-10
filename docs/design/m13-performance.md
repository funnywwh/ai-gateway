# M13 设计：性能与并发验证

> 目标：把「热路径零 DB、单写者批处理、有界队列」这些设计承诺变成**可复现的数字**，
> 并留下一个不需要外部工具就能跑的压测入口。

## 1. 环境约束（决定了做法）

- 环境里**没有** `hey`/`wrk`/`ab`，也没法随便装；
- 有 Go 工具链，所以压测器**自己写**：`cmd/loadgen`（标准库 net/http，多 goroutine，输出 rps 与分位延迟）；
- 单机、共享 CPU，因此：**不在 CI 里断言绝对数字**，而是（a）留基准与压测脚本，
  （b）把「回归」写成相对断言（例如缓存命中路径不得比缓存未命中慢一个数量级）。

## 2. 层次与测法

| 层 | 测法 | 关注点 |
| --- | --- | --- |
| 微基准 | `go test -bench` | 路由规划、计价求值、限速判定、Key 校验（命中缓存）、账本批处理 |
| 端到端 | `cmd/loadgen` + 真实二进制 | rps、p50/p95/p99、错误率；`testecho` 供应商保证可复现 |
| 并发正确性 | 现有的 `-race` 被环境禁用（无 cgo），改为**并发压测 + 不变量巡检**：压测后跑 `billing/invariants` 与 `rebuild-ledger` dry-run 必须一致 |

## 3. 关键热路径（也是基准列表）

1. **鉴权**：`apikey.Verifier.Verify` —— 前缀索引 + 常量时间比较 + 30s 正缓存；基准要覆盖命中与未命中；
2. **路由规划**：`routing.Router.Plan` —— 纯内存快照 + 候选过滤 + 分层排序；
3. **限速**：`quota.Limiter.Reserve` —— 64 分片 × 60 桶；
4. **计价**：`pricing.Evaluate` —— 整数运算 + 规则匹配；
5. **结算**：`store.SettleBatch` —— 单事务批处理；并发压测时它决定写吞吐上限。

## 4. cmd/loadgen

```
loadgen -url http://127.0.0.1:8080/v1/responses -key sk-gw-... -model echo \
        -concurrency 32 -duration 20s -stream=false -body-extra '{}'
```

- 输出：总请求、成功/失败、rps、平均/中位/p90/p95/p99/最大延迟；
- 失败按状态码归类，便于区分 401/402/429/5xx；
- `-stream` 会读取 SSE 到 `response.completed`，用于测流式路径；
- 不引入依赖，`go run ./cmd/loadgen` 即可。

## 5. pprof

`server.pprof: true` 时挂载 `net/http/pprof` 到 `/debug/pprof/`（默认关闭：生产不该暴露）。
压测期间用它取 CPU/堆 profile，结论写进本文档的实测小节。

## 6. 并发与资源上限的验证点

- **限速**：并发超过 rpm 时必须是 429 且带 `Retry-After`，且**不写出网用量**；
- **在途额度**：并发请求的总预留不得超过余额（压测后检查余额不为负、不变量成立）；
- **单写者批处理**：压测后 `billing/status` 的 `batches` 应远小于 `settled`（证明批处理生效）；
- **背压**：hook 队列满时丢事件并计数，不阻塞请求（压测期间 `hooks` 丢弃计数允许增长）。

## 7. 实测记录（本机 i7-12700K，单进程，testecho 供应商，2026-09）

### 微基准（`go test -bench`）

| 基准 | ns/op | 说明 |
| --- | --- | --- |
| `pricing.Evaluate` | 4229（并行 1874） | 4 条规则、3 个维度的求值 + 快照 |
| `routing.Plan` | 4717（并行 3957） | 1 模型 × 4 供应商的候选筛选与分层 |
| `httpapi` 非流式响应（端到端 in-process） | 1,738,786（约 1.74ms） | 含鉴权、路由、计量、结算落库 |
| `httpapi` 并发响应 | 991,123（约 0.99ms/op，并行） | 同上，多 goroutine |
| `GET /v1/models` | 81,994 | 纯内存快照读 |

### 端到端压测（`scripts/load.sh 32 10s`）

```
requests=6196 ok=6164 failed=32      rps=619.6
latency avg=51.6ms p50=1.73ms p90=7.70ms p95=44.5ms p99=1.73s max=3.99s
usage rows=6196  ledger charge entries=6196   （逐条对齐）
billing/invariants: ok=true（四条不变量全绿）
```

- 32 个 `transport_error` 全部是 10s 截止时刻仍在飞的请求，属于压测器主动取消；
- 结算批处理生效：首轮统计 `batches=30 / settled=5315`（平均每批约 177 条）；
- **p99 的长尾来自 SQLite 单写连接**：每个请求要写 usage+ledger（结算）、response 记录、request log 三种行，
  单写者串行化后排队；这是当前架构的已知上界，压测数据与设计预期一致。

## 8. 压测发现并修复的两个真缺陷（单元测试没抓到）

1. **重试复用过期 context → 整批落兜底文件**。`flushBatch` 用同一个 10s context 做「批量尝试 + 逐条重试」，
   批事务一旦超时，后续每条重试都在已过期的 context 上立刻失败，于是 11 条结算全部写进 `billing-fallback.jsonl`。
   修复：每次尝试各自新建 deadline（批量按行数放大到 10s+50ms/行，单条 5s）。
2. **不变量巡检读了两个不一致的快照 → 高并发下误报差异**。`CheckInvariants` 先后用两条独立查询汇总
   usage 与 ledger，写者在两条查询之间提交就会看到假差异（首轮压测 `charge_mismatch=1`）。
   修复：新增 `store.BillingAuditSnapshot`，在**一个只读事务**里取全部聚合（WAL 下即一致快照）。

修复后复跑：`ok=true`、`usage rows == ledger charge entries == 6196`、**无兜底文件**。

## 9. 实现与设计差异

1. **压测器自己写**（`cmd/loadgen`，标准库）：环境没有 hey/wrk，且自带的压测器能直接给出分位延迟与状态码分布；
2. **`scripts/load.sh` 一条命令跑完**：起服务 → 通过**管理 API**（而不是直接改库）建账户/供应商/模型/路由/Key →
   用 SQL 只补一笔充值（`/credits` 接口当时还没有；M12 之后可改为调用接口）→ 压测 → 校验账单与不变量；
3. **基准不做 CI 断言**：本机共享 CPU，绝对数字不可靠；基准与脚本留作回归对比（`go test -bench` 手动跑）；
4. **pprof 默认关闭**：`server.pprof: true` 才挂 `/debug/pprof/`（生产不该暴露 goroutine/heap）；
5. **额外加了并行基准**（`RunParallel`）以观察锁竞争：`routing.Plan` 并行反而更快（4.0µs vs 4.7µs），
   说明热路径确实没有全局锁竞争；
6. **并发正确性以「压测 + 不变量巡检」替代 `-race`**（环境无 cgo 无法跑 race detector），
   这条替代方案本身也促成了上面第 2 个缺陷的发现。
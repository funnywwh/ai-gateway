# M26 设计文档：请求路径的审计写入批量化

> 状态：已实现并在 8088 运行态生效（2026-09-12 10:17 重启）。
> 规格同步：`docs/api-responses.md`、`docs/architecture.md`、`README.md`、`config.example.yaml`、`docs/TODO.md`（M26 清单）。
> 流程说明：本文件是**实现完成后回填**的（先代码、后文档），偏离 `docs/PROCESS.md` 的
> 「设计文档先于代码」；差异与原因见文末。

## 背景（推翻 M25 的一个结论）

M25 的设计文档结论是：写入池串行「**不需要**改异步队列或背压」，理由是实测单行插入
p50 ≈ 40 µs、并发 × 单行成本 = 队首等待，余量足够。

M26 的测量推翻了它，因为 M25 低估了两件事：

1. **每个请求写两行，不是一个事务**：`responses`（若 `store:true`）+ `request_logs` 各一次
   `ExecContext`，各自一次事务、各自一次 `fsync`，都要独占唯一那条写连接。
2. **真实客户端的请求体远大于「用户输入」**：`recording.record_input=user` 只把用户消息落库，
   但**请求日志仍要记录 `request_bytes`**，而生产库里的实际值是平均 742 KB、最大 962 KB
   （`data/aigw-local.db` 的 `request_logs`：2056 行占 708 MB）。行大了，写连接被占用的
   时间就长，队首等待成倍放大。

由此在 8088 运行态上测到的现象（16 并发、离线 replay 上游、60 s）：

| 指标 | 实测 |
|---|---|
| 吞吐 | 63 rps |
| 每请求 CPU | 2.57 ms |
| p50 / p90 | 16 ms / 480 ms |
| pprof 归因 | `handleCreateResponse` 占进程 CPU 51.7%，其中 `storeRequestLog` 18.5% + `PutResponse` 13.5%；后台计费结算另占 11% |
| goroutine dump | 6 个请求 goroutine 卡在 `store.(*DB).PutRequestLog` → `database/sql.(*DB).conn` |

还有一条来自真实运行日志的直接证据：旧实例在 10:04:17 打过
`request log stored without content after a failed write request_bytes=1221313`
—— 那次写入真的失败了，**1.2 MB 请求的正文没落库，只留了骨架行**。

另外 profile 里 `_sqlite3Prepare` 累计占 14%：纯 Go 驱动（`modernc.org/sqlite`）**每次 Exec 都
重跑一遍 SQL 解析器**（`sqlite3Prepare` → `sqlite3RunParser` → `yy_reduce`），而写路径执行的是
三条固定不变的 INSERT。

## 关键决策

1. **审计行离开请求路径，进入后台批量事务**。请求把「存储响应 + 请求日志」两行交给
   `store.LogWriter`，由它在一个事务里成批提交。默认 250 ms / 256 请求 / 16 MiB 触发。
   - 取舍：审计行最多晚一个 flush 间隔落库；换来的是写连接不再被每请求独占。
   - 否决「只把 request_logs 异步、responses 保持同步」：那样写连接要被同步的 responses
     写占用，队首等待仍在。
2. **响应已经发给客户端之后才入队**。`persist()` 的所有计算（JSON 序列化、录制策略、
   `recordInput`）保持同步，只有最终落库异步 —— 这样「一次计算喂三处」（存储响应/请求日志/
   hook 事件）的既有约束不变，各计数口径也不变。
3. **审计语义一条都不放松**：批量整体失败 → **逐行重试**（一行坏数据不能带走同批邻居的审计行）；
   单行仍失败 → 报回 transport，由它写**内容为空的骨架行**并计数。`write_failures` / `dropped`
   的口径与同步路径完全一致，`requestlog.go` 仍是唯一的计数与兜底所有者。
4. **读一致性用「按 id 等待」解决，而不是让写入重新同步**。`GET /v1/responses/{id}` 在查询前
   调 `AwaitResponse(ctx, id)`：只有该 id 还在队列里时才等。绝大多数 GET 不受影响。
   - 这一条是**实现过程中被压测发现的**：第一版批量上线预演时，「POST 后立刻 GET 自己刚拿到的
     id」返回了 404（行还在队列里）。客户端拿到 id 就有权按 id 取回，这是 OpenAI Responses 的
     契约，必须成立。
5. **队列有上限并施加背压**。`QueueRows` / `QueueBytes`（默认 4096 / 32 MiB）满时，`Enqueue`
   等写入推进，最多 30 s；等不到空位的那一行走骨架行兜底并计入 `dropped`。
   - 这一条同样来自实测：**没有上限的队列在 700 KB 请求持续 116 rps 时涨到约 350 MB，
     并且 `docker stop` 15 s 排不空**。无限队列既是无界内存，也让优雅关闭失去保证。
6. **关闭顺序固定为：HTTP 优雅关闭 → 排空审计队列 → `db.Close()`**，排空超时 15 s（与整体
   关闭预算一致）。SIGTERM/`local-run.sh stop` 因此不丢行；硬杀进程丢最后 ≤250 ms 的窗口，
   这个代价写在 `config.example.yaml` 里。
7. **写路径复用预编译语句**（`writerStmtCache`），用 `database/sql` 的 `*sql.Stmt` 而不是自己
   持有 driver statement：语句归属于某条连接，而池会换连接；`*sql.Stmt` 由 `database/sql`
   按连接自动重建，换了连接最多多一次 prepare，不会执行到错的语句。

## 接口

```go
// internal/store/logwriter.go
type LogWriterConfig struct {
    FlushInterval time.Duration // 一行最多等多久（默认 250ms）
    MaxBatch      int           // 一个事务最多带多少请求（默认 256）
    MaxBytes      int           // 一个事务最多带多少字节（默认 16MiB）
    QueueRows     int           // 队列上限（默认 4096）
    QueueBytes    int           // 队列字节上限（默认 32MiB）
    MaxWait       time.Duration // 队列满时最多等多久（0 → 30s）
}
func NewLogWriter(db auditStore, cfg LogWriterConfig, log Logger,
    onFailure func(*domain.RequestLogRecord, error)) *LogWriter

func (w *LogWriter) EnqueueRecording(resp *domain.ResponseRecord, log *domain.RequestLogRecord)
func (w *LogWriter) WriteNow(ctx context.Context, resp *domain.ResponseRecord, log *domain.RequestLogRecord) error
func (w *LogWriter) AwaitResponse(ctx context.Context, id string) error
func (w *LogWriter) AwaitAll(ctx context.Context) error
func (w *LogWriter) Flush(ctx context.Context) error   // = AwaitAll，测试/运维用
func (w *LogWriter) Close(ctx context.Context) error   // 排空并停止
func (w *LogWriter) Stats() map[string]any
func (w *LogWriter) SetFailureHandler(func(*domain.RequestLogRecord, error))

// internal/httpapi：可选依赖，nil 时保持同步写路径（测试与小型部署）
type LogRecorder interface {
    EnqueueRecording(*domain.ResponseRecord, *domain.RequestLogRecord)
    WriteNow(ctx, *domain.ResponseRecord, *domain.RequestLogRecord) error
    Stats() map[string]any
}
type responseWaiter interface{ AwaitResponse(ctx context.Context, id string) error }
```

配置（`recording.*`，默认开）：`batch_writes`、`batch_flush_ms`、`batch_max_rows`、
`batch_max_bytes`、`batch_queue_rows`、`batch_queue_bytes`。
观测：`/metrics` 增加 `aigw_audit_batched_requests_total`、`aigw_audit_batches_total`、
`aigw_audit_queued_requests`；`/stats` 的 `request_log` 块增加 `batching` 子块（含 `backpressure`）。

## 数据流

```
handleCreateResponse
  └─ persist()                      同步：序列化输出/用量、应用录制策略、算 input 文档
       ├─ recordContent()           构造 RequestLogRecord
       └─ LogRecorder.EnqueueRecording(respRec, logRec)
                                    入队（不做 I/O；队列满时才等待）
            └─ LogWriter.loop()     后台：凑批（间隔/条数/字节）→ 一个事务
                 ├─ PutAuditBatch() INSERT responses（若有）→ INSERT request_logs → COMMIT
                 └─ 失败：逐行 putAuditSync() → 仍失败 → onFailure → 骨架行 + 计数

GET /v1/responses/{id}
  └─ AwaitResponse(id)              仅当该 id 还在队列里时才等
       └─ GetResponse()
```

## 异常与边界

| 情况 | 行为 |
|---|---|
| 批量提交失败（锁/超时） | 日志 WARN + **逐行重试**；同行中坏数据不影响邻居 |
| 单行重试仍失败 | 报回 transport → 写骨架行（保住 id/状态/字节数）+ `write_failures` |
| 骨架行也失败 | `dropped` 计数 + ERROR 日志（与 M25 相同口径） |
| 队列满 | 背压：等待写入推进，计数 `backpressure`；超 `MaxWait` 则走骨架行兜底 |
| 关闭时仍有队列 | `Close()` 排空（15 s 预算）；`Enqueue` 在关闭后改为直接同步写 |
| POST 后立刻 GET | `AwaitResponse` 等到该 id 落库，不返回 404 |
| 写连接被池换掉 | `*sql.Stmt` 由 `database/sql` 在新连接上重建；`statementUnusable` 命中时丢缓存重来一次 |
| `tx.StmtContext(池级语句)` | **禁用**：单连接写池下会自锁（语句占着那条连接，`StmtContext` 又去要同一条），改为事务内 `tx.PrepareContext` |

## 测试策略

- `internal/store/logwriter_test.go`：批量失败 → 逐行重试全部落库；队列满 → 背压 + 超时上报（用
  「写侧卡住」的 store 制造真实满队列）；Stats 队列深度。
- `internal/store/stmtcache_tx_test.go`：**自锁回归**（池级语句 + 事务路径必须不阻塞）。
- `internal/httpapi/audit_batch_test.go`：批量后读回同一批行；行确实**不在** handler 返回前落库；
  POST 后立刻 GET 返回 200；批失败 → 骨架行 + 计数各一次；关闭排空；`WriteNow` 绕过队列。
- A/B 压测（同机同脚本、副本实例）：基线二进制 vs 修复二进制，测 rps / 每请求 CPU / p50 / p90。
- 正确性压测：32 并发 20 s → 请求数 == 落库行数 == 计数器，队列归零，事务数远小于请求数。

## 依赖

无新增依赖。只用到 `database/sql`、`modernc.org/sqlite`（既有）与 `internal/domain`。

## 实测结果

| 指标 | 基线 | 修复后 |
|---|---|---|
| 吞吐（16 并发小请求 60 s） | 20–63 rps | 65–186 rps |
| 每请求 CPU | 1.97–2.57 ms | 0.82–1.06 ms |
| p50 | 13.8–16.3 ms | 4.2 ms |
| p90 | 480–775 ms | 17–19 ms |

- profile：`_sqlite3Prepare` 累计 14% → 5%；`_full_fsync` 0.34%。
- 正确性（副本，32 并发 20 s）：9616 请求 → 9616 行、0 失败、0 丢弃，**61 个事务**（同步路径 9616 个）。
- 关闭排空：200 请求后立刻 SIGTERM → 200 行全部落库，无排空失败。
- 生产规模（真实 746 MB 库副本 + 700 KB 请求体，8 并发）：请求 152 → 新增 152 行；同体积的
  1.2 MB 压测 42 请求 → 正文全部完整、0 失败（该体积在旧代码上正是退骨架行的那一次）。
- 运行态（8088，2026-09-12 10:17 重启）：启动日志出现 `audit writes batched in the background`，
  实测 16 个并发请求只用了 2 个事务，`write_failures` / `dropped` 均为 0，`queued_requests` 归零。

## 实现与设计差异

- **本文件在实现完成后才写**：`docs/PROCESS.md` 要求设计文档先于代码并在对话中确认。本轮的
  起点是线上采样（不是新功能规划），实际顺序是先量化 → 修复 → 验证；文档按流程要求补齐，但
  时序差异如实记在这里。
- **第一版实现漏了读一致性**：`responses` 行异步化之后，「POST 后立刻 GET 自己拿到的 id」会 404。
  这是预演压测发现的，补了 `AwaitResponse`（按 id 等待）与对应测试，而不是把 `responses` 写回同步。
- **第一版实现漏了队列上限**：生产规模压测（700 KB 请求持续 116 rps）把队列涨到约 350 MB 且
  关停排不空，因此补了 `QueueRows` / `QueueBytes` 背压与 `MaxWait`。
- **`tx.StmtContext` 自锁**：预编译语句接进事务路径的第一次尝试让
  `TestStreamingAbortChargesOnlyUpToTheDecisionPoint` 从 0.02 s 变成 18 s 超时；定位到单连接写池下
  `tx.StmtContext` 会去要语句自己占着的那条连接，改为事务内 `tx.PrepareContext` 并留回归测试。
- **`batched_rows` 改名为 `batched_requests`**：它计的是「请求数」而不是「行数」（一个请求可能带
  1–2 行），命名按实际语义改正。

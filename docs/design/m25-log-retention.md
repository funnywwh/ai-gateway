# M25 设计文档：请求日志的写入兜底与保留期清理

> 状态：实现中（本文件在实现前已在对话中输出并获「两个都做」的确认）。
> 规格同步：`docs/api-responses.md`、`config.example.yaml`、`docs/TODO.md`（M25 清单）。
> 编号说明：M24 已被「管理后台列表分页」（`docs/design/m24-console-pagination.md`）占用，本里程碑顺延为 M25。

## 背景（M23 收尾时量化出来的两个口子）

M23 把请求日志默认口径收窄到「只记用户输入」后，行大小从 ~985 KB 降到 ~1.5 KB。在真库副本上实测
（同一驱动、同一 pragma、写入池 `SetMaxOpenConns(1)`）：单行插入 p50 ≈ 40 µs、p95 ≈ 0.3 ms，
而旧形态 p50 ≈ 7 ms、p95 ≈ 0.5 s；`wal_checkpoint(PASSIVE)` 0.26–0.49 s。写入池按设计串行，
**单行成本 × 并发 = 队首等待**，所以旧默认的 ~1 MB/行是那 20 次 `context deadline exceeded` 的主导项，
收窄后余量已足够——**不需要**改异步队列或背压。

但还有两个真实口子：

1. **失败即丢行**：`recordContent` 的写入失败只记一条 WARN，整行消失。而且不止大正文会超时：
   在副本上给文件系统刷脏页时，1.8 KB 的行也出现过 5.5 s。丢掉的可能正是要排障的那一条
   （M19d/M19e 的盲点）。`recordDenied` 更差：它跟随请求 context，客户端挂断直接取消写入。
2. **保留期至今无效**：`recording.retention_days` 从 M7 起就是死配置，没有任何清理任务；
   `responses.expires_at` 也写了却没人执行。真库 708 MB 里 689 MB（97%）是历史正文。

## 关键决策

1. **写入兜底而不是加大超时**：新增统一入口 `storeRequestLog(ctx, rec)`。
   第一次失败 → 计数 `write_failures` + **去掉正文重写骨架行**（只留 request_id / key / account /
   endpoint / status / request_bytes / created_at），用新的独立超时（15 s，骨架行只有几十字节，
   队列正常情况下早已排空）；再失败 → `dropped` 计数 + ERROR 日志。
   调大超时不解决「一次多秒级停顿」，兜底保证最坏情况下请求仍然可见。
2. **两条写入路径共用该入口**：`recordContent` 与 `recordDenied`。顺带把 `recordDenied` 的写入
   改为脱离请求 context（`WithoutCancel` + 超时），与 `persist` 一致：拒绝的请求同样属于
   「事后要排障」的那一类，不该因客户端挂断而消失。
3. **计数可见**：`/admin/api/v1/stats` 增加 `request_log` 块（`retention_days`、`write_failures`、
   `dropped`、`pruned`），`/metrics` 增加 `aigw_request_log_write_failures_total`、
   `aigw_request_log_dropped_total`、`aigw_request_log_pruned_total`。
4. **保留期语义**：`recording.retention_days`（默认 30）**<=0 = 明确关闭**（不清理、且
   `responses` 不再写 `expires_at`）。这是唯一旋钮，`request_logs` 与 `responses` 都听它。
5. **`responses.expires_at` 由同一个旋钮推导**：`persist` 里不再硬编码 30 天，
   改为 `completed + retention_days`；关闭保留期时写 NULL（= 永不过期），与「行级真相」一致。
6. **清理是每日任务，不是实时反应**：新增 `internal/retention` 的 `Janitor`，启动时跑一次、
   之后每 24 h 一次（照 `billing.StartExpiryJob` 的写法）。**分批**删除：每批 500 行，
   每轮最多 200 批，避免长时间占用唯一的写连接。
7. **可手动触发**：`POST /admin/api/v1/requests/prune`（`roleAdmin` + Dangerous + ConfirmReason），
   照 `billing/expire-credit` 的先例：返回删除条数并写审计。控制台请求日志页显示保留期并提供
   「清理过期日志」按钮。
8. **不动计费与审计历史**：`usage_records`、`ledger_entries`、`audit_logs` 一律不清理——它们是
   账与合规记录，不属于「可丢的观测数据」。
9. **VACUUM 单独做**：删行只把页归还给 freelist，文件不缩。VACUUM 需要独占访问，属于宿主人工步骤，
   写进 `docs/TODO.md`，不放进任务里。
10. **`internal/retention` 是新的叶子包**：只依赖标准库（store 通过端口注入），因此
    `internal/arch` 的允许表新增一条空条目；httpapi 为了端口类型新增一条边。

## 接口

```go
// internal/store
func (db *DB) PruneRequestLogs(ctx context.Context, before time.Time, limit int) (int, error)
func (db *DB) PruneExpiredResponses(ctx context.Context, now time.Time, limit int) (int, error)

// internal/retention（叶子包，注入端口）
type Store interface {
    PruneRequestLogs(ctx context.Context, before time.Time, limit int) (int, error)
    PruneExpiredResponses(ctx context.Context, now time.Time, limit int) (int, error)
}
type Config struct{ RetentionDays, BatchSize, MaxBatches int }
type Result struct {
    RequestLogs int  `json:"request_logs"`
    Responses   int  `json:"responses"`
    Batches     int  `json:"batches"`
    Exhausted   bool `json:"exhausted"` // 达到每轮上限，下一轮继续
    Disabled    bool `json:"disabled"`  // retention_days <= 0
}
func New(store Store, cfg Config, log *slog.Logger) *Janitor
func (j *Janitor) Run(ctx context.Context) (Result, error)
func (j *Janitor) Start(ctx context.Context, interval time.Duration)
func (j *Janitor) RetentionDays() int
func (j *Janitor) PrunedTotal() int64

// internal/httpapi
type requestLogWriter ...                        // 见实现
func (s *Server) storeRequestLog(ctx context.Context, rec *domain.RequestLogRecord)
type LogJanitor interface {                      // Deps.LogJanitor，nil = 接口未接线
    Run(ctx context.Context) (retention.Result, error)
    RetentionDays() int
    PrunedTotal() int64
}
```

## 数据流

```
persist → recordInput → recordContent ─┐
rejectForQuota → recordDenied ─────────┴→ storeRequestLog(ctx, rec)
     ├─ 成功：落库
     ├─ 失败：write_failures++ → 去正文重写骨架行（新超时 15s）
     │        ├─ 成功：请求仍可见（正文为空，request_bytes 保留）
     │        └─ 失败：dropped++ + ERROR 日志
     └─ ctx 一律 WithoutCancel + 超时（客户端挂断不影响审计）

retention.Janitor（启动 + 每 24h，或 POST /admin/api/v1/requests/prune）
     ├─ request_logs: created_at < now - retention_days，每批 500，最多 200 批
     └─ responses:    expires_at < now，每批 500，最多 200 批
```

## 异常与边界

- 保留期关闭（`retention_days <= 0`）：`Run` 直接返回 `Disabled`，不删任何行；`persist` 写
  `expires_at = NULL`（客户端永久可取回该响应，这是关闭保留期的字面含义）。
- 每轮上限触发（`Exhausted`）：日志记 WARN 并在下一轮继续，不阻塞启动，也不在请求路径上删除。
- 删除与并发写入：删除走同一个单写连接、每批一个事务；`request_logs.created_at` 已有索引
  （`idx_request_logs_time`），`responses.expires_at` 新加索引（迁移 0007）。
- 骨架行的语义：`request_json` 为空 → 控制台显示「未录制」但保留体积与状态；与 `metadata` 档在
  记录层可区分（骨架行的 `record_input_mode` 仍是解析后的真实档位，`request_bytes` 只有骨架没有正文）。
- 骨架重写不重试第二次：宁可在计数里体现 dropped，也不把请求 goroutine 拖住更久。
- `usage_records`/`ledger_entries`/`audit_logs` 不受影响；对账/不变量不会因为清理而变化。

## 测试策略

- `internal/store`：批量删除的条数与上限、边界时间（不删等于 cutoff 的行）、空表、
  `responses.expires_at` 为 NULL 的行永不删除。
- `internal/retention`：假 store 驱动——分批到上限时 `Exhausted`、关闭时 `Disabled` 且不调用 store、
  `PrunedTotal` 累计、错误向上传播。
- `internal/httpapi`：注入一个「首次 PutRequestLog 必失败」的 Records 替身 → 骨架行落库且正文为空、
  计数与 `/stats` 字段正确；`recordDenied` 在客户端 context 被取消后仍写入；
  `POST /admin/api/v1/requests/prune` 需要 admin、返回计数、写审计；`/stats` 暴露 `request_log` 块。
- `internal/webui`：请求日志页文案包含保留期与「清理过期日志」入口（embed 断言）。
- `make verify` 全绿；`make ui-check` 视图全绿（requests 视图新增保留期/清理按钮断言）。

## 依赖

标准库 + 既有 `internal/{store,domain,config,httpapi}`；不新增外部依赖。

## 实现与设计差异

- **里程碑编号顺延为 M25**：实现期间仓库里出现了并发提交 `27e3cd5 M24: 管理后台所有列表分页`，
  M24 已被占用，本设计文档随之改名 `m25-log-retention.md`（内容不变）。
- **与分页改造的合并**：控制台请求日志页在 M24 里改成了 `pagedTable`，本里程碑的保留期提示与
  「清理过期日志」按钮是在**新结构上**重做的（清理后用 `view.reset()` 回到第 1 页，因为删除会平移所有
  offset）；harness 的 `#requests` 视图断言从 10 项扩到 18 项，fixtures 也补上了分页信封、`/stats`
  与 `/requests/prune` 三条。
- **`storeRequestLog` 的第二次尝试不复用第一次的超时**：调用方传进来的已是 `auditCtx`（5 s），
  骨架行自己拿一个新的 15 s 预算，因为失败原因通常正是「写连接被占满」。
- **骨架行与 `ON CONFLICT` 的相互作用**：`PutRequestLog` 的冲突分支不更新 `request_json`，所以
  「第一次其实写成功但返回了超时」时，骨架重写只会刷新状态列，不会把已存正文抹掉；反之若首次彻底
  失败，骨架行才作为新行插入——两种情况都不会损坏记录。
- **非目标**：`PutResponse`（存储响应正文）仍只记 WARN——客户端已经拿到回答，取回失败不属于审计缺失；
  VACUUM 不在任务里（需要独占访问，作为宿主人工步骤写进 TODO）。

# M1 设计文档：存储层（SQLite）与注册表快照

## 目标
落地持久化与内存注册表：迁移框架、DAL、YAML 引导、原子快照、审计日志。
本里程碑结束后，M3/M5 等模块可以从**内存快照**读配置，从 **Store 接口**读写业务数据。

## 关键决策
1. **双连接池（读写分离）**：同一 SQLite 文件开两个 `*sql.DB`：
   - `write`：`SetMaxOpenConns(1)` —— 进程内串行化写者，天然避免 SQLITE_BUSY 竞争；
   - `read`：`SetMaxOpenConns(N)` —— WAL 模式下与写者并发读。
2. **DSN 统一**：`file:<path>?_pragma=busy_timeout(...)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)`。
3. **迁移**：SQL 文件内嵌（`go:embed migrations/*.sql`），文件名 `NNNN_name.sql`；
   逐版本在**单个事务**内执行，成功写入 `schema_migrations`；已应用的版本跳过（幂等）。
4. **时间统一**：所有时间列存 **UTC Unix 秒**（INTEGER）；布尔存 0/1；金额存 int64 微美分。
5. **注册表快照**：`registry.Snapshot` 从 Store 一次性加载 providers/models/mappings/routes/tags，
   构建**只读**结构，通过 `atomic.Pointer[Snapshot]` 原子换入；热路径只读快照，不碰 DB。
6. **审计**：`audit_logs` 记录 actor/action/target/diff/result，供管理面「变更历史」使用。

## 接口（M1 产出）
- `store.DB`：`Open/Migrate/Close` + 实现 `domain.Store`（accounts、api_keys、providers、provider_models、
  models、model_mappings、routes、tags、ledger、usage）。
- `store.Bootstrap(ctx, cfg.Bootstrap)`：库为空时按 `bootstrap.mode` 灌入账户/Key（Key 只存哈希）。
- `registry.Registry`：`Snapshot()`、`Reload(ctx)`（重建并原子换入）、以及按名/按 id 的查询辅助。
- `audit.Writer`：`Log(ctx, Entry)`。

## 数据流
```
启动：Open(Database) → Migrate → Bootstrap（可选）→ registry.Reload
管理面写操作（M8）：Store 写入 → audit.Log → registry.Reload → 原子换入
热路径（M3/M5）：atomic load snapshot → 纯内存决策 → 结算走 store 写入
```

## 异常与边界
- 目录不存在：自动创建；迁移失败：回滚该版本事务并返回错误，进程退出。
- 重复应用同一迁移：跳过（幂等）。
- 引导：`mode=off` 不引导；`upsert` 仅在对应名称不存在时插入；`merge` 覆盖已存在记录。
- 未知迁移文件名：报错（避免静默漏迁移）。

## 测试策略
- 迁移幂等（连续 Migrate 两次）；schema_migrations 记录正确。
- DAL 往返：accounts / api_keys / providers / models / mappings / routes / tags / ledger / usage。
- 唯一约束（api_key prefix、idem_key、model+provider 路由）冲突行为。
- 快照：全量实体加载、原子换入后旧快照仍可用。
- 引导：空库插入、重复引导不重复插入、Key 以哈希存储。

## 依赖
`modernc.org/sqlite`（纯 Go，无 CGO）。

## 实现与设计差异
- DSN 的 `_pragma` 通过 `net/url` 编码，实机验证 `journal_mode=wal` 生效（有测试断言）。
- 多语句迁移体由驱动一次性执行，实机验证通过；迁移在单事务内应用并记录版本。
- `AppendLedger` 采用 `INSERT OR IGNORE` + `RowsAffected==0 则跳过` 的写法实现**幂等**：
  同一 `idem_key` 重放不会重复扣费，也不会二次更新余额（有测试覆盖）。
- `UpsertAccount` **刻意不覆盖 `balance_micros`**：余额的唯一所有者是账本；
  配置引导只更新计费模式/授信等字段。
- 引导范围比计划更完整：除 accounts/api_keys 外，还支持 providers（含 state_dir 推导）、
  provider_models、models、routes、tags；`upsert` 严格只补缺失，`merge` 才覆盖。
- 新增 `internal/secret` 包（SHA-256 哈希 + 前缀提取 + 常量时间比较），
  供引导期与后续鉴权共用；token 为高熵随机值，故不加盐。
- 注册表额外暴露 `Ready()` 与 `String()`，便于启动日志与就绪探针复用。

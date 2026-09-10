# M16 设计：数据库自动备份与恢复

> 规格：`docs/backup.md`（已输出）。本文是落地设计，实现后回填第 8 节差异。

## 1. 目标

在**不阻塞请求**的前提下，周期性产出一致点快照、验证其可读、按保留策略清理，并提供下载与冷恢复入口。

## 2. 一致点是怎么来的

SQLite 在 WAL 模式下，「一致」= 主库 + WAL 的某个时刻。三条路径可选：

1. **`VACUUM INTO '<file>'`**（本项目采用）：SQLite 自己在一个读事务里把库重写成紧凑副本，
   不需要停写、不需要拷贝 WAL，产出即是自洽的单文件；缺点是需要与库等量的临时空间；
2. `wal_checkpoint(TRUNCATE)` + 文件拷贝：需要保证拷贝期间没有写入，实现上要么停写要么加锁；
3. backup API：Go 驱动不一定暴露，且需要自己管分页循环。

因此实现顺序调整为先 `VACUUM INTO`，再对副本做校验；`wal_checkpoint(TRUNCATE)` 作为
**备份前的可选整理**（把 WAL 合并回主库，降低后续写入的 WAL 体积），失败不影响备份。

## 3. 与结算 writer 的配合

- 备份**不排空** writer 队列（规格里写了排空，但那会让备份在高峰期阻塞几十毫秒到几百毫秒）；
- 改为：备份前记录 `writer.Stats()`，备份结束后再记录一次；若期间 `fallbacks` 增长，
  说明有结算落到了兜底文件——备份文件里就**不包含**这些尚未入库的记录；
- 这一事实写进 `backup_jobs.note` 与接口返回的 `pending_settlements`，恢复流程据此提示人工重放。

## 4. 调度

- 自实现 cron 子集（`分 时 日 月 周`，支持 `*`、`a,b`、`a-b`、`*/n`），避免引入依赖；
- 调度器每 30s 醒一次，计算「下一次触发时刻」，到点执行；进程重启后按同一规则重新计算，不补跑历史；
- 手动触发与定时触发走同一条执行路径，只是 `trigger` 字段不同。

## 5. 保留策略

- 每日：保留最近 N 份（默认 7）；每周：每周保留 1 份，共 M 周（默认 4）；每月：每月 1 份，共 K 月（默认 3）；
- 实现为「先选保留集合，再删除不在集合内的文件」：按时间倒序遍历，对 daily 取前 N，
  对每周/每月的第一份标记保留，其余删除；
- **校验失败的文件不删除**（保留供人工分析），并在列表里标 `status=failed`。

## 6. 存储与元数据

- 文件名 `<dir>/aigw-YYYYMMDD-HHMMSS.db`，目录 0700、文件 0600；
- `backup_jobs` 表记录 `id/started_at/finished_at/path/size_bytes/status/quick_check/trigger/note/error`；
- 启动时若表中有 `running` 状态（进程崩溃），标记为 `failed` 并注明 `interrupted`。

## 7. 管理面与控制台

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/admin/api/v1/backups` | 列表 + 目录占用 + 下次触发时间 |
| POST | `/admin/api/v1/backups` | 手动触发（同步执行，返回 job） |
| DELETE | `/admin/api/v1/backups/{id}` | 删除一份（含文件） |
| GET | `/admin/api/v1/backups/{id}/download` | 下载（仅 admin） |
| POST | `/admin/api/v1/backups/{id}/restore` | 冷恢复（需 `confirm: true`，见下） |

## 8. 恢复的半自动语义

规格里写的是「停止网关 → 替换文件 → 重启」。进程内替换正在使用的数据库文件是危险动作（连接池、WAL 都在），
因此实现为**分两步的安全版本**：

1. `POST /backups/{id}/restore` 带 `{"confirm": true}`：把备份文件复制为 `<db>.restore-pending`，
   写审计与 `backup_jobs` 记录，返回「下次启动时恢复」的提示——**不改动正在使用的库**；
2. 进程启动时若发现 `<db>.restore-pending`：先把当前库改名为 `<db>.pre-restore-<ts>`（保留现场），
   再用 pending 文件替换，随后照常迁移 + `quick_check`。

这样恢复是「有计划的重启」而不是「运行中偷换文件」，且失败时仍有回退文件。

## 9. 测试

1. cron 解析与下次触发计算（含 `*/n`、列表、区间、跨日）；
2. 备份产出文件 + `quick_check=ok` + `backup_jobs` 落库；
3. 保留策略：构造 10 份文件（跨日/跨周）验证保留/删除集合；
4. 校验失败的文件不被删除；
5. HTTP：列表、手动触发、删除、下载、restore 缺 confirm 时 400；
6. 冷恢复：预置 pending 文件后重启进程（测试里直接调用启动钩子）验证替换 + 现场保留。

## 10. 实现与设计差异

1. **一致点改用 `VACUUM INTO`**（规格里同时列了它与 checkpoint+拷贝）：它在一个读事务里产出紧凑单文件，
   不需要停写、不需要处理 WAL 副本；`wal_checkpoint(TRUNCATE)` 降级为「备份前的可选整理」，失败只告警。
2. **备份不排空结算 writer 队列**（与规格相反）。排空会让备份在高峰期阻塞请求；改为不排空并在
   `backup_jobs.note` 里说明「快照不包含此刻仍在队列/兜底文件里的结算」，恢复流程据此提示人工重放。
3. **恢复做成两阶段**：`POST /backups/{id}/restore` 只把快照**暂存**为 `<db>.restore-pending`（运行中替换
   正在使用的数据库文件会破坏连接池与 WAL），进程启动时由 `ApplyPendingRestore` 完成替换，
   并把原库保留为 `<db>.pre-restore-<时间戳>`。实测：重启日志出现「database restored from a staged snapshot」，
   数据可读、现场保留。
4. **cron 子集额外支持名称**（`mon`/`jan` 等），规格只要求数字；标准 cron 的「日与周同时限定时取或」语义照实现。
5. **保留策略只删 `status=ok` 的快照**：校验失败的文件必须留给人看（规格里只提到「保留该文件」，
   实现把「不被清理」也一并保证）。
6. **`BackupJob` 类型放在 `internal/domain`**：store 需要持久化它，若留在 `internal/backup` 会形成循环依赖。
7. **管理面额外提供** `DELETE /backups/{id}`、`POST /backups/prune`，并在列表里返回目录、总占用与下次触发时刻；
   下载端点仅 admin 角色可用。
8. **`POST /backups` 需要 `{}` 与 JSON 类型头**（继承 M9 的 CSRF 收紧），控制台会自动带上；
   纯 curl 调用需显式加 `-H 'Content-Type: application/json' -d '{}'`。
9. **未实现**：控制台 Backups 页面仍是占位页（接口已就绪，列入下一轮）；备份到对象存储/异地同步按规格留到 v2。
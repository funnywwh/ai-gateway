# 数据库备份与恢复

> 状态：**已实现**（M16：`internal/backup` + `/admin/api/v1/backups`；恢复为两阶段冷恢复，见 `docs/design/m16-backup.md`）。SQLite 单文件 + WAL；备份目标是**一致点快照**。

## 1. 机制

1. **排空结算 writer 队列**（保证 `billing-fallback.jsonl` 为空、账本与 DB 一致）。
2. `PRAGMA wal_checkpoint(TRUNCATE)`，把 WAL 合并回主库。
3. 使用 SQLite 在线备份（`VACUUM INTO` 或 backup API）生成紧凑副本：
   `<backup.dir>/aigw-YYYYMMDD-HHMMSS.db`。
4. **校验**：对备份文件执行 `PRAGMA quick_check`，结果写入 `backup_jobs.quick_check`。
5. **记录**：`backup_jobs`（开始/结束时间、路径、大小、状态、触发方式 `cron|manual`、备注）。

## 2. 调度与保留

```yaml
backup:
  enabled: true
  dir: "./data/backups"
  cron: "30 3 * * *"     # 每日 03:30 低峰（自实现调度器）
  retention_daily: 7
  retention_weekly: 4
  retention_monthly: 3
  verify: true
```

超出保留份数时删除最旧备份（按 daily/weekly/monthly 分组计数）。

## 3. 失败处理

- 备份失败（磁盘满、checkpoint 超时等）→ `backup_jobs.status=failed` + **hook `backup.failed`** + 告警日志。
- `verify` 开启且 `quick_check` 未通过 → 状态标记失败并**保留该文件**供人工分析（不参与保留清理计数之外的静默删除）。
- 备份期间不阻塞请求；写路径短事务不受影响。

## 4. 管理面

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/v1/backups` | 列表（时间、路径、大小、状态、quick_check、触发方式） |
| POST | `/admin/api/v1/backups` | 手动触发一次备份 |
| GET | `/admin/api/v1/backups/{id}/download` | 下载备份文件（仅 admin） |
| POST | `/admin/api/v1/backups/{id}/restore` | 冷恢复（危险，二次确认 + 审计） |

Web 界面「Backups」页提供列表、手动触发、下载与恢复入口。

## 5. 恢复流程（冷恢复）

1. 停止网关（或在管理面发起恢复，由系统拒绝新请求并进入维护态）。
2. 备份当前数据库文件（防止误恢复）。
3. 用选定快照替换 `database.path` 指向的文件，并删除残留的 `-wal` / `-shm`。
4. 重新启动网关 → 自动执行迁移（新版本 schema 兼容）→ `quick_check` → 加载注册表。
5. 校验：`/billing/health`（四条账本不变量）+ 抽样查询用量/账本。
6. 记录审计（`action=restore`，包含备份 id 与操作者）。

> **注意**：恢复是**离线操作**，会丢失快照时间点之后的请求数据（其中未落库的结算已在
> `billing-fallback.jsonl` 中，但该文件不在快照内——恢复后需人工决定是否重放）。

## 6. 范围与限制

- v1 备份到**本地目录**；对象存储/异地同步列为 v2。
- 多副本部署（v2，Postgres）时改为数据库原生备份方案。
- 备份文件包含业务数据与哈希后的凭据密文；**请按敏感数据管理**（权限 0600、目录 0700）。

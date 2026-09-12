-- 请求日志的凭据维度（M30）：谁在调用（account_id / 用户）与用的是哪个 Key（api_key_id）。
--
-- 两列从 0001 起就写在每一行（服务路径与本地拒绝路径都写），本迁移只是给它们补上索引：
-- 控制台从此可以「按用户/按 Key 筛选」，而这正是请求日志最常被问的问题。
--
-- 索引形状与 0008 的三条一致，都带 id 列：历史列表的 ORDER BY 是
-- "created_at DESC, id DESC"（store/historyPageOrder），把 id 放进索引才能让等值前缀
-- 筛选继续走反向扫描；否则 SQLite 退回 temp B-tree，把整个窗口物化后再排序——正是 M24
-- 修掉的那个坑（docs/design/m24-console-pagination.md §8.10）。
--
-- 没有索引时，按账户/Key 筛选会退化成 idx_request_logs_time 上的窗口扫描：返回 50 行也
-- 要把窗口内每一行的正文溢出页读进来（默认口径 2.5 KB/行，`full` 口径可达 1 MiB/行）。
-- 键都是小整数，两条索引约 +30 B/行（对比 0008 三条的 +150 B/行）；实测数字见
-- docs/design/m30-request-log-owner-dimensions.md §7。
CREATE INDEX IF NOT EXISTS idx_request_logs_account ON request_logs(account_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_request_logs_key     ON request_logs(api_key_id, created_at, id);

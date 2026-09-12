-- 请求日志的身份维度：谁在调用（client）、要的哪个模型（model）、路由到哪个模型
-- （resolved_model）、在哪个工作区（workspace）、属于哪个会话（session_id）、是不是
-- 一次辅助调用（call_kind）以及会话标题（title）。
--
-- 它们全部在请求解析后提取，与正文录制口径无关（record_input=off 也写），因此回答
-- 「谁在用、用在哪、花了多少」不再依赖翻正文——而正文恰恰会撞 1 MiB 截断、又会按保留
-- 期被清掉，事后从库里反解并不可靠。
--
-- model / resolved_model 与 usage_records 同名同义：model 是客户端请求的模型名（账单
-- 口径，见 store/invoices.go 按 usage.model 分组），resolved_model 是路由后的规范模型
-- 名。被本地拒绝的请求没有走路由，resolved_model 为空。
ALTER TABLE request_logs ADD COLUMN client         TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN model          TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN resolved_model TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN workspace      TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN session_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN call_kind      TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN title          TEXT NOT NULL DEFAULT '';

-- 三个索引都带 id 列：历史列表的 ORDER BY 是 "created_at DESC, id DESC"
-- （store/historyPageOrder），把 id 放进索引才能让等值前缀筛选继续走反向扫描；
-- 否则 SQLite 会退回 temp B-tree，把整个窗口物化后再排序——正是 M24 修掉的那个坑
-- （docs/design/m24-console-pagination.md §8.10）。
--
-- workspace 与 call_kind 故意不建索引：workspace 基数高、索引大，而聚合查询还要取
-- title/workspace 必然回表、覆盖不了；call_kind 只有 agent/title 两个取值，没有区分度。
-- 实测代价见 docs/design/m27-request-dimensions.md：三条索引让插入的页写放大 1.13×。
CREATE INDEX IF NOT EXISTS idx_request_logs_client  ON request_logs(client, created_at, id);
CREATE INDEX IF NOT EXISTS idx_request_logs_session ON request_logs(session_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_request_logs_model   ON request_logs(model, created_at, id);

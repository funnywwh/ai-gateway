-- M78: 每条请求走的是哪条路由、发给上游的是哪个模型名，以及请求的模型经哪条映射规则变成规范模型。
--
-- 前两个事实是「那次尝试」的属性，不是「这个请求」的：一次请求可以失败转移到多家供应商
-- （M38），把 route_id/upstream_model 压到 request_logs 上，无论取「第一家」「最后一家」还是
-- 「成功那家」，都会把一次失败转移的路线搬到另一家头上 —— 与 M53 拒绝 provider 列同一条理由。
-- 因此它们跟着 usage_records 走（那里本来就是一行一次尝试），与成本、token、错误码同源同寿命。
--
-- 存快照而不是读时 join routes：路由是可编辑、可随供应商删除而消失的配置，而「当时真正发给
-- 上游的模型名」是既成事实，读时反查会让历史显示随配置漂移。route_id 只作身份（控制台
-- 「模型与路由」页上的那一条），路由被删后这个数字仍然保留，界面显示 #id。
--
-- matched_rule 则相反，是请求级事实（一个请求只有一个规范模型、一条命中规则），与
-- resolved_model 同规矩：身份元数据，record_input=off 也记，不参与 redact_paths；
-- 本地拒绝的请求为空（它没有走到模型）。它是常量形状的字符串（model: / mapping:<kind>:<pattern>
-- / alias: / fallback:），不是用户输入。
--
-- 旧行为 0/''：迁移之前的计量行与日志行不知道这些事实，界面显示「（未知路由）」/「—」。
ALTER TABLE usage_records ADD COLUMN route_id       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_records ADD COLUMN upstream_model TEXT    NOT NULL DEFAULT '';
ALTER TABLE request_logs  ADD COLUMN matched_rule   TEXT    NOT NULL DEFAULT '';

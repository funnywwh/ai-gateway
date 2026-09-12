-- 控制台「智能问答」改为按 MCP 令牌执行：会话绑定一个已签发的 MCP 令牌，工具面与可
-- 执行范围完全由该令牌的 scope 决定（query / admin_read / admin）。
--
-- 只存令牌 id，不存明文也不存 token_hash：令牌本身在 mcp_tokens 里，每轮工具调用都用
-- 这个 id 回查一次，所以令牌被撤销或过期后，会话在下一次调用立刻失效——不会出现
-- 「会话里冻结了一份已经作废的权限」。
--
-- 可为 NULL 且不回填：升级前的会话没有被授权的令牌，它们保持「未绑定」并需要显式改绑。
-- 这里刻意不加 REFERENCES：外键在 chat_sessions 的建表语句里只是声明，本仓库的迁移
-- 流程并没有对每条连接打开 foreign_keys，加了也不会真正生效，只会让后来的人误判。
ALTER TABLE chat_sessions ADD COLUMN mcp_token_id INTEGER;

-- 会话列表要按令牌回读展示信息，改绑也要查，索引让这两件事都是常数级。
CREATE INDEX IF NOT EXISTS idx_chat_sessions_mcp_token ON chat_sessions(mcp_token_id);

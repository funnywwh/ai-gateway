-- M87 第二步：key_prefix / token_prefix 不再是身份，只是"给人看的标签"。
--
-- 唯一索引降级为普通索引：它仍然要支持"按前缀问归属"（管理面 keys/lookup 的前缀分支与
-- 运维查询），但同一个前缀现在可以有多行 —— sub2api 那种允许自定义 key 的源系统里，
-- 两把不同的明文前缀相同是常态，迁移不该因此要求某个客户换 key。
--
-- 列本身不变（TEXT NOT NULL，允许空串），所以没有表重建。
DROP INDEX IF EXISTS idx_api_keys_prefix;
DROP INDEX IF EXISTS idx_mcp_tokens_prefix;
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix_lookup ON api_keys(key_prefix);
CREATE INDEX IF NOT EXISTS idx_mcp_tokens_prefix_lookup ON mcp_tokens(token_prefix);

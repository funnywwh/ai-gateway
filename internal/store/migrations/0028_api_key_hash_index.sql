-- M87: 鉴权查找键从"明文前 12 字符前缀"换成 token 的 SHA-256 哈希。
--
-- 为什么：前缀只有 30 bit（MCP token 是 15 bit），撞车会让后来者静默接管已有行的
-- account/name/hash/tags；哈希是 256 bit，实际不可能撞，而它早就在库里（key_hash /
-- token_hash），所以切换查找键对已发的 key 完全透明。
--
-- 前缀列这一步保留且仍然唯一：只回滚二进制时老版本仍按前缀查，行为不变。放开前缀唯一性
-- 是 0029 的事，两步之间可以安全回滚。
--
-- 前置条件：库里不能已有重复 key_hash / token_hash。重复只可能来自"同一个明文配了不同前缀
-- 导入了两次"（M43/M80 的哈希形式导入；签发路径不可能产生）。有重复时下面第一条语句失败、
-- 整个迁移在事务内回滚，网关拒绝启动并报
--   store: apply migration 0028_api_key_hash_index: UNIQUE constraint failed: api_keys.key_hash
-- 处置：
--   SELECT key_hash, group_concat(id), group_concat(key_prefix), group_concat(name)
--   FROM api_keys GROUP BY key_hash HAVING count(*) > 1;
-- 保留一把（例如 last_used_at 更新的那把），另一把 disable 或删除后重启。
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_tokens_hash ON mcp_tokens(token_hash);

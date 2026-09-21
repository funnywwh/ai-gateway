-- M73: 控制台「智能问答」的联网能力（web_search / web_fetch）。
--
-- 两个开关是刻意的：chat.web_access.enabled 是本部署的总闸，决定「这个部署有没有联网
-- 能力」；这一列决定「这一个会话用不用它」。总闸打开不应改变既有会话能触达的范围，所以
-- 每个会话默认关闭，由用户在会话里显式打开。
--
-- 默认 0 = 关闭，正好是既有行的状态：升级后已经存在的会话行为不变。
ALTER TABLE chat_sessions ADD COLUMN web_access INTEGER NOT NULL DEFAULT 0;

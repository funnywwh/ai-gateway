-- 控制台「智能问答」的持久化：会话、轮次、消息、工具调用、私有技能与预览产物。
--
-- 归属：全部按 admin_users.id（控制台登录账号）隔离，而不是按计费账户/API Key。
-- 聊天会用某个账户的 Key 计费，但「谁在问、技能归谁」是登录人自己的事；两者混用会
-- 让一个账户下的多个管理员互相看见彼此的技能，那是产品上明确不要的行为。
--
-- 因此每个读接口都必须带 owner_user_id 条件（store/chat.go 里没有任何 owner<=0 的
-- 通配语义），他人 id 一律返回 not found，避免泄露「存在性」。
CREATE TABLE IF NOT EXISTS chat_sessions (
    id               TEXT PRIMARY KEY,
    owner_user_id    INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    owner_username   TEXT    NOT NULL DEFAULT '',
    title            TEXT    NOT NULL DEFAULT '',
    model            TEXT    NOT NULL DEFAULT '',
    account_id       INTEGER NOT NULL DEFAULT 0,
    api_key_id       INTEGER NOT NULL DEFAULT 0,
    write_mode       TEXT    NOT NULL DEFAULT 'read_only',
    skill_ids_json   TEXT    NOT NULL DEFAULT '[]',
    status           TEXT    NOT NULL DEFAULT 'active',
    message_count    INTEGER NOT NULL DEFAULT 0,
    tokens_in        INTEGER NOT NULL DEFAULT 0,
    tokens_out       INTEGER NOT NULL DEFAULT 0,
    tokens_reasoning INTEGER NOT NULL DEFAULT 0,
    last_message_at  INTEGER,
    created_at       INTEGER NOT NULL DEFAULT 0,
    updated_at       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_owner ON chat_sessions(owner_user_id, updated_at DESC, id DESC);

-- 一轮 = 用户提一个问题（可能包含多步模型调用与多次工具调用）。
-- turn_id 由浏览器生成，是幂等键：同 (session_id, turn_id) 重复提交返回既有状态而不是
-- 再花一次钱；这也是「断线后刷新页面」不会重复计费的唯一依据。
CREATE TABLE IF NOT EXISTS chat_turns (
    id               TEXT PRIMARY KEY,
    session_id       TEXT    NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    turn_id          TEXT    NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'running',
    error            TEXT    NOT NULL DEFAULT '',
    request_ids_json TEXT    NOT NULL DEFAULT '[]',
    created_at       INTEGER NOT NULL DEFAULT 0,
    updated_at       INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_turns_identity ON chat_turns(session_id, turn_id);
CREATE INDEX IF NOT EXISTS idx_chat_turns_status ON chat_turns(status);

-- assistant 消息的 provider_items_json 保存规范化后的 provider items（含 reasoning 项与
-- function_call/function_call_output 配对），它才是下一轮对话真正回灌给模型的东西；
-- content 只是给人看的文本，由 parts 派生，绝不反向解析 content 重建上下文。
CREATE TABLE IF NOT EXISTS chat_messages (
    id                  TEXT PRIMARY KEY,
    session_id          TEXT    NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    turn_id             TEXT    NOT NULL DEFAULT '',
    seq                 INTEGER NOT NULL,
    role                TEXT    NOT NULL,
    content             TEXT    NOT NULL DEFAULT '',
    parts_json          TEXT    NOT NULL DEFAULT '[]',
    provider_items_json TEXT    NOT NULL DEFAULT '[]',
    reasoning           TEXT    NOT NULL DEFAULT '',
    status              TEXT    NOT NULL DEFAULT 'ok',
    truncated           INTEGER NOT NULL DEFAULT 0,
    error               TEXT    NOT NULL DEFAULT '',
    model               TEXT    NOT NULL DEFAULT '',
    resolved_model      TEXT    NOT NULL DEFAULT '',
    provider            TEXT    NOT NULL DEFAULT '',
    request_ids_json    TEXT    NOT NULL DEFAULT '[]',
    tokens_in           INTEGER NOT NULL DEFAULT 0,
    tokens_out          INTEGER NOT NULL DEFAULT 0,
    tokens_reasoning    INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_messages_seq ON chat_messages(session_id, seq);

-- 工具调用单独成行：它是「模型提议 → 服务端执行」的边界，必须先落 pending 再执行，
-- 这样进程崩溃/断线后能明确告诉用户「这一步可能已经执行」，而不是静默重放。
-- 幂等键是 (turn_id, step, call_id)：同一轮里模型的 call_id 只在一步内有意义。
CREATE TABLE IF NOT EXISTS chat_tool_calls (
    id          TEXT PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    turn_id     TEXT    NOT NULL DEFAULT '',
    step        INTEGER NOT NULL DEFAULT 0,
    call_id     TEXT    NOT NULL DEFAULT '',
    name        TEXT    NOT NULL DEFAULT '',
    arguments   TEXT    NOT NULL DEFAULT '',
    result      TEXT    NOT NULL DEFAULT '',
    is_error    INTEGER NOT NULL DEFAULT 0,
    status      TEXT    NOT NULL DEFAULT 'pending',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_tool_calls_identity ON chat_tool_calls(turn_id, step, call_id);

-- 技能是登录人自己的资产：name 只在 owner 内唯一。source_session_id 只是来源标注，
-- 会话被删掉不应连带删掉技能（技能已经是可以独立复用的东西了）。
CREATE TABLE IF NOT EXISTS chat_skills (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_user_id     INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    name              TEXT    NOT NULL,
    description       TEXT    NOT NULL DEFAULT '',
    instructions      TEXT    NOT NULL DEFAULT '',
    source_session_id TEXT    NOT NULL DEFAULT '',
    source_model      TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_skills_owner_name ON chat_skills(owner_user_id, name);
CREATE INDEX IF NOT EXISTS idx_chat_skills_owner_time ON chat_skills(owner_user_id, updated_at DESC, id DESC);

-- 预览产物：HTML5 页面或 SVG。正文按 (session_id, key) 幂等 upsert，key 由前端按「哪个
-- 代码块」生成，所以反复点「预览」不会堆积副本；超量时按会话淘汰最旧的。
CREATE TABLE IF NOT EXISTS chat_artifacts (
    id            TEXT PRIMARY KEY,
    owner_user_id INTEGER NOT NULL,
    session_id    TEXT    NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    key           TEXT    NOT NULL DEFAULT '',
    title         TEXT    NOT NULL DEFAULT '',
    format        TEXT    NOT NULL DEFAULT 'html',
    body          TEXT    NOT NULL DEFAULT '',
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_artifacts_key ON chat_artifacts(session_id, key);

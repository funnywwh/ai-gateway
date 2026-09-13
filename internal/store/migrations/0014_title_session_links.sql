-- Evidence is scoped to the owner and records only a digest, not prompt text.
-- Original session identity remains available for ambiguity rollback.
CREATE TABLE request_title_links (
 request_id TEXT PRIMARY KEY REFERENCES request_logs(request_id) ON DELETE CASCADE,
 account_id INTEGER NOT NULL,
 api_key_id INTEGER NOT NULL,
 workspace TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 source_session TEXT NOT NULL,
 call_kind TEXT NOT NULL,
 started_at INTEGER NOT NULL
);
CREATE INDEX idx_title_links_match ON request_title_links(account_id,api_key_id,workspace,fingerprint,call_kind,started_at,source_session);

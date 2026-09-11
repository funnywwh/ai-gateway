-- The retention janitor deletes stored responses whose expires_at has passed; without an
-- index that is a full scan of a table whose rows hold whole response bodies.
CREATE INDEX IF NOT EXISTS idx_responses_expires ON responses(expires_at);

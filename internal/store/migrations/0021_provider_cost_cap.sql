-- M56: per-provider cost cap (docs/design/m56-provider-cost-cap.md).
--
-- cost_limit_micros is what the gateway may spend on this upstream before the router stops
-- choosing it, in ledger micro-units; 0 means unlimited and is what every existing row gets.
-- cost_period decides when the accumulation restarts on its own ('none' counts from the
-- last reset), and cost_window_start is that last reset (NULL = never reset). A reset only
-- moves this instant forward: no metering row is ever rewritten or deleted.
ALTER TABLE providers ADD COLUMN cost_limit_micros INTEGER NOT NULL DEFAULT 0;
ALTER TABLE providers ADD COLUMN cost_period TEXT NOT NULL DEFAULT 'none';
ALTER TABLE providers ADD COLUMN cost_window_start INTEGER;

-- The cap is read by summing usage_records per provider over a time window, so that read
-- needs its own range scan: the existing indexes are all (something, created_at) for
-- account/key/model, and none of them can serve "this provider since T".
CREATE INDEX IF NOT EXISTS idx_usage_provider_time ON usage_records(provider_id, created_at);
